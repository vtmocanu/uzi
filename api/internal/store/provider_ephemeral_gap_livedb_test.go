package store_test

import "testing"

func TestUnactivatedProviderRunNeverProvisionsEphemeralLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)
	worker := fx.worker("docker-no-provider", capOf(1), true)
	vendor := queuedRunWithCaps(fx, []string{"docker"})
	bound := queuedRunWithCaps(fx, []string{"docker"})
	p := newProvider(fx.ctx, t, fx.q, fx.userID, "dormant")
	key, err := insertProviderKey(fx.ctx, fx.q, fx.userID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET harness = 'claude',
        model_provider_id = $2, model_provider_revision = 1,
        model_provider_key_mode = 'user_key', model_provider_key_secret_id = $3,
        model_provider_key_rev = (SELECT enablement_rev FROM user_secrets WHERE id = $3)
        WHERE id = $1`, bound, p.ID, key.ID)
	// The worker has Docker but does not advertise claude_provider_v1. The
	// provider capability gap must not turn into a pod that cannot claim its run.
	got := unplaceableRunIDs(fx)
	if got[bound] {
		t.Error("unactivated provider run was listed for ephemeral provisioning")
	}
	if got[vendor] {
		t.Error("the online Docker worker should satisfy the vendor run")
	}
	// Positive provisioning control: ordinary capability gaps still provision.
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET status = 'offline' WHERE id = $1", worker)
	got = unplaceableRunIDs(fx)
	if !got[vendor] || got[bound] {
		t.Errorf("offline fleet: vendor listed=%t, provider listed=%t; want true, false", got[vendor], got[bound])
	}
}
