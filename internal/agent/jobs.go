package agent

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/iamtaehyunpark/livegit/internal/proto"
	"github.com/iamtaehyunpark/livegit/internal/shellq"
)

// jobManager implements detached ("fire-and-forget") remote jobs — the async
// sibling of the command runner (exec.go).
//
// The problem it solves: `lg run` opens a FRESH ssh session per command and
// kills it on return. That ends the remote ssh login session, and systemd tears
// down the session's cgroup scope (KillUserProcesses=yes, the lab default),
// killing everything spawned in it — nohup, setsid, even a detached tmux
// server. A long GPU run therefore cannot be left behind through `lg run`
// itself. (lg is not the reaper: exec.go issues no process-group kill; systemd
// reaps the ephemeral session scope.)
//
// The fix is to launch the job in a cgroup that is NOT the ssh session scope:
// `systemd-run --user` places it under user@UID.service, a separate branch that
// outlives the login session, so the job survives the launching `lg run`
// returning and the ghost disconnecting. Where systemd --user is unavailable we
// fall back to setsid+nohup — best effort, since that still lives in the
// session scope and so needs `loginctl enable-linger` to be durable (surfaced
// to the user as a warning).
//
// One more durability condition: user@UID.service itself only outlives the
// user's sessions when LINGERING is on. With Linger=no, logind stops the whole
// user manager — and every lg job under it — the moment the last login session
// ends (e.g. the ghost's cached ssh connection dropping overnight). start()
// therefore checks lingering and enables it when off (systemd's default polkit
// policy allows users to linger themselves); if it can't, the user gets a
// warning that the job's lifetime is tied to their sessions.
//
// State is deliberately NOT held in the agent process: each `lg run --detach`
// spawns a fresh, short-lived agent that launches the job and dies. Liveness is
// therefore owned by systemd (or the recorded pid), and identity/logs by an
// on-disk jobs directory (~/.lg/jobs/<id>/) that any later agent can read.
// systemd answers "is it still running"; the jobs dir answers "what / where /
// exit code". No cross-agent shared memory is needed.
type jobManager struct {
	dir        string // e.g. ~/.lg/jobs on Source
	remoteRoot string

	sdOnce sync.Once
	sdOK   bool
	sdEnv  []string

	lingerOnce sync.Once
	lingerMsg  string
	lingerBin  string // "loginctl"; tests point it at a fake

	// forceNohup skips the systemd path (tests, so they don't create real
	// transient user units on a systemd host). It also skips the linger
	// check/enable, which would mutate real logind state on a dev machine.
	forceNohup bool
}

