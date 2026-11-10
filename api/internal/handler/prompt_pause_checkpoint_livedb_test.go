package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4116: an owner pause of a running prompt run was accepted, but the worker's checkpoint
// publish was answered Skipped:"unsupported" because the api derived no checkpoint key for the
// prompt kind. The old API returned unsupported for prompt publication, so the worker reported
// pause_failed instead of parking on a checkpoint. These tests drive the whole
// authenticated path with the real routers and Postgres: the owner pause (POST
// /api/runs/{id}/inputs with a uzc_ Bearer, the route the CLI uses), the run's real claim, the
// worker's publish and paused ACK, the owner resume and the next real claim. Only the forge push
// (the workersvc publish seam) is stubbed, and the stub records the options it was handed.
// Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

const ppcTip = "0123456789abcdef0123456789abcdef01234567"

type ppcPush struct {
	WorkBranch, CheckpointKey  string
	DeclaredTip, DefaultBranch string
}

type ppcEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	router http.Handler
	user   uuid.UUID
	repo   uuid.UUID
	owner  string // uzc_ Bearer for the owner routes
	wA, wB uuid.UUID
	tokA   string
	tokB   string

	mu     sync.Mutex
	pushes []ppcPush
}

func (e *ppcEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *ppcEnv) recorded() []ppcPush {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ppcPush(nil), e.pushes...)
}

// newPPCEnv seeds one fresh owner (so no run of another subtest is claimable), a forge connection
// whose bot token the publish route can open, a repo, an Anthropic credential the claim needs, and
// two workers advertising task_checkpoint_coordinates_v1 (A runs the run, B is the foreign bearer).
func newPPCEnv(t *testing.T) *ppcEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	e := &ppcEnv{t: t, ctx: ctx, pool: pool, user: uuid.New(), repo: uuid.New(), wA: uuid.New(), wB: uuid.New()}
	// Deleting the owner cascades to its runs, inputs, workers, connection and repo, so no fixture
	// stays visible to a later retention or salvage sweep.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", e.user); err != nil {
			t.Errorf("delete fixture owner: %v", err)
		}
	})

	box := newHandlerTestBox(t)
	conn := uuid.New()
	sealed, err := box.Seal([]byte("prompt-pause-bot-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO users (id,email,password_hash,vault_mode) VALUES ($1,$2,'x','unprotected')`, e.user, fmt.Sprintf("ppc-%s@e2e", e.user))
	e.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, e.user, sealed)
	e.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,7,$3,$4,'main',true)`, e.repo, conn, "g/ppc-"+e.repo.String(), "https://forge.e2e/g/ppc-"+e.repo.String())
	model, err := box.Seal([]byte("prompt-pause-model-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with)
 VALUES($1,$2,'anthropic_token',$3,true,$4,'master')`, uuid.New(), e.user, "ppc-"+uuid.NewString(), model)
	caps := []string{capability.TaskCheckpointCoordinatesV1, capability.ParkRunV1}
	for _, w := range []struct {
		id  uuid.UUID
		tok *string
	}{{e.wA, &e.tokA}, {e.wB, &e.tokB}} {
		tok, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		*w.tok = tok
		e.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities)
 VALUES ($1,$2,$3,$4,'online',now(),$5)`, w.id, e.user, "ppc-"+w.id.String(), hash, caps)
	}
	e.owner = cliMintToken(t, pool, e.user, "user")

	q := store.New(pool)
	svc := workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), WorkerHeartbeatStale: time.Minute,
		WorkerHeartbeatInterval: 10 * time.Second, WorkerAffinityCeiling: 2 * time.Hour, RunTimeout: time.Hour,
	})
	svc.SetVault(vault.NewPostgres(box, pool))
	svc.SetTxBeginner(pool)
	svc.SetCapabilitySettings(taskCheckpointSchedulingOff{})
	svc.SetForges(settleForgeBuilder{f: &settleFakeForge{head: settleHead}})
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		e.mu.Lock()
		e.pushes = append(e.pushes, ppcPush{WorkBranch: o.WorkBranch, CheckpointKey: o.CheckpointKey,
			DeclaredTip: o.DeclaredTip, DefaultBranch: o.DefaultBranch})
		e.mu.Unlock()
		return pushbroker.Result{Ref: "refs/uzi-checkpoints/" + o.CheckpointKey, Disposition: pushbroker.PublishAdvanced}, nil
	})
	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, settings: settings.New(q, time.Minute),
		cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour, RunTimeout: time.Hour, WorkerHeartbeatStale: time.Minute}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	return e
}

// seedQueued inserts a queued, claimable run of kind and returns its id, the checkpoint key and
// work branch the api must publish to.
func (e *ppcEnv) seedQueued(kind string) (id uuid.UUID, key, work string) {
	e.t.Helper()
	id = uuid.New()
	switch kind {
	case "issue":
		iid := int64(id[0]) + int64(id[1])*256 + int64(id[2])*65536 + 1
		e.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,auto_approve,plan_md,dispatched_at,harness)
 VALUES ($1,$2,$3,'issue',$4,'t','d','queued',true,'approved plan',now(),'claude')`, id, e.user, e.repo, iid)
		key = fmt.Sprintf("agent/issue-%d", iid)
		work = key
	case "self_improve":
		iid := int64(id[0]) + int64(id[1])*256 + int64(id[2])*65536 + 1
		e.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,auto_approve,plan_md,dispatched_at,harness)
 VALUES ($1,$2,$3,'self_improve',$4,'t','d','queued',true,'approved plan',now(),'claude')`, id, e.user, e.repo, iid)
		key = "uzi/self-improve/" + id.String()
		work = key
	case "prompt":
		e.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_title,issue_description,status,auto_approve,plan_md,dispatched_at,harness)
 VALUES ($1,$2,$3,'prompt','t','d','queued',true,'approved plan',now(),'claude')`, id, e.user, e.repo)
		key = "uzi/prompt-" + id.String()
		work = key
	case "task":
		key = "uzi/task/" + id.String()
		work = key
		e.exec(`INSERT INTO runs (id,user_id,repo_id,kind,branch,dispatched_at,issue_title,issue_description,status,harness)
 VALUES ($1,$2,$3,'task',$4,now(),'t','d','queued','claude')`, id, e.user, e.repo, key)
	default:
		e.t.Fatalf("unknown kind %q", kind)
	}
	return id, key, work
}

