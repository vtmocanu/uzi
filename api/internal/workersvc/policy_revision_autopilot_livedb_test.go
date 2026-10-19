package workersvc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func autopilotRevisionFixture(t *testing.T) (codexTestEnv, *gateFix, uuid.UUID, StateRequest) {
	t.Helper()
	e, f, run, _ := acceptanceUnpublishedFixture(t) // Public CreateTaskRun birth coverage.
	// Bridge worker admission only; plan/source writes below use the real report.
	e.exec("UPDATE runs SET status='claimed',plan_cross_check_required=false WHERE id=$1", run)
	return e, f, run, StateRequest{State: "running", ClaimGeneration: i64(1), PlanMd: strPtr("# Actual autopilot plan")}
}

func autopilotRevisionReport(t *testing.T, f *gateFix, run uuid.UUID, req StateRequest) store.Run {
	t.Helper()
	r, applied, _, err := f.svc.SetStateReport(f.ctx, f.wkr, run, req)
	if err != nil || !applied {
		t.Fatalf("SetStateReport: applied=%v err=%v", applied, err)
	}
	return r
}

func assertAutopilotApproval(t *testing.T, e codexTestEnv, run uuid.UUID, want bool) {
	t.Helper()
	r := mustRun(t, e, run)
	if r.PolicyRefusalCount <= 0 {
		t.Fatal("approval assertion must cross the constrained boundary")
	}
	if got := policyRevisionPlanApproved(e.ctx, e.q, r, true); got != want {
		t.Fatalf("approval=%v want=%v source=%s", got, want, r.PolicyRevisionSource)
	}
}

func TestPolicyRevisionAutopilotActualPlanLiveDB(t *testing.T) {
	e, f, run, req := autopilotRevisionFixture(t)
	// Model an earlier refusal without enabling a flag-based reader waiver.
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	f.svc.p.PolicyHoldNegotiated = false
	f.svc.p.PolicyHoldSnapshotEnabled = false
	assertAutopilotApproval(t, e, run, false)
	autopilotRevisionReport(t, f, run, req)
	assertAutopilotApproval(t, e, run, true)
	// A heartbeat and a same-body retry preserve the accepted pair.
	pair := acceptancePair(t, e, run)
	autopilotRevisionReport(t, f, run, StateRequest{State: "running", ClaimGeneration: i64(1)})
	autopilotRevisionReport(t, f, run, req)
	if acceptancePair(t, e, run) != pair {
		t.Fatal("retry changed accepted pair")
	}
}

func TestPolicyRevisionAutopilotMissingCheckRefusalLiveDB(t *testing.T) {
	e, f, run, candidate := acceptanceUnpublishedFixture(t)
	digest, err := candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	req := StateRequest{State: "running", ClaimGeneration: i64(1), PlanMd: &candidate.PlanMd,
		Milestones: &[]Milestone{}, RequiredCapabilities: &candidate.RequiredCapabilities,
		RequiredTools: &candidate.RequiredTools, SizeClass: &candidate.SizeClass,
		CandidateDigest: hex.EncodeToString(digest)}
	before := policyOwnerStopSnapshot(t, e, run)
	_, applied, _, err := f.svc.SetStateReport(e.ctx, f.wkr, run, req)
	if applied || !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing checker approval applied=%v err=%v; want invalid-state refusal", applied, err)
	}
	if policyOwnerStopSnapshot(t, e, run) != before {
		t.Fatal("missing checker refusal changed persisted state")
	}
}

