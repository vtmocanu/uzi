package workersvc

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These fixtures own a fresh user and never change global settings. The real
// ListActiveRunsForHealth projection is deliberately used at every health read.
type issue4044Env struct {
	e          interlockLiveDB
	svc        *Service
	at         time.Time
	owner, run uuid.UUID
	commit     pgtype.Text
}

func issue4044Caps(source bool) []string {
	caps := []string{capability.RecoveryInventoryV1}
	if source {
		caps = append(caps, capability.RepoAgentSourceV1)
	}
	return caps
}

func issue4044Setup(t *testing.T, retained bool) *issue4044Env {
	t.Helper()
	e := setupInterlockLiveDB(t)
	at := time.Now().UTC().Truncate(time.Microsecond)
	owner := e.seedSpreadWorker(t, issue4044Caps(false), 2)
	e.seedActiveRunOwnedBy(t, owner)
	run := e.seedQueuedRun(t, nil, nil)
	commit := pgtype.Text{}
	if retained {
		commit = pgtype.Text{String: strings.Repeat("a", 40), Valid: true}
	}
	e.exec(t, `UPDATE workers SET name='source-owner',kind='hosted',template_declared='base',hosted_size='m',draining_since=$2,last_heartbeat_at=$3 WHERE id=$1`, owner, at.Add(-time.Hour), at)
	e.exec(t, `UPDATE runs SET worker_id=$2,claim_generation=9,repo_agent_source_required=true,
 repo_agent_source_commit=$3,status_since=$4,updated_at=$4 WHERE id=$1`, run, owner, commit, at.Add(-3*time.Hour))
	svc := e.permitService(t)
	svc.SetCapabilitySettings(fakeCapabilitySettings{on: true})
	svc.p.WorkerAffinityCeiling = 2 * time.Hour
	svc.p.WorkerStaleRequeueGrace = 0
	t.Cleanup(func() {
		for _, sql := range []string{
			"UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1",
			"DELETE FROM recovery_custody_holds WHERE user_id=$1",
			"DELETE FROM runs WHERE user_id=$1",
			"DELETE FROM workers WHERE user_id=$1",
		} {
			if _, err := e.pool.Exec(e.ctx, sql, e.userID); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	var active, cap int
	if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM runs WHERE worker_id=w.id AND status IN ('claimed','running')), max_concurrent_runs FROM workers w WHERE id=$1`, owner).Scan(&active, &cap); err != nil || active != 1 || cap != 2 {
		t.Fatalf("old owner active=%d cap=%d err=%v", active, cap, err)
	}
	return &issue4044Env{e: e, svc: svc, at: at, owner: owner, run: run, commit: commit}
}

func (f *issue4044Env) health(t *testing.T, at time.Time, want string) {
	t.Helper()
	row := f.e.healthRowFor(t, f.run)
	actual, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil || row.ClaimGeneration != actual.ClaimGeneration {
		t.Fatalf("health generation disagrees with run: %v", err)
	}
	flag, reason := f.svc.healthTargetFor(f.e.ctx, at, row,
		healthThresholds{queued: time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true})
	if flag != healthWaitingWorker || reason != want {
		t.Fatalf("health=(%q,%q), want (%q,%q)", flag, reason, healthWaitingWorker, want)
	}
}

func (f *issue4044Env) params(worker uuid.UUID, caps []string) store.ClaimRunParams {
	cp := f.e.claimParams(worker, caps, true)
	cp.CustodyHoldLimit = testCustodyLimit
	cp.RecoveryCapable = slices.Contains(caps, capability.RecoveryInventoryV1) || slices.Contains(caps, capability.RecoveryArchiveV1) || slices.Contains(caps, capability.RecoveryArchiveV2)
	cp.WorkerIdentity = "w-" + worker.String()[:8]
	cp.ClaimantDraining = worker == f.owner
	cp.AffinityCutoff = ownerHoldStamp(f.at.Add(-2 * time.Hour))
	cp.HeartbeatCutoff = ownerHoldStamp(f.at.Add(-45 * time.Second))
	cp.SnapshotFreshCutoff = ownerHoldStamp(f.at.Add(-time.Minute))
	return cp
}

func (f *issue4044Env) refuse(t *testing.T, cp store.ClaimRunParams, hold uuid.UUID) {
	t.Helper()
	before, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	custody := func() (int, string) {
		t.Helper()
		var n int
		var body string
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1`, f.run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		// Each fixture creates at most nine historical holds; never aggregate an unbounded fixture.
		if n > 9 {
			t.Fatalf("unexpected custody row count %d", n)
		}
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY id),'[]')::text FROM recovery_custody_holds h WHERE run_id=$1`, f.run).Scan(&body); err != nil {
			t.Fatal(err)
		}
		return n, body
	}
	n, body := custody()
	if _, err := f.e.q.ClaimRun(f.e.ctx, cp); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ClaimRun=%v, want no rows", err)
	}
	after, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		a, b := reflect.ValueOf(before), reflect.ValueOf(after)
		for i := 0; i < a.NumField(); i++ {
			if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				t.Fatalf("refusal changed persisted run field %s", a.Type().Field(i).Name)
			}
		}
	}
	nn, next := custody()
	if nn != n || next != body {
		t.Fatalf("refusal changed ordered custody rows (before=%d after=%d)", n, nn)
	}

	if hold != uuid.Nil && mhHoldState(t, f.e, hold) != "open" {
		t.Fatal("refusal closed custody")
	}
}

func (f *issue4044Env) claim(t *testing.T, cp store.ClaimRunParams) {
	t.Helper()
	got, err := f.e.q.ClaimRun(f.e.ctx, cp)
	if err != nil || got.ID != f.run || got.ClaimGeneration != 10 {
		t.Fatalf("claim id=%s generation=%d err=%v", got.ID, got.ClaimGeneration, err)
	}
}

// The own-drain exception is not a protocol exception. Only the capability
// changes between the refused attempt and its admitted positive control.
func TestIssue4044OwnDrainCapabilityFenceLiveDB(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "required only", true: "retained commit"}[retained], func(t *testing.T) {
			f := issue4044Setup(t, retained)
			hold := mhOpenHold(t, f.e, f.run, 9, f.owner)
			cp := f.params(f.owner, issue4044Caps(false))
			f.refuse(t, cp, hold)
			f.e.exec(t, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.owner, issue4044Caps(true))
			cp.WorkerProtocolCaps = issue4044Caps(true)
			f.claim(t, cp)
		})
	}
}

func TestIssue4044PostHoldProjectionAndUpgradeBlockerLiveDB(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "no commit", true: "retained commit"}[retained], func(t *testing.T) {
			f := issue4044Setup(t, retained)
			f.health(t, f.at, reasonNoRepoAgentSourceCapableWorker+"; "+reasonAssignedRepoAgentSourceGap)
			rows, err := f.e.q.ListWorkerUpgradeBlockers(f.e.ctx, []uuid.UUID{f.owner})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range rows {
				if r.RunID.Valid && uuid.UUID(r.RunID.Bytes) == f.run {
					found = true
					if !r.MissingRepoAgentSource {
						t.Fatal("upgrade blocker lost missing capability")
					}
				}
			}
			if !found {
				t.Fatal("assigned continuation missing from upgrade blockers")
			}
		})
	}
}

// Capacity in ClaimRun is peer spread, not a claimant self-cap. We only call
// ClaimRun on the actually free peer. The worker self-cap lives in
// agent/src/worker.ts (claim loop), covered by agent/test/worker.test.ts.
func TestIssue4044CapablePeerBusyThenFreeLiveDB(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "no commit", true: "retained commit"}[retained], func(t *testing.T) {
			f := issue4044Setup(t, retained)
			f.health(t, f.at, reasonNoRepoAgentSourceCapableWorker+"; "+reasonAssignedRepoAgentSourceGap)
			peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
			active := f.e.seedActiveRunOwnedBy(t, peer)
			f.health(t, f.at, reasonAssignedRepoAgentSourceGap+sourcePeerUnavailableSuffix)
			counts, err := f.svc.WorkerEligibilityForHealth(f.e.ctx, f.at, f.run)
			if err != nil || counts.Claimable != 0 {
				t.Fatalf("busy peer counts=%+v err=%v", counts, err)
			}
			f.e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", active)
			f.health(t, f.at, reasonAssignedRepoAgentSourceGap)
			f.claim(t, f.params(peer, issue4044Caps(true)))
		})
	}
}

func TestIssue4044HeldProjectionBoundaryLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	updated := f.at.Add(-time.Hour)
	f.e.exec(t, "UPDATE runs SET status_since=$2,updated_at=$2 WHERE id=$1", f.run, updated)
	deadline := updated.Add(2 * time.Hour)
	want := "waiting until " + deadline.Format(time.RFC3339) + " for its previous worker source-owner to finish draining or maintenance" + heldRepoAgentSourceGapSuffix
	f.health(t, f.at, want)
	if !isStaleRequeuePinReason(want) {
		t.Fatal("held reason no longer classified as owner wait")
	}
	peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
	hold := mhOpenHold(t, f.e, f.run, 9, f.owner)
	f.refuse(t, f.params(peer, issue4044Caps(true)), hold)
	f.health(t, deadline.Add(time.Microsecond), reasonAssignedRepoAgentSourceGap+sourcePeerUnavailableSuffix)
}

func TestIssue4044PeerAffinityReleaseLiveDB(t *testing.T) {
	for _, mode := range []string{"online ceiling", "offline draining ceiling", "offline no drain"} {
		t.Run(mode, func(t *testing.T) {
			f := issue4044Setup(t, true)
			peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
			f.e.exec(t, "UPDATE runs SET updated_at=$2 WHERE id=$1", f.run, f.at.Add(-time.Hour))
			if mode != "online ceiling" {
				f.e.exec(t, "UPDATE workers SET status='offline',last_heartbeat_at=$2 WHERE id=$1", f.owner, f.at.Add(-time.Hour))
			}
			if mode == "offline no drain" {
				f.e.exec(t, "UPDATE workers SET draining_since=NULL WHERE id=$1", f.owner)
				f.health(t, f.at, reasonAssignedRepoAgentSourceGap)
				f.claim(t, f.params(peer, issue4044Caps(true)))
				return
			}
			f.health(t, f.at, "waiting until "+f.at.Add(time.Hour).Format(time.RFC3339)+" for its previous worker source-owner to finish draining or maintenance"+heldRepoAgentSourceGapSuffix)
			hold := mhOpenHold(t, f.e, f.run, 9, f.owner)
			f.refuse(t, f.params(peer, issue4044Caps(true)), hold)
			f.e.exec(t, "UPDATE runs SET updated_at=$2 WHERE id=$1", f.run, f.at.Add(-3*time.Hour))
			f.claim(t, f.params(peer, issue4044Caps(true)))
		})
	}
}

// A recorded source alone keeps the dependency even when the original flag is
// false. A generation-zero assignment is not a continuation diagnostic.
func TestIssue4044RecordedSourceAndGenerationControlsLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	f.e.exec(t, "UPDATE runs SET repo_agent_source_required=false WHERE id=$1", f.run)
	f.health(t, f.at, reasonNoRepoAgentSourceCapableWorker+"; "+reasonAssignedRepoAgentSourceGap)
	f.refuse(t, f.params(f.owner, issue4044Caps(false)), uuid.Nil)
	f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.owner, issue4044Caps(true))
	f.claim(t, f.params(f.owner, issue4044Caps(true)))
	f = issue4044Setup(t, true)
	f.e.exec(t, "UPDATE runs SET claim_generation=0 WHERE id=$1", f.run)
	f.health(t, f.at, reasonNoRepoAgentSourceCapableWorker)
}

func TestIssue4044SnapshotFenceLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
	hold := mhOpenHold(t, f.e, f.run, 9, f.owner)
	f.e.exec(t, "UPDATE workers SET snapshot_epoch=1,snapshot_register_nonce=$3,last_heartbeat_at=$2 WHERE id=$1", f.owner, f.at, uuid.NewString())
	f.e.exec(t, `INSERT INTO worker_active_runs
 (worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
 VALUES($1,$2,9,'running',false,1,$3)`, f.owner, f.run, f.at)
	cp := f.params(peer, issue4044Caps(true))
	f.refuse(t, cp, hold)
	f.e.exec(t, "UPDATE worker_active_runs SET claim_generation=8 WHERE worker_id=$1 AND run_id=$2", f.owner, f.run)
	f.claim(t, cp)
}

// Restrict the detector's iteration to this fixture while all reads and writes
// still use real Queries. Other test owners must never receive health writes.
type issue4044HealthStore struct {
	*store.Queries
	run uuid.UUID
}

func (s issue4044HealthStore) ListActiveRunsForHealth(ctx context.Context, models []string) ([]store.ListActiveRunsForHealthRow, error) {
	rows, err := s.Queries.ListActiveRunsForHealth(ctx, models)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ID == s.run {
			return []store.ListActiveRunsForHealthRow{r}, nil
		}
	}
	return nil, nil
}
func TestIssue4044PersistedHeldLifecycleLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	f.svc.q = issue4044HealthStore{Queries: f.e.q, run: f.run}
	f.svc.SetHealthSettings(defaultHealthSettings())
	b := &fakeBroadcaster{}
	f.svc.SetBroadcaster(b)
	f.e.exec(t, "UPDATE runs SET status_since=$2,updated_at=$2 WHERE id=$1", f.run, f.at.Add(-30*time.Second))
	f.svc.detectRunHealth(f.e.ctx, f.at)
	row := f.e.healthRowFor(t, f.run)
	if row.Health != healthWaitingWorker || !row.HealthReason.Valid || !isStaleRequeuePinReason(row.HealthReason.String) ||
		!row.HealthSince.Valid || !row.HealthSince.Time.Equal(f.at) || row.HealthNotifiedAt.Valid {
		t.Fatalf("young held persistence: %+v", row)
	}
	for _, nudge := range b.healthNudges {
		if nudge {
			t.Fatal("young held run nudged")
		}
	}
	checkAlarm := func(want bool) {
		t.Helper()
		rows, err := f.e.q.ListWaitingWorkerRuns(f.e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			found = found || r.RunID == f.run
		}
		if found != want {
			t.Fatalf("waiting alarm inclusion=%v want=%v", found, want)
		}
	}
	checkAlarm(false)
	owners, err := f.e.q.ListOwnersWaitingNoCapacity(f.e.ctx, store.ListOwnersWaitingNoCapacityParams{
		HeartbeatCutoff: ownerHoldStamp(f.at.Add(-45 * time.Second)), RollReason: ReasonWorkersUpgrading})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range owners {
		if o.UserID == f.e.userID {
			t.Fatal("held owner included in no-capacity alarm")
		}
	}
	// An already-notified aged held episode must not acquire a new nudge stamp.
	oldNotified := f.at.Add(-10 * time.Minute)
	f.e.exec(t, "UPDATE runs SET status_since=$2,updated_at=$2 WHERE id=$1", f.run, f.at.Add(-time.Hour))
	f.svc.detectRunHealth(f.e.ctx, f.at)
	f.e.exec(t, "UPDATE runs SET health_notified_at=$2 WHERE id=$1", f.run, oldNotified)
	f.svc.detectRunHealth(f.e.ctx, f.at.Add(time.Minute))
	row = f.e.healthRowFor(t, f.run)
	if !row.HealthNotifiedAt.Valid || !row.HealthNotifiedAt.Time.Equal(oldNotified) || !isStaleRequeuePinReason(row.HealthReason.String) {
		t.Fatal("aged held episode changed notification or lost hold")
	}
	for _, nudge := range b.healthNudges {
		if nudge {
			t.Fatal("held episode nudged")
		}
	}
	checkAlarm(false)
	after := f.at.Add(3 * time.Hour)
	// Keep the assigned worker online at the evaluation instant: only the hold
	// deadline changes, so the post-hold reason remains the source dependency.
	f.e.exec(t, "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", f.owner, after)
	f.svc.detectRunHealth(f.e.ctx, after)
	row = f.e.healthRowFor(t, f.run)
	want := reasonNoRepoAgentSourceCapableWorker + "; " + reasonAssignedRepoAgentSourceGap
	if row.HealthReason.String != want || !row.HealthSince.Time.Equal(after) {
		t.Fatalf("post-hold episode reason=%q since=%v", row.HealthReason.String, row.HealthSince)
	}
	checkAlarm(true)
	owners, err = f.e.q.ListOwnersWaitingNoCapacity(f.e.ctx, store.ListOwnersWaitingNoCapacityParams{HeartbeatCutoff: ownerHoldStamp(after.Add(-45 * time.Second)), RollReason: ReasonWorkersUpgrading})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range owners {
		found = found || o.UserID == f.e.userID
	}
	if !found {
		t.Fatal("post-hold owner absent from no-capacity alarm")
	}
}

// Complete guarded, empty FINAL history plus an authorized-origin cooldown
// releases the owner pin even inside the ceiling. Both halves are proved in SQL.
func TestIssue4044ForgeSafeHistoryReleasesPinLiveDB(t *testing.T) {
	f := issue4044Setup(t, false)
	peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
	f.e.exec(t, "UPDATE runs SET updated_at=$2 WHERE id=$1", f.run, f.at.Add(-time.Hour))
	// Build each generation in its real pre-source claimed state. Guarded identity
	// is immutable, so insert the original owner and guard rather than editing them.
	for gen := int64(1); gen <= 9; gen++ {
		f.e.exec(t, "UPDATE runs SET status='claimed',claim_generation=$2,claim_released_at=NULL WHERE id=$1", f.run, gen)
		hold := uuid.New()
		f.e.exec(t, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,$5,'open',$6,'source-owner',$6,$4,true)`, hold, f.e.userID, f.e.repoID, f.run, gen, f.owner)
		f.e.exec(t, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
 final_disposition='no_adopted_source',release_evidence='no_adopted_source',released_at=$2 WHERE id=$1`, hold, f.at)
		if mhHoldState(t, f.e, hold) != "released" {
			t.Fatal("guarded FINAL history was not released")
		}
	}
	f.e.exec(t, "UPDATE runs SET status='queued' WHERE id=$1", f.run)

	f.e.exec(t, `INSERT INTO forge_origin_cooldowns
 (user_id,worker_id,origin,cooled_until,last_reason,observed_at)
 VALUES($1,$2,'https://forge.e2e',$3,'transient_clone_fetch_exhausted',$4)`,
		f.e.userID, f.owner, f.at.Add(time.Hour), f.at)
	var safe, pins bool
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT fn_forge_source_history_safe(r),
 fn_forge_origin_owner_pins(w,r,$2) FROM runs r JOIN workers w ON w.id=r.worker_id
 WHERE r.id=$1`, f.run, f.at).Scan(&safe, &pins); err != nil || !safe || pins {
		t.Fatalf("safe=%v pins=%v err=%v", safe, pins, err)
	}
	f.health(t, f.at, reasonAssignedRepoAgentSourceGap)
	f.claim(t, f.params(peer, issue4044Caps(true)))
}

