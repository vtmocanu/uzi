package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// PRD #4206 through the real router. Skipped without UZI_TEST_DATABASE_URL.

type pruneRouteFx struct {
	*hibRouteFx
	id    uuid.UUID
	pod   uuid.UUID
	token string
	jwt   string
}

// newPruneRouteFx seeds a Docker worker that advertises hibernation_cache_prune_v1 and admits its hibernation.
func newPruneRouteFx(t *testing.T, withCapability bool) *pruneRouteFx {
	t.Helper()
	f := &pruneRouteFx{hibRouteFx: newHibRouteFx(t)}
	f.set.on = true
	f.advertise()
	// The advertisement is a singleton row other tests read: leave none behind.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `UPDATE controller_report_status SET hibernation_version = NULL, hibernation_advertised_at = NULL WHERE id = 1`)
	})
	f.id, f.pod = uuid.New(), uuid.New()
	f.token = "prune-" + f.id.String()
	caps := []string{}
	if withCapability {
		caps = append(caps, capability.HibernationCachePruneV1)
	}
	cliMustExec(t, f.pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared, registered_pod_uid, docker_enabled, protocol_capabilities, snapshot_register_nonce)
		VALUES ($1, $2, 'prune', $3, 'online', now(), 'hosted', 'm', 'base', $4, true, $5, 'nonce-1')`, f.id, f.admin, jointoken.Hash(f.token), f.pod, caps)
	f.jwt = cliMintJWT(t, f.pool, f.admin)
	if rec := cookieReq(t, f.router, http.MethodPost, "/api/admin/workers/"+f.id.String()+"/hibernate", f.jwt, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("hibernate = %d %s", rec.Code, rec.Body.String())
	}
	return f
}

func (f *pruneRouteFx) heartbeat(body string) *httptest.ResponseRecorder {
	return bearerReqBody(f.router, http.MethodPost, "/api/worker/heartbeat", f.token, body)
}

func (f *pruneRouteFx) prune(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("heartbeat body %q: %v", rec.Body.String(), err)
	}
	raw, ok := resp["hibernation_cache_prune"]
	if !ok {
		t.Fatalf("no hibernation_cache_prune member: %s", rec.Body.String())
	}
	if string(raw) == "null" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *pruneRouteFx) attempt(t *testing.T) (op, nonce, phase string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT cleanup_op_id::text, cleanup_register_nonce, cleanup_phase FROM workers WHERE id=$1`, f.id).Scan(&op, &nonce, &phase); err != nil {
		t.Fatal(err)
	}
	return
}

