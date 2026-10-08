package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func newDockerPlacementFixture(t *testing.T) *ephemeralFixture {
	t.Helper()
	fx := newEphemeralFixture(t, true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fx.pool.Exec(ctx, "DELETE FROM users WHERE id=$1", fx.userID); err != nil {
			t.Errorf("cleanup Docker placement fixture: %v", err)
		}
	})
	return fx
}

func (fx *ephemeralFixture) sealDockerClaimCredentials() {
	fx.t.Helper()
	for _, kind := range []string{"forge", "anthropic"} {
		sealed, err := fx.box.Seal([]byte("test-credential-" + kind))
		if err != nil {
			fx.t.Fatal(err)
		}
		if kind == "forge" {
			cliMustExec(fx.t, fx.pool, "UPDATE forge_connections SET token_ciphertext=$2 WHERE user_id=$1", fx.userID, sealed)
		} else {
			cliMustExec(fx.t, fx.pool, "INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'anthropic_token','seam',true,$3,'master')", uuid.New(), fx.userID, sealed)
		}
	}
}

// dockerPlacementSnapshot preserves an explicitly nonempty nonmatching allowlist.
func (fx *ephemeralFixture) dockerPlacementSnapshot(pref, tier bool, list string, fail bool, max int) (*hostedsvc.EphemeralProvisioner, *dockerAllowlistProbe, []uuid.UUID) {
	fx.t.Helper()
	_, probe := fx.dockerProvisioner(pref, tier, list == "target", max, fail)
	ids := []uuid.UUID{}
	value := ""
	if list != "empty" {
		id := fx.repoID
		if list == "other" {
			id = uuid.New()
		}
		ids = append(ids, id)
		value = id.String()
	}
	probe.values = ids
	probe.Cache = settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyEphemeralWorkersEnabled, Value: "true"},
		{Key: settings.KeyDockerRepoAllowlist, Value: value},
	}}, time.Minute)
	p := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, probe, hostedsvc.EphemeralConfig{BackgroundGrace: 5 * time.Minute,
		DockerEnabled: tier, MaxPerUser: max, DefaultSize: "m", Lease: 2 * time.Hour, SaturationDelay: time.Hour,
	})
	return p, probe, ids
}

func (fx *ephemeralFixture) dockerClaimService(probe *dockerAllowlistProbe, tier bool) *workersvc.Service {
	fx.t.Helper()
	fx.sealDockerClaimCredentials()
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{
		WorkerHeartbeatStale: time.Minute, WorkerAffinityCeiling: time.Minute, WorkerSpreadGrace: time.Hour,
	})
	svc.SetTxBeginner(fx.pool)
	svc.SetEphemeralLease(2 * time.Hour)
	svc.SetDockerAllowlist(probe)
	svc.SetEffectiveDockerTier(tier)
	svc.SetBackground(func(func()) {})
	return svc
}

func (fx *ephemeralFixture) assertDockerAvailability(run uuid.UUID, tier bool, ids []uuid.UUID, want int64) {
	fx.t.Helper()
	now := time.Now()
	row, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID: run, WorkerDockerEnabled: tier, DockerRepoAllowlist: ids,
		EphemeralLease: workersvc.LeaseInterval(2 * time.Hour), CapabilityAware: true,
		HeartbeatCutoff:    pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		AffinityCutoff:     pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		CodexCuratedModels: workersvc.CodexCuratedModels(),
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	if row.Claimable != want {
		fx.t.Fatalf("claimable=%d want=%d", row.Claimable, want)
	}
	if row.NonDrainingEligible != want {
		fx.t.Fatalf("non_draining_eligible=%d want=%d", row.NonDrainingEligible, want)
	}
}

func TestEphemeralDockerWarmClaimBaselineLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, list               string
		pref, tier, fail, docker bool
	}{
		{"preference off", "target", false, true, false, false},
		{"tier off", "target", true, false, false, false},
		{"nonallowlisted", "other", true, true, false, false},
		{"empty list", "empty", true, true, false, false},
		{"values with error", "target", true, true, true, false},
		{"Docker warm eligible", "target", true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDockerPlacementFixture(t)
			warm := fx.leasedWorker(time.Minute)
			cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=$2 WHERE id=$1", warm.id, tc.docker)
			run := fx.followUp(warm)
			_, probe, ids := fx.dockerPlacementSnapshot(tc.pref, tc.tier, tc.list, tc.fail, 2)
			// Claim discards values accompanying an error. The real empty-list count
			// also backs TestEphemeralDockerHealthReleasedAllowlistFallback.
			if tc.fail {
				ids = []uuid.UUID{}
			}
			fx.assertDockerAvailability(run, tc.tier, ids, 1)
			svc := fx.dockerClaimService(probe, tc.tier)
			worker, err := fx.q.GetWorkerByID(fx.ctx, warm.id)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := svc.Claim(fx.ctx, worker, nil)
			if err != nil || payload == nil || payload.RunID != run.String() {
				t.Fatalf("warm claim=%+v err=%v want=%s", payload, err, run)
			}
			status, owner := fx.runState(run)
			if status != "claimed" || !owner.Valid || owner.Bytes != warm.id {
				t.Fatalf("status=%s owner=%v want warm worker", status, owner)
			}
			rebound, err := fx.q.GetWorkerByID(fx.ctx, warm.id)
			if err != nil || !rebound.EphemeralRunID.Valid || rebound.EphemeralRunID.Bytes != run {
				t.Fatalf("warm rebind=%+v err=%v", rebound.EphemeralRunID, err)
			}
		})
	}
}

