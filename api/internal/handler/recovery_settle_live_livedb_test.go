package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #1751 M2 live-DB proofs for POST /api/worker/runs/{id}/recovery-holds/{holdID}/settle-live:
// the api releases ONE older-generation custody hold while the same-worker SUCCESSOR generation
// is still LIVE, ONLY on its own forge proof against the target the successor published (its
// checkpoint ref or its run branch), through the REAL router, the REAL workersvc service and the
// REAL Postgres guarded UPDATE. It also pins the completed path's new idempotency for a live
// settle whose ACK was lost. Skipped unless UZI_TEST_DATABASE_URL is set.

// liveFakeForge extends settleFakeForge with RefHead (the checkpoint-ref read) and records
// every ref asked, so a test can prove the api read exactly refs/uzi-checkpoints/<derived>.
type liveFakeForge struct {
	*settleFakeForge
	mu       sync.Mutex
	refErr   error
	refs     []string
	branches []string
}

func (f *liveFakeForge) RefHead(_ context.Context, _ int64, ref string) (string, error) {
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	err := f.refErr
	f.mu.Unlock()
	f.settleFakeForge.mu.Lock()
	defer f.settleFakeForge.mu.Unlock()
	if err != nil {
		return "", err
	}
	return f.head, nil
}

func (f *liveFakeForge) BranchHead(ctx context.Context, projectID int64, branch string) (string, error) {
	f.mu.Lock()
	f.branches = append(f.branches, branch)
	f.mu.Unlock()
	return f.settleFakeForge.BranchHead(ctx, projectID, branch)
}

// total counts every forge call of any kind.
func (f *liveFakeForge) total() int {
	hc, cc := f.calls()
	f.mu.Lock()
	defer f.mu.Unlock()
	return hc + cc + len(f.refs)
}

func (f *liveFakeForge) asked() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refs...), append([]string(nil), f.branches...)
}

// liveEnv is a settleEnv whose run is RUNNING (claim generation 2, held by worker A, claim
// unreleased, runs.branch NULL like a live issue run), with a liveFakeForge injected.
type liveEnv struct {
	*settleEnv
	lf *liveFakeForge
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	e := newSettleEnv(t)
	e.exec(`UPDATE runs SET status = 'running', branch = NULL, claim_released_at = NULL, claimed_at = now() WHERE id = $1`, e.run)
	lf := &liveFakeForge{settleFakeForge: e.fake}
	e.wsvc.SetForges(settleForgeBuilder{f: lf})
	return &liveEnv{settleEnv: e, lf: lf}
}

// restart builds a fresh router + service over the same database and forge, standing in for
// an api process restart between two settle attempts.
func (e *liveEnv) restart() {
	e.t.Helper()
	q := store.New(e.pool)
	box := newHandlerTestBox(e.t)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetForges(settleForgeBuilder{f: e.lf})
	h := &Handler{pool: e.pool, q: q, box: box, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}, wsvc: wsvc}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim)
	e.wsvc = wsvc
}

func liveBody(pred, succ int64, published, source, adopted, target string) map[string]any {
	return map[string]any{
		"predecessor_generation": pred, "successor_generation": succ,
		"published_sha": published, "source_sha": source, "adopted_sha": adopted, "target": target,
	}
}

func goodLiveBody(target string) map[string]any {
	return liveBody(1, 2, settlePushed, settleSource, settleAdopted, target)
}

