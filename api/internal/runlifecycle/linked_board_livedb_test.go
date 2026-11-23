package runlifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5: the lifecycle against a repo linked to the YouTrack fake. The card move
// writes the State, the cache keeps the column, a refused move keeps the marker, and a
// run whose issue is not cached is probed.

// saveStateMapping stores the open and closed States of the repo and the State of
// "Human Review", as the state mapping route would. The fake's bundle holds Open and Fixed
// by default; "In Review" is seeded by the caller.
func saveStateMapping(t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO repo_issue_tracker_states (repo_id, open_state, closed_state) VALUES ($1, 'Open', 'Fixed')`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_issue_tracker_column_states (repo_id, label_name, state_name) VALUES ($1, 'Human Review', 'In Review')`, repo); err != nil {
		t.Fatal(err)
	}
}

type linkedLifecycleLive struct {
	pool   *pgxpool.Pool
	q      *store.Queries
	svc    *forgesvc.Service
	srv    *youtracktest.Server
	repo   uuid.UUID
	user   uuid.UUID
	worker uuid.UUID
}

func newLinkedLifecycleLive(t *testing.T) *linkedLifecycleLive {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	srv := youtracktest.NewServer(t)
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })

	rec := forgetest.NewRecorder(&mrForge{})
	svc := forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return rec, nil })
	itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	svc.SetIssueTrackerDrivers(itSvc)
	sealed, err := svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}
	f := &linkedLifecycleLive{pool: pool, q: q, svc: svc, srv: srv, repo: uuid.New(), user: uuid.New(), worker: uuid.New()}
	conn := uuid.New()
	f.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.user, fmt.Sprintf("lb-%s@e2e", f.user))
	f.exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, f.user, "https://"+uuid.NewString()+".forge.test", sealed)
	yt, err := itSvc.Connect(ctx, f.user, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, 77, $3, 'https://forge.test/g/l', 'main', true, $4, $5, $6, $7, $8, now())`,
		f.repo, conn, "g/lb-"+f.repo.String(), yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	if err := forgesvc.EnsureBoardColumnRows(ctx, q, f.repo); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("lb-worker-" + f.worker.String()))
	f.exec(t, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'lb-worker',$3,'online')`, f.worker, f.user, sum[:])
	srv.SeedState("In Review", false)
	return f
}

func (f *linkedLifecycleLive) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// cacheIssue caches issue n as an open card with no column.
func (f *linkedLifecycleLive) cacheIssue(t *testing.T, n int64) {
	t.Helper()
	f.exec(t, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, $2, 't', 'opened', '[]', 'https://yt.test/i', now(), now())`, f.repo, n)
}

// completedRun inserts a run for issue n that completed with an MR, which stamps its
// move marker, and returns its id. Its origin column is the Open lane.
func (f *linkedLifecycleLive) completedRun(t *testing.T, n int64) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status, auto_approve,
	        origin_column, worker_id, claim_generation)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, 'running', false, '', $6, 1)`, runID, f.user, f.repo, n, "agent/issue-"+runID.String(), f.worker)
	rows, err := f.q.SetRunCompleted(context.Background(), store.SetRunCompletedParams{
		MrIid: pgtype.Int8{Int64: 13, Valid: true}, ID: runID, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true},
	})
	if err != nil || rows != 1 {
		t.Fatalf("SetRunCompleted rows=%d err=%v", rows, err)
	}
	return runID
}

func (f *linkedLifecycleLive) pending(t *testing.T, runID uuid.UUID) (pending bool, boardColumn pgtype.Text) {
	t.Helper()
	var since pgtype.Timestamptz
	if err := f.pool.QueryRow(context.Background(), `SELECT move_pending_since, board_column FROM runs WHERE id = $1`, runID).Scan(&since, &boardColumn); err != nil {
		t.Fatal(err)
	}
	return since.Valid, boardColumn
}

func (f *linkedLifecycleLive) lifecycle() *Lifecycle {
	l := New(f.q, f.svc, "https://uzi.test")
	l.SetIssueSources(f.svc)
	l.grace = 0
	return l
}