func TestEphemeralDockerAvailabilityLeaseAdmissionLiveDB(t *testing.T) {
	fx := newDockerPlacementFixture(t)
	warm := fx.leasedWorker(time.Minute)
	cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=false WHERE id=$1", warm.id)
	run := fx.followUp(warm)
	_, _, ids := fx.dockerPlacementSnapshot(true, true, "target", false, 2)
	fx.assertDockerAvailability(run, true, ids, 0)
	cliMustExec(t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=false WHERE id=$1", fx.userID)
	fx.assertDockerAvailability(run, true, ids, 1)
}

func TestEphemeralDockerPlainAlternativesClaimLiveDB(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound=%t", bound), func(t *testing.T) {
			fx := newDockerPlacementFixture(t)
			run := fx.queuedRun([]string{})
			workerID := fx.onlineWorkerWithCap("plain", false, 2)
			if bound {
				cliMustExec(t, fx.pool, "UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,kind='hosted',hosted_size='m',template_declared='base' WHERE id=$1", workerID, run)
			}
			_, probe, ids := fx.dockerPlacementSnapshot(true, true, "target", false, 2)
			fx.assertDockerAvailability(run, true, ids, 1)
			svc := fx.dockerClaimService(probe, true)
			worker, err := fx.q.GetWorkerByID(fx.ctx, workerID)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := svc.Claim(fx.ctx, worker, nil)
			if err != nil || payload == nil || payload.RunID != run.String() {
				t.Fatalf("plain alternative claim=%+v err=%v want=%s", payload, err, run)
			}
		})
	}
}

func TestEphemeralDockerNoWorkersGapBaselineLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, list string
		fail, want bool
	}{
		{"nonallowlisted", "other", false, false},
		{"empty list", "empty", false, false},
		{"values with error", "target", true, false},
		{"eligible immediate gap", "target", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDockerPlacementFixture(t)
			run := fx.queuedRun([]string{})
			p, _, ids := fx.dockerPlacementSnapshot(true, true, tc.list, tc.fail, 2)
			if tc.fail {
				ids = []uuid.UUID{}
			}
			gaps, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
				MaxRows: 10000, MaxPerUser: 2, EphemeralLease: workersvc.LeaseInterval(2 * time.Hour),
				WorkerDockerEnabled: true, DockerRepoAllowlist: ids, CodexCuratedModels: workersvc.CodexCuratedModels(),
			})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, gap := range gaps {
				if gap.ID == run {
					found = true
				}
			}
			if found != tc.want {
				t.Fatalf("gap candidate=%v want=%v", found, tc.want)
			}
			if _, err := p.ProvisionPass(fx.ctx); err != nil {
				t.Fatal(err)
			}
			rows := fx.ephemeralRows()
			if !tc.want && len(rows) != 0 {
				t.Fatalf("baseline provisioned=%+v", rows)
			}
			if tc.want && (len(rows) != 1 || rows[0].runID != run || !rows[0].docker) {
				t.Fatalf("eligible gap workers=%+v", rows)
			}
		})
	}
}

func TestEphemeralDockerSaturationLeaseMirrorsLiveDB(t *testing.T) {
	for _, freeWarm := range []bool{false, true} {
		t.Run(fmt.Sprintf("free warm=%t", freeWarm), func(t *testing.T) {
			fx := newDockerPlacementFixture(t)
			warm := fx.leasedWorker(time.Minute)
			cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=false,max_concurrent_runs=1 WHERE id=$1", warm.id)
			if freeWarm {
				busy := fx.onlineWorkerWithCap("persistent busy", false, 1)
				fx.activeRunOn(busy)
			} else {
				fx.busyRunOn(warm)
			}
			run := fx.followUp(warm)
			cliMustExec(t, fx.pool, "UPDATE runs SET status_since=now()-interval '2 hours' WHERE id=$1", run)
			_, _, ids := fx.dockerPlacementSnapshot(true, true, "target", false, 2)
			for _, pref := range []bool{true, false} {
				cliMustExec(t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=$2 WHERE id=$1", fx.userID, pref)
				rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
					MaxRows: 10000, MaxPerUser: 2, SaturationDelay: workersvc.LeaseInterval(time.Hour),
					EphemeralLease: workersvc.LeaseInterval(2 * time.Hour), WorkerDockerEnabled: true,
					DockerRepoAllowlist: ids, CodexCuratedModels: workersvc.CodexCuratedModels(),
				})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.ID == run {
						found = true
					}
				}
				want := pref == freeWarm
				if found != want {
					t.Fatalf("freeWarm=%v preference=%v saturation candidate=%v want=%v", freeWarm, pref, found, want)
				}
			}
		})
	}
}

