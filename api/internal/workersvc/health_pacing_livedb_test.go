package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthPacingStaticAndFenceLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"owner", "UPDATE workers SET user_id=$2 WHERE id=$1"},
		{"capability", "UPDATE workers SET capabilities='{}' WHERE id=$1"},
		{"protocol", "UPDATE runs SET completion_contract_version=1 WHERE id=$1"},
		{"lane", "UPDATE workers SET isolated_lane=true,ephemeral=true WHERE id=$1"},
		{"maintenance", "UPDATE workers SET maintenance_fenced=true WHERE id=$1"},
		{"maintenance-phase", "UPDATE workers SET maintenance_phase='requested' WHERE id=$1"},
		{"pressure", "UPDATE workers SET stats_disk_pressure_streak=2,draining_since=now(),draining_reason=NULL WHERE id=$1"},
		{"dind-pressure", "UPDATE workers SET dind_pressure_streak=2,maintenance_fenced=true,draining_since=now(),draining_reason=NULL WHERE id=$1"},
		{"independent-drain", "UPDATE workers SET draining_since=now(),draining_reason=NULL WHERE id=$1"},
		{"deleted", "DELETE FROM workers WHERE id=$1"},
		{"custody", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			run := e.seedQueuedRun(t, nil, []string{"gpu"})
			w := e.seedHeartbeatWorker(t, nil)
			now := time.Now().UTC()
			e.exec(t, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',capabilities='{gpu}',status='offline' WHERE id=$1", w)
			if _, err := svc.RecordRollHealth(e.ctx, RollHealthReport{WorkerID: w, Kind: "fleet", Phase: PhaseStuck, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: "held", TargetImage: "repo:bad"}}); err != nil {
				t.Fatal(err)
			}
			if got := svc.queuedReason(e.ctx, now, e.healthRowFor(t, run)); !strings.Contains(got, "worker roll") {
				t.Fatalf("baseline %q", got)
			}
			switch tc.name {
			case "owner":
				other := uuid.New()
				e.exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", other, other.String()+"@e2e")
				e.exec(t, tc.change, w, other)
			case "custody":
				e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
				VALUES($1,$2,$3,$4,1,'open',$5,'fixture',$5,$4,false)`, uuid.New(), e.userID, e.repoID, run, w)
			case "protocol":
				e.exec(t, tc.change, run)
			default:
				e.exec(t, tc.change, w)
			}
			if got := svc.queuedReason(e.ctx, now, e.healthRowFor(t, run)); strings.Contains(got, "worker roll") {
				t.Fatalf("false attribution %q", got)
			}
		})
	}
}

func TestHealthPacingCheckerEligibilityLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"eligible", ""},
		{"zero-slots", "UPDATE workers SET max_cross_check_slots=0 WHERE id=$1"},
		{"missing-lane-protocol", "UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1"},
		{"missing-runtime", "UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'codex_runtime_v2') WHERE id=$1"},
		{"checker-family", "UPDATE runs SET harness='claude',codex_secret_id=NULL,codex_material_revision=NULL WHERE id=$1"},
		{"affinity", ""},
		{"expired-affinity", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := laneFixture(t)
			now := time.Now().UTC()
			f.env.exec("UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',status='offline',last_heartbeat_at=$2 WHERE id=$1", f.workerID, now.Add(-time.Hour))
			if _, err := f.svc.RecordRollHealth(f.env.ctx, RollHealthReport{WorkerID: f.workerID, Kind: "fleet", Phase: PhaseStuck, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: "held", TargetImage: "repo:bad"}}); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "checker-family":
				f.env.exec(tc.change, f.runID)
			case "affinity", "expired-affinity":
				other := uuid.New()
				f.env.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'affinity',$3,'offline')", other, f.userID, other[:])
				updated := now
				if tc.name == "expired-affinity" {
					updated = now.Add(-time.Hour)
				}
				f.env.exec("UPDATE runs SET worker_id=$2,updated_at=$3 WHERE id=$1", f.runID, other, updated)
			default:
				if tc.change != "" {
					f.env.exec(tc.change, f.workerID)
				}
			}
			counts, err := f.svc.WorkerEligibilityForHealth(f.env.ctx, now, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.name == "eligible" || tc.name == "expired-affinity"
			if got := rollWaitReason(now, counts); strings.Contains(got, "worker roll") != want {
				t.Fatalf("reason %q counts %+v", got, counts)
			}
			if counts.StrictEligible != 0 || counts.Claimable != 0 {
				t.Fatalf("offline changed old counts %+v", counts)
			}
		})
	}
}

func TestHealthPacingProfilePlacementLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 901, true)
	now := time.Now().UTC()
	f.env.exec("UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',isolated_lane=true,ephemeral=true,protocol_capabilities='{isolated_fetch_v1}',status='offline' WHERE id=$1", f.workerID)
	f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", run, f.workerID)
	if _, err := f.svc.RecordRollHealth(f.env.ctx, RollHealthReport{WorkerID: f.workerID, Kind: "fleet", Phase: PhaseStuck, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: "held", TargetImage: "repo:bad"}}); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, CodexCuratedModels())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == run {
				return f.svc.queuedReason(f.env.ctx, now, r)
			}
		}
		t.Fatal("run missing")
		return ""
	}
	if got := read(); got != reasonWaitingIsolatedLane {
		t.Fatalf("ephemeral provisioning replaced %q", got)
	}
	// ck_workers_isolated_lane requires hosted + ephemeral. A held report
	// cannot override the existing isolated provisioner's explicit switch.
	f.svc.ephemeralSettings = fakeEphemeralSettings{on: false}
	if got := read(); got != reasonIsolatedLaneProvisioningOff {
		t.Fatalf("disabled provisioner concealed: %q", got)
	}
	counts, err := f.svc.WorkerEligibilityForHealth(f.env.ctx, now, run)
	if err != nil || counts.DiagnosticSuitable != 0 || counts.DiagnosticRolling != 0 {
		t.Fatalf("profile diagnostic %+v %v", counts, err)
	}
}

func TestHealthPacingOfflineLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	run := e.seedQueuedRun(t, nil, []string{"gpu"})
	w := e.seedHeartbeatWorker(t, nil)
	now := time.Now().UTC()
	e.exec(t, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',capabilities='{gpu}',status='offline',last_heartbeat_at=$2 WHERE id=$1", w, now.Add(-time.Hour))
	row := e.healthRowFor(t, run)
	if got := svc.queuedReason(e.ctx, now, row); got != reasonNoWorker {
		t.Fatalf("baseline %q", got)
	}
	reason := "original ImagePullBackOff"
	if _, err := svc.RecordRollHealth(e.ctx, RollHealthReport{WorkerID: w, Kind: "fleet", Phase: PhaseStuck, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: "held", TargetImage: "repo:bad", BlockingReason: &reason}}); err != nil {
		t.Fatal(err)
	}
	got := svc.queuedReason(e.ctx, now, row)
	if !strings.Contains(got, "waiting for worker roll (held) to repo:bad: original ImagePullBackOff") {
		t.Fatalf("held %q", got)
	}
	counts, err := svc.WorkerEligibilityForHealth(e.ctx, now, run)
	if err != nil || counts.StrictEligible != 0 || counts.Claimable != 0 || counts.DiagnosticSuitable != 1 || counts.DiagnosticRolling != 1 {
		t.Fatalf("counts %+v %v", counts, err)
	}
	// The old registration may remain API-online briefly after Recreate. The
	// fresh pod failure still explains a wait before that generic heartbeat expires.
	e.exec(t, "UPDATE workers SET status='online',last_heartbeat_at=$2 WHERE id=$1", w, now)
	if got := svc.queuedReason(e.ctx, now, row); !strings.Contains(got, "worker roll") {
		t.Fatalf("cached old heartbeat concealed replacement failure: %q", got)
	}
	e.exec(t, "UPDATE workers SET status='offline',last_heartbeat_at=$2 WHERE id=$1", w, now.Add(-time.Hour))
	// A fresh readiness receipt must not prolong old pacing.
	e.exec(t, "UPDATE worker_upgrade_reports SET pacing_observed_at=$2,observed_at=$3 WHERE worker_id=$1", w, now.Add(-time.Minute-time.Second), now)
	if got := svc.queuedReason(e.ctx, now, row); got != reasonNoWorker {
		t.Fatalf("expired %q", got)
	}
	e.exec(t, "UPDATE worker_upgrade_reports SET pacing_observed_at=$2 WHERE worker_id=$1", w, now)
	e.exec(t, "UPDATE workers SET capabilities='{}' WHERE id=$1", w)
	if got := svc.queuedReason(e.ctx, now, row); strings.Contains(got, "worker roll") {
		t.Fatalf("incompatible %q", got)
	}
	e.exec(t, "UPDATE workers SET capabilities='{gpu}' WHERE id=$1", w)
	peer := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE workers SET capabilities='{gpu}',max_concurrent_runs=1 WHERE id=$1", peer)
	e.seedActiveRunOwnedBy(t, peer)
	if got := svc.queuedReason(e.ctx, now, row); got != reasonAllWorkersBusy {
		t.Fatalf("healthy retained busy %q", got)
	}
	// A retained provider with a stuck report still vetoes roll attribution while busy.
	if _, err := svc.RecordRollHealth(e.ctx, RollHealthReport{WorkerID: peer, Kind: "fleet", Phase: PhaseStuck, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: "held", TargetImage: "repo:bad"}}); err != nil {
		t.Fatal(err)
	}
	if got := svc.queuedReason(e.ctx, now, row); got != reasonAllWorkersBusy {
		t.Fatalf("retained with stuck report %q", got)
	}
	e.exec(t, "DELETE FROM worker_upgrade_reports WHERE worker_id=$1", peer)
	// An offline suitable provider with no roll context vetoes exclusivity too.
	e.exec(t, "UPDATE workers SET status='offline',last_heartbeat_at=$2 WHERE id=$1", peer, now.Add(-time.Hour))
	if got := svc.queuedReason(e.ctx, now, row); got != reasonNoWorker {
		t.Fatalf("unexplained offline %q", got)
	}
}

// Active healthy drain-only waits retain the exact stored exemption. Held,
// evidence-blocked and replacement-gap waits remain genuine queue waits.
func TestHealthPacingDrainOnlyLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, state, phase     string
		offline, mixed, legacy bool
	}{
		{"active-drain-only", "active", PhaseSettled, false, false, true},
		{"held-drain", "held", PhaseSettled, false, false, false},
		{"missing-evidence", "waiting_evidence", PhaseSettled, false, false, false},
		{"replacement-gap", "active", PhaseRolling, false, false, false},
		{"offline-replacement", "active", PhaseSettled, true, false, false},
		{"drain-and-offline-replacement", "active", PhaseSettled, false, true, false},
		{"drain-and-unreported-offline", "active", PhaseSettled, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			run := e.seedQueuedRun(t, nil, []string{"gpu"})
			w := e.seedHeartbeatWorker(t, nil)
			now := time.Now().UTC().Truncate(time.Second)
			e.exec(t, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',capabilities='{gpu}',status='online',last_heartbeat_at=$2,draining_since=$3,draining_reason='image_roll',max_concurrent_runs=1 WHERE id=$1", w, now, now.Add(-time.Hour))
			e.seedActiveRunOwnedBy(t, w)
			e.exec(t, "UPDATE runs SET created_at=$2,status_since=$2 WHERE id=$1", run, now.Add(-time.Hour))
			if tc.offline {
				e.exec(t, "UPDATE workers SET status='offline',last_heartbeat_at=$2 WHERE id=$1", w, now.Add(-time.Hour))
			}
			if _, err := svc.RecordRollHealth(e.ctx, RollHealthReport{WorkerID: w, Kind: "fleet", Phase: tc.phase, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: tc.state, TargetImage: "repo:new"}}); err != nil {
				t.Fatal(err)
			}
			if tc.mixed {
				peer := e.seedHeartbeatWorker(t, nil)
				e.exec(t, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',capabilities='{gpu}',status='offline',last_heartbeat_at=$2 WHERE id=$1", peer, now.Add(-time.Hour))
				if tc.name != "drain-and-unreported-offline" {
					if _, err := svc.RecordRollHealth(e.ctx, RollHealthReport{WorkerID: peer, Kind: "fleet", Phase: PhaseRolling, ObservedAt: now, ControllerReportedAt: now, Pacing: &PacingReport{State: "active", TargetImage: "repo:new"}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := svc.queuedReason(e.ctx, now, e.healthRowFor(t, run))
			if tc.legacy && got != ReasonWorkersUpgrading {
				t.Fatalf("active drain-only exemption lost: %q", got)
			}
			if tc.name == "drain-and-unreported-offline" && got == ReasonWorkersUpgrading {
				t.Fatalf("unreported suitable offline peer restored drain exemption: %q", got)
			}
			if !tc.legacy && tc.name != "drain-and-unreported-offline" && !strings.Contains(got, "worker roll") {
				t.Fatalf("replacement/held wait incorrectly exempted: %q", got)
			}
			svc.healthSettings = defaultHealthSettings()
			svc.detectRunHealth(e.ctx, now)
			stored := e.healthRowFor(t, run)
			if stored.Health != healthWaitingWorker || stored.HealthReason.String != got {
				t.Fatalf("stored reason differs: %+v", stored)
			}
			owners, err := e.q.ListOwnersWaitingNoCapacity(e.ctx, store.ListOwnersWaitingNoCapacityParams{HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}, RollReason: ReasonWorkersUpgrading})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range owners {
				if row.RunID == run {
					found = true
					if row.HasRollReason != tc.legacy {
						t.Fatalf("capacity exemption=%v want=%v", row.HasRollReason, tc.legacy)
					}
				}
			}
			if !found {
				t.Fatal("stored wait absent from capacity query")
			}
		})
	}
}
