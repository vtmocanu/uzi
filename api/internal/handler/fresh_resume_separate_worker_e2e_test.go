package handler

// Issue #4628 real agent-to-API proof of a fresh resume on a separate hosted worker.
//
// The Go side owns the API (the real h.Routes() router, production wiring, LiveDB) and every assertion
// about server rows; the Node side (agent/test/fixtures/fresh-resume-e2e.ts) is the real WorkerClient and,
// for the destination, the real RunRunner. Three jointoken workers take part: W1 (the predecessor, which
// reaches its owner hold through the real claim, checkpoint publish, archive upload and recovery_wait park
// routes), W2 (the hosted destination, UZI_POD_UID set in its child only) and W3 (a later claimant). Only the
// forge is synthetic: a file:// bare origin behind a forge fake that answers branch, ref and ancestry
// questions from that origin.
//
// Seeded rows: none of the evidence, holds, checkpoints or parks is inserted. The only SQL writes are the
// owner/connection/repo/worker/run fixtures, and the evidence mutations the refusal subtests apply on top of
// real evidence (an expiry, a cleared checkpoint stamp, a moved claim generation under the guard's opt-in).
//
// Opt-in like the other composed proofs (it needs Node and the installed agent dependencies); once opted in
// it never skips. The per-subtest names are pinned by e2e/run-store-it.sh.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// freshE2EProject keeps every world's forge project id distinct across the shared LiveDB.
var freshE2EProject atomic.Int64

type freshE2EW1 struct {
	Generation    int64  `json:"generation"`
	M1            string `json:"m1"`
	M2            string `json:"m2"`
	Source        string `json:"source"`
	MainTip       string `json:"mainTip"`
	Tip           string `json:"tip"`
	CaptureID     string `json:"captureId"`
	WorkBranch    string `json:"workBranch"`
	CheckpointKey string `json:"checkpointKey"`
	Parked        struct {
		Applied bool    `json:"applied"`
		Status  string  `json:"status"`
		Cause   *string `json:"cause"`
	} `json:"parked"`
}

type freshE2EJournal struct {
	Frid       string `json:"frid"`
	AttemptID  string `json:"attemptId"`
	Generation int64  `json:"generation"`
	Sha        string `json:"sha"`
	Stage      string `json:"stage"`
}

type freshE2EW2 struct {
	Outcome         string           `json:"outcome"`
	Error           *string          `json:"error"`
	MrCreates       int              `json:"mrCreates"`
	ContinuedHead   string           `json:"continuedHead"`
	FinalHead       string           `json:"finalHead"`
	Description     string           `json:"description"`
	ExecutorRuns    int              `json:"executorRuns"`
	ExecutionBase   string           `json:"executionBase"`
	DefaultTip      string           `json:"defaultTip"`
	StrictlyAhead   bool             `json:"strictlyAhead"`
	CommitsAhead    int              `json:"commitsAhead"`
	Files           []string         `json:"files"`
	AttemptPath     string           `json:"attemptPath"`
	Journal         *freshE2EJournal `json:"journal"`
	JournalAfter    *freshE2EJournal `json:"journalAfter"`
	ClaimGeneration int64            `json:"claimGeneration"`
	ClaimFresh      map[string]any   `json:"claimFresh"`
	WorkerID        string           `json:"workerId"`
}

// freshE2EProc is one Node worker generation.
type freshE2EProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	out     *bytes.Buffer
	token   string
	cfgJSON string
	done    chan error
}

func (p *freshE2EProc) printable() string {
	return strings.ReplaceAll(p.out.String(), p.token, "[redacted]")
}

// Wait blocks (bounded by the command's own context) for the process and returns its RESULT line.
func (p *freshE2EProc) Wait() (json.RawMessage, error) {
	err := <-p.done
	p.t.Logf("worker fixture output:\n%s", p.printable())
	if err != nil {
		return nil, fmt.Errorf("worker fixture exit: %w", err)
	}
	for _, line := range strings.Split(p.out.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "RESULT "); ok {
			return json.RawMessage(rest), nil
		}
	}
	return nil, errors.New("worker fixture printed no RESULT line")
}

// WaitKilled waits for a process that was hard-stopped and reports that it did end by a signal.
func (p *freshE2EProc) WaitKilled() {
	err := <-p.done
	p.t.Logf("worker fixture output (killed):\n%s", p.printable())
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		p.t.Fatalf("expected the worker to die by SIGKILL, got %v", err)
	}
}

// Kill is the hard stop of a worker generation (SIGKILL): no shutdown handler runs.
func (p *freshE2EProc) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

type freshE2ETraffic struct {
	seeded     atomic.Int64
	abandons   atomic.Int64
	bundles    atomic.Int64
	dropSeeded atomic.Bool
	// onSeededCommit runs after the real handler committed a seeded receipt and before the reply is dropped.
	onSeededCommit atomic.Pointer[func()]
}

type freshE2EWorld struct {
	t       *testing.T
	ctx     context.Context
	root    string
	dir     string
	h       *Handler
	router  http.Handler
	pool    *pgxpool.Pool
	srv     *httptest.Server
	traffic *freshE2ETraffic

	origin                    string
	owner, conn, repo, run    uuid.UUID
	iid                       int
	w1, w2, w3                uuid.UUID
	w1Token, w2Token, w3Token string
	podUID                    string
	ownerToken                string
	w1Root, w2Root            string
	w1Result                  freshE2EW1
	w1Digest                  string
	procs                     int
	mu                        sync.Mutex
}

