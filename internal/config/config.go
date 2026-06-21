// Package config owns config.yaml parsing plus the three cross-cutting helpers
// flagged in spec §7 that must have a single implementation: path mapping
// (paths.go), the ignore matcher (ignore.go), and the config schema below.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Role identifies which half of the system this binary instance is acting as.
type Role string

const (
	RoleGhost  Role = "ghost"
	RoleSource Role = "source"
)

// Config is the parsed ~/.lg/config.yaml (spec §8).
type Config struct {
	Role Role `yaml:"role"`

	Source struct {
		Host       string `yaml:"host"`        // ssh target (D1: native ssh, host kept directly)
		RemoteRoot string `yaml:"remote_root"` // absolute path of the repo on Source
		User       string `yaml:"user"`        // optional; defaults to $USER
		Port       int    `yaml:"port"`        // optional; defaults to 22
		AgentBin   string `yaml:"agent_bin"`   // path to `lg` on Source; defaults to "lg"
	} `yaml:"source"`

	LocalRoot string `yaml:"local_root"` // absolute path of the FUSE mount on Ghost

	Ignore []string `yaml:"ignore"` // .gitignore-style patterns (also merged with .lgignore)

	Cache struct {
		EvictAfterIdleMinutes int `yaml:"evict_after_idle_minutes"`
		MaxCacheSizeGB        int `yaml:"max_cache_size_gb"`
	} `yaml:"cache"`

	SourceTriggers struct {
		Patterns             []string          `yaml:"patterns"`
		ExitCommandMap       map[string]string `yaml:"exit_command_map"`
		DirectoryMarkers     []string          `yaml:"directory_markers"`
		AlwaysSourcePatterns []string          `yaml:"always_source_patterns"`
	} `yaml:"source_triggers"`

	Offline struct {
		OnSourceTrigger string `yaml:"on_source_trigger"` // "queue" | "error"
	} `yaml:"offline"`

	ReadonlyCommands []string `yaml:"readonly_commands"`

	LogLevel string `yaml:"log_level"` // debug|info|warn|error (default info)
}

// Dir returns the lg home directory (~/.lg), honoring $LG_HOME for tests.
func Dir() string {
	if h := os.Getenv("LG_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lg")
}

// Path returns the default config file path.
func Path() string { return filepath.Join(Dir(), "config.yaml") }

// StateDBPath is the SQLite metadata db (spec §4.1).
func StateDBPath() string { return filepath.Join(Dir(), "state.db") }

// JournalPath is the append-only write journal (spec §4.5).
func JournalPath() string { return filepath.Join(Dir(), "journal.log") }

// CacheDir holds materialized file content for cached/live files.
func CacheDir() string { return filepath.Join(Dir(), "cache") }

// ConflictsPath records detected conflicts for `lg status` (§4.4).
func ConflictsPath() string { return filepath.Join(Dir(), "conflicts.log") }

// Exists reports whether a config file is present at the default path.
func Exists() bool {
	_, err := os.Stat(Path())
	return err == nil
}

// ErrNotSetUp is returned when no config exists yet, with a friendly hint.
var ErrNotSetUp = errNotSetUp{}

type errNotSetUp struct{}

func (errNotSetUp) Error() string {
	return "lg isn't set up yet — run 'lg init' to get started"
}

// Load reads and validates the config at the default path.
func Load() (*Config, error) {
	if !Exists() {
		return nil, ErrNotSetUp
	}
	return LoadFrom(Path())
}

// LoadFrom reads and validates a config at an explicit path.
func LoadFrom(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Source.Port == 0 {
		c.Source.Port = 22
	}
	if c.Source.User == "" {
		c.Source.User = os.Getenv("USER")
	}
	if c.Source.AgentBin == "" {
		c.Source.AgentBin = "lg"
	}
	if c.Cache.EvictAfterIdleMinutes == 0 {
		c.Cache.EvictAfterIdleMinutes = 30
	}
	if c.Cache.MaxCacheSizeGB == 0 {
		c.Cache.MaxCacheSizeGB = 10
	}
	if c.Offline.OnSourceTrigger == "" {
		c.Offline.OnSourceTrigger = "queue"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if len(c.ReadonlyCommands) == 0 {
		c.ReadonlyCommands = []string{"cat", "head", "tail", "less", "grep", "find", "ls"}
	}
}

// Validate enforces the invariants the rest of the system relies on.
func (c *Config) Validate() error {
	switch c.Role {
	case RoleGhost, RoleSource:
	default:
		return fmt.Errorf("role must be 'ghost' or 'source', got %q", c.Role)
	}
	if c.Role == RoleGhost {
		if c.LocalRoot == "" {
			return fmt.Errorf("ghost role requires local_root")
		}
		if !filepath.IsAbs(c.LocalRoot) {
			return fmt.Errorf("local_root must be absolute: %q", c.LocalRoot)
		}
		if c.Source.Host == "" {
			return fmt.Errorf("ghost role requires source.host")
		}
		if c.Source.RemoteRoot == "" || !filepath.IsAbs(c.Source.RemoteRoot) {
			return fmt.Errorf("source.remote_root must be an absolute path")
		}
	}
	if c.Offline.OnSourceTrigger != "queue" && c.Offline.OnSourceTrigger != "error" {
		return fmt.Errorf("offline.on_source_trigger must be 'queue' or 'error'")
	}
	return nil
}

// Save writes the config to the default path, creating ~/.lg if needed.
func (c *Config) Save() error { return c.SaveTo(Path()) }

// SaveTo writes the config to an explicit path.
func (c *Config) SaveTo(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
