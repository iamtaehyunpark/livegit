package transport

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"golang.org/x/crypto/ssh"
)

// AgentPicker returns the embedded Linux agent bytes for a GOARCH, or nil if not
// embedded in this build. (internal/agentbin.Pick satisfies this.)
type AgentPicker func(goarch string) []byte

// sshRunner runs one command on Source, optionally feeding stdin, returning
// stdout. It abstracts over the two transports so EnsureAgent doesn't care which.
type sshRunner func(cmd string, stdin *bytes.Reader) (string, error)

// EnsureAgent makes sure the `lg` agent is installed at ~/.local/bin/lg on
// Source — uploading the matching embedded Linux binary if it's missing, or
// re-uploading when its version differs from this build (wantVersion; an old
// agent silently times out on RPCs it doesn't know, e.g. `lg jobs`). Returns a
// human-readable summary. Used by `lg init` and `lg connect`.
//
// In system-ssh mode it runs over lg's ControlMaster (reusing the connection
// `lg connect` / `lg init` just authenticated), so it works on a Duo/2FA host
// without a second prompt. In native mode it uses the Go ssh client.
func EnsureAgent(cfg *config.Config, pick AgentPicker, wantVersion string) (string, error) {
	run, closeFn, err := agentRunner(cfg)
	if err != nil {
		return "", err
	}
	defer closeFn()

	// Already installed? (bare `lg` on PATH, or the standard install location.)
	// A version match means done; a mismatch falls through to a redeploy. "dev"
	// builds carry no comparable version, so they never force an upgrade.
	installedVer := ""
	if out, _ := run(`command -v lg 2>/dev/null || (test -x "$HOME/.local/bin/lg" && echo "$HOME/.local/bin/lg")`, nil); strings.TrimSpace(out) != "" {
		ver, _ := run(`PATH="$HOME/.local/bin:$PATH" lg --version 2>/dev/null`, nil)
		installedVer = strings.TrimSpace(ver)
		if wantVersion == "" || wantVersion == "dev" || installedVer == "lg "+wantVersion {
			return "agent already installed on Source (" + installedVer + ")", nil
		}
	}

	// Pick the binary for the remote architecture.
	unameOut, err := run("uname -m", nil)
	if err != nil {
		return "", fmt.Errorf("probe remote arch: %w", err)
	}
	goarch := unameToGoarch(strings.TrimSpace(unameOut))
	agent := pick(goarch)
	if agent == nil {
		if installedVer != "" {
			// Can't auto-upgrade this arch, but an agent is there — report, don't fail.
			return fmt.Sprintf("agent installed on Source (%s; local is lg %s, no embedded agent for arch %q to auto-upgrade)",
				installedVer, wantVersion, strings.TrimSpace(unameOut)), nil
		}
		return "", fmt.Errorf("no embedded agent for remote arch %q; deploy it manually:\n"+
			"    scp dist/lg-linux-%s %s:~/.local/bin/lg",
			strings.TrimSpace(unameOut), goarch, sshTargetOf(cfg))
	}

	// Upload: pipe the bytes into a temp file, chmod, atomic rename.
	upload := `mkdir -p "$HOME/.local/bin" && cat > "$HOME/.local/bin/lg.tmp" && ` +
		`chmod +x "$HOME/.local/bin/lg.tmp" && mv -f "$HOME/.local/bin/lg.tmp" "$HOME/.local/bin/lg"`
	if _, err := run(upload, bytes.NewReader(agent)); err != nil {
		return "", fmt.Errorf("upload agent: %w", err)
	}
	ver, err := run(`PATH="$HOME/.local/bin:$PATH" lg --version`, nil)
	if err != nil {
		return "", fmt.Errorf("uploaded, but the agent won't run: %w", err)
	}
	if installedVer != "" {
		return fmt.Sprintf("upgraded agent at ~/.local/bin/lg (%s -> %s, linux-%s)", installedVer, strings.TrimSpace(ver), goarch), nil
	}
	return fmt.Sprintf("deployed agent to ~/.local/bin/lg (linux-%s, %s)", goarch, strings.TrimSpace(ver)), nil
}

// agentRunner picks the transport for EnsureAgent's commands: the system ssh
// binary (over lg's master) for system mode, else the native Go client.
func agentRunner(cfg *config.Config) (sshRunner, func(), error) {
	if usesControlMaster(cfg) {
		return func(cmd string, stdin *bytes.Reader) (string, error) {
			return runSystemSSH(cfg, cmd, stdin)
		}, func() {}, nil
	}
	client, err := nativeClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	return func(cmd string, stdin *bytes.Reader) (string, error) {
		return runSSH(client, cmd, stdin)
	}, func() { _ = client.Close() }, nil
}

// runSystemSSH runs one command via the `ssh` binary over lg's ControlMaster.
// BatchMode makes it fail fast if the master is down (rather than prompt), so the
// caller should have established it first (EnsureMaster / lg connect).
func runSystemSSH(cfg *config.Config, command string, stdin *bytes.Reader) (string, error) {
	args := []string{"-T", "-o", "ControlMaster=auto", "-o", "BatchMode=yes"}
	args = append(args, masterOpts(cfg)...)
	args = append(args, portArgs(cfg)...)
	args = append(args, sshTargetOf(cfg), command)
	cmd := exec.Command("ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// runSSH runs one command over the native client, optionally feeding stdin,
// returning stdout. Errors include remote stderr for diagnostics.
func runSSH(client *ssh.Client, cmd string, stdin *bytes.Reader) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if stdin != nil {
		session.Stdin = stdin
	}
	if err := session.Run(cmd); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func unameToGoarch(uname string) string {
	switch uname {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return ""
	}
}

func sshTargetOf(cfg *config.Config) string {
	h := cfg.Source.Host
	if cfg.Source.User != "" && !strings.Contains(h, "@") {
		return cfg.Source.User + "@" + h
	}
	return h
}
