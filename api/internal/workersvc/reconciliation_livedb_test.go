package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type reconciliationFixture struct {
	codexTestEnv
	svc         *Service
	worker      store.Worker
	lead, child uuid.UUID
	candidate   PlanCrossCheckCandidate
	req         StateRequest
}

func newReconciliationFixture(t *testing.T, harness, verdict string) reconciliationFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, worker, repo := env.seedCodexInfra(t)
	lead := env.seedCodexRun(t, user, worker, repo)
	env.exec(`UPDATE runs SET status='running', harness=$2, auto_approve=true,
 plan_source='agent', plan_cross_check_required=true, claim_generation=1 WHERE id=$1`, lead, harness)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	generation := int64(1)
	plan, size, reason := "Attempted plan", "s", "interrupted"
	caps, tools, ms := []string{}, []string{}, []Milestone{}
	pid := uuid.New()
	f := reconciliationFixture{codexTestEnv: env, svc: svc, worker: store.Worker{ID: worker, UserID: user}, lead: lead,
		candidate: PlanCrossCheckCandidate{PlanMd: plan, Milestones: []byte(`[]`), RequiredCapabilities: caps, RequiredTools: tools,
			SizeClass: size, BaseCommit: strings.Repeat("a", 40), PlanningDiff: strings.Repeat("private large diff", 20000)},
		req: StateRequest{State: "awaiting_approval", ClaimGeneration: &generation, PlanMd: &plan, SizeClass: &size,
			Milestones: &ms, RequiredCapabilities: &caps, RequiredTools: &tools, PresentationID: &pid, PlanCrossCheckGateReason: &reason}}
	if harness == "codex" {
		reason = "codex_lead_unsupported"
	}
	if verdict == "" {
		return f
	}
	f.child = uuid.New()
	env.exec(`INSERT INTO runs (id,user_id,repo_id,worker_id,kind,target_run_id,harness,report_only,
 budget_wall_seconds,issue_title,issue_description,status,claim_generation)
 VALUES ($1,$2,$3,$4,'cross_check',$5,'codex',true,1800,'check','check','running',1)`, f.child, user, repo, worker, lead)
	digest, err := f.candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var reasonClass any
	if verdict != "pending" {
		reasonClass = verdict
	}
	env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
 required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
 checker_run_id,checker_harness,verdict,reason_class,findings,deadline_at)
 VALUES ($1,'plan',1,1,$2,$3,'{}','{}','s',$4,$5,$6,$7,'codex',$8,$9,
 '{"summary":"outcome","items":[]}',now()+interval '30 minutes')`,
		lead, plan, f.candidate.Milestones, f.candidate.BaseCommit, f.candidate.PlanningDiff, digest, f.child, verdict, reasonClass)
	return f
}

func assertReconciliation(t *testing.T, f reconciliationFixture, proof *LeadReconciliation, settled bool) {
	t.Helper()
	if proof == nil {
		t.Fatal("missing reconciliation")
	}
	run := mustRun(t, f.codexTestEnv, f.lead)
	sum := sha256.Sum256([]byte(run.PlanMd.String))
	if proof.LeadLastSeq != run.LastSeq || proof.ClaimGeneration != run.ClaimGeneration ||
		proof.PlanCrossCheckSettled != settled || proof.GateRevision != run.GateRevision ||
		proof.CurrentPlanSHA256 != hex.EncodeToString(sum[:]) || proof.GatePayloadDigest != hex.EncodeToString(run.GatePayloadDigest) {
		t.Fatalf("proof differs from committed authority: %+v", proof)
	}
	if run.GatePresentationID.Valid && (proof.GatePresentationID == nil || *proof.GatePresentationID != uuid.UUID(run.GatePresentationID.Bytes)) {
		t.Fatal("presentation identity missing")
	}
}

func TestReconciliationForcedParkAndRetryLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, harness, verdict, reason, refusal string }{
		{"pending trigger settlement", "claude", "pending", "interrupted", ""},
		{"decided block", "claude", "block", "interrupted", ""},
		{"Codex no cross-check", "codex", "", "codex_lead_unsupported", ""},
		{"failed submit no row", "claude", "", "checker_failed", "submit_failed"},
		{"refused candidate no row", "claude", "", "candidate_refused", "candidate_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconciliationFixture(t, tc.harness, tc.verdict)
			f.req.PlanCrossCheckGateReason = &tc.reason
			f.req.PlanCrossCheckRefusal = tc.refusal
			result, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
			if err != nil || !result.Applied {
				t.Fatalf("park: applied=%v err=%v", result.Applied, err)
			}
			assertReconciliation(t, f, result.Reconciliation, true)
			if result.GateRevision != 1 || result.Reconciliation.GatePresentationID == nil ||
				*result.Reconciliation.GatePresentationID != *f.req.PresentationID {
				t.Fatal("persisted gate not captured")
			}
			if tc.verdict == "pending" {
				cc, err := f.q.GetPlanCrossCheck(f.ctx, f.lead)
				if err != nil || cc.Verdict != "failed" || cc.ReasonClass.String != "superseded" {
					t.Fatalf("trigger settlement: %+v %v", cc, err)
				}
				if mustRun(t, f.codexTestEnv, f.child).Status != "cancelled" {
					t.Fatal("child was not cancelled")
				}
			}
			retry, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
			if err != nil || !retry.Applied || retry.GateRevision != result.GateRevision {
				t.Fatalf("lost ACK retry: %+v %v", retry, err)
			}
			assertReconciliation(t, f, retry.Reconciliation, true)
			status, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
			if tc.verdict == "" {
				if !errors.Is(err, ErrCrossCheckNoRow) {
					t.Fatalf("no row: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !status.Parked || status.CrossCheck.ID != uuid.Nil {
				t.Fatal("parked read returned candidate")
			}
			assertReconciliation(t, f, status.Reconciliation, true)
			raw, err := json.Marshal(status.Reconciliation)
			if err != nil || len(raw) > 600 || strings.Contains(string(raw), "private large diff") {
				t.Fatalf("unbounded proof: len=%d err=%v", len(raw), err)
			}
			original := *f.req.PresentationID
			changed := "Changed plan"
			f.req.PlanMd = &changed
			conflict, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
			if !errors.Is(err, ErrGatePresentationConflict) || conflict.Reconciliation != nil {
				t.Fatalf("changed payload: %v", err)
			}
			f.req.PresentationID = pointerUUID(uuid.New())
			f.req.PlanCrossCheckGateReason = nil
			f.req.PlanCrossCheckRefusal = ""
			next, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
			if err != nil || next.GateRevision != 2 {
				t.Fatalf("new human revision: %+v %v", next, err)
			}
			f.req.PresentationID = &original
			historical, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
			if !errors.Is(err, ErrGatePresentationHistorical) || historical.Reconciliation != nil {
				t.Fatalf("historical id: %v", err)
			}
		})
	}
}

func pointerUUID(id uuid.UUID) *uuid.UUID { return &id }

func TestReconciliationParkReadOnlyAndFencesLiveDB(t *testing.T) {
	f := newReconciliationFixture(t, "claude", "pending")
	if _, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req); err != nil {
		t.Fatal(err)
	}
	// A deliberately expired pending fixture after park tests strict settlement and
	// read-only semantics. NULL reason is valid for pending under the reason constraint.
	f.exec(`UPDATE cross_checks SET verdict='pending',reason_class=NULL,decided_at=NULL,
 deadline_at=now()-interval '1 second',lead_claim_generation=2 WHERE lead_run_id=$1`, f.lead)
	before, err := f.q.GetPlanCrossCheck(f.ctx, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	childBefore := mustRun(t, f.codexTestEnv, f.child)
	status, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if err != nil || !status.Parked || status.Verdict != "failed" || status.ReasonClass != "interrupted" {
		t.Fatalf("stale pending parked read: %+v %v", status, err)
	}
	assertReconciliation(t, f, status.Reconciliation, false)
	originalProof := status.Reconciliation
	// A later current plan change must be distinguishable even though the immutable
	// presentation still identifies the lost ACK's full payload.
	f.exec(`UPDATE runs SET plan_md='Later current plan' WHERE id=$1`, f.lead)
	revised, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if err != nil || revised.Reconciliation.CurrentPlanSHA256 == originalProof.CurrentPlanSHA256 ||
		revised.Reconciliation.GatePayloadDigest != originalProof.GatePayloadDigest ||
		*revised.Reconciliation.GatePresentationID != *originalProof.GatePresentationID {
		t.Fatal("current plan revision was hidden by presentation identity")
	}
	assertReconciliation(t, f, revised.Reconciliation, false)
	after, err := f.q.GetPlanCrossCheck(f.ctx, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	if before.Verdict != after.Verdict || before.ReasonClass != after.ReasonClass || before.DeadlineAt != after.DeadlineAt ||
		before.DecidedAt != after.DecidedAt || before.CheckerRunID != after.CheckerRunID {
		t.Fatal("parked read mutated expired pending attempt")
	}
	childAfter := mustRun(t, f.codexTestEnv, f.child)
	if childAfter.Status != childBefore.Status || childAfter.ClaimReleasedAt != childBefore.ClaimReleasedAt {
		t.Fatal("parked read revived child")
	}
	var children int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runs WHERE target_run_id=$1`, f.lead).Scan(&children); err != nil || children != 1 {
		t.Fatalf("children=%d err=%v", children, err)
	}
	// A decided historical approve must also remain an interruption.
	f.exec(`UPDATE cross_checks SET verdict='approve',reason_class='approve' WHERE lead_run_id=$1`, f.lead)
	status, err = f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if err != nil || status.Verdict != "failed" || status.ReasonClass != "interrupted" {
		t.Fatalf("historical approve granted: %+v %v", status, err)
	}
	assertReconciliation(t, f, status.Reconciliation, true)
	for _, tc := range []struct {
		name   string
		worker store.Worker
		gen    int64
	}{
		{"wrong generation", f.worker, 2},
		{"wrong user", store.Worker{ID: f.worker.ID, UserID: uuid.New()}, 1},
		{"wrong worker", store.Worker{ID: uuid.New(), UserID: f.worker.UserID}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, tc.worker, f.lead, tc.gen, 1)
			if !errors.Is(err, ErrCrossCheckRefused) || result.Reconciliation != nil {
				t.Fatalf("old owner proof: %+v %v", result, err)
			}
		})
	}
	f.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, f.lead)
	denied, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if !errors.Is(err, ErrCrossCheckRefused) || denied.Reconciliation != nil {
		t.Fatalf("released proof: %+v %v", denied, err)
	}
}

