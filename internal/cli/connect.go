package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iamtaehyunpark/livegit/internal/agentbin"
	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

// newConnectCmd establishes lg's own ssh ControlMaster to Source. On a Duo/2FA
// host this is where the (single) prompt happens — the connection is then cached
// for `source.control_persist` (default 8h; "max" = until the link drops) and
// every later `lg` command reuses it without re-prompting. Idempotent: a no-op
// when a master is already live.
func newConnectCmd() *cobra.Command {
	var check, stop bool
	c := &cobra.Command{
		Use:   "connect",
		Short: "Authenticate to Source once (handles Duo/2FA), then reuse the connection",
		Long: `Open (and cache) the ssh connection to Source.

On a server with Duo/2FA, this is the one place the prompt appears: approve it
once and lg keeps the authenticated connection alive (source.control_persist —
default 8h, or 'max' to keep it until the link drops). Every later 'lg <cmd>',
'lg run', and 'lg shell' multiplexes over it with no second prompt.

You rarely need to run this by hand — 'lg <cmd>' and 'lg shell' auto-connect on
a terminal. Use it to pre-authenticate (e.g. before a scripted run or a coding
agent that can't answer a Duo prompt), or to check/reset the connection.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGhost()
			if err != nil {
				return err
			}

			if cfg.Source.SSHMode == "native" {
				switch {
				case stop:
					fmt.Println("native mode: no cached connection to close (each command authenticates itself).")
					return nil
				case check:
					fmt.Println("native mode: no cached connection — each command authenticates itself with the stored credentials.")
					fmt.Println("Run `lg connect` (no flags) to test that those credentials still work.")
					return nil
				}
				return establishConnection(cfg, false)
			}

			switch {
			case stop:
				return closeConnection(cfg)
			case check:
				if transport.MasterLive(cfg) {
					fmt.Printf("connection: live (cached; new commands to %s won't re-prompt)\n", cfg.Source.Host)
				} else {
					fmt.Printf("connection: down — run `lg connect` to authenticate to %s\n", cfg.Source.Host)
				}
				return nil
			}
			return establishConnection(cfg, false)
		},
	}
	c.Flags().BoolVar(&check, "check", false, "just report whether the connection is live")
	c.Flags().BoolVar(&stop, "stop", false, "close the cached connection (next command re-authenticates)")
	return c
}

// establishConnection is the shared body of `lg connect` and `lg refresh`.
// In native mode there is no cached connection — it tests the stored
// credentials instead (and detects a Duo/2FA host, which native mode cannot
// serve, printing how to switch). In system mode it brings the master up
// (showing any Duo prompt on the terminal); with force set it closes a live
// master first so the authentication — and its cached window — start fresh.
func establishConnection(cfg *config.Config, force bool) error {
	// Keep the project-root guides in step with this binary — the docs
	// counterpart of the agent auto-upgrade below. Purely local, so it runs
	// before (and regardless of) any authentication.
	syncProjectDocs(filepath.Dir(config.Dir()))

	if cfg.Source.SSHMode == "native" {
		fmt.Printf("Testing the stored credentials against %s …\n", cfg.Source.Host)
		if err := transport.VerifyNative(cfg); err != nil {
			if errors.Is(err, transport.ErrSecondAuth) {
				return err
			}
			return fmt.Errorf("credential test failed: %w", err)
		}
		fmt.Println("✓ credentials work. Native mode authenticates every command automatically —")
		fmt.Println("  there is no session to keep alive, so you're done.")
		checkAgent(cfg)
		return nil
	}

	if transport.MasterLive(cfg) {
		if !force {
			fmt.Printf("Already connected to %s (reusing the cached ssh connection).\n", cfg.Source.Host)
			return nil
		}
		if err := transport.StopMaster(cfg); err != nil {
			return fmt.Errorf("close old connection: %w", err)
		}
	}
	if cfg.Source.Auth == "password" {
		fmt.Printf("Connecting to %s — password auto-filled; approve the Duo/2FA prompt if one appears…\n", cfg.Source.Host)
	} else {
		fmt.Printf("Connecting to %s — answer the password/Duo prompt if one appears…\n", cfg.Source.Host)
	}
	if err := transport.Connect(cfg); err != nil {
		return err
	}
	fmt.Printf("✓ connected. Cached %s; further lg commands won't re-prompt.\n", transport.PersistLabel(cfg))

	// While the connection is fresh, make sure the agent is in place — this
	// makes `lg connect`/`lg refresh` the one recovery command after an init
	// that couldn't authenticate (no need to run the init wizard again).
	checkAgent(cfg)
	return nil
}

// closeConnection tears down the cached master (shared by `lg disconnect` and
// `lg connect --stop`).
func closeConnection(cfg *config.Config) error {
	if !transport.MasterLive(cfg) {
		fmt.Println("no cached connection to close.")
		return nil
	}
	if err := transport.StopMaster(cfg); err != nil {
		return fmt.Errorf("stop connection: %w", err)
	}
	fmt.Println("ssh connection closed — the next lg command will re-authenticate.")
	return nil
}

// newDisconnectCmd closes the cached ssh connection (the explicit sibling of
// `lg connect`; same effect as `lg connect --stop`).
func newDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Close the cached ssh connection to Source",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGhost()
			if err != nil {
				return err
			}
			if cfg.Source.SSHMode == "native" {
				fmt.Println("native mode: no cached connection to close (each command authenticates itself).")
				return nil
			}
			return closeConnection(cfg)
		},
	}
}

// newRefreshCmd re-authenticates the connection: it closes any live master and
// opens a new one, restarting the cached window from now (e.g. before an
// overnight run, so the window doesn't expire midway).
func newRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Re-authenticate the connection to Source (restarts the cached window)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGhost()
			if err != nil {
				return err
			}
			return establishConnection(cfg, true)
		},
	}
}

// checkAgent verifies (and if needed deploys/upgrades) the remote agent over
// the connection that was just authenticated. Best-effort: failures are
// reported but don't fail `lg connect` — the connection itself is up.
func checkAgent(cfg *config.Config) {
	if msg, err := transport.EnsureAgent(cfg, agentbin.Pick, Version); err != nil {
		fmt.Fprintf(os.Stderr, "lg: connected, but couldn't verify the agent (%v)\n", err)
		fmt.Fprintf(os.Stderr, "    ensure `lg` exists at ~/.local/bin/lg on Source.\n")
	} else {
		fmt.Printf("✓ %s\n", msg)
	}
}
