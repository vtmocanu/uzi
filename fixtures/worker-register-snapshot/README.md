# worker-register-snapshot fixtures (issue #1742)

The exact `active_snapshot` a restarted worker sends on `POST /api/worker/register` when its
outbox holds an authenticated finalize-pending record (the executor finished at that claim
generation, but no terminal outcome was journaled before the process died):

- `finalize-resume.json`: no pending terminal journals (`active: []`, `pending_overflow: false`)
  and one `finalize_resume` entry naming the run and the exact claim generation.

Two tests read it, one per side, and neither reads the other:

- the agent side builds the register snapshot through the real `Worker`/`ActiveRunRegistry`
  after an outbox restart and asserts it equals this fixture (run id normalized);
- `api/internal/handler/worker_register_snapshot_fixture_test.go` feeds it through the api's
  real lenient `parseActiveSnapshot` and asserts the `FinalizeResume` entry decodes.

`finalize_resume` rides INSIDE `active_snapshot` because the register body itself is decoded
strictly; an older api ignores the inner field. `fixtures/` sits above `api/`, so run the Go
package with `-count=1` after a fixture-only edit.
