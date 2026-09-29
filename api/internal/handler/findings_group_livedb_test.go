package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Live-DB proof for the group filing API (issue #1724 M4a): POST /findings/group/issue, the
// operation release, the group draft, and the interplay with single filing, dismiss, Done and the
// forgesvc reconciler. Handler.pool/.q are concrete, so none of this is reachable from a fake
// store: the claim's row locks, the durable deadline, and the settlement transaction are exactly
// what these tests exist to exercise. The forge is a forgetest.BaseFake that records every call
// (or, where the classification of a real HTTP status matters, the real GitLab driver against an
// httptest server).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// ── fake forge ───────────────────────────────────────────────────────────────────────────────

type fgCreate struct {
	Title, Description string
	Labels             []string
}

// fgFake records every call. Only the methods the group filing path and the reconciler touch are
// overridden; anything else returns forgetest's loud not-stubbed error.
type fgFake struct {
	forgetest.BaseFake
	mu      sync.Mutex
	seq     int
	ensureN int
	ensureS int // seq of the first EnsureLabels
	createS int // seq of the first CreateIssue
	creates []fgCreate
	issues  []forge.Issue
	nextIID int64

	createErr  error // returned by CreateIssue when non-nil
	storeOnErr bool  // the issue exists on the forge even though CreateIssue errored (uncertain outcome)
	onCreate   func()
	ensureErr  error // returned by EnsureLabels when non-nil

	ensureEntered chan struct{} // signalled (buffered) when EnsureLabels is entered and gated
	ensureGate    chan struct{} // when non-nil EnsureLabels blocks until it is closed
}

func (f *fgFake) EnsureLabels(_ context.Context, _ int64, _ []forge.Label) error {
	f.mu.Lock()
	f.seq++
	f.ensureN++
	if f.ensureS == 0 {
		f.ensureS = f.seq
	}
	entered, gate, eerr := f.ensureEntered, f.ensureGate, f.ensureErr
	f.mu.Unlock()
	if gate != nil {
		if entered != nil {
			entered <- struct{}{}
		}
		<-gate
	}
	return eerr
}

func (f *fgFake) CreateIssue(_ context.Context, _ int64, title, description string, labels []string) (forge.Issue, error) {
	f.mu.Lock()
	f.seq++
	if f.createS == 0 {
		f.createS = f.seq
	}
	f.creates = append(f.creates, fgCreate{Title: title, Description: description, Labels: append([]string(nil), labels...)})
	hook, cerr, keep := f.onCreate, f.createErr, f.storeOnErr
	f.nextIID++
	iid := f.nextIID
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	issue := forge.Issue{
		IID: iid, Title: title, Description: description, Labels: append([]string(nil), labels...),
		State: string(forge.StateOpened), WebURL: fmt.Sprintf("https://forge.e2e/g/group/-/issues/%d", iid), UpdatedAt: time.Now(),
	}
	if cerr != nil {
		if keep {
			f.mu.Lock()
			f.issues = append(f.issues, issue)
			f.mu.Unlock()
		}
		return forge.Issue{}, cerr
	}
	f.mu.Lock()
	f.issues = append(f.issues, issue)
	f.mu.Unlock()
	return issue, nil
}

func (f *fgFake) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.Issue(nil), f.issues...), nil
}

// calls is every forge write-path call the handler could make.
func (f *fgFake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensureN + len(f.creates)
}

func (f *fgFake) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fgFake) create(i int) fgCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates[i]
}

// fgHTTPForge is an httptest GitLab whose issue-create status is configurable, so the REAL driver's
// definitive-versus-uncertain classification is what the handler acts on.
type fgHTTPForge struct {
	mu      sync.Mutex
	status  int
	creates int
}

func (s *fgHTTPForge) setStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

func (s *fgHTTPForge) createN() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

func (s *fgHTTPForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
		_, _ = w.Write([]byte("[]"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":7,"name":%q,"color":"#888888"}`, m["name"])
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
		s.mu.Lock()
		s.creates++
		status := s.status
		s.mu.Unlock()
		if status != http.StatusCreated {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"rejected"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":300,"iid":300,"project_id":1,"title":"t","description":"d","state":"opened","web_url":"https://forge.example/g/-/issues/300","labels":[]}`))
	default:
		_, _ = w.Write([]byte("{}"))
	}
}

// ── environment ──────────────────────────────────────────────────────────────────────────────

type fgEnv struct {
	t       *testing.T
	ctx     context.Context
	h       *Handler
	pool    *pgxpool.Pool
	q       *store.Queries
	svc     *forgesvc.Service
	fake    *fgFake
	owner   store.User
	other   store.User
	repoID  uuid.UUID
	repo2ID uuid.UUID
	prefix  string
	seq     int
	run     uuid.UUID
}

const fgProjectID = 9001

// newFGEnv builds a Handler over a live pool. With httpForge nil the forge is the recording fake;
// otherwise the real driver talks to the supplied httptest handler.
func newFGEnv(t *testing.T, httpForge http.Handler) *fgEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	box := newHandlerTestBox(t)
	e := &fgEnv{
		t: t, ctx: ctx, pool: pool, q: q, fake: &fgFake{nextIID: 500},
		owner:   store.User{ID: uuid.New(), Email: fmt.Sprintf("fg-owner-%s@e2e", uuid.NewString()[:8])},
		other:   store.User{ID: uuid.New(), Email: fmt.Sprintf("fg-other-%s@e2e", uuid.NewString()[:8])},
		repoID:  uuid.New(),
		repo2ID: uuid.New(),
		run:     uuid.New(),
		prefix:  "fg" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10] + "/",
	}
	baseURL := "https://forge.e2e"
	var builder forgesvc.ForgeBuilder
	if httpForge != nil {
		srv := httptest.NewServer(httpForge)
		t.Cleanup(srv.Close)
		baseURL = srv.URL
	} else {
		builder = func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return e.fake, nil }
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x'),($3,$4,'x')`, e.owner.ID, e.owner.Email, e.other.ID, e.other.Email)
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES ($1,$2,'gitlab',$3,'bot',1,$4)`, connID, e.owner.ID, baseURL, sealed)
	mustExecT(ctx, t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES ($1,$2,$3,$4,'https://forge.e2e/g/group','main',true),($5,$2,$6,$7,'https://forge.e2e/g/other','main',true)`,
		e.repoID, connID, fgProjectID, "g/group-"+e.prefix, e.repo2ID, fgProjectID+1, "g/other-"+e.prefix)
	mustExecT(ctx, t, pool, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
		VALUES ($1,$2,$3,42,'Do X','d','completed','issue')`, e.run, e.owner.ID, e.repoID)
	e.svc = forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, builder)
	e.svc.SetFindingGroupDB(pool)
	e.h = &Handler{
		pool: pool, q: q, box: box, cfg: config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{{Key: settings.KeyUziLabel, Value: "uzi"}}}, time.Minute),
		svc:      e.svc,
		wsvc:     workersvc.New(q, box, workersvc.Params{}),
	}
	return e
}

type fgMember struct {
	Disp, Finding   uuid.UUID
	Location, Title string
}

// seedMember inserts one evidence row and its open disposition. body is the evidence text.
func (e *fgEnv) seedMember(repo uuid.UUID, body string) fgMember {
	e.t.Helper()
	e.seq++
	m := fgMember{
		Location: fmt.Sprintf("%sfile%03d.go#Fn%03d", e.prefix, e.seq, e.seq),
		Title:    fmt.Sprintf("finding title %s-%03d", e.prefix[:len(e.prefix)-1], e.seq),
	}
	if repo == e.repoID {
		f, err := e.q.InsertFinding(e.ctx, store.InsertFindingParams{
			RunID: e.run, UserID: e.owner.ID, RepoID: repo, Location: m.Location, Title: m.Title,
			DescriptionMd: body, Labels: []byte(`["perf"]`), Confidence: "high",
		})
		if err != nil {
			e.t.Fatalf("InsertFinding: %v", err)
		}
		m.Finding = f.ID
	} else {
		// A finding in another repo needs a run of its own repo (runs are repo scoped).
		run := uuid.New()
		mustExecT(e.ctx, e.t, e.pool, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
			VALUES ($1,$2,$3,43,'Do Y','d','completed','issue')`, run, e.owner.ID, repo)
		f, err := e.q.InsertFinding(e.ctx, store.InsertFindingParams{
			RunID: run, UserID: e.owner.ID, RepoID: repo, Location: m.Location, Title: m.Title,
			DescriptionMd: body, Labels: []byte(`[]`), Confidence: "high",
		})
		if err != nil {
			e.t.Fatalf("InsertFinding (repo2): %v", err)
		}
		m.Finding = f.ID
	}
	d, err := e.q.UpsertOpenDisposition(e.ctx, store.UpsertOpenDispositionParams{
		UserID: e.owner.ID, RepoID: repo, Location: m.Location, ContentHash: "h-" + m.Location, LastTitle: m.Title,
	})
	if err != nil {
		e.t.Fatalf("UpsertOpenDisposition: %v", err)
	}
	m.Disp = d.ID
	return m
}

