package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func semanticCompletionFixture(t *testing.T, workerCriterion bool) store.Run {
	t.Helper()
	structural, proposal, sources := semanticFixture()
	maintainer := proposal.Criteria[0]
	maintainer.Kind = "maintainer"
	maintainer.Text = "observe production\u001b rollout"
	maintainer.Maintainer = &apitypes.SemanticMaintainer{Reason: "owner deployment", Conditions: []string{"deployment available"}, VerificationSteps: []string{"observe rollout"}}
	if workerCriterion {
		proposal.Criteria = append(proposal.Criteria, maintainer)
	} else {
		proposal.Criteria = []apitypes.ProposedSemanticCriterion{maintainer}
	}
	candidate, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Revision, candidate.Digest = 1, "frozen"
	for i := range candidate.Criteria {
		candidate.Criteria[i].ID = uuid.NewString()
	}
	run := interlockedRun(worker())
	run.Kind = "issue"
	run.GateRevision, run.GatePayloadDigest = 1, []byte{1}
	run.MilestonesFrozen = structural.Milestones
	p := semanticGateProjection(&semanticCoverageTx{candidate: candidate}, run)
	approval := semanticApprovalFor(run, p)
	run.CompletionContract, err = semanticGateContract(&p, &approval)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestSemanticCompletionCitationHash(t *testing.T) {
	run := semanticCompletionFixture(t, false)
	c, err := parseCompletionContract(run.CompletionContract)
	if err != nil || validateSemanticCompletionBounds(c) != nil {
		t.Fatalf("valid contract: %v", err)
	}
	c.Criteria[0].Citations[0].Hash = strings.Repeat("0", 64)
	if validateSemanticCompletionBounds(c) == nil {
		t.Fatal("citation hash differing from frozen source was admitted")
	}
}

func TestSemanticCompletionPendingPermitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mixed bool
		edit  func(*store.Run)
		grant bool
		deny  string
		unmet []string
	}{
		{name: "pending only", grant: true},
		{name: "mixed worker unmet", mixed: true, deny: CompletionDenyMissingMilestones, unmet: []string{"m1"}},
		{name: "mixed worker done", mixed: true, edit: func(r *store.Run) { r.MilestonesCompleted = idsJSON(t, "m1") }, grant: true},
		{name: "approved milestone without criterion", edit: func(r *store.Run) { r.MilestonesFrozen = frozenJSON(t, "m1", "m2") }, deny: CompletionDenyMissingMilestones, unmet: []string{"m2"}},
		{name: "missing auth", edit: func(r *store.Run) {
			c, _ := parseCompletionContract(r.CompletionContract)
			c.MaintainerAuthorizations = nil
			r.CompletionContract = semanticJSON(t, c)
		}, deny: CompletionDenyContractNotFrozen},
		{name: "foreign auth", edit: func(r *store.Run) {
			c, _ := parseCompletionContract(r.CompletionContract)
			c.MaintainerAuthorizations[0].ID = uuid.NewString()
			r.CompletionContract = semanticJSON(t, c)
		}, deny: CompletionDenyContractNotFrozen},
		{name: "future auth", edit: func(r *store.Run) {
			c, _ := parseCompletionContract(r.CompletionContract)
			c.MaintainerAuthorizations[0].Revision = 2
			r.CompletionContract = semanticJSON(t, c)
		}, deny: CompletionDenyContractNotFrozen},
		{name: "changed conditions", edit: func(r *store.Run) {
			c, _ := parseCompletionContract(r.CompletionContract)
			c.MaintainerAuthorizations[0].Conditions = []string{"changed"}
			r.CompletionContract = semanticJSON(t, c)
		}, deny: CompletionDenyContractNotFrozen},
		{name: "corrupt approval", edit: func(r *store.Run) {
			c, _ := parseCompletionContract(r.CompletionContract)
			c.SemanticApproval = nil
			r.CompletionContract = semanticJSON(t, c)
		}, deny: CompletionDenyContractNotFrozen},
		{name: "stale revision", edit: func(r *store.Run) { r.ContractRevision.Int32 = 2 }, deny: CompletionDenyContractNotFrozen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := semanticCompletionFixture(t, tc.mixed)
			if tc.edit != nil {
				tc.edit(&run)
			}
			before := append([]byte(nil), run.MilestonesCompleted...)
			fs := &fakeStore{runOwned: run}
			svc := New(fs, newBox(t), testParams())
			// Pure decision coverage only; public semantic success requires the LiveDB transaction tests.
			out, err := svc.requestCompletionPermitLocked(context.Background(), worker(), run.ID, CompletionPermitRequest{ContractRevision: int(run.ContractRevision.Int32), Branch: "agent/issue-1", Head: "head"})
			if err != nil || out.Granted != tc.grant || out.DenyReason != tc.deny || !reflect.DeepEqual(out.Unmet, tc.unmet) {
				t.Fatalf("permit=%+v err=%v", out, err)
			}
			if !reflect.DeepEqual(before, run.MilestonesCompleted) {
				t.Fatal("pending manufactured completion")
			}
			if tc.grant {
				pending, err := CompletionPending(run)
				if err != nil || !sameSemanticJSON(out.Permit.CompletionPending, pending) || pending.Criteria[0].Status != "unverified" || pending.Criteria[0].AuthorizationRevision != 1 {
					t.Fatalf("pending %+v err=%v", pending, err)
				}
				if pending.Criteria[0].CriterionID == "m1" || strings.HasSuffix(pending.Criteria[0].CriterionID, ".c1") {
					t.Fatal("criterion namespace collapsed")
				}
			}
		})
	}
}

