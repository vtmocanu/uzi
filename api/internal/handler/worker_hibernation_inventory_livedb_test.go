package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4208 M1: the worker routes over the real mounted worker router, driven by the shared wire
// fixture. Skipped without UZI_TEST_DATABASE_URL.

const invWireFixture = "../../../fixtures/hibernation-inventory/wire.json"

type invRouteFx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	h      *Handler
	router http.Handler
	token  string
	id     uuid.UUID
	fx     map[string]json.RawMessage
}

func newInvRouteFx(t *testing.T) *invRouteFx {
	t.Helper()
	raw, err := os.ReadFile(invWireFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	h, _, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, true)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, owner) })
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f := &invRouteFx{t: t, pool: pool, h: h, token: token, id: uuid.New(), fx: fx,
		router: h.WorkerRoutes(mw.NewLimiter(1_000_000, time.Hour, nil))}
	// Identity is the fixture's, so the fixture bodies bind without edits.
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared,
			registered_pod_uid, protocol_capabilities, snapshot_register_nonce, hibernation_state, hibernation_op_id, hibernation_requested_at)
		VALUES ($1, $2, 'inv', $3, 'online', now(), 'hosted', 'm', 'base', $4, ARRAY[$5]::text[], $6, 'preparing', $7, now())`,
		f.id, owner, hash, uuid.MustParse("66666666-6666-6666-6666-666666666666"), capability.HibernationInventoryV1,
		"11111111-2222-4333-8444-555555555555", uuid.MustParse("55555555-5555-5555-5555-555555555555"))
	return f
}

func (f *invRouteFx) observe(mod ...func(*workersvc.HibernationTransition)) {
	f.t.Helper()
	tr := workersvc.HibernationTransition{OpID: "55555555-5555-5555-5555-555555555555", Transition: workersvc.TransitionObserved,
		DeploymentUID: uuid.NewString(), ResourceVersion: "12340", PodUID: "66666666-6666-6666-6666-666666666666",
		PVCs: []workersvc.HibernationPVC{
			{Role: "data", Name: "uzi-hw-data", UID: "88888888-8888-8888-8888-888888888881", ClassReclaimPolicy: "Retain"},
			{Role: "nix", Name: "uzi-hw-nix", UID: "88888888-8888-8888-8888-888888888882", ClassReclaimPolicy: "Retain"},
			{Role: "dind_data", Name: "uzi-hw-dind", UID: "88888888-8888-8888-8888-888888888883", ClassReclaimPolicy: "Retain"},
		}}
	pods := []string{"66666666-6666-6666-6666-666666666666"}
	tr.PodUIDs, tr.PVCUserPods = &pods, &pods
	for _, m := range mod {
		m(&tr)
	}
	if _, err := f.h.wsvc.ApplyTransition(context.Background(), f.id, tr); err != nil {
		f.t.Fatalf("observed: %v", err)
	}
}

func (f *invRouteFx) post(path string, body []byte, bearer bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// mutated is the named fixture member with one top-level key replaced (nil deletes it).
func (f *invRouteFx) mutated(name string, set map[string]any) []byte {
	f.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.fx[name], &m); err != nil {
		f.t.Fatal(err)
	}
	for k, v := range set {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *invRouteFx) row() (status string, reason *string, entriesNull bool) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, invalidated_reason, entries IS NULL FROM worker_hibernation_inventories WHERE worker_id = $1`, f.id).Scan(&status, &reason, &entriesNull); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *invRouteFx) heartbeat(body string) *httptest.ResponseRecorder {
	return f.post("/api/worker/heartbeat", []byte(body), true)
}

func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

const (
	invPath   = "/api/worker/hibernation/inventory"
	finalPath = "/api/worker/hibernation/exclusion-final"
)

