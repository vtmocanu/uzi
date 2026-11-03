package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3841: SnapshotOwnerSteering is one statement whose shape guards are ordered CASE arms.
// These tests run it against raw rows of every wrong shape; none may fail the statement.

func steeringInput(fx *fleetFixture, run uuid.UUID, kind string, body, questionID any) int64 {
	fx.t.Helper()
	var id int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO run_user_inputs (run_id, kind, body, question_id)
		VALUES ($1, $2, $3, $4) RETURNING id`, run, kind, body, questionID).Scan(&id); err != nil {
		fx.t.Fatalf("insert %s input: %v", kind, err)
	}
	return id
}

// steeringQuestion inserts a question message at an explicit age (seconds before now), so the
// "at or before the answer" rule is deterministic.
func steeringQuestion(fx *fleetFixture, run uuid.UUID, seq int, agent, payload string, ageSeconds int) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO run_messages (run_id, seq, kind, agent, payload, created_at)
		VALUES ($1, $2, 'question', $3, $4::jsonb, now() - make_interval(secs => $5))`, run, seq, agent, payload, ageSeconds)
}

func steeringAnswerAt(fx *fleetFixture, run uuid.UUID, body, qid string, ageSeconds int) int64 {
	fx.t.Helper()
	var id int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO run_user_inputs (run_id, kind, body, question_id, created_at)
		VALUES ($1, 'answer', $2, $3, now() - make_interval(secs => $4)) RETURNING id`, run, body, qid, ageSeconds).Scan(&id); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func snapshotSteering(fx *fleetFixture, run uuid.UUID, max int32) []store.SnapshotOwnerSteeringRow {
	fx.t.Helper()
	rows, err := fx.q.SnapshotOwnerSteering(fx.ctx, store.SnapshotOwnerSteeringParams{RunID: run, MaxEntries: max})
	if err != nil {
		fx.t.Fatalf("SnapshotOwnerSteering must never fail on stored shapes: %v", err)
	}
	return rows
}

func steeringFixture(t *testing.T) *fleetFixture {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	return fx
}

func TestOwnerSteeringSelectionLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	run, other := fx.queuedRun(), fx.queuedRun()
	answerID := steeringInput(fx, run, "answer", `{"question_id":"q1","answers":["a"]}`, "q1")
	steeringInput(fx, run, "follow_up", "do more", nil)
	steeringInput(fx, run, "approve_plan", nil, nil)
	steeringInput(fx, run, "answer", `{"answers":["no qid"]}`, nil) // NULL question_id is not steering
	reviseID := steeringInput(fx, run, "revise_plan", "tighten", nil)
	last := steeringInput(fx, run, "follow_up", "trailing", nil)
	steeringInput(fx, other, "answer", `{"answers":["other run"]}`, "q9")

	// A second user's run never leaks into the first run's snapshot.
	fx2 := steeringFixture(t)
	foreign := fx2.queuedRun()
	steeringInput(fx2, foreign, "answer", `{"answers":["other user"]}`, "q1")

	rows := snapshotSteering(fx, run, 32)
	if len(rows) != 2 {
		t.Fatalf("selected %d rows, want the answer and the revise only", len(rows))
	}
	if rows[0].InputID.Int64 != answerID || rows[1].InputID.Int64 != reviseID {
		t.Fatalf("order/selection wrong: %d,%d want %d,%d", rows[0].InputID.Int64, rows[1].InputID.Int64, answerID, reviseID)
	}
	if rows[0].Cutoff != last || rows[0].Total != 2 {
		t.Fatalf("cutoff=%d total=%d, want %d and 2 (cutoff is the newest input of any kind)", rows[0].Cutoff, rows[0].Total, last)
	}

	empty := snapshotSteering(fx, fx.queuedRun(), 32)
	if len(empty) != 1 || empty[0].InputID.Valid || empty[0].Cutoff != 0 || empty[0].Total != 0 {
		t.Fatalf("zero-entry run must still return its cut row: %+v", empty)
	}
}

func TestOwnerSteeringCapAndTruncationLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	run := fx.queuedRun()
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, steeringInput(fx, run, "answer", fmt.Sprintf(`{"answers":["a%d"]}`, i), fmt.Sprintf("q%d", i)))
	}
	rows := snapshotSteering(fx, run, 3)
	if len(rows) != 3 {
		t.Fatalf("kept %d rows; want 3 of 5", len(rows))
	}
	if rows[0].Total != 5 {
		t.Fatalf("total %d; want 5", rows[0].Total)
	}
	for i, r := range rows {
		if r.InputID.Int64 != ids[2+i] {
			t.Fatalf("kept row %d = %d, want the newest three ascending (%d)", i, r.InputID.Int64, ids[2+i])
		}
	}

	trunc := fx.queuedRun()
	long := strings.Repeat("é", 10000) // beyond cap+margin: delivered as 8192+1024 characters
	steeringInput(fx, trunc, "revise_plan", long, nil)
	steeringInput(fx, trunc, "revise_plan", nil, nil)
	steeringInput(fx, trunc, "revise_plan", strings.Repeat("x", 8192), nil)
	bigAnswer, _ := json.Marshal(map[string]any{"answers": []string{strings.Repeat("y", 4000), "short"}})
	steeringInput(fx, trunc, "answer", string(bigAnswer), "qt")
	r := snapshotSteering(fx, trunc, 32)
	if len(r) != 4 {
		t.Fatalf("got %d rows", len(r))
	}
	if !r[0].ReviseTruncated.Bool || len([]rune(r[0].ReviseBody.String)) != 8192+1024 {
		t.Fatalf("long revise: truncated=%v chars=%d", r[0].ReviseTruncated.Bool, len([]rune(r[0].ReviseBody.String)))
	}
	if r[1].ReviseBody.Valid {
		t.Fatal("NULL revise body must stay NULL")
	}
	if r[2].ReviseTruncated.Bool {
		t.Fatal("exactly 8192 characters is not truncated")
	}
	var answers []string
	if err := json.Unmarshal(r[3].Answers, &answers); err != nil || len(answers) != 2 || len(answers[0]) != 2048+1024 || answers[1] != "short" || !r[3].AnswersTruncated.Bool {
		t.Fatalf("answer truncation: %v err=%v truncated=%v", len(answers), err, r[3].AnswersTruncated.Bool)
	}

	// In-between band: past the authoritative cap but inside cap+margin, delivered whole and
	// still flagged truncated so the Go builder applies the cap after scrubbing.
	band := fx.queuedRun()
	steeringInput(fx, band, "revise_plan", strings.Repeat("r", 9000), nil)
	bandAnswer, _ := json.Marshal(map[string]any{"answers": []string{strings.Repeat("z", 2500)}})
	steeringInput(fx, band, "answer", string(bandAnswer), "qb")
	b := snapshotSteering(fx, band, 32)
	if len(b) != 2 {
		t.Fatalf("band run returned %d rows, want 2", len(b))
	}
	if !b[0].ReviseTruncated.Bool || len(b[0].ReviseBody.String) != 9000 {
		t.Fatalf("9000-char revise: truncated=%v chars=%d, want whole and flagged", b[0].ReviseTruncated.Bool, len(b[0].ReviseBody.String))
	}
	var bandAnswers []string
	if err := json.Unmarshal(b[1].Answers, &bandAnswers); err != nil || len(bandAnswers) != 1 || len(bandAnswers[0]) != 2500 || !b[1].AnswersTruncated.Bool {
		t.Fatalf("2500-char answer: %d elements err=%v truncated=%v, want whole and flagged", len(bandAnswers), err, b[1].AnswersTruncated.Bool)
	}
}

func TestOwnerSteeringWrongShapeAnswersLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	elevenAnswers := `{"answers":["1","2","3","4","5","6","7","8","9","10","11"]}`
	tenAnswers := `{"answers":["1","2","3","4","5","6","7","8","9","10"]}`
	for _, tc := range []struct {
		name, body, want string
		text             bool
	}{
		{"invalid json", `{"answers":["a"`, "unparseable", false},
		{"empty text", ``, "unparseable", false},
		{"nul escape", `{"answers":["\u0000"]}`, "unparseable", false},
		{"top-level array", `["a"]`, "malformed", false},
		{"top-level scalar", `"a"`, "malformed", false},
		{"top-level number", `7`, "malformed", false},
		{"answers scalar", `{"answers":"a"}`, "malformed", false},
		{"answers object", `{"answers":{"0":"a"}}`, "malformed", false},
		{"answers missing", `{"question_id":"q"}`, "malformed", false},
		{"answers null", `{"answers":null}`, "malformed", false},
		{"number element", `{"answers":["a",5]}`, "malformed", false},
		{"object element", `{"answers":["a",{"x":1}]}`, "malformed", false},
		{"null element", `{"answers":["a",null]}`, "malformed", false},
		{"bool element", `{"answers":[true]}`, "malformed", false},
		{"eleven", elevenAnswers, "oversized", false},
		{"ten", tenAnswers, "ok", true},
		{"empty list", `{"answers":[]}`, "ok", true},
		{"ok", `{"question_id":"q","answers":["x","y"]}`, "ok", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := fx.queuedRun()
			steeringInput(fx, run, "answer", tc.body, "q")
			r := snapshotSteering(fx, run, 32)
			if len(r) != 1 {
				t.Fatalf("rows=%d, want 1", len(r))
			}
			if r[0].AnswersStatus.String != tc.want {
				t.Fatalf("status %q, want %q", r[0].AnswersStatus.String, tc.want)
			}
			if tc.text != (r[0].Answers != nil) {
				t.Fatalf("answers projection present=%v, want %v: %s", r[0].Answers != nil, tc.text, r[0].Answers)
			}
		})
	}
	// A NULL body is the same unparseable arm, never an error.
	run := fx.queuedRun()
	steeringInput(fx, run, "answer", nil, "q")
	if r := snapshotSteering(fx, run, 32); r[0].AnswersStatus.String != "unparseable" {
		t.Fatalf("NULL body status %q", r[0].AnswersStatus.String)
	}
	// A non-string element is never stringified into the projection.
	run = fx.queuedRun()
	steeringInput(fx, run, "answer", `{"answers":["a",5]}`, "q")
	if r := snapshotSteering(fx, run, 32); strings.Contains(string(r[0].Answers), "5") || r[0].Answers != nil {
		t.Fatal("non-string element was stringified")
	}
}

func TestOwnerSteeringWrongShapeQuestionsLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	elevenQuestions := "[" + strings.TrimSuffix(strings.Repeat(`{"question":"q"},`, 11), ",") + "]"
	elevenOptions := `[{"question":"q","options":[` + strings.TrimSuffix(strings.Repeat(`{"label":"l"},`, 11), ",") + `]}]`
	for _, tc := range []struct{ name, questions, want string }{
		{"missing", `null`, "malformed"},
		{"object", `{"question":"q"}`, "malformed"},
		{"string", `"q"`, "malformed"},
		{"eleven questions", elevenQuestions, "oversized"},
		{"element scalar", `["q"]`, "malformed"},
		{"element array", `[["q"]]`, "malformed"},
		{"question not string", `[{"question":5}]`, "malformed"},
		{"question missing", `[{"header":"h"}]`, "malformed"},
		{"header number", `[{"question":"q","header":3}]`, "malformed"},
		{"header null", `[{"question":"q","header":null}]`, "malformed"},
		{"multiSelect string", `[{"question":"q","multiSelect":"yes"}]`, "malformed"},
		{"options object", `[{"question":"q","options":{"label":"l"}}]`, "malformed"},
		{"options scalar", `[{"question":"q","options":"l"}]`, "malformed"},
		{"eleven options", elevenOptions, "malformed"},
		{"option string", `[{"question":"q","options":["l"]}]`, "malformed"},
		{"option label number", `[{"question":"q","options":[{"label":1}]}]`, "malformed"},
		{"option label missing", `[{"question":"q","options":[{"description":"d"}]}]`, "malformed"},
		{"ok minimal", `[{"question":"q"}]`, "ok"},
		{"ok full", `[{"header":"h","question":"q","multiSelect":true,"options":[{"label":"a","description":"d"},{"label":"b"}]}]`, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := fx.queuedRun()
			payload := fmt.Sprintf(`{"question_id":"q1","questions":%s}`, tc.questions)
			steeringQuestion(fx, run, 1, "lead", payload, 20)
			steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 10)
			r := snapshotSteering(fx, run, 32)
			if len(r) != 1 {
				t.Fatalf("rows=%d, want 1", len(r))
			}
			if r[0].QuestionsStatus.String != tc.want || r[0].Occurrences.Int64 != 1 {
				t.Fatalf("status %q occurrences %d, want %q", r[0].QuestionsStatus.String, r[0].Occurrences.Int64, tc.want)
			}
			if (tc.want == "ok") != (r[0].Questions != nil) {
				t.Fatalf("text projection present=%v for status %s", r[0].Questions != nil, tc.want)
			}
		})
	}
	// An ok projection is bounded and shaped for the builder.
	run := fx.queuedRun()
	steeringQuestion(fx, run, 1, "worker", fmt.Sprintf(`{"question_id":"q1","questions":[{"header":%q,"question":"q","multiSelect":true,"options":[{"label":"a"}]}]}`, strings.Repeat("h", 2000)), 20)
	steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 10)
	r := snapshotSteering(fx, run, 32)
	var qs []struct {
		Header      string   `json:"header"`
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		MultiSelect bool     `json:"multi_select"`
	}
	if err := json.Unmarshal(r[0].Questions, &qs); err != nil || len(qs) != 1 || len(qs[0].Header) != 128+1024 || !qs[0].MultiSelect ||
		len(qs[0].Options) != 1 || qs[0].Options[0] != "a" || !r[0].QuestionsTruncated.Bool || r[0].QuestionAgent.String != "worker" {
		t.Fatalf("projection wrong: %+v err=%v truncated=%v", qs, err, r[0].QuestionsTruncated.Bool)
	}
}

func TestOwnerSteeringContextBindingLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	q := func(id, text string) string {
		return fmt.Sprintf(`{"question_id":%q,"questions":[{"question":%q}]}`, id, text)
	}
	t.Run("identical re-emissions bind once", func(t *testing.T) {
		run := fx.queuedRun()
		steeringQuestion(fx, run, 1, "lead", q("q1", "same"), 30)
		steeringQuestion(fx, run, 2, "lead", q("q1", "same"), 20)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 10)
		r := snapshotSteering(fx, run, 32)[0]
		if r.Occurrences.Int64 != 2 || r.Variants.Int64 != 1 || r.QuestionsStatus.String != "ok" {
			t.Fatalf("occurrences=%d variants=%d status=%s", r.Occurrences.Int64, r.Variants.Int64, r.QuestionsStatus.String)
		}
	})
	t.Run("differing re-emissions are ambiguous", func(t *testing.T) {
		run := fx.queuedRun()
		steeringQuestion(fx, run, 1, "lead", q("q1", "first"), 30)
		steeringQuestion(fx, run, 2, "lead", q("q1", "second"), 20)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 10)
		if r := snapshotSteering(fx, run, 32)[0]; r.Variants.Int64 != 2 {
			t.Fatalf("variants=%d, want 2", r.Variants.Int64)
		}
	})
	t.Run("same text from a different agent is a variant", func(t *testing.T) {
		run := fx.queuedRun()
		steeringQuestion(fx, run, 1, "lead", q("q1", "same"), 30)
		steeringQuestion(fx, run, 2, "worker", q("q1", "same"), 20)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 10)
		if r := snapshotSteering(fx, run, 32)[0]; r.Variants.Int64 != 2 {
			t.Fatalf("variants=%d, want 2", r.Variants.Int64)
		}
	})
	t.Run("an occurrence after the answer is excluded", func(t *testing.T) {
		run := fx.queuedRun()
		steeringQuestion(fx, run, 1, "lead", q("q1", "first"), 30)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 20)
		steeringQuestion(fx, run, 2, "lead", q("q1", "later"), 10)
		r := snapshotSteering(fx, run, 32)[0]
		if r.Occurrences.Int64 != 1 || r.Variants.Int64 != 1 || !strings.Contains(string(r.Questions), "first") {
			t.Fatalf("later emission leaked: occurrences=%d variants=%d %s", r.Occurrences.Int64, r.Variants.Int64, r.Questions)
		}
	})
	t.Run("no occurrence", func(t *testing.T) {
		run := fx.queuedRun()
		steeringQuestion(fx, run, 1, "lead", q("other", "x"), 30)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q1", 10)
		r := snapshotSteering(fx, run, 32)[0]
		if r.Occurrences.Int64 != 0 || r.QuestionsStatus.Valid || r.Questions != nil {
			t.Fatalf("unavailable context carries data: %+v", r)
		}
	})
	t.Run("completion continue answer is excluded and uncounted", func(t *testing.T) {
		run := fx.queuedRun()
		steeringQuestion(fx, run, 2, "lead", q("q2", "ordinary"), 30)
		keptID := steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q2", 10)
		// The completion answer is the NEWEST input, so with MaxEntries=1 a regression would take
		// the kept slot. The audit row is written non-transactionally: only the shared
		// question_id links it.
		steeringQuestion(fx, run, 1, "lead", `{"question_id":"qc","questions":[{"header":"Completion blocked","question":"unmet"}]}`, 30)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body, question_id)
			VALUES ($1, 'completion_decision', 'continue', 'qc')`, run)
		steeringAnswerAt(fx, run, `{"answers":["continue"]}`, "qc", 0)
		r := snapshotSteering(fx, run, 1)
		if len(r) != 1 {
			t.Fatalf("rows=%d: want exactly the ordinary answer", len(r))
		}
		if r[0].InputID.Int64 != keptID || r[0].Total != 1 {
			t.Fatalf("first=%d total=%d: the completion answer must take no slot and not count", r[0].InputID.Int64, r[0].Total)
		}
	})
	t.Run("answer with a different question id than the completion decision is kept", func(t *testing.T) {
		run := fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body, question_id)
			VALUES ($1, 'completion_decision', 'continue', 'qc')`, run)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, "q-other", 10)
		// A completion_decision on ANOTHER run never excludes this run's answer.
		other := fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body, question_id)
			VALUES ($1, 'completion_decision', 'continue', 'q-other')`, other)
		if r := snapshotSteering(fx, run, 32); len(r) != 1 || !r[0].InputID.Valid || r[0].Total != 1 {
			t.Fatalf("answer must be kept: %+v", r)
		}
	})
}

