# worker-register-snapshot fixtures (issue #1742)

The exact `active_snapshot` a restarted worker sends on `POST /api/worker/register` when its
outbox holds an authenticated finalize-pending record (the executor finished at that claim
generation, but no terminal outcome was journaled before the process died):

- `finalize-resume.json`: no pending terminal journals (`active: []`, `pending_overflow: false`)
  and one `finalize_resume` entry naming the run and the exact claim generation.

Two tests read `finalize-resume.json`, one per side, and neither reads the other:

- the agent side builds the register snapshot through the real `Worker`/`ActiveRunRegistry`
  after an outbox restart and asserts it equals this fixture (run id normalized);
- `api/internal/handler/worker_register_snapshot_fixture_test.go` feeds it through the api's
  real lenient `parseActiveSnapshot` and asserts the `FinalizeResume` entry decodes.

`finalize_resume` rides INSIDE `active_snapshot` because the register body itself is decoded
strictly; an older api ignores the inner field. `fixtures/` sits above `api/`, so run the Go
package with `-count=1` after a fixture-only edit.

## Scoped claim (R3)

`scoped-claim.json` is a complete POST body for `/api/worker/runs/claim`, rather
than a boot/register snapshot. It retains three historical source generations
(1, 3, 7) for one protected run, whose independently observed current owner is
generation 9, plus an ordinary pending run at generation 2. With pending cap 2,
complete coverage counts the protected run as one group and the ordinary run as
one slot. Unknown physical source evidence still sets `pending_overflow: true`;
it grants no active lease at either a historical generation or owner generation 9.
The unrelated queued candidate may be claimed while the protected source remains
excluded for both its owner and another worker.

- `agent/test/scoped-claim-fixture.test.ts` creates real outbox journals, observes
  malformed historical files, completes a bounded inventory scan, records owner 9
  independently, and projects through `ActiveRunRegistry.build(true)`. The real
  `WorkerClient.claimRun` serializer must produce the exact compact fixture bytes,
  excluding only the file's trailing newline. Sorting the real source inventory
  by run/generation makes the fixture order deterministic.
- `api/internal/handler/worker_scoped_claim_fixture_livedb_test.go` sends the file
  bytes unchanged (including the newline) through Bearer authentication and
  `WorkerRoutes`, the real Claim transaction, SQL coverage storage and credential
  assembly. It asserts a 200 for the unrelated candidate, conservative closure,
  an ordinary identity row and no invented historical rows or leases, unchanged
  owner generation 9 and protected-source exclusion, including after capability loss.
  Historical grouped sources legitimately upsert zero rows: their complete wire
  coverage and validated current ownership account for them, while conservative
  worker overflow and unchanged candidate SQL preserve their protection.

The API test genuinely registers and checks capability negotiation first. It then
uses test SQL to seed the fixed run UUIDs, current owner/state and fixture nonce
with epoch 0, so the exact file can be posted without rewriting its nonce or IDs.
This is deterministic test setup, not a change to production nonce generation or
the nonce, epoch and ownership fences. Fixed runs are deleted at test cleanup.

`pending_outcome_scoped_claim_v1` is negotiated independently of provider policy
hold readiness; `provider_policy_hold_v1` is not advertised. The agent also
re-registers without the scoped feature and verifies the real serializer omits
`pending_policy_protection` while retaining overflow and source identities, and
the registry pauses claims. Grouping is protection evidence only: it grants no
custody, conversion, journal authentication, retirement or lifecycle authority.
Register/heartbeat evidence alone does not enable a claim exception; that remains
covered by the existing scoped-claim route/service tests. The API feature-loss
case registers a conservative boot snapshot before checking exclusion again.
