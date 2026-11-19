package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// bindRunToProvider freezes a provider binding on a queued run (a provider-bound run:
// model_provider_key_mode set), the shape workersvc.createRunAtomic stamps.
func bindRunToProvider(fx *fleetFixture, runID uuid.UUID, name string) {
	fx.t.Helper()
	p := newProvider(fx.ctx, fx.t, fx.q, fx.userID, name)
	key, err := insertProviderKey(fx.ctx, fx.q, fx.userID, p.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET harness = 'claude',
        model_provider_id = $2, model_provider_revision = 1,
        model_provider_key_mode = 'user_key', model_provider_key_secret_id = $3,
        model_provider_key_rev = (SELECT enablement_rev FROM user_secrets WHERE id = $3)
        WHERE id = $1`, runID, p.ID, key.ID)
}

const claudeProviderCap = "claude_provider_v1"

// PRD #4320 / #4505: provider execution is activated, so a provider-bound run is demand for an
// ephemeral worker (the new OR in ListUnplaceableQueuedRunsForEphemeral), suppressed by an online
// idle worker that advertises claude_provider_v1 (fn_placement_admitting_workers' provider clause).
func TestProviderRunProvisionsEphemeralLiveDB(t *testing.T) {
	t.Run("zero-capability provider run with only incapable workers is listed", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("old-image", capOf(1), false)
		fx.jobWorker("old-image-docker", capOf(1), true)
		bound := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, bound, "zero-cap")
		if got := unplaceableRunIDs(fx); !got[bound] {
			t.Fatalf("zero-capability provider run %s not listed although no online worker advertises %s", bound, claudeProviderCap)
		}
	})

	t.Run("zero-capability provider run with no worker at all is listed", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		bound := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, bound, "no-worker")
		if got := unplaceableRunIDs(fx); !got[bound] {
			t.Fatalf("provider run %s not listed with an empty fleet", bound)
		}
	})

	t.Run("an online idle provider-capable persistent worker suppresses it", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("old-image", capOf(1), false)
		fx.jobWorker("capable", capOf(1), false, claudeProviderCap)
		bound := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, bound, "capable")
		if got := unplaceableRunIDs(fx); got[bound] {
			t.Fatalf("provider run %s listed although an online idle %s worker can claim it", bound, claudeProviderCap)
		}
	})

	t.Run("a draining provider-capable worker does not suppress it", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := fx.jobWorker("capable-draining", capOf(1), false, claudeProviderCap)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, w)
		bound := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, bound, "draining")
		if got := unplaceableRunIDs(fx); !got[bound] {
			t.Fatalf("provider run %s not listed; a draining worker claims nothing", bound)
		}
	})

	t.Run("the equivalent vendor run is not listed", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		fx.jobWorker("old-image", capOf(1), false)
		vendor := queuedRunWithCaps(fx, []string{})
		if got := unplaceableRunIDs(fx); got[vendor] {
			t.Fatalf("zero-capability vendor run %s listed although the online worker can claim it", vendor)
		}
	})

	t.Run("capability-positive controls stay", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		worker := fx.worker("docker-no-provider", capOf(1), true)
		vendor := queuedRunWithCaps(fx, []string{"docker"})
		bound := queuedRunWithCaps(fx, []string{"docker"})
		bindRunToProvider(fx, bound, "docker-bound")
		got := unplaceableRunIDs(fx)
		if got[vendor] {
			t.Error("the online Docker worker should satisfy the vendor run")
		}
		if !got[bound] {
			t.Error("docker provider run not listed although the Docker worker lacks claude_provider_v1")
		}
		// Offline fleet: ordinary capability gaps still provision, provider-bound or not.
		mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET status = 'offline' WHERE id = $1", worker)
		got = unplaceableRunIDs(fx)
		if !got[vendor] || !got[bound] {
			t.Errorf("offline fleet: vendor listed=%t, provider listed=%t; want true, true", got[vendor], got[bound])
		}
	})
}

// The saturation sibling already carried the provider clause: a capable-but-full worker makes a
// provider run saturation demand, an incapable full worker does not count as capable (that is the
// gap trigger's case).
func TestProviderRunSaturationTriggerLiveDB(t *testing.T) {
	t.Run("capable but full worker: saturation lists it, the gap query does not", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("capable-full", capOf(1), false, claudeProviderCap)
		fx.holdActive(full, 1)
		bound := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, bound, "sat")
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, bound)
		if got := saturationRunIDs(fx); !got[bound] {
			t.Fatalf("provider run %s not listed by the saturation trigger although the only capable worker is full", bound)
		}
		if got := unplaceableRunIDs(fx); got[bound] {
			t.Fatalf("provider run %s listed by the gap trigger although a capable worker is online", bound)
		}
	})

	t.Run("incapable full worker does not count as capable", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("old-full", capOf(1), false)
		fx.holdActive(full, 1)
		bound := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, bound, "sat-incapable")
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, bound)
		if got := saturationRunIDs(fx); got[bound] {
			t.Fatalf("provider run %s listed by the saturation trigger with no capable worker", bound)
		}
		if got := unplaceableRunIDs(fx); !got[bound] {
			t.Fatalf("provider run %s not listed by the gap trigger with no capable worker", bound)
		}
	})

	t.Run("vendor run with a full incapable worker is a saturation case, provider-capability irrelevant", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		full := fx.jobWorker("old-full", capOf(1), false)
		fx.holdActive(full, 1)
		vendor := queuedRunWithCaps(fx, []string{})
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, vendor)
		if got := saturationRunIDs(fx); !got[vendor] {
			t.Fatalf("vendor run %s not listed by the saturation trigger when the only worker is full", vendor)
		}
		if got := unplaceableRunIDs(fx); got[vendor] {
			t.Fatalf("vendor run %s listed by the gap trigger", vendor)
		}
	})
}
