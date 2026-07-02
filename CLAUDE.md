# CLAUDE.md — working notes for Claude Code on this repo

Live Git (`lg`): real-time codebase sync + remote execution between a **Ghost**
(laptop) and a **Source** (GPU/lab server). Single Go binary; role decided by
config. This file is the operational cheat-sheet for *working on and testing* lg.

**NOTE (Pivot v1.0):** the product was pivoted to two literal features. The old
"smart unified shell" (trigger engine, LOCAL/SOURCE mode state machine,
ghost/cached/live FUSE tri-state, persistent tmux sessions) is **deleted**.
`README.md`/`GUIDE.md` still describe the old design and are stale — trust this
file and the code. The two features now are:
1. **Command runner** — `lg <command>` runs on Source in a PTY, streams output
   live, forwards Ctrl-C/SIGWINCH, propagates the exit code. `lg toggle` makes
   every command in the shell go remote until toggled off (no heuristics).
2. **Full-tree mount** — the entire remote tree's metadata is synced eagerly
   (OneDrive-style): the whole folder is browsable immediately with real sizes;
   file content is fetched lazily on open.

## Build / test / install (exact commands)

Go is installed via Homebrew and is **not on the default PATH** in this repo's
shells. Prefix Go commands:

```sh
export PATH=/opt/homebrew/bin:$PATH
make build          # -> ./bin/lg
make test           # full suite (unit + in-memory Ghost<->Source integration)
make vet
make install        # builds + atomically installs to ~/.local/bin/lg
make release        # cross-compiles ./dist/lg-{darwin,linux}-{arm64,amd64}
```

`~/.local/bin` is on the user's PATH, so `lg` runs directly after `make install`.

### macOS code-signing trap (IMPORTANT)

Never `cp` over the installed binary in place — on Apple Silicon that causes
intermittent `SIGKILL (Code Signature Invalid)` / "Invalid Page" because the
kernel pages in mismatched code pages of a running/cached binary. The Makefile
already handles this: it builds, **re-signs** with `codesign --force --sign -`,
and installs **atomically** (temp file + `mv`/rename → fresh inode). If you ever
install by hand, do the same. Diagnose a kill with:
`codesign -v ~/.local/bin/lg` and read `~/Library/Logs/DiagnosticReports/lg-*.ips`.

The binary is self-contained: once built it runs with **no Go toolchain**. Go is
only needed to build.

## Architecture (where things live)

```
cmd/lg/main.go            entrypoint; bare-command passthrough (lg <cmd> -> remote run)
internal/config           config.yaml, .lgignore matcher, local<->remote path mapper (shared)
internal/proto            message schema + length-prefixed framing (exec, tree, file RPC, invalidate)
internal/transport        ssh dial (system OR native) + yamux streams + single online flag + reconnect
internal/agent            Source daemon (lg serve): file server, full-tree walk, exec hub (PTY),
                          job manager (jobs.go: detached systemd-run --user jobs), watcher
internal/fuse             Ghost FUSE: full-tree metadata Index (+ snapshot), lazy content fetch,
                          journal write-through (last-write-wins), size-capped cache, mount lifecycle
internal/shell            command runner (run.go: RunRemote PTY bridge), jobs.go (detach/list/logs
                          ghost client), toggle state, zsh/bash hooks
internal/cli              cobra commands (init/config/shell/run/jobs/logs/toggle/local/serve/status/
                          unmount/hook)
```

The FUSE backend's pure logic is in `internal/fuse/backend.go` + `index.go`,
tested against a fake Source (`backend_test.go`) — no mount needed. Live mount
needs **macFUSE** (installed on this machine). The agent's exec + full-tree RPCs
have an in-memory end-to-end test in `internal/agent/integration_test.go`.

## Commands

- `lg <command>` — run it on Source, stream output live (PTY, exit code). Bare
  passthrough: any first word that isn't a subcommand/flag routes to `lg run`.
- `lg run -- <command>` — explicit form (use when a command name collides with a
  subcommand, e.g. `lg run -- status`).
- `lg run --detach -- <command>` (`-d`) — launch a **detached job** that outlives
  this invocation (and the ghost disconnecting). Prints a job id and returns.
  Runs on Source under `systemd-run --user` so it escapes the ssh session cgroup
  scope that would otherwise reap it (see "Detached jobs" below). For multi-hour
  GPU runs.
- `lg jobs` — list detached jobs (id, state, mode, age, command). Subcommands:
  `lg jobs kill <id>` (stop it), `lg jobs rm <id>` (forget a finished job + logs).
- `lg logs <id>` (`-f` to follow) — show/tail a detached job's output. Following
  ends when the job finishes; Ctrl-C stops following **without** killing the job.
