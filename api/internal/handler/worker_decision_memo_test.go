package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// decisionMemoStore is a minimal workersvc.Store for the decisions-memo worker routes (issue
// #2083): it overrides only the three queries the save and read paths reach.
type decisionMemoStore struct {
	workersvc.Store
	ownedRun store.Run
	ownedErr error

	upsertRows   int64
	upsertCalls  int
	upsertParams store.UpsertRunDecisionMemoFencedParams

	lineageRow    store.GetLatestDecisionMemoForLineageRow
	lineageErr    error
	lineageCalls  int
	lineageParams store.GetLatestDecisionMemoForLineageParams
}

func (m *decisionMemoStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return m.ownedRun, m.ownedErr
}

func (m *decisionMemoStore) UpsertRunDecisionMemoFenced(_ context.Context, arg store.UpsertRunDecisionMemoFencedParams) (int64, error) {
	m.upsertCalls++
	m.upsertParams = arg
	return m.upsertRows, nil
}

func (m *decisionMemoStore) GetLatestDecisionMemoForLineage(_ context.Context, arg store.GetLatestDecisionMemoForLineageParams) (store.GetLatestDecisionMemoForLineageRow, error) {
	m.lineageCalls++
	m.lineageParams = arg
	return m.lineageRow, m.lineageErr
}

type failingSettingsStore struct{}

func (failingSettingsStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	return nil, errors.New("settings store down")
}

// memoHandler builds a handler over st with decisions_memo_enabled set to value ("" = no row).
func memoHandler(t *testing.T, st *decisionMemoStore, value string) *Handler {
	t.Helper()
	h := newProtocolHandler(t, st)
	rows := []store.AppSetting{}
	if value != "" {
		rows = append(rows, store.AppSetting{Key: settings.KeyDecisionsMemoEnabled, Value: value})
	}
	h.settings = settings.New(&settingsStore{rows: rows}, time.Minute)
	return h
}

func memoPost(h *Handler, runID uuid.UUID, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.WorkerSaveDecisionsMemo(rec, workerReq(http.MethodPost, body, runID))
	return rec
}

func memoGet(h *Handler, runID uuid.UUID, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := workerReq(http.MethodGet, "", runID)
	req.URL.RawQuery = query
	h.WorkerGetDecisionsMemo(rec, req)
	return rec
}

func memoBodyJSON(t *testing.T, claimGen int, body string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"claim_generation": claimGen, "body": body})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func reworkRun() store.Run {
	return store.Run{
		ID: uuid.New(), UserID: uuid.New(), Kind: "mr_rework", ClaimGeneration: 4,
		RepoID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
		PipelineRef: pgtype.Text{String: "agent/issue-9", Valid: true},
		MrIid:       pgtype.Int8{Int64: 12, Valid: true},
	}
}

