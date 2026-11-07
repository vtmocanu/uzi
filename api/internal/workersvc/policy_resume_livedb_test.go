package workersvc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// policyLive wires a policy-hold capable Codex worker over the shared interlock fixture.
type policyLive struct {
	e      interlockLiveDB
	svc    *Service
	wid    uuid.UUID
	worker store.Worker
	caps   []string
}

func newPolicyLive(t *testing.T) policyLive {
	t.Helper()
	e := setupInterlockLiveDB(t)
	svc, _ := e.providerParkService(t, ppBound)
	caps := []string{capability.ProviderPolicyHoldV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
	wid := e.seedWorker(t, caps)
	worker, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	return policyLive{e: e, svc: svc, wid: wid, worker: worker, caps: caps}
}

// facts renders only the required-recovery columns, keeping failure output readable.
func facts(r store.Run) string {
	return "R=" + r.PolicyRequiredHead.String + " VG=" + strconv.FormatInt(r.PolicyRecoveryVerifiedGeneration.Int64, 10) +
		" VH=" + r.PolicyRecoveryVerifiedHead.String + " cause=" + r.PolicyHoldCause.String + " status=" + r.Status
}

func hexHead(c string) string { return strings.Repeat(c, 40) }

// guidanceHash is the audit's request identity, spelled out here rather than reusing production code.
func guidanceHash(g string) string {
	sum := sha256.Sum256([]byte(g))
	return hex.EncodeToString(sum[:])
}

// running seeds a Codex run running at generation gen with a saved session.
func (p policyLive) running(t *testing.T, gen int64) uuid.UUID {
	t.Helper()
	id := p.e.fpSeedRunning(t, p.wid, gen)
	p.e.exec(t, "UPDATE runs SET harness='codex',session_id='original-session',started_at=now() WHERE id=$1", id)
	return id
}

type holdSpec struct {
	mode      string // continuation mode; default same_conversation
	head      string
	published bool
	latest    bool
	rehold    string // non-empty: a resume_rejected re-hold with this cause
	source    int64  // default: claim generation
	claim     int64
	as        *store.Worker // report as this worker instead of the claim holder
}

// report sends one policy-hold report for run id at generation spec.claim.
func (p policyLive) report(t *testing.T, id uuid.UUID, spec holdSpec) (PolicyHoldReply, PolicyHoldRequest) {
	t.Helper()
	req := policyHoldFixture()
	req.OutcomeID = uuid.NewString()
	req.ClaimGeneration = spec.claim
	req.SourceGeneration = spec.claim
	if spec.source != 0 {
		req.SourceGeneration = spec.source
	}
	if spec.mode != "" {
		req.ContinuationMode = spec.mode
	}
	if spec.rehold != "" {
		req.Detail, req.Cause = "resume_rejected", spec.rehold
		req.PolicyTag, req.Phase, req.CorrelationID = nil, nil, nil
	}
	if spec.head != "" {
		h := spec.head
		req.Head = &h
		req.Published = spec.published
		if spec.latest {
			v := true
			req.CheckpointContainsLatest = &v
		}
	}
	reporter := p.worker
	if spec.as != nil {
		reporter = *spec.as
	}
	got, err := p.svc.ReportPolicyHold(p.e.ctx, reporter, id, req)
	if err != nil {
		t.Fatalf("policy hold report: %v", err)
	}
	return got, req
}

func (p policyLive) mustHold(t *testing.T, id uuid.UUID, spec holdSpec) PolicyHoldRequest {
	t.Helper()
	got, req := p.report(t, id, spec)
	if got.Disposition != "applied" {
		t.Fatalf("hold not applied: %+v", got)
	}
	return req
}

// reclaim claims the queued run through the real ClaimRun and returns the claimed row.
func (p policyLive) reclaim(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	run, err := p.e.q.ClaimRun(p.e.ctx, p.e.claimParams(p.wid, p.caps, false))
	if err != nil || run.ID != id {
		t.Fatalf("claim %s: got %v err=%v", id, run.ID, err)
	}
	return run
}

func (p policyLive) run(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	r, err := p.e.q.GetRunByID(p.e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// inputs lists the run's user inputs as "kind:body" in id order.
func (p policyLive) inputs(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	rows, err := p.e.pool.Query(p.e.ctx, "SELECT kind||':'||COALESCE(body,'') FROM run_user_inputs WHERE run_id=$1 ORDER BY id", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// untouched asserts a refused operation left the run row and its inputs byte-identical.
func (p policyLive) untouched(t *testing.T, id uuid.UUID, what string, op func()) {
	t.Helper()
	beforeRun, beforeInputs := p.run(t, id), p.inputs(t, id)
	op()
	if !reflect.DeepEqual(beforeRun, p.run(t, id)) || !reflect.DeepEqual(beforeInputs, p.inputs(t, id)) {
		t.Fatalf("%s mutated the run or its inputs", what)
	}
}

func (p policyLive) resume(id uuid.UUID, mode string, episode string, guidance string) (PolicyResumeResult, error) {
	return p.svc.ResumePolicyHold(p.e.ctx, p.e.userID, id, PolicyResumeRequest{Mode: mode, ExpectedEpisodeID: uuid.MustParse(episode), Guidance: guidance})
}

func TestPolicyResumeHoldLiveDB(t *testing.T) {
	t.Run("fresh_from_saved_work resolves the episode and ends the constraint", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		req := p.mustHold(t, id, holdSpec{claim: 1, mode: "fresh_from_saved_work", head: hexHead("a"), published: true, latest: true})
		held := p.run(t, id)
		if held.PolicyRequiredHead.String != hexHead("a") || held.SessionID.String != "original-session" {
			t.Fatalf("hold lost required head or session: %s", facts(held))
		}
		res, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, "narrow the scope")
		if err != nil || res.Replay {
			t.Fatalf("resume: replay=%v %v", res.Replay, err)
		}
		got := p.run(t, id)
		if got.Status != "queued" || got.HoldReason.Valid || got.SessionID.Valid || got.PolicyPreserveRequired || got.PolicyPendingFresh ||
			!got.PolicyResolvedAt.Valid || got.PolicyRequiredHead.String != hexHead("a") || got.PolicyRecoveryVerifiedGeneration.Valid || got.PolicyRecoveryVerifiedHead.Valid {
			t.Fatalf("fresh resume must keep the required head and clear the verification pair: %s", facts(got))
		}
		if uuid.UUID(got.PolicyOutcomeID.Bytes).String() != req.OutcomeID || got.PolicyContinuationMode.String != "fresh_from_saved_work" ||
			!got.PolicyApplyingWorkerID.Valid || got.PolicyApplyingGeneration.Int64 != 1 || got.PolicyRefusalCount != 1 || got.ClaimReleasedAt.Valid || !got.WorkerID.Valid {
			t.Fatalf("fresh resume erased reconciliation facts: %s", facts(got))
		}
		in := p.inputs(t, id)
		wantAudit := `resume:{"policy_mode":"fresh_from_saved_work","episode_id":"` + req.OutcomeID + `","guidance_sha256":"` + guidanceHash("narrow the scope") + `","prior_session_id":"original-session"}`
		if len(in) != 2 || in[0] != "follow_up:narrow the scope" || in[1] != wantAudit {
			t.Fatalf("inputs %q, want guidance follow_up then %s", in, wantAudit)
		}
		// Lost-ACK worker replay: nothing reopened, duplicated or rewritten.
		before, beforeInputs := p.run(t, id), p.inputs(t, id)
		again, err := p.svc.ReportPolicyHold(p.e.ctx, p.worker, id, req)
		if err != nil || again.Disposition != "already_applied" {
			t.Fatalf("replay after resume: %+v %v", again, err)
		}
		if !reflect.DeepEqual(before, p.run(t, id)) || !reflect.DeepEqual(beforeInputs, p.inputs(t, id)) {
			t.Fatal("replay after resume mutated the run")
		}
	})

	t.Run("preserve keeps session, constraint and required head", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		req := p.mustHold(t, id, holdSpec{claim: 1, head: hexHead("b"), published: true, latest: true})
		if _, err := p.resume(id, PolicyResumePreserve, req.OutcomeID, ""); err != nil {
			t.Fatal(err)
		}
		got := p.run(t, id)
		if got.Status != "queued" || got.HoldReason.Valid || got.SessionID.String != "original-session" || !got.PolicyPreserveRequired ||
			!got.PolicyResolvedAt.Valid || got.PolicyRequiredHead.String != hexHead("b") {
			t.Fatalf("preserve resume: %s", facts(got))
		}
		if in := p.inputs(t, id); len(in) != 1 || !strings.HasPrefix(in[0], "resume:") {
			t.Fatalf("empty guidance must store only the audit row: %q", in)
		}
	})

	t.Run("refusals are zero mutation", func(t *testing.T) {
		for _, lane := range []struct{ held, wrong string }{
			{"same_conversation", PolicyResumeFreshFromSavedWork},
			{"fresh_from_saved_work", PolicyResumePreserve},
		} {
			p := newPolicyLive(t)
			id := p.running(t, 1)
			req := p.mustHold(t, id, holdSpec{claim: 1, mode: lane.held})
			p.untouched(t, id, "cross-mode "+lane.wrong, func() {
				if _, err := p.resume(id, lane.wrong, req.OutcomeID, "guidance"); !errors.Is(err, ErrPolicyResumeConflict) {
					t.Fatalf("cross-mode %s on %s: %v", lane.wrong, lane.held, err)
				}
			})
			if lane.held == "fresh_from_saved_work" {
				// The one-claim Fresh belongs to a same_conversation episode only.
				p.untouched(t, id, "fresh_one_claim on an attempt-path episode", func() {
					if _, err := p.resume(id, PolicyResumeFreshOneClaim, req.OutcomeID, "guidance"); !errors.Is(err, ErrPolicyResumeConflict) {
						t.Fatalf("fresh_one_claim: %v", err)
					}
				})
			}
			p.untouched(t, id, "stale episode id", func() {
				if _, err := p.resume(id, lane.wrong, uuid.NewString(), ""); !errors.Is(err, ErrPolicyResumeConflict) {
					t.Fatalf("stale id: %v", err)
				}
			})
			right := PolicyResumePreserve
			if lane.held == "fresh_from_saved_work" {
				right = PolicyResumeFreshFromSavedWork
			}
			p.untouched(t, id, "stale episode id, right mode", func() {
				if _, err := p.resume(id, right, uuid.NewString(), "g"); !errors.Is(err, ErrPolicyResumeConflict) {
					t.Fatalf("stale id right mode: %v", err)
				}
			})
			for name, bad := range map[string]PolicyResumeRequest{
				"unknown mode":    {Mode: "fresh", ExpectedEpisodeID: uuid.MustParse(req.OutcomeID)},
				"nil episode":     {Mode: right},
				"NUL guidance":    {Mode: right, ExpectedEpisodeID: uuid.MustParse(req.OutcomeID), Guidance: "a\x00b"},
				"bad UTF-8":       {Mode: right, ExpectedEpisodeID: uuid.MustParse(req.OutcomeID), Guidance: string([]byte{0xff, 0xfe})},
				"oversize":        {Mode: right, ExpectedEpisodeID: uuid.MustParse(req.OutcomeID), Guidance: strings.Repeat("x", 16001)},
				"empty mode":      {ExpectedEpisodeID: uuid.MustParse(req.OutcomeID)},
				"uppercase alias": {Mode: "PRESERVE", ExpectedEpisodeID: uuid.MustParse(req.OutcomeID)},
			} {
				p.untouched(t, id, name, func() {
					if _, err := p.svc.ResumePolicyHold(p.e.ctx, p.e.userID, id, bad); !errors.Is(err, ErrInvalidState) {
						t.Fatalf("%s: %v", name, err)
					}
				})
			}
			// Guidance at the byte limit is accepted on the right mode.
			if _, err := p.resume(id, right, req.OutcomeID, strings.Repeat("y", 16000)); err != nil {
				t.Fatalf("limit guidance: %v", err)
			}
		}
	})

	t.Run("foreign owner and non-held runs", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		req := p.mustHold(t, id, holdSpec{claim: 1})
		p.untouched(t, id, "foreign owner", func() {
			_, err := p.svc.ResumePolicyHold(p.e.ctx, uuid.New(), id, PolicyResumeRequest{Mode: PolicyResumePreserve, ExpectedEpisodeID: uuid.MustParse(req.OutcomeID)})
			if !errors.Is(err, ErrRunNotFound) {
				t.Fatalf("foreign: %v", err)
			}
		})
		other := p.running(t, 1)
		p.untouched(t, other, "ordinary running run", func() {
			if _, err := p.resume(other, PolicyResumePreserve, req.OutcomeID, ""); !errors.Is(err, ErrPolicyResumeConflict) {
				t.Fatalf("running run: %v", err)
			}
		})
		p.e.exec(t, "UPDATE runs SET stop_kind='stopped' WHERE id=$1", id)
		p.untouched(t, id, "pending stop", func() {
			if _, err := p.resume(id, PolicyResumePreserve, req.OutcomeID, ""); !errors.Is(err, ErrPolicyResumeConflict) {
				t.Fatalf("stop pending: %v", err)
			}
		})
	})

	t.Run("lost owner response retries idempotently", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		req := p.mustHold(t, id, holdSpec{claim: 1, mode: "fresh_from_saved_work"})
		if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, "once"); err != nil {
			t.Fatal(err)
		}
		p.untouched(t, id, "retry with identical body", func() {
			res, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, "once")
			if err != nil || !res.Replay || res.Run.Status != "queued" {
				t.Fatalf("retry: replay=%v %v", res.Replay, err)
			}
		})
		const mismatchMsg = "different guidance"
		p.untouched(t, id, "retry with different guidance", func() {
			res, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, "twice")
			if !errors.Is(err, ErrPolicyResumeGuidanceMismatch) || res.Replay || !strings.Contains(err.Error(), mismatchMsg) {
				t.Fatalf("retry guidance: replay=%v %v", res.Replay, err)
			}
		})
		p.untouched(t, id, "retry with empty guidance after non-empty", func() {
			if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, ""); !errors.Is(err, ErrPolicyResumeGuidanceMismatch) {
				t.Fatalf("empty vs non-empty: %v", err)
			}
		})
		if in := p.inputs(t, id); len(in) != 2 || in[0] != "follow_up:once" {
			t.Fatalf("exactly one follow_up with the original text expected: %q", in)
		}
		// A mode that was never recorded for the resolved episode is a safe conflict, not a replay.
		p.untouched(t, id, "different mode after resolution", func() {
			if _, err := p.resume(id, PolicyResumePreserve, req.OutcomeID, ""); !errors.Is(err, ErrPolicyResumeConflict) {
				t.Fatalf("different mode: %v", err)
			}
		})
	})

	t.Run("a new refusal re-holds and the old episode is refused", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		first := p.mustHold(t, id, holdSpec{claim: 1, mode: "fresh_from_saved_work", head: hexHead("a"), published: true, latest: true})
		if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, first.OutcomeID, ""); err != nil {
			t.Fatal(err)
		}
		claimed := p.reclaim(t, id)
		// Ordinary later claim after fresh_from_saved_work: no preserving constraint, no required head.
		if claimed.PolicyPreserveRequired || policyExpectedHead(claimed) != nil || claimed.SessionID.Valid || claimed.ClaimGeneration != 2 {
			t.Fatalf("later claim carries a stale constraint: %s", facts(claimed))
		}
		second := p.mustHold(t, id, holdSpec{claim: 2, mode: "fresh_from_saved_work", head: hexHead("c"), published: true, latest: true})
		if second.OutcomeID == first.OutcomeID {
			t.Fatal("fixture reused the outcome id")
		}
		p.untouched(t, id, "old episode replay", func() {
			if res, err := p.resume(id, PolicyResumeFreshFromSavedWork, first.OutcomeID, ""); err == nil {
				t.Fatalf("old episode accepted against a new hold: %+v", res)
			} else if !errors.Is(err, ErrPolicyResumeConflict) {
				t.Fatalf("old episode: %v", err)
			}
		})
		if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, second.OutcomeID, ""); err != nil {
			t.Fatalf("new episode: %v", err)
		}
	})
}

