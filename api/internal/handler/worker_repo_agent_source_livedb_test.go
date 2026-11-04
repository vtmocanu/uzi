package handler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerRepoAgentSourceRevisionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	svc.SetTxBeginner(pool)
	svc.SetBackground(func(func()) {})

	exec := func(sql string, args ...any) {
		t.Helper()
		cliMustExec(t, pool, sql, args...)
	}
	owner, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`, owner, fmt.Sprintf("repo-agent-source-%s@e2e", owner))
	var runIDs []uuid.UUID
	t.Cleanup(func() {
		for _, id := range runIDs {
			exec("DELETE FROM runs WHERE id=$1", id)
		}
		exec("DELETE FROM workers WHERE id=$1", workerID)
		exec("DELETE FROM repos WHERE id=$1", repoID)
		exec("DELETE FROM forge_connections WHERE id=$1", connID)
		exec("DELETE FROM users WHERE id=$1", owner)
	})
	pat, err := box.Seal([]byte("toy-forge-credential"))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://forge.e2e','bot',1,$3)`, connID, owner, pat)
	exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES($1,$2,1,$3,'https://forge.e2e/g/r','main',true)`, repoID, connID, "g/repo-agent-source-"+repoID.String())
	workerToken, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'online',now())`, workerID, owner, "repo-agent-source-"+workerID.String(), hash)

	const generation = 3
	seed := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		runIDs = append(runIDs, id)
		iid := int64(binary.BigEndian.Uint64(id[:8]) >> 1)
		exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
 VALUES($1,$2,$3,'issue',$4,'test','test','running',$5,$6)`, id, owner, repoID, iid, workerID, generation)
		return id
	}

	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	router := h.WorkerRoutes(mw.NewLimiter(100000, time.Minute, nil))
	post := func(t *testing.T, path, body string, want int) map[string]any {
		t.Helper()
		rec := bearerReqBody(router, http.MethodPost, path, workerToken, body)
		if rec.Code != want {
			t.Fatalf("POST %s: status %d want %d: %s", path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return out
	}
	const trusted = "1111111111111111111111111111111111111111"
	const forged = "2222222222222222222222222222222222222222"
	report := func(commit, agent string) string {
		return fmt.Sprintf(`{"status":"running","repo_agents":[{"name":%q,"description":"d"}],"repo_agent_source_commit":%q}`, agent, commit)
	}

	t.Run("the first revision is stored and a different one is refused", func(t *testing.T) {
		id := seed(t)
		path := "/api/worker/runs/" + id.String() + "/state"
		out := post(t, path, report(trusted, "reviewer"), http.StatusOK)
		if run, _ := out["run"].(map[string]any); run["repo_agent_source_commit"] != trusted {
			t.Fatalf("ack run.repo_agent_source_commit = %v, want %s", run["repo_agent_source_commit"], trusted)
		}
		post(t, path, report(trusted, "reviewer"), http.StatusOK)
		refused := post(t, path, report(forged, "approver"), http.StatusConflict)
		if refused["reason"] != "repo_agent_source_conflict" {
			t.Fatalf("refusal reason = %v, want repo_agent_source_conflict", refused["reason"])
		}
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		agents, err := workersvc.DecodeRepoAgents(run.RepoAgents)
		if err != nil {
			t.Fatal(err)
		}
		if run.RepoAgentSourceCommit.String != trusted || len(agents) != 1 || agents[0].Name != "reviewer" {
			t.Fatalf("after the refusal: revision %q roster %+v, want %s and the first roster", run.RepoAgentSourceCommit.String, agents, trusted)
		}
	})

	t.Run("an unreadable revision holds the live claim", func(t *testing.T) {
		id := seed(t)
		path := "/api/worker/runs/" + id.String() + "/repo-agent-source-hold"
		post(t, path, fmt.Sprintf(`{"commit":%q,"claim_generation":%d}`, trusted, generation-1), http.StatusConflict)
		out := post(t, path, fmt.Sprintf(`{"commit":%q,"claim_generation":%d}`, trusted, generation), http.StatusOK)
		run, _ := out["run"].(map[string]any)
		if run["status"] != "paused" || run["hold_reason"] != workersvc.HoldReasonRepoAgentSourceUnavailable {
			t.Fatalf("held run = %v/%v, want paused/%s", run["status"], run["hold_reason"], workersvc.HoldReasonRepoAgentSourceUnavailable)
		}
		post(t, path, `{"commit":"nope","claim_generation":3}`, http.StatusBadRequest)
	})
}
