package shell

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/taehyun/lg/internal/config"
)

// Mode is the shell's current routing mode (§5.2).
type Mode string

const (
	ModeLocal  Mode = "local"
	ModeSource Mode = "source"
)

// SessionState is the per-tab state shared between the long-lived `lg shell`
// process and the short-lived preexec-hook invocations. Because the hook runs
// as a fresh process per command, the state machine must be persisted (§5.1).
type SessionState struct {
	TabID      string     `json:"tab_id"`
	Mode       Mode       `json:"mode"`
	EnteredVia EnteredVia `json:"entered_via,omitempty"`
	// MarkerScope is the rel directory subtree a "dir:" entry is scoped to; on
	// cd outside it, SOURCE mode exits (§5.4).
	MarkerScope string `json:"marker_scope,omitempty"`
	Session     string `json:"session,omitempty"` // remote tmux session name
}

// runDir holds per-tab state files.
func runDir() string { return filepath.Join(config.Dir(), "run") }

func statePath(tabID string) string {
	return filepath.Join(runDir(), tabID+".json")
}

// LoadState reads the per-tab state, defaulting to LOCAL if absent.
func LoadState(tabID string) *SessionState {
	b, err := os.ReadFile(statePath(tabID))
	if err != nil {
		return &SessionState{TabID: tabID, Mode: ModeLocal}
	}
	var s SessionState
	if err := json.Unmarshal(b, &s); err != nil {
		return &SessionState{TabID: tabID, Mode: ModeLocal}
	}
	if s.Mode == "" {
		s.Mode = ModeLocal
	}
	s.TabID = tabID
	return &s
}

// Save persists the per-tab state.
func (s *SessionState) Save() error {
	if err := os.MkdirAll(runDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(s.TabID), b, 0o644)
}

// SetLocal resets to LOCAL mode (forced exit / `lg local`).
func (s *SessionState) SetLocal() {
	s.Mode = ModeLocal
	s.EnteredVia = ""
	s.MarkerScope = ""
	s.Session = ""
}

// SetSource records SOURCE-mode entry.
func (s *SessionState) SetSource(via EnteredVia, markerScope, session string) {
	s.Mode = ModeSource
	s.EnteredVia = via
	s.MarkerScope = markerScope
	s.Session = session
}

// Clear removes the per-tab state file (on shell exit).
func ClearState(tabID string) { _ = os.Remove(statePath(tabID)) }
