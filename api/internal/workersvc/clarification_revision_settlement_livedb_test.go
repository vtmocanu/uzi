package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3985 live-DB controls for the clarification-during-human-revision settlement
// (clarification_settled): a plan cross-check run whose human revise is pending parks at
// awaiting_input on a clarification, the owner's answer is applied, and the worker's settlement
// returns the run to awaiting_approval WITHOUT passing through running. Every refusal must leave
// the run row, its input receipts and any semantic_planning row byte-identical.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

const crsQuestion = "question-1"

// crsRun is one run walked up the flow: plan A presented at gate revision 1, the human revise
// consumed and ACKed, the lead parked on crsQuestion.
type crsRun struct {
	id       uuid.UUID
	gen      int64
	revise   int64
	answer   int64
	question string
}

func (f *gateFix) crsNewGatedRun(mode string) uuid.UUID {
	f.t.Helper()
	id := f.newRun("issue")
	switch mode {
	case "gated":
		f.exec(`UPDATE runs SET plan_cross_check_gated = true WHERE id = $1`, id)
	case "required":
		// The server cleared auto_approve for a human revise; newRun already starts at false.
		f.exec(`UPDATE runs SET plan_cross_check_required = true WHERE id = $1`, id)
	case "plain":
	default:
		f.t.Fatalf("unknown mode %q", mode)
	}
	f.mustPublish(id, gateReq(i64(1), uid()), 1)
	f.liveWorker()
	return id
}

func (f *gateFix) crsRevise(id uuid.UUID, gen int64) int64 {
	f.t.Helper()
	res, err := f.submit(id, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})
	if err != nil {
		f.t.Fatalf("revise: %v", err)
	}
	f.crsAck(id, gen, res.ID)
	return res.ID
}

// crsAck replays the run's inputs and ACKs one row under claim generation gen.
func (f *gateFix) crsAck(id uuid.UUID, gen int64, row int64) {
	f.t.Helper()
	if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id, i64(gen)); err != nil {
		f.t.Fatalf("replay: %v", err)
	}
	ack, err := f.svc.AckInputs(f.ctx, f.wkr, id, gen, []int64{row})
	if err != nil || !ack.Active || len(ack.Inputs) != 1 {
		f.t.Fatalf("ack %d = %+v, err %v", row, ack, err)
	}
}

func (f *gateFix) crsPark(id uuid.UUID, gen int64, question string) {
	f.t.Helper()
	_, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "awaiting_input", OpenQuestionID: &question, ClaimGeneration: i64(gen)})
	if err != nil || !applied {
		f.t.Fatalf("park on %q = (applied %v, err %v)", question, applied, err)
	}
}

func (f *gateFix) crsAnswer(id uuid.UUID, gen int64, question string, apply bool) int64 {
	f.t.Helper()
	body, _ := json.Marshal(AnswerBody{QuestionID: question, Answers: []string{"yes"}})
	res, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "answer", string(body), nil, SubmitInputOptions{})
	if err != nil {
		f.t.Fatalf("answer: %v", err)
	}
	f.crsAck(id, gen, res.ID)
	if apply {
		applied, err := f.svc.ApplyInputs(f.ctx, f.wkr, id, gen, []int64{res.ID})
		if err != nil || !applied.Active {
			f.t.Fatalf("apply answer = %+v, err %v", applied, err)
		}
	}
	return res.ID
}

// crsParked walks a run to the point just before the settlement: revise ACKed, parked on the
// question, answer applied.
func (f *gateFix) crsParked(mode string) crsRun {
	f.t.Helper()
	id := f.crsNewGatedRun(mode)
	r := crsRun{id: id, gen: 1, question: crsQuestion}
	r.revise = f.crsRevise(id, r.gen)
	f.crsPark(id, r.gen, r.question)
	r.answer = f.crsAnswer(id, r.gen, r.question, true)
	return r
}

