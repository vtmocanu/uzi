package notifysvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// coalescingStore models the notifications table just enough to exercise the PRD #333 D6
// per-run coalescing path: it holds the rows Insert writes, resolves the coalescible
// unread row by (user, run, kind), and rewrites a row's payload on update. That lets a
// table test assert the row COUNT stays 1 across a run's findings while the payload count
// climbs — the coalescing invariant — without a database.
type coalescingStore struct {
	rows        []store.Notification
	updateCalls int
}

func (s *coalescingStore) InsertNotification(_ context.Context, arg store.InsertNotificationParams) (store.Notification, error) {
	row := store.Notification{
		ID:      uuid.New(),
		UserID:  arg.UserID,
		Kind:    arg.Kind,
		Payload: arg.Payload,
		RunID:   arg.RunID,
		// read_at is left NULL (pgtype zero) so the row counts as unread and is coalescible.
	}
	s.rows = append(s.rows, row)
	return row, nil
}

func (s *coalescingStore) PruneNotificationsForUser(context.Context, store.PruneNotificationsForUserParams) (int64, error) {
	return 0, nil
}

func (s *coalescingStore) GetRunByID(context.Context, uuid.UUID) (store.Run, error) {
	return store.Run{}, nil
}

func (s *coalescingStore) FindUnreadNotificationForRunKind(_ context.Context, arg store.FindUnreadNotificationForRunKindParams) (store.Notification, error) {
	// Newest-first, like the query's ORDER BY created_at DESC.
	for i := len(s.rows) - 1; i >= 0; i-- {
		r := s.rows[i]
		if r.UserID == arg.UserID && r.Kind == arg.Kind && r.ReadAt.Valid == false &&
			r.RunID.Valid && uuid.UUID(r.RunID.Bytes) == arg.RunID {
			return r, nil
		}
	}
	return store.Notification{}, pgx.ErrNoRows
}

func (s *coalescingStore) UpdateNotificationPayload(_ context.Context, arg store.UpdateNotificationPayloadParams) (store.Notification, error) {
	s.updateCalls++
	for i := range s.rows {
		if s.rows[i].ID == arg.ID && s.rows[i].UserID == arg.UserID {
			s.rows[i].Payload = arg.Payload
			return s.rows[i], nil
		}
	}
	return store.Notification{}, pgx.ErrNoRows
}

func decodeFindingPayload(t *testing.T, raw []byte) IncidentalFindingPayload {
	t.Helper()
	var p IncidentalFindingPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload not valid IncidentalFindingPayload json: %v (%s)", err, raw)
	}
	return p
}

