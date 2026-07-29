package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestPendingJudgeState pins the TOTALITY of the raw-status → display-state mapper
// (PRD #119 M1). Only 'queued' is "scheduled"; every other status the active set can
// contain is "running", and nothing is ever "".
//
// The rows below are not a wish-list of statuses — they are the ones
// GetActiveJudgeRunForTarget can actually hand back. Its predicate is
// uq_runs_one_active_judge_per_target verbatim, `status NOT IN ('completed','failed',
// 'cancelled')`, which is a set defined by SUBTRACTION: awaiting_approval (migration
// 00020) and awaiting_input (00092) are inside it today, and a future migration widening
// runs_status_check silently adds its new value too. That is why the "a status that does
// not exist yet" case is in the table: an enumerated switch over queued/claimed/running
// would pass every other row here and fall through to "" for that one, shipping
// state:"" and breaking the clients' closed "scheduled" | "running" union — a blank chip
// exactly where the panel is supposed to explain what is happening.
func TestPendingJudgeState(t *testing.T) {
	cases := []struct {
		status string
		want   string
		why    string
	}{
		{"queued", "scheduled", "enqueued, unclaimed — the auto-judge's first moment, and the only 'scheduled'"},
		{"claimed", "running", "a worker has taken it; work is under way as far as the user is concerned"},
		{"running", "running", "the obvious one"},
		{"awaiting_approval", "running",
			"INSIDE the index's active set (00020). A judge run cannot reach the approval gate today, " +
				"but the schema permits the status, so the query can return it"},
		{"awaiting_input", "running", "also inside the active set (00092), same argument"},
		{"some_future_status", "running",
			"the mapper must be total over a set defined by subtraction: an unknown active status " +
				"degrades to 'a judge is working on it', which is true of every member by construction"},
		{"", "running", "even the zero value must not map to the zero value"},
	}
	for _, tc := range cases {
		got := pendingJudgeState(tc.status)
		if got != tc.want {
			t.Errorf("pendingJudgeState(%q) = %q, want %q — %s", tc.status, got, tc.want, tc.why)
		}
		if got == "" {
			t.Errorf("pendingJudgeState(%q) returned the empty string; the mapper must be TOTAL — "+
				"an unmapped status breaks the \"scheduled\" | \"running\" union on the wire", tc.status)
		}
	}
}

// TestGetRunReviewPendingJudgeEnvelope pins the wire shape of GET /runs/{id}/review for
// the two states PRD #119 exists to tell apart, asserting on the RAW body so the
// envelope itself — both keys present, either nullable — is what is proven, not just a
// struct that happens to decode.
//
// A decode into a typed struct would pass whether or not "pending_judge" were emitted at
// all (a missing key decodes as the zero value), and the whole point of this change is
// that the key is ALWAYS there: "no judge is coming" is a claim this endpoint makes, and
// a client must not have to infer it from an absent field.
func TestGetRunReviewPendingJudgeEnvelope(t *testing.T) {
	enqueued := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)

	newStore := func() (*dispStore, uuid.UUID) {
		ownerID, runID := uuid.New(), uuid.New()
		return &dispStore{
			ownerID: ownerID,
			run:     store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: "issue"},
			// Unjudged: the verdict lookup finds nothing. This is the state the panel used
			// to render as "this run hasn't been judged yet" with a live button.
			reviewErr: pgx.ErrNoRows,
		}, runID
	}

	// ---- unjudged, judge in flight: review null, pending_judge populated -------------
	t.Run("unjudged with an active judge", func(t *testing.T) {
		st, runID := newStore()
		st.pendingJudge = &store.GetActiveJudgeRunForTargetRow{
			ID:        uuid.New(),
			Status:    "queued",
			CreatedAt: pgtype.Timestamptz{Time: enqueued, Valid: true},
		}
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		h.GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRunReview = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		// json.RawMessage keeps "absent" and "null" distinguishable, which a typed decode
		// cannot: a pointer field is nil for both.
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		raw, ok := body["review"]
		if !ok {
			t.Fatal("the review key must be PRESENT even when there is no verdict")
		}
		if string(raw) != "null" {
			t.Fatalf("review = %s, want null (the run is unjudged)", raw)
		}
		pj, ok := body["pending_judge"]
		if !ok {
			t.Fatal("the pending_judge key is missing — it is the entire point of PRD #119, and " +
				"a client cannot tell an absent key from a deliberate null")
		}
		var got struct {
			State      string    `json:"state"`
			EnqueuedAt time.Time `json:"enqueued_at"`
		}
		if err := json.Unmarshal(pj, &got); err != nil {
			t.Fatalf("decode pending_judge %s: %v", pj, err)
		}
		if got.State != "scheduled" {
			t.Errorf("pending_judge.state = %q, want %q for a 'queued' judge run", got.State, "scheduled")
		}
		if !got.EnqueuedAt.Equal(enqueued) {
			t.Errorf("pending_judge.enqueued_at = %v, want the judge run's created_at %v", got.EnqueuedAt, enqueued)
		}
	})

	// ---- a CLAIMED judge is "running", not "scheduled" -------------------------------
	t.Run("claimed judge reads as running", func(t *testing.T) {
		st, runID := newStore()
		st.pendingJudge = &store.GetActiveJudgeRunForTargetRow{
			ID: uuid.New(), Status: "claimed",
			CreatedAt: pgtype.Timestamptz{Time: enqueued, Valid: true},
		}
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		h.GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRunReview = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			PendingJudge *struct {
				State string `json:"state"`
			} `json:"pending_judge"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.PendingJudge == nil || body.PendingJudge.State != "running" {
			t.Fatalf("pending_judge = %+v, want state=running for a claimed judge", body.PendingJudge)
		}
	})

	// ---- no judge at all: BOTH keys null ---------------------------------------------
	t.Run("unjudged with no judge in flight", func(t *testing.T) {
		st, runID := newStore() // pendingJudge stays nil → the store returns pgx.ErrNoRows
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		h.GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRunReview = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		// The exact envelope: nothing else, both keys null. Go sorts map keys, so
		// pending_judge precedes review; the trim is for json.Encoder's trailing newline.
		// This is the genuinely-unjudged run whose enabled "Run judge" button is the
		// correct affordance — unchanged by #119.
		if got := strings.TrimSpace(rec.Body.String()); got != `{"pending_judge":null,"review":null}` {
			t.Fatalf("envelope = %s, want {\"pending_judge\":null,\"review\":null}", got)
		}
	})

	// ---- a settled verdict with no re-judge: review populated, pending_judge null -----
	t.Run("judged with no judge in flight", func(t *testing.T) {
		st, runID, _ := oneRecStore() // judged: a review + one recommendation, pendingJudge nil
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		h.GetRunReview(rec, runReq(store.User{ID: st.ownerID}, runID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRunReview = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(body["review"]) == "null" {
			t.Fatal("review = null, want the settled verdict")
		}
		raw, ok := body["pending_judge"]
		if !ok || string(raw) != "null" {
			t.Fatalf("pending_judge = %s (present=%v), want an explicit null alongside a populated review",
				raw, ok)
		}
	})
}
