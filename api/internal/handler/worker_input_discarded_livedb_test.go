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

// TestWorkerInputsDiscardedLiveDB pins issue #1604's DISCARDED receipt through the real worker
// router: POST /api/worker/runs/{id}/inputs/discarded takes the APPLIED body and answers the
// APPLIED shape and status mapping. It settles the claim's own approve_plan rows (applied_at set,
// disposition 'superseded'), so they leave the replay list without counting as approval. A
// non-approve id is a 400; a receipt this claim did not take, a released or stale claim, and a
// row already applied as a real approval are 409s; a pending credential switch is accepted; a
// retry of a lost reply succeeds.
func TestWorkerInputsDiscardedLiveDB(t *testing.T) {
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
	user, worker, legacy := uuid.New(), uuid.New(), uuid.New()
	exec(t, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("discard-%s@e2e", user))
	tokens := make(map[uuid.UUID]string)
	for id, caps := range map[uuid.UUID][]string{worker: {capability.InputReceiptsV1}, legacy: {}} {
		token, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
		exec(t, `INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,$3,$4,$5)`,
			id, user, "discard-"+id.String(), hash, caps)
	}
	newRun := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, `INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','awaiting_approval',$3,1)`, id, user, worker)
		return id
	}
	// addInput inserts a row this worker's claim at generation received (ACKed).
	addInput := func(t *testing.T, run uuid.UUID, kind string, generation int64) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id)
		      VALUES ($1,$2,'x',now(),$3,$4) RETURNING id`, run, kind, generation, worker).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	post := func(run uuid.UUID, path string, who uuid.UUID, generation int64, ids ...int64) (int, map[string]any) {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprint(id)
		}
		body := fmt.Sprintf(`{"ids":[%s],"claim_generation":%d}`, strings.Join(parts, ","), generation)
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+path, strings.NewReader(body))
		if who != uuid.Nil {
			req.Header.Set("Authorization", "Bearer "+tokens[who])
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	discard := func(run uuid.UUID, who uuid.UUID, generation int64, ids ...int64) (int, map[string]any) {
		return post(run, "/inputs/discarded", who, generation, ids...)
	}
	expect := func(t *testing.T, stage string, want int, wantReason any, code int, out map[string]any) {
		t.Helper()
		if code != want || out["reason"] != wantReason {
			t.Fatalf("%s: status %d reason %v, want %d reason %v (%v)", stage, code, out["reason"], want, wantReason, out)
		}
	}
	state := func(t *testing.T, id int64) (bool, string) {
		t.Helper()
		var applied bool
		var disposition *string
		if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL, disposition FROM run_user_inputs WHERE id=$1`, id).Scan(&applied, &disposition); err != nil {
			t.Fatal(err)
		}
		if disposition == nil {
			return applied, ""
		}
		return applied, *disposition
	}
	requireSuperseded := func(t *testing.T, stage string, id int64) {
		t.Helper()
		if applied, disposition := state(t, id); !applied || disposition != "superseded" {
			t.Fatalf("%s: input %d applied=%v disposition=%q, want applied and superseded", stage, id, applied, disposition)
		}
	}
	requireUnsettled := func(t *testing.T, stage string, id int64) {
		t.Helper()
		if applied, disposition := state(t, id); applied || disposition != "" {
			t.Fatalf("%s: input %d applied=%v disposition=%q, want unsettled", stage, id, applied, disposition)
		}
	}

	t.Run("unauthenticated and non-capable workers are refused", func(t *testing.T) {
		run := newRun(t)
		approve := addInput(t, run, "approve_plan", 1)
		code, _ := discard(run, uuid.Nil, 1, approve)
		if code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated discard: status %d", code)
		}
		code, out := discard(run, legacy, 1, approve)
		expect(t, "non-capable discard", http.StatusBadRequest, nil, code, out)
		requireUnsettled(t, "refused discard", approve)
	})

	t.Run("own approve rows are settled as superseded and leave the replay list", func(t *testing.T) {
		run := newRun(t)
		first, second := addInput(t, run, "approve_plan", 1), addInput(t, run, "approve_plan", 1)
		cancel := addInput(t, run, "cancel", 1)
		code, out := discard(run, worker, 1, first, second)
		expect(t, "discard", http.StatusOK, nil, code, out)
		if out["active"] != true || len(out["inputs"].([]any)) != 2 {
			t.Fatalf("discard body: %v", out)
		}
		requireSuperseded(t, "discard", first)
		requireSuperseded(t, "discard", second)
		replay, err := q.ListReplayRunInputs(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(replay) != 1 || replay[0].ID != cancel {
			t.Fatalf("replay list after discard = %+v, want only the cancel %d", replay, cancel)
		}
		// A lost reply's retry succeeds and changes nothing.
		code, out = discard(run, worker, 1, first, second)
		expect(t, "discard retry", http.StatusOK, nil, code, out)
		// A retry still succeeds after the claim released (the first reply was lost).
		exec(t, `UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
		code, out = discard(run, worker, 1, first)
		expect(t, "discard retry after release", http.StatusOK, workersvc.ReceiptReleased, code, out)
		// APPLIED on a discarded approve is refused: it never counted as approval.
		code, out = post(run, "/inputs/applied", worker, 1, first)
		expect(t, "APPLIED after discard", http.StatusConflict, workersvc.ReceiptReleased, code, out)
	})

	t.Run("a non-approve row is invalid, alone or in a mixed batch", func(t *testing.T) {
		run := newRun(t)
		approve := addInput(t, run, "approve_plan", 1)
		for _, kind := range []string{"follow_up", "cancel", "revise_plan", "reject_plan", "answer"} {
			other := addInput(t, run, kind, 1)
			code, out := discard(run, worker, 1, other)
			expect(t, kind+" discard", http.StatusBadRequest, nil, code, out)
			code, out = discard(run, worker, 1, approve, other)
			expect(t, "mixed "+kind+" discard", http.StatusBadRequest, nil, code, out)
			requireUnsettled(t, kind+" discard", other)
		}
		requireUnsettled(t, "mixed discard", approve)
	})

	t.Run("a receipt this claim did not take is a conflict", func(t *testing.T) {
		run := newRun(t)
		approve := addInput(t, run, "approve_plan", 1)
		exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, run)
		code, out := discard(run, worker, 2, approve)
		expect(t, "non-own discard", http.StatusConflict, "", code, out)
		requireUnsettled(t, "non-own discard", approve)
	})

	t.Run("released and stale claims are refused", func(t *testing.T) {
		for _, fence := range []struct{ sql, reason string }{
			{`UPDATE runs SET claim_released_at=now() WHERE id=$1`, workersvc.ReceiptReleased},
			{`UPDATE runs SET status='queued',claim_released_at=now() WHERE id=$1`, workersvc.ReceiptStale},
			{`UPDATE runs SET status='failed' WHERE id=$1`, workersvc.ReceiptStale},
			{`UPDATE runs SET claim_generation=2 WHERE id=$1`, workersvc.ReceiptStale},
		} {
			run := newRun(t)
			approve := addInput(t, run, "approve_plan", 1)
			exec(t, fence.sql, run)
			code, out := discard(run, worker, 1, approve)
			expect(t, "discard after "+fence.sql, http.StatusConflict, fence.reason, code, out)
			requireUnsettled(t, "discard after "+fence.sql, approve)
		}
	})

	t.Run("a pending credential switch is accepted", func(t *testing.T) {
		run := newRun(t)
		approve := addInput(t, run, "approve_plan", 1)
		exec(t, `UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=1 WHERE id=$1`, run)
		code, out := discard(run, worker, 1, approve)
		expect(t, "discard under switch", http.StatusOK, workersvc.ReceiptSwitchPending, code, out)
		if out["active"] != false {
			t.Fatalf("discard under switch body: %v", out)
		}
		requireSuperseded(t, "discard under switch", approve)
	})

	t.Run("a row already applied as a real approval is a conflict", func(t *testing.T) {
		run := newRun(t)
		approve := addInput(t, run, "approve_plan", 1)
		code, out := post(run, "/inputs/applied", worker, 1, approve)
		expect(t, "APPLIED", http.StatusOK, nil, code, out)
		code, out = discard(run, worker, 1, approve)
		expect(t, "discard of an applied approve", http.StatusConflict, "", code, out)
		if applied, disposition := state(t, approve); !applied || disposition != "" {
			t.Fatalf("real approve after a refused discard: applied=%v disposition=%q", applied, disposition)
		}
	})

	t.Run("an unknown run is a typed 404", func(t *testing.T) {
		code, out := discard(uuid.New(), worker, 1, 1)
		expect(t, "discard on an unknown run", http.StatusNotFound, workersvc.ReceiptStale, code, out)
	})
}
