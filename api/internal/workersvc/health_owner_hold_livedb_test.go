package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests deliberately use the projected health row, never staleRequeuePin,
// to decide whether the draining/maintenance owner explains the wait.
func setupOwnerHold(t *testing.T, at time.Time) *pinAgreementEnv {
	t.Helper()
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	svc.SetCapabilitySettings(fakeCapabilitySettings{on: true})
	svc.p.WorkerAffinityCeiling = 2 * time.Hour
	svc.p.WorkerStaleRequeueGrace = 0
	p := &pinAgreementEnv{e: e, svc: svc, owner: uuid.New(), peer: uuid.New(),
		gapRun: uuid.New(), satRun: uuid.New(), ceiling: 2 * time.Hour, queuedThreshold: time.Minute}
	capped, active := uuid.New(), uuid.New()
	t.Cleanup(func() {
		// This fixture owns this user's runs; remove demand before the next fixture runs.
		for _, stmt := range []string{
			"DELETE FROM runs WHERE user_id = $1",
			"DELETE FROM workers WHERE user_id = $1",
			"UPDATE users SET ephemeral_workers_enabled = false WHERE id = $1",
		} {
			if _, err := e.pool.Exec(e.ctx, stmt, e.userID); err != nil {
				t.Errorf("owner-hold cleanup: %v", err)
			}
		}
	})
	e.exec(t, "UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1", e.userID)
	for _, w := range []struct {
		id           uuid.UUID
		name, status string
		cap          any
	}{
		{p.owner, "previous-owner", "offline", nil},
		{p.peer, "peer", "offline", nil},
		{capped, "capable-at-cap", "online", 1},
	} {
		e.exec(t, `INSERT INTO workers
			(id,user_id,name,token_hash,status,last_heartbeat_at,max_concurrent_runs,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$6,$6)`,
			w.id, e.userID, w.name, w.id[:], w.status, at, w.cap)
	}
	for i, r := range []struct {
		id, worker uuid.UUID
		status     string
		caps       []string
	}{
		{p.gapRun, p.owner, "queued", []string{"gh"}},
		{p.satRun, p.owner, "queued", []string{}},
		{active, capped, "running", []string{}},
	} {
		e.exec(t, `INSERT INTO runs
			(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,worker_id,
			 required_capabilities,status_since,updated_at,created_at)
			VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,$8,$8)`,
			r.id, e.userID, e.repoID, i+1, r.status, r.worker, r.caps, at.Add(-3*time.Hour))
	}
	return p
}

func ownerHoldStamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}

func resetOwnerHoldRuns(t *testing.T, p *pinAgreementEnv, at, updated time.Time) {
	t.Helper()
	p.e.exec(t, `UPDATE runs SET status='queued',worker_id=$2,claim_generation=1,
		stale_requeue_generation=NULL,status_since=$3,updated_at=$4 WHERE id=ANY($1)`,
		[]uuid.UUID{p.gapRun, p.satRun}, p.owner, at.Add(-20*time.Minute), updated)
}