// jobMeta is the persisted identity of a job (jobs/<id>/meta.json). Liveness and
// exit code are NOT stored here — they are derived from systemd/pid and the
// sibling `exit` file, so a job started by one agent is fully understood by the
// next.
type jobMeta struct {
	ID      string `json:"id"`
	Cmd     string `json:"cmd"`
	Mode    string `json:"mode"` // "systemd" | "nohup"
	Unit    string `json:"unit,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Started int64  `json:"started"`
}

func newJobManager(dir, remoteRoot string) *jobManager {
	return &jobManager{dir: dir, remoteRoot: remoteRoot, lingerBin: "loginctl"}
}

// defaultJobsDir is ~/.lg/jobs on Source. Jobs are per-user (not per-project):
// one `lg jobs` view of everything running on the host, independent of which
// remote_root launched them.
func defaultJobsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".lg", "jobs")
}

// start launches cmdLine detached in relCwd (mapped under remoteRoot) and
// returns the new job's info. warn is a non-fatal caveat for the user.
func (m *jobManager) start(cmdLine, relCwd string) (proto.JobInfo, string, error) {
	log := logx.For("jobs")
	if strings.TrimSpace(cmdLine) == "" {
		return proto.JobInfo{}, "", fmt.Errorf("empty command")
	}
	id, err := newJobID()
	if err != nil {
		return proto.JobInfo{}, "", err
	}
	jobdir := filepath.Join(m.dir, id)
	if err := os.MkdirAll(jobdir, 0o755); err != nil {
		return proto.JobInfo{}, "", fmt.Errorf("create job dir: %w", err)
	}

	absdir := resolveDir(m.remoteRoot, relCwd)
	logPath := filepath.Join(jobdir, "log")
	exitPath := filepath.Join(jobdir, "exit")
	runPath := filepath.Join(jobdir, "run.sh")

	// The wrapper is identical for both launch modes so exit-code capture and
	// output redirection behave the same way regardless of how it's supervised.
	// A login shell (`sh -lc`) matches the command runner so PATH/conda/venv
	// resolve as they would interactively. `echo $?` right after captures the
	// user command's status; its own `> exit` redirect wins over the group's.
	script := fmt.Sprintf(
		"#!/bin/sh\ncd %s 2>/dev/null || cd %s || exit 1\n{ sh -lc %s; echo $? > %s; } > %s 2>&1\n",
		shellq.Quote(absdir), shellq.Quote(m.remoteRoot), shellq.Quote(cmdLine),
		shellq.Quote(exitPath), shellq.Quote(logPath),
	)
	if err := os.WriteFile(runPath, []byte(script), 0o755); err != nil {
		_ = os.RemoveAll(jobdir)
		return proto.JobInfo{}, "", fmt.Errorf("write runner: %w", err)
	}

	meta := jobMeta{ID: id, Cmd: cmdLine, Started: time.Now().Unix()}
	var warn string

	// Durability precondition, checked before any launch mode: without
	// lingering, everything under user@UID.service (systemd jobs) and the
	// session scopes (nohup jobs) dies when the user's last session ends.
	var lingerNote string
	if !m.forceNohup {
		lingerNote = m.ensureLinger()
	}

	if !m.forceNohup && m.systemdOK() {
		unit := "lg-job-" + id
		c := exec.Command("systemd-run", "--user", "--quiet",
			"--unit="+unit, "--description=lg: "+truncate(cmdLine, 60),
			"/bin/sh", runPath)
		c.Env = m.sdEnv
		if out, rerr := c.CombinedOutput(); rerr != nil {
			// The user manager exists but refused this unit — fall back rather
			// than fail, and tell the user why durability may be reduced.
			warn = "systemd-run --user failed (" + firstLine(string(out)) + "); launched via nohup instead"
			if perr := m.startNohup(&meta, runPath); perr != nil {
				_ = os.RemoveAll(jobdir)
				return proto.JobInfo{}, "", perr
			}
		} else {
			meta.Mode, meta.Unit = "systemd", unit
		}
	} else {
		warn = "systemd --user unavailable on Source; job started detached via nohup — run `loginctl enable-linger` there for it to survive a full logout"
		if perr := m.startNohup(&meta, runPath); perr != nil {
			_ = os.RemoveAll(jobdir)
			return proto.JobInfo{}, "", perr
		}
	}

	if err := m.writeMeta(meta); err != nil {
		log.Warn("could not persist job meta", "id", id, "err", err)
	}
	warn = joinNotes(warn, lingerNote)
	log.Info("detached job started", "id", id, "mode", meta.Mode, "cmd", truncate(cmdLine, 80))
	return proto.JobInfo{ID: id, Cmd: cmdLine, State: "running", Started: meta.Started, Mode: meta.Mode}, warn, nil
}

// ensureLinger checks that the user lingers (once per agent) and turns it on
// when off — the difference between a detached job that truly survives the
// ghost disconnecting and one that dies with the last ssh session (see the
// package comment). Returns "" when lingering is already on or the host has no
// logind; otherwise a note (enabled it) or a warning (couldn't) for the user.
func (m *jobManager) ensureLinger() string {
	m.lingerOnce.Do(func() {
		usr := lingerUser()
		state := func() string {
			out, err := exec.Command(m.lingerBin, "show-user", usr, "--property=Linger", "--value").Output()
			if err != nil {
				return "" // no logind / loginctl (e.g. non-systemd host): nothing to assess
			}
			return strings.TrimSpace(string(out))
		}
		if s := state(); s != "no" {
			return // already lingering ("yes"), or unknowable — stay quiet
		}
		// Off: enable it. systemd's default polkit policy (set-self-linger)
		// lets a user do this for themselves without root.
		if exec.Command(m.lingerBin, "enable-linger", usr).Run() == nil && state() == "yes" {
			logx.For("jobs").Info("enabled lingering so detached jobs survive disconnects", "user", usr)
			m.lingerMsg = "enabled `loginctl` lingering on Source so this job survives your sessions closing"
			return
		}
		m.lingerMsg = "lingering is off for " + usr + " on Source and lg could not enable it — " +
			"this job dies when your last ssh session there ends; run `loginctl enable-linger " +
			usr + "` on Source (may need an admin)"
	})
	return m.lingerMsg
}

