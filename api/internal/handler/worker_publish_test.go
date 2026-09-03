package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// publishStore backs the M8 checkpoint-publish handler tests: it answers the
// ownership lookup and the run-claim-context lookup the service reaches, and
// nothing else.
type publishStore struct {
	workersvc.Store
	ownedRun store.Run
	ownedErr error
	claim    store.GetRunClaimContextRow
	claimErr error
}

func (p *publishStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return p.ownedRun, p.ownedErr
}
func (p *publishStore) GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error) {
	return p.claim, p.claimErr
}

// SetRunCheckpointTip is the best-effort tip persist a successful publish makes
// (PRD #1042 M2). The handler tests only need it to not fault, so it reports one
// row moved and never errors.
func (p *publishStore) SetRunCheckpointTip(context.Context, store.SetRunCheckpointTipParams) (int64, error) {
	return 1, nil
}

// newPublishHandler builds a handler whose workersvc uses box (so a token sealed
// with the same box decrypts), with the SSRF gate and the go-git publisher wired to
// injectable stubs.
func newPublishHandler(t *testing.T, st workersvc.Store, box *secretbox.Box, allow func(string) bool, publish func(context.Context, pushbroker.Options) (pushbroker.Result, error)) *Handler {
	t.Helper()
	wsvc := workersvc.New(st, box, workersvc.Params{})
	wsvc.SetForgeBaseURLAllowed(allow)
	if publish != nil {
		wsvc.SetPublishFn(publish)
	}
	return &Handler{wsvc: wsvc}
}

func newBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

// publishReq builds a POST carrying an authed worker, an {id} route param, the tip
// header, and a raw body.
func publishReq(runID uuid.UUID, tip string, body io.Reader) *http.Request {
	req := reqWithWorkerAndParam(http.MethodPost, "/api/worker/x", body, runID)
	if tip != "" {
		req.Header.Set("X-Uzi-Checkpoint-Tip", tip)
	}
	return req
}

