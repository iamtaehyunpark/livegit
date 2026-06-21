package transport

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/taehyun/lg/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshConn is a live connection to Source whose pipe carries the yamux session.
// closer tears down whatever backs it (a native ssh client, or a spawned `ssh`
// subprocess).
type sshConn struct {
	pipe   *rwc
	closer func() error
}

func (c *sshConn) Close() error {
	if c.closer != nil {
		return c.closer()
	}
	return nil
}

// dialSSH dispatches to the configured transport. "system" (default) shells out
// to the real `ssh` binary so ~/.ssh/config applies; "native" uses the Go ssh
// client.
func dialSSH(cfg *config.Config, remoteBin string) (*sshConn, error) {
	if cfg.Source.SSHMode == "native" {
		return dialNativeSSH(cfg, remoteBin)
	}
	return dialSystemSSH(cfg, remoteBin)
}

// dialNativeSSH connects with the built-in Go ssh client (ignores ~/.ssh/config).
func dialNativeSSH(cfg *config.Config, remoteBin string) (*sshConn, error) {
	auths, err := authMethods()
	if err != nil {
		return nil, err
	}
	hostKeyCb, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	// Tolerate a host written as "user@host" (a common mistake): split it so we
	// don't try to DNS-resolve the whole string.
	host := cfg.Source.Host
	user := cfg.Source.User
	if at := strings.LastIndex(host, "@"); at >= 0 {
		user = host[:at]
		host = host[at+1:]
	}
	clientCfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hostKeyCb,
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", cfg.Source.Port))
	client, err := ssh.Dial("tcp", addr, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, err
	}
	// Stream stderr to our stderr for remote-agent diagnostics.
	session.Stderr = os.Stderr

	cmd := fmt.Sprintf("%s serve --remote-root %q", remoteBin, cfg.Source.RemoteRoot)
	if err := session.Start(cmd); err != nil {
		_ = session.Close()
		_ = client.Close()
		return nil, fmt.Errorf("start remote agent: %w", err)
	}
	return &sshConn{
		pipe: &rwc{r: stdout, w: stdin, c: nil},
		closer: func() error {
			_ = session.Close()
			return client.Close()
		},
	}, nil
}

// dialSystemSSH spawns the real `ssh` binary running `<remoteBin> serve` on
// Source, using its stdio as the yamux transport. Because it's the system ssh,
// it honors ~/.ssh/config entirely: Host aliases, ProxyJump/bastions,
// ControlMaster (so Duo/2FA isn't re-prompted), IdentityFile, known_hosts.
func dialSystemSSH(cfg *config.Config, remoteBin string) (*sshConn, error) {
	target := cfg.Source.Host
	if cfg.Source.User != "" && !strings.Contains(target, "@") {
		target = cfg.Source.User + "@" + target
	}
	args := []string{"-T"} // no pseudo-tty: we want a clean binary pipe
	if cfg.Source.Port != 0 && cfg.Source.Port != 22 {
		args = append(args, "-p", strconv.Itoa(cfg.Source.Port))
	}
	remoteCmd := fmt.Sprintf("%s serve --remote-root %s", remoteBin, shellQuote(cfg.Source.RemoteRoot))
	args = append(args, target, remoteCmd)

	cmd := exec.Command("ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Route ssh's own stderr (connection errors, occasional prompts) to the lg
	// log file rather than the terminal, so failures don't spam the shell. With
	// a ControlMaster already established (the common setup), no prompt occurs.
	if lf, ferr := os.OpenFile(config.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		cmd.Stderr = lf
	} else {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn ssh: %w", err)
	}
	return &sshConn{
		pipe: &rwc{r: stdout, w: stdin, c: nil},
		closer: func() error {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			return nil
		},
	}, nil
}

// shellQuote single-quotes a string for safe embedding in the remote ssh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	// 1. ssh-agent, if available.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}
	// 2. Default private keys under ~/.ssh.
	home, _ := os.UserHomeDir()
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		keyPath := filepath.Join(home, ".ssh", name)
		b, err := os.ReadFile(keyPath)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			continue // encrypted/unsupported; skip silently
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no ssh auth methods available (no agent, no usable keys in ~/.ssh)")
	}
	return methods, nil
}

func hostKeyCallback() (ssh.HostKeyCallback, error) {
	home, _ := os.UserHomeDir()
	kh := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(kh); err != nil {
		// No known_hosts yet: in this single-user tool, fall back to accepting
		// and recording is out of scope. We require the file to exist to avoid
		// silently trusting any host.
		return nil, fmt.Errorf("~/.ssh/known_hosts not found; add the Source host key first (ssh once manually)")
	}
	cb, err := knownhosts.New(kh)
	if err != nil {
		return nil, fmt.Errorf("parse known_hosts: %w", err)
	}
	return cb, nil
}