func TestReconciliationCommitFailureLiveDB(t *testing.T) {
	f := newReconciliationFixture(t, "claude", "pending")
	f.svc.SetTxBeginner(&failFirstCommitBeginner{pool: f.pool})
	result, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
	if !errors.Is(err, errInjectedCommit) || result.Reconciliation != nil || result.GateRevision != 0 || result.Applied {
		t.Fatalf("failed commit promised proof: %+v %v", result, err)
	}
	run := mustRun(t, f.codexTestEnv, f.lead)
	cc, err := f.q.GetPlanCrossCheck(f.ctx, f.lead)
	if err != nil || run.Status != "running" || run.GateRevision != 0 || cc.Verdict != "pending" {
		t.Fatalf("rollback failed: status=%s revision=%d verdict=%s err=%v", run.Status, run.GateRevision, cc.Verdict, err)
	}
	result, err = f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, f.req)
	if err != nil {
		t.Fatal(err)
	}
	assertReconciliation(t, f, result.Reconciliation, true)
	f.svc.SetTxBeginner(&failFirstCommitBeginner{pool: f.pool})
	status, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if !errors.Is(err, errInjectedCommit) || status.Reconciliation != nil || status.Parked {
		t.Fatalf("failed read commit exposed proof: %+v %v", status, err)
	}
}

