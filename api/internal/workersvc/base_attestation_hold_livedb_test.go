package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// issue #3194 M4: the base-attestation hold. A `--require-base` run whose worker adopted kept work
// no checkpoint attests is held (running -> paused, hold_reason 'base_attestation_missing') through
// the worker's fenced report; nothing but the owner's recovery, a successor or cancel leaves it.
// Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips).

// seedRequireBaseRun seeds a running --require-base run owned by o's worker at claim generation 3.
func seedRequireBaseRun(t *testing.T, env codexTestEnv, o reevalOwner, iid int64) uuid.UUID {
	t.Helper()
	id := seedRunningRun(t, env, o, iid, 0)
	env.exec(`UPDATE runs SET claim_generation = 3, require_base_match = true, planned_base_commit = 'abcdef1234567',
	              health = 'stalled', health_reason = 'x', health_since = now() WHERE id = $1`, id)
	return id
}

func TestSetRunBaseAttestationHoldFenceAndIdempotencyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	head := "0123456789abcdef0123456789abcdef01234567"

	t.Run("a stale generation is refused with the real status", func(t *testing.T) {
		id := seedRequireBaseRun(t, env, o, 31941)
		run, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, head, 2)
		if err != nil || applied || run.Status != "running" {
			t.Fatalf("stale generation = (%q, applied=%v, %v), want (running, false, nil)", run.Status, applied, err)
		}
	})

	t.Run("a released claim is refused", func(t *testing.T) {
		id := seedRequireBaseRun(t, env, o, 31942)
		env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
		if _, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, head, 3); err != nil || applied {
			t.Fatalf("released claim = (applied=%v, %v), want (false, nil)", applied, err)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running", s)
		}
	})

	t.Run("another worker is not the owner", func(t *testing.T) {
		id := seedRequireBaseRun(t, env, o, 31943)
		other := store.Worker{ID: uuid.New(), UserID: o.userID}
		if _, _, err := svc.SetRunBaseAttestationHold(env.ctx, other, id, head, 3); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign worker err = %v, want ErrRunNotOwned", err)
		}
	})

	t.Run("a run without --require-base cannot enter the hold", func(t *testing.T) {
		id := seedRequireBaseRun(t, env, o, 31944)
		env.exec(`UPDATE runs SET require_base_match = false WHERE id = $1`, id)
		if _, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, head, 3); err != nil || applied {
			t.Fatalf("no --require-base = (applied=%v, %v), want (false, nil)", applied, err)
		}
	})

	t.Run("a non-running run is refused", func(t *testing.T) {
		id := seedRequireBaseRun(t, env, o, 31945)
		env.exec(`UPDATE runs SET status = 'awaiting_approval' WHERE id = $1`, id)
		if _, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, head, 3); err != nil || applied {
			t.Fatalf("awaiting_approval = (applied=%v, %v), want (false, nil)", applied, err)
		}
	})

	t.Run("the live claim is held, and a repeat changes nothing", func(t *testing.T) {
		id := seedRequireBaseRun(t, env, o, 31946)
		run, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, "\x00 "+head+" ", 3)
		if err != nil || !applied {
			t.Fatalf("hold = (applied=%v, %v), want (true, nil)", applied, err)
		}
		if run.Status != "paused" || run.HoldReason.String != HoldReasonBaseAttestationMissing {
			t.Fatalf("held run = (%q, %q), want (paused, %s)", run.Status, run.HoldReason.String, HoldReasonBaseAttestationMissing)
		}
		if run.HoldCapturedHead.String != head {
			t.Fatalf("hold_captured_head = %q, want the NUL-stripped trimmed head", run.HoldCapturedHead.String)
		}
		if run.Health != "ok" || run.HealthReason.Valid || run.HealthSince.Valid {
			t.Fatalf("health = (%q, %v, %v), want reset to ok", run.Health, run.HealthReason, run.HealthSince)
		}
		if run.WorkerID.Bytes != o.workerID || run.ClaimReleasedAt.Valid {
			t.Fatal("the hold keeps the worker and the live claim (its custody stays on that worker)")
		}
		time.Sleep(10 * time.Millisecond)
		again, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, head, 3)
		if err != nil || !applied {
			t.Fatalf("repeat = (applied=%v, %v), want (true, nil): a lost ACK retried learns it is held", applied, err)
		}
		if !again.StatusSince.Time.Equal(run.StatusSince.Time) || again.BudgetPausedSeconds != run.BudgetPausedSeconds ||
			again.BudgetExtensionSeconds != run.BudgetExtensionSeconds {
			t.Fatal("a repeat must not reset status_since or any budget")
		}
		if _, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, head, 4); err != nil || applied {
			t.Fatalf("a repeat from another generation = (applied=%v, %v), want (false, nil)", applied, err)
		}
	})
}

