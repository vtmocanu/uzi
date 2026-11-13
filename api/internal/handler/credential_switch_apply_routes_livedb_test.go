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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4201: the in-place credential switch apply through the real worker router. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway database; run via ./e2e/run-store-it.sh.

const applyRouteNonce = "nonce-route"

type applyRouteEnv struct {
	pool   *pgxpool.Pool
	router http.Handler
	user   uuid.UUID
	worker uuid.UUID
	tokens map[uuid.UUID]string
	run    uuid.UUID
	op     uuid.UUID
	secret uuid.UUID
	plain  string
	gen    int64
}

func setupApplyRouteLiveDB(t *testing.T) *applyRouteEnv {
	t.Helper()
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
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	svc.SetTxBeginner(pool)
	h := &Handler{q: q, wsvc: svc}
	e := &applyRouteEnv{pool: pool, router: h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil)), tokens: map[uuid.UUID]string{}, gen: 3}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	e.user = uuid.New()
	exec(`INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')`, e.user, fmt.Sprintf("applyroute-%s@e2e", e.user))
	conn, repo := uuid.New(), uuid.New()
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, e.user, []byte("x"))
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, $3, $4, 'main', true)`, repo, conn, "g/"+repo.String(), "https://forge.e2e/g/"+repo.String())
	e.worker = e.seedWorker(t, exec, []string{capability.CredentialSwitchV1, capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1})
	exec(`UPDATE workers SET snapshot_register_nonce = $2, last_heartbeat_at = now() WHERE id = $1`, e.worker, applyRouteNonce)

	e.plain = "route-token-" + uuid.NewString()
	sealed, err := box.Seal([]byte(e.plain))
	if err != nil {
		t.Fatal(err)
	}
	e.secret = uuid.New()
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with) VALUES ($1, $2, 'anthropic_token', 'route', $3, 'master')`, e.secret, e.user, sealed)
	e.run = uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, status_since, worker_id,
	         started_at, claim_generation, credential_override_mode, credential_override_secret_id, last_seq, session_id)
	      VALUES ($1, $2, $3, 'issue', 4201, 't', 'd', 'running', now(), $4, now(), $5, 'pinned', $6, 2, 'sess')`,
		e.run, e.user, repo, e.worker, e.gen, e.secret)
	exec(`INSERT INTO run_messages (run_id, seq, kind, payload, claim_generation) VALUES ($1, 1, 'text', '{"t":"a"}', $2), ($1, 2, 'text', '{"t":"b"}', $2)`, e.run, e.gen)
	exec(`UPDATE runs SET credential_switch_requested_at = now(), credential_switch_generation = $2 WHERE id = $1`, e.run, e.gen)
	if err := pool.QueryRow(ctx, `SELECT credential_switch_op_id FROM runs WHERE id = $1`, e.run).Scan(&e.op); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *applyRouteEnv) seedWorker(t *testing.T, exec func(string, ...any), caps []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e.tokens[id] = token
	exec(`INSERT INTO workers (id, user_id, name, token_hash, protocol_capabilities) VALUES ($1, $2, $3, $4, $5)`, id, e.user, "applyroute-"+id.String(), hash, caps)
	return id
}

func (e *applyRouteEnv) do(worker uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token, ok := e.tokens[worker]; ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *applyRouteEnv) applyBody(op uuid.UUID) string {
	return fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`, e.gen, applyRouteNonce, op)
}

func (e *applyRouteEnv) applyPath() string {
	return "/api/worker/runs/" + e.run.String() + "/credential-switch/apply"
}

