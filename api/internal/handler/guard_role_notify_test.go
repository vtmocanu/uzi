package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/hub"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestBuildGuardRoleExcludedNotification pins the producer shape (PRD #319 M3): the
// notification carries the new kind, a run anchor, a payload whose title/body/roles name
// the excluded guard role, and a non-nil Slack render with a matching title/body and a
// deep link built from the operator base URL.
func TestBuildGuardRoleExcludedNotification(t *testing.T) {
	owner, runID := uuid.New(), uuid.New()
	n := buildGuardRoleExcludedNotification("https://uzi.example.com/", owner, runID, []string{"spec-keeper"})

	if n.UserID != owner {
		t.Errorf("notification owner = %v, want %v (owner-only)", n.UserID, owner)
	}
	if n.Kind != guardRoleExcludedNotificationKind {
		t.Errorf("kind = %q, want %q", n.Kind, guardRoleExcludedNotificationKind)
	}
	if n.Kind != "guard_role_excluded" {
		t.Errorf("kind literal = %q, want guard_role_excluded", n.Kind)
	}
	if n.RunID == nil || *n.RunID != runID {
		t.Errorf("run anchor = %v, want %v", n.RunID, runID)
	}
	if n.ReviewID != nil {
		t.Errorf("review anchor = %v, want nil (not a review notification)", n.ReviewID)
	}

	payload, ok := n.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", n.Payload)
	}
	title, _ := payload["title"].(string)
	if title != "Guard role excluded from run" {
		t.Errorf("payload title = %q, want the guard-excluded title", title)
	}
	body, _ := payload["body"].(string)
	if !strings.Contains(body, "spec-keeper") {
		t.Errorf("payload body = %q, want it to name the excluded role", body)
	}
	roles, _ := payload["roles"].([]string)
	if len(roles) != 1 || roles[0] != "spec-keeper" {
		t.Errorf("payload roles = %v, want [spec-keeper]", roles)
	}

	if n.Slack == nil {
		t.Fatal("slack render must be non-nil (best-effort DM)")
	}
	if n.Slack.Title != title {
		t.Errorf("slack title = %q, want %q", n.Slack.Title, title)
	}
	if n.Slack.Body != body {
		t.Errorf("slack body = %q, want the payload body %q", n.Slack.Body, body)
	}
	wantLink := "https://uzi.example.com/runs/" + runID.String()
	if n.Slack.Link != wantLink {
		t.Errorf("slack link = %q, want %q", n.Slack.Link, wantLink)
	}

	// The payload path must serialize cleanly.
	if _, err := json.Marshal(n); err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
}

// TestBuildGuardRoleExcludedNotificationNoBaseURL: an unset/empty public base URL yields
// no deep link (mirrors TestBuildReviewNotificationNoBaseURL), so the notification simply
// carries no link.
func TestBuildGuardRoleExcludedNotificationNoBaseURL(t *testing.T) {
	n := buildGuardRoleExcludedNotification("  ", uuid.New(), uuid.New(), []string{"spec-keeper"})
	if n.Slack == nil || n.Slack.Link != "" {
		t.Errorf("slack link = %v, want empty (no base URL)", n.Slack)
	}
}

// countingNotifStore is a notifysvc.Store that counts InsertNotification calls and
// records the last row's kind, so the emit DECISION is observable without a database.
type countingNotifStore struct {
	inserts  int
	lastKind string
}

func (s *countingNotifStore) InsertNotification(_ context.Context, arg store.InsertNotificationParams) (store.Notification, error) {
	s.inserts++
	s.lastKind = arg.Kind
	return store.Notification{Kind: arg.Kind, UserID: arg.UserID}, nil
}

func (s *countingNotifStore) PruneNotificationsForUser(context.Context, store.PruneNotificationsForUserParams) (int64, error) {
	return 0, nil
}

func (s *countingNotifStore) GetRunByID(context.Context, uuid.UUID) (store.Run, error) {
	return store.Run{}, nil
}