func TestIssue4044PrerequisitesWinLiveDB(t *testing.T) {
	for _, mode := range []string{"task setup", "custody unavailable", "vault locked"} {
		t.Run(mode, func(t *testing.T) {
			f := issue4044Setup(t, true)
			want := reasonHandoffSetup
			th := healthThresholds{queued: time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true}
			switch mode {
			case "task setup":
				f.e.exec(t, "UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/task/'||id::text WHERE id=$1", f.run)
			case "custody unavailable":
				th.custodyLimitOK = false
				want = reasonCustodyLimitUnavailable
			case "vault locked":
				f.svc.SetVault(newMemoryVault(newBox(t), newMemVaultStore()))
				want = reasonVaultLocked
			}
			flag, reason := f.svc.healthTargetFor(f.e.ctx, f.at, f.e.healthRowFor(t, f.run), th)
			if flag != healthWaitingWorker || reason != want {
				t.Fatalf("health=(%q,%q) want=%q", flag, reason, want)
			}
		})
	}
}

func TestIssue4044NonRegularCapableWorkersExcludedLiveDB(t *testing.T) {
	for _, lane := range []string{"ephemeral", "isolated"} {
		t.Run(lane, func(t *testing.T) {
			f := issue4044Setup(t, false)
			peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
			if lane == "ephemeral" {
				f.e.exec(t, "UPDATE workers SET kind='hosted',template_declared='base',hosted_size='m',ephemeral=true WHERE id=$1", peer)
			} else {
				f.e.exec(t, "UPDATE workers SET kind='hosted',template_declared='base',hosted_size='m',ephemeral=true,isolated_lane=true WHERE id=$1", peer)
			}
			f.health(t, f.at, reasonNoRepoAgentSourceCapableWorker+"; "+reasonAssignedRepoAgentSourceGap)
		})
	}
}

