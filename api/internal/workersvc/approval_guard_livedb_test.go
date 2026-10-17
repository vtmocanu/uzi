package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// All submissions and publications use the real Service and migrated database.
// SQL below only seeds historical controls and injects a run-scoped storage fault.
func approvalGuardRevise(t *testing.T, f *gateFix, id uuid.UUID) int64 {
	t.Helper()
	res, err := f.submit(id, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

// Start with a selection that a successful own-selection approval would change.
func approvalGuardSelectionFixture(t *testing.T, f *gateFix, id uuid.UUID) {
	t.Helper()
	f.exec("UPDATE runs SET agent_source='repo', agent_exclusions='[]' WHERE id=$1", id)
	run := reconciliationRow(t, f, id)
	if len(run.RequiredCapabilities) == 0 || run.AgentSource.String != AgentSourceRepo {
		t.Fatal("approval guard fixture needs required capabilities and a prior repo selection")
	}
}

func approvalGuardPending(t *testing.T, f *gateFix, id uuid.UUID, input int64) {
	t.Helper()
	var pending bool
	if err := f.pool.QueryRow(f.ctx, `SELECT applied_at IS NULL AND disposition IS DISTINCT FROM 'superseded'
 AND gate_binding='bound' AND gate_revision=1 FROM run_user_inputs WHERE run_id=$1 AND id=$2`, id, input).Scan(&pending); err != nil || !pending {
		t.Fatalf("current revision must remain pending: pending=%v err=%v", pending, err)
	}
}

func approvalGuardRefused(t *testing.T, f *gateFix, id uuid.UUID, sel *AgentSelection, opts SubmitInputOptions) {
	t.Helper()
	before := reconciliationRaceSnapshot(t, f, id)
	_, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", sel, opts)
	if !errors.Is(err, ErrPlanRevisionPending) {
		t.Errorf("pending approval error = %v, want ErrPlanRevisionPending", err)
	}
	reconciliationRaceUnchanged(t, f, id, before)
}

func TestApprovalGuardPendingRevisionLiveDB(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("selection=%v/override=%v", selection, override), func(t *testing.T) {
				f := newGateFix(t, 3)
				id := f.gatedRun()
				// Exercise a first approval that would freeze a structural contract.
				f.exec("UPDATE runs SET completion_contract_version=1 WHERE id=$1", id)
				approvalGuardSelectionFixture(t, f, id)
				run := reconciliationRow(t, f, id)
				if !run.CompletionContractVersion.Valid || len(run.MilestonesCandidate) == 0 ||
					len(run.MilestonesFrozen) != 0 || len(run.CompletionContract) != 0 || run.ContractRevision.Valid {
					t.Fatal("pending approval fixture must have an unfrozen structural contract and candidate milestones")
				}
				f.liveWorker()
				input := approvalGuardRevise(t, f, id)
				var sel *AgentSelection
				if selection {
					sel = &AgentSelection{Source: AgentSourceOwn}
				}
				opts := SubmitInputOptions{OverrideCapabilities: override}
				// Delivery and even repeated ACKs do not settle a revision request.
				approvalGuardRefused(t, f, id, sel, opts)
				opts.ExpectedGateRevision = i64(1)
				got, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id)
				if err != nil || len(got.Inputs) != 1 || got.Inputs[0].ID != input {
					t.Fatalf("delivery=%+v err=%v", got, err)
				}
				for n := 0; n < 2; n++ {
					ack, err := f.svc.AckInputs(f.ctx, f.wkr, id, 1, []int64{input})
					if err != nil || !ack.Active || len(ack.Inputs) != 1 {
						t.Fatalf("ACK %d=%+v err=%v", n, ack, err)
					}
					approvalGuardPending(t, f, id, input)
					approvalGuardRefused(t, f, id, sel, opts)
				}
			})
		}
	}
}