func freshE2EGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed Git binary, owned fixture paths and literal subcommands; no shell.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// freshE2EPublish stands in only for the forge transport of the checkpoint push: packs, ancestry and
// the ref update are real Git operations on the file:// origin.
func freshE2EPublish(origin string) func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
	zero := strings.Repeat("0", 40)
	return func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		git := func(input []byte, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", origin}, args...)...) //nolint:gosec // G204: only the fixed Git subcommands below, server-validated refs and SHAs, owned paths; no shell.
			cmd.Stdin = bytes.NewReader(input)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("fixture Git %s failed: %w", args[0], err)
			}
			return strings.TrimSpace(string(out)), nil
		}
		ref := "refs/uzi-checkpoints/" + o.CheckpointKey
		if _, err := git(nil, "check-ref-format", ref); err != nil {
			return pushbroker.Result{}, err
		}
		if _, err := git(o.Pack, "index-pack", "--stdin"); err != nil {
			return pushbroker.Result{}, err
		}
		if _, err := git(nil, "cat-file", "-e", o.DeclaredTip+"^{commit}"); err != nil {
			return pushbroker.Result{}, pushbroker.ErrTipMissing
		}
		old, err := git(nil, "rev-parse", "--verify", ref)
		if err != nil {
			old = zero
		}
		if old == o.DeclaredTip {
			return pushbroker.Result{Ref: ref, AlreadyCurrent: true}, nil
		}
		base := old
		if base == zero {
			if base, err = git(nil, "rev-parse", "refs/heads/"+o.DefaultBranch); err != nil {
				return pushbroker.Result{}, err
			}
		}
		if _, err := git(nil, "merge-base", "--is-ancestor", base, o.DeclaredTip); err != nil {
			return pushbroker.Result{}, pushbroker.ErrNotDescendant
		}
		if _, err := git(nil, "update-ref", ref, o.DeclaredTip, old); err != nil {
			return pushbroker.Result{}, err
		}
		return pushbroker.Result{Ref: ref}, nil
	}
}

// newFreshE2EWorld builds one isolated owner: connection, repo, queued Claude run, three workers and a
// file:// origin, serves the real router, then runs W1 through the real routes to its owner hold.
func newFreshE2EWorld(t *testing.T, root, evidence string) *freshE2EWorld {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	w := &freshE2EWorld{t: t, ctx: context.Background(), root: root, h: h, router: router, pool: pool, traffic: &freshE2ETraffic{}}
	scratch := filepath.Join(root, ".uzi", "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(scratch, "fresh-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	w.dir = dir
	t.Cleanup(func() {
		if !t.Failed() {
			if err := os.RemoveAll(dir); err != nil {
				t.Errorf("cleanup fixture directory: %v", err)
			}
		}
	})

	// The origin: one default-branch commit.
	seed := filepath.Join(dir, "seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	freshE2EGit(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# fresh resume proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	freshE2EGit(t, seed, "add", "README.md")
	freshE2EGit(t, seed, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "initial")
	w.origin = filepath.Join(dir, "origin.git")
	freshE2EGit(t, dir, "clone", "-q", "--bare", seed, w.origin)

	// Production wiring with the gate on. Only the forge is a fake.
	svc := workersvc.New(h.q, h.box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), FreshResume: true, WorkerHeartbeatStale: 45 * time.Second, RunTimeout: 2 * time.Hour,
		ActiveSnapshotMaxEntries: 256, WorkerOutboxMaxPending: 32, TerminalPendingLease: time.Hour,
		// The production defaults of the worker-death episode (RUN_MAX_REQUEUES, RUN_RECOVERY_PARK_BASE, RUN_RETAINED_RECOVERY_MAX_PARKS).
		RunMaxRequeues: 3, RunRecoveryParkBase: time.Minute, RunRecoveryMaxPark: time.Hour, RunRetainedRecoveryMaxParks: 3,
	})
	svc.SetVault(h.vault)
	svc.SetTxBeginner(pool)
	svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	svc.SetForges(settleForgeBuilder{f: &lostReplyForge{origin: w.origin}})
	svc.SetPublishFn(freshE2EPublish(w.origin))
	svc.SetDeleteCheckpointFn(func(ctx context.Context, o pushbroker.DeleteOptions) error {
		ref := o.Ref
		if ref == "" {
			ref = "refs/uzi-checkpoints/" + o.CheckpointKey
		}
		args := []string{"-C", w.origin, "update-ref", "-d", ref}
		if o.ExpectedOldTip != "" {
			args = append(args, o.ExpectedOldTip)
		}
		return exec.CommandContext(ctx, "git", args...).Run() //nolint:gosec // G204: fixed Git binary, owned directory and server-derived ref; no shell.
	})
	h.wsvc = svc
	rc := recoveryTestCfg()
	rc.JWTSecret, rc.AuthTokenTTL = h.cfg.JWTSecret, h.cfg.AuthTokenTTL
	h.cfg = rc

	// Fixtures: owner, forge connection, repo, model secret, queued run.
	w.owner = cliSeedUser(t, pool, false)
	pat, err := h.box.Seal([]byte("fixture-forge-pat"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := h.box.Seal([]byte("fixture-model-secret"))
	if err != nil {
		t.Fatal(err)
	}
	w.conn, w.repo, w.run = uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'gitlab','https://forge.example','bot',1,$3)`, w.conn, w.owner, pat)
	project := 9_000_000 + freshE2EProject.Add(1) + time.Now().UnixNano()%1_000_000
	cliMustExec(t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,$3,'fixture',$4,'main',true)`, w.repo, w.conn, project, "https://forge.example/fixture")
	cliMustExec(t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'anthropic_token','fixture',true,$3,'master')`,
		uuid.New(), w.owner, model)
	w.iid = 4628000 + int(freshE2EProject.Load()%900)
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status)
		VALUES($1,$2,$3,'issue',$4,'fresh resume proof','Implement the fixture change','queued')`, w.run, w.owner, w.repo, w.iid)
	w.ownerToken = cliMintToken(t, pool, w.owner, clitoken.ScopeUser)

	// Workers: W1 an ordinary worker, W2 and W3 hosted persistent workers; every token a real jointoken.
	mk := func(hosted bool) (uuid.UUID, string) {
		tok, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		if hosted {
			cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,kind,hosted_size,template_declared,protocol_capabilities)
				VALUES($1,$2,$3,$4,'online',now(),'hosted','m','base',$5)`, id, w.owner, "hosted-"+id.String(), hash,
				[]string{"fresh_resume_v1", "recovery_inventory_v1"})
		} else {
			cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at) VALUES($1,$2,$3,$4,'online',now())`,
				id, w.owner, "plain-"+id.String(), hash)
		}
		return id, tok
	}
	w.w1, w.w1Token = mk(false)
	w.w2, w.w2Token = mk(true)
	w.w3, w.w3Token = mk(true)
	w.podUID = uuid.NewString()
	w.w1Root = filepath.Join(dir, "w1")
	w.w2Root = filepath.Join(dir, "w2")
	t.Cleanup(func() {
		// A transfer row pins its capture (ON DELETE RESTRICT): remove it so a later test's cleanup can run.
		c, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = pool.Exec(c, `DELETE FROM run_fresh_resumes WHERE run_id=$1`, w.run)
	})

	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)

	// W1 reaches its hold through the real routes.
	var out freshE2EW1
	w.runFixtureInto(&out, w.start("w1", w.w1Token, w.w1Root, map[string]any{"evidence": evidence}))
	w.w1Result = out
	if !out.Parked.Applied || out.Parked.Status != "recovery_wait" || out.Parked.Cause == nil || *out.Parked.Cause != "retained_recovery_adoption_failed" {
		t.Fatalf("W1 park through the real route = %+v", out.Parked)
	}
	w.w1Digest = freshE2EDigest(t, w.w1Root)
	return w
}

