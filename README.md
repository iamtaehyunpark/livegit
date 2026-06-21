# Live Git (`lg`)

Real-time codebase sync + remote execution between a **Ghost** device (laptop)
and a **Source** device (GPU server). Implements the v0.2 spec.

- Edit on Ghost → propagates to Source via a journal-first async write-through.
- The moment a venv / heavy compute is needed, the shell auto-switches into a
  remote tmux session on Source.
- Local disk only holds what's actually opened (ghost → cached → evicted, LRU).

## Build

```sh
go build ./...
go build -o lg ./cmd/lg
go test ./...        # all pure logic + an in-memory Ghost↔Source integration test
```

Requires Go ≥ 1.24. Runtime extras: `tmux` on Source; a FUSE implementation on
Ghost (macFUSE on macOS, libfuse on Linux) for the actual mount.

## Quick start

```sh
# On Source (GPU server) — make `lg` available on PATH; the agent is launched
# automatically by Ghost over ssh.

# On Ghost (laptop):
lg init --role ghost --host gpu-1 --remote-root /home/u/proj --local-root ~/proj
lg shell        # mounts ~/proj, drops you into your shell with lg integration
```

Inside the shell:
- `conda activate ml` / `source .venv/bin/activate` / `python train.py` →
  auto-switch to SOURCE mode (remote tmux). Detach (Ctrl-b d) to return.
- `cat`, `ls`, `grep` etc. stay LOCAL; the FUSE layer fetches file bytes on demand.
- `lg status` — mode, file states, cache usage, journal backlog, conflicts.
- `lg sessions` — remote tmux sessions lg created.
- `lg local` — force back to LOCAL mode (escape hatch).

## Architecture (maps to the spec)

| Package | Role | Spec |
|---|---|---|
| `internal/config` | config.yaml, `.lgignore` matcher, `local↔remote` path mapper (the §7 shared helpers) | §8, §7 |
| `internal/proto` | message schema + hand-rolled `uvarint length + bytes` framing | D3 |
| `internal/transport` | native `x/crypto/ssh` + `yamux` streams, the single online flag, reconnect | D1, §6 |
| `internal/agent` | Source daemon: file server, tmux manager, PTY bridge, watcher | §3.1, §4.3, §5.3 |
| `internal/fuse` | ghost/cached/live state machine, journal-first write-through, LRU eviction, conflict backup, invalidation | §4 |
| `internal/shell` | trigger engine, LOCAL/SOURCE state machine, router, PTY bridge, zsh/bash integration | §5, D2 |
| `internal/cli` | `init`/`shell`/`serve`/`status`/`sessions`/`local`/`enter-source`/`hook` | — |

The three D1–D3 decisions and the §7 cross-cutting singletons are implemented
exactly as the spec fixed them: one SSH connection multiplexed by yamux; a
preexec-hook shell (zsh first, bash best-effort); hand-rolled length-prefixed
framing; one path mapper, one ignore matcher, one online flag, one logger.

## What is verified here vs. what needs hardware

`go test ./...` exercises, in memory, the parts that don't need a kernel mount
or a second machine:

- **Transport** — a real yamux Ghost↔Source over `net.Pipe`: framing, stream
  multiplexing, control ping/pong, and the file RPCs against the agent's file
  server (`internal/agent/integration_test.go`). This is the S1 spike as a test.
- **FUSE Backend state machine** — driven by a fake Source: ghost→cached fetch,
  journal-first write → flush → cached, conflict detection + `.lg-conflict`
  backup, LRU eviction (and that dirty/live files are never evicted), lazy vs.
  immediate invalidation, and offline accumulate → reconnect replay
  (`internal/fuse/backend_test.go`).
- **Trigger/router, ignore matcher, path mapper, framing** — unit tests.

The following compile and are wired end-to-end but require real hardware to run,
so they are **not** integration-tested in this environment:

- The actual FUSE **mount** (`go-fuse`) — needs macFUSE/libfuse installed.
- The **SSH dial** to a live Source and the `tmux` PTY **bridge** — need a real
  server + tmux.

## Known deviation from the spec

**Two SSH connections while in SOURCE mode, not one.** `lg shell` holds the
long-lived connection for the FUSE mount + journal flush; `lg enter-source`
(a separate short-lived process spawned by the shell hook) dials its own
connection for the PTY bridge. The spec's single-connection ideal would require
a per-session daemon exposing a unix socket that both the mount and the hook
processes share, relaying the PTY over it. That daemon+socket unification is the
natural next step; the current split keeps the process model simple and avoids
cross-process stream passing. The flush barrier on SOURCE entry (§5.3) works
across the two processes by polling the on-disk journal.

## SOURCE-mode exit

Primary exit is detaching the remote tmux session (Ctrl-b d), which closes the
bridge and returns to LOCAL. `lg local` force-resets the tab state. The
config'd `exit_command_map` is wired into the trigger engine for completeness,
but since SOURCE-mode keystrokes go straight to the remote tmux, lg does not see
them — detach is the reliable boundary.