func (f *linkedLifecycleLive) writes() []youtracktest.Request {
	var out []youtracktest.Request
	for _, r := range f.srv.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// A run whose issue is not cached is probed in the linked project: a moved-away issue and
// an issue without the eligibility tag clear the marker; an eligible one keeps it (the next
// sync caches it and reconciliation moves the card).
func TestLinkedRunWithAnUncachedIssueIsProbedLiveDB(t *testing.T) {
	f := newLinkedLifecycleLive(t)
	ctx := context.Background()

	moved := f.srv.SeedIssue("moved away", "b", youtracktest.EligibilityTagID)
	f.srv.MoveIssue(moved, youtracktest.OtherProjectID)
	untagged := f.srv.SeedIssue("never eligible", "b")
	eligible := f.srv.SeedIssue("not cached yet", "b", youtracktest.EligibilityTagID)

	cases := []struct {
		name      string
		issue     int64
		wantClear bool
	}{
		{"moved away: cleared", moved, true},
		{"found but not eligible: cleared", untagged, true},
		{"found and eligible: kept", eligible, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := f.completedRun(t, tc.issue)
			f.srv.ResetRequests()

			f.lifecycle().notifyOnce(ctx, runID, "completed")

			pending, col := f.pending(t, runID)
			if pending == tc.wantClear {
				t.Fatalf("marker pending = %v, want %v", pending, !tc.wantClear)
			}
			if col.Valid {
				t.Fatalf("board_column = %q for a move that did not happen", col.String)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("an uncached issue must never be written: %+v", w)
			}
		})
	}
}

// A move the issue tracker refuses (the column has no State mapped) keeps the marker and
// sends no write. Once the owner saves the mapping, reconciliation moves the card, records
// the column and clears the marker.
func TestLinkedRefusedMoveKeepsTheMarkerUntilTheMappingIsSavedLiveDB(t *testing.T) {
	f := newLinkedLifecycleLive(t)
	ctx := context.Background()
	n := f.srv.SeedIssue("a card", "b", youtracktest.EligibilityTagID)
	f.cacheIssue(t, n)
	runID := f.completedRun(t, n)
	f.srv.ResetRequests()

	l := f.lifecycle()
	l.notifyOnce(ctx, runID, "completed")
	if pending, _ := f.pending(t, runID); !pending {
		t.Fatal("a refused move must leave move_pending_since")
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a refused move sent a write: %+v", w)
	}
	l.Reconcile(ctx)
	if pending, _ := f.pending(t, runID); !pending {
		t.Fatal("a reconcile with the mapping still missing must keep the marker")
	}

	saveStateMapping(t, f.pool, f.repo)
	l.Reconcile(ctx)

	if pending, col := f.pending(t, runID); pending || col.String != board.ColumnHumanReview {
		t.Fatalf("after the mapping: pending=%v board_column=%+v, want cleared and Human Review", pending, col)
	}
	if got := f.srv.IssueState(n); got != "In Review" {
		t.Fatalf("issue State = %q, want In Review", got)
	}
	var cached pgtype.Text
	if err := f.pool.QueryRow(ctx, `SELECT issue_tracker_column FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, n).Scan(&cached); err != nil || cached.String != board.ColumnHumanReview {
		t.Fatalf("cached column = %+v (%v), want Human Review", cached, err)
	}
}

// A person resolved the issue in YouTrack and the cache still shows it open: a run
// transition writes no State and clears the marker, so the resolution stands.
func TestLinkedMoveOfAnIssueResolvedInYouTrackWritesNothingLiveDB(t *testing.T) {
	f := newLinkedLifecycleLive(t)
	ctx := context.Background()
	saveStateMapping(t, f.pool, f.repo)
	n := f.srv.SeedIssue("a card", "b", youtracktest.EligibilityTagID)
	f.cacheIssue(t, n)
	runID := f.completedRun(t, n)
	tr, err := f.svc.IssueTrackerForRepo(ctx, f.user, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SetIssueState(ctx, 77, n, forge.StateClosed); err != nil {
		t.Fatalf("resolve the issue in YouTrack: %v", err)
	}
	resolved := f.srv.IssueState(n)
	f.srv.ResetRequests()

	f.lifecycle().notifyOnce(ctx, runID, "completed")

	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a write reached the resolved issue: %+v", w)
	}
	if got := f.srv.IssueState(n); got != resolved {
		t.Fatalf("issue State = %q, want the resolved %q untouched", got, resolved)
	}
	if pending, col := f.pending(t, runID); pending || col.Valid {
		t.Fatalf("pending=%v board_column=%+v, want the marker cleared and nothing recorded", pending, col)
	}
}

// A moved-away cached issue is skipped: the write is attempted (the mapping is saved) and
// answers not-found, the marker is cleared, and the cache is untouched.
func TestLinkedMovedAwayCachedIssueClearsTheMarkerLiveDB(t *testing.T) {
	f := newLinkedLifecycleLive(t)
	saveStateMapping(t, f.pool, f.repo)
	n := f.srv.SeedIssue("a card", "b", youtracktest.EligibilityTagID)
	f.cacheIssue(t, n)
	runID := f.completedRun(t, n)
	f.srv.MoveIssue(n, youtracktest.OtherProjectID)

	f.lifecycle().notifyOnce(context.Background(), runID, "completed")

	if pending, col := f.pending(t, runID); pending || col.Valid {
		t.Fatalf("pending=%v board_column=%+v, want the marker cleared and nothing recorded", pending, col)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a write reached the moved-away issue: %+v", w)
	}
	var cached pgtype.Text
	if err := f.pool.QueryRow(context.Background(), `SELECT issue_tracker_column FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, n).Scan(&cached); err != nil || cached.Valid {
		t.Fatalf("cached column = %+v (%v), want NULL", cached, err)
	}
}

