package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestWorkerCodexStoppedHoldRouteLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold string
	}{
		{"policy_hold", "codex_policy_hold"},
		{"ordinary_hold", "completion_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, w := setupStoppedGuardsRoute(t)
			withdrawStoppedRoute(t, e, w)
			reportStoppedRoute(t, e, w, true, true)

			outcome := uuid.New()
			stoppedRouteExec(t, e, `UPDATE runs SET hold_reason=$2,
 policy_preserve_required=true,policy_continuation_mode='same_conversation',
 policy_outcome_id=$3,policy_applying_worker_id=$4,
 policy_source_generation=$5,policy_applying_generation=$5,
 policy_resolved_at=now(),policy_pending_fresh=false WHERE id=$1`,
				e.run, tc.hold, outcome, e.worker, e.gen)

			// Read the complete tuple: only the hold term should block this replay,
			// not a pending fresh start or an unresolved outcome.
			var isolated bool
			if err := e.pool.QueryRow(context.Background(), `SELECT
 status='paused' AND hold_reason=$2 AND policy_preserve_required
 AND policy_continuation_mode='same_conversation' AND policy_outcome_id=$3
 AND policy_outcome_id <> '00000000-0000-0000-0000-000000000000'::uuid
 AND policy_applying_worker_id=$4 AND policy_source_generation=$5
 AND policy_applying_generation=$5 AND policy_resolved_at IS NOT NULL
 AND NOT policy_pending_fresh
 FROM runs WHERE id=$1`, e.run, tc.hold, outcome, e.worker, e.gen).Scan(&isolated); err != nil {
				t.Fatal(err)
			}
			if !isolated {
				t.Fatal("stored resolved policy tuple does not isolate the hold term")
			}
			reportStoppedRoute(t, e, w, false, false)
		})
	}
}
