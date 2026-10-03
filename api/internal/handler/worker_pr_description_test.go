package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// begins counts BeginPrDescTx calls; onBegin, when set, runs at each one with that count, so
	// a test can change the tables between the stage's pre-sanitize fence and its write tx.
	begins  int
	onBegin func(n int)
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
	if !ok || v.RunID != a.RunID || v.State != "pending" || (v.MrIid.Valid && v.MrIid.Int64 != a.MrIid) ||
		(v.RenderedRegionSha256.Valid && (v.RenderedRegionSha256.String != a.RenderedRegionSha256 || v.RegionHasDiagram != a.RegionHasDiagram)) {
		return store.PrDescriptionVersion{}, pgx.ErrNoRows
	}
	v.MrIid = pgtype.Int8{Int64: a.MrIid, Valid: true}
	v.RenderedRegionSha256 = pgtype.Text{String: a.RenderedRegionSha256, Valid: true}
	v.RegionHasDiagram = a.RegionHasDiagram
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

func (f *prDescFakeStore) FirstPrDescriptionMrIidForRun(_ context.Context, runID uuid.UUID) (int64, error) {
	var best *store.PrDescriptionVersion
	for _, v := range f.versions {
		if v.RunID == runID && v.MrIid.Valid && (best == nil || v.CreatedAt.Time.Before(best.CreatedAt.Time)) {
			c := v
			best = &c
		}
	}
	if best == nil {
		return 0, pgx.ErrNoRows
	}
	return best.MrIid.Int64, nil
}

func (f *prDescFakeStore) CountPendingPrDescriptionVersionsForRunGeneration(_ context.Context, a store.CountPendingPrDescriptionVersionsForRunGenerationParams) (int64, error) {
	var n int64
	for _, v := range f.versions {
		if v.RunID == a.RunID && v.ClaimGeneration == a.ClaimGeneration && v.State == "pending" {
			n++
		}
	}
	return n, nil
}

func (f *prDescFakeStore) CountPrDescriptionVersionsForRun(_ context.Context, runID uuid.UUID) (int64, error) {
	var n int64
	for _, v := range f.versions {
		if v.RunID == runID {
			n++
		}
	}
	return n, nil
}

func (f *prDescFakeStore) AbandonStalePrDescriptionVersionsForRun(_ context.Context, a store.AbandonStalePrDescriptionVersionsForRunParams) (int64, error) {
	var n int64
	for id, v := range f.versions {
		if v.RunID == a.RunID && v.ClaimGeneration < a.ClaimGeneration && v.State == "pending" &&
			(!v.MrIid.Valid || !v.RenderedRegionSha256.Valid) {
			v.State = "abandoned"
			f.versions[id] = v
			n++
		}
	}
	return n, nil
}

