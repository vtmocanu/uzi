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
}

func (q *epochRaceQueries) fire(name string) {
	q.reads++
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
		})
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
	if q.reads != 2 {
		t.Fatalf("receipt reads = %d, want 2 (the read and one re-read)", q.reads)
	}
}
