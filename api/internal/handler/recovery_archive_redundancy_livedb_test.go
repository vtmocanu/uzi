package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #2625: POST /api/worker/runs/{id}/archives/{captureID}/redundancy through the real router,
// service and Postgres guard, over real-git vectors shared with the agent's tests.

type redundancyRoute struct {
	h       *Handler
	t       *testing.T
	pool    *pgxpool.Pool
	router  http.Handler
	forge   *completionRouteForge
	run     uuid.UUID
	capture uuid.UUID
	token   string
	body    map[string]any
	roots   []string
}

func newRedundancyRoute(t *testing.T) *redundancyRoute {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	h.wsvc.SetBackground(func(func()) {})
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 2507, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker, hold, capture := uuid.New(), uuid.New(), uuid.New()
	token := "redundancy-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'redundancy',$3,'online',$4)", worker, owner, sum[:], []string{capability.RecoveryCompletedPublicationV1})
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, owner, repo, run, worker)

	raw, err := os.ReadFile("../../../fixtures/recovery-archive-redundancy/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		Cases []struct {
			Name             string   `json:"name"`
			Roots            []string `json:"roots"`
			CurrentSha       string   `json:"currentSha"`
			Tree             string   `json:"tree"`
			Digest           string   `json:"digest"`
			SourceSha        string   `json:"sourceSha"`
			AggregateObjects []struct {
				BodyBase64 string `json:"bodyBase64"`
			} `json:"aggregateObjects"`
			CurrentObject struct {
				BodyBase64 string `json:"bodyBase64"`
			} `json:"currentObject"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}
	c := vec.Cases[0]
	if c.Name != "small" {
		t.Fatalf("first vector is %q", c.Name)
	}
	cliMustExec(t, pool, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,prerequisite_shas,expires_at,coverage_digest)
 VALUES($1,$2,$3,$4,$5,'ident',$6,'k','available',true,4096,'sum',1,'{}',now()+interval '7 days',$7)`, capture, hold, run, owner, worker, c.SourceSha, c.Digest)
	cliMustExec(t, pool, `INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,4,'\x01020304')`, capture)

	f := &completionRouteForge{t: t, branch: "agent/issue-7", settleFakeForge: settleFakeForge{head: settleHead, verdict: map[string]forge.Ancestry{settlePushed: forge.AncestryAncestor}}}
	h.wsvc.SetForges(settleForgeBuilder{f: f})
	r := &redundancyRoute{h: h, t: t, pool: pool, router: router, forge: f, run: run, capture: capture, token: token, roots: c.Roots}
	// Complete the run so its hold carries the completion identity and the receipt releases it.
	state, err := os.ReadFile("../../../fixtures/completed-publication/state-ack.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(state, &wire); err != nil {
		t.Fatal(err)
	}
	rec := r.do("/api/worker/runs/"+run.String()+"/state", token, string(wire.Request))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "completed_publication_receipt") {
		t.Fatalf("completion: %d %s", rec.Code, rec.Body.String())
	}
	objects := make([]string, 0, len(c.AggregateObjects))
	for _, o := range c.AggregateObjects {
		objects = append(objects, o.BodyBase64)
	}
	r.body = map[string]any{
		"generation": 1, "coverage_digest": c.Digest, "roots": c.Roots, "current_sha": c.CurrentSha, "tree": c.Tree,
		"aggregate_objects": objects, "current_object": c.CurrentObject.BodyBase64,
	}
	return r
}

