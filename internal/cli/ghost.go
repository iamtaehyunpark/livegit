package cli

import (
	"fmt"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/fuse"
	"github.com/taehyun/lg/internal/transport"
)

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

// openState opens the SQLite state store and journal for the ghost side.
func openGhostStores() (*fuse.StateStore, *fuse.Journal, error) {
	store, err := fuse.OpenState(config.StateDBPath())
	if err != nil {
		return nil, nil, err
	}
	journal, err := fuse.OpenJournal(config.JournalPath())
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return store, journal, nil
}

// buildMatcher loads ignore patterns from config + ~/.lg or repo .lgignore.
func buildMatcher(c *config.Config) (*config.Matcher, error) {
	// .lgignore lives at the mount root; merge with config.ignore.
	return config.LoadIgnoreFile(c.Ignore, c.LocalRoot+"/.lgignore")
}
