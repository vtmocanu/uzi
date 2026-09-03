package forge

import (
	"testing"

	"code.gitea.io/sdk/gitea"
)

// PRD #102 Decision 10. The neutral ListIssuesOptions.State has to be translated per
// driver, and THE TWO DRIVERS DO NOT AGREE ON THE WORD FOR "OPEN".
//
// Why this is a unit test per driver and not an e2e assertion, which is the durable
// point: uzi's neutral vocabulary is opened/closed/all, GitLab's request vocabulary is
// opened/closed/all, and Forgejo's is open/closed/all. GitLab's HAPPENS TO MATCH the
// neutral one, so a driver that forgets to translate is indistinguishable from a
// correct one when it is talking to anything GitLab-shaped — which is what the e2e
// fake forge is. The bug can only appear against Forgejo, and only on the REQUEST
// side; forgejoIssueState already normalises the RESPONSE back to "opened", so nothing
// downstream would notice the filter had silently not applied.
//
// That is a general statement about this repo's e2e fake rather than a fact about
// `state`: any place the two drivers' wire vocabularies diverge is invisible to it.

func TestGitLabIssueStateParam(t *testing.T) {
	// Pass-through, but asserted rather than assumed — if GitLab's vocabulary and the
	// neutral one ever diverge, this is where it should be caught.
	for _, tc := range []struct {
		in   IssueState
		want string
	}{
		{StateAll, "all"},
		{StateOpened, "opened"},
		{StateClosed, "closed"},
		{IssueState("garbage"), "all"}, // unknown falls back to the safe direction
	} {
		if got := gitlabIssueStateParam(tc.in); got != tc.want {
			t.Errorf("gitlabIssueStateParam(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestForgejoIssueStateParam(t *testing.T) {
	for _, tc := range []struct {
		in   IssueState
		want gitea.StateType
	}{
		{StateAll, gitea.StateAll},
		{StateOpened, gitea.StateOpen},
		{StateClosed, gitea.StateClosed},
		{IssueState("garbage"), gitea.StateAll},
	} {
		if got := forgejoIssueStateParam(tc.in); got != tc.want {
			t.Errorf("forgejoIssueStateParam(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE ASSERTION THAT ACTUALLY DISCRIMINATES. A pass-through implementation in the
// Forgejo driver — the natural thing to write, and the thing that passes every
// GitLab-shaped test — sends the literal "opened", which is not a state Forgejo
// recognises. Pinning the INEQUALITY means a future refactor that "simplifies" the two
// mappings into one shared function fails here rather than in production against a
// Forgejo instance nobody on this team runs day to day.
func TestTheTwoDriversDisagreeOnTheWordForOpen(t *testing.T) {
	neutral := string(StateOpened)
	if got := gitlabIssueStateParam(StateOpened); got != neutral {
		t.Errorf("GitLab's request vocabulary should equal the neutral one, got %q vs %q", got, neutral)
	}
	forgejoWord := string(forgejoIssueStateParam(StateOpened))
	if forgejoWord == neutral {
		t.Fatalf("forgejoIssueStateParam(StateOpened) = %q, which EQUALS the neutral %q — "+
			"either Forgejo's vocabulary changed or the translation was replaced by a "+
			"pass-through, and a pass-through sends a state Forgejo does not recognise",
			forgejoWord, neutral)
	}
	if forgejoWord != "open" {
		t.Errorf("forgejoIssueStateParam(StateOpened) = %q, want %q", forgejoWord, "open")
	}
}

// The MUTATE mappers are a separate family from the list-filter mappers above
// (PRD #1034 M1). A close/reopen write is never a filter: there are only two
// outcomes, and every non-StateClosed value must REOPEN — including the zero
// value StateAll ("") a caller-bug can pass. This pins that, and it is a
// regression guard: the Forgejo driver first reused the list-filter
// forgejoIssueStateParam here, whose default returns gitea.StateAll ("all") — an
// invalid mutate state — so the StateAll and "garbage" rows below FAIL against
// that old wiring and pass only with the dedicated forgejoIssueStateMutation.
// All three drivers must agree on the shape: StateClosed closes, else reopens.
func TestIssueStateMutationMappers(t *testing.T) {
	for _, tc := range []struct {
		in           IssueState
		gitlab       string
		forgejo      gitea.StateType
		githubState  string
		githubReason string
	}{
		{StateClosed, "close", gitea.StateClosed, "closed", "completed"},
		{StateOpened, "reopen", gitea.StateOpen, "open", "reopened"},
		// The zero value and any bogus value are out of contract, but a mutate must
		// still resolve to a valid state — reopen is the safe direction.
		{StateAll, "reopen", gitea.StateOpen, "open", "reopened"},
		{IssueState("garbage"), "reopen", gitea.StateOpen, "open", "reopened"},
	} {
		if got := gitlabIssueStateEvent(tc.in); got != tc.gitlab {
			t.Errorf("gitlabIssueStateEvent(%q) = %q, want %q", tc.in, got, tc.gitlab)
		}
		if got := forgejoIssueStateMutation(tc.in); got != tc.forgejo {
			t.Errorf("forgejoIssueStateMutation(%q) = %q, want %q", tc.in, got, tc.forgejo)
		}
		if gotState, gotReason := githubIssueStateMutation(tc.in); gotState != tc.githubState || gotReason != tc.githubReason {
			t.Errorf("githubIssueStateMutation(%q) = (%q, %q), want (%q, %q)",
				tc.in, gotState, gotReason, tc.githubState, tc.githubReason)
		}
	}
}

// StateAll must be the ZERO VALUE, so every pre-M6 caller that never sets State keeps
// getting "all". Decision 10 turns on this: the Closed column and de-label/close
// eviction both need closed issues, and there are callers in forgesvc, selfimprove and
// the seed path that were written before the field existed.
func TestZeroValueStateIsAll(t *testing.T) {
	var opts ListIssuesOptions
	if opts.State != StateAll {
		t.Fatalf("zero-value State = %q, want StateAll (%q) — every existing caller depends on this", opts.State, StateAll)
	}
	if got := gitlabIssueStateParam(opts.State); got != "all" {
		t.Errorf("gitlab default = %q, want all", got)
	}
	if got := forgejoIssueStateParam(opts.State); got != gitea.StateAll {
		t.Errorf("forgejo default = %q, want all", got)
	}
}