func (f *prDescFakeStore) setVersionState(id uuid.UUID, state string) int64 {
	v, ok := f.versions[id]
	if !ok || v.State != "pending" {
		return 0
	}
	if state == "published" && (!v.MrIid.Valid || !v.RenderedRegionSha256.Valid) {
		return 0 // the SQL guard and the migration's published_bound CHECK
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
	f.begins++
	if f.onBegin != nil {
		f.onBegin(f.begins)
	}
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

func prDescBool(v bool) *bool { return &v }

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
	if bound.Version.MrIid == nil || *bound.Version.MrIid != 41 || *bound.Version.RenderedRegionSha256 != prDescHashA || bound.Version.RegionHasDiagram != nil {
		t.Fatalf("bound version = %+v", bound.Version)
	}
	rec = prDescPost(t, router, runID, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(3), VersionID: v.ID, MrIid: 41, RenderedRegionSha256: prDescHashA,
	}, &bound)
	if rec.Code != http.StatusOK || bound.Version.RegionHasDiagram != nil {
		t.Fatalf("absent flag bind retry = %d %+v", rec.Code, bound.Version)
	}
	rec = prDescPost(t, router, runID, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(3), VersionID: v.ID, MrIid: 41, RenderedRegionSha256: prDescHashA, RegionHasDiagram: prDescBool(false),
	}, nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "version_conflict" {
		t.Fatalf("flag change after bind = %d %s", rec.Code, rec.Body.String())
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
		{"diagram title over raw cap", mut(func(b *apitypes.PrDescriptionStageRequest) {
			b.Fields.Diagram = &apitypes.PrDescriptionDiagram{Title: strings.Repeat("t", workersvc.MaxPrDescDiagramRawLabelBytes+1)}
		}), http.StatusBadRequest},
		{"diagram node label over raw cap", mut(func(b *apitypes.PrDescriptionStageRequest) {
			b.Fields.Diagram = &apitypes.PrDescriptionDiagram{Nodes: []apitypes.PrDescriptionDiagramNode{{Key: "a", Label: strings.Repeat("n", workersvc.MaxPrDescDiagramRawLabelBytes+1)}}}
		}), http.StatusBadRequest},
		{"diagram edge count over raw cap", mut(func(b *apitypes.PrDescriptionStageRequest) {
			b.Fields.Diagram = &apitypes.PrDescriptionDiagram{Edges: make([]apitypes.PrDescriptionDiagramEdge, workersvc.MaxPrDescDiagramRawEntries+1)}
		}), http.StatusBadRequest},
		{"unknown source", mut(func(b *apitypes.PrDescriptionStageRequest) { b.Source = "model" }), http.StatusBadRequest},
		{"bad head sha", mut(func(b *apitypes.PrDescriptionStageRequest) { b.HeadSha = "not-a-sha" }), http.StatusBadRequest},
		{"branch with newline", mut(func(b *apitypes.PrDescriptionStageRequest) { b.TargetBranch = "main\nx" }), http.StatusBadRequest},
		{"negative size", mut(func(b *apitypes.PrDescriptionStageRequest) { b.Size.Files = -1 }), http.StatusBadRequest},
		{"unavailable size with buckets", mut(func(b *apitypes.PrDescriptionStageRequest) { b.Size.Unavailable = true }), http.StatusBadRequest},
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

// prDescSeedRun stores a running, repo-ful issue run held by wkr at generation gen.
func prDescSeedRun(st *prDescFakeStore, wkr store.Worker, repoID uuid.UUID, gen int64) uuid.UUID {
	runID := uuid.New()
	st.runs[runID] = store.Run{
		ID: runID, UserID: wkr.UserID, RepoID: pgtype.UUID{Bytes: repoID, Valid: true}, Status: "running", Kind: "issue",
		WorkerID: pgtype.UUID{Bytes: wkr.ID, Valid: true}, ClaimGeneration: gen,
	}
	return runID
}

// prDescStageBind stages a version (with mrIid on the stage when stageMr) and binds it to mrIid
// with hash, failing the test on any non-200.
func prDescStageBind(t *testing.T, router http.Handler, runID uuid.UUID, gen, mrIid int64, hash string) apitypes.PrDescriptionVersionDTO {
	t.Helper()
	var staged apitypes.PrDescriptionStageResponse
	if rec := prDescPost(t, router, runID, "stage", prDescStageBody(gen, "text"), &staged); rec.Code != http.StatusOK {
		t.Fatalf("stage = %d %s", rec.Code, rec.Body.String())
	}
	var bound apitypes.PrDescriptionBindResponse
	if rec := prDescPost(t, router, runID, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(gen), VersionID: staged.Version.ID, MrIid: mrIid, RenderedRegionSha256: hash,
	}, &bound); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}
	return bound.Version
}

func prDescHash(c byte) string { return strings.Repeat(string(c), 64) }

