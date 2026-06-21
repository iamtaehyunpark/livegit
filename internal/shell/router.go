package shell

import (
	"strings"

	"github.com/taehyun/lg/internal/config"
)

// Class is how a LOCAL-mode command is routed (§5.6).
type Class int

const (
	// ClassReadonly: file-reading commands (cat/grep/...). The FUSE open() hook
	// transparently fetches ghost files, so these just run locally (§5.6).
	ClassReadonly Class = iota
	// ClassGeneral: low file-dependency commands run locally by default.
	ClassGeneral
	// ClassAlwaysSource: matched always_source_patterns; force SOURCE entry.
	ClassAlwaysSource
)

// Router classifies commands in LOCAL mode.
type Router struct {
	readonly map[string]bool
	engine   *TriggerEngine
}

// NewRouter builds a router from config + trigger engine.
func NewRouter(cfg *config.Config, engine *TriggerEngine) *Router {
	ro := map[string]bool{}
	for _, c := range cfg.ReadonlyCommands {
		ro[c] = true
	}
	return &Router{readonly: ro, engine: engine}
}

// Classify routes a command string (§5.6). always_source takes precedence so
// heavy compute is forced remote even if it superficially looks general.
func (r *Router) Classify(command string) Class {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ClassGeneral
	}
	for _, re := range r.engine.alwaysSource {
		if re.MatchString(cmd) {
			return ClassAlwaysSource
		}
	}
	if r.readonly[firstWord(cmd)] {
		return ClassReadonly
	}
	return ClassGeneral
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}
