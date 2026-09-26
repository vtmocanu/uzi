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
	"github.com/vtmocanu/uzi/api/internal/pgconv"
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
		// A receipt-capable worker's read-only reply declares receipt mode.
		if out["receipts"] != true {
			t.Fatalf("GET for a receipt-capable worker lacks receipts:true: %v", out)
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
	if _, marked := out["receipts"]; marked {
		t.Fatalf("a consume-on-read reply must not claim receipt mode: %v", out)
	}
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

// TestWorkerReviseAppliedUnderSwitchLiveDB pins issue #1604's receipt rule: a revise-only
// APPLIED from the claim that received those rows is accepted while that claim has a credential
// switch pending (the worker sends it only once each revise is final), so the resumed claim does
// not replay a settled revision. Only that case is widened: a mixed batch, a receipt this claim did not take, and a released or
// stale claim are all still refused, and the row stays unapplied.
func TestWorkerReviseAppliedUnderSwitchLiveDB(t *testing.T) {
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
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	user, worker, other := uuid.New(), uuid.New(), uuid.New()
	exec(t, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("revise-%s@e2e", user))
	tokens := make(map[uuid.UUID]string)
	for _, id := range []uuid.UUID{worker, other} {
		token, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
		exec(t, `INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,$3,$4,$5)`,
			id, user, "revise-"+id.String(), hash, []string{capability.InputReceiptsV1})
	}
	newRun := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, `INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','running',$3,1)`, id, user, worker)
		return id
	}
	addInput := func(t *testing.T, run uuid.UUID, kind string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body) VALUES ($1,$2,'x') RETURNING id`, run, kind).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// post answers the status code and decoded body, never failing the test itself, so a
	// goroutine can use it too.
	post := func(run uuid.UUID, path string, who uuid.UUID, generation int64, ids ...int64) (int, map[string]any) {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprint(id)
		}
		body := fmt.Sprintf(`{"ids":[%s],"claim_generation":%d}`, strings.Join(parts, ","), generation)
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokens[who])
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	expect := func(t *testing.T, stage string, want int, wantReason any, code int, out map[string]any) {
		t.Helper()
		if code != want || out["reason"] != wantReason {
			t.Fatalf("%s: status %d reason %v, want %d reason %v (%v)", stage, code, out["reason"], want, wantReason, out)
		}
	}
	ack := func(t *testing.T, run uuid.UUID, who uuid.UUID, generation int64, ids ...int64) {
		t.Helper()
		code, out := post(run, "/inputs/ack", who, generation, ids...)
		expect(t, "ACK", http.StatusOK, nil, code, out)
		if out["active"] != true {
			t.Fatalf("ACK on an active claim answered inactive: %v", out)
		}
	}
	appliedAt := func(t *testing.T, id int64) bool {
		t.Helper()
		var applied bool
		if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1`, id).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		return applied
	}
	requireUnapplied := func(t *testing.T, stage string, ids ...int64) {
		t.Helper()
		for _, id := range ids {
			if appliedAt(t, id) {
				t.Fatalf("%s applied input %d", stage, id)
			}
		}
	}
	switchPending := func(t *testing.T, run uuid.UUID, generation int64) {
		t.Helper()
		exec(t, `UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=$2 WHERE id=$1`, run, generation)
	}

	t.Run("revise-only APPLIED under switch_pending succeeds and applies", func(t *testing.T) {
		run := newRun(t)
		first, second := addInput(t, run, "revise_plan"), addInput(t, run, "revise_plan")
		ack(t, run, worker, 1, first, second)
		switchPending(t, run, 1)
		code, out := post(run, "/inputs/applied", worker, 1, first, second)
		expect(t, "APPLIED under switch", http.StatusOK, workersvc.ReceiptSwitchPending, code, out)
		if out["active"] != false || len(out["inputs"].([]any)) != 2 {
			t.Fatalf("APPLIED under switch body: %v", out)
		}
		if !appliedAt(t, first) || !appliedAt(t, second) {
			t.Fatal("APPLIED under switch answered 200 but left a revise_plan unapplied")
		}
		// The first reply was lost: the retry of already-applied rows still succeeds.
		code, out = post(run, "/inputs/applied", worker, 1, first, second)
		expect(t, "APPLIED retry under switch", http.StatusOK, workersvc.ReceiptSwitchPending, code, out)
	})

	t.Run("mixed revise_plan and follow_up under switch_pending is refused", func(t *testing.T) {
		run := newRun(t)
		revise, follow := addInput(t, run, "revise_plan"), addInput(t, run, "follow_up")
		ack(t, run, worker, 1, revise, follow)
		switchPending(t, run, 1)
		code, out := post(run, "/inputs/applied", worker, 1, revise, follow)
		expect(t, "mixed APPLIED", http.StatusConflict, workersvc.ReceiptSwitchPending, code, out)
		requireUnapplied(t, "mixed APPLIED", revise, follow)
		code, out = post(run, "/inputs/applied", worker, 1, follow)
		expect(t, "follow_up-only APPLIED", http.StatusConflict, workersvc.ReceiptSwitchPending, code, out)
		requireUnapplied(t, "follow_up-only APPLIED", follow)
	})

	t.Run("a receipt this claim did not take is refused under switch_pending", func(t *testing.T) {
		run := newRun(t)
		revise := addInput(t, run, "revise_plan")
		ack(t, run, worker, 1, revise)
		// The same worker reclaims at generation 2 and a switch is requested there before the
		// new claim ACKs the row: the receipt still belongs to generation 1.
		exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, run)
		switchPending(t, run, 2)
		code, out := post(run, "/inputs/applied", worker, 2, revise)
		expect(t, "non-owned APPLIED", http.StatusConflict, workersvc.ReceiptSwitchPending, code, out)
		requireUnapplied(t, "non-owned APPLIED", revise)
	})

	t.Run("released and stale claims are refused even for revise-only", func(t *testing.T) {
		for _, fence := range []struct{ sql, reason string }{
			{`UPDATE runs SET claim_released_at=now() WHERE id=$1`, workersvc.ReceiptReleased},
			{`UPDATE runs SET status='queued',claim_released_at=now() WHERE id=$1`, workersvc.ReceiptStale},
			{`UPDATE runs SET status='failed' WHERE id=$1`, workersvc.ReceiptStale},
			{`UPDATE runs SET claim_generation=2 WHERE id=$1`, workersvc.ReceiptStale},
		} {
			run := newRun(t)
			revise := addInput(t, run, "revise_plan")
			ack(t, run, worker, 1, revise)
			switchPending(t, run, 1)
			exec(t, fence.sql, run)
			code, out := post(run, "/inputs/applied", worker, 1, revise)
			expect(t, "APPLIED after "+fence.sql, http.StatusConflict, fence.reason, code, out)
			requireUnapplied(t, "APPLIED after "+fence.sql, revise)
		}
	})

	t.Run("same-worker successor generation", func(t *testing.T) {
		run := newRun(t)
		revise := addInput(t, run, "revise_plan")
		ack(t, run, worker, 1, revise)
		switchPending(t, run, 1)
		// The switch released generation 1 and the same worker reclaimed at generation 2.
		exec(t, `UPDATE runs SET claim_generation=2,claim_released_at=NULL,credential_switch_requested_at=NULL,credential_switch_generation=NULL WHERE id=$1`, run)
		code, out := post(run, "/inputs/applied", worker, 1, revise)
		expect(t, "generation-1 APPLIED after reclaim", http.StatusConflict, workersvc.ReceiptStale, code, out)
		requireUnapplied(t, "generation-1 APPLIED after reclaim", revise)
		ack(t, run, worker, 2, revise)
		code, out = post(run, "/inputs/applied", worker, 2, revise)
		expect(t, "generation-2 APPLIED", http.StatusOK, nil, code, out)
		if !appliedAt(t, revise) {
			t.Fatal("generation-2 APPLIED left the revise unapplied")
		}
	})

	// Concurrent release against APPLIED, in both lock orders. A third transaction holds the
	// run row lock while the two contenders queue behind it in a chosen order; Postgres grants
	// the tuple lock to the first waiter. Whichever commits first, the outcome is serialized:
	// released first refuses APPLIED and leaves the row unapplied; APPLIED first applies the
	// row and the release then still succeeds.
	// lockWaiters counts only sessions actually blocked (directly or transitively) behind
	// holderPid's row lock, not every lock-waiting session in the database: a concurrent test
	// in another package sharing this instrumented database, or the pool's own housekeeping,
	// would otherwise inflate the count and either race the deadline or pass too early.
	// Postgres queues concurrent row-lock waiters FIFO: the SECOND waiter here reports
	// pg_blocking_pids() = {first waiter's pid}, not holderPid, because it waits on the tuple's
	// "next in line" lock rather than directly on the holder's transaction id (measured live:
	// with a single FOR UPDATE holder and two concurrent contenders, the release path blocks
	// on the holder's pid while the apply path blocks on the release path's pid). A bare
	// `$1 = ANY(pg_blocking_pids(pid))` therefore undercounts; walk the blocking chain
	// transitively from holderPid instead.
	lockWaiters := func(t *testing.T, holderPid int32, want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		const q = `
WITH RECURSIVE waiters AS (
	SELECT pid, pg_blocking_pids(pid) AS blockers
	FROM pg_stat_activity
	WHERE datname = current_database() AND wait_event_type = 'Lock'
),
blocked AS (
	SELECT pid FROM waiters WHERE $1 = ANY(blockers)
	UNION
	SELECT w.pid FROM waiters w JOIN blocked b ON b.pid = ANY(w.blockers)
)
SELECT count(DISTINCT pid) FROM blocked`
		for {
			var n int
			if err := pool.QueryRow(ctx, q, holderPid).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("lock waiters = %d, want %d", n, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, releaseFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("concurrent release vs APPLIED, release first=%v", releaseFirst), func(t *testing.T) {
			run := newRun(t)
			revise := addInput(t, run, "revise_plan")
			ack(t, run, worker, 1, revise)
			switchPending(t, run, 1)
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx) //nolint:errcheck // committed below
			var holderPid int32
			if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, run); err != nil {
				t.Fatal(err)
			}
			type applyResult struct {
				code int
				out  map[string]any
			}
			applyDone := make(chan applyResult, 1)
			releaseDone := make(chan error, 1)
			var released int64
			startApply := func() {
				go func() {
					code, out := post(run, "/inputs/applied", worker, 1, revise)
					applyDone <- applyResult{code, out}
				}()
			}
			startRelease := func() {
				go func() {
					var err error
					released, err = q.ReleaseCredentialSwitch(ctx, store.ReleaseCredentialSwitchParams{ID: run, WorkerID: pgconv.UUID(worker), Generation: 1})
					releaseDone <- err
				}()
			}
			if releaseFirst {
				startRelease()
				lockWaiters(t, holderPid, 1)
				startApply()
			} else {
				startApply()
				lockWaiters(t, holderPid, 1)
				startRelease()
			}
			lockWaiters(t, holderPid, 2)
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-releaseDone; err != nil {
				t.Fatal(err)
			}
			res := <-applyDone
			if released != 1 {
				t.Fatalf("release rows = %d, want 1 in either order", released)
			}
			if releaseFirst {
				expect(t, "APPLIED after release", http.StatusConflict, workersvc.ReceiptStale, res.code, res.out)
				requireUnapplied(t, "APPLIED after release", revise)
			} else {
				expect(t, "APPLIED before release", http.StatusOK, workersvc.ReceiptSwitchPending, res.code, res.out)
				if !appliedAt(t, revise) {
					t.Fatal("APPLIED before release answered 200 but left the revise unapplied")
				}
			}
		})
	}
}
