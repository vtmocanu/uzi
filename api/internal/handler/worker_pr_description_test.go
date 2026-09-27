package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// prDescFakeStore is an in-memory, transactional stand-in for the PR-description tables: it
// implements the statements workersvc's fenced transaction runs (with the same WHERE guards as
// the SQL) plus the optional reader surface, so the stage -> bind -> ack seam EXECUTES through
// the handlers without a database. The live-DB suite (worker_pr_description_livedb_test.go and
// workersvc's) runs the real SQL.
type prDescFakeStore struct {
	workersvc.Store
	runs     map[uuid.UUID]store.Run
	versions map[uuid.UUID]store.PrDescriptionVersion
	prs      map[prDescFakeKey]store.PrDescription
	seq      int
}

type prDescFakeKey struct {
	repo uuid.UUID
	mr   int64
}

func newPrDescFakeStore() *prDescFakeStore {
	return &prDescFakeStore{
		runs:     map[uuid.UUID]store.Run{},
		versions: map[uuid.UUID]store.PrDescriptionVersion{},
		prs:      map[prDescFakeKey]store.PrDescription{},
	}
}

func (f *prDescFakeStore) GetRunOwnedByWorker(_ context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error) {
	r, ok := f.runs[arg.ID]
	if !ok || r.WorkerID != arg.WorkerID {
		return store.Run{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *prDescFakeStore) GetRunOwnedByWorkerForUpdate(ctx context.Context, arg store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error) {
	return f.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams(arg))
}

func (f *prDescFakeStore) InsertPrDescriptionVersion(_ context.Context, a store.InsertPrDescriptionVersionParams) (store.PrDescriptionVersion, error) {
	f.seq++
	v := store.PrDescriptionVersion{
		ID: uuid.New(), RunID: a.RunID, ClaimGeneration: a.ClaimGeneration, RepoID: a.RepoID, MrIid: a.MrIid,
		Fields: a.Fields, Size: a.Size, BaseSha: a.BaseSha, HeadSha: a.HeadSha, TargetBranch: a.TargetBranch,
		Source: a.Source, State: "pending",
		CreatedAt: pgtype.Timestamptz{Time: time.Unix(int64(1_700_000_000+f.seq), 0).UTC(), Valid: true},
	}
	f.versions[v.ID] = v
	return v, nil
}

func (f *prDescFakeStore) GetPrDescriptionVersionForRunForUpdate(_ context.Context, a store.GetPrDescriptionVersionForRunForUpdateParams) (store.PrDescriptionVersion, error) {
	v, ok := f.versions[a.ID]
	if !ok || v.RunID != a.RunID {
		return store.PrDescriptionVersion{}, pgx.ErrNoRows
	}
	return v, nil
}

func (f *prDescFakeStore) BindPrDescriptionVersion(_ context.Context, a store.BindPrDescriptionVersionParams) (store.PrDescriptionVersion, error) {
	v, ok := f.versions[a.ID]
	if !ok || v.RunID != a.RunID || v.State != "pending" || (v.MrIid.Valid && v.MrIid.Int64 != a.MrIid) {
		return store.PrDescriptionVersion{}, pgx.ErrNoRows
	}
	v.MrIid = pgtype.Int8{Int64: a.MrIid, Valid: true}
	v.RenderedRegionSha256 = pgtype.Text{String: a.RenderedRegionSha256, Valid: true}
	f.versions[v.ID] = v
	return v, nil
}

func (f *prDescFakeStore) EnsurePrDescription(_ context.Context, a store.EnsurePrDescriptionParams) error {
	k := prDescFakeKey{a.RepoID, a.MrIid}
	if _, ok := f.prs[k]; !ok {
		f.prs[k] = store.PrDescription{RepoID: a.RepoID, MrIid: a.MrIid}
	}
	return nil
}

func (f *prDescFakeStore) GetPrDescription(_ context.Context, a store.GetPrDescriptionParams) (store.PrDescription, error) {
	p, ok := f.prs[prDescFakeKey{a.RepoID, a.MrIid}]
	if !ok {
		return store.PrDescription{}, pgx.ErrNoRows
	}
	return p, nil
}

func (f *prDescFakeStore) GetPrDescriptionForUpdate(ctx context.Context, a store.GetPrDescriptionForUpdateParams) (store.PrDescription, error) {
	return f.GetPrDescription(ctx, store.GetPrDescriptionParams(a))
}

func (f *prDescFakeStore) GetPrDescriptionVersionByID(_ context.Context, id uuid.UUID) (store.PrDescriptionVersion, error) {
	v, ok := f.versions[id]
	if !ok {
		return store.PrDescriptionVersion{}, pgx.ErrNoRows
	}
	return v, nil
}

func (f *prDescFakeStore) FindPrDescriptionVersionByRegionHash(_ context.Context, a store.FindPrDescriptionVersionByRegionHashParams) (store.PrDescriptionVersion, error) {
	var best *store.PrDescriptionVersion
	for _, v := range f.versions {
		if v.RepoID == a.RepoID && v.MrIid.Valid && v.MrIid.Int64 == a.MrIid && v.State == a.State &&
			v.RenderedRegionSha256.Valid && v.RenderedRegionSha256.String == a.RenderedRegionSha256 {
			if best == nil || v.CreatedAt.Time.After(best.CreatedAt.Time) {
				c := v
				best = &c
			}
		}
	}
	if best == nil {
		return store.PrDescriptionVersion{}, pgx.ErrNoRows
	}
	return *best, nil
}

func (f *prDescFakeStore) LatestBoundPrDescriptionVersionForRun(_ context.Context, runID uuid.UUID) (store.PrDescriptionVersion, error) {
	var best *store.PrDescriptionVersion
	for _, v := range f.versions {
		if v.RunID == runID && v.MrIid.Valid && (best == nil || v.CreatedAt.Time.After(best.CreatedAt.Time)) {
			c := v
			best = &c
		}
	}
	if best == nil {
		return store.PrDescriptionVersion{}, pgx.ErrNoRows
	}
	return *best, nil
}

func (f *prDescFakeStore) setVersionState(id uuid.UUID, state string) int64 {
	v, ok := f.versions[id]
	if !ok || v.State != "pending" {
		return 0
	}
	v.State = state
	if state == "published" {
		v.PublishedAt = pgtype.Timestamptz{Time: time.Unix(1_800_000_000, 0).UTC(), Valid: true}
	}
	f.versions[id] = v
	return 1
}

func (f *prDescFakeStore) MarkPrDescriptionVersionPublished(_ context.Context, id uuid.UUID) (int64, error) {
	return f.setVersionState(id, "published"), nil
}

func (f *prDescFakeStore) MarkPrDescriptionVersionAbandoned(_ context.Context, id uuid.UUID) (int64, error) {
	return f.setVersionState(id, "abandoned"), nil
}

func (f *prDescFakeStore) SetPrDescriptionPublished(_ context.Context, a store.SetPrDescriptionPublishedParams) (int64, error) {
	k := prDescFakeKey{a.RepoID, a.MrIid}
	p, ok := f.prs[k]
	if !ok || p.LockVersion != a.ExpectedLockVersion {
		return 0, nil
	}
	p.PublishedVersionID = pgtype.UUID{Bytes: a.PublishedVersionID, Valid: true}
	p.LastOutcome = pgtype.Text{String: "published", Valid: true}
	p.LockVersion++
	f.prs[k] = p
	return 1, nil
}

func (f *prDescFakeStore) SetPrDescriptionOutcome(_ context.Context, a store.SetPrDescriptionOutcomeParams) (int64, error) {
	k := prDescFakeKey{a.RepoID, a.MrIid}
	p, ok := f.prs[k]
	if !ok || p.LockVersion != a.ExpectedLockVersion {
		return 0, nil
	}
	p.LastOutcome = pgtype.Text{String: a.LastOutcome, Valid: true}
	p.LockVersion++
	f.prs[k] = p
	return 1, nil
}

func (f *prDescFakeStore) RecoverPrDescriptionLostAck(_ context.Context, a store.RecoverPrDescriptionLostAckParams) (int64, error) {
	k := prDescFakeKey{a.RepoID, a.MrIid}
	p, ok := f.prs[k]
	if !ok {
		return 0, nil
	}
	p.PublishedVersionID = pgtype.UUID{Bytes: a.PublishedVersionID, Valid: true}
	p.LastOutcome = pgtype.Text{String: "published", Valid: true}
	f.prs[k] = p
	return 1, nil
}

// BeginPrDescTx opens a snapshot transaction: Rollback without Commit restores the tables.
func (f *prDescFakeStore) BeginPrDescTx(context.Context) (workersvc.PrDescTx, error) {
	snapV := make(map[uuid.UUID]store.PrDescriptionVersion, len(f.versions))
	for k, v := range f.versions {
		snapV[k] = v
	}
	snapP := make(map[prDescFakeKey]store.PrDescription, len(f.prs))
	for k, v := range f.prs {
		snapP[k] = v
	}
	return &prDescFakeTx{prDescFakeStore: f, snapV: snapV, snapP: snapP}, nil
}

type prDescFakeTx struct {
	*prDescFakeStore
	snapV map[uuid.UUID]store.PrDescriptionVersion
	snapP map[prDescFakeKey]store.PrDescription
	done  bool
}

func (t *prDescFakeTx) Commit(context.Context) error { t.done = true; return nil }
func (t *prDescFakeTx) Rollback(context.Context) error {
	if !t.done {
		t.versions, t.prs, t.done = t.snapV, t.snapP, true
	}
	return nil
}

// prDescTestRouter mounts the four PR-description handlers behind a middleware that
// authenticates the given worker, the way RequireWorker does on the real table.
func prDescTestRouter(st *prDescFakeStore, wkr store.Worker) http.Handler {
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), wkr)))
		})
	})
	r.Post("/api/worker/runs/{id}/pr-description/stage", h.WorkerStagePrDescription)
	r.Post("/api/worker/runs/{id}/pr-description/bind", h.WorkerBindPrDescription)
	r.Post("/api/worker/runs/{id}/pr-description/lookup", h.WorkerLookupPrDescription)
	r.Post("/api/worker/runs/{id}/pr-description/ack", h.WorkerAckPrDescription)
	return r
}

