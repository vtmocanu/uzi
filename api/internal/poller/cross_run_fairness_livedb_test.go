package poller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type crossRunQueryTrace struct {
	*store.Queries
	keys []store.ListCrossRunTargetsParams
}

func (q *crossRunQueryTrace) ListCrossRunTargets(ctx context.Context, p store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error) {
	q.keys = append(q.keys, p)
	return q.Queries.ListCrossRunTargets(ctx, p)
}

func crossRunFreshService(t *testing.T, e *mergedPollEnv) *forgesvc.Service {
	t.Helper()
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	s := forgesvc.NewWithForgeBuilder(e.q, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return e.forge, nil })
	s.SetMergedPredecessorRecovery(e.q, e.recovery)
	return s
}

func TestCrossRunDiscardFairnessAndRestartLiveDB(t *testing.T) {
	t.Run("refused_prefix_two_witnesses_wrap_restart_prune", func(t *testing.T) {
		e, w := newCrossRunDiscardEnv(t)
		second := uuid.New()
		crossRunWitness(e, second, 8)
		firstIID := int64(7)
		firstWitness, laterWitness := w, second
		if slices.Compare(w[:], second[:]) > 0 {
			firstIID = 8
			firstWitness, laterWitness = second, w
		}
		e.exec("UPDATE runs SET created_at='2020-01-06',finished_at='2020-01-07' WHERE id=$1", laterWitness)
		// Only the second witness postdates this target's finish. Both witnesses
		// must traverse the same permanently refused prefix before eligible work.
		laterTarget, laterHold := uuid.New(), uuid.New()
		e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,
			claim_generation,created_at,finished_at) VALUES($1,$2,$3,'issue',101,'later target','d','cancelled',
			1,'2019-12-31','2020-01-05')`, laterTarget, e.worker.UserID, e.repo)
		e.addHold(laterHold, laterTarget, e.worker.ID, 1)
		// Every newer target is permanently refused: its hold generation is
		// beyond the terminal generation. These are admitted legacy rows.
		var refused []uuid.UUID
		for n := 1; n <= 104; n++ {
			run, hold := uuid.New(), uuid.New()
			e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,
				claim_generation,created_at,finished_at) VALUES($1,$2,$3,'issue',101,'refused','d','cancelled',
				1,'2020-01-01'::timestamptz + $4 * interval '1 second','2020-01-02')`, run, e.worker.UserID, e.repo, n)
			e.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,
				original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state)
				VALUES($1,$2,$3,$4,2,$5,'legacy',$5,$4,false,'open')`, hold, e.worker.UserID, e.repo, run, e.worker.ID)
			refused = append(refused, hold)
		}
		trace := &crossRunQueryTrace{Queries: e.q}
		e.svc.SetMergedPredecessorRecovery(trace, e.recovery)
		e.forge.reads = 0
		var observed []int64
		e.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
			observed = append(observed, iid)
			return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
		}
		// One target tuple and one fresh GET per phase. Refusal must advance it.
		for phase := 0; phase < 105; phase++ {
			before := len(trace.keys)
			e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
			if len(trace.keys) != before+1 || len(observed) != phase+1 || observed[phase] != firstIID {
				t.Fatalf("phase %d: target queries=%d observations=%d", phase, len(trace.keys)-before, len(observed))
			}
			if phase < 104 && e.state(e.hold) != "open" {
				t.Fatalf("eligible oldest reached early at phase %d", phase)
			}
		}
		if e.state(e.hold) != "discarded" || e.state(laterHold) != "open" {
			t.Fatal("first witness must reach only its own eligible target")
		}
		crossRunAudit(t, e, e.hold, e.run, firstWitness, firstIID)
		for i, hold := range refused {
			if e.state(hold) != "open" {
				t.Fatalf("refused target %d changed", i)
			}
		}
		// The first witness exhausts its target keyset; the second starts at NULL.
		e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		last := trace.keys[len(trace.keys)-1]
		if observed[len(observed)-1] == firstIID || last.AfterCreatedAt.Valid || last.AfterRunID != uuid.Nil {
			t.Fatal("second witness inherited first witness target cursor")
		}
		// Disabling prunes both cursors; re-enabling must start the first witness
		// and the newest refused target again, using actual generated queries.
		e.svc.ReconcileMergedPredecessors(e.ctx, nil)
		e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		last = trace.keys[len(trace.keys)-1]
		if observed[len(observed)-1] != firstIID || last.AfterCreatedAt.Valid || last.AfterRunID != uuid.Nil {
			t.Fatal("disabled repo retained continuation")
		}
		restarted := crossRunFreshService(t, e)
		restarted.SetMergedPredecessorRecovery(trace, e.recovery)
		restarted.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		last = trace.keys[len(trace.keys)-1]
		if last.AfterCreatedAt.Valid || last.AfterRunID != uuid.Nil || observed[len(observed)-1] != firstIID {
			t.Fatal("restart did not rediscover durable refused work")
		}
		// Traverse both complete remaining inventories and exhaustion so the
		// next phase wraps back to the first witness without an in-phase repeat.
		wrapped := crossRunFreshService(t, e)
		start := len(observed)
		for phase := 0; phase < 211; phase++ {
			wrapped.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		}
		if len(observed)-start != 212 {
			t.Fatalf("wrapped observations=%d want 212", len(observed)-start)
		}
		otherIID := int64(7)
		if firstIID == 7 {
			otherIID = 8
		}
		for i, iid := range observed[start:] {
			want := firstIID
			if i >= 105 && i < 211 {
				want = otherIID
			}
			if iid != want {
				t.Fatalf("first wrap mismatch at %d: GET PR=%d want %d", i, iid, want)
			}
		}
		if e.state(laterHold) != "discarded" {
			t.Fatal("second witness refused prefix starved its eligible target")
		}
		var exactLater bool
		if err := e.pool.QueryRow(e.ctx, `SELECT
			merged_predecessor_discard_binding->>'witness_run_id'=$2::text
			AND merged_predecessor_discard_binding->>'target_run_id'=$3::text
			AND (merged_predecessor_discard_binding->>'target_finished_at')::timestamptz='2020-01-05'::timestamptz
			AND (merged_predecessor_discard_binding->>'witness_created_at')::timestamptz='2020-01-06'::timestamptz
			FROM recovery_custody_holds WHERE id=$1`, laterHold, laterWitness.String(), laterTarget.String()).Scan(&exactLater); err != nil || !exactLater {
			t.Fatalf("later target binding mismatch: exact=%v err=%v", exactLater, err)
		}
		t.Logf("refused targets=104 eligible=2 distinct witnesses=2; both witnesses reach eligible work, all refused holds retained")
	})
	t.Run("more_than_100_witnesses", func(t *testing.T) {
		e, w := newCrossRunDiscardEnv(t)
		e.exec("DELETE FROM runs WHERE id=$1", w)
		ids := make([]uuid.UUID, 106)
		for i := range ids {
			ids[i] = uuid.New()
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		for i, id := range ids {
			crossRunWitness(e, id, int64(1000+i))
		}
		var observed []int64
		e.forge.reads = 0
		e.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
			observed = append(observed, iid)
			state := forge.MRStateOpened
			if iid == 1105 {
				state = forge.MRStateMerged
			}
			return forge.MergeRequest{IID: iid, State: state}, nil
		}
		e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		if len(observed) != 100 || e.state(e.hold) != "open" {
			t.Fatalf("first phase GETs=%d hold=%s", len(observed), e.state(e.hold))
		}
		e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		if len(observed) != 106 || e.state(e.hold) != "discarded" {
			t.Fatalf("second phase GETs=%d hold=%s", len(observed), e.state(e.hold))
		}
		for i, iid := range observed {
			if iid != int64(1000+i) {
				t.Fatalf("first witness mismatch at %d: got %d", i, iid)
			}
		}
	})
	t.Run("more_than_100_repositories", func(t *testing.T) {
		e, _ := newCrossRunDiscardEnv(t)
		repos := []uuid.UUID{e.repo}
		holds := []uuid.UUID{e.hold}
		for n := 1; n <= 100; n++ {
			repo, run, hold, witness := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			repos = append(repos, repo)
			holds = append(holds, hold)
			e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES($1,$2,$3,$4,'https://forge.test/g/r',true)", repo, e.conn, int64(42+n), fmt.Sprint(repo))
			e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,created_at,finished_at)
				VALUES($1,$2,$3,'issue',101,'target','d','cancelled',1,'2020-01-01','2020-01-02')`, run, e.worker.UserID, repo)
			e.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state)
				VALUES($1,$2,$3,$4,1,$5,'original',$5,$4,true,'open')`, hold, e.worker.UserID, repo, run, e.worker.ID)
			e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,mr_iid,created_at)
				VALUES($1,$2,$3,'issue',101,'witness','d','completed',1,7,'2020-01-03')`, witness, e.worker.UserID, repo)
		}
		e.forge.reads = 0
		e.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
			return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
		}
		e.svc.ReconcileMergedPredecessors(e.ctx, repos)
		if e.forge.reads != 100 {
			t.Fatalf("first phase GETs=%d want 100", e.forge.reads)
		}
		open := 0
		for _, hold := range holds {
			if e.state(hold) == "open" {
				open++
			}
		}
		if open != 1 {
			t.Fatalf("remaining OPEN=%d want 1", open)
		}
		e.svc.ReconcileMergedPredecessors(e.ctx, repos)
		if e.forge.reads != 101 {
			t.Fatalf("second phase GETs=%d want 101", e.forge.reads)
		}
		for i, hold := range holds {
			if e.state(hold) != "discarded" {
				t.Fatalf("repository %d starved", i)
			}
		}
	})
}