func TestPolicyRequiredRecoveryIdentityLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	a, b, c, d := hexHead("a"), hexHead("b"), hexHead("c"), hexHead("d")
	id := p.running(t, 1)
	first := p.mustHold(t, id, holdSpec{claim: 1, head: a, published: true, latest: true})
	row := p.run(t, id)
	if row.PolicyRequiredHead.String != a || row.PolicyRecoveryVerifiedGeneration.Valid || row.PolicyHoldCause.String != "refused" {
		t.Fatalf("initial set: %s", facts(row))
	}
	resumePreserve := func(outcome string) {
		t.Helper()
		if _, err := p.resume(id, PolicyResumePreserve, outcome, ""); err != nil {
			t.Fatal(err)
		}
	}
	resumePreserve(first.OutcomeID)

	// Generation 2: a published-latest re-hold BEFORE verification must not replace R.
	claimed := p.reclaim(t, id)
	if p := policyExpectedHead(claimed); p == nil || *p != a || !claimed.PolicyPreserveRequired {
		t.Fatalf("next claim must carry R=%s: %+v", a, claimed.PolicyRequiredHead)
	}
	second := p.mustHold(t, id, holdSpec{claim: 2, rehold: "session_missing", head: b, published: true, latest: true})
	row = p.run(t, id)
	if row.PolicyRequiredHead.String != a || row.PolicyHoldCause.String != "session_missing" {
		t.Fatalf("pre-verification publication replaced R or kept a stale cause: %s", facts(row))
	}
	resumePreserve(second.OutcomeID)

	// Generation 3: verification, then a published-latest report supersedes R -> H.
	p.reclaim(t, id)
	rec, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, p.worker, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: 3, RequiredHead: a})
	if err != nil || rec.Disposition != "recorded" {
		t.Fatalf("verification: %+v %v", rec, err)
	}
	row = p.run(t, id)
	if row.PolicyRequiredHead.String != a || row.PolicyRecoveryVerifiedGeneration.Int64 != 3 || row.PolicyRecoveryVerifiedHead.String != a {
		t.Fatalf("verification must record without touching R: %s", facts(row))
	}
	// Without latest-checkpoint evidence the same generation still does not supersede.
	third := p.mustHold(t, id, holdSpec{claim: 3, rehold: "provider_resume_rejected", head: c, published: true, latest: false})
	if row = p.run(t, id); row.PolicyRequiredHead.String != a || row.PolicyRecoveryVerifiedHead.String != a {
		t.Fatalf("unpublished-latest report superseded R: %s", facts(row))
	}
	resumePreserve(third.OutcomeID)
	p.reclaim(t, id) // generation 4: the verification record names generation 3, so it is stale.
	if rec, err = p.svc.RecordPolicyRecoveryVerified(p.e.ctx, p.worker, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: 4, RequiredHead: a}); err != nil || rec.Disposition != "recorded" {
		t.Fatalf("generation 4 verification: %+v %v", rec, err)
	}
	fourth := p.mustHold(t, id, holdSpec{claim: 4, rehold: "session_missing", head: d, published: true, latest: true})
	row = p.run(t, id)
	if row.PolicyRequiredHead.String != d || row.PolicyRecoveryVerifiedHead.String != d || row.PolicyRecoveryVerifiedGeneration.Int64 != 4 {
		t.Fatalf("verified same-generation published report must supersede R and VH: %s", facts(row))
	}
	resumePreserve(fourth.OutcomeID)
	claimed = p.reclaim(t, id) // generation 5, VG=4 is stale for it
	if p := policyExpectedHead(claimed); p == nil || *p != d {
		t.Fatalf("next claim must carry the superseded head: %+v", claimed)
	}
	fifth := p.mustHold(t, id, holdSpec{claim: 5, rehold: "session_missing", head: b, published: true, latest: true})
	if row = p.run(t, id); row.PolicyRequiredHead.String != d {
		t.Fatalf("a previous claim's verification superseded R: %s", facts(row))
	}
	_ = fifth
}