// reqWithWorkerAndParam mirrors workerReq (worker_protocol_test.go) but takes an
// io.Reader body rather than a string, so the oversize case can stream.
func reqWithWorkerAndParam(method, target string, body io.Reader, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, target, body)
	ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()})
	if runID != uuid.Nil {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

const validTip = "0123456789abcdef0123456789abcdef01234567"

func TestWorkerRunPublishForeignRun404(t *testing.T) {
	st := &publishStore{ownedErr: pgx.ErrNoRows}
	h := newPublishHandler(t, st, newBox(t), func(string) bool { return true }, nil)
	rec := httptest.NewRecorder()
	h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("pack")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunPublishBadTip400(t *testing.T) {
	h := newPublishHandler(t, &publishStore{}, newBox(t), func(string) bool { return true }, nil)
	for _, tip := range []string{"", "nothex", "0123456789ABCDEF0123456789abcdef01234567", "0123"} {
		rec := httptest.NewRecorder()
		h.WorkerRunPublish(rec, publishReq(uuid.New(), tip, strings.NewReader("pack")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("tip %q: status = %d, want 400", tip, rec.Code)
		}
	}
}

func TestWorkerRunPublishOversize413(t *testing.T) {
	// A body that streams past maxPackBytes must be a truthful 413, never a silently
	// truncated pack. infReader yields bytes lazily, so the test does not itself
	// allocate the whole over-cap body.
	h := newPublishHandler(t, &publishStore{}, newBox(t), func(string) bool { return true }, nil)
	rec := httptest.NewRecorder()
	h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, infReader{}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body %q", rec.Code, rec.Body.String())
	}
}

// issueRun is a running ISSUE-kind run with its issue iid set but its branch column
// EMPTY — exactly the mid-run shape a checkpoint publish sees (runs.branch is written
// only at completion). The service must derive the branch agent/issue-<iid> from the
// iid, never from the (empty) branch column.
func issueRun(iid int64) store.Run {
	return store.Run{
		Kind:     "issue",
		IssueIid: pgtype.Int8{Int64: iid, Valid: true},
		Branch:   pgtype.Text{}, // NULL/empty mid-run
	}
}

// issueClaimRow is the run claim context for an issue run, including the repo's
// default branch (the delta pack's exclude boundary).
func issueClaimRow(sealed []byte) store.GetRunClaimContextRow {
	return store.GetRunClaimContextRow{
		RepoWebUrl:      "https://gitlab.example.com/team/repo",
		DefaultBranch:   pgtype.Text{String: "main", Valid: true},
		BaseUrl:         "https://gitlab.example.com",
		BotUsername:     "uzi-bot",
		TokenCiphertext: sealed,
	}
}

func TestWorkerRunPublishValid200(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	st := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
	var gotOpts pushbroker.Options
	publish := func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		gotOpts = o
		return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
	}
	h := newPublishHandler(t, st, box, func(u string) bool { return u == "https://gitlab.example.com" }, publish)

	rec := httptest.NewRecorder()
	h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("packbytes")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
	}
	if got["published"] != true {
		t.Fatalf("published = %v, want true", got["published"])
	}
	// The ref is SERVER-DERIVED from the issue iid (agent/issue-42), proving mid-run
	// publish works off an empty branch column.
	if got["ref"] != "refs/uzi-checkpoints/agent/issue-42" {
		t.Fatalf("ref = %v", got["ref"])
	}
	if _, ok := got["skipped"]; ok {
		t.Fatalf("a successful publish must omit skipped, got %q", rec.Body.String())
	}
	// The broker was handed SERVER-DERIVED coordinates, and the decrypted PAT — never
	// anything the worker named beyond the tip.
	if gotOpts.CloneURL != "https://gitlab.example.com/team/repo.git" {
		t.Fatalf("clone url = %q", gotOpts.CloneURL)
	}
	if gotOpts.Branch != "agent/issue-42" || gotOpts.Username != "uzi-bot" || gotOpts.PAT != "pat" {
		t.Fatalf("derived opts = %+v", gotOpts)
	}
	if gotOpts.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", gotOpts.DefaultBranch)
	}
	if gotOpts.DeclaredTip != validTip {
		t.Fatalf("declared tip = %q", gotOpts.DeclaredTip)
	}
}

// TestWorkerRunPublishSSRFReject500 proves the SSRF gate refuses a publish whose
// forge host is not allowlisted → 500 (best-effort, worker ignores it). A disallowed
// host must never be dialed.
func TestWorkerRunPublishSSRFReject500(t *testing.T) {
	box := newBox(t)
	sealed, _ := box.Seal([]byte("pat"))
	st := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
	dialed := false
	publish := func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		dialed = true
		return pushbroker.Result{}, nil
	}
	h := newPublishHandler(t, st, box, func(string) bool { return false }, publish)
	rec := httptest.NewRecorder()
	h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("pack")))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body %q", rec.Code, rec.Body.String())
	}
	if dialed {
		t.Fatal("SSRF-rejected publish still reached the go-git broker")
	}
}

// TestWorkerRunPublishDialedHostReject500 proves the SECOND SSRF gate — the host
// go-git actually DIALS (repo_web_url + ".git") — is the one doing the rejecting,
// not just the declared base_url gate. Here base_url IS allowlisted, but
// repo_web_url points at a DIFFERENT, non-allowlisted host; the publish must be
// refused (500) and the broker never reached. Without the dialed-host gate this
// would sail through the base_url check and dial the attacker host.
func TestWorkerRunPublishDialedHostReject500(t *testing.T) {
	box := newBox(t)
	sealed, _ := box.Seal([]byte("pat"))
	claim := issueClaimRow(sealed)
	claim.BaseUrl = "https://gitlab.example.com"            // allowlisted
	claim.RepoWebUrl = "https://evil.example.com/team/repo" // dialed host — NOT allowlisted
	st := &publishStore{ownedRun: issueRun(42), claim: claim}
	dialed := false
	publish := func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		dialed = true
		return pushbroker.Result{}, nil
	}
	// The allowlist admits ONLY the base_url host, so the base_url gate passes and the
	// dialed-host gate must be the one that rejects.
	allow := func(u string) bool { return u == "https://gitlab.example.com" }
	h := newPublishHandler(t, st, box, allow, publish)
	rec := httptest.NewRecorder()
	h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("pack")))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body %q", rec.Code, rec.Body.String())
	}
	if dialed {
		t.Fatal("a non-allowlisted DIALED host still reached the go-git broker")
	}
}

