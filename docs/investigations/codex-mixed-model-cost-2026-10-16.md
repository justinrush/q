# Codex mixed-model card costs

Investigated at q 328367c. The reported blank card has not yet been identified;
the evidence below does not establish the cause of that particular incident.

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
