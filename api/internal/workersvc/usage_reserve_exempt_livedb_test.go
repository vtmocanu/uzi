package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_reserve_exempt_livedb_test.go assembles the weekly usage reserve's exempt-run paths end to
// end (PRD #3522 M4b): a run the reserve parked, then released by an owner decision (a manual
// resume that records an override) or by the owner turning the pause toggle off, must be CLAIMED and
// ASSEMBLED on its own above-threshold credential. The reading never drops in these tests: the
// credential stays above the line, so only the exemption can let the run through, in each of the
// three credential selections (pinned, default, auto). Each case has a negative control in which the
// exemption has ended or never existed and the same claim is held.
//
// MUTATION: making the assembly gate (the reserveEnforced checks in the credential selection and
// reserveGate) treat every run as enforced reddens the exempt claims below with a requeue.

type exemptMode struct {
	name string
	bind string
	// setup seeds the above-threshold credential (and any decoy) and returns it.
	setup func(fx *reserveFix) uuid.UUID
	// pin is applied to the parked run so the selection mode is the one under test.
	pin func(fx *reserveFix, run, tok uuid.UUID)
}

func exemptModes() []exemptMode {
	return []exemptMode{
		{
			name: "pinned",
			bind: BindModeDefault,
			setup: func(fx *reserveFix) uuid.UUID {
				fx.token("decoy-default", true, false, 5) // a below-threshold default the pinned run must NOT use
				return fx.token("pinned", false, false, 90)
			},
			pin: func(fx *reserveFix, run, tok uuid.UUID) {
				fx.env.exec(`UPDATE runs SET credential_override_mode = 'pinned', credential_override_secret_id = $2 WHERE id = $1`, run, tok)
			},
		},
		{
			name:  "default",
			bind:  BindModeDefault,
			setup: func(fx *reserveFix) uuid.UUID { return fx.token("default", true, false, 90) },
			pin:   func(*reserveFix, uuid.UUID, uuid.UUID) {},
		},
		{
			name:  "auto",
			bind:  BindModeAuto,
			setup: func(fx *reserveFix) uuid.UUID { return fx.token("pooled", false, true, 90) },
			pin:   func(*reserveFix, uuid.UUID, uuid.UUID) {},
		},
	}
}

// reserveParked returns a run the reserve really parked: a running run on tok, the sweeper's request,
// then the worker's park (SetRunPaused). It asserts the weekly_reserve hold.
func (fx *reserveFix) reserveParked(m exemptMode, tok uuid.UUID) uuid.UUID {
	fx.t.Helper()
	fx.bindWorker(m.bind, nil)
	run := fx.pauseRun(pauseRun{cred: &tok, completed: 1})
	m.pin(fx, run, tok)
	if n := fx.pass(); n < 1 {
		fx.t.Fatalf("%s: the sweeper filed no reserve request", m.name)
	}
	if mode, _, req := fx.pausePending(run); !req || mode != "reserve" {
		fx.t.Fatalf("%s: pending request = (%q, %v), want reserve", m.name, mode, req)
	}
	if n, err := fx.env.q.SetRunPaused(fx.env.ctx, store.SetRunPausedParams{ID: run, WorkerID: pgconv.UUID(fx.workerID)}); err != nil || n != 1 {
		fx.t.Fatalf("%s: SetRunPaused = (%d, %v)", m.name, n, err)
	}
	if r := mustRun(fx.t, fx.env, run); r.Status != "paused" || r.HoldReason.String != "weekly_reserve" {
		fx.t.Fatalf("%s: parked run = %s/%q, want paused/weekly_reserve", m.name, r.Status, r.HoldReason.String)
	}
	return run
}

