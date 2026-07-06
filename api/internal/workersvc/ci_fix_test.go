package workersvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func sampleSnapshot() FailureSnapshot {
	return FailureSnapshot{
		PipelineID: 4200, Ref: "main", SHA: "deadbeef",
		WebURL:     "https://gitlab.example.com/g/p/-/pipelines/4200",
		FailedJobs: []SnapshotJob{{Name: "unit", Stage: "test", WebURL: "https://gitlab.example.com/g/p/-/jobs/1", LogTail: "--- FAIL: TestFoo\n"}},
	}
}

func TestCreateCIFixRunSerializesSnapshot(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{ciFixRunResult: store.Run{ID: uuid.New(), Kind: RunKindCIFix}}
	svc := New(fs, newBox(t), testParams())

	run, err := svc.CreateCIFixRun(context.Background(), user, repo, "main", "Fix CI: main", "desc", sampleSnapshot())
	if err != nil {
		t.Fatalf("CreateCIFixRun: %v", err)
	}
	if run.Kind != RunKindCIFix {
		t.Fatalf("expected a ci_fix run, got kind %q", run.Kind)
	}
	if fs.ciFixRunParams == nil {
		t.Fatal("CreateCIFixRun store call not made")
	}
	if fs.ciFixRunParams.PipelineRef.String != "main" || fs.ciFixRunParams.PipelineID.Int64 != 4200 {
		t.Fatalf("pipeline ref/id not carried onto the run: %+v", fs.ciFixRunParams)
	}
	var snap FailureSnapshot
	if err := json.Unmarshal(fs.ciFixRunParams.FailureSnapshot, &snap); err != nil {
		t.Fatalf("failure_snapshot is not valid jsonb: %v", err)
	}
	if len(snap.FailedJobs) != 1 || snap.FailedJobs[0].LogTail != "--- FAIL: TestFoo\n" {
		t.Fatalf("snapshot did not round-trip: %+v", snap)
	}
}

func TestCreateCIFixRunRefusesBranchInUse(t *testing.T) {
	fs := &fakeStore{activeBranchRuns: 1} // an active run already occupies the ref's worktree
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", "t", "d", sampleSnapshot()); err != ErrBranchInUse {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
}

func TestCreateCIFixRunMapsDuplicateToActiveFixExists(t *testing.T) {
	fs := &fakeStore{ciFixRunErr: &pgconn.PgError{Code: "23505"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), "main", "t", "d", sampleSnapshot()); err != ErrActiveFixExists {
		t.Fatalf("err = %v, want ErrActiveFixExists", err)
	}
}

func TestCreateRunRefusesWhenCIFixActiveOnBranch(t *testing.T) {
	// The reverse cross-kind guard: an issue run for issue 9 refuses to start when a
	// ci_fix run is already fixing agent/issue-9 (they would share one worktree).
	fs := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: true}, activeCIFixRuns: 1}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 9, "d"); err != ErrBranchInUse {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
}

func TestSetStateCompletedCarriesNotCodeVerdict(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned:         store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Kind: RunKindCIFix, Status: "running"},
		setCompletedRows: 1,
	}
	svc := New(fs, newBox(t), testParams())
	verdict := "not_code"
	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{State: "completed", FixVerdict: &verdict}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setCompleted == nil || !fs.setCompleted.FixVerdict.Valid || fs.setCompleted.FixVerdict.String != "not_code" {
		t.Fatalf("the not_code verdict must reach SetRunCompleted, got %+v", fs.setCompleted)
	}
}

func TestSetStateCompletedClampsForgedVerdict(t *testing.T) {
	// Integrity (PRD #6): verified/fix_failed are pipeline-sync-authoritative — a
	// worker reporting 'verified' on the wire must NOT be able to forge the badge.
	for _, forged := range []string{"verified", "fix_failed", "totally_fixed"} {
		w := worker()
		fs := &fakeStore{
			runOwned:         store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Kind: RunKindCIFix, Status: "running"},
			setCompletedRows: 1,
		}
		svc := New(fs, newBox(t), testParams())
		v := forged
		if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{State: "completed", FixVerdict: &v}); err != nil {
			t.Fatalf("SetState: %v", err)
		}
		if fs.setCompleted == nil || fs.setCompleted.FixVerdict.Valid {
			t.Fatalf("a wire-reported %q must be clamped to NULL, got %+v", forged, fs.setCompleted)
		}
	}
}

// ciFixWireFixture is the golden JSON for a ci_fix claim payload. It is the
// server side of the cross-side wire contract (PRD #6): the worker's own test pins
// the SAME file, so a ci_fix-specific field (kind, pipeline, null issue_iid,
// fix_verdict) can never be invented differently by two lenient fakes.
const ciFixWireFixture = "testdata/claim_ci_fix_wire.json"

// ciFixClaimPayload runs a real Claim of a ci_fix run so the golden reflects the
// server's actual assembly (store.Run → ClaimPayload → JSON), not a hand-built
// struct. Fixed ids/secrets keep the golden stable.
func ciFixClaimPayload(t *testing.T) ClaimPayload {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("FORGE-PAT-PLACEHOLDER"))
	sealedTok, _ := box.Seal([]byte("ANTHROPIC-OAUTH-PLACEHOLDER"))
	snapJSON, _ := json.Marshal(sampleSnapshot())

	fs := &fakeStore{
		claimRun: store.Run{
			ID:               uuid.MustParse("33333333-3333-3333-3333-333333333333"),
			RepoID:           uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			Kind:             RunKindCIFix,
			IssueTitle:       "Fix CI: main pipeline #4200",
			IssueDescription: "Diagnose and fix the failed pipeline for `main`.",
			Status:           "claimed",
			PipelineRef:      pgText("main"),
			PipelineID:       pgtype.Int8{Int64: 4200, Valid: true},
			FailureSnapshot:  snapJSON,
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
	return *payload
}

func TestCIFixClaimWireContract(t *testing.T) {
	payload := ciFixClaimPayload(t)

	// Shape assertions the worker relies on (belt-and-braces alongside the golden).
	if payload.Kind != RunKindCIFix {
		t.Fatalf("kind = %q, want ci_fix", payload.Kind)
	}
	if payload.IssueIID != nil {
		t.Fatalf("a ci_fix claim must carry null issue_iid, got %v", *payload.IssueIID)
	}
	if payload.Pipeline == nil || payload.Pipeline.ID != 4200 || payload.Pipeline.Ref != "main" {
		t.Fatalf("ci_fix claim must carry the failed-pipeline snapshot, got %+v", payload.Pipeline)
	}
	if len(payload.Pipeline.FailedJobs) != 1 || payload.Pipeline.FailedJobs[0].Name != "unit" {
		t.Fatalf("failed jobs not delivered: %+v", payload.Pipeline)
	}

	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	path := filepath.FromSlash(ciFixWireFixture)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `UPDATE_GOLDEN=1 go test` to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ci_fix claim wire shape drifted from %s.\nIf intended, regenerate with UPDATE_GOLDEN=1 and update the worker-side contract test to match.\n--- got ---\n%s", ciFixWireFixture, got)
	}
}
