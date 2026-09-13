package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
)

// PRD #1227 M2 unit coverage of CompletionScopeClaim, the WORKER-ONLY projection of a run's frozen
// completion contract's owner decisions. It is the seam the worker consumes to render the partial
// PR (deferred milestone ids+titles+reasons) and the accept warning block (accepted id+text+reason).
// These are pure (no DB); the claim-assembly wiring is covered separately.

// titledMilestones is a frozen milestone list whose TITLES differ from their ids, so a projection
// that echoes the id instead of looking up the criterion title is caught.
func titledMilestones() []Milestone {
	return []Milestone{
		{ID: "m1", Title: "First milestone"},
		{ID: "m2", Title: "Second milestone"},
		{ID: "m3", Title: "Third milestone"},
	}
}

// titledContract builds a revision-1 structural contract from titledMilestones.
func titledContract(t *testing.T) []byte {
	t.Helper()
	fj, err := encodeJSONArray(titledMilestones())
	if err != nil {
		t.Fatalf("encode frozen: %v", err)
	}
	c, err := buildCompletionContract(fj)
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	return c
}

// TestCompletionScopeClaimRevision1Nil: a revision-1 contract (no scope.out, no accepted) projects
// nil, so the ClaimConfig field's omitempty drops the key and a normal run's claim is byte-identical.
func TestCompletionScopeClaimRevision1Nil(t *testing.T) {
	if got := CompletionScopeClaim(titledContract(t)); got != nil {
		t.Fatalf("revision-1 contract must project nil, got %+v", got)
	}
}

// TestCompletionScopeClaimEmptyOrCorruptNil: an unfrozen/empty/corrupt contract is fail-safe nil.
func TestCompletionScopeClaimEmptyOrCorruptNil(t *testing.T) {
	for _, b := range [][]byte{nil, {}, []byte("not json"), []byte(`{"profile":`)} {
		if got := CompletionScopeClaim(b); got != nil {
			t.Fatalf("empty/corrupt contract must project nil, got %+v for %q", got, b)
		}
	}
}

// TestCompletionScopeClaimPartial: a partial-revised contract projects the deferred (out-of-scope)
// milestones with their TITLES (looked up from the matching criterion) and the owner reason, and no
// accepted entries.
func TestCompletionScopeClaimPartial(t *testing.T) {
	partial, err := buildRevisedContract(titledContract(t),
		CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "changed priority"},
		titledMilestones(), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract partial: %v", err)
	}
	got := CompletionScopeClaim(partial)
	if got == nil {
		t.Fatal("a partial contract must project a non-nil scope")
	}
	if len(got.Accepted) != 0 {
		t.Fatalf("a partial-only contract must carry no accepted; got %+v", got.Accepted)
	}
	// deferred = frozen − keep = {m2, m3}, each with the criterion's title and the owner's reason.
	byID := map[string]CompletionScopeDeferred{}
	for _, d := range got.Deferred {
		byID[d.MilestoneID] = d
	}
	if len(byID) != 2 {
		t.Fatalf("deferred = %+v, want m2,m3", got.Deferred)
	}
	if byID["m2"].Title != "Second milestone" || byID["m2"].Reason != "changed priority" {
		t.Fatalf("m2 deferred = %+v, want title 'Second milestone' reason 'changed priority'", byID["m2"])
	}
	if byID["m3"].Title != "Third milestone" || byID["m3"].Reason != "changed priority" {
		t.Fatalf("m3 deferred = %+v, want title 'Third milestone' reason 'changed priority'", byID["m3"])
	}
}

