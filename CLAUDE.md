# CLAUDE.md — working notes for Claude Code on this repo

Live Git (`lg`): real-time codebase sync + remote execution between a **Ghost**
(laptop) and a **Source** (GPU/lab server). Single Go binary; role decided by
config. Read `README.md` for the design and `GUIDE.md` for the user-facing flow.
This file is the operational cheat-sheet for *working on and testing* lg.

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
cmd/lg/main.go            entrypoint
internal/config           config.yaml, .lgignore matcher, local<->remote path mapper (§7 shared)
internal/proto            message schema + length-prefixed framing (D3)
internal/transport        ssh dial (system OR native) + yamux streams + single online flag + reconnect
internal/agent            Source daemon (lg serve): file server, tmux mgr, PTY bridge, watcher
internal/fuse             Ghost FUSE: ghost/cached/live state machine, journal-first write-through,
                          LRU eviction, conflict backup, invalidation, mount lifecycle
internal/shell            trigger engine, LOCAL/SOURCE state machine, router, PTY bridge, zsh/bash hooks
internal/cli              cobra commands (init/config/shell/serve/status/sessions/local/unmount/enter-source/hook)
```

The FUSE state machine's pure logic is in `internal/fuse/backend.go` and is fully
tested against a fake Source (`backend_test.go`) — no mount needed. Live mount
needs **macFUSE** (installed on this machine).

## Commands

- `lg init` — interactive setup wizard (flags also work; `-i` forces wizard).
- `lg config get|set|edit|show|path` — change settings safely (validates before save).
- `lg shell` — mount FUSE + run the user's shell with trigger integration.
- `lg unmount` — clear a leftover/stale FUSE mount.
- `lg status` / `lg sessions` / `lg local`.
- `lg serve` — Source-side agent (hidden; launched over ssh by Ghost).

## Two transports (D1 revisited)

`source.ssh_mode`:
- **`system`** (default): shells out to the real `ssh` binary to run `lg serve`
  on Source. Honors `~/.ssh/config` fully — Host aliases, ProxyJump/bastions,
  **ControlMaster (Duo/2FA reused, not re-prompted)**, IdentityFile, known_hosts.
  Required for lab/2FA servers.
- **`native`**: built-in Go ssh client; ignores `~/.ssh/config`. Needs the host
  key in `~/.ssh/known_hosts`.

## Live test environment (already set up)

The user's real Source is **galaxy-04** (UW–Madison CS). Both sides currently run
the same build — **keep them in sync** (protocol must match): after changing lg,
`make release && scp dist/lg-linux-amd64 galaxy-04:.local/bin/lg`.

```
Ghost (this Mac):  ~/.local/bin/lg   local_root=/Users/t/lg/two-stage-stitcher (empty mount point)
Source (galaxy-04): /home/tpark45/.local/bin/lg   remote_root=/home/tpark45/two-stage-stitcher
config: host=tpark45@galaxy-04.cs.wisc.edu  user=tpark45  ssh_mode=system
        agent_bin=/home/tpark45/.local/bin/lg   <-- ABSOLUTE: galaxy's non-interactive
        PATH lacks ~/.local/bin, so a bare "lg" would not be found.
```

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

### Verified end-to-end against galaxy (2026-06-21)

All core workflows were tested live against galaxy-04 and pass: browse + lazy
read (ghost→cached), write-through (local edit → galaxy), reverse propagation
(galaxy edit → mount), and SOURCE mode (auto-trigger → bridge to a tmux session
on the A100 box → detach → session persists). LOCAL commands stay local; `exit`
unmounts cleanly.

Real bugs this live testing found and fixed (none were caught by unit tests):
- tmux socket must be on LOCAL disk, not `~/.lg` — lab homes are **AFS/NFS** and
  Unix sockets fail there ("new-session -d" succeeds but no server persists).
  Agent now uses `/tmp/lg-tmux-<uid>.sock`.
- Forward Ghost's `$TERM` to the agent or `tmux attach` dies with "open terminal
  failed: terminal does not support clear".
- `lg shell` could leave a stale mount on exit (signal skips the defer). Now it
  also unmounts on SIGHUP/SIGTERM and force-unmounts as a backstop.
- Directory-marker triggers fired for nearly every command (the "absent on
  Ghost" half is always true for ignored markers). Disabled until a Source-side
  presence check exists; explicit patterns (conda/venv/poetry/python) are the
  reliable path.

### How to drive the interactive shell head-less (the test harness)

Run `lg shell` inside an isolated tmux server and send keys / capture the pane:

```sh
tmux -L lgtest new-session -d -s s -x 200 -y 50
tmux -L lgtest send-keys -t s 'SHELL=/bin/bash lg shell' Enter   # see note below
tmux -L lgtest send-keys -t s 'cat README.md' Enter
tmux -L lgtest capture-pane -t s -p | tail -20
```

IMPORTANT: launch with `SHELL=/bin/bash`. The user's `~/.zshrc` runs a
galaxy-01..05 Duo/ControlMaster automation on every interactive zsh; `lg shell`
sources it, which blocks and can push Duo. bash uses lg's own `--rcfile`
(no zsh automation). The FUSE mount/sync is shell-agnostic, so you test the same
behavior. To send a detach (Ctrl-b d) to the REMOTE tmux through the bridge,
`send-keys` injects directly into the pane: `send-keys -t s C-b` then `d`.

Always clean up after: `pkill -9 -f 'lg shell'; umount -f <local_root>;
ssh galaxy-04 'pkill -f "lg serve"; tmux -S /tmp/lg-tmux-$(id -u).sock kill-server'`.

### What you can and can't test non-interactively

- ✅ `go test ./...` — all logic incl. a real yamux Ghost<->Source over net.Pipe.
- ✅ Remote agent reachable: `ssh galaxy-04 '<agent_bin> serve --help'`.
- ✅ Config/CLI surface: `lg config show`, `lg status`, `lg hook check ...`.
- ⚠️ `lg shell` and `lg enter-source` are **interactive** (they exec a shell /
  bridge a PTY) — can't be driven head-less. Verify their pieces via the unit
  tests and the live checks above; ask the user to run `lg shell` for the real
  end-to-end. Logs go to `~/.lg/lg.log`.

## Recovering a wedged state

- Stuck `lg shell` spamming / won't stop: `pkill -f 'lg shell'` then `lg unmount`.
- Stale mount (ENXIO / "device not configured" touching local_root): `lg unmount`
  (or `umount -f <local_root>`). `lg shell` now auto-recovers these on start.
- Logs: `~/.lg/lg.log`. State db: `~/.lg/state.db`. Journal: `~/.lg/journal.log`.

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
- In SOURCE mode there are two SSH connections (shell holds one for FUSE,
  enter-source dials its own for the PTY); unifying via a per-session daemon +
  socket is future work (see README "Known deviation").
