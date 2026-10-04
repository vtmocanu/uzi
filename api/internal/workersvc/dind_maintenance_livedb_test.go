package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Reuse the existing migrated LiveDB environment. SQL seeds only admission
// evidence; every fence below goes through TransitionDindMaintenance.
func dindLiveFixture(t *testing.T) (codexTestEnv, store.Worker, *Service, DindMaintenance) {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, worker, _ := env.seedCodexInfra(t)
	nonce := uuid.NewString()
	env.exec(`UPDATE workers SET kind = 'hosted', template_declared = 'base', hosted_size = 'm', docker_enabled = true,
		protocol_capabilities = $2, snapshot_register_nonce = $3,
		dind_register_floor = now() - interval '1 minute',
		dind_meter_at = date_trunc('second', now()) - interval '1 second',
		dind_pressure_streak = 2, last_heartbeat_at = now(),
		maintenance_id = $4, maintenance_nonce = $5, maintenance_phase = 'requested',
		maintenance_deployment_uid = $6, maintenance_pvc_uid = $7,
		maintenance_register_nonce = $3, maintenance_activity_floor = now() - interval '1 minute'
		WHERE id = $1`, worker, []string{capability.DindMaintenanceV1}, nonce,
		uuid.New(), uuid.NewString(), uuid.NewString(), uuid.NewString())
	w, err := env.q.GetWorkerByID(env.ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if w.UserID != user {
		t.Fatal("fixture tenant mismatch")
	}
	p := testParams()
	p.DiskPressureThreshold = 0.9
	svc := New(env.q, env.box, p)
	svc.SetTxBeginner(env.pool)
	op := *DindMaintenanceFromWorker(w)
	op.Phase = "ready"
	return env, w, svc, op
}

func dindLiveRun(t *testing.T, env codexTestEnv, w store.Worker, kind, status string, owned bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var owner any
	if owned {
		owner = w.ID
	}
	if kind == "chat" {
		env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title,
			trigger_source, harness, status, worker_id)
			VALUES ($1, $2, 'chat', 'maintenance chat', '', 'maintenance chat',
			'chat', 'claude', $3, $4)`, id, w.UserID, status, owner)
	} else {
		var repo uuid.UUID
		if err := env.pool.QueryRow(env.ctx, `SELECT r.id FROM repos r JOIN forge_connections f
			ON f.id = r.connection_id WHERE f.user_id = $1 LIMIT 1`, w.UserID).Scan(&repo); err != nil {
			t.Fatal(err)
		}
		// A fresh UUID-derived issue number keeps the fixture's active-issue key unique.
		env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title,
			issue_description, kind, harness, status, worker_id)
			VALUES ($1, $2, $3, $4, 'maintenance issue', '', 'issue', 'claude', $5, $6)`,
			id, w.UserID, repo, int64(id.ID()), status, owner)
	}
	return id
}

func dindLiveClaim(ctx context.Context, q *store.Queries, w store.Worker, lane string, prior uuid.UUID) (store.Run, error) {
	switch lane {
	case "run":
		p := claimRunParamsFor(w)
		p.CapabilityAware = false
		return q.ClaimRun(ctx, p)
	case "chat":
		return q.ClaimChatRun(ctx, store.ClaimChatRunParams{
			UserID: w.UserID, WorkerID: pgconv.UUID(w.ID),
			AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
		})
	default:
		return q.CreateChatContinueRun(ctx, store.CreateChatContinueRunParams{
			UserID: w.UserID, WorkerID: pgconv.UUID(w.ID),
			ResumeOfRunID: pgconv.UUID(prior), IssueTitle: "maintenance continuation",
			Title: pgconv.TextOrNull("maintenance continuation"),
		})
	}
}

