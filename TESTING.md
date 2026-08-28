# Testing q

```sh
make tidy lint-blocking lint   # formatting and both lint sets
make test                      # unit and integration tests
make raceif                    # the same tests under the race detector
```

`make raceif` matters more here than in most services: hook events arrive from many
concurrent processes while the TUI reads the same state, and the store is shared between
them.

## What the automated tests cover

The valuable tests are not the ones with the highest coverage. They are the ones pinning
down behavior that was expensive to discover, mostly about the external tools q
drives.

**`internal/statem`** is the status state machine, and the most load-bearing test in the
repo. It is a pure function, so every transition is asserted directly: the whole plan-mode
arc, a `Stop` racing a `PermissionRequest` in both arrival orders, `closed` being terminal,
stale sessions and stale hook epochs being ignored, and `PostToolUse` clearing a waiting
card, which is what stops the board lying when you answer a prompt in the pane without
touching q.

**Argv construction** is asserted for git, tmux, every terminal mode, and both agents, because that is
where the subtle failures live. Specific things a test will fail on:

- a `git fetch` without both an explicit remote and refspec, which the user's global
  `fetch.all` and `fetch.force` would otherwise widen
- a tmux target missing its `=` prefix, which makes tmux prefix-match session names
- claude's variadic `--add-dir`, which silently swallows the positional prompt after it
- any bypass or trust-override flag in codex's argv
- a Ghostty launch that creates a separate application instance instead of using the
  native AppleScript window API, and a `terminal.command` template that never says where
  the command goes
- a TUI binding using `ctrl+h/j/k/l`, which tmux intercepts before q sees them

**Generated files** are golden-tested: the composed prompt, claude's settings, and codex's
profile. The codex profile test also asserts its `[hooks]` section stays byte-identical as
project entries change above it, because codex keys hook trust on the handler and a change
would silently require re-approval.

**Closing-question detection** is tested from both sides, using a real codex message that
ended on a numbered list of options: an ask has to reach *awaiting orders* with the ask as
the subtitle and no finish time, and a sign-off ("let me know if you want anything else"),
a summary list, and a rhetorical question have to stay in *debrief*. The false positive is
the expensive one — it parks finished work in a lane that asks for attention forever.

**Repo completion** is tested both ways: `internal/repofind` over a temporary tree for what
the walk finds and how a fragment ranks, and the operation form for what a keypress does —
a unique match completing in place, an ambiguous one opening a picker that `esc` returns
from with the form intact, a truncated picker saying how many it left out, and a line that
was never completed being refused at save rather than stored as a path the daemon cannot
resolve.

**The message-sending guard** has its own tests. If an agent has exited and its pane fell
back to a shell, pasting a message would type it into that shell and run it. Sending is
refused unless the pane is alive and running an agent.

**Delete disposition** is tested across every case: clean, unpushed commits, pushed
commits, uncommitted changes, a missing worktree, and a checkout that can no longer be
inspected.

## What a human has to test

Some behavior only exists when a real agent is running. None of it is covered above.

Before starting, `./install.sh && q daemon restart`, because a running daemon keeps
serving with the binary it started with.

### The status feedback loop

This is the thing worth checking first, because everything else depends on it.

1. Create a operation with one repo, then a claude mission, and launch it.
2. `watch q mission list`, or open the board.
3. Provoke a permission prompt — ask the agent to do something that needs one.
4. The card should reach **awaiting orders** within about a second, showing the tool name.
5. Answer the prompt **in the pane**, without touching q. The card should return to
   **active** on its own. This is the self-heal path and the most common real flow.
6. Let the mission finish. The card should reach **debrief** with the agent's closing message
   as its subtitle.
7. `tmux kill-session -t <session>`. Within 15 seconds the card should gain a `tmux-gone`
   badge and move to debrief.
8. `q daemon stop`, provoke more events, then restart. The spooled events should apply in
   order, so nothing is lost.

### Plan mode

Launch a claude mission with `--plan`. The card should reach **debrief** when the plan is
ready, *not* awaiting orders. Press `enter`: you should land in the live approval dialog.
Approve it there and the card should return to **active** with nothing restarted.

