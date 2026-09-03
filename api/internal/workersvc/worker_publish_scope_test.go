package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPublishWorkflowScopeRejectedSkipsCleanly pins PRD #456 M4 at the service seam: when
// the go-git broker reports ErrWorkflowScopeRejected (the branch is behind on
// .github/workflows/** and the bot's repo-only PAT cannot push the checkpoint), Publish
// returns a BENIGN skip — Published=false, Skipped="workflow_scope", Ref set, and a NIL
// error — so it never reaches the 5xx/slog.Error default arm and never fails the run.
// Checkpoints stay best-effort; the finalize base-align (M1) is the real safety net.
func TestPublishWorkflowScopeRejectedSkipsCleanly(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fs := &fakeStore{
		// A running ISSUE run the worker owns, with its issue iid set but an empty branch
		// column (the mid-run shape) — the service derives agent/issue-<iid> from the iid.
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 456, Valid: true},
			Branch:   pgtype.Text{},
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl:      "https://gitlab.example.com/team/repo",
			DefaultBranch:   pgtype.Text{String: "main", Valid: true},
			BaseUrl:         "https://gitlab.example.com",
			BotUsername:     "uzi-bot",
			TokenCiphertext: sealed,
		},
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
	// The broker rejects the push for a missing workflow scope.
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		return pushbroker.Result{}, pushbroker.ErrWorkflowScopeRejected
	})

	res, err := svc.Publish(context.Background(), worker(), uuid.New(), "0123456789abcdef0123456789abcdef01234567", []byte("pack"))
	if err != nil {
		t.Fatalf("Publish returned a non-nil error for a workflow-scope rejection: %v (must be a benign skip, not a run failure)", err)
	}
	if res.Published {
		t.Errorf("Published = true, want false (the checkpoint push was rejected)")
	}
	if res.Skipped != "workflow_scope" {
		t.Errorf("Skipped = %q, want %q", res.Skipped, "workflow_scope")
	}
	if res.Ref != "refs/uzi-checkpoints/agent/issue-456" {
		t.Errorf("Ref = %q, want the server-derived checkpoint ref", res.Ref)
	}
	// The benign-skip arm must NOT persist a tip: a stale first-only/skip tip would make
	// the later terminal CAS-delete refuse and leave a stale ref.
	if len(fs.checkpointTips) != 0 {
		t.Errorf("SetRunCheckpointTip called %d time(s) on a workflow-scope skip, want 0", len(fs.checkpointTips))
	}
}

// TestPublishSelfImproveDerivesRunIDBranch pins PRD #1062 M3: a self_improve run (which
// also carries a valid issue_iid, its stable tracking issue) checkpoints to a
// run-uuid-keyed branch uzi/self-improve/<runID>, NOT the issue branch — proving the
// server-side gate dispatches on kind FIRST. It would FAIL against the old issue-only gate
// (which returned Skipped:"unsupported" for any non-issue kind). The captured
// pushbroker.Options.Branch, the returned Ref, and the persisted tip are all asserted.
func TestPublishSelfImproveDerivesRunIDBranch(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fs := &fakeStore{
		// A running SELF_IMPROVE run the worker owns. It carries a valid issue_iid (the
		// stable tracking issue), which the server MUST ignore in favour of the run uuid.
		runOwned: store.Run{
			Kind:     runkind.SelfImprove,
			IssueIid: pgtype.Int8{Int64: 7, Valid: true},
			Branch:   pgtype.Text{},
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl:      "https://gitlab.example.com/team/repo",
			DefaultBranch:   pgtype.Text{String: "main", Valid: true},
			BaseUrl:         "https://gitlab.example.com",
			BotUsername:     "uzi-bot",
			TokenCiphertext: sealed,
		},
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
	var captured pushbroker.Options
	svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		captured = o
		return pushbroker.Result{}, nil
	})

	runID := uuid.New()
	wantBranch := "uzi/self-improve/" + runID.String()
	res, err := svc.Publish(context.Background(), worker(), runID, "0123456789abcdef0123456789abcdef01234567", []byte("pack"))
	if err != nil {
		t.Fatalf("Publish(self_improve): %v", err)
	}
	if !res.Published {
		t.Errorf("Published = false, want true (a self_improve run is checkpoint-eligible)")
	}
	if res.Skipped != "" {
		t.Errorf("Skipped = %q, want \"\" (self_improve must not be an unsupported skip)", res.Skipped)
	}
	if captured.Branch != wantBranch {
		t.Errorf("pushbroker Branch = %q, want %q (run-uuid-keyed, NOT the issue branch)", captured.Branch, wantBranch)
	}
	if res.Ref != "refs/uzi-checkpoints/"+wantBranch {
		t.Errorf("Ref = %q, want %q", res.Ref, "refs/uzi-checkpoints/"+wantBranch)
	}
	if len(fs.checkpointTips) != 1 {
		t.Errorf("SetRunCheckpointTip called %d time(s), want 1 (a successful publish persists the tip)", len(fs.checkpointTips))
	}
}

