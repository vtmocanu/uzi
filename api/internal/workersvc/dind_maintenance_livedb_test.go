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
			int64(blocker)).Scan(&blocked)
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

func TestDindMaintenanceReplacementReadinessLiveDB(t *testing.T) {
	env, w, svc, op := dindLiveFixture(t)
	transition := func(phase string) DindMaintenance {
		t.Helper()
		op.Phase = phase
		got, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op)
		if err != nil || got == nil {
			t.Fatalf("transition to %s: got=%+v err=%v", phase, got, err)
		}
		return *got
	}
	read := func() store.Worker {
		t.Helper()
		current, err := env.q.GetWorkerByID(env.ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	refuse := func(name, phase string) {
		t.Helper()
		before := read()
		request := op
		request.Phase = "complete"
		if got, err := svc.TransitionDindMaintenance(env.ctx, w.ID, request); !errors.Is(err, ErrDindMaintenanceConflict) {
			t.Fatalf("%s completed: got=%+v err=%v", name, got, err)
		}
		current := read()
		if current.MaintenancePhase != phase || !current.MaintenanceFenced ||
			current.MaintenanceRegisterNonce != w.MaintenanceRegisterNonce ||
			!current.MaintenanceActivityFloor.Time.Equal(before.MaintenanceActivityFloor.Time) {
			t.Fatalf("%s changed fenced operation: %+v", name, current)
		}
	}
	op = transition("ready")
	zero := 0
	if _, err := svc.AckDindMaintenance(env.ctx, w.ID, DindMaintenanceReadyACK{
		DindMaintenance: op, LocalClaims: &zero, LocalExecutions: &zero,
		CustodyClear: true, CustodyCheckedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	op = transition("stopping")
	refuse("stopping old registration", "stopping")
	op = transition("recycling")
	refuse("recycling old registration", "recycling")

	replacement := uuid.NewString()
	registered, err := env.q.RegisterWorker(env.ctx, store.RegisterWorkerParams{
		ID: w.ID, ProtocolCapabilities: []string{capability.DindMaintenanceV1},
		SnapshotRegisterNonce: pgconv.TextOrNull(replacement),
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.MaintenancePhase != "recycling" || !registered.MaintenanceFenced ||
		registered.MaintenanceRegisterNonce != op.RegisterNonce ||
		!registered.LastHeartbeatAt.Time.Equal(registered.DindRegisterFloor.Time) {
		t.Fatalf("registration lost fence/binding or stamped unequal heartbeat/floor: %+v", registered)
	}
	refuse("replacement registration without heartbeat", "recycling")
	fresh, err := svc.Heartbeat(env.ctx, read(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.LastHeartbeatAt.Time.After(fresh.DindRegisterFloor.Time) {
		t.Fatal("heartbeat did not follow replacement registration")
	}
	env.exec("UPDATE workers SET last_heartbeat_at = NULL WHERE id = $1", w.ID)
	refuse("missing replacement heartbeat", "recycling")
	env.exec(`UPDATE workers SET dind_register_floor = now() - interval '2 minutes',
		last_heartbeat_at = now() - interval '1 minute' WHERE id = $1`, w.ID)
	refuse("stale replacement heartbeat", "recycling")
	env.exec("UPDATE workers SET dind_register_floor = $2, last_heartbeat_at = $3, protocol_capabilities = '{}' WHERE id = $1",
		w.ID, fresh.DindRegisterFloor, fresh.LastHeartbeatAt)
	refuse("replacement without maintenance capability", "recycling")
	env.exec("UPDATE workers SET protocol_capabilities = $2 WHERE id = $1", w.ID, fresh.ProtocolCapabilities)

	// Terminal-only busy checking must include owned parked and queued lifecycles.
	for _, status := range []string{"queued", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait", "pool_wait", "paused", "recovery_wait"} {
		run := dindLiveRun(t, env, w, "issue", status, true)
		refuse("owned "+status, "recycling")
		env.exec("UPDATE runs SET status = 'completed' WHERE id = $1", run)
	}
	run := dindLiveRun(t, env, w, "issue", "completed", true)
	hold := uuid.New()
	env.exec(`INSERT INTO recovery_custody_holds
		(id, user_id, repo_id, run_id, generation, state, original_worker_id,
		 original_worker_identity, live_worker_id, live_run_id)
		SELECT $1, user_id, repo_id, id, claim_generation, 'open', $2, 'maintenance-test', $2, id
		FROM runs WHERE id = $3`, hold, w.ID, run)
	refuse("terminal run with open live custody", "recycling")
	env.exec(`UPDATE recovery_custody_holds SET state = 'discarded',
		live_worker_id = NULL, live_run_id = NULL WHERE id = $1`, hold)

	// The controller still sends the original operation binding after replacement.
	op = transition("complete")
	completed := read()
	if op.RegisterNonce != w.MaintenanceRegisterNonce || op.Fenced || op.ReadyACK ||
		completed.MaintenancePhase != "complete" || completed.MaintenanceFenced ||
		completed.MaintenanceOwnsDrain || completed.DrainingSince.Valid ||
		completed.SnapshotRegisterNonce.String != replacement {
		t.Fatalf("completion did not release fence/drain with original binding: op=%+v worker=%+v", op, completed)
	}
	// A lost completion response can be retried even after readiness grows stale.
	env.exec("UPDATE workers SET last_heartbeat_at = NULL, protocol_capabilities = '{}' WHERE id = $1", w.ID)
	for range 2 {
		retry := transition("complete")
		current := read()
		if retry != op || !current.MaintenanceActivityFloor.Time.Equal(completed.MaintenanceActivityFloor.Time) ||
			!current.UpdatedAt.Time.Equal(completed.UpdatedAt.Time) {
			t.Fatalf("completion retry changed binding/clocks: retry=%+v worker=%+v", retry, current)
		}
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
			prior := dindLiveRun(t, env, w, kind, status, lane == "continue")
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

func TestDindMaintenancePendingContinueUnassignedLiveDB(t *testing.T) {
	env, w, _, _ := dindLiveFixture(t)
	prior := dindLiveRun(t, env, w, "chat", "completed", true)
	if w.MaintenancePhase != "requested" || w.MaintenanceFenced {
		t.Fatalf("want pending worker before fence: %+v", w)
	}
	params := store.CountWorkerNonTerminalRunsParams{WorkerID: pgconv.UUID(w.ID), UserID: w.UserID}
	before, err := env.q.CountWorkerNonTerminalRuns(env.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	run, err := dindLiveClaim(env.ctx, env.q, w, "continue", prior)
	if err != nil || run.WorkerID.Valid || run.Status != "queued" ||
		!run.ResumeOfRunID.Valid || uuid.UUID(run.ResumeOfRunID.Bytes) != prior {
		t.Fatalf("pending Continue must preserve resume and queue unassigned: %+v err=%v", run, err)
	}
	after, err := env.q.CountWorkerNonTerminalRuns(env.ctx, params)
	if err != nil || after != before {
		t.Fatalf("pending Continue increased worker busy count: before=%d after=%d err=%v", before, after, err)
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
	ownChat := dindLiveRun(t, env, w, "chat", "queued", true)
	if _, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op); !errors.Is(err, ErrDindMaintenanceConflict) {
		t.Fatalf("owned queued chat did not block ready: %v", err)
	}
	chat, err := dindLiveClaim(env.ctx, env.q, w, "chat", uuid.Nil)
	if err != nil || chat.ID != ownChat || chat.Status != "claimed" ||
		!chat.WorkerID.Valid || uuid.UUID(chat.WorkerID.Bytes) != w.ID {
		t.Fatalf("own queued chat resume: %+v err=%v", chat, err)
	}
	finishedChat, applied, err := svc.SetState(env.ctx, w, ownChat, StateRequest{State: "completed"})
	if err != nil || !applied || finishedChat.Status != "completed" {
		t.Fatalf("finish owned chat: applied=%v status=%s err=%v", applied, finishedChat.Status, err)
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
	accepted := second
	for _, tc := range []struct {
		name      string
		epoch     time.Time
		sampledAt time.Time
		nilStats  bool
	}{
		{name: "future", epoch: base.Add(time.Hour), sampledAt: base.Add(time.Hour)},
		{name: "stale", epoch: base.Add(-40 * time.Second), sampledAt: base.Add(-40 * time.Second)},
		{name: "reversed", epoch: base, sampledAt: base},
		{name: "sampled_at_mismatch", epoch: base.Add(3 * time.Second), sampledAt: base.Add(4 * time.Second)},
		{name: "nil_stats", nilStats: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stats *WorkerStats
			if !tc.nilStats {
				stats = &WorkerStats{
					DindMeter:     &DindMeter{RegisterNonce: nonce, Epoch: tc.epoch.Unix(), SampledAt: tc.sampledAt},
					DiskDindBytes: &used, DiskDindTotalBytes: &total,
					DiskDindInodes: &inodes, DiskDindTotalInodes: &total,
				}
			}
			for _, evidence := range []struct {
				name   string
				streak int32
				below  bool
			}{
				{name: "pressure", streak: 2, below: false},
				{name: "below", streak: 0, below: true},
			} {
				t.Run(evidence.name, func(t *testing.T) {
					env.exec(`UPDATE workers SET dind_meter_epoch = $2, dind_meter_at = $3,
						dind_pressure_streak = $4, dind_below_threshold = $5
						WHERE id = $1`, w.ID, accepted.DindMeterEpoch, accepted.DindMeterAt.Time,
						evidence.streak, evidence.below)
					current, err := svc.Heartbeat(env.ctx, w, stats, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					if current.DindPressureStreak != 0 || current.DindBelowThreshold {
						t.Fatalf("invalid meter retained evidence: streak=%d below=%v", current.DindPressureStreak, current.DindBelowThreshold)
					}
					if current.DindMeterEpoch != accepted.DindMeterEpoch || !current.DindMeterAt.Valid ||
						!current.DindMeterAt.Time.Equal(accepted.DindMeterAt.Time) {
						t.Fatal("invalid meter changed the last accepted watermark")
					}
				})
			}
		})
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

func TestDindMaintenanceLegacyDrainHandoffLiveDB(t *testing.T) {
	for _, release := range []string{"uncordon", "registration"} {
		t.Run(release, func(t *testing.T) {
			env, w, svc, target := dindLiveFixture(t)
			env.exec(`UPDATE workers SET maintenance_phase = '', maintenance_fenced = false,
				maintenance_owns_drain = false, draining_since = NULL WHERE id = $1`, w.ID)
			if rows, err := env.q.CordonHostedWorker(env.ctx, w.ID); err != nil || rows != 1 {
				t.Fatalf("legacy cordon: rows=%d err=%v", rows, err)
			}
			request := DindMaintenance{Phase: "requested", DeploymentUID: target.DeploymentUID, PVCUID: target.PVCUID}
			op, err := svc.TransitionDindMaintenance(env.ctx, w.ID, request)
			if err != nil || op == nil {
				t.Fatalf("request: %+v err=%v", op, err)
			}
			op.Phase = "ready"
			op, err = svc.TransitionDindMaintenance(env.ctx, w.ID, *op)
			if err != nil || op == nil {
				t.Fatalf("ready: %+v err=%v", op, err)
			}
			before, err := env.q.GetWorkerByID(env.ctx, w.ID)
			if err != nil || before.MaintenanceOwnsDrain || !before.MaintenanceFenced || !before.DrainingSince.Valid {
				t.Fatalf("legacy drain before release: %+v err=%v", before, err)
			}
			wantPhase := "ready"
			if release == "uncordon" {
				if rows, err := env.q.UncordonHostedWorker(env.ctx, w.ID); err != nil || rows != 1 {
					t.Fatalf("legacy uncordon: rows=%d err=%v", rows, err)
				}
			} else {
				wantPhase = "requested"
				if _, err := env.q.RegisterWorker(env.ctx, store.RegisterWorkerParams{
					ID: w.ID, ProtocolCapabilities: []string{capability.DindMaintenanceV1},
					SnapshotRegisterNonce: pgconv.TextOrNull(uuid.NewString()),
				}); err != nil {
					t.Fatal(err)
				}
			}
			current, err := env.q.GetWorkerByID(env.ctx, w.ID)
			if err != nil || current.MaintenancePhase != wantPhase || !current.MaintenanceOwnsDrain || !current.MaintenanceFenced ||
				!current.DrainingSince.Valid || !current.DrainingSince.Time.Equal(before.DrainingSince.Time) {
				t.Fatalf("legacy release lost pending drain/fence or ownership: %+v err=%v", current, err)
			}
			op.Phase, op.Reason = "cancelled", "recycle_disabled"
			if cancelled, err := svc.TransitionDindMaintenance(env.ctx, w.ID, *op); err != nil || cancelled == nil {
				t.Fatalf("controller cancellation: %+v err=%v", cancelled, err)
			}
			current, err = env.q.GetWorkerByID(env.ctx, w.ID)
			if err != nil || current.MaintenancePhase != "cancelled" || current.MaintenanceOwnsDrain ||
				current.MaintenanceFenced || current.DrainingSince.Valid {
				t.Fatalf("cancellation retained handed-off drain/fence: %+v err=%v", current, err)
			}
		})
	}
}

func TestDindMaintenanceDrainCancellationRegistrationLiveDB(t *testing.T) {
	env, w, svc, target := dindLiveFixture(t)
	env.exec(`UPDATE workers SET maintenance_phase = '', maintenance_fenced = false,
		maintenance_owns_drain = false, draining_since = NULL WHERE id = $1`, w.ID)
	read := func() store.Worker {
		t.Helper()
		current, err := env.q.GetWorkerByID(env.ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	transition := func(op DindMaintenance) DindMaintenance {
		t.Helper()
		current, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op)
		if err != nil || current == nil {
			t.Fatalf("transition to %s: %+v err=%v", op.Phase, current, err)
		}
		return *current
	}
	request := DindMaintenance{Phase: "requested", DeploymentUID: target.DeploymentUID, PVCUID: target.PVCUID}
	polled := transition(request)
	current := read()
	if current.MaintenancePhase != "requested" || !current.MaintenanceOwnsDrain || !current.DrainingSince.Valid {
		t.Fatalf("request did not acquire drain: %+v", current)
	}
	drain := current.DrainingSince.Time
	polled.Phase = "ready"
	oldReady := transition(polled)
	current = read()
	if current.MaintenancePhase != "ready" || !current.MaintenanceOwnsDrain || !current.MaintenanceFenced ||
		!current.DrainingSince.Valid || !current.DrainingSince.Time.Equal(drain) {
		t.Fatalf("ready did not persist fence and owned drain: %+v", current)
	}
	zero := 0
	ack := DindMaintenanceReadyACK{
		DindMaintenance: oldReady, LocalClaims: &zero, LocalExecutions: &zero,
		CustodyClear: true, CustodyCheckedAt: time.Now(),
	}
	if got, err := svc.AckDindMaintenance(env.ctx, w.ID, ack); err != nil || got == nil || !got.ReadyACK {
		t.Fatalf("initial ready ACK: %+v err=%v", got, err)
	}
	freshNonce := uuid.NewString()
	if _, err := env.q.RegisterWorker(env.ctx, store.RegisterWorkerParams{
		ID: w.ID, ProtocolCapabilities: []string{capability.DindMaintenanceV1},
		SnapshotRegisterNonce: pgconv.TextOrNull(freshNonce),
	}); err != nil {
		t.Fatal(err)
	}
	current = read()
	if current.MaintenancePhase != "requested" || current.MaintenanceReadyAck || current.MaintenanceAckAt.Valid ||
		current.DindPressureStreak != 0 || current.DindMeterEpoch != 0 || current.DindMeterAt.Valid || current.DindBelowThreshold ||
		current.SnapshotRegisterNonce.String != freshNonce || !current.MaintenanceFenced || !current.MaintenanceOwnsDrain ||
		!current.DrainingSince.Valid || !current.DrainingSince.Time.Equal(drain) {
		t.Fatalf("registration lost fence/drain or retained old evidence: %+v", current)
	}
	ack.CustodyCheckedAt = time.Now()
	if _, err := svc.AckDindMaintenance(env.ctx, w.ID, ack); !errors.Is(err, ErrDindMaintenanceConflict) {
		t.Fatalf("old ACK accepted after registration: %v", err)
	}
	// Seed admission evidence as dindLiveFixture does; meter SQL is exercised separately.
	env.exec(`UPDATE workers SET dind_register_floor = now() - interval '1 minute',
		dind_meter_at = date_trunc('second', now()) - interval '1 second',
		dind_pressure_streak = 2 WHERE id = $1`, w.ID)
	polled.Phase = "requested"
	refreshed := transition(polled)
	current = read()
	if refreshed.ID != polled.ID || refreshed.Nonce == polled.Nonce || refreshed.RegisterNonce != freshNonce ||
		!refreshed.Fenced || current.MaintenancePhase != "requested" || !current.MaintenanceFenced ||
		!current.MaintenanceOwnsDrain || !current.DrainingSince.Valid || !current.DrainingSince.Time.Equal(drain) {
		t.Fatalf("old polled binding did not refresh with fence/drain preserved: %+v worker=%+v", refreshed, current)
	}
	refreshed.Phase = "ready"
	freshReady := transition(refreshed)
	current = read()
	if current.MaintenancePhase != "ready" || !current.MaintenanceFenced || current.MaintenanceReadyAck {
		t.Fatalf("refreshed ready fence not persisted: %+v", current)
	}
	ack.CustodyCheckedAt = time.Now()
	if _, err := svc.AckDindMaintenance(env.ctx, w.ID, ack); !errors.Is(err, ErrDindMaintenanceConflict) {
		t.Fatalf("old ACK accepted against refreshed ready binding: %v", err)
	}
	ack.DindMaintenance = freshReady
	ack.CustodyCheckedAt = time.Now()
	if got, err := svc.AckDindMaintenance(env.ctx, w.ID, ack); err != nil || got == nil || !got.ReadyACK {
		t.Fatalf("fresh ready ACK: %+v err=%v", got, err)
	}
	current = read()
	if current.MaintenancePhase != "ready" || !current.MaintenanceFenced || !current.MaintenanceReadyAck ||
		!current.MaintenanceAckAt.Valid || !current.MaintenanceAckAt.Time.Equal(ack.CustodyCheckedAt.Truncate(time.Microsecond)) {
		t.Fatalf("fresh ACK not persisted: %+v", current)
	}
	freshReady.Phase, freshReady.Reason = "cancelled", "recycle_disabled"
	transition(freshReady)
	current = read()
	if current.MaintenancePhase != "cancelled" || current.MaintenanceFenced || current.MaintenanceOwnsDrain ||
		current.DrainingSince.Valid || current.MaintenanceReadyAck || current.MaintenanceAckAt.Valid {
		t.Fatalf("toggle-off cancellation retained owned drain/fence: %+v", current)
	}

	// A pre-existing controller cordon belongs to the controller, including after cancellation.
	if rows, err := env.q.CordonHostedWorker(env.ctx, w.ID); err != nil || rows != 1 {
		t.Fatalf("legacy cordon: rows=%d err=%v", rows, err)
	}
	legacyDrain := read().DrainingSince
	legacy := transition(request)
	legacy.Phase = "ready"
	legacy = transition(legacy)
	current = read()
	if current.MaintenanceOwnsDrain || !current.MaintenanceFenced || !current.DrainingSince.Valid ||
		!current.DrainingSince.Time.Equal(legacyDrain.Time) {
		t.Fatalf("maintenance took ownership of legacy drain: %+v", current)
	}
	legacy.Phase, legacy.Reason = "cancelled", "recycle_disabled"
	transition(legacy)
	current = read()
	if current.MaintenancePhase != "cancelled" || current.MaintenanceFenced || current.MaintenanceOwnsDrain ||
		!current.DrainingSince.Valid || !current.DrainingSince.Time.Equal(legacyDrain.Time) {
		t.Fatalf("cancellation cleared legacy drain: %+v", current)
	}
	if rows, err := env.q.UncordonHostedWorker(env.ctx, w.ID); err != nil || rows != 1 {
		t.Fatalf("legacy uncordon: rows=%d err=%v", rows, err)
	}
	if current := read(); current.DrainingSince.Valid {
		t.Fatalf("uncordon retained legacy drain: %+v", current)
	}
}