func TestPolicyRevisionSeededActualPlanLiveDB(t *testing.T) {
	e, f, old, _ := autopilotRevisionFixture(t)
	repo := uuid.UUID(mustRun(t, e, old).RepoID.Bytes)
	seedEligibleIssue(t, e, repo, 9468)
	r, err := f.svc.CreateRun(e.ctx, f.userID, repo, 9468, "seeded", nil, nil, false,
		&SeededPlan{PlanMD: "# Owner supplied plan"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Only worker admission and the previous-refusal count are fixture bridges.
	e.exec("UPDATE runs SET worker_id=$2,status='claimed',claim_generation=1,policy_refusal_count=1 WHERE id=$1", r.ID, f.wkr.ID)
	before := acceptancePair(t, e, r.ID)
	autopilotRevisionReport(t, f, r.ID, StateRequest{State: "running", ClaimGeneration: i64(1)})
	assertAutopilotApproval(t, e, r.ID, true)
	if acceptancePair(t, e, r.ID) != before {
		t.Fatal("seeded heartbeat changed coverage")
	}
}

func TestPolicyRevisionAutopilotFutureGateFeedbackLiveDB(t *testing.T) {
	e, f, run, req := autopilotRevisionFixture(t)
	autopilotRevisionReport(t, f, run, req)
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	assertAutopilotApproval(t, e, run, true)
	stale := mustRun(t, e, run)
	// Real publication moves the live row to gate 1. The queued revision row
	// is an explicit input fixture bound to that actual gate, not a fabricated
	// future gate on the live row. This models the reader's snapshot boundary;
	// full Claim transaction wiring is a separate requirement.
	f.mustPublish(run, acceptanceGateRequest(), 1)
	acceptanceInput(t, e, run, "revise_plan", 1)
	assertAutopilotApproval(t, e, run, false)
	if policyRevisionPlanApproved(e.ctx, e.q, stale, true) {
		t.Fatal("future-gate feedback granted stale no-gate approval")
	}
}

func TestPolicyRevisionAutopilotNegativeEvidenceLiveDB(t *testing.T) {
	for _, mode := range []string{"pending_human", "unknown_history", "malformed", "selection", "primary"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run, req := autopilotRevisionFixture(t)
			e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
			switch mode {
			case "pending_human", "primary":
				input, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: run, Kind: "revise_plan", Body: pgconv.Text("revise")})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "primary" {
					acceptanceAck(t, f, run, input.ID)
					source := acceptanceSource(t, e, run)
					if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
						Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(input.ID)),
					}}, i64(1)); err != nil {
						t.Fatal(err)
					}
				}
			case "unknown_history":
				// An unsupported carrier destroys coverage through the real recorder.
				if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
					Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: []byte("{}"),
				}}, i64(1)); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				e.exec("UPDATE runs SET policy_revision_source='{}' WHERE id=$1", run)
			case "selection":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
			}
			before := acceptancePair(t, e, run)
			autopilotRevisionReport(t, f, run, req)
			if acceptancePair(t, e, run) != before {
				t.Fatal("negative evidence accepted")
			}
			assertAutopilotApproval(t, e, run, false)
		})
	}
}

