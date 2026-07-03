package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/spf13/cobra"
)

// field is one settable config key, mapped to typed get/set on the Config
// struct. Using an explicit table (rather than reflection) keeps `set` safe:
// it only ever touches the named key and validates types.
type field struct {
	get func(*config.Config) string
	set func(*config.Config, string) error
}

func setInt(p *int) func(*config.Config, string) error {
	return func(_ *config.Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		*p = n
		return nil
	}
}

// fields are the scalar keys `lg config set` understands. List-valued keys
// (ignore, trigger patterns) are edited via `lg config edit`.
func configFields(c *config.Config) map[string]field {
	return map[string]field{
		"role":                           {func(c *config.Config) string { return string(c.Role) }, func(c *config.Config, v string) error { c.Role = config.Role(v); return nil }},
		"source.host":                    {func(c *config.Config) string { return c.Source.Host }, func(c *config.Config, v string) error { c.Source.Host = v; return nil }},
		"source.remote_root":             {func(c *config.Config) string { return c.Source.RemoteRoot }, func(c *config.Config, v string) error { c.Source.RemoteRoot = v; return nil }},
		"source.user":                    {func(c *config.Config) string { return c.Source.User }, func(c *config.Config, v string) error { c.Source.User = v; return nil }},
		"source.port":                    {func(c *config.Config) string { return strconv.Itoa(c.Source.Port) }, setInt(&c.Source.Port)},
		"source.agent_bin":               {func(c *config.Config) string { return c.Source.AgentBin }, func(c *config.Config, v string) error { c.Source.AgentBin = v; return nil }},
		"source.ssh_mode":                {func(c *config.Config) string { return c.Source.SSHMode }, func(c *config.Config, v string) error { c.Source.SSHMode = v; return nil }},
		"source.auth":                    {func(c *config.Config) string { return c.Source.Auth }, func(c *config.Config, v string) error { c.Source.Auth = v; return nil }},
		"source.control_persist":         {func(c *config.Config) string { return c.Source.ControlPersist }, func(c *config.Config, v string) error { c.Source.ControlPersist = v; return nil }},
		"local_root":                     {func(c *config.Config) string { return c.LocalRoot }, func(c *config.Config, v string) error { c.LocalRoot = v; return nil }},
		"cache.evict_after_idle_minutes": {func(c *config.Config) string { return strconv.Itoa(c.Cache.EvictAfterIdleMinutes) }, setInt(&c.Cache.EvictAfterIdleMinutes)},
		"cache.max_cache_size_gb":        {func(c *config.Config) string { return strconv.Itoa(c.Cache.MaxCacheSizeGB) }, setInt(&c.Cache.MaxCacheSizeGB)},
		"default_target":                 {func(c *config.Config) string { return c.DefaultTarget }, func(c *config.Config, v string) error { c.DefaultTarget = v; return nil }},
		"log_level":                      {func(c *config.Config) string { return c.LogLevel }, func(c *config.Config, v string) error { c.LogLevel = v; return nil }},
	}
}

func sortedKeys(c *config.Config) []string {
	keys := make([]string, 0)
	for k := range configFields(c) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and change lg config (~/.lg/config.yaml)",
	}
	cmd.AddCommand(newConfigGetCmd(), newConfigSetCmd(), newConfigEditCmd(), newConfigShowCmd(), newConfigPathCmd())
	return cmd
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print a single config value (e.g. source.host)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			f, ok := configFields(c)[args[0]]
			if !ok {
				return unknownKey(c, args[0])
			}
			fmt.Println(f.get(c))
			return nil
		},
	}
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change one config value, leaving everything else intact",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			c, err := config.Load()
			if err != nil {
				return err
			}
			f, ok := configFields(c)[key]
			if !ok {
				return unknownKey(c, key)
			}
			old := f.get(c)
			if err := f.set(c, value); err != nil {
				return fmt.Errorf("invalid value for %s: %w", key, err)
			}
			// Validate the whole config before persisting, so a bad edit never
			// lands on disk.
			if err := c.Validate(); err != nil {
				return fmt.Errorf("would make config invalid: %w", err)
			}
			if err := c.Save(); err != nil {
				return err
			}
			fmt.Printf("%s: %s -> %s\n", key, old, f.get(c))
			return nil
		},
	}
}

func newConfigEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open the config in $EDITOR (validates on save)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.Path()
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("no config at %s — run 'lg init' first", path)
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			ed := exec.Command(editor, path)
			ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := ed.Run(); err != nil {
				return err
			}
			// Re-validate after editing; warn but don't auto-revert.
			if _, err := config.Load(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: config has problems: %v\n", err)
			}
			return nil
		},
	}
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the whole config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(config.Path())
			if err != nil {
				return err
			}
			fmt.Print(string(b))
			return nil
		},
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the active project's config file path",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !config.Exists() {
				return config.ErrNotSetUp
			}
			fmt.Println(config.Path())
			return nil
		},
	}
}

func unknownKey(c *config.Config, key string) error {
	return fmt.Errorf("unknown key %q\nsettable keys:\n  %s\n(for lists like ignore/patterns, use 'lg config edit')",
		key, strings.Join(sortedKeys(c), "\n  "))
}
