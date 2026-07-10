# Using `lg` as an agent — operating guide

`lg` (Live Git) bolts a remote server — a GPU box, a lab machine — onto this
laptop **as if the two were one device**. The server's repo is simultaneously:

- a place you can **run things**: `lg <command>` executes on the server, in the
  right directory, streaming live, with real exit codes — ssh without the
  ceremony;
- a folder you can **touch**: `lg mount` makes the entire remote tree a normal
  local directory — browse it, grep it, edit it with your native file tools,
  and every change syncs both ways automatically;
- a machine that **keeps working when you leave**: `lg run -d` launches jobs
  that outlive your session, with `lg jobs` / `lg logs -f` to manage them.

Treat it the way you treat git: a tool you've known for years. One cached,
authenticated connection carries everything; you never think about ssh again.
This file is self-contained — read it once and you know the whole surface.

---

## Choosing the right tool (the decision that matters)

| You want to… | Use | Why |
|---|---|---|
| Run/build/test something on the server | `lg <cmd>` / `lg run -- <cmd>` | One round trip, live stream, real exit code. |
| Check the environment (GPU, disk, processes) | `lg nvidia-smi`, `lg df -h`, … | Same. |
| Read one file / quick search | `lg run -- cat/grep/sed …` | Cheaper than mounting for a one-off. |
| **Explore or edit code seriously** (multi-file reads, refactors, reviews) | **`lg mount`, then your native Read/Edit/Write/Grep tools on the mount** | The whole tree becomes local files — your best tools work at full power; edits sync back in milliseconds. |
| Start a long training run / anything that must survive disconnects | `lg run -d -- <cmd>` → `lg logs -f <id>` | Detached job on the server; your session (and laptop) can die. |
| Watch a running job | `lg logs -f <id>` (Ctrl-C stops *following*, never the job) | |
| Check project/connection health | `lg status`, `lg connect --check` | Both safe, read-only. |
| Recover / reset the link | `lg disconnect`, then human runs `lg connect` | See [Authentication](#authentication--whats-yours-whats-human). |

Rule of thumb: **one command → `lg run`; real work in the codebase → mount it.**
Mixing is normal: mount for editing while firing `lg run -- pytest` for
execution — lg guarantees your just-saved edits reach the server before the
command runs (a built-in flush barrier).

---

## Step 0 — Orient yourself

```sh
cd <project-dir>       # any dir at/under one containing .lg/ (like .git/)
lg status              # mount live? connection up? tree synced? pending writes?
```

- `not an lg project` → wrong directory. `lg scan` lists every lg project on
  this machine with its connection state; `cd` into one.
- `connection: down — run 'lg connect'` → see
  [Authentication](#authentication--whats-yours-whats-human).

A project looks like:

```
<project>/
  .lg/            config + state (config.yaml, lg.log, tree.json, cache/)
  <repo-name>/    the mount point (local_root) — the server's tree appears
                  here while mounted; empty otherwise
```

`lg config get source.remote_root` / `lg config get local_root` print the two
sides of the mapping.

---

## Running commands (ssh, minus the ssh)

```sh
lg <command> [args…]           # bare form — any first word that isn't an lg subcommand
lg run -- <command> [args…]    # explicit form — never ambiguous; prefer in scripts
```

```sh
lg run -- python -c 'import torch; print(torch.cuda.is_available())'
lg run -- pytest -q ; echo "exit: $?"        # exit codes propagate unchanged
lg run -- git status
lg nvidia-smi
```

Mechanics worth knowing:

- **Directory mapping.** The command runs in the server directory matching your
  local cwd: at the project root or above → `remote_root`; inside the mount at
  `<repo>/src/models` → `remote_root/src/models`. Simplest habit: stay at the
  project root and pass repo-relative paths.
- **Quoting.** Everything after `--` is reassembled and run by a login shell
  (`sh -lc`) on the server, so remote globs/redirects/`$VARS` work — protect
  them from *your* shell with single quotes:
  `lg run -- bash -c 'for f in *.py; do wc -l "$f"; done'`
- **Exit codes.** `$?` is the remote command's code. Don't pipe `lg`'s output
  and then read `$?` (that's the filter's code) — use `${PIPESTATUS[0]}` or
  capture first: `out=$(lg run -- …)`.
- **Streaming & interrupts.** Output streams live (it's a real PTY). Give long
  commands a generous tool timeout; interrupting `lg` interrupts the remote
  process, exactly like ssh.
- **Edit-then-run is ordered.** `lg run` waits (up to 10s) for any unflushed
  mount edits under your cwd to land on the server first. Save, run, trust it.
- **Interactive programs** (`vim`, `htop`, REPLs, `watch`) run fine but expect
  a responding terminal — under an agent's non-interactive shell they'll sit
  waiting forever. Use their non-interactive forms (`sed`/`ls`/`nvidia-smi`;
  `python script.py` over a REPL), or edit through the mount instead of remote
  vim. If you genuinely control a terminal (e.g. a tmux pane you can send keys
  to), nothing stops you from running them there.

---

## The mount — the server's tree as local files

This is lg's superpower, and it is fully yours to use:

```sh
lg mount        # headless: mounts <project>/<repo-name>/, returns immediately
                # (idempotent — safe to run when already mounted)
…work…          # your native file tools, on real paths
lg unmount      # done (also fine to leave mounted for the next task)
```

While mounted:

- **The whole tree is browsable instantly** — names, sizes, structure at every
  depth (metadata syncs eagerly; file *content* downloads on first open, so
  don't cat a 5 GB checkpoint without meaning it).
- **Your edits sync to the server automatically** within milliseconds
  (journaled write-through; `lg status` shows anything still pending). Create,
  rename, delete — git-style atomic saves all work.
- **Server-side changes appear locally** within ~a second (a watcher pushes
  invalidations; your next read refetches).
- **Verify like a skeptic if you like**: `lg run -- sha256sum path` vs a local
  hash — but the flush barrier already makes save→run safe.

When to mount vs not: mounting costs one command and gives your file tools
native, full-speed access — reach for it whenever a task touches more than a
file or two. For a single read or a log grep, `lg run -- cat/grep` is leaner.

Practical notes:

- `lg mount` needs the connection up; if it isn't, it fails fast with
  `run 'lg connect'` — that's a [human step](#authentication--whats-yours-whats-human)
  on 2FA hosts.
- `lg shell` is the interactive sibling (mount + a human's shell with extras
  like per-tab toggle mode). It expects a terminal; as an agent you get
  everything you need from `lg mount` — but if you're driving a real terminal
  (tmux), `lg shell` works there too. If a mount is already live (yours or a
  human's `lg shell`), just use it; lg refuses double mounts with a clear
  message.
- Don't create files under `local_root` while it's **not** mounted — they'd be
  real local files that a later mount hides.

### Editing without a mount (fallback)

If you choose not to mount (single surgical edit), write whole files via a
base64 argument — robust for any content, no stdin involved (the remote PTY
doesn't signal EOF cleanly, so never pipe into `lg run`):

```sh
B64=$(printf '%s' "$NEW_CONTENT" | base64)
lg run -- bash -c "printf %s '$B64' | base64 -d > 'path/to/file'"
lg run -- cat path/to/file    # verify
```

---

## Fire-and-forget jobs (long runs that outlive you)

Plain `lg run` ties the remote process to your invocation — Ctrl-C or a dropped
session ends it. For anything long (training, sweeps, downloads):

```sh
lg run -d -- python train.py --epochs 100     # prints a job id, returns at once
lg jobs                                       # id, state, mode, age, command
lg logs <id>                                  # output so far
lg logs -f <id>                               # follow live; Ctrl-C detaches, job keeps running
lg jobs kill <id>                             # stop it
lg jobs rm <id>                               # forget a finished job + its logs
```

Jobs run on the server under `systemd --user`, so they survive your disconnect,
your laptop sleeping, even the connection window expiring. lg also switches on
`loginctl` lingering for the user when it's off — without it the server reaps
every background unit once the last ssh session ends. (Where systemd or
lingering isn't available lg falls back / warns — surface any start-time
warning to the human.) Exit codes are recorded: `lg jobs` shows `done(0)` /
`done(1)`.

Pattern for an agent: start the job detached, poll `lg jobs` / tail
`lg logs <id>` between other work, report the exit code when it lands.

---

## Authentication — what's yours, what's human

lg authenticates **once**, caches the connection (hours; `lg status` shows it),
and every command reuses it. Your normal state is "it just works."

- **Safe for you, always:** every `lg <cmd>`, `lg status`, `lg connect --check`,
  `lg disconnect`, `lg mount`/`lg unmount` (they fail fast rather than prompt).
- **Human steps:** anything that can pop an interactive prompt you cannot
  answer — a Duo push lands on a *phone*. That's `lg connect` and `lg refresh`
  on 2FA hosts, and any password entry. When you hit
  `not connected … run 'lg connect'`, ask the human to run `lg connect`; one
  approval buys hours of your commands working. Check readiness anytime with
  `lg connect --check`.
- On hosts with stored-password auth (no 2FA), nothing is interactive — even
  `lg connect` is just a credential test you may run freely.

---

## Setting up a project (only when there's no `.lg/` yet)

```sh
cd <new-project-dir>
lg init --role ghost --host <ssh-host> --user <user> --remote-root <abs-repo-path> --yes
```

That writes `.lg/config.yaml`, creates the mount point (named after the repo),
auto-installs the `lg` agent on the server, and drops this guide + GUIDE.md.
Password (`--auth password`) or 2FA (`--two-factor`) setups involve prompts —
hand those to the human. Settings change safely later via
`lg config set <key> <value>`.

---

## Failures → diagnosis → fix

| Symptom | Meaning | Fix |
|---|---|---|
| `not an lg project` | cwd isn't under a `.lg/`. | `lg scan` to find projects; `cd` there. Don't `lg init` over nothing unless you mean to create one. |
| `not connected … run 'lg connect'` | No authenticated connection and lg can't prompt. | Human runs `lg connect` (Duo). Verify with `lg connect --check`. |
| `… interactive second authentication step (Duo/2FA)` | Project is in native mode but the host wants Duo. | Run the two printed commands (`lg config set source.ssh_mode system`, human runs `lg connect`). |
| `mount didn't come up — see …/lg.log` | FUSE couldn't start (macFUSE/libfuse missing?) or connection died mid-mount. | Read the log; report to the human if FUSE itself is absent. |
| `already mounted — an lg shell or lg mount is active` | The tree is being served. | That's success — just use the mounted folder. |
| ENXIO / "device not configured" touching the mount | Stale mount from a killed holder. | `lg unmount`, then remount. (`lg mount`/`lg shell` also auto-recover this.) |
| A `lg run` command never returns | The remote program is waiting for interactive input. | Interrupt it; rerun non-interactively, or do the work through the mount. |
| `flush barrier: timed out (continuing)` | A mount edit hadn't reached the server in 10s (offline?). | Check `lg status` (journal pending? connection?); rerun once it drains. |
| log shows `lg: command not found` (remote) | Agent binary missing on the server. | `lg connect` (human, if 2FA) auto-deploys/upgrades it; or `lg init` again. |
| `Permission denied` on a remote path | Server-side Unix perms. | Not an lg problem — report it. |

`<project>/.lg/lg.log` is the primary diagnostic for anything
connection-shaped. `lg config path` locates it.

---

## Quick reference

```sh
# orient
lg status ; lg scan
lg config get source.remote_root ; lg config get local_root

# run (ssh, minus the ssh)
lg run -- <cmd>                    # exit code propagates; streams live
out=$(lg run -- cat path/file)     # capture
lg run -- grep -rn PATTERN .       # search without mounting

# real work in the codebase
lg mount                           # tree appears at local_root — use native tools
lg unmount                         # when done (leaving it mounted is fine too)

# long jobs
lg run -d -- <cmd>  →  lg jobs / lg logs -f <id> / lg jobs kill <id>

# connection (safe: status/--check/disconnect; Duo prompts are human steps)
lg connect --check ; lg disconnect
```

**The philosophy in one line: the GPU box is bolted onto this laptop. Run
things with `lg <cmd>` as if the server were a faster core; touch its files
via `lg mount` as if its disk were yours; leave long jobs behind with
`lg run -d` as if it kept working after you closed the lid — because it does.**