func (e *fgEnv) members(n int) []fgMember {
	ms := make([]fgMember, n)
	for i := range ms {
		ms[i] = e.seedMember(e.repoID, fmt.Sprintf("evidence body for member %d", i))
	}
	return ms
}

func fgIDs(ms ...fgMember) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Disp.String()
	}
	return out
}

type fgDisp struct {
	Status  string
	IID     *int64
	URL     string
	GroupOp pgtype.UUID
	SetVia  pgtype.Text
}

func (e *fgEnv) disp(id uuid.UUID) fgDisp {
	e.t.Helper()
	var d fgDisp
	var iid pgtype.Int8
	if err := e.pool.QueryRow(e.ctx, `SELECT status, filed_issue_iid, filed_issue_url, group_operation_id, set_via FROM finding_dispositions WHERE id=$1`, id).
		Scan(&d.Status, &iid, &d.URL, &d.GroupOp, &d.SetVia); err != nil {
		e.t.Fatalf("read disposition: %v", err)
	}
	if iid.Valid {
		v := iid.Int64
		d.IID = &v
	}
	return d
}

func (e *fgEnv) requireOpen(ms ...fgMember) {
	e.t.Helper()
	for _, m := range ms {
		if d := e.disp(m.Disp); d.Status != "open" || d.GroupOp.Valid || d.IID != nil {
			e.t.Errorf("member %s: want open and unclaimed, got %+v", m.Location, d)
		}
	}
}

func (e *fgEnv) requireClaimed(op uuid.UUID, ms ...fgMember) {
	e.t.Helper()
	for _, m := range ms {
		d := e.disp(m.Disp)
		if d.Status != "filing" || !d.GroupOp.Valid || uuid.UUID(d.GroupOp.Bytes) != op || d.IID != nil {
			e.t.Errorf("member %s: want filing under %s, got %+v", m.Location, op, d)
		}
	}
}

func (e *fgEnv) requireFiled(iid int64, ms ...fgMember) {
	e.t.Helper()
	for _, m := range ms {
		d := e.disp(m.Disp)
		if d.Status != "filed" || d.IID == nil || *d.IID != iid || d.URL == "" || d.GroupOp.Valid {
			e.t.Errorf("member %s: want filed as #%d, got %+v", m.Location, iid, d)
		}
	}
}

func (e *fgEnv) opPhase(op uuid.UUID) (string, *int64) {
	e.t.Helper()
	var phase string
	var iid pgtype.Int8
	if err := e.pool.QueryRow(e.ctx, `SELECT phase, issue_iid FROM finding_group_operations WHERE id=$1`, op).Scan(&phase, &iid); err != nil {
		e.t.Fatalf("read operation: %v", err)
	}
	if iid.Valid {
		v := iid.Int64
		return phase, &v
	}
	return phase, nil
}

