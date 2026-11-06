package workersvc

import (
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3974 contract pin (server half). A lead run carries a pending credential-switch stamp and
// a pending plan cross-check round. The owner's cancel, with a live poller, is QUEUED (not applied
// server-side) and the worker only ever sees the switch signal: ConsumeInputs delivers nothing
// else while the stamp is pending, so the cancel row is withheld. The worker's terminal `failed`
// report then ends the run cancelled atomically (CancelRunByWorkerSettlingCancel), clears the
// stamp, settles the cancel row and, through the runs_settle_exited_plan_cross_check trigger,
// supersedes the pending round and cancels its child. The agent fix relies on all of this, so each
// piece is pinned here. Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

type withheldSwitch struct {
	crossCheckContentionFixture
	leadWorker store.Worker
	gen        int64
}

type steeringRow struct {
	ID          int64
	Kind        string
	Applied     bool
	Consumed    bool
	Disposition *string
}

// newWithheldSwitch builds the lane fixture, optionally claims the checker child first (a claim is
// refused while a switch is pending, so it must precede the stamp), gives the lead's worker a fresh
// heartbeat (the live poller) and optionally stamps the switch at the lead's current generation.
func newWithheldSwitch(t *testing.T, claimChild, stamp bool) withheldSwitch {
	t.Helper()
	f := laneFixture(t)
	if claimChild {
		f.claimed(t, laneClaim(t, f, "cross_check", nil))
	}
	lead := mustRun(t, f.env, f.lead)
	workerID := uuid.UUID(lead.WorkerID.Bytes)
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1`,
		workerID, []string{capability.CredentialSwitchV1})
	w := withheldSwitch{f, wkrRow(t, f.env, workerID), lead.ClaimGeneration}
	if stamp {
		w.stamp()
	}
	return w
}

func (w withheldSwitch) stamp() {
	w.env.exec(`UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=claim_generation
		WHERE id=$1`, w.lead)
}

func (w withheldSwitch) requirePreconditions(t *testing.T, beforeDeadline bool) {
	t.Helper()
	run := mustRun(t, w.env, w.lead)
	if run.Status != "running" || !run.WorkerID.Valid || run.ClaimReleasedAt.Valid {
		t.Fatalf("lead = status %q worker %v released %v, want a live running claim", run.Status,
			run.WorkerID.Valid, run.ClaimReleasedAt.Valid)
	}
	var fresh bool
	if err := w.env.pool.QueryRow(w.env.ctx, `SELECT last_heartbeat_at > now()-interval '30 seconds' FROM workers WHERE id=$1`,
		w.leadWorker.ID).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("lead worker heartbeat fresh=%v err=%v", fresh, err)
	}
	if pending, err := w.env.q.RunHasPendingOutcomeLease(w.env.ctx, w.lead); err != nil || pending {
		t.Fatalf("pending-outcome lease=%v err=%v, want none", pending, err)
	}
	if !run.CredentialSwitchRequestedAt.Valid || run.CredentialSwitchGeneration.Int64 != run.ClaimGeneration {
		t.Fatalf("switch stamp = (%v, %v), want pending at generation %d", run.CredentialSwitchRequestedAt,
			run.CredentialSwitchGeneration, run.ClaimGeneration)
	}
	var ok bool
	if err := w.env.pool.QueryRow(w.env.ctx, `SELECT verdict='pending' AND lead_claim_generation=$2 AND (deadline_at>now())=$3
		FROM cross_checks WHERE lead_run_id=$1 AND stage='plan' ORDER BY round DESC LIMIT 1`,
		w.lead, run.ClaimGeneration, beforeDeadline).Scan(&ok); err != nil || !ok {
		t.Fatalf("latest plan round pending at generation %d (before deadline=%v): ok=%v err=%v", run.ClaimGeneration, beforeDeadline, ok, err)
	}
}

// snapshot captures every row a refused or withheld operation must leave untouched.
func (w withheldSwitch) snapshot(t *testing.T) string {
	t.Helper()
	var s string
	if err := w.env.pool.QueryRow(w.env.ctx, `SELECT jsonb_build_object(
		'lead',(SELECT to_jsonb(r) FROM runs r WHERE r.id=$1),
		'child',(SELECT to_jsonb(r) FROM runs r WHERE r.id=$2),
		'checks',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.round) FROM cross_checks c WHERE c.lead_run_id=$1),
		'inputs',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM run_user_inputs i WHERE i.run_id=$1))::text`,
		w.lead, w.runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (w withheldSwitch) inputs(t *testing.T) []steeringRow {
	t.Helper()
	rows, err := w.env.pool.Query(w.env.ctx, `SELECT id,kind,applied_at IS NOT NULL,consumed_at IS NOT NULL,disposition
		FROM run_user_inputs WHERE run_id=$1 ORDER BY id`, w.lead)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []steeringRow
	for rows.Next() {
		var r steeringRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Applied, &r.Consumed, &r.Disposition); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// queueWithheldCancel queues an answer and a follow_up and then the owner cancel, which must be
// QUEUED (ServerSide=false) because the lead has a live poller. A real answer submit is refused
// unless the run is awaiting_input, so the answer row is inserted directly; the follow_up and the
// cancel go through SubmitInput. It returns the steering rows as they stand afterwards.
func (w withheldSwitch) queueWithheldCancel(t *testing.T) []steeringRow {
	t.Helper()
	w.env.exec(`INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'answer',$2)`, w.lead, `{"question_id":"q-1","answers":["a"]}`)
	if _, err := w.svc.SubmitInput(w.env.ctx, w.userID, w.lead, "follow_up", "steer", nil); err != nil {
		t.Fatalf("follow_up: %v", err)
	}
	res, err := w.svc.SubmitInput(w.env.ctx, w.userID, w.lead, "cancel", "owner cancel", nil)
	if err != nil || res.ServerSide {
		t.Fatalf("cancel = %+v, %v, want queued (ServerSide=false)", res, err)
	}
	rows := w.inputs(t)
	if len(rows) != 3 {
		t.Fatalf("inputs = %+v, want answer, follow_up, cancel", rows)
	}
	for _, r := range rows {
		if r.Applied || r.Consumed {
			t.Fatalf("input %+v already applied or consumed", r)
		}
	}
	return rows
}

func (w withheldSwitch) requireOwnership(t *testing.T, wantCancel bool) {
	t.Helper()
	status, _, gen, _, released, _, cancel, err := w.svc.RunOwnership(w.env.ctx, w.leadWorker, w.lead)
	if err != nil || status != "running" || gen != w.gen || released || cancel != wantCancel {
		t.Fatalf("RunOwnership = status %q gen %d released %v cancellation_requested %v err %v, want running gen %d cancel %v",
			status, gen, released, cancel, err, w.gen, wantCancel)
	}
}

// requireOnlySwitchDelivered polls as a legacy and as a receipt-capable worker: only the switch
// signal comes back, and nothing is drained, consumed or applied.
func (w withheldSwitch) requireOnlySwitchDelivered(t *testing.T) {
	t.Helper()
	before := w.snapshot(t)
	for _, caps := range [][]string{nil, {capability.InputReceiptsV1}} {
		worker := w.leadWorker
		worker.ProtocolCapabilities = caps
		got, err := w.svc.ConsumeInputs(w.env.ctx, worker, w.lead)
		if err != nil || got.CredentialSwitch == nil || len(got.Inputs) != 0 || got.TerminalCancelled != nil || got.Receipts {
			t.Fatalf("ConsumeInputs(caps=%v) = %+v, %v, want only the switch signal", caps, got, err)
		}
	}
	if w.snapshot(t) != before {
		t.Fatal("polling while the switch is pending changed state")
	}
}

func (w withheldSwitch) reportFailed(t *testing.T, gen int64) (store.Run, bool, error) {
	t.Helper()
	return w.svc.SetState(w.env.ctx, w.leadWorker, w.lead, StateRequest{State: "failed", ClaimGeneration: &gen})
}

// requireSettledCancelled applies the worker's failed report and pins the atomic outcome: lead
// cancelled with stop_kind cancelled and the stamp cleared, the cancel row applied, the steering
// rows exactly as they were, the pending round failed with reasonClass and interrupted_at, and the
// child cancelled with its claim released.
func (w withheldSwitch) requireSettledCancelled(t *testing.T, queued []steeringRow, reasonClass string) {
	t.Helper()
	run, applied, err := w.reportFailed(t, w.gen)
	if err != nil || !applied {
		t.Fatalf("failed report: applied=%v err=%v", applied, err)
	}
	if run.Status != "cancelled" || !run.StopKind.Valid || run.StopKind.String != "cancelled" ||
		run.CredentialSwitchRequestedAt.Valid || run.CredentialSwitchGeneration.Valid {
		t.Fatalf("lead = %q stop_kind %v stamp (%v, %v), want cancelled/cancelled with the stamp cleared", run.Status,
			run.StopKind, run.CredentialSwitchRequestedAt, run.CredentialSwitchGeneration)
	}
	got := w.inputs(t)
	for i, r := range got {
		want := queued[i]
		if r.Kind == "cancel" {
			want.Applied, want.Consumed = true, true
		}
		if !reflect.DeepEqual(r, want) {
			t.Fatalf("input %d = %+v, want %+v (only the cancel row is settled, applied and consumed)", i, r, want)
		}
	}
	var verdict, class string
	var interrupted bool
	if err := w.env.pool.QueryRow(w.env.ctx, `SELECT verdict,COALESCE(reason_class,''),interrupted_at IS NOT NULL FROM cross_checks
		WHERE lead_run_id=$1 AND stage='plan' ORDER BY round DESC LIMIT 1`, w.lead).Scan(&verdict, &class, &interrupted); err != nil {
		t.Fatal(err)
	}
	if verdict != "failed" || class != reasonClass || !interrupted {
		t.Fatalf("round = %q/%q interrupted=%v, want failed/%s interrupted", verdict, class, interrupted, reasonClass)
	}
	child := mustRun(t, w.env, w.runID)
	if child.Status != "cancelled" || !child.ClaimReleasedAt.Valid {
		t.Fatalf("child = %q released %v, want cancelled with the claim released", child.Status, child.ClaimReleasedAt.Valid)
	}
}

func (w withheldSwitch) checkerWorker(t *testing.T) store.Worker { return wkrRow(t, w.env, w.workerID) }

// requireRefusedAfterCancel: nothing a stale flight can still send changes the settled run.
func (w withheldSwitch) requireRefusedAfterCancel(t *testing.T) {
	t.Helper()
	findings := []byte(`{"summary":"ok","items":[]}`)
	c := w.candidate
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	g := w.gen
	milestones := []Milestone{}
	candidateReq := StateRequest{State: "running", ClaimGeneration: &g, PlanMd: &c.PlanMd, Milestones: &milestones,
		RequiredCapabilities: &c.RequiredCapabilities, RequiredTools: &c.RequiredTools, SizeClass: &c.SizeClass,
		CandidateDigest: hex.EncodeToString(digest)}
	presentation := uuid.New()
	approvalReq := candidateReq
	approvalReq.State, approvalReq.CandidateDigest, approvalReq.PresentationID = "awaiting_approval", "", &presentation
	before := w.snapshot(t)
	if _, err := w.svc.DecidePlanCrossCheck(w.env.ctx, w.checkerWorker(t), w.runID, 1, "approve", "approve", findings); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("DecidePlanCrossCheck after cancel: %v, want ErrCrossCheckRefused", err)
	}
	for name, req := range map[string]StateRequest{
		"credential_switch release": {State: "credential_switch", ClaimGeneration: &g},
		"credential_switch_failed":  {State: "credential_switch_failed", ClaimGeneration: &g},
		"running with candidate":    candidateReq,
		"awaiting_approval":         approvalReq,
	} {
		if _, applied, _ := w.svc.SetState(w.env.ctx, w.leadWorker, w.lead, req); applied {
			t.Fatalf("%s applied after the cancel", name)
		}
	}
	// A repeated identical failed report is idempotent or refused, but changes nothing either way.
	_, _, _ = w.reportFailed(t, w.gen)
	if w.snapshot(t) != before {
		t.Fatal("a refused or repeated report changed the settled state")
	}
}

// seedOpenHold gives the lead a published checkpoint and an open custody hold. inventory_guarded
// is set at INSERT: an UPDATE of that flag is silently suppressed by a guard trigger.
func (w withheldSwitch) seedOpenHold(t *testing.T) {
	t.Helper()
	lead := mustRun(t, w.env, w.lead)
	w.env.exec(`UPDATE runs SET checkpoint_tip=$2 WHERE id=$1`, w.lead, "3333333333333333333333333333333333333333")
	w.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,
		original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,$5,'open',$6,'ident',$6,$4,false)`, uuid.New(), w.userID, uuid.UUID(lead.RepoID.Bytes),
		w.lead, w.gen, w.leadWorker.ID)
}

