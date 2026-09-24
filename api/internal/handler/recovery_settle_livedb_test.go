package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #1582 M1 live-DB proofs for POST /api/worker/runs/{id}/recovery-holds/{holdID}/settle:
// the api releases ONE older-generation custody hold on a completed run ONLY on its own forge
// ancestry proof, through the REAL router (RequireWorker Bearer auth), the REAL workersvc
// service and the REAL Postgres guarded UPDATE. The forge is a fake injected through the
// workersvc forges seam (SetForges), except the 429 case, which drives the REAL GitHub driver
// against an httptest server. Lives in the handler package so e2e/run-store-it.sh and CI's
// test-api-store-it job execute it. Skipped unless UZI_TEST_DATABASE_URL is set.

const (
	settleHead    = "dddddddddddddddddddddddddddddddddddddddd"
	settlePushed  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	settleSource  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	settleAdopted = "cccccccccccccccccccccccccccccccccccccccc"
	settleOther   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// settleFakeForge answers BranchHead / CompareAncestry from scripted values and counts calls.
// beforeCompare, when set, runs once before the first CompareAncestry answer (the mid-proof
// mutation hook).
type settleFakeForge struct {
	forgetest.BaseFake
	mu            sync.Mutex
	head          string
	headErr       error
	verdict       map[string]forge.Ancestry
	verdictErr    map[string]error
	beforeCompare func()
	headCalls     int
	compareCalls  int
}

func (f *settleFakeForge) BranchHead(context.Context, int64, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headCalls++
	return f.head, f.headErr
}

func (f *settleFakeForge) CompareAncestry(_ context.Context, _ int64, head, candidate string) (forge.Ancestry, error) {
	f.mu.Lock()
	hook := f.beforeCompare
	f.beforeCompare = nil
	f.compareCalls++
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if head != f.head {
		return forge.AncestryUnknown, fmt.Errorf("fake: compared against unexpected head %q", head)
	}
	a, ok := f.verdict[candidate]
	if err := f.verdictErr[candidate]; err != nil {
		// An error answer returns the scripted verdict (Unknown by default) ALONGSIDE the
		// error, so a test can prove an error-carrying "ancestor" is never read as proof.
		if !ok {
			a = forge.AncestryUnknown
		}
		return a, err
	}
	if ok {
		return a, nil
	}
	return forge.AncestryAncestor, nil
}

func (f *settleFakeForge) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headCalls, f.compareCalls
}

// settleForgeBuilder is the workersvc.ForgeBuilder seam: every connection resolves to f.
type settleForgeBuilder struct{ f forge.Forge }

func (b settleForgeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

// settleEnv is one owner with two workers (A = the caller, B = a sibling), a completed run
// held by A at claim_generation 2 on branch agent/issue-N, and three holds on that run:
//   - pred: generation 1, taken by A, OPEN — the hold under test;
//   - sibGen: generation 2, taken by A, OPEN — a sibling generation that must stay untouched;
//   - sibWorker: generation 1, taken by B, OPEN — a sibling worker's hold that must stay untouched.
type settleEnv struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	router   http.Handler
	fake     *settleFakeForge
	wsvc     *workersvc.Service
	user     uuid.UUID
	workerA  uuid.UUID
	tokenA   string
	workerB  uuid.UUID
	tokenB   string
	run      uuid.UUID
	branch   string
	pred     uuid.UUID
	sibGen   uuid.UUID
	sibWork  uuid.UUID
	repo     uuid.UUID
	iidCount int64
}