func (f *gateFix) crsSettle(r crsRun) (store.Run, bool, error) {
	return f.crsSettleWith(r.id, r.gen, r.question, r.revise, 1)
}

func (f *gateFix) crsSettleWith(id uuid.UUID, gen int64, question string, revise, gate int64) (store.Run, bool, error) {
	f.t.Helper()
	return f.svc.SetState(f.ctx, f.wkr, id, StateRequest{
		State:           "clarification_settled",
		ClaimGeneration: i64(gen),
		ClarificationSettlement: &ClarificationSettlement{
			QuestionID: question, RevisionInputID: revise, GateRevision: gate,
		},
	})
}

// crsSnapshot is the whole state a refusal must leave alone: the run row, every receipt row of the
// run and any semantic_planning row.
func (f *gateFix) crsSnapshot(ids ...uuid.UUID) string {
	f.t.Helper()
	out := ""
	for _, id := range ids {
		var run, inputs, semantic string
		if err := f.pool.QueryRow(f.ctx, `SELECT
		    (SELECT to_jsonb(r)::text FROM runs r WHERE r.id = $1),
		    COALESCE((SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id)::text FROM run_user_inputs i WHERE i.run_id = $1), '[]'),
		    COALESCE((SELECT to_jsonb(s)::text FROM semantic_planning s WHERE s.run_id = $1), 'none')`, id).Scan(&run, &inputs, &semantic); err != nil {
			f.t.Fatalf("snapshot: %v", err)
		}
		out += run + "|" + inputs + "|" + semantic + "\n"
	}
	return out
}

