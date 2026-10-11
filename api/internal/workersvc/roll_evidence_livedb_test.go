package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRollEvidenceRegistrationHeartbeatLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user := env.seedUser(t)
	id := seedSnapshotWorker(t, env, user, "prior")
	env.exec("UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', name='user chosen' WHERE id=$1", id)
	svc := snapshotSvc(env, testParams())
	w, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pod := uuid.New()
	w, nonce, err := svc.RegisterWithCrossCheckSlots(env.ctx, w, "1.2.3", "base", nil, nil, nil, nil, nil, pod)
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "user chosen" || !w.RegisteredPodUid.Valid || uuid.UUID(w.RegisteredPodUid.Bytes) != pod || w.RollHeartbeatAt.Valid {
		t.Fatalf("register evidence/name incorrect: name=%q uid=%v heartbeat=%v", w.Name, w.RegisteredPodUid, w.RollHeartbeatAt)
	}
	for _, snap := range []*ActiveSnapshot{
		nil,
		{RegisterNonce: "prior", SnapshotEpoch: 100},
		{RegisterNonce: "", SnapshotEpoch: 100},
		{RegisterNonce: nonce, SnapshotEpoch: -1},
		{RegisterNonce: nonce, SnapshotEpoch: 0},
		{RegisterNonce: nonce, SnapshotEpoch: 1, Active: []ActiveRunEntry{{RunID: "malformed", Phase: "running"}}},
	} {
		w, err = svc.Heartbeat(env.ctx, w, nil, nil, snap)
		if err != nil {
			t.Fatal(err)
		}
		if w.RollHeartbeatAt.Valid {
			t.Fatal("missing/invalid snapshot proved heartbeat")
		}
	}
	w, err = svc.Heartbeat(env.ctx, w, nil, nil, &ActiveSnapshot{RegisterNonce: nonce, SnapshotEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := env.q.GetWorkerByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !w.RollHeartbeatAt.Valid || stored.SnapshotEpoch != 1 {
		t.Fatal("valid current registration heartbeat not proven")
	}
	proven := w.RollHeartbeatAt.Time
	w, err = svc.Heartbeat(env.ctx, w, nil, nil, &ActiveSnapshot{RegisterNonce: nonce, SnapshotEpoch: 1})
	if err != nil || !w.RollHeartbeatAt.Time.Equal(proven) {
		t.Fatalf("stale heartbeat moved proof: %v", err)
	}
	// A register-carried snapshot cannot prove a heartbeat, even with valid pod identity.
	w, _, err = svc.RegisterWithCrossCheckSlots(env.ctx, w, "1.2.4", "base", nil, nil, nil, nil, &ActiveSnapshot{SnapshotEpoch: 100}, pod)
	if err != nil {
		t.Fatal(err)
	}
	if !w.RegisteredPodUid.Valid || w.RollHeartbeatAt.Valid {
		t.Fatal("re-register identity/proof incorrect")
	}
	// Claim-carried snapshots must never stamp heartbeat proof.
	if applied, err := applySnapshot(t, svc, env, id, &ActiveSnapshot{RegisterNonce: w.SnapshotRegisterNonce.String, SnapshotEpoch: 101}, snapshotModeClaim); err != nil || !applied {
		t.Fatalf("claim snapshot: applied=%v err=%v", applied, err)
	}
	w, err = env.q.GetWorkerByID(env.ctx, id)
	if err != nil || w.RollHeartbeatAt.Valid {
		t.Fatalf("claim stamped heartbeat: %v", err)
	}
	w, err = svc.Heartbeat(env.ctx, w, nil, nil, &ActiveSnapshot{RegisterNonce: nonce, SnapshotEpoch: 1000})
	if err != nil || w.RollHeartbeatAt.Valid {
		t.Fatalf("previous-registration heartbeat stamped proof: %v", err)
	}
	w, _, err = svc.Register(env.ctx, w, "1.2.5", "base", nil, nil, nil, nil)
	if err != nil || w.RegisteredPodUid.Valid || w.RollHeartbeatAt.Valid {
		t.Fatalf("legacy register retained evidence: %v", err)
	}
}

func TestRollEvidencePollEligibilityDrainLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user := env.seedUser(t)
	id := seedSnapshotWorker(t, env, user, "registration")
	env.exec("UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base' WHERE id=$1", id)
	for _, slots := range []any{nil, 0, 2} {
		for _, lane := range []bool{false, true} {
			for _, checker := range []bool{false, true} {
				caps := []string{}
				if checker {
					caps = append(caps, "cross_check_v1")
				}
				if lane {
					caps = append(caps, "cross_check_lane_v1")
				}
				env.exec("UPDATE workers SET max_cross_check_slots=$2,protocol_capabilities=$3 WHERE id=$1", id, slots, caps)
				rows, err := env.q.ListHostedWorkersForController(env.ctx, store.ListHostedWorkersForControllerParams{})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.ID == id {
						found = true
						wantOrdinary := checker && slots == nil && !lane
						want := checker && slots == 2 && lane
						if row.OrdinaryCoverage != wantOrdinary || row.CheckerCoverage != want {
							t.Fatalf("slots=%v lane=%v: ordinary=%v checker=%v want=%v", slots, lane, row.OrdinaryCoverage, row.CheckerCoverage, want)
						}
					}
				}
				if !found {
					t.Fatal("worker absent")
				}
			}
		}
	}
	read := func() store.Worker {
		t.Helper()
		w, err := env.q.GetWorkerByID(env.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	cordon := func() {
		t.Helper()
		if n, err := env.q.CordonHostedWorkerImageRoll(env.ctx, id); err != nil || n != 1 {
			t.Fatalf("cordon %d %v", n, err)
		}
	}
	clear := func() {
		t.Helper()
		if n, err := env.q.UncordonHostedWorkerImageRoll(env.ctx, id); err != nil || n != 1 {
			t.Fatalf("clear %d %v", n, err)
		}
	}
	cordon()
	first := read()
	cordon()
	if !read().DrainingSince.Time.Equal(first.DrainingSince.Time) || first.DrainingReason.String != "image_roll" {
		t.Fatal("cordon ownership/deadline drift")
	}
	clear()
	if read().DrainingSince.Valid || read().DrainingReason.Valid {
		t.Fatal("image drain not cleared")
	}
	if _, err := env.q.CordonHostedWorker(env.ctx, id); err != nil {
		t.Fatal(err)
	}
	cordon()
	clear()
	if !read().DrainingSince.Valid || read().DrainingReason.Valid {
		t.Fatal("image clear erased legacy drain")
	}
	if _, err := env.q.UncordonHostedWorker(env.ctx, id); err != nil {
		t.Fatal(err)
	}
	cordon()
	env.exec("UPDATE workers SET maintenance_phase='requested',maintenance_fenced=true WHERE id=$1", id)
	clear()
	if !read().DrainingSince.Valid || read().DrainingReason.String != "image_roll" {
		t.Fatal("image clear erased maintenance fence")
	}
	env.exec("UPDATE workers SET maintenance_phase='',maintenance_fenced=false WHERE id=$1", id)
	clear()
	if read().DrainingSince.Valid {
		t.Fatal("completed independent fence did not allow clear")
	}
	// A generic liveness update cannot manufacture roll proof.
	env.exec("UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", id, time.Now())
	if read().RollHeartbeatAt.Valid {
		t.Fatal("liveness became roll evidence")
	}
}

func TestRollEvidenceLivenessProjectionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	id := seedSnapshotWorker(t, env, env.seedUser(t), "current-registration")
	env.exec("UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base' WHERE id=$1", id)
	svc := snapshotSvc(env, testParams())
	read := func() store.Worker {
		t.Helper()
		w, err := env.q.GetWorkerByID(env.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	pollRow := func() store.ListHostedWorkersForControllerRow {
		t.Helper()
		rows, err := env.q.ListHostedWorkersForController(env.ctx, store.ListHostedWorkersForControllerParams{})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == id {
				return row
			}
		}
		t.Fatal("worker absent from poll")
		return store.ListHostedWorkersForControllerRow{}
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status string
		at     time.Time
	}{
		{"legacy_fresh", "online", at},
		{"online_stale", "online", at.Add(-time.Hour)},
		{"offline_fresh", "offline", at},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env.exec("UPDATE workers SET status=$2,last_heartbeat_at=$3 WHERE id=$1", id, tc.status, tc.at)
			row := pollRow()
			if row.Status != tc.status || !row.LastHeartbeatAt.Valid || !row.LastHeartbeatAt.Time.Equal(tc.at) ||
				!row.SnapshotRegisterNonce.Valid || row.RegisteredPodUid.Valid || row.RollHeartbeatAt.Valid {
				t.Fatalf("legacy liveness/proof projection = %+v", row)
			}
		})
	}
	// A legacy plain heartbeat refreshes API liveness without pod-bound proof.
	w, err := svc.Heartbeat(env.ctx, read(), nil, nil, nil)
	if err != nil || !w.LastHeartbeatAt.Valid || w.RollHeartbeatAt.Valid {
		t.Fatalf("legacy heartbeat liveness/proof incorrect: %v", err)
	}
	// Establish proof through a pod-bound registration and actual current-nonce heartbeat.
	w, nonce, err := svc.RegisterWithCrossCheckSlots(env.ctx, w, "1.2.3", "base", nil, nil, nil, nil, nil, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	w, err = svc.Heartbeat(env.ctx, w, nil, nil, &ActiveSnapshot{RegisterNonce: nonce, SnapshotEpoch: 1})
	if err != nil || !w.RollHeartbeatAt.Valid {
		t.Fatalf("current-nonce heartbeat did not stamp proof: %v", err)
	}
	// Age that proof independently, then send newer ordinary and invalid-nonce heartbeats.
	proven := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	env.exec("UPDATE workers SET roll_heartbeat_at=$2,last_heartbeat_at=$2 WHERE id=$1", id, proven)
	for _, snap := range []*ActiveSnapshot{nil, {RegisterNonce: "invalid-registration", SnapshotEpoch: 2}} {
		w, err = svc.Heartbeat(env.ctx, read(), nil, nil, snap)
		if err != nil {
			t.Fatal(err)
		}
		row := pollRow()
		if row.Status != "online" || !row.LastHeartbeatAt.Valid || !row.LastHeartbeatAt.Time.After(proven) ||
			!row.RollHeartbeatAt.Valid || !row.RollHeartbeatAt.Time.Equal(proven) {
			t.Fatalf("ordinary/invalid-nonce heartbeat moved proof or lost liveness: %+v", row)
		}
	}
	w, err = svc.Heartbeat(env.ctx, read(), nil, nil, &ActiveSnapshot{RegisterNonce: nonce, SnapshotEpoch: 2})
	if err != nil || !w.RollHeartbeatAt.Valid || !w.RollHeartbeatAt.Time.After(proven) {
		t.Fatalf("new current-nonce heartbeat did not advance proof: %v", err)
	}
}

func TestRollEvidenceOfflineLadderLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	run := e.seedQueuedRun(t, nil, nil)
	worker := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE workers SET draining_since=now(),status='offline' WHERE id=$1", worker)
	row := e.healthRowFor(t, run)
	if got := svc.queuedReason(e.ctx, time.Now(), row); got != reasonNoWorker {
		t.Fatalf("offline roll reason=%q want=%q", got, reasonNoWorker)
	}
	t.Logf("observed offline-mid-roll reason: %s", reasonNoWorker)
}