func TestApprovalGuardNonApprovalControlsLiveDB(t *testing.T) {
	for _, kind := range []string{"revise_plan", "cancel", "reject_plan"} {
		t.Run(kind, func(t *testing.T) {
			f := newGateFix(t, 3)
			id := f.gatedRun()
			f.liveWorker()
			input := approvalGuardRevise(t, f, id)
			approvalGuardRefused(t, f, id, nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})

			opts := SubmitInputOptions{}
			if kind != "cancel" {
				opts.ExpectedGateRevision = i64(1)
			}
			result, err := f.submit(id, kind, nil, opts)
			if err != nil || result.ServerSide {
				t.Fatalf("%s with pending revision: result=%+v err=%v", kind, result, err)
			}
			approvalGuardPending(t, f, id, input)
			var newInput int64
			var pending bool
			if err := f.pool.QueryRow(f.ctx, `SELECT id, body='because' AND applied_at IS NULL
 AND consumed_at IS NULL AND disposition IS NULL
 FROM run_user_inputs WHERE run_id=$1 AND kind=$2 AND id<>$3`, id, kind, input).Scan(&newInput, &pending); err != nil || !pending || newInput <= input {
				t.Fatalf("%s durable input=%d pending=%v err=%v", kind, newInput, pending, err)
			}
			run := reconciliationRow(t, f, id)
			wantCount := int32(1)
			binding := "bound(1)"
			switch kind {
			case "revise_plan":
				wantCount = 2
				if result.ID != newInput || run.StopKind.Valid {
					t.Fatalf("second revise result=%+v input=%d stop=%v", result, newInput, run.StopKind)
				}
				approvalGuardPending(t, f, id, newInput)
			case "cancel":
				binding = "legacy"
				if !run.StopKind.Valid || run.StopKind.String != "cancelled" || !run.StopReason.Valid || run.StopReason.String != "because" {
					t.Fatalf("cancel intent=%v reason=%v", run.StopKind, run.StopReason)
				}
			case "reject_plan":
				if !run.StopKind.Valid || run.StopKind.String != "plan_rejected" || run.StopReason.Valid {
					t.Fatalf("reject intent=%v reason=%v", run.StopKind, run.StopReason)
				}
			}
			if run.ReviseCount != wantCount || run.Status != "awaiting_approval" || run.GateRevision != 1 {
				t.Fatalf("%s changed revision count or gate: count=%d status=%s revision=%d", kind, run.ReviseCount, run.Status, run.GateRevision)
			}
			f.assertVerdicts(t, id, "revise_plan=bound(1)", kind+"="+binding)
		})
	}
}

func TestApprovalGuardPendingPredicateControlsLiveDB(t *testing.T) {
	for _, control := range []string{"settled", "old gate", "unbound", "superseded", "declined pending"} {
		t.Run(control, func(t *testing.T) {
			f := newGateFix(t, 3)
			id := f.gatedRun()
			approvalGuardSelectionFixture(t, f, id)
			f.liveWorker()
			input := approvalGuardRevise(t, f, id)
			switch control {
			case "settled":
				f.exec("UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", input)
			case "old gate":
				f.mustPublish(id, gateReq(i64(1), uid()), 2)
			case "unbound":
				f.exec("UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", input)
			case "superseded":
				// The disposition excludes this row independently of applied_at.
				f.exec("UPDATE run_user_inputs SET disposition='superseded' WHERE id=$1", input)
			case "declined pending":
				// Only superseded is excluded; another non-null disposition is pending.
				f.exec("UPDATE run_user_inputs SET disposition='declined' WHERE id=$1", input)
			}
			if control == "declined pending" {
				approvalGuardRefused(t, f, id, nil, SubmitInputOptions{})
				return
			}
			rev := f.row(id).revision
			if _, err := f.submit(id, "approve_plan", nil, SubmitInputOptions{ExpectedGateRevision: &rev}); err != nil {
				t.Fatalf("%s must permit approval: %v", control, err)
			}
			f.assertVerdicts(t, id, "revise_plan="+map[bool]string{true: "unbound", false: "bound(1)"}[control == "unbound"], fmt.Sprintf("approve_plan=bound(%d)", rev))
		})
	}
}