// TestWorkerPrDescriptionPublishedAckNeedsBind (review B2): a version staged WITH an mr_iid (the
// refresh path) but never bound has no rendered_region_sha256, so a published ack for it is a
// version_conflict; a skip outcome is still recorded.
func TestWorkerPrDescriptionPublishedAckNeedsBind(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	runID := prDescSeedRun(st, wkr, uuid.New(), 1)
	router := prDescTestRouter(st, wkr)

	v1 := prDescStageBind(t, router, runID, 1, 41, prDescHashA)
	if rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(1), VersionID: v1.ID, Outcome: "published"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("v1 ack = %d %s", rec.Code, rec.Body.String())
	}
	var staged apitypes.PrDescriptionStageResponse
	body := prDescStageBody(1, "refresh")
	body.MrIid = i64(41)
	if rec := prDescPost(t, router, runID, "stage", body, &staged); rec.Code != http.StatusOK {
		t.Fatalf("refresh stage = %d %s", rec.Code, rec.Body.String())
	}
	rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{
		ClaimGeneration: i64(1), VersionID: staged.Version.ID, Outcome: "published", ExpectedLockVersion: 1,
	}, nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "version_conflict" {
		t.Fatalf("published ack of an unbound version = %d %s, want 409 version_conflict", rec.Code, rec.Body.String())
	}
	pr := st.prs[prDescFakeKey{st.runs[runID].RepoID.Bytes, 41}]
	if uuid.UUID(pr.PublishedVersionID.Bytes).String() != v1.ID || pr.LockVersion != 1 {
		t.Fatalf("pr moved: %+v", pr)
	}
	var acked apitypes.PrDescriptionAckResponse
	if rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{
		ClaimGeneration: i64(1), VersionID: staged.Version.ID, Outcome: "skipped_snapshot_moved", ExpectedLockVersion: 1,
	}, &acked); rec.Code != http.StatusOK || acked.PR.PublishedVersion == nil || acked.PR.PublishedVersion.ID != v1.ID {
		t.Fatalf("skip ack of an unbound version = %d %s", rec.Code, rec.Body.String())
	}
}

// TestWorkerPrDescriptionLostAckNeverMovesBackwards (review N1): recovery publishes a pending
// version only when it is newer than the published one and the forge does not already show the
// published version's region.
func TestWorkerPrDescriptionLostAckNeverMovesBackwards(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	runID := prDescSeedRun(st, wkr, uuid.New(), 1)
	router := prDescTestRouter(st, wkr)
	ack := func(v apitypes.PrDescriptionVersionDTO, outcome string, lock int64, observed string) apitypes.PrDescriptionAckResponse {
		t.Helper()
		var out apitypes.PrDescriptionAckResponse
		req := apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(1), VersionID: v.ID, Outcome: outcome, ExpectedLockVersion: lock}
		if observed != "" {
			req.ObservedRegionSha256 = &observed
		}
		if rec := prDescPost(t, router, runID, "ack", req, &out); rec.Code != http.StatusOK {
			t.Fatalf("ack %s = %d %s", v.ID, rec.Code, rec.Body.String())
		}
		return out
	}

	// V1 wrote region A and lost its ack; V2 wrote region B and acked.
	v1 := prDescStageBind(t, router, runID, 1, 41, prDescHash('a'))
	v2 := prDescStageBind(t, router, runID, 1, 41, prDescHash('b'))
	ack(v2, "published", 0, "")
	// V3 finds region A on the forge (a human restored the older text): V1 is OLDER than the
	// published V2, so it is not recovered and V2 stays published.
	v3 := prDescStageBind(t, router, runID, 1, 41, prDescHash('c'))
	got := ack(v3, "skipped_human_edit", 1, prDescHash('a'))
	if got.RecoveredVersionID != nil || got.PR.PublishedVersion == nil || got.PR.PublishedVersion.ID != v2.ID {
		t.Fatalf("an older pending version was recovered over the published one: %+v", got)
	}
	if st.versions[uuid.MustParse(v1.ID)].State != "pending" {
		t.Fatal("V1 must stay pending")
	}
	// V4 renders the same region B as the published V2 and loses its ack; V5 observes B, which
	// the published version already rendered: nothing to recover.
	_ = prDescStageBind(t, router, runID, 1, 41, prDescHash('b'))
	v5 := prDescStageBind(t, router, runID, 1, 41, prDescHash('e'))
	got = ack(v5, "skipped_human_edit", 2, prDescHash('b'))
	if got.RecoveredVersionID != nil || got.PR.PublishedVersion.ID != v2.ID {
		t.Fatalf("recovery ran although the published version rendered the observed region: %+v", got)
	}
	// V6 wrote region F (newer than V2) and lost its ack; V7 observes F: V6 is recovered, then
	// V7 publishes.
	v6 := prDescStageBind(t, router, runID, 1, 41, prDescHash('f'))
	v7 := prDescStageBind(t, router, runID, 1, 41, prDescHash('7'))
	got = ack(v7, "published", 3, prDescHash('f'))
	if got.RecoveredVersionID == nil || *got.RecoveredVersionID != v6.ID || got.PR.PublishedVersion.ID != v7.ID {
		t.Fatalf("newer lost ack not recovered: %+v", got)
	}
}

