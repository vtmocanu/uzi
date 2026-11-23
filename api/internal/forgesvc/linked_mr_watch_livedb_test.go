package forgesvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5: the board-free MR-state recorder moves a linked repo's card on an MR
// close/reopen edge through the issue tracker, for the issue's latest run only.

type linkedMRWatch struct {
	pool  *pgxpool.Pool
	q     *store.Queries
	svc   *Service
	srv   *youtracktest.Server
	tr    forge.IssueTracker
	repo  uuid.UUID
	user  uuid.UUID
	forge uuid.UUID // the forge connection id
}

// newLinkedMRWatch starts a linked repo over the YouTrack fake. columnStates maps the
// board columns to State names; the bundle holds the Open and Fixed defaults plus
// "In Progress" and "In Review".
func newLinkedMRWatch(t *testing.T, columnStates map[string]string) *linkedMRWatch {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	srv := youtracktest.NewServer(t)
	srv.SeedState("In Progress", false)
	srv.SeedState("In Review", false)
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })
	d, err := issuetracker.New(issuetracker.KindYouTrack, issuetracker.Options{
		BaseURL:        srv.URL,
		Token:          srv.Token,
		Project:        issuetracker.ProjectRef{ID: youtracktest.ProjectID, ShortName: youtracktest.ProjectShortName},
		ProjectID:      77,
		EligibilityTag: issuetracker.TagRef{ID: youtracktest.EligibilityTagID, Name: youtracktest.EligibilityTagName},
		OpenState:      "Open",
		ClosedState:    "Fixed",
		ColumnStates:   columnStates,
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &linkedMRWatch{
		pool: pool, q: store.New(pool), svc: New(store.New(pool), nil, time.Second, nil), srv: srv,
		tr: &issueWriteGuard{inner: d}, repo: uuid.New(), user: uuid.New(), forge: uuid.New(),
	}
	itConn := uuid.New()
	f.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.user, fmt.Sprintf("lmw-%s@e2e", f.user))
	f.exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, f.forge, f.user, "https://"+uuid.NewString()+".forge.e2e", []byte{1})
	f.exec(t, `INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
	      VALUES ($1, $2, 'youtrack', $3, '1-1', 'bot', $4)`, itConn, f.user, "https://"+uuid.NewString()+".yt.e2e", []byte{1})
	f.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, 77, $3, 'https://forge.e2e/g/l', 'main', true, $4, '0-1', 'UZI', '5-1', 'uzi', now())`,
		f.repo, f.forge, "g/lmw-"+f.repo.String(), itConn)
	if err := EnsureBoardColumnRows(ctx, f.q, f.repo); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *linkedMRWatch) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// card seeds a YouTrack issue sitting in column (its State is written to match) and
// caches it with that column. An empty column caches the Open lane.
func (f *linkedMRWatch) card(t *testing.T, column string) int64 {
	t.Helper()
	n := f.srv.SeedIssue("a card", "b", youtracktest.EligibilityTagID)
	if column != "" {
		if err := f.tr.MoveToColumn(context.Background(), 77, n, forge.ColumnMove{Column: column}); err != nil {
			t.Fatalf("seed the State: %v", err)
		}
	}
	f.cache(t, n, column)
	return n
}

func (f *linkedMRWatch) cache(t *testing.T, n int64, column string) {
	t.Helper()
	f.exec(t, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at, issue_tracker_column)
	      VALUES ($1, $2, 't', 'opened', '[]', 'https://yt.e2e/i', now(), now(), $3)`, f.repo, n, column)
}

// run inserts a completed issue-lane run for issue n with an MR in mrState.
func (f *linkedMRWatch) run(t *testing.T, n, mr int64, mrState string, age time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, created_at)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, 'completed', now() - $8::interval)`,
		id, f.user, f.repo, n, "agent/issue-"+id.String(), mr, mrState, fmt.Sprintf("%d seconds", int(age.Seconds())))
	return id
}

func (f *linkedMRWatch) mrState(t *testing.T, run uuid.UUID) string {
	t.Helper()
	var s pgtype.Text
	if err := f.pool.QueryRow(context.Background(), `SELECT mr_state FROM runs WHERE id = $1`, run).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func (f *linkedMRWatch) column(t *testing.T, n int64) string {
	t.Helper()
	var c pgtype.Text
	if err := f.pool.QueryRow(context.Background(), `SELECT issue_tracker_column FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, n).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c.String
}

