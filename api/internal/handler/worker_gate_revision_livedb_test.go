package handler

import (
	"context"
	"encoding/binary"
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

// TestWorkerGateRevisionWireLiveDB drives the REAL worker routes for PRD #1795 M1:
//
//   - GET /api/worker/runs/{id}/inputs carries each verdict row's persisted gate_binding and
//     gate_revision on BOTH delivery paths (a receipt-capable worker's read-only replay, and a
//     legacy worker's consume-on-read), and POST /inputs/ack echoes them; a legacy row carries
//     neither key. The rows are inserted directly (the verdict inserts stamp in a later
//     milestone).
//   - POST /state for awaiting_approval answers the allocated revision as a top-level
//     gate_revision on the ACK, 409 {run, reason} for a refused presentation, and 400
//     claim_generation_required for an id-bearing report without a claim generation.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestWorkerGateRevisionWireLiveDB(t *testing.T) {
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
	svc := workersvc.New(q, box, workersvc.Params{RunGateRefusalMax: 3})
	svc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: svc}
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("gaterev-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))

	tokens := map[uuid.UUID]string{}
	newWorker := func(caps []string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		token, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
		exec(`INSERT INTO workers (id, user_id, name, token_hash, protocol_capabilities) VALUES ($1, $2, $3, $4, $5)`,
			id, userID, "gaterev-"+id.String()[:8], hash, caps)
		return id
	}
	newRun := func(worker uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		        claim_generation, auto_approve, plan_source, session_id)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1, false, 'agent', 'sess')`, id, userID, repoID, uniq(id), worker)
		return id
	}
	call := func(worker, run uuid.UUID, method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, "/api/worker/runs/"+run.String()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokens[worker])
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return out
	}
	// seedInputs inserts one row per binding shape and returns their ids by label.
	seedInputs := func(run uuid.UUID) map[string]int64 {
		t.Helper()
		ids := map[string]int64{}
		for _, c := range []struct {
			label, kind string
			binding     any
			revision    any
		}{
			{"bound", "approve_plan", "bound", int64(2)},
			{"unbound", "reject_plan", "unbound", nil},
			{"legacy", "follow_up", nil, nil},
		} {
			var id int64
			if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id, kind, body, gate_binding, gate_revision)
			      VALUES ($1, $2, 'x', $3, $4) RETURNING id`, run, c.kind, c.binding, c.revision).Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids[c.label] = id
		}
		return ids
	}
	assertBindings := func(stage string, inputs []any, ids map[string]int64) {
		t.Helper()
		byID := map[int64]map[string]any{}
		for _, raw := range inputs {
			in := raw.(map[string]any)
			byID[int64(in["id"].(float64))] = in
		}
		for label, id := range ids {
			in, ok := byID[id]
			if !ok {
				continue
			}
			binding, hasBinding := in["gate_binding"]
			revision, hasRevision := in["gate_revision"]
			switch label {
			case "bound":
				if binding != "bound" || !hasRevision || revision.(float64) != 2 {
					t.Fatalf("%s: bound row = %v, want gate_binding bound, gate_revision 2", stage, in)
				}
			case "unbound":
				if binding != "unbound" || hasRevision {
					t.Fatalf("%s: unbound row = %v, want gate_binding unbound and no gate_revision", stage, in)
				}
			case "legacy":
				if hasBinding || hasRevision {
					t.Fatalf("%s: legacy row = %v, want neither key", stage, in)
				}
			}
		}
	}

	t.Run("receipt replay and ACK carry the binding", func(t *testing.T) {
		worker := newWorker([]string{capability.InputReceiptsV1})
		run := newRun(worker)
		ids := seedInputs(run)
		out := call(worker, run, http.MethodGet, "/inputs", "", 200)
		if out["receipts"] != true {
			t.Fatalf("receipt-capable GET lacks receipts:true: %v", out)
		}
		inputs := out["inputs"].([]any)
		if len(inputs) != 3 {
			t.Fatalf("GET inputs = %v, want 3", inputs)
		}
		assertBindings("receipt replay", inputs, ids)
		ack := call(worker, run, http.MethodPost, "/inputs/ack",
			fmt.Sprintf(`{"ids":[%d,%d,%d],"claim_generation":1}`, ids["bound"], ids["unbound"], ids["legacy"]), 200)
		ackInputs := ack["inputs"].([]any)
		if len(ackInputs) != 3 {
			t.Fatalf("ACK inputs = %v, want 3", ack)
		}
		assertBindings("receipt ACK", ackInputs, ids)
	})

	t.Run("legacy consume carries the binding", func(t *testing.T) {
		worker := newWorker([]string{})
		run := newRun(worker)
		ids := seedInputs(run)
		out := call(worker, run, http.MethodGet, "/inputs", "", 200)
		if _, receipts := out["receipts"]; receipts {
			t.Fatalf("legacy GET declared receipts: %v", out)
		}
		inputs := out["inputs"].([]any)
		if len(inputs) != 3 {
			t.Fatalf("GET inputs = %v, want 3", inputs)
		}
		assertBindings("legacy consume", inputs, ids)
	})

	t.Run("state ACK carries the allocated revision; refusals answer typed", func(t *testing.T) {
		worker := newWorker([]string{capability.InputReceiptsV1, capability.GateRevisionV1})
		run := newRun(worker)
		a, b := uuid.New(), uuid.New()
		report := func(pid *uuid.UUID, gen string, want int) map[string]any {
			t.Helper()
			body := `{"status":"awaiting_approval","plan_md":"# Plan","session_id":"sess"`
			if gen != "" {
				body += `,"claim_generation":` + gen
			}
			if pid != nil {
				body += `,"presentation_id":"` + pid.String() + `"`
			}
			return call(worker, run, http.MethodPost, "/state", body+"}", want)
		}
		// Every other state's ACK carries no gate_revision.
		if out := call(worker, run, http.MethodPost, "/state", `{"status":"running","claim_generation":1}`, 200); out["gate_revision"] != nil {
			t.Fatalf("running ACK carried gate_revision: %v", out)
		}
		if got := report(&a, "1", 200)["gate_revision"]; got != float64(1) {
			t.Fatalf("first ACK gate_revision = %v, want 1", got)
		}
		// The lost-ACK retry returns the same revision.
		if got := report(&a, "1", 200)["gate_revision"]; got != float64(1) {
			t.Fatalf("retry ACK gate_revision = %v, want 1", got)
		}
		if got := report(&b, "1", 200)["gate_revision"]; got != float64(2) {
			t.Fatalf("new presentation ACK gate_revision = %v, want 2", got)
		}
		// The DTO exposes the revision too.
		out := report(&b, "1", 200)
		if dto := out["run"].(map[string]any); dto["gate_revision"] != float64(2) {
			t.Fatalf("run DTO gate_revision = %v, want 2", dto["gate_revision"])
		}
		// An id-less (old worker) report allocates and is answered too.
		if got := report(nil, "", 200)["gate_revision"]; got != float64(3) {
			t.Fatalf("id-less ACK gate_revision = %v, want 3", got)
		}
		refused := report(&a, "1", 409)
		if refused["reason"] != "gate_presentation_historical" || refused["run"] == nil {
			t.Fatalf("historical 409 = %v, want reason gate_presentation_historical with the run", refused)
		}
		if _, has := refused["gate_revision"]; has {
			t.Fatalf("a refusal carried gate_revision: %v", refused)
		}
		bad := report(&a, "", 400)
		if bad["reason"] != "claim_generation_required" {
			t.Fatalf("id-bearing report without a generation = %v, want reason claim_generation_required", bad)
		}
	})
}
