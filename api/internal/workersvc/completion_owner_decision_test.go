package workersvc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1227 M1 unit coverage of the contract-revision shape, the owner-decision validation and the
// idempotency comparison — all pure (no DB). The transaction wiring is covered by the LiveDB tests.

// milestoneList builds the []Milestone frozen-list shape buildRevisedContract splits on.
func milestoneList(ids ...string) []Milestone {
	ms := make([]Milestone, 0, len(ids))
	for _, id := range ids {
		ms = append(ms, Milestone{ID: id, Title: id})
	}
	return ms
}

// TestCompletionContractRevision1ByteIdentical proves a revision-1 contract (no scope/accepted)
// marshals with NEITHER a "scope" nor an "accepted" key — the omitempty guarantee that keeps the
// #1226 freeze goldens and the permit-path recompute byte-identical after the #1227 struct widening.
func TestCompletionContractRevision1ByteIdentical(t *testing.T) {
	got := contractJSON(t, "m1", "m2")
	if strings.Contains(string(got), "\"scope\"") {
		t.Fatalf("a revision-1 contract must NOT marshal a scope key; got %s", got)
	}
	if strings.Contains(string(got), "\"accepted\"") {
		t.Fatalf("a revision-1 contract must NOT marshal an accepted key; got %s", got)
	}
	// The exact revision-1 byte shape must round-trip through the extended struct unchanged.
	var c completionContract
	if err := json.Unmarshal(got, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Scope != nil || c.Accepted != nil {
		t.Fatalf("a revision-1 contract must decode with nil scope/accepted; got scope=%v accepted=%v", c.Scope, c.Accepted)
	}
	re, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(re) != string(got) {
		t.Fatalf("revision-1 contract not byte-identical after round-trip:\n got: %s\nwant: %s", re, got)
	}
}

// TestComputeUnmetCriteriaScopeExclusions pins the #1227 additions to the server recompute: a
// DEFERRED milestone (scope.out) and an ACCEPTED criterion (accepted) are BOTH excluded from unmet,
// while a revision-1 contract is byte-identical to the milestone-done-only behavior.
func TestComputeUnmetCriteriaScopeExclusions(t *testing.T) {
	rev := pgtype.Int4{Int32: 1, Valid: true}
	frozen := frozenJSON(t, "m1", "m2", "m3")

	t.Run("revision-1 is done-only (byte-identical behavior)", func(t *testing.T) {
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        contractJSON(t, "m1", "m2", "m3"),
			MilestonesCompleted:       idsJSON(t, "m1"),
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if !verifiable || len(unmet) != 2 || unmet[0] != "m2" || unmet[1] != "m3" {
			t.Fatalf("unmet = %v verifiable = %v, want [m2 m3] true", unmet, verifiable)
		}
	})

	t.Run("deferred milestone excluded", func(t *testing.T) {
		revised, err := buildRevisedContract(contractJSON(t, "m1", "m2", "m3"),
			CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "changed priority"},
			milestoneList("m1", "m2", "m3"), 2)
		if err != nil {
			t.Fatalf("buildRevisedContract: %v", err)
		}
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        revised,
			MilestonesFrozen:          frozen,
			MilestonesCompleted:       idsJSON(t), // nothing done
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if !verifiable {
			t.Fatal("a revised contract is verifiable")
		}
		// m2, m3 are deferred → excluded; only m1 (in-scope, not done) is unmet.
		if len(unmet) != 1 || unmet[0] != "m1" {
			t.Fatalf("unmet = %v, want [m1] (m2/m3 deferred are excluded)", unmet)
		}
	})

	t.Run("accepted criterion excluded", func(t *testing.T) {
		// Accept m2's criterion "m2.c1"; m2 is neither done nor deferred but is owner-accepted.
		revised, err := buildRevisedContract(contractJSON(t, "m1", "m2", "m3"),
			CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "accepted as-is"},
			milestoneList("m1", "m2", "m3"), 2)
		if err != nil {
			t.Fatalf("buildRevisedContract: %v", err)
		}
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        revised,
			MilestonesFrozen:          frozen,
			MilestonesCompleted:       idsJSON(t, "m1"), // m1 done
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if !verifiable {
			t.Fatal("verifiable")
		}
		// m1 done, m2 accepted → only m3 unmet.
		if len(unmet) != 1 || unmet[0] != "m3" {
			t.Fatalf("unmet = %v, want [m3] (m2 accepted is excluded)", unmet)
		}
	})
}

