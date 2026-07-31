package cli

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newProgressCmd is the live transfer monitor: one gauge per in-flight
// download with bytes, speed, and ETA, refreshed twice a second, plus the
// queued-upload list. Progress is read the same way `lg status` reads it (the
// growing .lg-tmp staging files — no IPC with the mount process); speed is the
// sampled growth between ticks. Exits by itself once transfers it was watching
// finish; Ctrl-C stops it any time.
func newProgressCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "progress",
		Short: "Live transfer monitor: download gauges with speed and ETA",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := loadGhost(); err != nil {
				return err
			}
			speeds := map[string]*speedSample{}
			sawWork := false
			for {
				dls := downloads()
				pend, _ := pendingEntries()
				if len(dls) == 0 && len(pend) == 0 {
					if sawWork {
						fmt.Print("\033[H\033[2J")
						fmt.Println("✓ all transfers finished")
						return nil
					}
					fmt.Print("\033[H\033[2J")
					fmt.Println("no transfers in flight — watching (Ctrl-C to stop)…")
				} else {
					sawWork = true
					var out strings.Builder
					if len(dls) > 0 {
						out.WriteString("downloading:\n")
						for _, d := range dls {
							out.WriteString(renderDownload(d, speeds))
						}
					}
					if len(pend) > 0 {
						fmt.Fprintf(&out, "uploading (%d queued):\n", len(pend))
						show := pend
						if len(show) > 5 {
							show = show[:5]
						}
						for _, p := range show {
							out.WriteString("  " + p.op + ": " + p.rel + "\n")
						}
						if len(pend) > len(show) {
							fmt.Fprintf(&out, "  … and %d more\n", len(pend)-len(show))
						}
					}
					fmt.Print("\033[H\033[2J" + out.String() + "\n(Ctrl-C to stop)\n")
				}
				time.Sleep(500 * time.Millisecond)
			}
		},
	}
}

// speedSample tracks one download's byte growth for a smoothed rate.
type speedSample struct {
	staged int64
	at     time.Time
	rate   float64 // bytes/sec, exponentially smoothed
}

func renderDownload(d inflight, speeds map[string]*speedSample) string {
	now := time.Now()
	s := speeds[d.rel]
	if s == nil {
		s = &speedSample{staged: d.staged, at: now}
		speeds[d.rel] = s
	} else if dt := now.Sub(s.at).Seconds(); dt > 0.2 {
		inst := float64(d.staged-s.staged) / dt
		if s.rate == 0 {
			s.rate = inst
		} else {
			s.rate = 0.7*s.rate + 0.3*inst // smooth out chunk-arrival jitter
		}
		s.staged, s.at = d.staged, now
	}

	name := d.rel
	if len(name) > 58 {
		name = "…" + name[len(name)-57:]
	}
	line := "  " + name + "\n"
	if d.stalled {
		return line + fmt.Sprintf("  %s  %s  [stalled — `lg cancel` cleans this up]\n",
			gauge(frac(d), 28), fmtMB(d.staged))
	}
	stats := fmtMB(d.staged)
	if d.total > 0 {
		stats = fmt.Sprintf("%s / %s (%d%%)", fmtMB(d.staged), fmtMB(d.total), int(frac(d)*100))
	}
	if s.rate > 1 {
		stats += fmt.Sprintf("   %s/s", fmtMB(int64(s.rate)))
		if d.total > 0 {
			stats += "   ETA " + fmtETA(float64(d.total-d.staged)/s.rate)
		}
	}
	return line + "  " + gauge(frac(d), 28) + "  " + stats + "\n"
}

func frac(d inflight) float64 {
	if d.total <= 0 {
		return 0
	}
	return float64(d.staged) / float64(d.total)
}

// gauge renders a fixed-width progress bar like [██████░░░░░░].
func gauge(f float64, width int) string {
	if math.IsNaN(f) || f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	fill := int(f*float64(width) + 0.5)
	return "[" + strings.Repeat("█", fill) + strings.Repeat("░", width-fill) + "]"
}

// fmtETA renders seconds as m:ss or h:mm:ss ("--:--" when unknowable).
func fmtETA(sec float64) string {
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec > 359999 {
		return "--:--"
	}
	s := int(sec + 0.5)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
