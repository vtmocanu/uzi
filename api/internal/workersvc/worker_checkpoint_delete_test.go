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

// testCheckpointTip is a plausible checkpoint tip SHA the delete helper seeds by
// default, so a run under test is treated as one that DID publish a checkpoint
// (checkpoint_tip non-NULL) and therefore exercises the CAS delete (PRD #1042 M3). A
// test covering the never-published case clears fs.claimCtx.CheckpointTip to NULL.
const testCheckpointTip = "1111111111111111111111111111111111111111"

// checkpointDeleteSvc builds a Service wired for the terminal-transition checkpoint
// cleanup (PRD #1030 M4): the SSRF gate open, a sealed bot PAT in the claim context,
// the background dispatcher forced SYNCHRONOUS so the async delete is observed
// deterministically, and the delete seam stubbed to record every DeleteOptions it is
// called with (and optionally fail, to prove the terminal state is set regardless).
//
// The seeded claim context carries a non-NULL checkpoint_tip (testCheckpointTip): the
// runs these tests exercise DID publish a checkpoint, so the delete must fire. The
// PRD #1042 M3 NULL-skip case clears it explicitly.
func checkpointDeleteSvc(t *testing.T, fs *fakeStore, deleteErr error) (*Service, *[]pushbroker.DeleteOptions) {
	t.Helper()
	box := newBox(t)
	sealed, err := box.Seal([]byte("bot-pat-CHECKPOINTDELETE-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fs.claimCtx = store.GetRunClaimContextRow{
		RepoWebUrl:      "https://gitlab.example.com/team/repo",
		DefaultBranch:   pgtype.Text{String: "main", Valid: true},
		BaseUrl:         "https://gitlab.example.com",
		BotUsername:     "uzi-bot",
		TokenCiphertext: sealed,
		CheckpointTip:   pgtype.Text{String: testCheckpointTip, Valid: true},
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
	svc.SetBackground(func(fn func()) { fn() }) // run the async delete inline, deterministically
	var calls []pushbroker.DeleteOptions
	svc.SetDeleteCheckpointFn(func(_ context.Context, o pushbroker.DeleteOptions) error {
		calls = append(calls, o)
		return deleteErr
	})
	return svc, &calls
}

// TestSetStateCompletedDeletesCheckpoint proves a `completed` terminal transition on a
// checkpoint-eligible issue run deletes the run's checkpoint ref, derived server-side
// as agent/issue-<iid>, and that the completion itself is recorded.
func TestSetStateCompletedDeletesCheckpoint(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 123, Valid: true},
			Status:   "completed",
		},
		setCompletedRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)

	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true")
	}
	if fs.setCompleted == nil {
		t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded")
	}
	if len(*calls) != 1 {
		t.Fatalf("delete calls = %d, want 1", len(*calls))
	}
	if got := (*calls)[0].Branch; got != "agent/issue-123" {
		t.Errorf("delete branch = %q, want agent/issue-123", got)
	}
	if got := (*calls)[0].CloneURL; got != "https://gitlab.example.com/team/repo.git" {
		t.Errorf("delete clone URL = %q, want the server-derived clone URL", got)
	}
	if got := (*calls)[0].Username; got != "uzi-bot" {
		t.Errorf("delete username = %q, want uzi-bot", got)
	}
	if string((*calls)[0].PAT) != "bot-pat-CHECKPOINTDELETE-abcdef1234567890" {
		t.Errorf("delete PAT was not the decrypted bot PAT")
	}
}

// TestSetStateCompletedSelfImproveDeletesCheckpoint pins PRD #1062 M3: a terminal
// self_improve run whose checkpoint_tip is non-NULL deletes its checkpoint ref, targeting
// the run-uuid-keyed branch uzi/self-improve/<runID> — NOT the issue branch, even though
// the run carries a valid issue_iid. It would FAIL against the old issue-only gate (which
// no-op'd every non-issue kind, so this delete never fired).
func TestSetStateCompletedSelfImproveDeletesCheckpoint(t *testing.T) {
	runID := uuid.New()
	fs := &fakeStore{
		runOwned: store.Run{
			ID:       runID,
			Kind:     runkind.SelfImprove,
			IssueIid: pgtype.Int8{Int64: 7, Valid: true}, // stable tracking issue, must be ignored
			Status:   "completed",
		},
		setCompletedRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)

	_, applied, err := svc.SetState(context.Background(), worker(), runID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true")
	}
	if fs.setCompleted == nil {
		t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded")
	}
	if len(*calls) != 1 {
		t.Fatalf("delete calls = %d, want 1 (a self_improve run's checkpoint must be cleaned up)", len(*calls))
	}
	wantBranch := "uzi/self-improve/" + runID.String()
	if got := (*calls)[0].Branch; got != wantBranch {
		t.Errorf("delete branch = %q, want %q (run-uuid-keyed, NOT the issue branch)", got, wantBranch)
	}
	if got := (*calls)[0].ExpectedOldTip; got != testCheckpointTip {
		t.Errorf("delete ExpectedOldTip = %q, want the persisted tip %q", got, testCheckpointTip)
	}
}

