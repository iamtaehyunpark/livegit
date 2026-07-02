package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/agentbin"
	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/docs"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newInitCmd() *cobra.Command {
	var role, host, remoteRoot, user, auth string
	var port int
	var yes, forceInteractive bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up lg for the current directory — writes ./.lg/config.yaml",
		Long: `Set up lg for a project.

lg is project-local: this writes a config into a .lg/ directory in your CURRENT
directory (like git's .git/). Any lg command you later run from this directory
(or a subdirectory) auto-discovers and uses it.

Run with no flags for a guided, step-by-step setup:

    cd ~/myproject
    lg init

Or pass flags to skip the prompts (useful for scripts):

    lg init --role ghost --host gpu-1 --remote-root /home/u/proj

The remote tree mounts at a folder named exactly after the Source repo (the
basename of --remote-root), next to .lg/. There is no mount-name option — the
local mirror always carries the repo's own name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			in := &wizardInput{
				role: role, host: host, remoteRoot: remoteRoot,
				user: user, port: port, auth: auth,
			}
			// Interactive when forced, or on a TTY without full flag input.
			interactive := forceInteractive || (term.IsTerminal(int(os.Stdin.Fd())) && !in.complete())
			if interactive {
				if err := runWizard(in); err != nil {
					return err
				}
			}
			// Password (for --auth password) is never taken on the command line;
			// prompt for it (hidden) whenever password auth is chosen but unset.
			if in.auth == "password" && in.password == "" {
				target := in.host
				if in.user != "" && !strings.Contains(target, "@") {
					target = in.user + "@" + target
				}
				pw, err := askPassword("SSH password for " + target)
				if err != nil {
					return err
				}
				in.password = pw
			}
			return writeConfig(in, yes || !interactive)
		},
	}
	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&role, "role", "", "ghost|source")
	cmd.Flags().StringVar(&host, "host", "", "Source ssh host (ghost role)")
	cmd.Flags().StringVar(&remoteRoot, "remote-root", "", "absolute repo path on Source")
	cmd.Flags().StringVar(&user, "user", "", "ssh user (default $USER)")
	cmd.Flags().StringVar(&auth, "auth", "", "auth method: '' (ssh key/agent) or 'password' (prompts, stored encrypted)")
	cmd.Flags().IntVar(&port, "port", 0, "ssh port (default 22)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompts")
	cmd.Flags().BoolVarP(&forceInteractive, "interactive", "i", false, "force the step-by-step wizard")
	_ = home
	return cmd
}

// wizardInput collects answers (from flags and/or prompts). The local mount
// point is NOT an input: it's always derived as a sibling of .lg/ named exactly
// after the Source repo (basename of remote_root).
type wizardInput struct {
	role       string
	host       string
	remoteRoot string
	user       string
	port       int
	auth       string // "" or "password"
	password   string // collected in-memory only; stored encrypted, never in argv
}

// complete reports whether enough was supplied via flags to skip prompting.
func (w *wizardInput) complete() bool {
	switch config.Role(w.role) {
	case config.RoleGhost:
		return w.host != "" && w.remoteRoot != ""
	case config.RoleSource:
		return w.remoteRoot != ""
	default:
		return false
	}
}

func runWizard(in *wizardInput) error {
	r := bufio.NewReader(os.Stdin)
	fmt.Println("Welcome to lg! Let's set things up. (press Enter to accept [defaults])")
	fmt.Println()

	in.role = choose(r, "Is this machine your laptop (ghost) or the server (source)?",
		[]string{"ghost", "source"}, orDefault(in.role, "ghost"))

	if config.Role(in.role) == config.RoleSource {
		in.remoteRoot = ask(r, "Absolute path of the repo on this server", in.remoteRoot, true)
		return nil
	}

	// Ghost.
	fmt.Println()
	fmt.Println("Now the server (Source) you'll connect to:")
	in.host = ask(r, "  SSH host or alias (e.g. gpu-1 or user@1.2.3.4)", in.host, true)
	in.remoteRoot = ask(r, "  Absolute repo path on the server", in.remoteRoot, true)

	curUser := in.user
	if curUser == "" {
		curUser = os.Getenv("USER")
	}
	in.user = ask(r, "  SSH user", curUser, false)
	in.port = askInt(r, "  SSH port", orInt(in.port, 22))

	// Auth: default to ssh key/agent; if the host needs a password, collect it
	// (hidden) and store it encrypted. Skip if --auth already chose the method.
	if in.auth == "" {
		fmt.Println()
		fmt.Println("Auth: leave the password blank to use your ssh key/agent (recommended).")
		fmt.Println("Only enter a password if this host requires one and your key isn't set up.")
		pw, err := askPassword("  SSH password (blank = ssh key/agent)")
		if err != nil {
			return err
		}
		if pw != "" {
			in.auth = "password"
			in.password = pw
		}
	}

	fmt.Println()
	fmt.Printf("The remote tree will mount at:  %s\n", mountFor(in.remoteRoot))
	fmt.Println("(a folder named after the repo, next to this project's .lg/ config)")
	return nil
}

// mountFor is the local mount point for a project: a sibling of `.lg/` named
// exactly after the Source repo (basename of remote_root). There is no choice
// here — the local mirror always carries the repo's own name.
func mountFor(remoteRoot string) string {
	name := filepath.Base(strings.TrimRight(remoteRoot, "/"))
	return filepath.Join(filepath.Dir(config.InitDir()), name)
}

func writeConfig(in *wizardInput, skipConfirm bool) error {
	if config.Role(in.role) != config.RoleGhost && config.Role(in.role) != config.RoleSource {
		return fmt.Errorf("role must be 'ghost' or 'source' (got %q)", in.role)
	}

	c := &config.Config{Role: config.Role(in.role)}
	c.Source.Host = in.host
	c.Source.RemoteRoot = in.remoteRoot
	c.Source.User = in.user
	c.Source.Port = in.port
	c.Source.RemoteRoot = strings.TrimRight(c.Source.RemoteRoot, "/")
	c.Source.Auth = in.auth
	if in.auth == "password" {
		c.Source.SSHMode = "native" // system `ssh` can't answer a prompt non-interactively
	}
	if config.Role(in.role) == config.RoleGhost {
		// Mount dir is always named after the Source repo (no selection).
		c.LocalRoot = mountFor(c.Source.RemoteRoot)
	}

	// Sensible defaults so a fresh config is immediately usable.
	c.Ignore = []string{".DS_Store", ".venv/", "node_modules/", "*.pt"}
	c.Cache.EvictAfterIdleMinutes = 30
	c.Cache.MaxCacheSizeGB = 10

	if err := c.Validate(); err != nil {
		return err
	}

	// lg init always targets a `.lg/` in the CURRENT directory (it bypasses the
	// walk-up discovery, which would find a parent project or nothing).
	initDir := config.InitDir()
	cfgPath := filepath.Join(initDir, "config.yaml")

	// Summary + confirmation.
	if !skipConfirm {
		fmt.Println()
		fmt.Println("Here's what I'll write:")
		printSummary(c)
		if _, err := os.Stat(cfgPath); err == nil {
			fmt.Printf("\n%s already exists and will be overwritten.\n", cfgPath)
		}
		if !confirm(bufio.NewReader(os.Stdin), "Write this config?", true) {
			fmt.Println("aborted — nothing written.")
			return nil
		}
	}

	if err := os.MkdirAll(initDir, 0o755); err != nil {
		return err
	}
	if c.Role == config.RoleGhost {
		_ = os.MkdirAll(filepath.Join(initDir, "cache"), 0o755)
		_ = os.MkdirAll(c.LocalRoot, 0o755)
	}
	if err := c.SaveTo(cfgPath); err != nil {
		return err
	}
	fmt.Printf("\n✓ wrote %s\n", cfgPath)

	// Drop the guides into the project root (next to .lg/) so any CLI or coding
	// agent working here references them natively. Don't clobber a copy the user
	// may have edited — only write when absent.
	projectRoot := filepath.Dir(initDir)
	for _, d := range docs.Files() {
		dst := filepath.Join(projectRoot, d.Name)
		if _, err := os.Stat(dst); err == nil {
			continue // already there — leave the user's copy alone
		}
		if err := os.WriteFile(dst, d.Content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "lg: warning: couldn't write %s: %v\n", dst, err)
			continue
		}
		fmt.Printf("✓ wrote %s\n", dst)
	}

	if c.Role == config.RoleSource {
		fmt.Println("\nThis machine is the Source. Make sure `lg` is on its PATH;")
		fmt.Println("your laptop launches the agent over ssh automatically.")
		return nil
	}

	// Store the password (encrypted) so lg can authenticate on its own.
	if c.Source.Auth == "password" {
		if err := config.SavePassword(in.password); err != nil {
			fmt.Fprintf(os.Stderr, "lg: warning: couldn't store the password: %v\n", err)
		} else {
			fmt.Printf("✓ stored password (encrypted) in %s\n", config.CredentialsPath())
		}
	}

	// Authenticate the ssh connection first (system mode). On a Duo/2FA host this
	// shows the prompt now, while we have the terminal, and caches the connection
	// so the agent check below — and every later lg command — reuses it without a
	// second prompt.
	if c.Source.SSHMode != "native" && c.Source.Auth != "password" {
		fmt.Printf("→ connecting to %s (approve the Duo/2FA prompt if one appears) …\n", sshTarget(c))
		if err := transport.Connect(c); err != nil {
			fmt.Fprintf(os.Stderr, "lg: couldn't connect (%v)\n", err)
			fmt.Fprintf(os.Stderr, "    you can retry later with `lg connect`, then `lg init` to finish agent setup.\n")
		}
	}

	// Confirm/deploy the agent on the remote (best-effort — a host needing an
	// interactive step, e.g. Duo, may not be reachable non-interactively here).
	fmt.Printf("→ checking the agent on %s …\n", sshTarget(c))
	if msg, err := transport.EnsureAgent(c, agentbin.Pick); err != nil {
		fmt.Fprintf(os.Stderr, "lg: couldn't verify/deploy the agent automatically (%v)\n", err)
		fmt.Fprintf(os.Stderr, "    ensure `lg` exists at ~/.local/bin/lg on Source (or run `lg init` again once ssh works).\n")
	} else {
		fmt.Printf("✓ %s\n", msg)
	}

	fmt.Println("\nStart working:  lg shell")
	fmt.Println("Change settings later with:  lg config set <key> <value>")
	return nil
}

func printSummary(c *config.Config) {
	fmt.Printf("  role:        %s\n", c.Role)
	if c.Role == config.RoleGhost {
		fmt.Printf("  server:      %s\n", sshTarget(c))
		fmt.Printf("  remote repo: %s\n", c.Source.RemoteRoot)
		fmt.Printf("  local mount: %s\n", c.LocalRoot)
		if c.Source.Auth == "password" {
			fmt.Printf("  auth:        password (stored encrypted, native ssh)\n")
		}
	} else {
		fmt.Printf("  remote repo: %s\n", c.Source.RemoteRoot)
	}
}

func sshTarget(c *config.Config) string {
	host := c.Source.Host
	if c.Source.User != "" && !strings.Contains(host, "@") {
		host = c.Source.User + "@" + host
	}
	if c.Source.Port != 0 && c.Source.Port != 22 {
		host += fmt.Sprintf(" -p %d", c.Source.Port)
	}
	return host
}

// --- prompt helpers ---

// askPassword reads a secret without echoing it. Falls back to a plain line read
// when stdin isn't a terminal (e.g. piped input in scripts).
func askPassword(prompt string) (string, error) {
	fmt.Printf("%s: ", prompt)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), nil
	}
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return string(b), err
}

func ask(r *bufio.Reader, prompt, def string, required bool) string {
	for {
		if def != "" {
			fmt.Printf("%s [%s]: ", prompt, def)
		} else {
			fmt.Printf("%s: ", prompt)
		}
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			line = def
		}
		if line == "" && required {
			fmt.Println("  (required)")
			continue
		}
		return line
	}
}

func askInt(r *bufio.Reader, prompt string, def int) int {
	for {
		fmt.Printf("%s [%d]: ", prompt, def)
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			fmt.Println("  (enter a number)")
			continue
		}
		return n
	}
}

func choose(r *bufio.Reader, prompt string, options []string, def string) string {
	for {
		fmt.Printf("%s (%s) [%s]: ", prompt, strings.Join(options, "/"), def)
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		if line == "" {
			return def
		}
		for _, o := range options {
			if line == o {
				return o
			}
		}
		fmt.Printf("  (choose one of: %s)\n", strings.Join(options, ", "))
	}
}

func confirm(r *bufio.Reader, prompt string, def bool) bool {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Printf("%s [%s]: ", prompt, hint)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