// TestPublishIneligibleKindSkipsUnsupported pins that an ineligible kind (chat) is a
// benign unsupported skip — no publish, no tip persisted — under the new checkpoint-eligible
// gate, exactly as before M3.
func TestPublishIneligibleKindSkipsUnsupported(t *testing.T) {
	box := newBox(t)
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:   runkind.Chat,
			Branch: pgtype.Text{},
		},
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		t.Fatal("publishFn must not be called for an ineligible kind")
		return pushbroker.Result{}, nil
	})

	res, err := svc.Publish(context.Background(), worker(), uuid.New(), "0123456789abcdef0123456789abcdef01234567", []byte("pack"))
	if err != nil {
		t.Fatalf("Publish(chat): %v", err)
	}
	if res.Published {
		t.Errorf("Published = true, want false (chat is not checkpoint-eligible)")
	}
	if res.Skipped != "unsupported" {
		t.Errorf("Skipped = %q, want %q", res.Skipped, "unsupported")
	}
	if len(fs.checkpointTips) != 0 {
		t.Errorf("SetRunCheckpointTip called %d time(s), want 0 (an unsupported skip persists nothing)", len(fs.checkpointTips))
	}
}

// TestPublishPersistsTipOnEverySuccessAndNotOnSkip pins PRD #1042 M2: a CAS-accepted
// advance (err == nil) persists the just-published tip to runs.checkpoint_tip on EVERY
// success — so the tip ADVANCES across multiple publishes, not first-only — while a
// benign-skip publish (ErrNotDescendant) persists nothing.
func TestPublishPersistsTipOnEverySuccessAndNotOnSkip(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 456, Valid: true},
			Branch:   pgtype.Text{},
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl:      "https://gitlab.example.com/team/repo",
			DefaultBranch:   pgtype.Text{String: "main", Valid: true},
			BaseUrl:         "https://gitlab.example.com",
			BotUsername:     "uzi-bot",
			TokenCiphertext: sealed,
		},
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })

	runID := uuid.New()

	// First successful publish → persists tip1.
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		return pushbroker.Result{}, nil
	})
	const tip1 = "1111111111111111111111111111111111111111"
	res, err := svc.Publish(context.Background(), worker(), runID, tip1, []byte("pack"))
	if err != nil {
		t.Fatalf("Publish(tip1): %v", err)
	}
	if !res.Published {
		t.Fatalf("Published = false on a CAS-accepted advance, want true")
	}
	if len(fs.checkpointTips) != 1 {
		t.Fatalf("after publish 1: SetRunCheckpointTip called %d time(s), want 1", len(fs.checkpointTips))
	}
	if got := fs.checkpointTips[0]; got.ID != runID || got.CheckpointTip.String != tip1 || !got.CheckpointTip.Valid {
		t.Fatalf("persisted tip1 = {id:%v tip:%q valid:%v}, want {id:%v tip:%q valid:true}",
			got.ID, got.CheckpointTip.String, got.CheckpointTip.Valid, runID, tip1)
	}

	// Second successful publish with a DIFFERENT tip → tip ADVANCES (not first-only).
	const tip2 = "2222222222222222222222222222222222222222"
	if _, err := svc.Publish(context.Background(), worker(), runID, tip2, []byte("pack")); err != nil {
		t.Fatalf("Publish(tip2): %v", err)
	}
	if len(fs.checkpointTips) != 2 {
		t.Fatalf("after publish 2: SetRunCheckpointTip called %d time(s), want 2 (tip must advance on every success, not first-only)", len(fs.checkpointTips))
	}
	if got := fs.checkpointTips[1]; got.CheckpointTip.String != tip2 {
		t.Fatalf("persisted tip on publish 2 = %q, want %q (advanced)", got.CheckpointTip.String, tip2)
	}

	// A benign-skip publish (ErrNotDescendant) must persist NOTHING.
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		return pushbroker.Result{}, pushbroker.ErrNotDescendant
	})
	const tip3 = "3333333333333333333333333333333333333333"
	res, err = svc.Publish(context.Background(), worker(), runID, tip3, []byte("pack"))
	if err != nil {
		t.Fatalf("Publish(skip): %v", err)
	}
	if res.Published {
		t.Fatalf("Published = true on a not-descendant skip, want false")
	}
	if len(fs.checkpointTips) != 2 {
		t.Fatalf("after benign skip: SetRunCheckpointTip called %d time(s) total, want 2 (a skip must not persist)", len(fs.checkpointTips))
	}
}

