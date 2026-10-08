# q

[![CI](https://github.com/justinrush/q/actions/workflows/ci.yml/badge.svg)](https://github.com/justinrush/q/actions/workflows/ci.yml)
[![Release](https://github.com/justinrush/q/actions/workflows/release.yml/badge.svg)](https://github.com/justinrush/q/actions/workflows/release.yml)

[Install](#install) · [Using it](#using-it) · [Two machines](#two-machines) · [Configuration](#configuration) · [Contributing](#contributing)

`q` is a terminal UI for running coding agents across several git repos at once.

Q runs the mission board. An **operation** is an area of investigation: a high-level
summary plus the repos it spans. A **mission** is one unit of agent work inside an
operation. It inherits the operation's repos and can add repos needed only for that
mission. q gives each mission its own git worktree per repo, starts `claude`, `codex`,
`agy`, or `opencode` in a detached tmux session, and shows every mission on a board
that updates itself as the agents work.

```
q  Board   Operations

 BRIEFING 1                            ACTIVE 0    AWAITING ORDERS 1                      DEBRIEF 1                  CLOSED 1
 ╭──────────────────────────────────╮  —           ╭───────────────────────────────────╮  ╭───────────────────────╮   old-cleanup
 │ ◐ discussions-endpoint           │              │ ⏸ Bash(rm -rf build)              │  │ ○ terraform-wiring    │   z to expand
 │ Add the endpoint.                │              │ Audit the pipeline.               │  │ Wired the module.     │
 │ ◆ claude · opus · plan · 2r · 4m │              │ ◇ codex · gpt-5.4-mini · 3r · 22m │  │ ◆ claude · haiku · 1r │
 ╰──────────────────────────────────╯              ╰───────────────────────────────────╯  ╰───────────────────────╯
 nebula-migration                                  discussions-api                        discussions-api
```

Each card carries a bar in its operation's color, so a glance tells you which
investigation it belongs to.

## Why it exists

Running several agents at once across a multi-repo codebase creates two problems this
solves.

**You lose track of who needs you.** An agent that stops to ask a question looks exactly
like one that is thinking. q watches the agents' own hooks and moves cards to *awaiting
orders* the moment one blocks, and to *debrief* when one finishes.

**Concurrent agents fight over branches.** A checkout can only be on one branch, so two
missions touching the same repo collide. q gives every mission its own worktree per repo,
branched from a freshly fetched default branch. Your own checkouts are never modified.

A mission that is about work already in progress can name a different starting point per
repo, so its worktree contains the branch you want to read. See [Base branches](#base-branches).

## What q can do

- Run independent missions in isolated worktrees, with live agent status and messages.
- Queue briefed missions and launch them as active slots become free.
- Create follow-up missions that inherit the parent mission's agent and repositories.
- Pair two machines over SSH, mirror worktrees, and move agent execution between hosts.
- Discover models from installed agents and estimate Claude/Codex usage costs.

## Requirements

- **git** and **tmux** (git 2.40+ for merging paired-machine work)
- **[claude](https://claude.com/claude-code)** or **[codex](https://developers.openai.com/codex/cli)** or **[agy](https://antigravity.google/docs/cli/getting-started/)** or **[opencode](https://opencode.ai)** — at least one
- **Go 1.26+** when building from source
- macOS or Linux. Desktop windows on macOS use [Ghostty](https://ghostty.org) 1.3+;
  remote shells use the current terminal. For other desktop windows, name your own
  terminal command, or let q print the `tmux attach` line for you. See
  [Configuration](#configuration).

## Install

Download a macOS or Linux archive for your architecture (`arm64` or `amd64`) from
[Releases](https://github.com/justinrush/q/releases). Each archive contains `q`, the
README, and the license; `checksums.txt` lists SHA-256 hashes. Extract it, place `q`
on your PATH (for example, in `~/.local/bin`), and run `q --version`.
The binaries include their release version and commit. Agent CLIs, git, and tmux
are installed separately.

To build from source instead:

```sh
git clone https://github.com/justinrush/q
cd q
./install.sh                      # builds and installs to ~/.local/bin/q
./install.sh --prefix /usr/local  # or somewhere else
```

Then:

```sh
q doctor        # check git, tmux, an agent, and your editor are all reachable
q config init   # optional: write ~/.q-config.json with the current effective values
q               # open the board
```

After upgrading, run `q daemon restart` so the daemon uses the new binary.

`q doctor` is worth running first. It reports the resolved path and version of every
external tool, where state lives, and what your configuration actually resolved to.

## Using it

Press `?` for the full keymap. The essentials:

| key | action |
|---|---|
| `tab` | switch between Board and Operations |
| `a` (Operations) | add an operation: a summary plus the repos it spans, named by fragment |
| `n` (Board) | new mission, optionally with a model, effort, and additional repos; `ctrl+g` sets base branches, `ctrl+s` saves, `ctrl+r` launches |
| `H` / `L` | move a card between lanes. Out of briefing launches the agent |
| `enter` | open a debrief: attaches to the live agent and opens an editor per changed repo |
| `l` | open the selected card’s full launch error; scroll with `j`/`k` or Page Up/Down, `c` copies the full log, `t` queues a troubleshooting mission with the source brief and diagnostics, close with `esc` |
| `←` / `→` | focus the previous/next lane |
| `m` | send a message to a running agent |
| `a` (Board) | queue a briefed mission so q starts it when a slot is free; press again to unqueue |
| `t` | take a mission from the paired host and run its agent here |
| `d` | delete a mission and reclaim its worktrees |
| `/` | filter the board to one operation |

Log copying uses the native clipboard on a local Mac and tmux’s `load-buffer -w` over SSH inside tmux. Elsewhere it sends OSC 52 to the viewing terminal. Remote copying requires terminal clipboard support; tmux supports `set-clipboard external` or `on`. Troubleshooting missions inherit the source operation, agent, model, effort, extra repos, and base branches.

Mouse is supported out of the box:
- **Click** a mission card to shift focus to it. Click a lane header or lane column to focus that lane.
- **Double-click** a card to open its debrief session.
- **Click tabs** (`Board` / `Operations`) in the header to switch views.
- **Scroll wheel** steps through cards in the hovered lane.

Everything is also scriptable, which is the quickest way to see what the board is doing:

```sh
op=$(q operation add "Discussions API" --summary "…" --repo ~/dev/weave --repo ~/dev/azure-tf)
q mission add discussions-endpoint --operation "$op" --prompt "Add the endpoint." --plan
q mission add tidy-imports --operation "$op" --prompt "Tidy them." --model haiku
q mission add review-login --operation "$op" --prompt "Review it." --base weave=feat/login
q models                       # what each agent offers, and the default a mission gets
q mission move ms_… active     # launches the agent
q mission queue ms_… ms_…      # or let q launch them as slots free up
q mission list                 # the board, as text
q open ms_…                    # open the debrief session
q mission rm ms_… --dry-run    # what deleting would discard
```

### Queuing follow-up work

A mission that designs the next piece of work is usually the one best placed to
queue it, because it already holds the context. `q mission add --from` copies
another mission's operation, agent, model, effort, repositories, and base
branches, so a follow-up can be written without restating any of it:

```sh
parent=$Q_MISSION_ID   # the mission q launched this agent into
q mission add ship-it --from "$parent" --prompt "…"
```

`--from` defaults to `$Q_MISSION_ID`, which q exports into every agent's
session, so inside a mission the flag can be left off entirely. `Q_BIN` is
exported alongside it for the same reason: an agent's own `PATH` is the vendor's
to set, and q is often not on it, so use `"$Q_BIN"` if `q` is not found.

Inheritance is a default, not an override. Anything passed on the command line
wins, and a `--repo` or `--base` you name replaces the parent's list outright
rather than adding to it — so naming one repo narrows the mission's worktrees
instead of quietly widening them.

`--plan` is the exception: it is never inherited. Whether a mission stops for
approval is the launcher's decision, not the parent brief's.

Created missions land in briefing either way. Nothing runs until a mission is
moved to active, or queued.

### Letting q start missions

A briefed mission waits for you by default. Queue it and the daemon starts it
instead:

```sh
q mission add tidy-imports --operation "$op" --prompt "Tidy them." --queue
q mission queue ms_… ms_…      # queue missions that already exist
q mission unqueue ms_…
```

Queued missions start in board order, at most `queue.maxConcurrent` at a time.
A slot is held only while a mission is in the active lane: one that stops to
ask a question or finishes its turn frees it, so a queue keeps moving with
nobody there to answer.

A queued mission is started once. If the launch fails, the card goes back to
briefing with the reason and is no longer queued, rather than being retried
every few seconds.

Queueing says nothing about order between missions beyond their position on
the board. Two missions where the second builds on the first's unmerged work
should not both be queued.

## Two machines

q can share its missions between the machine you sit at and one that is always
on, so a queue keeps moving after a laptop lid closes and the code is still
local when it opens again.

```sh
# on the laptop, once
q remote setup mini.local
q remote status
q remote sync     # request an exchange now
```

The arguments to `setup` are whatever you would give `ssh` to get a shell on
the other machine. That machine needs q installed and its daemon running, and
nothing else: it learns it is half of a pair when the laptop first calls. Its
daemon should run as a service so it survives a reboot, because q will not
start one over ssh. A daemon started that way would hand the bare environment
of a non-interactive session to every agent it launched.

A machine has one peer. `setup` asks the other machine who it is before saving
anything, and refuses if either one is already paired with a third. To pair the
laptop with a different machine:

```sh
q remote forget          # tells the old one too, and removes the saved address
q remote setup vm.work
```

If the old machine cannot be reached when you forget it, the command says so.
Run `q remote forget` there as well: a machine left believing in the pairing
waits out `remote.takeoverAfter` and then starts running the laptop's missions.

To pin a mission to a host at creation, use `q mission add ... --on local` or
`--on remote`. Run `q mission take ms_…` (or press `t` on its card) to bring an
existing mission to this machine.

### What each machine does

The laptop is the **primary** and the always-on machine the **secondary**. The
roles are not configured; the machine told how to reach the other is the
primary, because dialing is what a primary does.

Every mission is on both machines: its card, and once it has launched, a real
worktree per repository. What moves between them is which one runs the agent.

| situation | where the agent runs |
|---|---|
| the laptop is awake | on the laptop |
| the laptop has been silent for `remote.takeoverAfter` | on the secondary, including a mission whose agent was mid-turn |
| the same, with a [witness](#a-witness-for-split-networks) that says the laptop is still awake | on the laptop |
| the laptop is back, and an agent on the secondary finishes a turn | the next turn runs on the laptop |
| a mission was created with `--on remote`, or `--on local` | where it was pinned, always |

A card for a mission the other machine is running is marked `@mini`. Everything
you can do to a card still works. Messages and lane moves are carried out by
the machine running the agent; `enter` opens editors on this machine's own
worktrees, with the agent's pane attached to the other machine over ssh. Press
`t`, or run `q mission take`, to bring a mission here without waiting for a
turn to end.

### A witness for split networks

Silence is all the secondary can observe, and it has two causes. A closed lid
should be covered for. A laptop that is awake on a network the secondary is not
on, say off the VPN, should not be, and a laptop that wakes there should not
carry on with missions the secondary took while it slept. Without help the
secondary cannot tell these apart, and both machines run the same missions.

A witness is a third place both can reach when they cannot reach each other.
It holds one small record naming which machine is answerable for the pair's
missions:

- The laptop renews its claim every `remote.interval` while it is awake. The
  claim lasts `remote.takeoverAfter`.
- The secondary takes over only once it has gone that long without an exchange
  **and** the laptop's claim has lapsed, at which point it replaces it with its
  own. If it cannot reach the witness, it takes nothing.
- A laptop that finds the secondary's claim there, or that can reach neither
  the witness nor the secondary for long enough that the secondary may have
  acted, **stands by**. It stops the agents of the missions the secondary
  would have taken (mid-turn and not pinned `--on local`), marks them
  `standby`, and starts no queued ones.
- Standing by ends when the laptop completes an exchange with the secondary.
  It accepts whatever was taken over, takes the claim back, and restarts its
  own agent, in the conversation it had, for anything that was not.

Two stores are supported. Both machines consult the witness, naming the same
record. Run the command on the laptop and it runs the same one on the other
machine over ssh, so one command sets up the pair:

```sh
# a Kubernetes Lease, with each machine's own kubeconfig
q remote witness kubernetes --namespace q --lease laptop-and-mini

# or a blob in an Azure storage account, with `az login` or a managed identity
q remote witness azure mystorageaccount --container q

q remote witness status   # who the witness names, read live
q remote witness take     # claim it for this machine regardless
q remote witness clear    # stop consulting it, on both machines
```

Each machine reaches the witness with its own credentials. The other machine
uses its default kubeconfig and context unless `--peer-kubeconfig` or
`--peer-context` name others, and whatever `az login` says there. `--local`
changes this machine only; if the other machine could not be reached or could
not use the witness, the command says so and prints what to run there.

Either command reads the record and writes it back before saving anything, so
missing permissions are found then and not at the first closed lid. For
Kubernetes that is `get`, `create` and `update` on `leases` in the namespace;
for Azure, Storage Blob Data Contributor on a container that already exists.

Until both machines name the same witness nothing is taken over in either
direction, and `q remote status` says which machine is missing it. That is
deliberate: a check only one side makes protects nothing.

`q remote witness take` is the override for when you know better: the other
machine is gone for good, or you are off its network on purpose and want the
work here. The other machine is not told, so if it is running the same missions
both now are, and their work is merged as in
[When both machines changed something](#when-both-machines-changed-something).

The witness has to be reachable from wherever the laptop might be without the
VPN, or the laptop stands by whenever it is away from both. The two machines'
clocks need to agree to well within a tenth of `remote.takeoverAfter`, which
NTP does.

### How the code gets across

q does not push anything, and does not ask the agent to. On every exchange,
each machine records its worktrees as commits under `refs/q/` in your
repository. Those are not branches: they appear in no branch listing, reach no
forge, and are removed with the mission. A recorded state includes uncommitted
and untracked files, and is made without touching the worktree, the index, or
the branch. The laptop then moves those commits between the two repositories
with git, over the same command it reaches the other machine by.

The machine not running the agent lays each state out in its own worktree, on
the same commit, with the same changes still uncommitted. Ignored files are
never carried, so a `.env` or a build directory belongs to each machine
separately.

Repositories are matched by their `origin`, not by path, so
`~/dev/weave` on one machine and `/srv/src/weave` on the other are the same
repository as long as both are clones of the same remote and both sit under
`repos.roots`. A mission is not started, taken over, or resumed on a machine
that is missing one of its repositories; its card says which.

### When both machines changed something

A worktree is only ever overwritten when it has not been edited since the two
machines last agreed. If you fix something in the laptop's copy while the
agent is busy on the other machine, the card shows `local-edits` and the fix
waits until the agent is idle, then goes across.

If both sides changed, the laptop merges them once the agent is idle. Work
that was uncommitted stays uncommitted, and commits made on both sides are
joined by a merge commit. If the two changed the same lines, q does not pick
quietly: the laptop's version stays on the branch, the other machine's is kept
on `<branch>--<host>`, and the card shows `diverged` with that branch's name.

The usual way to get there is a laptop that slept mid-turn. Its agent is frozen
rather than dead, and wakes with it. The laptop then finds its mission was
taken over, stops its own agent, and whatever that agent did in the meantime
is merged like any other edit.

### What does not come across

The agent's conversation. An agent started on a machine the mission has just
arrived at is told that it is continuing from another machine, shown the last
thing the previous agent said, and pointed at `git status` and `git log`. If
the mission ran on this machine before, its earlier session is resumed with
that same note.

### Limits

- Without a [witness](#a-witness-for-split-networks), a laptop that is awake
  but cannot reach the other machine keeps running its missions, marked
  `unconfirmed`, and starts no queued ones. If the other machine has taken a
  mission over in the meantime, the work is done twice and merged when they
  next speak.
- With one, a laptop waking after a long sleep has its agents running for the
  few seconds it takes the daemon to notice and stop them.
- A takeover loses at most `remote.interval` of the laptop's work until the
  laptop returns with it.
- Both machines must run the same q. A mismatch stops the exchange and says so
  in `q remote status`.
- Merging needs git 2.40 or newer on the laptop.

## Configuration

q works with no configuration. When you want to change something, it reads
`~/.q-config.json`. `q config init` writes a file populated with the values q resolved on
your machine, which is the easiest starting point; `q config show` prints the effective
settings without writing anything.

```json
{
  "repos": {
    "roots": ["~/dev", "~/src", "~/code"],
    "maxDepth": 5,
    "skip": ["node_modules", "vendor", "target", "Library"]
  },
  "git": { "branchPrefix": "jane" },
  "agents": {
    "default": "claude",
    "modelRefresh": "6h",
    "agy": { "bin": "", "args": [], "model": "", "effort": "", "models": [] },
    "opencode": { "bin": "", "args": [], "model": "", "effort": "", "models": [] },
    "claude": { "bin": "", "args": [], "model": "", "effort": "", "models": [] },
    "codex": { "bin": "", "args": [], "model": "", "effort": "", "models": [],
               "configDir": "~/.codex", "profile": "q" }
  },
  "editor": { "command": ["nvim", "+Neotree"] },
  "terminal": { "mode": "ghostty", "command": [] },
  "paths": { "dataDir": "", "stateDir": "" },
  "cost": { "disabled": false, "models": { "gpt-5.6-sol": { "input": 4, "output": 20, "cacheRead": 0.4 } } },
  "tui": { "mouse": true },
  "tools": { "tmux": "/usr/bin/tmux" },
  "logLevel": "info"
}
```

| key | what it does |
|---|---|
| `repos.roots` | where the repo picker searches for git checkouts |
| `repos.maxDepth` | how many levels below a root to walk; the walk stops at each checkout |
| `repos.skip` | directory names never descended into, on top of hidden ones |
| `git.branchPrefix` | namespace for mission branches, e.g. `jane/add-endpoint`. Defaults to `$USER` |
| `agents.default` | the agent a new mission starts with |
| `agents.<agent>.bin` | absolute path to the agent, when it is not on `PATH` |
| `agents.<agent>.args` | extra arguments, appended after q's own and before the prompt |
| `agents.<agent>.model` | override the default model q discovers by asking the agent |
| `agents.<agent>.effort` | override the default reasoning effort |
| `agents.<agent>.models` | the models to offer when the agent itself cannot be asked |
| `agents.modelRefresh` | how often to re-ask each agent what it offers. Defaults to `6h` |
| `agents.codex.configDir` | where codex keeps its configuration. q writes only its own profile there |
| `agents.codex.profile` | the codex profile name q writes and selects |
| `editor.command` | argv opened on each changed worktree in a debrief. Defaults to `$VISUAL`, `$EDITOR`, then `vi` |
| `terminal.mode` | `ghostty`, `current`, `command`, or `none` — see below |
| `terminal.command` | the argv template for `command` mode |
| `paths.dataDir` | overrides where state and mission worktrees live |
| `paths.stateDir` | overrides where the daemon handle, hook spool, and logs live |
| `cost.disabled` | turns metering off: no transcripts are read and no card carries a cost |
| `cost.models` | rates in dollars per million tokens, keyed by model id, layered over the built-in table |
| `queue.maxConcurrent` | how many queued missions run at once on this machine. Defaults to `2` |
| `remote.ssh` | the command that gets a shell on the paired machine, e.g. `["ssh", "mini.local"]`. Setting it makes this machine the primary |
| `remote.bin` | the q binary on the paired machine. Defaults to `~/.local/bin/q` |
| `remote.name` | what this machine is called on cards. Defaults to its hostname |
| `remote.interval` | how often the primary exchanges state with its pair. Defaults to `15s` |
| `remote.takeoverAfter` | how long the pair waits for a silent primary before running its missions. Defaults to `5m` |
| `remote.witness.kubernetes` | keep the pair's claim in a Lease: `namespace` and `lease` (the same on both machines), and this machine's `kubeconfig` and `context`. Set with `q remote witness kubernetes` |
| `remote.witness.azure` | keep it in a blob instead: `account`, `container`, `blob`, and optionally `endpoint`. Set with `q remote witness azure` |
| `tui.mouse` | enable mouse support in the TUI (clicking cards, tabs, and scrolling). Defaults to `true` |
| `tools` | absolute paths for `git`, `tmux`, `osascript`, … overriding `PATH` |
| `logLevel` | `debug`, `info`, `warn`, or `error` |

### Terminal modes

Opening a debrief joins the mission's existing tmux session:

- **`current`** (the default on Linux and over SSH) attaches in the terminal running q.
  Inside tmux, Enter switches your client to the mission session; q keeps running in its
  original session. Outside tmux, the board pauses until you detach, then resumes.
  `q open <mission-id>` uses the same workflow.
- **`ghostty`** (the local macOS default) uses Ghostty 1.3+'s native AppleScript interface. The
  window is created inside the running Ghostty application, so macOS includes it in the
  normal ⌘-\` window cycle. The first one may prompt for Automation permission.
- **`command`** runs an argv template of your own. `{dir}` is replaced by the working
  directory, `{argv}` splices in the command's arguments, and `{cmd}` is the same command
  as one shell-quoted string. The template is expanded without a shell, so a path
  containing a space cannot change what runs.

  ```json
  { "terminal": { "mode": "command",
                  "command": ["wezterm", "start", "--cwd", "{dir}", "--", "{argv}"] } }
  ```

  Other examples: `["kitty", "--directory", "{dir}", "{argv}"]`,
  `["alacritty", "--working-directory", "{dir}", "-e", "{argv}"]`,
  `["gnome-terminal", "--working-directory={dir}", "--", "{argv}"]`.

- **`none`** opens nothing. q arranges the panes and tells you the
  `tmux attach-session` line to run yourself.

For a persistent remote board, start `tmux new-session -A -s q-board`, then run q.
Enter opens the selected mission. Use your tmux prefix followed by `s` to choose
the board session, or prefix then `L` to return to the previous session.
With the companion mac repo's config, the prefix is Ctrl+A (tmux's stock prefix
is Ctrl+B). Ctrl+S by itself is not the session picker.

You can also run q directly over SSH. Enter attaches; prefix then `d` detaches
and returns to the board without restarting q. Ctrl+D exits the active shell or
agent instead of detaching.

Existing explicit terminal settings still take precedence. To migrate a remote
config that names Ghostty or `none`, set `terminal.mode` to `current`, or run
`Q_TERMINAL=current q`. Restart the daemon after changing saved settings
(`q daemon restart`).

### Where settings come from

Built-in defaults, then the config file, then the environment. The environment wins so a
one-off run can point q somewhere else without editing the file:

| variable | overrides |
|---|---|
| `Q_CONFIG` | the config file location (also `--config`) |
| `Q_REPO_ROOTS` | `repos.roots`, as a `:`-separated list |
| `Q_DATA_DIR`, `Q_STATE_DIR` | `paths.dataDir`, `paths.stateDir` |
| `Q_EDITOR` | `editor.command` |
| `Q_TERMINAL` | `terminal.mode` |
| `Q_BRANCH_PREFIX` | `git.branchPrefix` |
| `Q_DEFAULT_AGENT` | `agents.default` |
| `Q_CLAUDE_MODEL`, `Q_CODEX_MODEL`, `Q_AGY_MODEL` | `agents.<agent>.model` |
| `Q_LOG_LEVEL` | `logLevel` |
| `Q_MOUSE` | `tui.mouse` (enable/disable mouse with `true`/`false`) |
| `Q_MAX_CONCURRENT` | `queue.maxConcurrent` |
| `Q_REMOTE_SSH` | `remote.ssh`, split on spaces |
| `Q_HOST_NAME` | `remote.name` |
| `Q_<TOOL>_BIN` | one tool's path, e.g. `Q_CODEX_BIN` |

The daemon reads the configuration when it starts, so after editing the file run
`q daemon restart`.

With nothing configured, q keeps state under the XDG directories:
`~/.local/share/q` for state and mission worktrees, `~/.local/state/q` for the daemon
handle, hook spool, and logs.

## Understanding

### The daemon

`q daemon` owns all state and supervises running agents. It starts on demand; you should
not normally need to think about it.

It exists as a separate process because agents outlive the board. A mission keeps running
after you close the TUI, its hooks still need somewhere to report status, and the
reconciler that keeps cards honest has to run when nobody is watching. Being the only
writer is also what makes a plain JSON state file safe.

One thing worth knowing: a running daemon keeps serving with the binary and environment
it started with, and `q daemon run` defers to it rather than replacing it. After
reinstalling, use `q daemon restart`. `q daemon status` reports which binary is running.

### Inside a mission

The launch script q writes exports four variables into every agent's session, so a
mission can identify itself and reach q without being told where either lives:

| | |
|---|---|
| `Q_MISSION_ID` | the mission the agent is running inside; also the default for `q mission add --from` |
| `Q_HOOK_EPOCH` | the launch generation, so events from an abandoned session can be discarded |
| `Q_DAEMON_FILE` | the daemon handle, as a path because a tmux session's environment prints in plaintext |
| `Q_BIN` | the absolute path to the q binary |

An agent's own `PATH` is its vendor's to set, and q is often not on it even though q
started the agent, so prefer `"$Q_BIN"` over a bare `q` when writing commands for the
mission to run.

### Models

A mission carries a model and, where the model takes one, a reasoning effort. Both are
chosen in the briefing form, frozen once the agent starts — they are baked into its
argv — and shown on the card.

q does not keep a list of model names. It asks the agents, because a table compiled into
q would be wrong within weeks and would not know what a given account is entitled to:

- **claude** answers an `initialize` control request over its stream-json protocol with
  its models, their descriptions, and the effort levels each accepts. The probe runs with
  `--no-session-persistence`, so it leaves no session behind for `--resume` or for q's own
  healer to trip over.
- **codex** answers `model/list` on its app-server. q starts a private app-server for the
  question rather than using the managed daemon, so this works on an install that has no
  managed daemon. If codex cannot be reached, q falls back to the models named in
  `~/.codex/config.toml` and offers no effort levels, because which efforts a model takes
  is knowable only from codex.

The daemon asks on startup and every `agents.modelRefresh` after that, caching the answer
so a restarted daemon has something to offer immediately. A refresh that returns no
models retains the last usable catalog rather than emptying the model picker.
`q models` prints the catalog and how long ago it was learned; `q models --refresh`
asks again now. Probing claude
starts it, which fires your own `SessionStart` hooks — that is why it is cached and
infrequent rather than done every time a form opens.

The default a new mission gets is what that agent would have used unprompted. Your own
configuration wins over the account default, in the agent's own precedence order: for
claude, `ANTHROPIC_MODEL`, then managed settings, then `model` in `~/.claude/settings.json`;
for codex, `model` under the profile q launches with, then the top-level one. `q doctor`
reports what each resolves to. Nothing validates a mission's model against the catalog —
a stale probe should not stop you launching a model the agent would have accepted.

OpenCode discovers models with `opencode models --verbose`, falling back to the plain
list and then configured `agents.opencode.models`. It supports model and effort
selection and plan mode. Choose `--tool opencode` on the CLI or select it in the
mission form; `agents.opencode.bin` and `Q_OPENCODE_BIN` select its executable.
q generates a mission-local status plugin under `.opencode/plugins/q-status.js`.

### Base branches

By default every worktree is cut from a freshly fetched default branch. A mission that
exists to review or build on work already pushed can say otherwise, per repo: `ctrl+g` in
the briefing form lists every repo the mission spans — the ones inherited from its
operation as well as the ones it adds — and picking one opens a type-to-filter box over
that repo's branches.

```sh
q mission add review-login --operation "$op" --prompt "Review it." --base weave=feat/login
```

Only the starting point changes. The worktree still gets the mission's own
`<prefix>/<slug>` branch, so two missions can be based on the same branch and neither can
move the other's working branch by committing. The generated prompt distinguishes
the working branch from the selected existing branch:

```
- weave: ./weave (branch jane/review-login, from origin/feat/login at 1a2b3c4)
  User-selected existing branch: feat/login on origin. jane/review-login is this mission's isolated working branch.
```

Selecting a base does not authorize publishing. For a task that updates the selected
branch, the prompt tells agents to commit on the mission branch and use the selected
branch as the destination when a push is authorized, or as the target of a requested
PR/MR. Before a direct push, agents should fetch, integrate concurrent changes, and
rerun relevant checks, then use an explicit destination such as
`git push origin HEAD:refs/heads/feat/login`. If another mission pushes first, fetch
and integrate again instead of force-pushing. These are agent instructions; q does
not automatically synchronize or publish mission branches.

The picker fills from the remote-tracking refs already on disk, then merges in whatever
origin reports a moment later, so a branch pushed since your last fetch still appears. A
branch it has never heard of can be typed anyway; if it turns out not to exist, the launch
fails saying so. Base branches are fixed once a mission launches, like its repositories.

### Lanes

`briefing → active → awaiting orders → debrief → closed`

Three moves do more than bookkeeping. Moving an unlaunched mission **into active**
launches the agent. Moving **into active** from awaiting or debrief resumes it, delivering
whatever you type as its next message and restarting the agent first if its session has
died. Moving **into closed** stops the agent and reclaims its worktrees while retaining
the card as history.

`closed` is terminal: no agent event moves a card out of it. Filing refuses uncommitted
changes until you explicitly confirm their loss. Branches with unpushed commits are kept.

**awaiting orders** covers a permission prompt and one more thing: a turn that ended by
asking you a question. Both look like a finished turn at the hook level, so q reads the
closing message, and a turn that ends on an ask — "Should I:" above two options, or
anything ending in a question mark — lands in *awaiting orders* with the ask as the card's
subtitle rather than in *debrief*. Answering in the pane clears it, like any other block.

Alongside the lane, each card shows what its agent is observably doing (`◐` busy, `⏸`
waiting, `○` idle, `✕` gone). These are deliberately separate. An agent can be mid-thought
while its card correctly sits in *debrief* because you have not looked yet, and conflating
the two is what makes a board lie.

For small ad-hoc work, a repo-less operation keeps the organization deliberately loose
while each mission names only what it needs:

```sh
misc=$(q operation add "Misc" --summary "Small ad-hoc work")
q mission add update-readme --operation "$misc" --prompt "Improve the setup docs." \
  --repo ~/dev/q
```

### Cost

Each card carries a running total of what its mission has consumed:

```
◆ claude · $1.23 · 2r · 14m
```

**It is an estimate of equivalent API spend, not a bill.** A session running under a
subscription is not charged per token at all. The number exists so that two missions can
be compared against each other — which one ran away with your afternoon — and so a card
sitting in *debrief* can tell you what it cost to get there.

q reads it from what the agent already writes down. Both agents log their own usage and
hand q the path on their hooks, so metering makes no network call, needs no credentials,
and adds nothing to the bill it reports on. A mission is measured when a turn ends and
again on the reconciler's tick while it is still running, so the total grows during a long
turn rather than appearing when it finishes.

Two things the number tells you about itself:

- A trailing `+` (`$1.23+`) means a model in the mission had no rate in the price table,
  so the figure is a floor rather than a total.
- A token count instead of a figure (`1.2M`) means *nothing* in the mission had a rate.
  Unknown models can be priced under `cost.models`.

q ships Claude rates (including Opus/Sonnet 5.5 and Fable/Mythos 5.1) and OpenAI rates
for GPT-6.1 Sol, GPT-6 Astra/Sol/Luna, GPT-5.6 Sol/Terra/Luna, GPT-5.5, GPT-5.4,
and GPT-5/5.1/5.3 Codex.
Codex usage is priced per response using the recorded turn model, including discounted
cached input. Rates use [standard short-context API pricing](https://developers.openai.com/api/docs/pricing)
and [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing),
verified October 5, 2026; they exclude service-tier, long-context, regional, and tool
surcharges. This is a comparison estimate, not a subscription charge. Override rates
with `cost.models` as needed; metering does not fetch prices at runtime.
Catalog availability does not imply a published price: `gpt-reserve` and
`codex-auto-review` remain unpriced. OpenCode and Antigravity currently have no
usage meter, so adding a model rate alone does not enable cost display for them.

`q doctor` reports any model your missions used that the table cannot price, which is how
a table that has fallen behind announces itself rather than quietly under-reporting.

### Usage limits

When you start a mission, the agent row warns you if that agent's usage window is
currently used up:

```
  Agent      ◆ claude
             ⚠ 5-hour limit hit · resets 4:20pm
```

It warns; it does not block. q cannot see your account's real quota — it only knows what
the agents write into their own logs, and the two write different things. codex records
how much of each window is gone, so q can say a window is exhausted the moment it is.
claude records nothing until a request is actually refused, so for claude the warning is
retrospective: it appears once something has been turned away, and lasts until the window
it named reopens.

### Naming repos

An operation's repos and a mission's additional repos use the same picker. Repos go in one
per line, but not in full. Type part of a checkout's name and press `enter`: q replaces
the line with the path, so `weave` becomes `/Users/you/dev/weave`, and leaves you on a
fresh line for the next one. Several matches open a picker. An exact name wins outright,
so `bob` means `bob` even though `bob.next` matches too, and a fragment with a slash —
`labs/pipeline` — narrows a nested checkout.

Only git checkouts are offered, because an operation's repos are worktree sources and
anything else could never be one. The search covers `repos.roots` (by default `~/dev`,
`~/src`, and `~/code`) five levels deep and stops inside anything it has already
recognized as a checkout. Pasting a full path still works, and a line that is not a full
path when you save is reported rather than stored.

### Worktrees

Launching a mission creates one worktree for every operation repo plus every additional
mission repo under `~/.local/share/q/missions/<operation>--<mission>/`, all on
`<prefix>/<mission-slug>`, branched from a freshly fetched default branch. That combined
set is frozen at launch, so later edits to the operation cannot change which worktrees the
mission resumes or reclaims.

Worktrees are not clones: they share your repo's object database, so a three-repo mission
costs a few megabytes. They contain **tracked files only** — no `node_modules`, no
`.terraform` — so an agent that needs those runs the install step itself.

Finishing or deleting a mission reclaims them. Finishing keeps the card; deleting removes
it. A branch is deleted only when nothing would be lost with it; one carrying commits that
are not pushed anywhere is kept and reported. A worktree holding uncommitted changes is
refused, because git refuses it and that refusal is the last thing between a keystroke and
lost work. Explicit confirmation overrides it, and even then a branch with commits is
kept.

### Plan mode

A claude mission can start in plan mode. When the agent finishes planning it asks to leave
plan mode, q routes that to **debrief** rather than to *awaiting orders*, and opening the
debrief drops you into the live approval dialog. Approving it there switches claude into
accept-edits and the card returns to *active*. Nothing is killed or restarted.

OpenCode missions also support `--plan`; q launches the plan agent and handles its
`plan_exit` approval event.

codex has no plan mode, so the toggle is disabled for codex missions and says why. The toggle is also disabled for agy: its CLI has a plan mode, but q does not yet have a verified plan-approval event for it.

## Operating it

`q doctor` is the first thing to run when something looks wrong. It reports where the
configuration came from and what it resolved to, the resolved path and version of every
external tool, where state lives, mission directories with no mission behind them, and
environment variables that would degrade q.

Logs are in `~/.local/state/q/logs/` and rotate at 8 MB. They are `log/slog` logfmt,
so `grep 'level=WARN'` and `grep mission=ms_...` both work.

Each mission keeps its generated artifacts in `<mission-dir>/.q/`: the composed prompt,
the agent's hook configuration, and `launch.sh`. **`launch.sh` can be run by hand** to
reproduce a launch exactly, which is the fastest way to understand a mission that
misbehaved.

### A card is not updating

Look for a `hooks-silent` badge. It means the agent has never reported, so q cannot track
hook-specific details. Usually the agent is stopped at a startup prompt; q reads the pane
and puts the question on the card, so attach to the session and answer it. For codex,
app-server status still supplies busy, waiting, and idle state and can recover the session
id from the mission directory even when the startup hook was missed.

The first codex mission ever run needs a one-time approval of q's hooks. Attach and choose
*Trust all and continue*. Codex records the approval in `[hooks.state]` within q's
profile, and q preserves that codex-owned section when refreshing its mission-directory
entries.

### Codex specifics

q writes `~/.codex/q.config.toml` and selects it with `--profile q`. That file pre-trusts
mission directories, because codex otherwise stops to ask about them in a detached session
where nobody would see the question. Your own `~/.codex/config.toml`, including your
approval policy, sandbox mode, and MCP servers, is never modified.

Codex missions start the managed app-server and connect the ordinary terminal UI to its
local Unix socket. The q daemon uses a separate read-only proxy connection and polls
`thread/read`; it never resumes the thread and therefore never receives or answers the
terminal's approval requests. Structured runtime states distinguish active work,
approvals, user input, idle turns, and system errors. If app-server is unavailable, the
launch script falls back to the direct codex invocation and existing hooks.

Codex briefly reports some automatically reviewed tool calls as approval requests. q holds
those requests for two seconds before moving the card, so routine tool use stays *active*
while a real unattended approval still reaches *awaiting orders*. A tool starting or codex
returning to active work clears the pending request immediately. The same grace period
applies to the hook fallback when app-server status cannot be read.

## Security

q sends nothing anywhere except what the agents themselves send, unless you
pair it with a second machine of your own.

The daemon listens on an ephemeral loopback port and requires a bearer token compared in
constant time, a loopback peer, and a q-specific header. The token lives only in
`~/.local/state/q/daemon.json` at mode 0600 and is rotated on every start. It is never
placed in an environment variable or argv, because `tmux show-environment` prints session
environments in plaintext and `ps -E` can expose them; children are told the path to the
file and read it themselves.

Everything q writes is owner-only. State holds mission prompts.

Pairing two machines opens no port on either. The primary reaches the secondary
by running a command, ssh by default, whose far end is `q rpc`: it reads one
request, relays it to that machine's own loopback daemon with the token from
that machine's own handle file, and exits. So the access q relies on is the
shell access you already have, and the daemon's promise to accept connections
only from its own host holds on both sides. A machine remembers the first peer
that pairs with it and refuses a different one until told to `q remote forget`.

Worktree states travel between the two repositories as git objects under
`refs/q/`, over the same command. q allows git's `ext` transport for those
invocations only, with a URL it builds from your configuration.

q reads claude's session registry to recover from missed hooks. That directory also holds
credential files, so the scan reads only `*.json`, and a test plants a decoy key file to
prove it stays closed.

No trust check is ever bypassed. codex offers a flag to skip hook trust; q does not use
it, because it would disable the check for every hook in the invocation rather than just
q's.

## Google Antigravity CLI (agy)

Select `agy` in the mission form or pass `--tool agy` when creating a mission.
Set `agents.default` to `agy` to use it by default. Configure `agents.agy.bin`
and `agents.agy.args` like the other agents; `Q_AGY_BIN` overrides the executable.
q searches PATH, `~/.local/bin/agy`, and `/opt/homebrew/bin/agy`. `q doctor`
reports whether it can find the executable. Authenticate with `agy` interactively
before launching your first mission.

q starts the interactive CLI with the mission prompt and adds each repo worktree
with `--add-dir`. It records the conversation ID from hooks and uses
`--conversation` when relaunching. If no ID was reported, relaunch starts a new
conversation instead of risking continuation of a different mission.

q writes a `q-mission` entry in the mission root's `.agents/hooks.json`, preserving
other entries. Malformed existing hook files are left untouched; repair the JSON
and relaunch if hooks are silent. Global agy configuration stays in place.

The [agy hook contract](https://antigravity.google/docs/hooks/) supports activity,
turn completion, errors, and whether background work remains. Permission waits
are not reported, so attach to the mission to answer permission prompts even if
its card still shows active. q's plan-approval toggle is unavailable for agy.

Agy participates in `q models`, automatic catalog refresh, and the board's model
and effort pickers. q reads `agy models` and passes mission selections through
`--model` and `--effort`, including on resume. Set `agents.agy.model` (or
`Q_AGY_MODEL`) and `agents.agy.effort` for defaults. `agents.agy.models` supplies
fallback choices when discovery fails. The CLI catalog does not identify its
default, so q leaves the model unset unless configured or selected.

Agy cost and limit metering is unavailable: the local 1.1.27 transcripts contain
no token-usage or quota-reset records. Its missions display no cost, and `q doctor`
reports this limitation. Configuring prices alone cannot supply missing usage.

## Contributing

Pull requests run [CI](.github/workflows/ci.yml) on GitHub-hosted Linux and macOS
runners: formatting, `go vet`, the full test suite with the race detector, a build,
and release-version tests. The same checks gate every release on `main`.
No local runner, agent login, or API credentials are needed: agent/terminal tests
use fakes, and the git integration tests use temporary repositories.

```sh
go build ./... && go vet ./... && go test ./...
python3 -m unittest discover -s scripts -p 'test_*.py'  # release-version tests
go test -race ./...       # the store is shared between concurrent hook processes
golangci-lint run ./...   # optional; .golangci.yml is the project's config
```

[`TESTING.md`](TESTING.md) covers what the automated tests actually pin down and what only
a human can verify. A few expectations worth knowing before opening a pull request:

**Anything that runs a subprocess goes through `internal/runner`.** It is the only place
that calls `os/exec`, which is what keeps the gosec suppression to one justified site. New
external commands should be argv built from validated state, never a shell string.

**External-tool behavior is verified, not assumed.** Several of this tool's hardest bugs
came from a flag that looked applied and was not. If a change relies on how `claude`,
`codex`, `tmux`, `git`, or a terminal emulator behaves, say how that was checked, and add
a test asserting the argv or the generated file.

**Constraints are enforced by construction where possible.** `internal/git` exposes no
general `Fetch`, only an explicit single-refspec one, because a user's global git config
may set `fetch.all` and `fetch.force`. `internal/terminal` targets render their own `=`
prefix, because tmux otherwise prefix-matches session names. The TUI keymap has a
`Forbidden` set for keys tmux intercepts. A change that works around one of these instead
of extending it deserves scrutiny.

**Destructive paths need a refusal, not a warning.** Deleting a mission can discard an
agent's uncommitted work. The rule is that git's own refusal is surfaced rather than
overruled, and forcing is explicit.

**Comments explain why, not what.** The non-obvious constraints are the valuable part of
this codebase; a change that drops one of those explanations loses more than it looks.

### Releases and versioning

[Release](.github/workflows/release.yml) runs on every push to `main`, including
merged pull requests. After Linux and macOS checks pass, it creates a `vMAJOR.MINOR.PATCH`
tag on that exact commit and a GitHub release with generated notes, macOS/Linux
binaries for `amd64` and `arm64`, and SHA-256 checksums. Release runs queue while
another is in progress. The workflow uses GitHub's built-in token; no personal
access token or self-hosted runner is required.

The first release is `v0.1.0`. Later releases examine commit messages since the
highest stable version tag, using [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

| commit message | next version from `v1.2.3` |
|---|---|
| `fix: …`, `docs: …`, or an ordinary commit message | `v1.2.4` |
| `feat: …` or `feat(queue): …` | `v1.3.0` |
| `feat!: …`, `fix(api)!: …`, or a `BREAKING CHANGE:` / `BREAKING-CHANGE:` footer | `v2.0.0` |

The largest bump wins. Breaking changes increment the major even before `v1.0.0`.
Use the convention in the squash commit title/body when squash-merging; normal
merges also inspect the branch's commits. Every successful main push releases,
including documentation-only changes. Prerelease and unrelated non-version tags
are ignored. Failed checks publish nothing. Rerunning an interrupted release
reuses its tag and repairs asset uploads; a delayed run whose commit already has
a newer descendant release skips publishing older code as latest.

### Layout

Packages are cut by domain, and every arrow in the import graph points inward toward
`internal/mission`.

| package | what lives there |
|---|---|
| `cmd/q` | the command tree, `~/.q-config.json`, tool resolution, and all wiring |
| `internal/mission` | operations, missions, lanes, the state machine, the store, and the interfaces the rest implement |
| `internal/api` | the daemon protocol: wire types, the handle, and the client |
| `internal/daemon` | the service rules, the HTTP server, hook intake, the reconciler, the scheduler, and the exchange with a paired q |
| `internal/remote` | reaching the q daemon on another machine over ssh |
| `internal/k8s` | the pair's witness as a Kubernetes Lease |
| `internal/azure` | the pair's witness as a blob in an Azure storage account |
| `internal/claude` | running missions with `claude`, and reading its session registry |
| `internal/agy` | running Antigravity missions and discovering its models |
| `internal/opencode` | running OpenCode missions, its status plugin, and model discovery |
| `internal/codex` | running missions with `codex`, and its app-server client |
| `internal/git` | git operations, worktree provisioning and reclaim, checkout discovery |
| `internal/terminal` | tmux, and the window openers one per terminal strategy |
| `internal/launch` | the launch sequence: provision, write, start, relaunch |
| `internal/debrief` | arranging and attaching a debrief session |
| `internal/runner` | the single seam through which q executes external programs |
| `internal/paths` | the on-disk layout |
| `internal/spool` | hook events buffered while the daemon is down |
| `internal/usage` | reading what a session consumed out of the agents' own logs, and pricing it |
| `internal/tui` | the board |

**Adding an agent** is three edits: an entry in the `known` table in
`internal/mission/tool.go`, a package implementing `mission.Agent` — what argv it takes,
what files it needs written, which hook events it reports — and a line in
`cmd/q/assemble.go` that builds it from config. If it can report on its own live sessions
it also implements `mission.Runtime` (authoritative, polled) or `mission.Healer`
(advisory, used only to correct a card a dropped hook left wrong), and if it writes down
what its sessions consume, `mission.Meter`. Nothing else branches on which agent a mission
uses.

## License

MIT. See [LICENSE](LICENSE).
