package store_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type retentionRecordingSlacker struct {
	ids     []uuid.UUID
	renders []notifysvc.SlackRender
}

func (s *retentionRecordingSlacker) PublishNotification(_ uuid.UUID, r notifysvc.SlackRender) {
	s.ids = append(s.ids, r.DeliveryID)
	s.renders = append(s.renders, r)
}

type retentionPoster struct {
	slacksvc.Poster
	posts chan struct{}
}

func (p *retentionPoster) OpenDM(context.Context, string) (string, error) { return "D1", nil }
func (p *retentionPoster) PostBlocks(context.Context, string, string, string, []slack.Block) (string, error) {
	p.posts <- struct{}{}
	return "1.0", nil
}

// Every stamp executes the real query; the channel observes all settlements,
// including rows which the subsequent retention call deletes.
type retentionDeliveryStore struct {
	*store.Queries
	stamps chan uuid.UUID
}

func (s *retentionDeliveryStore) MarkNotificationSlackDelivered(ctx context.Context, id uuid.UUID) error {
	if err := s.Queries.MarkNotificationSlackDelivered(ctx, id); err != nil {
		return err
	}
	s.stamps <- id
	return nil
}

type retentionFixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *store.Queries
	user     uuid.UUID
	other    uuid.UUID
	ids      []uuid.UUID
	otherIDs []uuid.UUID
	recorder *retentionRecordingSlacker
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &retentionFixture{ctx: ctx, pool: pool, q: store.New(pool), user: uuid.New(), other: uuid.New(), recorder: &retentionRecordingSlacker{}}
	// Delete only this test's users (notifications cascade); never clear neighbours.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = ANY($1::uuid[])`, []uuid.UUID{f.user, f.other}); err != nil {
			t.Errorf("fixture cleanup: %v", err)
		}
	})
	for _, u := range []uuid.UUID{f.user, f.other} {
		mustExec(ctx, t, pool, `INSERT INTO users (id,email,password_hash,slack_resolved_id,slack_link_confirmed_at)
   VALUES ($1,$2,'x',$3,now())`, u, fmt.Sprintf("retention-%s@e2e", u), "U"+u.String())
	}
	var anchor time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	svc := notifysvc.New(f.q, f.recorder, 1, nil)
	kinds := []string{"ci_autofix_halted", "mr_rework_halted", "ci_autofix_halted"}
	for _, u := range []uuid.UUID{f.user, f.other} {
		var ids []uuid.UUID
		for i, kind := range kinds {
			n, err := svc.Notify(ctx, notifysvc.Notification{
				UserID: u, Kind: kind, Payload: map[string]string{"reason": "halted"}, DurableSlack: true,
				Slack: &notifysvc.SlackRender{Title: fmt.Sprintf("halt %d", i)},
			})
			if err != nil {
				t.Fatalf("Notify: %v", err)
			}
			ids = append(ids, n.ID)
			// Distinct timestamps make keep=1's strict boundary unambiguous.
			mustExec(ctx, t, pool, `UPDATE notifications SET created_at=$2 WHERE id=$1`, n.ID, anchor.Add(-24*time.Hour+time.Duration(i)*time.Second))
		}
		if u == f.user {
			f.ids = ids
		} else {
			f.otherIDs = ids
		}
	}
	f.assertIDs(t, f.user, f.ids) // insertion pruning must spare all pending rows
	f.assertIDs(t, f.other, f.otherIDs)
	return f
}

