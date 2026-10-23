package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type m2RecordStore struct {
	*store.Queries
	fail bool
}

func (s *m2RecordStore) UpsertAutopilotTrigger(ctx context.Context, p store.UpsertAutopilotTriggerParams) error {
	if s.fail {
		return errors.New("injected record failure")
	}
	return s.Queries.UpsertAutopilotTrigger(ctx, p)
}

func TestM2AutopilotDurableOverflowLiveDB(t *testing.T) {
	for _, mode := range []string{"overflow", "record_failure", "comment_failure", "transient"} {
		t.Run(mode, func(t *testing.T) {
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
			exec("INSERT INTO users(id,email,password_hash,autopilot_enabled) VALUES($1,$2,'x',true)", user, fmt.Sprintf("%s@m2.test", user))
			var notes, fetches atomic.Int64
			var transient atomic.Bool
			transient.Store(mode == "transient")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/resource_label_events"):
					_, _ = w.Write([]byte(`[{"id":9,"action":"add","label":{"name":"autopilot"},"user":{"username":"alice"}}]`))
				case strings.HasSuffix(r.URL.Path, "/notes"):
					notes.Add(1)
					trigger, err := q.GetAutopilotTrigger(ctx, store.GetAutopilotTriggerParams{RepoID: repo, IssueIid: 7})
					if err != nil || trigger.LastEventID != 9 {
						t.Errorf("comment before durable record: %+v %v", trigger, err)
					}
					if mode == "comment_failure" {
						http.Error(w, "fixture rejected", http.StatusBadRequest)
						return
					}
					_, _ = w.Write([]byte(`{"id":1,"body":"posted"}`))
				default:
					fetches.Add(1)
					if transient.Load() {
						http.Error(w, "temporary", http.StatusBadRequest)
						return
					}
					w.(http.Flusher).Flush()
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "iid": 7, "description": strings.Repeat("x", 2<<20)})
				}
			}))
			t.Cleanup(srv.Close)
			exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,human_username,token_ciphertext) VALUES($1,$2,'gitlab',$3,'bot',1,'alice','x')", conn, user, srv.URL)
			exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES($1,$2,42,$3,'https://example.test/r',true)", repo, conn, repo.String())
			exec(`INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,author,web_url,forge_updated_at,synced_at) VALUES($1,7,'cached','opened','["uzi","autopilot"]','alice','https://example.test/i',now(),now())`, repo)
			driver, err := forge.New(forge.TypeGitLab, srv.URL, "fixture", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			st := &m2RecordStore{Queries: q, fail: mode == "record_failure"}
			runs := &apRuns{}
			row := store.ListEnabledReposWithConnectionsRow{ID: repo, ConnectionID: conn, ForgeProjectID: 42}
			detect := func() { NewAutopilot(st, runs, nil).detect(ctx, row, driver) }
			detect()
			if mode == "record_failure" || mode == "transient" {
				if notes.Load() != 0 {
					t.Fatal("unrecorded failure posted comment")
				}
				if _, err := q.GetAutopilotTrigger(ctx, store.GetAutopilotTriggerParams{RepoID: repo, IssueIid: 7}); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("failure consumed event: %v", err)
				}
				st.fail = false
				transient.Store(false)
				detect()
			}
			if notes.Load() != 1 {
				t.Fatalf("comment attempts=%d", notes.Load())
			}
			before := fetches.Load()
			detect() // a fresh detector must consult the persisted event, not process-local state.
			if notes.Load() != 1 || fetches.Load() != before || len(runs.calls) != 0 || len(runs.resolutions) != 0 {
				t.Fatal("durable dedup or pre-harness overflow gate failed")
			}
		})
	}
}
