package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/shell"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show connection, toggle mode, cache usage, transfers, and pending writes",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if !watch {
				return printStatus(c)
			}
			for {
				fmt.Print("\033[H\033[2J") // clear screen, cursor home
				if err := printStatus(c); err != nil {
					return err
				}
				fmt.Println("\n(watching — Ctrl-C to stop)")
				time.Sleep(1 * time.Second)
			}
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh every second (live transfer progress)")
	return cmd
}

func printStatus(c *config.Config) error {
	fmt.Printf("role:        %s\n", c.Role)
	if c.Role == config.RoleGhost {
		mp := c.MountDir()
		if fuse.IsMounted(mp) {
			fmt.Printf("mount:       %s (live — browse/edit it directly)\n", mp)
		} else {
			fmt.Printf("mount:       %s (not mounted — `lg mount` or `lg shell` to browse/edit)\n", mp)
		}
		fmt.Printf("source:      %s:%s\n", c.Source.Host, c.Source.RemoteRoot)

		// ssh connection (Duo/2FA reuse) — system mode only; native mode
		// carries the stored credentials on every connection, no master.
		if c.Source.SSHMode == "native" {
			fmt.Println("connection:  per-command (native ssh; each command authenticates itself)")
		} else if transport.MasterLive(c) {
			fmt.Printf("connection:  live (cached %s; new commands won't re-prompt)\n", transport.PersistLabel(c))
		} else {
			fmt.Println("connection:  down — run `lg connect` to authenticate (handles Duo/2FA)")
		}
	}

	// Toggle mode (if invoked inside an lg shell).
	if tab := os.Getenv("LG_TAB_ID"); tab != "" {
		if shell.ToggleOn(tab) {
			fmt.Println("toggle:      ON (commands run on Source)")
		} else {
			fmt.Println("toggle:      off (commands run locally)")
		}
	}

	// Full-tree snapshot freshness.
	if info, err := os.Stat(treeSnapshotPath()); err == nil {
		fmt.Printf("tree:        %d entries cached, synced %s\n",
			countSnapshotEntries(), info.ModTime().Format("2006-01-02 15:04:05"))
	} else {
		fmt.Println("tree:        not synced yet")
	}

	// On-disk content cache size.
	if used := cacheBytes(); used >= 0 {
		fmt.Printf("cache used:  %.1f MB / %d GB\n",
			float64(used)/(1<<20), c.Cache.MaxCacheSizeGB)
	}

	// In-flight downloads. No IPC with the mount process needed: every fetch
	// stages into <cache>/<rel>.lg-tmp, so the staging file's size IS the
	// bytes received so far, and the total comes from the tree snapshot.
	if dls := downloads(); len(dls) > 0 {
		fmt.Println("downloading:")
		for _, d := range dls {
			line := fmt.Sprintf("  %s  %s", d.rel, fmtMB(d.staged))
			if d.total > 0 {
				line += fmt.Sprintf(" / %s (%d%%)", fmtMB(d.total), d.staged*100/d.total)
			}
			if d.stalled {
				line += "  [paused or stalled]"
			}
			fmt.Println(line)
		}
	}

	// Pending journal entries (unflushed writes), with what's actually queued.
	pend, err := pendingEntries()
	if err == nil {
		fmt.Printf("journal:     %d pending write(s)\n", len(pend))
		show := pend
		if len(show) > 5 {
			show = show[:5]
		}
		for _, p := range show {
			if p.op == "write" {
				if info, err := os.Stat(config.CacheDir() + "/" + p.rel); err == nil {
					fmt.Printf("  uploading: %s (%s)\n", p.rel, fmtMB(info.Size()))
					continue
				}
			}
			fmt.Printf("  %s: %s\n", p.op, p.rel)
		}
		if len(pend) > len(show) {
			fmt.Printf("  … and %d more\n", len(pend)-len(show))
		}
		if len(pend) > 0 {
			fmt.Println("  (`lg pending` for the whole queue; `lg pending drop` abandons entries)")
		}
	}
	return nil
}

// inflight is one in-progress content download, read from its staging file.
type inflight struct {
	rel     string
	staged  int64
	total   int64 // 0 = size unknown (not in the tree snapshot)
	stalled bool  // staging hasn't grown recently — likely an interrupted fetch
}

func downloads() []inflight {
	var sizes map[string]int64 // built lazily: only when a staging file exists
	var out []inflight
	dir := config.CacheDir()
	_ = filepath.WalkDir(dir, func(p string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".lg-tmp") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		relPath, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		if sizes == nil {
			sizes = snapshotSizes()
		}
		rel := strings.TrimSuffix(filepath.ToSlash(relPath), ".lg-tmp")
		out = append(out, inflight{
			rel:     rel,
			staged:  info.Size(),
			total:   sizes[rel],
			stalled: time.Since(info.ModTime()) > 30*time.Second,
		})
		return nil
	})
	return out
}

// snapshotSizes maps rel -> size from the tree snapshot (for download totals).
func snapshotSizes() map[string]int64 {
	sizes := map[string]int64{}
	b, err := os.ReadFile(treeSnapshotPath())
	if err != nil {
		return sizes
	}
	var list []struct {
		Rel  string `json:"rel"`
		Size int64  `json:"size"`
	}
	if json.Unmarshal(b, &list) != nil {
		return sizes
	}
	for _, e := range list {
		sizes[e.Rel] = e.Size
	}
	return sizes
}

func fmtMB(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func treeSnapshotPath() string { return config.Dir() + "/tree.json" }

func countSnapshotEntries() int {
	b, err := os.ReadFile(treeSnapshotPath())
	if err != nil {
		return 0
	}
	// tree.json is a JSON array of entries; count elements without decoding each
	// one (RawMessage skips per-field work and is correct for any file name — a
	// hand-rolled brace counter miscounts names containing '{'/'}').
	var list []json.RawMessage
	if json.Unmarshal(b, &list) != nil {
		return 0
	}
	return len(list)
}

func cacheBytes() int64 {
	var total int64 = -1
	dir := config.CacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return total
	}
	total = 0
	var walk func(string)
	walk = func(p string) {
		es, err := os.ReadDir(p)
		if err != nil {
			return
		}
		for _, e := range es {
			fp := p + "/" + e.Name()
			if e.IsDir() {
				walk(fp)
				continue
			}
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	for _, e := range entries {
		fp := dir + "/" + e.Name()
		if e.IsDir() {
			walk(fp)
		} else if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// pendingEntry is one unflushed journal entry (rel + operation), read from the
// journal file without disturbing a live mount's journal state.
type pendingEntry struct {
	rel string
	op  string
}

func pendingEntries() ([]pendingEntry, error) {
	f, err := os.Open(config.JournalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []pendingEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e struct {
			Rel string `json:"rel"`
			Op  string `json:"op"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, pendingEntry{rel: e.Rel, op: e.Op})
		}
	}
	return out, sc.Err()
}

// countPending counts unflushed journal entries (used by `lg flush`).
func countPending() (int, error) {
	p, err := pendingEntries()
	return len(p), err
}
