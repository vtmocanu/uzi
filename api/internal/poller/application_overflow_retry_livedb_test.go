package poller

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"os"
	"strings"
	"testing"
	"time"
)

func TestR3AutopilotDurableOverflowRetryLiveDB(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, mode := range []string{"503", "metadata", "issue", "early_metadata"} {
			if (mode == "metadata" || mode == "early_metadata") && kind != "github" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				dsn := os.Getenv("UZI_TEST_DATABASE_URL")
				if dsn == "" {
					t.Skip("UZI_TEST_DATABASE_URL unset; run e2e/run-store-it.sh")
				}
				ctx := context.Background()
				if err := store.Migrate(ctx, dsn); err != nil {
					t.Fatal(err)
				}
				pool, err := store.OpenPool(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				q := store.New(pool)
				user, conn, repo := uuid.New(), uuid.New(), uuid.New()
				exec := func(sql string, args ...any) {
					t.Helper()
					if _, err := pool.Exec(ctx, sql, args...); err != nil {
						t.Fatal(err)
					}
				}
				exec("INSERT INTO users(id,email,password_hash,autopilot_enabled) VALUES($1,$2,'x',true)", user, fmt.Sprintf("%s@r3.test", user))
				fixtureMode := mode
				if mode == "early_metadata" {
					fixtureMode = "metadata"
				}
				wire, driver := r3Forge(t, kind, fixtureMode)
				exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,human_username,token_ciphertext) VALUES($1,$2,$3,$4,'bot',1,'alice','x')", conn, user, kind, wire.server.URL)
				exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES($1,$2,42,$3,'https://example.test/r',true)", repo, conn, repo.String())
				exec(`INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,author,web_url,forge_updated_at,synced_at) VALUES($1,7,'cached','opened','["uzi","autopilot"]','alice','https://example.test/i',now(),now())`, repo)
				runs := &apRuns{}
				st := &m2RecordStore{Queries: q}
				row := store.ListEnabledReposWithConnectionsRow{ID: repo, ConnectionID: conn, ForgeProjectID: 42}
				wire.beforeNote = func(body string) {
					trigger, err := q.GetAutopilotTrigger(ctx, store.GetAutopilotTriggerParams{RepoID: repo, IssueIid: 7})
					if err != nil || trigger.LastEventID != 9 {
						t.Error("comment before durable record")
					}
					if strings.Contains(body, "REMOTE_PRIVATE") {
						t.Error("remote content in comment")
					}
				}
				for attempt := int64(1); attempt <= 2; attempt++ {
					if mode == "metadata" {
						// Full detection primes repoSlugFor through ListIssueLabelEvents. Exercise the eligible handle seam with a fresh driver.
						fresh, err := forge.New(forge.TypeGitHub, wire.server.URL, "fixture", time.Second)
						if err != nil {
							t.Fatal(err)
						}
						cc, err := q.GetAutopilotConnectionContext(ctx, conn)
						if err != nil {
							t.Fatal(err)
						}
						NewAutopilot(st, runs, nil).handle(ctx, row, fresh, apLabel, cc, candIssue(7, "alice"), 9, "alice")
					} else {
						NewAutopilot(st, runs, nil).detect(ctx, row, driver)
					}
					if len(runs.calls)+len(runs.resolutions) != 0 {
						t.Fatal("failed capture reached harness/create")
					}
					trigger, err := q.GetAutopilotTrigger(ctx, store.GetAutopilotTriggerParams{RepoID: repo, IssueIid: 7})
					meta, target, events, notes := int64(0), attempt, attempt, int64(0)
					if kind == "github" {
						meta = 1
					}
					if mode == "issue" {
						if err != nil || trigger.LastEventID != 9 {
							t.Fatalf("own overflow not durably consumed: %v", err)
						}
						target, notes = 1, 1
					} else if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("transient failure consumed event: %v", err)
					}
					if mode == "metadata" || mode == "early_metadata" {
						meta, target, events = attempt, 0, 0
					}
					wire.assertAttempts(t, meta, target, events, notes)
					var n int
					if err := pool.QueryRow(ctx, "SELECT count(*) FROM runs WHERE repo_id=$1", repo).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Fatalf("runs=%d", n)
					}
				}
			})
		}
	}
}
