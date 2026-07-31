package cli

import (
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/spf13/cobra"
)

// newCacheCmd shows content-cache usage; `lg cache clear` empties it. Cached
// content is just an optimization — everything re-fetches on demand — EXCEPT
// files with unflushed journal entries, whose cache copy is the only copy of
// the user's edit until the flush lands; those are always kept.
func newCacheCmd() *cobra.Command {
	cache := &cobra.Command{
		Use:   "cache",
		Short: "Show content-cache usage (see `lg cache clear`)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			files, bytes := cacheInventory()
			fmt.Printf("cache: %d file(s), %s (cap %d GB, idle eviction after %d min)\n",
				files, fmtMB(bytes), c.Cache.MaxCacheSizeGB, c.Cache.EvictAfterIdleMinutes)
			return nil
		},
	}
	cache.AddCommand(&cobra.Command{
		Use:   "clear",
		Short: "Delete all cached content (files with unflushed edits are kept)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := loadGhost(); err != nil {
				return err
			}
			// Files with pending journal entries hold the ONLY copy of local
			// edits until the flush worker pushes them — never delete those.
			pendSet := map[string]bool{}
			if pend, err := pendingEntries(); err == nil {
				for _, p := range pend {
					pendSet[p.rel] = true
				}
			}
			removed, freed, kept := clearCache(pendSet)
			fmt.Printf("✓ removed %d file(s), freed %s\n", removed, fmtMB(freed))
			if kept > 0 {
				fmt.Printf("  kept %d file(s) with unflushed edits — `lg flush` pushes them\n", kept)
			}
			return nil
		},
	})
	return cache
}

func cacheInventory() (int, int64) {
	var files int
	var bytes int64
	_ = filepath.WalkDir(config.CacheDir(), func(p string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			files++
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes
}

// clearCache removes cached content (including stale .lg-tmp staging), keeping
// files in pendSet, then prunes the emptied directory skeleton.
func clearCache(pendSet map[string]bool) (removed int, freed int64, kept int) {
	dir := config.CacheDir()
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir {
				dirs = append(dirs, p)
			}
			return nil
		}
		relPath, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		rel := strings.TrimSuffix(filepath.ToSlash(relPath), ".lg-tmp")
		if pendSet[rel] {
			kept++
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && os.Remove(p) == nil {
			removed++
			freed += info.Size()
		}
		return nil
	})
	// Deepest-first so each rmdir finds its children already gone; non-empty
	// dirs (kept files) just decline.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
	return removed, freed, kept
}