// TestGuardRoleExcludedEmitDecision pins the producer notifyGuardRoleExcluded (PRD #319
// M3): a guard-role exclusion emits exactly one Notify with the new kind, and a nil
// notifier is a no-op. The empty-skip decision is covered end-to-end through the real
// handler gate by TestCreateRunInputApproveExcludingGuardRoleNotifies.
func TestGuardRoleExcludedEmitDecision(t *testing.T) {
	t.Run("guard excluded emits exactly one row with the new kind", func(t *testing.T) {
		ns := &countingNotifStore{}
		h := &Handler{}
		h.SetNotifier(notifysvc.New(ns, nil, 0, nil))
		h.notifyGuardRoleExcluded(context.Background(), uuid.New(), uuid.New(), []string{"spec-keeper"})
		if ns.inserts != 1 {
			t.Fatalf("inserts = %d, want exactly 1", ns.inserts)
		}
		if ns.lastKind != guardRoleExcludedNotificationKind {
			t.Fatalf("kind = %q, want %q", ns.lastKind, guardRoleExcludedNotificationKind)
		}
	})

	t.Run("no notifier wired is a no-op", func(t *testing.T) {
		h := &Handler{} // notifier nil
		// Must not panic; nothing to assert beyond the nil-guard surviving.
		h.notifyGuardRoleExcluded(context.Background(), uuid.New(), uuid.New(), []string{"spec-keeper"})
	})
}

// TestCreateRunInputApproveExcludingGuardRoleNotifies drives the REAL CreateRunInput
// approve path (PRD #319 M3) and asserts the production glue in workers.go — the
// `if len(res.ExcludedGuardRoles) > 0 { h.notifyGuardRoleExcluded(...) }` block — fires
// exactly when the approved selection explicitly excludes a guard role, and stays silent
// otherwise. This covers the wiring (right path, right run id, right owner) and the
// empty-case skip through production, which the emit-decision unit test cannot.
func TestCreateRunInputApproveExcludingGuardRoleNotifies(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()

	// newH mirrors newRunsHandler (a workersvc over the runsStore fake + a hub) but also
	// wires a notifier over a countingNotifStore, so the approve-path emit is observable.
	newH := func(t *testing.T, ns *countingNotifStore) *Handler {
		t.Helper()
		box, err := secretbox.New(make([]byte, secretbox.KeySize))
		if err != nil {
			t.Fatalf("new box: %v", err)
		}
		st := &runsStore{
			ownerID: owner.ID,
			run:     store.Run{ID: runID, UserID: owner.ID, Status: "awaiting_approval"},
			// The own-source roster (lead stripped) must keep at least one survivor after the
			// exclusion, so validateSelection's "at least one subagent must remain" passes.
			claimTemplates: []store.AgentTemplate{
				{Name: "lead"},
				{Name: "spec-keeper"},
				{Name: "coder"},
			},
		}
		h := &Handler{wsvc: workersvc.New(st, box, workersvc.Params{}), hub: hub.New()}
		h.SetNotifier(notifysvc.New(ns, nil, 0, nil))
		return h
	}

	t.Run("guard role excluded fires exactly one notify", func(t *testing.T) {
		ns := &countingNotifStore{}
		h := newH(t, ns)
		rec := httptest.NewRecorder()
		h.CreateRunInput(rec, inputReq(owner, runID,
			`{"kind":"approve_plan","selection":{"source":"own","exclusions":["spec-keeper"]}}`))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("approve = %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if ns.inserts != 1 {
			t.Fatalf("inserts = %d, want exactly 1 (guard role excluded)", ns.inserts)
		}
		if ns.lastKind != guardRoleExcludedNotificationKind {
			t.Fatalf("kind = %q, want %q", ns.lastKind, guardRoleExcludedNotificationKind)
		}
	})

	t.Run("non-guard exclusion emits nothing", func(t *testing.T) {
		ns := &countingNotifStore{}
		h := newH(t, ns)
		rec := httptest.NewRecorder()
		h.CreateRunInput(rec, inputReq(owner, runID,
			`{"kind":"approve_plan","selection":{"source":"own","exclusions":["coder"]}}`))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("approve = %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if ns.inserts != 0 {
			t.Fatalf("inserts = %d, want 0 (no guard role excluded)", ns.inserts)
		}
	})
}
