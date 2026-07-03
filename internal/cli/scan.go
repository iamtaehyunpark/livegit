package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

// newScanCmd finds every lg project on the machine (regardless of the shell's
// location) and reports each one's Source host and connection state. lg keeps no
// global registry — it's project-local like git — so this is a bounded
// filesystem walk from a root (default $HOME), skipping dotdirs and known noise.
func newScanCmd() *cobra.Command {
	var doConnect bool
	var maxDepth int
	c := &cobra.Command{
		Use:   "scan [root]",
		Short: "Find every lg project on this machine and show its connection state",
		Long: `Walk the filesystem for lg projects and report each one's Source and
connection state — a machine-wide view that doesn't depend on your cwd.

lg has no global registry (it's project-local like git), so this is a bounded
walk from a root directory (default: your home directory), skipping dotdirs,
node_modules, and ~/Library. Increase --max-depth for deeply nested projects.

With --connect, it establishes the ssh connection for every reachable
system-mode project (each may prompt for Duo/2FA once).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, _ := os.UserHomeDir()
			if len(args) == 1 {
				root = args[0]
			}
			abs, err := filepath.Abs(root)
			if err != nil {
				return err
			}
			configs := findProjectConfigs(abs, maxDepth)
			if len(configs) == 0 {
				fmt.Printf("no lg projects found under %s (try a different root, or raise --max-depth)\n", shortHome(abs))
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tROLE\tSOURCE\tMODE\tCONNECTION")
			for _, cfgPath := range configs {
				proj := shortHome(filepath.Dir(filepath.Dir(cfgPath))) // parent of .lg/
				cfg, err := config.LoadFrom(cfgPath)
				if err != nil {
					fmt.Fprintf(tw, "%s\t?\t?\t?\tinvalid config (%v)\n", proj, err)
					continue
				}
				role, host, mode, state := describeProject(cfg, doConnect)
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", proj, role, host, mode, state)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&doConnect, "connect", false, "bring up the connection for each reachable system-mode project (may prompt for Duo/2FA)")
	c.Flags().IntVar(&maxDepth, "max-depth", 6, "directory levels below root to search")
	return c
}

// describeProject returns the columns for one project. When connect is set and a
// system-mode project's master is down, it tries to establish it (interactive).
func describeProject(cfg *config.Config, connect bool) (role, host, mode, state string) {
	role = string(cfg.Role)
	if cfg.Role != config.RoleGhost {
		return role, "—", "—", "(no client connection)"
	}
	host = cfg.Source.Host
	if cfg.Source.SSHMode == "native" {
		mode = "native"
		if cfg.Source.Auth == "password" {
			mode = "native/password"
		}
		return role, host, mode, "n/a (per-command auth)"
	}
	// system mode: report (and optionally establish) the cached ssh connection.
	mode = "system"
	if transport.MasterLive(cfg) {
		return role, host, mode, "live"
	}
	if !connect {
		return role, host, mode, "down"
	}
	if err := transport.Connect(cfg); err != nil {
		return role, host, mode, "down (connect failed: " + err.Error() + ")"
	}
	return role, host, mode, "connected"
}

// findProjectConfigs walks root (up to maxDepth levels deep) collecting every
// `.lg/config.yaml`. It never descends into a `.lg` dir, skips dotdirs and known
// heavy dirs, and ignores unreadable paths (e.g. a stale FUSE mount) rather than
// failing the whole scan.
func findProjectConfigs(root string, maxDepth int) []string {
	var out []string
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	noise := map[string]bool{"node_modules": true, "Library": true}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil // unreadable dir / stale mount / non-dir → skip quietly
		}
		depth := strings.Count(filepath.Clean(p), string(os.PathSeparator)) - rootDepth
		if depth > maxDepth {
			return fs.SkipDir
		}
		name := d.Name()
		if name == ".lg" {
			if st, e := os.Stat(filepath.Join(p, "config.yaml")); e == nil && !st.IsDir() {
				out = append(out, filepath.Join(p, "config.yaml"))
			}
			return fs.SkipDir // don't recurse into a project's own state dir
		}
		// Skip dotdirs (except .lg, handled above) and known heavy dirs. depth>0
		// so a root that itself starts with '.' (e.g. ~/.config) still gets walked.
		if depth > 0 && (strings.HasPrefix(name, ".") || noise[name]) {
			return fs.SkipDir
		}
		return nil
	})
	return out
}

// shortHome rewrites a leading $HOME with ~ for compact display.
func shortHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(os.PathSeparator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}