func TestSemanticCompletionInitialAuthority(t *testing.T) {
	run := semanticCompletionFixture(t, false)
	c, _ := parseCompletionContract(run.CompletionContract)
	for _, tc := range []struct {
		name string
		edit func(*apitypes.SemanticGatePresentation, *apitypes.SemanticGateApproval)
	}{
		{"missing", func(_ *apitypes.SemanticGatePresentation, a *apitypes.SemanticGateApproval) { a.Maintainers = nil }},
		{"foreign", func(_ *apitypes.SemanticGatePresentation, a *apitypes.SemanticGateApproval) {
			a.Maintainers[0].ID = uuid.NewString()
		}},
		{"changed steps", func(_ *apitypes.SemanticGatePresentation, a *apitypes.SemanticGateApproval) {
			a.Maintainers[0].VerificationSteps = []string{"changed"}
		}},
		{"presentation omits", func(p *apitypes.SemanticGatePresentation, _ *apitypes.SemanticGateApproval) {
			p.Maintainers = []apitypes.SemanticMaintainerApproval{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p apitypes.SemanticGatePresentation
			var a apitypes.SemanticGateApproval
			_ = json.Unmarshal(semanticJSON(t, c.SemanticPresentation), &p)
			_ = json.Unmarshal(semanticJSON(t, c.SemanticApproval), &a)
			tc.edit(&p, &a)
			if _, err := semanticGateContract(&p, &a); err == nil {
				t.Fatal("inexact authority frozen")
			}
		})
	}
	if _, err := semanticGateContract(c.SemanticPresentation, nil); err == nil {
		t.Fatal("automatic freeze authorized maintainer")
	}
}

func TestSemanticCompletionRevisionCarryAndReplay(t *testing.T) {
	run := semanticCompletionFixture(t, true)
	prior, _ := parseCompletionContract(run.CompletionContract)
	dec := CompletionDecisionInput{Decision: "authorize_maintainer", Maintainers: semanticMaintainers(prior), Reason: "renew exact authorization", ContractRevision: 1}
	ms, _ := DecodeMilestones(run.MilestonesFrozen)
	if err := validateDecision(run, dec, ms); err != nil {
		t.Fatal(err)
	}
	next, err := buildRevisedContract(run.CompletionContract, dec, ms, 2)
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionContract, run.ContractRevision = next, pgtype.Int4{Int32: 2, Valid: true}
	if !decisionAlreadyEncoded(run, dec, 2) {
		t.Fatal("lost response cannot replay")
	}
	changed := dec
	changed.Reason = "different"
	if decisionAlreadyEncoded(run, changed, 2) {
		t.Fatal("different reason replayed")
	}
	changed = dec
	changed.Maintainers = append([]apitypes.SemanticMaintainerApproval(nil), dec.Maintainers...)
	changed.Maintainers[0].Conditions = []string{"different"}
	if decisionAlreadyEncoded(run, changed, 2) || validateDecision(run, changed, ms) == nil {
		t.Fatal("changed conditions authorized")
	}
	got, err := CompletionPending(run)
	if err != nil || got.Criteria[0].AuthorizationRevision != 2 || got.Criteria[0].Status != "unverified" {
		t.Fatalf("pending=%+v err=%v", got, err)
	}
	current, _ := parseCompletionContract(next)
	if !sameSemanticJSON(prior.Criteria, current.Criteria) || !sameSemanticJSON(prior.SemanticApproval, current.SemanticApproval) || !sameSemanticJSON(prior.SemanticPresentation, current.SemanticPresentation) || !sameSemanticJSON(prior.Sources, current.Sources) {
		t.Fatal("typed revision lost semantic fields")
	}
	for _, cr := range current.Criteria {
		if err := validateDecision(run, CompletionDecisionInput{Decision: "accept", Criteria: []string{cr.ID}}, ms); err == nil {
			t.Fatal("semantic accept waived obligation")
		}
	}
	// Typed serialization preserves the immutable contract even for an accept;
	// public validation above prevents it from becoming a semantic waiver.
	carried, err := buildRevisedContract(next, CompletionDecisionInput{Decision: "accept", Criteria: []string{got.Criteria[0].CriterionID}}, ms, 3)
	if err != nil {
		t.Fatal(err)
	}
	cc, _ := parseCompletionContract(carried)
	if len(cc.Accepted) != 0 || !sameSemanticJSON(cc.Criteria, current.Criteria) || !sameSemanticJSON(cc.MaintainerAuthorizations, current.MaintainerAuthorizations) {
		t.Fatal("accept marked pending performed or dropped data")
	}
}

func TestSemanticCompletionStructuralAbsence(t *testing.T) {
	run := interlockedRun(worker())
	run.CompletionContract = contractJSON(t, "m1")
	pending, err := CompletionPending(run)
	if err != nil || pending != nil {
		t.Fatalf("structural pending=%+v err=%v", pending, err)
	}
	raw := semanticJSON(t, ClaimConfig{CompletionPending: pending})
	if strings.Contains(string(raw), "completion_pending") {
		t.Fatal(string(raw))
	}
}

func TestSemanticCompletionPendingOrderAndTitles(t *testing.T) {
	run := semanticCompletionFixture(t, false)
	c, _ := parseCompletionContract(run.CompletionContract)
	// Two different UUIDs, in reverse order, on the same milestone.
	second := c.SemanticPresentation.Candidate.Criteria[0]
	second.ID = "00000000-0000-4000-8000-000000000001"
	c.SemanticPresentation.Candidate.Criteria = append(c.SemanticPresentation.Candidate.Criteria, second)
	p := semanticGateProjection(&semanticCoverageTx{candidate: c.SemanticPresentation.Candidate}, run)
	a := semanticApprovalFor(run, p)
	var err error
	run.CompletionContract, err = semanticGateContract(&p, &a)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := CompletionPending(run)
	if err != nil || len(pending.Criteria) != 2 || pending.Criteria[0].CriterionID != second.ID {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	ms, _ := DecodeMilestones(run.MilestonesFrozen)
	run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
	ms = append(ms, Milestone{ID: "m2", Title: "Frozen milestone title"})
	run.CompletionContract, err = buildRevisedContract(run.CompletionContract, CompletionDecisionInput{Decision: "partial", Keep: []string{"m2"}, Reason: "later"}, ms, 2)
	if err != nil {
		t.Fatal(err)
	}
	scope := CompletionScopeClaim(run.CompletionContract, semanticJSON(t, ms))
	if scope.Deferred[0].MilestoneID != "m1" || scope.Deferred[0].Title != "Behavior" {
		t.Fatalf("semantic title from criterion: %+v", scope)
	}
}

func TestSemanticCompletionPendingEmptyAndScopeBranches(t *testing.T) {
	run := semanticCompletionFixture(t, true)
	c, _ := parseCompletionContract(run.CompletionContract)
	candidate := c.SemanticPresentation.Candidate
	candidate.Criteria = nil
	for _, cr := range c.SemanticPresentation.Candidate.Criteria {
		if cr.Kind != "maintainer" {
			candidate.Criteria = append(candidate.Criteria, cr)
		}
	}
	p := semanticGateProjection(&semanticCoverageTx{candidate: candidate}, run)
	var err error
	run.CompletionContract, err = semanticGateContract(&p, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := CompletionPending(run)
	if err != nil || pending.Criteria == nil || len(pending.Criteria) != 0 {
		t.Fatalf("empty semantic pending=%+v %v", pending, err)
	}
	run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
	run.MilestonesCompleted = idsJSON(t, "m1")
	run.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	if eligible, valid := scopeCapEligibility(run); !eligible || !valid {
		t.Fatalf("semantic scope cap=%v %v", eligible, valid)
	}
	if _, valid := headlessCompletedUnion(run, StateRequest{}); valid {
		t.Fatal("headless omitted approved milestone")
	}
	ids := []string{"m2"}
	if _, valid := headlessCompletedUnion(run, StateRequest{MilestonesCompleted: &ids}); !valid {
		t.Fatal("semantic UUID criteria rejected full frozen completion")
	}
	// Corrupt semantic fields cannot be disguised as a structural contract.
	c, _ = parseCompletionContract(run.CompletionContract)
	c.Profile = contractProfileStructural
	run.CompletionContract = semanticJSON(t, c)
	if _, err := CompletionPending(run); err == nil {
		t.Fatal("profile corruption hidden")
	}
	if _, valid := computeUnmetCriteria(run); valid {
		t.Fatal("profile corruption permitted")
	}
}

func TestSemanticCompletionPendingWireBound(t *testing.T) {
	// Worst JSON expansion: every decoded text byte needs six wire bytes.
	text := strings.Repeat("\u0001", 2048)
	step := strings.Repeat("\u0001", 512)
	out := apitypes.CompletionPendingDTO{Version: 1, ContractRevision: 1, Criteria: []apitypes.CompletionPendingCriterion{}}
	for i := 0; i < 256; i++ {
		m := apitypes.SemanticMaintainer{Reason: text, Conditions: []string{}, VerificationSteps: []string{}}
		for j := 0; j < 8; j++ {
			m.Conditions = append(m.Conditions, step)
			m.VerificationSteps = append(m.VerificationSteps, step)
		}
		out.Criteria = append(out.Criteria, apitypes.CompletionPendingCriterion{CriterionID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), MilestoneID: strings.Repeat("m", 64), Kind: "maintainer", Text: text, SemanticMaintainer: m, AuthorizationRevision: 1, Status: "unverified"})
	}
	raw := semanticJSON(t, out)
	t.Logf("maximally escaped pending payload: %d bytes; transport allowance needed: 20 MiB plus other response fields", len(raw))
	if len(raw) < 18<<20 || len(raw) >= 20<<20 {
		t.Fatalf("pending wire estimate drifted: %d", len(raw))
	}
}
