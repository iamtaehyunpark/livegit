// Package logx is the single leveled logger shared by every lg package.
// Debugging a multiplexed SSH + FUSE + PTY system without consistent logging is
// brutal, so all components funnel through here from M0 onward.
package logx

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Level mirrors slog levels but is configured from a simple string.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

var root = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Init configures the global logger. level is one of debug|info|warn|error.
// Called once at process start from the CLI layer.
func Init(level string, w io.Writer) {
	if w == nil {
		w = os.Stderr
	}
	root = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: parseLevel(level)}))
}

// For returns a logger tagged with a component name, e.g. logx.For("fuse").
func For(component string) *slog.Logger {
	return root.With("comp", component)
}

// L returns the root logger.
func L() *slog.Logger { return root }
