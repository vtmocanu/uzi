package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1247 M1 — the per-run credential override resolution ladder, the one validator,
// and the effectiveNextClaimMode policy. The ladder tests reuse the autoFixture harness
// (newAutoFixture) so the claim path, open and record are exercised end to end.

func pinnedMode(id uuid.UUID) (pgtype.Text, pgtype.UUID) {
	return pgtype.Text{String: "pinned", Valid: true}, pgtype.UUID{Bytes: id, Valid: true}
}

// TestRunOverridePinnedBeatsWorkerBind is the NAMED mutation-check test for the ladder
// order: the worker is AUTO with a one-token pool, but the run's per-run override pins a
// DIFFERENT token, so the claim must spend the override's token with reason run_pinned.
//
// MUTATION THIS CATCHES: moving the run-override rung BELOW the worker-bind rung. With
// the rungs swapped the auto worker runs the selector and records emptyID/auto instead of
// the override's fullID/run_pinned — this test reddens on both the id and the reason.
func TestRunOverridePinnedBeatsWorkerBind(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModeAuto // the worker would otherwise auto-pick emptyID
	f.fs.claimRun.CredentialOverrideMode, f.fs.claimRun.CredentialOverrideSecretID = pinnedMode(f.fullID)

	if payload := f.claim(t); payload == nil {
		t.Fatal("expected a payload, got idle")
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fullID {
		t.Fatalf("recorded %v, want the run-override's token %v (not the worker's auto pick)",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fullID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonRunPinned) {
		t.Fatalf("reason = %q, want run_pinned", rec.AnthropicSelectReason.String)
	}
	// The override open is kind-scoped (D9): the run-override path consulted the
	// kind-scoped lookup for its own token, owner-and-kind-scoped.
	if len(f.fs.metaOfKindLookups) != 1 || f.fs.metaOfKindLookups[0].ID != f.fullID ||
		f.fs.metaOfKindLookups[0].Kind != store.KindAnthropicToken {
		t.Fatalf("kind-scoped lookups = %+v, want one for fullID of kind anthropic_token", f.fs.metaOfKindLookups)
	}
	// The attribution epoch was written for this claim, keyed by the run's generation.
	if len(f.fs.recordedEpochs) != 1 {
		t.Fatalf("epoch writes = %d, want exactly 1: %+v", len(f.fs.recordedEpochs), f.fs.recordedEpochs)
	}
	ep := f.fs.recordedEpochs[0]
	if ep.RunID != f.runID || ep.ClaimGeneration != f.fs.claimRun.ClaimGeneration ||
		uuid.UUID(ep.SecretID.Bytes) != f.fullID || ep.SelectReason.String != string(autoselect.ReasonRunPinned) {
		t.Fatalf("epoch = %+v, want run=%v gen=%d secret=%v reason=run_pinned",
			ep, f.runID, f.fs.claimRun.ClaimGeneration, f.fullID)
	}
}

// TestRunOverrideAutoRunsSelector: an override of mode `auto` runs the pool selector even
// on a worker PINNED to another token, and records the selector's own reason (auto),
// never run_pinned/run_default.
func TestRunOverrideAutoRunsSelector(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	f.fs.claimRun.CredentialOverrideMode = pgtype.Text{String: "auto", Valid: true}
	// A single-token pool (emptyID) so the selector's pick is unambiguous.
	if payload := f.claim(t); payload == nil {
		t.Fatal("expected a payload, got idle")
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("recorded %v, want the selector's pooled pick %v", uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want auto (the selector's own reason, not a run override reason)",
			rec.AnthropicSelectReason.String)
	}
}

// TestRunOverrideDefaultRecordsRunDefault: an override of mode `default` spends the owner
// default but records reason run_default — the deliberate per-run choice, distinct from an
// unset binding falling through to `default` (staticChoice(nil) cannot express this).
func TestRunOverrideDefaultRecordsRunDefault(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	f.fs.claimRun.CredentialOverrideMode = pgtype.Text{String: "default", Valid: true}

	if payload := f.claim(t); payload == nil {
		t.Fatal("expected a payload, got idle")
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != fakeDefaultSecretID {
		t.Fatalf("recorded %v, want the owner default %v", uuid.UUID(rec.AnthropicSecretID.Bytes), fakeDefaultSecretID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonRunDefault) {
		t.Fatalf("reason = %q, want run_default", rec.AnthropicSelectReason.String)
	}
}

// TestRunOverrideSelfImproveUnaffected: a self_improve run carries an override, but the
// judge binding is checked FIRST and the override never applies (D10). It records the
// judge's token with reason judge.
func TestRunOverrideSelfImproveUnaffected(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.claimRun.Kind = runkind.SelfImprove
	// The judge lane pins a token (reuse fullID's staged ciphertext).
	f.fs.judgeBindMode = BindModePinned
	f.fs.judgeSecret = pgtype.UUID{Bytes: f.fullID, Valid: true}
	// And a run override that MUST be ignored — pin emptyID.
	f.fs.claimRun.CredentialOverrideMode, f.fs.claimRun.CredentialOverrideSecretID = pinnedMode(f.emptyID)

	if payload := f.claim(t); payload == nil {
		t.Fatal("expected a payload, got idle")
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fullID {
		t.Fatalf("recorded %v, want the JUDGE token %v — the override must be ignored for self_improve",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fullID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonJudge) {
		t.Fatalf("reason = %q, want judge", rec.AnthropicSelectReason.String)
	}
	// The override's kind-scoped lookup was never consulted.
	if len(f.fs.metaOfKindLookups) != 0 {
		t.Fatalf("kind-scoped lookups = %+v, want none for a self_improve run", f.fs.metaOfKindLookups)
	}
}

// TestRunOverrideNulledPinInherits: a `pinned` override whose secret id was nulled by a
// token delete resolves as inherit (D1), so the WORKER binding decides. Here the worker is
// pinned to fullID and the claim records it with reason pinned (not run_pinned).
func TestRunOverrideNulledPinInherits(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	f.fs.claimRun.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
	f.fs.claimRun.CredentialOverrideSecretID = pgtype.UUID{} // nulled by the FK on delete

	if payload := f.claim(t); payload == nil {
		t.Fatal("expected a payload, got idle")
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fullID {
		t.Fatalf("recorded %v, want the worker binding %v (nulled override inherits)",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fullID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPinned) {
		t.Fatalf("reason = %q, want pinned (the worker binding, not run_pinned)", rec.AnthropicSelectReason.String)
	}
	if len(f.fs.metaOfKindLookups) != 0 {
		t.Fatalf("kind-scoped lookups = %+v, want none — a nulled pin inherits without an override lookup", f.fs.metaOfKindLookups)
	}
}

// TestRunOverrideForeignIDIsUnavailable: a `pinned` override naming a foreign (or vanished)
// id is refused at resolution — errCredentialUnavailable, a terminal run failure — never
// silently inherited onto the worker binding.
func TestRunOverrideForeignIDIsUnavailable(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	foreign := uuid.New() // not staged in byIDSecrets → the kind-scoped lookup returns no rows
	f.fs.claimRun.CredentialOverrideMode, f.fs.claimRun.CredentialOverrideSecretID = pinnedMode(foreign)

	if payload := f.claim(t); payload != nil {
		t.Fatal("expected idle: a foreign run-override token must be terminal, not inherited")
	}
	if f.fs.claimFailed == nil {
		t.Fatal("a foreign run-override token did not fail the run")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential nothing opened: %+v", f.fs.recordedCreds)
	}
}

// TestRunOverrideWrongKindRefused: a `pinned` override naming the caller's own secret of a
// NON-anthropic kind (a codex_auth alias) is refused (D9): the kind-scoped lookup returns no
// rows, so the run fails rather than spending a wrong-kind credential.
func TestRunOverrideWrongKindRefused(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.fullID, Valid: true}
	wrongKind := uuid.New()
	f.fs.byIDSecrets[wrongKind] = store.GetUserSecretCiphertextByIDRow{
		UserID: f.owner, Kind: store.KindCodexAuth, Ciphertext: []byte("x"), SealedWith: store.SealedWithMaster,
	}
	f.fs.claimRun.CredentialOverrideMode, f.fs.claimRun.CredentialOverrideSecretID = pinnedMode(wrongKind)

	if payload := f.claim(t); payload != nil {
		t.Fatal("expected idle: a wrong-kind run-override token must be refused, not spent")
	}
	if f.fs.claimFailed == nil {
		t.Fatal("a wrong-kind run-override token did not fail the run")
	}
	if len(f.fs.recordedCreds) != 0 {
		t.Fatalf("recorded a credential nothing opened: %+v", f.fs.recordedCreds)
	}
}

// --- the one validator ----------------------------------------------------------

// TestValidateCredentialOverrideAcceptsEachMode: the four accepted modes on a switchable
// lane (issue) and a claude harness, with an owner-owned anthropic token for the pin.
func TestValidateCredentialOverrideAcceptsEachMode(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()
	// The pinned target must be the caller's OWN anthropic_token (staged in byIDSecrets).
	got, err := f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessClaude, CredentialOverrideModePinned, &f.fullID)
	if err != nil {
		t.Fatalf("pinned: %v", err)
	}
	if got == nil || got.Mode != CredentialOverrideModePinned || got.SecretID == nil || *got.SecretID != f.fullID {
		t.Fatalf("pinned override = %+v, want {pinned, fullID}", got)
	}

	got, err = f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessClaude, CredentialOverrideModeAuto, nil)
	if err != nil || got == nil || got.Mode != CredentialOverrideModeAuto || got.SecretID != nil {
		t.Fatalf("auto override = %+v, err %v; want {auto, nil}", got, err)
	}

	got, err = f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessClaude, CredentialOverrideModeDefault, nil)
	if err != nil || got == nil || got.Mode != CredentialOverrideModeDefault || got.SecretID != nil {
		t.Fatalf("default override = %+v, err %v; want {default, nil}", got, err)
	}

	// inherit clears both columns → a nil override, no error.
	got, err = f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessClaude, CredentialOverrideModeInherit, nil)
	if err != nil || got != nil {
		t.Fatalf("inherit override = %+v, err %v; want nil override, nil err", got, err)
	}
}

// TestValidateCredentialOverride404 covers the not-found class: a pinned override naming a
// foreign id, and a pinned override with no id at all.
func TestValidateCredentialOverride404(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()
	foreign := uuid.New()
	if _, err := f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessClaude, CredentialOverrideModePinned, &foreign); !errors.Is(err, ErrCredentialOverrideSecretNotFound) {
		t.Fatalf("foreign pin err = %v, want ErrCredentialOverrideSecretNotFound", err)
	}
	if _, err := f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessClaude, CredentialOverrideModePinned, nil); !errors.Is(err, ErrCredentialOverridePinnedNeedsSecret) {
		t.Fatalf("pin without id err = %v, want ErrCredentialOverridePinnedNeedsSecret", err)
	}
}

// TestValidateCredentialOverride409 covers the lane class: chat/judge/self_improve are not
// switchable (D10), for EVERY mode including inherit — the lane refusal precedes the
// per-mode handling.
func TestValidateCredentialOverride409(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()
	for _, kind := range []string{runkind.Chat, runkind.Judge, runkind.SelfImprove} {
		for _, mode := range []string{CredentialOverrideModePinned, CredentialOverrideModeAuto, CredentialOverrideModeDefault, CredentialOverrideModeInherit} {
			var sid *uuid.UUID
			if mode == CredentialOverrideModePinned {
				sid = &f.fullID
			}
			if _, err := f.svc.validateCredentialOverride(ctx, f.owner, kind, harnessClaude, mode, sid); !errors.Is(err, ErrCredentialOverrideLaneNotSwitchable) {
				t.Fatalf("kind=%s mode=%s err = %v, want ErrCredentialOverrideLaneNotSwitchable", kind, mode, err)
			}
		}
	}
}

// TestValidateCredentialOverride422 covers the harness class: a codex-harness run/schedule
// cannot carry an Anthropic override (D9), for EVERY mode including inherit.
func TestValidateCredentialOverride422(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()
	for _, mode := range []string{CredentialOverrideModePinned, CredentialOverrideModeAuto, CredentialOverrideModeDefault, CredentialOverrideModeInherit} {
		var sid *uuid.UUID
		if mode == CredentialOverrideModePinned {
			sid = &f.fullID
		}
		if _, err := f.svc.validateCredentialOverride(ctx, f.owner, runkind.Issue, harnessCodex, mode, sid); !errors.Is(err, ErrCredentialOverrideHarnessUnsupported) {
			t.Fatalf("mode=%s codex err = %v, want ErrCredentialOverrideHarnessUnsupported", mode, err)
		}
	}
}

// TestValidateCredentialOverrideInvalidMode: a mode outside the closed set is refused.
func TestValidateCredentialOverrideInvalidMode(t *testing.T) {
	f := newAutoFixture(t)
	if _, err := f.svc.validateCredentialOverride(context.Background(), f.owner, runkind.Issue, harnessClaude, "sideways", nil); !errors.Is(err, ErrCredentialOverrideInvalidMode) {
		t.Fatalf("invalid mode err = %v, want ErrCredentialOverrideInvalidMode", err)
	}
}

// --- effectiveNextClaimMode policy ----------------------------------------------

// TestEffectiveNextClaimMode covers every rung of the pure policy (PRD #1247): the
// self_improve→judge rung, the three override modes, a nulled pin inheriting, no override
// inheriting the worker, and the two unknown cases (no recorded worker, deleted worker).
func TestEffectiveNextClaimMode(t *testing.T) {
	worker := store.Worker{ID: uuid.New(), AnthropicBindMode: BindModePinned}
	withWorker := func(r store.Run) store.Run {
		r.WorkerID = pgtype.UUID{Bytes: worker.ID, Valid: true}
		return r
	}
	overrideRun := func(mode string, idValid bool) store.Run {
		r := withWorker(store.Run{Kind: runkind.Issue})
		r.CredentialOverrideMode = pgtype.Text{String: mode, Valid: mode != ""}
		if idValid {
			r.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		}
		return r
	}

	cases := []struct {
		name   string
		run    store.Run
		judge  string
		worker store.Worker
		want   string
	}{
		{"self_improve follows judge mode", withWorker(store.Run{Kind: runkind.SelfImprove}), BindModeAuto, worker, BindModeAuto},
		{"override pinned", overrideRun(BindModePinned, true), "", worker, BindModePinned},
		{"override auto", overrideRun(BindModeAuto, false), "", worker, BindModeAuto},
		{"override default", overrideRun(BindModeDefault, false), "", worker, BindModeDefault},
		{"nulled pin inherits worker", overrideRun(BindModePinned, false), "", worker, BindModePinned},
		{"no override inherits worker", withWorker(store.Run{Kind: runkind.Issue}), "", worker, BindModePinned},
		{"no recorded worker is unknown", store.Run{Kind: runkind.Issue}, "", worker, effectiveClaimModeUnknown},
		{"deleted worker is unknown", withWorker(store.Run{Kind: runkind.Issue}), "", store.Worker{}, effectiveClaimModeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveNextClaimMode(tc.run, tc.judge, tc.worker); got != tc.want {
				t.Fatalf("effectiveNextClaimMode = %q, want %q", got, tc.want)
			}
		})
	}
}