// TestCompletionScopeClaimAccept: an accept-revised contract projects the accepted criteria with
// id/milestone_id/text/reason and no deferred entries.
func TestCompletionScopeClaimAccept(t *testing.T) {
	accepted, err := buildRevisedContract(titledContract(t),
		CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "accepted as-is"},
		titledMilestones(), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract accept: %v", err)
	}
	got := CompletionScopeClaim(accepted)
	if got == nil {
		t.Fatal("an accept contract must project a non-nil scope")
	}
	if len(got.Deferred) != 0 {
		t.Fatalf("an accept-only contract must carry no deferred; got %+v", got.Deferred)
	}
	if len(got.Accepted) != 1 {
		t.Fatalf("accepted = %+v, want one (m2.c1)", got.Accepted)
	}
	a := got.Accepted[0]
	if a.ID != "m2.c1" || a.MilestoneID != "m2" || a.Text != "Second milestone" || a.Reason != "accepted as-is" {
		t.Fatalf("accepted[0] = %+v, want {m2.c1 m2 'Second milestone' 'accepted as-is'}", a)
	}
}

// TestCompletionScopeClaimBoth: a contract carrying BOTH a deferred and an accepted set (a partial
// then an accept) projects both halves populated.
func TestCompletionScopeClaimBoth(t *testing.T) {
	partial, err := buildRevisedContract(titledContract(t),
		CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "defer m3"},
		titledMilestones(), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract partial: %v", err)
	}
	both, err := buildRevisedContract(partial,
		CompletionDecisionInput{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "accept m1"},
		titledMilestones(), 3)
	if err != nil {
		t.Fatalf("buildRevisedContract accept: %v", err)
	}
	got := CompletionScopeClaim(both)
	if got == nil {
		t.Fatal("a partial+accept contract must project non-nil")
	}
	if len(got.Deferred) != 1 || got.Deferred[0].MilestoneID != "m3" || got.Deferred[0].Title != "Third milestone" || got.Deferred[0].Reason != "defer m3" {
		t.Fatalf("deferred = %+v, want [{m3 'Third milestone' 'defer m3'}]", got.Deferred)
	}
	if len(got.Accepted) != 1 || got.Accepted[0].ID != "m1.c1" || got.Accepted[0].Text != "First milestone" || got.Accepted[0].Reason != "accept m1" {
		t.Fatalf("accepted = %+v, want [{m1.c1 First milestone accept m1}]", got.Accepted)
	}
}

// TestCompletionScopeClaimOmitemptyOnClaimConfig proves the wire behavior the field promises: a nil
// projection drops the completion_scope key entirely (byte-identical claim), a partial-only
// projection carries `deferred` but omits `accepted`, and an accept-only projection the reverse.
func TestCompletionScopeClaimOmitemptyOnClaimConfig(t *testing.T) {
	marshal := func(cfg ClaimConfig) string {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal ClaimConfig: %v", err)
		}
		return string(b)
	}

	if s := marshal(ClaimConfig{}); strings.Contains(s, "completion_scope") {
		t.Fatalf("a nil CompletionScope must omit the key; got %s", s)
	}

	partial, err := buildRevisedContract(titledContract(t),
		CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "r"},
		titledMilestones(), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract partial: %v", err)
	}
	sp := marshal(ClaimConfig{CompletionScope: CompletionScopeClaim(partial)})
	if !strings.Contains(sp, `"completion_scope"`) || !strings.Contains(sp, `"deferred"`) {
		t.Fatalf("a partial projection must carry completion_scope.deferred; got %s", sp)
	}
	if strings.Contains(sp, `"accepted"`) {
		t.Fatalf("a partial-only projection must omit accepted; got %s", sp)
	}

	accepted, err := buildRevisedContract(titledContract(t),
		CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "r"},
		titledMilestones(), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract accept: %v", err)
	}
	sa := marshal(ClaimConfig{CompletionScope: CompletionScopeClaim(accepted)})
	if !strings.Contains(sa, `"accepted"`) {
		t.Fatalf("an accept projection must carry completion_scope.accepted; got %s", sa)
	}
	if strings.Contains(sa, `"deferred"`) {
		t.Fatalf("an accept-only projection must omit deferred; got %s", sa)
	}
}
