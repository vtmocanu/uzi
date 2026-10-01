package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_lane_livedb_test.go pins PRD #1906 M5's placement (Decision 9): ClaimRun's two-way
// isolated-lane clause, its mirrors (the spread peer, CountOnlineWorkersClaimableForRun, the
// queued reason), the chat lane's exclusion and the custody-hold skip, on the REAL claim path
// against a real Postgres. Each refused case also asserts the run is still queued and
// unowned, so a regression that claimed it and then failed it in assembly is red too.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type laneWorkerSpec struct {
	hosted    bool
	ephemeral bool
	boundRun  uuid.UUID
	isolated  bool
	protoCaps []string
}

// seedWorker inserts an online, freshly-heartbeating worker for the fixture user and returns
// the row as RequireWorker would load it.
func (f *isoFix) seedWorker(t *testing.T, spec laneWorkerSpec) store.Worker {
	t.Helper()
	id := uuid.New()
	a, b := uuid.New(), uuid.New()
	kind := "external"
	var tmpl, size pgtype.Text
	var docker pgtype.Bool
	if spec.hosted {
		kind = "hosted"
		tmpl = pgtype.Text{String: "base", Valid: true}
		size = pgtype.Text{String: "m", Valid: true}
		docker = pgtype.Bool{Bool: false, Valid: true}
	}
	var bound pgtype.UUID
	if spec.boundRun != uuid.Nil {
		bound = pgconv.UUID(spec.boundRun)
	}
	caps := spec.protoCaps
	if caps == nil {
		caps = []string{}
	}
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, template_declared,
	                                 hosted_size, docker_enabled, ephemeral, ephemeral_run_id, isolated_lane, protocol_capabilities)
	            VALUES ($1, $2, $3, $4, 'online', now(), $5, $6, $7, $8, $9, $10, $11, $12)`,
		id, f.userID, "lane-"+id.String(), append(a[:], b[:]...), kind, tmpl, size, docker, spec.ephemeral, bound, spec.isolated, caps)
	w, err := f.env.q.GetWorkerByID(f.env.ctx, id)
	if err != nil {
		t.Fatalf("load worker: %v", err)
	}
	return w
}

// laneWorker is the only worker shape that may claim a profile-bound run: hosted, ephemeral,
// bound to run, isolated_lane set by the api, advertising isolated_fetch_v1.
func (f *isoFix) laneWorker(t *testing.T, run uuid.UUID) store.Worker {
	t.Helper()
	return f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: run, isolated: true,
		protoCaps: []string{capability.IsolatedFetchV1}})
}

// refuse asserts wkr's claim is idle and run stays queued with no owner.
func (f *isoFix) refuse(t *testing.T, wkr store.Worker, run uuid.UUID, why string) {
	t.Helper()
	p, err := f.svc.Claim(f.env.ctx, wkr, nil)
	if err != nil {
		t.Fatalf("%s: Claim: %v", why, err)
	}
	if p != nil {
		t.Fatalf("%s: claimed run %s, want idle", why, p.RunID)
	}
	var status string
	var owner pgtype.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, worker_id FROM runs WHERE id = $1`, run).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || owner.Valid {
		t.Fatalf("%s: run is %s owned=%v, want queued and unowned", why, status, owner.Valid)
	}
}

