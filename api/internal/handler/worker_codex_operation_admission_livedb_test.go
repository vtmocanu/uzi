package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCodexOperationPersistedLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
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
	h := &Handler{q: q, wsvc: svc}
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	owner, connection, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, "admission-"+owner.String()+"@example.test")
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
        VALUES($1,$2,'gitlab','https://forge.example','bot',1,$3)`, connection, owner, []byte("fixture"))
	mustExecT(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
        VALUES($1,$2,1,$3,'https://forge.example/repo','main',true)`, repo, connection, "admission/"+repo.String())
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`, worker, owner, worker.String(), hash)
	secret, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: owner, Kind: store.KindCodexAuth, Label: "admission", WantDefault: true, Ciphertext: []byte("fixture"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: owner, ProviderUserID: "principal-" + uuid.NewString(), WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin: []byte("fixture"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret.ID, UserID: owner, Status: "staging"}); err != nil {
		t.Fatal(err)
	}
	if _, err = q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: owner, MaterialRevision: 0}); err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
        VALUES($1,$2,$3,'issue',1,'admission','fixture','claimed',$4,1)`, run, owner, repo, worker)
	if err = svc.FreezeCodexBinding(ctx, owner, run, secret.ID, "subscription"); err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewString()
	body := fmt.Sprintf(`{"claim_generation":1,"operation_id":%q,"kind":"child"}`, operation)
	path := "/api/worker/runs/" + run.String() + "/codex/operation-admission"
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Body.Len() > 8192 {
			t.Fatal("response cap/cache")
		}
		return rec
	}
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var value map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, status := range []string{"claimed", "running", "awaiting_approval"} {
		mustExecT(ctx, t, pool, "UPDATE runs SET status=$2 WHERE id=$1", run, status)
		rec := post()
		value := decode(rec)
		if rec.Code != 200 || len(value) != 3 || value["decision"] != "admit" || value["operation_id"] != operation || value["claim_generation"] != float64(1) {
			t.Fatalf("admit %d %v", rec.Code, value)
		}
	}
	mustExecT(ctx, t, pool, "UPDATE runs SET status='running' WHERE id=$1", run)
	if err := svc.RecordCodexAdmissionSignal(ctx, store.Worker{ID: worker, UserID: owner}, run, workersvc.CodexAdmissionSignal{ClaimGeneration: 1, AttemptID: uuid.NewString(), Class: "rate_limit"}); err != nil {
		t.Fatal(err)
	}
	rec := post()
	value := decode(rec)
	if rec.Code != 200 || len(value) != 7 || value["decision"] != "wait" || value["class"] != "rate_limit" || value["operation_id"] != operation || value["claim_generation"] != float64(1) {
		t.Fatalf("wait %d %v", rec.Code, value)
	}
	for _, field := range []string{"server_now", "eligible_at"} {
		text, ok := value[field].(string)
		if !ok {
			t.Fatalf("missing timestamp %s", field)
		}
		if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
			t.Fatal(err)
		}
	}
	if episode, ok := value["episode_id"].(string); !ok || uuid.Validate(episode) != nil {
		t.Fatal("missing episode")
	}
	presentation := uuid.New()
	mustExecT(ctx, t, pool, "UPDATE runs SET gate_revision=1,gate_presentation_id=$2 WHERE id=$1", run, presentation)
	var input int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision,consumed_at,consumed_worker_id,consumed_claim_generation)
 VALUES($1,'revise_plan','revise','bound',1,now(),$2,1) RETURNING id`, run, worker).Scan(&input); err != nil {
		t.Fatal(err)
	}
	rec = post()
	value = decode(rec)
	if rec.Code != 409 || value["reason"] != "stale_source" {
		t.Fatalf("stale %d %v", rec.Code, value)
	}
	body = fmt.Sprintf(`{"claim_generation":1,"operation_id":%q,"kind":"child","source":{"gate_revision":1,"presentation_id":%q,"revision_input_id":%d}}`, operation, presentation, input)
	if rec = post(); rec.Code != 200 {
		t.Fatalf("valid revision %d %s", rec.Code, rec.Body.String())
	}
	mustExecT(ctx, t, pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
	rec = post()
	value = decode(rec)
	if rec.Code != 404 || value["reason"] != "authority_lost" || len(value) != 2 {
		t.Fatalf("opaque authority error %d %v", rec.Code, value)
	}
}