func TestPolicyRecoveryVerifiedRefusalsLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	a := hexHead("a")
	id := p.running(t, 1)
	first := p.mustHold(t, id, holdSpec{claim: 1, head: a, published: true, latest: true})
	if _, err := p.resume(id, PolicyResumePreserve, first.OutcomeID, ""); err != nil {
		t.Fatal(err)
	}
	p.reclaim(t, id) // generation 2, claimed
	ask := func(w store.Worker, gen int64, head string) PolicyRecoveryVerifiedReply {
		t.Helper()
		got, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, w, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: gen, RequiredHead: head})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	expect := func(what string, got PolicyRecoveryVerifiedReply, disposition, reason string) {
		t.Helper()
		if got.Disposition != disposition || got.Reason != reason {
			t.Fatalf("%s: %+v, want %s/%s", what, got, disposition, reason)
		}
		if row := p.run(t, id); row.PolicyRecoveryVerifiedGeneration.Valid || row.PolicyRecoveryVerifiedHead.Valid {
			t.Fatalf("%s wrote a verification record: %+v", what, row)
		}
	}
	expect("stale generation", ask(p.worker, 1, a), "pending", "stale_claim")
	expect("future generation", ask(p.worker, 3, a), "pending", "stale_claim")
	expect("mismatched head", ask(p.worker, 2, hexHead("f")), "refused", "head_mismatch")
	foreign := p.e.seedWorker(t, p.caps)
	foreignWorker, _ := p.e.q.GetWorkerByID(p.e.ctx, foreign)
	expect("foreign worker", ask(foreignWorker, 2, a), "pending", "ownership_unconfirmed")
	incapable := p.e.seedWorker(t, nil)
	incapableWorker, _ := p.e.q.GetWorkerByID(p.e.ctx, incapable)
	expect("incapable worker", ask(incapableWorker, 2, a), "pending", "worker_not_capable")
	for _, bad := range []PolicyRecoveryVerifiedRequest{{ClaimGeneration: 0, RequiredHead: a}, {ClaimGeneration: 2, RequiredHead: "ABC"}, {ClaimGeneration: 2, RequiredHead: strings.ToUpper(a)}, {ClaimGeneration: policyMaxSafeGeneration + 1, RequiredHead: a}} {
		if _, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, p.worker, id, bad); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid request %+v accepted: %v", bad, err)
		}
	}
	p.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
	expect("released claim", ask(p.worker, 2, a), "pending", "claim_released")
	p.e.exec(t, "UPDATE runs SET claim_released_at=NULL,status='paused' WHERE id=$1", id)
	expect("ineligible status", ask(p.worker, 2, a), "pending", "ineligible_status")
	p.e.exec(t, "UPDATE runs SET status='running' WHERE id=$1", id)
	if got := ask(p.worker, 2, a); got.Disposition != "recorded" {
		t.Fatalf("valid verification: %+v", got)
	}
	// Idempotent re-record of the same values.
	if got := ask(p.worker, 2, a); got.Disposition != "recorded" {
		t.Fatalf("re-record: %+v", got)
	}
}