func TestDecisionsMemoDisabledFailsClosed(t *testing.T) {
	cases := map[string]*Handler{}
	cases["off"] = memoHandler(t, &decisionMemoStore{upsertRows: 1}, "false")
	cases["absent row defaults off"] = memoHandler(t, &decisionMemoStore{upsertRows: 1}, "")
	nilSettings := newProtocolHandler(t, &decisionMemoStore{upsertRows: 1})
	cases["nil settings"] = nilSettings
	unreadable := newProtocolHandler(t, &decisionMemoStore{upsertRows: 1})
	unreadable.settings = settings.New(failingSettingsStore{}, time.Minute)
	cases["unreadable"] = unreadable

	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			rec := memoPost(h, uuid.New(), memoBodyJSON(t, 1, "x"))
			if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "decisions_memo_disabled" {
				t.Fatalf("POST = %d %s, want 409 decisions_memo_disabled", rec.Code, rec.Body.String())
			}
			rec = memoGet(h, uuid.New(), "claim_generation=1")
			out := decodeMap(t, rec)
			if rec.Code != http.StatusOK || out["enabled"] != false || out["memo"] != nil {
				t.Fatalf("GET = %d %s, want 200 {enabled:false, memo:null}", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDecisionsMemoClaimGenerationRequired(t *testing.T) {
	st := &decisionMemoStore{upsertRows: 1}
	h := memoHandler(t, st, "true")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"POST missing":  memoPost(h, uuid.New(), `{"body":"x"}`),
		"POST negative": memoPost(h, uuid.New(), `{"claim_generation":-1,"body":"x"}`),
		"POST string":   memoPost(h, uuid.New(), `{"claim_generation":"1","body":"x"}`),
		"GET missing":   memoGet(h, uuid.New(), ""),
		"GET junk":      memoGet(h, uuid.New(), "claim_generation=abc"),
		"GET negative":  memoGet(h, uuid.New(), "claim_generation=-3"),
	} {
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
			continue
		}
		// A non-numeric JSON value is rejected by the decoder before the field check, so only the
		// missing / negative forms carry the typed reason.
		if name != "POST string" && decodeMap(t, rec)["reason"] != "claim_generation_required" {
			t.Errorf("%s: body %s, want reason claim_generation_required", name, rec.Body.String())
		}
	}
	if st.upsertCalls != 0 {
		t.Fatalf("a rejected request reached the store %d times", st.upsertCalls)
	}
}

func TestDecisionsMemoNotOwnedRun404(t *testing.T) {
	st := &decisionMemoStore{ownedErr: pgx.ErrNoRows, upsertRows: 1}
	h := memoHandler(t, st, "true")
	if rec := memoPost(h, uuid.New(), memoBodyJSON(t, 1, "x")); rec.Code != http.StatusNotFound {
		t.Fatalf("POST = %d, want 404", rec.Code)
	}
	if rec := memoGet(h, uuid.New(), "claim_generation=1"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET = %d, want 404", rec.Code)
	}
	if st.upsertCalls != 0 || st.lineageCalls != 0 {
		t.Fatal("a run the worker does not hold must not reach the memo queries")
	}
}

func TestDecisionsMemoSaveSizeCapAfterSanitize(t *testing.T) {
	st := &decisionMemoStore{ownedRun: reworkRun(), upsertRows: 1}
	h := memoHandler(t, st, "true")

	atCap := strings.Repeat("a", workersvc.MaxDecisionsMemoBytes)
	if rec := memoPost(h, uuid.New(), memoBodyJSON(t, 4, atCap)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("8192-byte body = %d %q, want 204 with no echo", rec.Code, rec.Body.String())
	}
	if rec := memoPost(h, uuid.New(), memoBodyJSON(t, 4, atCap+"a")); rec.Code != http.StatusBadRequest {
		t.Fatalf("8193-byte body = %d, want 400", rec.Code)
	}
	// Control bytes count against the wire size but not the stored size: this body is over the cap
	// on the wire and exactly at it once sanitized.
	st.upsertCalls = 0
	dirty := atCap + strings.Repeat("\x1b", 50) + strings.Repeat("\x00", 50)
	if rec := memoPost(h, uuid.New(), memoBodyJSON(t, 4, dirty)); rec.Code != http.StatusNoContent {
		t.Fatalf("sanitized-to-cap body = %d, want 204", rec.Code)
	}
	if st.upsertCalls != 1 || st.upsertParams.Body != atCap {
		t.Fatalf("stored body len %d (calls %d), want the sanitized 8192-byte body", len(st.upsertParams.Body), st.upsertCalls)
	}
}

func TestDecisionsMemoSaveStripsControlCharsAndFences(t *testing.T) {
	run := reworkRun()
	st := &decisionMemoStore{ownedRun: run, upsertRows: 1}
	h := memoHandler(t, st, "true")
	runID := uuid.New()

	rec := memoPost(h, runID, memoBodyJSON(t, 4, "decided\x1b[31m red\x00\nline two\ttab"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if got := st.upsertParams.Body; strings.ContainsAny(got, "\x1b\x00") || !strings.Contains(got, "\n") || !strings.Contains(got, "\t") {
		t.Fatalf("stored body %q: control characters must go, newline and tab stay", got)
	}
	if st.upsertParams.RunID != runID || st.upsertParams.ClaimGeneration != 4 {
		t.Fatalf("fence params = %+v, want run %s generation 4", st.upsertParams, runID)
	}

	for name, body := range map[string]string{"empty": "", "whitespace": " \n\t ", "control only": "\x01\x02"} {
		if rec := memoPost(h, runID, memoBodyJSON(t, 4, body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s body = %d, want 400", name, rec.Code)
		}
	}
}

func TestDecisionsMemoSaveZeroRowsIsClaimNotCurrent(t *testing.T) {
	st := &decisionMemoStore{ownedRun: reworkRun(), upsertRows: 0}
	rec := memoPost(memoHandler(t, st, "true"), uuid.New(), memoBodyJSON(t, 1, "x"))
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "claim_not_current" {
		t.Fatalf("POST = %d %s, want 409 claim_not_current", rec.Code, rec.Body.String())
	}
}

func TestDecisionsMemoGet(t *testing.T) {
	source := uuid.New()
	run := reworkRun()
	st := &decisionMemoStore{ownedRun: run, lineageRow: store.GetLatestDecisionMemoForLineageRow{RunID: source, Body: "prior decisions", FormatVersion: 1}}
	h := memoHandler(t, st, "true")

	rec := memoGet(h, run.ID, "claim_generation=4")
	out := decodeMap(t, rec)
	memo, _ := out["memo"].(map[string]any)
	if rec.Code != http.StatusOK || out["enabled"] != true || memo["body"] != "prior decisions" ||
		memo["source_run_id"] != source.String() || memo["format"] != float64(1) {
		t.Fatalf("GET = %d %s, want the prior memo", rec.Code, rec.Body.String())
	}
	// The lineage comes off the claimed run, never the request.
	p := st.lineageParams
	if p.UserID != run.UserID || p.RepoID != run.RepoID || p.Branch.String != "agent/issue-9" || p.MrIid.Int64 != 12 || p.SelfRunID != run.ID {
		t.Fatalf("lineage params = %+v, want the run's own owner/repo/pipeline_ref/mr_iid and self id", p)
	}

	// No compatible memo.
	st.lineageErr = pgx.ErrNoRows
	if out := decodeMap(t, memoGet(h, run.ID, "claim_generation=4")); out["enabled"] != true || out["memo"] != nil {
		t.Fatalf("no-rows GET = %v, want enabled true, memo null", out)
	}
	// A generation that is not the run's, or a released claim, is not current.
	for name, mutate := range map[string]func(*store.Run){
		"stale generation": func(r *store.Run) { r.ClaimGeneration = 5 },
		"released claim":   func(r *store.Run) { r.ClaimReleasedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true} },
	} {
		stale := run
		mutate(&stale)
		st.ownedRun = stale
		rec := memoGet(h, run.ID, "claim_generation=4")
		if rec.Code != http.StatusConflict || decodeMap(t, rec)["reason"] != "claim_not_current" {
			t.Errorf("%s: GET = %d %s, want 409 claim_not_current", name, rec.Code, rec.Body.String())
		}
	}
}

func TestDecisionsMemoGetNonReworkAndIncompleteRunsHaveNoMemo(t *testing.T) {
	base := reworkRun()
	for name, mutate := range map[string]func(*store.Run){
		"issue run":          func(r *store.Run) { r.Kind = "issue" },
		"no pipeline_ref":    func(r *store.Run) { r.PipelineRef = pgtype.Text{} },
		"no mr_iid":          func(r *store.Run) { r.MrIid = pgtype.Int8{} },
		"no repo":            func(r *store.Run) { r.RepoID = pgtype.UUID{} },
		"empty pipeline ref": func(r *store.Run) { r.PipelineRef = pgtype.Text{Valid: true} },
	} {
		t.Run(name, func(t *testing.T) {
			run := base
			mutate(&run)
			st := &decisionMemoStore{ownedRun: run, lineageRow: store.GetLatestDecisionMemoForLineageRow{RunID: uuid.New(), Body: "x", FormatVersion: 1}}
			rec := memoGet(memoHandler(t, st, "true"), run.ID, "claim_generation=4")
			out := decodeMap(t, rec)
			if rec.Code != http.StatusOK || out["enabled"] != true || out["memo"] != nil {
				t.Fatalf("GET = %d %s, want enabled true, memo null", rec.Code, rec.Body.String())
			}
			if st.lineageCalls != 0 {
				t.Fatal("the lineage query ran for a run that cannot have a lineage")
			}
		})
	}
}
