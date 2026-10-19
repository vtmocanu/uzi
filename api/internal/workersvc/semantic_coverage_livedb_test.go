package workersvc

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

type coverageLiveFixture struct {
	e           interlockLiveDB
	svc         *Service
	worker, run uuid.UUID
	now         time.Time
	candidate   apitypes.SemanticCandidate
	structural  PlanCrossCheckCandidate
	proposal    apitypes.SemanticProposal
	sources     []apitypes.SemanticSourceCapture
}

func newCoverageLiveFixture(t *testing.T) *coverageLiveFixture {
	t.Helper()
	e := setupInterlockLiveDB(t)
	f := &coverageLiveFixture{e: e, now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	f.worker = e.seedWorker(t, nil)
	f.run = e.seedActiveRunOwnedBy(t, f.worker)
	e.exec(t, `UPDATE runs SET claim_generation=7,completion_contract_version=1 WHERE id=$1`, f.run)
	e.exec(t, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, f.run)
	f.svc = &Service{txBeginner: e.pool, now: func() time.Time { return f.now }}
	f.structural, f.proposal, f.sources = semanticFixture()
	f.sources[0].Bytes = []byte("frozen issue requirement")
	f.proposal.Criteria[0].Citations[0].Hash = semanticHash(f.sources[0].Bytes)
	e.exec(t, `UPDATE runs SET issue_description=$2 WHERE id=$1`, f.run, string(f.sources[0].Bytes))
	f.sources = append(f.sources, apitypes.SemanticSourceCapture{ID: "missing-prd", Kind: "prd", Provenance: "repo:missing", Completeness: "unknown"})
	f.submit(t)
	return f
}
func (f *coverageLiveFixture) submit(t *testing.T) {
	t.Helper()
	var err error
	f.candidate, err = f.svc.SubmitSemanticCandidate(f.e.ctx, f.worker, f.run, 7, f.structural, semanticJSON(t, f.proposal), f.sources)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *coverageLiveFixture) reserve(t *testing.T, review string) apitypes.SemanticCoverageReservation {
	t.Helper()
	out, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), ReviewID: review, SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Supported: true}))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *coverageLiveFixture) settle(t *testing.T, r apitypes.SemanticCoverageReservation, status string, tokens *int) []byte {
	t.Helper()
	raw := semanticJSON(t, apitypes.SemanticCoverageSettleRequest{ReviewID: r.ReviewID, InvocationID: r.InvocationID, Status: status, Tokens: tokens})
	if err := f.svc.SettleSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
func (f *coverageLiveFixture) status(t *testing.T) apitypes.SemanticCoverageStatus {
	t.Helper()
	out, err := f.svc.SemanticCoverageStatus(f.e.ctx, f.worker, f.run, 7)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSemanticCoverageConcurrencyLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	raw := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Supported: true})
	const count = 8
	results := make([]apitypes.SemanticCoverageReservation, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw)
		}()
	}
	wg.Wait()
	reserved := 0
	id := ""
	for i, result := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if result.Outcome == "reserved" {
			reserved++
			id = result.InvocationID
			if result.Candidate == nil || !bytes.Equal(result.Plan, []byte(f.structural.PlanMd)) || len(result.Sources) != 2 {
				t.Fatal("missing server payload")
			}
			for j, source := range result.Sources {
				if !bytes.Equal(source.Bytes, f.sources[j].Bytes) || source.Completeness != f.sources[j].Completeness {
					t.Fatal("snapshot bytes/completeness changed")
				}
			}
		} else if result.Outcome != "replay" {
			t.Fatalf("outcome=%s", result.Outcome)
		}
	}
	if reserved != 1 {
		t.Fatalf("reserved=%d", reserved)
	}
	for _, result := range results {
		if result.InvocationID != id {
			t.Fatal("replay minted new invocation")
		}
	}
	status := f.status(t)
	if status.Reviews != 1 || status.Invocations != 1 || status.ChargedTokens != 40000 {
		t.Fatalf("status=%+v", status)
	}
	// A fresh request during an unsettled invocation cannot launch a retry.
	if got := f.reserve(t, results[0].ReviewID); got.Outcome != "unavailable" {
		t.Fatal("pending invocation retried")
	}
}

func TestSemanticCoverageRetryDeadlineLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	first := f.reserve(t, "")
	thousand := 1000
	raw := f.settle(t, first, "schema_error", &thousand)
	if err := f.svc.SettleSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t); got.ChargedTokens != 1000 {
		t.Fatal("duplicate settlement refunded twice")
	}
	f.now = f.now.Add(299 * time.Second)
	retry := f.reserve(t, first.ReviewID)
	if retry.Outcome != "reserved" || retry.MaxTokens != 39000 || !retry.Deadline.Equal(first.Deadline) || retry.ReviewID != first.ReviewID {
		t.Fatalf("retry=%+v", retry)
	}
	f.now = f.now.Add(time.Second)
	zero := 0
	f.settle(t, retry, "success", &zero)
	if got := f.status(t); got.ChargedTokens != 40000 {
		t.Fatal("expired invocation refunded")
	}
	if got := f.reserve(t, first.ReviewID); got.Outcome != "unavailable" {
		t.Fatal("third invocation admitted")
	}
	f.now = f.now.Add(601 * time.Second)
	if got := f.reserve(t, ""); got.Outcome != "reserved" || !got.Deadline.Equal(f.now.Add(5*time.Minute)) {
		t.Fatal("human waiting consumed the next review allowance")
	}
}

func TestSemanticCoverageStaleChargesLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	first := f.reserve(t, "")
	f.proposal.Criteria[0].Text = "changed behavior"
	f.submit(t)
	zero := 0
	raw := f.settle(t, first, "success", &zero)
	status := f.status(t)
	if status.Reviews != 1 || status.ChargedTokens != 40000 || status.Report != nil {
		t.Fatalf("revision reset charge: %+v", status)
	}
	second := f.reserve(t, "")
	if second.ChargedTokens != 80000 {
		t.Fatal("new revision lost charge")
	}
	// Conflicting settlement cannot change a prior settlement.
	conflict := semanticJSON(t, apitypes.SemanticCoverageSettleRequest{ReviewID: first.ReviewID, InvocationID: first.InvocationID, Status: "unknown"})
	if err := f.svc.SettleSemanticCoverage(f.e.ctx, f.worker, f.run, 7, conflict); err == nil {
		t.Fatal("conflicting settlement accepted")
	}
	f.e.exec(t, `UPDATE runs SET claim_generation=8 WHERE id=$1`, f.run)
	if err := f.svc.SettleSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale claim: %v", err)
	}
	// Reclaimed work can be settled for accounting but cannot refund old calls.
	raw = semanticJSON(t, apitypes.SemanticCoverageSettleRequest{ReviewID: second.ReviewID, InvocationID: second.InvocationID, Status: "success", Tokens: &zero})
	if err := f.svc.SettleSemanticCoverage(f.e.ctx, f.worker, f.run, 8, raw); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.SemanticCoverageStatus(f.e.ctx, f.worker, f.run, 8)
	if err != nil || status.ChargedTokens != 80000 || status.Reviews != 2 {
		t.Fatalf("reclaim reset: %+v %v", status, err)
	}
}

func TestSemanticCoverageCapsAndAdmissionLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	unsupported := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), SemanticCoverageBinding: SemanticCoverageBinding(f.candidate)})
	out, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, unsupported)
	if err != nil || out.Outcome != "unavailable" || f.status(t).Reviews != 0 {
		t.Fatal("unsupported admission charged")
	}
	// Three lost invocations consume the full run allowance; no settlement is
	// necessary to retain their conservative charges.
	for range 3 {
		if got := f.reserve(t, ""); got.Outcome != "reserved" {
			t.Fatalf("reservation=%+v", got)
		}
	}
	if got := f.reserve(t, ""); got.Outcome != "unavailable" {
		t.Fatal("fourth review admitted")
	}
	if got := f.status(t); got.Reviews != 3 || got.ChargedTokens != 120000 || got.RemainingTokens != 0 {
		t.Fatalf("cap=%+v", got)
	}
	for _, tc := range []struct {
		status   string
		released bool
		want     error
	}{
		{"running", true, ErrStaleClaim},
		{"awaiting_approval", false, ErrInvalidState},
		{"completed", false, ErrRunTerminal},
	} {
		f.e.exec(t, `UPDATE runs SET status=$2,claim_released_at=CASE WHEN $3::boolean THEN now() ELSE NULL END WHERE id=$1`, f.run, tc.status, tc.released)
		_, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, unsupported)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.status, err)
		}
	}
}

func TestSemanticCoverageIndependentReviewCapsLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	zero := 0
	first := f.reserve(t, "")
	f.settle(t, first, "transport_error", &zero)
	retry := f.reserve(t, first.ReviewID)
	if retry.Outcome != "reserved" {
		t.Fatal("retry refused")
	}
	f.settle(t, retry, "schema_error", &zero)
	if got := f.reserve(t, first.ReviewID); got.Outcome != "unavailable" {
		t.Fatal("third invocation admitted before deadline")
	}
	for range 2 {
		r := f.reserve(t, "")
		if r.Outcome != "reserved" {
			t.Fatal("review refused below cap")
		}
		f.settle(t, r, "success", &zero)
	}
	if got := f.reserve(t, ""); got.Outcome != "unavailable" {
		t.Fatal("review cap refunded with tokens")
	}
	if got := f.status(t); got.Reviews != 3 || got.Invocations != 4 || got.ChargedTokens != 0 {
		t.Fatalf("independent caps=%+v", got)
	}
}

func TestSemanticCoverageUnknownAndBindingsLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	wrong := SemanticCoverageBinding(f.candidate)
	wrong.SourceDigest = "wrong source"
	raw := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), Supported: true, SemanticCoverageBinding: wrong})
	if _, err := f.svc.ReserveSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err == nil {
		t.Fatal("foreign source admitted")
	}
	if got := f.status(t); got.Invocations != 0 {
		t.Fatal("invalid binding charged")
	}
	r := f.reserve(t, "")
	zero := 0
	f.settle(t, r, "unknown", &zero)
	if got := f.status(t); got.ChargedTokens != 40000 {
		t.Fatal("unknown usage refunded")
	}
	if got := f.reserve(t, r.ReviewID); got.Outcome != "unavailable" {
		t.Fatal("unknown outcome retried")
	}
}

func TestSemanticCoverageDormantLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	worker := e.seedWorker(t, nil)
	run := e.seedActiveRunOwnedBy(t, worker)
	e.exec(t, `UPDATE runs SET completion_contract_version=1 WHERE id=$1`, run)
	raw := semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: uuid.NewString(), Supported: true})
	out, err := (&Service{txBeginner: e.pool}).ReserveSemanticCoverage(e.ctx, worker, run, 0, raw)
	if err != nil || out.Outcome != "unavailable" || out.InvocationID != "" {
		t.Fatalf("unenrolled run admitted: %+v %v", out, err)
	}
}

func TestSemanticCoveragePublicationLiveDB(t *testing.T) {
	f := newCoverageLiveFixture(t)
	reservation := f.reserve(t, "")
	report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: SemanticCoverageBinding(f.candidate), Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "behavior represented", Citations: f.candidate.Criteria[0].Citations, CriterionIDs: []string{f.candidate.Criteria[0].ID}}}, Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: f.candidate.Criteria[0].ID, Atomic: true, Explanation: "atomic behavior"}}}
	raw := semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: reservation.ReviewID, InvocationID: reservation.InvocationID, Report: report})
	if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err == nil {
		t.Fatal("unsettled report accepted")
	}
	used := 25
	f.settle(t, reservation, "success", &used)
	for range 2 {
		if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.status(t); got.Outcome != "uncertain" || got.Report == nil || got.ChargedTokens != 25 {
		t.Fatalf("unknown source hidden: %+v", got)
	}
	f.proposal.Criteria[0].Text = "revision after report"
	f.submit(t)
	if err := f.svc.PublishSemanticCoverage(f.e.ctx, f.worker, f.run, 7, raw); err == nil {
		t.Fatal("stale report accepted")
	}
	if got := f.status(t); got.Report != nil || got.ChargedTokens != 25 || got.Reviews != 1 {
		t.Fatalf("stale evidence reused: %+v", got)
	}
}
