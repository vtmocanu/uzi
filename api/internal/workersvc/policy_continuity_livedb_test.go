package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// settledPreserving seeds a running Codex run at generation gen that already carries a resolved
// same_conversation episode with R=a and the given verification record. Seeding the record directly
// isolates the SQL conjunct under test from the verification route (which has its own tests).
func (p policyLive) settledPreserving(t *testing.T, gen int64, vg *int64, vh string) uuid.UUID {
	t.Helper()
	id := p.running(t, gen)
	p.e.exec(t, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode='same_conversation',policy_applying_worker_id=$3,
		policy_applying_generation=$4,policy_source_generation=$4,policy_resolved_at=now(),policy_preserve_required=true,
		policy_required_head=$5,policy_recovery_verified_generation=$6,policy_recovery_verified_head=NULLIF($7,''),
		policy_hold_cause='refused' WHERE id=$1`, id, uuid.New(), p.wid, gen-1, hexHead("a"), vg, vh)
	return id
}

// releaseAndReclaim ends the current claim (a worker loss) and claims the run again through ClaimRun.
func (p policyLive) releaseAndReclaim(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	p.e.exec(t, "UPDATE runs SET status='queued',status_since=now(),worker_id=NULL,claim_released_at=now() WHERE id=$1", id)
	return p.reclaim(t, id)
}

// A verification whose reply was lost still committed for ITS generation only: a later claim
// generation must verify again before a published report may supersede R.
func TestPolicyLostVerificationReplyChainLiveDB(t *testing.T) {
	a, c := hexHead("a"), hexHead("c")
	setup := func(t *testing.T) (policyLive, uuid.UUID) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		first := p.mustHold(t, id, holdSpec{claim: 1, head: a, published: true, latest: true})
		if _, err := p.resume(id, PolicyResumePreserve, first.OutcomeID, ""); err != nil {
			t.Fatal(err)
		}
		p.reclaim(t, id) // generation 2
		// The reply to this record is discarded by the caller: only the committed row matters.
		if _, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, p.worker, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: 2, RequiredHead: a}); err != nil {
			t.Fatal(err)
		}
		if row := p.run(t, id); row.PolicyRecoveryVerifiedGeneration.Int64 != 2 || row.PolicyRequiredHead.String != a {
			t.Fatalf("lost-reply record did not commit: %s", facts(row))
		}
		if claimed := p.releaseAndReclaim(t, id); claimed.ClaimGeneration != 3 {
			t.Fatalf("expected generation 3, got %d", claimed.ClaimGeneration)
		}
		return p, id
	}
	t.Run("generation G+1 hold without its own verification keeps R", func(t *testing.T) {
		p, id := setup(t)
		p.mustHold(t, id, holdSpec{claim: 3, rehold: "session_missing", head: c, published: true, latest: true})
		row := p.run(t, id)
		if row.PolicyRequiredHead.String != a || row.PolicyRecoveryVerifiedGeneration.Int64 != 2 || row.PolicyRecoveryVerifiedHead.String != a {
			t.Fatalf("G+1 report superseded R on G's verification: %s", facts(row))
		}
	})
	t.Run("generation G+1 hold after its own verification supersedes R", func(t *testing.T) {
		p, id := setup(t)
		if got, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, p.worker, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: 3, RequiredHead: a}); err != nil || got.Disposition != "recorded" {
			t.Fatalf("G+1 verification: %+v %v", got, err)
		}
		p.mustHold(t, id, holdSpec{claim: 3, rehold: "session_missing", head: c, published: true, latest: true})
		row := p.run(t, id)
		if row.PolicyRequiredHead.String != c || row.PolicyRecoveryVerifiedHead.String != c || row.PolicyRecoveryVerifiedGeneration.Int64 != 3 {
			t.Fatalf("verified G+1 report must supersede: %s", facts(row))
		}
	})
}

// Every conjunct of the ApplyPolicyHold supersession rule has its own negative, beside one positive control.
func TestPolicyHoldSupersessionConjunctsLiveDB(t *testing.T) {
	a, b := hexHead("a"), hexHead("b")
	g := func(v int64) *int64 { return &v }
	type tc struct {
		name      string
		vg        *int64
		vh        string
		spec      holdSpec
		wantSuper bool
		mutate    string // SQL run against the seeded row before the report
		foreign   bool
	}
	pub := holdSpec{claim: 2, rehold: "session_missing", head: b, published: true, latest: true}
	with := func(f func(*holdSpec)) holdSpec { s := pub; f(&s); return s }
	cases := []tc{
		{name: "positive control", vg: g(2), vh: a, spec: pub, wantSuper: true},
		{name: "published=false", vg: g(2), vh: a, spec: with(func(s *holdSpec) { s.published, s.latest = false, false })},
		{name: "checkpoint_contains_latest=false", vg: g(2), vh: a, spec: with(func(s *holdSpec) { s.latest = false })},
		{name: "verified generation differs", vg: g(1), vh: a, spec: pub},
		{name: "no verification record", spec: pub},
		{name: "verified head differs from R", vg: g(2), vh: b, spec: pub},
		{name: "released claim", vg: g(2), vh: a, spec: pub, mutate: "UPDATE runs SET claim_released_at=now() WHERE id=$1"},
		{name: "foreign worker", vg: g(2), vh: a, spec: pub, foreign: true},
		{name: "stale generation report", vg: g(2), vh: a, spec: with(func(s *holdSpec) { s.claim = 1 })},
		{name: "headless re-hold", vg: g(2), vh: a, spec: with(func(s *holdSpec) { s.head, s.published, s.latest = "", false, false })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPolicyLive(t)
			id := p.settledPreserving(t, 2, c.vg, c.vh)
			if c.mutate != "" {
				p.e.exec(t, c.mutate, id)
			}
			spec := c.spec
			if c.foreign {
				fw, err := p.e.q.GetWorkerByID(p.e.ctx, p.e.seedWorker(t, p.caps))
				if err != nil {
					t.Fatal(err)
				}
				spec.as = &fw
			}
			got, _ := p.report(t, id, spec)
			row := p.run(t, id)
			if c.wantSuper {
				if got.Disposition != "applied" || row.PolicyRequiredHead.String != b || row.PolicyRecoveryVerifiedHead.String != b {
					t.Fatalf("positive control: %+v %s", got, facts(row))
				}
				return
			}
			if row.PolicyRequiredHead.String != a || row.PolicyRecoveryVerifiedHead.String != c.vh {
				t.Fatalf("R or verification changed (%s): %s", got.Disposition, facts(row))
			}
		})
	}
}

// Ordinary reports of a preserving run (running, status heartbeat) never move R or the verification record.
func TestPolicyOrdinaryReportsKeepRequiredHeadLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	a := hexHead("a")
	id := p.settledPreserving(t, 2, func() *int64 { v := int64(2); return &v }(), a)
	before := facts(p.run(t, id))
	gen, sid := int64(2), "same-session"
	if _, _, err := p.svc.SetState(p.e.ctx, p.worker, id, StateRequest{State: "running", ClaimGeneration: &gen, SessionID: &sid}); err != nil {
		t.Fatal(err)
	}
	if after := facts(p.run(t, id)); after != before {
		t.Fatalf("ordinary running report changed the recovery identity:\n%s\n%s", before, after)
	}
}
