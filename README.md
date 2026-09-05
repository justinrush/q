# q

`q` is a terminal UI for running coding agents across several git repos at once.

Q runs the mission board. An **operation** is an area of investigation: a high-level
summary plus the repos it spans. A **mission** is one unit of agent work inside an
operation. It inherits the operation's repos and can add repos needed only for that
mission. q gives each mission its own git worktree per repo, starts `claude` or `codex`
in a detached tmux session, and shows every mission on a board that updates itself as the
agents work.

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

## Requirements

- **git** and **tmux**
- **[claude](https://claude.com/claude-code)** or **[codex](https://developers.openai.com/codex/cli)** — at least one; q drives both
- **Go 1.26+** to build
- macOS or Linux. Opening a debrief window natively uses
  [Ghostty](https://ghostty.org) 1.3+ on macOS; everywhere else you name your own
  terminal command, or let q print the `tmux attach` line for you. See
  [Configuration](#configuration).

## Install

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

`q doctor` is worth running first. It reports the resolved path and version of every
external tool, where state lives, and what your configuration actually resolved to.

## Using it

Press `?` for the full keymap. The essentials:

| key | action |
|---|---|
| `tab` | switch between Board and Operations |
| `a` (Operations) | add an operation: a summary plus the repos it spans, named by fragment |
| `n` (Board) | new mission, optionally with a model, effort, and additional repos; `ctrl+s` saves, `ctrl+r` launches |
| `H` / `L` | move a card between lanes. Out of briefing launches the agent |
| `enter` | open a debrief: attaches to the live agent and opens an editor per changed repo |
| `m` | send a message to a running agent |
| `d` | delete a mission and reclaim its worktrees |
| `/` | filter the board to one operation |

Everything is also scriptable, which is the quickest way to see what the board is doing:

```sh
op=$(q operation add "Discussions API" --summary "…" --repo ~/dev/weave --repo ~/dev/azure-tf)
q mission add discussions-endpoint --operation "$op" --prompt "Add the endpoint." --plan
q mission add tidy-imports --operation "$op" --prompt "Tidy them." --model haiku
q models                       # what each agent offers, and the default a mission gets
q mission move ms_… active     # launches the agent
q mission list                 # the board, as text
q open ms_…                    # open the debrief session
q mission rm ms_… --dry-run    # what deleting would discard
```

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
    "claude": { "bin": "", "args": [], "model": "", "effort": "", "models": [] },
    "codex": { "bin": "", "args": [], "model": "", "effort": "", "models": [],
               "configDir": "~/.codex", "profile": "q" }
  },
  "editor": { "command": ["nvim", "+Neotree"] },
  "terminal": { "mode": "ghostty", "command": [] },
  "paths": { "dataDir": "", "stateDir": "" },
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
| `terminal.mode` | `ghostty`, `command`, or `none` — see below |
| `terminal.command` | the argv template for `command` mode |
| `paths.dataDir` | overrides where state and mission worktrees live |
| `paths.stateDir` | overrides where the daemon handle, hook spool, and logs live |
| `tools` | absolute paths for `git`, `tmux`, `osascript`, … overriding `PATH` |
| `logLevel` | `debug`, `info`, `warn`, or `error` |

### Terminal modes

Opening a debrief means putting a terminal in front of the mission's tmux session, and
there is no portable way to do that. So:

- **`ghostty`** (the macOS default) uses Ghostty 1.3+'s native AppleScript interface. The
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

- **`none`** (the default off macOS) opens nothing. q arranges the panes and tells you the
  `tmux attach-session` line to run yourself, which is what a headless or remote setup
  wants.

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
| `Q_CLAUDE_MODEL`, `Q_CODEX_MODEL` | `agents.<agent>.model` |
| `Q_LOG_LEVEL` | `logLevel` |
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
so a restarted daemon has something to offer immediately. `q models` prints the catalog
and how long ago it was learned; `q models --refresh` asks again now. Probing claude
starts it, which fires your own `SessionStart` hooks — that is why it is cached and
infrequent rather than done every time a form opens.

The default a new mission gets is what that agent would have used unprompted. Your own
configuration wins over the account default, in the agent's own precedence order: for
claude, `ANTHROPIC_MODEL`, then managed settings, then `model` in `~/.claude/settings.json`;
for codex, `model` under the profile q launches with, then the top-level one. `q doctor`
reports what each resolves to. Nothing validates a mission's model against the catalog —
a stale probe should not stop you launching a model the agent would have accepted.

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

codex has no plan mode, so the toggle is disabled for codex missions and says why.

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

q runs entirely on one machine and sends nothing anywhere except what the agents
themselves send.

The daemon listens on an ephemeral loopback port and requires a bearer token compared in
constant time, a loopback peer, and a q-specific header. The token lives only in
`~/.local/state/q/daemon.json` at mode 0600 and is rotated on every start. It is never
placed in an environment variable or argv, because `tmux show-environment` prints session
environments in plaintext and `ps -E` can expose them; children are told the path to the
file and read it themselves.

Everything q writes is owner-only. State holds mission prompts.

q reads claude's session registry to recover from missed hooks. That directory also holds
credential files, so the scan reads only `*.json`, and a test plants a decoy key file to
prove it stays closed.

No trust check is ever bypassed. codex offers a flag to skip hook trust; q does not use
it, because it would disable the check for every hook in the invocation rather than just
q's.

## Contributing

```sh
go build ./... && go vet ./... && go test ./...
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

### Layout

Packages are cut by domain, and every arrow in the import graph points inward toward
`internal/mission`.

| package | what lives there |
|---|---|
| `cmd/q` | the command tree, `~/.q-config.json`, tool resolution, and all wiring |
| `internal/mission` | operations, missions, lanes, the state machine, the store, and the interfaces the rest implement |
| `internal/api` | the daemon protocol: wire types, the handle, and the client |
| `internal/daemon` | the service rules, the HTTP server, hook intake, and the reconciler |
| `internal/claude` | running missions with `claude`, and reading its session registry |
| `internal/codex` | running missions with `codex`, and its app-server client |
| `internal/git` | git operations, worktree provisioning and reclaim, checkout discovery |
| `internal/terminal` | tmux, and the window openers one per terminal strategy |
| `internal/launch` | the launch sequence: provision, write, start, relaunch |
| `internal/debrief` | arranging and attaching a debrief session |
| `internal/runner` | the single seam through which q executes external programs |
| `internal/paths` | the on-disk layout |
| `internal/spool` | hook events buffered while the daemon is down |
| `internal/tui` | the board |

**Adding an agent** is three edits: an entry in the `known` table in
`internal/mission/tool.go`, a package implementing `mission.Agent` — what argv it takes,
what files it needs written, which hook events it reports — and a line in
`cmd/q/assemble.go` that builds it from config. If it can report on its own live sessions
it also implements `mission.Runtime` (authoritative, polled) or `mission.Healer`
(advisory, used only to correct a card a dropped hook left wrong). Nothing else branches
on which agent a mission uses.

## License

MIT. See [LICENSE](LICENSE).
