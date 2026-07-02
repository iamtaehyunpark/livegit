package cli

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/shell"
	"github.com/iamtaehyunpark/livegit/internal/shellq"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

// newRunCmd is the explicit form of the command runner: `lg run -- <command>`.
// The bare form `lg <command>` (see dispatchPassthrough) routes here too. It
// runs the command on Source in a PTY, streams it live, and exits with the
// remote process's exit code.
func newRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:                "run -- <command>",
		Short:              "Run a command on Source, streaming output live",
		DisableFlagParsing: true, // pass every token through to the remote command
		RunE: func(cmd *cobra.Command, args []string) error {
			// Leading flags arrive as literal args (flag parsing is off):
			//   --local-fallback : shell integration uses it for auto-routed read
			//                      commands so they run locally when Source is down.
			//   --detach / -d    : launch the command as a detached job that
			//                      outlives this invocation, print its id, return.
			localFallback, detach := false, false
		flags:
			for len(args) > 0 {
				switch args[0] {
				case "--local-fallback":
					localFallback = true
					args = args[1:]
				case "--detach", "-d":
					detach = true
					args = args[1:]
				default:
					break flags
				}
			}
			args = stripDashes(args)
			if len(args) == 0 {
				return fmt.Errorf("usage: lg run [--detach] -- <command>")
			}
			code := runRemote(args, localFallback, detach)
			os.Exit(code)
			return nil
		},
	}
	return c
}

// runRemote executes args as a single command line on Source and returns the
// remote exit code. It is the shared entrypoint for `lg run` and `lg <command>`.
// If localFallback is set and Source can't be reached, it runs the command in
// the local shell instead of failing (used by auto-routed read commands).
func runRemote(args []string, localFallback, detach bool) int {
	cfg, err := loadGhost()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lg: %v\n", err)
		return 1
	}
	routeLogsToFile(cfg) // keep reconnect/health noise out of the terminal

	client := newClient(cfg)
	defer client.Close()

	// A short wait when we can fall back locally (don't make the user wait long
	// before the local command runs); the normal full wait otherwise.
	wait := 15 * time.Second
	if localFallback {
		wait = 3 * time.Second
	}
	if err := waitOnline(client, wait); err != nil {
		if localFallback {
			return runLocal(args)
		}
		fmt.Fprintf(os.Stderr, "lg: not connected to %s (%v)\n", cfg.Source.Host, err)
		return 1
	}

	// If a local edit is still in flight, make sure Source has it before running.
	cwd, _ := os.Getwd()
	relDir := relDirOf(config.NewPathMapper(cfg), cwd)
	if err := flushBarrier(relDir, 10*time.Second, client); err != nil {
		fmt.Fprintf(os.Stderr, "lg: flush barrier: %v (continuing)\n", err)
	}

	if detach {
		return startDetached(client, shellq.Join(args), relDir)
	}

	code, err := shell.RunRemote(client, shellq.Join(args), relDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lg: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// startDetached launches cmd as a background job on Source and prints how to
// follow it. The job outlives this command (and the ghost): it runs under
// systemd --user, escaping the ssh session scope that would otherwise reap it.
func startDetached(client *transport.Client, cmd, relDir string) int {
	resp, err := shell.StartJob(client, cmd, relDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lg: %v\n", err)
		return 1
	}
	if resp.Warn != "" {
		fmt.Fprintf(os.Stderr, "lg: %s\n", resp.Warn)
	}
	fmt.Printf("started job %s (%s)\n", resp.ID, resp.Mode)
	fmt.Printf("  follow:  lg logs -f %s\n", resp.ID)
	fmt.Printf("  list:    lg jobs\n")
	return 0
}

// runLocal runs the already-tokenized command in the local shell (the fallback
// when Source is unreachable for an auto-routed read command). The tokens were
// expanded by the shell before reaching us, so exec them directly.
func runLocal(args []string) int {
	c := exec.Command(args[0], args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "lg (local fallback): %v\n", err)
		return 127
	}
	return 0
}

// stripDashes drops a leading "--" that cobra leaves in args under
// DisableFlagParsing.
func stripDashes(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

// waitOnline blocks until the client reports online or the timeout elapses.
func waitOnline(client *transport.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if client.Status().Online() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connection not established")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// flushBarrier polls the on-disk journal until no pending entries remain for
// relDir (the cross-process barrier; the flush worker lives in `lg shell`).
func flushBarrier(relDir string, timeout time.Duration, client *transport.Client) error {
	deadline := time.Now().Add(timeout)
	for {
		pending, err := fuse.ScanPendingDir(config.JournalPath(), relDir)
		if err != nil {
			return err
		}
		if !pending {
			return nil
		}
		if time.Now().After(deadline) {
			return fuse.ErrBarrierTimeout
		}
		if !client.Status().Online() {
			return fuse.ErrBarrierOffline
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// relDirOf converts an absolute cwd to a rel dir under local_root, or "" if the
// cwd is outside the mount (commands there run in the remote root).
func relDirOf(mapper *config.PathMapper, cwd string) string {
	if cwd == "" {
		return ""
	}
	rel, err := mapper.LocalToRel(cwd)
	if err != nil {
		return ""
	}
	return rel
}