// serve is the real router with two fault seams: the seeded receipt's reply can be dropped after the real
// handler committed it, and a hook can fire at that instant.
func (w *freshE2EWorld) serve(rw http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/abandon") && r.Method == http.MethodPost:
		w.traffic.abandons.Add(1)
	case strings.HasSuffix(p, "/bundle") && r.Method == http.MethodGet:
		w.traffic.bundles.Add(1)
	}
	if r.Method == http.MethodPost && strings.Contains(p, "/fresh-resume/") && strings.HasSuffix(p, "/seeded") {
		w.traffic.seeded.Add(1)
		if w.traffic.dropSeeded.Load() {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			r.Body = io.NopCloser(bytes.NewReader(body))
			rec := httptest.NewRecorder()
			w.router.ServeHTTP(rec, r)
			if rec.Code == http.StatusOK {
				if hook := w.traffic.onSeededCommit.Load(); hook != nil {
					(*hook)()
				}
			}
			if c, _, err := rw.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
			return
		}
	}
	w.router.ServeHTTP(rw, r)
}

// start launches one Node worker generation. The child's config names only its own root.
func (w *freshE2EWorld) start(mode, token, root string, extra map[string]any) *freshE2EProc {
	w.t.Helper()
	w.mu.Lock()
	w.procs++
	n := w.procs
	w.mu.Unlock()
	cfg := map[string]any{
		"mode": mode, "url": w.srv.URL, "token": token, "run": w.run.String(), "origin": w.origin, "root": root,
		"iid": w.iid, "logFile": filepath.Join(w.dir, fmt.Sprintf("%02d-%s.log", n, mode)), "markerFile": filepath.Join(w.dir, fmt.Sprintf("%02d-marker", n)),
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 4*time.Minute)
	w.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "test/fixtures/fresh-resume-e2e.ts")
	cmd.Dir = filepath.Join(w.root, "agent")
	tmp := filepath.Join(w.dir, fmt.Sprintf("tmp-%02d", n))
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		w.t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "UZI_FRESH_E2E_FIXTURE="+string(raw), "TMPDIR="+tmp)
	if mode != "w1" && mode != "edit-journal" {
		// The hosted slot: a UUID pod id, set in the destination's child only.
		cmd.Env = append(cmd.Env, "UZI_POD_UID="+w.podUID)
	}
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	p := &freshE2EProc{t: w.t, cmd: cmd, out: out, token: token, cfgJSON: string(raw), done: make(chan error, 1)}
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
	return p
}

func (w *freshE2EWorld) runFixtureInto(dst any, p *freshE2EProc) {
	w.t.Helper()
	raw, err := p.Wait()
	if err != nil {
		w.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		w.t.Fatalf("decode RESULT: %v\n%s", err, raw)
	}
}

// startW2 launches one destination generation without waiting for it.
func (w *freshE2EWorld) startW2(executor string, polls int, extra ...map[string]any) *freshE2EProc {
	w.t.Helper()
	cfg := map[string]any{"executor": executor, "claimPolls": polls}
	for _, e := range extra {
		for k, v := range e {
			cfg[k] = v
		}
	}
	p := w.start("w2", w.w2Token, w.w2Root, cfg)
	if strings.Contains(p.cfgJSON, w.w1Root) {
		w.t.Fatal("W2's child config names W1's root")
	}
	return p
}

func (w *freshE2EWorld) finishW2(p *freshE2EProc) freshE2EW2 {
	w.t.Helper()
	var out freshE2EW2
	w.runFixtureInto(&out, p)
	return out
}

// runW2 runs one destination generation to completion.
func (w *freshE2EWorld) runW2(executor string, polls int, extra ...map[string]any) freshE2EW2 {
	w.t.Helper()
	return w.finishW2(w.startW2(executor, polls, extra...))
}

// claimOnly runs a registering (polls > 0: and claiming) worker generation that never starts a runner.
func (w *freshE2EWorld) claimOnly(token string, polls int) map[string]any {
	w.t.Helper()
	w.mu.Lock()
	root := filepath.Join(w.dir, fmt.Sprintf("claimer-%d", w.procs+1))
	w.mu.Unlock()
	var out map[string]any
	w.runFixtureInto(&out, w.start("claim", token, root, map[string]any{"claimPolls": polls}))
	return out
}

// restartW2 is the server's view of a W2 restart: the new process registers with an empty active snapshot
// and the api requeues the run its predecessor generation left claimed (within the worker-death budget).
func (w *freshE2EWorld) restartW2() freshE2ERun {
	w.t.Helper()
	w.claimOnly(w.w2Token, 0)
	got := w.runRow()
	if got.Status != "queued" {
		w.t.Fatalf("W2's restart left the run %s cause=%v, want it requeued", got.Status, freshE2EDeref(got.Cause))
	}
	return got
}

// waitMarker waits (bounded) for the hold executor's marker file, failing if the process ends first.
func (w *freshE2EWorld) waitMarker(p *freshE2EProc) {
	w.t.Helper()
	marker := ""
	var cfg map[string]any
	if err := json.Unmarshal([]byte(p.cfgJSON), &cfg); err == nil {
		marker, _ = cfg["markerFile"].(string)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case err := <-p.done:
			p.done <- err
			w.t.Fatalf("the worker ended before its model started: %v\n%s", err, p.printable())
		case <-time.After(100 * time.Millisecond):
		}
	}
	w.t.Fatal("the model never started on W2 within 90s")
}

func (w *freshE2EWorld) preview() (int, apitypes.FreshResumePreviewDTO) {
	w.t.Helper()
	rec := bearerReq(w.router, http.MethodGet, fmt.Sprintf("/api/runs/%s/fresh-resume", w.run), w.ownerToken)
	var dto apitypes.FreshResumePreviewDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			w.t.Fatalf("decode preview: %v", err)
		}
	}
	return rec.Code, dto
}

func (w *freshE2EWorld) startRequest(selection string) *httptest.ResponseRecorder {
	return bearerReqBody(w.router, http.MethodPost, fmt.Sprintf("/api/runs/%s/fresh-resume", w.run), w.ownerToken, freshStartBody(selection, uuid.New()))
}

