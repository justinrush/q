# Codex status and directory trust

Investigated against Codex CLI and managed app-server 0.162.1, starting from q
16e38b2. A mission's saved card said it was waiting at its original planning
prompt, while the terminal was implementing work.

## Confirmed causes

- `codex app-server proxy` forwards raw Unix socket bytes. That endpoint uses
  WebSocket with an HTTP Upgrade handshake, not the JSON-lines protocol exposed
  by standalone `codex app-server`. JSON-line initialization timed out; the same
  proxy immediately answered WebSocket requests and reported the mission active.
- Runtime read failures were silently ignored. The startup-prompt fallback
  stopped checking once it changed the agent from unknown to waiting.
- Plan recognition depended on hooks and an empty closing message. Busy runtime
  readings could override a pending plan; an unloaded recorded session prevented
  discovery of a replacement after relaunch.
- The remote terminal's directory trust gate reads the server's base config.
  An isolated home with a trusted mission in `q.config.toml` still prompted with
  `--remote ... --profile q`. Adding only that directory's trust entry through
  `config/value/write` removed the prompt without a daemon restart.

## Changes

The observer upgrades the proxy to WebSocket, retaining JSON lines for standalone
model-catalog queries. A real WebSocket regression test covers the handshake and
frames without trailing newlines. The initialization cancellation test now waits
for the fake process to start before cancelling it.

An unavailable status source produces a `status-unavailable` badge and one log
entry per outage. Successful observations clear the badge. Startup waits are
rechecked and cleared when the screen no longer supports them; activity stays
unknown until a reliable source reports it.

Planning missions query the newest structured turn for a completed plan item or
Codex's explicit implementation prompt. Ordinary replies such as "yes" are not
assumed to approve a plan. Missing optional history support leaves hook-based
plan information intact. Session discovery skips subagents and paginates; a
replacement ID is accepted only when the previous thread was unloaded and the
recorded session and launch epoch still match. Plan approval uses the existing
model-switch/relaunch path.

Managed launches run `q codex-prepare` before the terminal. This asks Codex to
persist only `projects.<canonical-mission-directory>.trust_level = "trusted"`
in its base config. It does not read or rewrite that config itself, change
sandbox/approval settings, or bypass hook review. Trust entries remain after
mission closure. Preparation failure stops the launch visibly.

## Validation and deployment

Run the commands in TESTING.md, particularly `go test -race -count=1
-timeout=5m ./...`. This repository has no `make test` target.

The repaired observer was checked against the existing daemon using read-only
thread status queries. Directory trust was reproduced and checked using separate
temporary Codex homes, a dummy local provider, and isolated daemons; no real
provider requests or live configuration changes were needed.

Apply the exported mbox patch with `git am`, rebuild/install q, and restart the
q daemon. Existing launch scripts gain the trust-preparation step when q next
regenerates them during launch/relaunch. Do not restart the Codex daemon merely
to install this patch; that would interrupt unrelated sessions.

Check a newly launched planning mission, a user-input prompt, plan acceptance
with different planning/implementation models, and relaunch on the destination
machine. The wire integration was validated on 0.162.1; future experimental
app-server protocol changes should be checked against that CLI's generated
schemas. End-to-end model switching with paid inference was not exercised.
