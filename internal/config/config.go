// Package config owns config.yaml parsing plus the three cross-cutting helpers
// that must have a single implementation: path mapping
// (paths.go), the ignore matcher (ignore.go), and the config schema below.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// Role identifies which half of the system this binary instance is acting as.
type Role string

const (
	RoleGhost  Role = "ghost"
	RoleSource Role = "source"
)

// Config is the parsed ~/.lg/config.yaml.
type Config struct {
	Role Role `yaml:"role"`

	Source struct {
		Host       string `yaml:"host"`        // ssh target (native ssh; host kept as-is)
		RemoteRoot string `yaml:"remote_root"` // absolute path of the repo on Source
		User       string `yaml:"user"`        // optional; defaults to $USER
		Port       int    `yaml:"port"`        // optional; defaults to 22
		AgentBin   string `yaml:"agent_bin"`   // path to `lg` on Source; defaults to "lg"
		// SSHMode selects the transport:
		//   "system" (default): shell out to the `ssh` binary, so ~/.ssh/config
		//     (Host aliases, ProxyJump, Duo/ControlMaster, known_hosts) all apply.
		//     This is what makes lab/bastion/2FA hosts work.
		//   "native": use the built-in Go ssh client (ignores ~/.ssh/config).
		SSHMode string `yaml:"ssh_mode"`
		// Auth selects how to authenticate:
		//   "" (default): ssh key / agent (system or native ssh). On a system-mode
		//     host that prompts interactively (password and/or Duo/2FA), `lg
		//     connect` shows the prompt once and the cached master carries every
		//     later connection.
		//   "password": use the password stored (encrypted) in .lg/credentials.
		//     With ssh_mode: native (what `lg init` picks for a host with no
		//     second factor) the built-in Go client answers the prompt itself.
		//     With ssh_mode: system (the Duo/OTP setup) `lg connect` auto-fills
		//     the password into ssh via SSH_ASKPASS — the user only answers the
		//     Duo step, once per control_persist window.
		Auth string `yaml:"auth"`
		// ControlPersist is how long lg's own ssh master (system mode only) stays
		// alive after the last connection closes — the window in which no auth
		// prompt reappears. ssh duration syntax ("8h", "30m") or "max": no timer
		// at all, alive until the link actually drops (reboot, network death).
		// `lg init` picks "max" for a second-auth host so a human answers Duo as
		// rarely as possible. Default "8h".
		ControlPersist string `yaml:"control_persist"`
	} `yaml:"source"`

	LocalRoot string `yaml:"local_root"` // absolute path of the FUSE mount on Ghost

	Ignore []string `yaml:"ignore"` // .gitignore-style patterns (also merged with .lgignore)

	// AutoRemoteCommands are commands that, typed in `lg shell` WITHOUT the `lg`
	// prefix, auto-run on Source (as if `lg <cmd>`), falling back to the local
	// command if Source is unreachable. Matched on the first word (basename).
	// Set to an explicit empty list ([]) to disable; absent uses the default set.
	AutoRemoteCommands []string `yaml:"auto_remote_commands"`

	Cache struct {
		EvictAfterIdleMinutes int `yaml:"evict_after_idle_minutes"`
		MaxCacheSizeGB        int `yaml:"max_cache_size_gb"`
	} `yaml:"cache"`

	// DefaultTarget: where `lg shell` starts. "source" starts with toggle mode on
	// (every command runs on Source); "local" (default) starts as a normal local
	// shell. `lg toggle` flips it at any time.
	DefaultTarget string `yaml:"default_target"`

	LogLevel string `yaml:"log_level"` // debug|info|warn|error (default info)
}

// lg is project-local: config + all per-project state live in a `.lg/` dir at
// the top of each project (like `.git/`). Commands discover the nearest one by
// walking up from the current directory. `$LG_HOME` overrides discovery
// entirely (used by tests and as an escape hatch).

var (
	dirOnce sync.Once
	dirVal  string // resolved active .lg dir for this process ("" = no project)
)

// findProjectDir walks up from start looking for a directory that contains
// `.lg/config.yaml`, and returns that `.lg` path. Like git's `.git` discovery.
func findProjectDir(start string) (string, bool) {
	dir := start
	for {
		lg := filepath.Join(dir, ".lg")
		if st, err := os.Stat(filepath.Join(lg, "config.yaml")); err == nil && !st.IsDir() {
			return lg, true
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached the filesystem root
			return "", false
		}
		dir = parent
	}
}

// Dir returns the active project's `.lg` directory for this process: `$LG_HOME`
// if set, else the nearest `.lg/` discovered by walking up from the cwd, else ""
// (no project here). Resolved once and cached — a single lg invocation has one
// cwd, and the long-lived `lg shell` keeps the dir it was launched from.
func Dir() string {
	// $LG_HOME is read fresh every call (tests set it per-case; never cache it).
	if h := os.Getenv("LG_HOME"); h != "" {
		return h
	}
	// The walk-up discovery is cached: a process has one cwd, and `lg shell`
	// keeps the directory it was launched from.
	dirOnce.Do(func() {
		if cwd, err := os.Getwd(); err == nil {
			if d, ok := findProjectDir(cwd); ok {
				dirVal = d
			}
		}
	})
	return dirVal
}

// InitDir returns where `lg init` should create a fresh project's `.lg` dir:
// `$LG_HOME` if set, else `<cwd>/.lg`. It deliberately bypasses discovery (which
// would find nothing before init runs).
func InitDir() string {
	if h := os.Getenv("LG_HOME"); h != "" {
		return h
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return filepath.Join(cwd, ".lg")
}

// Path returns the active project's config file path.
func Path() string { return filepath.Join(Dir(), "config.yaml") }

// JournalPath is the append-only write journal.
func JournalPath() string { return filepath.Join(Dir(), "journal.log") }

// CacheDir holds materialized (lazily fetched) file content.
func CacheDir() string { return filepath.Join(Dir(), "cache") }

// LogPath is where long-running commands (shell, run) write their logs, so
// background reconnect noise never spams the user's terminal.
func LogPath() string { return filepath.Join(Dir(), "lg.log") }

// Exists reports whether the cwd is inside an lg project (a discoverable `.lg/
// config.yaml`).
func Exists() bool {
	d := Dir()
	if d == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(d, "config.yaml"))
	return err == nil
}

// ErrNotSetUp is returned when the cwd is not inside an lg project.
var ErrNotSetUp = errNotSetUp{}

type errNotSetUp struct{}

func (errNotSetUp) Error() string {
	return "not an lg project — run 'lg init' in your project directory to set one up"
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
	if c.Source.SSHMode == "" {
		c.Source.SSHMode = "system"
	}
	if c.Source.ControlPersist == "" {
		c.Source.ControlPersist = "8h"
	}
	if c.Cache.EvictAfterIdleMinutes == 0 {
		c.Cache.EvictAfterIdleMinutes = 30
	}
	if c.Cache.MaxCacheSizeGB == 0 {
		c.Cache.MaxCacheSizeGB = 10
	}
	if c.AutoRemoteCommands == nil { // nil = absent; explicit [] disables
		c.AutoRemoteCommands = []string{
			"ls", "cat", "tree", "head", "tail", "less", "grep", "find", "stat", "wc", "file",
		}
	}
	if c.DefaultTarget == "" {
		c.DefaultTarget = "local"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
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