func TestNotifyIncidentalFindingCoalescesPerRun(t *testing.T) {
	cs := &coalescingStore{}
	slk := &fakeSlacker{}
	svc := New(cs, slk, 0, nil)

	user, run, repo := uuid.New(), uuid.New(), uuid.New()
	f1, f2 := uuid.New(), uuid.New()

	// (1) The run's FIRST finding: one inbox row inserted (unread → counts toward the
	//     bell) and exactly one Slack DM with a populated SlackRender.
	if err := svc.NotifyIncidentalFinding(context.Background(), IncidentalFindingNotifyInput{
		UserID: user, RunID: run, RepoID: repo, RepoPath: "acme/widgets", FindingID: f1,
		Link: "https://uzi.example/runs/" + run.String(),
	}); err != nil {
		t.Fatalf("first NotifyIncidentalFinding: %v", err)
	}
	if len(cs.rows) != 1 {
		t.Fatalf("after the first finding, rows=%d want exactly 1", len(cs.rows))
	}
	if cs.rows[0].Kind != KindIncidentalFinding {
		t.Errorf("row kind = %q, want %q", cs.rows[0].Kind, KindIncidentalFinding)
	}
	if cs.rows[0].ReadAt.Valid {
		t.Error("the coalesced row must be unread so it counts toward the bell badge")
	}
	if slk.calls != 1 {
		t.Fatalf("Slack fired %d times on the first finding, want exactly 1", slk.calls)
	}
	if slk.lastUserID != user {
		t.Errorf("Slack DM went to %s, want the run owner %s", slk.lastUserID, user)
	}
	if slk.lastRender.Title == "" || slk.lastRender.Emoji == "" || slk.lastRender.Body != "acme/widgets" ||
		slk.lastRender.Link != "https://uzi.example/runs/"+run.String() {
		t.Errorf("first-finding SlackRender is not fully populated: %+v", slk.lastRender)
	}
	p := decodeFindingPayload(t, cs.rows[0].Payload)
	if p.Count != 1 || len(p.FindingIDs) != 1 || p.FindingIDs[0] != f1 || p.RunID != run || p.RepoID != repo || p.RepoPath != "acme/widgets" {
		t.Errorf("first payload = %+v, want count=1 + the first finding id and run/repo anchors", p)
	}

	// (2) A SECOND finding on the SAME run: the existing unread row's payload bumps to
	//     count=2 (the row COUNT stays 1) and NO new Slack DM fires.
	if err := svc.NotifyIncidentalFinding(context.Background(), IncidentalFindingNotifyInput{
		UserID: user, RunID: run, RepoID: repo, RepoPath: "acme/widgets", FindingID: f2,
		Link: "https://uzi.example/runs/" + run.String(),
	}); err != nil {
		t.Fatalf("second NotifyIncidentalFinding: %v", err)
	}
	if len(cs.rows) != 1 {
		t.Fatalf("after the second finding, rows=%d want still 1 (coalesced)", len(cs.rows))
	}
	if cs.updateCalls != 1 {
		t.Fatalf("the second finding must UPDATE the existing row exactly once, got %d", cs.updateCalls)
	}
	if slk.calls != 1 {
		t.Fatalf("Slack fired %d times total, want 1 (no re-fire on the coalesced finding)", slk.calls)
	}
	p = decodeFindingPayload(t, cs.rows[0].Payload)
	if p.Count != 2 || len(p.FindingIDs) != 2 || p.FindingIDs[1] != f2 {
		t.Errorf("coalesced payload = %+v, want count=2 and both finding ids", p)
	}
}

func TestNotifyIncidentalFindingNilSlackerIsInboxOnly(t *testing.T) {
	// With no Slacker wired the Slack seam is a no-op — never an error — and the inbox row
	// still persists (the durable half). Mirrors TestNotifyNilSlackerIsInboxOnly.
	cs := &coalescingStore{}
	svc := New(cs, nil, 0, nil)
	if err := svc.NotifyIncidentalFinding(context.Background(), IncidentalFindingNotifyInput{
		UserID: uuid.New(), RunID: uuid.New(), RepoID: uuid.New(), RepoPath: "acme/widgets", FindingID: uuid.New(),
	}); err != nil {
		t.Fatalf("NotifyIncidentalFinding with a nil slacker must not error: %v", err)
	}
	if len(cs.rows) != 1 {
		t.Fatalf("the inbox row must still persist with no Slacker wired, rows=%d", len(cs.rows))
	}
}

func TestNotifyIncidentalFindingCapsFindingIDs(t *testing.T) {
	// The finding_ids slice is capped so a run cannot grow one row's jsonb without bound,
	// while count keeps climbing (it is the badge number). The per-run capture cap is far
	// below maxCoalescedFindingIDs, so this only guards the pathological path.
	cs := &coalescingStore{}
	svc := New(cs, nil, 0, nil)
	user, run, repo := uuid.New(), uuid.New(), uuid.New()
	total := maxCoalescedFindingIDs + 5
	for i := 0; i < total; i++ {
		if err := svc.NotifyIncidentalFinding(context.Background(), IncidentalFindingNotifyInput{
			UserID: user, RunID: run, RepoID: repo, RepoPath: "acme/widgets", FindingID: uuid.New(),
		}); err != nil {
			t.Fatalf("finding %d: %v", i, err)
		}
	}
	if len(cs.rows) != 1 {
		t.Fatalf("all findings on one run coalesce to a single row, got %d", len(cs.rows))
	}
	p := decodeFindingPayload(t, cs.rows[0].Payload)
	if p.Count != total {
		t.Errorf("payload count = %d, want the full %d (count is uncapped)", p.Count, total)
	}
	if len(p.FindingIDs) != maxCoalescedFindingIDs {
		t.Errorf("finding_ids len = %d, want capped at %d", len(p.FindingIDs), maxCoalescedFindingIDs)
	}
}
