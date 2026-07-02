package transport

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"golang.org/x/crypto/ssh"
)

// AgentPicker returns the embedded Linux agent bytes for a GOARCH, or nil if not
// embedded in this build. (internal/agentbin.Pick satisfies this.)
type AgentPicker func(goarch string) []byte

// EnsureAgent connects to Source (native ssh — password auth if configured) and
// makes sure the `lg` agent is installed at ~/.local/bin/lg, uploading the
// matching embedded Linux binary if it's missing. It returns a human-readable
// summary of what it did. Used by `lg init`.
func EnsureAgent(cfg *config.Config, pick AgentPicker) (string, error) {
	client, err := nativeClient(cfg)
	if err != nil {
		return "", err
	}
	defer client.Close()

	// Already installed? (bare `lg` on PATH, or the standard install location.)
	if out, _ := runSSH(client, `command -v lg 2>/dev/null || (test -x "$HOME/.local/bin/lg" && echo "$HOME/.local/bin/lg")`, nil); strings.TrimSpace(out) != "" {
		ver, _ := runSSH(client, `PATH="$HOME/.local/bin:$PATH" lg --version 2>/dev/null`, nil)
		return "agent already installed on Source (" + strings.TrimSpace(ver) + ")", nil
	}

	// Pick the binary for the remote architecture.
	unameOut, err := runSSH(client, "uname -m", nil)
	if err != nil {
		return "", fmt.Errorf("probe remote arch: %w", err)
	}
	goarch := unameToGoarch(strings.TrimSpace(unameOut))
	agent := pick(goarch)
	if agent == nil {
		return "", fmt.Errorf("no embedded agent for remote arch %q; deploy it manually:\n"+
			"    scp dist/lg-linux-%s %s:~/.local/bin/lg",
			strings.TrimSpace(unameOut), goarch, sshTargetOf(cfg))
	}

	// Upload: pipe the bytes into a temp file, chmod, atomic rename.
	upload := `mkdir -p "$HOME/.local/bin" && cat > "$HOME/.local/bin/lg.tmp" && ` +
		`chmod +x "$HOME/.local/bin/lg.tmp" && mv -f "$HOME/.local/bin/lg.tmp" "$HOME/.local/bin/lg"`
	if _, err := runSSH(client, upload, bytes.NewReader(agent)); err != nil {
		return "", fmt.Errorf("upload agent: %w", err)
	}
	ver, err := runSSH(client, `PATH="$HOME/.local/bin:$PATH" lg --version`, nil)
	if err != nil {
		return "", fmt.Errorf("uploaded, but the agent won't run: %w", err)
	}
	return fmt.Sprintf("deployed agent to ~/.local/bin/lg (linux-%s, %s)", goarch, strings.TrimSpace(ver)), nil
}

// runSSH runs one command over the client, optionally feeding stdin, returning
// combined-ish stdout. Errors include remote stderr for diagnostics.
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
