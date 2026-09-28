# worker-state-request fixtures (PRD #1795 M3)

The exact `awaiting_approval` body the worker POSTs to `/api/worker/runs/{id}/state`:

- `awaiting_approval.json`: the api did NOT advertise the `gate_revision_v1` register feature, so
  the report carries no gate presentation field.
- `awaiting_approval.gate_revision_v1.json`: the api advertised it, so the report carries a minted
  `presentation_id` (normalized to a fixed UUID; the worker mints a random one per gate).

Two tests read them, one per side, and neither reads the other:

- `agent/test/runner-gate-revision-reclaim.test.ts` captures the body the REAL runner sends in each
  case and asserts it equals the fixture, so a worker wire change reddens the agent gate.
- `api/internal/handler/worker_state_decode_test.go` feeds both through the api's real strict
  decoder (`httpx.DecodeJSON`, `DisallowUnknownFields`): the first decodes into the pre-#1795
  `StateRequest` field set (an older api) and the second must fail it with
  `unknown field "presentation_id"`; both decode into the current `StateRequest`.

The fixtures are RECORDED, not authored: on a mismatch the agent test prints the full body, and a
deliberate wire change is a copy of it. `fixtures/` sits above `api/`, so run the Go package with
`-count=1` after a fixture-only edit.
