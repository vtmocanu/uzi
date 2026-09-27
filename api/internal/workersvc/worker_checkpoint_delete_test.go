package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// testCheckpointTip is a plausible checkpoint tip SHA the fake claim context carries, so a
// run under test is one that DID publish a checkpoint (checkpoint_tip non-NULL): the case in
// which a delete would be possible at all, and which these fake-store tests show is retained.
const testCheckpointTip = "1111111111111111111111111111111111111111"

// checkpointDeleteSvc builds a FAKE-store Service wired for the terminal-transition
// checkpoint handling (PRD #1030 M4, PRD #1810 M1): the SSRF gate open, a sealed bot PAT in
// the claim context, the background dispatcher forced SYNCHRONOUS, and the delete seam stubbed
// to record every DeleteOptions it is called with.
//
// It wires NO retention lock pool (a fake store has no Postgres session to hold an advisory
// lock on), so PRD #1810's fail-safe applies: every terminal transition RETAINS the ref and
// the delete seam is never called. The delete path itself, with a real lock, custody holds and
// checkpoint_retentions rows, is covered by checkpoint_retention_livedb_test.go.
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

// retainedFakeCases are terminal transitions on checkpoint-eligible runs that published a
// checkpoint. Without a retention lock pool none of them may call the delete seam, whatever the
// terminal status, and each must still record its terminal state.
func TestTerminalTransitionWithoutRetentionLockRetainsCheckpoint(t *testing.T) {
	cases := []struct {
		name  string
		run   store.Run
		state string
		rows  func(fs *fakeStore)
		check func(t *testing.T, fs *fakeStore)
	}{
		{
			name:  "completed",
			run:   store.Run{Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 123, Valid: true}, Status: "completed"},
			state: "completed",
			rows:  func(fs *fakeStore) { fs.setCompletedRows = 1 },
			check: func(t *testing.T, fs *fakeStore) {
				if fs.setCompleted == nil {
					t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded")
				}
			},
		},
		{
			name:  "completed self_improve",
			run:   store.Run{ID: uuid.New(), Kind: runkind.SelfImprove, IssueIid: pgtype.Int8{Int64: 7, Valid: true}, Status: "completed"},
			state: "completed",
			rows:  func(fs *fakeStore) { fs.setCompletedRows = 1 },
			check: func(t *testing.T, fs *fakeStore) {
				if fs.setCompleted == nil {
					t.Fatalf("SetRunCompleted was not called; the terminal state must be recorded")
				}
			},
		},
		{
			name:  "failed routed to cancelled",
			run:   store.Run{Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 77, Valid: true}, Status: "cancelled", StopKind: pgtype.Text{String: "cancelled", Valid: true}},
			state: "failed",
			rows:  func(*fakeStore) {},
			check: func(t *testing.T, fs *fakeStore) {
				if fs.cancelledByWorker == nil {
					t.Fatalf("CancelRunByWorker was not called; the terminal state must be recorded")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{runOwned: tc.run}
			tc.rows(fs)
			svc, calls := checkpointDeleteSvc(t, fs, nil)
			_, applied, err := svc.SetState(context.Background(), worker(), fs.runOwned.ID, StateRequest{State: tc.state})
			if err != nil {
				t.Fatalf("SetState(%s): %v", tc.state, err)
			}
			if !applied {
				t.Fatalf("applied = false, want true")
			}
			tc.check(t, fs)
			if len(*calls) != 0 {
				t.Fatalf("delete calls = %d, want 0 (no retention lock pool: the ref must be retained)", len(*calls))
			}
		})
	}
}

// TestServerSideCancelWithoutRetentionLockRetainsCheckpoint: the server-side cancel path (no
// live poller, the run is committed terminal outside SetState) with no retention lock pool
// records the cancel and retains the ref.
func TestServerSideCancelWithoutRetentionLockRetainsCheckpoint(t *testing.T) {
	userID := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{
		// No WorkerID → hasLivePoller is false, so the cancel commits SERVER-SIDE.
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
	if len(*calls) != 0 {
		t.Fatalf("delete calls = %d, want 0 (no retention lock pool: the ref must be retained)", len(*calls))
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

// TestRetentionBackoff pins the failed-delete retry schedule: one minute after the first
// failure, doubling per prior failure, capped at one hour.
func TestRetentionBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempts int32
		want     time.Duration
	}{
		{0, time.Minute}, {1, 2 * time.Minute}, {2, 4 * time.Minute}, {5, 32 * time.Minute}, {6, time.Hour}, {40, time.Hour},
	} {
		if got := retentionBackoff(tc.attempts); got != tc.want {
			t.Errorf("retentionBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// TestWithRetentionLockWithoutPoolNotAcquired: no pool wired means the lock is never taken and
// fn never runs, so a caller can only retain.
func TestWithRetentionLockWithoutPoolNotAcquired(t *testing.T) {
	svc := New(&fakeStore{}, nil, testParams())
	ran := false
	acquired, err := svc.withRetentionLock(context.Background(), uuid.New(), func(context.Context, func(context.Context) error) error {
		ran = true
		return nil
	})
	if err != nil || acquired || ran {
		t.Fatalf("withRetentionLock without a pool: acquired=%v err=%v ran=%v, want false/nil/false", acquired, err, ran)
	}
}