// TestBuildRevisedContractPartial pins the partial revision shape: sorted scope.in == keep, scope.out
// == frozen−keep at the new revision, prior deferrals PRESERVED (original reason/revision), criteria
// unchanged, revision bumped.
func TestBuildRevisedContractPartial(t *testing.T) {
	prior := contractJSON(t, "m1", "m2", "m3", "m4")
	// First partial at rev 2: keep {m1,m2}, defer {m3,m4}.
	rev2, err := buildRevisedContract(prior,
		CompletionDecisionInput{Decision: "partial", Keep: []string{"m2", "m1"}, Reason: "reason-A"},
		milestoneList("m1", "m2", "m3", "m4"), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract rev2: %v", err)
	}
	var c2 completionContract
	if err := json.Unmarshal(rev2, &c2); err != nil {
		t.Fatalf("unmarshal rev2: %v", err)
	}
	if c2.Revision != 2 {
		t.Fatalf("revision = %d, want 2", c2.Revision)
	}
	if len(c2.Criteria) != 4 {
		t.Fatalf("criteria must be carried through unchanged; got %d, want 4", len(c2.Criteria))
	}
	if c2.Scope == nil {
		t.Fatal("scope must be set")
	}
	if strings.Join(c2.Scope.In, ",") != "m1,m2" {
		t.Fatalf("scope.in = %v, want sorted [m1 m2]", c2.Scope.In)
	}
	gotOut := map[string]deferredEntry{}
	for _, d := range c2.Scope.Out {
		gotOut[d.MilestoneID] = d
	}
	if len(gotOut) != 2 || gotOut["m3"].Revision != 2 || gotOut["m3"].Reason != "reason-A" || gotOut["m4"].Revision != 2 {
		t.Fatalf("scope.out = %+v, want m3,m4 at revision 2 reason-A", c2.Scope.Out)
	}

	// Second partial at rev 3 against rev2: keep {m1}, so m2 becomes newly deferred at rev3; m3,m4
	// were already deferred at rev2 and MUST keep their original reason/revision.
	rev3, err := buildRevisedContract(rev2,
		CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "reason-B"},
		milestoneList("m1", "m2", "m3", "m4"), 3)
	if err != nil {
		t.Fatalf("buildRevisedContract rev3: %v", err)
	}
	var c3 completionContract
	if err := json.Unmarshal(rev3, &c3); err != nil {
		t.Fatalf("unmarshal rev3: %v", err)
	}
	out3 := map[string]deferredEntry{}
	for _, d := range c3.Scope.Out {
		out3[d.MilestoneID] = d
	}
	if out3["m2"].Revision != 3 || out3["m2"].Reason != "reason-B" {
		t.Fatalf("m2 must be newly deferred at rev3 reason-B; got %+v", out3["m2"])
	}
	if out3["m3"].Revision != 2 || out3["m3"].Reason != "reason-A" {
		t.Fatalf("m3 (deferred at rev2) must keep its original reason/revision; got %+v", out3["m3"])
	}
	if out3["m4"].Revision != 2 || out3["m4"].Reason != "reason-A" {
		t.Fatalf("m4 (deferred at rev2) must keep its original reason/revision; got %+v", out3["m4"])
	}
}

