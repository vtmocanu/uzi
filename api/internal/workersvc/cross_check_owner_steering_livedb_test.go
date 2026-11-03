package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3841: the owner-steering snapshot is frozen with the round, byte-identical on every
// read and claim, and gates the claim on cross_check_owner_steering_v1.

const steeringLeadQuestion = `{"question_id":%q,"questions":[{"header":"DB","question":"Which database?","options":[{"label":"pg"},{"label":"sqlite"}]}]}`

// seedSteeringAnswer inserts a lead question at an explicit run_messages seq and the owner's answer
// to it through exec. The seq is explicit because two inserts that each pick max(seq)+1 (one in an
// uncommitted transaction) would collide on UNIQUE(run_id, seq) and block.
func seedSteeringAnswer(t *testing.T, exec func(string, ...any), lead uuid.UUID, seq int, qid, answer string) {
	t.Helper()
	exec(`INSERT INTO run_messages (run_id, seq, kind, agent, payload, created_at)
		VALUES ($1, $3, 'question', 'lead', $2::jsonb, now() - interval '5 seconds')`,
		lead, fmt.Sprintf(steeringLeadQuestion, qid), seq)
	exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id) VALUES ($1, 'answer', $2, $3)`,
		lead, fmt.Sprintf(`{"question_id":%q,"answers":[%q]}`, qid, answer), qid)
}

func storedSteering(t *testing.T, env codexTestEnv, cc store.CrossCheck) string {
	t.Helper()
	row, err := env.q.GetOwnerSteering(env.ctx, cc.ID)
	if err != nil {
		t.Fatalf("round %d has no frozen steering: %v", cc.Round, err)
	}
	sum := sha256.Sum256([]byte(row.Snapshot))
	if !bytes.Equal(sum[:], row.Digest) {
		t.Fatal("stored digest is not the sha256 of the stored snapshot")
	}
	return row.Snapshot
}

func decideRevise(t *testing.T, env codexTestEnv, svc *Service, w store.Worker, cc store.CrossCheck) {
	t.Helper()
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
	if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerSteeringFreezeAcrossRoundsLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsHarnessFixture(t, 2, "codex")
	q1, q2 := "q1-"+uuid.NewString(), "q2-"+uuid.NewString()
	seedSteeringAnswer(t, env.exec, lead, 9001, q1, "use postgres")
	// A real pre-gate revise: SubmitInput permits it while the lead runs and labels it unbound.
	if _, err := svc.SubmitInput(env.ctx, w.UserID, lead, "revise_plan", "tighten the scope", nil); err != nil {
		t.Fatalf("pre-gate revise: %v", err)
	}
	// Non-steering inputs never appear.
	env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'please also fix docs')`, lead)

	cc1, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := storedSteering(t, env, cc1)
	var snap struct {
		Entries []struct {
			Kind        string   `json:"kind"`
			QuestionID  string   `json:"question_id"`
			GateBinding string   `json:"gate_binding"`
			Feedback    string   `json:"feedback"`
			Answers     []string `json:"answers"`
		} `json:"owner_entries"`
		Context []struct {
			Status string `json:"status"`
			Author string `json:"author"`
		} `json:"question_context"`
		Cutoff int64 `json:"cutoff_input_id"`
	}
	if err := json.Unmarshal([]byte(first), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 2 || snap.Entries[0].Kind != "answer" || snap.Entries[0].QuestionID != q1 || snap.Entries[0].Answers[0] != "use postgres" ||
		snap.Entries[1].Kind != "revise" || snap.Entries[1].GateBinding != "unbound" || snap.Entries[1].Feedback != "tighten the scope" {
		t.Fatalf("round 1 snapshot wrong: %s", first)
	}
	if len(snap.Context) != 1 || snap.Context[0].Status != "bound" || snap.Context[0].Author != "lead" || strings.Contains(first, "fix docs") {
		t.Fatalf("round 1 context wrong: %s", first)
	}

	// A new answer after the freeze: the same-round retry returns the same round and bytes.
	seedSteeringAnswer(t, env.exec, lead, 9002, q2, "yes, add an index")
	retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
	if err != nil || retry.ID != cc1.ID {
		t.Fatalf("retry: %v", err)
	}
	if got := storedSteering(t, env, retry); got != first || strings.Contains(got, q2) {
		t.Fatalf("retry changed or extended the frozen round: %s", got)
	}

	decideRevise(t, env, svc, w, cc1)
	cc2, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	second := storedSteering(t, env, cc2)
	if !strings.Contains(second, q1) || !strings.Contains(second, q2) || !strings.Contains(second, "yes, add an index") {
		t.Fatalf("round 2 lacks the new answer: %s", second)
	}
	if got := storedSteering(t, env, cc1); got != first {
		t.Fatal("round 1 bytes changed after round 2 froze")
	}
}

