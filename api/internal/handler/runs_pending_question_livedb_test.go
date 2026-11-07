package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// seedAwaitingInputRun seeds a summaryLiveRun and parks it at awaiting_input.
func seedAwaitingInputRun(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID, ageSeconds int) summaryLiveRun {
	t.Helper()
	r := seedSummaryRun(t, pool, ownerID, ageSeconds)
	cliMustExec(t, pool, `UPDATE runs SET status = 'awaiting_input' WHERE id = $1`, r.id)
	return r
}

// seedQuestionMessage inserts a `question` run message (jsonb payload) at seq.
func seedQuestionMessage(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, seq int, payload string) {
	t.Helper()
	cliMustExec(t, pool,
		`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, $2, 'question', $3::jsonb)`,
		runID, seq, payload)
}

func questionPayloadFor(id string, questions ...string) string {
	type q struct {
		Question string `json:"question"`
	}
	p := struct {
		QuestionID string `json:"question_id"`
		Questions  []q    `json:"questions"`
	}{QuestionID: id}
	for _, text := range questions {
		p.Questions = append(p.Questions, q{Question: text})
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func livePendingQuestion(t *testing.T, b liveListBody, id uuid.UUID) (string, bool) {
	t.Helper()
	for _, row := range b.Runs {
		var got string
		if err := json.Unmarshal(row["id"], &got); err != nil {
			t.Fatalf("row id: %v", err)
		}
		if got != id.String() {
			continue
		}
		raw, ok := row["pending_question"]
		if !ok {
			return "", false
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode pending_question: %v", err)
		}
		return s, true
	}
	t.Fatalf("run %s not in list", id)
	return "", false
}

// TestRunListPendingQuestionLiveDB (PRD #4040) drives the owner and admin lists through the
// real router against Postgres: the batched newest-question-per-run query, its bound in
// SQL, and the owner-only scoping.
func TestRunListPendingQuestionLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	ownerID, otherID, adminID := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false), cliSeedUser(t, pool, true)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`,
			[]uuid.UUID{ownerID, otherID, adminID}); err != nil {
			t.Logf("cleanup users: %v", err)
		}
	})

	newest := seedAwaitingInputRun(t, pool, ownerID, 40)    // older question, then a newer one
	oversized := seedAwaitingInputRun(t, pool, ownerID, 30) // valid older question, oversized newest
	multi := seedAwaitingInputRun(t, pool, ownerID, 20)     // several questions in one payload
	running := seedSummaryRun(t, pool, ownerID, 10)         // not awaiting_input, has a question row
	foreign := seedAwaitingInputRun(t, pool, otherID, 5)

	seedQuestionMessage(t, pool, newest.id, 1, questionPayloadFor("q-old", "The older question?"))
	seedQuestionMessage(t, pool, newest.id, 2, questionPayloadFor("q-new", "Should the cap count\nexports still queued?"))
	seedQuestionMessage(t, pool, oversized.id, 1, questionPayloadFor("q-old", "The older valid question?"))
	seedQuestionMessage(t, pool, oversized.id, 2, questionPayloadFor("q-big", strings.Repeat("x", 70*1024)))
	seedQuestionMessage(t, pool, multi.id, 1, questionPayloadFor("q-m", "First?", "Second?", "Third?"))
	seedQuestionMessage(t, pool, running.id, 1, questionPayloadFor("q-r", "Not parked"))
	seedQuestionMessage(t, pool, foreign.id, 1, questionPayloadFor("q-f", "Someone else's question"))

	ownerTok := cliMintToken(t, pool, ownerID, clitoken.ScopeUser)
	for _, path := range []string{"/api/runs", "/api/runs?view=summary"} {
		b := liveList(t, router, path, ownerTok)
		if s, ok := livePendingQuestion(t, b, newest.id); !ok || s != "Should the cap count exports still queued?" {
			t.Errorf("%s newest: pending_question = %q, %v; want the newest question excerpt", path, s, ok)
		}
		if s, ok := livePendingQuestion(t, b, oversized.id); ok {
			t.Errorf("%s oversized newest: pending_question = %q; want none (never the older question)", path, s)
		}
		if s, ok := livePendingQuestion(t, b, multi.id); !ok || s != "First? (+2 more)" {
			t.Errorf("%s multi: pending_question = %q, %v", path, s, ok)
		}
		if s, ok := livePendingQuestion(t, b, running.id); ok {
			t.Errorf("%s non-awaiting run carries pending_question %q", path, s)
		}
		if got := b.idsIn(t, map[string]bool{foreign.id.String(): true}); len(got) != 0 {
			t.Errorf("%s leaked another user's run %v", path, got)
		}
	}

	// Admin lists never carry the field, even for an awaiting_input run with a question.
	adminTok := cliMintToken(t, pool, adminID, clitoken.ScopeAdminRO)
	for _, path := range []string{"/api/admin/runs", "/api/admin/runs?view=summary"} {
		b := liveList(t, router, path, adminTok)
		seen := 0
		for _, id := range []uuid.UUID{newest.id, oversized.id, multi.id, foreign.id} {
			for _, row := range b.Runs {
				var got string
				_ = json.Unmarshal(row["id"], &got)
				if got != id.String() {
					continue
				}
				seen++
				if _, ok := row["pending_question"]; ok {
					t.Errorf("%s run %s carries pending_question", path, id)
				}
			}
		}
		if seen != 4 {
			t.Errorf("%s returned %d of the 4 seeded awaiting_input runs", path, seen)
		}
	}
}
