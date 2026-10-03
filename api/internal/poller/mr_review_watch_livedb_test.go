package poller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

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
			runs := &mrwRuns{}
			f := landedForge(mrwComment(120, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), mrwHeadSHA))
			row := mrwRepoRow()
			row.ID = repo
			watch := NewMRReviewWatch(q, runs, nil, mrwSettings{enabled: true, capVal: 5}, 5, 0)
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
			if len(runs.calls) != 1 {
				t.Fatalf("first note: starts=%d", len(runs.calls))
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
			if after != before {
				t.Fatalf("ineligible tick changed ledger: before=%+v after=%+v", before, after)
			}
			if len(runs.calls) != 1 {
				t.Fatalf("ineligible tick: starts=%d", len(runs.calls))
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
			if len(runs.calls) != 1 {
				t.Fatalf("same consumed note duplicated: starts=%d", len(runs.calls))
			}
			f.comments = append(f.comments, mrwComment(121, time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), mrwHeadSHA))
			watch.detect(ctx, row, f)
			ledger(2, 121)
			if len(runs.calls) != 2 {
				t.Fatalf("higher-ID positive control: starts=%d", len(runs.calls))
			}
		})
	}
}