func autopilotDecide(t *testing.T, e codexTestEnv, f *gateFix, cc store.CrossCheck, verdict string) {
	t.Helper()
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	// Bridge execution of the real, opposite-harness checker child.
	e.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1", child, f.wkr.ID)
	if _, err := f.svc.DecidePlanCrossCheck(e.ctx, f.wkr, child, 1, verdict, verdict,
		[]byte(`{"summary":"fixture","items":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRevisionAutopilotHistoricalRevisionLiveDB(t *testing.T) {
	e, f, run, c := acceptanceUnpublishedFixture(t)
	first, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	autopilotDecide(t, e, f, first, "revise")
	source := acceptanceSource(t, e, run)
	id := first.ID.String()
	if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
		Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising",
		Payload: revisionWriterPayload(t, source, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id}),
	}}, i64(1)); err != nil {
		t.Fatal(err)
	}
	c.PlanMd = "# Newly approved automatic candidate"
	second, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	autopilotDecide(t, e, f, second, "approve")
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	assertAutopilotApproval(t, e, run, false)
	history := acceptanceHistory(t, e, run)
	req := StateRequest{State: "running", ClaimGeneration: i64(1), PlanMd: &c.PlanMd,
		Milestones: &[]Milestone{}, RequiredCapabilities: &c.RequiredCapabilities, RequiredTools: &c.RequiredTools,
		SizeClass: &c.SizeClass, CandidateDigest: hex.EncodeToString(digest)}
	autopilotRevisionReport(t, f, run, req)
	accepted := acceptanceSource(t, e, run)
	if accepted.PrimaryRevision != nil || len(accepted.AssociatedCrossChecks) != 1 ||
		*accepted.AssociatedCrossChecks[0].CrossCheckID != second.ID.String() {
		t.Fatal("wrong retained inventory")
	}
	assertAutopilotApproval(t, e, run, true)
	if acceptanceHistory(t, e, run) != history {
		t.Fatal("acceptance mutated immutable feedback history")
	}
}

func TestPolicyRevisionAutopilotRetryCannotAcceptNewRevisionLiveDB(t *testing.T) {
	e, f, run, req := autopilotRevisionFixture(t)
	autopilotRevisionReport(t, f, run, req)
	input, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: run, Kind: "revise_plan", Body: pgconv.Text("new revision")})
	if err != nil {
		t.Fatal(err)
	}
	acceptanceAck(t, f, run, input.ID)
	source := acceptanceSource(t, e, run)
	if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
		Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(input.ID)),
	}}, i64(1)); err != nil {
		t.Fatal(err)
	}
	// Retain actual APPLIED history while proving retries cannot accept the primary.
	acceptanceApply(t, f, run, input.ID)
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	before, history := acceptancePair(t, e, run), acceptanceHistory(t, e, run)
	if acceptanceSource(t, e, run).PrimaryRevision == nil {
		t.Fatal("recorder did not establish primary")
	}
	autopilotRevisionReport(t, f, run, req)
	autopilotRevisionReport(t, f, run, StateRequest{State: "running", ClaimGeneration: i64(1)})
	if acceptancePair(t, e, run) != before || acceptanceHistory(t, e, run) != history {
		t.Fatal("retry/heartbeat accepted new revision")
	}
	assertAutopilotApproval(t, e, run, false)
}

type autopilotFaultBeginner struct {
	TxBeginner
	point string
	hits  int
	plans int
}
type autopilotFaultTx struct {
	pgx.Tx
	owner *autopilotFaultBeginner
}

func (b *autopilotFaultBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &autopilotFaultTx{Tx: tx, owner: b}, nil
}
func (tx *autopilotFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	source := strings.HasPrefix(sql, "-- name: AcceptPolicyAutopilotRevisionSource :execrows")
	running := strings.HasPrefix(sql, "-- name: SetRunRunning :execrows")
	if source {
		tx.owner.hits++
	}
	if (source && tx.owner.point == "source_zero") || (running && tx.owner.point == "running_zero") {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	if (source && tx.owner.point == "source_error") || (running && tx.owner.point == "running_error") {
		return pgconn.CommandTag{}, errors.New("injected autopilot write failure")
	}
	tag, err := tx.Tx.Exec(ctx, sql, args...)
	if err == nil && strings.HasPrefix(sql, "-- name: SetRunAutopilotPlan :execrows") && tag.RowsAffected() == 1 {
		tx.owner.plans++
	}
	return tag, err
}
func (tx *autopilotFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx.owner.point == "pending_read" && strings.HasPrefix(sql, "-- name: ListPolicyStopRevisionInputs :many") {
		return nil, errors.New("injected autopilot pending read failure")
	}
	return tx.Tx.Query(ctx, sql, args...)
}
func (tx *autopilotFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.owner.point == "cross_check_read" && strings.HasPrefix(sql, "-- name: GetPolicyStopCrossCheck :one") {
		return revisionWriterFailureRow{err: errors.New("injected exact cross-check read failure")}
	}
	if tx.owner.point == "post_read" && strings.HasPrefix(sql, "-- name: GetRunByID :one") {
		return revisionWriterFailureRow{err: errors.New("injected persisted candidate read failure")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *autopilotFaultTx) Commit(ctx context.Context) error {
	if tx.owner.point == "commit" {
		_ = tx.Rollback(ctx)
		return errors.New("injected autopilot commit failure")
	}
	return tx.Tx.Commit(ctx)
}

func TestPolicyRevisionAutopilotUnknownRunningZeroLiveDB(t *testing.T) {
	e, f, run, req := autopilotRevisionFixture(t)
	if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
		Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: []byte("{}"),
	}}, i64(1)); err != nil {
		t.Fatal(err)
	}
	if len(mustRun(t, e, run).PolicyRevisionSource) != 0 {
		t.Fatal("unsupported revision did not invalidate coverage")
	}
	before := policyOwnerStopSnapshot(t, e, run)
	injection := &autopilotFaultBeginner{TxBeginner: e.pool, point: "running_zero"}
	f.svc.SetTxBeginner(injection)
	_, applied, _, err := f.svc.SetStateReport(e.ctx, f.wkr, run, req)
	if err != nil || applied || injection.plans != 1 || injection.hits != 0 {
		t.Fatalf("refusal applied=%v err=%v actual plan writes=%d source writes=%d", applied, err, injection.plans, injection.hits)
	}
	if policyOwnerStopSnapshot(t, e, run) != before {
		t.Fatal("uncertain-source refusal committed part of the plan/running pair")
	}
	f.svc.SetTxBeginner(e.pool)
	autopilotRevisionReport(t, f, run, req)
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	assertAutopilotApproval(t, e, run, false)
}

func TestPolicyRevisionAutopilotRollbackLiveDB(t *testing.T) {
	for _, point := range []string{"pending_read", "post_read", "running_error", "running_zero", "source_error", "source_zero", "commit"} {
		t.Run(point, func(t *testing.T) {
			e, f, run, req := autopilotRevisionFixture(t)
			e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
			before, history := acceptancePair(t, e, run), acceptanceHistory(t, e, run)
			injection := &autopilotFaultBeginner{TxBeginner: e.pool, point: point}
			f.svc.SetTxBeginner(injection)
			if _, _, _, err := f.svc.SetStateReport(e.ctx, f.wkr, run, req); err == nil {
				t.Fatal("injected failure succeeded")
			}
			r := mustRun(t, e, run)
			if r.PlanMd.Valid || r.Status != "claimed" || acceptancePair(t, e, run) != before || acceptanceHistory(t, e, run) != history {
				t.Fatal("failed report did not roll back plan, running, source pair and history")
			}
			wantHits := 0
			if strings.HasPrefix(point, "source_") || point == "commit" {
				wantHits = 1
			}
			if injection.hits != wantHits {
				t.Fatalf("source writes=%d want=%d", injection.hits, wantHits)
			}
			f.svc.SetTxBeginner(e.pool)
			autopilotRevisionReport(t, f, run, req)
			assertAutopilotApproval(t, e, run, true)
		})
	}
}

func TestPolicyRevisionAutopilotPendingAndInvalidCrossCheckLiveDB(t *testing.T) {
	for _, mode := range []string{"same_generation_claimed", "same_generation_running", "old_generation", "digest", "future_generation", "selection", "mismatched_approve", "source_read"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run, c := acceptanceUnpublishedFixture(t)
			cc, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, c, 1)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "digest" || mode == "future_generation" || mode == "selection" || mode == "mismatched_approve" {
				autopilotDecide(t, e, f, cc, "approve")
			}
			e.exec("UPDATE runs SET plan_cross_check_required=false,policy_refusal_count=1 WHERE id=$1", run)
			req := StateRequest{State: "running", ClaimGeneration: i64(1), PlanMd: strPtr("# Actual plan")}
			if mode == "same_generation_claimed" {
				// Prewrite claimed is no excuse to drop a pending verdict: running revives it.
				e.exec("UPDATE runs SET status='claimed' WHERE id=$1", run)
			}
			switch mode {
			case "old_generation":
				e.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", run)
				req.ClaimGeneration = i64(2)
			case "digest":
				e.exec("UPDATE cross_checks SET candidate_digest=decode('01','hex') WHERE id=$1", cc.ID)
			case "future_generation":
				e.exec("UPDATE cross_checks SET lead_claim_generation=2 WHERE id=$1", cc.ID)
			case "selection":
				source := acceptanceSource(t, e, run)
				source.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{}
				autopilotPersistSource(t, e, run, source)
			}
			before, history := acceptancePair(t, e, run), acceptanceHistory(t, e, run)
			if mode == "source_read" {
				injection := &autopilotFaultBeginner{TxBeginner: e.pool, point: "cross_check_read"}
				f.svc.SetTxBeginner(injection)
				if _, _, _, err := f.svc.SetStateReport(e.ctx, f.wkr, run, req); err == nil {
					t.Fatal("cross-check read failure succeeded")
				}
				if mustRun(t, e, run).PlanMd.Valid || acceptancePair(t, e, run) != before {
					t.Fatal("read failure committed plan/source")
				}
				return
			}
			autopilotRevisionReport(t, f, run, req)
			if acceptanceHistory(t, e, run) != history {
				t.Fatal("feedback history changed")
			}
			if mode == "old_generation" {
				got := acceptanceSource(t, e, run)
				if len(got.AssociatedCrossChecks) != 0 || mustRun(t, e, run).PolicyCandidateCrossCheckID.Valid {
					t.Fatal("generation-fenced pending selection survived")
				}
				assertAutopilotApproval(t, e, run, true)
			} else {
				if acceptancePair(t, e, run) != before {
					t.Fatal("unresolved or invalid feedback accepted")
				}
				assertAutopilotApproval(t, e, run, false)
			}
		})
	}
}

func TestPolicyRevisionAutopilotHoldResumeLiveDB(t *testing.T) {
	e, f, run, req := autopilotRevisionFixture(t)
	session := "policy-autopilot-session"
	req.SessionID = &session
	autopilotRevisionReport(t, f, run, req)
	hold := policyRequest()
	phase := "implementation"
	hold.Phase = &phase
	held, disposition, err := f.svc.ReportPolicyHold(e.ctx, f.wkr, run, hold)
	if err != nil || disposition != PolicyHoldApplied {
		t.Fatalf("hold=%s err=%v", disposition, err)
	}
	assertAutopilotApproval(t, e, run, true)
	resumed, err := f.svc.ResumePolicyHold(e.ctx, f.userID, run, PolicyHoldActionToken(held), PolicyResumeRequest{})
	if err != nil || resumed.Status != "queued" {
		t.Fatalf("resume status=%s err=%v", resumed.Status, err)
	}
	// Explicit claim-admission bridge: this test covers real hold/resume and the
	// next generation's reader/report, not full Claim admission/assembly or ACK.
	generation := f.reclaim(run)
	e.exec("UPDATE runs SET status='claimed' WHERE id=$1", run)
	req.ClaimGeneration = &generation
	before := acceptancePair(t, e, run)
	autopilotRevisionReport(t, f, run, req)
	assertAutopilotApproval(t, e, run, true)
	if acceptancePair(t, e, run) != before {
		t.Fatal("reclaimed retry rewrote source pair")
	}
}

// Keep JSON fixture construction checked without copying a second source schema.
func autopilotPersistSource(t *testing.T, e codexTestEnv, run uuid.UUID, source apitypes.PolicyRevisionSourceV1) {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", run, raw)
}
