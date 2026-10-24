# completion-wire fixtures (PRD #1232 M2)

The completion-interlock responses the api returns on the worker permit/attempt wire, for a structural
run frozen with five milestones `m1`..`m5` (one criterion `mN.c1` each):

- `attempt-unmet.json`: `POST /api/worker/runs/{id}/completion/attempt` with `m1`..`m4` declared; the
  server-authoritative unmet set is `["m5"]`.
- `permit-denied.json`: `POST /api/worker/runs/{id}/completion/permit` at contract revision 1 with
  `m1`..`m4` complete; a non-terminal denial, `deny_reason` `missing_milestones`, `unmet` `["m5"]`.
- `permit-granted.json`: the same request with all five complete; the granted permit (id, contract
  revision, branch, head, issue time, finding ids, `completion_scope`).

Two tests read them, one per side, and neither reads the other:

- `api/internal/handler/completion_wire_fixture_test.go` (`TestCompletionWireFixtures`) drives the real
  handlers over a stubbed store and asserts the normalized response bytes equal each file.
- `agent/test/client-completion-wire.test.ts` parses the same files with the
  worker client, so an api wire change reddens the agent gate and vice versa.

Normalization (the only edits to the handler output): every UUID becomes
`00000000-0000-4000-8000-000000000000` and every RFC 3339 timestamp becomes `2026-01-01T00:00:00Z`.
Nothing else is rewritten. Files are pretty-printed with a two-space indent and a trailing newline;
key order is the handler's own.

The fixtures are RECORDED, never hand-authored. To re-record after a deliberate wire change, from `api/`:

    UZI_RECORD_COMPLETION_WIRE=1 go test ./internal/handler -run TestCompletionWireFixtures -count=1

then run the same command without the variable to confirm equality. `fixtures/` sits above `api/`, so
use `-count=1` after a fixture-only edit or Go will serve a cached pass.
