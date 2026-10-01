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

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerInputsIncludedRouteLiveDB drives POST /api/worker/runs/{id}/inputs/included through
// the real worker router: auth, body validation, the capability gate (400), the claim fence
// (200 active=false reason stale), and the stamp itself.
func TestWorkerInputsIncludedRouteLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
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
	svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
	svc.SetTxBeginner(pool)
	h := &Handler{q: q, wsvc: svc}
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	user := uuid.New()
	exec(`INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("included-%s@e2e", user))
	tokens := map[uuid.UUID]string{}
	newWorker := func(caps ...string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		token, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
		exec(`INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,$3,$4,$5)`, id, user, "w-"+id.String(), hash, caps)
		return id
	}
	capable := newWorker(capability.InputReceiptsV1, capability.InputInclusionV1)
	legacy := newWorker(capability.InputReceiptsV1)
	other := newWorker(capability.InputReceiptsV1, capability.InputInclusionV1)
	run := uuid.New()
	exec(`INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','running',$3,1)`, run, user, capable)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id)
		VALUES ($1,'follow_up','x',now(),1,$2) RETURNING id`, run, capable).Scan(&id); err != nil {
		t.Fatal(err)
	}
	post := func(who uuid.UUID, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/inputs/included", strings.NewReader(body))
		if who != uuid.Nil {
			req.Header.Set("Authorization", "Bearer "+tokens[who])
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("status %d want %d: %s", rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	included := func() bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT included_at IS NOT NULL FROM run_user_inputs WHERE id=$1`, id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, id)

	post(uuid.Nil, body, http.StatusUnauthorized)
	post(capable, `{"ids":[1]}`, http.StatusBadRequest) // claim_generation is required
	post(legacy, body, http.StatusBadRequest)           // capability gate
	if out := post(other, body, http.StatusOK); out["active"] != false || out["reason"] != "stale" || included() {
		t.Fatalf("other worker: %v included=%v", out, included())
	}
	post(capable, fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, id), http.StatusOK)
	if included() {
		t.Fatal("wrong generation stamped")
	}
	if out := post(capable, body, http.StatusOK); out["active"] != true || !included() {
		t.Fatalf("current claim: %v included=%v", out, included())
	}
}