func TestEphemeralDockerPersistentSaturationDebounceLiveDB(t *testing.T) {
	fx := newDockerPlacementFixture(t)
	busy := fx.onlineWorkerWithCap("persistent busy", false, 1)
	fx.activeRunOn(busy)
	run := fx.queuedRun([]string{})
	p, _, ids := fx.dockerPlacementSnapshot(true, true, "target", false, 2)
	gaps, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
		MaxRows: 10000, MaxPerUser: 2, EphemeralLease: workersvc.LeaseInterval(2 * time.Hour),
		WorkerDockerEnabled: true, DockerRepoAllowlist: ids, CodexCuratedModels: workersvc.CodexCuratedModels(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, gap := range gaps {
		if gap.ID == run {
			t.Fatal("busy persistent placement incorrectly became an immediate gap")
		}
	}
	for _, old := range []bool{false, true} {
		if old {
			cliMustExec(t, fx.pool, "UPDATE runs SET status_since=now()-interval '2 hours' WHERE id=$1", run)
		}
		rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
			MaxRows: 10000, MaxPerUser: 2, SaturationDelay: workersvc.LeaseInterval(time.Hour),
			EphemeralLease: workersvc.LeaseInterval(2 * time.Hour), WorkerDockerEnabled: true,
			DockerRepoAllowlist: ids, CodexCuratedModels: workersvc.CodexCuratedModels(),
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.ID == run {
				found = true
			}
		}
		if found != old {
			t.Fatalf("past debounce=%v candidate=%v", old, found)
		}
		if _, err := p.ProvisionPass(fx.ctx); err != nil {
			t.Fatal(err)
		}
		workers := fx.ephemeralRows()
		if !old && len(workers) != 0 {
			t.Fatalf("provisioned before debounce: %+v", workers)
		}
		if old && (len(workers) != 1 || workers[0].runID != run || !workers[0].docker) {
			t.Fatalf("missing Docker worker after debounce: %+v", workers)
		}
	}
}

func TestEphemeralDockerFleetSpreadLeasePeerLiveDB(t *testing.T) {
	fx := newDockerPlacementFixture(t)
	peer := fx.leasedWorker(time.Minute)
	cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=false WHERE id=$1", peer.id)
	claimant := fx.onlineWorkerWithCap("persistent claimant", false, 2)
	fx.activeRunOn(claimant)
	run := fx.followUp(peer)
	_, probe, _ := fx.dockerPlacementSnapshot(false, true, "target", false, 2)
	svc := fx.dockerClaimService(probe, true)
	worker, err := fx.q.GetWorkerByID(fx.ctx, claimant)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := svc.Claim(fx.ctx, worker, nil)
	if err != nil || payload != nil {
		t.Fatalf("preference off: claim=%+v err=%v want deferred", payload, err)
	}
	status, owner := fx.runState(run)
	if status != "queued" || owner.Valid {
		t.Fatalf("deferred status=%s owner=%v", status, owner)
	}
	// Only preference changes: the persistent claimant remains a valid alternative,
	// but its strictly less loaded plain leased peer is now excluded.
	cliMustExec(t, fx.pool, "UPDATE users SET ephemeral_docker_enabled=true WHERE id=$1", fx.userID)
	payload, err = svc.Claim(fx.ctx, worker, nil)
	if err != nil || payload == nil || payload.RunID != run.String() {
		t.Fatalf("preference on: claim=%+v err=%v want=%s", payload, err, run)
	}
	status, owner = fx.runState(run)
	if status != "claimed" || !owner.Valid || owner.Bytes != claimant {
		t.Fatalf("claim status=%s owner=%v want persistent claimant", status, owner)
	}
}

func TestEphemeralDockerEarlyCapEvictionLiveDB(t *testing.T) {
	fx := newDockerPlacementFixture(t)
	warm := fx.leasedWorker(time.Minute)
	cliMustExec(t, fx.pool, "UPDATE workers SET docker_enabled=false WHERE id=$1", warm.id)
	run := fx.followUp(warm)
	p, _, _ := fx.dockerPlacementSnapshot(true, true, "target", false, 1)
	if _, err := p.ProvisionPass(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if fx.workerExists(warm.id) {
		t.Fatal("unexpired plain warm lease was not evicted at cap")
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 || rows[0].id == warm.id || rows[0].runID != run || !rows[0].docker {
		t.Fatalf("cap=1 replacement workers=%+v", rows)
	}
	if _, err := p.ProvisionPass(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if rows = fx.ephemeralRows(); len(rows) != 1 || rows[0].runID != run || !rows[0].docker {
		t.Fatalf("second pass exceeds cap or changes binding: %+v", rows)
	}
}