func (f *linkedMRWatch) writes() []youtracktest.Request {
	var out []youtracktest.Request
	for _, r := range f.srv.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func (f *linkedMRWatch) sync(t *testing.T, mrs map[int64]string) {
	t.Helper()
	cf := &fakeForge{mrByIID: map[int64]forge.MergeRequest{}}
	for iid, st := range mrs {
		cf.mrByIID[iid] = forgeMR(iid, st)
	}
	if err := f.svc.SyncBoardFreeMRStatesWithIssueSide(context.Background(), f.repo, 77, cf, f.tr); err != nil {
		t.Fatal(err)
	}
}

var humanReviewMapping = map[string]string{board.ColumnHumanReview: "In Review", board.ColumnInProgress: "In Progress"}

// A closed MR moves the cached card Human Review -> In Progress (State written, column
// cached), a reopened one moves it back, and a merge records with no move.
func TestLinkedMRCloseAndReopenMoveTheCardLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.card(t, board.ColumnHumanReview)
	run := f.run(t, n, 13, "opened", time.Minute)
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.srv.IssueState(n); got != "In Progress" {
		t.Fatalf("State after close = %q, want In Progress", got)
	}
	if got := f.column(t, n); got != board.ColumnInProgress {
		t.Fatalf("cached column after close = %q, want In Progress", got)
	}
	if got := f.mrState(t, run); got != forge.MRStateClosed {
		t.Fatalf("mr_state after close = %q, want closed", got)
	}

	f.sync(t, map[int64]string{13: forge.MRStateOpened})
	if got := f.srv.IssueState(n); got != "In Review" {
		t.Fatalf("State after reopen = %q, want In Review", got)
	}
	if got := f.column(t, n); got != board.ColumnHumanReview {
		t.Fatalf("cached column after reopen = %q, want Human Review", got)
	}
	if got := f.mrState(t, run); got != forge.MRStateOpened {
		t.Fatalf("mr_state after reopen = %q, want opened", got)
	}

	f.sync(t, map[int64]string{13: forge.MRStateMerged})
	if got := f.mrState(t, run); got != forge.MRStateMerged {
		t.Fatalf("mr_state after merge = %q, want merged", got)
	}
	if got := f.srv.IssueState(n); got != "In Review" {
		t.Fatalf("a merge changed the State to %q", got)
	}
	if got := f.column(t, n); got != board.ColumnHumanReview {
		t.Fatalf("a merge changed the cached column to %q", got)
	}
}

// A run that a newer run of the same issue superseded records its MR state and never
// moves the card; the latest run's edge does.
func TestLinkedMRSupersededRunRecordsWithoutMovingLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.card(t, board.ColumnHumanReview)
	old := f.run(t, n, 13, "opened", time.Hour)
	latest := f.run(t, n, 14, "opened", time.Minute)
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed, 14: forge.MRStateOpened})
	if got := f.mrState(t, old); got != forge.MRStateClosed {
		t.Fatalf("superseded mr_state = %q, want closed recorded", got)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a superseded run wrote to the issue tracker: %+v", w)
	}
	if got := f.column(t, n); got != board.ColumnHumanReview {
		t.Fatalf("cached column = %q, want it untouched", got)
	}

	f.sync(t, map[int64]string{13: forge.MRStateClosed, 14: forge.MRStateClosed})
	if got := f.mrState(t, latest); got != forge.MRStateClosed {
		t.Fatalf("latest mr_state = %q, want closed", got)
	}
	if got := f.column(t, n); got != board.ColumnInProgress {
		t.Fatalf("cached column = %q, want In Progress after the latest run's close", got)
	}
}

