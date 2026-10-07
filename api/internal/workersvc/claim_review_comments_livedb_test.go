package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// seedClaimedMRRework seeds a completed source issue run and a claimed mr_rework run reworking
// its MR (runs_kind_shape: pipeline_ref, mr_iid and target_run_id set, issue_iid NULL), so the
// review-snapshot claim tests exercise the real rework shape. It returns the rework and source ids.
func seedClaimedMRRework(t *testing.T, env codexTestEnv, userID, workerID, repoID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	sourceID := env.seedCodexRun(t, userID, workerID, repoID)
	env.exec(`UPDATE runs SET issue_iid = 2, status = 'completed' WHERE id = $1`, sourceID)
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, worker_id,
	             pipeline_ref, mr_iid, target_run_id)
	          VALUES ($1, $2, $3, 'mr_rework', 't', 'd', 'claimed', $4, 'agent/issue-2', 77, $5)`,
		runID, userID, repoID, workerID, sourceID)
	if got := mustRun(t, env, runID).Kind; got != "mr_rework" {
		t.Fatalf("seeded run kind = %q, want mr_rework", got)
	}
	return runID, sourceID
}

// TestAssembleClaimReplaysOnlyAssessedReviewSnapshotLiveDB drives the REAL assembleClaim against a
// REAL Postgres (issue #2347). A runs.review_comments snapshot written before comment authors were
// assessed (no version, or any version but the current one) must never replay its bodies: the claim
// carries an empty version-0 snapshot so the agent can say a legacy snapshot was withheld. A current
// snapshot replays as stored, withheld counts included.
func TestAssembleClaimReplaysOnlyAssessedReviewSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := seedResumeClaimInfra(t, env)
	runID, _ := seedClaimedMRRework(t, env, userID, workerID, repoID)
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

	// A stored "comments":null (an all-withheld snapshot from an older build) replays as an array.
	nullComments := assemble(`{"version":2,"comments":null,"truncated":false,"withheld_not_eligible":2}`)
	if raw, err := json.Marshal(nullComments); err != nil || !strings.Contains(string(raw), `"comments":[]`) {
		t.Fatalf("a stored null comments array replayed as %s (%v), want \"comments\":[]", raw, err)
	}

	// A current snapshot keeps its reusable context: the resume payload carries both.
	env.exec(`UPDATE runs SET plan_md = 'CURRENT-PLAN', session_id = 'sess-current', plan_source = 'agent' WHERE id = $1`, runID)
	cur, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
	if err != nil || cur.PlanMd == nil || *cur.PlanMd != "CURRENT-PLAN" || cur.SessionID == nil || *cur.SessionID != "sess-current" {
		t.Fatalf("a current snapshot run must resume with its plan and session: %+v, %v", cur, err)
	}
	env.exec(`UPDATE runs SET plan_md = NULL, session_id = NULL WHERE id = $1`, runID)

	env.exec(`UPDATE runs SET review_comments = NULL WHERE id = $1`, runID)
	payload, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
	if err != nil || payload.ReviewComments != nil {
		t.Fatalf("a run with no snapshot must claim none: %+v, %v", payload.ReviewComments, err)
	}
}

// TestLegacyReviewSnapshotWithReusableContextFailsClaimLiveDB drives the REAL
// assembleAndFinishRunClaim (issue #2347): a legacy, never-assessed review snapshot on a run that
// already carries a stored plan or a session must not resume (an auto-approved resume would reuse
// a transcript or plan written after reading the unvetted comment bodies). The claim returns no
// payload and the run fails terminally as guardrail_blocked with the rework-again guidance.
func TestLegacyReviewSnapshotWithReusableContextFailsClaimLiveDB(t *testing.T) {
	const legacy = `{"comments":[{"id":7,"author_username":"mallory","author_forge_user_id":9,"body":"LEGACY-UNVETTED-BODY","reply_id":"r1","resolve_id":"t1","review_state":"inline"}],"truncated":false}`
	for _, tc := range []struct{ name, set string }{
		{"plan only", `plan_md = 'LEGACY-PLAN-SENTINEL', session_id = NULL`},
		{"session only", `plan_md = NULL, session_id = 'legacy-session'`},
		{"both", `plan_md = 'LEGACY-PLAN-SENTINEL', session_id = 'legacy-session'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := seedResumeClaimInfra(t, env)
			runID, sourceID := seedClaimedMRRework(t, env, userID, workerID, repoID)
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			env.exec(`UPDATE runs SET review_comments = $2::jsonb, auto_approve = true, plan_source = 'agent', `+
				tc.set+` WHERE id = $1`, runID, legacy)

			payload, err := svc.assembleAndFinishRunClaim(env.ctx, store.Worker{ID: workerID, UserID: userID}, mustRun(t, env, runID), false)
			if err != nil || payload != nil {
				t.Fatalf("claim = %+v, %v; want no payload and no error (the run fails terminally)", payload, err)
			}
			r := mustRun(t, env, runID)
			if r.Status != "failed" || r.FailOrigin.String != "guardrail_blocked" {
				t.Fatalf("status=%s origin=%v, want failed/guardrail_blocked", r.Status, r.FailOrigin)
			}
			reason := r.FailureReason.String
			if !strings.Contains(reason, "cannot resume; start a new rework with Rework now") ||
				!strings.Contains(reason, sourceID.String()) || strings.Contains(reason, "default-branch") {
				t.Fatalf("failure_reason = %q, want the legacy-rework text naming run %s", reason, sourceID)
			}
		})
	}
}
