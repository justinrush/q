# Codex planning settings and model handoff

The `q codex plan` mission requested `gpt-6-astra/low` for planning and
`gpt-6-sol/low` for implementation. Its initial generated argv selected Astra and
`model_reasoning_effort=low`; its first recorded turn instead used native Plan
Mode with Astra/**medium**. The missing setting was
`plan_mode_reasoning_effort`: Codex's planning preset has its own default.

After native approval, the rollout recorded a default-mode Astra/low turn.
The generated restart argv selected Sol/low, but a terminal restart alone is
not evidence that a managed-server turn stopped or changed models. The sanitized
records are in `internal/codex/testdata/plan-settings.jsonl`; they omit prompts,
paths, account data, and real session identifiers.

The old daemon recorded `PlanApproved` before attempting a restart. A failed
automatic restart only logged a warning, and later runtime polls stopped reading
plan state once approval was recorded. This could strand a mission on the
planning model indefinitely. Reading only the newest turn also missed earlier
approval prompts after another turn had begun.

The fix explicitly configures planning effort, incrementally reads execution
settings and approval evidence from complete rollout records, and persists a
handoff independently of approval. Native approval and q approval use the same
serialized transition. Server-side interruption precedes terminal replacement;
implementation mode/model/effort are set explicitly through the managed server.
A fresh matching turn verifies the handoff. A mismatch, timeout, or failed
control operation pauses the mission with a retry action. Missing capabilities
cannot silently become an ordinary unobserved session.

`turn/start` is not retried at the transport layer: a response can be lost after
the server accepted the request. The durable verification boundary allows q to
observe an accepted turn after restarting without submitting it again.

The opt-in managed-server smoke test verified three completed turns on Codex
0.163.0: native Plan Mode with Astra/low, explicit implementation with Sol/low,
and a subsequent turn without overrides retaining Sol/low. The test waits for
rollout completion events because turn history can transiently report an active
turn as interrupted. Interruption uses live thread status and falls back to the
rollout's active turn ID when history lags.

References: [configuration](https://learn.chatgpt.com/docs/config-file/config-reference),
[App Server](https://learn.chatgpt.com/docs/app-server).
