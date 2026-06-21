package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/fuse"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show mode, sync state, cache usage, and conflicts",
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

			// Per-tab mode (if invoked inside an lg shell).
			if tab := os.Getenv("LG_TAB_ID"); tab != "" {
				st := shellLoadState(tab)
				fmt.Printf("mode:        %s", st.mode)
				if st.via != "" {
					fmt.Printf(" (via %s)", st.via)
				}
				fmt.Println()
			}

			// File-state counts + cache usage from the state DB.
			if store, err := fuse.OpenState(config.StateDBPath()); err == nil {
				defer store.Close()
				g, ca, li, _ := store.Counts()
				used, _ := store.CachedSizeBytes()
				fmt.Printf("files:       %d ghost, %d cached, %d live\n", g, ca, li)
				fmt.Printf("cache used:  %.1f MB / %d GB\n",
					float64(used)/(1<<20), c.Cache.MaxCacheSizeGB)
			}

			// Pending journal entries (unflushed writes).
			if pending, err := countPending(); err == nil {
				fmt.Printf("journal:     %d pending write(s)\n", pending)
			}

			// Conflicts.
			conflicts := readConflicts()
			if len(conflicts) == 0 {
				fmt.Println("conflicts:   none")
			} else {
				fmt.Printf("conflicts:   %d\n", len(conflicts))
				for _, c := range conflicts {
					fmt.Printf("  - %s", c.Rel)
					if c.BackupRel != "" {
						fmt.Printf(" (backup: %s)", c.BackupRel)
					}
					fmt.Println()
				}
			}
			return nil
		},
	}
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

func readConflicts() []fuse.Conflict {
	f, err := os.Open(config.ConflictsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []fuse.Conflict
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c fuse.Conflict
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// shellLoadState is a thin local view of the per-tab state to avoid importing
// the shell package just for display fields.
type tabView struct {
	mode string
	via  string
}

func shellLoadState(tab string) tabView {
	b, err := os.ReadFile(config.Dir() + "/run/" + tab + ".json")
	if err != nil {
		return tabView{mode: "local"}
	}
	var raw struct {
		Mode       string `json:"mode"`
		EnteredVia string `json:"entered_via"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Mode == "" {
		return tabView{mode: "local"}
	}
	return tabView{mode: raw.Mode, via: raw.EnteredVia}
}
