package cli

import (
	"bufio"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/shell"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show connection, toggle mode, cache usage, and pending writes",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Printf("role:        %s\n", c.Role)
			if c.Role == config.RoleGhost {
				fmt.Printf("mount:       %s\n", c.LocalRoot)
				fmt.Printf("source:      %s:%s\n", c.Source.Host, c.Source.RemoteRoot)
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

			// Pending journal entries (unflushed writes).
			if pending, err := countPending(); err == nil {
				fmt.Printf("journal:     %d pending write(s)\n", pending)
			}
			return nil
		},
	}
}

func treeSnapshotPath() string { return config.Dir() + "/tree.json" }

func countSnapshotEntries() int {
	b, err := os.ReadFile(treeSnapshotPath())
	if err != nil {
		return 0
	}
	// Entries are a JSON array; count is cheap enough via the decoder.
	n := 0
	depth := 0
	for _, r := range b {
		switch r {
		case '{':
			if depth == 1 {
				n++
			}
			depth++
		case '[':
			depth++
		case '}':
			depth--
		case ']':
			depth--
		}
	}
	return n
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

// countPending counts unflushed journal entries by scanning the journal file.
func countPending() (int, error) {
	f, err := os.Open(config.JournalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n, sc.Err()
}