// TestWorkerPrDescriptionMrIidFence (review N2): stage and bind must name the run's own PR —
// runs.mr_iid when set, and otherwise the PR the run first staged or bound for.
func TestWorkerPrDescriptionMrIidFence(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	repoID := uuid.New()
	router := prDescTestRouter(st, wkr)
	conflict := func(rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		if rec.Code != http.StatusConflict || prDescReason(t, rec) != "version_conflict" {
			t.Fatalf("%s = %d %s, want 409 version_conflict", what, rec.Code, rec.Body.String())
		}
	}

	// An mr_rework-style run whose runs.mr_iid is 41.
	rework := prDescSeedRun(st, wkr, repoID, 1)
	r := st.runs[rework]
	r.MrIid = pgtype.Int8{Int64: 41, Valid: true}
	st.runs[rework] = r
	body := prDescStageBody(1, "x")
	body.MrIid = i64(42)
	conflict(prDescPost(t, router, rework, "stage", body, nil), "stage for another PR than runs.mr_iid")
	var staged apitypes.PrDescriptionStageResponse
	if rec := prDescPost(t, router, rework, "stage", prDescStageBody(1, "x"), &staged); rec.Code != http.StatusOK {
		t.Fatalf("stage = %d", rec.Code)
	}
	conflict(prDescPost(t, router, rework, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(1), VersionID: staged.Version.ID, MrIid: 42, RenderedRegionSha256: prDescHashA,
	}, nil), "bind to another PR than runs.mr_iid")
	_ = prDescStageBind(t, router, rework, 1, 41, prDescHashA)

	// An issue run (no runs.mr_iid) that bound PR 50 is pinned to it.
	issue := prDescSeedRun(st, wkr, repoID, 1)
	_ = prDescStageBind(t, router, issue, 1, 50, prDescHashA)
	body.MrIid = i64(51)
	conflict(prDescPost(t, router, issue, "stage", body, nil), "stage for a second PR")
	if rec := prDescPost(t, router, issue, "stage", prDescStageBody(1, "x"), &staged); rec.Code != http.StatusOK {
		t.Fatalf("stage = %d", rec.Code)
	}
	conflict(prDescPost(t, router, issue, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(1), VersionID: staged.Version.ID, MrIid: 51, RenderedRegionSha256: prDescHashB,
	}, nil), "bind to a second PR")
	if rec := prDescPost(t, router, issue, "bind", apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(1), VersionID: staged.Version.ID, MrIid: 50, RenderedRegionSha256: prDescHashB,
	}, nil); rec.Code != http.StatusOK {
		t.Fatalf("bind to the run's own PR = %d %s", rec.Code, rec.Body.String())
	}
}

