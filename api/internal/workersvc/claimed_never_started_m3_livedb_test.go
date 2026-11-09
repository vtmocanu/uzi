package workersvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859 m3: late writes after the unwitnessed park, commit outcomes of the witness writes
// and the charge, and the end-to-end bound on a refused-report loop. Everything runs through the
// real Service entry points against the live database.

// parkedFixtureColumns are the run columns a straggler must never move after the park: the
// release itself, the cause, and the six witness columns.
var parkedFixtureColumns = []string{
	"status", "recovery_wait_cause", "claim_released_at", "claim_generation", "worker_id",
	"released_worker_id", "released_worker_nonce", "requeue_count", "worker_recovery_evidence",
	"registration_evidence_generation", "registration_evidence_nonce", "registration_evidence_epoch",
	"snapshot_departed_generation", "snapshot_departed_nonce", "snapshot_departed_epoch",
}

func (f *cncFixture) pick(t *testing.T, run uuid.UUID, cols []string) map[string]string {
	t.Helper()
	row := f.rowJSON(t, run)
	out := make(map[string]string, len(cols))
	for _, c := range cols {
		v, ok := row[c]
		if !ok {
			t.Fatalf("runs has no column %q", c)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[c] = string(b)
	}
	return out
}

func assertPicked(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, b := range before {
		if after[k] != b {
			t.Fatalf("%s: column %s changed: %s -> %s", what, k, b, after[k])
		}
	}
}

// TestUnwitnessedParkLateWritesLiveDB: after the sweep parked a continuation unwitnessed, none of
// the stragglers the departed flight (or its worker) can still send revives the claim, overwrites
// the cause, or moves a witness column; every evidence write is refused.
func TestUnwitnessedParkLateWritesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	type write struct {
		do          func(f *cncFixture, run uuid.UUID) StateReportResult
		wantRefused bool // the report carries evidence, which must be ACKed "refused"
	}
	report := func(req func(f *cncFixture) StateRequest) func(f *cncFixture, run uuid.UUID) StateReportResult {
		return func(f *cncFixture, run uuid.UUID) StateReportResult {
			// The report may be answered with an error (stale claim) or a no-op; neither revives.
			res, _ := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, req(f))
			return res
		}
	}
	gen := func() *int64 { g := int64(2); return &g }
	snapshot := func(listed bool) func(f *cncFixture, run uuid.UUID) StateReportResult {
		return func(f *cncFixture, run uuid.UUID) StateReportResult {
			active := []ActiveRunEntry{}
			if listed {
				active = append(active, f.listEntry(run, 2))
			}
			e := f.next()
			if _, err := f.svc.Heartbeat(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, nil, nil,
				&ActiveSnapshot{SnapshotEpoch: e, RegisterNonce: f.nonce, Active: active}); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			if got := f.acceptedEpoch(t); got != e {
				t.Fatalf("snapshot at epoch %d was not accepted (worker epoch %d): the case would prove nothing", e, got)
			}
			return StateReportResult{}
		}
	}
	cases := map[string]write{
		"running report with evidence (same generation and nonce)": {wantRefused: true, do: report(func(f *cncFixture) StateRequest {
			e := f.next()
			return StateRequest{State: "running", ClaimGeneration: gen(),
				RegistrationEvidence: &RegistrationEvidence{RegisterNonce: f.nonce, ClaimGeneration: 2, RegistrationEpoch: &e}}
		})},
		"running report without evidence": {do: report(func(f *cncFixture) StateRequest {
			return StateRequest{State: "running", ClaimGeneration: gen()}
		})},
		"recovery_wait report from the worker": {do: report(func(f *cncFixture) StateRequest {
			return StateRequest{State: "recovery_wait", ClaimGeneration: gen()}
		})},
		"accepted snapshot listing the run at the parked generation": {do: snapshot(true)},
		"accepted snapshot omitting the run":                         {do: snapshot(false)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, run, _, _ := parkedUnwitnessed(t, env)
			before := f.pick(t, run, parkedFixtureColumns)
			if before["recovery_wait_cause"] != `"retained_continuation_unwitnessed"` || before["snapshot_departed_generation"] != "null" {
				t.Fatalf("precondition: %v", before)
			}
			res := c.do(f, run)
			if c.wantRefused && res.RegistrationEvidence != RegistrationEvidenceRefused {
				t.Fatalf("evidence ack = %q, want %q", res.RegistrationEvidence, RegistrationEvidenceRefused)
			}
			f.plainHeartbeat(t)
			assertPicked(t, "late write", before, f.pick(t, run, parkedFixtureColumns))
			r := f.run(t, run)
			if !r.ClaimReleasedAt.Valid || r.Status != "recovery_wait" {
				t.Fatalf("claim revived: status=%s released=%v", r.Status, r.ClaimReleasedAt.Valid)
			}
			f.assertUnwitnessedPark(t, run, 1, "after the late write")
			if sw := f.sweep(t); sw.ClaimedUnwitnessed != 0 {
				t.Fatalf("a later sweep re-parked the run: %+v", sw)
			}
			assertPicked(t, "later sweep", before, f.pick(t, run, parkedFixtureColumns))
		})
	}
}

