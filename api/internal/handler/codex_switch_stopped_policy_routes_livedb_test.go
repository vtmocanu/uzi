package handler

import (
	"context"
	"testing"
)

func TestWorkerCodexStoppedPolicyRouteLiveDB(t *testing.T) {
	for _, status := range []string{"running", "paused"} {
		for _, fence := range []string{"policy_pending_fresh", "unresolved_policy_outcome"} {
			t.Run(status+"/"+fence, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}

				fresh := fence == "policy_pending_fresh"
				// Keep the outcome resolved for Fresh, and Fresh absent for the
				// unresolved outcome, so each case isolates one semantic fence.
				// The nullable identities from migrations 00419 and 00420 stay
				// at their defaults; no hold, stop, or pause marker is changed.
				stoppedRouteExec(t, e, `UPDATE runs SET
 policy_preserve_required=true,
 policy_continuation_mode='same_conversation',
 policy_outcome_id=gen_random_uuid(),
 policy_applying_worker_id=$2,
 policy_source_generation=$3,
 policy_applying_generation=$3,
 policy_pending_fresh=$4,
 policy_resolved_at=CASE WHEN $4::boolean THEN now() ELSE NULL END
 WHERE id=$1`, e.run, e.worker, e.gen, fresh)

				// Read the stored tuple rather than inferring it from UPDATE
				// success or reusing the production policy predicate.
				var isolated bool
				err := e.pool.QueryRow(context.Background(), `SELECT
 status=$5 AND hold_reason IS NULL
 AND policy_preserve_required
 AND policy_continuation_mode='same_conversation'
 AND policy_outcome_id IS NOT NULL
 AND policy_outcome_id <> '00000000-0000-0000-0000-000000000000'::uuid
 AND policy_applying_worker_id=$2
 AND policy_source_generation=$3 AND policy_applying_generation=$3
 AND policy_pending_fresh=$4
 AND (policy_resolved_at IS NOT NULL)=$4
 AND policy_fresh_consumed_generation IS NULL
 AND policy_fresh_confirmed_generation IS NULL
 AND policy_required_head IS NULL
 AND policy_recovery_verified_generation IS NULL
 AND policy_recovery_verified_head IS NULL
 AND policy_hold_cause IS NULL
 FROM runs WHERE id=$1`, e.run, e.worker, e.gen, fresh, status).Scan(&isolated)
				if err != nil {
					t.Fatal(err)
				}
				if !isolated {
					t.Fatal("stored policy tuple does not isolate the intended semantic fence")
				}

				// This helper requires HTTP 409 and equality of the complete
				// run, credential epochs, messages, inputs, and custody snapshot.
				reportStoppedRoute(t, e, w, false, false)
			})
		}
	}
}