// An answer insert whose transaction is still open holds a key-share lock on the lead row (the
// foreign key), so the freeze, which locks the lead FOR UPDATE, waits for it and then sees it.
func TestOwnerSteeringWaitsForInFlightAnswerLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsHarnessFixture(t, 2, "codex")
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	q := "q-" + uuid.NewString()
	seedSteeringAnswer(t, func(sql string, args ...any) {
		if _, err := tx.Exec(env.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}, lead, 9001, q, "late but committed")
	done := make(chan store.CrossCheck, 1)
	go func() {
		cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
		if err != nil {
			t.Error(err)
		}
		done <- cc
	}()
	select {
	case <-done:
		t.Fatal("the freeze did not wait for the in-flight answer")
	case <-time.After(750 * time.Millisecond):
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Fatal(err)
	}
	cc := <-done
	if got := storedSteering(t, env, cc); !strings.Contains(got, "late but committed") {
		t.Fatalf("committed answer missing from the snapshot: %s", got)
	}
}

// With the foreign-key lock out of the picture (replica role skips the FK triggers in the
// in-flight transaction), an uncommitted answer is invisible to the freeze. The frozen round never
// changes when it commits later, and the next round contains both answers.
func TestOwnerSteeringConcurrentInsertFrozenLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsHarnessFixture(t, 2, "codex")
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if _, err := tx.Exec(env.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Skipf("cannot skip FK triggers on this role: %v", err)
	}
	inflight, committed := "q-inflight-"+uuid.NewString(), "q-committed-"+uuid.NewString()
	seedSteeringAnswer(t, func(sql string, args ...any) {
		if _, err := tx.Exec(env.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}, lead, 9001, inflight, "uncommitted answer")
	seedSteeringAnswer(t, env.exec, lead, 9002, committed, "committed answer")

	cc1, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := storedSteering(t, env, cc1)
	if !strings.Contains(first, committed) || strings.Contains(first, inflight) {
		t.Fatalf("round 1 must hold only the committed answer: %s", first)
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedSteering(t, env, cc1); got != first {
		t.Fatal("stored round 1 bytes changed when the in-flight answer committed")
	}
	decideRevise(t, env, svc, w, cc1)
	cc2, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := storedSteering(t, env, cc2); !strings.Contains(got, committed) || !strings.Contains(got, inflight) {
		t.Fatalf("round 2 must contain both answers: %s", got)
	}
}

// steeringFailTx fails the steering insert and passes every other statement through.
type steeringFailTx struct{ pgx.Tx }

func (tx steeringFailTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO cross_check_owner_steering") {
		return pgconn.CommandTag{}, errors.New("injected steering insert failure")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

type steeringFailBeginner struct{ pool *pgxpool.Pool }

func (b steeringFailBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	return steeringFailTx{tx}, err
}

// A failed steering write aborts the round: no cross_checks row and no child run survive.
func TestOwnerSteeringFailureAbortsRoundLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsHarnessFixture(t, 2, "codex")
	svc.SetTxBeginner(steeringFailBeginner{env.pool})
	if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1); err == nil {
		t.Fatal("round created without its steering row")
	}
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM cross_checks WHERE lead_run_id=$1`, lead).Scan(&n); err != nil || n != 0 {
		t.Fatalf("aborted round left %d cross_checks rows (err=%v)", n, err)
	}
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE target_run_id=$1`, lead).Scan(&n); err != nil || n != 0 {
		t.Fatalf("aborted round left %d child runs (err=%v)", n, err)
	}
}

func steeringClaimRow(fx *claudeCheckerFix) (string, []byte) {
	snapshot := `{"version":1,"cutoff_input_id":0,"owner_entries":[],"question_context":[],"omitted_entries":0,"overflow":false,"overflow_reason":""}`
	sum := sha256.Sum256([]byte(snapshot))
	return snapshot, sum[:]
}

func (fx *claudeCheckerFix) addSteeringRow(snapshot string, digest []byte) {
	fx.env.exec(`INSERT INTO cross_check_owner_steering (cross_check_id, snapshot, digest)
		SELECT id, $2, $3 FROM cross_checks WHERE checker_run_id=$1`, fx.kid, snapshot, digest)
}