// assertOwnerHold checks demand before making the peer online for the count mirror.
// The latter uses StrictEligible, whose affinity predicate mirrors ClaimRun.
func assertOwnerHold(t *testing.T, p *pinAgreementEnv, at time.Time, wantReason string, held bool) {
	t.Helper()
	gap, sat := p.demand(t, at)
	if gap != !held || sat != !held {
		t.Errorf("demand gap=%v saturation=%v, want both %v", gap, sat, !held)
	}
	for _, id := range []uuid.UUID{p.gapRun, p.satRun} {
		row := p.e.healthRowFor(t, id)
		flag, reason := p.svc.healthTargetFor(p.e.ctx, at, row, healthThresholds{queued: p.queuedThreshold, custodyLimit: testCustodyLimit, custodyLimitOK: true})
		if held {
			if flag != healthWaitingWorker || reason != wantReason {
				t.Errorf("run %s health=(%q,%q), want (%q,%q)", id, flag, reason, healthWaitingWorker, wantReason)
			}
		} else {
			if strings.HasPrefix(reason, "waiting until ") {
				t.Errorf("released run %s retains hold reason %q", id, reason)
			}
			if flag != healthWaitingWorker || reason == "" {
				t.Errorf("released run %s lacks ordinary fleet reason: (%q,%q)", id, flag, reason)
			}
			if row.Kind != "cross_check" {
				want := reasonAllWorkersBusy
				if id == p.gapRun {
					want = reasonNoEligibleWorker
				}
				if reason != want {
					t.Errorf("released run %s reason=%q, want %q", id, reason, want)
				}
			}
		}
	}
	p.e.exec(t, `UPDATE workers SET status='offline' WHERE user_id=$1 AND id<>$2`, p.e.userID, p.peer)
	p.e.exec(t, `UPDATE workers SET status='online',last_heartbeat_at=$2 WHERE id=$1`, p.peer, at)
	for _, id := range []uuid.UUID{p.gapRun, p.satRun} {
		n, err := p.e.q.CountOnlineWorkersClaimableForRun(p.e.ctx, store.CountOnlineWorkersClaimableForRunParams{
			RunID: id, HeartbeatCutoff: ownerHoldStamp(at.Add(-45 * time.Second)),
			AffinityCutoff: ownerHoldStamp(at.Add(-p.ceiling)), StaleRequeueCutoff: StaleRequeueCutoff(at, p.grace),
			CrossCheckEvaluatedAt: ownerHoldStamp(at), CrossCheckAffinityCutoff: ownerHoldStamp(at.Add(-2 * time.Minute)),
			CodexCuratedModels: codexCuratedModelsSlice(),
		})
		want := int64(1)
		if held {
			want = 0
		}
		if err != nil || n.StrictEligible != want {
			t.Errorf("count mirror run %s: strict=%d err=%v, want %d", id, n.StrictEligible, err, want)
		}
	}
	cp := store.ClaimRunParams{
		UserID: p.e.userID, WorkerID: pgtype.UUID{Bytes: p.peer, Valid: true},
		HeartbeatCutoff: ownerHoldStamp(at.Add(-45 * time.Second)),
		AffinityCutoff:  ownerHoldStamp(at.Add(-p.ceiling)), StaleRequeueCutoff: StaleRequeueCutoff(at, p.grace),
		SpreadCutoff: ownerHoldStamp(at.Add(-9 * time.Second)), BackgroundGraceCutoff: ownerHoldStamp(at.Add(-15 * time.Minute)),
		CrossCheckEvaluatedAt: ownerHoldStamp(at), CrossCheckAffinityCutoff: ownerHoldStamp(at.Add(-2 * time.Minute)),
		WorkerCaps: []string{}, WorkerProtocolCaps: []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1},
		CodexCuratedModels: codexCuratedModelsSlice(),
	}
	run, err := p.e.q.ClaimRun(p.e.ctx, cp)
	if held {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("peer ClaimRun=%s err=%v, want no rows", run.ID, err)
		}
	} else if err != nil || (run.ID != p.gapRun && run.ID != p.satRun) {
		t.Errorf("released peer ClaimRun=%s err=%v, want fixture run", run.ID, err)
	}
}

func TestDrainingOwnerHoldAgreesAcrossClaimProvisioningAndHealthLiveDB(t *testing.T) {
	for _, heartbeatAge := range []time.Duration{0, 10 * time.Minute} {
		t.Run(heartbeatAge.String(), func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Microsecond)
			p := setupOwnerHold(t, at)
			p.e.exec(t, `UPDATE workers SET draining_since=$2,last_heartbeat_at=$3 WHERE id=$1`,
				p.owner, at.Add(-time.Hour), at.Add(-heartbeatAge))
			resetOwnerHoldRuns(t, p, at, at.Add(-2*time.Hour))
			want := "waiting until " + at.Format(time.RFC3339) +
				" for its previous worker previous-owner to finish draining or maintenance; another worker may take it after that"
			assertOwnerHold(t, p, at, want, true)
			assertOwnerHoldReleasedAfterBoundary(t, p, at)
		})
	}
}

func TestMaintenanceOwnerHoldAgreesAcrossClaimProvisioningAndHealthLiveDB(t *testing.T) {
	for _, phase := range []string{"requested", "ready", "stopping", "recycling", "fenced-only"} {
		t.Run(phase, func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Microsecond)
			p := setupOwnerHold(t, at)
			fenced := phase == "fenced-only"
			storedPhase := phase
			if fenced {
				storedPhase = ""
			}
			p.e.exec(t, `UPDATE workers SET maintenance_phase=$2,maintenance_fenced=$3,
				last_heartbeat_at=$4 WHERE id=$1`, p.owner, storedPhase, fenced, at.Add(-10*time.Minute))
			resetOwnerHoldRuns(t, p, at, at.Add(-2*time.Hour))
			want := "waiting until " + at.Format(time.RFC3339) +
				" for its previous worker previous-owner to finish draining or maintenance; another worker may take it after that"
			assertOwnerHold(t, p, at, want, true)
			assertOwnerHoldReleasedAfterBoundary(t, p, at)
		})
	}
}