func prDescPost(t *testing.T, router http.Handler, runID uuid.UUID, op string, body any, out any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+runID.String()+"/pr-description/"+op, bytes.NewReader(b)))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s response: %v (%s)", op, err, rec.Body.String())
		}
	}
	return rec
}

func prDescReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Reason
}

const (
	prDescHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prDescHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func prDescStageBody(gen int64, summary string) apitypes.PrDescriptionStageRequest {
	return apitypes.PrDescriptionStageRequest{
		ClaimGeneration: i64(gen),
		Source:          "generated",
		Fields: apitypes.PrDescriptionFields{
			Summary: summary,
			Changes: []string{"API: a <b>new</b> route, see [docs](https://x.test)"},
			ScopeNotes: []apitypes.PrDescriptionScopeNote{
				{Kind: "deferred", Text: "the CLI half ships later"},
			},
			ReviewPointers: []string{"check the lock order"},
			Verification: []apitypes.PrDescriptionVerification{
				{Command: "task gate:api", Result: "pass", VerifiedAtSha: "0123abcd"},
			},
		},
		Size:         &apitypes.PrDescriptionSize{Files: 3, Code: apitypes.PrDescriptionSizeBucket{Added: 10, Deleted: 2}},
		BaseSha:      "1111111111111111111111111111111111111111",
		HeadSha:      "2222222222222222222222222222222222222222",
		TargetBranch: "main",
	}
}

// TestWorkerPrDescriptionSeamThroughRouter drives stage -> bind -> lookup -> ack -> a refresh
// run's stage/bind/ack through the mounted handlers: the seam the agent, web and CLI consume.
func TestWorkerPrDescriptionSeamThroughRouter(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	runID := uuid.New()
	st := newPrDescFakeStore()
	st.runs[runID] = store.Run{
		ID: runID, UserID: uid, RepoID: pgtype.UUID{Bytes: repoID, Valid: true}, Status: "running", Kind: "issue",
		WorkerID: pgtype.UUID{Bytes: wkr.ID, Valid: true}, ClaimGeneration: 3,
	}
	router := prDescTestRouter(st, wkr)

	// Stage: the response carries the SANITIZED fields.
	var staged apitypes.PrDescriptionStageResponse
	rec := prDescPost(t, router, runID, "stage", prDescStageBody(3, "Adds retries. Fixes #12 for @alice <!-- uzi:description:end -->"), &staged)
	if rec.Code != http.StatusOK {
		t.Fatalf("stage = %d; body=%s", rec.Code, rec.Body.String())
	}
	v := staged.Version
	if v.State != "pending" || v.MrIid != nil || v.RenderedRegionSha256 != nil || v.Source != "generated" {
		t.Fatalf("staged version = %+v", v)
	}
	if strings.Contains(v.Fields.Summary, "Fixes #12") || strings.Contains(v.Fields.Summary, "<!--") ||
		!strings.Contains(v.Fields.Summary, "@\u200Balice") {
		t.Fatalf("summary not sanitized: %q", v.Fields.Summary)
	}
	if v.Fields.Changes[0] != "API: a new route, see docs" {
		t.Fatalf("change not sanitized: %q", v.Fields.Changes[0])
	}
	if v.Size == nil || v.Size.Code.Added != 10 || v.Size.Files != 3 {
		t.Fatalf("size = %+v", v.Size)
	}

	// Bind: creates the PR row; nothing published yet.
	var bound apitypes.PrDescriptionBindResponse
	rec = prDescPost(t, router, runID, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(3), VersionID: v.ID, MrIid: 41, RenderedRegionSha256: prDescHashA,
	}, &bound)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind = %d; body=%s", rec.Code, rec.Body.String())
	}
	if bound.PR.MrIid != 41 || bound.PR.LockVersion != 0 || bound.PR.PublishedVersion != nil || bound.PR.LastOutcome != nil {
		t.Fatalf("bind pr state = %+v", bound.PR)
	}
	if bound.Version.MrIid == nil || *bound.Version.MrIid != 41 || *bound.Version.RenderedRegionSha256 != prDescHashA {
		t.Fatalf("bound version = %+v", bound.Version)
	}

	// Lookup before the ack: the forge region matches a PENDING version (a lost ack).
	var look apitypes.PrDescriptionLookupResponse
	rec = prDescPost(t, router, runID, "lookup", apitypes.PrDescriptionLookupRequest{ClaimGeneration: i64(3), MrIid: 41, RegionSha256: prDescHashA}, &look)
	if rec.Code != http.StatusOK || look.Match != "pending" || look.MatchedVersionID == nil || *look.MatchedVersionID != v.ID {
		t.Fatalf("lookup pending = %d %+v", rec.Code, look)
	}

	// A stale claim generation loses on every route.
	for _, op := range []string{"stage", "bind", "lookup", "ack"} {
		var body any
		switch op {
		case "stage":
			body = prDescStageBody(2, "x")
		case "bind":
			body = apitypes.PrDescriptionBindRequest{ClaimGeneration: i64(2), VersionID: v.ID, MrIid: 41, RenderedRegionSha256: prDescHashA}
		case "lookup":
			body = apitypes.PrDescriptionLookupRequest{ClaimGeneration: i64(2), MrIid: 41, RegionSha256: prDescHashA}
		case "ack":
			body = apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(2), VersionID: v.ID, Outcome: "published"}
		}
		rec = prDescPost(t, router, runID, op, body, nil)
		if rec.Code != http.StatusConflict || prDescReason(t, rec) != "stale_claim" {
			t.Fatalf("%s with a stale generation = %d %s, want 409 stale_claim", op, rec.Code, rec.Body.String())
		}
	}

	// Ack with a stale lock_version loses the compare-and-swap.
	rec = prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(3), VersionID: v.ID, Outcome: "published", ExpectedLockVersion: 5}, nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "lock_conflict" {
		t.Fatalf("stale lock ack = %d %s, want 409 lock_conflict", rec.Code, rec.Body.String())
	}

	// Ack published.
	var acked apitypes.PrDescriptionAckResponse
	rec = prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(3), VersionID: v.ID, Outcome: "published"}, &acked)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack = %d; body=%s", rec.Code, rec.Body.String())
	}
	if acked.PR.LockVersion != 1 || acked.PR.PublishedVersion == nil || acked.PR.PublishedVersion.ID != v.ID ||
		acked.PR.PublishedVersion.State != "published" || acked.PR.PublishedVersion.PublishedAt == nil ||
		acked.PR.LastOutcome == nil || *acked.PR.LastOutcome != "published" || acked.RecoveredVersionID != nil {
		t.Fatalf("ack state = %+v", acked)
	}
	// A retry of the same ack (lost response) is idempotent.
	rec = prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(3), VersionID: v.ID, Outcome: "published"}, &acked)
	if rec.Code != http.StatusOK || acked.PR.LockVersion != 1 {
		t.Fatalf("ack retry = %d %s, want an idempotent 200 at lock 1", rec.Code, rec.Body.String())
	}
	// Lookup now classifies the region as published.
	rec = prDescPost(t, router, runID, "lookup", apitypes.PrDescriptionLookupRequest{ClaimGeneration: i64(3), MrIid: 41, RegionSha256: prDescHashA}, &look)
	if rec.Code != http.StatusOK || look.Match != "published" || look.PR == nil || look.PR.LockVersion != 1 {
		t.Fatalf("lookup published = %d %+v", rec.Code, look)
	}
	rec = prDescPost(t, router, runID, "lookup", apitypes.PrDescriptionLookupRequest{ClaimGeneration: i64(3), MrIid: 41, RegionSha256: prDescHashB}, &look)
	if rec.Code != http.StatusOK || look.Match != "none" || look.MatchedVersionID != nil {
		t.Fatalf("lookup unknown = %d %+v", rec.Code, look)
	}

	// A refresh stages with the known mr_iid, binds, and a human edit is recorded as a skip that
	// leaves the published version untouched.
	var refresh apitypes.PrDescriptionStageResponse
	body := prDescStageBody(3, "Refreshed.")
	body.MrIid = i64(41)
	if rec = prDescPost(t, router, runID, "stage", body, &refresh); rec.Code != http.StatusOK || refresh.Version.MrIid == nil {
		t.Fatalf("refresh stage = %d %s", rec.Code, rec.Body.String())
	}
	if rec = prDescPost(t, router, runID, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(3), VersionID: refresh.Version.ID, MrIid: 41, RenderedRegionSha256: prDescHashB,
	}, &bound); rec.Code != http.StatusOK || bound.PR.PublishedVersion == nil || *bound.PR.PublishedVersion.RenderedRegionSha256 != prDescHashA {
		t.Fatalf("refresh bind = %d %s", rec.Code, rec.Body.String())
	}
	rec = prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{
		ClaimGeneration: i64(3), VersionID: refresh.Version.ID, Outcome: "skipped_human_edit", ExpectedLockVersion: 1,
	}, &acked)
	if rec.Code != http.StatusOK || acked.PR.PublishedVersion == nil || acked.PR.PublishedVersion.ID != v.ID ||
		*acked.PR.LastOutcome != "skipped_human_edit" || acked.PR.LockVersion != 2 {
		t.Fatalf("skip ack = %d %+v", rec.Code, acked)
	}
	if st.versions[uuid.MustParse(refresh.Version.ID)].State != "abandoned" {
		t.Fatal("a skipped version must be abandoned")
	}
	// Binding a version to a different PR than it was staged for is a version conflict.
	if rec = prDescPost(t, router, runID, "stage", body, &refresh); rec.Code != http.StatusOK {
		t.Fatalf("stage = %d", rec.Code)
	}
	rec = prDescPost(t, router, runID, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(3), VersionID: refresh.Version.ID, MrIid: 99, RenderedRegionSha256: prDescHashB,
	}, nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "version_conflict" {
		t.Fatalf("cross-PR bind = %d %s, want 409 version_conflict", rec.Code, rec.Body.String())
	}
}