func (e *liveEnv) settleLive(token string, run, hold uuid.UUID, body any) (int, apitypes.RecoverySettleResponse, string) {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/recovery-holds/%s/settle-live", run, hold), bytes.NewReader(b))
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

func (e *liveEnv) assertReleased(code int, res apitypes.RecoverySettleResponse, raw string) {
	e.t.Helper()
	if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != settleHead ||
		res.RunID != e.run.String() || res.HoldID != e.pred.String() || res.Reason != "" {
		e.t.Fatalf("settle = %d %+v (%s), want 200 released with final head", code, res, raw)
	}
}

func (e *liveEnv) releaseTarget(id uuid.UUID) string {
	e.t.Helper()
	var target *string
	if err := e.pool.QueryRow(e.ctx, `SELECT release_target FROM recovery_custody_holds WHERE id = $1`, id).Scan(&target); err != nil {
		e.t.Fatalf("read release_target: %v", err)
	}
	if target == nil {
		return ""
	}
	return *target
}

func (e *liveEnv) runStatus() string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM runs WHERE id = $1`, e.run).Scan(&s); err != nil {
		e.t.Fatalf("read run status: %v", err)
	}
	return s
}

func (e *liveEnv) assertNoLiveForgeCalls() {
	e.t.Helper()
	if n := e.lf.total(); n != 0 {
		refs, branches := e.lf.asked()
		e.t.Fatalf("forge was asked %d time(s) (refs %v, branches %v), want no forge call", n, refs, branches)
	}
}

// wantLiveRow is the released row a live settle of goodLiveBody(target) stamps.
func (e *liveEnv) wantLiveRow() holdRow {
	return holdRow{state: "released", evidence: "live_ancestry", pushed: settlePushed, source: settleSource,
		adopted: settleAdopted, finalHead: settleHead, bran: e.branch, successor: 2, liveWorkerNull: true, liveRunNull: true}
}

// TestRecoveryLiveSettleCheckpointReleasesLiveDB: a RUNNING same-worker resume whose checkpoint
// ref head covers published/source/adopted releases the predecessor hold while the run stays
// running, stamping 'live_ancestry' with all audit columns, release_target='checkpoint' and the
// DERIVED checkpoint branch; the api read exactly refs/uzi-checkpoints/<derived branch>; a
// repeat is an idempotent released with no further forge call; siblings stay open.
func TestRecoveryLiveSettleCheckpointReleasesLiveDB(t *testing.T) {
	e := newLiveEnv(t)
	code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
	e.assertReleased(code, res, raw)
	if h := e.hold(e.pred); h != e.wantLiveRow() {
		t.Fatalf("released hold = %+v\nwant %+v", h, e.wantLiveRow())
	}
	if got := e.releaseTarget(e.pred); got != "checkpoint" {
		t.Fatalf("release_target = %q, want checkpoint", got)
	}
	if s := e.runStatus(); s != "running" {
		t.Fatalf("run status = %q, want running (the settle never touches the run)", s)
	}
	refs, branches := e.lf.asked()
	if len(refs) != 1 || refs[0] != "refs/uzi-checkpoints/"+e.branch || len(branches) != 0 {
		t.Fatalf("forge reads: refs %v branches %v, want exactly refs/uzi-checkpoints/%s", refs, branches, e.branch)
	}
	if _, cc := e.fake.calls(); cc != 3 {
		t.Fatalf("CompareAncestry calls = %d, want 3 (one per distinct candidate)", cc)
	}
	e.assertOpen(e.sibGen, e.sibWork)

	before := e.lf.total()
	code, res, raw = e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
	e.assertReleased(code, res, raw)
	if after := e.lf.total(); after != before {
		t.Fatalf("idempotent repeat made %d forge call(s), want 0", after-before)
	}
}

// TestRecoveryLiveSettleBranchReleasesLiveDB: a live run with a runs.branch set, target=branch,
// releases on the branch head (BranchHead, never RefHead) and stamps release_target='branch'.
func TestRecoveryLiveSettleBranchReleasesLiveDB(t *testing.T) {
	e := newLiveEnv(t)
	e.exec(`UPDATE runs SET branch = $2 WHERE id = $1`, e.run, e.branch)
	code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetBranch))
	e.assertReleased(code, res, raw)
	if h := e.hold(e.pred); h != e.wantLiveRow() {
		t.Fatalf("released hold = %+v\nwant %+v", h, e.wantLiveRow())
	}
	if got := e.releaseTarget(e.pred); got != "branch" {
		t.Fatalf("release_target = %q, want branch", got)
	}
	refs, branches := e.lf.asked()
	if len(refs) != 0 || len(branches) != 1 || branches[0] != e.branch {
		t.Fatalf("forge reads: refs %v branches %v, want exactly branch %s", refs, branches, e.branch)
	}
	e.assertOpen(e.sibGen, e.sibWork)
}

// TestRecoveryLiveSettleProofFailuresLiveDB: a candidate the target head does not contain is
// retained/not_ancestor with the hold open; a forge error is ancestry_unknown, and a retry once
// the forge is healthy releases.
func TestRecoveryLiveSettleProofFailuresLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, candidate string }{
		{"divergent source", settleSource},
		{"adopted not in the target head", settleAdopted},
		{"published not on the target", settlePushed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLiveEnv(t)
			e.fake.verdict = map[string]forge.Ancestry{tc.candidate: forge.AncestryNotAncestor}
			code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
			e.assertRetained(code, res, raw, apitypes.RecoverySettleNotAncestor)
			e.assertOpen(e.pred, e.sibGen, e.sibWork)
		})
	}
	t.Run("compare error then healthy retry", func(t *testing.T) {
		e := newLiveEnv(t)
		e.fake.verdictErr = map[string]error{settleSource: errors.New("forge 502")}
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleAncestryUnknown)
		e.assertOpen(e.pred)
		e.fake.mu.Lock()
		e.fake.verdictErr = nil
		e.fake.mu.Unlock()
		code, res, raw = e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertReleased(code, res, raw)
	})
	t.Run("checkpoint ref missing is branch_missing", func(t *testing.T) {
		e := newLiveEnv(t)
		e.lf.refErr = forge.ErrRefNotFound
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleBranchMissing)
		e.assertOpen(e.pred)
	})
	t.Run("checkpoint ref read error is ancestry_unknown", func(t *testing.T) {
		e := newLiveEnv(t)
		e.lf.refErr = errors.New("forge timeout")
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleAncestryUnknown)
		e.assertOpen(e.pred)
	})
	// An api restart between an unfinished proof and the write: the first attempt read the
	// head and failed mid-proof, nothing was written; a fresh process retries and releases.
	t.Run("restart between proof and write then retry", func(t *testing.T) {
		e := newLiveEnv(t)
		e.fake.verdictErr = map[string]error{settleAdopted: context.DeadlineExceeded}
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleAncestryUnknown)
		e.assertOpen(e.pred)
		e.fake.mu.Lock()
		e.fake.verdictErr = nil
		e.fake.mu.Unlock()
		e.restart()
		code, res, raw = e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertReleased(code, res, raw)
		if h := e.hold(e.pred); h != e.wantLiveRow() {
			t.Fatalf("released hold = %+v\nwant %+v", h, e.wantLiveRow())
		}
	})
}

// TestRecoveryLiveSettleFencedMidProofLiveDB: the run or hold changes between the proof and the
// guarded UPDATE (the fake forge's beforeCompare hook). Every change moves zero rows and the
// answer is state_changed, never released, with the hold still open. Each case is the proof
// that one re-asserted predicate of ReleasePredecessorCustodyHoldByLiveAncestry is load-bearing.
func TestRecoveryLiveSettleFencedMidProofLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		target string
		branch bool // seed runs.branch = e.branch first (a branch-target run)
		sql    string
	}{
		{"stale generation", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`},
		{"cancelled", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET status = 'cancelled' WHERE id = $1`},
		{"requeued", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET status = 'queued' WHERE id = $1`},
		{"claim released", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET claim_released_at = now() WHERE id = $1`},
		{"worker changed", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET worker_id = (SELECT h.original_worker_id FROM recovery_custody_holds h WHERE h.run_id = runs.id AND h.original_worker_id <> runs.worker_id LIMIT 1) WHERE id = $1`},
		{"checkpoint: branch appeared", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET branch = 'agent/elsewhere' WHERE id = $1`},
		{"checkpoint: issue iid changed", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET issue_iid = issue_iid + 1000 WHERE id = $1`},
		{"checkpoint: kind changed", apitypes.RecoverySettleTargetCheckpoint, false, `UPDATE runs SET kind = 'self_improve' WHERE id = $1`},
		{"branch: branch changed", apitypes.RecoverySettleTargetBranch, true, `UPDATE runs SET branch = branch || '-moved' WHERE id = $1`},
		{"branch: branch cleared", apitypes.RecoverySettleTargetBranch, true, `UPDATE runs SET branch = NULL WHERE id = $1`},
	}
	for _, m := range cases {
		t.Run(m.name, func(t *testing.T) {
			e := newLiveEnv(t)
			if m.branch {
				e.exec(`UPDATE runs SET branch = $2 WHERE id = $1`, e.run, e.branch)
			}
			e.fake.beforeCompare = func() {
				if _, err := e.pool.Exec(e.ctx, m.sql, e.run); err != nil {
					t.Errorf("mutate run: %v", err)
				}
			}
			code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(m.target))
			e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
			e.assertOpen(e.pred, e.sibGen, e.sibWork)
		})
	}
	t.Run("hold discarded mid-proof", func(t *testing.T) {
		e := newLiveEnv(t)
		e.fake.beforeCompare = func() {
			if _, err := e.pool.Exec(e.ctx, `UPDATE recovery_custody_holds SET state = 'discarded', release_evidence = 'owner_discard',
			      live_worker_id = NULL, live_run_id = NULL WHERE id = $1`, e.pred); err != nil {
				t.Errorf("discard: %v", err)
			}
		}
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
		if h := e.hold(e.pred); h.state != "discarded" || h.evidence != "owner_discard" {
			t.Fatalf("discarded hold was overwritten: %+v", h)
		}
	})
	// The successor generation's own open hold on the same worker is the durability backstop
	// (settle_live.go): without it, releasing the predecessor could drop the only custody of
	// checkpoint-only commits. Each way it can be missing retains.
	for _, sc := range []struct {
		name string
		sql  string
	}{
		{"successor hold discarded mid-proof", `UPDATE recovery_custody_holds SET state = 'discarded', release_evidence = 'owner_discard',
		      live_worker_id = NULL, live_run_id = NULL WHERE id = $1`},
		{"successor hold deleted mid-proof", `DELETE FROM recovery_custody_holds WHERE id = $1`},
		{"successor hold taken by another worker", `UPDATE recovery_custody_holds SET original_worker_id =
		      (SELECT o.original_worker_id FROM recovery_custody_holds o WHERE o.run_id = recovery_custody_holds.run_id
		         AND o.original_worker_id <> recovery_custody_holds.original_worker_id LIMIT 1) WHERE id = $1`},
	} {
		t.Run(sc.name, func(t *testing.T) {
			e := newLiveEnv(t)
			e.fake.beforeCompare = func() {
				if _, err := e.pool.Exec(e.ctx, sc.sql, e.sibGen); err != nil {
					t.Errorf("mutate successor hold: %v", err)
				}
			}
			code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
			e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
			e.assertOpen(e.pred)
		})
	}
	t.Run("no successor hold at all", func(t *testing.T) {
		e := newLiveEnv(t)
		e.exec(`DELETE FROM recovery_custody_holds WHERE id = $1`, e.sibGen)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		if code == 200 && res.Outcome == apitypes.RecoverySettleReleased {
			t.Fatalf("released without a successor hold: %s", raw)
		}
		e.assertOpen(e.pred)
	})
	t.Run("capture planted mid-proof with another source", func(t *testing.T) {
		e := newLiveEnv(t)
		e.fake.beforeCompare = func() { e.insertCapture(e.pred, settleOther) }
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleStateChanged)
		e.assertOpen(e.pred)
	})
}

