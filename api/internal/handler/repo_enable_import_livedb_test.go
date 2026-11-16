package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/poller"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4399: enabling a repo returns once the enable commits; the first import is
// a durable waiting/running/succeeded|failed row the poller runs. These tests drive
// the real SetRepoEnabled handler and a real poller.Engine against a real Postgres
// and a fake GitLab whose issue listing can be slow, gated, failing or counted.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// importProbe serves GET .../issues for the fake forge and records, per forge
// project, how many listings ran, how many were in flight at once, and what to do
// with the next ones. It is the instrument for the "one import at a time per repo"
// claims, so it counts at the wire, not in the code under test.
type importProbe struct {
	mu          sync.Mutex
	listings    map[int64]int
	inflight    map[int64]int
	maxInflight map[int64]int
	issues      map[int64][]int64 // forge iids per project; absent means [5]
	delayFirst  map[int64]time.Duration
	gate        map[int64]chan struct{} // listings block until closed
	status      map[int64]int           // non-zero answers with this HTTP status
	onListing   func(project int64, n int)
	aborted     map[int64]int // listings whose HTTP request context was cancelled mid-flight
}

func newImportProbe() *importProbe {
	return &importProbe{
		listings: map[int64]int{}, inflight: map[int64]int{}, maxInflight: map[int64]int{},
		issues: map[int64][]int64{}, delayFirst: map[int64]time.Duration{},
		gate: map[int64]chan struct{}{}, status: map[int64]int{}, aborted: map[int64]int{},
	}
}

func (p *importProbe) abortedCount(project int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aborted[project]
}

func (p *importProbe) set(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
}

func (p *importProbe) setIssues(project int64, iids ...int64) {
	p.set(func() { p.issues[project] = iids })
}

func (p *importProbe) listingCount(project int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listings[project]
}

func (p *importProbe) maxConcurrent(project int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInflight[project]
}

