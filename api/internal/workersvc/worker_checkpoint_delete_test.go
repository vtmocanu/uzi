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

// checkpointDeleteSvc builds a Service wired for the terminal-transition checkpoint
// cleanup (PRD #1030 M4): the SSRF gate open, a sealed bot PAT in the claim context,
// the background dispatcher forced SYNCHRONOUS so the async delete is observed
// deterministically, and the delete seam stubbed to record every DeleteOptions it is
// called with (and optionally fail, to prove the terminal state is set regardless).
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
