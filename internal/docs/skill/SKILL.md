---
name: livegit
description: Operate on a livegit (`lg`) project that bolts a remote server or GPU machine onto this laptop. Use whenever `.lg/` exists in the workspace, when `lg` or `livegit` is mentioned, when a repo folder is empty because `local_root` is unmounted, or when running builds, tests, or training jobs on a remote server.
---

# Live Git (`lg`) — Agent Operating Guide

`lg` bolts a remote server (a GPU box, a lab machine) onto this laptop as if the two were one device:
- **Run commands on the server:** `lg run -- <cmd>` (or bare `lg <cmd>`) executes on the server in the matching directory, streaming live with real exit codes.
- **Touch remote files locally:** `lg mount` exposes the entire remote tree as a normal local directory (`<project>/<repo-name>/`) so your native file-reading, search, and editing tools work at full speed with automatic two-way sync.
- **Run long jobs:** `lg run -d -- <cmd>` launches detached jobs under `systemd --user` that outlive disconnects, managed via `lg jobs` / `lg logs`.

## Step 0 — Orient Yourself

```sh
lg status                              # mount live? connection up? tree synced? pending writes?
lg config get source.remote_root       # remote repo path
lg config get local_root               # local mount path (or derived <project>/<repo-name>)
```

- If `lg status` says `not an lg project`: run `lg scan` to list known `lg` projects on this machine and `cd` into the right one.
- If `lg status` says `connection: down — run 'lg connect'`: ask the user to run `lg connect` (handles Duo/2FA push on their phone). Verify readiness with `lg connect --check`.

A project layout looks like:
```text
<project>/
  .lg/            config + state (config.yaml, lg.log, tree.json, cache/)
  <repo-name>/    mount point (local_root) — populated while mounted; empty otherwise
```

## Choosing the Right Tool

| Goal | Command / Action | Why |
|---|---|---|
| Check project & link health | `lg status`, `lg connect --check` | Fast, local, read-only. |
| Check environment (GPU, disk, git) | `lg run -- nvidia-smi`, `lg run -- df -h` | One round trip, real exit code. |
| Quick single-file read or grep | `lg run -- cat <path>` or `lg run -- grep -rn ...` | Leaner than mounting for a one-off. |
| **Explore or edit code seriously** (multi-file reads, searches, refactors) | **`lg mount`**, then use native Read/Edit/Write/Grep tools inside `<repo-name>/` | Full tree locally; edits sync to server in milliseconds. |
| Run/build/test after editing | `lg run -- pytest -q` (or any build/test cmd) | Built-in flush barrier waits for mount edits to land first. |
| Long training run / download | `lg run -d -- <cmd>` → `lg logs <id>` | Detached systemd job; survives disconnects. |
| Reset a broken link | `lg disconnect`, then ask user for `lg connect` | Cleanly tears down stale control master. |

**Rule of thumb:** One command → `lg run`; real work in the codebase → `lg mount`. Mixing both is normal and safe because `lg run` has a built-in flush barrier.

## Mounting & Editing Files

```sh
lg mount        # headless, idempotent — safe to run even if already mounted
# ... use native file tools on <project>/<repo-name>/ ...
lg unmount      # optional when done; leaving it mounted is fine
```

Key mechanics:
- **Metadata syncs eagerly; file content is lazy:** Directory listings are instant, and file contents download on first open. Avoid `cat`ting multi-GB checkpoints unnecessarily.
- **Automatic two-way sync:** Local edits sync to the server within milliseconds via a write-through journal (`lg status` shows any pending writes). Server-side changes appear locally within ~1 second.
- **Never create files under `<repo-name>/` while unmounted:** They become real local files that a later `lg mount` will hide. Always check `lg status` or run `lg mount` first.
- **Single-edit fallback without mounting:** See [references/troubleshooting.md](references/troubleshooting.md) for the base64 write pattern (never pipe stdin into `lg run`).

## Running Commands on the Server

```sh
lg run -- <command> [args...]     # explicit form (never ambiguous; prefer this)
lg <command> [args...]            # bare form when first word isn't an lg subcommand
```

Mechanics to respect:
- **Directory mapping:** Commands run in the server directory matching your local `cwd` (project root → `remote_root`; `<repo>/src` → `remote_root/src`). Simplest habit: stay at the project root and pass repo-relative paths.
- **Quoting:** Everything after `--` is run by `sh -lc` on the server. Protect remote globs, redirects, pipes, and `$VARS` from your local shell with single quotes:
  ```sh
  lg run -- bash -c 'for f in *.py; do wc -l "$f"; done'
  ```
- **Exit codes:** `$?` is the remote command's exit code. Don't pipe `lg` output if you need the exit code (`out=$(lg run -- ...)` or `${PIPESTATUS[0]}`).
- **Edit-then-run barrier:** `lg run` waits up to 10s for unflushed mount edits under your `cwd` to reach the server before executing.
- **No interactive TUIs/REPLs:** Avoid `vim`, `htop`, `watch`, or bare `python` REPLs in non-interactive agent shells—use non-interactive flags or edit via the mount.

## Fire-and-Forget Detached Jobs

Plain `lg run` dies if your command is interrupted or the connection drops. For long runs:

```sh
lg run -d -- python train.py --epochs 100   # prints job id, returns immediately
lg jobs                                    # id, state (running/done(0)/done(1)), age, cmd
lg logs <id>                               # output so far
lg logs -f <id>                            # follow live (Ctrl-C detaches, job keeps running)
lg jobs kill <id>                          # stop a running job
lg jobs rm <id>                            # remove a finished job + its logs
```

## Authentication Boundaries

- **Agent-safe (run freely):** `lg status`, `lg scan`, `lg connect --check`, `lg disconnect`, `lg mount`, `lg unmount`, `lg run`, `lg jobs`, `lg logs`, `lg pending`, `lg config get`.
- **Human-only on 2FA/password hosts:** `lg connect` and `lg refresh` (Duo push or password prompt). If you hit `not connected ... run 'lg connect'`, stop and ask the user to run `lg connect` in their terminal.
- On hosts with stored-password auth (no 2FA), `lg connect` is non-interactive and safe.

## Troubleshooting & Recovery

When something goes wrong—connection errors, `ENXIO` stale mounts, `flush barrier: timed out`, `lg pending` stuck writes, or missing remote agent—read [references/troubleshooting.md](references/troubleshooting.md) and check `.lg/lg.log`.