- `lg toggle` — flip toggle mode for the current shell tab (every command → Source).
  `lg local` is the explicit "toggle off".
- **Auto-remote commands**: in `lg shell`, a configurable list (`auto_remote_commands`,
  default `ls cat tree head tail less grep find stat wc file`) auto-runs on Source —
  without the `lg` prefix — **when the cwd is inside the mount**, falling back to the
  local command if Source is unreachable (`lg run --local-fallback`). Matched on the
  first word (basename, so `/bin/ls` matches `ls`). Escape to local with `\ls` or
  `command ls`. Set `auto_remote_commands: []` to disable. The zsh `accept-line`
  widget / bash DEBUG trap (`internal/shell/integration.go`) do the rewrite; the list
  is baked into the generated hook at `lg shell` start.
- `lg init` — interactive setup wizard (flags also work; `-i` forces wizard).
- `lg config get|set|edit|show|path` — change settings safely (validates before save).
- `lg shell` — mount the full-tree FUSE folder + run the user's shell (toggle hooks).
- `lg unmount` — clear a leftover/stale FUSE mount.
- `lg status` — connection, toggle on/off, tree-sync freshness, cache, pending writes.
- `lg serve --remote-root <p> [--ignore <csv>]` — Source agent (hidden; launched over ssh).

## Detached jobs (fire-and-forget remote runs)

`lg run` opens a **fresh ssh session + fresh `lg serve` per command** and kills
it on return (`cli/run.go`: `defer client.Close()`). That ends the remote ssh
login session, and systemd tears down the session's cgroup scope
(`KillUserProcesses=yes`, the lab default) — killing *everything* spawned in it:
`nohup`, `setsid`, even a detached `tmux` server. So a plain backgrounded job
cannot be left behind through `lg run`.

**lg is NOT the reaper** — don't go looking for a process-group kill to soften.
`internal/agent/exec.go` only does `cmd.Wait()` + `ptmx.Close()`. The reaper is
systemd tearing down the ephemeral ssh session scope. Evidence it's cgroup
teardown and not a signal: `nohup` (ignores SIGHUP) and `setsid` (leaves the tty)
both die anyway, while `systemd-run --user` (a different cgroup branch,
`user@UID.service`) survives.

So detached jobs launch via **`systemd-run --user`** to escape the session
scope. Design (`internal/agent/jobs.go`):
- **systemd is the source of truth for liveness; an on-disk `~/.lg/jobs/<id>/`
  dir on Source is the source of truth for identity/logs.** State is NOT held in
  the agent — each `lg run --detach` spawns a short-lived agent that launches the
  job and dies; `lg jobs`/`lg logs` run in *later* agents that reconstruct
  everything from systemd + the jobs dir. No cross-agent shared memory.
- Each job dir holds `meta.json` (id/cmd/mode/unit-or-pid/started), `run.sh` (a
  wrapper: `sh -lc <cmd>` for PATH/conda parity, capturing `$?` to an `exit`
  file), and `log` (combined output). State = done(code) if the `exit` file
  exists, else running if systemd/pid is alive, else dead.
- Fallback where systemd --user is unavailable: `setsid`+nohup (best effort — it
  still lives in the session scope, so it needs `loginctl enable-linger` to be
  durable; this is reported to the user as a warning). The agent sets
  `XDG_RUNTIME_DIR`/`DBUS_SESSION_BUS_ADDRESS` to `/run/user/<uid>` when missing
  so `systemctl --user` is reachable from a non-interactive ssh exec.
- Wire protocol: `TypeJobStart/List/Act` RPCs on the **control stream** (like
  ping); log tailing on a dedicated `StreamJobLog` stream (like the PTY data
  stream — first line is a JSON `JobLogReq`).

## Two transports (D1 revisited)

`source.ssh_mode`:
- **`system`** (default): shells out to the real `ssh` binary to run `lg serve`
  on Source. Honors `~/.ssh/config` fully — Host aliases, ProxyJump/bastions,
  **ControlMaster (Duo/2FA reused, not re-prompted)**, IdentityFile, known_hosts.
  Required for lab/2FA servers.
- **`native`**: built-in Go ssh client; ignores `~/.ssh/config`. Needs the host
  key in `~/.ssh/known_hosts`.

### Password auth + agent auto-deploy

`source.auth: password` (forces native mode) uses a password stored **encrypted**
at `<project>/.lg/credentials` (AES-GCM, key derived from the machine id via
`ioreg`/`/etc/machine-id` — copying the file to another machine won't decrypt;
`internal/config/secret.go`). The system-`ssh` path can't answer a prompt from
lg's non-interactive launch, so password hosts must use native. `authMethods`
offers both `ssh.Password` and `ssh.KeyboardInteractive` (many servers deliver
"password" as keyboard-interactive). `lg init --auth password` prompts (hidden,
never in argv) and stores it.