// TestPublishSucceedsDespiteTipPersistError pins PRD #1042 M2's best-effort property in the
// err == nil arm: the runs.checkpoint_tip persist is fire-and-log, so a SetRunCheckpointTip
// error must NOT fail the publish — the checkpoint ref is already advanced on the forge, so
// the CAS-accepted advance still returns Published=true and a nil error. This is the only
// test that drives fakeStore.checkpointTipErr non-nil, exercising the perr != nil branch; it
// would fail if the err == nil arm were changed to propagate the persist error.
func TestPublishSucceedsDespiteTipPersistError(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 456, Valid: true},
			Branch:   pgtype.Text{},
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl:      "https://gitlab.example.com/team/repo",
			DefaultBranch:   pgtype.Text{String: "main", Valid: true},
			BaseUrl:         "https://gitlab.example.com",
			BotUsername:     "uzi-bot",
			TokenCiphertext: sealed,
		},
		// Force the best-effort persist to fail on every SetRunCheckpointTip call.
		checkpointTipErr: errors.New("boom"),
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
	// A CAS-accepted advance (publishFn returns nil), exactly like the success case above.
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		return pushbroker.Result{}, nil
	})

	res, err := svc.Publish(context.Background(), worker(), uuid.New(), "0123456789abcdef0123456789abcdef01234567", []byte("pack"))
	// A persist failure must NOT fail the publish: the ref is already advanced on the forge.
	if err != nil {
		t.Fatalf("Publish returned a non-nil error when only the tip persist failed: %v (a persist failure must not fail the publish)", err)
	}
	if !res.Published {
		t.Errorf("Published = false despite a CAS-accepted advance, want true (persist error is best-effort)")
	}
	if res.Ref != "refs/uzi-checkpoints/agent/issue-456" {
		t.Errorf("Ref = %q, want the server-derived checkpoint ref", res.Ref)
	}
	// The persist WAS attempted (and errored) — the err == nil arm still tries the write.
	if len(fs.checkpointTips) != 1 {
		t.Errorf("SetRunCheckpointTip called %d time(s), want 1 (the err == nil arm attempts the best-effort persist)", len(fs.checkpointTips))
	}
}
