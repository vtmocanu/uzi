package poller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// poolQueue is the production queue mutation (begin, lock, fn, commit) over a pool, standing in
// for workersvc.Service.MutateReviewAuthorQueue without building a whole Service.
type poolQueue struct{ pool *pgxpool.Pool }

func (p poolQueue) MutateReviewAuthorQueue(ctx context.Context, repoID uuid.UUID, ref string, fn func(workersvc.ReviewAuthorQueueOps) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.LockReviewAuthorQueue(ctx, store.LockReviewAuthorQueueParams{RepoID: repoID, Ref: ref}); err != nil {
		return err
	}
	if err := fn(q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// startCounter counts the creates a real MRReworkRunStarter is asked for and the ones it accepts.
type startCounter struct {
	inner    MRReworkRunStarter
	attempts int // every create call, accepted or refused
	starts   int // accepted creates only
}

func (c *startCounter) CreateAutoMRReworkRunAndAdvance(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, res *workersvc.ReviewSnapshotResult, capLimit int) (store.Run, error) {
	c.attempts++
	run, err := c.inner.CreateAutoMRReworkRunAndAdvance(ctx, userID, repoID, ref, mrIID, sourceRunID, title, description, res, capLimit)
	if err == nil {
		c.starts++
	}
	return run, err
}

// TestMRReworkEligibilityRoundTripLiveDB exercises detection with real queries:
// temporary eligibility changes must preserve the consumed review and loop budget.
func TestMRReworkEligibilityRoundTripLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set a throwaway database: UZI_TEST_DATABASE_URL=<throwaway DSN> go test -buildvcs=false -race -count=1 -p 1 -v -run 'TestMRRework.*LiveDB$' ./internal/poller")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	for _, toggle := range []string{"token", "run", "account"} {
		t.Run(toggle, func(t *testing.T) {
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			owner, conn, repo, source := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", owner, fmt.Sprintf("%s@1811.test", owner))
			exec("INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://forge.test','bot',$3,$4)", conn, owner, mrwBotID, []byte{1})
			exec("INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES ($1,$2,42,$3,'https://forge.test/g/r','main',true)", repo, conn, repo.String())
			addToken := func() {
				exec("INSERT INTO user_secrets (user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,'anthropic_token','default',true,$2,'master')", owner, []byte{2})
			}
			addToken()
			exec("INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,status,created_at) VALUES ($1,$2,$3,'issue',7,'t','d',$4,$5,'opened','completed','2020-01-01')", source, owner, repo, mrwRef, mrwMrIID)
			exec("INSERT INTO pipeline_statuses (repo_id,ref,pipeline_id,sha,status,web_url,synced_at) VALUES ($1,$2,7001,$3,'success','https://forge.test/p','2020-01-01')", repo, mrwRef, mrwHeadSHA)
			// The REAL workersvc create (recheck + atomic ledger advance over the pool), wrapped to
			// count the starts it accepts.
			svc := workersvc.New(q, nil, workersvc.Params{})
			svc.SetTxBeginner(pool)
			runs := &startCounter{inner: svc}
			f := landedForge(mrwComment(120, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), mrwHeadSHA))
			row := mrwRepoRow()
			row.ID = repo
			watch := NewMRReviewWatch(q, runs, poolQueue{pool}, nil, mrwSettings{enabled: true, capVal: 5}, 5, 0)
			ledger := func(count int32, highwater int64) store.MrReworkLedger {
				t.Helper()
				got, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: mrwRef})
				if err != nil {
					t.Fatalf("ledger must survive %s eligibility change: %v", toggle, err)
				}
				if got.AttemptCount != count || got.HighWater != highwater {
					t.Fatalf("ledger = %+v, want count=%d highwater=%d", got, count, highwater)
				}
				return got
			}
			candidates := func(want int) {
				t.Helper()
				got, err := q.ListMRReworkCandidates(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != want {
					t.Fatalf("candidates=%+v, want %d", got, want)
				}
			}
			candidates(1)
			watch.detect(ctx, row, f)
			before := ledger(1, 120)
			if runs.starts != 1 {
				t.Fatalf("first note: starts=%d", runs.starts)
			}
			switch toggle {
			case "token":
				exec("DELETE FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", owner)
			case "run":
				exec("UPDATE runs SET mr_rework_enabled=false WHERE id=$1", source)
			case "account":
				exec("UPDATE users SET mr_rework_enabled=false WHERE id=$1", owner)
			}
			candidates(0)
			watch.detect(ctx, row, f)
			after := ledger(1, 120)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("ineligible tick changed ledger: before=%+v after=%+v", before, after)
			}
			if runs.starts != 1 || runs.attempts != 1 {
				t.Fatalf("ineligible tick: starts=%d attempts=%d, want 1 and 1 (no new create attempt)", runs.starts, runs.attempts)
			}
			switch toggle {
			case "token":
				addToken()
			case "run":
				exec("UPDATE runs SET mr_rework_enabled=true WHERE id=$1", source)
			case "account":
				exec("UPDATE users SET mr_rework_enabled=true WHERE id=$1", owner)
			}
			candidates(1)
			watch.detect(ctx, row, f)
			ledger(1, 120)
			if runs.starts != 1 || runs.attempts != 1 {
				t.Fatalf("same consumed note duplicated: starts=%d attempts=%d, want 1 and 1 (no new create attempt)", runs.starts, runs.attempts)
			}
			// The first rework run finishes before the next cycle (one active rework per MR).
			exec("UPDATE runs SET status='completed' WHERE kind='mr_rework' AND target_run_id=$1", source)
			f.comments = append(f.comments, mrwComment(121, time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), mrwHeadSHA))
			watch.detect(ctx, row, f)
			ledger(2, 121)
			if runs.starts != 2 {
				t.Fatalf("higher-ID positive control: starts=%d", runs.starts)
			}
		})
	}
}
