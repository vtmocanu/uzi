package handler

import (
	"context"
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

// Real Bearer -> route -> handler -> service -> SQL, before any M2 dependents.
// This uses the worker_codex_livedb_test.go fixture pattern without minting a
// refresh capability: the admission endpoint must not require one.
func TestWorkerCodexAdmissionPersistedLiveDB(t *testing.T) {
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
	body := fmt.Sprintf(`{"claim_generation":1,"attempt_id":%q,"class":"rate_limit","provider_thread_id":"thread","provider_turn_id":"turn"}`, uuid.NewString())
	path := "/api/worker/runs/" + run.String() + "/codex/admission-signal"
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := post()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	state, err := q.GetCodexAdmissionState(ctx, store.GetCodexAdmissionStateParams{UserID: owner, ProviderAccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	if state.RateStep != 0 || !state.RateExpiry.Valid || !state.EpisodeID.Valid {
		t.Fatalf("missing persisted transition: %+v", state)
	}
	duration := state.RateExpiry.Time.Sub(state.RateLastQualifying.Time)
	if duration < 30*time.Second || duration > 36*time.Second {
		t.Fatalf("server jitter duration=%s", duration)
	}
	before := string(state.Receipts)
	if rec = post(); rec.Code != http.StatusNoContent {
		t.Fatalf("duplicate = %d", rec.Code)
	}
	state, err = q.GetCodexAdmissionState(ctx, store.GetCodexAdmissionStateParams{UserID: owner, ProviderAccountID: account.ID})
	if err != nil || string(state.Receipts) != before {
		t.Fatalf("duplicate altered receipts: %v", err)
	}
	eligible, err := q.GetCodexAdmissionRunEligibility(ctx, run)
	if err != nil || !eligible.Valid || eligible.Time.Before(state.EffectiveExpiry.Time) ||
		!eligible.Time.Before(state.EffectiveExpiry.Time.Add(30*time.Second)) {
		t.Fatalf("spread eligibility=%v: %v", eligible, err)
	}
	mustExecT(ctx, t, pool, "UPDATE runs SET status='recovery_wait',claim_released_at=now() WHERE id=$1", run)
	if rec = post(); rec.Code != http.StatusNotFound {
		t.Fatalf("post-park = %d %s", rec.Code, rec.Body.String())
	}
}