// TestWorkerRunPublishNonIssueSkip proves a non-issue-kind run (e.g. ci_fix) is a
// 200 best-effort skip "unsupported" — checkpoints only fire for issue runs — and
// never reaches the broker.
func TestWorkerRunPublishNonIssueSkip(t *testing.T) {
	box := newBox(t)
	sealed, _ := box.Seal([]byte("pat"))
	st := &publishStore{
		ownedRun: store.Run{Kind: "ci_fix", IssueIid: pgtype.Int8{}},
		claim:    issueClaimRow(sealed),
	}
	dialed := false
	publish := func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		dialed = true
		return pushbroker.Result{}, nil
	}
	h := newPublishHandler(t, st, box, func(string) bool { return true }, publish)
	rec := httptest.NewRecorder()
	h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("pack")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 skip, body %q", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["published"] != false || got["skipped"] != "unsupported" {
		t.Fatalf("body = %q, want published=false skipped=unsupported", rec.Body.String())
	}
	if dialed {
		t.Fatal("a non-issue run still reached the go-git broker")
	}
}

func TestWorkerRunPublishSkipMappings(t *testing.T) {
	box := newBox(t)
	sealed, _ := box.Seal([]byte("pat"))
	base := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
	cases := []struct {
		name    string
		err     error
		skipped string
	}{
		{"not_descendant", pushbroker.ErrNotDescendant, "not_descendant"},
		{"unsupported", pushbroker.ErrTipMissing, "unsupported"},
		{"too_large", pushbroker.ErrPackTooLarge, "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			publish := func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				return pushbroker.Result{}, tc.err
			}
			h := newPublishHandler(t, base, box, func(string) bool { return true }, publish)
			rec := httptest.NewRecorder()
			h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("pack")))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 skip, body %q", rec.Code, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["published"] != false || got["skipped"] != tc.skipped {
				t.Fatalf("body = %q, want published=false skipped=%s", rec.Body.String(), tc.skipped)
			}
		})
	}
}

// TestWorkerRunPublishGoldenWire pins the EXACT response bytes for a published and a
// skipped outcome, so a field rename/reorder that would break the agent's TS
// contract fails here. The agent pins its own TS golden separately — this is the api
// half, keys and order matching the M8 wire contract.
func TestWorkerRunPublishGoldenWire(t *testing.T) {
	box := newBox(t)
	sealed, _ := box.Seal([]byte("pat"))
	st := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
	cases := []struct {
		name string
		fn   func(context.Context, pushbroker.Options) (pushbroker.Result, error)
		want string
	}{
		{
			"published",
			func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
			},
			`{"published":true,"ref":"refs/uzi-checkpoints/agent/issue-42"}` + "\n",
		},
		{
			"skipped",
			func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				return pushbroker.Result{}, pushbroker.ErrNotDescendant
			},
			`{"published":false,"ref":"refs/uzi-checkpoints/agent/issue-42","skipped":"not_descendant"}` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPublishHandler(t, st, box, func(string) bool { return true }, tc.fn)
			rec := httptest.NewRecorder()
			h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, strings.NewReader("pack")))
			if rec.Body.String() != tc.want {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// infReader yields 'x' forever, so a test can exceed maxPackBytes without allocating
// the whole over-cap body up front.
type infReader struct{}

func (infReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
