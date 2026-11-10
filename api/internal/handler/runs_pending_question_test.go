package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// pendingQuestionRow scripts the newest question message of run id.
func pendingQuestionRow(id uuid.UUID, payload string) store.PendingQuestionsForRunsRow {
	row := store.PendingQuestionsForRunsRow{RunID: id}
	if payload != "" {
		row.Payload = []byte(payload)
	}
	return row
}

// listPendingQuestions drives a list handler and returns run id -> raw item, so a test can
// tell an absent pending_question key from an empty one.
func listPendingQuestions(t *testing.T, serve func(http.ResponseWriter, *http.Request), target string) map[string]map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", target, rec.Code, rec.Body)
	}
	var body struct {
		Runs []map[string]json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	out := make(map[string]map[string]json.RawMessage, len(body.Runs))
	for _, r := range body.Runs {
		var id string
		if err := json.Unmarshal(r["id"], &id); err != nil {
			t.Fatalf("decode id: %v", err)
		}
		out[id] = r
	}
	return out
}

func pendingQuestionOf(t *testing.T, item map[string]json.RawMessage) (string, bool) {
	t.Helper()
	raw, ok := item["pending_question"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode pending_question: %v", err)
	}
	return s, true
}

// PRD #4040: ListRuns decorates only the page's awaiting_input rows, in one batched call,
// and the decoration is best-effort.
func TestListRunsPendingQuestion(t *testing.T) {
	user := store.User{ID: uuid.New()}
	serve := func(h *Handler) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			h.ListRuns(w, r.WithContext(mw.ContextWithUser(r.Context(), user)))
		}
	}

	awaitA, awaitB := uuid.New(), uuid.New()
	running, completed, followup := uuid.New(), uuid.New(), uuid.New()
	pageRows := []store.ListRunsForUserRow{
		{Run: store.Run{ID: awaitA, Status: "awaiting_input"}},
		{Run: store.Run{ID: running, Status: "running"}},
		{Run: store.Run{ID: awaitB, Status: "awaiting_input"}},
		{Run: store.Run{ID: completed, Status: "completed"}},
		{Run: store.Run{ID: followup, Status: "awaiting_followup"}},
	}
	qA := `{"question_id":"q-1","questions":[{"question":"Should the cap\ncount queued exports?"},{"question":"second"}]}`
	qB := `{"question_id":"q-2","questions":[{"question":"Which store?"}]}`
	// Rows for non-awaiting runs exist but must never be requested, hence never surface.
	stray := `{"question_id":"q-9","questions":[{"question":"stale"}]}`

	for _, view := range []string{"/api/runs", "/api/runs?view=summary"} {
		t.Run("decorates awaiting_input rows only "+view, func(t *testing.T) {
			st := &runsStore{
				userRuns: pageRows,
				pendingQuestionRows: []store.PendingQuestionsForRunsRow{
					pendingQuestionRow(awaitA, qA), pendingQuestionRow(awaitB, qB),
					pendingQuestionRow(running, stray), pendingQuestionRow(completed, stray), pendingQuestionRow(followup, stray),
				},
			}
			items := listPendingQuestions(t, serve(newRunsHandler(t, st)), view)

			if len(st.pendingQuestionCalls) != 1 {
				t.Fatalf("PendingQuestionsForRuns calls = %d, want exactly 1", len(st.pendingQuestionCalls))
			}
			got := map[uuid.UUID]bool{}
			for _, id := range st.pendingQuestionCalls[0] {
				got[id] = true
			}
			if len(got) != 2 || !got[awaitA] || !got[awaitB] {
				t.Fatalf("queried ids = %v, want exactly the two awaiting_input ids", st.pendingQuestionCalls[0])
			}
			if s, ok := pendingQuestionOf(t, items[awaitA.String()]); !ok || s != "Should the cap count queued exports? (+1 more)" {
				t.Errorf("awaitA pending_question = %q, %v", s, ok)
			}
			if s, ok := pendingQuestionOf(t, items[awaitB.String()]); !ok || s != "Which store?" {
				t.Errorf("awaitB pending_question = %q, %v", s, ok)
			}
			for _, id := range []uuid.UUID{running, completed, followup} {
				if s, ok := pendingQuestionOf(t, items[id.String()]); ok {
					t.Errorf("run %s must not carry pending_question, got %q", id, s)
				}
			}
		})
	}

	t.Run("no awaiting_input rows skips the query", func(t *testing.T) {
		st := &runsStore{
			userRuns:            []store.ListRunsForUserRow{{Run: store.Run{ID: running, Status: "running"}}},
			pendingQuestionRows: []store.PendingQuestionsForRunsRow{pendingQuestionRow(running, stray)},
		}
		items := listPendingQuestions(t, serve(newRunsHandler(t, st)), "/api/runs")
		if len(st.pendingQuestionCalls) != 0 {
			t.Fatalf("PendingQuestionsForRuns called %d times, want 0", len(st.pendingQuestionCalls))
		}
		if _, ok := pendingQuestionOf(t, items[running.String()]); ok {
			t.Error("pending_question must be absent")
		}
	})

	t.Run("store error is best-effort", func(t *testing.T) {
		st := &runsStore{
			userRuns:           []store.ListRunsForUserRow{{Run: store.Run{ID: awaitA, Status: "awaiting_input"}}},
			pendingQuestionErr: errors.New("boom"),
		}
		items := listPendingQuestions(t, serve(newRunsHandler(t, st)), "/api/runs")
		if len(st.pendingQuestionCalls) != 1 {
			t.Fatalf("calls = %d, want 1", len(st.pendingQuestionCalls))
		}
		if _, ok := pendingQuestionOf(t, items[awaitA.String()]); ok {
			t.Error("pending_question must be omitted on a store error")
		}
	})

	t.Run("oversized (NULL) and malformed payloads omit the field", func(t *testing.T) {
		st := &runsStore{
			userRuns: []store.ListRunsForUserRow{
				{Run: store.Run{ID: awaitA, Status: "awaiting_input"}},
				{Run: store.Run{ID: awaitB, Status: "awaiting_input"}},
				{Run: store.Run{ID: running, Status: "awaiting_input"}},
			},
			pendingQuestionRows: []store.PendingQuestionsForRunsRow{
				pendingQuestionRow(awaitA, ""), // NULL: the SQL bound dropped an oversized newest row
				pendingQuestionRow(awaitB, `{"question_id":"q","questions":`),
				pendingQuestionRow(running, `{"questions":[{"question":"no id"}]}`),
			},
		}
		items := listPendingQuestions(t, serve(newRunsHandler(t, st)), "/api/runs")
		for _, id := range []uuid.UUID{awaitA, awaitB, running} {
			if s, ok := pendingQuestionOf(t, items[id.String()]); ok {
				t.Errorf("run %s must omit pending_question, got %q", id, s)
			}
		}
	})
}