`lg init` also **confirms/deploys the agent**: `transport.EnsureAgent`
(`internal/transport/deploy.go`) connects, checks for `lg` on the remote, and if
missing pipes the matching embedded Linux binary to `~/.local/bin/lg` (no scp/
sftp — streamed over an ssh session). The Linux agents are embedded via
`internal/agentbin` (`//go:embed all:data`); `make agents` (run by `make build`)
cross-compiles them from the same source, so the deployed agent always matches
this build's protocol. Plain `go build`/`go test` embed nothing (data/ has only
`.gitkeep`) → deploy degrades to printing a manual `scp` command. `agent_bin`
stays `"lg"` (resolved by the PATH-prefix in `remoteAgentCmd`).

## Live test environment (already set up)

The user's real Source is **galaxy-04** (UW–Madison CS). Both sides currently run
the same build — **keep them in sync** (protocol must match): after changing lg,
`make release && scp dist/lg-linux-amd64 galaxy-04:.local/bin/lg`.

```
Ghost (this Mac):  ~/.local/bin/lg   (the binary)
Source (galaxy-04): /home/tpark45/.local/bin/lg   remote_root=/home/tpark45/two-stage-stitcher
config: host=tpark45@galaxy-04.cs.wisc.edu  user=tpark45  ssh_mode=system
        agent_bin=/home/tpark45/.local/bin/lg   <-- ABSOLUTE: galaxy's non-interactive
        PATH lacks ~/.local/bin, so a bare "lg" would not be found.
```

### Config is project-local (per-directory)

lg is project-local, like git. `lg init` in a directory writes `<dir>/.lg/
config.yaml` and ALL per-project state lives under that `.lg/` (journal, cache,
`tree.json`, `lg.log`, hooks, run). Any lg command **discovers the nearest `.lg/`
by walking up from the cwd** (project-only — no global config; outside a project
it errors "not an lg project"). The remote tree mounts at a sibling dir **named
exactly after the Source repo** (basename of remote_root, e.g. `<project>/two-
stage-stitcher/`) next to `.lg/` — no mount-name option (FUSE can't mount over
the project root without hiding `.lg/`). `lg <cmd>` runs in the remote dir
matching your cwd: under `<mount>/a/b/c`, `lg ls` runs in `remote_root/a/b/c`
(relDir from `os.Getwd` via `PathMapper.LocalToRel` → `ExecReq.Cwd`).
`config.Dir()` is the single resolver (`internal/config/config.go`): it returns
`$LG_HOME` if set (tests/escape hatch, read fresh — never cached), else the
discovered `.lg/`. `lg init` uses `config.InitDir()` = `<cwd>/.lg` to bypass
discovery. So per-project paths "just work" everywhere via `config.Dir()`.

To live-test, `cd` into a project dir first (or `export LG_HOME=<some .lg dir>`).
There is no longer a single `~/.lg/config.yaml`.

### Testing against galaxy WITHOUT triggering Duo

The user has a persistent ControlMaster (`~/.ssh/cm-%r@%h:%p`, ControlPersist yes).
Reuse it; never open a fresh auth that would push Duo.

```sh
ssh -O check galaxy-04                       # is a master live? ("Master running")
ssh -o BatchMode=yes galaxy-04 'uname -sm'   # runs over the master, no prompt
ssh -o BatchMode=yes galaxy-04 '/home/tpark45/.local/bin/lg --version'
```

If no master is live, do NOT auto-connect — ask the user to bring one up (their
shell login automation does), because the first connect needs a Duo push.

### Verified end-to-end against galaxy (2026-06-30, Pivot v1.0)

All core workflows tested live against galaxy-04 and pass:
- **Command runner** — `lg pwd` → remote root; `lg false`→1, `lg run -- sh -c
  'exit 7'`→7 (exit codes propagate); `lg ls -la` (bare passthrough) streams;
  incremental streaming (lines arrive 1s apart, not buffered); PTY allocated.
- **Full-tree mount** — `lg shell` mounts; the entire tree is browsable
  immediately with **real sizes on unopened files**; opening a file materializes
  content on demand; `exit` unmounts cleanly.
- **Sync both ways** — local edit → galaxy (write-through journal); galaxy edit →
  mount (watcher invalidation updates the index).
- **Tree-sync ignore** — propagating Ghost's `ignore` patterns to the agent cut
  the initial walk from 32673 entries/~32s to 3196/~2.4s (skips `.venv`). The
  agent honors `--ignore <csv>` (sent automatically by the Ghost dialer).

