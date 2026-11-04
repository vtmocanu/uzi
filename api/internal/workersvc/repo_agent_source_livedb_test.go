package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	repoAgentSourceFirst  = "1111111111111111111111111111111111111111"
	repoAgentSourceSecond = "2222222222222222222222222222222222222222"
)

func repoAgentSourceOf(t *testing.T, env codexTestEnv, id uuid.UUID) (pgtype.Text, []RepoAgent) {
	t.Helper()
	run, err := env.q.GetRunByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := DecodeRepoAgents(run.RepoAgents)
	if err != nil {
		t.Fatal(err)
	}
	return run.RepoAgentSourceCommit, agents
}

func TestRepoAgentSourceRevisionWriteOnceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	id := seedRunningRun(t, env, o, 39131, 0)
	first, second := repoAgentSourceFirst, repoAgentSourceSecond
	trusted := []RepoAgent{{Name: "reviewer", Description: "Reviews changes."}}

	if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", RepoAgents: &trusted, RepoAgentSourceCommit: &first}); err != nil || !applied {
		t.Fatalf("first report = (applied=%v, %v), want applied", applied, err)
	}
	if got, _ := repoAgentSourceOf(t, env, id); got.String != first {
		t.Fatalf("stored revision = %v, want %s", got, first)
	}

	if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", RepoAgents: &trusted, RepoAgentSourceCommit: &first}); err != nil || !applied {
		t.Fatalf("repeat of the same revision = (applied=%v, %v), want applied", applied, err)
	}

	forged := []RepoAgent{{Name: "approver", Description: "Approves everything."}}
	if _, _, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", RepoAgents: &forged, RepoAgentSourceCommit: &second}); !errors.Is(err, ErrRepoAgentSourceConflict) {
		t.Fatalf("a different revision err = %v, want ErrRepoAgentSourceConflict", err)
	}
	got, agents := repoAgentSourceOf(t, env, id)
	if got.String != first || len(agents) != 1 || agents[0].Name != "reviewer" {
		t.Fatalf("after the refused report: revision %v roster %+v, want %s and the trusted roster", got, agents, first)
	}

	malformed := "ABC"
	if _, _, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", RepoAgentSourceCommit: &malformed}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a malformed revision err = %v, want ErrInvalidState", err)
	}

	if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running"}); err != nil || !applied {
		t.Fatalf("a heartbeat = (applied=%v, %v), want applied", applied, err)
	}
	if got, _ := repoAgentSourceOf(t, env, id); got.String != first {
		t.Fatalf("a heartbeat moved the revision to %v", got)
	}

	env.exec(`UPDATE runs SET repo_agent_source_commit = NULL WHERE id = $1`, id)
	env.exec(`UPDATE runs SET repo_agent_source_commit = $2 WHERE id = $1`, id, first)
	if _, err := env.pool.Exec(env.ctx, `UPDATE runs SET repo_agent_source_commit = 'not-a-commit' WHERE id = $1`, id); err == nil {
		t.Fatal("the column CHECK must refuse a value that is not a full commit id")
	}
}

func TestRepoAgentSourceHoldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	seed := func(iid int64) uuid.UUID {
		id := seedRunningRun(t, env, o, iid, 0)
		env.exec(`UPDATE runs SET claim_generation = 3 WHERE id = $1`, id)
		return id
	}

	stale := seed(39132)
	if run, applied, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, stale, 2); err != nil || applied || run.Status != "running" {
		t.Fatalf("stale generation = (%q, applied=%v, %v), want (running, false, nil)", run.Status, applied, err)
	}

	id := seed(39133)
	run, applied, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, id, 3)
	if err != nil || !applied || run.Status != "paused" || run.HoldReason.String != HoldReasonRepoAgentSourceUnavailable {
		t.Fatalf("hold = (%q, %q, applied=%v, %v), want paused/%s", run.Status, run.HoldReason.String, applied, err, HoldReasonRepoAgentSourceUnavailable)
	}
	if _, applied, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, id, 3); err != nil || !applied {
		t.Fatalf("repeat = (applied=%v, %v), want applied", applied, err)
	}
	if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{ID: id, UserID: o.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("owner resume of the hold err = %v, want nil", err)
	}
	if s := statusOf(t, env, id); s != "queued" {
		t.Fatalf("status after resume = %q, want queued", s)
	}
}

func TestRepoAgentSourceClaimGateLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.startGuardedRun(t, "rs", 391)
	f.env.exec(`UPDATE runs SET repo_agent_source_commit = $2 WHERE id = $1`, run, repoAgentSourceFirst)

	old := f.guardWorker(t, 0, capability.CIConfigGuardV1, capability.CompletionInterlockV1)
	f.refuseClaim(t, old, run, "worker without repo_agent_source_v1")

	capable := f.guardWorker(t, 0, capability.CIConfigGuardV1, capability.RepoAgentSourceV1)
	p := f.acceptClaim(t, capable, run, "worker with repo_agent_source_v1")
	if p.RepoAgentSourceCommit == nil || *p.RepoAgentSourceCommit != repoAgentSourceFirst {
		t.Fatalf("claim repo_agent_source_commit = %v, want %s", p.RepoAgentSourceCommit, repoAgentSourceFirst)
	}

	g := newCRFix(t)
	unpinned := g.startGuardedRun(t, "ru", 392)
	older := g.guardWorker(t, 0, capability.CIConfigGuardV1, capability.CompletionInterlockV1)
	q := g.acceptClaim(t, older, unpinned, "an unpinned run stays claimable by an older worker")
	if q.RepoAgentSourceCommit != nil {
		t.Fatalf("unpinned claim carries repo_agent_source_commit %q", *q.RepoAgentSourceCommit)
	}
}