// TestWorkerPrDescriptionStageCap (auditor M4): a run holds at most
// MaxPrDescPendingVersionsPerRun pending versions; the next stage is 409 too_many_versions.
func TestWorkerPrDescriptionStageCap(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	runID := prDescSeedRun(st, wkr, uuid.New(), 1)
	router := prDescTestRouter(st, wkr)
	for i := 0; i < workersvc.MaxPrDescPendingVersionsPerRun; i++ {
		if rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusOK {
			t.Fatalf("stage %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "too_many_versions" {
		t.Fatalf("stage past the cap = %d %s, want 409 too_many_versions", rec.Code, rec.Body.String())
	}
	if len(st.versions) != workersvc.MaxPrDescPendingVersionsPerRun {
		t.Fatalf("versions = %d, want %d", len(st.versions), workersvc.MaxPrDescPendingVersionsPerRun)
	}
	// Another run of the same worker has its own budget.
	other := prDescSeedRun(st, wkr, uuid.New(), 1)
	if rec := prDescPost(t, router, other, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusOK {
		t.Fatalf("another run's stage = %d", rec.Code)
	}
}

// prDescOverlayStore is a runsStore (GetRun's reads) plus the PR-description reader surface,
// served by a prDescFakeStore or failing with readErr.
type prDescOverlayStore struct {
	*runsStore
	desc    *prDescFakeStore
	readErr error
}

func (s *prDescOverlayStore) GetPrDescription(ctx context.Context, a store.GetPrDescriptionParams) (store.PrDescription, error) {
	if s.readErr != nil {
		return store.PrDescription{}, s.readErr
	}
	return s.desc.GetPrDescription(ctx, a)
}

func (s *prDescOverlayStore) GetPrDescriptionVersionByID(ctx context.Context, id uuid.UUID) (store.PrDescriptionVersion, error) {
	return s.desc.GetPrDescriptionVersionByID(ctx, id)
}

func (s *prDescOverlayStore) FindPrDescriptionVersionByRegionHash(ctx context.Context, a store.FindPrDescriptionVersionByRegionHashParams) (store.PrDescriptionVersion, error) {
	return s.desc.FindPrDescriptionVersionByRegionHash(ctx, a)
}

func (s *prDescOverlayStore) LatestBoundPrDescriptionVersionForRun(ctx context.Context, runID uuid.UUID) (store.PrDescriptionVersion, error) {
	return s.desc.LatestBoundPrDescriptionVersionForRun(ctx, runID)
}

// TestGetRunPrDescriptionOverlay (review N6): GetRun fills pr_description and
// pr_description_outcome from the run's PR record, and an overlay read error still answers 200
// with both null.
func TestGetRunPrDescriptionOverlay(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	wkr := store.Worker{ID: uuid.New(), UserID: owner.ID}
	desc := newPrDescFakeStore()
	repoID := uuid.New()
	runID := prDescSeedRun(desc, wkr, repoID, 1)
	router := prDescTestRouter(desc, wkr)
	v := prDescStageBind(t, router, runID, 1, 41, prDescHashA)
	if rec := prDescPost(t, router, runID, "ack", apitypes.PrDescriptionAckRequest{ClaimGeneration: i64(1), VersionID: v.ID, Outcome: "published"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("ack = %d %s", rec.Code, rec.Body.String())
	}
	run := desc.runs[runID]

	get := func(t *testing.T, readErr error) (int, map[string]json.RawMessage) {
		t.Helper()
		st := &prDescOverlayStore{runsStore: &runsStore{ownerID: owner.ID, run: run}, desc: desc, readErr: readErr}
		h := newRunsHandler(t, st)
		h.q = store.New(noRowUserDB{}) // GetRun's other repo-ful reads find nothing
		rec := httptest.NewRecorder()
		h.GetRun(rec, runReq(owner, runID))
		var body struct {
			Run map[string]json.RawMessage `json:"run"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return rec.Code, body.Run
	}

	code, got := get(t, nil)
	if code != http.StatusOK {
		t.Fatalf("GetRun = %d", code)
	}
	var pd apitypes.RunPrDescriptionDTO
	if err := json.Unmarshal(got["pr_description"], &pd); err != nil {
		t.Fatalf("pr_description: %v (%s)", err, got["pr_description"])
	}
	if pd.MrIid != 41 || pd.Source != "generated" || pd.PublishedAt == nil || pd.Fields.Summary != "text" || pd.HeadSha != v.HeadSha {
		t.Fatalf("pr_description = %+v", pd)
	}
	if string(got["pr_description_outcome"]) != `"published"` {
		t.Fatalf("pr_description_outcome = %s", got["pr_description_outcome"])
	}

	code, got = get(t, errors.New("db down"))
	if code != http.StatusOK {
		t.Fatalf("GetRun with an overlay error = %d, want 200", code)
	}
	if string(got["pr_description"]) != "null" || string(got["pr_description_outcome"]) != "null" {
		t.Fatalf("overlay error must leave both null, got %s / %s", got["pr_description"], got["pr_description_outcome"])
	}
}

// TestWorkerPrDescriptionStageFencesBeforeSanitizing (H-A): the run/claim fence and the version
// cap are checked before the raw fields are sanitized, so a caller that does not hold the run (or
// a run at its cap) is refused on the fence even with a body only the sanitizer would reject (a
// verification result outside pass|fail).
func TestWorkerPrDescriptionStageFencesBeforeSanitizing(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	runID := prDescSeedRun(st, wkr, uuid.New(), 2)
	router := prDescTestRouter(st, wkr)
	bad := func(gen int64) apitypes.PrDescriptionStageRequest {
		b := prDescStageBody(gen, strings.Repeat("<", workersvc.MaxPrDescSummaryRawBytes-1)+"a")
		b.Fields.Verification[0].Result = "ok"
		return b
	}

	rec := prDescPost(t, router, runID, "stage", bad(1), nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "stale_claim" {
		t.Fatalf("stale-claim stage with an invalid body = %d %s, want 409 stale_claim", rec.Code, rec.Body.String())
	}
	for i := 0; i < workersvc.MaxPrDescPendingVersionsPerRun; i++ {
		if rec := prDescPost(t, router, runID, "stage", prDescStageBody(2, "x"), nil); rec.Code != http.StatusOK {
			t.Fatalf("stage %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec = prDescPost(t, router, runID, "stage", bad(2), nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "too_many_versions" {
		t.Fatalf("capped stage with an invalid body = %d %s, want 409 too_many_versions", rec.Code, rec.Body.String())
	}
	// A held run under its cap still validates the body: 400.
	other := prDescSeedRun(st, wkr, uuid.New(), 2)
	if rec := prDescPost(t, router, other, "stage", bad(2), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("held run with an invalid body = %d %s, want 400", rec.Code, rec.Body.String())
	}
}

// TestWorkerPrDescriptionStageCapPerGeneration (N1): pending versions left by an older claim
// generation (a crashed attempt) do not count toward the live generation's cap; the unbound ones
// are abandoned at stage, the bound ones stay pending for lost-ack recovery; and a per-run total
// backstop still ends an endless reclaim loop.
func TestWorkerPrDescriptionStageCapPerGeneration(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	runID := prDescSeedRun(st, wkr, uuid.New(), 1)
	router := prDescTestRouter(st, wkr)
	bound := prDescStageBind(t, router, runID, 1, 70, prDescHashA)
	for i := 1; i < workersvc.MaxPrDescPendingVersionsPerRun; i++ {
		if rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusOK {
			t.Fatalf("gen-1 stage %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil); rec.Code != http.StatusConflict {
		t.Fatalf("gen-1 stage past the cap = %d, want 409", rec.Code)
	}

	// Reclaim: the generation advances and the new flight stages.
	r := st.runs[runID]
	r.ClaimGeneration = 2
	st.runs[runID] = r
	if rec := prDescPost(t, router, runID, "stage", prDescStageBody(2, "x"), nil); rec.Code != http.StatusOK {
		t.Fatalf("gen-2 stage after 20 gen-1 pending = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var pending1, abandoned1 int
	for _, v := range st.versions {
		if v.ClaimGeneration != 1 {
			continue
		}
		switch v.State {
		case "pending":
			pending1++
		case "abandoned":
			abandoned1++
		}
	}
	if pending1 != 1 || abandoned1 != workersvc.MaxPrDescPendingVersionsPerRun-1 {
		t.Fatalf("gen-1 versions: %d pending, %d abandoned; want only the bound one pending", pending1, abandoned1)
	}
	if st.versions[uuid.MustParse(bound.ID)].State != "pending" {
		t.Fatalf("the bound gen-1 version must stay pending")
	}

	// The per-run backstop: enough versions in all refuses even a fresh generation.
	for g := int64(3); len(st.versions) < workersvc.MaxPrDescVersionsPerRun; g++ {
		r := st.runs[runID]
		r.ClaimGeneration = g
		st.runs[runID] = r
		for i := 0; i < 10 && len(st.versions) < workersvc.MaxPrDescVersionsPerRun; i++ {
			if rec := prDescPost(t, router, runID, "stage", prDescStageBody(g, "x"), nil); rec.Code != http.StatusOK {
				t.Fatalf("gen-%d stage = %d %s", g, rec.Code, rec.Body.String())
			}
		}
	}
	r = st.runs[runID]
	r.ClaimGeneration++
	st.runs[runID] = r
	rec := prDescPost(t, router, runID, "stage", prDescStageBody(r.ClaimGeneration, "x"), nil)
	if rec.Code != http.StatusConflict || prDescReason(t, rec) != "too_many_versions" {
		t.Fatalf("stage at the per-run backstop = %d %s, want 409 too_many_versions", rec.Code, rec.Body.String())
	}
}

// TestWorkerPrDescriptionStageRefencesInWriteTx: the stage's write transaction re-applies the
// whole fence under the run row lock, so a claim that moved, a cap that filled or a run that
// ended while the fields were being sanitized (between the first and the second transaction)
// is refused, and nothing is written.
func TestWorkerPrDescriptionStageRefencesInWriteTx(t *testing.T) {
	cases := []struct {
		name, reason string
		mutate       func(st *prDescFakeStore, runID uuid.UUID)
	}{
		{"claim generation bumped", "stale_claim", func(st *prDescFakeStore, runID uuid.UUID) {
			r := st.runs[runID]
			r.ClaimGeneration++
			st.runs[runID] = r
		}},
		{"pending cap filled", "too_many_versions", func(st *prDescFakeStore, runID uuid.UUID) {
			r := st.runs[runID]
			for i := 0; i < workersvc.MaxPrDescPendingVersionsPerRun; i++ {
				if _, err := st.InsertPrDescriptionVersion(context.Background(), store.InsertPrDescriptionVersionParams{
					RunID: runID, ClaimGeneration: r.ClaimGeneration, RepoID: uuid.UUID(r.RepoID.Bytes), Source: "generated",
				}); err != nil {
					t.Fatalf("seed version: %v", err)
				}
			}
		}},
		{"run became terminal", "run_terminal", func(st *prDescFakeStore, runID uuid.UUID) {
			r := st.runs[runID]
			r.Status = "completed"
			st.runs[runID] = r
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			st := newPrDescFakeStore()
			runID := prDescSeedRun(st, wkr, uuid.New(), 1)
			router := prDescTestRouter(st, wkr)
			var before int
			st.onBegin = func(n int) {
				if n == 2 {
					c.mutate(st, runID)
					before = len(st.versions)
				}
			}
			rec := prDescPost(t, router, runID, "stage", prDescStageBody(1, "x"), nil)
			if rec.Code != http.StatusConflict || prDescReason(t, rec) != c.reason {
				t.Fatalf("stage = %d %s, want 409 %s", rec.Code, rec.Body.String(), c.reason)
			}
			if st.begins != 2 {
				t.Fatalf("transactions = %d, want 2 (pre-sanitize fence, write)", st.begins)
			}
			if len(st.versions) != before {
				t.Fatalf("versions = %d, want %d: a refused stage must write nothing", len(st.versions), before)
			}
		})
	}
}

// TestWorkerPrDescriptionStageDeterministicIgnoresFields: a deterministic_only stage stores no
// model or lead text, so its raw fields are ignored, never validated: an over-cap summary, too
// many entries, an unknown scope kind or a bad verification result is not a 400, and the stored
// fields are empty.
func TestWorkerPrDescriptionStageDeterministicIgnoresFields(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	st := newPrDescFakeStore()
	runID := prDescSeedRun(st, wkr, uuid.New(), 1)
	router := prDescTestRouter(st, wkr)
	body := prDescStageBody(1, strings.Repeat("s", workersvc.MaxPrDescSummaryRawBytes+1))
	body.Source = "deterministic_only"
	body.Fields.Changes = make([]string, workersvc.MaxPrDescListRawEntries+1)
	body.Fields.Changes[0] = strings.Repeat("c", workersvc.MaxPrDescItemRawBytes+1)
	body.Fields.ScopeNotes = []apitypes.PrDescriptionScopeNote{{Kind: "risk", Text: "t"}}
	body.Fields.Verification[0].Result = "ok"
	var staged apitypes.PrDescriptionStageResponse
	rec := prDescPost(t, router, runID, "stage", body, &staged)
	if rec.Code != http.StatusOK {
		t.Fatalf("deterministic_only stage with invalid raw fields = %d %s, want 200", rec.Code, rec.Body.String())
	}
	f := staged.Version.Fields
	if staged.Version.Source != "deterministic_only" || f.Summary != "" || len(f.Changes) != 0 || len(f.ScopeNotes) != 0 ||
		len(f.ReviewPointers) != 0 || len(f.Verification) != 0 {
		t.Fatalf("deterministic_only version must carry no text: %+v", staged.Version)
	}
	// The same body as a generated stage is refused.
	body.Source = "generated"
	if rec := prDescPost(t, router, runID, "stage", body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("generated stage with the same fields = %d, want 400", rec.Code)
	}
}