// TestBuildRevisedContractAccept pins the accept revision shape: each new criterion id becomes an
// accepted entry with milestone_id/text copied from the matching criteria[] entry, at the new
// revision, and prior acceptances are preserved. scope is untouched.
func TestBuildRevisedContractAccept(t *testing.T) {
	prior := contractJSON(t, "m1", "m2") // criteria ids m1.c1, m2.c1; text == id in the fixture
	rev2, err := buildRevisedContract(prior,
		CompletionDecisionInput{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "accept-A"},
		milestoneList("m1", "m2"), 2)
	if err != nil {
		t.Fatalf("buildRevisedContract accept rev2: %v", err)
	}
	var c2 completionContract
	if err := json.Unmarshal(rev2, &c2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c2.Scope != nil {
		t.Fatalf("accept must not touch scope; got %+v", c2.Scope)
	}
	if len(c2.Accepted) != 1 {
		t.Fatalf("accepted len = %d, want 1", len(c2.Accepted))
	}
	a := c2.Accepted[0]
	if a.ID != "m1.c1" || a.MilestoneID != "m1" || a.Text != "m1" || a.Reason != "accept-A" || a.Revision != 2 {
		t.Fatalf("accepted[0] = %+v, want {m1.c1 m1 m1 accept-A 2}", a)
	}

	// Second accept at rev3 preserves the prior acceptance and appends the new one.
	rev3, err := buildRevisedContract(rev2,
		CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "accept-B"},
		milestoneList("m1", "m2"), 3)
	if err != nil {
		t.Fatalf("buildRevisedContract accept rev3: %v", err)
	}
	var c3 completionContract
	if err := json.Unmarshal(rev3, &c3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(c3.Accepted) != 2 {
		t.Fatalf("accepted len = %d, want 2 (prior preserved + new)", len(c3.Accepted))
	}
	byID := map[string]acceptedEntry{}
	for _, a := range c3.Accepted {
		byID[a.ID] = a
	}
	if byID["m1.c1"].Revision != 2 || byID["m1.c1"].Reason != "accept-A" {
		t.Fatalf("prior acceptance must be preserved; got %+v", byID["m1.c1"])
	}
	if byID["m2.c1"].Revision != 3 || byID["m2.c1"].Reason != "accept-B" {
		t.Fatalf("new acceptance at rev3; got %+v", byID["m2.c1"])
	}
}

// TestContractHasDeferrals pins the deferral predicate: true for a partial contract, false for a
// revision-1 contract, an accept-only contract, and a corrupt/empty one.
func TestContractHasDeferrals(t *testing.T) {
	if contractHasDeferrals(contractJSON(t, "m1", "m2")) {
		t.Fatal("a revision-1 contract has no deferrals")
	}
	partial, _ := buildRevisedContract(contractJSON(t, "m1", "m2"),
		CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "r"},
		milestoneList("m1", "m2"), 2)
	if !contractHasDeferrals(partial) {
		t.Fatal("a partial contract with scope.out has deferrals")
	}
	acceptOnly, _ := buildRevisedContract(contractJSON(t, "m1", "m2"),
		CompletionDecisionInput{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "r"},
		milestoneList("m1", "m2"), 2)
	if contractHasDeferrals(acceptOnly) {
		t.Fatal("an accept-only contract has no deferrals")
	}
	if contractHasDeferrals([]byte("{not json")) || contractHasDeferrals(nil) {
		t.Fatal("a corrupt/empty contract is fail-safe false")
	}
}