// TestProfileBoundRunPlacementLiveDB: a profile-bound run can be claimed by nothing but a lane
// worker advertising isolated_fetch_v1, whatever the kill-switch, the run's requirements or the
// worker's own report say; the lane worker then claims it (positive control).
func TestProfileBoundRunPlacementLiveDB(t *testing.T) {
	t.Run("capability_aware off", func(t *testing.T) {
		f := newIsoFix(t)
		f.svc.capabilitySettings = fakeCapabilitySettings{on: false}
		run := f.queuedRun(t, 401, true)
		plain := f.seedWorker(t, laneWorkerSpec{protoCaps: []string{capability.IsolatedFetchV1}})
		f.refuse(t, plain, run, "kill-switch off, ordinary worker")
		f.claimAs(t, f.laneWorker(t, run), run)
	})
	t.Run("cleared required_capabilities", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 402, true)
		// The state ClearRunRequiredCapabilities leaves: an empty requirement set. (That query
		// only runs at the plan gate, where a profile-bound run cannot park, per
		// runs_egress_profile_no_in_place_park, so the state is written directly.)
		f.env.exec(`UPDATE runs SET required_capabilities = '{docker}' WHERE id = $1`, run)
		f.env.exec(`UPDATE runs SET required_capabilities = '{}' WHERE id = $1`, run)
		plain := f.seedWorker(t, laneWorkerSpec{})
		f.refuse(t, plain, run, "requirements cleared, ordinary worker")
		f.claimAs(t, f.laneWorker(t, run), run)
	})
	t.Run("external worker self-reporting isolated_fetch_v1", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 403, true)
		ext := f.seedWorker(t, laneWorkerSpec{protoCaps: []string{capability.IsolatedFetchV1, capability.RecoveryArchiveV1}})
		f.refuse(t, ext, run, "external worker with the capability")
		// The column is never a worker's to set: the schema refuses a lane marker on an
		// external (or persistent) worker outright.
		_, err := f.env.pool.Exec(f.env.ctx, `UPDATE workers SET isolated_lane = true WHERE id = $1`, ext.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "ck_workers_isolated_lane" {
			t.Fatalf("marking an external worker as lane = %v, want the ck_workers_isolated_lane violation", err)
		}
	})
	t.Run("hosted non-lane worker", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 404, true)
		hosted := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: run,
			protoCaps: []string{capability.IsolatedFetchV1}})
		f.refuse(t, hosted, run, "hosted ephemeral worker bound to the run, not in the lane")
	})
	t.Run("lane worker without isolated_fetch_v1", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 405, true)
		old := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: run, isolated: true,
			protoCaps: []string{capability.RecoveryArchiveV1}})
		f.refuse(t, old, run, "old lane worker")
	})
	t.Run("chat lane", func(t *testing.T) {
		f := newIsoFix(t)
		run := f.queuedRun(t, 406, true)
		plain := f.seedWorker(t, laneWorkerSpec{})
		p, err := f.svc.ClaimChat(f.env.ctx, plain)
		if err != nil || p != nil {
			t.Fatalf("ClaimChat = %+v, %v; want idle", p, err)
		}
		f.refuse(t, plain, run, "ordinary worker after the chat claim")
	})
}

// TestLaneWorkerNeverClaimsUnboundRunLiveDB: a lane worker, even one bound to an unbound run
// (which the ephemeral clause alone would let it claim), never claims it.
func TestLaneWorkerNeverClaimsUnboundRunLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 410, false)
	lane := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: run, isolated: true,
		protoCaps: []string{capability.IsolatedFetchV1}})
	f.refuse(t, lane, run, "lane worker, unbound run")
	// Positive control: an ordinary worker claims the same run.
	f.claimAs(t, f.seedWorker(t, laneWorkerSpec{}), run)
}

// withDroppedConstraint runs fn inside a transaction that first drops constraint on runs, and
// always rolls back, so a shape the schema forbids can be offered to a claim query to prove the
// query refuses it on its own. The tx holds the runs table lock until the rollback.
func withDroppedConstraint(t *testing.T, env codexTestEnv, constraint string, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(env.ctx, `ALTER TABLE runs DROP CONSTRAINT `+constraint); err != nil {
		t.Fatalf("drop %s: %v", constraint, err)
	}
	fn(tx)
}

