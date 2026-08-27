package notifysvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// text builds a valid pgtype.Text (a NULL is the zero value).
func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// TestRunFailureNotifierHandle drives handle directly (synchronous, no goroutine
// race) across the stop_kind gate. slack is nil throughout, so a written
// notification is inbox-only by construction.
func TestRunFailureNotifierHandle(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()

	cases := []struct {
		name       string
		run        store.Run
		wantNotify bool
	}{
		{
			name:       "failed + stop_kind NULL ⇒ notify",
			run:        store.Run{ID: runID, UserID: user, Status: "failed", Kind: "issue"},
			wantNotify: true,
		},
		{
			name:       "failed + stop_kind auto_stopped ⇒ notify",
			run:        store.Run{ID: runID, UserID: user, Status: "failed", Kind: "issue", StopKind: text("auto_stopped")},
			wantNotify: true,
		},
		{
			name:       "failed + stop_kind cancelled ⇒ no notify (human stop)",
			run:        store.Run{ID: runID, UserID: user, Status: "failed", Kind: "issue", StopKind: text("cancelled")},
			wantNotify: false,
		},
		{
			name:       "failed + stop_kind plan_rejected ⇒ no notify (human stop)",
			run:        store.Run{ID: runID, UserID: user, Status: "failed", Kind: "issue", StopKind: text("plan_rejected")},
			wantNotify: false,
		},
		{
			name:       "status moved off failed ⇒ no notify (defensive)",
			run:        store.Run{ID: runID, UserID: user, Status: "completed", Kind: "issue"},
			wantNotify: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{run: tc.run}
			svc := New(fs, nil, 0, nil) // slack == nil ⇒ inbox-only
			n := NewRunFailureNotifier(fs, svc, nil)

			n.handle(context.Background(), runID)

			if tc.wantNotify {
				if !fs.insertCalled {
					t.Fatalf("expected an InsertNotification call")
				}
				if fs.inserted.Kind != "run_failed" {
					t.Fatalf("kind = %q, want run_failed", fs.inserted.Kind)
				}
				if fs.inserted.UserID != user {
					t.Fatalf("user = %s, want %s", fs.inserted.UserID, user)
				}
				if !fs.inserted.RunID.Valid || uuid.UUID(fs.inserted.RunID.Bytes) != runID {
					t.Fatalf("run anchor = %+v, want %s", fs.inserted.RunID, runID)
				}
			} else if fs.insertCalled {
				t.Fatalf("expected NO notification, but InsertNotification was called")
			}
		})
	}
}

// TestRunFailureNotifierPayload asserts the inbox payload carries the run id,
// kind, and issue iid when present.
func TestRunFailureNotifierPayload(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{run: store.Run{
		ID:       runID,
		UserID:   user,
		Status:   "failed",
		Kind:     "ci_fix",
		IssueIid: pgtype.Int8{Int64: 216, Valid: true},
	}}
	svc := New(fs, nil, 0, nil)
	n := NewRunFailureNotifier(fs, svc, nil)

	n.handle(context.Background(), runID)

	if !fs.insertCalled {
		t.Fatalf("expected a notification")
	}
	var payload map[string]any
	if err := json.Unmarshal(fs.inserted.Payload, &payload); err != nil {
		t.Fatalf("payload not valid json: %v (%s)", err, fs.inserted.Payload)
	}
	if payload["run_id"] != runID.String() {
		t.Fatalf("payload run_id = %v, want %s", payload["run_id"], runID)
	}
	if payload["kind"] != "ci_fix" {
		t.Fatalf("payload kind = %v, want ci_fix", payload["kind"])
	}
	// JSON numbers decode as float64.
	if payload["issue_iid"] != float64(216) {
		t.Fatalf("payload issue_iid = %v, want 216", payload["issue_iid"])
	}
}

// TestRunFailureNotifierOmitsIssueIidWhenNull confirms a NULL issue_iid is left
// out of the payload rather than serialized as a zero.
func TestRunFailureNotifierOmitsIssueIidWhenNull(t *testing.T) {
	runID := uuid.New()
	fs := &fakeStore{run: store.Run{ID: runID, UserID: uuid.New(), Status: "failed", Kind: "issue"}}
	svc := New(fs, nil, 0, nil)
	n := NewRunFailureNotifier(fs, svc, nil)

	n.handle(context.Background(), runID)

	var payload map[string]any
	if err := json.Unmarshal(fs.inserted.Payload, &payload); err != nil {
		t.Fatalf("payload not valid json: %v", err)
	}
	if _, ok := payload["issue_iid"]; ok {
		t.Fatalf("issue_iid should be omitted when NULL, got %v", payload["issue_iid"])
	}
}

// TestRunFailureNotifierLoadErrorIsSwallowed confirms a GetRunByID error is
// logged-and-returned, not surfaced, and writes nothing.
func TestRunFailureNotifierLoadErrorIsSwallowed(t *testing.T) {
	fs := &fakeStore{runErr: context.DeadlineExceeded}
	svc := New(fs, nil, 0, nil)
	n := NewRunFailureNotifier(fs, svc, nil)

	n.handle(context.Background(), uuid.New())

	if fs.insertCalled {
		t.Fatalf("no notification should be written when the run load fails")
	}
}

// TestRunFailureNotifierPublishStateEnqueue confirms only a "failed" status
// enqueues; other statuses are ignored so the drain never runs for them.
func TestRunFailureNotifierPublishStateEnqueue(t *testing.T) {
	fs := &fakeStore{run: store.Run{Status: "failed"}}
	svc := New(fs, nil, 0, nil)
	n := NewRunFailureNotifier(fs, svc, nil)

	n.PublishState(uuid.New(), "completed")
	n.PublishState(uuid.New(), "running")
	if len(n.ch) != 0 {
		t.Fatalf("non-failed statuses must not enqueue, queue len = %d", len(n.ch))
	}

	n.PublishState(uuid.New(), "failed")
	if len(n.ch) != 1 {
		t.Fatalf("a failed status must enqueue exactly one, queue len = %d", len(n.ch))
	}
}