// TestSetStateCompletedNullCheckpointTipNoDelete is the PRD #1042 M3 skip-on-NULL
// guard: a checkpoint-eligible issue run whose runs.checkpoint_tip is NULL NEVER
// published a checkpoint ref, so it owns nothing — deleteCheckpointBestEffort must NOT
// invoke the delete seam (an unconditional delete could clobber a SIBLING run's fresh
// checkpoint on the same branch). The terminal completion is still recorded.
func TestSetStateCompletedNullCheckpointTipNoDelete(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 123, Valid: true},
			Status:   "completed",
		},
		setCompletedRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)
	// This run NEVER published a checkpoint: checkpoint_tip is NULL.
	fs.claimCtx.CheckpointTip = pgtype.Text{} // Valid == false

	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true")
	}
	if fs.setCompleted == nil {
		t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded")
	}
	if len(*calls) != 0 {
		t.Fatalf("delete calls = %d, want 0 (a never-published run owns no ref and must not attempt a delete)", len(*calls))
	}
}

// TestSetStateCompletedPassesExpectedOldTip proves the non-NULL path: a run that DID
// publish a checkpoint deletes it, passing its persisted tip as the CAS Old
// (DeleteOptions.ExpectedOldTip), so the ref is removed only while origin still points
// at exactly the tip THIS run published.
func TestSetStateCompletedPassesExpectedOldTip(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 55, Valid: true},
			Status:   "completed",
		},
		setCompletedRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil) // seeds CheckpointTip = testCheckpointTip

	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true")
	}
	if len(*calls) != 1 {
		t.Fatalf("delete calls = %d, want 1 (a published run must clean up its ref)", len(*calls))
	}
	if got := (*calls)[0].ExpectedOldTip; got != testCheckpointTip {
		t.Errorf("delete ExpectedOldTip = %q, want the persisted tip %q", got, testCheckpointTip)
	}
}

// TestSetStateFailedCancelledDeletesCheckpoint proves the failed→cancelled terminal
// route (a consumed operator cancel arriving as `failed`, stop_kind='cancelled', which
// SetState routes to CancelRunByWorker) also triggers the checkpoint delete.
func TestSetStateFailedCancelledDeletesCheckpoint(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 77, Valid: true},
			Status:   "cancelled",
			StopKind: pgtype.Text{String: "cancelled", Valid: true},
		},
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)

	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "failed"})
	if err != nil {
		t.Fatalf("SetState(failed): %v", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true")
	}
	if fs.cancelledByWorker == nil {
		t.Fatalf("CancelRunByWorker was not called; the terminal state must be recorded")
	}
	if len(*calls) != 1 {
		t.Fatalf("delete calls = %d, want 1", len(*calls))
	}
	if got := (*calls)[0].Branch; got != "agent/issue-77" {
		t.Errorf("delete branch = %q, want agent/issue-77", got)
	}
}

// TestSetStateCompletedIneligibleKindNoDelete proves a non-issue run — which never
// published a checkpoint ref — triggers NO delete on its terminal transition.
func TestSetStateCompletedIneligibleKindNoDelete(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:   runkind.Task, // not an issue run: no checkpoint ref ever existed
			Status: "completed",
		},
		setCompletedRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)

	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true")
	}
	if fs.setCompleted == nil {
		t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded")
	}
	if len(*calls) != 0 {
		t.Fatalf("delete calls = %d, want 0 (ineligible kind must not attempt a delete)", len(*calls))
	}
}