func (e *fgEnv) opsForOwner() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM finding_group_operations WHERE user_id=$1`, e.owner.ID).Scan(&n); err != nil {
		e.t.Fatalf("count operations: %v", err)
	}
	return n
}

func (e *fgEnv) expireOp(op uuid.UUID) {
	e.t.Helper()
	mustExecT(e.ctx, e.t, e.pool, `UPDATE finding_group_operations SET deadline_at = now() - interval '1 second' WHERE id=$1`, op)
}

func fgRequest(user store.User, method, target string, body any, params map[string]string) *http.Request {
	var reader io.Reader = strings.NewReader("")
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, target, reader)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

func (e *fgEnv) fileGroup(user store.User, body map[string]any) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.h.FileFindingGroup(rr, fgRequest(user, http.MethodPost, "/x", body, nil))
	return rr
}

func (e *fgEnv) release(user store.User, op string, confirmed bool) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.h.ReleaseFindingGroup(rr, fgRequest(user, http.MethodPost, "/x", map[string]any{"confirmed_absent": confirmed}, map[string]string{"operation-id": op}))
	return rr
}

func (e *fgEnv) groupDraft(user store.User, ids []string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.h.GetFindingGroupIssueDraft(rr, fgRequest(user, http.MethodGet, "/x?ids="+strings.Join(ids, ","), nil, nil))
	return rr
}

func fgResult(t *testing.T, rr *httptest.ResponseRecorder) apitypes.FindingGroupFileResultDTO {
	t.Helper()
	var res apitypes.FindingGroupFileResultDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result %q: %v", rr.Body.String(), err)
	}
	return res
}

func fgOp(t *testing.T, res apitypes.FindingGroupFileResultDTO) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(res.OperationID)
	if err != nil {
		t.Fatalf("operation id %q: %v", res.OperationID, err)
	}
	return id
}

// blockSettlement installs a trigger that raises on any UPDATE that would set a disposition of this
// env to 'filed', so the settlement transaction fails after the issue identity was recorded. The
// returned func removes it (idempotent; also registered as cleanup).
func (e *fgEnv) blockSettlement() func() {
	e.t.Helper()
	name := "fg_block_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	mustExecT(e.ctx, e.t, e.pool, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION 'settlement blocked'; END $f$`)
	mustExecT(e.ctx, e.t, e.pool, `CREATE TRIGGER `+name+` BEFORE UPDATE ON finding_dispositions FOR EACH ROW
		WHEN (NEW.status = 'filed' AND NEW.location LIKE '`+e.prefix+`%') EXECUTE FUNCTION `+name+`()`)
	var once sync.Once
	drop := func() {
		once.Do(func() {
			_, _ = e.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON finding_dispositions`)
			_, _ = e.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
		})
	}
	e.t.Cleanup(drop)
	return drop
}

// ── unit (no DB): the roster must not be duplicated when the web sends the draft back ────────

func TestComposeFiledFindingGroupUneditedDraftHasOneRoster(t *testing.T) {
	parts := []groupDraftPart{
		{title: "leaked ticker", location: "internal/a.go#run", evidence: "ev one"},
		{title: "tick `code` title", location: "src/b`c.go", evidence: "ev two"},
		{title: "third", location: "x`` y.go", evidence: "ev three"},
	}
	// The preview is composed the way GetFindingGroupIssueDraft does: each part's location is the
	// already-wrapped issuedraft.FindingDraft.Location, while filing wraps the raw coordinate.
	previewParts := make([]groupDraftPart, len(parts))
	for i, p := range parts {
		previewParts[i] = groupDraftPart{title: p.title, location: issuedraft.SafeInlineCode(p.location), evidence: p.evidence}
	}
	_, preview := composeFindingGroupDraft(previewParts)
	body, ok := composeFiledFindingGroup(parts, &preview, uuid.New())
	if !ok {
		t.Fatal("body did not fit")
	}
	if n := strings.Count(body, "## Findings"); n != 1 {
		t.Fatalf("roster heading appears %d times, want 1:\n%s", n, body)
	}
	for _, p := range parts {
		line := issuedraft.SafeInlineCode(p.title) + " — " + issuedraft.SafeInlineCode(p.location)
		if n := strings.Count(body, line); n != 1 {
			t.Errorf("roster line %q appears %d times, want 1", line, n)
		}
	}
	for _, ev := range []string{"ev one", "ev two", "ev three"} {
		if !strings.Contains(body, ev) {
			t.Errorf("evidence %q lost", ev)
		}
	}
}

// ── filing ───────────────────────────────────────────────────────────────────────────────────

func TestFileFindingGroupThreeFindingsOneIssueLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(3)

	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	res := fgResult(t, rr)
	if res.Phase != "settled" || res.Issue == nil || res.Warning != "" || len(res.DispositionIDs) != 3 {
		t.Fatalf("result = %+v", res)
	}
	if e.fake.createCount() != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", e.fake.createCount())
	}
	e.fake.mu.Lock()
	ensureS, createS := e.fake.ensureS, e.fake.createS
	e.fake.mu.Unlock()
	if ensureS == 0 || ensureS >= createS {
		t.Errorf("EnsureLabels must precede CreateIssue: ensure=%d create=%d", ensureS, createS)
	}
	e.requireFiled(res.Issue.IID, ms...)
	c := e.fake.create(0)
	for _, m := range ms {
		if !strings.Contains(c.Description, m.Location) || !strings.Contains(c.Description, m.Title) {
			t.Errorf("issue body misses member %s / %s", m.Location, m.Title)
		}
	}
	if !strings.Contains(c.Description, "<!-- uzi-finding-group-operation: "+res.OperationID+" -->") {
		t.Error("operation marker absent from the filed body")
	}
	if len(c.Labels) == 0 || c.Labels[0] != findingTestMarker {
		t.Errorf("labels %v must lead with the finding marker %q", c.Labels, findingTestMarker)
	}
	if phase, iid := e.opPhase(fgOp(t, res)); phase != "settled" || iid == nil || *iid != res.Issue.IID {
		t.Errorf("operation = %s iid=%v, want settled #%d", phase, iid, res.Issue.IID)
	}
}

// The web always sends the draft description back. Filing with the unedited draft text must yield
// exactly one roster (M2 defect: the filed roster wrapped locations in inline code, the draft did not).
func TestFileFindingGroupUneditedDraftDescriptionOneRosterLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(3)
	ids := fgIDs(ms...)

	dr := e.groupDraft(e.owner, ids)
	if dr.Code != http.StatusOK {
		t.Fatalf("draft status = %d; body=%s", dr.Code, dr.Body.String())
	}
	var draft apitypes.FindingGroupDraftDTO
	if err := json.Unmarshal(dr.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.DispositionIDs) != 3 {
		t.Fatalf("draft ids = %v", draft.DispositionIDs)
	}
	rr := e.fileGroup(e.owner, map[string]any{"ids": ids, "title": draft.Title, "description": draft.Description, "labels": draft.Labels})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	body := e.fake.create(0).Description
	if n := strings.Count(body, "## Findings"); n != 1 {
		t.Fatalf("roster heading appears %d times, want 1:\n%s", n, body)
	}
	for _, m := range ms {
		line := issuedraft.SafeInlineCode(m.Title) + " — " + issuedraft.SafeInlineCode(m.Location)
		if n := strings.Count(body, line); n != 1 {
			t.Errorf("roster line for %s appears %d times, want 1", m.Location, n)
		}
	}
	if !strings.Contains(body, "evidence body for member 0") {
		t.Errorf("draft evidence lost:\n%s", body)
	}

	// A user who deleted the roster from the description still gets the server's roster once, and
	// one who edited evidence text keeps the edit.
	ms2 := e.members(2)
	rr = e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms2...), "description": "only my own words"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("edited status = %d; body=%s", rr.Code, rr.Body.String())
	}
	body2 := e.fake.create(1).Description
	if strings.Count(body2, "## Findings") != 1 || !strings.Contains(body2, "only my own words") || !strings.Contains(body2, ms2[1].Location) {
		t.Errorf("edited body wrong:\n%s", body2)
	}
}