func (fx *claudeCheckerFix) setCaps(caps ...string) {
	fx.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, fx.workerID, caps)
}

func TestOwnerSteeringClaimGateAndDeliveryLiveDB(t *testing.T) {
	base := []string{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1}
	t.Run("legacy child without a row claims without the capability", func(t *testing.T) {
		fx := newClaudeCheckerFix(t, BindModePinned)
		p := fx.claim(t)
		if p == nil || p.CrossCheck == nil || p.CrossCheckOwnerSteering != nil {
			t.Fatalf("legacy child: payload=%v", p)
		}
		raw, err := MarshalCrossCheckClaim(p)
		if err != nil || strings.Contains(string(raw), "cross_check_owner_steering") {
			t.Fatalf("legacy claim carries steering: %v", err)
		}
	})
	t.Run("steering child is invisible to an incapable worker and delivered to a capable one", func(t *testing.T) {
		fx := newClaudeCheckerFix(t, BindModePinned)
		snapshot, digest := steeringClaimRow(fx)
		fx.addSteeringRow(snapshot, digest)
		if p := fx.claim(t); p != nil {
			t.Fatal("a worker without cross_check_owner_steering_v1 claimed a steering-bearing child")
		}
		if st := mustRun(t, fx.env, fx.kid).Status; st != "queued" {
			t.Fatalf("child left queued state: %s", st)
		}
		fx.setCaps(append(base, capability.CrossCheckOwnerSteeringV1)...)
		p := fx.claim(t)
		if p == nil || p.CrossCheckOwnerSteering == nil || p.CrossCheckOwnerSteering.Snapshot != snapshot ||
			p.CrossCheckOwnerSteering.Digest != fmt.Sprintf("%x", digest) {
			t.Fatalf("steering not delivered unchanged: %+v", p)
		}
		first, err := MarshalCrossCheckClaim(p)
		if err != nil {
			t.Fatal(err)
		}
		// Resume/re-claim: assemble the claim again for the same child; identical steering bytes.
		fx.env.exec(`UPDATE runs SET status='claimed' WHERE id=$1`, fx.kid)
		worker := wkrRow(t, fx.env, fx.workerID)
		again, err := fx.svc.assembleClaim(fx.env.ctx, worker, mustRun(t, fx.env, fx.kid))
		if err != nil || again == nil || again.CrossCheckOwnerSteering == nil {
			t.Fatalf("re-claim lost steering: %v", err)
		}
		if *again.CrossCheckOwnerSteering != *p.CrossCheckOwnerSteering {
			t.Fatal("re-claim delivered different steering bytes")
		}
		second, err := MarshalCrossCheckClaim(again)
		if err != nil || !bytes.Contains(second, []byte(`"cross_check_owner_steering"`)) || !bytes.Contains(first, []byte(`"cross_check_owner_steering"`)) {
			t.Fatalf("wire lost the field: %v", err)
		}
	})
	t.Run("tampered digest refuses the claim", func(t *testing.T) {
		fx := newClaudeCheckerFix(t, BindModePinned)
		snapshot, _ := steeringClaimRow(fx)
		fx.addSteeringRow(snapshot, bytes.Repeat([]byte{7}, 32))
		fx.setCaps(append(base, capability.CrossCheckOwnerSteeringV1)...)
		if p := fx.claim(t); p != nil {
			t.Fatal("a steering row with a bad digest was delivered")
		}
		if st := mustRun(t, fx.env, fx.kid).Status; st != "failed" {
			t.Fatalf("tampered child status %s, want failed", st)
		}
	})
}

func TestOwnerSteeringHealthCountsLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	snapshot, digest := steeringClaimRow(fx)
	read := func() store.GetRunSemanticHealthRow {
		row, err := fx.env.q.GetRunSemanticHealth(fx.env.ctx, store.GetRunSemanticHealthParams{RunID: fx.kid, UserID: fx.userID})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	if row := read(); row.OwnerSteeringEnrolled || row.OwnerSteeringCapable != 0 {
		t.Fatalf("legacy child enrolled: %+v", row)
	}
	fx.addSteeringRow(snapshot, digest)
	if row := read(); !row.OwnerSteeringEnrolled || row.OwnerSteeringCapable != 0 {
		t.Fatalf("steering child with no capable worker: %+v", row)
	}
	fx.setCaps(capability.CrossCheckV1, capability.CrossCheckOwnerSteeringV1)
	if row := read(); !row.OwnerSteeringEnrolled || row.OwnerSteeringCapable != 1 {
		t.Fatalf("steering child with a capable worker: %+v", row)
	}
}
