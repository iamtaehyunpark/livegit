// Package shell implements the unified shell: preexec-driven SOURCE-mode trigger
// detection (D2, §5.1–5.2), the LOCAL/SOURCE state machine (§5.2), command
// routing (§5.6), and the local-terminal <-> remote-tmux PTY bridge (§5.3).
package shell

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/taehyun/lg/internal/config"
)

// EnteredVia records what triggered SOURCE-mode entry, which also determines the
// exit condition (§5.4).
type EnteredVia string

// Decision is the result of evaluating a command line in LOCAL mode.
type Decision struct {
	Enter      bool       // should we switch to SOURCE mode?
	Via        EnteredVia // "conda" | "venv" | "poetry" | "dir:<marker>" | "always"
	ExitCmd    string     // command string whose execution exits SOURCE mode (if pattern-based)
	MatchedPat string     // the trigger pattern that matched (for exit-map lookup)
}

// TriggerEngine evaluates whether a command in a directory should enter SOURCE
// mode. Compiled once from config; the preexec hook calls Evaluate per command.
type TriggerEngine struct {
	patterns        []*regexp.Regexp
	patternSrc      []string
	exitMap         map[string]*regexp.Regexp // pattern -> compiled exit-command regexp
	dirMarkers      []string
	alwaysSource    []*regexp.Regexp
	alwaysSourceRaw []string
}

// NewTriggerEngine compiles trigger config. Invalid regexes are skipped.
func NewTriggerEngine(cfg *config.Config) *TriggerEngine {
	t := &TriggerEngine{exitMap: map[string]*regexp.Regexp{}}
	st := cfg.SourceTriggers
	for _, p := range st.Patterns {
		if re, err := regexp.Compile(p); err == nil {
			t.patterns = append(t.patterns, re)
			t.patternSrc = append(t.patternSrc, p)
		}
	}
	for pat, exit := range st.ExitCommandMap {
		// exit is a literal command (e.g. "conda deactivate"); match it anchored.
		if re, err := regexp.Compile("^" + regexp.QuoteMeta(strings.TrimSpace(exit))); err == nil {
			t.exitMap[pat] = re
		}
	}
	t.dirMarkers = st.DirectoryMarkers
	for _, p := range st.AlwaysSourcePatterns {
		if re, err := regexp.Compile(p); err == nil {
			t.alwaysSource = append(t.alwaysSource, re)
			t.alwaysSourceRaw = append(t.alwaysSourceRaw, p)
		}
	}
	return t
}

// markerPresence reports, for each directory marker, whether it exists at relDir
// on Ghost. The caller supplies this because only the FUSE/daemon side knows
// Ghost-vs-Source presence; the engine stays pure.
type MarkerPresence func(relDir, marker string) (onGhost bool)

// Evaluate decides whether the given command (entered in relDir, LOCAL mode)
// should enter SOURCE mode. presence may be nil (directory-marker rule skipped).
//
// readonly indicates the command is a read-oriented command (cat/ls/grep/...).
// Such commands always stay LOCAL — the FUSE layer fetches their files
// transparently (§5.6) — so the directory-marker rule is suppressed for them.
// Explicit env-activation patterns and always_source patterns still fire.
func (t *TriggerEngine) Evaluate(command, relDir string, readonly bool, presence MarkerPresence) Decision {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return Decision{}
	}

	// (a) command-pattern matching — checked first per §5.2.
	for i, re := range t.patterns {
		if re.MatchString(cmd) {
			via := viaForPattern(t.patternSrc[i])
			return Decision{Enter: true, Via: via, MatchedPat: t.patternSrc[i]}
		}
	}

	// always_source_patterns: heavy compute forced to Source (§5.6).
	for _, re := range t.alwaysSource {
		if re.MatchString(cmd) {
			return Decision{Enter: true, Via: "always"}
		}
	}

	// (b) directory-marker detection: marker present on Source but absent on
	// Ghost (§5.2b) — but never for readonly commands, which stay local.
	if !readonly && presence != nil {
		for _, marker := range t.dirMarkers {
			if onGhost := presence(relDir, marker); !onGhost {
				return Decision{Enter: true, Via: EnteredVia("dir:" + marker)}
			}
		}
	}
	return Decision{}
}

// IsExit reports whether, given the entered_via, this command ends SOURCE mode
// (§5.4). For pattern-based entries it consults the exit-command map; for
// dir-based entries the caller handles cd-tracking separately.
func (t *TriggerEngine) IsExit(via EnteredVia, command string) bool {
	cmd := strings.TrimSpace(command)
	// Find the originating pattern for this via and test its exit command.
	for pat, exitRe := range t.exitMap {
		if viaForPattern(pat) == via && exitRe.MatchString(cmd) {
			return true
		}
	}
	// Generic fallbacks.
	switch via {
	case "venv":
		return cmd == "deactivate"
	case "conda":
		return strings.HasPrefix(cmd, "conda deactivate")
	case "poetry":
		return cmd == "exit"
	}
	return false
}

// ExitsDirMarker reports whether a cd command leaves the marker subtree that a
// "dir:<marker>" entry was scoped to (§5.4). prevDir/newDir are rel paths.
func ExitsDirMarker(via EnteredVia, markerDir string) bool {
	// markerDir is the rel directory of the new cwd relative to where the marker
	// lives; "" or ".." prefix means we've left the subtree.
	return markerDir == "" || strings.HasPrefix(markerDir, "..")
}

// viaForPattern maps a known trigger pattern to a short via label.
func viaForPattern(pattern string) EnteredVia {
	switch {
	case strings.Contains(pattern, "conda"):
		return "conda"
	case strings.Contains(pattern, "bin/activate"):
		return "venv"
	case strings.Contains(pattern, "poetry"):
		return "poetry"
	case strings.Contains(pattern, "pyenv"):
		return "pyenv"
	default:
		return "pattern"
	}
}

// MarkerDir is the conventional directory a "dir:<marker>" entry is scoped to:
// the directory containing the marker. Helper for cd-based exit tracking.
func MarkerDir(relDir, marker string) string {
	return config.Rel(filepath.ToSlash(filepath.Join(relDir, "..")))
}
