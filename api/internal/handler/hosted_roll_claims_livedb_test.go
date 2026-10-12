package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This integration bridge uses actual HTTP router, worker/controller bearer middleware and
// PostgreSQL; controller child owns the simulated Kubernetes objects/clock.
func TestHostedRollRetainedClaimsLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := newSeededPlanFixture(ctx, t)
	q := store.New(f.pool)
	h := &Handler{pool: f.pool, q: q, box: f.box, wsvc: f.svc}
	h.cfg.WorkerHostingEnabled = true
	controllerKey := uuid.NewString()
	h.cfg.ControllerTokenSHA256 = jointoken.Hash(controllerKey)
	h.hsvc = hostedsvc.New(q, f.box, time.Now, time.Minute)
	router := controllerRoutes(h)
	type member struct {
		ID  string `json:"id"`
		UID string `json:"uid"`
	}
	members := []member{}
	keys := map[string]string{}
	nonces := map[string]string{}
	for i := 0; i < 4; i++ {
		id := uuid.New()
		if i == 0 {
			id = f.wkr.ID
		}
		key := uuid.NewString()
		hash := jointoken.Hash(key)
		if i == 0 {
			mustExecT(ctx, t, f.pool, `UPDATE workers SET token_hash=$2 WHERE id=$1`, id, hash)
		} else {
			mustExecT(ctx, t, f.pool, `INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,'fleet-old',$3,'online')`, id, f.owner, hash)
		}
		mustExecT(ctx, t, f.pool, `UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',draining_since=now(),draining_reason=NULL WHERE id=$1`, id)
		members = append(members, member{id.String(), uuid.NewString()})
		keys[id.String()] = key
	}
	call := func(base, path, key string, body any) ([]byte, int, error) {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return data, resp.StatusCode, err
	}
	var base string
	claimErrors := make(chan error, 1)
	mux := http.NewServeMux()
	mux.Handle("/api/", router)
	mux.HandleFunc("/fixture/claim-retained", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+controllerKey {
			w.WriteHeader(401)
			return
		}
		var input struct {
			Retained []string `json:"retained"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Retained) != 3 {
			w.WriteHeader(400)
			return
		}
		fail := func(err error) { claimErrors <- err; w.WriteHeader(500) }
		seen := map[string]bool{}
		for _, id := range input.Retained {
			if seen[id] || keys[id] == "" {
				fail(fmt.Errorf("invalid retained member"))
				return
			}
			seen[id] = true
			raw, status, err := call(base, "/api/worker/runs/claim", keys[id], map[string]any{"active_snapshot": workersvc.ActiveSnapshot{RegisterNonce: nonces[id], SnapshotEpoch: 2, Active: []workersvc.ActiveRunEntry{}}})
			if err != nil || status != 200 {
				fail(fmt.Errorf("retained claim status=%d transport=%v", status, err))
				return
			}
			var payload workersvc.ClaimPayload
			if json.Unmarshal(raw, &payload) != nil || payload.RunID == "" {
				fail(fmt.Errorf("claim returned no run"))
				return
			}
			runID, err := uuid.Parse(payload.RunID)
			if err != nil {
				fail(err)
				return
			}
			run, err := q.GetRunByID(ctx, runID)
			if err != nil || !run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes).String() != id || run.Status != "claimed" {
				fail(fmt.Errorf("SQL assignment mismatch"))
				return
			}
			wk, err := q.GetWorkerByID(ctx, uuid.MustParse(id))
			if err != nil || wk.DrainingSince.Valid || wk.DrainingReason.Valid {
				fail(fmt.Errorf("retained worker cordoned"))
				return
			}
		}
		claimErrors <- nil
		w.WriteHeader(204)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	for _, m := range members {
		raw, status, err := call(base, "/api/worker/register", keys[m.ID], map[string]any{"name": "uzi-pod:" + m.UID, "version": "0.1.0", "template": "base"})
		if err != nil || status != 200 {
			t.Fatalf("register status=%d transport=%v", status, err)
		}
		var reg struct {
			Nonce string `json:"register_nonce"`
		}
		if json.Unmarshal(raw, &reg) != nil || reg.Nonce == "" {
			t.Fatal("register nonce absent")
		}
		nonces[m.ID] = reg.Nonce
		wk, err := q.GetWorkerByID(ctx, uuid.MustParse(m.ID))
		if err != nil || wk.DrainingSince.Valid || wk.DrainingReason.Valid || !wk.RegisteredPodUid.Valid || uuid.UUID(wk.RegisteredPodUid.Bytes).String() != m.UID || wk.SnapshotRegisterNonce.String != reg.Nonce {
			t.Fatal("register failed to clear default drain/bind UID/nonce")
		}
		_, status, err = call(base, "/api/worker/heartbeat", keys[m.ID], map[string]any{"version": "0.1.0", "active_snapshot": workersvc.ActiveSnapshot{RegisterNonce: reg.Nonce, SnapshotEpoch: 1, Active: []workersvc.ActiveRunEntry{}}})
		if err != nil || status != 200 {
			t.Fatalf("heartbeat status=%d transport=%v", status, err)
		}
		wk, err = q.GetWorkerByID(ctx, wk.ID)
		if err != nil || !wk.RollHeartbeatAt.Valid {
			t.Fatal("HTTP current-nonce heartbeat not persisted")
		}
	}
	for i := int64(0); i < 3; i++ {
		iid := f.iid + i + 1
		mustExecT(ctx, t, f.pool, `INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,has_prd_link,forge_updated_at,synced_at) VALUES($1,$2,'fleet issue','opened','["PRD","uzi"]'::jsonb,'https://example.com',true,now(),now())`, f.repoID, iid)
		if _, err := q.CreateRun(ctx, store.CreateRunParams{Harness: "claude", UserID: f.owner, RepoID: f.repoID, IssueIid: pgconv.Int8Ptr(&iid), IssueTitle: "fleet", IssueDescription: "fleet", PlanSource: "agent", TriggerSource: "manual"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(members)
	controllerDir, err := filepath.Abs("../../../controller")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "go", "test", "-buildvcs=false", "-race", "-count=1", "-v", "./internal/kube", "-run", "^TestHostedRollRetainedClaimsBridge$")
	cmd.Dir = controllerDir
	cmd.Env = append(os.Environ(), "UZI_ROLL_TEST_API="+base, "UZI_ROLL_TEST_CONTROLLER_KEY="+controllerKey, "UZI_ROLL_TEST_MEMBERS="+string(raw))
	out, err := cmd.CombinedOutput()
	t.Logf("controller child:\n%s", out)
	if err != nil {
		select {
		case claimErr := <-claimErrors:
			t.Logf("claim callback: %v", claimErr)
		default:
		}
		t.Fatalf("controller child exit: %v", err)
	}
	if !strings.Contains(string(out), "--- PASS: TestHostedRollRetainedClaimsBridge") || strings.Contains(string(out), "--- SKIP:") {
		t.Fatal("child positive control missing")
	}
	select {
	case err := <-claimErrors:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("real claim callback never ran")
	}
	t.Log("PG proof: N=4, N-1=3 retained old workers each claimed a real queued compatible owner run after controller deadline; SQL assignments verified; post-claim fresh HTTP Busy=3, no retained API cordon")
}
