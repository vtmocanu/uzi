package workersvc

import (
	"errors"
	"testing"
	"time"

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
	if run, applied, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, stale, 2, ""); err != nil || applied || run.Status != "running" {
		t.Fatalf("stale generation = (%q, applied=%v, %v), want (running, false, nil)", run.Status, applied, err)
	}

	id := seed(39133)
	run, applied, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, id, 3, "")
	if err != nil || !applied || run.Status != "paused" || run.HoldReason.String != HoldReasonRepoAgentSourceUnavailable {
		t.Fatalf("hold = (%q, %q, applied=%v, %v), want paused/%s", run.Status, run.HoldReason.String, applied, err, HoldReasonRepoAgentSourceUnavailable)
	}
	if _, applied, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, id, 3, ""); err != nil || !applied {
		t.Fatalf("repeat = (applied=%v, %v), want applied", applied, err)
	}
	if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{ID: id, UserID: o.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("owner resume of the hold err = %v, want nil", err)
	}
	if s := statusOf(t, env, id); s != "queued" {
		t.Fatalf("status after resume = %q, want queued", s)
	}
	if got, _ := repoAgentSourceOf(t, env, id); got.Valid {
		t.Fatalf("a hold without a commit recorded revision %q", got.String)
	}
	malformed := seed(39134)
	if _, _, err := svc.SetRunRepoAgentSourceHold(env.ctx, wkr, malformed, 3, "ABC"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a malformed hold commit err = %v, want ErrInvalidState", err)
	}
}

func TestRepoAgentSourceHoldPinsRevisionAcrossResumeLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	run := f.startPlainPromptRun(t)
	w := f.protocolWorker(t, 0, capability.RepoAgentSourceV1)
	first := f.acceptClaim(t, w, run, "first claim")
	if first.RepoAgentSourceCommit != nil {
		t.Fatalf("first claim carries repo_agent_source_commit %q", *first.RepoAgentSourceCommit)
	}
	gen := first.ClaimGeneration
	if _, applied, err := f.svc.SetState(ctx, w, run, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("running report = (applied=%v, %v), want applied", applied, err)
	}

	held, applied, err := f.svc.SetRunRepoAgentSourceHold(ctx, w, run, gen, repoAgentSourceFirst)
	if err != nil || !applied || held.Status != "paused" || held.RepoAgentSourceCommit.String != repoAgentSourceFirst {
		t.Fatalf("hold = (%q, %v, applied=%v, %v), want paused with %s", held.Status, held.RepoAgentSourceCommit, applied, err, repoAgentSourceFirst)
	}
	if _, err := f.env.q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: run, UserID: f.user, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("owner resume err = %v", err)
	}

	resumed := f.acceptClaim(t, w, run, "claim after the owner resumed the hold")
	if resumed.RepoAgentSourceCommit == nil || *resumed.RepoAgentSourceCommit != repoAgentSourceFirst {
		t.Fatalf("resumed claim repo_agent_source_commit = %v, want the held revision %s", resumed.RepoAgentSourceCommit, repoAgentSourceFirst)
	}
	gen = resumed.ClaimGeneration
	moved := repoAgentSourceSecond
	if _, _, err := f.svc.SetState(ctx, w, run, StateRequest{State: "running", ClaimGeneration: &gen, RepoAgentSourceCommit: &moved}); !errors.Is(err, ErrRepoAgentSourceConflict) {
		t.Fatalf("a report of the moved default branch err = %v, want ErrRepoAgentSourceConflict", err)
	}
	if _, applied, err := f.svc.SetState(ctx, w, run, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("running report = (applied=%v, %v), want applied", applied, err)
	}
	again, applied, err := f.svc.SetRunRepoAgentSourceHold(ctx, w, run, gen, moved)
	if err != nil || !applied || again.RepoAgentSourceCommit.String != repoAgentSourceFirst {
		t.Fatalf("second hold = (%v, applied=%v, %v), want the first revision %s kept", again.RepoAgentSourceCommit, applied, err, repoAgentSourceFirst)
	}
}

func TestRepoAgentSourceClaimGateLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.startGuardedRun(t, "rs", 391)
	f.env.exec(`UPDATE runs SET repo_agent_source_commit = $2 WHERE id = $1`, run, repoAgentSourceFirst)

	old := f.protocolWorker(t, 0, capability.CIConfigGuardV1, capability.CompletionInterlockV1)
	f.refuseClaim(t, old, run, "worker without repo_agent_source_v1")

	capable := f.protocolWorker(t, 0, capability.CIConfigGuardV1, capability.RepoAgentSourceV1)
	p := f.acceptClaim(t, capable, run, "worker with repo_agent_source_v1")
	if p.RepoAgentSourceCommit == nil || *p.RepoAgentSourceCommit != repoAgentSourceFirst {
		t.Fatalf("claim repo_agent_source_commit = %v, want %s", p.RepoAgentSourceCommit, repoAgentSourceFirst)
	}
}