// TestRecoveryLiveSettleEligibilityLiveDB: every live-eligibility mismatch is retained/
// not_eligible (or a 404 / 400) with no forge call and every hold untouched.
func TestRecoveryLiveSettleEligibilityLiveDB(t *testing.T) {
	cp := apitypes.RecoverySettleTargetCheckpoint
	t.Run("cross-worker hold", func(t *testing.T) {
		e := newLiveEnv(t)
		// Worker A (the live run's holder) names worker B's gen-1 hold.
		code, res, raw := e.settleLive(e.tokenA, e.run, e.sibWork, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		// Worker B names its own gen-1 hold on A's live run.
		code, res, raw = e.settleLive(e.tokenB, e.run, e.sibWork, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoLiveForgeCalls()
		e.assertOpen(e.pred, e.sibGen, e.sibWork)
	})
	for _, st := range []string{"completed", "queued", "failed", "cancelled", "paused"} {
		t.Run("run "+st+" with the hold open", func(t *testing.T) {
			e := newLiveEnv(t)
			e.exec(`UPDATE runs SET status = $2 WHERE id = $1`, e.run, st)
			code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
			e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
			e.assertNoLiveForgeCalls()
			e.assertOpen(e.pred)
		})
	}
	for _, st := range []string{"claimed", "awaiting_approval", "awaiting_input"} {
		t.Run("run "+st+" is live", func(t *testing.T) {
			e := newLiveEnv(t)
			e.exec(`UPDATE runs SET status = $2 WHERE id = $1`, e.run, st)
			code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
			e.assertReleased(code, res, raw)
		})
	}
	t.Run("claim released", func(t *testing.T) {
		e := newLiveEnv(t)
		e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.run)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoLiveForgeCalls()
		e.assertOpen(e.pred)
	})
	for _, b := range []struct {
		name string
		body map[string]any
		hold func(*liveEnv) uuid.UUID
	}{
		{"successor is not the run's claim generation", liveBody(1, 3, settlePushed, settleSource, settleAdopted, cp), func(e *liveEnv) uuid.UUID { return e.pred }},
		{"predecessor does not match the hold", liveBody(2, 3, settlePushed, settleSource, settleAdopted, cp), func(e *liveEnv) uuid.UUID { return e.pred }},
		{"predecessor not older than successor", liveBody(2, 2, settlePushed, settleSource, settleAdopted, cp), func(e *liveEnv) uuid.UUID { return e.sibGen }},
	} {
		t.Run(b.name, func(t *testing.T) {
			e := newLiveEnv(t)
			code, res, raw := e.settleLive(e.tokenA, e.run, b.hold(e), b.body)
			e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
			e.assertNoLiveForgeCalls()
			e.assertOpen(e.pred, e.sibGen, e.sibWork)
		})
	}
	t.Run("branch target without a run branch", func(t *testing.T) {
		e := newLiveEnv(t)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetBranch))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoLiveForgeCalls()
		e.assertOpen(e.pred)
	})
	t.Run("checkpoint target on a kind with no checkpoint", func(t *testing.T) {
		e := newLiveEnv(t)
		e.exec(`UPDATE runs SET kind = 'prompt', issue_iid = NULL WHERE id = $1`, e.run)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoLiveForgeCalls()
		e.assertOpen(e.pred)
	})
	// A task run carries its branch from creation and no checkpoint: the checkpoint target is
	// not eligible, the branch target proves against runs.branch and releases.
	t.Run("task run: checkpoint not eligible, branch releases", func(t *testing.T) {
		e := newLiveEnv(t)
		e.branch = "uzi/task/" + e.run.String()
		e.exec(`UPDATE runs SET kind = 'task', issue_iid = NULL, branch = $2 WHERE id = $1`, e.run, e.branch)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		e.assertNoLiveForgeCalls()
		code, res, raw = e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetBranch))
		e.assertReleased(code, res, raw)
		if h := e.hold(e.pred); h != e.wantLiveRow() {
			t.Fatalf("released hold = %+v\nwant %+v", h, e.wantLiveRow())
		}
	})
	t.Run("capture source mismatch", func(t *testing.T) {
		e := newLiveEnv(t)
		e.insertCapture(e.pred, settleOther)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleCandidateMismatch)
		e.assertNoLiveForgeCalls()
		e.assertOpen(e.pred)
	})
	t.Run("run of another owner is 404", func(t *testing.T) {
		e := newLiveEnv(t)
		foreignUser, foreignWorker := uuid.New(), uuid.New()
		e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, foreignUser, fmt.Sprintf("settle-live-foreign-%s@e2e", foreignUser))
		tok := e.insertWorker(foreignUser, foreignWorker)
		if code, _, raw := e.settleLive(tok, e.run, e.pred, goodLiveBody(cp)); code != http.StatusNotFound {
			t.Fatalf("foreign-owner settle = %d (%s), want 404", code, raw)
		}
		e.assertNoLiveForgeCalls()
		e.assertOpen(e.pred)
	})
	for _, bad := range []struct {
		name string
		body any
	}{
		{"unknown target", liveBody(1, 2, settlePushed, settleSource, settleAdopted, "tag")},
		{"missing target", settleBody(1, 2, settlePushed, settleSource, settleAdopted)},
		{"malformed sha", liveBody(1, 2, "ABC", settleSource, settleAdopted, cp)},
		{"zero generation", liveBody(0, 2, settlePushed, settleSource, settleAdopted, cp)},
		{"extra verdict field", map[string]any{
			"predecessor_generation": 1, "successor_generation": 2, "published_sha": settlePushed,
			"source_sha": settleSource, "adopted_sha": settleAdopted, "target": cp, "ancestry": "ancestor",
		}},
	} {
		t.Run("400 "+bad.name, func(t *testing.T) {
			e := newLiveEnv(t)
			if code, _, raw := e.settleLive(e.tokenA, e.run, e.pred, bad.body); code != http.StatusBadRequest {
				t.Fatalf("settle = %d (%s), want 400", code, raw)
			}
			e.assertNoLiveForgeCalls()
			e.assertOpen(e.pred)
		})
	}
}