// TestSetStateNonTerminalNoDelete proves a non-terminal transition (`running`) on an
// eligible issue run does NOT delete the (still live) checkpoint ref.
func TestSetStateNonTerminalNoDelete(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 5, Valid: true},
			Status:   "running",
		},
		setRunningRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)

	if _, _, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "running"}); err != nil {
		t.Fatalf("SetState(running): %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("delete calls = %d, want 0 (a live run's checkpoint must not be deleted)", len(*calls))
	}
}

// TestSetStateCompletedDeleteErrorStillCompletes proves the delete is BEST-EFFORT: a
// delete failure must NOT fail the terminal transition — the completion is still
// recorded and SetState returns success.
func TestSetStateCompletedDeleteErrorStillCompletes(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 9, Valid: true},
			Status:   "completed",
		},
		setCompletedRows: 1,
	}
	svc, calls := checkpointDeleteSvc(t, fs, errors.New("forge is down"))

	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState returned an error on a failed checkpoint delete: %v (the delete is best-effort and must never fail the terminal report)", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true (completion must be recorded despite the delete error)")
	}
	if fs.setCompleted == nil {
		t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded regardless of the delete outcome")
	}
	if len(*calls) != 1 {
		t.Fatalf("delete calls = %d, want 1 (the delete was attempted)", len(*calls))
	}
}

// TestSetStateCompletedDeletePanicRecovered proves the checkpoint-delete closure
// RECOVERS a panic from the delete seam (pushbroker.Delete drives go-git, which has
// nil-deref panic paths on hostile forge responses). In production s.background is a
// DETACHED goroutine, so an unrecovered panic there crashes the whole api process. The
// test forces the delete seam to PANIC and runs it under the SYNCHRONOUS background seam
// (fn() inline), so the panic — absent the recover — would propagate out to this test
// caller and fail it. With the recover in the closure body it is swallowed, the terminal
// transition still returns success, and the completion is recorded.
func TestSetStateCompletedDeletePanicRecovered(t *testing.T) {
	fs := &fakeStore{
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 42, Valid: true},
			Status:   "completed",
		},
		setCompletedRows: 1,
	}
	svc, _ := checkpointDeleteSvc(t, fs, nil)
	// Override the seam to PANIC, as go-git can on a malformed/hostile forge response.
	svc.SetDeleteCheckpointFn(func(_ context.Context, _ pushbroker.DeleteOptions) error {
		panic("go-git nil-deref on hostile forge response")
	})

	// With the synchronous background seam, a panic that escaped the closure would
	// propagate here and crash/fail the test. The recover in the closure body must
	// swallow it.
	_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: "completed"})
	if err != nil {
		t.Fatalf("SetState(completed): %v (a panic in the best-effort delete must not fail the terminal report)", err)
	}
	if !applied {
		t.Fatalf("applied = false, want true (completion must be recorded despite the delete panic)")
	}
	if fs.setCompleted == nil {
		t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded regardless of the delete panic")
	}
}

// TestServerSideCancelDeletesCheckpoint proves the server-side cancel path (no live
// poller — the run is committed terminal outside SetState) also deletes the run's
// checkpoint ref.
func TestServerSideCancelDeletesCheckpoint(t *testing.T) {
	userID := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{
		// GetRunByIDForUser feeds SubmitInput. No WorkerID → hasLivePoller is false, so
		// the cancel commits SERVER-SIDE (the distinct terminal path M4 must also hook).
		runByID: store.Run{
			ID:       runID,
			UserID:   userID,
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 321, Valid: true},
			Status:   "running",
		},
	}
	svc, calls := checkpointDeleteSvc(t, fs, nil)

	res, err := svc.SubmitInput(context.Background(), userID, runID, "cancel", "operator says stop", nil)
	if err != nil {
		t.Fatalf("SubmitInput(cancel): %v", err)
	}
	if !res.ServerSide {
		t.Fatalf("ServerSide = false, want true (no live poller → server-side cancel)")
	}
	if fs.cancelled == nil {
		t.Fatalf("CancelRunServerSide was not called; the terminal state must be recorded")
	}
	if len(*calls) != 1 {
		t.Fatalf("delete calls = %d, want 1", len(*calls))
	}
	if got := (*calls)[0].Branch; got != "agent/issue-321" {
		t.Errorf("delete branch = %q, want agent/issue-321", got)
	}
}
