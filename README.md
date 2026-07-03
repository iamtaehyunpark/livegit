# Live Git (`lg`)

**Work on your GPU/lab server as if its files were on your laptop — and let your
coding agent do the same.**

`lg` links a laptop (your **Ghost**) to a remote machine (your **Source**) and
gives you two things that feel local but run remote:

1. **Run any command on the server, inline.** Prefix it with `lg` and it runs on
   Source, streaming live in a real terminal — colors, progress bars, Ctrl-C,
   the works — and exits with the remote command's own exit code.
2. **Browse the whole remote tree in your own editor.** `lg shell` mounts the
   entire server repo on your laptop. The full folder shows up instantly with
   real file sizes; the bytes of a file are fetched only when you open it.

No syncing a whole repo up front, no `rsync` loops, no living in `ssh`.

> **The real point: hand your remote server to a coding agent.** `lg init` drops
> an `AGENTS.md` into your project, so an agent like Claude Code drives `lg` from
> the very first prompt — editing code locally in the mounted repo, running
> experiments on the GPU box and reading their live output, kicking off long jobs
> that survive a disconnect, then debugging and plotting the results. It works
> your server as if its compute were bolted to your laptop.
> → [Working with a coding agent](#working-with-a-coding-agent)

```console
$ lg python train.py --epochs 50      # runs on the GPU box, streams here live
Epoch 1/50  loss=2.31  ██░░░░░░░░  (Ctrl-C reaches the remote process)
...

$ lg nvidia-smi                       # one-off remote command
$ code .                              # your editor, browsing the server's tree
$ echo $?                             # the remote exit code came back
```

---

## Install

`lg` is a single self-contained binary. **You don't need Go (or any compiler) to
run it** — only to build from source. Once installed it behaves like any other
CLI (`rg`, `fzf`, `gh`): you just type `lg`.

**One-line installer (prebuilt binary):**

```sh
curl -fsSL https://raw.githubusercontent.com/iamtaehyunpark/livegit/main/install.sh | sh
```

**Homebrew (via tap):**

```sh
brew tap iamtaehyunpark/livegit
brew install lg
```

**From source (needs Go ≥ 1.24, once):**

```sh
make install                 # builds a static binary -> ~/.local/bin/lg
make install PREFIX=/usr/local
```

**Requirements**

- On the **Source** server: just have `lg` on its `PATH`. `lg init` can deploy
  the matching binary for you over SSH — no manual copy.
- On your **laptop**: a FUSE implementation for the mount — [macFUSE] on macOS,
  `libfuse` on Linux. (You only need this for `lg shell`; plain `lg <command>`
  works without it.)

[macFUSE]: https://osxfuse.github.io

---

## Quick start

`lg` is project-local, like `git`: you set it up per repository.

```sh
mkdir ~/code/my-project && cd ~/code/my-project
lg init                  # interactive: server host, remote repo path
```

`lg init` walks you through everything with sensible defaults and a summary
before it writes anything, then offers to deploy the agent to the server. The
remote repo mounts as a subfolder right here, named after the repo — you don't
pick a path. Prefer flags?

```sh
lg init --role ghost --host gpu-1 --remote-root /home/you/my-project -y
```

That's it. Now you can run remote commands from anywhere in the project:

```sh
lg make            # build on the server
lg pytest -q       # test on the server, watch it stream
lg ls -la          # peek at the server's files
```

For an editor-friendly view of the remote tree:

```sh
lg shell           # mounts the server repo next to your project and opens a shell
```

The mount appears as a subfolder named after the server repo. Open it in
your editor — the entire tree is browsable immediately, and files materialize as
you open them. Edits you save flow back to the server automatically; changes made
on the server show up in the mount. Type `exit` to unmount.

On a brand-new machine, just typing `lg` greets you and points you at `lg init`.

---

## The two ideas, in a bit more depth

### Running commands on the server

Any first word that isn't an `lg` subcommand is treated as a remote command:

```sh
lg python train.py         # bare form — the common case
lg run -- status           # explicit form, for names that clash with subcommands
```

The command runs in a PTY on the server, so interactive tools behave exactly as
they would over `ssh`: live output, terminal resizing, and Ctrl-C delivered to
the remote process. Its exit code becomes `lg`'s exit code, so `lg ... && ...`
and scripts just work.

**Go all-in with toggle mode.** Flip a switch and *every* command in that shell
tab runs on the server until you flip it back — no `lg` prefix needed:

```sh
lg toggle          # prompt gains a  remote  tag; commands now run on Source
python train.py    # runs remotely
make               # runs remotely
lg local           # back to running locally
```

Inside a mounted `lg shell`, common read commands (`ls`, `cat`, `grep`, `find`,
`tail`, …) automatically run on the server when your working directory is inside
the mount — and fall back to the local command if the server is unreachable, so
browsing never stalls.

### Browsing the whole tree

`lg shell` syncs the server tree's *metadata* eagerly (like OneDrive/Dropbox
placeholder files): the complete directory structure with real sizes and
timestamps is there the instant the mount comes up, even for a huge repo. A
file's contents are fetched on first open and cached locally; the on-disk cache
is size-capped and evicts least-recently-used content, while the file listing
stays complete. Writes are journaled and pushed back to the server with
last-write-wins conflict handling (a diverging server file is backed up, never
silently clobbered).

Files and directories matched by your `ignore` patterns (or a `.lgignore` at the
repo root) are skipped on both sides — keep `.venv`, `node_modules`, and friends
out to make the initial sync fast.

---

## Long-running jobs

For training runs that should outlive your terminal (and your laptop going to
sleep), detach them:

