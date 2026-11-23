package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.
//
// A run-environment ciphertext is opened at the vault epoch read in the same statement. These
// tests read the rows while the owner's vault is unprotected, then (between the read and the
// open) the owner enables protection and locks: an epoch-less master_aad open would still
// succeed with the server key and deliver the plaintext; the epoch-fenced open must not.

// epochRaceQueries runs after() once, right after the named list read returns its rows.
type epochRaceQueries struct {
	*store.Queries
	trigger string // "effective", "redact" or "receipts"
	once    sync.Once
	after   func()
	reads   int
	byName  map[string]int
}

func (q *epochRaceQueries) fire(name string) {
	q.reads++
	if q.byName == nil {
		q.byName = map[string]int{}
	}
	q.byName[name]++
	if name == q.trigger {
		q.once.Do(q.after)
	}
}

func (q *epochRaceQueries) ListRunEnvEffectiveForRun(ctx context.Context, id uuid.UUID) ([]store.ListRunEnvEffectiveForRunRow, error) {
	rows, err := q.Queries.ListRunEnvEffectiveForRun(ctx, id)
	q.fire("effective")
	return rows, err
}

func (q *epochRaceQueries) ListRunEnvRedactForRun(ctx context.Context, id uuid.UUID) ([]store.ListRunEnvRedactForRunRow, error) {
	rows, err := q.Queries.ListRunEnvRedactForRun(ctx, id)
	q.fire("redact")
	return rows, err
}

func (q *epochRaceQueries) ListRunEnvSecretReceiptVersionsForRun(ctx context.Context, id uuid.UUID) ([]store.ListRunEnvSecretReceiptVersionsForRunRow, error) {
	rows, err := q.Queries.ListRunEnvSecretReceiptVersionsForRun(ctx, id)
	q.fire("receipts")
	return rows, err
}

// armProtection gives the owner a password and an optional vault policy, so Enable can succeed.
func armProtection(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	hash, err := auth.HashPassword(codexRacePassword)
	if err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE users SET password_hash=$2 WHERE id=$1", userID, hash)
	optionalWorkerVaultPolicy(t, env)
}

// enableAndLock is the owner's race: protection is enabled (epoch bump, rows rewrapped) and the
// vault locked again.
func (fx *reFix) enableAndLock(t *testing.T) func() {
	return func() {
		if err := fx.env.vlt.Enable(fx.env.ctx, fx.userID, codexRacePassword); err != nil {
			t.Errorf("enable protection: %v", err)
		}
		fx.env.vlt.Lock(fx.userID)
	}
}

func payloadHasValue(p *ClaimPayload, value string) bool {
	for _, v := range p.Secrets.RunEnv {
		if strings.Contains(v.Value, value) {
			return true
		}
	}
	for _, v := range p.Secrets.RunEnvRedact {
		if strings.Contains(v.Value, value) {
			return true
		}
	}
	return false
}