func TestFileFindingGroupFiftyMembersBoundedBodyLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := make([]fgMember, 50)
	bulky := strings.Repeat("evidence line that is long enough to overflow the budget.\n", 150) // ~8.7 KiB each, ~430 KiB total
	for i := range ms {
		ms[i] = e.seedMember(e.repoID, bulky)
	}
	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%.300s", rr.Code, rr.Body.String())
	}
	res := fgResult(t, rr)
	body := e.fake.create(0).Description
	if len(body) > workersvc.MaxIssueDescriptionBytes {
		t.Fatalf("body is %d bytes, bound %d", len(body), workersvc.MaxIssueDescriptionBytes)
	}
	for _, m := range ms {
		if !strings.Contains(body, m.Location) || !strings.Contains(body, m.Title) {
			t.Errorf("member %s missing from the bounded body", m.Location)
		}
	}
	if !strings.Contains(body, "<!-- uzi-finding-group-operation: "+res.OperationID+" -->") {
		t.Error("marker truncated away")
	}
	e.requireFiled(res.Issue.IID, ms...)

	// A 51st id is refused before anything is claimed.
	over := append(fgIDs(ms...), e.seedMember(e.repoID, "x").Disp.String())
	if rr := e.fileGroup(e.owner, map[string]any{"ids": over}); rr.Code != http.StatusBadRequest {
		t.Errorf("51 ids: status = %d, want 400", rr.Code)
	}
}

func TestFileFindingGroupRefusedBeforeAnyForgeWriteLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	pair := e.members(2)
	ownerOnly := e.members(1)[0]

	dismissed := e.seedMember(e.repoID, "dismissed member")
	mustExecT(e.ctx, t, e.pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do' WHERE id=$1`, dismissed.Disp)
	evidenceless := uuid.New()
	mustExecT(e.ctx, t, e.pool, `INSERT INTO finding_dispositions (id,user_id,repo_id,location,status) VALUES ($1,$2,$3,$4,'open')`,
		evidenceless, e.owner.ID, e.repoID, e.prefix+"no-evidence.go")
	otherRepo := e.seedMember(e.repo2ID, "other repo member")

	cases := []struct {
		name string
		user store.User
		ids  []string
		want int
	}{
		{"another owner's ids", e.other, fgIDs(pair...), http.StatusNotFound},
		{"unknown id", e.owner, []string{pair[0].Disp.String(), uuid.NewString()}, http.StatusNotFound},
		{"mixed repositories", e.owner, fgIDs(pair[0], otherRepo), http.StatusBadRequest},
		{"non-open member", e.owner, fgIDs(pair[0], dismissed), http.StatusConflict},
		{"evidence-less member", e.owner, []string{pair[0].Disp.String(), evidenceless.String()}, http.StatusConflict},
		{"invalid id", e.owner, []string{"nope"}, http.StatusBadRequest},
		{"no ids", e.owner, []string{}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := e.fileGroup(tc.user, map[string]any{"ids": tc.ids})
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.want, rr.Body.String())
			}
			if n := e.fake.calls(); n != 0 {
				t.Fatalf("a refused request reached the forge %d times", n)
			}
			// All-or-nothing: the valid member of a refused request must not be left claimed.
			e.requireOpen(pair[0], pair[1], ownerOnly)
			if n := e.opsForOwner(); n != 0 {
				t.Fatalf("%d operations were created by a refused request", n)
			}
		})
	}
	// A foreign draft read is the same 404 as an unknown one.
	if rr := e.groupDraft(e.other, fgIDs(pair...)); rr.Code != http.StatusNotFound {
		t.Errorf("foreign draft: %d, want 404", rr.Code)
	}
	if rr := e.groupDraft(e.owner, fgIDs(pair[0], evidenceless2(e))); rr.Code != http.StatusConflict {
		t.Errorf("evidence-less draft: %d, want 409", rr.Code)
	}
}

// evidenceless2 seeds a second evidence-less open disposition and returns its id.
func evidenceless2(e *fgEnv) fgMember {
	id := uuid.New()
	mustExecT(e.ctx, e.t, e.pool, `INSERT INTO finding_dispositions (id,user_id,repo_id,location,status) VALUES ($1,$2,$3,$4,'open')`,
		id, e.owner.ID, e.repoID, e.prefix+"no-evidence-2-"+id.String()[:6]+".go")
	return fgMember{Disp: id}
}

func TestFileFindingGroupSecondClaimOfSameMembersRefusedLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(2)
	if rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}); rr.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", rr.Code, rr.Body.String())
	}
	if rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}); rr.Code != http.StatusConflict {
		t.Errorf("re-filing filed members: %d, want 409", rr.Code)
	}
	if e.fake.createCount() != 1 {
		t.Errorf("CreateIssue calls = %d, want 1", e.fake.createCount())
	}
}

// ── older evidence resolves to the coordinate's disposition ──────────────────────────────────