// TestClaimedNeverStartedCommitOutcomesLiveDB: the witness writes and the charge are idempotent
// under replay and all-or-nothing under a rolled-back sweep transaction.
func TestClaimedNeverStartedCommitOutcomesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	witnessCols := []string{
		"registration_evidence_generation", "registration_evidence_nonce", "registration_evidence_epoch",
		"snapshot_departed_generation", "snapshot_departed_nonce", "snapshot_departed_epoch",
	}

	t.Run("an evidence report resent after the committed write leaves identical values", func(t *testing.T) {
		f := newCNCFixture(t, env, 5)
		run := f.seedGuardedRun(t)
		f.claimRun(t, run, 1)
		e := f.next()
		if res := f.evidenceReport(t, run, 1, f.nonce, e); res.RegistrationEvidence != RegistrationEvidenceRecorded {
			t.Fatalf("first report ack = %q, want recorded", res.RegistrationEvidence)
		}
		first := f.pick(t, run, witnessCols)
		if first["registration_evidence_epoch"] != fmt.Sprint(e) || first["registration_evidence_nonce"] != `"`+f.nonce+`"` {
			t.Fatalf("stored evidence = %v, want nonce %q epoch %d", first, f.nonce, e)
		}
		f.evidenceReport(t, run, 1, f.nonce, e)
		assertPicked(t, "resent evidence", first, f.pick(t, run, witnessCols))
		if r := f.run(t, run); r.Status != "claimed" {
			t.Fatalf("status = %s, want the guard-refused run still claimed", r.Status)
		}
	})

	t.Run("a same-epoch snapshot replayed after the committed apply is stale and stamps once", func(t *testing.T) {
		f := newCNCFixture(t, env, 5)
		run := f.seedGuardedRun(t)
		f.claimRun(t, run, 1)
		f.evidenceReport(t, run, 1, f.nonce, f.next())
		e2 := f.omit(t)
		if d := f.departure(t, run); !d.is(1, f.nonce, e2) {
			t.Fatalf("departure = %+v, want (1, nonce, %d)", d, e2)
		}
		stamped := f.pick(t, run, witnessCols)
		var xmin string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT xmin::text FROM runs WHERE id = $1`, run).Scan(&xmin); err != nil {
			t.Fatal(err)
		}
		if got := f.heartbeat(t, e2, f.nonce); got != e2 {
			t.Fatalf("worker epoch after the replay = %d, want it unchanged at %d (replay rejected as stale)", got, e2)
		}
		assertPicked(t, "replayed snapshot", stamped, f.pick(t, run, witnessCols))
		var xminAfter string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT xmin::text FROM runs WHERE id = $1`, run).Scan(&xminAfter); err != nil {
			t.Fatal(err)
		}
		if xminAfter != xmin {
			t.Fatalf("the replayed snapshot rewrote the run row (xmin %s -> %s): a second stamp", xmin, xminAfter)
		}
	})

	t.Run("a second sweep after a committed charge charges once", func(t *testing.T) {
		f := newCNCFixture(t, env, 5)
		run, _ := f.qualifying(t, 1)
		if sw := f.sweep(t); sw.ClaimedCharged < 1 {
			t.Fatalf("first sweep = %+v, want the charge", sw)
		}
		f.assertQueued(t, run, 2, "charged once")
		holds := f.openHolds(t)
		charged := f.pick(t, run, parkedFixtureColumns)
		f.sweep(t)
		f.assertQueued(t, run, 2, "second sweep")
		assertPicked(t, "second sweep", charged, f.pick(t, run, parkedFixtureColumns))
		if got := f.openHolds(t); got != holds {
			t.Fatalf("open holds %d -> %d across the second sweep", holds, got)
		}
	})

	t.Run("a sweep transaction rolled back before commit changes nothing and the next sweep charges", func(t *testing.T) {
		f := newCNCFixture(t, env, 5)
		run, _ := f.qualifying(t, 1)
		beforeRow, beforeCustody := f.rowJSON(t, run), f.custodyRows(t, run)
		tx, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		res, err := store.New(tx).SweepClaimedNeverStartedCohort(f.env.ctx, store.SweepClaimedNeverStartedCohortParams{
			Cutoff: pgconv.Time(now.Add(-time.Minute)), WitnessCutoff: pgconv.Time(now.Add(-2 * time.Minute)),
			WitnessWindowSeconds: 120, MaxRequeues: 5,
		})
		if err != nil {
			_ = tx.Rollback(f.env.ctx)
			t.Fatalf("sweep inside the outer tx: %v", err)
		}
		charged := false
		for _, r := range res.Charged {
			charged = charged || r.ID == run
		}
		if !charged {
			_ = tx.Rollback(f.env.ctx)
			t.Fatalf("the in-tx sweep did not charge the run (charged %d rows): the rollback below would prove nothing", len(res.Charged))
		}
		if err := tx.Rollback(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		if after := f.rowJSON(t, run); !sameJSON(beforeRow, after) {
			t.Fatalf("a rolled-back sweep changed the run:\n before %v\n after  %v", beforeRow, after)
		}
		afterCustody := f.custodyRows(t, run)
		for table, rows := range beforeCustody {
			if afterCustody[table] != rows {
				t.Fatalf("a rolled-back sweep changed %s", table)
			}
		}
		f.sweep(t)
		f.assertQueued(t, run, 2, "next real sweep")
	})
}

