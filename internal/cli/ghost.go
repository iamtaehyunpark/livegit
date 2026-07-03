package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/iamtaehyunpark/livegit/internal/transport"
)

// routeLogsToFile sends lg's own logs to ~/.lg/lg.log instead of the terminal,
// so background reconnect/health noise from long-running commands (shell, run)
// doesn't spam the user's shell. Returns the log path.
func routeLogsToFile(c *config.Config) string {
	path := config.LogPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "" // fall back to stderr (already configured)
	}
	logx.Init(c.LogLevel, f)
	return path
}

// loadGhost loads config and asserts the ghost role.
func loadGhost() (*config.Config, error) {
	c, err := config.Load()
	if err != nil {
		return nil, err
	}
	if c.Role != config.RoleGhost {
		return nil, fmt.Errorf("this command requires role=ghost (config says %q)", c.Role)
	}
	return c, nil
}

// newClient builds (and starts) a transport client for Source.
func newClient(c *config.Config) *transport.Client {
	client := transport.NewClient(c, c.Source.AgentBin)
	client.Start()
	return client
}

// ensureAuthenticated is the shared pre-dial auth guard: on a Duo/2FA host it
// bootstraps the ssh connection interactively when a terminal is attached (the
// automatic `lg connect` fallback), and otherwise returns actionable "run
// `lg connect`" guidance instead of letting the background dialer fail
// silently. No-op in native mode (connections carry their own credentials).
func ensureAuthenticated(cfg *config.Config) error {
	err := transport.EnsureMaster(cfg)
	if err == nil {
		return nil
	}
	if errors.Is(err, transport.ErrNeedConnect) {
		return fmt.Errorf("not connected to %s — run `lg connect` first (handles Duo/2FA)", cfg.Source.Host)
	}
	return fmt.Errorf("couldn't connect to %s: %w", cfg.Source.Host, err)
}

// connectedClient loads the ghost config, dials Source, and blocks until the
// link is online (or times out). The caller owns Close(). It's the shared setup
// for the short-lived job commands (jobs/logs), which just need one RPC round
// trip rather than the full shell/run machinery.
func connectedClient(timeout time.Duration) (*transport.Client, error) {
	cfg, err := loadGhost()
	if err != nil {
		return nil, err
	}
	routeLogsToFile(cfg)
	if err := ensureAuthenticated(cfg); err != nil {
		return nil, err
	}
	client := newClient(cfg)
	if err := waitOnline(client, timeout); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("not connected to %s (%v) — see %s", cfg.Source.Host, err, config.LogPath())
	}
	return client, nil
}

// openGhostJournal opens the write-through journal for the ghost side. (The
// full-tree metadata index is owned by the FUSE backend and persisted as a
// snapshot — no separate SQLite store anymore.)
func openGhostJournal() (*fuse.Journal, error) {
	return fuse.OpenJournal(config.JournalPath())
}

// buildMatcher loads ignore patterns from config + ~/.lg or repo .lgignore.
func buildMatcher(c *config.Config) (*config.Matcher, error) {
	// .lgignore lives at the mount root; merge with config.ignore.
	return config.LoadIgnoreFile(c.Ignore, c.LocalRoot+"/.lgignore")
}
