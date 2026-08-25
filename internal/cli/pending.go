package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/spf13/cobra"
)

// newPendingCmd shows the write queue and — via `lg pending drop` — abandons
// entries in it. The queue is normally invisible plumbing that drains in ms,
// but it can strand: deleting a big directory through Finder queues one entry
// per file, and if Source refuses them (files owned by another user) they sit
// there forever with nothing to clear them. `lg cache clear` and `lg cancel`
// deliberately never touch the journal — pending entries are the only copy of
// local intent — so dropping needs its own explicit command.
func newPendingCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "pending",
		Short: "Show the queue of unflushed writes (see `lg pending drop`)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := loadGhost(); err != nil {
				return err
			}
			pend, err := pendingEntries()
			if err != nil {
				return err
			}
			if len(pend) == 0 {
				fmt.Println("nothing pending — Source is up to date")
				return nil
			}
			printPending(pend, limit)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 10, "how many individual entries to list")
	cmd.AddCommand(newPendingDropCmd())
	return cmd
}

func newPendingDropCmd() *cobra.Command {
	var all, yes bool
	cmd := &cobra.Command{
		Use:   "drop [path...]",
		Short: "Abandon queued writes (they are never sent to Source)",
		Long: "Drop pending journal entries. The local intent is thrown away and Source is\n" +
			"left exactly as it is: a dropped delete makes the path reappear in the mount\n" +
			"at the next tree sync, and a dropped edit is replaced by Source's version.\n\n" +
			"A path selects its whole subtree. Use --all to clear the entire queue.",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			if len(args) == 0 && !all {
				return fmt.Errorf("give a path to drop, or --all to clear the whole queue")
			}
			if len(args) > 0 && all {
				return fmt.Errorf("--all drops everything; don't also pass paths")
			}

			pend, err := pendingEntries()
			if err != nil {
				return err
			}
			if len(pend) == 0 {
				fmt.Println("nothing pending — nothing to drop")
				return nil
			}

			var rels []string
			if !all {
				if rels, err = resolvePendingArgs(args, pend, c.MountDir()); err != nil {
					return err
				}
			}
			match := fuse.SelectRels(rels)
			var hit []pendingEntry
			for _, p := range pend {
				if match(fuse.JournalEntry{Rel: p.rel, Op: fuse.JournalOp(p.op)}) {
					hit = append(hit, p)
				}
			}
			if len(hit) == 0 {
				fmt.Println("no pending entries match — nothing to drop")
				return nil
			}
			if !yes && !confirmDrop(hit) {
				fmt.Println("aborted — nothing dropped")
				return nil
			}

			n, err := dropPending(c, rels)
			if err != nil {
				return err
			}
			fmt.Printf("✓ dropped %d pending entry(s) — Source unchanged\n", n)
			fmt.Println("  the mount catches up with Source at the next tree sync (≤60s), or run `lg unmount && lg mount`")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "drop every pending entry")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

// dropPending performs the drop. A live mount owns the journal (in memory AND
// on disk — its next Ack rewrites the file), so it is asked over SIGUSR2 rather
// than edited behind its back; with no mount running we edit the journal
// ourselves, which is also the offline escape hatch.
func dropPending(c *config.Config, rels []string) (int, error) {
	pid := mountPid()
	if pid > 0 && fuse.IsMounted(c.MountDir()) && syscall.Kill(pid, 0) == nil {
		if !fuse.HandlesDropSignal() {
			// Never signal a mount that predates the handler: SIGUSR2's default
			// action is to kill the process, which would strand the mount.
			return 0, fmt.Errorf("the running mount is from an older build and can't be asked to drop entries — " +
				"run `lg unmount` first (then `lg pending drop …` edits the journal directly), or restart it with `lg mount`")
		}
		_ = os.Remove(fuse.DropAckPath())
		if len(rels) > 0 {
			if err := os.WriteFile(fuse.DropReqPath(), []byte(strings.Join(rels, "\n")+"\n"), 0o644); err != nil {
				return 0, err
			}
		}
		if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
			_ = os.Remove(fuse.DropReqPath())
			return 0, err
		}
		// The handler writes the count it dropped; compacting a 75k-entry log
		// takes a moment, so wait for it rather than guessing.
		for i := 0; i < 100; i++ {
			if b, err := os.ReadFile(fuse.DropAckPath()); err == nil {
				n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				_ = os.Remove(fuse.DropAckPath())
				return n, nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return 0, fmt.Errorf("the mount didn't acknowledge the drop within 10s — check .lg/lg.log")
	}

	// No mount: the journal file is ours to rewrite.
	j, err := openGhostJournal()
	if err != nil {
		return 0, err
	}
	defer j.Close()
	dropped, err := j.Drop(fuse.SelectRels(rels))
	if err != nil {
		return len(dropped), err
	}
	// A dropped write/create abandons the local edit; its cache copy would
	// otherwise shadow Source's version on the next mount.
	for _, e := range dropped {
		if e.Op == fuse.OpWrite || e.Op == fuse.OpCreate {
			_ = os.Remove(filepath.Join(config.CacheDir(), filepath.FromSlash(e.Rel)))
		}
	}
	return len(dropped), nil
}

// confirmDrop describes what is about to be abandoned and asks. Dropping a
// write is real data loss (the cached copy is the only one), so it is spelled
// out separately from the deletes.
func confirmDrop(hit []pendingEntry) bool {
	byOp := map[string]int{}
	for _, p := range hit {
		byOp[p.op]++
	}
	fmt.Printf("about to drop %d pending entry(s):", len(hit))
	for _, op := range sortedOps(byOp) {
		fmt.Printf("  %s=%d", op, byOp[op])
	}
	fmt.Println()
	if byOp["write"]+byOp["create"] > 0 {
		fmt.Printf("  %d of them are local edits not yet on Source — those edits are lost\n",
			byOp["write"]+byOp["create"])
	}
	fmt.Print("proceed? [y/N] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// printPending summarizes the queue: totals per op, the directories carrying
// the bulk of it, then a few individual entries. A stranded queue is usually
// tens of thousands of entries, so the summary is what's actually readable.
func printPending(pend []pendingEntry, limit int) {
	byOp := map[string]int{}
	byDir := map[string]int{}
	for _, p := range pend {
		byOp[p.op]++
		byDir[topDir(p.rel)]++
	}
	fmt.Printf("pending:     %d entry(s)", len(pend))
	for _, op := range sortedOps(byOp) {
		fmt.Printf("  %s=%d", op, byOp[op])
	}
	fmt.Println()

	type kv struct {
		dir string
		n   int
	}
	var dirs []kv
	for d, n := range byDir {
		dirs = append(dirs, kv{d, n})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].n > dirs[j].n })
	if len(dirs) > 8 {
		dirs = dirs[:8]
	}
	fmt.Println("by path:")
	for _, d := range dirs {
		fmt.Printf("  %-50s %d\n", d.dir, d.n)
	}

	if limit > 0 {
		show := pend
		if len(show) > limit {
			show = show[:limit]
		}
		fmt.Println("oldest first (the queue drains in order):")
		for _, p := range show {
			fmt.Printf("  %-8s %s\n", p.op, p.rel)
		}
		if len(pend) > len(show) {
			fmt.Printf("  … and %d more\n", len(pend)-len(show))
		}
	}
	fmt.Println("\n`lg flush` pushes these to Source; `lg pending drop <path>` abandons them.")
	fmt.Println("Entries that keep failing are logged as \"flush parked\" in .lg/lg.log.")
}

// topDir is the first path component, used to group the summary.
func topDir(rel string) string {
	if i := strings.Index(rel, "/"); i > 0 {
		return rel[:i] + "/"
	}
	return rel
}

func sortedOps(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolvePendingArgs maps user-typed paths onto journal rels. Accepted forms:
// a path inside the mount (absolute or relative to the cwd), or the rel itself
// as `lg pending` prints it. Unlike `lg cancel`, a path need not name an entry
// exactly — it selects its subtree — but it must match something, so a typo
// errors instead of silently dropping nothing.
func resolvePendingArgs(args []string, pend []pendingEntry, mountDir string) ([]string, error) {
	var rels []string
	for _, arg := range args {
		norm := strings.Trim(filepath.ToSlash(arg), "/")
		if abs, err := filepath.Abs(arg); err == nil {
			if under, err := filepath.Rel(mountDir, abs); err == nil && !strings.HasPrefix(under, "..") {
				norm = strings.Trim(filepath.ToSlash(under), "/")
			}
		}
		if norm == "" || norm == "." {
			return nil, fmt.Errorf("%q means the whole tree — use --all if that's what you want", arg)
		}
		n := 0
		for _, p := range pend {
			if p.rel == norm || strings.HasPrefix(p.rel, norm+"/") {
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("no pending entry at or under %q (see `lg pending`)", arg)
		}
		rels = append(rels, norm)
	}
	return rels, nil
}