// ownerResume is the owner's resume of a paused run, exactly as the handler's ResumeRunNow builds it:
// a weekly_reserve hold records the override computed by ComputeReserveOverride. withOverride=false
// resumes without the grant (an ordinary resume), the negative control.
func (fx *reserveFix) ownerResume(run uuid.UUID, withOverride bool) {
	fx.t.Helper()
	r := mustRun(fx.t, fx.env, run)
	params := store.ResumePausedRunParams{
		ID: run, UserID: fx.userID,
		GlobalTimeoutSeconds: int32(fx.svc.p.RunTimeout.Seconds()),
		StaleCutoff:          fx.svc.staleWorkerCutoff(),
		RequiredHoldReason:   pgconv.Text("weekly_reserve"),
	}
	if withOverride {
		grant, err := ComputeReserveOverride(fx.env.ctx, fx.env.q, r, ReserveIntervals{Claude: 5 * time.Minute, Codex: 5 * time.Minute}, time.Now())
		if err != nil || grant == nil {
			fx.t.Fatalf("ComputeReserveOverride = (%v, %v)", grant, err)
		}
		params.ReserveExemptGrantedAt = pgtype.Timestamptz{Time: grant.GrantedAt, Valid: true}
		params.ReserveExemptUntil = pgtype.Timestamptz{Time: grant.Until, Valid: true}
		if grant.CredentialID != nil {
			params.ReserveExemptCredentialID = pgtype.UUID{Bytes: *grant.CredentialID, Valid: true}
		}
		if grant.WindowReset != nil {
			params.ReserveExemptWindowReset = pgtype.Timestamptz{Time: *grant.WindowReset, Valid: true}
		}
		if grant.PriorPct != nil {
			params.ReserveExemptPriorPct = pgtype.Int2{Int16: *grant.PriorPct, Valid: true}
		}
	}
	if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, params); err != nil {
		fx.t.Fatalf("ResumePausedRun: %v", err)
	}
	if got := mustRun(fx.t, fx.env, run); got.Status != "queued" {
		fx.t.Fatalf("resumed run status = %s, want queued", got.Status)
	}
}

// requireAssembled claims once and asserts the run was claimed and assembled on tok with no reserve
// hold left behind.
func (fx *reserveFix) requireAssembled(label string, run, tok uuid.UUID) {
	fx.t.Helper()
	p := fx.claim()
	if p == nil || p.Secrets.AnthropicOAuthToken == "" {
		r := mustRun(fx.t, fx.env, run)
		fx.t.Fatalf("%s: the exempt run was not assembled (status %s, reserve_hold_reason %q)", label, r.Status, r.ReserveHoldReason.String)
	}
	r := mustRun(fx.t, fx.env, run)
	if r.Status != "claimed" || r.ClaimGeneration < 1 {
		fx.t.Fatalf("%s: status=%s generation=%d, want claimed with a new generation (no requeue)", label, r.Status, r.ClaimGeneration)
	}
	if r.ReserveHoldReason.Valid {
		fx.t.Fatalf("%s: reserve_hold_reason = %q, want NULL", label, r.ReserveHoldReason.String)
	}
	if r.AnthropicSecretID != (pgtype.UUID{Bytes: tok, Valid: true}) {
		fx.t.Fatalf("%s: assembled on credential %v, want the run's %s", label, r.AnthropicSecretID, tok)
	}
}

// requireHeld claims once and asserts nothing was assembled: the run stays queued, untouched or
// requeued with the reserve reason, never claimed.
func (fx *reserveFix) requireHeld(label string, run uuid.UUID) {
	fx.t.Helper()
	if p := fx.claim(); p != nil {
		fx.t.Fatalf("%s: the run was assembled although the reserve must hold it", label)
	}
	if r := mustRun(fx.t, fx.env, run); r.Status != "queued" {
		fx.t.Fatalf("%s: status = %s, want queued", label, r.Status)
	}
}

