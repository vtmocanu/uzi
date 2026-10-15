package poller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMergedPredecessorKeysetFairnessLiveDB(t *testing.T) {
	a, b := newMergedPollEnv(t), newMergedPollEnv(t)
	repos := []uuid.UUID{a.repo, b.repo}
	slices.SortFunc(repos, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for _, spec := range []struct {
		e     *mergedPollEnv
		start int64
	}{{a, 1000}, {b, 2000}} {
		spec.e.exec("UPDATE runs SET status='completed',claim_generation=2,mr_iid=$2 WHERE id=$1", spec.e.run, spec.start)
		for n := int64(1); n < 150; n++ {
			id := uuid.New()
			spec.e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,status,claim_generation,mr_iid,mr_state) VALUES($1,$2,$3,'prompt','t','d','completed',2,$4,'merged')", id, spec.e.worker.UserID, spec.e.repo, spec.start+n)
			spec.e.addHold(uuid.New(), id, spec.e.worker.ID, 1)
		}
	}
	var reads []int64
	a.forge.read = func(ctx context.Context, p, i int64) (forge.MergeRequest, error) {
		reads = append(reads, i)
		return forge.MergeRequest{IID: i, State: forge.MRStateOpened}, nil
	}
	a.svc.SetMergedPredecessorRecovery(a.q, a.recovery)
	// The shared service serializes phase/cursor ownership even with two callers.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.svc.ReconcileMergedPredecessors(context.Background(), repos) }()
	}
	wg.Wait()
	if len(reads) != 300 {
		t.Fatalf("reads=%d", len(reads))
	}
	seen := map[int64]bool{}
	firstA := repos[0] == a.repo
	for index, iid := range reads {
		if seen[iid] {
			t.Fatalf("duplicate attempt %d", iid)
		}
		seen[iid] = true
		wantA := index%2 == 0
		if !firstA {
			wantA = !wantA
		}
		if (iid < 2000) != wantA {
			t.Fatalf("round robin at %d: iid=%d", index, iid)
		}
	}
	a.svc.ReconcileMergedPredecessors(context.Background(), repos)
	if len(reads) != 300 {
		t.Fatal("exhaustion duplicated candidates")
	}
	a.svc.ReconcileMergedPredecessors(context.Background(), repos)
	if len(reads) != 400 {
		t.Fatal("OPEN refusals lost on wrap")
	}
}

type mergedEnabledSet struct {
	q   *store.Queries
	ids map[uuid.UUID]bool
}

func (r mergedEnabledSet) ListEnabledReposWithConnections(ctx context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	rows, err := r.q.ListEnabledReposWithConnections(ctx)
	var result []store.ListEnabledReposWithConnectionsRow
	for _, row := range rows {
		if r.ids[row.ID] {
			result = append(result, row)
		}
	}
	return result, err
}

func TestMergedPredecessorAllEnabledAfterOrdinaryLiveDB(t *testing.T) {
	e := newMergedPollEnv(t)
	ids := map[uuid.UUID]bool{e.repo: true}
	holds := []uuid.UUID{e.hold}
	e.exec("UPDATE runs SET status='completed',claim_generation=2 WHERE id=$1", e.run)
	for n := 1; n < defaultMaxConcurrency+2; n++ {
		repo, run, hold := uuid.New(), uuid.New(), uuid.New()
		ids[repo] = true
		holds = append(holds, hold)
		e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES($1,$2,$3,$4,'https://forge.test/g/r',true)", repo, e.conn, int64(42+n), repo.String())
		e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,status,claim_generation,mr_iid,mr_state) VALUES($1,$2,$3,'prompt','t','d','completed',2,7,'merged')", run, e.worker.UserID, repo)
		e.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state) VALUES($1,$2,$3,$4,1,$5,'original',$5,$4,true,'open')", hold, e.worker.UserID, repo, run, e.worker.ID)
	}
	e.engine.q = mergedEnabledSet{e.q, ids}
	var mu sync.Mutex
	completed := map[int64]bool{}
	e.forge.list = func(_ context.Context, p int64) ([]forge.Issue, error) {
		mu.Lock()
		completed[p] = true
		mu.Unlock()
		return nil, errors.New("ordinary sync unavailable")
	}
	e.forge.read = func(_ context.Context, p, i int64) (forge.MergeRequest, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(completed) != len(ids) || !completed[p] {
			t.Errorf("merge read preceded ordinary ListIssues completion: %d/%d", len(completed), len(ids))
		}
		return forge.MergeRequest{IID: i, State: forge.MRStateMerged}, nil
	}
	e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
	e.engine.PollOnce(e.ctx)
	if e.forge.reads != len(ids) {
		t.Fatalf("visited %d of %d enabled repos", e.forge.reads, len(ids))
	}
	for _, hold := range holds {
		if e.state(hold) != "discarded" {
			t.Fatal("enabled repository hold not settled")
		}
	}
}

func TestMergedPredecessorParentCancellationRetainsHoldLiveDB(t *testing.T) {
	e := newMergedPollEnv(t)
	e.exec("UPDATE runs SET status='completed',claim_generation=2 WHERE id=$1", e.run)
	parent, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.forge.read = func(_ context.Context, _, i int64) (forge.MergeRequest, error) {
		cancel()
		return forge.MergeRequest{IID: i, State: forge.MRStateMerged}, nil
	}
	e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
	e.engine.PollOnce(parent)
	if e.forge.reads != 1 || e.state(e.hold) != "open" {
		t.Fatal("parent cancellation accepted forged merge or lost hold")
	}
}
