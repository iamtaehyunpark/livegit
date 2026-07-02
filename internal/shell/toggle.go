package shell

import (
	"os"
	"path/filepath"

	"github.com/iamtaehyunpark/livegit/internal/config"
)

// Toggle mode: when on, every command typed in the shell is sent to
// Source. State is a per-tab marker file so the long-lived `lg shell` and the
// short-lived per-command hook invocations agree without IPC. The marker's mere
// existence means "on" — no contents to parse.

// runDir holds per-tab state files.
func runDir() string { return filepath.Join(config.Dir(), "run") }

func togglePath(tabID string) string {
	return filepath.Join(runDir(), tabID+".toggle")
}

// ToggleOn reports whether the tab currently routes commands to Source.
func ToggleOn(tabID string) bool {
	if tabID == "" {
		return false
	}
	_, err := os.Stat(togglePath(tabID))
	return err == nil
}

// SetToggle turns toggle mode on or off for a tab. Returns the new state.
func SetToggle(tabID string, on bool) error {
	if on {
		if err := os.MkdirAll(runDir(), 0o755); err != nil {
			return err
		}
		return os.WriteFile(togglePath(tabID), []byte("on\n"), 0o644)
	}
	if err := os.Remove(togglePath(tabID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearToggle removes the marker on shell exit.
func ClearToggle(tabID string) { _ = os.Remove(togglePath(tabID)) }
