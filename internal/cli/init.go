package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/agentbin"
	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newInitCmd() *cobra.Command {
	var role, host, remoteRoot, user, auth string
	var port int
	var yes, forceInteractive, twoFactor bool
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
				user: user, port: port, auth: auth, twoFactor: twoFactor,
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
				pw, err := askPassword(bufio.NewReader(os.Stdin), "SSH password for "+target)
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
	cmd.Flags().BoolVar(&twoFactor, "two-factor", false, "host adds a Duo/OTP step: keep system ssh; `lg connect` auto-fills the stored password")
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
	twoFactor  bool   // host adds a Duo/OTP step: system mode + `lg connect` (with the stored password auto-filled, if any)
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

	// Auth: two independent facts — does ssh ask for a password (no key set
	// up), and does it add a second step (Duo push / OTP)? Any combination
	// works:
	//   password only       -> stored encrypted; native ssh answers it (zero typing)
	//   password + 2nd step -> stored encrypted; `lg connect` auto-fills it via
	//                          SSH_ASKPASS — you only approve the Duo prompt
	//   2nd step only       -> `lg connect` once per window (key + Duo)
	//   neither             -> ssh key/agent, nothing to store
	// Skip if --auth already chose the method.
	if in.auth == "" {
		fmt.Println()
		fmt.Println("Auth: answer 'n' to both questions to use your ssh key/agent (recommended).")
		if confirm(r, "Do you type a password when you ssh into this server (no key set up)?", false) {
			pw, err := askPassword(r, "  SSH password (stored encrypted, never in plaintext)")
			if err != nil {
				return err
			}
			if pw != "" {
				in.auth = "password"
				in.password = pw
			}
		}
		if confirm(r, "Does it also ask a second auth step (Duo push / passcode / OTP)?", false) {
			in.twoFactor = true
		}
		switch {
		case in.auth == "password" && in.twoFactor:
			fmt.Println("  OK — lg auto-fills the stored password when connecting; you only approve")
			fmt.Println("  the Duo prompt, once per cached window (10h).")
		case in.auth == "password":
			fmt.Println("  OK — lg stores it encrypted and logs in by itself (nothing to type).")
		case in.twoFactor:
			fmt.Println("  OK — you'll authenticate interactively ONCE with `lg connect` (password and/or")
			fmt.Println("  Duo as usual); lg caches that connection for 10h and every command reuses it.")
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
// mountFor is init-time twin of config.MountDir: the same sibling-of-.lg
// derivation, but anchored on InitDir (project discovery can't run before the
// .lg dir exists). Used only for display and to pre-create the mount dir —
// the path itself is never written to the config.
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
	if in.auth == "password" && !in.twoFactor {
		// Plain password host: the native client answers the prompt itself.
		// With a second factor the config stays in system mode — `lg connect`
		// auto-fills the stored password via SSH_ASKPASS and the user answers
		// only the Duo step; the cached master carries everything after.
		c.Source.SSHMode = "native"
	}
	if in.twoFactor {
		// Second-auth prompts are the expensive ones (a human must answer), so
		// give 2FA hosts a longer window: one Duo approval covers a full work
		// day. Capped at 10h rather than unbounded ("max" remains available via
		// `lg config set source.control_persist max`) so a forgotten laptop
		// doesn't hold an authenticated session open indefinitely.
		c.Source.ControlPersist = "10h"
	}
	// local_root is deliberately NOT written: the mountpoint is derived at
	// runtime (config.MountDir — a sibling of .lg/ named after the Source
	// repo), so moving the project later moves the mount with it. Writing an
	// absolute path here is what used to strand the mount at the init-time
	// location. `lg config set local_root <path>` still pins one explicitly.

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
		_ = os.MkdirAll(mountFor(c.Source.RemoteRoot), 0o755)
	}
	if err := c.SaveTo(cfgPath); err != nil {
		return err
	}
	fmt.Printf("\n✓ wrote %s\n", cfgPath)

	// Drop the guides into the project root (next to .lg/) so any CLI or coding
	// agent working here references them natively. Marker-gated (see docs.go):
	// lg-written copies refresh on upgrade, unmarked user files are left alone.
	projectRoot := filepath.Dir(initDir)
	syncProjectDocs(projectRoot)

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

	// Authenticate the ssh connection first (system mode). On a Duo/2FA (or
	// password) host this shows the prompt now, while we have the terminal, and
	// caches the connection so the agent check below — and every later lg
	// command — reuses it without a second prompt. Without a terminal there is
	// nowhere to render the prompt, so skip cleanly instead of hanging.
	connected := true
	if c.Source.SSHMode != "native" && !transport.MasterLive(c) {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			if c.Source.Auth == "password" {
				fmt.Printf("→ connecting to %s (password auto-filled — approve the Duo/2FA prompt if one appears) …\n", sshTarget(c))
			} else {
				fmt.Printf("→ connecting to %s (answer the password/Duo prompt if one appears) …\n", sshTarget(c))
			}
			if err := transport.Connect(c); err != nil {
				connected = false
				fmt.Fprintf(os.Stderr, "lg: couldn't connect (%v)\n", err)
			}
		} else {
			connected = false
			fmt.Fprintln(os.Stderr, "lg: no terminal to show the ssh/Duo prompt on.")
		}
	}
	if !connected {
		fmt.Fprintf(os.Stderr, "    finish setup once you can authenticate: `lg connect`, then `lg init` again for the agent check.\n")
		fmt.Println("\nStart working (after `lg connect`):  lg shell")
		fmt.Println("Change settings later with:  lg config set <key> <value>")
		return nil
	}

	// Confirm/deploy the agent on the remote (best-effort; the connection above
	// is live, so this runs without further prompts).
	fmt.Printf("→ checking the agent on %s …\n", sshTarget(c))
	if msg, err := transport.EnsureAgent(c, agentbin.Pick, Version); err != nil {
		fmt.Fprintf(os.Stderr, "lg: couldn't verify/deploy the agent automatically (%v)\n", err)
		if errors.Is(err, transport.ErrSecondAuth) {
			fmt.Fprintf(os.Stderr, "    this host requires a second auth step; switch it to the cached interactive login:\n")
			fmt.Fprintf(os.Stderr, "        lg config set source.ssh_mode system\n        lg connect      (then `lg init` again for the agent check)\n")
		} else {
			fmt.Fprintf(os.Stderr, "    ensure `lg` exists at ~/.local/bin/lg on Source (or run `lg init` again once ssh works).\n")
		}
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
		fmt.Printf("  local mount: %s\n", mountFor(c.Source.RemoteRoot))
		switch {
		case c.Source.Auth == "password" && c.Source.SSHMode == "native":
			fmt.Printf("  auth:        password (stored encrypted, native ssh)\n")
		case c.Source.Auth == "password":
			fmt.Printf("  auth:        password (stored encrypted, auto-filled) + Duo/2FA via `lg connect`\n")
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

// askPassword reads a secret without echoing it. Falls back to a plain line
// read from r when stdin isn't a terminal (e.g. piped input in scripts) — r
// must be the SAME reader the other prompts use, because a bufio.Reader slurps
// everything piped stdin has; a second reader would find it already drained.
func askPassword(r *bufio.Reader, prompt string) (string, error) {
	fmt.Printf("%s: ", prompt)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, _ := r.ReadString('\n')
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
