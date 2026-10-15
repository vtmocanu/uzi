package workersvc

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHeartbeatProtocolRefreshUnionAndIncarnationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	nonce, err := mintSnapshotNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := seedSnapshotWorker(t, env, user, nonce)
	env.exec("UPDATE workers SET protocol_capabilities=$2, capabilities=$3, snapshot_epoch=7 WHERE id=$1",
		id, []string{"formerly_known_v1"}, []string{"docker"})
	run := seedOutageRun(t, env, user, repo, id, "running", "issue", 1, 0)
	beforeRun, err := env.q.GetRunByID(env.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Heartbeat(env.ctx, prior, nil, nil, nil, &ProtocolRefresh{
		RegisterNonce: nonce, ProtocolCapabilities: []string{capability.CrossCheckLaneV1, "docker", "jvm"},
		MaxConcurrentRuns: 8, MaxCrossCheckSlots: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.MaxConcurrentRuns.Valid || updated.MaxConcurrentRuns.Int32 != 8 ||
		!updated.MaxCrossCheckSlots.Valid || updated.MaxCrossCheckSlots.Int32 != 4 ||
		!slices.Contains(updated.ProtocolCapabilities, "formerly_known_v1") ||
		!slices.Contains(updated.ProtocolCapabilities, capability.CrossCheckLaneV1) ||
		slices.Contains(updated.ProtocolCapabilities, "docker") || slices.Contains(updated.ProtocolCapabilities, "jvm") {
		t.Fatalf("refresh failed union/NULL init: %+v", updated)
	}
	if updated.SnapshotRegisterNonce != prior.SnapshotRegisterNonce || updated.SnapshotEpoch != 7 ||
		!reflect.DeepEqual(updated.TokenHash, prior.TokenHash) || !reflect.DeepEqual(updated.Capabilities, prior.Capabilities) {
		t.Fatal("refresh changed incarnation or scheduling authority")
	}
	afterRun, err := env.q.GetRunByID(env.ctx, run)
	if err != nil || !reflect.DeepEqual(beforeRun, afterRun) {
		t.Fatalf("refresh replayed or changed owned work: %v", err)
	}
	// Empty, duplicate and lower reports cannot shrink either limits or names.
	updated, err = svc.Heartbeat(env.ctx, prior, nil, nil, nil, &ProtocolRefresh{
		RegisterNonce: nonce, MaxConcurrentRuns: 1, MaxCrossCheckSlots: 0,
	})
	if err != nil || updated.MaxConcurrentRuns.Int32 != 8 || updated.MaxCrossCheckSlots.Int32 != 4 ||
		!slices.Contains(updated.ProtocolCapabilities, "formerly_known_v1") ||
		!slices.Contains(updated.ProtocolCapabilities, capability.CrossCheckLaneV1) {
		t.Fatalf("empty refresh shrank state: %+v %v", updated, err)
	}
	// Real registration replaces the set/limits and rotates the nonce. The old
	// middleware row must not authorize a refresh after this competing register.
	one, zero := 1, 0
	registered, newNonce, err := svc.RegisterWithCrossCheckSlots(env.ctx, updated, "v2", "base", &one, &zero, nil, nil, nil, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := svc.Heartbeat(env.ctx, prior, nil, nil, nil, &ProtocolRefresh{
		RegisterNonce: nonce, ProtocolCapabilities: []string{capability.CrossCheckLaneV1},
		MaxConcurrentRuns: 256, MaxCrossCheckSlots: 16,
	})
	if err != nil || newNonce == nonce || stale.SnapshotRegisterNonce != registered.SnapshotRegisterNonce ||
		stale.MaxConcurrentRuns.Int32 != 1 || stale.MaxCrossCheckSlots.Int32 != 0 ||
		slices.Contains(stale.ProtocolCapabilities, capability.CrossCheckLaneV1) || slices.Contains(stale.ProtocolCapabilities, "formerly_known_v1") {
		t.Fatalf("old incarnation refresh applied after register: %+v %v", stale, err)
	}
}

func TestHeartbeatProtocolRefreshConcurrentMaxLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user := env.seedUser(t)
	svc := snapshotSvc(env, testParams())
	nonce, err := mintSnapshotNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := seedSnapshotWorker(t, env, user, nonce)
	w, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Two independent heartbeats start together. Each is bounded by the context;
	// either error is reported after both siblings finish.
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, refresh := range []*ProtocolRefresh{
		{RegisterNonce: nonce, ProtocolCapabilities: []string{capability.CrossCheckLaneV1}, MaxConcurrentRuns: 32, MaxCrossCheckSlots: 2},
		{RegisterNonce: nonce, ProtocolCapabilities: []string{capability.InputReceiptsV1}, MaxConcurrentRuns: 2, MaxCrossCheckSlots: 16},
	} {
		go func(r *ProtocolRefresh) {
			<-start
			_, err := svc.Heartbeat(ctx, w, nil, nil, nil, r)
			results <- err
		}(refresh)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	current, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil || current.MaxConcurrentRuns.Int32 != 32 || current.MaxCrossCheckSlots.Int32 != 16 ||
		!slices.Contains(current.ProtocolCapabilities, capability.CrossCheckLaneV1) ||
		!slices.Contains(current.ProtocolCapabilities, capability.InputReceiptsV1) {
		t.Fatalf("concurrent reports lost union/max: %+v %v", current, err)
	}
}

func TestHeartbeatProtocolRefreshEphemeralAndSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	nonce, err := mintSnapshotNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := seedSnapshotWorker(t, env, user, nonce)
	env.exec("UPDATE workers SET max_concurrent_runs=1, max_cross_check_slots=0 WHERE id=$1", id)
	w, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	snap := &ActiveSnapshot{RegisterNonce: nonce, SnapshotEpoch: 1}
	for range 5 {
		run := seedOutageRun(t, env, user, repo, id, "running", "issue", 1, 0)
		snap.Active = append(snap.Active, entry(run, 1, "running", false))
	}
	// Old cap is 1+2, refreshed cross-check lane adds two live entries.
	current, err := svc.Heartbeat(env.ctx, w, nil, nil, snap, &ProtocolRefresh{
		RegisterNonce: nonce, ProtocolCapabilities: []string{capability.CrossCheckLaneV1},
		MaxConcurrentRuns: 1, MaxCrossCheckSlots: 2,
	})
	if err != nil || countActiveRuns(t, env, id) != 5 {
		t.Fatalf("snapshot validated before refresh: %v", err)
	}
	env.exec("UPDATE workers SET ephemeral=true WHERE id=$1", id)
	current, err = svc.Heartbeat(env.ctx, current, nil, nil, nil, &ProtocolRefresh{
		RegisterNonce: nonce, MaxConcurrentRuns: 256, MaxCrossCheckSlots: 0,
	})
	if err != nil || current.MaxConcurrentRuns.Int32 != 1 {
		t.Fatalf("ephemeral cap = %v, err %v", current.MaxConcurrentRuns, err)
	}
}

func TestHeartbeatProtocolRefreshDindNextTickLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user := env.seedUser(t)
	p := testParams()
	p.DiskPressureThreshold = 0.9
	svc := snapshotSvc(env, p)
	nonce, err := mintSnapshotNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := seedSnapshotWorker(t, env, user, nonce)
	env.exec("UPDATE workers SET dind_register_floor=now()-interval '1 minute' WHERE id=$1", id)
	w, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Truncate(time.Second).Add(-time.Second)
	used, total := int64(95), int64(100)
	stats := &WorkerStats{DindMeter: &DindMeter{RegisterNonce: nonce, Epoch: at.Unix(), SampledAt: at},
		DiskDindBytes: &used, DiskDindTotalBytes: &total, DiskDindInodes: &used, DiskDindTotalInodes: &total}
	current, err := svc.Heartbeat(env.ctx, w, stats, nil, nil, &ProtocolRefresh{
		RegisterNonce: nonce, ProtocolCapabilities: []string{capability.DindMaintenanceV1},
		MaxConcurrentRuns: 1, MaxCrossCheckSlots: 0,
	})
	if err != nil || !slices.Contains(current.ProtocolCapabilities, capability.DindMaintenanceV1) || current.DindMeterEpoch != 0 {
		t.Fatalf("new DinD capability must persist without same-tick meter: %+v %v", current, err)
	}
	current, err = svc.Heartbeat(env.ctx, current, stats, nil, nil)
	if err != nil || current.DindMeterEpoch != at.Unix() || current.DindPressureStreak != 1 {
		t.Fatalf("ordinary next tick did not accept telemetry: epoch %d streak %d err %v", current.DindMeterEpoch, current.DindPressureStreak, err)
	}
}

func TestHeartbeatProtocolRefreshImmediatelyClaimableLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	id := e.seedWorker(t, nil)
	runID := e.seedCodexQueuedRun(t)
	nonce, err := mintSnapshotNonce()
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, "UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", id, nonce)
	w, err := e.q.GetWorkerByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(id, nil, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("incapable worker claimed before refresh: %v", err)
	}
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(e.q, box, testParams())
	svc.SetTxBeginner(e.pool)
	current, err := svc.Heartbeat(e.ctx, w, nil, nil, nil, &ProtocolRefresh{
		RegisterNonce: nonce, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2},
		MaxConcurrentRuns: 1, MaxCrossCheckSlots: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.q.ClaimRun(e.ctx, e.claimParams(id, current.ProtocolCapabilities, false))
	if err != nil || claimed.ID != runID || claimed.Status != "claimed" {
		t.Fatalf("new protocol capability did not immediately admit claim: %v, %v", claimed.ID, err)
	}
}

// RefreshWorkerProtocol's observable store seam must modify exactly three fields.
func TestHeartbeatProtocolRefreshOnlyThreeFieldsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user := env.seedUser(t)
	nonce, err := mintSnapshotNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := seedSnapshotWorker(t, env, user, nonce)
	before, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	after, err := env.q.RefreshWorkerProtocol(env.ctx, store.RefreshWorkerProtocolParams{
		ID: id, RegisterNonce: nonce, ProtocolCapabilities: []string{capability.InputReceiptsV1},
		MaxConcurrentRuns: 4, MaxCrossCheckSlots: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	after.ProtocolCapabilities = before.ProtocolCapabilities
	after.MaxConcurrentRuns = before.MaxConcurrentRuns
	after.MaxCrossCheckSlots = before.MaxCrossCheckSlots
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refresh SQL changed another worker field")
	}
}