// An issue that left the cache is not deferred: its MR state is recorded, nothing is
// written, and the run stops being a candidate.
func TestLinkedMREvictedIssueRecordsAndIsNotPolledForeverLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.srv.SeedIssue("evicted", "b", youtracktest.EligibilityTagID)
	run := f.run(t, n, 13, "opened", time.Minute)
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.mrState(t, run); got != forge.MRStateClosed {
		t.Fatalf("mr_state = %q, want closed recorded for an uncached issue", got)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes for an uncached issue: %+v", w)
	}
	rows, err := f.q.ListBoardFreeMRStateWatchCandidates(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("candidates = %d, want the evicted issue's run dropped", len(rows))
	}
}

// A target column the issue tracker refuses (no State mapped) records the MR state and
// leaves the card where it is, without a write.
func TestLinkedMRRefusedMoveRecordsAndLeavesTheCardLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, map[string]string{board.ColumnHumanReview: "In Review"}) // In Progress unmapped
	n := f.card(t, board.ColumnHumanReview)
	run := f.run(t, n, 13, "opened", time.Minute)
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.mrState(t, run); got != forge.MRStateClosed {
		t.Fatalf("mr_state = %q, want closed recorded", got)
	}
	if got := f.column(t, n); got != board.ColumnHumanReview {
		t.Fatalf("cached column = %q, want the card left in Human Review", got)
	}
	if got := f.srv.IssueState(n); got != "In Review" {
		t.Fatalf("State = %q, want it untouched", got)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a refused move sent a write: %+v", w)
	}
}

