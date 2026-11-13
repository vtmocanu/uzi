package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestGetRunReviewSkipEnvelope pins the issue #3257 "skip" key of GET /api/runs/{id}/review:
// always present, null unless the viewer can see the run AND there is neither a review nor a
// pending judge, and the skip read is not even issued otherwise.
func TestGetRunReviewSkipEnvelope(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 8, 7, 0, time.UTC)
	skipRow := &store.JudgeSkip{Harness: "codex", Reason: "credential_disabled", CreatedAt: pgtype.Timestamptz{Time: at, Valid: true}}
	newStore := func() (*dispStore, uuid.UUID) {
		ownerID, runID := uuid.New(), uuid.New()
		return &dispStore{
			ownerID:   ownerID,
			run:       store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: "issue"},
			reviewErr: pgx.ErrNoRows, // unjudged
			skip:      skipRow,
		}, runID
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRunReview = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		return body
	}
	assertSkip := func(t *testing.T, body map[string]json.RawMessage) {
		t.Helper()
		var got apitypes.JudgeSkipDTO
		if err := json.Unmarshal(body["skip"], &got); err != nil {
			t.Fatalf("decode skip %s: %v", body["skip"], err)
		}
		if got.Harness != "codex" || got.Reason != "credential_disabled" || !got.At.Equal(at) {
			t.Fatalf("skip = %+v, want codex/credential_disabled/%s", got, at)
		}
	}

	t.Run("owner sees the skip", func(t *testing.T) {
		st, runID := newStore()
		rec := httptest.NewRecorder()
		newRunsHandler(t, st).GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		assertSkip(t, decode(t, rec))
	})
	t.Run("admin sees the skip", func(t *testing.T) {
		st, runID := newStore()
		rec := httptest.NewRecorder()
		newRunsHandler(t, st).GetRunReview(rec, runReq(store.User{ID: uuid.New(), IsAdmin: true}, runID))
		assertSkip(t, decode(t, rec))
	})
	t.Run("another user gets 404 and no skip read", func(t *testing.T) {
		st, runID := newStore()
		rec := httptest.NewRecorder()
		newRunsHandler(t, st).GetRunReview(rec, runReq(store.User{ID: uuid.New()}, runID))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GetRunReview = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if st.skipLookups != 0 {
			t.Fatalf("skip lookups = %d, want 0 for an invisible run", st.skipLookups)
		}
	})
	t.Run("no recorded skip is an explicit null", func(t *testing.T) {
		st, runID := newStore()
		st.skip = nil
		rec := httptest.NewRecorder()
		newRunsHandler(t, st).GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		body := decode(t, rec)
		if raw, ok := body["skip"]; !ok || string(raw) != "null" {
			t.Fatalf("skip = %s (present=%v), want an explicit null", raw, ok)
		}
	})
	t.Run("a pending judge hides the skip", func(t *testing.T) {
		st, runID := newStore()
		st.pendingJudge = &store.GetActiveJudgeRunForTargetRow{ID: uuid.New(), Status: "queued", CreatedAt: pgtype.Timestamptz{Time: at, Valid: true}}
		rec := httptest.NewRecorder()
		newRunsHandler(t, st).GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		if body := decode(t, rec); string(body["skip"]) != "null" {
			t.Fatalf("skip = %s, want null while a judge is pending", body["skip"])
		}
		if st.skipLookups != 0 {
			t.Fatalf("skip lookups = %d, want 0 while a judge is pending", st.skipLookups)
		}
	})
	t.Run("a review hides the skip", func(t *testing.T) {
		st, runID, _ := oneRecStore()
		st.skip = skipRow
		rec := httptest.NewRecorder()
		newRunsHandler(t, st).GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		if body := decode(t, rec); string(body["skip"]) != "null" || string(body["review"]) == "null" {
			t.Fatalf("review = %s skip = %s, want a review and a null skip", body["review"], body["skip"])
		}
		if st.skipLookups != 0 {
			t.Fatalf("skip lookups = %d, want 0 when a review exists", st.skipLookups)
		}
	})
}
