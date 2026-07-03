# Live Git (`lg`) — User Guide

Work on a remote server (GPU box, lab machine) as if its code lived on your
laptop. `lg` gives you two things:

1. **A command runner** — type `lg <anything>` and it runs on the server, with
   output streamed live to your terminal. Exit codes, Ctrl-C, and full-screen
   programs (vim, htop) all work, exactly like `ssh` — but the connection is
   invisible and never re-prompts you.
2. **A cloud folder** — the server's whole repo shows up as a normal folder on
   your laptop (like OneDrive/iCloud Drive). Browse it in Finder or your editor;
   files download on demand when you open them; edits sync back automatically.

`lg` is one self-contained binary. Your laptop is the **Ghost**; the server is
the **Source**.

---

## Install

```sh
export PATH=/opt/homebrew/bin:$PATH   # Go lives here on this machine
make install                           # builds + installs to ~/.local/bin/lg
```

`~/.local/bin` is on your PATH, so `lg` runs from anywhere. You only need Go to
*build* it; the installed binary is self-contained.

---

## Set up a project

`lg` is **project-local**, like git. In each project directory you run `lg init`
once; it writes a `.lg/` folder there (config + state), and every `lg` command
you later run from that directory (or any subdirectory) uses it automatically.

```sh
mkdir ~/myproject && cd ~/myproject
lg init
```

The wizard asks for:

| Prompt | Meaning |
|---|---|
| ghost / source | Pick **ghost** (this is your laptop). |
| SSH host | `gpu-1`, `user@1.2.3.4`, or an `~/.ssh/config` alias. |
| Remote repo path | Absolute path of the repo **on the server**, e.g. `/home/you/myrepo`. |
| SSH user / port | Defaults to `$USER` / 22. |
| Password? | Answer **y** only if you type a password when you ssh in (no key set up). It's stored **encrypted** and lg fills it in for you from then on (see [Auth](#authentication--log-in-once-never-again)). Answer **n** to use your ssh key/agent (recommended). |
| Second auth step? | Answer **y** if the host *also* asks for Duo / a passcode / an OTP. With a stored password, `lg connect` auto-fills it and you only approve the Duo prompt — once per cached window (**10h** on these hosts; `control_persist: 10h`). Without a stored password, you authenticate interactively once with `lg connect`, exactly like plain ssh. |

Non-interactive (scriptable) form:

```sh
lg init --role ghost --host gpu-1 --remote-root /home/you/myrepo
# password host:
lg init --role ghost --host 1.2.3.4 --user you --remote-root /data/proj --auth password
# password + Duo host (password auto-filled; you approve the Duo prompt at `lg connect`):
lg init --role ghost --host lab-1 --user you --remote-root /data/proj --auth password --two-factor
```

