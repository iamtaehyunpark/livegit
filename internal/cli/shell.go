package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/shell"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Start the unified shell (mounts the FUSE tree, runs your $SHELL)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell()
		},
	}
}

func runShell() error {
	c, err := loadGhost()
	if err != nil {
		return err
	}
	_, cleanup, logPath, err := setupProjectMount(c)
	if err != nil {
		return err
	}
	// Unmount on normal return AND on termination signals. When lg shell runs a
	// child shell in a terminal, exiting can deliver SIGHUP/SIGTERM that would
	// otherwise skip the defer and orphan the mount. cleanup is idempotent.
	defer cleanup()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	if _, _, err := shell.InstallIntegration(c.AutoRemoteCommands); err != nil {
		return err
	}
	tabID, err := genTabID()
	if err != nil {
		return err
	}
	defer shell.ClearToggle(tabID)

	fmt.Fprintf(os.Stderr, "lg: mounted %s (tab %s)\n", c.LocalRoot, tabID)
	fmt.Fprintf(os.Stderr, "lg: connecting to %s in the background", c.Source.Host)
	if logPath != "" {
		fmt.Fprintf(os.Stderr, " — logs: %s", logPath)
	}
	fmt.Fprintf(os.Stderr, "\n")

	// default_target=source: start with toggle mode ON, so every command in this
	// shell runs on Source from the outset. The mount stays live; `lg
	// toggle` (or `lg local`) drops back to a normal local shell.
	if c.DefaultTarget == "source" {
		_ = shell.SetToggle(tabID, true)
		fmt.Fprintf(os.Stderr, "lg: toggle ON — commands run on %s. `lg toggle` to run locally.\n", c.Source.Host)
	}

	fmt.Fprintf(os.Stderr, "lg: type 'exit' to leave (unmounts and disconnects cleanly).\n")
	return execUserShell(c, tabID)
}

// setupProjectMount brings up everything a live mount needs — connection
// bootstrap, stale-mount recovery, double-mount guard, journal, client,
// backend, and the FUSE mount itself. It is the shared core of `lg shell`
// (which runs the user's shell on top) and `lg mount` (which just holds the
// mount until unmounted). The returned cleanup is idempotent and safe to call
// from a signal handler: unmount, then close the client and journal.
func setupProjectMount(c *config.Config) (mount *fuse.Mount, cleanup func(), logPath string, err error) {
	// Resolve the mountpoint (pinned local_root, or derived from the project's
	// current location) once, in memory, so the rest of this function and its
	// callers read c.LocalRoot directly. Nothing on this path saves the config
	// back, so a derived path is never frozen into the file. The MkdirAll
	// covers a moved project, whose sibling mount dir doesn't exist yet at the
	// new location.
	c.LocalRoot = c.MountDir()
	_ = os.MkdirAll(c.LocalRoot, 0o755)

	// On a Duo/2FA host, authenticate before mounting so the (single) prompt is
	// clean and up front, not buried under FUSE/connection noise — and so the
	// background dialer, which can't answer a prompt, finds a live connection.
	// Best-effort: if it can't (e.g. no terminal), warn and continue; the mount
	// still comes up and the supervisor connects once `lg connect` succeeds.
	if err := transport.EnsureMaster(c); err != nil {
		if errors.Is(err, transport.ErrNeedConnect) {
			fmt.Fprintf(os.Stderr, "lg: not connected to %s yet — run `lg connect` (handles Duo/2FA); the mount will link up once it succeeds.\n", c.Source.Host)
		} else {
			fmt.Fprintf(os.Stderr, "lg: couldn't connect to %s: %v (continuing; will retry)\n", c.Source.Host, err)
		}
	}

	// A previous mount holder killed without unmounting leaves a stale FUSE
	// mount here; reading anything under local_root would then fail with ENXIO.
	// Clear it before we touch the path (e.g. to read .lgignore).
	if recovered, rerr := fuse.RecoverStaleMount(c.LocalRoot); rerr != nil {
		return nil, nil, "", fmt.Errorf("a stale mount at %s couldn't be cleared automatically.\n"+
			"Run:  lg unmount   (or: umount -f %q)\nthen try again. (%v)",
			c.LocalRoot, c.LocalRoot, rerr)
	} else if recovered {
		fmt.Fprintf(os.Stderr, "lg: cleaned up a stale mount at %s\n", c.LocalRoot)
	}

	// A LIVE mount that survived the stale check means another `lg shell` or
	// `lg mount` is already serving this project. Without this check the run
	// continues into a bogus "directory is not empty" warning (IsNonEmptyDir
	// reads the *mounted* tree) and then a confusing double-mount failure.
	if fuse.IsMounted(c.LocalRoot) {
		return nil, nil, "", fmt.Errorf("%s is already mounted — an `lg shell` or `lg mount` is active for this project.\n"+
			"Use the live mount as-is, or close it first (`exit` that shell, or `lg unmount`).", c.LocalRoot)
	}

	// Mounting over a populated directory hides those files while active. Warn
	// loudly — local_root is meant to be an empty mount point.
	if fuse.IsNonEmptyDir(c.LocalRoot) {
		fmt.Fprintf(os.Stderr,
			"lg: warning: %s is not empty — its files will be hidden while lg is mounted.\n"+
				"     local_root should be an empty mount point. Change it with:\n"+
				"       lg config set local_root ~/some-empty-dir\n",
			c.LocalRoot)
	}

	matcher, err := buildMatcher(c)
	if err != nil {
		return nil, nil, "", err
	}
	journal, err := openGhostJournal()
	if err != nil {
		return nil, nil, "", err
	}

	// Send background connection/health logs to a file, not the terminal.
	logPath = routeLogsToFile(c)

	// Long-lived connection + FUSE mount.
	client := newClient(c)
	source := fuse.NewClientSource(client)
	backend := fuse.NewBackend(c, journal, source, matcher)
	client.OnInvalidate(backend.Invalidate)

	mount, err = fuse.NewMount(c.LocalRoot, backend)
	if err != nil {
		_ = client.Close()
		_ = journal.Close()
		return nil, nil, "", fmt.Errorf("mount failed: %w", err)
	}
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			_ = mount.Unmount()
			_ = client.Close()
			_ = journal.Close()
		})
	}
	return mount, cleanup, logPath, nil
}

