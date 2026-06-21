package transport

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/taehyun/lg/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshConn is the live SSH connection plus the exec session that carries yamux.
type sshConn struct {
	client  *ssh.Client
	session *ssh.Session
	pipe    *rwc
}

func (c *sshConn) Close() error {
	if c.session != nil {
		_ = c.session.Close()
	}
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// dialSSH connects to Source and starts `<remoteBin> serve` over an exec
// session, returning a pipe (its stdio) suitable for a yamux client. This is
// D1's "native x/crypto/ssh" path: we reuse Source's existing sshd rather than
// running our own daemon port.
func dialSSH(cfg *config.Config, remoteBin string) (*sshConn, error) {
	auths, err := authMethods()
	if err != nil {
		return nil, err
	}
	hostKeyCb, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	clientCfg := &ssh.ClientConfig{
		User:            cfg.Source.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCb,
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(cfg.Source.Host, fmt.Sprintf("%d", cfg.Source.Port))
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
		client:  client,
		session: session,
		pipe:    &rwc{r: stdout, w: stdin, c: nil},
	}, nil
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
