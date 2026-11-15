package workersvc

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func (f *crFix) createRequestedRun(t *testing.T, iid int64, requested *AgentSelection) uuid.UUID {
	t.Helper()
	seedEligibleIssue(t, f.env, f.repo, iid)
	run, err := f.svc.CreateRun(f.env.ctx, f.user, f.repo, iid, "desc", nil, nil, false, nil, requested, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun(%d): %v", iid, err)
	}
	return run.ID
}

func TestCreateRunExplicitAgentSourceLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.env.exec(`UPDATE repos SET required_capabilities = '{docker}' WHERE id = $1`, f.repo)

	own := f.createRequestedRun(t, 93401, &AgentSelection{Source: AgentSourceOwn})
	got := readRequested(t, f.env, own)
	wantRequested(t, got, "own", "explicit")
	if got.implemented.Valid {
		t.Fatalf("explicit create recorded agent_source %v, want NULL", got.implemented)
	}
	if !slices.Equal(got.caps, []string{"docker"}) {
		t.Fatalf("explicit run required_capabilities = %v, want the repo's [docker]", got.caps)
	}

	setRepoDefault(f, f.repo, "repo")
	overridden := f.createRequestedRun(t, 93402, &AgentSelection{Source: AgentSourceOwn, Exclusions: []string{}})
	wantRequested(t, readRequested(t, f.env, overridden), "own", "explicit")
	defaulted := f.createRequestedRun(t, 93403, nil)
	wantRequested(t, readRequested(t, f.env, defaulted), "repo", "repo_default")

	setRepoDefault(f, f.repo, "own")
	repoRun := f.createRequestedRun(t, 93404, &AgentSelection{Source: AgentSourceRepo})
	wantRequested(t, readRequested(t, f.env, repoRun), "repo", "explicit")
	setRepoDefault(f, f.repo, nil)
	wantRequested(t, readRequested(t, f.env, repoRun), "repo", "explicit")
}

func TestCreateRunExplicitAgentSourceRefusalsLiveDB(t *testing.T) {
	f := newCRFix(t)
	for i, tc := range []struct {
		name      string
		seed      *SeededPlan
		requested *AgentSelection
		want      error
	}{
		{"exclusions", nil, &AgentSelection{Source: AgentSourceOwn, Exclusions: []string{"tester"}}, ErrCreateExclusionsNeedPlan},
		{"unknown source", nil, &AgentSelection{Source: "both"}, ErrInvalidSelection},
		{"empty source", nil, &AgentSelection{}, ErrInvalidSelection},
		{"with a seeded plan", &SeededPlan{PlanMD: "do it"}, &AgentSelection{Source: AgentSourceOwn}, ErrInvalidSelection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iid := int64(93411 + i)
			seedEligibleIssue(t, f.env, f.repo, iid)
			_, err := f.svc.CreateRun(f.env.ctx, f.user, f.repo, iid, "desc", nil, nil, false, tc.seed, tc.requested, nil, nil, nil, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateRun err = %v, want %v", err, tc.want)
			}
			var n int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND issue_iid = $2`, f.repo, iid).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("a refused create left %d run rows", n)
			}
		})
	}
}

func TestExplicitAgentSourceRunClaimGateLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.createRequestedRun(t, 93421, &AgentSelection{Source: AgentSourceRepo})
	old := f.guardWorker(t, 0)
	f.refuseClaim(t, old, run, "worker without agent_source_request_v1")
	capable := f.guardWorker(t, 0, capability.AgentSourceRequestV1)
	p := f.acceptClaim(t, capable, run, "worker with agent_source_request_v1")
	if p.RequestedAgentSource == nil || *p.RequestedAgentSource != AgentSourceRepo {
		t.Fatalf("claim requested_agent_source = %v, want repo", p.RequestedAgentSource)
	}
	if p.AgentSelection != nil {
		t.Fatalf("claim agent_selection = %+v, want none before approval", p.AgentSelection)
	}
}

func TestNoSelectionApprovalHonoursExplicitCreateSourceLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	f.liveWorker()
	for _, tc := range []struct {
		name      string
		requested string
		sel       *AgentSelection
		want      string
	}{
		{"explicit own over a repo roster", AgentSourceOwn, nil, AgentSourceOwn},
		{"explicit repo with a roster", AgentSourceRepo, nil, AgentSourceRepo},
		{"a gate choice still wins", AgentSourceOwn, &AgentSelection{Source: AgentSourceRepo}, AgentSourceRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := f.gatedRun()
			f.exec(`UPDATE runs SET agent_source_requested = $2, agent_source_requested_basis = 'explicit', repo_agents = $3::jsonb WHERE id = $1`,
				run, tc.requested, requestedSourceRoster)
			if _, err := f.submit(run, "approve_plan", tc.sel, SubmitInputOptions{}); err != nil {
				t.Fatalf("approve: %v", err)
			}
			implemented, requested, basis, _ := f.approvalState(t, run)
			if !implemented.Valid || implemented.String != tc.want {
				t.Fatalf("agent_source = %v, want %s", implemented, tc.want)
			}
			if requested.String != tc.requested || basis.String != "explicit" {
				t.Fatalf("approval changed the request to (%v, %v)", requested, basis)
			}
		})
	}
}