func TestIssue4044NoSourceDependencyLiveDB(t *testing.T) {
	f := issue4044Setup(t, false)
	f.e.exec(t, "UPDATE runs SET repo_agent_source_required=false WHERE id=$1", f.run)
	row := f.e.healthRowFor(t, f.run)
	_, reason := f.svc.healthTargetFor(f.e.ctx, f.at, row, healthThresholds{queued: time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true})
	t.Logf("source-free health reason: %q", reason)
	if strings.Contains(reason, "repo_agent_source") || strings.Contains(reason, reasonAssignedRepoAgentSourceGap) || strings.Contains(reason, reasonNoRepoAgentSourceCapableWorker) {
		t.Fatalf("source-free diagnostic=%q", reason)
	}
	f.claim(t, f.params(f.owner, issue4044Caps(false)))
}

func TestIssue4044CustodyLimitLiveDB(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "peer", true: "own"}[own], func(t *testing.T) {
			f := issue4044Setup(t, true)
			worker := f.owner
			if own {
				f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, issue4044Caps(true))
			} else {
				worker = f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
			}
			hold := mhOpenHold(t, f.e, f.run, 9, f.owner)
			cp := f.params(worker, issue4044Caps(true))
			cp.CustodyHoldLimit = 1
			f.refuse(t, cp, hold)
			flag, reason := f.svc.healthTargetFor(f.e.ctx, f.at, f.e.healthRowFor(t, f.run), healthThresholds{queued: time.Minute, custodyLimit: 1, custodyLimitOK: true})
			if flag != healthWaitingWorker || reason != reasonCustodyLimit {
				t.Fatalf("custody limit health=(%q,%q)", flag, reason)
			}
			cp.CustodyHoldLimit = 2
			f.claim(t, cp)
		})
	}
}