// requireHoldRetained: the cancel leaves the hold open, and the terminal trigger recorded the
// checkpoint as retained. Retention is not wired to a forge in this harness, so recordCheckpointRetention
// (the record half, no forge call) is invoked directly and must not move the record out of retained.
func (w withheldSwitch) requireHoldRetained(t *testing.T) {
	t.Helper()
	state := func(sql string) string {
		var s string
		if err := w.env.pool.QueryRow(w.env.ctx, sql, w.lead).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	lead := mustRun(t, w.env, w.lead)
	w.svc.recordCheckpointRetention(w.env.ctx, w.lead, lead.Kind, lead.IssueIid)
	if h, r := state(`SELECT state FROM recovery_custody_holds WHERE run_id=$1`),
		state(`SELECT state FROM checkpoint_retentions WHERE run_id=$1`); h != "open" || r != "retained" {
		t.Fatalf("hold %q retention %q after the cancel, want open/retained", h, r)
	}
}

func TestWithheldSwitchCancelContractLiveDB(t *testing.T) {
	t.Run("claimed child settles atomically", func(t *testing.T) {
		w := newWithheldSwitch(t, true, true)
		w.seedOpenHold(t)
		w.requirePreconditions(t, true)
		queued := w.queueWithheldCancel(t)
		w.requireOnlySwitchDelivered(t)
		w.requireOwnership(t, true)
		w.requireSettledCancelled(t, queued, "superseded")
		got, err := w.svc.ConsumeInputs(w.env.ctx, w.checkerWorker(t), w.runID)
		if err != nil || got.TerminalCancelled == nil || got.TerminalCancelled.RunID != w.runID || got.TerminalCancelled.Generation != 1 {
			t.Fatalf("claimed child poll = %+v, %v, want TerminalCancelled at generation 1", got, err)
		}
		w.requireRefusedAfterCancel(t)
		w.requireHoldRetained(t)
	})

	t.Run("after the deadline the round is timed_out", func(t *testing.T) {
		w := newWithheldSwitch(t, true, true)
		w.env.exec(`UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1`, w.lead)
		w.requirePreconditions(t, false)
		queued := w.queueWithheldCancel(t)
		w.requireSettledCancelled(t, queued, "timed_out")
	})

	t.Run("unclaimed child is cancelled and never claimable", func(t *testing.T) {
		w := newWithheldSwitch(t, false, true)
		w.requirePreconditions(t, true)
		queued := w.queueWithheldCancel(t)
		w.requireSettledCancelled(t, queued, "superseded")
		if p := laneClaim(t, w.crossCheckContentionFixture, "cross_check", nil); p != nil {
			t.Fatal("cancelled child was claimed from the cross-check lane")
		}
		if p := w.claim(t, false, "", uuid.Nil); p != nil {
			t.Fatal("cancelled child was claimed from the run lane")
		}
	})

	t.Run("cancel before the stamp is withheld too", func(t *testing.T) {
		w := newWithheldSwitch(t, true, false)
		queued := w.queueWithheldCancel(t)
		w.requireOwnership(t, true)
		w.stamp()
		w.requirePreconditions(t, true)
		w.requireOnlySwitchDelivered(t)
		w.requireOwnership(t, true)
		w.requireSettledCancelled(t, queued, "superseded")
	})

	t.Run("mr rework cancel queues under a pending switch", func(t *testing.T) {
		w := newWithheldSwitch(t, false, true)
		lead := mustRun(t, w.env, w.lead)
		rework := uuid.New()
		w.env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_title,issue_description,status,status_since,worker_id,
			pipeline_ref,mr_iid,target_run_id,claim_generation,credential_switch_requested_at,credential_switch_generation)
			VALUES ($1,$2,$3,'mr_rework','t','d','running',now(),$4,'agent/issue-42',4242,$5,1,now(),1)`,
			rework, w.userID, uuid.UUID(lead.RepoID.Bytes), w.leadWorker.ID, w.lead)
		if err := w.svc.CancelReworkForMR(w.env.ctx, uuid.UUID(lead.RepoID.Bytes), 4242, "mr closed"); err != nil {
			t.Fatal(err)
		}
		var kind string
		var applied bool
		if err := w.env.pool.QueryRow(w.env.ctx, `SELECT kind,applied_at IS NOT NULL FROM run_user_inputs WHERE run_id=$1`, rework).Scan(&kind, &applied); err != nil ||
			kind != "cancel" || applied {
			t.Fatalf("rework cancel row = %q applied=%v err=%v, want a queued cancel", kind, applied, err)
		}
		status, _, _, _, _, _, cancel, err := w.svc.RunOwnership(w.env.ctx, w.leadWorker, rework)
		if err != nil || status != "running" || !cancel {
			t.Fatalf("rework ownership = %q cancel=%v err=%v, want running with cancellation_requested", status, cancel, err)
		}
		got, err := w.svc.ConsumeInputs(w.env.ctx, w.leadWorker, rework)
		if err != nil || got.CredentialSwitch == nil || len(got.Inputs) != 0 {
			t.Fatalf("rework poll = %+v, %v, want only the switch signal", got, err)
		}
	})

	t.Run("release winning first leaves every row unapplied", func(t *testing.T) {
		w := newWithheldSwitch(t, true, true)
		queued := w.queueWithheldCancel(t)
		g := w.gen
		if run, applied, err := w.svc.SetState(w.env.ctx, w.leadWorker, w.lead, StateRequest{State: "credential_switch", ClaimGeneration: &g}); err != nil || !applied ||
			run.Status != "queued" || !run.ClaimReleasedAt.Valid {
			t.Fatalf("release = %q applied=%v err=%v, want queued and released", run.Status, applied, err)
		}
		before := w.snapshot(t)
		if _, applied, _ := w.reportFailed(t, w.gen); applied {
			t.Fatal("old-generation failed report applied after the release")
		}
		if got := mustRun(t, w.env, w.lead); got.Status != "queued" || w.snapshot(t) != before {
			t.Fatalf("lead status %q or state changed by the refused failed report", got.Status)
		}
		if got := w.inputs(t); !reflect.DeepEqual(got, queued) {
			t.Fatalf("inputs = %+v, want all unapplied as queued %+v", got, queued)
		}
	})

	t.Run("stale generation failed report is declined", func(t *testing.T) {
		w := newWithheldSwitch(t, true, true)
		w.queueWithheldCancel(t)
		before := w.snapshot(t)
		if _, applied, err := w.reportFailed(t, w.gen+1); applied || !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale failed report applied=%v err=%v, want declined with ErrStaleClaim", applied, err)
		}
		if w.snapshot(t) != before {
			t.Fatal("stale failed report changed state")
		}
	})
}
