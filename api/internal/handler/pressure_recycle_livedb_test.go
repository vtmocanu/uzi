package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Reuse the real compatible-run fixture from hosted_roll_claims_livedb_test.go.
func TestPressureRecycleRouteClaimsLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newSeededPlanFixture(ctx, t)
	q := store.New(f.pool)
	h := &Handler{pool: f.pool, q: q, box: f.box, wsvc: f.svc}
	h.cfg.WorkerHostingEnabled = true
	key := uuid.NewString()
	workerKey := uuid.NewString()
	h.cfg.ControllerTokenSHA256 = jointoken.Hash(key)
	h.hsvc = hostedsvc.New(q, f.box, time.Now, time.Minute)
	mustExecT(ctx, t, f.pool, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',token_hash=$2 WHERE id=$1", f.wkr.ID, jointoken.Hash(workerKey))
	server := httptest.NewServer(controllerRoutes(h))
	defer server.Close()
	call := func(method, path, token, body string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		return resp.StatusCode, data, err
	}
	drainPath := "/api/controller/workers/" + f.wkr.ID.String() + "/drain"
	assertCall := func(method, path, token, body string, want int) []byte {
		t.Helper()
		status, data, err := call(method, path, token, body)
		if err != nil || status != want {
			t.Fatalf("%s status=%d want=%d err=%v body=%s", path, status, want, err, data)
		}
		return data
	}
	raw := assertCall("POST", "/api/worker/register", workerKey, `{"name":"pressure-worker","version":"0.1.0","template":"base"}`, 200)
	var reg struct {
		Nonce string `json:"register_nonce"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil || reg.Nonce == "" {
		t.Fatal("registration missing")
	}
	assertCall("POST", "/api/worker/heartbeat", workerKey, fmt.Sprintf(`{"version":"0.1.0","active_snapshot":{"register_nonce":%q,"snapshot_epoch":1,"active":[]}}`, reg.Nonce), 200)
	run, err := q.CreateRun(ctx, store.CreateRunParams{Harness: "claude", UserID: f.owner, RepoID: f.repoID, IssueIid: pgconv.Int8Ptr(&f.iid), IssueTitle: "pressure", IssueDescription: "pressure", PlanSource: "agent", TriggerSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkerID.Valid {
		t.Fatal("new run already assigned")
	}
	assertCall("POST", drainPath, key, `{"reason":"pressure_recycle"}`, 204)
	before, err := q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertCall("POST", drainPath, key, `{"reason":"pressure_recycle"}`, 204)
	after, err := q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil || !after.DrainingSince.Time.Equal(before.DrainingSince.Time) || after.DrainingReason.String != "pressure_recycle" {
		t.Fatal("repeat reset pressure")
	}
	claimBody := func(epoch int) string {
		return fmt.Sprintf(`{"active_snapshot":{"register_nonce":%q,"snapshot_epoch":%d,"active":[]}}`, reg.Nonce, epoch)
	}
	assertCall("POST", "/api/worker/runs/claim", workerKey, claimBody(2), 204)
	poll := func() { t.Helper(); assertCall("GET", "/api/controller/poll", key, "", 200) }
	// A fence acquired after a clean poll must still retain ownership.
	poll()
	mustExecT(ctx, t, f.pool, "UPDATE workers SET stats_disk_pressure_streak=2 WHERE id=$1", f.wkr.ID)
	assertCall("DELETE", drainPath, key, `{"reason":"pressure_recycle"}`, 204)
	after, err = q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil || !after.DrainingSince.Valid {
		t.Fatal("persistent pressure released")
	}
	mustExecT(ctx, t, f.pool, "UPDATE workers SET stats_disk_pressure_streak=0 WHERE id=$1", f.wkr.ID)
	poll()
	mustExecT(ctx, t, f.pool, "UPDATE workers SET maintenance_fenced=true WHERE id=$1", f.wkr.ID)
	assertCall("DELETE", drainPath, key, `{"reason":"pressure_recycle"}`, 204)
	after, err = q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil || !after.DrainingSince.Valid {
		t.Fatal("post-poll maintenance released")
	}
	mustExecT(ctx, t, f.pool, "UPDATE workers SET maintenance_fenced=false WHERE id=$1", f.wkr.ID)
	// Session A follows ClaimRun's worker-first custody order; B must visibly wait.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qt := q.WithTx(tx)
	if _, err := qt.GetWorkerForUpdate(ctx, f.wkr.ID); err != nil {
		t.Fatal(err)
	}
	holdRun := uuid.New()
	if _, err := tx.Exec(ctx, "INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status) VALUES($1,$2,'chat','t','d','running')", holdRun, f.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state) VALUES($1,$2,$3,1,$4,$5,$4,$3,true,'open')", uuid.New(), f.owner, holdRun, f.wkr.ID, f.wkr.ID.String()); err != nil {
		t.Fatal(err)
	}
	pid := tx.Conn().PgConn().PID()
	done := make(chan error, 1)
	go func() {
		status, _, err := call("DELETE", drainPath, key, `{"reason":"pressure_recycle"}`)
		if err == nil && status != 204 {
			err = fmt.Errorf("DELETE status %d", status)
		}
		done <- err
	}()
	// Bound observation to five seconds and poll actual PostgreSQL blockers.
	// Match the leading sqlc name: pg_stat_activity may truncate the long projection.
	deadline := time.Now().Add(5 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := f.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::bigint=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' AND query LIKE '%GetWorkerForUpdate%')", int64(pid)).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("DELETE did not wait: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !waiting {
		t.Fatal("no observed worker-lock wait")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after, err = q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil || after.DrainingReason.String != "pressure_recycle" || !after.DrainingSince.Valid {
		t.Fatal("committed custody missed after lock wait")
	}
	t.Log("observed DELETE blocked on A's worker lock; committed custody retained pressure ownership")
	// Terminal custody settlement removes the independent fence.
	mustExecT(ctx, t, f.pool, "UPDATE runs SET status='failed',claim_generation=1 WHERE id=$1", holdRun)
	mustExecT(ctx, t, f.pool, "UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,release_evidence='forge_no_output',final_disposition='settled',final_coverage_digest='e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',released_at=now() WHERE run_id=$1", holdRun)
	assertCall("DELETE", drainPath, key, `{"reason":"pressure_recycle"}`, 204)
	after, err = q.GetWorkerByID(ctx, f.wkr.ID)
	if err != nil || after.DrainingSince.Valid || after.DrainingReason.Valid {
		t.Fatal("release did not clear")
	}
	raw = assertCall("POST", "/api/worker/runs/claim", workerKey, claimBody(3), 200)
	var payload workersvc.ClaimPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.RunID != run.ID.String() {
		t.Fatal("new compatible run not claimed after release")
	}
	t.Log("new unassigned compatible run denied while draining and claimed after pressure release")
}