// lingerUser names the user for loginctl; falls back to the numeric uid, which
// loginctl accepts equally.
func lingerUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return strconv.Itoa(os.Getuid())
}

// joinNotes combines two user-facing caveats into one Warn string.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// startNohup is the fallback launcher: a new session (Setsid) so the job detaches
// from the ssh PTY, with stdio to /dev/null (the wrapper redirects to the log).
// It still lives in the ssh session's cgroup, so it needs linger to be durable.
func (m *jobManager) startNohup(meta *jobMeta, runPath string) error {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	c := exec.Command("/bin/sh", runPath)
	c.Stdin, c.Stdout, c.Stderr = devnull, devnull, devnull
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("nohup launch: %w", err)
	}
	meta.Mode = "nohup"
	meta.PID = c.Process.Pid // Setsid => pid is the session/pgid leader
	_ = c.Process.Release()  // don't wait; this agent process is short-lived
	return nil
}

// list returns every known job with its current state.
func (m *jobManager) list() []proto.JobInfo {
	m.systemdOK() // ensure sdEnv is populated even in an agent that launched nothing
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	var out []proto.JobInfo
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		meta, err := m.readMeta(e.Name())
		if err != nil {
			continue
		}
		info := proto.JobInfo{ID: meta.ID, Cmd: meta.Cmd, Started: meta.Started, Mode: meta.Mode}
		if code, done := m.exitCode(meta.ID); done {
			info.State, info.Code = "done", code
		} else if m.alive(meta) {
			info.State = "running"
		} else {
			info.State = "dead"
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
	return out
}

// act performs "kill" or "rm" on a job.
func (m *jobManager) act(id, action string) (string, error) {
	if !isJobID(id) {
		return "", fmt.Errorf("invalid job id %q", id)
	}
	meta, err := m.readMeta(id)
	if err != nil {
		return "", fmt.Errorf("no such job %q", id)
	}
	switch action {
	case "kill":
		if _, done := m.exitCode(id); done {
			return "already finished", nil
		}
		return m.kill(meta)
	case "rm":
		if _, done := m.exitCode(id); !done && m.alive(meta) {
			return "", fmt.Errorf("job %s is still running — kill it first", id)
		}
		if meta.Mode == "systemd" {
			m.systemdOK()
			// Clear the (now inactive) transient unit so the name is reusable
			// and it stops showing in `systemctl --user list-units`.
			c := exec.Command("systemctl", "--user", "reset-failed", meta.Unit)
			c.Env = m.sdEnv
			_ = c.Run()
		}
		if err := os.RemoveAll(filepath.Join(m.dir, id)); err != nil {
			return "", err
		}
		return "removed", nil
	default:
		return "", fmt.Errorf("unknown action %q (want kill|rm)", action)
	}
}

func (m *jobManager) kill(meta jobMeta) (string, error) {
	switch meta.Mode {
	case "systemd":
		m.systemdOK()
		c := exec.Command("systemctl", "--user", "stop", meta.Unit)
		c.Env = m.sdEnv
		if out, err := c.CombinedOutput(); err != nil {
			return "", fmt.Errorf("stop unit: %s", firstLine(string(out)))
		}
		return "stopped", nil
	default:
		if meta.PID <= 0 {
			return "", fmt.Errorf("no pid recorded for job %s", meta.ID)
		}
		// Setsid made the pid a session/group leader: signal the whole group so
		// children (the login shell's descendants) die too.
		_ = syscall.Kill(-meta.PID, syscall.SIGTERM)
		_ = syscall.Kill(meta.PID, syscall.SIGTERM)
		return "signalled", nil
	}
}

// serveLog streams a job's log file back to Ghost over a StreamJobLog stream.
// The first line is a JSON JobLogReq (mirroring the exec data stream's token
// line). With Follow set it tails until the job finishes (or Ghost disconnects).
func (m *jobManager) serveLog(stream io.ReadWriteCloser) {
	log := logx.For("jobs")
	defer stream.Close()
	br := bufio.NewReader(stream)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var req proto.JobLogReq
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil || !isJobID(req.ID) {
		log.Warn("bad job-log request", "line", strings.TrimSpace(line))
		return
	}
	logPath := filepath.Join(m.dir, req.ID, "log")

	// The job may have only just been launched; give the log a moment to appear.
	var f *os.File
	for i := 0; i < 20; i++ {
		if f, err = os.Open(logPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		return
	}
	defer f.Close()

	// Detect Ghost hanging up (Ctrl-C on `lg logs -f`): a read on the stream
	// returns an error once the peer closes it, so we can stop tailing promptly
	// even while no new log bytes are arriving.
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(closed)
	}()

	buf := make([]byte, 32<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := stream.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr == io.EOF {
			if !req.Follow {
				return
			}
			if _, done := m.exitCode(req.ID); done {
				return // job finished and log fully drained
			}
			select {
			case <-closed:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if rerr != nil {
			return
		}
	}
}

// --- liveness / exit code ---

// alive reports whether the job's process is still running (used only when no
// exit code has been recorded yet).
func (m *jobManager) alive(meta jobMeta) bool {
	switch meta.Mode {
	case "systemd":
		m.systemdOK()
		c := exec.Command("systemctl", "--user", "is-active", meta.Unit)
		c.Env = m.sdEnv
		out, _ := c.Output()
		s := strings.TrimSpace(string(out))
		return s == "active" || s == "activating"
	default:
		if meta.PID <= 0 {
			return false
		}
		return syscall.Kill(meta.PID, 0) == nil
	}
}

// exitCode reads the `exit` file the wrapper writes on completion. ok is false
// while the job is still running (no file yet).
func (m *jobManager) exitCode(id string) (code int, ok bool) {
	b, err := os.ReadFile(filepath.Join(m.dir, id, "exit"))
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// systemdOK probes for a reachable `systemctl --user` once and caches the result
// plus the environment needed to reach the user manager. A non-interactive ssh
// exec often lacks XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS, so we synthesize
// the standard /run/user/<uid> values when they're missing.
func (m *jobManager) systemdOK() bool {
	m.sdOnce.Do(func() {
		env := os.Environ()
		uid := os.Getuid()
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid))
		}
		if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
			env = append(env, fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid))
		}
		m.sdEnv = env
		c := exec.Command("systemctl", "--user", "show-environment")
		c.Env = env
		m.sdOK = c.Run() == nil
	})
	return m.sdOK
}

// --- meta persistence ---

func (m *jobManager) writeMeta(meta jobMeta) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.dir, meta.ID, "meta.json"), b, 0o644)
}

func (m *jobManager) readMeta(id string) (jobMeta, error) {
	var meta jobMeta
	if !isJobID(id) {
		return meta, fmt.Errorf("invalid job id")
	}
	b, err := os.ReadFile(filepath.Join(m.dir, id, "meta.json"))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// --- helpers ---

// newJobID returns 8 hex chars from crypto/rand — short enough to type, wide
// enough to avoid collisions across concurrent launches.
func newJobID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// isJobID guards path construction against a hostile/garbled id from the wire.
func isJobID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
