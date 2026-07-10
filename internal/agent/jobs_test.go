package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDetachedJobLifecycle drives the job manager's nohup path (start -> run to
// completion -> exit code recorded -> log captured -> rm) without a live yamux
// link. systemd --user isn't available in test, so this exercises the fallback
// launcher, which is also the path most likely to run in CI.
func TestDetachedJobLifecycle(t *testing.T) {
	root := t.TempDir()
	m := newJobManager(t.TempDir(), root)
	m.forceNohup = true

	info, _, err := m.start(`printf 'hello-detached\n'; exit 3`, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if info.Mode != "nohup" || info.State != "running" || !isJobID(info.ID) {
		t.Fatalf("unexpected start info: %+v", info)
	}

	// Poll until the wrapper records the exit code.
	var done bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs := m.list()
		if len(jobs) != 1 {
			t.Fatalf("want 1 job, got %+v", jobs)
		}
		if jobs[0].State == "done" {
			if jobs[0].Code != 3 {
				t.Fatalf("exit code = %d, want 3", jobs[0].Code)
			}
			done = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !done {
		t.Fatal("job did not reach done state in time")
	}

	// The command's stdout was captured to the job's log file.
	logBytes, err := os.ReadFile(filepath.Join(m.dir, info.ID, "log"))
	if err != nil || !strings.Contains(string(logBytes), "hello-detached") {
		t.Fatalf("log = %q, err = %v", logBytes, err)
	}

	// A finished job can be removed; a bogus id is rejected.
	if _, err := m.act("zzzzzzzz", "rm"); err == nil {
		t.Fatal("expected error removing an invalid id")
	}
	if msg, err := m.act(info.ID, "rm"); err != nil {
		t.Fatalf("rm: %v (%s)", err, msg)
	}
	if jobs := m.list(); len(jobs) != 0 {
		t.Fatalf("job remained after rm: %+v", jobs)
	}
}

// fakeLoginctl writes a stand-in loginctl whose Linger state lives in a temp
// file, so ensureLinger can be driven through every branch without touching
// real logind. deny makes enable-linger fail (a site polkit override).
func fakeLoginctl(t *testing.T, initial string, deny bool) *jobManager {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(state, []byte(initial+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	enable := "echo yes > " + state
	if deny {
		enable = "exit 1"
	}
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"show-user) cat " + state + " ;;\n" +
		"enable-linger) " + enable + " ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "loginctl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newJobManager(t.TempDir(), t.TempDir())
	m.lingerBin = bin
	return m
}

// TestEnsureLinger covers the durability check that makes detached jobs survive
// the user's last session ending: already-on stays quiet, off gets enabled (with
// a note), and a refused enable produces an actionable warning.
func TestEnsureLinger(t *testing.T) {
	if msg := fakeLoginctl(t, "yes", false).ensureLinger(); msg != "" {
		t.Fatalf("linger already on: want no message, got %q", msg)
	}

	m := fakeLoginctl(t, "no", false)
	if msg := m.ensureLinger(); !strings.Contains(msg, "enabled") {
		t.Fatalf("linger off + enable ok: want 'enabled' note, got %q", msg)
	}
	// Cached: the second call must not re-run loginctl (same message back).
	if msg := m.ensureLinger(); !strings.Contains(msg, "enabled") {
		t.Fatalf("cached result lost: got %q", msg)
	}

	if msg := fakeLoginctl(t, "no", true).ensureLinger(); !strings.Contains(msg, "could not enable") {
		t.Fatalf("linger off + enable denied: want warning, got %q", msg)
	}

	// No loginctl at all (non-systemd host): stay quiet, don't error.
	m = newJobManager(t.TempDir(), t.TempDir())
	m.lingerBin = filepath.Join(t.TempDir(), "missing-loginctl")
	if msg := m.ensureLinger(); msg != "" {
		t.Fatalf("no loginctl: want no message, got %q", msg)
	}
}

// TestDetachedJobKill starts a long job and stops it, then confirms it no longer
// reports running.
func TestDetachedJobKill(t *testing.T) {
	root := t.TempDir()
	m := newJobManager(t.TempDir(), root)
	m.forceNohup = true

	info, _, err := m.start("sleep 60", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Give the wrapper a moment to fork the sleep.
	time.Sleep(200 * time.Millisecond)
	if jobs := m.list(); len(jobs) != 1 || jobs[0].State != "running" {
		t.Fatalf("expected one running job, got %+v", jobs)
	}
	if _, err := m.act(info.ID, "kill"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// In production the short-lived launcher agent has already exited, so init
	// reaps the killed process and a separate `lg jobs` agent sees it gone. Here
	// the test process is the launcher, so the killed child would linger as a
	// zombie (kill -0 still succeeds) — reap it ourselves to mirror init.
	if meta, err := m.readMeta(info.ID); err == nil && meta.PID > 0 {
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(meta.PID, &ws, 0, nil)
	}
	// After SIGTERM to the group the process should exit shortly.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if jobs := m.list(); len(jobs) == 1 && jobs[0].State != "running" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job still running after kill")
}