func newSettleEnv(t *testing.T) *settleEnv {
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

	q := store.New(pool)
	box := newHandlerTestBox(t)
	fake := &settleFakeForge{head: settleHead}
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetForges(settleForgeBuilder{f: fake})
	h := &Handler{pool: pool, q: q, box: box, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}, wsvc: wsvc}
	lim := mw.NewLimiter(100000, time.Minute, nil)

	e := &settleEnv{
		t: t, ctx: ctx, pool: pool, router: h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim),
		fake: fake, wsvc: wsvc, user: uuid.New(), workerA: uuid.New(), workerB: uuid.New(), repo: uuid.New(),
	}
	connID := uuid.New()
	e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, e.user, fmt.Sprintf("settle-%s@e2e", e.user))
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, e.user, []byte{0x1})
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 7, 'g/settle', 'https://forge.e2e/g/settle', 'main', true)`, e.repo, connID)
	e.tokenA = e.insertWorker(e.user, e.workerA)
	e.tokenB = e.insertWorker(e.user, e.workerB)
	e.run, e.branch = e.insertCompletedRun(e.workerA, 2)
	e.pred = e.insertHold(e.run, 1, e.workerA)
	// The predecessor claimed well before the successor: its hold predates the default
	// capture age (insertCapture, an hour ago), which predates the successor hold (now), so a
	// capture cutoff read off the WRONG generation's hold is visible.
	e.exec(`UPDATE recovery_custody_holds SET created_at = now() - interval '2 hours' WHERE id = $1`, e.pred)
	e.sibGen = e.insertHold(e.run, 2, e.workerA)
	e.sibWork = e.insertHold(e.run, 1, e.workerB)
	return e
}

func (e *settleEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *settleEnv) insertWorker(user, id uuid.UUID) string {
	e.t.Helper()
	tok, hash, err := jointoken.Generate()
	if err != nil {
		e.t.Fatalf("token: %v", err)
	}
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		id, user, "settle-"+id.String(), hash)
	return tok
}

// insertCompletedRun seeds a run 'completed' by worker at claimGen on a fresh branch.
func (e *settleEnv) insertCompletedRun(worker uuid.UUID, claimGen int64) (uuid.UUID, string) {
	e.t.Helper()
	e.iidCount++
	id := uuid.New()
	branch := fmt.Sprintf("agent/issue-%d", e.iidCount)
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed')`, id, e.user, e.repo, e.iidCount)
	e.exec(`UPDATE runs SET claim_generation = $2, worker_id = $3, branch = $4, status_since = now() WHERE id = $1`,
		id, claimGen, worker, branch)
	return id, branch
}

