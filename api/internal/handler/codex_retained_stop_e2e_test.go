package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestCodexRetainedStopE2E composes the real Node executor and worker HTTP routes
// with an isolated migrated schema. Opting in requires Node and Postgres; no fallback.
func TestCodexRetainedStopE2E(t *testing.T) {
	if os.Getenv("UZI_CODEX_REFRESH_LOSTREPLY_E2E") == "" {
		t.Skip("opt in with UZI_CODEX_REFRESH_LOSTREPLY_E2E")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fixture root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("combined proof requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("combined proof requires installed agent tsx")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("combined proof requires UZI_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// LiveDB leaves unresolved accounts sealed by other test boxes. The real global
	// survivor must see only this proof's fixtures, including on pooled transactions.
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture schema administration pool")
	}
	defer admin.Close()
	schema := "retainedstop_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Preserve failed custody proofs until the dedicated target destroys its DB.
		if t.Failed() {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("cleanup fixture schema: %v", err)
		}
	}()
	// Set the startup parameter on the DSN used by BOTH goose and every pool
	// connection; a one-off SET would not cover new sessions or transactions.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("cannot parse fixture database URL")
		}
		params := u.Query()
		params.Set("search_path", schema)
		u.RawQuery = params.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	dir, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "retained-stop-")
	if err != nil {
		t.Fatal(err)
	}
	// Keep failed filesystem evidence until the caller disposes its isolated proof.
	defer func() {
		if !t.Failed() {
			_ = os.RemoveAll(dir)
		}
	}()
	origin := filepath.Join(dir, "origin.git")
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), RunMaxRequeues: 3,
		WorkerHeartbeatStale: time.Minute, WorkerHeartbeatInterval: 15 * time.Second,
		ActiveSnapshotMaxEntries: 100, RunTimeout: time.Hour,
	})
	svc.SetTxBeginner(pool)
	svc.SetVault(vault.NewPostgres(box, pool))
	svc.SetForges(settleForgeBuilder{f: &lostReplyForge{origin: origin}})
	svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	// Exercise the real broker against the owned bare origin, without external DNS.
	svc.SetPublishFn(func(ctx context.Context, options pushbroker.Options) (pushbroker.Result, error) {
		options.CloneURL, options.Username, options.PAT = "file://"+origin, "", ""
		return pushbroker.Publish(ctx, options)
	})
	svc.SetDeleteCheckpointFn(func(ctx context.Context, options pushbroker.DeleteOptions) error {
		options.CloneURL, options.Username, options.PAT = "file://"+origin, "", ""
		return pushbroker.Delete(ctx, options)
	})
	var clockMu sync.Mutex
	clock := time.Now()
	svc.SetClock(func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock })
	owner, conn, repo, worker, run, secret := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users(id,email,password_hash,vault_mode) VALUES($1,$2,'x','unprotected')`, owner, owner.String()+"@example.test")
	pat, err := box.Seal([]byte("fixture-forge-pat"))
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'gitlab','https://forge.example','bot',1,$3)`, conn, owner, pat)
	mustExecT(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES($1,$2,1,$3,'https://forge.example/fixture','main',true)`, repo, conn, "fixture/"+repo.String())
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'online',now())`, worker, owner, worker.String(), hash)
	key, err := box.Seal([]byte("retained-stop-key-canary"))
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,ciphertext,sealed_with)
 VALUES($1,$2,'openai_api_key','retained-stop',$3,'master')`, secret, owner, key)
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: owner, Status: "static"}); err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_source,plan_md,completion_contract_version,milestones_frozen,completion_contract,contract_revision)
 VALUES($1,$2,$3,'issue',2852,'retained stop','Implement the approved fixture change','queued','codex',true,'seeded','# Approved plan',1,'[{"id":"M1","title":"Retained stop composition"}]','{"profile":"structural","revision":1,"criteria":[{"id":"M1.c1","milestone_id":"M1","text":"Retained stop composition","audit":null,"finding_ids":[]}]}',1)`, run, owner, repo)
	if err := svc.FreezeCodexBinding(ctx, owner, run, secret, "api_key"); err != nil {
		t.Fatal(err)
	}
	inspect := func() (map[string]any, error) {
		var status, workerID, nonce string
		var session *string
		var generation, epoch int64
		var requeues int
		var revoked, failFree, released bool
		if err := pool.QueryRow(ctx, `SELECT r.status,r.session_id,r.claim_generation,coalesce(r.worker_id::text,''),r.requeue_count,
   r.codex_cap_hash IS NULL,r.fail_origin IS NULL,r.claim_released_at IS NOT NULL,w.snapshot_epoch,coalesce(w.snapshot_register_nonce::text,'')
   FROM runs r JOIN workers w ON w.id=$2 WHERE r.id=$1`, run, worker).Scan(
			&status, &session, &generation, &workerID, &requeues, &revoked, &failFree, &released, &epoch, &nonce); err != nil {
			return nil, err
		}
		var holds []int64
		if err := pool.QueryRow(ctx, `SELECT coalesce(array_agg(generation ORDER BY generation),ARRAY[]::bigint[])
   FROM recovery_custody_holds WHERE run_id=$1 AND state='open'`, run).Scan(&holds); err != nil {
			return nil, err
		}
		return map[string]any{"status": status, "session": session, "claim_generation": generation, "worker_id": workerID,
			"requeues": requeues, "cap_revoked": revoked, "fail_free": failFree, "released": released, "snapshot_epoch": epoch, "nonce": nonce, "holds": holds}, nil
	}
	h := &Handler{q: q, pool: pool, box: box, wsvc: svc}
	router := h.WorkerRoutes(mw.NewLimiter(10000, time.Minute, nil))
	var wireMu sync.Mutex
	var states []string
	var capabilities []string
	var snapshots []int64
	var snapshotNonce string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/fixture/") {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
			switch strings.TrimPrefix(r.URL.Path, "/fixture/") {
			case "pause":
				if _, err := svc.SubmitInput(r.Context(), owner, run, "pause", "now", nil); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			case "fence":
				clockMu.Lock()
				clock = time.Now().Add(2 * time.Minute)
				clockMu.Unlock()
			case "inspect":
			default:
				http.Error(w, "unknown control", 400)
				return
			}
			state, err := inspect()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			http.Error(w, "read", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Status     string `json:"status"`
			Capability string `json:"capability"`
			Snapshot   *struct {
				Epoch int64  `json:"snapshot_epoch"`
				Nonce string `json:"register_nonce"`
			} `json:"active_snapshot"`
		}
		if json.Unmarshal(body, &request) == nil {
			wireMu.Lock()
			if strings.HasSuffix(r.URL.Path, "/state") {
				states = append(states, request.Status)
			}
			if request.Capability != "" {
				capabilities = append(capabilities, request.Capability)
			}
			if request.Snapshot != nil && strings.HasSuffix(r.URL.Path, "/heartbeat") {
				snapshots = append(snapshots, request.Snapshot.Epoch)
				if snapshotNonce == "" {
					snapshotNonce = request.Snapshot.Nonce
				}
				if request.Snapshot.Nonce == "" || request.Snapshot.Nonce != snapshotNonce {
					wireMu.Unlock()
					http.Error(w, "snapshot nonce mismatch", 500)
					return
				}
			}
			wireMu.Unlock()
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg, err := json.Marshal(map[string]any{"url": server.URL, "token": token, "run": run.String(), "origin": origin, "scratch": dir})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "test/fixtures/codex-retained-stop-e2e.ts")
	cmd.Dir = filepath.Join(root, "agent")
	cmd.Env = append(os.Environ(), "UZI_RETAINED_STOP_FIXTURE="+string(cfg), "TMPDIR="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	printable := strings.ReplaceAll(string(out), token, "[redacted]")
	printable = strings.ReplaceAll(printable, "retained-stop-key-canary", "[redacted]")
	wireMu.Lock()
	for _, capability := range capabilities {
		printable = strings.ReplaceAll(printable, capability, "[redacted]")
	}
	t.Logf("worker fixture: %s", printable)
	if err != nil {
		wireMu.Unlock()
		t.Fatalf("worker fixture exit: %v", err)
	}
	for _, status := range states {
		if status == "failed" {
			t.Error("retained stop emitted a terminal failure")
		}
	}
	if len(snapshots) < 3 || snapshotNonce == "" {
		t.Error("missing nonce-stamped heartbeat controls")
	}
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i] <= snapshots[i-1] {
			t.Error("heartbeat epochs not monotonic")
		}
	}
	wireMu.Unlock()
	t.Logf("M1: real retained stop, in-budget heartbeat requeue, reclaim, independent recovery and fresh model execution")
}
