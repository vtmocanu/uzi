package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func sampleReviewSnapshot() *ReviewCommentsSnapshot {
	return &ReviewCommentsSnapshot{
		Comments: []ReviewCommentSnapshot{
			{ID: 120, AuthorUsername: "human", Body: "tighten the loop guard", ReviewState: "inline"},
		},
	}
}

func TestCreateAutoMRReworkRunSetsFields(t *testing.T) {
	user, repo, source := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{repoRow: aValidRepoRow(), mrReworkRunResult: store.Run{ID: uuid.New(), Kind: RunKindMRRework}}
	svc := New(fs, newBox(t), testParams())

	run, err := svc.CreateAutoMRReworkRun(context.Background(), user, repo, "agent/issue-7", 55, source, "Rework MR review", "desc", sampleReviewSnapshot())
	if err != nil {
		t.Fatalf("CreateAutoMRReworkRun: %v", err)
	}
	if run.Kind != RunKindMRRework {
		t.Fatalf("expected an mr_rework run, got kind %q", run.Kind)
	}
	if fs.mrReworkRunParams == nil {
		t.Fatal("CreateAutoMRReworkRun store call not made")
	}
	p := fs.mrReworkRunParams
	// pipeline_ref = agent/issue-N written AT INSERT (Decision 6 — the cross-kind guard key).
	if p.PipelineRef.String != "agent/issue-7" || !p.PipelineRef.Valid {
		t.Fatalf("pipeline_ref not written at insert: %+v", p.PipelineRef)
	}
	if p.MrIid.Int64 != 55 || !p.MrIid.Valid {
		t.Fatalf("mr_iid not carried: %+v", p.MrIid)
	}
	// target_run_id = the source completed run whose MR is watched.
	if !p.TargetRunID.Valid || uuid.UUID(p.TargetRunID.Bytes) != source {
		t.Fatalf("target_run_id not carried: %+v", p.TargetRunID)
	}
	var snap ReviewCommentsSnapshot
	if err := json.Unmarshal(p.ReviewComments, &snap); err != nil {
		t.Fatalf("review_comments is not valid jsonb: %v", err)
	}
	if len(snap.Comments) != 1 || snap.Comments[0].ID != 120 {
		t.Fatalf("review snapshot did not round-trip onto the run: %+v", snap)
	}
}

func TestCreateAutoMRReworkRunNilSnapshotStoresNull(t *testing.T) {
	fs := &fakeStore{repoRow: aValidRepoRow(), mrReworkRunResult: store.Run{ID: uuid.New(), Kind: RunKindMRRework}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", 9, uuid.New(), "t", "d", nil); err != nil {
		t.Fatalf("CreateAutoMRReworkRun: %v", err)
	}
	if fs.mrReworkRunParams == nil || fs.mrReworkRunParams.ReviewComments != nil {
		t.Fatalf("a nil snapshot must store NULL review_comments, got %v", fs.mrReworkRunParams.ReviewComments)
	}
}

func TestCreateAutoMRReworkRunRefusesBranchInUse(t *testing.T) {
	// The create-time CROSS-KIND branch guard is now the create query's own atomic
	// INSERT … WHERE NOT EXISTS. When a committed active ci_fix OR mr_rework sibling
	// occupies the ref's pipeline_ref, the insert matches zero rows and the generated
	// :one returns pgx.ErrNoRows, which the service maps to ErrBranchInUse (the detector
	// swallows it). The insert now RUNS (the old "block before the insert" no longer holds).
	fs := &fakeStore{repoRow: aValidRepoRow(), mrReworkRunErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", 9, uuid.New(), "t", "d", sampleReviewSnapshot()); err != ErrBranchInUse {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
}

func TestCreateAutoMRReworkRunBranchRefConflictIsBranchInUse(t *testing.T) {
	// Concurrent-window race: the atomic INSERT's snapshot could not see a racing sibling,
	// so it slipped past WHERE NOT EXISTS and the durable spanning index arbitrated —
	// raising 23505 on uq_runs_one_active_branch_ref. That maps to ErrBranchInUse, NOT
	// ErrActiveMRReworkExists (the pre-fix generic mapping got this wrong).
	fs := &fakeStore{repoRow: aValidRepoRow(), mrReworkRunErr: &pgconn.PgError{Code: "23505", ConstraintName: "uq_runs_one_active_branch_ref"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", 9, uuid.New(), "t", "d", sampleReviewSnapshot()); err != ErrBranchInUse {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
}

func TestCreateAutoMRReworkRunMapsDuplicate(t *testing.T) {
	// A 23505 on uq_runs_one_active_mr_rework — a second active rework on the same MR —
	// maps to ErrActiveMRReworkExists.
	fs := &fakeStore{repoRow: aValidRepoRow(), mrReworkRunErr: &pgconn.PgError{Code: "23505", ConstraintName: "uq_runs_one_active_mr_rework"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-7", 55, uuid.New(), "t", "d", sampleReviewSnapshot()); err != ErrActiveMRReworkExists {
		t.Fatalf("err = %v, want ErrActiveMRReworkExists", err)
	}
}

func TestCreateAutoMRReworkRunRepoNotFound(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-7", 55, uuid.New(), "t", "d", sampleReviewSnapshot()); err != ErrRepoNotFound {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// TestMRReworkClaimBranchFromPipelineRef proves the real Claim path sources the
// claim's Branch from pipeline_ref for an mr_rework run (PRD #700 / issue #778):
// runs.branch is NULL for such a run and the MR branch lives in pipeline_ref, so
// the worker gets the MR branch off the already-wired Branch field. Modeled on
// ciFixClaimPayload / TestCIFixClaimWireContract in ci_fix_test.go.
func TestMRReworkClaimBranchFromPipelineRef(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("FORGE-PAT-PLACEHOLDER"))
	sealedTok, _ := box.Seal([]byte("ANTHROPIC-OAUTH-PLACEHOLDER"))

	fs := &fakeStore{
		claimRun: store.Run{
			ID:               uuid.MustParse("44444444-4444-4444-4444-444444444444"),
			RepoID:           pgUUID(uuid.MustParse("22222222-2222-2222-2222-222222222222")),
			Kind:             RunKindMRRework,
			IssueTitle:       "Rework MR: address review comments",
			IssueDescription: "Fold the MR review-comment fixes onto the existing branch.",
			Status:           "claimed",
			PlanSource:       planSourceAgent,
			// mr_rework: no issue iid; the MR branch lives in pipeline_ref, not branch.
			PipelineRef: pgText("agent/issue-42"),
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic: sealedTok,
	}
	svc := New(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil || payload == nil {
		t.Fatalf("Claim: %v (payload=%v)", err, payload)
	}

	if payload.Kind != RunKindMRRework {
		t.Fatalf("kind = %q, want mr_rework", payload.Kind)
	}
	if payload.IssueIID != nil {
		t.Fatalf("an mr_rework claim must carry null issue_iid, got %v", *payload.IssueIID)
	}
	if payload.Branch == nil || *payload.Branch != "agent/issue-42" {
		t.Fatalf("mr_rework claim must carry the MR branch from pipeline_ref, got %v", payload.Branch)
	}
}