func sameJSON(a, b map[string]any) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

// TestClaimedNeverStartedRefusalLoopBoundedLiveDB is the end-to-end oracle for issue #3859: a
// retained-custody run whose first running report is refused on every claim is claimed again and
// again by nothing but the sweep's own resets. The loop is driven for more cycles than the budget
// allows with no fail-fast inside it: each cycle just tries to claim, and the assertions run on
// the totals afterwards, so a regression shows its counts instead of stopping at the first bad
// cycle. The body of every first running report is the shared wire fixture, decoded strictly into
// StateRequest with the run's generation, the worker nonce and a fresh epoch substituted.
func TestClaimedNeverStartedRefusalLoopBoundedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 3
	f := newCNCFixture(t, env, max, func(p *Params) { p.CustodyHoldLimit = &fakeCustodyLimit{n: 1000} })
	run := f.seedGuardedRun(t)
	holdsStart := f.openHolds(t)

	raw, err := os.ReadFile("../../../fixtures/worker-protocol/registration-evidence-running.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	report := func(gen, epoch int64) StateReportResult {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("fixture is not JSON: %v", err)
		}
		// Substitute VALUES only: the nested keys stay the fixture's, so a rename there reddens
		// the strict decode below.
		body["claim_generation"] = gen
		ev, ok := body["registration_evidence"].(map[string]any)
		if !ok {
			t.Fatalf("fixture registration_evidence is not an object: %T", body["registration_evidence"])
		}
		ev["register_nonce"] = f.nonce
		ev["claim_generation"] = gen
		ev["registration_epoch"] = epoch
		wire, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(wire))
		dec.DisallowUnknownFields() // the handler's strict decoder
		var req StateRequest
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("the fixture body does not decode into StateRequest: %v", err)
		}
		if got := req.RegistrationEvidence; got == nil || got.RegisterNonce != f.nonce || got.ClaimGeneration != gen || got.RegistrationEpoch == nil || *got.RegistrationEpoch != epoch {
			t.Fatalf("decoded registration_evidence = %+v, want nonce=%q generation=%d epoch=%d", got, f.nonce, gen, epoch)
		}
		res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, req)
		if err != nil {
			t.Fatalf("running report: %v", err)
		}
		return res
	}

	const cycles = max + 4
	claims := 0
	for i := 0; i < cycles; i++ {
		got, gen := f.claim(t)
		if got != run {
			break // not claimable: the loop is bounded
		}
		claims++
		f.backdate(run)
		res := report(gen, f.next())
		if res.Applied {
			t.Errorf("cycle %d: the guard-refused report was applied", i+1)
		}
		f.omit(t)
		f.sweep(t)
	}

	r := f.run(t, run)
	newHolds := f.openHolds(t) - holdsStart
	var predecessorOpen int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND generation = 1 AND state = 'open'`, run).Scan(&predecessorOpen); err != nil {
		t.Fatal(err)
	}
	t.Logf("refusal loop: cycles offered=%d claims=%d new open holds=%d final status=%s cause=%v requeue_count=%d predecessor hold open=%d",
		cycles, claims, newHolds, r.Status, r.RecoveryWaitCause.String, r.RequeueCount, predecessorOpen)

	// generation 1 is uncharged, max generations are charged, and one more claim is the one that parks.
	bound := int64(max + 2)
	if int64(claims) > bound {
		t.Errorf("claims = %d, want at most %d", claims, bound)
	}
	if newHolds > bound {
		t.Errorf("new open holds = %d, want at most %d", newHolds, bound)
	}
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" {
		t.Errorf("final status=%s cause=%v, want recovery_wait/worker_requeue_exhausted", r.Status, r.RecoveryWaitCause)
	}
	if predecessorOpen != 1 {
		t.Errorf("generation 1 hold open count = %d, want 1 (the predecessor stays open)", predecessorOpen)
	}
}