// parkEvidence is the SQL-level supersede check shared by the wall and iteration park writers.
func TestPolicyParkWritersSupersedeOnlyWithVerifiedEvidenceLiveDB(t *testing.T) {
	a, b := hexHead("a"), hexHead("b")
	type attempt struct {
		name                        string
		vg                          *int64 // verification generation; nil = none
		vh                          string
		published, latest, preserve bool
		head                        string
		wantSuper                   bool
	}
	g := func(v int64) *int64 { return &v }
	cases := []attempt{
		{name: "verified published latest", vg: g(2), vh: a, published: true, latest: true, preserve: true, head: b, wantSuper: true},
		{name: "no verification record", published: true, latest: true, preserve: true, head: b},
		{name: "stale generation", vg: g(1), vh: a, published: true, latest: true, preserve: true, head: b},
		{name: "verified a different head", vg: g(2), vh: b, published: true, latest: true, preserve: true, head: b},
		{name: "not published", vg: g(2), vh: a, latest: true, preserve: true, head: b},
		{name: "checkpoint lacks latest", vg: g(2), vh: a, published: true, preserve: true, head: b},
		{name: "empty head", vg: g(2), vh: a, published: true, latest: true, preserve: true},
		{name: "non-hex head", vg: g(2), vh: a, published: true, latest: true, preserve: true, head: "not-a-sha"},
		{name: "unconstrained run", vg: g(2), vh: a, published: true, latest: true, head: b},
	}
	for _, writer := range []string{"wall park", "iteration park"} {
		for _, tc := range cases {
			t.Run(writer+"/"+tc.name, func(t *testing.T) {
				p := newPolicyLive(t)
				id := p.running(t, 2)
				p.e.exec(t, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode='same_conversation',policy_applying_worker_id=$3,
					policy_applying_generation=1,policy_source_generation=1,policy_resolved_at=now(),policy_preserve_required=$4,
					policy_required_head=$5,policy_recovery_verified_generation=$6,policy_recovery_verified_head=NULLIF($7,''),
					started_at=now()-interval '48 hours' WHERE id=$1`,
					id, uuid.New(), p.wid, tc.preserve, a, tc.vg, tc.vh)
				published, latest := tc.published, pgconv.BoolPtr(&tc.latest)
				gen := int64(2)
				switch writer {
				case "wall park":
					_, err := p.e.q.SetRunWallPark(p.e.ctx, store.SetRunWallParkParams{
						ID: id, WorkerID: pgconv.UUID(p.wid), HoldCapturedHead: pgconv.TextOrNull(tc.head),
						Now: pgconv.Time(p.svc.now()), GlobalTimeoutSeconds: 3600, ClaimGeneration: pgconv.Int8Ptr(&gen),
						Published: published, CheckpointContainsLatest: latest,
					})
					if err != nil {
						t.Fatal(err)
					}
				default:
					p.e.exec(t, "UPDATE runs SET iteration_count=5 WHERE id=$1", id)
					n, err := p.e.q.SetRunIterationPark(p.e.ctx, store.SetRunIterationParkParams{
						ID: id, WorkerID: pgconv.UUID(p.wid), ClaimGeneration: gen, HoldCapturedHead: pgconv.TextOrNull(tc.head),
						GlobalMaxIterations: 5, MaxIterations: 5, IterationCount: 5, Published: published, CheckpointContainsLatest: latest,
					})
					if err != nil || n != 1 {
						t.Fatalf("iteration park: %d %v", n, err)
					}
				}
				row := p.run(t, id)
				if row.Status != "paused" {
					t.Fatalf("park did not apply: %s", row.Status)
				}
				want, wantVH := a, tc.vh
				if tc.wantSuper {
					want, wantVH = b, b
				}
				if row.PolicyRequiredHead.String != want || row.PolicyRecoveryVerifiedHead.String != wantVH {
					t.Fatalf("R=%q VH=%q, want R=%q VH=%q", row.PolicyRequiredHead.String, row.PolicyRecoveryVerifiedHead.String, want, wantVH)
				}
				if row.PolicyPreserveRequired != tc.preserve || !row.PolicyResolvedAt.Valid {
					t.Fatalf("park rewrote the constraint or the resolved episode: %+v", row)
				}
			})
		}
	}
}

// savedWorkRun drives a real hold -> fresh_from_saved_work resume and returns the run, queued, with the
// required head kept and not preserving.
func (p policyLive) savedWorkRun(t *testing.T, head string) uuid.UUID {
	t.Helper()
	id := p.running(t, 1)
	req := p.mustHold(t, id, holdSpec{claim: 1, mode: "fresh_from_saved_work", head: head, published: true, latest: true})
	if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPolicySavedWorkHeadLiveDB(t *testing.T) {
	a := hexHead("a")
	verify := func(p policyLive, w store.Worker, id uuid.UUID, gen int64, head string) PolicyRecoveryVerifiedReply {
		t.Helper()
		got, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, w, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: gen, RequiredHead: head})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("claim carries the saved-work head until verification at the current generation clears it", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.savedWorkRun(t, a)
		claimed := p.reclaim(t, id)
		if got := policySavedWorkHead(claimed); got == nil || *got != a || policyExpectedHead(claimed) != nil || claimed.PolicyPreserveRequired {
			t.Fatalf("claim must carry only the saved-work head: %s", facts(claimed))
		}
		payload, _ := json.Marshal(ClaimPayload{PolicyPreserveOnly: claimed.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(claimed), PolicySavedWorkHead: policySavedWorkHead(claimed)})
		if !strings.Contains(string(payload), `"policy_saved_work_head":"`+a+`"`) {
			t.Fatalf("payload %s", payload)
		}
		if strings.Contains(string(payload), "policy_preserve_only") || strings.Contains(string(payload), "policy_expected_head") {
			t.Fatalf("saved-work claim leaked preserve keys: %s", payload)
		}
		// An ordinary running report never clears the requirement.
		if _, err := p.e.q.SetRunRunning(p.e.ctx, store.SetRunRunningParams{ID: id, WorkerID: pgconv.UUID(p.wid)}); err != nil {
			t.Fatalf("running report: %v", err)
		}
		if row := p.run(t, id); row.PolicyRequiredHead.String != a {
			t.Fatalf("ordinary running report cleared the saved-work head: %s", facts(row))
		}
		if got := verify(p, p.worker, id, 2, a); got.Disposition != "recorded" {
			t.Fatalf("verification: %+v", got)
		}
		row := p.run(t, id)
		if row.PolicyRequiredHead.Valid || row.PolicyRecoveryVerifiedGeneration.Valid || row.PolicyRecoveryVerifiedHead.Valid || row.PolicyPreserveRequired || !row.PolicyResolvedAt.Valid {
			t.Fatalf("verification must clear head and pair only: %s", facts(row))
		}
		// Idempotent after clearing: the head is gone, so a replayed record is a head mismatch.
		if got := verify(p, p.worker, id, 2, a); got.Disposition != "refused" || got.Reason != "head_mismatch" {
			t.Fatalf("replay after clear: %+v", got)
		}
		// A later claim (after a new hold/resume) carries nothing.
		req := p.mustHold(t, id, holdSpec{claim: 2, mode: "fresh_from_saved_work"})
		if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, ""); err != nil {
			t.Fatal(err)
		}
		if next := p.reclaim(t, id); policySavedWorkHead(next) != nil || policyExpectedHead(next) != nil {
			t.Fatalf("next claim must carry no head: %s", facts(next))
		}
	})
	t.Run("refusals leave the head in place", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.savedWorkRun(t, a)
		p.reclaim(t, id) // generation 2
		foreign := p.e.seedWorker(t, p.caps)
		foreignWorker, _ := p.e.q.GetWorkerByID(p.e.ctx, foreign)
		keep := func(what string, got PolicyRecoveryVerifiedReply, disposition, reason string) {
			t.Helper()
			if got.Disposition != disposition || got.Reason != reason {
				t.Fatalf("%s: %+v", what, got)
			}
			if row := p.run(t, id); row.PolicyRequiredHead.String != a {
				t.Fatalf("%s cleared the head: %s", what, facts(row))
			}
		}
		keep("stale generation", verify(p, p.worker, id, 1, a), "pending", "stale_claim")
		keep("wrong head", verify(p, p.worker, id, 2, hexHead("f")), "refused", "head_mismatch")
		keep("foreign worker", verify(p, foreignWorker, id, 2, a), "pending", "ownership_unconfirmed")
		p.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
		keep("released claim", verify(p, p.worker, id, 2, a), "pending", "claim_released")
		p.e.exec(t, "UPDATE runs SET claim_released_at=NULL,status='queued' WHERE id=$1", id)
		keep("ineligible status", verify(p, p.worker, id, 2, a), "pending", "ineligible_status")
	})
	t.Run("only a policy-capable worker claims a saved-work row", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.savedWorkRun(t, a)
		plain := p.e.seedWorker(t, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2})
		if run, err := p.e.q.ClaimRun(p.e.ctx, p.e.claimParams(plain, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}, false)); err == nil {
			t.Fatalf("incapable worker claimed saved-work run %s", run.ID)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if run := p.reclaim(t, id); run.ID != id {
			t.Fatal("capable worker must claim")
		}
	})
}