// A bounded poll observes a real PostgreSQL lock wait, rather than assuming
// a goroutine has reached SQL after an arbitrary delay. Failure stops this case.
func dindLiveWaitBlocked(t *testing.T, ctx context.Context, env codexTestEnv, blocker uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := env.pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid)))`,
			int32(blocker)).Scan(&blocked)
		if err != nil {
			t.Fatalf("observe worker lock: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("worker row never blocked the competing SQL: %v", ctx.Err())
		}
	}
}

type dindLiveResult struct {
	run store.Run
	err error
}

// The wrapper pauses only Commit; all production queries execute against the
// real transaction. The context bounds the pause, including failure cleanup.
type dindLiveCommitTx struct {
	pgx.Tx
	reached chan struct{}
	release chan struct{}
}

func (tx *dindLiveCommitTx) Begin(context.Context) (pgx.Tx, error) {
	return tx, nil
}

func (tx *dindLiveCommitTx) Commit(ctx context.Context) error {
	close(tx.reached)
	select {
	case <-tx.release:
		return tx.Tx.Commit(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDindMaintenanceOwnershipBeforeFenceLiveDB(t *testing.T) {
	for _, lane := range []string{"run", "chat", "continue"} {
		t.Run(lane, func(t *testing.T) {
			env, w, svc, op := dindLiveFixture(t)
			ctx, cancel := context.WithTimeout(env.ctx, 8*time.Second)
			defer cancel()
			kind, status := "issue", "queued"
			if lane != "run" {
				kind = "chat"
			}
			if lane == "continue" {
				status = "completed"
			}
			prior := dindLiveRun(t, env, w, kind, status, true)
			// Ownership begins before maintenance is requested. A NEW Continue
			// after requested must instead be queued unassigned.
			env.exec(`UPDATE workers SET maintenance_phase = '' WHERE id = $1`, w.ID)
			tx, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			qtx := store.New(tx)
			if _, err := qtx.GetWorkerForUpdate(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
			got, err := dindLiveClaim(ctx, qtx, w, lane, prior)
			if err != nil || !got.WorkerID.Valid || uuid.UUID(got.WorkerID.Bytes) != w.ID {
				t.Fatalf("ownership query: run=%+v err=%v", got, err)
			}
			if lane != "continue" && (got.ID != prior || got.Status != "claimed") {
				t.Fatalf("claimed %s/%s, want %s/claimed", got.ID, got.Status, prior)
			}
			if _, err := tx.Exec(ctx, `UPDATE workers SET maintenance_phase = 'requested' WHERE id = $1`, w.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := svc.TransitionDindMaintenance(ctx, w.ID, op)
				done <- err
			}()
			// Join the goroutine even if a lock-observation assertion fails.
			defer func() {
				cancel()
				_ = tx.Rollback(context.Background())
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("fence goroutine did not stop")
				}
			}()
			dindLiveWaitBlocked(t, ctx, env, tx.Conn().PgConn().PID())
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			// Refill for the deferred join after the result has been consumed.
			done <- err
			if !errors.Is(err, ErrDindMaintenanceConflict) {
				t.Fatalf("ownership committed before fence: %v", err)
			}
			current, err := env.q.GetWorkerByID(ctx, w.ID)
			if err != nil || current.MaintenanceFenced || current.MaintenancePhase != "requested" {
				t.Fatalf("busy worker fenced: phase=%s fenced=%v err=%v",
					current.MaintenancePhase, current.MaintenanceFenced, err)
			}
		})
	}
}

func TestDindMaintenanceFenceBeforeOwnershipLiveDB(t *testing.T) {
	for _, lane := range []string{"run", "chat", "continue"} {
		t.Run(lane, func(t *testing.T) {
			env, w, svc, op := dindLiveFixture(t)
			ctx, cancel := context.WithTimeout(env.ctx, 8*time.Second)
			defer cancel()
			kind, status := "issue", "queued"
			if lane != "run" {
				kind = "chat"
			}
			if lane == "continue" {
				status = "completed"
			}
			prior := dindLiveRun(t, env, w, kind, status, lane == "continue")
			tx, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := store.New(tx).GetWorkerForUpdate(ctx, w.ID); err != nil {
				t.Fatal(err)
			}
			paused := &dindLiveCommitTx{Tx: tx, reached: make(chan struct{}), release: make(chan struct{})}
			svc.SetTxBeginner(paused)
			fenceDone := make(chan error, 1)
			go func() {
				_, err := svc.TransitionDindMaintenance(ctx, w.ID, op)
				fenceDone <- err
			}()
			claimDone := make(chan dindLiveResult, 1)
			startedClaim := false
			defer func() {
				cancel()
				select {
				case <-fenceDone:
				case <-time.After(2 * time.Second):
					t.Error("fence goroutine did not stop")
				}
				if startedClaim {
					select {
					case <-claimDone:
					case <-time.After(2 * time.Second):
						t.Error("ownership goroutine did not stop")
					}
				}
			}()
			select {
			case <-paused.reached:
			case err := <-fenceDone:
				fenceDone <- err
				t.Fatalf("fence failed before commit: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			startedClaim = true
			go func() {
				run, err := dindLiveClaim(ctx, env.q, w, lane, prior)
				claimDone <- dindLiveResult{run: run, err: err}
			}()
			dindLiveWaitBlocked(t, ctx, env, tx.Conn().PgConn().PID())
			close(paused.release)
			err = <-fenceDone
			fenceDone <- err
			if err != nil {
				t.Fatalf("fence commit: %v", err)
			}
			got := <-claimDone
			claimDone <- got
			if lane == "continue" {
				if got.err != nil || got.run.WorkerID.Valid || got.run.Status != "queued" {
					t.Fatalf("fenced Continue must queue unassigned: %+v err=%v", got.run, got.err)
				}
			} else {
				if !errors.Is(got.err, pgx.ErrNoRows) {
					t.Fatalf("fenced claim: %s err=%v", got.run.ID, got.err)
				}
				var owner any
				var status string
				if err := env.pool.QueryRow(ctx, `SELECT status, worker_id FROM runs WHERE id = $1`,
					prior).Scan(&status, &owner); err != nil || status != "queued" || owner != nil {
					t.Fatalf("fenced claim mutated queue: status=%s owner=%v err=%v", status, owner, err)
				}
			}
			current, err := env.q.GetWorkerByID(ctx, w.ID)
			if err != nil || !current.MaintenanceFenced || current.MaintenancePhase != "ready" {
				t.Fatalf("ready fence not persisted: %+v err=%v", current, err)
			}
			svc.SetTxBeginner(env.pool)
			// The service consumes the fresh persisted fence as well.
			if lane == "run" {
				payload, err := svc.Claim(ctx, current, nil)
				if err != nil || payload != nil {
					t.Fatalf("Service.Claim after fence: payload=%+v err=%v", payload, err)
				}
			} else {
				payload, err := svc.ClaimChat(ctx, current)
				if err != nil || payload != nil {
					t.Fatalf("Service.ClaimChat after fence: payload=%+v err=%v", payload, err)
				}
			}
		})
	}
}

func TestDindMaintenanceEveryNonterminalBlocksReadyACKLiveDB(t *testing.T) {
	for _, status := range []string{
		"queued", "claimed", "running", "awaiting_approval", "awaiting_input",
		"awaiting_followup", "limit_wait", "pool_wait", "paused", "recovery_wait",
	} {
		t.Run(status, func(t *testing.T) {
			env, w, svc, op := dindLiveFixture(t)
			id := dindLiveRun(t, env, w, "issue", status, true)
			if _, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op); !errors.Is(err, ErrDindMaintenanceConflict) {
				t.Fatalf("%s ownership did not block ready: %v", status, err)
			}
			// Even zero local counts cannot override the authoritative SQL ownership.
			env.exec(`UPDATE workers SET maintenance_phase = 'ready', maintenance_fenced = true WHERE id = $1`, w.ID)
			zero := 0
			_, err := svc.AckDindMaintenance(env.ctx, w.ID, DindMaintenanceReadyACK{
				DindMaintenance: op, LocalClaims: &zero, LocalExecutions: &zero,
				CustodyClear: true, CustodyCheckedAt: time.Now(),
			})
			if !errors.Is(err, ErrDindMaintenanceConflict) {
				t.Fatalf("%s ownership accepted zero-count ACK: %v", status, err)
			}
			current, err := env.q.GetWorkerByID(env.ctx, w.ID)
			if err != nil || current.MaintenanceReadyAck {
				t.Fatalf("ACK persisted for owned run %s: ack=%v err=%v", id, current.MaintenanceReadyAck, err)
			}
		})
	}
}

func TestDindMaintenancePendingOwnParkResumesLiveDB(t *testing.T) {
	env, w, svc, op := dindLiveFixture(t)
	dindLiveRun(t, env, w, "issue", "queued", false)
	dindLiveRun(t, env, w, "chat", "queued", false)
	for _, lane := range []string{"run", "chat"} {
		if _, err := dindLiveClaim(env.ctx, env.q, w, lane, uuid.Nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("pending maintenance admitted new %s: %v", lane, err)
		}
	}
	if payload, err := svc.Claim(env.ctx, w, nil); err != nil || payload != nil {
		t.Fatalf("pending Service.Claim: payload=%+v err=%v", payload, err)
	}
	if payload, err := svc.ClaimChat(env.ctx, w); err != nil || payload != nil {
		t.Fatalf("pending Service.ClaimChat: payload=%+v err=%v", payload, err)
	}
	own := dindLiveRun(t, env, w, "issue", "recovery_wait", true)
	env.exec(`UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1`, own)
	if _, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op); !errors.Is(err, ErrDindMaintenanceConflict) {
		t.Fatalf("park did not block ready: %v", err)
	}
	if _, err := env.q.PromoteRecoveryWaitRuns(env.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatal(err)
	}
	run, err := dindLiveClaim(env.ctx, env.q, w, "run", uuid.Nil)
	if err != nil || run.ID != own || run.Status != "claimed" {
		t.Fatalf("own promoted park: id=%s status=%s err=%v", run.ID, run.Status, err)
	}
	finished, applied, err := svc.SetState(env.ctx, w, own, StateRequest{State: "completed"})
	if err != nil || !applied || finished.Status != "completed" {
		t.Fatalf("finish owned run: applied=%v status=%s err=%v", applied, finished.Status, err)
	}
	ready, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op)
	if err != nil || ready == nil || !ready.Fenced {
		t.Fatalf("ready after own completion: %+v err=%v", ready, err)
	}
}

func TestDindMaintenanceMeterDistinctEpochsLiveDB(t *testing.T) {
	env, w, svc, _ := dindLiveFixture(t)
	env.exec(`UPDATE workers SET maintenance_phase = '', maintenance_id = NULL,
		dind_pressure_streak = 0, dind_meter_epoch = 0, dind_meter_at = NULL WHERE id = $1`, w.ID)
	register := func(nonce string) {
		t.Helper()
		if _, err := env.q.RegisterWorker(env.ctx, store.RegisterWorkerParams{
			ID: w.ID, ProtocolCapabilities: []string{capability.DindMaintenanceV1},
			SnapshotRegisterNonce: pgconv.TextOrNull(nonce),
		}); err != nil {
			t.Fatal(err)
		}
		// Move only the registration floor into the past; epochs remain valid
		// Unix seconds inside SQL's freshness window without a 30-second sleep.
		env.exec(`UPDATE workers SET dind_register_floor = now() - interval '1 minute' WHERE id = $1`, w.ID)
	}
	nonce := uuid.NewString()
	register(nonce)
	base := time.Now().Truncate(time.Second).Add(-20 * time.Second)
	used, total, inodes := int64(1), int64(100), int64(95)
	beat := func(at time.Time, registration string, want int32, below bool) store.Worker {
		t.Helper()
		stats := &WorkerStats{
			DindMeter:     &DindMeter{RegisterNonce: registration, Epoch: at.Unix(), SampledAt: at},
			DiskDindBytes: &used, DiskDindTotalBytes: &total,
			DiskDindInodes: &inodes, DiskDindTotalInodes: &total,
		}
		current, err := svc.Heartbeat(env.ctx, w, stats, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if current.DindPressureStreak != want || current.DindBelowThreshold != below ||
			current.StatsDiskPressureStreak != 0 || current.NixPressure || current.DataPressure {
			t.Fatalf("meter: streak=%d below=%v legacy=%d nix=%v data=%v; want %d/%v and legacy false",
				current.DindPressureStreak, current.DindBelowThreshold, current.StatsDiskPressureStreak,
				current.NixPressure, current.DataPressure, want, below)
		}
		return current
	}
	first := beat(base, nonce, 1, false)
	for range 3 {
		duplicate := beat(base, nonce, 1, false)
		if duplicate.DindMeterEpoch != first.DindMeterEpoch || !duplicate.DindMeterAt.Time.Equal(first.DindMeterAt.Time) {
			t.Fatal("duplicate heartbeat advanced meter evidence")
		}
	}
	second := beat(base.Add(time.Second), nonce, 2, false)
	if second.DindMeterEpoch != base.Add(time.Second).Unix() {
		t.Fatal("second distinct epoch not persisted")
	}
	for range 3 {
		beat(base.Add(time.Second), nonce, 2, false)
	}
	inodes = 1
	beat(base.Add(2*time.Second), nonce, 0, true)
	inodes = 95
	beat(base.Add(3*time.Second), nonce, 1, false)
	beat(base.Add(4*time.Second), nonce, 2, false)
	freshNonce := uuid.NewString()
	register(freshNonce)
	reset, err := env.q.GetWorkerByID(env.ctx, w.ID)
	if err != nil || reset.DindPressureStreak != 0 || reset.DindMeterEpoch != 0 || reset.DindMeterAt.Valid || reset.DindBelowThreshold {
		t.Fatalf("registration did not reset evidence: %+v err=%v", reset, err)
	}
	beat(base.Add(5*time.Second), nonce, 0, false)
	beat(base.Add(6*time.Second), freshNonce, 1, false)
	beat(base.Add(7*time.Second), freshNonce, 2, false)
}