// Lead claim: the effective list and the redact list are each read unprotected, then the owner
// protects and locks before the open. The run goes back to queued; no value is delivered.
func TestRunEnvClaimStaleEpochDeliversNothingLiveDB(t *testing.T) {
	for _, trigger := range []string{"effective", "redact"} {
		t.Run(trigger, func(t *testing.T) {
			fx := newREFix(t, true)
			armProtection(t, fx.env, fx.userID)
			runID := fx.queuedRun(t, "issue")
			secret := envSecretValue(trigger)
			fx.put(t, "API_KEY", true, secret)
			if trigger == "redact" {
				// Deliver once, then retire the version: the next claim holds it only as a
				// redact-only receipt.
				if p := fx.claim(t); p == nil || runEnvByName(p)["API_KEY"].Value != secret {
					t.Fatalf("first claim = %+v", p)
				}
				fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE user_id=$1 AND name='API_KEY'`, fx.userID)
				fx.requeue(runID)
			}
			q := &epochRaceQueries{Queries: fx.env.q, trigger: trigger, after: fx.enableAndLock(t)}
			fx.svc.q = q
			p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if p != nil && payloadHasValue(p, secret) {
				t.Fatalf("claim delivered the plaintext after the owner protected and locked: %+v", p.Secrets)
			}
			if p != nil {
				t.Fatalf("claim = %+v, want an idle (requeued) claim", p.Secrets)
			}
			if r := mustRun(t, fx.env, runID); r.Status != "queued" || r.HoldReason.Valid || r.FailOrigin.Valid {
				t.Fatalf("status=%s hold=%v fail=%v, want queued with nothing parked or failed", r.Status, r.HoldReason, r.FailOrigin)
			}
			if q.byName[trigger] != 2 { // the stale seal was re-read once
				t.Fatalf("%s list reads = %d, want 2 (the read and one re-read)", trigger, q.byName[trigger])
			}
		})
	}
}

// A stale seal whose refetch opens: the owner enables protection (and stays unlocked) between the
// read and the open. The re-read row is sealed at the new epoch and opens, so the value is
// delivered and the claim finishes against the new epoch.
func TestRunEnvClaimStaleEpochRefetchOpensLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	armProtection(t, fx.env, fx.userID)
	runID := fx.queuedRun(t, "issue")
	secret := envSecretValue("refetch")
	fx.put(t, "API_KEY", true, secret)
	q := &epochRaceQueries{Queries: fx.env.q, trigger: "effective", after: func() {
		if err := fx.env.vlt.Enable(fx.env.ctx, fx.userID, codexRacePassword); err != nil {
			t.Errorf("enable protection: %v", err)
		}
	}}
	fx.svc.q = q
	p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if p == nil || runEnvByName(p)["API_KEY"].Value != secret {
		t.Fatalf("claim = %+v, want the refetched row opened and delivered", p)
	}
	if q.byName["effective"] != 2 {
		t.Fatalf("effective list reads = %d, want 2", q.byName["effective"])
	}
	if r := mustRun(t, fx.env, runID); r.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", r.Status)
	}
}

// The window after the open: the values opened at epoch E, then the owner enables protection
// (epoch E+1, every key unchanged) and locks before the claim commits. The finish compares the
// epoch it reads under the users-row lock with the one the values were opened at: nothing is
// delivered and the run goes back to queued.
func TestRunEnvClaimEpochChangeAfterOpenDeliversNothingLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	armProtection(t, fx.env, fx.userID)
	runID := fx.queuedRun(t, "issue")
	secret := envSecretValue("window")
	fx.put(t, "API_KEY", true, secret)
	var assembled *ClaimPayload
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil || runEnvByName(p)["API_KEY"].Value != secret {
			t.Errorf("assembly did not open the value: payload=%v err=%v", p != nil, err)
		}
		assembled = p
		fx.enableAndLock(t)()
	}}
	p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if assembled == nil {
		t.Fatal("assembly hook never ran")
	}
	if p != nil {
		t.Fatalf("claim = %+v, want an idle (requeued) claim", p.Secrets)
	}
	if r := mustRun(t, fx.env, runID); r.Status != "queued" || r.HoldReason.Valid || r.FailOrigin.Valid {
		t.Fatalf("status=%s hold=%v fail=%v, want queued with nothing parked or failed", r.Status, r.HoldReason, r.FailOrigin)
	}
	if got := fx.receipts(t, runID); len(got) != 0 {
		t.Fatalf("receipts = %+v, want none", got)
	}
}

// Cross-check child: the lead's receipted versions are read unprotected; the owner protects and
// locks before the open. The checker is not claimed with a value.
func TestRunEnvCrossCheckStaleEpochDeliversNothingLiveDB(t *testing.T) {
	f, w := admissionCheckerBase(t, "plan")
	f, _ = admissionCheckerSubmit(t, f, w, "plan", 0, nil)
	crossCheckEnvCapable(f, true)
	armProtection(t, f.env, f.userID)
	secret := envSecretValue("lead")
	seedLeadSecret(t, f, "API_KEY", secret)
	q := &epochRaceQueries{Queries: f.env.q, trigger: "receipts", after: func() {
		if err := f.env.vlt.Enable(f.env.ctx, f.userID, codexRacePassword); err != nil {
			t.Errorf("enable protection: %v", err)
		}
		f.env.vlt.Lock(f.userID)
	}}
	f.svc.q = q
	p := laneClaim(t, f, "cross_check", nil)
	if p != nil {
		t.Fatalf("checker claimed with redact=%+v run_env=%+v after the owner protected and locked", p.Secrets.RunEnvRedact, p.Secrets.RunEnv)
	}
	// The receipts were re-read once, and the check is still open: the child is queued with no
	// failure origin (a requeue), not decided checker_unavailable.
	if q.byName["receipts"] != 2 {
		t.Fatalf("receipt reads = %d, want 2 (the read and one re-read)", q.byName["receipts"])
	}
	child := mustRun(t, f.env, f.runID)
	if child.Status != "queued" || child.FailOrigin.Valid || child.HoldReason.Valid {
		t.Fatalf("child status=%s fail=%v hold=%v, want queued with no failure origin", child.Status, child.FailOrigin, child.HoldReason)
	}
	var verdict, outcome, reason string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(verdict,''), COALESCE(outcome,''), COALESCE(reason_class,'')
	    FROM cross_checks WHERE checker_run_id=$1`, f.runID).Scan(&verdict, &outcome, &reason); err != nil {
		t.Fatal(err)
	}
	if verdict == "failed" || outcome == "failed" || reason == "checker_unavailable" {
		t.Fatalf("check decided: verdict %q outcome %q reason %q, want it still open", verdict, outcome, reason)
	}
}

// The window after the open, cross-check lane: the child's redact versions are opened at epoch E,
// then the owner enables protection (epoch E+1) and locks before the claim commits. The finish
// fences the epoch like the lead lane: nothing is delivered and the check is not decided.
func TestRunEnvCrossCheckEpochChangeAfterOpenDeliversNothingLiveDB(t *testing.T) {
	f, w := admissionCheckerBase(t, "plan")
	f, _ = admissionCheckerSubmit(t, f, w, "plan", 0, nil)
	crossCheckEnvCapable(f, true)
	armProtection(t, f.env, f.userID)
	secret := envSecretValue("child-window")
	seedLeadSecret(t, f, "API_KEY", secret)
	assembled := false
	f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil || !payloadHasValue(p, secret) {
			t.Errorf("assembly did not open the redact value: payload=%v err=%v", p != nil, err)
		}
		assembled = true
		if err := f.env.vlt.Enable(f.env.ctx, f.userID, codexRacePassword); err != nil {
			t.Errorf("enable protection: %v", err)
		}
		f.env.vlt.Lock(f.userID)
	}}
	p := laneClaim(t, f, "cross_check", nil)
	if !assembled {
		t.Fatal("assembly hook never ran")
	}
	if p != nil {
		t.Fatalf("checker claimed with redact=%+v after the epoch changed before the commit", p.Secrets.RunEnvRedact)
	}
	child := mustRun(t, f.env, f.runID)
	if child.Status != "queued" || child.FailOrigin.Valid || child.HoldReason.Valid {
		t.Fatalf("child status=%s fail=%v hold=%v, want queued with no failure origin", child.Status, child.FailOrigin, child.HoldReason)
	}
	var verdict, outcome, reason string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(verdict,''), COALESCE(outcome,''), COALESCE(reason_class,'')
	    FROM cross_checks WHERE checker_run_id=$1`, f.runID).Scan(&verdict, &outcome, &reason); err != nil {
		t.Fatal(err)
	}
	if verdict == "failed" || outcome == "failed" || reason == "checker_unavailable" {
		t.Fatalf("check decided: verdict %q outcome %q reason %q, want it still open", verdict, outcome, reason)
	}
}

// Redact route: the receipted versions are read unprotected; the owner protects and locks before
// the open. The route answers ErrRunEnvVaultLocked and no version.
func TestRunEnvRedactRouteStaleEpochIsVaultLockedLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	armProtection(t, fx.env, fx.userID)
	runID := fx.queuedRun(t, "issue")
	secret := envSecretValue("route")
	fx.put(t, "API_KEY", true, secret)
	if p := fx.claim(t); p == nil || runEnvByName(p)["API_KEY"].Value != secret {
		t.Fatalf("claim = %+v", p)
	}
	q := &epochRaceQueries{Queries: fx.env.q, trigger: "receipts", after: fx.enableAndLock(t)}
	fx.svc.q = q
	res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID)
	if !errors.Is(err, ErrRunEnvVaultLocked) || len(res.Versions) != 0 {
		t.Fatalf("RunEnvRedact = %+v, %v; want ErrRunEnvVaultLocked and no version", res, err)
	}
	// The retry re-read the list once: two reads in all.
	if q.reads != 2 || q.byName["receipts"] != 2 {
		t.Fatalf("receipt reads = %d, want 2 (the read and one re-read)", q.reads)
	}
}
