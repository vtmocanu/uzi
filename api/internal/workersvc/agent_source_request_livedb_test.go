package workersvc

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

type requestedCols struct {
	source, basis, implemented pgtype.Text
	caps                       []string
}

func readRequested(t *testing.T, env codexTestEnv, runID uuid.UUID) requestedCols {
	t.Helper()
	var c requestedCols
	if err := env.pool.QueryRow(env.ctx, `SELECT agent_source_requested, agent_source_requested_basis, agent_source, required_capabilities FROM runs WHERE id = $1`, runID).
		Scan(&c.source, &c.basis, &c.implemented, &c.caps); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return c
}

func wantRequested(t *testing.T, got requestedCols, source, basis string) {
	t.Helper()
	if source == "" {
		if got.source.Valid || got.basis.Valid {
			t.Fatalf("requested = (%v, %v), want (NULL, NULL)", got.source, got.basis)
		}
		return
	}
	if got.source.String != source || !got.source.Valid || got.basis.String != basis || !got.basis.Valid {
		t.Fatalf("requested = (%v, %v), want (%s, %s)", got.source, got.basis, source, basis)
	}
}

func setRepoDefault(f *crFix, repoID uuid.UUID, value any) {
	f.env.exec(`UPDATE repos SET agent_source_default = $2 WHERE id = $1`, repoID, value)
}

func (f *crFix) createIssueRun(t *testing.T, iid int64, seed *SeededPlan) uuid.UUID {
	t.Helper()
	seedEligibleIssue(t, f.env, f.repo, iid)
	run, err := f.svc.CreateRun(f.env.ctx, f.user, f.repo, iid, "desc", nil, nil, false, seed, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun(%d): %v", iid, err)
	}
	return run.ID
}

func TestCreateRunFreezesAgentSourceRequestLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.env.exec(`UPDATE repos SET required_capabilities = '{docker}' WHERE id = $1`, f.repo)

	otherUser, _, otherRepo := f.env.seedCodexInfra(t)
	t.Cleanup(func() {
		f.env.exec(`DELETE FROM workers WHERE user_id = $1`, otherUser)
		f.env.exec(`DELETE FROM users WHERE id = $1`, otherUser)
	})
	setRepoDefault(f, otherRepo, "repo")

	unset := f.createIssueRun(t, 93101, nil)
	got := readRequested(t, f.env, unset)
	wantRequested(t, got, "", "")
	if !slices.Equal(got.caps, []string{"docker"}) {
		t.Fatalf("unset run required_capabilities = %v, want the repo's [docker]", got.caps)
	}

	setRepoDefault(f, f.repo, "own")
	owned := f.createIssueRun(t, 93102, nil)
	got = readRequested(t, f.env, owned)
	wantRequested(t, got, "own", "repo_default")
	if !slices.Equal(got.caps, []string{"docker"}) {
		t.Fatalf("requested run required_capabilities = %v, want the repo's [docker]", got.caps)
	}

	setRepoDefault(f, f.repo, "repo")
	wantRequested(t, readRequested(t, f.env, owned), "own", "repo_default")
	wantRequested(t, readRequested(t, f.env, unset), "", "")
	repoRun := f.createIssueRun(t, 93103, nil)
	wantRequested(t, readRequested(t, f.env, repoRun), "repo", "repo_default")

	seededOwn := f.createIssueRun(t, 93104, &SeededPlan{PlanMD: "do the thing", Selection: &AgentSelection{Source: AgentSourceOwn, Exclusions: []string{}}})
	wantRequested(t, readRequested(t, f.env, seededOwn), "own", "explicit")
	seededBare := f.createIssueRun(t, 93105, &SeededPlan{PlanMD: "do the thing"})
	wantRequested(t, readRequested(t, f.env, seededBare), "repo", "repo_default")

	setRepoDefault(f, f.repo, nil)
	seededRepo := f.createIssueRun(t, 93106, &SeededPlan{PlanMD: "do the thing", Selection: &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{}}})
	wantRequested(t, readRequested(t, f.env, seededRepo), "repo", "explicit")
	wantRequested(t, readRequested(t, f.env, repoRun), "repo", "repo_default")
	otherUnaffected := f.createIssueRun(t, 93107, nil)
	wantRequested(t, readRequested(t, f.env, otherUnaffected), "", "")
}

func TestAgentSourceRequestClaimGateLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*crFix)
		proto []string
	}{
		{"default", func(*crFix) {}, nil},
		{"capability_aware off", func(f *crFix) { f.svc.capabilitySettings = fakeCapabilitySettings{on: false} }, nil},
		{"old worker reporting every other protocol capability", func(*crFix) {}, []string{
			capability.CompletionInterlockV1, capability.CIConfigGuardV1, capability.CodexHarnessV1, capability.CodexRuntimeV2,
			capability.RecoveryArchiveV1, capability.CrossCheckV1, capability.JobRunnerV1, capability.GateRevisionV1,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCRFix(t)
			tc.setup(f)
			setRepoDefault(f, f.repo, "own")
			run := f.createIssueRun(t, 93201, nil)
			old := f.guardWorker(t, 0, tc.proto...)
			f.refuseClaim(t, old, run, "worker without agent_source_request_v1")

			capable := f.guardWorker(t, 0, capability.AgentSourceRequestV1)
			p := f.acceptClaim(t, capable, run, "worker with agent_source_request_v1")
			if p.RequestedAgentSource == nil || *p.RequestedAgentSource != AgentSourceOwn {
				t.Fatalf("claim requested_agent_source = %v, want own", p.RequestedAgentSource)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["requested_agent_source"] != "own" {
				t.Fatalf("claim wire requested_agent_source = %v, want own", wire["requested_agent_source"])
			}
		})
	}
}

func TestUnsetRunStaysClaimableByOldWorkerLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.createIssueRun(t, 93301, nil)
	old := f.guardWorker(t, 0)
	p := f.acceptClaim(t, old, run, "old worker, unset run")
	if p.RequestedAgentSource != nil {
		t.Fatalf("unset claim carries requested_agent_source %q", *p.RequestedAgentSource)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["requested_agent_source"]; ok {
		t.Fatal("an unset claim must not carry the requested_agent_source key")
	}
}

func (f *gateFix) setRequested(runID uuid.UUID, source string, roster string) {
	f.t.Helper()
	f.exec(`UPDATE runs SET agent_source_requested = $2, agent_source_requested_basis = CASE WHEN $2::text IS NULL THEN NULL ELSE 'repo_default' END,
	        repo_agents = $3::jsonb WHERE id = $1`, runID, pgtype.Text{String: source, Valid: source != ""}, roster)
}

func (f *gateFix) approvalState(t *testing.T, runID uuid.UUID) (implemented, requested, basis, body pgtype.Text) {
	t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT r.agent_source, r.agent_source_requested, r.agent_source_requested_basis, i.body
	        FROM runs r JOIN run_user_inputs i ON i.run_id = r.id AND i.kind = 'approve_plan'
	        WHERE r.id = $1 ORDER BY i.id DESC LIMIT 1`, runID).Scan(&implemented, &requested, &basis, &body); err != nil {
		t.Fatalf("read approval state: %v", err)
	}
	return implemented, requested, basis, body
}

func TestNoSelectionApprovalMaterializesRequestedSourceLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	f.liveWorker()
	roster := `[{"name":"site-plan-check","description":"repo plan check"}]`
	for _, tc := range []struct {
		name      string
		requested string
		roster    string
		sel       *AgentSelection
		want      string
	}{
		{"own requested, roster present", "own", roster, nil, AgentSourceOwn},
		{"repo requested, roster present", "repo", roster, nil, AgentSourceRepo},
		{"repo requested, no roster", "repo", `[]`, nil, AgentSourceOwn},
		{"explicit own wins over a repo request", "repo", roster, &AgentSelection{Source: AgentSourceOwn}, AgentSourceOwn},
		{"explicit repo wins over an own request", "own", roster, &AgentSelection{Source: AgentSourceRepo}, AgentSourceRepo},
		{"unset approval stays a bare approval", "", roster, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := f.gatedRun()
			f.setRequested(run, tc.requested, tc.roster)
			if _, err := f.submit(run, "approve_plan", tc.sel, SubmitInputOptions{}); err != nil {
				t.Fatalf("approve: %v", err)
			}
			implemented, requested, basis, body := f.approvalState(t, run)
			if tc.requested == "" {
				if requested.Valid || basis.Valid {
					t.Fatalf("requested columns = (%v, %v), want NULL", requested, basis)
				}
			} else if requested.String != tc.requested || basis.String != "repo_default" {
				t.Fatalf("approval changed the request to (%v, %v)", requested, basis)
			}
			if tc.want == "" {
				if implemented.Valid {
					t.Fatalf("agent_source = %q, want NULL for an unset bare approval", implemented.String)
				}
				return
			}
			if !implemented.Valid || implemented.String != tc.want {
				t.Fatalf("agent_source = %v, want %s", implemented, tc.want)
			}
			var sel AgentSelection
			if err := json.Unmarshal([]byte(body.String), &sel); err != nil || sel.Source != tc.want {
				t.Fatalf("approve_plan body = %q, want an explicit %s selection", body.String, tc.want)
			}
		})
	}
}
