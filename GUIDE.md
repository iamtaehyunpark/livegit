# Live Git — A Friendly Guide

Hey 👋 — this is the practical, no-jargon walkthrough of `lg`. If you just want
to *use* it, you're in the right place. (For the design/architecture, see
`README.md`.)

---

## The one-minute mental model

You have two machines:

- **Source** 🖥️ — your GPU server. The real files and the real compute live here.
- **Ghost** 💻 — your laptop. It *shows* you Source's files but only downloads
  the ones you actually open. Think of it like a Netflix of your codebase: the
  whole library is browsable, but nothing streams until you press play.

`lg` glues them together so that:

1. **Editing feels local.** You open and edit files on your laptop normally.
   Changes fly back to Source on their own.
2. **Running heavy stuff goes remote, automatically.** The instant you activate
   a venv or run `python train.py`, your shell quietly hops onto Source and runs
   it there — on the GPU, in the real environment. No `ssh` dance.
3. **Your disk stays light.** Files you haven't touched take up ~0 bytes locally.
   Old ones get cleaned up automatically.

You stay in **one shell the whole time.** It just knows when to be local and
when to be remote.

---

## Setup (once)

### 1. Install `lg` on both machines

`lg` is a single self-contained binary — **no Go, no compiler, nothing to run
it with.** You just put it on your `PATH`, like `rg` or `gh`. Pick one:

```sh
# easiest — one-line installer (downloads the prebuilt binary)
curl -fsSL https://raw.githubusercontent.com/iamtaehyunpark/livegit/main/install.sh | sh

# or Homebrew
brew tap iamtaehyunpark/livegit && brew install lg

# or build once from source (the only path that needs Go)
make install
```

Do this on your **laptop (Ghost)** and once on **Source** (drop the matching
binary somewhere on Source's `PATH`, e.g. `~/.local/bin/lg`). You never start
anything on Source by hand — your laptop launches the Source side over SSH
automatically.

> Already have it? `lg --version` confirms it's installed and on your PATH.

### 2. Make sure plain SSH already works

`lg` rides on your normal SSH. Before anything else, confirm this works on its own:

```sh
ssh gpu-1      # whatever your server alias / host is
```

If that connects without fuss, you're good. (`lg` reads your SSH keys/agent and
`~/.ssh/known_hosts`, so the host needs to be known — that first manual `ssh`
takes care of it.)

### 3. Install a FUSE backend on the laptop (Ghost)

This is what lets your laptop *show* Source's files.