func TestWorkerPrDescriptionValidation(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	runID := uuid.New()
	st := newPrDescFakeStore()
	st.runs[runID] = store.Run{
		ID: runID, UserID: uid, RepoID: pgtype.UUID{Bytes: repoID, Valid: true}, Status: "running", Kind: "issue",
		WorkerID: pgtype.UUID{Bytes: wkr.ID, Valid: true}, ClaimGeneration: 1,
	}
	router := prDescTestRouter(st, wkr)

	mut := func(f func(*apitypes.PrDescriptionStageRequest)) apitypes.PrDescriptionStageRequest {
		b := prDescStageBody(1, "ok")
		f(&b)
		return b
	}
	cases := []struct {
		name string
		body apitypes.PrDescriptionStageRequest
		want int
	}{
		{"missing generation", mut(func(b *apitypes.PrDescriptionStageRequest) { b.ClaimGeneration = nil }), http.StatusBadRequest},
		{"summary over raw cap", mut(func(b *apitypes.PrDescriptionStageRequest) {
			b.Fields.Summary = strings.Repeat("s", workersvc.MaxPrDescSummaryRawBytes+1)
		}), http.StatusBadRequest},
		{"change over raw cap", mut(func(b *apitypes.PrDescriptionStageRequest) {
			b.Fields.Changes = []string{strings.Repeat("c", workersvc.MaxPrDescItemRawBytes+1)}
		}), http.StatusBadRequest},
		{"unknown source", mut(func(b *apitypes.PrDescriptionStageRequest) { b.Source = "model" }), http.StatusBadRequest},
		{"bad head sha", mut(func(b *apitypes.PrDescriptionStageRequest) { b.HeadSha = "not-a-sha" }), http.StatusBadRequest},
		{"branch with newline", mut(func(b *apitypes.PrDescriptionStageRequest) { b.TargetBranch = "main\nx" }), http.StatusBadRequest},
		{"negative size", mut(func(b *apitypes.PrDescriptionStageRequest) { b.Size.Files = -1 }), http.StatusBadRequest},
		{"bad scope kind", mut(func(b *apitypes.PrDescriptionStageRequest) {
			b.Fields.ScopeNotes = []apitypes.PrDescriptionScopeNote{{Kind: "risk", Text: "t"}}
		}), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := prDescPost(t, router, runID, "stage", c.body, nil); rec.Code != c.want {
				t.Fatalf("%s = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			}
		})
	}
	if len(st.versions) != 0 {
		t.Fatalf("a refused stage must store nothing, got %d versions", len(st.versions))
	}

	// deterministic_only stores no text whatever was sent.
	var staged apitypes.PrDescriptionStageResponse
	body := prDescStageBody(1, "model text")
	body.Source = "deterministic_only"
	if rec := prDescPost(t, router, runID, "stage", body, &staged); rec.Code != http.StatusOK ||
		staged.Version.Fields.Summary != "" || len(staged.Version.Fields.Changes) != 0 || staged.Version.Fields.Changes == nil {
		t.Fatalf("deterministic_only stage = %d %+v", rec.Code, staged.Version.Fields)
	}

	// Not this worker's run → 404; a terminal run → 409 run_terminal; unknown version → 404.
	other := prDescTestRouter(st, store.Worker{ID: uuid.New(), UserID: uid})
	if rec := prDescPost(t, other, runID, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign worker stage = %d, want 404", rec.Code)
	}
	if rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(1), VersionID: uuid.NewString(), Outcome: "published"}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown version ack = %d, want 404", rec.Code)
	}
	if rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(1), VersionID: staged.Version.ID, Outcome: "shipped"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown outcome = %d, want 400", rec.Code)
	}
	if rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(1), VersionID: staged.Version.ID, Outcome: "published"}, nil); rec.Code != http.StatusConflict || prDescReason(t, rec) != "version_conflict" {
		t.Fatalf("ack of an unbound version = %d %s, want 409 version_conflict", rec.Code, rec.Body.String())
	}
	r := st.runs[runID]
	r.Status = "completed"
	st.runs[runID] = r
	if rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusConflict || prDescReason(t, rec) != "run_terminal" {
		t.Fatalf("terminal stage = %d %s, want 409 run_terminal", rec.Code, rec.Body.String())
	}
	r.Status = "running"
	r.ClaimReleasedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	st.runs[runID] = r
	if rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusConflict || prDescReason(t, rec) != "stale_claim" {
		t.Fatalf("released claim stage = %d %s, want 409 stale_claim", rec.Code, rec.Body.String())
	}
}
