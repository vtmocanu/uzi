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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerInputReceiptsLiveDB(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	user, worker, nextWorker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("receipt-%s@e2e", user))
	exec(`INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,'receipt',$3,ARRAY[$4]::text[])`, worker, user, worker[:], capability.InputReceiptsV1)
	exec(`INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,'receipt2',$3,ARRAY[$4]::text[])`, nextWorker, user, nextWorker[:], capability.InputReceiptsV1)
	exec(`INSERT INTO runs (id,user_id,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,1,'t','d','running',$3,1)`, run, user, worker)
	ids := make(map[string]int64)
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,$2,'x') RETURNING id`, run, kind).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[kind] = id
	}
	wkr := store.Worker{ID: worker, UserID: user, ProtocolCapabilities: []string{capability.InputReceiptsV1}}
	request := func(method, path, body string, who store.Worker) *http.Request {
		req := httptest.NewRequest(method, "/api/worker/runs/"+run.String()+path, strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", run.String())
		return req.WithContext(context.WithValue(mw.ContextWithWorker(req.Context(), who), chi.RouteCtxKey, rc))
	}
	call := func(fn func(http.ResponseWriter, *http.Request), method, path, body string, who store.Worker, want int) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		fn(rec, request(method, path, body, who))
		if rec.Code != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	get := func(who store.Worker, want int) {
		t.Helper()
		out := call(h.WorkerRunInputs, "GET", "/inputs", "", who, 200)
		if got := len(out["inputs"].([]any)); got != want {
			t.Fatalf("GET inputs=%d want %d", got, want)
		}
	}
	get(wkr, 3)
	get(wkr, 3) // lost GET reply is safe to repeat before ACK
	var pendingCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND consumed_at IS NOT NULL`, run).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("GET consumed %d inputs", pendingCount)
	}
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids[kind])
		out := call(h.WorkerRunInputsAck, "POST", "/inputs/ack", body, wkr, 200)
		if out["active"] != true || len(out["inputs"].([]any)) != 1 {
			t.Fatalf("ACK %s: %v", kind, out)
		}
		get(wkr, 3) // consumed but unapplied is replayed
		exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
		retry := call(h.WorkerRunInputsAck, "POST", "/inputs/ack", body, wkr, 200)
		if retry["active"] != false || len(retry["inputs"].([]any)) != 1 {
			t.Fatalf("released retry %s: %v", kind, retry)
		}
		call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", body, wkr, 409)
		exec(`UPDATE runs SET claim_released_at=NULL WHERE id=$1`, run)
	}
	exec(`UPDATE runs SET worker_id=$2,claim_generation=2 WHERE id=$1`, run, nextWorker)
	body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids["cancel"])
	retry := call(h.WorkerRunInputsAck, "POST", "/inputs/ack", body, wkr, 200)
	if retry["active"] != false {
		t.Fatalf("old claim retry active: %v", retry)
	}
	newer := store.Worker{ID: nextWorker, UserID: user, ProtocolCapabilities: []string{capability.InputReceiptsV1}}
	get(newer, 3)
	call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", body, wkr, 409)
	// Each replayed receipt moves to the active claim, then can be applied.
	for i, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		var consumedBefore string
		if err := pool.QueryRow(ctx, `SELECT consumed_at::text FROM run_user_inputs WHERE id=$1`, ids[kind]).Scan(&consumedBefore); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, ids[kind])
		ack := call(h.WorkerRunInputsAck, "POST", "/inputs/ack", body, newer, 200)
		if ack["active"] != true || len(ack["inputs"].([]any)) != 1 {
			t.Fatalf("takeover ACK %s: %v", kind, ack)
		}
		var generation int64
		var owner uuid.UUID
		var appliedAt *string
		var consumedAfter string
		if err := pool.QueryRow(ctx, `SELECT consumed_claim_generation,consumed_worker_id,applied_at::text,consumed_at::text FROM run_user_inputs WHERE id=$1`, ids[kind]).Scan(&generation, &owner, &appliedAt, &consumedAfter); err != nil {
			t.Fatal(err)
		}
		if generation != 2 || owner != nextWorker || appliedAt != nil || consumedAfter != consumedBefore {
			t.Fatalf("takeover ownership %s: generation=%d worker=%s applied=%v consumed=%q before=%q", kind, generation, owner, appliedAt, consumedAfter, consumedBefore)
		}
		oldBody := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids[kind])
		call(h.WorkerRunInputsAck, "POST", "/inputs/ack", oldBody, wkr, 409)
		call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", body, wkr, 409)
		call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", body, newer, 200)
		get(newer, 2-i)
	}
	call(h.WorkerRunInputsAck, "POST", "/inputs/ack", body, wkr, 409)
	var fresh int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'follow_up','new') RETURNING id`, run).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	freshBody := fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, fresh)
	call(h.WorkerRunInputsAck, "POST", "/inputs/ack", freshBody, newer, 200)
	call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", freshBody, newer, 200)
	call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", freshBody, newer, 200)
	get(newer, 0) // all replayed rows and the fresh follow-up are applied

	// A legacy consume-on-read row was backfilled as applied and cannot be taken over.
	var legacy int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,applied_at) VALUES ($1,'cancel','legacy',now(),now()) RETURNING id`, run).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	call(h.WorkerRunInputsAck, "POST", "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, legacy), newer, 409)
	get(newer, 0)

	otherRun := uuid.New()
	exec(`INSERT INTO runs (id,user_id,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,2,'t','d','running',$3,2)`, otherRun, user, nextWorker)
	var foreign int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'cancel','foreign') RETURNING id`, otherRun).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	call(h.WorkerRunInputsAck, "POST", "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, foreign), newer, 400)

	var switched int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'follow_up','switch') RETURNING id`, run).Scan(&switched); err != nil {
		t.Fatal(err)
	}
	switchBody := fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, switched)
	exec(`UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=2 WHERE id=$1`, run)
	call(h.WorkerRunInputsAck, "POST", "/inputs/ack", switchBody, newer, 409)
	exec(`UPDATE runs SET credential_switch_requested_at=NULL,credential_switch_generation=NULL WHERE id=$1`, run)
	call(h.WorkerRunInputsAck, "POST", "/inputs/ack", switchBody, newer, 200)
	exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
	call(h.WorkerRunInputsApplied, "POST", "/inputs/applied", switchBody, newer, 409)
}