Bug this live testing found (not caught by the first unit pass, then covered by a
test): the exec hub deadlocked — it waited for the stdin-copy goroutine to finish
before sending `ExecExit`, but stdin only ends when the client closes the data
stream, and the client keeps it open until it sees `ExecExit`. Fix: drive the
exit off the **output/process** side (`cmd.Wait` after the pty EOFs), never the
stdin side. See `internal/agent/exec.go` `serveData`.

### How to drive `lg shell` head-less (the test harness)

`lg shell` is interactive (mounts FUSE + execs a shell), but the **mount is a
real filesystem** — so the simplest test is: start `lg shell` in a tmux pane,
then verify the mount from a *separate* process (`ls`/`cat` the mountpoint
directly). No need to send keys into the lg shell at all.

```sh
# CRITICAL: start the pane shell as bash, NOT the user's zsh. `new-session`
# without a command launches the login shell (zsh), whose ~/.zshrc runs a
# galaxy-01..05 Duo/ControlMaster automation BEFORE your typed command — that
# can push Duo. Starting the pane as bash --norc avoids it entirely.
tmux -L lgtest new-session -d -s s -x 200 -y 50 '/bin/bash --norc --noprofile'
tmux -L lgtest send-keys -t s 'SHELL=/bin/bash /path/to/bin/lg shell' Enter
sleep 10                                   # wait for connect + tree sync (watch lg.log)
ls -la  /Users/t/lg/two-stage-stitcher     # verify the full tree from THIS process
head -5 /Users/t/lg/two-stage-stitcher/README.md   # materialize content on open
tmux -L lgtest send-keys -t s 'exit' Enter # clean unmount
```

`SHELL=/bin/bash` makes lg's *child* shell bash (its own `--rcfile`, no zsh
automation). Watch sync progress in `~/.lg/lg.log` (`tree synced entries=N`).
Command-runner tests (`lg run -- ...`, `lg <cmd>`) are non-interactive and need
no tmux at all — run them directly.

Always clean up: `tmux -L lgtest kill-server; pkill -9 -f 'lg shell';
lg unmount; ssh galaxy-04 'pkill -f "lg serve"'`.

### What you can and can't test non-interactively

- ✅ `go test ./...` — all logic incl. a real yamux Ghost<->Source over net.Pipe
  (exec round-trip, full-tree RPC, file RPC, framing).
- ✅ Command runner end-to-end: `lg run -- <cmd>` / `lg <cmd>` (reuses the master).
- ✅ Remote agent reachable: `ssh -o BatchMode=yes galaxy-04 '<agent_bin> --version'`.
- ✅ Config/CLI surface: `lg config show`, `lg status`.
- ⚠️ `lg shell` mount: drive via the tmux+separate-process recipe above, or ask
  the user to run it. Logs go to `~/.lg/lg.log`.

## Recovering a wedged state

- Stuck `lg shell` / won't stop: `pkill -f 'lg shell'` then `lg unmount`.
- Stale mount (ENXIO / "device not configured" touching local_root): `lg unmount`
  (or `umount -f <local_root>`). `lg shell` auto-recovers these on start.
- Logs/state are per-project under `<project>/.lg/`: `lg.log`, `journal.log`,
  `tree.json`, `cache/`. (Find the project with `lg config path`.)

## House rules

- This is a personal tool for one user; keep changes simple and well-commented.
- Don't trigger a Duo push without the user expecting it (see above).
- `local_root` must be an **empty** dir (FUSE mounts Source's tree over it).
- Commit style: end messages with the Co-Authored-By trailer. Work on a branch
  (current: `feat/livegit-v0.2`), push when the user asks.
- After any protocol/transport change, redeploy the Linux binary to galaxy so the
  two sides stay on the same build.

## Known gaps / TODO

- A `make deploy-source` one-step redeploy of the Linux binary to Source.
- A `lg doctor` that checks config + ssh reachability + remote agent + remote_root
  + macFUSE + stale mounts in one non-interactive command (great for agent tests).
- Full-tree sync ships one whole `TreeResp` snapshot (fine for typical repos under
  the 256 MiB frame cap); page it if a repo is huge.
- `.git` is still synced into the mount (only `ignore` patterns are skipped). Add
  `.git/` to config `ignore` if you want it out — git ops should run via `lg <cmd>`.
- `lg toggle` uses the zsh/bash preexec hook; the bash DEBUG-trap path is
  best-effort (zsh is the reliable one). Toggle state is `~/.lg/run/<tab>.toggle`.
- `README.md`/`GUIDE.md` still describe the pre-pivot product — rewrite them.