func TestWorkerCredentialSwitchApplyRouteLiveDB(t *testing.T) {
	e := setupApplyRouteLiveDB(t)

	t.Run("applied response is no-store and carries the token", func(t *testing.T) {
		rec := e.do(e.worker, "POST", e.applyPath(), e.applyBody(e.op))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"outcome": "applied", "token": e.plain, "ordinal": float64(1), "message_seq": float64(3), "replay": false}
		if fmt.Sprint(body) != fmt.Sprint(want) {
			t.Fatalf("body = %v, want %v", body, want)
		}
	})

	t.Run("a replay says so", func(t *testing.T) {
		rec := e.do(e.worker, "POST", e.applyPath(), e.applyBody(e.op))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["replay"] != true || body["token"] != e.plain {
			t.Fatalf("replay body = %s (err %v)", rec.Body, err)
		}
	})

	t.Run("the token appears nowhere else the worker or the feed reads", func(t *testing.T) {
		inputs := e.do(e.worker, "GET", "/api/worker/runs/"+e.run.String()+"/inputs", "")
		if inputs.Code != http.StatusOK || strings.Contains(inputs.Body.String(), e.plain) {
			t.Fatalf("/inputs: status %d, token leaked=%v", inputs.Code, strings.Contains(inputs.Body.String(), e.plain))
		}
		ack := e.do(e.worker, "POST", "/api/worker/runs/"+e.run.String()+"/state", fmt.Sprintf(`{"status":"running","claim_generation":%d}`, e.gen))
		if ack.Code != http.StatusOK || strings.Contains(ack.Body.String(), e.plain) {
			t.Fatalf("/state: status %d, token leaked=%v body %s", ack.Code, strings.Contains(ack.Body.String(), e.plain), ack.Body)
		}
		var leaked int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_messages WHERE run_id = $1 AND payload::text LIKE '%' || $2 || '%'`, e.run, e.plain).Scan(&leaked); err != nil || leaked != 0 {
			t.Fatalf("run_messages carrying the token = %d (err %v)", leaked, err)
		}
	})

	t.Run("a run the worker does not hold is the exact 404", func(t *testing.T) {
		other := e.seedWorker(t, func(sql string, args ...any) {
			if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
				t.Fatal(err)
			}
		}, []string{capability.CredentialSwitchInPlaceV1})
		rec := e.do(other, "POST", e.applyPath(), e.applyBody(e.op))
		if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != `{"error":"run not found for this worker"}` {
			t.Fatalf("status %d body %q", rec.Code, rec.Body)
		}
	})

	t.Run("an unknown worker path is chi's plain-text 404", func(t *testing.T) {
		rec := e.do(e.worker, "POST", "/api/worker/runs/"+e.run.String()+"/credential-switch/nope", "{}")
		if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("status %d content-type %q body %q, want chi's plain-text 404", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
	})

	t.Run("the request is strict", func(t *testing.T) {
		for name, body := range map[string]string{
			"unknown field":   fmt.Sprintf(`{"claim_generation":%d,"register_nonce":"n","op_id":%q,"extra":1}`, e.gen, e.op),
			"bad op id":       fmt.Sprintf(`{"claim_generation":%d,"register_nonce":"n","op_id":"nope"}`, e.gen),
			"missing nonce":   fmt.Sprintf(`{"claim_generation":%d,"op_id":%q}`, e.gen, e.op),
			"trailing value":  e.applyBody(e.op) + ` {}`,
			"negative total":  fmt.Sprintf(`{"claim_generation":%d,"register_nonce":"n","op_id":%q,"old_cumulative":{"a":-1}}`, e.gen, e.op),
			"fractional":      fmt.Sprintf(`{"claim_generation":%d,"register_nonce":"n","op_id":%q,"old_cumulative":{"a":1.5}}`, e.gen, e.op),
			"too many totals": fmt.Sprintf(`{"claim_generation":%d,"register_nonce":"n","op_id":%q,"old_cumulative":%s}`, e.gen, e.op, manyEntries(65)),
		} {
			if rec := e.do(e.worker, "POST", e.applyPath(), body); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status %d body %s, want 400", name, rec.Code, rec.Body)
			}
		}
	})
}

func manyEntries(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"k%d":1`, i)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestWorkerCredentialSwitchStateUncertainRouteLiveDB(t *testing.T) {
	e := setupApplyRouteLiveDB(t)
	path := "/api/worker/runs/" + e.run.String() + "/state"
	body := fmt.Sprintf(`{"status":"credential_switch_uncertain","claim_generation":%d,"register_nonce":%q,"op_id":%q}`, e.gen, applyRouteNonce, e.op)
	rec := e.do(e.worker, "POST", path, body)
	var ack map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ack) != nil || ack["disposition"] != "uncertain" {
		t.Fatalf("uncertain: status %d body %s", rec.Code, rec.Body)
	}
	// A wall pause pending is a distinct 409 refusal.
	e2 := setupApplyRouteLiveDB(t)
	if _, err := e2.pool.Exec(context.Background(), `UPDATE runs SET pause_requested_at = now(), pause_mode = 'wall' WHERE id = $1`, e2.run); err != nil {
		t.Fatal(err)
	}
	body = fmt.Sprintf(`{"status":"credential_switch_uncertain","claim_generation":%d,"register_nonce":%q,"op_id":%q}`, e2.gen, applyRouteNonce, e2.op)
	rec = e2.do(e2.worker, "POST", "/api/worker/runs/"+e2.run.String()+"/state", body)
	var refusal map[string]any
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &refusal) != nil || refusal["reason"] != "park_pending" {
		t.Fatalf("wall-pending uncertain: status %d body %s, want 409 park_pending", rec.Code, rec.Body)
	}
}