func approvalGuardDeliver(t *testing.T, f *gateFix, id uuid.UUID, input int64) {
	t.Helper()
	got, err := f.svc.ConsumeInputs(f.ctx, f.wkr, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range got.Inputs {
		if row.ID == input {
			found = true
		}
	}
	if !found {
		t.Fatalf("revision %d missing from delivery: %+v", input, got)
	}
	ack, err := f.svc.AckInputs(f.ctx, f.wkr, id, 1, []int64{input})
	if err != nil || !ack.Active || len(ack.Inputs) != 1 {
		t.Fatalf("revision ACK=%+v err=%v", ack, err)
	}
}

func approvalGuardReplacement(input int64) StateRequest {
	req := gateReq(i64(1), uid())
	plan := "# Replacement after revision"
	req.PlanMd = &plan
	req.Milestones = &[]Milestone{{ID: "M1", Title: "Changed first"}, {ID: "M2", Title: "New second"}}
	req.ExpectedGateRevision = i64(1)
	req.RevisionInputID = &input
	return req
}

func TestApprovalGuardReplacementSettlementLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprintf("frozen=%v", frozen), func(t *testing.T) {
			f := newGateFix(t, 3)
			id := f.gatedRun()
			approvalGuardSelectionFixture(t, f, id)
			f.liveWorker()
			if frozen {
				if _, err := f.submit(id, "approve_plan", &AgentSelection{Source: AgentSourceOwn}, SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
					t.Fatal(err)
				}
			}
			input := approvalGuardRevise(t, f, id)
			approvalGuardRefused(t, f, id, nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})
			approvalGuardDeliver(t, f, id, input)
			req := approvalGuardReplacement(input)
			// Exercise the public SetState seam as well as SetStateReport.
			_, applied, err := f.svc.SetState(f.ctx, f.wkr, id, req)
			if err != nil || !applied {
				t.Fatalf("replacement applied=%v err=%v", applied, err)
			}
			var settled bool
			if err := f.pool.QueryRow(f.ctx, "SELECT applied_at IS NOT NULL AND disposition IS NULL FROM run_user_inputs WHERE id=$1", input).Scan(&settled); err != nil || !settled {
				t.Fatalf("replacement did not settle revision: %v %v", settled, err)
			}
			before := reconciliationRaceSnapshot(t, f, id)
			_, err = f.submit(id, "approve_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(2)})
			if frozen {
				if !errors.Is(err, ErrReconciliationRequired) {
					t.Fatalf("changed frozen contract approval=%v", err)
				}
				reconciliationRaceUnchanged(t, f, id, before)
			} else if err != nil {
				t.Fatalf("settled unfrozen replacement refused: %v", err)
			}
		})
	}
}

func TestApprovalGuardCapabilityClearRollbackLiveDB(t *testing.T) {
	for _, selection := range []bool{false, true} {
		t.Run(fmt.Sprintf("selection=%v", selection), func(t *testing.T) {
			f := newGateFix(t, 3)
			id := f.gatedRun()
			approvalGuardSelectionFixture(t, f, id)
			name := "approval_clear_" + fmt.Sprintf("%x", id[:])
			f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.id='%s'::uuid AND cardinality(OLD.required_capabilities)>0 AND cardinality(NEW.required_capabilities)=0
 THEN RAISE EXCEPTION 'approval clear injected failure' USING ERRCODE='P0001'; END IF; RETURN NEW; END $$`, name, id))
			f.exec(fmt.Sprintf("CREATE TRIGGER %s BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION %s()", name, name))
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := f.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON runs; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
					t.Errorf("trigger cleanup: %v", err)
				}
			})
			var sel *AgentSelection
			if selection {
				sel = &AgentSelection{Source: AgentSourceOwn}
			}
			opts := SubmitInputOptions{ExpectedGateRevision: i64(1), OverrideCapabilities: true}
			before := reconciliationRaceSnapshot(t, f, id)
			_, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", sel, opts)
			if err == nil || !strings.Contains(err.Error(), "approval clear injected failure") {
				t.Fatalf("clear fault not reached: %v", err)
			}
			// Continue through retry even on the baseline's partial commit.
			if after := reconciliationRaceSnapshot(t, f, id); after != before {
				t.Error("failed capability clear committed approval, selection or freeze")
			}
			f.exec("DROP TRIGGER " + name + " ON runs")
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", sel, opts); err != nil {
				t.Fatalf("retry: %v", err)
			}
			var approvals int
			var cleared bool
			if err := f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'),cardinality(required_capabilities)=0 FROM runs WHERE id=$1`, id).Scan(&approvals, &cleared); err != nil {
				t.Fatal(err)
			}
			if approvals != 1 || !cleared {
				t.Fatalf("retry approvals=%d cleared=%v, want 1/true", approvals, cleared)
			}
		})
	}
}