// startOK previews, asserts the evidence is selectable and starts the transfer through the owner routes.
func (w *freshE2EWorld) startOK(kind, sha string) (apitypes.FreshResumePreviewDTO, apitypes.FreshResumeStartDTO) {
	w.t.Helper()
	code, dto := w.preview()
	if code != http.StatusOK || dto.Status != apitypes.FreshResumeStatusAvailable || dto.Kind != kind || dto.Sha != sha || dto.SelectionToken == "" {
		w.t.Fatalf("owner preview = %d %+v, want available %s %s", code, dto, kind, sha)
	}
	rec := w.startRequest(dto.SelectionToken)
	var started apitypes.FreshResumeStartDTO
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &started) != nil || started.State != "pending" || started.Replay {
		w.t.Fatalf("owner start = %d %s", rec.Code, rec.Body.String())
	}
	return dto, started
}

type freshE2ERun struct {
	Status string
	Worker *uuid.UUID
	Gen    int64
	Cause  *string
	Fail   *string
}

func (w *freshE2EWorld) runRow() freshE2ERun {
	w.t.Helper()
	var r freshE2ERun
	if err := w.pool.QueryRow(w.ctx, `SELECT status, worker_id, claim_generation, recovery_wait_cause, fail_origin FROM runs WHERE id=$1`, w.run).
		Scan(&r.Status, &r.Worker, &r.Gen, &r.Cause, &r.Fail); err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *freshE2EWorld) transferStates() []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.ctx, `SELECT state FROM run_fresh_resumes WHERE run_id=$1 ORDER BY created_at`, w.run)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

type freshE2ETransfer struct {
	ID          uuid.UUID
	State       string
	Refusal     *string
	Attempt     *string
	SeededGen   *int64
	ClaimGen    *int64
	Destination *uuid.UUID
}

func (w *freshE2EWorld) transfer() freshE2ETransfer {
	w.t.Helper()
	var x freshE2ETransfer
	if err := w.pool.QueryRow(w.ctx, `SELECT id, state, refusal_reason, destination_attempt_id, seeded_generation, claim_generation, destination_worker_id
		FROM run_fresh_resumes WHERE run_id=$1 ORDER BY created_at DESC LIMIT 1`, w.run).Scan(&x.ID, &x.State, &x.Refusal, &x.Attempt, &x.SeededGen, &x.ClaimGen, &x.Destination); err != nil {
		w.t.Fatal(err)
	}
	return x
}

// w1Rows is W1's custody as stored: its holds and the captures under them, as the jsonb of each row.
func (w *freshE2EWorld) w1Rows() string {
	w.t.Helper()
	var s string
	if err := w.pool.QueryRow(w.ctx, `SELECT
		coalesce((SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM recovery_custody_holds h WHERE h.run_id=$1 AND h.original_worker_id=$2),'[]'::jsonb)::text ||
		coalesce((SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM recovery_captures c WHERE c.run_id=$1 AND c.original_worker_id=$2),'[]'::jsonb)::text`,
		w.run, w.w1).Scan(&s); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *freshE2EWorld) feed() string {
	w.t.Helper()
	var s string
	if err := w.pool.QueryRow(w.ctx, `SELECT coalesce(string_agg(payload::text,' ' ORDER BY seq),'') FROM run_messages WHERE run_id=$1`, w.run).Scan(&s); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *freshE2EWorld) logs() string {
	w.t.Helper()
	files, _ := filepath.Glob(filepath.Join(w.dir, "*.log"))
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // G304: a log file globbed under the test's own fixture directory.
		if err != nil {
			w.t.Fatal(err)
		}
		b.Write(raw)
	}
	return strings.NewReplacer(w.w1Token, "[redacted]", w.w2Token, "[redacted]", w.w3Token, "[redacted]").Replace(b.String())
}

// lastW2Log is the worker log of the most recent destination generation only.
func (w *freshE2EWorld) lastW2Log() string {
	w.t.Helper()
	files, _ := filepath.Glob(filepath.Join(w.dir, "*-w2.log"))
	if len(files) == 0 {
		w.t.Fatal("no destination generation log")
	}
	sort.Strings(files)
	raw, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		w.t.Fatal(err)
	}
	return strings.NewReplacer(w.w1Token, "[redacted]", w.w2Token, "[redacted]", w.w3Token, "[redacted]").Replace(string(raw))
}

// assertW1Untouched compares W1's whole root with its digest taken when it parked, and asserts W2 was never
// given W1's root: separate trees, and W2's attempt clone under its own.
func (w *freshE2EWorld) assertW1Untouched(attemptPath string) {
	w.t.Helper()
	if got := freshE2EDigest(w.t, w.w1Root); got != w.w1Digest {
		w.t.Fatalf("W1's root changed: digest %s, was %s", got, w.w1Digest)
	}
	if strings.HasPrefix(w.w2Root+string(os.PathSeparator), w.w1Root+string(os.PathSeparator)) || strings.HasPrefix(w.w1Root+string(os.PathSeparator), w.w2Root+string(os.PathSeparator)) {
		w.t.Fatal("W1 and W2 share a root")
	}
	if attemptPath != "" && !strings.HasPrefix(attemptPath, w.w2Root+string(os.PathSeparator)) {
		w.t.Fatalf("W2's attempt clone %q is not under W2's own root", attemptPath)
	}
}

// freshE2EDigest is a full tree digest: every path, type, mode, file content and link target.
func freshE2EDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s|%o\n", rel, info.Mode())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "->%s\n", target)
		case info.Mode().IsRegular():
			f, err := os.Open(path) //nolint:gosec // G304: a path walked under the test's own fixture directory.
			if err != nil {
				return err
			}
			defer f.Close() //nolint:errcheck // read-only handle on a fixture file.
			sum := sha256.New()
			if _, err := io.Copy(sum, f); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "#%s\n", hex.EncodeToString(sum.Sum(nil)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("digest %s: %v", root, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// workerCall is a raw worker route call on the real router (the replays and fences of the route subtests).
func (w *freshE2EWorld) workerCall(method, tail, token, body string) *httptest.ResponseRecorder {
	return bearerReqBody(w.router, method, fmt.Sprintf("/api/worker/runs/%s/fresh-resume/%s%s", w.run, w.transfer().ID, tail), token, body)
}

func freshE2EReason(rec *httptest.ResponseRecorder) string {
	var body map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		return ""
	}
	r, _ := body["reason"].(string)
	return r
}

func (w *freshE2EWorld) wantRefused(rec *httptest.ResponseRecorder, what string) {
	w.t.Helper()
	if rec.Code != http.StatusConflict || freshE2EReason(rec) != apitypes.FreshResumeReasonNotClaimed {
		w.t.Fatalf("%s = %d %s, want 409 %s", what, rec.Code, rec.Body.String(), apitypes.FreshResumeReasonNotClaimed)
	}
}

func (w *freshE2EWorld) authority(token string, live int64) (string, bool) {
	w.t.Helper()
	rec := w.workerCall(http.MethodGet, fmt.Sprintf("/state?live_generation=%d", live), token, "")
	var dto apitypes.FreshResumeStateDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &dto) != nil {
		w.t.Fatalf("state = %d %s", rec.Code, rec.Body.String())
	}
	return dto.State, dto.CurrentAuthority
}

