package agent

import (
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/hashx"
	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
)

// Watcher detects changes to Source's real files and pushes lazy "invalidated"
// notifications to Ghost (§4.3) — metadata only, never content. It shares the
// ignore matcher so it never pushes for excluded paths (§4.6, §7).
type Watcher struct {
	root    string
	mapper  *config.PathMapper
	matcher *config.Matcher
	notify  func(proto.Invalidate)
	fsw     *fsnotify.Watcher
}

// NewWatcher creates (but does not start) a watcher rooted at remote_root.
func NewWatcher(remoteRoot string, matcher *config.Matcher, notify func(proto.Invalidate)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	cfg := &config.Config{LocalRoot: remoteRoot}
	cfg.Source.RemoteRoot = remoteRoot
	return &Watcher{
		root:    filepath.Clean(remoteRoot),
		mapper:  config.NewPathMapper(cfg),
		matcher: matcher,
		notify:  notify,
		fsw:     fsw,
	}, nil
}

// Run watches recursively until ctx-equivalent stop (Close) is called.
func (w *Watcher) Run() {
	log := logx.For("watcher")
	w.addRecursive(w.root)
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			log.Warn("watch error", "err", err)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	rel, err := w.mapper.RemoteToRel(ev.Name)
	if err != nil {
		return
	}
	info, statErr := os.Stat(ev.Name)
	isDir := statErr == nil && info.IsDir()
	if w.matcher != nil && w.matcher.Match(rel, isDir) {
		return // ignored path; no push
	}
	// Newly created directories must be watched too.
	if isDir && ev.Op&(fsnotify.Create) != 0 {
		w.addRecursive(ev.Name)
		return
	}
	inv := proto.Invalidate{Rel: rel}
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		inv.Deleted = true
	} else if statErr == nil && !isDir {
		inv.ModTime = info.ModTime().Unix()
		if h, err := hashx.File(ev.Name); err == nil {
			inv.Hash = h
		}
	}
	if w.notify != nil {
		w.notify(inv)
	}
}

func (w *Watcher) addRecursive(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := w.mapper.RemoteToRel(path)
		if rerr == nil && w.matcher != nil && w.matcher.Match(rel, true) {
			return filepath.SkipDir
		}
		_ = w.fsw.Add(path)
		return nil
	})
}

// Close stops watching.
func (w *Watcher) Close() error { return w.fsw.Close() }