// Issue #4201 m3a: a worker whose current registration negotiated in-place apply gets a typed 409 for
// the v1 release, and registration through the real route is what stamps the negotiation.
func TestWorkerCredentialSwitchV1ReleaseRetiredRouteLiveDB(t *testing.T) {
	ctx := context.Background()
	state := func(e *applyRouteEnv, body string) (int, map[string]any) {
		rec := e.do(e.worker, "POST", "/api/worker/runs/"+e.run.String()+"/state", body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	release := func(e *applyRouteEnv) string {
		return fmt.Sprintf(`{"status":"credential_switch","claim_generation":%d}`, e.gen)
	}

	t.Run("negotiated registration: 409 with a stable reason and nothing written", func(t *testing.T) {
		e := setupApplyRouteLiveDB(t)
		if _, err := e.pool.Exec(ctx, `UPDATE workers SET credential_switch_apply_nonce = snapshot_register_nonce WHERE id = $1`, e.worker); err != nil {
			t.Fatal(err)
		}
		code, out := state(e, release(e))
		if code != http.StatusConflict || out["reason"] != "credential_switch_release_retired" {
			t.Fatalf("v1 release: status %d body %v, want 409 credential_switch_release_retired", code, out)
		}
		var status string
		var released bool
		if err := e.pool.QueryRow(ctx, `SELECT status, claim_released_at IS NOT NULL FROM runs WHERE id = $1`, e.run).Scan(&status, &released); err != nil || status != "running" || released {
			t.Fatalf("run status=%s released=%v err=%v, want running and unreleased", status, released, err)
		}
		// The un-fenced v1 hold gets the same refusal; the op-fenced hold is accepted.
		code, out = state(e, fmt.Sprintf(`{"status":"credential_switch_held","claim_generation":%d,"register_nonce":%q}`, e.gen, applyRouteNonce))
		if code != http.StatusConflict || out["reason"] != "credential_switch_release_retired" {
			t.Fatalf("v1 hold: status %d body %v, want 409 credential_switch_release_retired", code, out)
		}
		code, out = state(e, fmt.Sprintf(`{"status":"credential_switch_held","claim_generation":%d,"register_nonce":%q,"op_id":%q}`, e.gen, applyRouteNonce, e.op))
		if code != http.StatusOK || out["disposition"] != "held" {
			t.Fatalf("op-fenced hold: status %d body %v, want 200 held", code, out)
		}
	})
	t.Run("old replica re-registration: the v1 release is accepted", func(t *testing.T) {
		e := setupApplyRouteLiveDB(t)
		if _, err := e.pool.Exec(ctx, `UPDATE workers SET credential_switch_apply_nonce = snapshot_register_nonce WHERE id = $1`, e.worker); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE workers SET snapshot_register_nonce = 'nonce-old-replica' WHERE id = $1`, e.worker); err != nil {
			t.Fatal(err)
		}
		if code, out := state(e, release(e)); code != http.StatusOK {
			t.Fatalf("mid-roll v1 release: status %d body %v, want 200", code, out)
		}
	})
	t.Run("register stamps the nonce only when the capability is advertised", func(t *testing.T) {
		e := setupApplyRouteLiveDB(t)
		register := func(caps string) {
			rec := e.do(e.worker, "POST", "/api/worker/register", fmt.Sprintf(`{"version":"1.0.0","protocol_capabilities":%s}`, caps))
			if rec.Code != http.StatusOK {
				t.Fatalf("register: status %d body %s", rec.Code, rec.Body)
			}
		}
		stamped := func() (apply, snap string) {
			if err := e.pool.QueryRow(ctx, `SELECT coalesce(credential_switch_apply_nonce, ''), coalesce(snapshot_register_nonce, '') FROM workers WHERE id = $1`, e.worker).Scan(&apply, &snap); err != nil {
				t.Fatal(err)
			}
			return apply, snap
		}
		register(`["credential_switch_in_place_v1"]`)
		first, snap := stamped()
		if first == "" || first != snap {
			t.Fatalf("after an in-place registration apply=%q snapshot=%q, want equal and set", first, snap)
		}
		register(`[]`)
		if apply, snap := stamped(); apply != first || apply == snap {
			t.Fatalf("after a registration without the capability apply=%q snapshot=%q, want the stale %q", apply, snap, first)
		}
	})
}