// The same MR sequence over a cached card yields the same edges and the same card
// column through the board lane (unlinked repo, SyncMRStates) as through the linked
// board-free lane.
func TestLinkedMRWatchMatchesTheBoardLaneLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	ctx := context.Background()

	plain := uuid.New()
	f.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 2, $3, 'https://forge.e2e/g/p', 'main', true)`, plain, f.forge, "g/plain-"+plain.String())
	if err := EnsureBoardColumnRows(ctx, f.q, plain); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, 7, 't', 'opened', '["Human Review"]', 'https://forge.e2e/i/7', now(), now())`, plain)
	plainRun := uuid.New()
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
	      VALUES ($1, $2, $3, 'issue', 7, 't', 'd', $4, 13, 'opened', 'completed')`, plainRun, f.user, plain, "agent/issue-7-"+plain.String())

	n := f.card(t, board.ColumnHumanReview)
	linkedRun := f.run(t, n, 13, "opened", time.Minute)

	plainColumn := func() string {
		t.Helper()
		var labels []byte
		if err := f.pool.QueryRow(ctx, `SELECT labels FROM issues WHERE repo_id = $1 AND forge_issue_iid = 7`, plain).Scan(&labels); err != nil {
			t.Fatal(err)
		}
		switch s := string(labels); s {
		case `["Human Review"]`:
			return board.ColumnHumanReview
		case `["In Progress"]`:
			return board.ColumnInProgress
		default:
			return s
		}
	}
	type step struct{ mrState, column string }
	steps := []string{forge.MRStateClosed, forge.MRStateClosed, forge.MRStateOpened, forge.MRStateLocked, forge.MRStateClosed, forge.MRStateMerged}
	for i, observed := range steps {
		cf := &fakeForge{mrByIID: map[int64]forge.MergeRequest{13: forgeMR(13, observed)}}
		if err := f.svc.SyncMRStates(ctx, plain, 2, cf, cf); err != nil {
			t.Fatal(err)
		}
		f.sync(t, map[int64]string{13: observed})
		got := step{f.mrState(t, linkedRun), f.column(t, n)}
		want := step{f.mrState(t, plainRun), plainColumn()}
		if got != want {
			t.Fatalf("step %d (MR %s): linked lane = %+v, board lane = %+v", i, observed, got, want)
		}
	}
}

// ListBoardFreeMRStateWatchCandidates reports issue_iid, kind and is_latest without
// changing which runs it returns or their order.
func TestBoardFreeCandidatesReportLatestnessLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	ctx := context.Background()
	ins := func(kind string, iid any, mr int64, status string, age time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, status, created_at)
		      VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, $8, now() - $9::interval)`,
			id, f.user, f.repo, kind, iid, "b/"+id.String(), mr, status, fmt.Sprintf("%d seconds", int(age.Seconds())))
		return id
	}
	// issue 7: an older run and a newer run, both completed with an MR.
	old7 := ins("issue", 7, 1, "completed", 50*time.Minute)
	new7 := ins("issue", 7, 2, "completed", 40*time.Minute)
	// issue 8: only a newer self_improve run exists beside it: scheduled kinds never supersede.
	run8 := ins("issue", 8, 3, "completed", 30*time.Minute)
	ins("self_improve", 8, 4, "running", 5*time.Minute)
	// issue 9: a newer run that has not completed still supersedes it.
	old9 := ins("issue", 9, 5, "completed", 20*time.Minute)
	ins("issue", 9, 6, "running", 2*time.Minute)
	// issue-less and scheduled runs are never superseded.
	prompt := ins("prompt", nil, 7, "completed", 10*time.Minute)
	selfImprove := ins("self_improve", 7, 8, "completed", 15*time.Minute)
	rework := uuid.New()
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, target_run_id, status, created_at)
	      VALUES ($1, $2, $3, 'mr_rework', 't', 'd', 'b/rw', 9, $4, 'completed', now() - interval '12 minutes')`, rework, f.user, f.repo, old7)

	// A newer issue 8 run in ANOTHER repo does not supersede this repo's.
	other := uuid.New()
	f.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 3, $3, 'https://forge.e2e/g/o', 'main', true)`, other, f.forge, "g/o-"+other.String())
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status)
	      VALUES ($1, $2, $3, 'issue', 8, 't', 'd', 'b/o', 'running')`, uuid.New(), f.user, other)

	rows, err := f.q.ListBoardFreeMRStateWatchCandidates(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	type view struct {
		iid    pgtype.Int8
		kind   string
		latest bool
	}
	want := []struct {
		id uuid.UUID
		v  view
	}{ // created_at DESC among live rows: prompt, rework, self_improve, old9, run8, new7, old7
		{prompt, view{pgtype.Int8{}, "prompt", true}},
		{rework, view{pgtype.Int8{}, "mr_rework", true}},
		{selfImprove, view{pgtype.Int8{Int64: 7, Valid: true}, "self_improve", true}},
		{old9, view{pgtype.Int8{Int64: 9, Valid: true}, "issue", false}},
		{run8, view{pgtype.Int8{Int64: 8, Valid: true}, "issue", true}},
		{new7, view{pgtype.Int8{Int64: 7, Valid: true}, "issue", true}},
		{old7, view{pgtype.Int8{Int64: 7, Valid: true}, "issue", false}},
	}
	if len(rows) != len(want) {
		t.Fatalf("candidates = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		r := rows[i]
		if r.ID != w.id {
			t.Fatalf("row %d is run %s, want %s (membership or order changed)", i, r.ID, w.id)
		}
		if got := (view{r.IssueIid, r.Kind, r.IsLatest}); got != w.v {
			t.Errorf("row %d = %+v, want %+v", i, got, w.v)
		}
	}
}

// When the poller could not build the linked issue side it hands over the
// UnavailableIssueTracker. The close edge is still recorded (a permanent cause would
// otherwise leave mr_state opened and the rework gate open on a closed MR), nothing is
// moved or written, and the closed run stays a reopen-watch candidate like any linked
// issue-lane run whose cached issue is still opened.
func TestLinkedMRCloseIsRecordedWhenTheIssueSideIsUnavailableLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.card(t, board.ColumnHumanReview)
	run := f.run(t, n, 13, "opened", time.Minute)
	f.tr = UnavailableIssueTracker(errors.New("down"))
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.mrState(t, run); got != forge.MRStateClosed {
		t.Fatalf("mr_state = %q, want closed recorded despite the unavailable issue side", got)
	}
	if got := f.column(t, n); got != board.ColumnHumanReview {
		t.Fatalf("cached column = %q, want it untouched", got)
	}
	if got := f.srv.IssueState(n); got != "In Review" {
		t.Fatalf("State = %q, want it untouched", got)
	}
	if reqs := f.srv.Requests(); len(reqs) != 0 {
		t.Fatalf("requests reached the issue tracker: %+v", reqs)
	}
	rows, err := f.q.ListBoardFreeMRStateWatchCandidates(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != run || rows[0].MrState.String != forge.MRStateClosed {
		t.Fatalf("candidates = %+v, want only the closed run, watched for a reopen", rows)
	}
}

// A failed repo-source read is the one transient unavailable cause: the linked issue-lane
// close edge is deferred (mr_state stays opened, so the next tick sees it again), while a
// permanent cause is recorded as in the test above.
func TestLinkedMRCloseIsDeferredOnATransientRepoSourceReadFailureLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.card(t, board.ColumnHumanReview)
	run := f.run(t, n, 13, "opened", time.Minute)
	real := f.tr
	f.tr = UnavailableIssueTracker(fmt.Errorf("read repo issue source: %w: %w", errRepoSourceRead, errors.New("conn reset")))
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.mrState(t, run); got != forge.MRStateOpened {
		t.Fatalf("mr_state = %q, want opened (edge deferred, not consumed)", got)
	}
	if reqs := f.srv.Requests(); len(reqs) != 0 {
		t.Fatalf("requests reached the issue tracker: %+v", reqs)
	}

	// The read recovers: the same edge is seen again and moves the card.
	f.tr = real
	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.mrState(t, run); got != forge.MRStateClosed {
		t.Fatalf("mr_state = %q after recovery, want closed", got)
	}
}

// During a deferral only linked issue-lane candidates are skipped: a self_improve run
// (issue_iid set) and an issue-less helper run on the same repo are still recorded.
func TestLinkedMRDeferralStillRecordsNonIssueLaneRunsLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.card(t, board.ColumnHumanReview)
	issueRun := f.run(t, n, 13, "opened", time.Minute)
	selfRun := f.run(t, n, 14, "opened", 2*time.Minute)
	f.exec(t, `UPDATE runs SET kind = 'self_improve' WHERE id = $1`, selfRun)
	helperRun := f.run(t, n, 15, "opened", 3*time.Minute)
	f.exec(t, `UPDATE runs SET kind = 'task', issue_iid = NULL WHERE id = $1`, helperRun)
	f.tr = UnavailableIssueTracker(fmt.Errorf("read repo issue source: %w: %w", errRepoSourceRead, errors.New("conn reset")))
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed, 14: forge.MRStateClosed, 15: forge.MRStateClosed})
	if got := f.mrState(t, issueRun); got != forge.MRStateOpened {
		t.Fatalf("issue-lane mr_state = %q, want opened (deferred)", got)
	}
	if got := f.mrState(t, selfRun); got != forge.MRStateClosed {
		t.Fatalf("self_improve mr_state = %q, want closed (recorded during the deferral)", got)
	}
	if got := f.mrState(t, helperRun); got != forge.MRStateClosed {
		t.Fatalf("issue-less mr_state = %q, want closed (recorded during the deferral)", got)
	}
}

// A self_improve run carries an issue_iid but is not an issue-lane run: its close edge is
// recorded and moves no card, even over a Human Review card. (A prompt run cannot carry an
// issue_iid: runs_kind_shape refuses it, so only self_improve can reach the kind check.)
func TestLinkedMRSelfImproveRunNeverMovesTheCardLiveDB(t *testing.T) {
	f := newLinkedMRWatch(t, humanReviewMapping)
	n := f.card(t, board.ColumnHumanReview)
	run := f.run(t, n, 13, "opened", time.Minute)
	f.exec(t, `UPDATE runs SET kind = 'self_improve' WHERE id = $1`, run)
	f.srv.ResetRequests()

	f.sync(t, map[int64]string{13: forge.MRStateClosed})
	if got := f.mrState(t, run); got != forge.MRStateClosed {
		t.Fatalf("mr_state = %q, want closed recorded", got)
	}
	if got := f.column(t, n); got != board.ColumnHumanReview {
		t.Fatalf("cached column = %q, want it untouched", got)
	}
	if got := f.srv.IssueState(n); got != "In Review" {
		t.Fatalf("State = %q, want it untouched", got)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a self_improve run wrote to the issue tracker: %+v", w)
	}
}