func (f *retentionFixture) readIDs(t *testing.T, u uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT id FROM notifications WHERE user_id=$1 ORDER BY created_at,id`, u)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}
func (f *retentionFixture) assertIDs(t *testing.T, u uuid.UUID, want []uuid.UUID) {
	t.Helper()
	if got := f.readIDs(t, u); !reflect.DeepEqual(got, want) {
		t.Fatalf("user %s retention IDs = %v, want %v", u, got, want)
	}
}

// The production claim is global and limited. Hold foreign rows on a different
// connection so SKIP LOCKED isolates this fixture without deleting other fixtures.
// The context bounds lock acquisition and cleanup releases the transaction.
func (f *retentionFixture) isolateClaims(t *testing.T) {
	t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanupCtx); err != nil {
			t.Errorf("isolation rollback: %v", err)
		}
	})
	rows, err := tx.Query(f.ctx, `SELECT id FROM notifications WHERE user_id <> $1 FOR UPDATE`, f.user)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
func (f *retentionFixture) stale(t *testing.T, attempts int32) {
	t.Helper()
	mustExec(f.ctx, t, f.pool, `UPDATE notifications SET slack_attempts=$2,
  slack_attempted_at=now()-interval '1 day' WHERE user_id=$1`, f.user, attempts)
}

func TestNotificationDeliveryRetentionSettlementLiveDB(t *testing.T) {
	f := newRetentionFixture(t)
	f.isolateClaims(t)
	ds := &retentionDeliveryStore{Queries: f.q, stamps: make(chan uuid.UUID, 3)}
	poster := &retentionPoster{posts: make(chan struct{}, 3)}
	n := slacksvc.NewNotifier(ds, poster, func(context.Context) (string, error) { return "https://uzi.example", nil }, nil, slacksvc.WithNotificationUserCap(1))
	runCtx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { defer close(done); n.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	for i, id := range f.ids {
		r := f.recorder.renders[i]
		if r.DeliveryID != id {
			t.Fatalf("recorded delivery ID = %s, want %s", r.DeliveryID, id)
		}
		n.PublishNotification(f.user, r)
	}
	for _, id := range f.ids {
		select {
		case got := <-ds.stamps:
			if got != id {
				t.Fatalf("settlement ID = %s, want %s", got, id)
			}
		case <-f.ctx.Done():
			t.Fatal("timed out awaiting all settlements")
		}
	}
	// A stamp signal precedes pruning; wait for the retained IDs AND the last stamp.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	// Fail on retained rows before the fixture's database context expires.
	retentionTimeout := time.NewTimer(5 * time.Second)
	defer retentionTimeout.Stop()
	for {
		ids := f.readIDs(t, f.user)
		var delivered bool
		if err := f.pool.QueryRow(f.ctx, `SELECT slack_delivered_at IS NOT NULL FROM notifications WHERE id=$1`, f.ids[2]).Scan(&delivered); err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(ids, f.ids[2:]) && delivered {
			break
		}
		select {
		case <-ticker.C:
		case <-retentionTimeout.C:
			t.Fatalf("retention did not converge: %v", ids)
		case <-f.ctx.Done():
			t.Fatalf("fixture expired before retention converged: %v", ids)
		}
	}
	cancel()
	<-done
	if len(poster.posts) != 3 {
		t.Fatalf("posts = %d, want 3", len(poster.posts))
	}
	f.assertIDs(t, f.other, f.otherIDs)
	f.stale(t, 1) // delivery, rather than a fresh attempted_at, must prevent retry
	if got, err := notifysvc.NewRedeliverer(f.q, f.recorder, nil, notifysvc.WithRedeliveryUserCap(1)).Pass(f.ctx); got != 0 || err != nil {
		t.Fatalf("settled rows retried: %d, %v", got, err)
	}
}

func TestNotificationExhaustionRetentionSettlementLiveDB(t *testing.T) {
	testRetentionExhaustion(t, false)
}
func TestNotificationCorruptExhaustionRetentionSettlementLiveDB(t *testing.T) {
	testRetentionExhaustion(t, true)
}
func testRetentionExhaustion(t *testing.T, corrupt bool) {
	t.Helper()
	f := newRetentionFixture(t)
	if corrupt {
		// Valid jsonb, but incompatible with durableRender's string title.
		mustExec(f.ctx, t, f.pool, `UPDATE notifications SET slack_render='{"title":42}'::jsonb WHERE user_id=$1`, f.user)
	}
	f.isolateClaims(t)
	recorder := &retentionRecordingSlacker{} // records only; never stamps delivery
	red := notifysvc.NewRedeliverer(f.q, recorder, nil, notifysvc.WithRedeliveryUserCap(1))
	f.stale(t, notifysvc.MaxSlackAttempts-2)
	want := int64(3)
	if corrupt {
		want = 0
	}
	if got, err := red.Pass(f.ctx); got != want || err != nil {
		t.Fatalf("pre-final Pass = %d, %v, want %d", got, err, want)
	}
	f.assertIDs(t, f.user, f.ids) // no cleanup while all rows remain pending
	f.assertIDs(t, f.other, f.otherIDs)
	recorder.ids = nil
	f.stale(t, notifysvc.MaxSlackAttempts-1)
	if got, err := red.Pass(f.ctx); got != want || err != nil {
		t.Fatalf("final Pass = %d, %v, want %d", got, err, want)
	}
	wantIDs := f.ids
	if corrupt {
		wantIDs = nil
	}
	// UPDATE RETURNING has no ordering guarantee; compare the actual IDs as a set.
	gotIDs := map[uuid.UUID]bool{}
	for _, id := range recorder.ids {
		gotIDs[id] = true
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Fatalf("claimed final row %s was deleted without publishing", id)
		}
	}
	if len(recorder.ids) != len(wantIDs) {
		t.Fatalf("published IDs = %v, want set %v", recorder.ids, wantIDs)
	}
	f.assertIDs(t, f.user, f.ids[2:])
	f.assertIDs(t, f.other, f.otherIDs)
	var attempts int32
	var delivered bool
	if err := f.pool.QueryRow(f.ctx, `SELECT slack_attempts,slack_delivered_at IS NOT NULL FROM notifications WHERE id=$1`, f.ids[2]).Scan(&attempts, &delivered); err != nil {
		t.Fatal(err)
	}
	if attempts != notifysvc.MaxSlackAttempts || delivered {
		t.Fatalf("exhausted row attempts=%d delivered=%v", attempts, delivered)
	}
	// Even after the retry window, exhaustion forbids another publish.
	f.stale(t, notifysvc.MaxSlackAttempts)
	if got, err := red.Pass(f.ctx); got != 0 || err != nil {
		t.Fatalf("exhausted rows retried: %d, %v", got, err)
	}
}