func TestFindingIssueDraftOlderEvidenceResolvesCoordinateDispositionLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	m := e.seedMember(e.repoID, "first sighting")
	// A newer sighting of the same coordinate by a later run; the first row is now the OLDER evidence.
	run2 := uuid.New()
	mustExecT(e.ctx, t, e.pool, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
		VALUES ($1,$2,$3,44,'Later','d','completed','issue')`, run2, e.owner.ID, e.repoID)
	newer, err := e.q.InsertFinding(e.ctx, store.InsertFindingParams{
		RunID: run2, UserID: e.owner.ID, RepoID: e.repoID, Location: m.Location, Title: m.Title,
		DescriptionMd: "second sighting", Labels: []byte(`[]`), Confidence: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(e.ctx, t, e.pool, `UPDATE findings SET created_at = now() - interval '1 hour' WHERE id=$1`, m.Finding)

	for name, id := range map[string]uuid.UUID{"older evidence": m.Finding, "latest evidence": newer.ID} {
		rr := httptest.NewRecorder()
		e.h.GetFindingIssueDraft(rr, fgRequest(e.owner, http.MethodGet, "/x", nil, map[string]string{"id": id.String()}))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d; body=%s", name, rr.Code, rr.Body.String())
		}
		var dto apitypes.IncidentalFindingIssueDraftDTO
		if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
			t.Fatal(err)
		}
		if dto.DispositionID != m.Disp.String() {
			t.Errorf("%s: disposition_id = %q, want the coordinate's %s", name, dto.DispositionID, m.Disp)
		}
	}
	// A stranger gets the same 404 as for an unknown id.
	rr := httptest.NewRecorder()
	e.h.GetFindingIssueDraft(rr, fgRequest(e.other, http.MethodGet, "/x", nil, map[string]string{"id": m.Finding.String()}))
	if rr.Code != http.StatusNotFound {
		t.Errorf("foreign evidence: %d, want 404", rr.Code)
	}
	// The group draft, keyed on the disposition id, uses the LATEST evidence.
	dr := e.groupDraft(e.owner, []string{m.Disp.String()})
	if dr.Code != http.StatusOK || !strings.Contains(dr.Body.String(), "second sighting") {
		t.Errorf("group draft: %d %s", dr.Code, dr.Body.String())
	}
}

// ── concurrency: exactly one writer wins the members ─────────────────────────────────────────

const fgRaceIterations = 20

// raceGroupAgainst files a two-member group while another action targets member 0, many times, and
// hands each outcome to check. start gates both goroutines so they overlap as much as possible.
func raceGroupAgainst(t *testing.T, e *fgEnv, other func(m fgMember) int, check func(iter int, m0, m1 fgMember, groupCode, otherCode int, groupIID int64, createsBefore int)) {
	t.Helper()
	for i := 0; i < fgRaceIterations; i++ {
		ms := e.members(2)
		before := e.fake.createCount()
		var groupCode, otherCode int
		var groupIID int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
			groupCode = rr.Code
			if rr.Code == http.StatusCreated {
				groupIID = fgResult(t, rr).Issue.IID
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			otherCode = other(ms[0])
		}()
		close(start)
		wg.Wait()
		if groupCode != http.StatusCreated && groupCode != http.StatusConflict {
			t.Fatalf("iter %d: group status %d, want 201 or 409", i, groupCode)
		}
		check(i, ms[0], ms[1], groupCode, otherCode, groupIID, before)
		// Whatever happened, nothing is left mid-claim and no operation is pending.
		var stuck int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM finding_dispositions WHERE user_id=$1 AND (status='filing' OR group_operation_id IS NOT NULL)`, e.owner.ID).Scan(&stuck); err != nil {
			t.Fatal(err)
		}
		if stuck != 0 {
			t.Fatalf("iter %d: %d dispositions left claimed", i, stuck)
		}
		if n, _, err := store.FindingGroupPendingStats(e.ctx, e.pool, e.owner.ID); err != nil || n != 0 {
			t.Fatalf("iter %d: pending operations = %d (%v)", i, n, err)
		}
	}
}

func TestFileFindingGroupVersusSingleFileLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	var groupWins, singleWins int
	raceGroupAgainst(t, e, func(m fgMember) int {
		rr := httptest.NewRecorder()
		e.h.FileFinding(rr, fileFindingReq(e.owner, m.Finding, nil))
		return rr.Code
	}, func(i int, m0, m1 fgMember, groupCode, singleCode int, groupIID int64, before int) {
		if (groupCode == http.StatusCreated) == (singleCode == http.StatusCreated) {
			t.Fatalf("iter %d: group=%d single=%d, want exactly one 201", i, groupCode, singleCode)
		}
		if e.fake.createCount() != before+1 {
			t.Fatalf("iter %d: %d CreateIssue calls, want exactly 1", i, e.fake.createCount()-before)
		}
		if groupCode == http.StatusCreated {
			groupWins++
			if singleCode != http.StatusConflict {
				t.Fatalf("iter %d: losing single file = %d, want 409", i, singleCode)
			}
			e.requireFiled(groupIID, m0, m1)
			return
		}
		singleWins++
		d0 := e.disp(m0.Disp)
		if d0.Status != "filed" || d0.IID == nil {
			t.Fatalf("iter %d: member 0 = %+v, want filed by the single file", i, d0)
		}
		e.requireOpen(m1)
	})
	t.Logf("group won %d, single won %d", groupWins, singleWins)
}

func TestFileFindingGroupVersusDismissLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	raceGroupAgainst(t, e, func(m fgMember) int {
		rr := httptest.NewRecorder()
		e.h.DismissFinding(rr, dismissFindingReq(e.owner, m.Finding, map[string]any{"reason": "wont_do"}))
		return rr.Code
	}, func(i int, m0, m1 fgMember, groupCode, dismissCode int, groupIID int64, before int) {
		if (groupCode == http.StatusCreated) == (dismissCode == http.StatusOK) {
			t.Fatalf("iter %d: group=%d dismiss=%d, want exactly one winner", i, groupCode, dismissCode)
		}
		if groupCode == http.StatusCreated {
			if e.fake.createCount() != before+1 {
				t.Fatalf("iter %d: creates %d, want 1", i, e.fake.createCount()-before)
			}
			e.requireFiled(groupIID, m0, m1)
			return
		}
		if e.fake.createCount() != before {
			t.Fatalf("iter %d: the dismissed group reached the forge", i)
		}
		if d0 := e.disp(m0.Disp); d0.Status != "dismissed" {
			t.Fatalf("iter %d: member 0 = %+v, want dismissed", i, d0)
		}
		e.requireOpen(m1)
	})
}

func TestFileFindingGroupVersusMarkDoneLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	raceGroupAgainst(t, e, func(m fgMember) int {
		rr := httptest.NewRecorder()
		e.h.MarkFindingDone(rr, fgRequest(e.owner, http.MethodPost, "/x", nil, map[string]string{"id": m.Finding.String()}))
		return rr.Code
	}, func(i int, m0, m1 fgMember, groupCode, doneCode int, groupIID int64, before int) {
		switch {
		case groupCode == http.StatusCreated:
			// Done either lost (409 while filing) or ran after settlement, which keeps the issue link.
			if e.fake.createCount() != before+1 {
				t.Fatalf("iter %d: creates %d, want 1", i, e.fake.createCount()-before)
			}
			switch doneCode {
			case http.StatusConflict:
				e.requireFiled(groupIID, m0, m1)
			case http.StatusOK:
				d0 := e.disp(m0.Disp)
				if d0.Status != "done" || d0.IID == nil || *d0.IID != groupIID {
					t.Fatalf("iter %d: done after settle = %+v, want done keeping #%d", i, d0, groupIID)
				}
				e.requireFiled(groupIID, m1)
			default:
				t.Fatalf("iter %d: done = %d beside a filed group", i, doneCode)
			}
		case doneCode == http.StatusOK:
			if e.fake.createCount() != before {
				t.Fatalf("iter %d: a refused group reached the forge", i)
			}
			if d0 := e.disp(m0.Disp); d0.Status != "done" || d0.IID != nil {
				t.Fatalf("iter %d: member 0 = %+v, want a plain done", i, d0)
			}
			e.requireOpen(m1)
		default:
			t.Fatalf("iter %d: group=%d done=%d, neither won", i, groupCode, doneCode)
		}
	})
}