func TestWorkerHibernationInventoryRoutesLiveDB(t *testing.T) {
	f := newInvRouteFx(t)
	f.observe()

	// Heartbeat: the awaiting member is exactly the fixture's, and an old worker's body (no
	// hibernation_exclusion) is untouched.
	rec := f.heartbeat(`{"version":"v"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var wantMembers map[string]json.RawMessage
	_ = json.Unmarshal(f.fx["heartbeat_response_member"], &wantMembers)
	var wantAwaiting map[string]json.RawMessage
	_ = json.Unmarshal(wantMembers["awaiting"], &wantAwaiting)
	if !jsonEqual(t, resp["hibernation_inventory"], wantAwaiting["hibernation_inventory"]) {
		t.Fatalf("awaiting member:\n got %s\nwant %s", resp["hibernation_inventory"], wantAwaiting["hibernation_inventory"])
	}

	// Authentication is the bearer; a body cannot name a worker.
	if rec := f.post(invPath, f.fx["inventory_request"], false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer = %d", rec.Code)
	}
	if st, _, _ := f.row(); st != "awaiting" {
		t.Fatalf("status = %q", st)
	}

	rec = f.post(invPath, f.fx["inventory_request"], true)
	if rec.Code != http.StatusOK || !jsonEqual(t, rec.Body.Bytes(), []byte(`{"status":"accepted"}`)) {
		t.Fatalf("inventory = %d %s", rec.Code, rec.Body)
	}
	if st, reason, entriesNull := f.row(); st != "accepted" || reason != nil || entriesNull {
		t.Fatalf("row = %q %v entriesNull=%v", st, reason, entriesNull)
	}

	rec = f.heartbeat(string(f.fx["heartbeat_request_member"]))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat with exclusion = %d %s", rec.Code, rec.Body)
	}
	resp = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	var wantAccepted map[string]json.RawMessage
	_ = json.Unmarshal(wantMembers["accepted"], &wantAccepted)
	if !jsonEqual(t, resp["hibernation_inventory"], wantAccepted["hibernation_inventory"]) {
		t.Fatalf("accepted member:\n got %s\nwant %s", resp["hibernation_inventory"], wantAccepted["hibernation_inventory"])
	}
	if _, reason, _ := f.row(); reason != nil {
		t.Fatalf("held=true invalidated: %v", *reason)
	}

	// A second inventory finds a settled row.
	if rec := f.post(invPath, f.fx["inventory_request"], true); rec.Code != http.StatusConflict {
		t.Fatalf("replayed inventory = %d", rec.Code)
	}

	rec = f.post(finalPath, f.fx["exclusion_final_request"], true)
	if rec.Code != http.StatusOK {
		t.Fatalf("exclusion-final = %d %s", rec.Code, rec.Body)
	}
	if _, reason, _ := f.row(); reason != nil {
		t.Fatalf("a matching final report invalidated: %v", *reason)
	}

	// A digest that differs from the accepted inventory is written-after-inventory.
	rec = f.post(finalPath, f.mutated("exclusion_final_request", map[string]any{"digest": strings.Repeat("c", 64)}), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("mismatching final = %d %s", rec.Code, rec.Body)
	}
	if _, reason, _ := f.row(); reason == nil || *reason != workersvc.InvalidatedWrittenAfterInventory {
		t.Fatalf("reason = %v", reason)
	}
}

func TestWorkerHibernationInventoryStatusBodiesLiveDB(t *testing.T) {
	var reqs []json.RawMessage
	f0 := newInvRouteFx(t)
	if err := json.Unmarshal(f0.fx["inventory_status_requests"], &reqs); err != nil || len(reqs) != 3 {
		t.Fatalf("fixture: %v", err)
	}
	for i, body := range reqs {
		f := newInvRouteFx(t)
		f.observe()
		var req struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &req)
		rec := f.post(invPath, body, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status body %d = %d %s", i, rec.Code, rec.Body)
		}
		if st, _, _ := f.row(); st != req.Status {
			t.Fatalf("status body %d: row status = %q, want %q", i, st, req.Status)
		}
		// Settled: a second status is a conflict.
		if rec := f.post(invPath, body, true); rec.Code != http.StatusConflict {
			t.Fatalf("repeated status = %d", rec.Code)
		}
	}
}

func TestWorkerHibernationInventoryMismatchIs409LiveDB(t *testing.T) {
	cases := map[string]map[string]any{
		"wrong pod":   {"pod_uid": uuid.NewString()},
		"wrong nonce": {"register_nonce": "stale"},
		"wrong op":    {"op_id": uuid.NewString()},
		"wrong rv":    {"deployment_resource_version": "999"},
		"wrong pvc uid": {"pvcs": []map[string]string{
			{"role": "data", "name": "uzi-hw-data", "uid": "88888888-8888-8888-8888-888888888890"},
			{"role": "nix", "name": "uzi-hw-nix", "uid": "88888888-8888-8888-8888-888888888882"},
			{"role": "dind_data", "name": "uzi-hw-dind", "uid": "88888888-8888-8888-8888-888888888883"}}},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInvRouteFx(t)
			f.observe()
			if rec := f.post(invPath, f.mutated("inventory_request", set), true); rec.Code != http.StatusConflict {
				t.Fatalf("inventory = %d %s", rec.Code, rec.Body)
			}
			if st, _, entriesNull := f.row(); st != "awaiting" || !entriesNull {
				t.Fatalf("a refused inventory persisted: %q entriesNull=%v", st, entriesNull)
			}
		})
	}
	t.Run("no observation yet", func(t *testing.T) {
		f := newInvRouteFx(t)
		if rec := f.post(invPath, f.fx["inventory_request"], true); rec.Code != http.StatusConflict {
			t.Fatalf("inventory = %d", rec.Code)
		}
	})
	t.Run("exclusion-final with the wrong nonce", func(t *testing.T) {
		f := newInvRouteFx(t)
		f.observe()
		if rec := f.post(invPath, f.fx["inventory_request"], true); rec.Code != http.StatusOK {
			t.Fatalf("inventory = %d", rec.Code)
		}
		if rec := f.post(finalPath, f.mutated("exclusion_final_request", map[string]any{"register_nonce": "stale"}), true); rec.Code != http.StatusConflict {
			t.Fatalf("exclusion-final = %d", rec.Code)
		}
	})
	t.Run("exclusion-final before an inventory", func(t *testing.T) {
		f := newInvRouteFx(t)
		f.observe()
		if rec := f.post(finalPath, f.fx["exclusion_final_request"], true); rec.Code != http.StatusConflict {
			t.Fatalf("exclusion-final = %d", rec.Code)
		}
	})
}

func TestWorkerHibernationInventoryBadBodiesLiveDB(t *testing.T) {
	f := newInvRouteFx(t)
	f.observe()
	bad := map[string][]byte{
		"unknown field":    f.mutated("inventory_request", map[string]any{"extra": 1}),
		"short digest":     f.mutated("inventory_request", map[string]any{"digest": "abc"}),
		"op not a uuid":    f.mutated("inventory_request", map[string]any{"op_id": "x"}),
		"no entries":       f.mutated("inventory_request", map[string]any{"entries": nil}),
		"not json":         []byte(`{`),
		"two values":       append(append([]byte{}, f.fx["inventory_request"]...), []byte(`{}`)...),
		"inventory+reason": f.mutated("inventory_request", map[string]any{"reason": "x"}),
	}
	var statuses []map[string]any
	_ = json.Unmarshal(f.fx["inventory_status_requests"], &statuses)
	statuses[0]["status"] = "accepted"
	b, _ := json.Marshal(statuses[0])
	bad["worker may not report accepted"] = b
	for name, body := range bad {
		if rec := f.post(invPath, body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: inventory = %d %s", name, rec.Code, rec.Body)
		}
	}
	for name, body := range map[string][]byte{
		"unknown field": f.mutated("exclusion_final_request", map[string]any{"extra": 1}),
		"no held":       f.mutated("exclusion_final_request", map[string]any{"held": nil}),
		"short digest":  f.mutated("exclusion_final_request", map[string]any{"digest": "z"}),
	} {
		if rec := f.post(finalPath, body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: exclusion-final = %d %s", name, rec.Code, rec.Body)
		}
	}
	if st, _, _ := f.row(); st != "awaiting" {
		t.Fatalf("a bad body moved the row to %q", st)
	}
}

func TestWorkerHibernationInventoryOversizeIs413LiveDB(t *testing.T) {
	f := newInvRouteFx(t)
	f.observe()
	big := f.mutated("inventory_request", map[string]any{"fingerprint": strings.Repeat("a", maxHibernationInventoryBody)})
	if rec := f.post(invPath, big, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize inventory = %d", rec.Code)
	}
	if rec := f.post(finalPath, bytes.Repeat([]byte(" "), 9<<10), true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize exclusion-final = %d", rec.Code)
	}
	// 10,000 short entries fit the route and the stored bound.
	if rec := f.post(invPath, f.withEntries(10000, 10), true); rec.Code != http.StatusOK {
		t.Fatalf("10k short entries = %d %s", rec.Code, rec.Body)
	}
}

// withEntries is the fixture inventory with n entries whose paths are pad characters long.
func (f *invRouteFx) withEntries(n, pad int) []byte {
	var m map[string]any
	_ = json.Unmarshal(f.fx["inventory_request"], &m)
	entries := make([]any, n)
	for i := range entries {
		entries[i] = map[string]any{"kind": "k", "path": "/" + strings.Repeat("p", pad), "classification": "c"}
	}
	m["entries"] = entries
	body, _ := json.Marshal(m)
	return body
}

// The canonical entries JSON is under 1 MiB, but jsonb's printed form (spaces after ':' and ',') is
// larger, so the table's CHECK fires: that must reach the worker as a 400, not a 500, and persist nothing.
func TestWorkerHibernationInventoryCheckViolationIs400LiveDB(t *testing.T) {
	f := newInvRouteFx(t)
	f.observe()
	var body []byte
	for pad := 1; pad < 200; pad++ {
		candidate := f.withEntries(workersvc.InventoryMaxEntries, pad)
		var m struct {
			Entries json.RawMessage `json:"entries"`
		}
		_ = json.Unmarshal(candidate, &m)
		if n := len(m.Entries); n > 1_035_000 && n <= 1<<20 {
			body = candidate
			break
		}
	}
	if body == nil {
		t.Fatal("no entry size lands between the compact and the printed bound")
	}
	if rec := f.post(invPath, body, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d %s", rec.Code, rec.Body)
	}
	if st, _, entriesNull := f.row(); st != "awaiting" || !entriesNull {
		t.Fatalf("row = %q entriesNull=%v", st, entriesNull)
	}
}

func TestWorkerHeartbeatHibernationExclusionLiveDB(t *testing.T) {
	op := "55555555-5555-5555-5555-555555555555"
	t.Run("held=false invalidates through the locked path", func(t *testing.T) {
		f := newInvRouteFx(t)
		f.observe()
		if rec := f.post(invPath, f.fx["inventory_request"], true); rec.Code != http.StatusOK {
			t.Fatalf("inventory = %d", rec.Code)
		}
		rec := f.heartbeat(`{"hibernation_exclusion":{"op_id":"` + op + `","held":false}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
		}
		if _, reason, _ := f.row(); reason == nil || *reason != workersvc.InvalidatedExclusionLost {
			t.Fatalf("reason = %v", reason)
		}
	})
	t.Run("malformed member is a 400", func(t *testing.T) {
		f := newInvRouteFx(t)
		f.observe()
		for name, body := range map[string]string{
			"bad op":        `{"hibernation_exclusion":{"op_id":"x","held":true}}`,
			"no held":       `{"hibernation_exclusion":{"op_id":"` + op + `"}}`,
			"unknown field": `{"hibernation_exclusion":{"op_id":"` + op + `","held":true,"x":1}}`,
		} {
			if rec := f.heartbeat(body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d", name, rec.Code)
			}
		}
	})
	t.Run("no member without the capability", func(t *testing.T) {
		f := newInvRouteFx(t)
		f.observe()
		cliMustExec(t, f.pool, `UPDATE workers SET protocol_capabilities = '{}' WHERE id = $1`, f.id)
		rec := f.heartbeat(`{}`)
		var resp map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if _, ok := resp["hibernation_inventory"]; ok || rec.Code != http.StatusOK {
			t.Fatalf("heartbeat = %d, member present=%v", rec.Code, ok)
		}
	})
	t.Run("an awake worker's heartbeat is unchanged", func(t *testing.T) {
		f := newInvRouteFx(t)
		cliMustExec(t, f.pool, `UPDATE workers SET hibernation_state = 'none', hibernation_op_id = NULL WHERE id = $1`, f.id)
		rec := f.heartbeat(`{"version":"v"}`)
		var resp map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if _, ok := resp["hibernation_inventory"]; ok || rec.Code != http.StatusOK {
			t.Fatalf("heartbeat = %d, member present=%v", rec.Code, ok)
		}
	})
}