func (p *importProbe) serve(w http.ResponseWriter, r *http.Request) bool {
	project := projectIDFromPath(r.URL.Path)
	p.mu.Lock()
	p.listings[project]++
	n := p.listings[project]
	p.inflight[project]++
	if p.inflight[project] > p.maxInflight[project] {
		p.maxInflight[project] = p.inflight[project]
	}
	var delay time.Duration
	if n == 1 {
		delay = p.delayFirst[project]
	}
	gate, status, cb := p.gate[project], p.status[project], p.onListing
	iids, ok := p.issues[project]
	if !ok {
		iids = []int64{5}
	}
	iids = append([]int64(nil), iids...)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inflight[project]--
		p.mu.Unlock()
	}()
	abort := func() bool {
		p.mu.Lock()
		p.aborted[project]++
		p.mu.Unlock()
		return true
	}

	if cb != nil {
		cb(project, n)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return abort()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return abort()
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"forge unhappy"}`))
		return true
	}
	items := make([]map[string]any, 0, len(iids))
	for _, iid := range iids {
		items = append(items, map[string]any{
			"id": 9000 + iid, "iid": iid, "title": fmt.Sprintf("Card %d", iid), "state": "opened",
			"labels": []string{"uzi"}, "web_url": fmt.Sprintf("https://forge.e2e/g/r/-/issues/%d", iid),
			"updated_at": "2026-11-01T00:00:00Z",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
	return true
}

func (f enableGuardFixture) useProbe() *importProbe {
	p := newImportProbe()
	f.forge.mu.Lock()
	f.forge.issuesHook = p.serve
	f.forge.mu.Unlock()
	return p
}

// ownedRepos narrows the poller to this fixture's owner: the database is shared with
// other tests' enabled repos, whose connections this fake does not serve.
type ownedRepos struct {
	q     *store.Queries
	owner uuid.UUID
}

func (o ownedRepos) ListEnabledReposWithConnections(ctx context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	rows, err := o.q.ListEnabledReposWithConnections(ctx)
	var out []store.ListEnabledReposWithConnectionsRow
	for _, r := range rows {
		if r.UserID == o.owner {
			out = append(out, r)
		}
	}
	return out, err
}

// newPoller builds the real engine on the handler's own forge service and wires the
// handler's requester to it, WITHOUT starting it, so a test chooses when the poller
// begins relative to the enable request.
func (f enableGuardFixture) newPoller(interval time.Duration) *poller.Engine {
	en := poller.New(f.h.svc, ownedRepos{q: f.h.q, owner: f.owner.ID}, interval, 10)
	// The floor keeps a slow listing from being cut by a short test interval.
	en.SetForgeTimeout(5 * time.Second)
	en.SetImportStateStore(f.h.q)
	f.h.SetInitialImportRequester(en.RequestImport)
	return en
}

func (f enableGuardFixture) runPoller(t *testing.T, en *poller.Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); en.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

type handlerImportRow struct {
	state, class string
	gen          int64
	updatedAt    time.Time
}

func (f enableGuardFixture) importRow(ctx context.Context, t *testing.T, repoID uuid.UUID) handlerImportRow {
	t.Helper()
	var st, cl *string
	var at *time.Time
	var row handlerImportRow
	if err := f.pool.QueryRow(ctx, `SELECT initial_import_state, initial_import_generation, initial_import_error_class, initial_import_updated_at FROM repos WHERE id=$1`, repoID).Scan(&st, &row.gen, &cl, &at); err != nil {
		t.Fatalf("read import row: %v", err)
	}
	if st != nil {
		row.state = *st
	}
	if cl != nil {
		row.class = *cl
	}
	if at != nil {
		row.updatedAt = *at
	}
	return row
}

func (f enableGuardFixture) cachedIIDs(ctx context.Context, t *testing.T, repoID uuid.UUID) []int64 {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT forge_issue_iid FROM issues WHERE repo_id=$1`, repoID)
	if err != nil {
		t.Fatalf("read issues: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var iid int64
		if err := rows.Scan(&iid); err != nil {
			t.Fatalf("scan issue: %v", err)
		}
		out = append(out, iid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (f enableGuardFixture) waitImport(ctx context.Context, t *testing.T, repoID uuid.UUID, state string) handlerImportRow {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		row := f.importRow(ctx, t, repoID)
		if row.state == state {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("import state = %+v, want %q within 20s", row, state)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (f enableGuardFixture) requireOK(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

// The reported bug: a large repo's first import ran on the enable request, so a
// gateway cut it at its deadline. Now the request returns inside the deadline with
// the import recorded, and the poller completes it afterwards.
func TestSetRepoEnableReturnsBeforeSlowImportLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	probe := f.useProbe()
	const project = 201
	probe.set(func() { probe.delayFirst[project] = 3 * time.Second })
	probe.setIssues(project, 5, 6, 7)
	repoID := f.seedRepo(ctx, t, project, false, protClean)
	engine := f.newPoller(100 * time.Millisecond)

	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	w := f.setEnabledCtx(reqCtx, t, repoID, true)
	elapsed := time.Since(start)
	f.requireOK(t, w)
	if elapsed >= 500*time.Millisecond || reqCtx.Err() != nil {
		t.Fatalf("enable took %v (request context err %v), want it to return inside the 500ms gateway deadline", elapsed, reqCtx.Err())
	}
	if row := f.importRow(ctx, t, repoID); row.state != "waiting" {
		t.Fatalf("import row right after the enable = %+v, want waiting", row)
	}

	// The poller then runs the slow import to completion on its own context.
	f.runPoller(t, engine)
	row := f.waitImport(ctx, t, repoID, "succeeded")
	if row.gen != 1 || row.class != "" {
		t.Errorf("settled row = %+v, want generation 1 with a NULL class", row)
	}
	if got := f.cachedIIDs(ctx, t, repoID); fmt.Sprint(got) != "[5 6 7]" {
		t.Errorf("cached issues = %v, want the forge's [5 6 7]", got)
	}
}

// Cancelling the enable request's context while the import's first issue listing is
// in flight must not cancel the import: the poller owns it. The listing hook cancels
// the request context the moment the first listing starts, and the fake counts any
// listing whose own HTTP request was cancelled mid-flight. An import that ran on the
// request context would see such an abort; the poller's import must see none.
func TestSetRepoEnableImportSurvivesRequestCancelLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	probe := f.useProbe()
	const project = 202
	probe.set(func() { probe.delayFirst[project] = 500 * time.Millisecond })
	probe.setIssues(project, 5, 6)
	repoID := f.seedRepo(ctx, t, project, false, protClean)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	probe.set(func() {
		probe.onListing = func(p int64, n int) {
			if p == project {
				once.Do(cancel) // the first listing is now in progress
			}
		}
	})
	f.runPoller(t, f.newPoller(100*time.Millisecond))

	// The handler runs in a goroutine so that an implementation importing on the
	// request context cannot wedge the test; every wait below is bounded.
	resp := make(chan *httptest.ResponseRecorder, 1)
	go func() { resp <- f.setEnabledCtx(reqCtx, t, repoID, true) }()
	select {
	case w := <-resp:
		f.requireOK(t, w)
	case <-time.After(20 * time.Second):
		t.Fatal("the enable request did not return within 20s")
	}

	f.waitImport(ctx, t, repoID, "succeeded")
	if reqCtx.Err() == nil {
		t.Fatal("the request context was never cancelled: the first listing did not start")
	}
	if n := probe.abortedCount(project); n != 0 {
		t.Errorf("%d issue listing(s) were cancelled mid-flight by the request cancellation, want 0", n)
	}
	if got := f.cachedIIDs(ctx, t, repoID); fmt.Sprint(got) != "[5 6]" {
		t.Errorf("cached issues = %v, want [5 6] after the request was cancelled", got)
	}
}

// Real enable interleavings through SetRepoEnabled with the poller idle: disable then
// two back-to-back enables each take a newer generation, the poller then runs ONE
// import at the newest generation, and enabling an already-succeeded repo changes
// nothing (no new generation, no state reset).
func TestSetRepoEnableGenerationsAndIdempotentEnableLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	probe := f.useProbe()
	const project = 203
	probe.setIssues(project, 5, 6, 7)
	repoID := f.seedRepo(ctx, t, project, false, protClean)
	engine := f.newPoller(100 * time.Millisecond)

	f.requireOK(t, f.setEnabled(t, repoID, true))
	if row := f.importRow(ctx, t, repoID); row.state != "waiting" || row.gen != 1 {
		t.Fatalf("after the first enable = %+v, want waiting at generation 1", row)
	}
	f.requireOK(t, f.setEnabled(t, repoID, false))
	if row := f.importRow(ctx, t, repoID); row.gen != 1 {
		t.Fatalf("a disable must not touch the generation, got %+v", row)
	}
	f.requireOK(t, f.setEnabled(t, repoID, true))
	f.requireOK(t, f.setEnabled(t, repoID, true))
	if row := f.importRow(ctx, t, repoID); row.state != "waiting" || row.gen != 3 {
		t.Fatalf("after disable + two enables = %+v, want waiting at generation 3", row)
	}

	f.runPoller(t, engine)
	done := f.waitImport(ctx, t, repoID, "succeeded")
	if done.gen != 3 || done.class != "" {
		t.Fatalf("settled row = %+v, want generation 3 with a NULL class", done)
	}
	if got := f.cachedIIDs(ctx, t, repoID); fmt.Sprint(got) != "[5 6 7]" {
		t.Errorf("cached issues = %v, want [5 6 7]", got)
	}
	if m := probe.maxConcurrent(project); m != 1 {
		t.Errorf("max concurrent issue listings for the repo = %d, want 1", m)
	}

	// Enabling a repo whose import already succeeded is a no-op on the import row.
	f.requireOK(t, f.setEnabled(t, repoID, true))
	time.Sleep(400 * time.Millisecond) // a few poller passes
	if after := f.importRow(ctx, t, repoID); after != done {
		t.Fatalf("idempotent enable changed the import row: before %+v, after %+v", done, after)
	}
}

// A poller pass blocked inside the repo's import while the repo is disabled and
// re-enabled: the stale import's finish must not overwrite the newer generation, the
// newest generation is imported afterwards (never concurrently with the stale one),
// and the cache ends equal to the forge's set.
func TestSetRepoEnableDuringInFlightImportLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	probe := f.useProbe()
	const project = 204
	probe.setIssues(project, 5, 6, 7)
	gate := make(chan struct{})
	probe.set(func() { probe.gate[project] = gate })
	repoID := f.seedRepo(ctx, t, project, false, protClean)

	// The first listing of the stale (generation 1) import is #1; the first listing
	// of the next import is #4 (FullSync makes three sequential listings). Capture the
	// row there: a stale finish that overwrote the newer generation would have left
	// it succeeded instead of running.
	var atFourth handlerImportRow
	var captured bool
	var capMu sync.Mutex
	probe.set(func() {
		probe.onListing = func(p int64, n int) {
			if p != project || n != 4 {
				return
			}
			var st string
			var gen int64
			if err := f.pool.QueryRow(context.Background(), `SELECT initial_import_state, initial_import_generation FROM repos WHERE id=$1`, repoID).Scan(&st, &gen); err == nil {
				capMu.Lock()
				atFourth, captured = handlerImportRow{state: st, gen: gen}, true
				capMu.Unlock()
			}
		}
	})
	f.runPoller(t, f.newPoller(100*time.Millisecond))

	f.requireOK(t, f.setEnabled(t, repoID, true))
	// Wait for the poller to be inside the generation-1 import.
	deadline := time.Now().Add(15 * time.Second)
	for probe.listingCount(project) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the poller never started the import")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row := f.importRow(ctx, t, repoID); row.state != "running" || row.gen != 1 {
		t.Fatalf("mid-import row = %+v, want running at generation 1", row)
	}

	f.requireOK(t, f.setEnabled(t, repoID, false))
	f.requireOK(t, f.setEnabled(t, repoID, true))
	// The poller goroutine is blocked in the stale import, so nothing claims yet.
	if row := f.importRow(ctx, t, repoID); row.state != "waiting" || row.gen != 2 {
		t.Fatalf("after disable + enable mid-import = %+v, want waiting at generation 2", row)
	}

	close(gate)
	done := f.waitImport(ctx, t, repoID, "succeeded")
	if done.gen != 2 {
		t.Fatalf("settled row = %+v, want succeeded at generation 2", done)
	}
	if got := f.cachedIIDs(ctx, t, repoID); fmt.Sprint(got) != "[5 6 7]" {
		t.Errorf("cached issues = %v, want [5 6 7]", got)
	}
	if m := probe.maxConcurrent(project); m != 1 {
		t.Errorf("max concurrent issue listings for the repo = %d, want 1 (the stale and the new import must not overlap)", m)
	}
	capMu.Lock()
	defer capMu.Unlock()
	if !captured {
		t.Fatal("the generation-2 import never listed issues")
	}
	if atFourth.state != "running" || atFourth.gen != 2 {
		t.Errorf("row when the generation-2 import began = %+v, want running at generation 2 (the stale finish overwrote it)", atFourth)
	}
}

type boardImport struct {
	State      string    `json:"state"`
	UpdatedAt  time.Time `json:"updated_at"`
	ErrorClass string    `json:"error_class"`
}

// getBoardImport reads initial_import from the real GET board response; raw reports
// whether the JSON carried an explicit null.
func (f enableGuardFixture) getBoardImport(t *testing.T, repoID uuid.UUID) (imp *boardImport, nullField bool) {
	t.Helper()
	rr := httptest.NewRecorder()
	f.h.GetBoard(rr, boardWriterReq(f.owner, repoID, "", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("GetBoard status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Board struct {
			InitialImport json.RawMessage `json:"initial_import"`
			Cards         []any           `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	if resp.Board.InitialImport == nil {
		t.Fatalf("board JSON has no initial_import key: %s", rr.Body.String())
	}
	if strings.TrimSpace(string(resp.Board.InitialImport)) == "null" {
		return nil, true
	}
	var out boardImport
	if err := json.Unmarshal(resp.Board.InitialImport, &out); err != nil {
		t.Fatalf("decode initial_import: %v", err)
	}
	return &out, false
}

// The board reports the durable import status, never inferred from the card count: a
// repo with zero issues succeeds with an empty board, a failing forge reports its
// class and later flips to succeeded, and a legacy repo reports null.
func TestBoardReportsInitialImportStatusLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	probe := f.useProbe()
	const project = 205
	probe.setIssues(project) // the forge has no issues at all
	repoID := f.seedRepo(ctx, t, project, false, protClean)
	engine := f.newPoller(100 * time.Millisecond)

	f.requireOK(t, f.setEnabled(t, repoID, true))
	imp, _ := f.getBoardImport(t, repoID)
	if imp == nil || imp.State != "waiting" || imp.ErrorClass != "" || imp.UpdatedAt.IsZero() {
		t.Fatalf("board right after the enable = %+v, want a waiting import with a timestamp", imp)
	}

	// The forge is failing: the poller records the class.
	probe.set(func() { probe.status[project] = http.StatusInternalServerError })
	f.runPoller(t, engine)
	var failed *boardImport
	deadline := time.Now().Add(20 * time.Second)
	for {
		failed, _ = f.getBoardImport(t, repoID)
		if failed != nil && failed.State == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("board import = %+v, want failed within 20s", failed)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if failed.ErrorClass != "server_error" {
		t.Errorf("failed import error_class = %q, want server_error", failed.ErrorClass)
	}

	// The forge recovers: a regular pass retries the failed import; zero issues is a
	// success with an empty board, not "failed" or "importing".
	probe.set(func() { probe.status[project] = 0 })
	f.waitImport(ctx, t, repoID, "succeeded")
	imp, _ = f.getBoardImport(t, repoID)
	if imp == nil || imp.State != "succeeded" || imp.ErrorClass != "" {
		t.Fatalf("board after recovery = %+v, want succeeded with no error class", imp)
	}

	// A legacy repo (enabled before the import row existed) reports null.
	legacy := f.seedRepo(ctx, t, 206, true, protClean)
	mustExecT(ctx, t, f.pool, `INSERT INTO board_columns (repo_id, label_name, position) VALUES ($1, 'Planned', 0)`, legacy)
	if imp, isNull := f.getBoardImport(t, legacy); imp != nil || !isNull {
		t.Fatalf("legacy repo board initial_import = %+v, want null", imp)
	}
}