// TestRecoveryLiveSettleLostAckLiveDB (issue #1751 M2 R1): a live settle released the hold but
// its ACK never reached the worker. Whatever the run did since (completed, reclaimed at a newer
// generation), a retry with the SAME identity answers released from the stored row with ZERO
// forge calls, on the live endpoint and (without pushed_sha, which the completed request
// carries as the final head) on the completed endpoint; any other identity is not_eligible.
func TestRecoveryLiveSettleLostAckLiveDB(t *testing.T) {
	cp := apitypes.RecoverySettleTargetCheckpoint
	// settled returns a liveEnv whose predecessor hold was released by a live settle, with the
	// forge call counters observed after it.
	settled := func(t *testing.T) (*liveEnv, int) {
		t.Helper()
		e := newLiveEnv(t)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertReleased(code, res, raw)
		return e, e.lf.total()
	}
	complete := func(e *liveEnv) {
		e.exec(`UPDATE runs SET status = 'completed', branch = $2, status_since = now(), claim_released_at = now() WHERE id = $1`, e.run, e.branch)
	}
	assertNoNewCalls := func(t *testing.T, e *liveEnv, before int) {
		t.Helper()
		if n := e.lf.total(); n != before {
			t.Fatalf("retry made %d forge call(s), want 0", n-before)
		}
	}

	t.Run("run completed then live retry", func(t *testing.T) {
		e, before := settled(t)
		complete(e)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertReleased(code, res, raw)
		assertNoNewCalls(t, e, before)
		if h := e.hold(e.pred); h != e.wantLiveRow() {
			t.Fatalf("hold rewritten: %+v", h)
		}
	})
	t.Run("run reclaimed at a newer generation then live retry", func(t *testing.T) {
		e, before := settled(t)
		e.exec(`UPDATE runs SET claim_generation = claim_generation + 1, status = 'awaiting_approval' WHERE id = $1`, e.run)
		code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertReleased(code, res, raw)
		assertNoNewCalls(t, e, before)
	})
	t.Run("run completed then completed-path retry with another pushed head", func(t *testing.T) {
		e, before := settled(t)
		complete(e)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, settleBody(1, 2, settleOther, settleSource, settleAdopted))
		e.assertReleased(code, res, raw)
		assertNoNewCalls(t, e, before)
		if h := e.hold(e.pred); h != e.wantLiveRow() {
			t.Fatalf("hold rewritten: %+v", h)
		}
	})
	for _, m := range []struct {
		name string
		body map[string]any
	}{
		{"other source", liveBody(1, 2, settlePushed, settleOther, settleAdopted, cp)},
		{"other adopted", liveBody(1, 2, settlePushed, settleSource, settleOther, cp)},
		{"other published", liveBody(1, 2, settleOther, settleSource, settleAdopted, cp)},
		{"other successor", liveBody(1, 3, settlePushed, settleSource, settleAdopted, cp)},
		{"other target", liveBody(1, 2, settlePushed, settleSource, settleAdopted, apitypes.RecoverySettleTargetBranch)},
	} {
		t.Run("live endpoint mismatched identity: "+m.name, func(t *testing.T) {
			e, before := settled(t)
			code, res, raw := e.settleLive(e.tokenA, e.run, e.pred, m.body)
			e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
			assertNoNewCalls(t, e, before)
		})
	}
	for _, m := range []struct {
		name string
		body map[string]any
	}{
		{"other source", settleBody(1, 2, settlePushed, settleOther, settleAdopted)},
		{"other adopted", settleBody(1, 2, settlePushed, settleSource, settleOther)},
		{"other successor", settleBody(1, 3, settlePushed, settleSource, settleAdopted)},
	} {
		t.Run("completed endpoint mismatched identity: "+m.name, func(t *testing.T) {
			e, before := settled(t)
			complete(e)
			code, res, raw := e.settle(e.tokenA, e.run, e.pred, m.body)
			e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
			assertNoNewCalls(t, e, before)
		})
	}
	t.Run("another worker retrying is not_eligible", func(t *testing.T) {
		e, before := settled(t)
		code, res, raw := e.settleLive(e.tokenB, e.run, e.pred, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		complete(e)
		code, res, raw = e.settle(e.tokenB, e.run, e.pred, goodBody())
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
		assertNoNewCalls(t, e, before)
	})
	// An 'ancestry' (completed-path) release is never acknowledged by the live endpoint.
	t.Run("completed-path release is not a live acknowledgement", func(t *testing.T) {
		e := newLiveEnv(t)
		complete(e)
		code, res, raw := e.settle(e.tokenA, e.run, e.pred, goodBody())
		if code != http.StatusOK || res.Outcome != apitypes.RecoverySettleReleased {
			t.Fatalf("completed settle = %d %+v (%s), want released", code, res, raw)
		}
		code, res, raw = e.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(cp))
		e.assertRetained(code, res, raw, apitypes.RecoverySettleNotEligible)
	})
}