// laneClaimParams is a lane worker's ClaimRun params for a direct store call.
func laneClaimParams(userID uuid.UUID, wkr store.Worker) store.ClaimRunParams {
	return store.ClaimRunParams{
		WorkerID:           pgconv.UUID(wkr.ID),
		UserID:             userID,
		AffinityCutoff:     pgconv.Time(time.Now().Add(-2 * time.Minute)),
		SpreadCutoff:       pgconv.Time(time.Now().Add(-9 * time.Second)),
		HeartbeatCutoff:    pgconv.Time(time.Now().Add(-45 * time.Second)),
		WorkerCaps:         []string{},
		CapabilityAware:    true,
		WorkerProtocolCaps: wkr.ProtocolCapabilities,
		WorkerIsolatedLane: wkr.IsolatedLane,
		IsEphemeral:        wkr.Ephemeral,
		EphemeralRunID:     wkr.EphemeralRunID,
	}
}

// TestCodexProfileBoundRunRefusedLiveDB: with the schema's no-Codex CHECK dropped (in a rolled
// back tx), a Codex profile-bound run is still not claimable by a lane worker that advertises
// every Codex capability: the lane clause refuses it on its own.
func TestCodexProfileBoundRunRefusedLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 420, true)
	lane := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: run, isolated: true,
		protoCaps: []string{capability.IsolatedFetchV1, capability.CodexHarnessV1, capability.CodexCustomModelV1,
			capability.CodexCompletionInterlockV1, capability.CompletionInterlockV1}})
	withDroppedConstraint(t, f.env, "runs_egress_profile_not_codex", func(tx pgx.Tx) {
		if _, err := tx.Exec(f.env.ctx, `UPDATE runs SET harness = 'codex' WHERE id = $1`, run); err != nil {
			t.Fatalf("make the run Codex: %v", err)
		}
		_, err := store.New(tx).ClaimRun(f.env.ctx, laneClaimParams(f.userID, lane))
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun on a Codex profile-bound run = %v, want no rows", err)
		}
		// Positive control inside the same tx: the Claude shape of the run is claimable.
		if _, err := tx.Exec(f.env.ctx, `UPDATE runs SET harness = 'claude' WHERE id = $1`, run); err != nil {
			t.Fatal(err)
		}
		got, err := store.New(tx).ClaimRun(f.env.ctx, laneClaimParams(f.userID, lane))
		if err != nil || got.ID != run {
			t.Fatalf("ClaimRun on the Claude run = %v, %v; want the run", got.ID, err)
		}
	})
}

// TestChatClaimRefusesProfileBoundChatLiveDB: with the schema's no-chat CHECK dropped (in a
// rolled back tx), ClaimChatRun still never hands out a profile-bound chat run.
func TestChatClaimRefusesProfileBoundChatLiveDB(t *testing.T) {
	f := newIsoFix(t)
	plain := f.seedWorker(t, laneWorkerSpec{})
	withDroppedConstraint(t, f.env, "runs_egress_profile_not_chat", func(tx pgx.Tx) {
		id := uuid.New()
		if _, err := tx.Exec(f.env.ctx, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, egress_profile_id)
		        VALUES ($1, $2, 'chat', 'chat', 'hi', 'queued', $3)`, id, f.userID, f.profile); err != nil {
			t.Fatalf("insert profile-bound chat: %v", err)
		}
		params := store.ClaimChatRunParams{
			WorkerID:       pgconv.UUID(plain.ID),
			UserID:         f.userID,
			AffinityCutoff: pgconv.Time(time.Now().Add(-2 * time.Minute)),
		}
		if _, err := store.New(tx).ClaimChatRun(f.env.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimChatRun on a profile-bound chat = %v, want no rows", err)
		}
		// Positive control: unbind it and the same call claims it.
		if _, err := tx.Exec(f.env.ctx, `ALTER TABLE runs DISABLE TRIGGER runs_egress_binding_immutable_trg`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.env.ctx, `UPDATE runs SET egress_profile_id = NULL WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if got, err := store.New(tx).ClaimChatRun(f.env.ctx, params); err != nil || got.ID != id {
			t.Fatalf("ClaimChatRun on the unbound chat = %v, %v; want it", got.ID, err)
		}
	})
}

