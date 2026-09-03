package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// toolUseRow builds a scripted newest-tool_use row (LatestToolUseForRuns) for run id,
// with the given frame agent/agent_label and the tool_use payload JSON runactivity folds.
func toolUseRow(id uuid.UUID, seq int32, agent, agentLabel, payload string) store.LatestToolUseForRunsRow {
	return store.LatestToolUseForRunsRow{
		RunID:      id,
		Seq:        seq,
		Kind:       "tool_use",
		Agent:      pgtype.Text{String: agent, Valid: agent != ""},
		AgentLabel: pgtype.Text{String: agentLabel, Valid: agentLabel != ""},
		Payload:    []byte(payload),
		CreatedAt:  pgtype.Timestamptz{Time: time.Unix(1_700_000_000+int64(seq), 0).UTC(), Valid: true},
	}
}

// currentActivityJSON is the decode target for a RunDTO's current_activity object.
type currentActivityJSON struct {
	Agent      string `json:"agent"`
	AgentLabel string `json:"agent_label"`
	Tool       string `json:"tool"`
	Detail     string `json:"detail"`
	Seq        int32  `json:"seq"`
}

// TestCurrentActivityDTO exercises PRD #1064 M2's current_activity population glue — the
// wiring runToDTO deliberately does NOT do (it stays pure), set instead in the handler's
// list builders and the GET path from the batched LatestToolUseForRuns lookup. It asserts
// the folded fields land, the IsTerminalRunStatus filter is applied (a terminal run reads
// null even with a row), a run with no tool_use frame reads null, and the batched query is
// keyed per run id so two runs map to their OWN activity.
//
// It goes RED if the terminal filter or the per-id keying is removed (verified by mutating
// the handler in a scratch copy).
func TestCurrentActivityDTO(t *testing.T) {
	// Two non-terminal runs with DIFFERENT activities (per-id keying), a terminal run that
	// still has a scripted row (terminal filter), and a non-terminal run with no row.
	runRead := uuid.New()       // non-terminal: a Read tool_use → file_path detail, frame agent verbatim
	runAgent := uuid.New()      // non-terminal: an Agent dispatch → subagent_type/description fold
	runTerminal := uuid.New()   // completed, yet a row exists → must read null
	runNoActivity := uuid.New() // non-terminal, no tool_use row → must read null

	rows := []store.LatestToolUseForRunsRow{
		toolUseRow(runRead, 7, "coder", "implement the handler",
			`{"name":"Read","input":{"file_path":"api/internal/handler/runs.go"}}`),
		toolUseRow(runAgent, 4, "lead", "",
			`{"name":"Agent","input":{"subagent_type":"reviewer","description":"review the diff"}}`),
		// Present but must never surface: the run is terminal, so its id is never queried.
		toolUseRow(runTerminal, 9, "coder", "leftover",
			`{"name":"Read","input":{"file_path":"api/internal/handler/runs_dto.go"}}`),
	}

	t.Run("list builder: per-id keying, terminal filter, and no-row all map correctly", func(t *testing.T) {
		user := store.User{ID: uuid.New()}
		st := &runsStore{
			userRuns: []store.ListRunsForUserRow{
				{Run: store.Run{ID: runRead, Status: "running"}},
				{Run: store.Run{ID: runAgent, Status: "running"}},
				{Run: store.Run{ID: runTerminal, Status: "completed"}},
				{Run: store.Run{ID: runNoActivity, Status: "queued"}},
			},
			latestToolUseRows: rows,
		}
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
		if rec.Code != http.StatusOK {
			t.Fatalf("ListRuns = %d, want 200", rec.Code)
		}

		var body struct {
			Runs []struct {
				ID              string               `json:"id"`
				CurrentActivity *currentActivityJSON `json:"current_activity"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		byID := make(map[string]*currentActivityJSON, len(body.Runs))
		for _, r := range body.Runs {
			byID[r.ID] = r.CurrentActivity
		}

		// runRead: a Read tool_use → Tool "Read", Detail the file_path, frame agent verbatim.
		act := byID[runRead.String()]
		if act == nil {
			t.Fatalf("runRead current_activity = null, want the Read now line")
		}
		if act.Tool != "Read" || act.Detail != "api/internal/handler/runs.go" || act.Agent != "coder" || act.Seq != 7 {
			t.Errorf("runRead activity = %+v, want {agent:coder tool:Read detail:api/internal/handler/runs.go seq:7}", act)
		}

		// runAgent: an Agent dispatch → Agent from subagent_type, Detail/AgentLabel from the
		// description. Distinct from runRead — proving the batched result is keyed per id.
		act = byID[runAgent.String()]
		if act == nil {
			t.Fatalf("runAgent current_activity = null, want the Agent now line")
		}
		if act.Tool != "Agent" || act.Agent != "reviewer" || act.Detail != "review the diff" || act.AgentLabel != "review the diff" {
			t.Errorf("runAgent activity = %+v, want {agent:reviewer tool:Agent detail/label:review the diff}", act)
		}

		// runTerminal: completed, so its id was never queried — null even though a row exists.
		if act := byID[runTerminal.String()]; act != nil {
			t.Errorf("a terminal run must read a null current_activity even with a row, got %+v", act)
		}
		// runNoActivity: non-terminal but no tool_use row → null (the back-compat contract).
		if act := byID[runNoActivity.String()]; act != nil {
			t.Errorf("a run with no tool_use frame must read a null current_activity, got %+v", act)
		}

		// The IsTerminalRunStatus filter must keep the terminal run out of the batched query.
		for _, id := range st.latestToolUseArg {
			if id == runTerminal {
				t.Errorf("terminal run id %s must be excluded from LatestToolUseForRuns, got arg %v", runTerminal, st.latestToolUseArg)
			}
		}
		if len(st.latestToolUseArg) != 3 {
			t.Errorf("LatestToolUseForRuns arg len = %d, want 3 (the non-terminal ids): %v", len(st.latestToolUseArg), st.latestToolUseArg)
		}
	})

	t.Run("get path: populates a non-terminal run's now line", func(t *testing.T) {
		owner := store.User{ID: uuid.New()}
		st := &runsStore{
			ownerID:           owner.ID,
			run:               store.Run{ID: runRead, UserID: owner.ID, Status: "running"},
			latestToolUseRows: rows,
		}
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		h.GetRun(rec, runReq(owner, runRead))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRun = %d, want 200", rec.Code)
		}
		var body struct {
			Run struct {
				CurrentActivity *currentActivityJSON `json:"current_activity"`
			} `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		act := body.Run.CurrentActivity
		if act == nil {
			t.Fatalf("GetRun current_activity = null, want the Read now line")
		}
		if act.Tool != "Read" || act.Detail != "api/internal/handler/runs.go" || act.Agent != "coder" {
			t.Errorf("GetRun activity = %+v, want {agent:coder tool:Read detail:api/internal/handler/runs.go}", act)
		}
	})

	t.Run("get path: terminal run reads null even with a row", func(t *testing.T) {
		owner := store.User{ID: uuid.New()}
		st := &runsStore{
			ownerID:           owner.ID,
			run:               store.Run{ID: runTerminal, UserID: owner.ID, Status: "completed"},
			latestToolUseRows: rows,
		}
		h := newRunsHandler(t, st)

		rec := httptest.NewRecorder()
		h.GetRun(rec, runReq(owner, runTerminal))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRun = %d, want 200", rec.Code)
		}
		var body struct {
			Run struct {
				CurrentActivity *currentActivityJSON `json:"current_activity"`
			} `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Run.CurrentActivity != nil {
			t.Errorf("a terminal run's GetRun must read null current_activity, got %+v", body.Run.CurrentActivity)
		}
		// A terminal run short-circuits before the query — it is never called at all.
		if st.latestToolUseArg != nil {
			t.Errorf("a terminal GetRun must not query LatestToolUseForRuns, got arg %v", st.latestToolUseArg)
		}
	})
}