func TestIssue4044RequestGenerationFenceLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.owner, issue4044Caps(true))
	hold := mhOpenHold(t, f.e, f.run, 9, f.owner)
	cp := f.params(f.owner, issue4044Caps(true))
	cp.RequestActiveIds = []uuid.UUID{f.run}
	cp.RequestActiveGens = []int64{9}
	f.refuse(t, cp, hold)
	cp.RequestActiveGens = []int64{8}
	f.claim(t, cp)
}

func TestIssue4044HeldProvisioningExclusionsLiveDB(t *testing.T) {
	for _, saturation := range []bool{false, true} {
		t.Run(map[bool]string{false: "source gap", true: "source capable busy"}[saturation], func(t *testing.T) {
			f := issue4044Setup(t, false)
			f.e.exec(t, "UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", f.e.userID)
			if saturation {
				peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
				f.e.seedActiveRunOwnedBy(t, peer)
			}
			p := pinAgreementEnv{e: f.e, svc: f.svc, owner: f.owner, gapRun: f.run, satRun: f.run, ceiling: 2 * time.Hour}
			f.e.exec(t, "UPDATE runs SET updated_at=$2 WHERE id=$1", f.run, f.at.Add(-time.Hour))
			if gap, sat := p.demand(t, f.at); gap || sat {
				t.Fatalf("held demand gap=%v saturation=%v", gap, sat)
			}
			f.e.exec(t, "UPDATE runs SET updated_at=$2 WHERE id=$1", f.run, f.at.Add(-3*time.Hour))
			gap, sat := p.demand(t, f.at)
			if (!saturation && !gap) || (saturation && !sat) {
				t.Fatalf("expired demand absent gap=%v saturation=%v", gap, sat)
			}
		})
	}
}

