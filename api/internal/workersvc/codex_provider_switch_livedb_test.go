package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Trace the service's transaction pool as well as its owner lookup. Fixture and
// snapshot queries use the original pool and do not pollute these observations.
func traceCodexSwitch(t *testing.T, f applyFx) *frozenLeadVendorReads {
	t.Helper()
	reads := &frozenLeadVendorReads{}
	reads.reset()
	pool, err := store.OpenPool(f.env.ctx, f.env.pool.Config().ConnString(), store.WithQueryTracer(reads))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.svc.q = store.New(pool)
	f.svc.SetTxBeginner(pool)
	return reads
}

func TestCodexProviderSwitchRequestPreflightApplyNoWriteLiveDB(t *testing.T) {
	fx := newProvFix(t)
	seedCodexSource(t, fx)
	run, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	wantCodexBinding(t, fx, run, 1, fx.keyRev())
	sourceA := fx.provider
	// Seal a real, inert provider key; no switch path is allowed to open it.
	sealed, err := fx.env.box.Seal([]byte("inert-provider-switch-key"))
	if err != nil {
		t.Fatal(err)
	}
	fx.env.exec("UPDATE user_secrets SET ciphertext=$2 WHERE id=$1", fx.key, sealed)
	target := fx.env.seedStaticAPIKey(t, fx.userID, "switch-native-target", "switch-native-key")
	f := applyFx{env: fx.env, svc: fx.svc, o: reevalOwner{userID: fx.userID, workerID: fx.workerID}, gen: 7}
	fx.env.exec(`UPDATE workers SET last_heartbeat_at=now(), snapshot_register_nonce=$2,
 credential_switch_apply_nonce=$2, protocol_capabilities=ARRAY['codex_credential_switch_v1','codex_switch_stopped_v1']
 WHERE id=$1`, fx.workerID, applyFixtureNonce)
	// Custody enrollment of a lawfully created provider run; no vendor tuple or
	// baseline is fabricated.
	fx.env.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=$3 WHERE id=$1", run.ID, fx.workerID, f.gen)
	reads := traceCodexSwitch(t, f)
	// Source B is independently legal and must not change the existing run's authority.
	seedCodexSource(t, fx)
	sourceB := fx.provider
	for _, source := range []uuid.UUID{sourceA, sourceB, uuid.Nil} {
		if source == uuid.Nil {
			fx.env.exec("UPDATE users SET codex_provider_id=NULL WHERE id=$1", fx.userID)
		} else {
			fx.env.exec("UPDATE users SET codex_provider_id=$2 WHERE id=$1", fx.userID, source)
		}
		reads.reset()
		f.noWrite(t, run.ID, func() {
			_, err := f.svc.SetRunCredential(f.env.ctx, fx.userID, run.ID, BindModePinned, &target)
			if !errors.Is(err, ErrCredentialOverrideProviderSource) {
				t.Fatal("provider request admitted", err)
			}
		})
		if got := reads.vendor(); len(got) != 0 {
			t.Fatalf("provider request queried vendor: %v", got)
		}
	}
	// An incomplete earlier observation must not bypass the locked-run backstop.
	// Only this caller snapshot is altered; the committed provider tuple stays intact.
	observed := mustRun(t, f.env, run.ID)
	observed.ModelProviderID = pgtype.UUID{}
	reads.reset()
	f.noWrite(t, run.ID, func() {
		_, err := f.svc.requestCodexCredentialSwitch(f.env.ctx, observed, BindModePinned, &target)
		if !errors.Is(err, ErrCredentialOverrideProviderSource) {
			t.Fatal("locked provider request admitted", err)
		}
	})
	if got := reads.vendor(); len(got) != 0 {
		t.Fatalf("locked provider request queried vendor: %v", got)
	}
	// A forged pending request does not supply vendor authority.
	op := f.restamp(t, run.ID)
	req := f.req(op)
	reads.reset()
	f.noWrite(t, run.ID, func() {
		pre, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), run.ID,
			CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: req, TargetSecretID: target})
		if err != nil || pre.Outcome != ApplyOutcomeNotApplied || pre.Reason != applyReasonProviderBound {
			t.Fatalf("provider preflight: %v %v", pre, err)
		}
		got := f.apply(t, run.ID, req)
		f.want(t, got, ApplyOutcomeNotApplied, applyReasonProviderBound)
		if got.Codex != nil || got.Token != "" {
			t.Fatal("provider refusal delivered vendor material")
		}
	})
	if got := reads.vendor(); len(got) != 0 {
		t.Fatalf("provider preflight/apply queried vendor: %v", got)
	}
}

func TestCodexNativeSwitchProviderTargetNoWriteLiveDB(t *testing.T) {
	f, run, native, _ := codexUnrequestedFixture(t, false, false, false)
	p, err := f.env.q.InsertModelProvider(f.env.ctx, store.InsertModelProviderParams{OwnerUserID: f.o.userID, Name: "switch-race-" + uuid.NewString(), Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.env.q.InsertModelProviderRevision(f.env.ctx, store.InsertModelProviderRevisionParams{ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`)}); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.env.box.Seal([]byte("inert-switch-provider-target"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := f.env.q.InsertProviderKeySecret(f.env.ctx, store.InsertProviderKeySecretParams{UserID: f.o.userID, Label: "provider-target", Ciphertext: sealed, SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	key := secret.ID
	reads := traceCodexSwitch(t, f)
	f.noWrite(t, run, func() {
		_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &key)
		if err == nil {
			t.Fatal("provider key accepted as native target")
		}
	})
	for name := range reads.vendor() {
		if strings.Contains(name, "Ciphertext") {
			t.Fatal("target refusal read ciphertext", name)
		}
	}
	// Positive control through the same production seam, followed by an owner
	// source edit between preflight and apply. The frozen vendor tuple wins.
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &native); err != nil {
		t.Fatal(err)
	}
	req := f.req(f.opOf(t, run))
	pre, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), run,
		CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: req, TargetSecretID: native})
	if err != nil || pre.Outcome != "ready" {
		t.Fatal("usable native control not ready", pre, err)
	}
	// Select the usable Codex provider after native preflight has admitted the target.
	f.env.exec("UPDATE users SET codex_provider_id=$2 WHERE id=$1", f.o.userID, p.ID)
	// Restore the owner setting before the fixture's cascading user cleanup.
	defer f.env.exec("UPDATE users SET codex_provider_id=NULL WHERE id=$1", f.o.userID)
	got := f.apply(t, run, req)
	if got.Outcome != ApplyOutcomeApplied || got.Codex == nil || got.Codex.AccessToken != "apply-target-key" {
		t.Fatal("source edit reclassified native run", got)
	}
	if providerBoundRun(mustRun(t, f.env, run)) {
		t.Fatal("native switch rebound provider authority")
	}
}