func TestWorkerHeartbeatHibernationCachePruneLiveDB(t *testing.T) {
	f := newPruneRouteFx(t, true)
	op, nonce, _ := f.attempt(t)

	// 1. A plain heartbeat shows the requested attempt, bound to op id and nonce.
	rec := f.heartbeat(`{"version":"1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body.String())
	}
	got := f.prune(t, rec)
	if got["op_id"] != op || got["register_nonce"] != nonce || got["phase"] != "requested" || got["cancel_requested"] != false || got["reconcile_seq"] != float64(0) {
		t.Fatalf("requested projection = %v", got)
	}

	// 2. Stale and mismatched reports are 409 and change nothing.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ready := func(o, n string) string {
		return `{"hibernation_cache_prune_report":{"op_id":"` + o + `","register_nonce":"` + n + `","kind":"ready","local_claims":0,"local_executions":0,"custody_clear":true,"custody_checked_at":"` + now + `","gate_exclusive":true}}`
	}
	for name, body := range map[string]string{
		"other op":    ready(uuid.NewString(), nonce),
		"other nonce": ready(op, "nonce-other"),
	} {
		if rec := f.heartbeat(body); rec.Code != http.StatusConflict {
			t.Fatalf("%s = %d %s, want 409", name, rec.Code, rec.Body.String())
		}
	}
	if _, _, phase := f.attempt(t); phase != "requested" {
		t.Fatalf("a 409 moved the phase to %q", phase)
	}

	// 3. A valid ready commits started BEFORE the response is built: the same response shows it.
	rec = f.heartbeat(ready(op, nonce))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.prune(t, rec); got["phase"] != "started" {
		t.Fatalf("response to ready shows %v, want started", got)
	}

	// 4. A confirmed result settles and the response shows settled.
	result := `{"hibernation_cache_prune_report":{"op_id":"` + op + `","register_nonce":"` + nonce + `","kind":"result","steps":[{"command":"image_prune_dangling","state":"exited_ok"},{"command":"builder_prune","state":"exited_ok"}],"gate_exclusive_throughout":true}}`
	rec = f.heartbeat(result)
	if rec.Code != http.StatusOK {
		t.Fatalf("result = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.prune(t, rec); got["phase"] != "settled" {
		t.Fatalf("response to result shows %v, want settled", got)
	}
	var outcome string
	if err := f.pool.QueryRow(context.Background(), `SELECT hibernation_cleanup FROM workers WHERE id=$1`, f.id).Scan(&outcome); err != nil || outcome != "pruned" {
		t.Fatalf("outcome = %q, %v", outcome, err)
	}
}

func TestWorkerHeartbeatHibernationCachePruneWithoutCapabilityLiveDB(t *testing.T) {
	f := newPruneRouteFx(t, false)
	rec := f.heartbeat(`{"version":"1"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "hibernation_cache_prune") {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.heartbeat(`{"hibernation_cache_prune_report":{"op_id":"` + uuid.NewString() + `","register_nonce":"nonce-1","kind":"ready"}}`); rec.Code != http.StatusConflict {
		t.Fatalf("report from a worker without the capability = %d, want 409", rec.Code)
	}
	var outcome, reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT hibernation_cleanup, hibernation_cleanup_reason FROM workers WHERE id=$1`, f.id).Scan(&outcome, &reason); err != nil || outcome != "skipped" || reason != "unsupported" {
		t.Fatalf("outcome = %q/%q, %v; want skipped/unsupported", outcome, reason, err)
	}
}

func TestControllerCleanupStoppedAndDeleteRouteLiveDB(t *testing.T) {
	f := newPruneRouteFx(t, true)
	op, nonce, _ := f.attempt(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if rec := f.heartbeat(`{"hibernation_cache_prune_report":{"op_id":"` + op + `","register_nonce":"` + nonce + `","kind":"ready","local_claims":0,"local_executions":0,"custody_clear":true,"custody_checked_at":"` + now + `","gate_exclusive":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("ready = %d %s", rec.Code, rec.Body.String())
	}
	// An unsettled attempt keeps the worker: DELETE is a 409, never a removal.
	if rec := cookieReq(t, f.router, http.MethodDelete, "/api/workers/"+f.id.String(), f.jwt, ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete with an unsettled attempt = %d %s, want 409", rec.Code, rec.Body.String())
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM workers WHERE id=$1`, f.id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the worker was removed: %d %v", n, err)
	}

	cliMustExec(t, f.pool, `UPDATE workers SET cleanup_phase='unknown', cleanup_unknown_at=now() WHERE id=$1`, f.id)
	_, hop := f.state(f.id)
	path := "/api/controller/workers/" + f.id.String() + "/hibernation"
	stopped := func(cleanupOp, pod string) string {
		return `{"op_id":"` + *hop + `","transition":"cleanup_stopped","deployment_uid":"` + uuid.NewString() + `","pod_uid":"` + pod + `","cleanup_op_id":"` + cleanupOp + `"}`
	}
	if rec := f.controller(path, strings.Replace(stopped(op, f.pod.String()), `,"cleanup_op_id":"`+op+`"`, "", 1), true); rec.Code != http.StatusBadRequest {
		t.Fatalf("cleanup_stopped without cleanup_op_id = %d, want 400", rec.Code)
	}
	if rec := f.controller(path, stopped(uuid.NewString(), f.pod.String()), true); rec.Code != http.StatusConflict {
		t.Fatalf("mismatched cleanup op = %d, want 409", rec.Code)
	}
	if rec := f.controller(path, stopped(op, uuid.NewString()), true); rec.Code != http.StatusConflict {
		t.Fatalf("mismatched pod = %d, want 409", rec.Code)
	}
	if _, _, phase := f.attempt(t); phase != "unknown" {
		t.Fatalf("a rejected report moved the phase to %q", phase)
	}
	if rec := f.controller(path, stopped(op, f.pod.String()), true); rec.Code != http.StatusOK {
		t.Fatalf("matching cleanup_stopped = %d %s", rec.Code, rec.Body.String())
	}
	if _, _, phase := f.attempt(t); phase != "settled" {
		t.Fatalf("phase after cleanup_stopped = %q", phase)
	}
}
