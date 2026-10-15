package poller

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestMergedPredecessorPanicIsolationLiveDB(t *testing.T) {
	for _, postCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "forge_read", true: "post_commit_hook"}[postCommit], func(t *testing.T) {
			a, b := newMergedPollEnv(t), newMergedPollEnv(t)
			if slices.Compare(a.repo[:], b.repo[:]) > 0 {
				a, b = b, a
			}
			a.exec("UPDATE runs SET status='completed',claim_generation=2,mr_iid=7 WHERE id=$1", a.run)
			b.exec("UPDATE runs SET status='completed',claim_generation=2,mr_iid=8 WHERE id=$1", b.run)
			a.engine.q = mergedEnabledSet{a.q, map[uuid.UUID]bool{a.repo: true, b.repo: true}}
			a.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
				if iid == 7 && !postCommit {
					panic("untrusted forge response")
				}
				return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
			}
			hooks := 0
			a.recovery.SetCustodySettledHook(func(run uuid.UUID) {
				hooks++
				if run == a.run && postCommit {
					if a.state(a.hold) != "discarded" {
						t.Error("hook ran before the discard committed")
					}
					panic("synchronous custody hook failed")
				}
			})
			if postCommit {
				// Model the existing retained checkpoint whose immediate settlement hook is lost.
				// The public reconciler reads a bounded page from the shared DB; put this fixture
				// before other tests' due rows so its one-pass assertion reaches its own checkpoint.
				a.exec("INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,tip,ref,state,next_attempt_at) VALUES($1,$2,$3,'agent/issue-1',$4,'refs/uzi-checkpoints/agent/issue-1','retained','-infinity')",
					a.run, a.worker.UserID, a.repo, strings.Repeat("a", 40))
			}
			a.svc.SetMergedPredecessorRecovery(a.q, a.recovery)
			a.engine.PollOnce(a.ctx)
			want := "open"
			if postCommit {
				want = "discarded"
			}
			if a.state(a.hold) != want || b.state(b.hold) != "discarded" || a.forge.reads != 2 {
				t.Fatalf("panic abandoned sibling or changed custody: first=%s sibling=%s reads=%d",
					a.state(a.hold), b.state(b.hold), a.forge.reads)
			}
			var audit []byte
			if err := a.pool.QueryRow(a.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", a.hold).Scan(&audit); err != nil {
				t.Fatal(err)
			}
			if (len(audit) > 0) != postCommit {
				t.Fatalf("panic audit disagrees with commit boundary: %s", audit)
			}
			reads, calls := a.forge.reads, hooks
			if postCommit {
				a.engine.PollOnce(a.ctx)
				var after []byte
				if err := a.pool.QueryRow(a.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", a.hold).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if a.forge.reads != reads || hooks != calls || !reflect.DeepEqual(audit, after) {
					t.Fatal("next tick rewrote committed audit or replayed the hook")
				}
				box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
				if err != nil {
					t.Fatal(err)
				}
				wsvc := workersvc.New(a.q, box, workersvc.Params{})
				wsvc.SetTxBeginner(a.pool)
				wsvc.SetRetentionLockPool(a.pool)
				wsvc.SetForgeBaseURLAllowed(func(url string) bool { return url == "https://forge.test" })
				wsvc.SetBackground(func(fn func()) { fn() })
				deletes := 0
				wsvc.SetDeleteCheckpointFn(func(_ context.Context, opts pushbroker.DeleteOptions) error {
					deletes++
					if opts.ExpectedOldTip != strings.Repeat("a", 40) {
						t.Error("backstop lost the recorded checkpoint tip")
					}
					return nil
				})
				if _, err := wsvc.ReconcileCheckpointRetentions(a.ctx); err != nil {
					t.Fatal(err)
				}
				retained, err := a.q.GetCheckpointRetention(a.ctx, a.run)
				if err != nil || retained.State != "deleted" || deletes != 1 {
					t.Fatalf("existing backstop did not settle lost hook: state=%s deletes=%d err=%v", retained.State, deletes, err)
				}
			} else {
				// The exhausted cursor wraps for a later visit; retry without the broken response.
				a.forge.read = func(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
					return forge.MergeRequest{IID: iid, State: forge.MRStateMerged}, nil
				}
				a.engine.PollOnce(a.ctx)
				if a.state(a.hold) != "discarded" || a.forge.reads != reads+1 || hooks != calls+1 {
					t.Fatal("forge panic lost durable retry on a later tick")
				}
			}
		})
	}
}
