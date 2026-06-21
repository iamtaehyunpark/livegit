package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
)

func newInitCmd() *cobra.Command {
	var role, host, remoteRoot, localRoot string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize lg config (~/.lg/config.yaml)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if config.Role(role) != config.RoleGhost && config.Role(role) != config.RoleSource {
				return fmt.Errorf("--role must be 'ghost' or 'source'")
			}
			c := &config.Config{Role: config.Role(role)}
			c.Source.Host = host
			c.Source.RemoteRoot = remoteRoot
			c.LocalRoot = localRoot

			// Sensible defaults so a fresh config is immediately usable (§8).
			c.Ignore = []string{".venv/", "node_modules/", "*.pt"}
			c.Cache.EvictAfterIdleMinutes = 30
			c.Cache.MaxCacheSizeGB = 10
			c.SourceTriggers.Patterns = []string{
				"^conda activate", "^source .*/bin/activate", "^poetry shell", "^pyenv activate",
			}
			c.SourceTriggers.ExitCommandMap = map[string]string{
				"^conda activate":         "conda deactivate",
				"^source .*/bin/activate": "deactivate",
				"^poetry shell":           "exit",
			}
			c.SourceTriggers.DirectoryMarkers = []string{".venv", "node_modules", "Pipfile.lock", "poetry.lock"}
			c.SourceTriggers.AlwaysSourcePatterns = []string{"^python "}
			c.Offline.OnSourceTrigger = "queue"
			c.ReadonlyCommands = []string{"cat", "head", "tail", "less", "grep", "find", "ls"}

			if err := c.Validate(); err != nil {
				return err
			}
			if err := os.MkdirAll(config.Dir(), 0o755); err != nil {
				return err
			}
			if config.Role(role) == config.RoleGhost {
				if err := os.MkdirAll(config.CacheDir(), 0o755); err != nil {
					return err
				}
				if err := os.MkdirAll(c.LocalRoot, 0o755); err != nil {
					return err
				}
			}
			if err := c.Save(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "wrote %s (role=%s)\n", config.Path(), role)
			if config.Role(role) == config.RoleGhost {
				fmt.Fprintf(os.Stdout, "mount point: %s\nrun 'lg shell' to start.\n", c.LocalRoot)
			}
			return nil
		},
	}
	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&role, "role", "", "ghost|source (required)")
	cmd.Flags().StringVar(&host, "host", "", "Source ssh host (ghost role)")
	cmd.Flags().StringVar(&remoteRoot, "remote-root", "", "absolute repo path on Source")
	cmd.Flags().StringVar(&localRoot, "local-root", filepath.Join(home, "lg-mount"), "Ghost FUSE mount point")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}
