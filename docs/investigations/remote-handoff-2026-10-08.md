# Concierge provisioning handoff investigation

Mission `ms_92072810e841`, inspected October 8, 2026 (America/Chicago).

## Evidence

- Created at 05:43:23 and launched at 05:43:28 on Justins-Mac-mini.
- Mini event log: `mission.hook.stop` at 05:51:10.010, followed by
  `mission.lease.return` at 05:51:10.915.
- Both saved records retain `startedAt`, debrief status, last response, and usage.
  Lease epoch 2 names Justins-MacBook-Air. The task was not reset to briefing.
- Mini retains Claude session `e554d78a-fc75-4641-8ca2-4d1da26accd7`
  and its 942,969-byte transcript. All six worktrees exist and report clean status.
- Laptop has mirrors for denied-life, maestro, and ticketbooth, but lacks
  checkouts for concierge, steward, and courier. Its daemon repeatedly logged
  those missing repositories before handback.
- The final saved response says stage 1 was pushed to denied-life's
  `justinrush/concierge-provisioning` branch (MR !10), awaiting the human's merge
  and `maestro apply`. This is the agent's recorded report, not independent
  verification of the forge or deployment.

## Cause

`daemon.handBack` returns unpinned, completed turns to a reachable primary.
`release` stops the secondary's tmux session before transferring ownership.
It does not establish that the primary has a complete mirror. Consequently
this handback moved ownership to a host unable to resume the mission.

`debrief.Opener.Open` then interpreted an empty local tmux session name as
"never been launched", even though launch history was intact. This also
prevented the normal missing-session relaunch flow. The accompanying fix
returns `NeedsRelaunch` for launched missions without a local session.
The daemon's existing completeness check still prevents an incomplete launch.

## Recovery and remaining work

The original conversation is recoverable on the Mini; no state reconstruction
or new mission is needed. Moving ownership back alone is insufficient: an
unpinned mission can immediately hand back again while between turns.
The current update API refuses changing a pin after launch. A durable recovery
on the Mini therefore needs a supported post-launch pin change or a coordinated
backup/stop/edit/restart of the Mini's state, followed by taking the mission and
resuming its retained session. Do not edit a live daemon's state file.

Alternatively, clone the three missing repositories on the laptop, allow a
complete mirror, then resume there with a continuation note. That uses a fresh
local conversation rather than the Mini's full transcript.

A complete handback fix should negotiate destination readiness before stopping
an agent. Shared mission payloads deliberately omit local repository paths and
worktree readiness, so checking the secondary's own mirror is insufficient.
The handback protocol needs an explicit readiness signal or primary-driven
acceptance. Regression coverage should include missing repositories and
incomplete/failed mirrors at the destination.

Live task state was not modified during this investigation.
