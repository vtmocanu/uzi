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
	// The create-time CROSS-KIND branch guard: an active ci_fix OR mr_rework run on the
	// ref's pipeline_ref blocks the create with ErrBranchInUse (the detector swallows it).
	fs := &fakeStore{repoRow: aValidRepoRow(), activeBranchRefRuns: 1}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", 9, uuid.New(), "t", "d", sampleReviewSnapshot()); err != ErrBranchInUse {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
	if fs.mrReworkRunParams != nil {
		t.Fatal("the branch guard must block BEFORE the insert")
	}
}

func TestCreateAutoMRReworkRunMapsDuplicate(t *testing.T) {
	fs := &fakeStore{repoRow: aValidRepoRow(), mrReworkRunErr: &pgconn.PgError{Code: "23505"}}
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