func (e *ppcEnv) call(method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" && strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// claim performs the worker's real claim and returns its payload; want is the run it must deliver.
func (e *ppcEnv) claim(token string, want uuid.UUID) workersvc.ClaimPayload {
	e.t.Helper()
	rec := e.call(http.MethodPost, "/api/worker/runs/claim", token, "", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("claim = %d %s", rec.Code, rec.Body.String())
	}
	var p workersvc.ClaimPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		e.t.Fatal(err)
	}
	if p.RunID != want.String() {
		e.t.Fatalf("claim delivered run %s, want %s", p.RunID, want)
	}
	return p
}

func (e *ppcEnv) state(token string, id uuid.UUID, status string, gen int64) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodPost, "/api/worker/runs/"+id.String()+"/state", token,
		fmt.Sprintf(`{"status":%q,"claim_generation":%d}`, status, gen), nil)
}

func (e *ppcEnv) publish(token string, id uuid.UUID, gen int64) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/publish?claim_generation=%d", id, gen), token, "pack",
		map[string]string{"X-Uzi-Checkpoint-Tip": ppcTip, "Content-Type": "application/octet-stream"})
}

func (e *ppcEnv) ownerInput(id uuid.UUID, kind, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	payload, err := json.Marshal(map[string]string{"kind": kind, "body": body})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.call(http.MethodPost, "/api/runs/"+id.String()+"/inputs", e.owner, string(payload), nil)
}