func (r *redundancyRoute) do(path, token, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func (r *redundancyRoute) claim(token string, body map[string]any) (int, apitypes.RecoveryArchiveRedundancyResponse) {
	r.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		r.t.Fatal(err)
	}
	rec := r.do("/api/worker/runs/"+r.run.String()+"/archives/"+r.capture.String()+"/redundancy", token, string(raw))
	var res apitypes.RecoveryArchiveRedundancyResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			r.t.Fatalf("response %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, res
}

func (r *redundancyRoute) chunks() int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), "SELECT count(*) FROM recovery_capture_chunks WHERE capture_id=$1", r.capture).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func TestRecoveryArchiveRedundancyRouteExpiresLiveDB(t *testing.T) {
	r := newRedundancyRoute(t)
	path := "/api/worker/runs/" + r.run.String() + "/archives/" + r.capture.String() + "/redundancy"
	if rec := r.do(path, "invalid", "{}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	if code, _ := r.claim(r.token, map[string]any{"generation": 1, "surprise": true}); code != http.StatusBadRequest {
		t.Fatalf("unknown field must be a 400: %d", code)
	}
	unsorted := map[string]any{}
	for k, v := range r.body {
		unsorted[k] = v
	}
	roots := append([]string(nil), r.roots...)
	roots[0], roots[1] = roots[1], roots[0]
	unsorted["roots"] = roots
	if code, _ := r.claim(r.token, unsorted); code != http.StatusBadRequest {
		t.Fatalf("unsorted roots must be a 400: %d", code)
	}
	// Pad a known field, so the body cap is the only thing that can refuse the oversized body: the
	// same claim below the cap decodes and is answered 200, one byte-for-byte over it is a 400.
	padded := func(n int) map[string]any {
		b := map[string]any{}
		for k, v := range r.body {
			b[k] = v
		}
		b["current_object"] = strings.Repeat("A", n)
		return b
	}
	if code, res := r.claim(r.token, padded(workersvc.RedundancyRequestMaxBytes/2)); code != http.StatusOK ||
		res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != apitypes.RecoveryRedundancyInventoryTooLarge {
		t.Fatalf("a large body under the cap must decode and be refused by the claim caps: %d %+v", code, res)
	}
	if code, _ := r.claim(r.token, padded(workersvc.RedundancyRequestMaxBytes+1)); code != http.StatusBadRequest {
		t.Fatalf("over the body cap: %d", code)
	}
	if n := r.chunks(); n != 1 || r.forge.compareCalls != 1 {
		t.Fatalf("malformed claims must change nothing: chunks=%d compare calls=%d", n, r.forge.compareCalls)
	}

	code, res := r.claim(r.token, r.body)
	if code != http.StatusOK || res.Outcome != apitypes.RecoveryRedundancyExpired || res.CaptureID != r.capture.String() {
		t.Fatalf("claim: %d %+v", code, res)
	}
	if n := r.chunks(); n != 0 {
		t.Fatalf("chunks after expiry: %d", n)
	}
	var state, reason string
	var proof []byte
	if err := r.pool.QueryRow(context.Background(), "SELECT state, reason, redundancy_proof FROM recovery_captures WHERE id=$1", r.capture).Scan(&state, &reason, &proof); err != nil ||
		state != "expired" || reason != "published_redundant" || !strings.Contains(string(proof), `"anchor_head": "`+settleHead+`"`) {
		t.Fatalf("capture: %s %s %s %v", state, reason, proof, err)
	}
	calls := r.forge.compareCalls
	if code, res := r.claim(r.token, r.body); code != http.StatusOK || res.Outcome != apitypes.RecoveryRedundancyExpired || r.forge.compareCalls != calls {
		t.Fatalf("replay: %d %+v compare calls %d->%d", code, res, calls, r.forge.compareCalls)
	}
}

func TestRecoveryArchiveRedundancyRouteRetainsLiveDB(t *testing.T) {
	t.Run("uncovered_root_then_cooldown", func(t *testing.T) {
		r := newRedundancyRoute(t)
		r.forge.verdict[r.roots[0]] = forge.AncestryNotAncestor
		code, res := r.claim(r.token, r.body)
		if code != http.StatusOK || res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != apitypes.RecoveryRedundancyUncoveredRoot || r.chunks() != 1 {
			t.Fatalf("%d %+v chunks=%d", code, res, r.chunks())
		}
		calls := r.forge.compareCalls
		r.forge.verdict[r.roots[0]] = forge.AncestryAncestor
		code, res = r.claim(r.token, r.body)
		if code != http.StatusOK || res.Reason != apitypes.RecoveryRedundancyCoolingDown || r.forge.compareCalls != calls || r.chunks() != 1 {
			t.Fatalf("cool-down: %d %+v compare calls %d->%d", code, res, calls, r.forge.compareCalls)
		}
	})
	t.Run("another_worker_cannot_claim", func(t *testing.T) {
		r := newRedundancyRoute(t)
		var owner uuid.UUID
		if err := r.pool.QueryRow(context.Background(), "SELECT user_id FROM runs WHERE id=$1", r.run).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		other := uuid.New()
		token := "redundancy-other-" + uuid.NewString()
		sum := sha256.Sum256([]byte(token))
		cliMustExec(t, r.pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'other',$3,'online',$4)", other, owner, sum[:], []string{capability.RecoveryCompletedPublicationV1})
		calls := r.forge.compareCalls
		code, res := r.claim(token, r.body)
		if code != http.StatusOK || res.Reason != apitypes.RecoveryRedundancyBindingMismatch || r.forge.compareCalls != calls || r.chunks() != 1 {
			t.Fatalf("%d %+v", code, res)
		}
	})
}

// The claim spends the owner's forge quota, so it rides the per-worker limiter on the real worker
// router: with a budget of 2 the third call is a 429 that never reaches the service.
func TestRecoveryArchiveRedundancyIsRateLimitedLiveDB(t *testing.T) {
	r := newRedundancyRoute(t)
	r.forge.headErr = errors.New("transient")
	r.router = r.h.WorkerRoutes(mw.NewLimiter(2, time.Hour, nil))
	for i, want := range []string{apitypes.RecoveryRedundancyAncestryUnknown, apitypes.RecoveryRedundancyCoolingDown} {
		if code, res := r.claim(r.token, r.body); code != http.StatusOK || res.Reason != want {
			t.Fatalf("call %d: %d %+v want %s", i+1, code, res, want)
		}
	}
	if code, _ := r.claim(r.token, r.body); code != http.StatusTooManyRequests {
		t.Fatalf("third claim = %d, want 429", code)
	}
	if r.chunks() != 1 {
		t.Fatal("a limited claim must not touch the archive")
	}
}