func (e *settleEnv) insertHold(run uuid.UUID, gen int64, worker uuid.UUID) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO recovery_custody_holds
	      (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	      VALUES ($1, $2, $3, $4, $5, 'open', $6, $7, $6, $4)`,
		id, e.user, e.repo, run, gen, worker, "settle-"+worker.String())
	return id
}

type holdRow struct {
	state, evidence                          string
	pushed, source, adopted, finalHead, bran string
	successor                                int64
	liveWorkerNull, liveRunNull              bool
}

func (e *settleEnv) hold(id uuid.UUID) holdRow {
	e.t.Helper()
	var r holdRow
	var ev, p, s, a, fh, b *string
	var succ *int64
	if err := e.pool.QueryRow(e.ctx, `SELECT state, release_evidence, release_pushed_sha, release_source_sha,
	        release_adopted_sha, release_final_head_sha, release_branch, release_successor_generation,
	        live_worker_id IS NULL, live_run_id IS NULL
	      FROM recovery_custody_holds WHERE id = $1`, id).
		Scan(&r.state, &ev, &p, &s, &a, &fh, &b, &succ, &r.liveWorkerNull, &r.liveRunNull); err != nil {
		e.t.Fatalf("read hold: %v", err)
	}
	deref := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	r.evidence, r.pushed, r.source, r.adopted, r.finalHead, r.bran = deref(ev), deref(p), deref(s), deref(a), deref(fh), deref(b)
	if succ != nil {
		r.successor = *succ
	}
	return r
}

func settleBody(pred, succ int64, pushed, source, adopted string) map[string]any {
	return map[string]any{
		"predecessor_generation": pred, "successor_generation": succ,
		"pushed_sha": pushed, "source_sha": source, "adopted_sha": adopted,
	}
}

func (e *settleEnv) settle(token string, run, hold uuid.UUID, body any) (int, apitypes.RecoverySettleResponse, string) {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/recovery-holds/%s/settle", run, hold), bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var res apitypes.RecoverySettleResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			e.t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, res, rec.Body.String()
}

// assertOpen fails unless every listed hold is still 'open' with no evidence and live FKs set.
func (e *settleEnv) assertOpen(ids ...uuid.UUID) {
	e.t.Helper()
	for _, id := range ids {
		if h := e.hold(id); h.state != "open" || h.evidence != "" || h.liveWorkerNull || h.liveRunNull {
			e.t.Fatalf("hold %s = %+v, want untouched open", id, h)
		}
	}
}

func (e *settleEnv) assertRetained(code int, res apitypes.RecoverySettleResponse, raw, reason string) {
	e.t.Helper()
	if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleRetained || res.Reason != reason || res.FinalHeadSha != "" {
		e.t.Fatalf("settle = %d %+v (%s), want 200 retained/%s", code, res, raw, reason)
	}
}

// assertNoForgeCalls fails if the fake forge was asked anything: eligibility is decided from
// the server's own rows before any proof is attempted.
func (e *settleEnv) assertNoForgeCalls() {
	e.t.Helper()
	if hc, cc := e.fake.calls(); hc+cc != 0 {
		e.t.Fatalf("forge was asked (BranchHead=%d CompareAncestry=%d), want no proof attempt", hc, cc)
	}
}

func goodBody() map[string]any { return settleBody(1, 2, settlePushed, settleSource, settleAdopted) }

// TestRecoverySettleReleasesByAncestryLiveDB: the positive path releases exactly the named
// predecessor hold with 'ancestry' evidence and all six audit columns, a repeat is an
// idempotent released without another forge proof, a repeat with a DIFFERENT identity is
// not_eligible, and both sibling holds stay open throughout.
func TestRecoverySettleReleasesByAncestryLiveDB(t *testing.T) {
	e := newSettleEnv(t)

	code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
	if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != settleHead ||
		res.RunID != e.run.String() || res.HoldID != e.pred.String() || res.Reason != "" {
		t.Fatalf("settle = %d %+v (%s), want 200 released with final head", code, res, raw)
	}
	h := e.hold(e.pred)
	want := holdRow{state: "released", evidence: "ancestry", pushed: settlePushed, source: settleSource,
		adopted: settleAdopted, finalHead: settleHead, bran: e.branch, successor: 2, liveWorkerNull: true, liveRunNull: true}
	if h != want {
		t.Fatalf("released hold = %+v\nwant %+v", h, want)
	}
	if hc, cc := e.fake.calls(); hc != 1 || cc != 3 {
		t.Fatalf("forge calls: BranchHead=%d CompareAncestry=%d, want 1 and 3 (one per candidate)", hc, cc)
	}
	e.assertOpen(e.sibGen, e.sibWork)

	// Repeat: idempotent released, from the stored identity, with NO new forge proof.
	code, res, raw = e.settle(e.tokenA, e.run, e.pred, goodBody())
	if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != settleHead {
		t.Fatalf("repeat settle = %d %+v (%s), want idempotent released", code, res, raw)
	}
	if hc, cc := e.fake.calls(); hc != 1 || cc != 3 {
		t.Fatalf("repeat made forge calls (BranchHead=%d CompareAncestry=%d); the ack must come from the stored identity", hc, cc)
	}

	// A repeat naming a DIFFERENT identity is not_eligible, and the stored audit is unchanged.
	code, res, raw = e.settle(e.tokenA, e.run, e.pred, settleBody(1, 2, settlePushed, settleOther, settleAdopted))
	e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
	if h2 := e.hold(e.pred); h2 != want {
		t.Fatalf("hold after a mismatched repeat = %+v, want unchanged %+v", h2, want)
	}
	e.assertOpen(e.sibGen, e.sibWork)
}

// TestRecoverySettleRejectsWorkerClaimsLiveDB: a fabricated worker claim never releases. An
// extra proof field is a strict-decode 400; malformed SHAs/generations are 400; a forge that
// reports diverged, unknown, unsupported, a missing branch, or an error leaves the hold open.
func TestRecoverySettleRejectsWorkerClaimsLiveDB(t *testing.T) {
	t.Run("extra ancestry field is 400", func(t *testing.T) {
		e := newSettleEnv(t)
		body := goodBody()
		body["ancestry"] = "ancestor"
		if code, _, raw := e.settle(e.tokenA, e.run, e.pred, body); code != http.StatusBadRequest {
			t.Fatalf("settle with a worker ancestry claim = %d (%s), want 400", code, raw)
		}
		if hc, cc := e.fake.calls(); hc+cc != 0 {
			t.Fatalf("a rejected body reached the forge (%d, %d)", hc, cc)
		}
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("malformed shas and generations are 400", func(t *testing.T) {
		e := newSettleEnv(t)
		for _, body := range []map[string]any{
			settleBody(1, 2, "main", settleSource, settleAdopted),
			settleBody(1, 2, settlePushed, strings.ToUpper(settleSource), settleAdopted),
			settleBody(1, 2, settlePushed, settleSource, settleAdopted[:39]),
			settleBody(0, 2, settlePushed, settleSource, settleAdopted),
			settleBody(1, -1, settlePushed, settleSource, settleAdopted),
			{"predecessor_generation": 1, "successor_generation": 2, "pushed_sha": settlePushed, "source_sha": settleSource},
		} {
			if code, _, raw := e.settle(e.tokenA, e.run, e.pred, body); code != http.StatusBadRequest {
				t.Fatalf("settle %v = %d (%s), want 400", body, code, raw)
			}
		}
		e.assertOpen(e.pred)
	})

	proofCases := []struct {
		name   string
		setup  func(f *settleFakeForge)
		reason string
	}{
		{"diverged candidate", func(f *settleFakeForge) {
			f.verdict = map[string]forge.Ancestry{settleAdopted: forge.AncestryNotAncestor}
		}, apitypes.RecoverySettleNotAncestor},
		{"not_ancestor wins over an earlier unknown", func(f *settleFakeForge) {
			f.verdict = map[string]forge.Ancestry{settlePushed: forge.AncestryUnknown, settleAdopted: forge.AncestryNotAncestor}
		}, apitypes.RecoverySettleNotAncestor},
		{"unknown verdict", func(f *settleFakeForge) {
			f.verdict = map[string]forge.Ancestry{settleSource: forge.AncestryUnknown}
		}, apitypes.RecoverySettleAncestryUnknown},
		{"ancestor verdict with an error is unknown", func(f *settleFakeForge) {
			f.verdictErr = map[string]error{settleSource: errors.New("transient")}
			f.verdict = map[string]forge.Ancestry{settleSource: forge.AncestryAncestor}
		}, apitypes.RecoverySettleAncestryUnknown},
		// A 404 on the branch read is its own terminal reason, not ancestry_unknown.
		{"missing branch head", func(f *settleFakeForge) { f.headErr = forge.ErrRefNotFound }, apitypes.RecoverySettleBranchMissing},
		{"branch head transport error", func(f *settleFakeForge) { f.headErr = errors.New("transient") }, apitypes.RecoverySettleAncestryUnknown},
		{"malformed branch head", func(f *settleFakeForge) { f.head = "not-a-sha" }, apitypes.RecoverySettleAncestryUnknown},
	}
	for _, tc := range proofCases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSettleEnv(t)
			tc.setup(e.fake)
			code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
			e.assertRetained(code, res, raw, tc.reason)
			e.assertOpen(e.pred, e.sibGen, e.sibWork)
		})
	}
}

// TestRecoverySettleRealGitHub429LiveDB drives the REAL GitHub driver (via a builder that
// points it at an httptest server): the branch head reads fine, every compare answers 429,
// so the hold is retained with ancestry_unknown.
func TestRecoverySettleRealGitHub429LiveDB(t *testing.T) {
	e := newSettleEnv(t)
	var compares int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/repositories/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "name": "settle", "owner": map[string]any{"login": "g"}})
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/g/settle/branches/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": e.branch, "commit": map[string]any{"sha": settleHead}})
		case strings.HasPrefix(r.URL.Path, "/api/v3/repos/g/settle/compare/"):
			mu.Lock()
			compares++
			mu.Unlock()
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ghDriver, err := forge.New(forge.TypeGitHub, srv.URL, "settle-test-secret-value-0001", 5*time.Second)
	if err != nil {
		t.Fatalf("forge.New: %v", err)
	}
	e.wsvc.SetForges(settleForgeBuilder{f: ghDriver})

	code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
	e.assertRetained(code, res, raw, apitypes.RecoverySettleAncestryUnknown)
	mu.Lock()
	defer mu.Unlock()
	if compares == 0 {
		t.Fatalf("the real driver never reached the compare endpoint")
	}
	e.assertOpen(e.pred, e.sibGen, e.sibWork)
}

// TestRecoverySettleStateChangedMidProofLiveDB: the run's completion identity changes between
// the proof and the guarded UPDATE (a hook in the fake forge mutates the run while the api is
// asking the forge). The UPDATE moves zero rows and the answer is state_changed — never
// released — with every hold still open.
func TestRecoverySettleStateChangedMidProofLiveDB(t *testing.T) {
	mutations := []struct {
		name string
		sql  string
	}{
		{"branch", `UPDATE runs SET branch = branch || '-moved' WHERE id = $1`},
		{"claim_generation", `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`},
		{"status_since", `UPDATE runs SET status_since = status_since + interval '1 second' WHERE id = $1`},
		{"status", `UPDATE runs SET status = 'failed' WHERE id = $1`},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			e := newSettleEnv(t)
			e.fake.beforeCompare = func() {
				if _, err := e.pool.Exec(e.ctx, m.sql, e.run); err != nil {
					t.Errorf("mutate run: %v", err)
				}
			}
			code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
			e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
			e.assertOpen(e.pred, e.sibGen, e.sibWork)
		})
	}
	t.Run("hold discarded mid-proof", func(t *testing.T) {
		e := newSettleEnv(t)
		e.fake.beforeCompare = func() {
			if _, err := e.pool.Exec(e.ctx, `UPDATE recovery_custody_holds SET state = 'discarded', release_evidence = 'owner_discard',
			      live_worker_id = NULL, live_run_id = NULL WHERE id = $1`, e.pred); err != nil {
				t.Errorf("discard: %v", err)
			}
		}
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
		if h := e.hold(e.pred); h.state != "discarded" || h.evidence != "owner_discard" {
			t.Fatalf("discarded hold was overwritten: %+v", h)
		}
		e.assertOpen(e.sibGen, e.sibWork)
	})
}

// TestRecoverySettleEligibilityLiveDB: every eligibility mismatch is retained/not_eligible (or
// a 404 for a run of another owner) and never asks the forge or touches a hold.
func TestRecoverySettleEligibilityLiveDB(t *testing.T) {
	t.Run("run not completed", func(t *testing.T) {
		e := newSettleEnv(t)
		e.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, e.run)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred)
	})
	t.Run("caller is not runs.worker_id", func(t *testing.T) {
		e := newSettleEnv(t)
		// Worker B (same owner) calls about ITS OWN gen-1 hold on A's completed run.
		code, res, raw := e.settle(e.tokenB, e.run, e.sibWork, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("foreign original worker", func(t *testing.T) {
		e := newSettleEnv(t)
		// Worker A (the run's completer) names worker B's gen-1 hold.
		code, res, raw := e.settle(e.tokenA, e.run, e.sibWork, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("predecessor not older than successor", func(t *testing.T) {
		e := newSettleEnv(t)
		code, res, raw := e.settle(e.tokenA, e.run, e.sibGen, settleBody(2, 2, settlePushed, settleSource, settleAdopted))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("request generation does not match the hold", func(t *testing.T) {
		e := newSettleEnv(t)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, settleBody(3, 4, settlePushed, settleSource, settleAdopted))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred)
	})
	t.Run("successor is not the run's claim generation", func(t *testing.T) {
		e := newSettleEnv(t)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, settleBody(1, 3, settlePushed, settleSource, settleAdopted))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred)
	})
	t.Run("hold id from another run", func(t *testing.T) {
		e := newSettleEnv(t)
		otherRun, _ := e.insertCompletedRun(e.workerA, 2)
		otherHold := e.insertHold(otherRun, 1, e.workerA)
		code, res, raw := e.settle(e.tokenA, e.run, otherHold, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, otherHold)
	})
	t.Run("run of another owner is 404", func(t *testing.T) {
		e := newSettleEnv(t)
		foreignUser, foreignWorker := uuid.New(), uuid.New()
		e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, foreignUser, fmt.Sprintf("settle-foreign-%s@e2e", foreignUser))
		tok := e.insertWorker(foreignUser, foreignWorker)
		if code, _, raw := e.settle(tok, e.run, e.pred, goodBody()); code != http.StatusNotFound {
			t.Fatalf("foreign-owner settle = %d (%s), want 404", code, raw)
		}
		e.assertNoForgeCalls()
		e.assertOpen(e.pred)
	})
	// Issue #1582 M1 rework: runs.branch is worker-reported, so a value that is not a git
	// branch name never reaches a forge URL.
	for _, bad := range []string{"../../../admin", "agent/x?per_page=1", "a b", "/agent", "agent//x", "agent/", "x.lock", "a@{0}", "a\x01b", "a~1", ".hidden"} {
		t.Run("invalid branch "+strings.ReplaceAll(bad, "/", "_"), func(t *testing.T) {
			e := newSettleEnv(t)
			e.exec(`UPDATE runs SET branch = $2 WHERE id = $1`, e.run, bad)
			code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
			e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
			e.assertNoForgeCalls()
			e.assertOpen(e.pred, e.sibGen, e.sibWork)
		})
	}
	t.Run("no bearer is 401", func(t *testing.T) {
		e := newSettleEnv(t)
		if code, _, raw := e.settle("uzw_not-a-real-token", e.run, e.pred, goodBody()); code != http.StatusUnauthorized {
			t.Fatalf("bad bearer settle = %d (%s), want 401", code, raw)
		}
		e.assertOpen(e.pred)
	})
}

// insertCapture registers a recovery capture under hold with the given source_sha, reserved
// an hour ago: BEFORE the successor generation's hold (created at env setup), so it binds.
func (e *settleEnv) insertCapture(hold uuid.UUID, source string) {
	e.t.Helper()
	e.insertCaptureAt(hold, source, -time.Hour)
}

// insertCaptureAt registers a recovery capture under hold with the given source_sha and
// created_at = now() + offset (a positive offset is a reservation after the successor claimed).
func (e *settleEnv) insertCaptureAt(hold uuid.UUID, source string, offset time.Duration) {
	e.t.Helper()
	e.exec(`INSERT INTO recovery_captures
	      (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state, created_at)
	      VALUES ($1, $2, $3, $4, $5, 'settle-cap', $6, $7, 'preparing', now() + make_interval(secs => $8))`,
		uuid.New(), hold, e.run, e.user, e.workerA, source, "settle-"+uuid.NewString(), offset.Seconds())
}

// makeInterlocked marks the run interlocked at contract revision rev.
func (e *settleEnv) makeInterlocked(rev int) {
	e.t.Helper()
	e.exec(`UPDATE runs SET completion_contract_version = 1, contract_revision = $2, completion_contract = '{}'::jsonb WHERE id = $1`, e.run, rev)
}

// insertConsumedPermit records a CONSUMED completion permit for the run.
func (e *settleEnv) insertConsumedPermit(rev int, head string, worker uuid.UUID, consumedAgo time.Duration) {
	e.t.Helper()
	e.exec(`INSERT INTO run_completion_permits (run_id, contract_revision, branch, head, issued_by_worker_id, consumed_at)
	      VALUES ($1, $2, $3, $4, $5, now() - $6::interval)`,
		e.run, rev, e.branch, head, worker, fmt.Sprintf("%d seconds", int(consumedAgo.Seconds())))
}

// TestRecoverySettleCandidateBindingLiveDB (issue #1582 M1 rework): the worker-chosen
// candidates are bound to the facts the server already holds. A source_sha no capture under the
// hold carries, or a pushed_sha that is not the head of the completion permit an interlocked
// run's completion consumed, is retained/candidate_mismatch with no forge call and the hold
// open; matching candidates release.
func TestRecoverySettleCandidateBindingLiveDB(t *testing.T) {
	t.Run("capture source mismatch", func(t *testing.T) {
		e := newSettleEnv(t)
		e.insertCapture(e.pred, settleOther)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("a sibling hold's capture does not bind this hold", func(t *testing.T) {
		e := newSettleEnv(t)
		e.insertCapture(e.sibGen, settleOther)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased {
			t.Fatalf("settle = %d %+v (%s), want released", code, res, raw)
		}
	})
	t.Run("capture source match releases", func(t *testing.T) {
		e := newSettleEnv(t)
		// Two captures with different sources: the request's source_sha must equal ONE of them.
		e.insertCapture(e.pred, settleOther)
		e.insertCapture(e.pred, settleSource)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != settleHead {
			t.Fatalf("settle = %d %+v (%s), want released", code, res, raw)
		}
		if h := e.hold(e.pred); h.state != "released" || h.source != settleSource {
			t.Fatalf("hold = %+v, want released with the capture's source", h)
		}
		e.assertOpen(e.sibGen, e.sibWork)
	})
	t.Run("capture registered mid-proof refuses the release", func(t *testing.T) {
		e := newSettleEnv(t)
		// The capture is backdated to before the successor hold, so it is a BINDING capture
		// the service never saw: only the guarded UPDATE's re-asserted binding can refuse it.
		e.fake.beforeCompare = func() {
			if _, err := e.pool.Exec(e.ctx, `INSERT INTO recovery_captures
			      (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state, created_at)
			      VALUES ($1, $2, $3, $4, $5, 'settle-cap', $6, 'mid-proof', 'preparing', now() - interval '1 hour')`,
				uuid.New(), e.pred, e.run, e.user, e.workerA, settleOther); err != nil {
				t.Errorf("insert capture: %v", err)
			}
		}
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("permit head mismatch", func(t *testing.T) {
		e := newSettleEnv(t)
		e.makeInterlocked(2)
		e.insertConsumedPermit(2, settleOther, e.workerA, 0)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("an invalidated older-revision permit does not bind", func(t *testing.T) {
		e := newSettleEnv(t)
		e.makeInterlocked(2)
		// Revision 1's permit (for the pushed candidate) was invalidated by a decision; the
		// completion consumed revision 2's permit at another head.
		e.insertConsumedPermit(1, settlePushed, e.workerA, 0)
		e.insertConsumedPermit(2, settleOther, e.workerA, 60*time.Second)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoForgeCalls()
	})
	t.Run("interlocked run without a consumed permit", func(t *testing.T) {
		e := newSettleEnv(t)
		e.makeInterlocked(2)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred)
	})
	t.Run("permit head match releases", func(t *testing.T) {
		e := newSettleEnv(t)
		e.makeInterlocked(2)
		e.insertConsumedPermit(2, settlePushed, e.workerA, 0)
		e.insertCapture(e.pred, settleSource)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != settleHead {
			t.Fatalf("settle = %d %+v (%s), want released", code, res, raw)
		}
		if h := e.hold(e.pred); h.state != "released" || h.evidence != "ancestry" || h.pushed != settlePushed {
			t.Fatalf("hold = %+v, want released by ancestry with the permit's head", h)
		}
		e.assertOpen(e.sibGen, e.sibWork)
	})
}

// TestRecoverySettleCaptureCutoffLiveDB (issue #1582 M1 follow-up, L-1): only captures created
// BEFORE the successor generation claimed bind source_sha: that generation's hold's created_at
// on the same run, else runs.claimed_at, else every capture binds. A capture reserved later
// (for example after completion, with a planted source) is ignored entirely, in BOTH the
// service read and the guarded UPDATE: it can neither satisfy a pre-existing binding nor, on
// its own, create one.
func TestRecoverySettleCaptureCutoffLiveDB(t *testing.T) {
	released := func(e *settleEnv, code int, res apitypes.RecoverySettleResponse, raw string) {
		e.t.Helper()
		if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != settleHead {
			e.t.Fatalf("settle = %d %+v (%s), want released", code, res, raw)
		}
		if h := e.hold(e.pred); h.state != "released" || h.evidence != "ancestry" || h.source != settleSource {
			e.t.Fatalf("hold = %+v, want released by ancestry with the request's source", h)
		}
	}
	t.Run("a planted post-successor capture does not satisfy a pre-existing binding", func(t *testing.T) {
		e := newSettleEnv(t)
		e.insertCapture(e.pred, settleOther)                 // the predecessor's real capture
		e.insertCaptureAt(e.pred, settleSource, time.Minute) // reserved after the successor claimed
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	t.Run("a post-successor capture alone is no binding capture", func(t *testing.T) {
		e := newSettleEnv(t)
		e.insertCaptureAt(e.pred, settleOther, time.Minute)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		released(e, code, res, raw)
		e.assertOpen(e.sibGen, e.sibWork)
	})
	t.Run("a post-successor capture registered mid-proof is ignored by the guarded update", func(t *testing.T) {
		e := newSettleEnv(t)
		e.fake.beforeCompare = func() {
			if _, err := e.pool.Exec(e.ctx, `INSERT INTO recovery_captures
			      (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state)
			      VALUES ($1, $2, $3, $4, $5, 'settle-cap', $6, 'mid-proof-late', 'preparing')`,
				uuid.New(), e.pred, e.run, e.user, e.workerA, settleOther); err != nil {
				t.Errorf("insert capture: %v", err)
			}
		}
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		released(e, code, res, raw)
		e.assertOpen(e.sibGen, e.sibWork)
	})
	t.Run("without a successor hold the cutoff is runs.claimed_at", func(t *testing.T) {
		e := newSettleEnv(t)
		e.exec(`DELETE FROM recovery_custody_holds WHERE id = $1`, e.sibGen)
		e.exec(`UPDATE runs SET claimed_at = now() - interval '30 minutes' WHERE id = $1`, e.run)
		e.insertCapture(e.pred, settleOther)                     // an hour ago: before the claim, binds
		e.insertCaptureAt(e.pred, settleSource, -10*time.Minute) // after the claim: ignored
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibWork)
	})
	t.Run("without a successor hold a post-claim capture alone does not bind", func(t *testing.T) {
		e := newSettleEnv(t)
		e.exec(`DELETE FROM recovery_custody_holds WHERE id = $1`, e.sibGen)
		e.exec(`UPDATE runs SET claimed_at = now() - interval '30 minutes' WHERE id = $1`, e.run)
		e.insertCaptureAt(e.pred, settleOther, -10*time.Minute)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		released(e, code, res, raw)
		e.assertOpen(e.sibWork)
	})
	t.Run("with neither a successor hold nor claimed_at every capture binds", func(t *testing.T) {
		e := newSettleEnv(t)
		e.exec(`DELETE FROM recovery_custody_holds WHERE id = $1`, e.sibGen)
		e.exec(`UPDATE runs SET claimed_at = NULL WHERE id = $1`, e.run)
		e.insertCaptureAt(e.pred, settleOther, time.Minute)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoForgeCalls()
		e.assertOpen(e.pred, e.sibWork)
	})
}

// TestRecoverySettleIsRateLimitedLiveDB (issue #1582 M1 rework): the settle route rides the
// per-worker limiter on the REAL worker router (WorkerRoutes, the same mount Routes uses), so
// a looping worker cannot spend the owner's forge quota without bound. With a budget of 2 the
// third call is a 429 that never reaches the service, while ANOTHER worker keeps its own
// budget.
func TestRecoverySettleIsRateLimitedLiveDB(t *testing.T) {
	e := newSettleEnv(t)
	e.fake.headErr = errors.New("transient") // each admitted call reaches the forge once
	e.router = e.routerWithWorkerLimiter(mw.NewLimiter(2, time.Hour, nil))
	for i := 1; i <= 2; i++ {
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleAncestryUnknown)
	}
	if code, _, raw := e.settle(e.tokenA, e.run, e.pred, goodBody()); code != http.StatusTooManyRequests {
		t.Fatalf("third settle = %d (%s), want 429", code, raw)
	}
	if hc, _ := e.fake.calls(); hc != 2 {
		t.Fatalf("BranchHead calls = %d, want 2 (the limited call must not reach the forge)", hc)
	}
	// Worker B has its own bucket: its call is admitted (and not_eligible on A's run).
	code, res, raw := e.settle(e.tokenB, e.run, e.sibWork, goodBody())
	e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
	e.assertOpen(e.pred, e.sibGen, e.sibWork)
}

func (e *settleEnv) routerWithWorkerLimiter(lim *mw.Limiter) http.Handler {
	h := &Handler{pool: e.pool, q: store.New(e.pool), box: newHandlerTestBox(e.t), cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}, wsvc: e.wsvc}
	return h.WorkerRoutes(lim)
}
