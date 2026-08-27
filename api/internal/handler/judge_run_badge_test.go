package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #98 M4: the /runs per-row judge badge. The two fields come from DIFFERENT mechanisms
// and these tests keep that visible — the verdict rides the list query's safe UNIQUE join,
// the count is a separate bounded read bucketed in Go by the shared BucketOf.

func listRuns(t *testing.T, h *Handler, user store.User) []apitypes.RunListItemDTO {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Runs
}

// TestRunListJudgeTodoCountUsesSharedLadder: the count is the number of recommendations the
// SHARED BucketOf calls todo — not "every recommendation", and not a SQL tally. The fixture
// spans the ladder so a wrong rung shows up as a wrong number: two open, one settled-filed,
// one done, one dismissed → 2.
func TestRunListJudgeTodoCountUsesSharedLadder(t *testing.T) {
	txt := func(s string) pgtype.Text {
		if s == "" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	st, runID := runsStoreWithOneRun(t)
	st.judgeTriageRunRows = []store.ListJudgeTriageRowsForRunsRow{
		{RunID: runID, DispositionStatus: txt(""), FiledSettled: false},          // todo
		{RunID: runID, DispositionStatus: txt(""), FiledSettled: false},          // todo
		{RunID: runID, DispositionStatus: txt(""), FiledSettled: true},           // filed → not todo
		{RunID: runID, DispositionStatus: txt("done"), FiledSettled: false},      // done
		{RunID: runID, DispositionStatus: txt("dismissed"), FiledSettled: false}, // dismissed
	}
	h := newRunsHandler(t, st)

	runs := listRuns(t, h, store.User{ID: st.ownerID})
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].JudgeTodoCount != 2 {
		t.Fatalf("judge_todo_count = %d, want 2 — only the bucket==todo members count", runs[0].JudgeTodoCount)
	}
	// Owner scoping and the bound both reach the query.
	if st.judgeTriageRunArg == nil || st.judgeTriageRunArg.UserID != st.ownerID {
		t.Fatalf("triage query args = %+v, want it scoped to the caller", st.judgeTriageRunArg)
	}
	if st.judgeTriageRunArg.Lim != workersvc.JudgeRunTodoMaxRows {
		t.Errorf("limit = %d, want the bound %d — this enumeration must be bounded",
			st.judgeTriageRunArg.Lim, workersvc.JudgeRunTodoMaxRows)
	}
	if len(st.judgeTriageRunArg.RunIds) != 1 || st.judgeTriageRunArg.RunIds[0] != runID {
		t.Errorf("run ids = %v, want just the page's run", st.judgeTriageRunArg.RunIds)
	}
}

// TestRunListUnjudgedRunHasNoVerdict: an unjudged run carries a NULL verdict and a zero
// count, and the DTO keeps the verdict nil rather than inventing a neutral one — "not
// judged" and "judged fine" are different facts and the badge renders them differently.
func TestRunListUnjudgedRunHasNoVerdict(t *testing.T) {
	st, _ := runsStoreWithOneRun(t)
	h := newRunsHandler(t, st)

	runs := listRuns(t, h, store.User{ID: st.ownerID})
	if runs[0].JudgeVerdict != nil {
		t.Fatalf("judge_verdict = %q, want nil for an unjudged run", *runs[0].JudgeVerdict)
	}
	if runs[0].JudgeTodoCount != 0 {
		t.Fatalf("judge_todo_count = %d, want 0", runs[0].JudgeTodoCount)
	}
}

// TestRunListJudgeCountFailureDoesNotFailTheList: the badge is decoration, so a failure in
// the (separate, best-effort) count read must leave the run list itself intact with counts
// at 0 — never a 500. The list is the page; the badge is an ornament on it.
func TestRunListJudgeCountFailureDoesNotFailTheList(t *testing.T) {
	st, _ := runsStoreWithOneRun(t)
	st.judgeTriageRunErr = errTriageBoom
	h := newRunsHandler(t, st)

	runs := listRuns(t, h, store.User{ID: st.ownerID})
	if len(runs) != 1 {
		t.Fatalf("the run list must survive a judge-count failure, got %d runs", len(runs))
	}
	if runs[0].JudgeTodoCount != 0 {
		t.Errorf("judge_todo_count = %d, want 0 when the count read failed", runs[0].JudgeTodoCount)
	}
}

var errTriageBoom = errors.New("triage read failed")

// runsStoreWithOneRun is one owner holding a single completed run on the list.
func runsStoreWithOneRun(t *testing.T) (*runsStore, uuid.UUID) {
	t.Helper()
	ownerID, runID := uuid.New(), uuid.New()
	return &runsStore{
		ownerID: ownerID,
		userRuns: []store.ListRunsForUserRow{{
			Run:      store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: "issue"},
			RepoPath: "g/r",
		}},
	}, runID
}