func (e *ppcEnv) run(id uuid.UUID) store.Run {
	e.t.Helper()
	r, err := store.New(e.pool).GetRunByID(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// startRunning claims the queued run through the real claim path and reports it running.
func (e *ppcEnv) startRunning(id uuid.UUID) int64 {
	e.t.Helper()
	p := e.claim(e.tokA, id)
	if rec := e.state(e.tokA, id, "running", p.ClaimGeneration); rec.Code != http.StatusOK {
		e.t.Fatalf("running state = %d %s", rec.Code, rec.Body.String())
	}
	if got := e.run(id).Status; got != "running" {
		e.t.Fatalf("status = %s, want running", got)
	}
	return p.ClaimGeneration
}

// TestPromptPauseCheckpointRoutesLiveDB: an owner pause of a running run publishes its checkpoint
// to the kind's key and parks with that tip, and the next claim carries the tip. Issue,
// self_improve and the non-interactive task are the controls that already worked; prompt is the
// subject that was answered "unsupported" before issue #4116.
func TestPromptPauseCheckpointRoutesLiveDB(t *testing.T) {
	for _, kind := range []string{"prompt", "issue", "self_improve", "task"} {
		for _, mode := range []string{"now", "milestone"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				e := newPPCEnv(t)
				id, key, work := e.seedQueued(kind)
				gen := e.startRunning(id)
				if kind == "task" {
					if got := e.run(id).CheckpointCoordinates; len(got) == 0 {
						t.Fatal("task claim did not enroll checkpoint coordinates")
					}
				}

				// (1) The owner pause is accepted and leaves the run running with a pending request.
				rec := e.ownerInput(id, "pause", mode)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("owner pause = %d %s", rec.Code, rec.Body.String())
				}
				r := e.run(id)
				if r.Status != "running" || !r.PauseRequestedAt.Valid || r.PauseMode.String != mode {
					t.Fatalf("pending pause: status=%s requested=%v mode=%q", r.Status, r.PauseRequestedAt, r.PauseMode.String)
				}

				// (2) The worker publishes its checkpoint: published, to the kind's key.
				rec = e.publish(e.tokA, id, gen)
				var pub struct {
					Published bool   `json:"published"`
					Ref       string `json:"ref"`
					Skipped   string `json:"skipped"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
					t.Fatalf("publish body %q: %v", rec.Body.String(), err)
				}
				if rec.Code != http.StatusOK || !pub.Published || pub.Skipped != "" || pub.Ref != "refs/uzi-checkpoints/"+key {
					t.Fatalf("publish = %d %s; want published ref refs/uzi-checkpoints/%s", rec.Code, rec.Body.String(), key)
				}
				pushes := e.recorded()
				if len(pushes) != 1 || pushes[0].CheckpointKey != key || pushes[0].WorkBranch != work ||
					pushes[0].DeclaredTip != ppcTip || pushes[0].DefaultBranch != "main" {
					t.Fatalf("push options = %+v; want one push key=%s work=%s tip=%s", pushes, key, work, ppcTip)
				}

				// (3) The worker's paused ACK parks the run on that tip and clears the request.
				if rec = e.state(e.tokA, id, "paused", gen); rec.Code != http.StatusOK {
					t.Fatalf("paused ACK = %d %s", rec.Code, rec.Body.String())
				}
				r = e.run(id)
				if r.Status != "paused" || r.PauseRequestedAt.Valid || r.PauseMode.Valid ||
					!r.CheckpointTip.Valid || r.CheckpointTip.String != ppcTip {
					t.Fatalf("parked run: status=%s pending=%v mode=%v tip=%v; want paused, no request, tip %s",
						r.Status, r.PauseRequestedAt, r.PauseMode, r.CheckpointTip, ppcTip)
				}

				// (4) Owner resume, then the next real claim carries the checkpoint tip.
				if rec = e.call(http.MethodPost, "/api/runs/"+id.String()+"/resume-now", e.owner, "", nil); rec.Code != http.StatusOK {
					t.Fatalf("owner resume = %d %s", rec.Code, rec.Body.String())
				}
				next := e.claim(e.tokA, id)
				if next.ClaimGeneration != gen+1 || next.CheckpointTip == nil || *next.CheckpointTip != ppcTip {
					t.Fatalf("resume claim: generation=%d tip=%v; want generation %d tip %s", next.ClaimGeneration, next.CheckpointTip, gen+1, ppcTip)
				}

				// (5) The superseded (older) generation can no longer publish once the run was reclaimed.
				if rec = e.publish(e.tokA, id, gen); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"stale_claim"`) {
					t.Fatalf("superseded generation publish = %d %s; want 409 stale_claim", rec.Code, rec.Body.String())
				}
				if got := e.recorded(); len(got) != 1 {
					t.Fatalf("a superseded publish reached the forge push: %+v", got)
				}
			})
		}
	}
}