// Restore the demand fixture after the count/claim probes, keeping the same owner
// flags and updated_at. Only evaluation time moves, by one microsecond.
func assertOwnerHoldReleasedAfterBoundary(t *testing.T, p *pinAgreementEnv, at time.Time) {
	t.Helper()
	p.e.exec(t, `UPDATE workers SET status=CASE WHEN name='capable-at-cap' THEN 'online' ELSE 'offline' END
		WHERE user_id=$1`, p.e.userID)
	resetOwnerHoldRuns(t, p, at, at.Add(-2*time.Hour))
	assertOwnerHold(t, p, at.Add(time.Microsecond), "", false)
}

func TestOwnerHoldReleaseAndControlsLiveDB(t *testing.T) {
	for _, control := range []string{"drain past ceiling", "maintenance past ceiling", "stale non-draining",
		"inactive maintenance", "NULL worker", "deleted owner", "PLAN cross_check"} {
		t.Run(control, func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Microsecond)
			p := setupOwnerHold(t, at)
			p.e.exec(t, `UPDATE workers SET draining_since=$2,last_heartbeat_at=$3 WHERE id=$1`,
				p.owner, at.Add(-time.Hour), at.Add(-10*time.Minute))
			updated := at.Add(-time.Hour)
			if strings.Contains(control, "past ceiling") {
				updated = at.Add(-2*time.Hour - time.Microsecond)
			}
			resetOwnerHoldRuns(t, p, at, updated)
			switch control {
			case "maintenance past ceiling":
				p.e.exec(t, `UPDATE workers SET draining_since=NULL,maintenance_phase='requested' WHERE id=$1`, p.owner)
			case "stale non-draining":
				p.e.exec(t, `UPDATE workers SET draining_since=NULL WHERE id=$1`, p.owner)
			case "inactive maintenance":
				p.e.exec(t, `UPDATE workers SET draining_since=NULL,maintenance_phase='complete' WHERE id=$1`, p.owner)
			case "NULL worker":
				p.e.exec(t, `UPDATE runs SET worker_id=NULL WHERE id=ANY($1)`, []uuid.UUID{p.gapRun, p.satRun})
			case "deleted owner":
				p.e.exec(t, `DELETE FROM workers WHERE id=$1`, p.owner)
			case "PLAN cross_check":
				protocols := []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}
				p.e.exec(t, `UPDATE workers SET protocol_capabilities=$2 WHERE user_id=$1`, p.e.userID, protocols)
				for i, id := range []uuid.UUID{p.gapRun, p.satRun} {
					lead := uuid.New()
					p.e.exec(t, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,
						status,claim_generation,status_since,updated_at,created_at)
						VALUES ($1,$2,$3,$4,'lead','d','running',1,$5,$5,$5)`,
						lead, p.e.userID, p.e.repoID, 10+i, at.Add(-time.Hour))
					p.e.exec(t, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
						harness='codex',report_only=true,budget_wall_seconds=1800,required_tools='{}' WHERE id=$1`, id, lead)
					p.e.exec(t, `INSERT INTO cross_checks (id,lead_run_id,checker_run_id,stage,round,
						lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,deadline_at)
						VALUES ($1,$2,$3,'plan',1,1,'plan','[]','s',repeat('a',40),$4,'codex',$5)`,
						uuid.New(), lead, id, []byte("test-digest"), at.Add(5*time.Minute))
				}
			}
			assertOwnerHold(t, p, at, "", false)
		})
	}
}

func TestOwnerHoldPrecedesStalePinAndPreservesStaleOnlyLiveDB(t *testing.T) {
	for _, mode := range []string{"overlap ceiling", "stale only", "empty owner name", "below queued threshold"} {
		t.Run(mode, func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Microsecond)
			p := setupOwnerHold(t, at)
			resetOwnerHoldRuns(t, p, at, at.Add(-time.Hour))
			p.grace = 3 * time.Hour
			p.svc.p.WorkerStaleRequeueGrace = p.grace
			p.e.exec(t, `UPDATE runs SET stale_requeue_generation=claim_generation,status_since=$2 WHERE id=ANY($1)`,
				[]uuid.UUID{p.gapRun, p.satRun}, at.Add(-5*time.Minute))
			p.e.exec(t, `UPDATE workers SET draining_since=$2,last_heartbeat_at=$3 WHERE id=$1`,
				p.owner, at.Add(-time.Hour), at.Add(-10*time.Minute))
			deadline := at.Add(time.Hour).Format(time.RFC3339)
			want := "waiting until " + deadline + " for its previous worker previous-owner to finish draining or maintenance; another worker may take it after that"
			switch mode {
			case "stale only":
				p.e.exec(t, `UPDATE workers SET draining_since=NULL WHERE id=$1`, p.owner)
				want = "waiting until " + deadline + " for its previous worker previous-owner to return; another worker may take it after that"
			case "empty owner name":
				p.e.exec(t, `UPDATE workers SET name='' WHERE id=$1`, p.owner)
				want = "waiting until " + deadline + " for its previous worker to finish draining or maintenance; another worker may take it after that"
			case "below queued threshold":
				p.svc.p.WorkerStaleRequeueGrace = 0
				p.grace = 0
				p.e.exec(t, `UPDATE runs SET status_since=$2,stale_requeue_generation=NULL WHERE id=ANY($1)`,
					[]uuid.UUID{p.gapRun, p.satRun}, at.Add(-30*time.Second))
			}
			assertOwnerHold(t, p, at, want, true)
		})
	}
}