// TestBaseAttestationHoldRefusesOwnerResumeVerbsLiveDB: resume (both callers), extend, stop and
// a token switch are refused and change nothing; the owner's cancel still applies.
func TestBaseAttestationHoldRefusesOwnerResumeVerbsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	id := seedRequireBaseRun(t, env, o, 31947)
	held, applied, err := svc.SetRunBaseAttestationHold(env.ctx, wkr, id, "", 3)
	if err != nil || !applied {
		t.Fatalf("hold = (applied=%v, %v)", applied, err)
	}

	for _, allow := range []bool{false, true} {
		if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{
			ID: id, UserID: o.userID, AllowCompletionBlockedHold: allow, GlobalTimeoutSeconds: 3600,
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ResumePausedRun(allow=%v) err = %v, want ErrNoRows", allow, err)
		}
	}
	for _, kind := range []string{"extend", "stop"} {
		if _, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, kind, "3600", nil, SubmitInputOptions{}); !errors.Is(err, ErrBaseAttestationHeld) {
			t.Fatalf("%s err = %v, want ErrBaseAttestationHeld", kind, err)
		}
	}
	if _, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok); !errors.Is(err, ErrBaseAttestationHeld) {
		t.Fatalf("set-token err = %v, want ErrBaseAttestationHeld", err)
	}
	after, err := env.q.GetRunByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "paused" || after.HoldReason.String != HoldReasonBaseAttestationMissing ||
		after.BudgetExtensionSeconds != held.BudgetExtensionSeconds || after.CredentialOverrideMode.Valid {
		t.Fatalf("a refused verb changed the held run: status=%q hold=%q ext=%d override=%v",
			after.Status, after.HoldReason.String, after.BudgetExtensionSeconds, after.CredentialOverrideMode)
	}

	// Controls on an owner pause of the same shape (same worker, generation, --require-base): the
	// refusals above come from the hold, not from a broken predicate. Resume applies; extend, stop
	// and set-token never answer ErrBaseAttestationHeld (they keep their ordinary outcomes).
	for i, allow := range []bool{false, true} {
		ctl := seedRequireBaseRun(t, env, o, 31948+int64(i))
		env.exec(`UPDATE runs SET status = 'paused', status_since = now() WHERE id = $1`, ctl)
		if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{
			ID: ctl, UserID: o.userID, AllowCompletionBlockedHold: allow, GlobalTimeoutSeconds: 3600,
		}); err != nil {
			t.Fatalf("control: ResumePausedRun(allow=%v) on an owner pause err = %v, want nil", allow, err)
		}
		if s := statusOf(t, env, ctl); s != "queued" {
			t.Fatalf("control: status after resume = %q, want queued", s)
		}
	}
	ctl := seedRequireBaseRun(t, env, o, 31950)
	env.exec(`UPDATE runs SET status = 'paused', status_since = now() WHERE id = $1`, ctl)
	for _, kind := range []string{"extend", "stop"} {
		if _, err := svc.SubmitInputWithOptions(env.ctx, o.userID, ctl, kind, "3600", nil, SubmitInputOptions{}); errors.Is(err, ErrBaseAttestationHeld) {
			t.Fatalf("control: %s on an owner pause was refused as a base-attestation hold", kind)
		}
	}
	if _, err := svc.SetRunCredential(env.ctx, o.userID, ctl, CredentialOverrideModePinned, &o.altTok); errors.Is(err, ErrBaseAttestationHeld) {
		t.Fatal("control: set-token on an owner pause was refused as a base-attestation hold")
	}

	res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "owner cancel", nil, SubmitInputOptions{})
	if err != nil || !res.ServerSide {
		t.Fatalf("owner cancel = (%+v, %v), want a server-side cancel", res, err)
	}
	if s := statusOf(t, env, id); s != "cancelled" {
		t.Fatalf("status after the owner's cancel = %q, want cancelled", s)
	}
}
