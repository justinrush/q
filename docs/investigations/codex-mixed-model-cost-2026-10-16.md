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

## October 19 follow-up: q multi line model

Rebased this mission onto main at `e87b49a`, which includes the earlier runtime
path recovery and eight-row card changes. Installed q was still v0.3.12 at
`0f95d09`, explaining why those changes were absent locally.

Read-only inspection of mission `ms_2afd9566ac47` found no saved transcript path
or usage, despite a local Codex rollout containing completed response records.
Both configured phases use `gpt-6-astra/low`, so this mission cannot itself prove
a mixed-model transition. An isolated snapshot of its rollout contained 35
responses: 2,199,635 input tokens, including 2,065,408 cached input tokens, and
13,790 output tokens. Independent arithmetic and the actual meter both returned
$4.097178. An isolated render showed `$4.10` plus separate `plan:` and
`implement:` rows. The temporary test and copied conversation are not committed.
The installed binary, live daemon, and live mission state were not modified.

The remaining thread-replacement gap is addressed with local checkpoints keyed
by rollout path and response ID. Accepted hooks/runtime readings retain both old
and new paths. Measured response counts survive state reloads and missing older
files; totals deduplicate response IDs across paths. Rollout session metadata
identifies the owning thread, excluding parent records replayed before a fork's
own usage. Legacy files without session metadata retain the first-thread fallback.
Checkpoint data never syncs to peers; aggregate usage still does.

Missing model context in a split-model rollout remains unpriced rather than
being charged at the implementation model's rate. For legacy single-model
rollouts, the existing configured-model fallback is retained and checkpointed.
Pricing sums models in a stable order to avoid floating-point changes triggering
repeated state writes. In-flight scans cannot overwrite a replacement session's
checkpoint or a newer measurement.

Astra and GPT-6.1 Sol standard short-context rates were rechecked against
https://developers.openai.com/api/docs/pricing on October 19: input/cached-input/
output remain $10/$1/$50 and $2/$0.10/$10 per million respectively. Existing
service-tier, long-context, regional, and tool-surcharge exclusions still apply.

Regression cases cover same-thread switches, fresh-thread approval, inherited
parent history, copied rollouts, replacement before the first metering tick,
missing measured rollouts, JSON/store reloads with a new meter, ambiguous model
attribution, local-only checkpoint cloning, unchanged-state polling, and stale
scan rejection. Data already lost before q discovered or measured a rollout
cannot be reconstructed by a checkpoint.

Validation for this follow-up:

- `go test -race ./...`
- `go vet ./...`
- `go build -o /tmp/q-multi-line-model-check ./cmd/q`
- `golangci-lint run --new-from-rev=origin/main ./...` (zero issues)
- `gofmt -l .` and `git diff --check`

Full lint reports 15 pre-existing findings. The touched rollout parser's
complexity warning was removed by extracting response recording. Socket-based
tests ran outside the filesystem sandbox; Go/lint caches used temporary paths.