func TestRepoAgentSourceStampedUnpinnedClaimGateLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.startPlainPromptRun(t)
	stored, err := f.env.q.GetRunByID(f.env.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.RepoAgentSourceRequired || stored.RepoAgentSourceCommit.Valid {
		t.Fatalf("a new prompt run: required=%v revision=%v, want stamped and unpinned", stored.RepoAgentSourceRequired, stored.RepoAgentSourceCommit)
	}

	old := f.protocolWorker(t, 0, capability.CompletionInterlockV1, capability.CIConfigGuardV1, capability.CodexHarnessV1,
		capability.CodexRuntimeV2, capability.RecoveryArchiveV1, capability.CrossCheckV1, capability.JobRunnerV1)
	eligibility, err := f.svc.WorkerEligibilityForHealth(f.env.ctx, time.Now(), run)
	if err != nil {
		t.Fatal(err)
	}
	if eligibility.Claimable != 0 {
		t.Fatalf("claimable workers = %d with only a worker lacking repo_agent_source_v1 online, want 0", eligibility.Claimable)
	}
	f.refuseClaim(t, old, run, "a stamped unpinned first claim by a worker without repo_agent_source_v1")

	health, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, codexCuratedModelsSlice())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range health {
		if r.ID == run {
			found = r.RepoAgentSourceRequired
		}
	}
	if !found {
		t.Fatal("ListActiveRunsForHealth must list the queued run with repo_agent_source_required")
	}

	capable := f.protocolWorker(t, 0, capability.RepoAgentSourceV1)
	if eligibility, err = f.svc.WorkerEligibilityForHealth(f.env.ctx, time.Now(), run); err != nil {
		t.Fatal(err)
	}
	if eligibility.Claimable != 1 {
		t.Fatalf("claimable workers = %d after a repo_agent_source_v1 worker came online, want 1", eligibility.Claimable)
	}
	p := f.acceptClaim(t, capable, run, "a stamped unpinned first claim by a worker with repo_agent_source_v1")
	if p.RepoAgentSourceCommit != nil {
		t.Fatalf("unpinned claim carries repo_agent_source_commit %q", *p.RepoAgentSourceCommit)
	}
}

func TestRepoAgentSourceUnstampedLegacyRunClaimableLiveDB(t *testing.T) {
	f := newCRFix(t)
	run := f.startPlainPromptRun(t)
	f.env.exec(`UPDATE runs SET repo_agent_source_required = false WHERE id = $1`, run)
	old := f.protocolWorker(t, 0, capability.CompletionInterlockV1)
	p := f.acceptClaim(t, old, run, "an unstamped legacy run by a worker without repo_agent_source_v1")
	if p.RepoAgentSourceCommit != nil {
		t.Fatalf("legacy claim carries repo_agent_source_commit %q", *p.RepoAgentSourceCommit)
	}
}

func TestRepoAgentSourcePeerDeferralLiveDB(t *testing.T) {
	f := newCRFix(t)
	capable := f.protocolWorker(t, 4, capability.CIConfigGuardV1, capability.RepoAgentSourceV1)
	f.protocolWorker(t, 4, capability.CIConfigGuardV1)
	busy := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, $3, 'issue', 773, 't', 'd', 'running', $4)`, busy, f.user, f.repo, capable.ID)
	run := f.startGuardedRun(t, "k", 100)
	f.acceptClaim(t, capable, run, "busy capable worker with an idle peer lacking repo_agent_source_v1")
}

func TestRepoAgentSourcePlacementMirrorsLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, f.user)
	run := f.startGuardedRun(t, "k", 100)
	plain := f.startPlainPromptRun(t)
	f.env.exec(`UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = ANY($1)`, []uuid.UUID{run, plain})

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	gapFor := func(target uuid.UUID) bool {
		t.Helper()
		rows, err := f.env.q.ListUnplaceableQueuedRunsForEphemeral(ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == target {
				return true
			}
		}
		return false
	}
	gap := func() bool { return gapFor(run) }
	saturated := func() bool {
		t.Helper()
		rows, err := f.env.q.ListSaturationQueuedRunsForEphemeral(ctx, store.ListSaturationQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == run {
				return true
			}
		}
		return false
	}

	f.protocolWorker(t, 0, capability.CIConfigGuardV1)
	if !gap() {
		t.Fatal("with only workers lacking repo_agent_source_v1 online the run must be a capability-gap candidate")
	}
	if saturated() {
		t.Fatal("with only workers lacking repo_agent_source_v1 online the run is a gap, not slot saturation")
	}
	if !gapFor(plain) {
		t.Fatal("a stamped unpinned run with no other demand must be a capability-gap candidate when only workers lacking repo_agent_source_v1 are online")
	}

	capable := f.protocolWorker(t, 1, capability.CIConfigGuardV1, capability.RepoAgentSourceV1)
	if gap() || saturated() || gapFor(plain) {
		t.Fatal("a capable worker with room is online: neither a gap nor saturation")
	}

	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, $3, 'issue', 774, 't', 'd', 'running', $4)`, uuid.New(), f.user, f.repo, capable.ID)
	if gap() {
		t.Fatal("a capable worker is online: not a capability gap")
	}
	if !saturated() {
		t.Fatal("the capable worker is full and the other cannot serve the run: it must be listed as saturated")
	}
}
