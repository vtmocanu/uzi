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
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	tokens := make(map[uuid.UUID]string)
	user, worker, nextWorker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	newWorker := func(id uuid.UUID, name string, capable bool) {
		t.Helper()
		token, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
		caps := []string{}
		if capable {
			caps = append(caps, capability.InputReceiptsV1)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,$3,$4,$5)`, id, user, name, hash, caps); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("receipt-%s@e2e", user))
	newWorker(worker, "receipt", true)
	newWorker(nextWorker, "receipt2", true)
	exec(`INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','running',$3,1)`, run, user, worker)
	ids := make(map[string]int64)
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,$2,'x') RETURNING id`, run, kind).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[kind] = id
	}
	wkr := store.Worker{ID: worker, UserID: user, ProtocolCapabilities: []string{capability.InputReceiptsV1}}
	request := func(method, path, body, token string) *http.Request {
		req := httptest.NewRequest(method, "/api/worker/runs/"+run.String()+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req
	}
	call := func(method, path, body string, who store.Worker, want int) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, request(method, path, body, tokens[who.ID]))
		if rec.Code != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, path := range []string{"/inputs", "/inputs/ack", "/inputs/applied"} {
		method := http.MethodPost
		if path == "/inputs" {
			method = http.MethodGet
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, request(method, path, `{"ids":[1],"claim_generation":1}`, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s: status %d", method, path, rec.Code)
		}
	}
	get := func(who store.Worker, want int) {
		t.Helper()
		out := call("GET", "/inputs", "", who, 200)
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
	type receiptStamp struct {
		consumedAt string
		generation int64
		workerID   uuid.UUID
	}
	readStamp := func(kind string) receiptStamp {
		t.Helper()
		var stamp receiptStamp
		if err := pool.QueryRow(ctx, `SELECT consumed_at::text, consumed_claim_generation, consumed_worker_id FROM run_user_inputs WHERE id=$1`, ids[kind]).Scan(&stamp.consumedAt, &stamp.generation, &stamp.workerID); err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	firstStamps := make(map[string]receiptStamp)
	assertRetry := func(kind, stage string, out map[string]any, active bool) {
		t.Helper()
		inputs := out["inputs"].([]any)
		if out["active"] != active || len(inputs) != 1 {
			t.Fatalf("%s %s: %v", stage, kind, out)
		}
		input := inputs[0].(map[string]any)
		if got := int64(input["id"].(float64)); got != ids[kind] {
			t.Fatalf("%s %s returned row ID %d, want %d", stage, kind, got, ids[kind])
		}
		if got := readStamp(kind); got != firstStamps[kind] {
			t.Fatalf("%s %s changed receipt: got %+v, first ACK %+v", stage, kind, got, firstStamps[kind])
		}
	}
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids[kind])
		out := call("POST", "/inputs/ack", body, wkr, 200)
		if out["active"] != true || len(out["inputs"].([]any)) != 1 {
			t.Fatalf("ACK %s: %v", kind, out)
		}
		firstStamps[kind] = readStamp(kind)
		get(wkr, 3) // consumed but unapplied is replayed
		// Model a lost ACK response: the worker retries the same receipt.
		repeat := call("POST", "/inputs/ack", body, wkr, 200)
		assertRetry(kind, "lost ACK retry", repeat, true)
		exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
		retry := call("POST", "/inputs/ack", body, wkr, 200)
		assertRetry(kind, "released retry", retry, false)
		call("POST", "/inputs/applied", body, wkr, 409)
		exec(`UPDATE runs SET claim_released_at=NULL WHERE id=$1`, run)
	}
	// A switch fences the old claim before release. A lost ACK response remains
	// recoverable for each input kind while the claim changes hands.
	exec(`UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=1 WHERE id=$1`, run)
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids[kind])
		retry := call("POST", "/inputs/ack", body, wkr, 200)
		assertRetry(kind, "switched retry", retry, false)
	}
	exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids[kind])
		retry := call("POST", "/inputs/ack", body, wkr, 200)
		assertRetry(kind, "released switch retry", retry, false)
	}
	exec(`UPDATE runs SET worker_id=$2,claim_generation=2,claim_released_at=NULL,credential_switch_requested_at=NULL,credential_switch_generation=NULL WHERE id=$1`, run, nextWorker)
	for _, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids[kind])
		retry := call("POST", "/inputs/ack", body, wkr, 200)
		assertRetry(kind, "old claim retry", retry, false)
	}
	body := fmt.Sprintf(`{"ids":[%d],"claim_generation":1}`, ids["cancel"])
	newer := store.Worker{ID: nextWorker, UserID: user, ProtocolCapabilities: []string{capability.InputReceiptsV1}}
	replayed := call("GET", "/inputs", "", newer, 200)["inputs"].([]any)
	for i, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		if len(replayed) != 3 {
			t.Fatalf("new claim replayed %d inputs: %v", len(replayed), replayed)
		}
		input := replayed[i].(map[string]any)
		if input["kind"] != kind || int64(input["id"].(float64)) != ids[kind] {
			t.Fatalf("new claim replay %d = %v, want %s input %d", i, input, kind, ids[kind])
		}
	}
	call("POST", "/inputs/applied", body, wkr, 409)
	// Each replayed receipt moves to the active claim, then can be applied.
	for i, kind := range []string{"cancel", "approve_plan", "follow_up"} {
		var consumedBefore string
		if err := pool.QueryRow(ctx, `SELECT consumed_at::text FROM run_user_inputs WHERE id=$1`, ids[kind]).Scan(&consumedBefore); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, ids[kind])
		ack := call("POST", "/inputs/ack", body, newer, 200)
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
		call("POST", "/inputs/ack", oldBody, wkr, 409)
		call("POST", "/inputs/applied", body, wkr, 409)
		call("POST", "/inputs/applied", body, newer, 200)
		get(newer, 2-i)
	}
	call("POST", "/inputs/ack", body, wkr, 409)
	var fresh int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'follow_up','new') RETURNING id`, run).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	freshBody := fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, fresh)
	call("POST", "/inputs/ack", freshBody, newer, 200)
	call("POST", "/inputs/applied", freshBody, newer, 200)
	call("POST", "/inputs/applied", freshBody, newer, 200)
	get(newer, 0) // all replayed rows and the fresh follow-up are applied
	// The first APPLIED committed but its reply was lost, then the claim was
	// released: the retry of rows this claim already applied still succeeds.
	for _, fence := range []string{
		`UPDATE runs SET claim_released_at=now() WHERE id=$1`,
		`UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=2 WHERE id=$1`,
	} {
		exec(fence, run)
		if out := call("POST", "/inputs/applied", freshBody, newer, 200); out["active"] != false || len(out["inputs"].([]any)) != 1 {
			t.Fatalf("applied retry after fence %q: %v", fence, out)
		}
		exec(`UPDATE runs SET claim_released_at=NULL,credential_switch_requested_at=NULL,credential_switch_generation=NULL WHERE id=$1`, run)
	}

	// A legacy consume-on-read row was backfilled as applied and cannot be taken over.
	var legacy int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,applied_at) VALUES ($1,'cancel','legacy',now(),now()) RETURNING id`, run).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	call("POST", "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, legacy), newer, 409)
	get(newer, 0)

	otherRun := uuid.New()
	exec(`INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','running',$3,2)`, otherRun, user, nextWorker)
	var foreign int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'cancel','foreign') RETURNING id`, otherRun).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	call("POST", "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, foreign), newer, 400)

	var switched int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'follow_up','switch') RETURNING id`, run).Scan(&switched); err != nil {
		t.Fatal(err)
	}
	switchBody := fmt.Sprintf(`{"ids":[%d],"claim_generation":2}`, switched)
	exec(`UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=2 WHERE id=$1`, run)
	call("POST", "/inputs/ack", switchBody, newer, 409)
	exec(`UPDATE runs SET credential_switch_requested_at=NULL,credential_switch_generation=NULL WHERE id=$1`, run)
	call("POST", "/inputs/ack", switchBody, newer, 200)
	exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
	call("POST", "/inputs/applied", switchBody, newer, 409)

	// A legacy worker taking over must receive ACKed rows that were never applied.
	legacyWorker := uuid.New()
	newWorker(legacyWorker, "legacy", false)
	exec(`UPDATE runs SET worker_id=$2,claim_generation=3,claim_released_at=NULL WHERE id=$1`, run, legacyWorker)
	legacyWkr := store.Worker{ID: legacyWorker, UserID: user}
	var before string
	if err := pool.QueryRow(ctx, `SELECT consumed_at::text FROM run_user_inputs WHERE id=$1`, switched).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// The earlier ACKed follow-up is first in ID order, before this fresh row.
	var freshLegacy int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'cancel','fresh legacy') RETURNING id`, run).Scan(&freshLegacy); err != nil {
		t.Fatal(err)
	}
	var audit int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'extend','audit') RETURNING id`, run).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	out := call("GET", "/inputs", "", legacyWkr, 200)
	inputs := out["inputs"].([]any)
	if len(inputs) != 2 || int64(inputs[0].(map[string]any)["id"].(float64)) != switched ||
		int64(inputs[1].(map[string]any)["id"].(float64)) != freshLegacy {
		t.Fatalf("legacy handoff inputs = %v, want ACKed follow-up then fresh cancel", inputs)
	}
	var after string
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at::text, applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1`, switched).Scan(&after, &applied); err != nil {
		t.Fatal(err)
	}
	if before != after || !applied {
		t.Fatalf("legacy handoff consumed_at=%q before=%q applied=%v", after, before, applied)
	}
	if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1`, audit).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("legacy drain applied a server-only audit row")
	}
	if got := call("GET", "/inputs", "", legacyWkr, 200)["inputs"].([]any); len(got) != 0 {
		t.Fatalf("legacy replayed applied inputs: %v", got)
	}

	// A NEW ACK is fenced on run status: a terminal run keeps worker_id and generation, and a
	// requeued run keeps worker_id, so an ACK in flight across either must not deliver a late
	// cancel. The 409 names why the claim is inactive, so the worker knows whether to keep polling.
	exec(`UPDATE runs SET worker_id=$2,claim_generation=4,status='running',claim_released_at=NULL WHERE id=$1`, run, nextWorker)
	var late int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,'cancel','late') RETURNING id`, run).Scan(&late); err != nil {
		t.Fatal(err)
	}
	lateBody := fmt.Sprintf(`{"ids":[%d],"claim_generation":4}`, late)
	pending := func() bool {
		t.Helper()
		var consumed bool
		if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM run_user_inputs WHERE id=$1`, late).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		return !consumed
	}
	for _, fence := range []struct{ sql, reason string }{
		{`UPDATE runs SET status='completed' WHERE id=$1`, "stale"},
		{`UPDATE runs SET status='cancelled' WHERE id=$1`, "stale"},
		{`UPDATE runs SET status='failed' WHERE id=$1`, "stale"},
		{`UPDATE runs SET status='queued' WHERE id=$1`, "stale"},
		{`UPDATE runs SET claim_released_at=now() WHERE id=$1`, "released"},
		{`UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=4 WHERE id=$1`, "switch_pending"},
	} {
		exec(fence.sql, run)
		if out := call("POST", "/inputs/ack", lateBody, newer, 409); out["reason"] != fence.reason {
			t.Fatalf("ACK after %q: reason %v, want %s", fence.sql, out["reason"], fence.reason)
		}
		if !pending() {
			t.Fatalf("ACK after %q consumed the late input", fence.sql)
		}
		exec(`UPDATE runs SET status='running',claim_released_at=NULL,credential_switch_requested_at=NULL,credential_switch_generation=NULL WHERE id=$1`, run)
	}
	if out := call("POST", "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":3}`, late), newer, 409); out["reason"] != "stale" {
		t.Fatalf("superseded generation ACK: %v", out)
	}
	// An already-received row answers read-only with the reason; applied rows stay idempotent.
	if out := call("POST", "/inputs/ack", lateBody, newer, 200); out["active"] != true || out["reason"] != nil {
		t.Fatalf("active ACK: %v", out)
	}
	exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
	if out := call("POST", "/inputs/ack", lateBody, newer, 200); out["active"] != false || out["reason"] != "released" {
		t.Fatalf("released ACK retry: %v", out)
	}
	exec(`UPDATE runs SET claim_released_at=NULL WHERE id=$1`, run)
	call("POST", "/inputs/applied", lateBody, newer, 200)
	exec(`UPDATE runs SET status='completed' WHERE id=$1`, run)
	if out := call("POST", "/inputs/applied", lateBody, newer, 200); out["active"] != false || out["reason"] != "stale" {
		t.Fatalf("applied retry after completion: %v", out)
	}

	// A run this worker cannot see answers a TYPED 404 (reason stale), so the worker can tell it
	// from an untyped 404 of an older api pod that lacks the route and only retries that one.
	for _, path := range []string{"/inputs/ack", "/inputs/applied"} {
		saved := run
		run = uuid.New()
		out := call("POST", path, lateBody, newer, 404)
		run = saved
		if out["reason"] != "stale" {
			t.Fatalf("%s on an unknown run: %v", path, out)
		}
	}
}