// snapshotStatementSQL returns the SnapshotOwnerSteering statement text from queries/, with sqlc's
// named parameters rewritten to positional ones.
func snapshotStatementSQL(t *testing.T) string {
	t.Helper()
	body, ok := namedQueryBodies(t)["SnapshotOwnerSteering"]
	if !ok {
		t.Fatal("SnapshotOwnerSteering not found in queries/")
	}
	_, body, _ = strings.Cut(body, "\n")
	body = strings.ReplaceAll(body, "@run_id", "$1")
	body = strings.ReplaceAll(body, "@max_entries", "$2")
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), ";"))
}

// Under pgx statement caching Postgres can switch to a generic plan; the snapshot must stay
// bounded by the kept entries there too (no CTE re-run per outer row).
func TestOwnerSteeringGenericPlanBoundedLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	run := fx.queuedRun()
	for i := 0; i < 32; i++ {
		qid := fmt.Sprintf("qg%d", i)
		steeringQuestion(fx, run, i+1, "lead", fmt.Sprintf(`{"question_id":%q,"questions":[{"question":"q"}]}`, qid), 100)
		steeringAnswerAt(fx, run, `{"answers":["a"]}`, qid, 10)
	}
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fx.ctx) }()
	if _, err := tx.Exec(fx.ctx, `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	// Run the statement through a cached prepared statement so the generic plan is actually used.
	if _, err := tx.Exec(fx.ctx, `PREPARE steer_probe(uuid, int) AS `+snapshotStatementSQL(t)); err != nil {
		t.Fatal(err)
	}
	var plan []byte
	// EXPLAIN is a utility statement and takes no bind parameters, so the uuid is inlined.
	if err := tx.QueryRow(fx.ctx, fmt.Sprintf(`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE steer_probe('%s', 32)`, run)).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	var doc []struct{ Plan map[string]any }
	if err := json.Unmarshal(plan, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("plan json: %v", err)
	}
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if loops, _ := n["Actual Loops"].(float64); loops > 32 {
			t.Errorf("node %v ran %v loops under a generic plan, want <= 32", n["Node Type"], loops)
		}
		kids, _ := n["Plans"].([]any)
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				walk(m)
			}
		}
	}
	walk(doc[0].Plan)
}

func steeringPlanRound(fx *fleetFixture, stage string) uuid.UUID {
	fx.t.Helper()
	lead := fx.queuedRun()
	id := uuid.New()
	if stage == "plan" {
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
			(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,
			 checker_harness,deadline_at,automatic_rounds_enabled,automatic_revision_limit)
			VALUES ($1,$2,'plan',1,1,'plan','[]','s',repeat('a',40),$3,'claude',now()+interval '5 minutes',false,0)`,
			id, lead, []byte("digest"))
		return id
	}
	checker := fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
		(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,verdict,
		 head_commit,base_commit,candidate_digest,outcome,reason_class,code_context,
		 guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,
		 checker_harness,created_at,deadline_at,decided_at)
		VALUES ($1,$2,$3,'code',1,1,'failed',repeat('b',40),repeat('a',40),$4,'pending',NULL,'{}','',
		 '',sha256(convert_to('', 'UTF8')),'',sha256(convert_to('', 'UTF8')),'codex',
		 now()-interval '10 seconds',now()+interval '5 minutes',NULL)`, id, lead, checker, []byte("digest"))
	return id
}

func TestInsertOwnerSteeringLiveDB(t *testing.T) {
	fx := steeringFixture(t)
	digest := make([]byte, 32)
	plan := steeringPlanRound(fx, "plan")
	n, err := fx.q.InsertOwnerSteering(fx.ctx, store.InsertOwnerSteeringParams{CrossCheckID: plan, Snapshot: `{"version":1}`, Digest: digest})
	if err != nil || n != 1 {
		t.Fatalf("plan insert: n=%d err=%v", n, err)
	}
	got, err := fx.q.GetOwnerSteering(fx.ctx, plan)
	if err != nil || got.Snapshot != `{"version":1}` || len(got.Digest) != 32 {
		t.Fatalf("read back: %+v err=%v", got, err)
	}

	code := steeringPlanRound(fx, "code")
	if n, err := fx.q.InsertOwnerSteering(fx.ctx, store.InsertOwnerSteeringParams{CrossCheckID: code, Snapshot: "{}", Digest: digest}); err != nil || n != 0 {
		t.Fatalf("code-stage insert must write 0 rows: n=%d err=%v", n, err)
	}
	if _, err := fx.q.GetOwnerSteering(fx.ctx, code); err == nil {
		t.Fatal("a code-stage round gained a steering row")
	}

	_, err = fx.pool.Exec(fx.ctx, `UPDATE cross_check_owner_steering SET snapshot = '{}' WHERE cross_check_id = $1`, plan)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "immutable owner steering snapshot") {
		t.Fatalf("update of a frozen snapshot must raise: %v", err)
	}
	for name, p := range map[string]store.InsertOwnerSteeringParams{
		"digest length": {CrossCheckID: steeringPlanRound(fx, "plan"), Snapshot: "{}", Digest: make([]byte, 31)},
		"snapshot size": {CrossCheckID: steeringPlanRound(fx, "plan"), Snapshot: strings.Repeat("x", 65537), Digest: digest},
	} {
		if _, err := fx.q.InsertOwnerSteering(fx.ctx, p); err == nil {
			t.Fatalf("%s CHECK not enforced", name)
		}
	}
	// Cascade with the round.
	if _, err := fx.pool.Exec(context.Background(), `DELETE FROM cross_checks WHERE id = $1`, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.q.GetOwnerSteering(fx.ctx, plan); err == nil {
		t.Fatal("steering row outlived its round")
	}
}

// The steering claim clause: a plan child with a steering row needs cross_check_owner_steering_v1;
// a legacy plan child without a row and a code-stage child keep their existing eligibility.
func TestOwnerSteeringClaimClauseLiveDB(t *testing.T) {
	for _, withRow := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			t.Run(fmt.Sprintf("row=%v/capable=%v", withRow, capable), func(t *testing.T) {
				fx := steeringFixture(t)
				w := fx.worker("w", capOf(1), false)
				caps := claudeCheckerProtocols(true, 1)
				if capable {
					caps = append(caps, "cross_check_owner_steering_v1")
				}
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, caps)
				child, _ := seedClaudeChild(fx, fx.worker("lead-host", nil, false), 1)
				if withRow {
					mustExec(fx.ctx, t, fx.pool, `INSERT INTO cross_check_owner_steering (cross_check_id, snapshot, digest)
						SELECT id, '{}', sha256('{}'::bytea) FROM cross_checks WHERE checker_run_id=$1`, child)
				}
				want := int64(0)
				if !withRow || capable {
					want = 1
				}
				iv := leaseInterval(leaseTwoHours)
				if got := claimableCount(fx, child, iv); got != want {
					t.Fatalf("claimable=%d, want %d", got, want)
				}
				got, err := claimWithProtocolCaps(fx, w, caps)
				if want == 1 {
					if err != nil || got.ID != child {
						t.Fatalf("claim: run=%s err=%v", got.ID, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("a worker without cross_check_owner_steering_v1 claimed a steering-bearing child: %s", got.ID)
				}
			})
		}
	}
}
