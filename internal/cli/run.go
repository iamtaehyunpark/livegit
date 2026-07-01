package cli

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/fuse"
	"github.com/taehyun/lg/internal/shell"
	"github.com/taehyun/lg/internal/transport"
)

// newRunCmd is the explicit form of the command runner: `lg run -- <command>`.
// The bare form `lg <command>` (see dispatchPassthrough) routes here too. It
// runs the command on Source in a PTY, streams it live, and exits with the
// remote process's exit code (§1.1).
func newRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:                "run -- <command>",
		Short:              "Run a command on Source, streaming output live",
		DisableFlagParsing: true, // pass every token through to the remote command
		RunE: func(cmd *cobra.Command, args []string) error {
			// --local-fallback arrives as a literal arg (flag parsing is off); the
			// shell integration uses it for auto-routed read commands so they run
			// the local command when Source is unreachable.
			localFallback := false
			if len(args) > 0 && args[0] == "--local-fallback" {
				localFallback = true
				args = args[1:]
			}
			args = stripDashes(args)
			if len(args) == 0 {
				return fmt.Errorf("usage: lg run -- <command>")
			}
			code := runRemote(args, localFallback)
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
func runRemote(args []string, localFallback bool) int {
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

	code, err := shell.RunRemote(client, joinArgs(args), relDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lg: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
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

// joinArgs reconstructs a shell command line from argv, quoting tokens that
// contain whitespace so `lg run -- echo "a b"` survives the round-trip.
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += quoteArg(a)
	}
	return out
}

func quoteArg(a string) string {
	needs := a == ""
	for _, r := range a {
		if r == ' ' || r == '\t' || r == '\n' {
			needs = true
			break
		}
	}
	if !needs {
		return a
	}
	// Single-quote and escape embedded single quotes.
	out := "'"
	for _, r := range a {
		if r == '\'' {
			out += `'\''`
		} else {
			out += string(r)
		}
	}
	return out + "'"
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