// PRD #4040: the admin list is never decorated, in either view, and never reads questions.
func TestAdminListRunsNeverCarriesPendingQuestion(t *testing.T) {
	admin := store.User{ID: uuid.New()}
	id := uuid.New()
	for _, target := range []string{"/api/admin/runs", "/api/admin/runs?view=summary"} {
		t.Run(target, func(t *testing.T) {
			st := &runsStore{
				activeRuns: []store.ListActiveRunsAllRow{{Run: store.Run{ID: id, Kind: "issue", Status: "awaiting_input"}}},
				pendingQuestionRows: []store.PendingQuestionsForRunsRow{
					pendingQuestionRow(id, `{"question_id":"q-1","questions":[{"question":"private to the owner"}]}`),
				},
			}
			h := newRunsHandler(t, st)
			items := listPendingQuestions(t, func(w http.ResponseWriter, r *http.Request) {
				h.AdminListRuns(w, r.WithContext(mw.ContextWithUser(r.Context(), admin)))
			}, target)
			if _, ok := items[id.String()]; !ok {
				t.Fatalf("admin list did not return the run: %v", items)
			}
			if s, ok := pendingQuestionOf(t, items[id.String()]); ok {
				t.Errorf("admin list must not carry pending_question, got %q", s)
			}
			if len(st.pendingQuestionCalls) != 0 {
				t.Errorf("admin list must not read questions, got %d calls", len(st.pendingQuestionCalls))
			}
		})
	}
}