// crsRefused runs do and requires it to be refused (not applied, wantErr when set) with every
// listed run left byte-identical.
func (f *gateFix) crsRefused(t *testing.T, ids []uuid.UUID, wantErr error, do func() (store.Run, bool, error)) {
	t.Helper()
	before := f.crsSnapshot(ids...)
	_, applied, err := do()
	if applied {
		t.Fatalf("a refused settlement applied")
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if wantErr == nil && err != nil {
		t.Fatalf("a refusal must be an ordinary not-applied result, got err %v", err)
	}
	if after := f.crsSnapshot(ids...); after != before {
		t.Fatalf("a refusal changed state:\nbefore %s\nafter  %s", before, after)
	}
}

type crsSix struct {
	status      string
	question    *string
	completion  *string
	paused      int32
	statusSince string
}

func (f *gateFix) crsSix(id uuid.UUID) crsSix {
	f.t.Helper()
	var s crsSix
	if err := f.pool.QueryRow(f.ctx, `SELECT status, open_question_id, completion_question_at::text, budget_paused_seconds, status_since::text FROM runs WHERE id = $1`, id).
		Scan(&s.status, &s.question, &s.completion, &s.paused, &s.statusSince); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// crsRestSnapshot is every column the settlement must not write.
func (f *gateFix) crsRestSnapshot(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT (to_jsonb(runs) - ARRAY['status','status_since','updated_at','open_question_id','completion_question_at','budget_paused_seconds'])::text FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *gateFix) crsBackdate(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE runs SET status_since = now() - interval '5 seconds' WHERE id = $1`, id)
}

func (f *gateFix) crsReplay(id uuid.UUID, gen int64, question string, revise, gate int64) bool {
	f.t.Helper()
	got, err := f.q.GetClarificationGateSettlementReplay(f.ctx, store.GetClarificationGateSettlementReplayParams{
		ID: id, WorkerID: pgconv.UUID(f.wkr.ID), ClaimGeneration: gen, GateRevision: gate, QuestionID: question, RevisionInputID: revise,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestClarificationRevisionSettlementControlsLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	requireSettled := func(t *testing.T, r crsRun, paused0 int32) {
		t.Helper()
		got, applied, err := f.crsSettle(r)
		if err != nil || !applied {
			t.Fatalf("settle = (applied %v, err %v), want applied", applied, err)
		}
		if got.Status != "awaiting_approval" {
			t.Fatalf("returned status %q, want awaiting_approval", got.Status)
		}
		six := f.crsSix(r.id)
		if six.status != "awaiting_approval" || six.question != nil || six.completion != nil {
			t.Fatalf("after settle: %+v, want awaiting_approval with open_question_id and completion_question_at NULL", six)
		}
		if six.paused < paused0+5 {
			t.Fatalf("budget_paused_seconds %d -> %d, want the 5s awaiting_input gap banked", paused0, six.paused)
		}
		var applyAt, disposition *string
		if err := f.pool.QueryRow(f.ctx, `SELECT applied_at::text, disposition FROM run_user_inputs WHERE id = $1`, r.revise).Scan(&applyAt, &disposition); err != nil {
			t.Fatal(err)
		}
		if applyAt != nil || disposition != nil {
			t.Fatalf("the revise must stay pending after the settlement, got applied_at %v disposition %v", applyAt, disposition)
		}
	}

	for _, mode := range []string{"gated", "required"} {
		t.Run(mode+" mode settles to the gate", func(t *testing.T) {
			r := f.crsParked(mode)
			f.crsBackdate(r.id)
			rest := f.crsRestSnapshot(r.id)
			paused0 := f.crsSix(r.id).paused
			requireSettled(t, r, paused0)
			if got := f.crsRestSnapshot(r.id); got != rest {
				t.Fatalf("columns outside the settlement set changed:\nbefore %s\nafter  %s", rest, got)
			}
		})
	}

	t.Run("required mode reclaimed with auto_approve still set is refused until it is cleared, and the revise is re-ACKed under the new claim", func(t *testing.T) {
		id := f.crsNewGatedRun("required")
		revise := f.crsRevise(id, 1)
		gen := f.reclaim(id)
		// The claim-1 receipt does not authorize the claim-2 flight until it re-ACKs the replay.
		stale := crsRun{id: id, gen: gen, revise: revise, question: crsQuestion}
		f.crsPark(id, gen, stale.question)
		stale.answer = f.crsAnswer(id, gen, stale.question, true)
		// crsAnswer re-ACKed only the answer; the revise still carries generation 1.
		f.crsRefused(t, []uuid.UUID{id}, nil, func() (store.Run, bool, error) { return f.crsSettle(stale) })
		f.crsAck(id, gen, revise)
		// Everything else is now valid; only auto_approve stands in the way. The published gate's
		// SetRunAwaitingApproval already cleared it, so a direct UPDATE re-sets it to model a run
		// that reaches the settlement with it still true.
		f.exec(`UPDATE runs SET auto_approve = true WHERE id = $1`, id)
		f.crsRefused(t, []uuid.UUID{id}, nil, func() (store.Run, bool, error) { return f.crsSettle(stale) })
		// Clearing it is a direct UPDATE standing in for SetRunAwaitingApproval's auto_approve = false.
		f.exec(`UPDATE runs SET auto_approve = false WHERE id = $1`, id)
		f.crsBackdate(id)
		requireSettled(t, stale, f.crsSix(id).paused)
	})

	t.Run("run-level guards each refuse an otherwise valid settlement", func(t *testing.T) {
		cases := []struct {
			name, mode, sql string
		}{
			{"auto_approve set", "required", `UPDATE runs SET auto_approve = true WHERE id = $1`},
			{"completion question pending", "gated", `UPDATE runs SET completion_question_at = now() WHERE id = $1`},
			{"kind outside issue, prompt and ci_fix", "required", `UPDATE runs SET kind = 'self_improve' WHERE id = $1`},
			{"unresolved policy outcome", "gated", `UPDATE runs SET policy_outcome_id = gen_random_uuid(), policy_continuation_mode = 'same_conversation',
			    policy_applying_worker_id = worker_id, policy_applying_generation = 1, policy_source_generation = 1,
			    policy_preserve_required = true WHERE id = $1`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := f.crsParked(c.mode)
				f.exec(c.sql, r.id)
				f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) { return f.crsSettle(r) })
			})
		}
		t.Run("a resolved policy outcome does not refuse", func(t *testing.T) {
			r := f.crsParked("gated")
			f.exec(`UPDATE runs SET policy_outcome_id = gen_random_uuid(), policy_continuation_mode = 'same_conversation',
			    policy_applying_worker_id = worker_id, policy_applying_generation = 1, policy_source_generation = 1,
			    policy_resolved_at = now() WHERE id = $1`, r.id)
			f.crsBackdate(r.id)
			requireSettled(t, r, f.crsSix(r.id).paused)
		})
		t.Run("codex_policy_hold cannot coexist with awaiting_input", func(t *testing.T) {
			// runs_policy_hold_shape confines hold_reason = 'codex_policy_hold' to terminal and
			// paused statuses, so the settlement's explicit hold_reason clause is a belt over a CHECK:
			// the database itself rejects the state a test would need to reach it.
			r := f.crsParked("gated")
			if _, err := f.pool.Exec(f.ctx, `UPDATE runs SET hold_reason = 'codex_policy_hold' WHERE id = $1`, r.id); err == nil {
				t.Fatalf("hold_reason codex_policy_hold was accepted on an awaiting_input run; add a refusal control for it")
			}
		})
	})

	t.Run("semantic-enrolled run settles with its semantic_planning row unchanged", func(t *testing.T) {
		r := f.crsParked("gated")
		f.exec(`INSERT INTO semantic_planning (run_id, enrollment) VALUES ($1, '{"v":1}'::jsonb)`, r.id)
		var before string
		if err := f.pool.QueryRow(f.ctx, `SELECT to_jsonb(s)::text FROM semantic_planning s WHERE run_id = $1`, r.id).Scan(&before); err != nil {
			t.Fatal(err)
		}
		f.crsBackdate(r.id)
		requireSettled(t, r, f.crsSix(r.id).paused)
		var after string
		if err := f.pool.QueryRow(f.ctx, `SELECT to_jsonb(s)::text FROM semantic_planning s WHERE run_id = $1`, r.id).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("semantic_planning changed:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("answer must exist, be applied, be live and name the open question", func(t *testing.T) {
		// Missing: parked, never answered.
		id := f.crsNewGatedRun("gated")
		missing := crsRun{id: id, gen: 1, question: crsQuestion}
		missing.revise = f.crsRevise(id, 1)
		f.crsPark(id, 1, crsQuestion)
		f.crsRefused(t, []uuid.UUID{id}, nil, func() (store.Run, bool, error) { return f.crsSettle(missing) })

		// Unapplied: answered and ACKed but not applied.
		id = f.crsNewGatedRun("gated")
		unapplied := crsRun{id: id, gen: 1, question: crsQuestion}
		unapplied.revise = f.crsRevise(id, 1)
		f.crsPark(id, 1, crsQuestion)
		unapplied.answer = f.crsAnswer(id, 1, crsQuestion, false)
		f.crsRefused(t, []uuid.UUID{id}, nil, func() (store.Run, bool, error) { return f.crsSettle(unapplied) })

		// Wrong question: the report names a question the run is not parked on.
		r := f.crsParked("gated")
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) {
			return f.crsSettleWith(r.id, r.gen, "other-question", r.revise, 1)
		})

		// Superseded answer.
		f.exec(`UPDATE run_user_inputs SET disposition = 'superseded' WHERE id = $1`, r.answer)
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) { return f.crsSettle(r) })

		// An applied answer to a DIFFERENT question does not satisfy the open one.
		r2 := f.crsNewGatedRun("gated")
		other := crsRun{id: r2, gen: 1, question: crsQuestion}
		other.revise = f.crsRevise(r2, 1)
		f.crsPark(r2, 1, crsQuestion)
		f.crsAnswer(r2, 1, crsQuestion, true)
		f.exec(`UPDATE run_user_inputs SET question_id = 'different' WHERE run_id = $1 AND kind = 'answer'`, r2)
		f.crsRefused(t, []uuid.UUID{r2}, nil, func() (store.Run, bool, error) { return f.crsSettle(other) })
	})

	t.Run("the revise must be the pending, bound, non-empty one consumed by this worker and generation", func(t *testing.T) {
		cases := []struct {
			name, sql string
		}{
			{"superseded", `UPDATE run_user_inputs SET disposition = 'superseded' WHERE id = $1`},
			{"already applied", `UPDATE run_user_inputs SET applied_at = now() WHERE id = $1`},
			{"empty body", `UPDATE run_user_inputs SET body = E' \t\n ' WHERE id = $1`},
			{"consumed by another generation", `UPDATE run_user_inputs SET consumed_claim_generation = 7 WHERE id = $1`},
			{"bound to another gate revision", `UPDATE run_user_inputs SET gate_revision = 2 WHERE id = $1`},
			{"unconsumed", `UPDATE run_user_inputs SET consumed_at = NULL WHERE id = $1`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := f.crsParked("gated")
				f.exec(c.sql, r.revise)
				f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) { return f.crsSettle(r) })
			})
		}
		t.Run("consumed by another worker", func(t *testing.T) {
			r := f.crsParked("gated")
			other := uuid.New()
			f.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-other-3985', $3, 'offline')`, other, f.userID, other[:])
			f.exec(`UPDATE run_user_inputs SET consumed_worker_id = $2 WHERE id = $1`, r.revise, other)
			f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) { return f.crsSettle(r) })
		})
		t.Run("the report names another gate revision", func(t *testing.T) {
			r := f.crsParked("gated")
			f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) {
				return f.crsSettleWith(r.id, r.gen, r.question, r.revise, 2)
			})
		})
		t.Run("no pending revise at all", func(t *testing.T) {
			// Parked and answered, but no revise_plan row exists for the run at all.
			id := f.crsNewGatedRun("gated")
			f.crsPark(id, 1, crsQuestion)
			f.crsAnswer(id, 1, crsQuestion, true)
			var revises int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'revise_plan'`, id).Scan(&revises); err != nil || revises != 0 {
				t.Fatalf("revise rows = %d, err %v, want none", revises, err)
			}
			f.crsRefused(t, []uuid.UUID{id}, nil, func() (store.Run, bool, error) {
				return f.crsSettleWith(id, 1, crsQuestion, 999999999, 1)
			})
		})
	})

	t.Run("cross-run receipts never satisfy another run", func(t *testing.T) {
		// Runs A and B: same worker, equal claim generation, equal gate revision, both parked.
		a := f.crsNewGatedRun("gated")
		b := f.crsNewGatedRun("gated")
		ra := crsRun{id: a, gen: 1, question: crsQuestion}
		rb := crsRun{id: b, gen: 1, question: crsQuestion}
		ra.revise = f.crsRevise(a, 1)
		rb.revise = f.crsRevise(b, 1)
		f.crsPark(a, 1, crsQuestion)
		f.crsPark(b, 1, crsQuestion)
		both := []uuid.UUID{a, b}

		// (a) the applied answer exists only on B and carries A's question id.
		rb.answer = f.crsAnswer(b, 1, crsQuestion, true)
		f.crsRefused(t, both, nil, func() (store.Run, bool, error) { return f.crsSettle(ra) })
		if f.crsReplay(a, 1, crsQuestion, ra.revise, 1) {
			t.Fatalf("the replay read of A was satisfied by B's applied answer")
		}

		// (b) A's settlement names B's pending revise.
		ra.answer = f.crsAnswer(a, 1, crsQuestion, true)
		f.crsRefused(t, both, nil, func() (store.Run, bool, error) {
			return f.crsSettleWith(a, 1, crsQuestion, rb.revise, 1)
		})

		// (c) after A settles legitimately, B's report naming A's ids is refused too.
		f.crsBackdate(a)
		if _, applied, err := f.crsSettle(ra); err != nil || !applied {
			t.Fatalf("A's legitimate settlement = (applied %v, err %v)", applied, err)
		}
		f.crsRefused(t, both, nil, func() (store.Run, bool, error) {
			return f.crsSettleWith(b, 1, crsQuestion, ra.revise, 1)
		})
		if f.crsReplay(b, 1, crsQuestion, ra.revise, 1) {
			t.Fatalf("the replay read of B accepted A's revise")
		}
		// Once B settles legitimately, a replay naming A's revise is still refused.
		f.crsBackdate(b)
		if _, applied, err := f.crsSettle(rb); err != nil || !applied {
			t.Fatalf("B's legitimate settlement = (applied %v, err %v)", applied, err)
		}
		f.crsRefused(t, both, nil, func() (store.Run, bool, error) {
			return f.crsSettleWith(b, 1, crsQuestion, ra.revise, 1)
		})
		if f.crsReplay(b, 1, crsQuestion, ra.revise, 1) {
			t.Fatalf("the settled B's replay read accepted A's revise")
		}
	})

	t.Run("a stale or released claim is ErrStaleClaim", func(t *testing.T) {
		r := f.crsParked("gated")
		f.crsRefused(t, []uuid.UUID{r.id}, ErrStaleClaim, func() (store.Run, bool, error) {
			return f.crsSettleWith(r.id, r.gen+1, r.question, r.revise, 1)
		})
		f.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, r.id)
		f.crsRefused(t, []uuid.UUID{r.id}, ErrStaleClaim, func() (store.Run, bool, error) { return f.crsSettle(r) })
	})

	t.Run("a lost reply replays idempotently and banks the gap once", func(t *testing.T) {
		r := f.crsParked("gated")
		f.crsBackdate(r.id)
		paused0 := f.crsSix(r.id).paused
		requireSettled(t, r, paused0)
		after := f.crsSnapshot(r.id)
		got, applied, err := f.crsSettle(r)
		if err != nil || !applied || got.Status != "awaiting_approval" {
			t.Fatalf("replay = (status %q, applied %v, err %v), want applied idempotently", got.Status, applied, err)
		}
		if again := f.crsSnapshot(r.id); again != after {
			t.Fatalf("the replay wrote:\nafter first %s\nafter replay %s", after, again)
		}
		// A replay naming another revise is not the same settlement.
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) {
			return f.crsSettleWith(r.id, r.gen, r.question, r.revise+1000000, 1)
		})
	})

	t.Run("a publication-resume-enrolled run settles without the fence exception", func(t *testing.T) {
		r := f.crsParked("gated")
		f.svc.p.PublicationResume = true
		f.wkr.ProtocolCapabilities = append(append([]string{}, f.wkr.ProtocolCapabilities...), capability.PublicationResumeV1)
		f.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.wkr.ID, f.wkr.ProtocolCapabilities)
		f.exec(`UPDATE runs SET branch = 'agent/issue-3985', started_at = now() WHERE id = $1`, r.id)
		intent, err := json.Marshal(map[string]any{
			"run_id": r.id, "repo_id": f.repoID, "branch": "agent/issue-3985", "claim_generation": r.gen,
			"original_head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "floor": map[string]any{"kind": "absent"}, "candidate": nil,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Enrollment is only admitted on a running run: lift the park for the enrollment call and
		// restore it (the park columns are untouched) so the settlement sees the original state.
		f.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, r.id)
		if _, err := f.svc.RegisterPublicationIntent(f.ctx, f.wkr, r.id, PublicationIntentRequest{ClaimGeneration: r.gen, Intent: intent}); err != nil {
			t.Fatalf("enroll: %v", err)
		}
		f.exec(`UPDATE runs SET status = 'awaiting_input' WHERE id = $1`, r.id)
		var enrolled int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_publication_resume WHERE run_id = $1 AND state = 'active'`, r.id).Scan(&enrolled); err != nil || enrolled != 1 {
			t.Fatalf("active enrollments = %d, err %v, want 1", enrolled, err)
		}
		f.crsBackdate(r.id)
		requireSettled(t, r, f.crsSix(r.id).paused)
	})

	t.Run("a credential switch stamped after the answer applied still settles, then releases from the gate", func(t *testing.T) {
		r := f.crsParked("gated")
		f.exec(`UPDATE runs SET credential_switch_requested_at = now(), credential_switch_generation = $2 WHERE id = $1`, r.id, r.gen)
		f.crsBackdate(r.id)
		requireSettled(t, r, f.crsSix(r.id).paused)
		_, applied, err := f.svc.SetState(f.ctx, f.wkr, r.id, StateRequest{State: "credential_switch", ClaimGeneration: i64(r.gen)})
		if err != nil || !applied {
			t.Fatalf("release = (applied %v, err %v), want applied", applied, err)
		}
		if st := f.crsSix(r.id).status; st != "queued" {
			t.Fatalf("status after the release = %q, want queued", st)
		}
	})

	t.Run("the generic running report stays refused while the answer is applied", func(t *testing.T) {
		r := f.crsParked("gated")
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) {
			return f.svc.SetState(f.ctx, f.wkr, r.id, StateRequest{State: "running", ClaimGeneration: i64(r.gen)})
		})
		if st := f.crsSix(r.id).status; st != "awaiting_input" {
			t.Fatalf("status = %q, want awaiting_input", st)
		}
	})

	t.Run("an ordinary non-cross-check run is refused by the new writer", func(t *testing.T) {
		r := f.crsParked("plain")
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) { return f.crsSettle(r) })
	})

	t.Run("initial planning with no stored plan is refused", func(t *testing.T) {
		id := f.newRun("issue")
		f.exec(`UPDATE runs SET plan_cross_check_gated = true WHERE id = $1`, id)
		f.crsPark(id, 1, crsQuestion)
		f.crsAnswer(id, 1, crsQuestion, true)
		before := f.crsSnapshot(id)
		for _, rev := range []int64{0, 1} {
			_, applied, err := f.crsSettleWith(id, 1, crsQuestion, 1, rev)
			if rev == 0 {
				if !errors.Is(err, ErrInvalidState) || applied {
					t.Fatalf("gate_revision 0 = (applied %v, err %v), want ErrInvalidState", applied, err)
				}
				continue
			}
			if err != nil || applied {
				t.Fatalf("no stored plan = (applied %v, err %v), want a not-applied refusal", applied, err)
			}
		}
		if f.crsSnapshot(id) != before {
			t.Fatalf("a refusal changed the initial-planning run")
		}
	})

	t.Run("an already-applied bound approval refuses the settlement", func(t *testing.T) {
		r := f.crsParked("gated")
		f.exec(`INSERT INTO run_user_inputs (run_id, kind, body, gate_binding, gate_revision, consumed_at, consumed_worker_id, consumed_claim_generation, applied_at)
		        VALUES ($1, 'approve_plan', '', 'bound', 1, now(), $2, $3, now())`, r.id, f.wkr.ID, r.gen)
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) { return f.crsSettle(r) })
	})

	t.Run("a settled run is not settled again by a report naming another question", func(t *testing.T) {
		r := f.crsParked("gated")
		f.crsBackdate(r.id)
		requireSettled(t, r, f.crsSix(r.id).paused)
		// The run is at awaiting_approval now; a second, different report finds nothing to settle.
		f.crsRefused(t, []uuid.UUID{r.id}, nil, func() (store.Run, bool, error) {
			return f.crsSettleWith(r.id, r.gen, "another-question", r.revise, 1)
		})
	})

	t.Run("malformed reports are invalid before any lock", func(t *testing.T) {
		r := f.crsParked("gated")
		before := f.crsSnapshot(r.id)
		bad := []StateRequest{
			{State: "clarification_settled", ClaimGeneration: i64(r.gen)},
			{State: "clarification_settled", ClarificationSettlement: &ClarificationSettlement{QuestionID: "q", RevisionInputID: 1, GateRevision: 1}},
			{State: "clarification_settled", ClaimGeneration: i64(0), ClarificationSettlement: &ClarificationSettlement{QuestionID: "q", RevisionInputID: 1, GateRevision: 1}},
			{State: "clarification_settled", ClaimGeneration: i64(r.gen), ClarificationSettlement: &ClarificationSettlement{QuestionID: "  ", RevisionInputID: 1, GateRevision: 1}},
			{State: "clarification_settled", ClaimGeneration: i64(r.gen), ClarificationSettlement: &ClarificationSettlement{QuestionID: "q", RevisionInputID: 0, GateRevision: 1}},
			{State: "clarification_settled", ClaimGeneration: i64(r.gen), ClarificationSettlement: &ClarificationSettlement{QuestionID: "q", RevisionInputID: 1, GateRevision: 0}},
			{State: "clarification_settled", ClaimGeneration: i64(r.gen), ClarificationSettlement: &ClarificationSettlement{QuestionID: fmt.Sprintf("%0200d", 1), RevisionInputID: 1, GateRevision: 1}},
			{State: "running", ClaimGeneration: i64(r.gen), ClarificationSettlement: &ClarificationSettlement{QuestionID: "q", RevisionInputID: 1, GateRevision: 1}},
			{State: "awaiting_approval", ClaimGeneration: i64(r.gen), ClarificationSettlement: &ClarificationSettlement{QuestionID: "q", RevisionInputID: 1, GateRevision: 1}},
		}
		for i, req := range bad {
			if _, applied, err := f.svc.SetState(f.ctx, f.wkr, r.id, req); !errors.Is(err, ErrInvalidState) || applied {
				t.Fatalf("report %d = (applied %v, err %v), want ErrInvalidState", i, applied, err)
			}
		}
		if f.crsSnapshot(r.id) != before {
			t.Fatalf("a malformed report changed the run")
		}
	})

	// Observation only (the lead asked to be told, not to fix): on an ORDINARY human-gated run with a
	// stored candidate, does a clarification during a revise followed by the generic running report
	// freeze milestones from the candidate?
	t.Run("observation: ordinary run, clarification during revise, then generic running", func(t *testing.T) {
		r := f.crsParked("plain")
		var frozenBefore, candidate *string
		if err := f.pool.QueryRow(f.ctx, `SELECT milestones_frozen::text, milestones_candidate::text FROM runs WHERE id = $1`, r.id).Scan(&frozenBefore, &candidate); err != nil {
			t.Fatal(err)
		}
		_, applied, err := f.svc.SetState(f.ctx, f.wkr, r.id, StateRequest{State: "running", ClaimGeneration: i64(r.gen)})
		var frozenAfter *string
		var status string
		if qerr := f.pool.QueryRow(f.ctx, `SELECT status, milestones_frozen::text FROM runs WHERE id = $1`, r.id).Scan(&status, &frozenAfter); qerr != nil {
			t.Fatal(qerr)
		}
		str := func(p *string) string {
			if p == nil {
				return "NULL"
			}
			return *p
		}
		t.Logf("OBSERVED premature-freeze control: running applied=%v err=%v status=%s milestones_candidate=%s milestones_frozen before=%s after=%s",
			applied, err, status, str(candidate), str(frozenBefore), str(frozenAfter))
	})
}