- **macOS:** install [macFUSE](https://macfuse.github.io/) → then reboot/approve
  the system extension in System Settings.
- **Linux:** `sudo apt install fuse3` (or your distro's equivalent).

### 4. Initialize

On the laptop, just run:

```sh
lg init
```

It walks you through it, step by step — no flags to remember:

```
👋 Welcome to lg! Let's set things up. (press Enter to accept [defaults])

Is this machine your laptop (ghost) or the server (source)? (ghost/source) [ghost]:
Now the server (Source) you'll connect to:
  SSH host or alias (e.g. gpu-1 or user@1.2.3.4): gpu-1
  Absolute repo path on the server: /home/you/projects/myrepo
  SSH user [you]:
  SSH port [22]:
And where on this laptop should the project appear?
  Local mount point [/Users/you/myrepo]:

Here's what I'll write:
  role:        ghost
  server:      you@gpu-1
  remote repo: /home/you/projects/myrepo
  local mount: /Users/you/myrepo
Write this config? [Y/n]:
```

Press Enter to accept the bracketed defaults; type to override. It shows you a
summary and asks before writing anything.

> Just type `lg` on a fresh machine and it'll point you here. Prefer one line?
> `lg init --role ghost --host gpu-1 --remote-root /home/you/myrepo --local-root ~/myrepo`
> skips the prompts (handy for scripts).

This writes `~/.lg/config.yaml`. Change any setting later with
`lg config set …` (see "Changing settings" below) — no need to re-run `lg init`.

---

## Daily use

### Start your day

```sh
lg shell
```

That mounts the project at your `--local-root` and drops you into your **normal
shell** (zsh, with all your config intact). You'll see a note like:

```
lg: mounted /Users/you/myrepo, entering shell (tab a1b2c3)
```

Now just… work. `cd ~/myrepo`, open files in your editor, browse around.

### Reading & browsing files — feels instant

```sh
cd ~/myrepo
ls
cat src/model.py
grep -r "def train" .
```

These stay **local**. The first time you open a file, `lg` fetches its contents
in the background; after that it's cached. Big files you haven't opened show
their real size but take no disk space until you actually read them.

### Editing files — just save

Open a file in VS Code / vim / whatever, edit, save. Done. `lg` ships your save
back to Source within milliseconds. You don't run a sync command, ever.

> 💡 Saves **never block** on the network. Even if your wifi drops, your save
> completes instantly and gets queued — it'll catch up when you reconnect.

### Running things — it switches for you

Here's the magic. Type any of these and `lg` automatically jumps onto Source:

```sh
conda activate ml
source .venv/bin/activate
poetry shell
python train.py
```

Your prompt picks up a `[SOURCE]` badge and you're now in a **real tmux session
on the GPU server**. Run your training, start a notebook, whatever — it's all
remote, with the real environment and hardware.

```
[SOURCE] you@gpu-1 ~/myrepo $ python train.py
Epoch 1/100 ...
```

### Coming back to local

Detach the remote session with **`Ctrl-b` then `d`** (the tmux detach combo).
You pop right back to your local shell — and importantly, **whatever you started
keeps running on Source.** Your training doesn't die when you detach or even
when you close the laptop.

Need to bail out forcefully? `lg local` always returns you to local mode.

### Picking up where you left off

Re-run the same trigger (e.g. `conda activate ml`) from the same terminal tab
and you **reattach to the same session** — your training is still going, output
and all. Different terminal tabs get their own independent sessions.

See everything that's running:

```sh
lg sessions
```

---

## Check on things

```sh
lg status
```

```
role:        ghost
mount:       /Users/you/myrepo
source:      gpu-1:/home/you/projects/myrepo
mode:        local
files:       1240 ghost, 18 cached, 2 live
cache used:  43.2 MB / 10 GB
journal:     0 pending write(s)
conflicts:   none
```

What to look at:
- **files** — how many are placeholders (`ghost`), downloaded (`cached`), or
  have un-synced local edits (`live`).
- **journal** — writes waiting to reach Source. `0` means you're fully synced.
  A non-zero number that's stuck usually means you're offline.
- **conflicts** — see below.

---

## When things get weird

**"I edited a file but Source has a different version."**
This is a *conflict* — it happens if the same file changed on both sides at once
(e.g. an agent edited it on Source while you edited on your laptop). `lg` never
silently picks a winner. It backs up the other version next to the file as
`yourfile.lg-conflict-<timestamp>` and lists it in `lg status`. Open both,
reconcile by hand, delete the `.lg-conflict` file.

**"I went offline."**
Keep working. Reads of already-cached files work; edits get queued. When you
reconnect, the queue replays automatically and `lg status` journal count drops
back to 0. (Files you *haven't* downloaded can't be read while offline — they
live on Source.)

**"My venv / node_modules isn't showing up on the laptop."**
On purpose 🙂. Big, machine-specific folders (`.venv`, `node_modules`, `*.pt`
weights…) are intentionally *not* mirrored — they're listed in `ignore` in your
config. You don't need them locally; entering SOURCE mode gives you the real
ones. That absence is also what tells `lg` "this is a Source-backed project,
run heavy commands remotely."

**"It didn't switch to SOURCE when I expected."**
Trigger detection works in **zsh** out of the box (bash is best-effort). Check
that `lg shell` actually launched zsh, and that your command matches a pattern
in `source_triggers`. You can always force it with `python ...` (matches
`always_source_patterns`) or just run the command after activating a venv.

---

## Changing settings

Two ways — both safe, no daemon to restart (config is read fresh each run):

```sh
lg config set source.host gpu-2        # change one setting, leaves the rest intact
lg config get source.host              # print one setting
lg config edit                         # open the whole file in $EDITOR
lg config show                         # dump the current config
```

`lg config set` validates before saving, so a typo can't break your config.
For list-valued settings (ignore patterns, triggers) use `lg config edit`.

> Don't re-run `lg init` just to change a field — it rewrites the whole file
> back to defaults.

## Config cheat sheet (`~/.lg/config.yaml`)

```yaml
source:
  host: gpu-1                       # your SSH host
  remote_root: /home/you/myrepo     # repo path on Source
local_root: /Users/you/myrepo       # mount point on the laptop

ignore:                             # not mirrored locally (edit freely)
  - ".venv/"
  - "node_modules/"
  - "*.pt"

cache:
  evict_after_idle_minutes: 30      # forget files untouched this long
  max_cache_size_gb: 10             # hard cap on local cache

source_triggers:
  patterns:                         # commands that flip you to SOURCE
    - "^conda activate"
    - "^source .*/bin/activate"
    - "^poetry shell"
  directory_markers:                # folders that mark a "remote" project
    - ".venv"
    - "node_modules"
  always_source_patterns:           # always run these on Source
    - "^python "

offline:
  on_source_trigger: queue          # 'queue' (wait) or 'error' (refuse) when offline
```

Add your own trigger patterns or readonly commands anytime — they're plain
regexes and command names.

---

## The whole loop, in one breath

`lg shell` → browse and edit like it's all local → type `conda activate` and
you're on the GPU → train → `Ctrl-b d` to step away (it keeps running) → back to
editing → `lg status` to peek → close the laptop, come back tomorrow, reattach.
One shell, two machines, no babysitting. 🚀