// freshE2EAttempt is a well-formed destination attempt id for generation g.
func freshE2EAttempt(g int64) string { return fmt.Sprintf("20260101T000000Z-g%d-0123456789abcdef", g) }

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, x := range want {
		if !set[x] {
			return false
		}
	}
	return true
}

// assertCompletedFreshRun is the end state every continuing subtest shares: the model ran once on the
// seeded base, the run completed on W2 at G'+1 and was never failed.
func (w *freshE2EWorld) assertCompletedFreshRun(out freshE2EW2, sha string) {
	w.t.Helper()
	if out.Outcome != "returned" || out.ExecutorRuns != 1 || out.MrCreates != 1 {
		w.t.Fatalf("W2 = %+v\nlogs:\n%s", out, w.logs())
	}
	if out.ExecutionBase != sha || !out.StrictlyAhead {
		w.t.Fatalf("execution base %s (strictly ahead %v), want the evidence %s", out.ExecutionBase, out.StrictlyAhead, sha)
	}
	run := w.runRow()
	if run.Status != "completed" || run.Fail != nil || run.Worker == nil || *run.Worker != w.w2 {
		w.t.Fatalf("run after finalize = %+v", run)
	}
}

func TestFreshResumeSeparateWorkerE2E(t *testing.T) {
	if os.Getenv("UZI_CODEX_REFRESH_LOSTREPLY_E2E") == "" {
		t.Skip("opt in with task test:codex-refresh-lostreply-e2e")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fixture root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("combined proof requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("combined proof requires installed agent tsx")
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Fatal("combined proof requires UZI_TEST_DATABASE_URL")
	}

	// 1 and 2: the two evidence kinds, end to end through seed, receipt, continuation and finalize.
	for _, kind := range []string{apitypes.FreshResumeKindCapture, apitypes.FreshResumeKindCheckpointRef} {
		t.Run(kind, func(t *testing.T) {
			w := newFreshE2EWorld(t, root, kind)
			r1 := w.w1Result
			sha := r1.M2
			dto, started := w.startOK(kind, sha)
			if dto.EvidenceGeneration != r1.Generation || dto.ClaimGeneration != r1.Generation {
				t.Fatalf("preview generations = %d/%d, want %d", dto.EvidenceGeneration, dto.ClaimGeneration, r1.Generation)
			}
			if kind == apitypes.FreshResumeKindCapture && dto.CaptureID != r1.CaptureID {
				t.Fatalf("preview capture %s, want the one W1 uploaded (%s)", dto.CaptureID, r1.CaptureID)
			}
			if kind == apitypes.FreshResumeKindCheckpointRef && dto.CheckpointRef != "refs/uzi-checkpoints/"+r1.CheckpointKey {
				t.Fatalf("preview checkpoint ref %q", dto.CheckpointRef)
			}
			out := w.runW2("finish", 12)
			w.assertCompletedFreshRun(out, sha)
			// The seeded artifact is the transfer's sha; the claim named the same evidence.
			if out.Journal == nil || out.Journal.Sha != sha || out.Journal.Sha != started.Sha || out.Journal.Frid != started.ID {
				t.Fatalf("seeded journal %+v, transfer sha %s id %s", out.Journal, started.Sha, started.ID)
			}
			if out.ClaimFresh["sha"] != sha || out.ClaimFresh["kind"] != kind || out.ClaimFresh["id"] != started.ID {
				t.Fatalf("claim fresh_resume = %v", out.ClaimFresh)
			}
			// PRD #628 criterion 2: the execution base is strictly ahead of the default branch, and it is the evidence.
			if out.ExecutionBase == out.DefaultTip || out.DefaultTip != r1.MainTip {
				t.Fatalf("execution base %s default %s", out.ExecutionBase, out.DefaultTip)
			}
			// Criterion 4: the seed reports a fresh resume and keeps W1's milestones (both commits, both files).
			if out.CommitsAhead != 2 || !containsAll(out.Files, "milestone-1.txt", "milestone-2.txt") {
				t.Fatalf("milestones not preserved: ahead=%d files=%v", out.CommitsAhead, out.Files)
			}
			if !strings.Contains(w.feed(), "continues from saved work selected for a fresh resume") {
				t.Fatalf("the run feed does not report the fresh resume seed:\n%s", w.feed())
			}
			// The model continued from the seeded base: the pushed head descends from it.
			freshE2EGit(t, w.origin, "merge-base", "--is-ancestor", sha, out.FinalHead)
			// L4 landed before the model started: the journal the executor saw is already ready, for the same attempt.
			if out.Journal.Stage != "ready" || out.JournalAfter == nil || out.JournalAfter.Stage != "ready" || out.JournalAfter.AttemptID != out.Journal.AttemptID {
				t.Fatalf("journal before %+v after %+v", out.Journal, out.JournalAfter)
			}
			if run := w.runRow(); run.Gen != r1.Generation+1 {
				t.Fatalf("claim generation %d, want G'=%d", run.Gen, r1.Generation+1)
			}
			tr := w.transfer()
			if tr.Attempt == nil || *tr.Attempt != out.Journal.AttemptID || tr.SeededGen == nil || *tr.SeededGen != r1.Generation+1 {
				t.Fatalf("server receipt %+v does not match the worker's seed %+v", tr, out.Journal)
			}
			if tr.Destination == nil || *tr.Destination != w.w2 {
				t.Fatalf("the transfer's destination %v is not W2 %s", tr.Destination, w.w2)
			}
			// A capture is streamed exactly once; a checkpoint ref has no bundle to stream. Nothing was given back.
			wantBundles := int64(0)
			if kind == apitypes.FreshResumeKindCapture {
				wantBundles = 1
			}
			if got := w.traffic.bundles.Load(); got != wantBundles || w.traffic.abandons.Load() != 0 || w.traffic.seeded.Load() != 1 {
				t.Fatalf("worker traffic bundles=%d abandons=%d seeded=%d, want %d/0/1", got, w.traffic.abandons.Load(), w.traffic.seeded.Load(), wantBundles)
			}
			w.assertW1Untouched(out.AttemptPath)
			t.Logf("end state: transfer=%s", tr.State)
		})
	}

	// lostReply drives the first W2 generation of subtests 3 to 5: the real handler commits the seeded
	// receipt and the reply never reaches the worker.
	lostReply := func(w *freshE2EWorld, extra ...map[string]any) (freshE2EW2, freshE2ETransfer) {
		t.Helper()
		w.startOK(apitypes.FreshResumeKindCapture, w.w1Result.M2)
		w.traffic.dropSeeded.Store(true)
		first := w.runW2("finish", 12, extra...)
		w.traffic.dropSeeded.Store(false)
		tr := w.transfer()
		if tr.State != "seeded" || tr.Attempt == nil || tr.SeededGen == nil || *tr.SeededGen != w.w1Result.Generation+1 {
			t.Fatalf("the receipt was not committed server-side: %+v\nlogs:\n%s", tr, w.logs())
		}
		if first.ExecutorRuns != 0 || first.MrCreates != 0 || first.JournalAfter == nil || first.JournalAfter.AttemptID != *tr.Attempt || first.JournalAfter.Stage != "seeding" {
			t.Fatalf("first generation = %+v, server attempt %s", first, *tr.Attempt)
		}
		if run := w.runRow(); run.Status == "failed" || run.Fail != nil {
			t.Fatalf("a lost seeded reply failed the run: %+v", run)
		}
		return first, tr
	}

	// 3: the reply is lost, W2 restarts gracefully at G'+1: Proof A, L4, continuation and finalize.
	t.Run("lost_seeded_reply_graceful_restart", func(t *testing.T) {
		w := newFreshE2EWorld(t, root, "capture")
		sha := w.w1Result.M2
		first, tr := lostReply(w, map[string]any{"authority": true})
		w.restartW2()
		second := w.runW2("finish", 12, map[string]any{"authority": true})
		w.assertCompletedFreshRun(second, sha)
		if second.Journal == nil || second.Journal.AttemptID != *tr.Attempt || second.Journal.Sha != sha || second.Journal.Stage != "ready" {
			t.Fatalf("restart adopted %+v, want the receipted attempt %s", second.Journal, *tr.Attempt)
		}
		if run := w.runRow(); run.Gen != w.w1Result.Generation+2 {
			t.Fatalf("claim generation %d after the restart, want G'+1=%d", run.Gen, w.w1Result.Generation+2)
		}
		_ = first
		w.assertW1Untouched(second.AttemptPath)
	})

	// 4: the same lost reply, but the worker is killed (SIGKILL, no shutdown handler) the instant the receipt commits.
	t.Run("lost_seeded_reply_sigkill_restart", func(t *testing.T) {
		w := newFreshE2EWorld(t, root, "capture")
		sha := w.w1Result.M2
		w.startOK(apitypes.FreshResumeKindCapture, sha)
		w.traffic.dropSeeded.Store(true)
		p := w.startW2("finish", 12, map[string]any{"authority": true})
		kill := func() { p.Kill() }
		w.traffic.onSeededCommit.Store(&kill)
		p.WaitKilled()
		w.traffic.dropSeeded.Store(false)
		tr := w.transfer()
		if tr.State != "seeded" || tr.Attempt == nil {
			t.Fatalf("receipt not committed before the kill: %+v", tr)
		}
		if run := w.runRow(); run.Status == "failed" || run.Fail != nil || run.Worker == nil || *run.Worker != w.w2 {
			t.Fatalf("run after the kill = %+v", run)
		}
		w.restartW2()
		second := w.runW2("finish", 12, map[string]any{"authority": true})
		run := w.runRow()
		t.Logf("after the restart: run=%s cause=%s worker=%v gen=%d outcome=%s executorRuns=%d mr=%d", run.Status, freshE2EDeref(run.Cause), run.Worker, run.Gen, second.Outcome, second.ExecutorRuns, second.MrCreates)
		t.Logf("restart log: %s | %s", firstLine(w.lastW2Log(), "fresh resume"), firstLine(w.lastW2Log(), "retained"))
		// The hard kill left the seeded attempt's slot active under a dead incarnation, so the restarted worker cannot prove
		// its source use ended: it keeps the seed and its custody and asks for the owner hold. The run is never failed.
		if run.Status != "recovery_wait" || run.Cause == nil || *run.Cause != "retained_recovery_adoption_failed" || run.Fail != nil || run.Worker == nil || *run.Worker != w.w2 {
			t.Fatalf("run after the killed worker's restart = %+v, want the owner hold", run)
		}
		if second.ExecutorRuns != 0 || second.MrCreates != 0 {
			t.Fatalf("the restart continued from a source whose use was not proven ended: %+v", second)
		}
		if !strings.Contains(w.lastW2Log(), `"reason":"foreign_active"`) {
			t.Fatalf("the hold is not the foreign-active slot hold:\n%s", w.lastW2Log())
		}
		if got := w.transfer(); got.State != "seeded" || got.Attempt == nil || *got.Attempt != *tr.Attempt {
			t.Fatalf("the server's receipt changed: %+v", got)
		}
		if second.JournalAfter == nil || second.JournalAfter.AttemptID != *tr.Attempt || second.JournalAfter.Stage != "seeding" {
			t.Fatalf("custody not kept: journal after %+v", second.JournalAfter)
		}
		w.assertW1Untouched("")
	})

	// 5: the journal and ledger name another attempt than the server's receipt: a nonterminal stop, custody kept.
	t.Run("changed_attempt_id", func(t *testing.T) {
		w := newFreshE2EWorld(t, root, "capture")
		_, tr := lostReply(w)
		var edit map[string]any
		w.runFixtureInto(&edit, w.start("edit-journal", w.w2Token, w.w2Root, nil))
		if edit["from"] != *tr.Attempt {
			t.Fatalf("edited attempt %v, server attempt %s", edit["from"], *tr.Attempt)
		}
		w.restartW2()
		second := w.runW2("finish", 12)
		if second.ExecutorRuns != 0 || second.MrCreates != 0 {
			t.Fatalf("a changed attempt id still ran the model or published: %+v", second)
		}
		run := w.runRow()
		if run.Status == "failed" || run.Status == "completed" || run.Fail != nil {
			t.Fatalf("a changed attempt id ended the run: %+v", run)
		}
		// The stop is the SECOND generation's: the first one's lost reply logged its own.
		logs := w.lastW2Log()
		if !strings.Contains(logs, "fresh resume stopped nonterminally") || !strings.Contains(logs, `"reason":"attempt_differs"`) {
			t.Fatalf("the restarted worker did not stop with attempt_differs:\n%s", logs)
		}
		if second.JournalAfter == nil || second.JournalAfter.AttemptID != edit["to"] {
			t.Fatalf("custody not kept: journal after %+v, edited to %v", second.JournalAfter, edit["to"])
		}
		if got := w.transfer(); got.State != "seeded" || got.Attempt == nil || *got.Attempt != *tr.Attempt {
			t.Fatalf("the server's receipt changed: %+v", got)
		}
		t.Logf("restart stop: %s", firstLine(logs, "fresh resume stopped nonterminally"))
	})

	// 6: a capture whose bundle does not verify: W2 abandons, and the run is back at its owner hold.
	t.Run("unverifiable_bundle_abandon", func(t *testing.T) {
		w := newFreshE2EWorld(t, root, "junk_capture")
		before := w.w1Rows()
		w.startOK(apitypes.FreshResumeKindCapture, w.w1Result.Source)
		out := w.runW2("finish", 12)
		if out.ExecutorRuns != 0 || out.MrCreates != 0 {
			t.Fatalf("W2 ran on evidence it could not verify: %+v", out)
		}
		tr := w.transfer()
		if tr.State != "refused" || tr.Refusal == nil || *tr.Refusal != workersvc.FreshAbandonedReason || tr.Attempt != nil {
			t.Fatalf("transfer after the abandon = %+v", tr)
		}
		run := w.runRow()
		if run.Status != "recovery_wait" || run.Cause == nil || *run.Cause != "retained_recovery_adoption_failed" || run.Worker == nil || *run.Worker != w.w1 || run.Fail != nil {
			t.Fatalf("run after the abandon = %+v, want back at its W1 hold", run)
		}
		if w.traffic.abandons.Load() != 1 || w.traffic.seeded.Load() != 0 {
			t.Fatalf("worker traffic abandons=%d seeded=%d, want exactly one abandon and no receipt", w.traffic.abandons.Load(), w.traffic.seeded.Load())
		}
		if after := w.w1Rows(); after != before {
			t.Fatalf("W1's custody changed:\n%s\n%s", before, after)
		}
		// The fixture records the failing seed step; the junk bundle must fail at the fetch, not at a later step.
		if l := w.lastW2Log(); !strings.Contains(l, "fresh seed step failed") || !strings.Contains(l, `"step":"fetchFreshBundle"`) {
			t.Fatalf("no fetchFreshBundle failure recorded:\n%s", l)
		}
		w.assertW1Untouched("")
	})

	// 7: expired, ambiguous and unbound evidence: preview and start refuse, the run stays held.
	t.Run("evidence_refused", func(t *testing.T) {
		refused := func(t *testing.T, w *freshE2EWorld, status, token string) {
			t.Helper()
			code, dto := w.preview()
			if code != http.StatusOK || dto.Status != status || dto.SelectionToken != "" {
				t.Fatalf("preview = %d %+v, want status %s with no token", code, dto, status)
			}
			if token == "" {
				token = "0000000000000000000000000000000000000000000000000000000000000000"
			}
			rec := w.startRequest(token)
			if rec.Code != http.StatusConflict || freshE2EReason(rec) != status {
				t.Fatalf("start = %d %s, want 409 %s", rec.Code, rec.Body.String(), status)
			}
			if got := w.transferStates(); len(got) != 0 {
				t.Fatalf("a refused start left transfers %v", got)
			}
			run := w.runRow()
			if run.Status != "recovery_wait" || run.Cause == nil || *run.Cause != "retained_recovery_adoption_failed" || run.Worker == nil || *run.Worker != w.w1 || run.Fail != nil {
				t.Fatalf("run after the refusal = %+v", run)
			}
			w.assertW1Untouched("")
		}
		t.Run("expired", func(t *testing.T) {
			w := newFreshE2EWorld(t, root, "capture")
			code, dto := w.preview()
			if code != http.StatusOK || dto.Status != apitypes.FreshResumeStatusAvailable {
				t.Fatalf("baseline preview = %d %+v", code, dto)
			}
			// The capture is inside its last 30 minutes and the checkpoint ref is gone: nothing usable remains.
			cliMustExec(t, w.pool, `UPDATE recovery_captures SET expires_at=now()+interval '10 minutes' WHERE run_id=$1`, w.run)
			freshE2EGit(t, w.origin, "update-ref", "-d", "refs/uzi-checkpoints/"+w.w1Result.CheckpointKey)
			refused(t, w, apitypes.FreshResumeStatusExpired, dto.SelectionToken)
		})
		t.Run("ambiguous", func(t *testing.T) {
			w := newFreshE2EWorld(t, root, "ambiguous")
			refused(t, w, apitypes.FreshResumeStatusAmbiguous, "")
		})
		t.Run("unbound", func(t *testing.T) {
			w := newFreshE2EWorld(t, root, "checkpoint_ref")
			// A checkpoint tip whose generation stamp is not the tip's own may hold work of an unknown generation.
			cliMustExec(t, w.pool, `UPDATE runs SET checkpoint_tip_generation=NULL, checkpoint_tip_generation_tip=NULL WHERE id=$1`, w.run)
			refused(t, w, apitypes.FreshResumeStatusUnbound, "")
		})
	})

	// 8: W2's claim generation moved: the claim-generation-fenced routes (bundle, seeded, abandon) refuse it.
	// The state route is fenced only on run, owner and destination, so it is not exercised here.
	t.Run("stale_claim_refused", func(t *testing.T) {
		w := newFreshE2EWorld(t, root, "capture")
		w.startOK(apitypes.FreshResumeKindCapture, w.w1Result.M2)
		claim := w.claimOnly(w.w2Token, 12)
		g := int64(claim["claim_generation"].(float64))
		if g != w.w1Result.Generation+1 {
			t.Fatalf("W2 claimed at %d", g)
		}
		if tr := w.transfer(); tr.State != "claimed" || tr.ClaimGen == nil || *tr.ClaimGen != g {
			t.Fatalf("transfer after the claim = %+v", tr)
		}
		stale := func(gen int64, what string) {
			t.Helper()
			w.wantRefused(w.workerCall(http.MethodGet, fmt.Sprintf("/bundle?claim_generation=%d", gen), w.w2Token, ""), what+" bundle")
			w.wantRefused(w.workerCall(http.MethodPost, "/seeded", w.w2Token, freshSeededBody(gen, freshE2EAttempt(gen), gen)), what+" seeded")
			w.wantRefused(w.workerCall(http.MethodPost, "/abandon", w.w2Token, freshGenBody(gen)), what+" abandon")
		}
		// Positive control: the same route at the current generation, as the claimed destination, is served.
		if rec := w.workerCall(http.MethodGet, fmt.Sprintf("/bundle?claim_generation=%d", g), w.w2Token, ""); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("bundle at the current generation = %d (%d bytes), want 200 with a body", rec.Code, rec.Body.Len())
		}
		stale(g-1, "an older generation")
		stale(g+1, "a newer generation")
		// Another worker with the right generation is refused too.
		w.wantRefused(w.workerCall(http.MethodGet, fmt.Sprintf("/bundle?claim_generation=%d", g), w.w3Token, ""), "another worker")
		// Move the run's claim generation under the guard's opt-in, as a reclaim would.
		tx, err := w.pool.Begin(w.ctx)
		if err != nil {
			t.Fatal(err)
		}
		q := store.New(tx)
		if err := q.OptInFreshResume(w.ctx, w.transfer().ID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(w.ctx, `UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1`, w.run); err != nil {
			t.Fatal(err)
		}
		if err := q.ClearFreshResumeOptIn(w.ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(w.ctx); err != nil {
			t.Fatal(err)
		}
		stale(g, "the generation W2 claimed at, after it moved")
		if tr := w.transfer(); tr.State != "claimed" {
			t.Fatalf("a refused call changed the transfer: %+v", tr)
		}
	})

	// 9: W1 is untouched while W2 seeds and its model runs: tree digest, holds and captures byte-equal.
	t.Run("w1_unchanged", func(t *testing.T) {
		w := newFreshE2EWorld(t, root, "capture")
		rowsBefore := w.w1Rows()
		w.startOK(apitypes.FreshResumeKindCapture, w.w1Result.M2)
		p := w.startW2("hold", 12)
		w.waitMarker(p)
		if run := w.runRow(); run.Status != "running" || run.Worker == nil || *run.Worker != w.w2 || run.Fail != nil {
			t.Fatalf("run while W2's model runs = %+v", run)
		}
		if tr := w.transfer(); tr.State != "seeded" {
			t.Fatalf("transfer while W2's model runs = %+v", tr)
		}
		if got := w.w1Rows(); got != rowsBefore {
			t.Fatalf("W1's holds or captures changed while W2 ran:\n before %s\n after  %s", rowsBefore, got)
		}
		w.assertW1Untouched("")
		p.Kill()
		p.WaitKilled()
		if got := w.w1Rows(); got != rowsBefore {
			t.Fatalf("W1's holds or captures changed after W2 died:\n before %s\n after  %s", rowsBefore, got)
		}
		w.assertW1Untouched("")
	})

	// 10: a replayed receipt reports current_authority false once W2 no longer holds the run's live claim.
	t.Run("receipt_replay_current_authority", func(t *testing.T) {
		seed := func(w *freshE2EWorld) (g int64, attempt string) {
			w.startOK(apitypes.FreshResumeKindCapture, w.w1Result.M2)
			claim := w.claimOnly(w.w2Token, 12)
			g = int64(claim["claim_generation"].(float64))
			attempt = freshE2EAttempt(g)
			rec := w.workerCall(http.MethodPost, "/seeded", w.w2Token, freshSeededBody(g, attempt, g))
			var dto apitypes.FreshResumeStateDTO
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &dto) != nil || dto.State != "seeded" || !dto.CurrentAuthority {
				t.Fatalf("receipt = %d %s", rec.Code, rec.Body.String())
			}
			return g, attempt
		}
		replay := func(w *freshE2EWorld, g int64, attempt string, live int64) bool {
			t.Helper()
			rec := w.workerCall(http.MethodPost, "/seeded", w.w2Token, freshSeededBody(g, attempt, live))
			if rec.Code != http.StatusOK {
				t.Fatalf("replay at live generation %d = %d %s", live, rec.Code, rec.Body.String())
			}
			var dto apitypes.FreshResumeStateDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			return dto.CurrentAuthority
		}
		t.Run("cancel", func(t *testing.T) {
			w := newFreshE2EWorld(t, root, "capture")
			g, attempt := seed(w)
			if !replay(w, g, attempt, g) {
				t.Fatal("baseline replay lost authority")
			}
			rec := bearerReqBody(w.router, http.MethodPost, fmt.Sprintf("/api/runs/%s/inputs", w.run), w.ownerToken, `{"kind":"cancel"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("owner cancel = %d %s", rec.Code, rec.Body.String())
			}
			if _, authority := w.authority(w.w2Token, g); authority {
				t.Fatal("state route still reports current authority after the owner cancel")
			}
			if replay(w, g, attempt, g) {
				t.Fatal("replayed receipt still reports current authority after the owner cancel")
			}
		})
		t.Run("requeue_and_reclaim", func(t *testing.T) {
			w := newFreshE2EWorld(t, root, "capture")
			g, attempt := seed(w)
			// W2 restarts without its run: the api requeues it.
			w.restartW2()
			if replay(w, g, attempt, g) {
				t.Fatal("replayed receipt reports current authority for a requeued run")
			}
			// W3 reclaims at G'+1: the old receipt is judged against the old generation and the new holder.
			claim := w.claimOnly(w.w3Token, 12)
			if next := int64(claim["claim_generation"].(float64)); next != g+1 {
				t.Fatalf("W3 reclaimed at %v, want %d", claim["claim_generation"], g+1)
			}
			if replay(w, g, attempt, g) || replay(w, g, attempt, g+1) {
				t.Fatal("replayed receipt reports current authority after a reclaim at G'+1")
			}
			if state, authority := w.authority(w.w2Token, g+1); state != "seeded" || authority {
				t.Fatalf("state after the reclaim = %s authority=%v", state, authority)
			}
		})
	})
}

func freshE2EDeref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func firstLine(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