### Review sessions

With a mission that has changed something, press `enter`. Expect one tmux window with the
live agent plus one editor pane per **changed** repo — an untouched repo should get no
pane. Press `enter` again: no duplicate panes. Close a pane by hand and press `enter`
again: it should come back.

With the default `ghostty` terminal mode, Ghostty 1.3 or newer is required. Whether q is
running inside or outside tmux, this opens a new Ghostty window in the existing
application instance and leaves the board's current tmux client alone. The first launch may produce macOS's one-time Automation
permission prompt. Ghostty must not ask separately for permission to execute tmux on each
launch.

With the debrief window focused, `cmd+backtick` should cycle to the board and back. Closing
the debrief window before moving the mission to closed is the expected workflow; Ghostty 1.3.1
does not reliably auto-close command-backed AppleScript windows when the command exits.

If the session is already attached elsewhere, the new Ghostty window should attach as an
additional client without detaching the existing one.

### Codex

The first codex mission ever run stops at **Hooks need review**. The card should say so
rather than sitting silent — attach, choose *Trust all and continue*, and the approval
persists for every later mission. Confirm `~/.codex/config.toml` is unchanged apart from the
trust entry codex itself writes — q writes only `~/.codex/q.config.toml`.

There should be **no directory-trust prompt**, ever. If one appears, the profile's project
entries are not being written.

Run several ordinary Codex tool calls that are approved automatically. Brief approval
checks must leave the card **active** without flashing through **awaiting orders**.

Then provoke an approval prompt that remains unanswered. After about two seconds, the card
should move to **awaiting orders** with the tool name or `Codex approval`. Approve it in the
terminal; as soon as Codex resumes, the card should return to **active**, without
waiting for the tool to finish. `q doctor` should report `codex app-server  available`.

### Finishing

Move a launched mission to **closed**. Its card should remain, but its tmux session, worktrees,
and mission directory should be gone. Confirm the Codex or Claude process and any editor
panes from that session also exited.

- clean or pushed work: finish immediately and delete the local branch
- unpushed commits: show the branch that will be kept before finishing
- uncommitted changes: require explicit confirmation before discarding them
- a simulated cleanup failure: leave the card unfinished so cleanup can be retried

### Deleting

`q mission rm <id> --dry-run` first; it should name exactly what would be lost. Then:

- a clean mission: worktree removed, branch deleted, mission directory gone
- a mission with unpushed commits: worktree removed, **branch kept and reported**
- a mission with uncommitted changes: **refused**, and nothing touched
- the same with `--force`: worktree discarded, but a branch with commits still kept

### Non-interference

This is the property most worth being sure of, because the failure is other people's work.

Throughout all of the above, your own checkouts must be untouched:

```sh
git -C ~/dev/… status          # unchanged
git -C ~/dev/… rev-parse --abbrev-ref HEAD   # still your branch
```

Also confirm your own claude hooks still fire during a q-launched mission. q
appends its hooks rather than replacing yours, and that only works because claude merges
settings arrays.

### Recovering from a reboot

Restart the machine, or `tmux kill-server`. Every mission should come back badged
`tmux-gone` rather than silently wrong. Moving one back to active should relaunch the
agent against its surviving worktrees and resume the conversation.

## Things known not to be covered automatically

Stated plainly so nobody assumes otherwise:

- **Codex app-server against a real authenticated turn.** Protocol parsing, generated
  launch commands, state mapping, missed-hook recovery, and the doctor probe are tested,
  but the terminal/app-server pairing still needs the manual check above after a Codex
  upgrade.

## Testing a configuration change

`~/.q-config.json` changes what q resolves, so anything touching it wants two checks:

```sh
q config show     # the effective settings, after the file and the environment
q doctor          # what those settings actually resolved to on this machine
```

A change to `terminal`, `editor`, or `tools` is not tested until a real debrief has been
opened with it, because those values are only used at that moment. A change to `paths` or
`repos` should be visible in `q doctor` immediately.

Remember that the daemon reads the configuration when it starts. After editing the file,
`q daemon restart`.