```sh
lg run -d -- python train.py --epochs 300     # prints a job id and returns
lg jobs                                        # list jobs: id, state, age, command
lg logs -f <id>                                # follow output live (Ctrl-C stops
                                               # following, NOT the job)
lg jobs kill <id>                              # stop a running job
lg jobs rm <id>                                # forget a finished job + its logs
```

Detached jobs keep running on the server after `lg` exits and after you
disconnect. Where the server supports it they run under `systemd --user` so a
closed SSH session can't reap them; `lg` tells you if durability is reduced on a
given host.

---

## Working with a coding agent

`lg` turns a coding agent (Claude Code, and similar CLIs) into something that can
*use your GPU box as if it were the local machine* — write code locally, run it
remotely, watch it, iterate.

When you run `lg init`, it drops an **`AGENTS.md`** into your project root (next
to `GUIDE.md`) — a concise operating guide that teaches an agent how to drive
`lg`: how to run things on the server, how output streams, how the local↔remote
paths map, and how to edit files the right way. Point your agent at it once and
it behaves as if it natively understood the split between your laptop and the
server — no hand-holding per command. Both guides carry a marker line and are
refreshed automatically by `lg connect` when you upgrade lg (remove the marker
to keep your own copy).

A typical loop looks like this. Link a server-side repo — even an empty one —
with `lg init`, mount it with `lg shell`, then start your agent in the mounted
folder on your laptop. From there the agent can:

- **Edit code with its normal file tools**, right in the mounted folder — changes
  sync to the server automatically. If a mount write ever fails, `AGENTS.md` has
  it fall back to writing the file server-side over `lg`, so editing never gets
  stuck.
- **Run the experiment on the server** (`lg python train.py`) and read the
  **live streamed output** as it runs — the same bytes you'd see over `ssh`.
- **Launch long runs as detached jobs and monitor them.** The job keeps running
  on the server even if your laptop sleeps, the connection drops, or the `lg`
  Ghost process exits entirely — nothing is lost. The agent (or you) reconnects
  later and tails the logs with `lg logs -f`.
- **Debug, plot results, and iterate** — reading files, re-running, inspecting
  artifacts — all from the local machine.

The net effect: the agent gets a remote server's compute with a fast, local
edit-and-inspect loop, and it "just works" from the first prompt because
`AGENTS.md` gives it the operating manual.

---

## Everyday commands

| Command | What it does |
|---|---|
| `lg <command>` | Run `<command>` on the server, streaming live |
| `lg run -- <command>` | Explicit run (use when a name clashes with a subcommand) |
| `lg run -d -- <command>` | Launch a detached job that outlives this shell |
| `lg jobs` / `lg logs [-f] <id>` | List detached jobs / show or follow their output |
| `lg shell` | Mount the remote tree and open a shell with `lg` integration |
| `lg connect` / `lg refresh` / `lg disconnect` | Authenticate once (Duo/2FA) / re-authenticate now / close the cached connection |
| `lg toggle` / `lg local` | Send every command to the server / turn that off |
| `lg status` | Connection, toggle state, sync freshness, cache, pending writes |
| `lg config get\|set\|edit\|show` | Inspect or change settings safely |
| `lg unmount` | Clear a leftover/stale mount |

---

## Configuration

Everything for a project lives in a `.lg/` directory at its root — config, cache,
tree snapshot, journal, and logs — discovered by walking up from your working
directory, exactly like `.git/`. There's no global state.

Connecting to the server uses your existing SSH setup:

- **`system` mode (default)** shells out to the real `ssh` binary, so your
  `~/.ssh/config` fully applies — `Host` aliases, `ProxyJump`/bastions,
  `ControlMaster` (2FA/Duo isn't re-prompted), `IdentityFile`, `known_hosts`.
  This is what makes lab and 2FA servers just work: authenticate once with
  `lg connect` (a stored password is auto-filled — you only approve the Duo
  push) and the cached connection carries every later command.
- **`native` mode** uses a built-in SSH client. Password auth (stored encrypted,
  keyed to the machine) logs in by itself on hosts without a second factor.

Change a setting with `lg config set source.host gpu-2`, or edit the file with
`lg config edit` (it's validated before saving).

---

## Build & test from source

Go is only needed to build; the resulting binary runs standalone.

```sh
make build     # -> ./bin/lg
make test      # unit tests + an in-memory Ghost↔Source integration test
make vet
make release   # cross-compiled static binaries for all platforms -> ./dist
make install   # build + install to ~/.local/bin/lg
```

The test suite runs a real multiplexed Ghost↔Source session over an in-memory
pipe — command execution, the full-tree and file RPCs, framing, and the FUSE
backend logic against a fake Source — so most behavior is verified without a
kernel mount or a second machine. The live FUSE mount and the SSH dial to a real
server naturally require actual hardware.

---

## How it works

One SSH connection to the server carries several logical streams multiplexed with
[yamux]: a control channel, file RPCs, change notifications, and a PTY per remote
command. The server side is a small agent (`lg serve`) launched over SSH on
demand — no long-running daemon or extra open port. On the laptop, a FUSE
filesystem serves the eagerly-synced metadata index and fetches content lazily,
while a journal handles write-through back to the server.

```
internal/config     config, .lgignore matcher, local↔remote path mapper
internal/proto      message schema + length-prefixed framing
internal/transport  SSH dial (system or native) + yamux streams + reconnect
internal/agent      Source agent: file server, full-tree walk, PTY exec, jobs, watcher
internal/fuse       Ghost FUSE: metadata index, lazy content, write-through journal
internal/shell      command runner (PTY bridge), jobs client, toggle, shell hooks
internal/cli        the cobra command surface
internal/shellq     the single shell-quoting helper used everywhere
```

[yamux]: https://github.com/hashicorp/yamux