// TestPromptPauseCheckpointGuardsLiveDB: the publish route's existing fences hold for a prompt run
// (a stale claim generation and another worker's bearer are refused before any forge push), and an
// interactive task is still refused a pause with 409.
func TestPromptPauseCheckpointGuardsLiveDB(t *testing.T) {
	t.Run("stale generation and foreign worker are refused", func(t *testing.T) {
		e := newPPCEnv(t)
		id, key, _ := e.seedQueued("prompt")
		gen := e.startRunning(id)
		if rec := e.ownerInput(id, "pause", "now"); rec.Code >= 300 {
			t.Fatalf("owner pause = %d %s", rec.Code, rec.Body.String())
		}

		rec := e.publish(e.tokA, id, gen+1)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"stale_claim"`) {
			t.Fatalf("stale generation publish = %d %s; want 409 stale_claim", rec.Code, rec.Body.String())
		}
		if rec = e.publish(e.tokB, id, gen); rec.Code != http.StatusNotFound {
			t.Fatalf("foreign worker publish = %d %s; want 404", rec.Code, rec.Body.String())
		}
		if rec = e.publish("", id, gen); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated publish = %d %s; want 401", rec.Code, rec.Body.String())
		}
		if got := e.recorded(); len(got) != 0 {
			t.Fatalf("a refused publish reached the forge push: %+v", got)
		}
		if r := e.run(id); r.CheckpointTip.Valid {
			t.Fatalf("a refused publish persisted tip %v", r.CheckpointTip)
		}

		// The same request, correctly authenticated and stamped, is the only one that publishes.
		rec = e.publish(e.tokA, id, gen)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"published":true`) {
			t.Fatalf("control publish = %d %s", rec.Code, rec.Body.String())
		}
		if got := e.recorded(); len(got) != 1 || got[0].CheckpointKey != key {
			t.Fatalf("control push = %+v; want one push to %s", got, key)
		}
	})

	t.Run("interactive task pause is 409", func(t *testing.T) {
		e := newPPCEnv(t)
		id, _, _ := e.seedQueued("task")
		gen := e.startRunning(id)
		// interactive is fixed at create on a real run; flipping it after the claim is enough here
		// because the pause refusal reads only the run's kind and interactive flag.
		e.exec("UPDATE runs SET interactive=true WHERE id=$1", id)
		rec := e.ownerInput(id, "pause", "now")
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "interactive tasks park after each turn") {
			t.Fatalf("interactive task pause = %d %s; want 409 naming the interactive refusal", rec.Code, rec.Body.String())
		}
		if r := e.run(id); r.Status != "running" || r.PauseRequestedAt.Valid || r.ClaimGeneration != gen {
			t.Fatalf("refused pause changed the run: status=%s pending=%v generation=%d", r.Status, r.PauseRequestedAt, r.ClaimGeneration)
		}
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause'`, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("refused pause wrote %d audit rows (err=%v)", n, err)
		}
	})
}