// Two groups sharing one member: exactly one claims it, the other is refused whole (its unshared
// member is not left claimed), and only one issue reaches the forge. Sorted row locks also mean the
// overlapping claims cannot deadlock each other.
func TestFileFindingGroupOverlappingGroupsLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	for i := 0; i < fgRaceIterations; i++ {
		ms := e.members(3)
		before := e.fake.createCount()
		codes := make([]int, 2)
		iids := make([]int64, 2)
		groups := [][]fgMember{{ms[0], ms[1]}, {ms[1], ms[2]}}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := range groups {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(groups[g]...)})
				codes[g] = rr.Code
				if rr.Code == http.StatusCreated {
					iids[g] = fgResult(t, rr).Issue.IID
				}
			}()
		}
		close(start)
		wg.Wait()
		if (codes[0] == http.StatusCreated) == (codes[1] == http.StatusCreated) {
			t.Fatalf("iter %d: codes %v, want exactly one 201", i, codes)
		}
		if e.fake.createCount() != before+1 {
			t.Fatalf("iter %d: %d creates, want 1", i, e.fake.createCount()-before)
		}
		win, lose := 0, 1
		if codes[1] == http.StatusCreated {
			win, lose = 1, 0
		}
		if codes[lose] != http.StatusConflict {
			t.Fatalf("iter %d: loser = %d, want 409", i, codes[lose])
		}
		e.requireFiled(iids[win], groups[win]...)
		for _, m := range groups[lose] {
			shared := m.Disp == ms[1].Disp
			if !shared {
				e.requireOpen(m)
			}
		}
	}
}

// Failures after the claim but before the forge call release the claim definitively: the group
// is fileable again and the forge never sees a CreateIssue.
func TestFileFindingGroupPreCallFailuresReleaseLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	t.Run("blank title", func(t *testing.T) {
		ms := e.members(2)
		rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...), "title": " / \n "})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		e.requireOpen(ms...)
	})
	t.Run("oversized description", func(t *testing.T) {
		ms := e.members(2)
		big := strings.Repeat("x", workersvc.MaxIssueDescriptionBytes+1)
		if rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...), "description": big}); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		e.requireOpen(ms...)
	})
	t.Run("label ensure fails", func(t *testing.T) {
		ms := e.members(2)
		e.fake.mu.Lock()
		e.fake.ensureErr = errors.New("label service unavailable")
		e.fake.mu.Unlock()
		rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
		e.fake.mu.Lock()
		e.fake.ensureErr = nil
		e.fake.mu.Unlock()
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
		}
		if e.fake.createCount() != 0 {
			t.Fatalf("CreateIssue reached after a failed label ensure")
		}
		e.requireOpen(ms...)
		// And the same members file once the forge recovers.
		if rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}); rr.Code != http.StatusCreated {
			t.Fatalf("retry = %d %s", rr.Code, rr.Body.String())
		}
	})
}

// ── forge outcomes ───────────────────────────────────────────────────────────────────────────

func TestFileFindingGroupDefinitiveRejectionReleasesLiveDB(t *testing.T) {
	forgeStub := &fgHTTPForge{status: http.StatusUnprocessableEntity}
	e := newFGEnv(t, forgeStub)
	ms := e.members(2)

	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if forgeStub.createN() != 1 {
		t.Fatalf("forge saw %d creates, want 1", forgeStub.createN())
	}
	e.requireOpen(ms...)
	var phase string
	if err := e.pool.QueryRow(e.ctx, `SELECT phase FROM finding_group_operations WHERE user_id=$1`, e.owner.ID).Scan(&phase); err != nil || phase != "released" {
		t.Fatalf("operation phase = %q (%v), want released", phase, err)
	}
	// Refileable once the forge accepts.
	forgeStub.setStatus(http.StatusCreated)
	rr = e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
	}
	e.requireFiled(300, ms...)
}

func TestFileFindingGroupUncertainOutcomeStaysClaimedLiveDB(t *testing.T) {
	forgeStub := &fgHTTPForge{status: http.StatusForbidden} // 403 may be an abuse limit: the outcome is unknown
	e := newFGEnv(t, forgeStub)
	ms := e.members(2)

	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	res := fgResult(t, rr)
	op := fgOp(t, res)
	if res.Phase != "returned_uncertain" || res.Issue != nil {
		t.Fatalf("result = %+v", res)
	}
	e.requireClaimed(op, ms...)
	if phase, iid := e.opPhase(op); phase != "returned_uncertain" || iid != nil {
		t.Errorf("operation = %s iid=%v", phase, iid)
	}
	// Single filing and dismissal of a claimed member are refused, and a release before the deadline is too.
	rr = httptest.NewRecorder()
	e.h.FileFinding(rr, fileFindingReq(e.owner, ms[0].Finding, nil))
	if rr.Code != http.StatusConflict {
		t.Errorf("single file of a claimed member: %d, want 409", rr.Code)
	}
	rr = httptest.NewRecorder()
	e.h.DismissFinding(rr, dismissFindingReq(e.owner, ms[0].Finding, map[string]any{"reason": "wont_do"}))
	if rr.Code != http.StatusConflict {
		t.Errorf("dismiss of a claimed member: %d, want 409", rr.Code)
	}
	if rr := e.release(e.owner, op.String(), true); rr.Code != http.StatusConflict {
		t.Errorf("release before the deadline: %d, want 409", rr.Code)
	}
	e.requireClaimed(op, ms...)
}