// TestProfileBoundClaimOpensNoCustodyHoldLiveDB: a lane worker that advertises
// recovery_archive_v1 claims a profile-bound run without opening a custody hold; an ordinary
// recovery-capable claim of an unbound run opens one (positive control).
func TestProfileBoundClaimOpensNoCustodyHoldLiveDB(t *testing.T) {
	f := newIsoFix(t)
	bound := f.queuedRun(t, 430, true)
	lane := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: bound, isolated: true,
		protoCaps: []string{capability.IsolatedFetchV1, capability.RecoveryArchiveV1}})
	f.claimAs(t, lane, bound)
	holds := func(run uuid.UUID) int {
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := holds(bound); n != 0 {
		t.Fatalf("profile-bound claim opened %d custody holds, want 0", n)
	}
	unbound := f.queuedRun(t, 431, false)
	f.claimAs(t, f.seedWorker(t, laneWorkerSpec{protoCaps: []string{capability.RecoveryArchiveV1}}), unbound)
	if n := holds(unbound); n != 1 {
		t.Fatalf("ordinary recovery-capable claim opened %d holds, want 1 (the control)", n)
	}
}

// TestLaneMirrorsLiveDB: the placement diagnostics agree with the claim. The spread never
// defers a profile-bound run away from the lane worker to a less-loaded ordinary peer (nor an
// unbound run to a lane peer), CountOnlineWorkersClaimableForRun counts exactly the lane
// worker for a profile-bound run, and the queued reason names the lane.
func TestLaneMirrorsLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 440, true)
	lane := f.laneWorker(t, run)
	// An idle ordinary peer with free slots that would be "strictly less loaded" than a busy
	// lane claimant. Give the lane worker a cap and an active run so it would defer.
	peer := f.seedWorker(t, laneWorkerSpec{protoCaps: []string{capability.IsolatedFetchV1}})
	f.env.exec(`UPDATE workers SET max_concurrent_runs = 4 WHERE id = ANY($1)`, []uuid.UUID{lane.ID, peer.ID})
	busy := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, $3, 'issue', 441, 't', 'd', 'running', $4)`, busy, f.userID, f.repo, lane.ID)
	f.env.exec(`UPDATE runs SET updated_at = now() WHERE id = $1`, run)
	lane, err := f.env.q.GetWorkerByID(f.env.ctx, lane.ID)
	if err != nil {
		t.Fatal(err)
	}

	n, err := f.env.q.CountOnlineWorkersClaimableForRun(f.env.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID: run, HeartbeatCutoff: pgconv.Time(time.Now().Add(-45 * time.Second)), CapabilityAware: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The busy lane worker has a free slot (1 of 4), the ordinary peer is excluded by the lane.
	if n != 1 {
		t.Fatalf("CountOnlineWorkersClaimableForRun = %d, want 1 (the lane worker only)", n)
	}

	// Spread: the lane claimant must not defer to the idle ordinary peer.
	got, err := f.env.q.ClaimRun(f.env.ctx, laneClaimParams(f.userID, lane))
	if err != nil || got.ID != run {
		t.Fatalf("busy lane worker ClaimRun = %v, %v; want the run (no deferral to a non-lane peer)", got.ID, err)
	}

	// Queued reason for a profile-bound run.
	run2 := f.queuedRun(t, 442, true)
	var row store.ListActiveRunsForHealthRow
	rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, codexCuratedModelsSlice())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == run2 {
			row = r
		}
	}
	if !row.EgressProfileID.Valid {
		t.Fatalf("ListActiveRunsForHealth row for the bound run carries no egress_profile_id: %+v", row)
	}
	if got := f.svc.queuedReason(f.env.ctx, time.Now(), row); got != reasonWaitingIsolatedLane {
		t.Fatalf("queuedReason = %q, want %q", got, reasonWaitingIsolatedLane)
	}
}

// TestProfileBoundFailedAssemblySettlesLiveDB: a recovery-capable lane worker's claim of a
// profile-bound run opens no custody hold, so when assembly then fails terminally (the owner's
// Anthropic secret is gone) finishRunClaim must expect zero holds and fail the run, not refuse
// with the custody-count mismatch and leave it claimed.
func TestProfileBoundFailedAssemblySettlesLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 450, true)
	lane := f.seedWorker(t, laneWorkerSpec{hosted: true, ephemeral: true, boundRun: run, isolated: true,
		protoCaps: []string{capability.IsolatedFetchV1, capability.RecoveryArchiveV1}})
	f.env.exec(`DELETE FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'`, f.userID)
	p, err := f.svc.Claim(f.env.ctx, lane, nil)
	if err != nil {
		t.Fatalf("Claim = %v, want the failed assembly settled (not %v)", err, errClaimRecoveryCustody)
	}
	if p != nil {
		t.Fatalf("Claim delivered run %s, want no payload", p.RunID)
	}
	var status string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id = $1`, run).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
}

type fakeEphemeralSettings struct {
	on  bool
	err error
}

func (f fakeEphemeralSettings) EphemeralWorkersEnabled(context.Context) (bool, error) {
	return f.on, f.err
}

// TestIsolatedLaneQueuedReasonLiveDB: the queued reason of a profile-bound run is truthful. A
// run requiring docker names docker (the lane never has it) whatever the kill-switch says;
// with the instance ephemeral kill-switch off the reason says provisioning is off; with it on,
// or unreadable, the generic lane wait stands.
func TestIsolatedLaneQueuedReasonLiveDB(t *testing.T) {
	f := newIsoFix(t)
	plain := f.queuedRun(t, 460, true)
	docker := f.queuedRun(t, 461, true)
	f.env.exec(`UPDATE runs SET required_capabilities = '{docker}' WHERE id = $1`, docker)
	rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, codexCuratedModelsSlice())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]store.ListActiveRunsForHealthRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, tc := range []struct {
		name     string
		settings EphemeralSettingsReader
		laneOff  bool
		run      uuid.UUID
		want     string
	}{
		{"switch on", fakeEphemeralSettings{on: true}, false, plain, reasonWaitingIsolatedLane},
		{"switch off", fakeEphemeralSettings{on: false}, false, plain, reasonIsolatedLaneProvisioningOff},
		{"switch unreadable", fakeEphemeralSettings{err: errors.New("cold cache")}, false, plain, reasonWaitingIsolatedLane},
		{"no reader", nil, false, plain, reasonWaitingIsolatedLane},
		{"docker, switch on", fakeEphemeralSettings{on: true}, false, docker, reasonIsolatedLaneNeedsDocker},
		{"docker, switch off", fakeEphemeralSettings{on: false}, false, docker, reasonIsolatedLaneNeedsDocker},
		// Issue #1965: a deployment without the lane says so whatever the kill-switch reads.
		{"lane off, switch on", fakeEphemeralSettings{on: true}, true, plain, reasonIsolatedLaneNotEnabled},
		{"lane off, switch off", fakeEphemeralSettings{on: false}, true, plain, reasonIsolatedLaneNotEnabled},
		{"docker, lane off", fakeEphemeralSettings{on: true}, true, docker, reasonIsolatedLaneNeedsDocker},
	} {
		f.svc.ephemeralSettings = tc.settings
		f.svc.SetIsolatedLaneEnabled(!tc.laneOff)
		if got := f.svc.queuedReason(f.env.ctx, time.Now(), byID[tc.run]); got != tc.want {
			t.Errorf("%s: queuedReason = %q, want %q", tc.name, got, tc.want)
		}
	}
	f.svc.SetIsolatedLaneEnabled(true)
}