type reconciliationHookBeginner struct {
	pool interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	beforeLock   func()
	beforeCommit func(pgx.Tx) error
}

func (b reconciliationHookBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if b.beforeLock != nil {
		b.beforeLock()
	}
	return &reconciliationHookTx{Tx: tx, check: b.beforeCommit}, nil
}

type reconciliationHookTx struct {
	pgx.Tx
	check     func(pgx.Tx) error
	proofRead bool
}

func (tx *reconciliationHookTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: HasPendingPlanCrossCheck") {
		tx.proofRead = true
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *reconciliationHookTx) Commit(ctx context.Context) error {
	if !tx.proofRead {
		return errors.New("commit reached before settlement proof read")
	}
	if tx.check != nil {
		if err := tx.check(tx.Tx); err != nil {
			return err
		}
	}
	return tx.Tx.Commit(ctx)
}

func TestReconciliationForcedDecisionRaceLiveDB(t *testing.T) {
	for _, verdict := range []string{"block", "approve"} {
		t.Run(verdict, func(t *testing.T) {
			f := newReconciliationFixture(t, "claude", "pending")
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			started, resume := make(chan struct{}), make(chan struct{})
			f.svc.SetTxBeginner(reconciliationHookBeginner{pool: f.pool,
				beforeLock: func() {
					close(started)
					select {
					case <-resume:
					case <-ctx.Done():
					}
				},
				beforeCommit: func(tx pgx.Tx) error {
					// At the proof-read/commit seam, another connection must still be unable
					// to acquire the lead. NOWAIT bounds the probe; no sibling operation blocks.
					_, err := f.pool.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT`, f.lead)
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
						return errors.New("lead lock released before proof commit")
					}
					return nil
				}})
			type answer struct {
				result StateReportResult
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				result, err := f.svc.SetStateReportWithReconciliation(ctx, f.worker, f.lead, f.req)
				done <- answer{result, err}
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("park goroutine did not stop after cancellation")
					}
				}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				close(resume)
				t.Fatal("park did not reach lock seam")
			}
			// The workflow already failed and its unlocked snapshot had cursor zero.
			// Decide commits a real server event before the forced park acquires its lock.
			decider := New(f.q, f.box, testParams())
			decider.SetTxBeginner(f.pool)
			_, decisionErr := decider.DecidePlanCrossCheck(ctx, f.worker, f.child, 1, verdict, verdict, []byte(`{"summary":"decision","items":[]}`))
			close(resume)
			var got answer
			select {
			case got = <-done:
				joined = true
			case <-ctx.Done():
				t.Fatal("bounded race timed out")
			}
			if decisionErr != nil {
				t.Fatal(decisionErr)
			}
			if got.err != nil || !got.result.Applied {
				t.Fatalf("forced park: %+v %v", got.result, got.err)
			}
			assertReconciliation(t, f, got.result.Reconciliation, true)
			if got.result.Reconciliation.LeadLastSeq != 1 {
				t.Fatal("proof used pre-park cursor; server event was lost")
			}
			var event string
			if err := f.pool.QueryRow(ctx, `SELECT payload->>'verdict' FROM run_messages WHERE run_id=$1 AND seq=1`, f.lead).Scan(&event); err != nil || event != verdict {
				t.Fatalf("server event: %q %v", event, err)
			}
			status, err := decider.PlanCrossCheckStatusWithReconciliation(ctx, f.worker, f.lead, 1, 1)
			if err != nil || !status.Parked || status.CrossCheck.ID != uuid.Nil {
				t.Fatalf("lost forced ACK read: %+v %v", status, err)
			}
			assertReconciliation(t, f, status.Reconciliation, true)
			run := mustRun(t, f.codexTestEnv, f.lead)
			want := verdict
			if verdict == "approve" {
				want = "interrupted"
			}
			// A concurrent approval cannot erase the forced gate’s interruption reason.
			if run.PlanCrossCheckGateReason.String != want {
				t.Fatalf("forced %s race reason=%q, want %q", verdict, run.PlanCrossCheckGateReason.String, want)
			}
		})
	}
}

func TestReconciliationGuardedRunningLiveDB(t *testing.T) {
	f := newReconciliationFixture(t, "claude", "approve")
	digest, err := f.candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	req := f.req
	req.State, req.PresentationID, req.PlanCrossCheckGateReason = "running", nil, nil
	req.CandidateDigest = hex.EncodeToString(digest)
	result, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, req)
	if err != nil || !result.Applied {
		t.Fatalf("approved running write: %+v %v", result, err)
	}
	assertReconciliation(t, f, result.Reconciliation, true)
	retry, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, req)
	if err != nil || !retry.Applied {
		t.Fatalf("guarded running retry: applied=%v err=%v", retry.Applied, err)
	}
	assertReconciliation(t, f, retry.Reconciliation, true)
	if result.Reconciliation.GatePresentationID != nil || result.GateRevision != 0 {
		t.Fatal("running write invented gate")
	}
	// A required lead heartbeat carries the same committed cursor. This proof alone
	// does not establish that an exact candidate plan write was applied.
	req.PlanMd = nil
	ordinary, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, req)
	if err != nil || !ordinary.Applied {
		t.Fatalf("heartbeat proof: %+v %v", ordinary, err)
	}
	assertReconciliation(t, f, ordinary.Reconciliation, true)
	req.PlanMd, req.ClaimGeneration = f.req.PlanMd, nil
	unfenced, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, req)
	if !errors.Is(err, ErrClaimGenerationRequired) || unfenced.Reconciliation != nil {
		t.Fatalf("unfenced proof: %+v %v", unfenced, err)
	}
	stale := int64(2)
	req.ClaimGeneration = &stale
	rejected, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, req)
	if !errors.Is(err, ErrStaleClaim) || rejected.Reconciliation != nil {
		t.Fatalf("stale proof: %+v %v", rejected, err)
	}
}

func TestReconciliationStateRefusalsAndOrdinaryLiveDB(t *testing.T) {
	for _, tc := range []string{"wrong user", "wrong worker", "released", "terminal", "missing transaction", "missing generation", "ordinary human"} {
		t.Run(tc, func(t *testing.T) {
			f := newReconciliationFixture(t, "codex", "")
			worker := f.worker
			switch tc {
			case "wrong user":
				worker.UserID = uuid.New()
			case "wrong worker":
				worker.ID = uuid.New()
			case "released":
				f.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, f.lead)
			case "terminal":
				f.exec(`UPDATE runs SET status='cancelled' WHERE id=$1`, f.lead)
			case "missing transaction":
				f.svc.SetTxBeginner(nil)
			case "missing generation":
				f.req.ClaimGeneration = nil
			case "ordinary human":
				f.exec(`UPDATE runs SET plan_cross_check_required=false,auto_approve=false WHERE id=$1`, f.lead)
				f.req.PlanCrossCheckGateReason = nil
			}
			result, err := f.svc.SetStateReportWithReconciliation(f.ctx, worker, f.lead, f.req)
			if result.Reconciliation != nil {
				t.Fatalf("%s granted proof", tc)
			}
			switch tc {
			case "ordinary human":
				if err != nil || !result.Applied || result.GateRevision != 1 {
					t.Fatalf("ordinary report: applied=%v revision=%d err=%v", result.Applied, result.GateRevision, err)
				}
			case "terminal":
				if err != nil || result.Applied {
					t.Fatalf("terminal no-op: applied=%v err=%v", result.Applied, err)
				}
			default:
				if err == nil || result.Applied {
					t.Fatalf("%s accepted: applied=%v err=%v", tc, result.Applied, err)
				}
				if mustRun(t, f.codexTestEnv, f.lead).GateRevision != 0 {
					t.Fatalf("%s wrote presentation", tc)
				}
			}
		})
	}
}

func TestReconciliationRunningCommitFailureLiveDB(t *testing.T) {
	f := newReconciliationFixture(t, "claude", "approve")
	digest, err := f.candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	req := f.req
	req.State, req.PresentationID, req.PlanCrossCheckGateReason = "running", nil, nil
	req.CandidateDigest = hex.EncodeToString(digest)
	f.svc.SetTxBeginner(&failFirstCommitBeginner{pool: f.pool})
	result, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.worker, f.lead, req)
	if !errors.Is(err, errInjectedCommit) || result.Reconciliation != nil || result.Applied {
		t.Fatalf("running commit promised proof: applied=%v err=%v", result.Applied, err)
	}
	if mustRun(t, f.codexTestEnv, f.lead).PlanMd.Valid {
		t.Fatal("running plan survived rollback")
	}
}

func TestReconciliationActivePollLiveDB(t *testing.T) {
	f := newReconciliationFixture(t, "claude", "pending")
	active, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if err != nil || active.Parked || active.CrossCheck.PlanningDiff.String != f.candidate.PlanningDiff {
		t.Fatalf("active candidate changed: %v", err)
	}
	assertReconciliation(t, f, active.Reconciliation, false)
	f.exec(`UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1`, f.lead)
	expired, err := f.svc.PlanCrossCheckStatusWithReconciliation(f.ctx, f.worker, f.lead, 1, 1)
	if err != nil || expired.Verdict != "failed" || expired.ReasonClass != "timed_out" || expired.LeadLastSeq != 1 {
		t.Fatalf("expiry behavior changed: %+v %v", expired, err)
	}
	assertReconciliation(t, f, expired.Reconciliation, true)
}