func TestIssue4044ManualCordonWinsLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	peer := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
	cordoned, err := store.ChangeWorkerManualCordon(f.e.ctx, f.e.pool, f.e.userID, peer, "source operator", pgtype.Text{String: "investigating", Valid: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := f.svc.WorkerEligibilityForHealth(f.e.ctx, f.at, f.run)
	if err != nil || counts.ManualSuitable != 1 || counts.ManualCordoned != 1 || counts.AssignedClaimable != 0 {
		t.Fatalf("manual suitable counts=%+v err=%v", counts, err)
	}
	want := manualCordonWaitPrefix + " source operator (" + f.e.userID.String() + ") at " + cordoned.ManualCordonedAt.Time.UTC().Format(time.RFC3339) + ": investigating"
	f.health(t, f.at, want)
}

// A source-capable worker and a separate docker-capable worker are not one compatible
// peer: the reason must not claim a compatible worker is merely unavailable.
func TestIssue4044SplitFleetNoComposedPeerLiveDB(t *testing.T) {
	f := issue4044Setup(t, true)
	f.e.exec(t, `UPDATE runs SET required_capabilities='{docker}' WHERE id=$1`, f.run)
	srcOnly := f.e.seedSpreadWorker(t, issue4044Caps(true), 1)
	f.e.seedActiveRunOwnedBy(t, srcOnly)
	dockerOnly := f.e.seedSpreadWorker(t, issue4044Caps(false), 1)
	f.e.seedActiveRunOwnedBy(t, dockerOnly)
	f.e.exec(t, `UPDATE workers SET capabilities='{docker}' WHERE id=$1`, dockerOnly)
	counts, err := f.svc.WorkerEligibilityForHealth(f.e.ctx, f.at, f.run)
	if err != nil || counts.StrictEligible != 0 {
		t.Fatalf("split fleet counts=%+v err=%v", counts, err)
	}
	f.health(t, f.at, reasonAssignedRepoAgentSourceGap)
}
