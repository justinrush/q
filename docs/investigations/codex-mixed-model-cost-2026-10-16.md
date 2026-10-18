# Codex mixed-model card costs

Initially investigated at q 328367c without an affected card. The October 18
follow-up below identifies the missing measurement on a specific mission.

## Verified behavior

- The Codex meter attributes each response to its preceding `turn_context`
  model. A planning/execution split is already supported, including replayed
  response IDs and cached scans. Astra and Sol both have built-in prices.
- `commitRelaunch` preserves the stored transcript path and usage. Resuming the
  same rollout lets the next scan include both phases without adding the old
  measurement twice.
- The local saved `retinue-ui` card used `gpt-6-astra` for planning and
  `gpt-6.1-sol` for execution. Its stored breakdown contains both models and an
  estimated total of $14.3977276. This proves the split can work, but does not
  prove what a different affected card displayed.
- The card omits cost only when stored usage is empty. Entirely unpriced usage
  displays a token count; partially priced usage displays a dollar floor.
- Inspected local CLI 0.160.0 rollout metadata and record types without copying
  conversation content. Those rollouts contain the `token_usage_record` format
  the meter expects. No affected mixed-model rollout was available locally.

Added `TestCodexMeterAcrossPlanModelRelaunch` in `internal/daemon/meter_test.go`.
It feeds real rollout records through hook ingestion, the real Codex meter,
plan approval bookkeeping, and `commitRelaunch`, then checks that timer scans
retain both model costs after a resume hook omits the transcript path. This
passes without a production change. It does not run a real Codex terminal or
exercise clear-context thread replacement.

## Failure paths to check on an affected mission

1. **Missing transcript path.** Only accepted hooks populate `TranscriptPath`.
   Runtime session recovery can restore `AgentSessionID` but never restores the
   rollout path. If path-bearing hooks were missed, null, stale by launch epoch,
   or rejected because of a session mismatch, runtime status can work while
   metering returns no measurement. This is a code-level recovery gap, not a
   confirmed explanation for the report. The path is host-local: its absence
   on a peer's replica is expected and says nothing about the owner's meter.
2. **Unreadable or incompatible rollout.** A nonexistent path or a transcript
   without recognized nonzero per-response records gives no measurement.
   Other file errors are logged as `metering a mission`. Scanner errors are
   currently ignored, so an oversized line may also stop a scan silently.
3. **Thread replacement.** Clear-context approval can create a new thread.
   Check the saved session ID/path against the new thread, accepted hook
   epochs, and usage on the host holding the mission lease. Recovering a
   replacement session in the runtime does not itself update its transcript
   path. Replacing the path also means the meter only sees the new rollout;
   it does not accumulate earlier rollouts on the same host.
4. **Deployment or mirroring.** Compare the owner's stored usage with the
   displayed host's replica and check the running daemon version. A new CLI
   binary does not replace an already running daemon.

The next useful evidence is a specific affected mission ID and machine, its
owner's saved session/path/usage fields, rollout record types and model names,
and matching daemon warnings. No live mission state, agent sessions, installed
binary, or daemon configuration was changed during this investigation.

## Validation

- `go test -race ./internal/usage ./internal/mission ./internal/codex ./internal/daemon`
- `go vet ./internal/daemon ./internal/usage`
- `git diff --check`

All passed. The protocol tests required local test sockets outside the sandbox;
the Go build cache was placed in a temporary directory.

## October 18 follow-up: courier troubleshoot

The user identified the active `courier troubleshoot` card, mission
`ms_3bf886dfca0b`, on Justins-MacBook-Air. Read-only inspection found:

- Stored session ID `01a14fb0-bf3f-73d0-b2a3-836934e1b100`, hook epoch 2,
  and no transcript path or usage. The card also carried `hooks-silent`.
- Its local Codex rollout exists and contains 12 `token_usage_record` entries,
  one `turn_context` naming `gpt-6-astra`, and no oversized lines. No
  conversation text was needed for this inspection.
- The mission has not yet approved its plan. It is configured to implement on
  `gpt-6.1-sol/low` after planning on `gpt-6-astra/medium`.

This confirms the missing-path failure described above. It happens before
implementation starts; the model switch itself is not needed to trigger it.
The evidence does not explain why the path-bearing hooks were missed.

The runtime already recovers session IDs through `thread/read` or `thread/list`,
but discarded the thread's `path`. Both now carry that metadata through the
runtime reading into the saved mission, behind the existing session and launch
epoch guards. The installed Codex JSON schema confirms `Thread.path` is a
nullable string. Missing optional paths preserve an already known path.
The regular reconciler can then meter the rollout without another hook.
No session-directory guessing or live-state repair is needed.

Cards also grow from six to eight terminal rows, giving planning and
implementation models (including effort) separate labeled lines. Tool, host,
and cost remain ahead of other run metadata. Layout and scrolling use the new
height.

Regression coverage exercises protocol path decoding, runtime discovery and
known-session recovery, stale-session/epoch guards, recovered-path metering,
repeated measurements, and card cost/model visibility and height. The earlier
mixed-model relaunch regression remains in place.

The live daemon, installed binary, and mission state were not changed. Installing
the updated q and restarting its daemon is needed for runtime recovery to run.

Follow-up validation:

- `go test ./internal/codex ./internal/daemon ./internal/tui ./internal/usage ./internal/mission`
- `go test -race ./internal/codex ./internal/daemon ./internal/tui ./internal/usage ./internal/mission`
- `go vet ./...`
- `golangci-lint run --new-from-rev=328367c ./...` (no new issues)
- `gofmt -l .` and `git diff --check`

Go and lint caches were placed in temporary directories. Protocol, daemon, and
TUI tests used local sockets outside the sandbox.

Full `golangci-lint run ./...` reports 16 existing issues on unchanged code
(including function length/complexity, unchecked Close, and existing constants).
The check against the mission base reports zero new issues.