// workersvc cannot import hostedsvc, so the mirror's new constants are pinned here.
func TestHibernationObservationConstantsMatchWire(t *testing.T) {
	if workersvc.TransitionObserved != hostedsvc.TransitionObserved || workersvc.TransitionHeld != hostedsvc.TransitionHeld {
		t.Fatal("workersvc transition constants drifted from hostedsvc")
	}
}

// The controller's observed and held reports reach the service with every new field copied.
func TestControllerObservedAndHeldRoutesLiveDB(t *testing.T) {
	f := newHibRouteFx(t)
	id, pod := f.worker()
	f.advertise()
	f.set.on = true
	if rec := f.controller("/api/controller/workers/"+id.String()+"/hibernation", `{}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d", rec.Code)
	}
	cliMustExec(t, f.pool, `UPDATE workers SET hibernation_state='preparing', hibernation_op_id=$2, hibernation_requested_at=now(), hibernation_admission_pod_uid=registered_pod_uid WHERE id=$1`, id, uuid.New())
	_, op := f.state(id)
	deploy := uuid.NewString()
	observed, _ := json.Marshal(hostedsvc.HibernationTransition{OpID: *op, Transition: hostedsvc.TransitionObserved, DeploymentUID: deploy,
		ResourceVersion: "7", PodUID: pod.String(),
		PVCs:    []hostedsvc.HibernationPVC{{Role: hostedsvc.PVCRoleData, Name: "d", UID: "u-d", ClassReclaimPolicy: hostedsvc.ReclaimPolicyRetain}},
		PodUIDs: &[]string{pod.String()}, PVCUserPods: &[]string{}})
	path := "/api/controller/workers/" + id.String() + "/hibernation"
	if rec := f.controller(path, string(observed), true); rec.Code != http.StatusOK {
		t.Fatalf("observed = %d %s", rec.Code, rec.Body)
	}
	var status, reason *string
	var pods string
	if err := f.pool.QueryRow(context.Background(), `SELECT status, invalidated_reason, pvcs::text FROM worker_hibernation_inventories WHERE worker_id=$1`, id).Scan(&status, &reason, &pods); err != nil {
		t.Fatal(err)
	}
	if status == nil || *status != "no_capability" || reason != nil || !strings.Contains(pods, `"u-d"`) {
		t.Fatalf("row = %v %v %s", status, reason, pods)
	}
	// Stale or non-hibernated held is a 409; a malformed pod set a 400.
	held, _ := json.Marshal(hostedsvc.HibernationTransition{OpID: *op, Transition: hostedsvc.TransitionHeld, DeploymentUID: deploy})
	if rec := f.controller(path, string(held), true); rec.Code != http.StatusConflict {
		t.Fatalf("held while preparing = %d", rec.Code)
	}
	if rec := f.controller(path, `{"op_id":"`+*op+`","transition":"held","deployment_uid":"`+deploy+`","pod_uids":["nope"]}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad pod set = %d", rec.Code)
	}
}
