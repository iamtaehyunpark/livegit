package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
	"golang.org/x/term"
)

func newInitCmd() *cobra.Command {
	var role, host, remoteRoot, localRoot, user string
	var port int
	var yes, forceInteractive bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up lg (interactive) — writes ~/.lg/config.yaml",
		Long: `Set up lg.

Run with no flags for a guided, step-by-step setup:

    lg init

Or pass flags to skip the prompts (useful for scripts):

    lg init --role ghost --host gpu-1 --remote-root /home/u/proj --local-root ~/proj`,
		RunE: func(cmd *cobra.Command, args []string) error {
			in := &wizardInput{
				role: role, host: host, remoteRoot: remoteRoot,
				localRoot: localRoot, user: user, port: port,
			}
			// Interactive when forced, or on a TTY without full flag input.
			interactive := forceInteractive || (term.IsTerminal(int(os.Stdin.Fd())) && !in.complete())
			if interactive {
				if err := runWizard(in); err != nil {
					return err
				}
			}
			return writeConfig(in, yes || !interactive)
		},
	}
	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&role, "role", "", "ghost|source")
	cmd.Flags().StringVar(&host, "host", "", "Source ssh host (ghost role)")
	cmd.Flags().StringVar(&remoteRoot, "remote-root", "", "absolute repo path on Source")
	cmd.Flags().StringVar(&localRoot, "local-root", "", "Ghost FUSE mount point")
	cmd.Flags().StringVar(&user, "user", "", "ssh user (default $USER)")
	cmd.Flags().IntVar(&port, "port", 0, "ssh port (default 22)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompts")
	cmd.Flags().BoolVarP(&forceInteractive, "interactive", "i", false, "force the step-by-step wizard")
	_ = home
	return cmd
}

// wizardInput collects answers (from flags and/or prompts).
type wizardInput struct {
	role       string
	host       string
	remoteRoot string
	localRoot  string
	user       string
	port       int
}

// complete reports whether enough was supplied via flags to skip prompting.
func (w *wizardInput) complete() bool {
	switch config.Role(w.role) {
	case config.RoleGhost:
		return w.host != "" && w.remoteRoot != "" && w.localRoot != ""
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

	fmt.Println()
	fmt.Println("And where on this laptop should the project appear?")
	defMount := in.localRoot
	if defMount == "" {
		home, _ := os.UserHomeDir()
		defMount = filepath.Join(home, filepath.Base(strings.TrimRight(in.remoteRoot, "/")))
	}
	in.localRoot = ask(r, "  Local mount point", defMount, true)
	return nil
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
	c.LocalRoot = expandHome(in.localRoot)
	c.Source.RemoteRoot = strings.TrimRight(c.Source.RemoteRoot, "/")

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

	// Summary + confirmation.
	if !skipConfirm {
		fmt.Println()
		fmt.Println("Here's what I'll write:")
		printSummary(c)
		if config.Exists() {
			fmt.Printf("\n%s already exists and will be overwritten.\n", config.Path())
		}
		if !confirm(bufio.NewReader(os.Stdin), "Write this config?", true) {
			fmt.Println("aborted — nothing written.")
			return nil
		}
	}

	if err := os.MkdirAll(config.Dir(), 0o755); err != nil {
		return err
	}
	if c.Role == config.RoleGhost {
		_ = os.MkdirAll(config.CacheDir(), 0o755)
		_ = os.MkdirAll(c.LocalRoot, 0o755)
	}
	if err := c.Save(); err != nil {
		return err
	}

	fmt.Printf("\n✓ wrote %s\n", config.Path())
	if c.Role == config.RoleGhost {
		fmt.Println("\nNext steps:")
		fmt.Printf("  1. Make sure plain ssh works:   ssh %s\n", sshTarget(c))
		fmt.Println("  2. Start working:               lg shell")
		fmt.Println("\nChange anything later with:       lg config set <key> <value>")
	} else {
		fmt.Println("\nThis machine is the Source. Make sure `lg` is on its PATH;")
		fmt.Println("your laptop launches the agent over ssh automatically.")
	}
	return nil
}

func printSummary(c *config.Config) {
	fmt.Printf("  role:        %s\n", c.Role)
	if c.Role == config.RoleGhost {
		fmt.Printf("  server:      %s\n", sshTarget(c))
		fmt.Printf("  remote repo: %s\n", c.Source.RemoteRoot)
		fmt.Printf("  local mount: %s\n", c.LocalRoot)
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

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return p
}