// PRD #4173 M5 U6: the autopilot terminal comment reaches the YouTrack issue through the
// resolver, the guard and the driver. A moved-away issue is logged and the lifecycle
// carries on (the move marker is still cleared).
func autopilotCompletedRun(t *testing.T, f *linkedLifecycleLive, n int64) uuid.UUID {
	t.Helper()
	f.cacheIssue(t, n)
	runID := f.completedRun(t, n)
	f.exec(t, `UPDATE runs SET auto_approve = true WHERE id = $1`, runID)
	return runID
}

func TestLinkedTerminalCommentLandsOnTheYouTrackIssueLiveDB(t *testing.T) {
	f := newLinkedLifecycleLive(t)
	saveStateMapping(t, f.pool, f.repo)
	n := f.srv.SeedIssue("a card", "b", youtracktest.EligibilityTagID)
	runID := autopilotCompletedRun(t, f, n)

	f.lifecycle().notifyOnce(context.Background(), runID, "completed")

	tr, err := f.svc.IssueTrackerForRepo(context.Background(), f.user, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := tr.ListIssueComments(context.Background(), 77, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Body, "**Autopilot opened a merge request.**") || !strings.Contains(cs[0].Body, "!13") {
		t.Fatalf("YouTrack comments = %+v, want the terminal comment", cs)
	}
}

func TestLinkedTerminalCommentOnAMovedAwayIssueIsLoggedAndTheLifecycleContinuesLiveDB(t *testing.T) {
	f := newLinkedLifecycleLive(t)
	saveStateMapping(t, f.pool, f.repo)
	n := f.srv.SeedIssue("a card", "b", youtracktest.EligibilityTagID)
	runID := autopilotCompletedRun(t, f, n)
	f.srv.MoveIssue(n, youtracktest.OtherProjectID)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f.lifecycle().notifyOnce(context.Background(), runID, "completed")

	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a write reached the moved-away issue: %+v", w)
	}
	if !strings.Contains(logs.String(), "autopilot terminal comment skipped, the linked issue is gone or moved") {
		t.Fatalf("the skipped comment was not logged: %s", logs.String())
	}
	if pending, _ := f.pending(t, runID); pending {
		t.Fatal("the move marker was left set")
	}
}
