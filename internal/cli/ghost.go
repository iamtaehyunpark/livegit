package cli

import (
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
	client := newClient(cfg)
	if err := waitOnline(client, timeout); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("not connected to %s (%v)", cfg.Source.Host, err)
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