// Each race uses two independent database connections. The first operation pauses
// before COMMIT while holding its row lock; pg_stat_activity proves the second
// operation is blocked. The shared 15s deadline bounds every barrier and query.
func TestApprovalGuardSerializationLiveDB(t *testing.T) {
	for _, order := range []string{"revise before approve", "approve before revise", "replacement before approve"} {
		t.Run(order, func(t *testing.T) {
			f := newGateFix(t, 3)
			reconciliationRaceDeadline(t, f)
			id := f.gatedRun()
			approvalGuardSelectionFixture(t, f, id)
			f.liveWorker()
			var input int64
			if order == "replacement before approve" {
				input = approvalGuardRevise(t, f, id)
				approvalGuardDeliver(t, f, id, input)
			}
			first, err := f.pool.Acquire(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(first.Release)
			second, err := f.pool.Acquire(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(second.Release)
			for index, conn := range []*pgx.Conn{first.Conn(), second.Conn()} {
				// Only approval inherits REPEATABLE READ: admission must explicitly
				// override it. Revision and publication keep their normal isolation.
				isolation := "READ COMMITTED"
				if (order == "approve before revise" && index == 0) || (order != "approve before revise" && index == 1) {
					isolation = "REPEATABLE READ"
				}
				if _, err := conn.Exec(f.ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL "+isolation); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := conn.Exec(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
						t.Errorf("restore isolation: %v", err)
					}
				})
			}
			a := reconciliationRaceService(f.svc, store.New(first))
			b := reconciliationRaceService(f.svc, store.New(second))
			barrier := &reconciliationRaceBeginner{conn: first.Conn(), ready: make(chan struct{}), release: make(chan struct{})}
			a.SetTxBeginner(barrier)
			b.SetTxBeginner(second)
			var once sync.Once
			release := func() { once.Do(func() { close(barrier.release) }) }
			var wg sync.WaitGroup
			defer func() { release(); wg.Wait() }()
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			approve := func(s *Service) error {
				expected := i64(1)
				if order == "replacement before approve" {
					expected = nil
				}
				_, err := s.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: expected})
				return err
			}
			revise := func(s *Service) error {
				_, err := s.SubmitInputWithOptions(f.ctx, f.userID, id, "revise_plan", "revise", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})
				return err
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				var err error
				switch order {
				case "approve before revise":
					err = approve(a)
				case "revise before approve":
					// Revision submission is normally autocommit. Hold an explicit
					// transaction around the real Service so its writes stay locked
					// until this race's commit barrier releases them.
					var tx pgx.Tx
					tx, err = barrier.Begin(f.ctx)
					if err == nil {
						defer func() { _ = tx.Rollback(f.ctx) }()
						writer := reconciliationRaceService(f.svc, store.New(tx))
						err = revise(writer)
						if err == nil {
							err = tx.Commit(f.ctx)
						}
					}
				default:
					_, applied, _, e := a.SetStateReport(f.ctx, f.wkr, id, approvalGuardReplacement(input))
					err = e
					if err == nil && !applied {
						err = errors.New("replacement not applied")
					}
				}
				firstDone <- err
			}()
			reconciliationRaceWait(t, f.ctx, barrier.ready)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if order == "approve before revise" {
					secondDone <- revise(b)
				} else {
					secondDone <- approve(b)
				}
			}()
			reconciliationRaceBlocked(t, f, second.Conn().PgConn().PID())
			release()
			wg.Wait()
			if err := <-firstDone; err != nil {
				t.Fatalf("first operation: %v", err)
			}
			secondErr := <-secondDone
			if order == "revise before approve" {
				if !errors.Is(secondErr, ErrPlanRevisionPending) {
					t.Errorf("approval error = %v, want ErrPlanRevisionPending", secondErr)
				}
			} else if secondErr != nil {
				t.Fatalf("second operation: %v", secondErr)
			}
			var approvals, revisions int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER(WHERE kind='approve_plan'),count(*) FILTER(WHERE kind='revise_plan') FROM run_user_inputs WHERE run_id=$1`, id).Scan(&approvals, &revisions); err != nil {
				t.Fatal(err)
			}
			want := 1
			if order == "revise before approve" {
				want = 0
			}
			if approvals != want || revisions != 1 {
				t.Fatalf("approvals=%d revisions=%d, want %d/1", approvals, revisions, want)
			}
			if order == "replacement before approve" {
				f.assertVerdicts(t, id, "revise_plan=bound(1)", "approve_plan=bound(2)")
			} else {
				approvalGuardPending(t, f, id, replacementIDs(t, f, id)[len(replacementIDs(t, f, id))-1])
			}
		})
	}
}

func TestApprovalGuardReconciliationPendingLiveDB(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("selection=%v/override=%v", selection, override), func(t *testing.T) {
				f, id, opts := newMilestoneReconciliationFix(t)
				approvalGuardSelectionFixture(t, f, id)
				input := approvalGuardRevise(t, f, id)
				// Consent describes the current state, so the pending guard is
				// exercised independently of stale consent and witness readiness.
				opts.ExpectedResetStateDigest, _ = ResetStateDigest(reconciliationRow(t, f, id))
				opts.OverrideCapabilities = override
				var sel *AgentSelection
				if selection {
					sel = &AgentSelection{Source: AgentSourceOwn}
				}
				approvalGuardRefused(t, f, id, sel, opts)
				approvalGuardPending(t, f, id, input)
			})
		}
	}
}