// execUserShell runs the user's real shell with lg integration injected via
// ZDOTDIR (zsh) or an rcfile (bash), preserving their normal config (D2).
func execUserShell(c *config.Config, tabID string) error {
	shellPath := os.Getenv("SHELL")
	if shellPath == "" {
		shellPath = "/bin/zsh"
	}
	env := append(os.Environ(),
		"LG_TAB_ID="+tabID,
		"LG_PROJECT="+projectID(c),
		"LG_LOCAL_ROOT="+c.LocalRoot, // mount path: auto-route only applies under it
	)

	var cmd *exec.Cmd
	base := filepath.Base(shellPath)
	switch {
	case strings.Contains(base, "zsh"):
		zdot, err := writeZdotdir()
		if err != nil {
			return err
		}
		env = append(env, "ZDOTDIR="+zdot)
		cmd = exec.Command(shellPath, "-i")
	case strings.Contains(base, "bash"):
		rc := filepath.Join(config.Dir(), "hooks", "bash-integration.bash")
		cmd = exec.Command(shellPath, "--rcfile", rc, "-i")
	default:
		cmd = exec.Command(shellPath, "-i")
	}
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// writeZdotdir creates a ZDOTDIR whose .zshrc sources the user's real config
// then the lg integration, so nothing the user has is lost (D2).
func writeZdotdir() (string, error) {
	dir := filepath.Join(config.Dir(), "run", "zdotdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	integ := filepath.Join(config.Dir(), "hooks", "zsh-integration.zsh")
	content := fmt.Sprintf(`# auto-generated by lg
export LG_BASE_PS1=""
[ -f %q ] && source %q
[ -f %q ] && source %q
source %q
`,
		filepath.Join(home, ".zshrc"), filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".zprofile"), filepath.Join(home, ".zprofile"),
		integ)
	if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(content), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func projectID(c *config.Config) string {
	return filepath.Base(strings.TrimRight(c.MountDir(), "/"))
}

func genTabID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