// TestRecoveryLiveAncestryChecksLiveDB: migration 00255's CHECKs refuse a 'live_ancestry' row
// missing release_target (or any 00251 audit column), and a release_target on a row of any
// other evidence class, at the database, not just in code.
func TestRecoveryLiveAncestryChecksLiveDB(t *testing.T) {
	e := newLiveEnv(t)
	set := func(sql string, args ...any) error {
		_, err := e.pool.Exec(e.ctx, sql, args...)
		return err
	}
	full := `UPDATE recovery_custody_holds SET state = 'released', live_worker_id = NULL, live_run_id = NULL,
	    release_evidence = $2, release_pushed_sha = $3, release_source_sha = $3, release_adopted_sha = $3,
	    release_final_head_sha = $3, release_successor_generation = 2, release_branch = 'agent/x', release_target = $4
	  WHERE id = $1`
	for _, tc := range []struct {
		name     string
		evidence string
		target   any
		wantOK   bool
	}{
		{"live_ancestry without release_target", "live_ancestry", nil, false},
		{"live_ancestry with an unknown target", "live_ancestry", "tag", false},
		{"ancestry with a release_target", "ancestry", "checkpoint", false},
		{"publication with a release_target", "publication", "branch", false},
		{"live_ancestry complete", "live_ancestry", "checkpoint", true},
		{"ancestry without release_target", "ancestry", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold := e.insertHold(e.run, 1, e.workerA)
			err := set(full, hold, tc.evidence, settleHead, tc.target)
			if tc.wantOK && err != nil {
				t.Fatalf("update = %v, want accepted", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("update accepted, want a CHECK violation")
			}
		})
	}
	t.Run("release_target with NULL evidence", func(t *testing.T) {
		// A plain `release_evidence = 'live_ancestry'` is NULL here, which a CHECK accepts.
		hold := e.insertHold(e.run, 1, e.workerA)
		if err := set(`UPDATE recovery_custody_holds SET release_target = 'checkpoint' WHERE id = $1`, hold); err == nil {
			t.Fatalf("release_target on an evidence-free row accepted, want a CHECK violation")
		}
	})
	t.Run("live_ancestry missing an 00251 audit column", func(t *testing.T) {
		hold := e.insertHold(e.run, 1, e.workerA)
		err := set(`UPDATE recovery_custody_holds SET state = 'released', live_worker_id = NULL, live_run_id = NULL,
		    release_evidence = 'live_ancestry', release_target = 'branch', release_pushed_sha = $2 WHERE id = $1`, hold, settleHead)
		if err == nil {
			t.Fatalf("partial live_ancestry row accepted, want a CHECK violation")
		}
	})
}
