# Using `lg` as an agent — operating guide

You are an AI coding agent working in a shell. `lg` (Live Git) lets you run
commands and work with code **on a remote server** (a GPU box / lab machine)
from this local machine, over one invisible, always-authenticated connection.
This guide is a harness: follow it and you can drive `lg` as if the remote
server were local — flawlessly, non-interactively.

Read this whole file once. It is self-contained.

---

## The three golden rules

1. **Run remote commands with `lg <command>`** (or `lg run -- <command>`). That's
   the primitive for everything you'd do on the server: run code, tests, git,
   read/list/search files, inspect the environment.
2. **NEVER run `lg shell`, `lg toggle`, or `lg vim`/`lg htop`/other full-screen
   or interactive programs.** `lg shell` starts an interactive shell and will
   **hang you forever**. Interactive TUIs need a human. Use non-interactive
   commands only (`lg cat`, `lg run -- sed -n …`, `lg python script.py`).
3. **You must be inside an lg project.** Commands work from the project directory
   or any subdirectory (config is discovered by walking up to the nearest
   `.lg/`, like git's `.git/`). If you're outside one, `lg` errors "not an lg
   project" — `cd` into the project first.

Output is clean (remote diagnostics go to a log file, not your terminal) and
**exit codes propagate**, so `lg <cmd>` composes in scripts exactly like a local
command.

---

## Step 0 — Orient yourself

Before doing anything, confirm the project and connection:

```sh
cd <project-dir>          # a dir that contains .lg/ (or is under one)
lg status                 # role, mount path, connection, tree freshness
lg config get source.remote_root    # absolute repo path on the server
lg config get local_root            # the local mount folder (named after the repo)
```

If `lg status` / any `lg <cmd>` prints `not an lg project`, you are in the wrong
directory — find the project (look for a `.lg/` dir) and `cd` there. `lg scan`
lists every lg project on the machine and its connection state, which is a quick
way to locate one. If a command prints `not connected …`, see
[Failures](#failures--diagnosis--fix).

A project looks like:
```
<project>/
  .lg/              config + state  (config.yaml, lg.log, tree.json, cache/)
  <repo-name>/      the mount folder (local_root) — only populated while a
                    human runs `lg shell`; usually EMPTY for you.
```

---

## Running commands on the server

```sh
lg <command> [args...]        # bare form — first word must not be an lg subcommand
lg run -- <command> [args...] # explicit form — always safe, no ambiguity
```

Prefer `lg run -- <command>` in scripts: it never collides with lg's own
subcommands (`status`, `config`, `run`, `init`, `shell`, `toggle`, `local`,
`unmount`, `help`, `completion`). Use bare `lg <cmd>` only for clearly-not-a-
subcommand programs (`lg pytest`, `lg python …`, `lg nvidia-smi`).

**Examples:**
```sh
lg run -- pwd
lg run -- ls -la src/
lg run -- python -c 'import torch; print(torch.cuda.is_available())'
lg run -- pytest -q
lg run -- git status
lg run -- nvidia-smi
```

**Exit codes** come back unchanged:
```sh
lg run -- pytest -q ; echo "tests exit: $?"
lg run -- test -f config.yaml && echo "exists on server"
```
Do NOT pipe `lg`'s output through a filter and then read `$?` — `$?` would be the
filter's exit code, not the remote command's. If you must pipe, use
`${PIPESTATUS[0]}` (bash) to get lg's code.

**Long-running commands** stream live. Give your shell tool a large timeout for
training/downloads; interrupting your local `lg` sends the interrupt to the
remote process too (like ssh).

**Quoting:** everything after `lg run --` is reassembled into one command line
and run by a login shell (`sh -lc`) on the server, so remote globs/redirects/env
work. Quote for YOUR local shell as usual; use single quotes to keep the remote
shell from expanding things locally:
```sh
lg run -- bash -c 'cd subdir && for f in *.py; do wc -l "$f"; done'
```

---

## Directory mapping (important)

The command runs in the **server directory that corresponds to your current
local directory**, relative to the mount root (`local_root` → `remote_root`):

| Your local cwd | Runs on server in |
|---|---|
| `<project>/` (at/above the mount) | `<remote_root>` (repo root) |
| `<project>/<repo>/` (mount root) | `<remote_root>` |
| `<project>/<repo>/src/models` | `<remote_root>/src/models` |

So to run something in a specific server subdirectory, either `cd` into the
matching local path first, or just pass the path in the command
(`lg run -- ls src/models`). When in doubt, pass explicit paths relative to the
repo root and stay at the project root.

---

## Reading & searching server files

Without a mount (your normal situation), use commands:

```sh
lg run -- cat path/to/file.py                 # whole file
lg run -- sed -n '1,80p' path/to/file.py      # a line range
lg run -- wc -l path/to/file.py
lg run -- ls -la path/to/dir
lg run -- grep -rn "def train" .              # search (or: rg if installed)
lg run -- find . -name '*.yaml' -not -path './.venv/*'
```

Capture output the normal way (`out=$(lg run -- cat file)`).

---

## Editing / creating server files

### If a mount is available (best — check `local_root` is non-empty)
A human may have `lg shell` running; then `<project>/<repo>/` is a live folder.
Test it: `ls "$(lg config get local_root)"` — if it shows the repo tree, use your
**native file tools (Read / Edit / Write / Grep / Glob)** directly on paths under
`local_root`. Edits sync to the server automatically. This is the "native" path.

### If there is no mount (default) — write whole files over `lg`
Base64-encode the full new content and decode it on the server in one command
(no stdin, no heredoc — robust for any content, verified):

```sh
# write NEW_CONTENT to <path> on the server (relative to your cwd mapping):
B64=$(printf '%s' "$NEW_CONTENT" | base64)
lg run -- bash -c "printf %s '$B64' | base64 -d > 'path/to/file'"
```

For a surgical edit, read the file first (`lg run -- cat path`), apply your
change locally to produce the full new content, then write it back with the
pattern above. Verify with `lg run -- cat path` or a diff. (Do NOT try to pipe
content into `lg run` via stdin — the remote PTY doesn't cleanly signal EOF and
it can hang. Always pass content as a command argument, as above.)

Create dirs / move / delete like any command: `lg run -- mkdir -p data/out`,
`lg run -- rm -f tmp.txt`.

---

## Setting up a project (only if there's no `.lg/` yet)

Most of the time a project already exists. If you must create one, you need the
server details. Prefer non-interactive flags. **Password auth requires an
interactive prompt you cannot answer — a human must run that step** (or the host
already uses an ssh key).

```sh
# key/agent auth (no password needed):
cd <new-project-dir>
lg init --role ghost --host <ssh-host> --user <user> --remote-root <abs-repo-path> --yes
```

`lg init` writes `.lg/config.yaml`, creates the mount folder (named after the
repo), and **auto-installs the `lg` agent on the server** if it's missing.
- If it prints `✓ deployed agent …` or `agent already installed`, you're ready.
- If it needs a password (`--auth password`), **stop and ask the human** to run
  `lg init … --auth password` (it prompts hidden and stores the secret
  encrypted). You cannot supply the password yourself.

Change settings later: `lg config set <key> <value>` (e.g.
`lg config set source.remote_root /new/path`); lists via `lg config edit`.

---

## Failures → diagnosis → fix

| Symptom | Meaning | What to do |
|---|---|---|
| `not an lg project — run 'lg init'` | Your cwd isn't under a `.lg/`. | `cd` into the project (find the dir with `.lg/`). Don't `lg init` unless you're sure there's no project. |
| `lg: not connected to <host> …` | Couldn't reach/authenticate the server, or the agent is missing. | Read `<project>/.lg/lg.log` (last lines). Common causes below. |
| `lg: not connected … run \`lg connect\`` | The server needs interactive 2FA/Duo authentication you can't answer. | **A human step** — ask the human to run `lg connect` and approve the Duo prompt. It caches the connection for hours; then your `lg <cmd>`s work. Check readiness with `lg connect --check` (safe, read-only). Do NOT run bare `lg connect` yourself — it may block on a prompt you can't answer. |
| `… interactive second authentication step (Duo/2FA)` | The project is in native/password mode but the host demands a Duo/OTP answer stored credentials can't give. | Run the two printed commands: `lg config set source.ssh_mode system`, then ask the human to run `lg connect` (the stored password is auto-filled; they only approve Duo once, and the connection stays cached until it drops). |
| log shows `lg: command not found` | Agent not installed on the server. | A human runs `lg init` again (it re-deploys), or deploy manually. |
| log shows `Permission denied` / auth failure | Key not accepted / password needed / wrong user. | Human sets up key or re-runs `lg init --auth password`. |
| `Permission denied` writing a file (remote) | Server-side Unix perms — repo owned by another user. | Not an lg problem. Report it; the human fixes ownership/group or connects as the owner. |
| A command hangs and never returns | You ran an interactive/TUI program (`lg shell`, `lg vim`, a REPL). | Never do this. Kill it; use a non-interactive equivalent. |
| Mount folder is empty | No `lg shell` is running (normal for you). | Use the execution-only file patterns above; don't rely on the mount. |

`<project>/.lg/lg.log` is your primary diagnostic — read it whenever a connection
fails. Find it via `lg config path` (config is next to the log).

---

## Do / Don't

**Do**
- Use `lg run -- <cmd>` for everything on the server; trust exit codes.
- Stay at the project root and pass repo-relative paths, or `cd` to map cwd.
- Read `.lg/lg.log` when a connection fails.
- Write files with the base64-argument pattern; verify with a read-back.
- Use native file tools on `local_root` only after confirming the mount is live.

**Don't**
- Don't run `lg shell`, `lg toggle`, or any interactive/full-screen program.
- Don't pipe `lg` output and then read `$?` (it masks the remote exit code).
- Don't pipe content into `lg run` via stdin (PTY EOF can hang).
- Don't run `lg init` blindly — check for an existing `.lg/` first.
- Don't try to supply an ssh password yourself; that's a human step.
- Don't run bare `lg connect` on a 2FA/Duo host (it may block on a prompt you
  can't answer) — ask the human. `lg connect --check` is safe.

---

## Quick reference

```sh
# orient
cd <project> && lg status
lg config get source.remote_root ; lg config get local_root

# run
lg run -- <cmd>                         # exit code propagates; output is clean
out=$(lg run -- cat path/file)          # read
lg run -- grep -rn PATTERN .            # search
lg run -- mkdir -p dir                  # fs ops

# write a whole file (no mount)
B64=$(printf '%s' "$CONTENT" | base64)
lg run -- bash -c "printf %s '$B64' | base64 -d > 'path'"

# setup (key auth only; password is a human step)
lg init --role ghost --host H --user U --remote-root /abs/repo --yes

# NEVER: lg shell | lg toggle | lg vim | lg <repl>   (interactive → hang)
```

One line to remember: **`lg run -- <anything>` runs it on the server, in the
directory matching your cwd, with real exit codes and clean output — that alone
is enough to do remote development.**