// TestReserveManualOverrideAssemblesOnHeldCredentialLiveDB: reserve park -> owner resume with a
// recorded override -> svc.Claim assembles the run on its above-threshold credential, in each
// credential selection. Control: once the override is ended by an observed reset (the week
// rolled over and usage is already high again) the same claim is held; and a resume that recorded no
// override is held at once.
func TestReserveManualOverrideAssemblesOnHeldCredentialLiveDB(t *testing.T) {
	for _, m := range exemptModes() {
		t.Run(m.name, func(t *testing.T) {
			fx := newReserveFix(t, 70, true)
			tok := m.setup(fx)
			run := fx.reserveParked(m, tok)
			fx.ownerResume(run, true)
			if r := mustRun(t, fx.env, run); !r.ReserveExemptUntil.Valid || r.HoldReason.Valid {
				t.Fatalf("resumed run: exempt_until=%v hold=%q, want a recorded override and no hold", r.ReserveExemptUntil, r.HoldReason.String)
			}
			fx.requireAssembled("override active", run, tok)
		})
		// Control A: the override has ended (the week reset early, usage is already back above the line).
		t.Run(m.name+"/override ended by an observed reset", func(t *testing.T) {
			ctl := newReserveFix(t, 70, true)
			ctok := m.setup(ctl)
			crun := ctl.reserveParked(m, ctok)
			ctl.ownerResume(crun, true)
			ctl.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = now() + interval '9 days', seven_day_pct = 90, synced_at = now() WHERE user_secret_id = $1`, ctok)
			ctl.requireHeld("override ended by an observed reset", crun)
		})
		// Control B: a resume that recorded no override leaves the run enforced.
		t.Run(m.name+"/resume without an override", func(t *testing.T) {
			plain := newReserveFix(t, 70, true)
			ptok := m.setup(plain)
			prun := plain.reserveParked(m, ptok)
			plain.ownerResume(prun, false)
			if r := mustRun(t, plain.env, prun); r.ReserveExemptUntil.Valid {
				t.Fatalf("a resume without a grant recorded an exemption: %v", r.ReserveExemptUntil)
			}
			plain.requireHeld("resume without an override", prun)
		})
	}
}

// TestReserveToggleOffAssemblesOnHeldCredentialLiveDB: reserve park -> the owner turns the pause
// toggle off -> the sweeper releases the park -> svc.Claim assembles the run on its above-threshold
// credential (a started run is not gated once pausing running runs is off), in each credential
// selection. Control: with the toggle left on the park is not released, and a requeued run is held.
func TestReserveToggleOffAssemblesOnHeldCredentialLiveDB(t *testing.T) {
	for _, m := range exemptModes() {
		t.Run(m.name, func(t *testing.T) {
			fx := newReserveFix(t, 70, true)
			tok := m.setup(fx)
			run := fx.reserveParked(m, tok)
			fx.setReserve(70, false)
			if n := fx.pass(); n < 1 {
				t.Fatal("the sweeper did not release the park after the toggle went off")
			}
			if r := mustRun(t, fx.env, run); r.Status != "queued" || r.HoldReason.Valid {
				t.Fatalf("released run = %s/%q, want queued with no hold", r.Status, r.HoldReason.String)
			}
			fx.requireAssembled("toggle off", run, tok)
		})
		// Control: the toggle stays on. The park is not released while the credential is above the line ...
		t.Run(m.name+"/toggle on", func(t *testing.T) {
			ctl := newReserveFix(t, 70, true)
			ctok := m.setup(ctl)
			crun := ctl.reserveParked(m, ctok)
			ctl.pass()
			if r := mustRun(t, ctl.env, crun); r.Status != "paused" || r.HoldReason.String != "weekly_reserve" {
				t.Fatalf("toggle on: run = %s/%q, want still parked", r.Status, r.HoldReason.String)
			}
			// ... and a started run requeued without an exemption is held at the claim.
			ctl.env.exec(`UPDATE runs SET status = 'queued', hold_reason = NULL WHERE id = $1`, crun)
			ctl.requireHeld("toggle on", crun)
		})
	}
}
