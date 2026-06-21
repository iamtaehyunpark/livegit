package agent

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/taehyun/lg/internal/proto"
)

// tmuxManager wraps the tmux binary on Source. Sessions live on a dedicated
// `-S` socket so lg's sessions are isolated from the user's normal tmux (§5.3).
type tmuxManager struct {
	socket string // path to the -S socket
}

func newTmuxManager(socket string) *tmuxManager { return &tmuxManager{socket: socket} }

func (t *tmuxManager) cmd(args ...string) *exec.Cmd {
	full := append([]string{"-S", t.socket}, args...)
	return exec.Command("tmux", full...)
}

// ensure attaches to an existing session or creates a detached one, returning
// whether it was newly created (§5.3 step 3).
func (t *tmuxManager) ensure(name string, cols, rows uint16) (created bool, err error) {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if err := t.cmd("has-session", "-t", name).Run(); err == nil {
		return false, nil // already exists
	}
	out, err := t.cmd("new-session", "-d", "-s", name,
		"-x", strconv.Itoa(int(cols)), "-y", strconv.Itoa(int(rows))).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("tmux new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// attachCmd builds the command that bridges a PTY to the session. It is run
// inside a pty by the caller; closing the pty detaches (does not kill) the
// session, preserving background work (§5.4).
func (t *tmuxManager) attachCmd(name string) *exec.Cmd {
	return t.cmd("attach-session", "-t", name)
}

// resize sets the session's client size (driven by Ghost SIGWINCH).
func (t *tmuxManager) resize(name string, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return t.cmd("refresh-client", "-C", fmt.Sprintf("%d,%d", cols, rows)).Run()
}

// list returns all sessions on the socket (§5.7, `lg sessions`).
func (t *tmuxManager) list() ([]proto.SessionInfo, error) {
	const format = "#{session_name}|#{session_attached}|#{session_windows}|#{session_created}"
	out, err := t.cmd("list-sessions", "-F", format).Output()
	if err != nil {
		// No server running yet => no sessions, not an error.
		return nil, nil
	}
	var sessions []proto.SessionInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			continue
		}
		attached, _ := strconv.Atoi(parts[1])
		windows, _ := strconv.Atoi(parts[2])
		created, _ := strconv.ParseInt(parts[3], 10, 64)
		sessions = append(sessions, proto.SessionInfo{
			Name:     parts[0],
			Attached: attached > 0,
			Windows:  windows,
			Created:  created,
		})
	}
	return sessions, nil
}