// A generic timeout, then a reconciler pass that finds nothing, leave the group claimed even past
// its deadline: only the owner's explicit, confirmed release reopens it.
func TestFileFindingGroupTimeoutAndReconcilerNoMatchStayClaimedLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(2)
	e.fake.createErr = fmt.Errorf("create issue: %w", context.DeadlineExceeded)

	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	op := fgOp(t, fgResult(t, rr))
	e.requireClaimed(op, ms...)

	e.expireOp(op)
	if _, err := e.svc.FullSync(e.ctx, e.repoID, fgProjectID, e.fake); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if _, err := e.svc.IncrementalSync(e.ctx, e.repoID, fgProjectID, e.fake, forgesvc.Marks{}); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	e.requireClaimed(op, ms...)
	if phase, _ := e.opPhase(op); phase != "returned_uncertain" {
		t.Errorf("phase = %s, want returned_uncertain untouched", phase)
	}
	// The stranded-claim sweeper skips group members entirely (its count may include other
	// tests' leftover single-file claims in the shared database, so only the members are asserted).
	if _, err := e.q.SweepStrandedFilingFindings(e.ctx, pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	e.requireClaimed(op, ms...)

	if rr := e.release(e.owner, op.String(), true); rr.Code != http.StatusOK {
		t.Fatalf("confirmed release after deadline: %d %s", rr.Code, rr.Body.String())
	}
	e.requireOpen(ms...)
}

// ── settlement failure and marker-based recovery ─────────────────────────────────────────────

func TestFileFindingGroupSettleFailureRecoversWithoutDuplicateLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(3)
	unblock := e.blockSettlement()

	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 with a warning; body=%s", rr.Code, rr.Body.String())
	}
	res := fgResult(t, rr)
	op := fgOp(t, res)
	if res.Phase != "issue_recorded" || res.Warning == "" || res.Issue == nil {
		t.Fatalf("result = %+v", res)
	}
	if phase, iid := e.opPhase(op); phase != "issue_recorded" || iid == nil || *iid != res.Issue.IID {
		t.Fatalf("operation = %s iid=%v", phase, iid)
	}
	e.requireClaimed(op, ms...) // recorded issue identity but not settled: still claimed
	// A recorded issue can never be released, deadline or not.
	e.expireOp(op)
	if rr := e.release(e.owner, op.String(), true); rr.Code != http.StatusConflict {
		t.Fatalf("release of a recorded operation: %d, want 409", rr.Code)
	}

	unblock()
	if _, err := e.svc.FullSync(e.ctx, e.repoID, fgProjectID, e.fake); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	e.requireFiled(res.Issue.IID, ms...)
	if phase, _ := e.opPhase(op); phase != "settled" {
		t.Errorf("operation phase = %s, want settled", phase)
	}
	if e.fake.createCount() != 1 {
		t.Errorf("CreateIssue calls = %d, want 1 (no duplicate)", e.fake.createCount())
	}
}

func TestFileFindingGroupUncertainRecoversFromMarkerLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(3)
	e.fake.createErr, e.fake.storeOnErr = errors.New("read tcp: connection reset by peer"), true

	rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	op := fgOp(t, fgResult(t, rr))
	e.requireClaimed(op, ms...)

	if _, err := e.svc.FullSync(e.ctx, e.repoID, fgProjectID, e.fake); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	e.fake.mu.Lock()
	iid := e.fake.issues[0].IID
	e.fake.mu.Unlock()
	e.requireFiled(iid, ms...)
	if phase, got := e.opPhase(op); phase != "settled" || got == nil || *got != iid {
		t.Errorf("operation = %s iid=%v, want settled #%d", phase, got, iid)
	}
	// A second pass and a re-filing attempt neither duplicate nor disturb it.
	if _, err := e.svc.FullSync(e.ctx, e.repoID, fgProjectID, e.fake); err != nil {
		t.Fatal(err)
	}
	if rr := e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}); rr.Code != http.StatusConflict {
		t.Errorf("re-file after recovery: %d, want 409", rr.Code)
	}
	if e.fake.createCount() != 1 {
		t.Errorf("CreateIssue calls = %d, want 1", e.fake.createCount())
	}
}

// ── releasing an operation ───────────────────────────────────────────────────────────────────

// claimDirect claims n fresh members with a far-future deadline, leaving the operation in pre_call.
func (e *fgEnv) claimDirect(n int) (store.FindingGroupClaimOperation, []fgMember) {
	e.t.Helper()
	ms := e.members(n)
	ids := make([]uuid.UUID, n)
	for i, m := range ms {
		ids[i] = m.Disp
	}
	op, _, err := store.ClaimFindingGroup(e.ctx, e.pool, e.owner.ID, ids, time.Now().Add(time.Hour))
	if err != nil {
		e.t.Fatalf("claim: %v", err)
	}
	return op, ms
}

func TestReleaseFindingGroupRefusalsLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)

	t.Run("settled operation", func(t *testing.T) {
		ms := e.members(2)
		res := fgResult(t, e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}))
		e.expireOp(fgOp(t, res)) // even after a "deadline"
		if rr := e.release(e.owner, res.OperationID, true); rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
		}
		e.requireFiled(res.Issue.IID, ms...)
	})
	t.Run("recorded forge iid and url", func(t *testing.T) {
		op, ms := e.claimDirect(2)
		mustExecT(e.ctx, t, e.pool, `UPDATE finding_group_operations SET phase='issue_recorded', issue_iid=77, issue_url='https://forge.e2e/i/77', deadline_at=now()-interval '1 minute' WHERE id=$1`, op.ID)
		if rr := e.release(e.owner, op.ID.String(), true); rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
		}
		e.requireClaimed(op.ID, ms...)
	})
	t.Run("uncertain operation with a recorded forge iid", func(t *testing.T) {
		// Pins the issue_iid guard on its own: the phase is one release accepts, the deadline
		// has passed, and only the recorded identity stands in the way.
		op, ms := e.claimDirect(2)
		mustExecT(e.ctx, t, e.pool, `UPDATE finding_group_operations SET phase='returned_uncertain', issue_iid=78, issue_url='https://forge.e2e/i/78', deadline_at=now()-interval '1 minute' WHERE id=$1`, op.ID)
		if rr := e.release(e.owner, op.ID.String(), true); rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
		}
		e.requireClaimed(op.ID, ms...)
		if phase, iid := e.opPhase(op.ID); phase != "returned_uncertain" || iid == nil || *iid != 78 {
			t.Errorf("operation = %s iid=%v, want untouched", phase, iid)
		}
	})
	t.Run("another user's operation", func(t *testing.T) {
		op, ms := e.claimDirect(2)
		e.expireOp(op.ID)
		if rr := e.release(e.other, op.ID.String(), true); rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
		}
		e.requireClaimed(op.ID, ms...)
	})
	t.Run("unconfirmed", func(t *testing.T) {
		op, ms := e.claimDirect(1)
		e.expireOp(op.ID)
		if rr := e.release(e.owner, op.ID.String(), false); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		e.requireClaimed(op.ID, ms...)
	})
	t.Run("unknown operation", func(t *testing.T) {
		if rr := e.release(e.owner, uuid.NewString(), true); rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
	t.Run("malformed operation id", func(t *testing.T) {
		if rr := e.release(e.owner, "nope", true); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})
}

func TestReleaseFindingGroupHonorsDurableDeadlineLiveDB(t *testing.T) {
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	for _, phase := range []string{"pre_call", "in_flight", "returned_uncertain"} {
		t.Run(phase, func(t *testing.T) {
			e := newFGEnv(t, nil)
			op, ms := e.claimDirect(2)
			// Crash-left states, set up directly: the handler that owned the claim is gone.
			mustExecT(e.ctx, t, e.pool, `UPDATE finding_group_operations SET phase=$2 WHERE id=$1`, op.ID, phase)

			rr := e.release(e.owner, op.ID.String(), true)
			if rr.Code != http.StatusConflict {
				t.Fatalf("before the deadline: %d, want 409; body=%s", rr.Code, rr.Body.String())
			}
			e.requireClaimed(op.ID, ms...)

			e.expireOp(op.ID)
			rr = e.release(e.owner, op.ID.String(), true)
			if rr.Code != http.StatusOK {
				t.Fatalf("after the deadline: %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			e.requireOpen(ms...)
			if got, _ := e.opPhase(op.ID); got != "released" {
				t.Errorf("phase = %s, want released", got)
			}
			if rr := e.release(e.owner, op.ID.String(), true); rr.Code != http.StatusConflict {
				t.Errorf("second release: %d, want 409", rr.Code)
			}
		})
	}
}

// A handler paused inside EnsureLabels while the operation is released (deadline passed) must not
// create the issue when it resumes: the durable in_flight write is the gate, not the local context.
func TestFileFindingGroupPausedThenReleasedNeverCreatesLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(2)
	e.fake.ensureEntered = make(chan struct{}, 1)
	e.fake.ensureGate = make(chan struct{})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}) }()
	select {
	case <-e.fake.ensureEntered:
	case <-time.After(20 * time.Second):
		t.Fatal("handler never reached EnsureLabels")
	}
	var op uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM finding_group_operations WHERE user_id=$1`, e.owner.ID).Scan(&op); err != nil {
		t.Fatal(err)
	}
	e.expireOp(op)
	if rr := e.release(e.owner, op.String(), true); rr.Code != http.StatusOK {
		t.Fatalf("release while the handler is paused: %d %s", rr.Code, rr.Body.String())
	}
	e.requireOpen(ms...)
	close(e.fake.ensureGate)

	rr := <-done
	if rr.Code != http.StatusAccepted {
		t.Fatalf("resumed handler: %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if e.fake.createCount() != 0 {
		t.Fatalf("a released operation reached CreateIssue %d times", e.fake.createCount())
	}
	e.requireOpen(ms...)
	if phase, _ := e.opPhase(op); phase != "released" {
		t.Errorf("phase = %s, want released", phase)
	}
}

// Variant: the deadline lapses while paused but nobody releases. The resumed handler still must not
// call the forge, and the group stays claimed for the owner to release.
func TestFileFindingGroupPausedPastDeadlineNeverCreatesLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(2)
	e.fake.ensureEntered = make(chan struct{}, 1)
	e.fake.ensureGate = make(chan struct{})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}) }()
	select {
	case <-e.fake.ensureEntered:
	case <-time.After(20 * time.Second):
		t.Fatal("handler never reached EnsureLabels")
	}
	var op uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM finding_group_operations WHERE user_id=$1`, e.owner.ID).Scan(&op); err != nil {
		t.Fatal(err)
	}
	e.expireOp(op)
	close(e.fake.ensureGate)

	select {
	case rr := <-done:
		if rr.Code != http.StatusAccepted {
			t.Fatalf("resumed handler: %d, want 202; body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("resumed handler never returned")
	}
	if e.fake.createCount() != 0 {
		t.Fatalf("CreateIssue reached after the durable deadline: %d", e.fake.createCount())
	}
	e.requireClaimed(op, ms...)
	if phase, _ := e.opPhase(op); phase != "pre_call" {
		t.Errorf("phase = %s, want pre_call", phase)
	}
}

// ── close -> Done and Undo ───────────────────────────────────────────────────────────────────

func TestGroupIssueCloseMovesEveryMemberDoneUndoIsMemberSpecificLiveDB(t *testing.T) {
	e := newFGEnv(t, nil)
	ms := e.members(3)
	res := fgResult(t, e.fileGroup(e.owner, map[string]any{"ids": fgIDs(ms...)}))
	if res.Issue == nil {
		t.Fatal("group did not file")
	}
	iid := res.Issue.IID
	mustExecT(e.ctx, t, e.pool, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		VALUES ($1,$2,'grouped','closed','[]'::jsonb,'https://x',false,now(),now())`, e.repoID, iid)

	if err := e.svc.SyncFindingIssueCloses(e.ctx, e.repoID); err != nil {
		t.Fatalf("SyncFindingIssueCloses: %v", err)
	}
	for _, m := range ms {
		d := e.disp(m.Disp)
		if d.Status != "done" || !d.SetVia.Valid || d.SetVia.String != "issue_close" || d.IID == nil || *d.IID != iid {
			t.Fatalf("member %s = %+v, want done via issue_close keeping #%d", m.Location, d, iid)
		}
	}

	rr := httptest.NewRecorder()
	e.h.UndoFindingDisposition(rr, fgRequest(e.owner, http.MethodDelete, "/x", nil, map[string]string{"id": ms[0].Disp.String()}))
	if rr.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rr.Code, rr.Body.String())
	}
	if d := e.disp(ms[0].Disp); d.Status != "filed" || d.IID == nil || *d.IID != iid {
		t.Errorf("undone member = %+v, want filed #%d", d, iid)
	}
	for _, m := range ms[1:] {
		if d := e.disp(m.Disp); d.Status != "done" {
			t.Errorf("sibling %s = %+v, want still done", m.Location, d)
		}
	}
	// The consumed close edge is not replayed over the Undo.
	if err := e.svc.SyncFindingIssueCloses(e.ctx, e.repoID); err != nil {
		t.Fatal(err)
	}
	if d := e.disp(ms[0].Disp); d.Status != "filed" {
		t.Errorf("undone member re-resolved: %+v", d)
	}
}
