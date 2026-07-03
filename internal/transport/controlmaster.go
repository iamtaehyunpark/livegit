package transport

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"golang.org/x/term"
)

// lg manages its OWN ssh ControlMaster so a Duo/2FA host is authenticated
// exactly once and every later connection — the data channel, the agent deploy,
// each `lg <cmd>` — multiplexes over the same socket with no further prompt.
// Without this, lg relied entirely on a master the user had established
// out-of-band (their shell login automation); the first connection when none
// existed would hang, because lg launches ssh non-interactively (no tty, stderr
// to the log file) and a Duo prompt has nowhere to render.
//
// This only applies in system-ssh mode: the native Go client can't multiplex, so
// every helper here is a no-op for native/password auth.
//
// The socket path is keyed by ssh's %C token (a hash of local host + remote host
// + port + user), so it is short (fits macOS's 104-byte unix-socket path limit)
// and shared by every lg project pointing at the same Source — one master, one
// Duo, many projects. ssh expands both `~` and `%C` itself, so we pass the
// literal template on every invocation and never resolve it in Go; identical
// (host, port, user) inputs therefore always resolve to the same socket.
const controlPathTemplate = "~/.ssh/lg-cm-%C"

// ErrNeedConnect signals that no master is live and lg can't bootstrap one
// without an interactive terminal (a Duo/2FA prompt needs somewhere to render).
// Callers turn this into "run `lg connect`" guidance.
var ErrNeedConnect = errors.New("no ssh connection to Source; run `lg connect` to authenticate (handles Duo/2FA)")

// usesControlMaster reports whether this config drives ssh through the system
// binary — the only mode where multiplexing (and thus a master) applies. Note
// that auth=password does NOT opt out by itself: password + ssh_mode=system is
// the supported setup for a password+Duo host — `lg connect` shows ssh's own
// password and Duo prompts once, and the master carries everything after.
func usesControlMaster(cfg *config.Config) bool {
	return cfg.Source.SSHMode != "native"
}

// controlPathArgs binds an ssh invocation to lg's master socket (enough for
// `-O check`/`-O exit`, which only need the path + destination).
func controlPathArgs() []string {
	return []string{"-o", "ControlPath=" + controlPathTemplate}
}

// masterOpts returns the options an invocation needs to CREATE and persist the
// master: the socket path plus ControlPersist (how long it outlives the last
// client) and keepalives so an idle NAT/firewall doesn't silently kill it (and a
// dead peer is reaped promptly instead of wedging reuse behind a stale socket).
func masterOpts(cfg *config.Config) []string {
	args := controlPathArgs()
	return append(args,
		"-o", "ControlPersist="+persistValue(cfg),
		"-o", "ServerAliveInterval=60",
		"-o", "ServerAliveCountMax=3",
	)
}

// persistValue maps config.control_persist to ssh's ControlPersist value.
// "max" (and the spellings users guess: forever/yes/0) means no timer at all —
// the master lives until the connection actually drops (reboot, network death;
// the keepalives above reap a dead peer). That's ssh's "yes". We write "max"
// in config.yaml rather than "yes" because bare `yes` is a boolean to YAML.
func persistValue(cfg *config.Config) string {
	switch strings.ToLower(cfg.Source.ControlPersist) {
	case "":
		return "8h"
	case "max", "forever", "yes", "0":
		return "yes"
	}
	return cfg.Source.ControlPersist
}

// portArgs is the `-p <port>` pair (empty for the default port). Shared so the
// destination spelling — and therefore the %C hash — is identical everywhere.
func portArgs(cfg *config.Config) []string {
	if cfg.Source.Port != 0 && cfg.Source.Port != 22 {
		return []string{"-p", strconv.Itoa(cfg.Source.Port)}
	}
	return nil
}

// MasterLive reports whether a usable master socket is already established
// (`ssh -O check` exits 0 == "Master running"). False for native/password mode.
func MasterLive(cfg *config.Config) bool {
	if !usesControlMaster(cfg) {
		return false
	}
	args := append([]string{"-O", "check"}, controlPathArgs()...)
	args = append(args, portArgs(cfg)...)
	args = append(args, sshTargetOf(cfg))
	return exec.Command("ssh", args...).Run() == nil
}

// Connect establishes (or reuses) lg's ssh master, showing any Duo/2FA prompt on
// the attached terminal. Idempotent: returns immediately if a master is already
// live. System-ssh mode only.
func Connect(cfg *config.Config) error {
	if !usesControlMaster(cfg) {
		return fmt.Errorf("this project authenticates with the native ssh client; there is no ssh master to establish")
	}
	if MasterLive(cfg) {
		return nil
	}
	// Open the master with the real terminal attached so an interactive Duo push
	// / passcode prompt is visible and answerable. `true` is the trivial payload:
	// ssh authenticates, runs it, the foreground client exits — and ControlPersist
	// keeps the master alive in the background for every later connection.
	args := []string{"-o", "ControlMaster=auto"}
	args = append(args, masterOpts(cfg)...)
	args = append(args, portArgs(cfg)...)
	args = append(args, sshTargetOf(cfg), "true")
	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// password + system mode (the password+Duo setup): auto-fill the stored
	// password via SSH_ASKPASS so the user only answers the Duo step. Best
	// effort — if the shim can't be set up, ssh just prompts on the tty.
	if cfg.Source.Auth == "password" {
		if env, err := askpassEnv(); err == nil {
			cmd.Env = append(os.Environ(), env...)
		} else {
			fmt.Fprintf(os.Stderr, "lg: password auto-fill unavailable (%v); type it at the prompt.\n", err)
		}
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("establish ssh connection: %w", err)
	}
	if !MasterLive(cfg) {
		return fmt.Errorf("ssh master did not come up (authentication may have failed)")
	}
	return nil
}

// EnsureMaster guarantees a live master before lg opens data connections. If none
// is up and stdin is a terminal, it bootstraps one interactively (the "just run
// lg connect for me" path). If none is up and there's no terminal — a script or a
// coding agent driving lg — it returns ErrNeedConnect so the caller can print
// actionable guidance instead of hanging. No-op for native/password mode.
func EnsureMaster(cfg *config.Config) error {
	if !usesControlMaster(cfg) {
		return nil
	}
	if MasterLive(cfg) {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return ErrNeedConnect
	}
	return Connect(cfg)
}

// StopMaster tears down lg's master socket (`ssh -O exit`); the next connection
// re-authenticates. No-op (nil) for native/password mode.
func StopMaster(cfg *config.Config) error {
	if !usesControlMaster(cfg) {
		return nil
	}
	args := append([]string{"-O", "exit"}, controlPathArgs()...)
	args = append(args, portArgs(cfg)...)
	args = append(args, sshTargetOf(cfg))
	return exec.Command("ssh", args...).Run()
}

// PersistLabel is the human-readable master lifetime for status/CLI messages —
// a phrase that completes "cached …": "for 8h", or for the no-timer setting,
// "until the connection drops".
func PersistLabel(cfg *config.Config) string {
	if v := persistValue(cfg); v != "yes" {
		return "for " + v
	}
	return "until the connection drops (no expiry)"
}
