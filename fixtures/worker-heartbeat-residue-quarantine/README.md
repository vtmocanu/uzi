# worker-heartbeat-residue-quarantine fixtures (issue #2213)

The heartbeat extension a single-uid worker sends while its residue quarantine is latched: an
unreadable, unattributed runner-uid process was found, so the worker claims nothing, starts no
forge-credentialed git and no new provider turn until its container restarts.

- `latched.json`: the top-level `residue_quarantine` member of the heartbeat body
  (`POST /api/worker/heartbeat`). The agent sends it ONLY while latched and ONLY when the api
  advertised the `worker_residue_quarantine` protocol feature; it is stripped on the strict-decode
  fallback like `outbox`. Absent means "not latched".
  - `cause`: the agent-sanitized detail (control and bidi characters replaced, at most 160
    characters). Untrusted: the api sanitizes it again and every surface renders it as plain text.
  - `latched_at`: RFC 3339 (the agent's `Date.toISOString()`).
  - `run_id`: the run whose check detected the process, or `null` when none applies.
  - `site`: the detection site (`pre_clone`, a quiescence site name, or `review_pre_fetch`).

Two tests read it, one per side: the agent test asserts the body its heartbeat builds for a latched
worker carries exactly this member's shape, and the api test feeds the whole object through the
real heartbeat decoder and asserts the overlaid DTO fields.
