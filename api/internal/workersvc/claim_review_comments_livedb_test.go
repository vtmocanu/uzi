package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAssembleClaimReplaysOnlyAssessedReviewSnapshotLiveDB drives the REAL assembleClaim against a
// REAL Postgres (issue #2347). A runs.review_comments snapshot written before comment authors were
// assessed (no version, or any version but the current one) must never replay its bodies: the claim
// carries an empty version-0 snapshot so the agent can say a legacy snapshot was withheld. A current
// snapshot replays as stored, withheld counts included.
func TestAssembleClaimReplaysOnlyAssessedReviewSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := seedResumeClaimInfra(t, env)
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	svc := New(env.q, env.box, testParams())
	wkr := store.Worker{ID: workerID, UserID: userID}

	assemble := func(stored string) *ReviewCommentsSnapshot {
		t.Helper()
		env.exec(`UPDATE runs SET review_comments = $2::jsonb WHERE id = $1`, runID, stored)
		payload, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
		if err != nil {
			t.Fatalf("assembleClaim: %v", err)
		}
		return payload.ReviewComments
	}

	const body = `{"id":7,"author_username":"mallory","author_forge_user_id":9,"body":"LEGACY-UNVETTED-BODY","reply_id":"r1","resolve_id":"t1","review_state":"inline"}`
	for name, stored := range map[string]string{
		"no version key":  `{"comments":[` + body + `],"truncated":false}`,
		"version zero":    `{"version":0,"comments":[` + body + `],"truncated":false}`,
		"another version": `{"version":1,"comments":[` + body + `],"truncated":false}`,
	} {
		got := assemble(stored)
		if got == nil || got.Version != 0 || len(got.Comments) != 0 || got.WithheldNotEligible != 0 {
			t.Fatalf("%s: replayed %+v, want an empty version-0 snapshot", name, got)
		}
	}

	current := assemble(`{"version":2,"comments":[` + body + `],"truncated":true,"withheld_not_eligible":3,"withheld_unknown":1}`)
	if current == nil || current.Version != ReviewSnapshotVersion || len(current.Comments) != 1 || current.Comments[0].ID != 7 ||
		!current.Truncated || current.WithheldNotEligible != 3 || current.WithheldUnknown != 1 {
		t.Fatalf("a current snapshot did not replay as stored: %+v", current)
	}

	env.exec(`UPDATE runs SET review_comments = NULL WHERE id = $1`, runID)
	payload, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
	if err != nil || payload.ReviewComments != nil {
		t.Fatalf("a run with no snapshot must claim none: %+v, %v", payload.ReviewComments, err)
	}
}