// TestValidateDecision walks the D1 rejection matrix and the accept path against a locked run whose
// contract is at revision 1 with m1 completed.
func TestValidateDecision(t *testing.T) {
	frozenMs := milestoneList("m1", "m2", "m3")
	lockedRev1 := store.Run{
		CompletionContract:  contractJSON(t, "m1", "m2", "m3"),
		MilestonesFrozen:    frozenJSON(t, "m1", "m2", "m3"),
		MilestonesCompleted: idsJSON(t, "m1"),
	}

	mustInvalid := func(t *testing.T, dec CompletionDecisionInput) {
		t.Helper()
		if err := validateDecision(lockedRev1, dec, frozenMs); !errors.Is(err, ErrCompletionDecisionInvalid) {
			t.Fatalf("want ErrCompletionDecisionInvalid, got %v", err)
		}
	}

	t.Run("partial empty keep", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "partial", Reason: "r"})
	})
	t.Run("partial unknown milestone", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "zzz"}, Reason: "r"})
	})
	t.Run("partial duplicate", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m1"}, Reason: "r"})
	})
	t.Run("partial removes nothing (keep all)", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m2", "m3"}, Reason: "r"})
	})
	t.Run("partial valid", func(t *testing.T) {
		if err := validateDecision(lockedRev1, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "r"}, frozenMs); err != nil {
			t.Fatalf("valid partial rejected: %v", err)
		}
	})

	t.Run("accept empty", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "accept", Reason: "r"})
	})
	t.Run("accept unknown criterion", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "accept", Criteria: []string{"zzz.c1"}, Reason: "r"})
	})
	t.Run("accept duplicate", func(t *testing.T) {
		mustInvalid(t, CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1", "m2.c1"}, Reason: "r"})
	})
	t.Run("accept already satisfied", func(t *testing.T) {
		// m1 is completed, so accepting m1.c1 is invalid.
		mustInvalid(t, CompletionDecisionInput{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "r"})
	})
	t.Run("accept valid", func(t *testing.T) {
		if err := validateDecision(lockedRev1, CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "r"}, frozenMs); err != nil {
			t.Fatalf("valid accept rejected: %v", err)
		}
	})

	t.Run("restoration and out-of-scope on a revised contract", func(t *testing.T) {
		// Revise to rev2 deferring m2,m3 (keep m1). Then a keep of a deferred id, and an accept of a
		// deferred criterion, are both invalid.
		revised, _ := buildRevisedContract(lockedRev1.CompletionContract,
			CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "r"}, frozenMs, 2)
		lockedRev2 := store.Run{
			CompletionContract:  revised,
			MilestonesFrozen:    frozenJSON(t, "m1", "m2", "m3"),
			MilestonesCompleted: idsJSON(t, "m1"),
		}
		// keep m2 (currently deferred) → restoration → invalid.
		if err := validateDecision(lockedRev2, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "r"}, frozenMs); !errors.Is(err, ErrCompletionDecisionInvalid) {
			t.Fatalf("restoration must be invalid; got %v", err)
		}
		// accept m2.c1 (m2 out of scope) → invalid.
		if err := validateDecision(lockedRev2, CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "r"}, frozenMs); !errors.Is(err, ErrCompletionDecisionInvalid) {
			t.Fatalf("accepting an out-of-scope criterion must be invalid; got %v", err)
		}
	})
}

// TestDecisionAlreadyEncoded pins the idempotency comparison for both decisions: the SAME request
// against the contract it already produced reads true; a different keep/criteria/reason reads false.
func TestDecisionAlreadyEncoded(t *testing.T) {
	frozenMs := milestoneList("m1", "m2", "m3")
	base := contractJSON(t, "m1", "m2", "m3")

	t.Run("partial", func(t *testing.T) {
		dec := CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "reason-A"}
		revised, _ := buildRevisedContract(base, dec, frozenMs, 2)
		locked := store.Run{CompletionContract: revised, MilestonesFrozen: frozenJSON(t, "m1", "m2", "m3")}
		if !decisionAlreadyEncoded(locked, dec, 2) {
			t.Fatal("the same partial must read as already-encoded at rev 2")
		}
		// Different keep set.
		if decisionAlreadyEncoded(locked, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "reason-A"}, 2) {
			t.Fatal("a different keep set must NOT read as already-encoded")
		}
		// Different reason.
		if decisionAlreadyEncoded(locked, CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "other"}, 2) {
			t.Fatal("a different reason must NOT read as already-encoded")
		}
	})

	t.Run("accept", func(t *testing.T) {
		dec := CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "acc"}
		revised, _ := buildRevisedContract(base, dec, frozenMs, 2)
		locked := store.Run{CompletionContract: revised, MilestonesFrozen: frozenJSON(t, "m1", "m2", "m3")}
		if !decisionAlreadyEncoded(locked, dec, 2) {
			t.Fatal("the same accept must read as already-encoded at rev 2")
		}
		if decisionAlreadyEncoded(locked, CompletionDecisionInput{Decision: "accept", Criteria: []string{"m3.c1"}, Reason: "acc"}, 2) {
			t.Fatal("a different criteria set must NOT read as already-encoded")
		}
		if decisionAlreadyEncoded(locked, CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "other"}, 2) {
			t.Fatal("a different reason must NOT read as already-encoded")
		}
	})
}