After you confirm, `lg init`:
- writes `~/myproject/.lg/config.yaml`,
- creates the mount folder `~/myproject/<repo-name>/` (named exactly after the
  server repo — no choice; the local mirror carries the repo's own name),
- (password auth) stores the password encrypted in `~/myproject/.lg/credentials`,
- **connects and installs the `lg` agent on the server** if it isn't there yet
  (uploaded straight over ssh — no manual scp).

Result:

```
~/myproject/
  .lg/         config + local state (like .git/)
  myrepo/      ← the server's repo appears here (empty until you run `lg shell`)
```

---

## Two ways to work

### A. Run commands on the server: `lg <command>`

From anywhere inside your project, prefix any command with `lg`:

```sh
cd ~/myproject
lg python train.py          # runs on the server, streams output
lg ls -la                   # server listing
lg pytest -q                # exit code comes back: `lg pytest && lg deploy.sh` works
lg vim config.yaml          # full-screen editor, over the wire
lg nvidia-smi
```

Press **Ctrl-C** to interrupt the remote job, just like ssh. `lg` connects in the
background and reuses that connection, so there's no per-command login.

**Directory awareness:** the command runs in the server directory that matches
where you are. Inside the mounted folder, `cd myrepo/src && lg ls` lists the
server's `myrepo/src`. Outside the mount, commands run at the repo root.

If a command name clashes with an `lg` subcommand (e.g. you literally want to run
a program called `status`), use the explicit form: `lg run -- status`.

### B. Browse & edit files: `lg shell`

```sh
cd ~/myproject
lg shell
```

This mounts the server's repo at `~/myproject/myrepo/` and drops you into your
normal shell. Now:

- Open `~/myproject/myrepo/` in Finder or your editor — the **whole tree is
  there immediately**, with real file sizes, even before you open anything.
- Open a file and its contents download on demand (a brief pause the first
  time, like OneDrive materializing a file).
- Edit and save — it syncs up to the server automatically. No manual sync.
- Type `exit` to unmount and disconnect cleanly.

Inside `lg shell`, common read commands (`ls`, `cat`, `tree`, `grep`, `find`,
`head`, `tail`, …) automatically run **on the server** when you're inside the
mounted folder, so they reflect the real server tree fast. Everything else runs
locally. (Configurable — see `auto_remote_commands` below. Escape to a local
command with `\ls` or `command ls`.)

**Toggle mode** — want *every* command to go to the server for a while?

```sh
lg toggle        # now every command in this shell runs on the server
python train.py  # runs remotely (no `lg` prefix needed)
lg toggle        # back to a normal local shell   (or: lg local)
```

---

## Authentication — log in once, reuse it for hours

`lg` keeps **one** authenticated ssh connection to the server and runs every
command over it, so there's no per-command login.

- **SSH key / agent (default):** `lg` uses the system `ssh`, so your
  `~/.ssh/config` applies in full — host aliases, ProxyJump/bastions,
  IdentityFile, known_hosts. On a key-only host you never see a prompt.
- **2FA / Duo hosts:** the first connection needs your approval (a Duo push or a
  passcode). `lg` owns a dedicated, reusable ssh connection and authenticates it
  **once** with `lg connect`, then keeps it cached for `source.control_persist`
  and reuses it for every later `lg <cmd>`, `lg run`, and `lg shell`. For hosts
  set up with the "second auth step" answer, `lg init` writes
  `control_persist: 10h` — one Duo approval covers a full work day. (Other
  hosts default to `8h`; set `max` for no expiry at all.)

  ```sh
  lg connect            # approve the Duo prompt once; cached for the window (10h)
  lg connect --check    # is the connection live?
  lg refresh            # re-authenticate NOW — restarts the window (e.g. before
                        # an overnight run, so it can't expire midway)
  lg disconnect         # close it (the next command re-authenticates)
  ```

  You rarely run `lg connect` by hand: on a terminal, `lg <cmd>` and `lg shell`
  bring the connection up for you (you'll just see the Duo prompt inline the
  first time). Run it explicitly to pre-authenticate — e.g. before a scripted
  run, or so a tool that can't answer a Duo prompt finds the connection ready.
- **Password (no second factor):** for hosts that only take a password and
  where you can't use a key. `lg init` (or `--auth password`) prompts once and
  stores the password **encrypted** at `<project>/.lg/credentials` (AES-GCM,
  with a key derived from your machine — copying the file to another computer
  won't decrypt it). It's never written in plaintext and never in
  `config.yaml`. lg then authenticates automatically with the built-in ssh
  client (no ssh master needed).
- **Password + Duo/2FA:** answer **y** to both wizard questions (or pass
  `--auth password --two-factor`). The password is stored encrypted exactly as
  above, and `lg connect` **auto-fills it** into ssh's prompt (via an
  `SSH_ASKPASS` helper) — the only thing left for you is the Duo approval,
  once per cached connection. A stored password can't answer the Duo challenge
  itself, which is why these hosts stay in system-ssh mode with the cached
  master instead of the native client. If a project was mistakenly set to
  native mode on a Duo host, `lg connect` detects it and prints the switch:
  `lg config set source.ssh_mode system`, then `lg connect`.

After the first login you shouldn't see another prompt — across laptop sleep,
wifi changes, or restarting `lg` — until the cached connection expires (`8h`
default, `10h` on second-auth hosts) — or run `lg refresh` to restart the
window on demand.

---

## Everyday commands

| Command | What it does |
|---|---|
| `lg <cmd>` | Run `<cmd>` on the server (streamed, exit code, PTY). |
| `lg run -- <cmd>` | Same, explicit form (use when `<cmd>` clashes with a subcommand). |
| `lg shell` | Mount the repo folder + start your shell. `exit` to leave. |
| `lg connect` | Authenticate to the server once (handles Duo/2FA); reused for hours. `--check` / `--stop`. |
| `lg refresh` | Re-authenticate now — restarts the cached window (e.g. before an overnight run). |
| `lg disconnect` | Close the cached connection (next command re-authenticates). |
| `lg toggle` / `lg local` | Turn "everything runs on the server" on / off. |
| `lg status` | Connection state, toggle, tree-sync freshness, cache, pending writes. |
| `lg scan [dir]` | List every lg project on this machine (default `$HOME`) and its connection state. |
| `lg config show` | Print the active project's config. |
| `lg config set <key> <val>` | Change a setting (e.g. `lg config set source.port 2222`). |
| `lg config edit` | Open the config in `$EDITOR` (for list values like `ignore`). |
| `lg config path` | Print the active config file path. |
| `lg unmount` | Clear a stale/leftover mount (rarely needed). |
| `lg init` | Set up (or re-set-up) the current directory. |

---

## Configuration

Settings live in `<project>/.lg/config.yaml`. Change scalars with
`lg config set <key> <value>`; edit lists with `lg config edit`.

| Key | Meaning |
|---|---|
| `source.host` / `source.user` / `source.port` | The server. |
| `source.remote_root` | Absolute repo path on the server. |
| `source.ssh_mode` | `system` (default, honors ~/.ssh/config) or `native` (built-in client). |
| `source.auth` | `` (key/agent or interactive-via-`lg connect`) or `password` (encrypted store; native ssh answers it directly, or — with `ssh_mode: system` on a Duo host — `lg connect` auto-fills it via SSH_ASKPASS). |
| `source.control_persist` | How long the cached ssh connection lives after last use: a duration (default `8h`; `lg init` picks `10h` for second-auth hosts) or `max` (no expiry — lives until the link drops). Longer = fewer 2FA prompts. `system` mode only. |
| `source.agent_bin` | Path to `lg` on the server; default `lg` (resolved from `~/.local/bin`). |
| `local_root` | The mount folder (set for you at init, named after the repo). |
| `ignore` | Patterns never synced/listed (`.venv/`, `node_modules/`, `.DS_Store`, `*.pt`, …). Keeps the tree fast and clean. |
| `auto_remote_commands` | Commands that auto-run on the server inside `lg shell` (default `ls cat tree head tail less grep find stat wc file`). Set to `[]` to disable. |
| `default_target` | `local` (default) or `source` (start `lg shell` with toggle already on). |
| `cache.max_cache_size_gb` | Cap for downloaded file content on your laptop. |

Multiple projects each have their own `.lg/`; just `cd` into a project and `lg`
uses that one. Outside any project, `lg` says "not an lg project — run `lg init`".

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| `not connected …` right after start | The agent likely isn't on the server, or auth failed. Re-run `lg init` (it re-deploys the agent); check `lg status`; look at `<project>/.lg/lg.log`. |
| `not connected … run \`lg connect\`` (2FA/Duo host) | The cached connection expired or was never established — run `lg connect` and approve the prompt, then retry. `lg connect --check` shows the state. |
| Mount is empty | The tree is still syncing (large repos take a few seconds — watch `lg.log` for `tree synced`), or you're not connected. |
| `lg shell` won't stop / stale mount | `lg unmount`. `lg shell` also auto-recovers stale mounts on start. |
| Permission denied writing files | Server-side: the repo may be owned by a different user. Fix ownership/group on the server, or connect as the owning account. |
| "not an lg project" | You're outside a project directory — `cd` into one, or `lg init` here. |
| Password moved to a new laptop | The encrypted store is machine-bound; re-run `lg init` to re-enter it. |

Logs and state are per-project under `<project>/.lg/` (`lg.log`, `journal.log`,
`tree.json`, `cache/`). Find the active one with `lg config path`.

---

## Mental model in one paragraph

`lg` keeps one authenticated connection to the server alive and multiplexes
everything over it. Your command line (`lg <cmd>`) runs in a remote PTY in the
directory matching your local cwd. The mount shows the server's entire file tree
by syncing its *metadata* eagerly (so browsing is instant) and fetching file
*content* lazily on open; your edits are journaled and pushed back. The server is
the single source of truth for content — think "iTerm2 + ssh, but invisible" plus
"OneDrive, but backed by your GPU box."

---

*Working with an AI coding agent? Point it at [`AGENTS.md`](AGENTS.md) — a
prescriptive guide that lets an agent drive `lg` as a native remote-dev tool.*