// Manual intent must not turn a stale owner into an automatic affinity hold.
func TestManualCordonStaleOwnerReleasesAcrossClaimProvisioningAndHealthLiveDB(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	p := setupOwnerHold(t, at)
	for _, manual := range []bool{false, true, false} {
		p.e.exec(t, `UPDATE workers SET status=CASE WHEN name='capable-at-cap' THEN 'online' ELSE 'offline' END WHERE user_id=$1`, p.e.userID)
		if _, err := store.ChangeWorkerManualCordon(p.e.ctx, p.e.pool, p.e.userID, p.owner, "lane operator", pgtype.Text{}, manual); err != nil {
			t.Fatal(err)
		}
		p.e.exec(t, `UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1`, p.owner, at.Add(-10*time.Minute))
		resetOwnerHoldRuns(t, p, at, at.Add(-time.Hour))
		for _, id := range []uuid.UUID{p.gapRun, p.satRun} {
			if row := p.e.healthRowFor(t, id); row.OwnerHoldPinnable {
				t.Fatalf("manual=%v run=%s: stale owner gained automatic hold", manual, id)
			}
		}
		// Includes real demand queries, peer strict eligibility and a successful peer ClaimRun.
		assertOwnerHold(t, p, at, "", false)
	}
}

func TestManualCordonFreshOwnerRetainsIndependentAffinityLiveDB(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	p := setupOwnerHold(t, at)
	resetOwnerHoldRuns(t, p, at, at.Add(-time.Hour))
	gapBefore, satBefore := p.demand(t, at)
	if _, err := store.ChangeWorkerManualCordon(p.e.ctx, p.e.pool, p.e.userID, p.owner, "lane operator", pgtype.Text{}, true); err != nil {
		t.Fatal(err)
	}
	if gap, sat := p.demand(t, at); gap != gapBefore || sat != satBefore {
		t.Fatalf("manual cordon changed fresh owner demand: gap=%v saturation=%v, before gap=%v saturation=%v", gap, sat, gapBefore, satBefore)
	}
	for _, id := range []uuid.UUID{p.gapRun, p.satRun} {
		if row := p.e.healthRowFor(t, id); row.OwnerHoldPinnable {
			t.Fatalf("fresh manually cordoned owner gained automatic hold: %s", id)
		}
	}
	p.e.exec(t, `UPDATE workers SET status='offline' WHERE user_id=$1 AND id<>$2`, p.e.userID, p.peer)
	p.e.exec(t, `UPDATE workers SET status='online',last_heartbeat_at=$2 WHERE id=$1`, p.peer, at)
	counts, err := p.svc.workerEligibilityForHealth(p.e.ctx, at, p.satRun, true)
	if err != nil || counts.StrictEligible != 0 {
		t.Fatalf("fresh owner peer eligibility=%+v err=%v", counts, err)
	}
	_, err = p.e.q.ClaimRun(p.e.ctx, store.ClaimRunParams{
		UserID: p.e.userID, WorkerID: pgtype.UUID{Bytes: p.peer, Valid: true},
		HeartbeatCutoff: ownerHoldStamp(at.Add(-45 * time.Second)), AffinityCutoff: ownerHoldStamp(at.Add(-p.ceiling)),
		StaleRequeueCutoff: StaleRequeueCutoff(at, p.grace), SpreadCutoff: ownerHoldStamp(at.Add(-9 * time.Second)),
		BackgroundGraceCutoff: ownerHoldStamp(at.Add(-15 * time.Minute)),
		CrossCheckEvaluatedAt: ownerHoldStamp(at), CrossCheckAffinityCutoff: ownerHoldStamp(at.Add(-2 * time.Minute)),
		WorkerCaps: []string{}, WorkerProtocolCaps: []string{}, CodexCuratedModels: codexCuratedModelsSlice(),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fresh owner peer ClaimRun err=%v, want no rows", err)
	}
}
