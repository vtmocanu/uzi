package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep PostgreSQL locks, savepoints, errors and commits real. Inject only at the
// optional projection query boundary, including within nested transactions.
type cooldownProjectionFault struct {
	runID     uuid.UUID
	mode      string
	hits      int
	rollbacks int
}

type cooldownProjectionTx struct {
	pgx.Tx
	fault  *cooldownProjectionFault
	nested bool
}

func (tx *cooldownProjectionTx) Begin(ctx context.Context) (pgx.Tx, error) {
	sp, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &cooldownProjectionTx{Tx: sp, fault: tx.fault, nested: true}, nil
}

func (tx *cooldownProjectionTx) Rollback(ctx context.Context) error {
	if tx.nested {
		tx.fault.rollbacks++
	}
	return tx.Tx.Rollback(ctx)
}

func (tx *cooldownProjectionTx) targets(args []any) bool {
	for _, arg := range args {
		if id, ok := arg.(uuid.UUID); ok && id == tx.fault.runID {
			return true
		}
	}
	return false
}

func (tx *cooldownProjectionTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	authority := strings.Contains(sql, "-- name: GetCodexOperationAuthority :one")
	source := strings.Contains(sql, "-- name: ValidCodexOperationSource :one")
	if tx.targets(args) && ((authority && tx.fault.mode != "source_sql" && tx.fault.mode != "write_sql") ||
		(source && tx.fault.mode == "source_sql")) {
		tx.fault.hits++
		switch tx.fault.mode {
		case "missing_episode":
			// A valid row shape reaches the application's missing-episode error.
			sql = strings.Replace(sql, "t.episode_id,", "NULL::uuid AS episode_id,", 1)
		case "timeout":
			// A server statement timeout aborts the savepoint, not the connection.
			if _, err := tx.Tx.Exec(ctx, "SET LOCAL statement_timeout = '5ms'"); err != nil {
				return claimProjectionErrorRow{err: err}
			}
			return tx.Tx.QueryRow(ctx, "SELECT pg_sleep(0.1)")
		default:
			return tx.Tx.QueryRow(ctx, "SELECT 1 / 0")
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *cooldownProjectionTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.fault.mode == "write_sql" && tx.targets(args) &&
		strings.Contains(sql, "-- name: SetWorkerActiveCooldown :exec") {
		tx.fault.hits++
		return tx.Tx.Exec(ctx, "SELECT 1 / 0")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestActiveCooldownDisplayFailurePreservesCustodyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []struct {
		name  string
		value snapshotMode
	}{{"register", snapshotModeRegister}, {"heartbeat", snapshotModeHeartbeat}, {"claim", snapshotModeClaim}} {
		for _, failure := range []string{"authority_sql", "source_sql", "timeout", "missing_episode", "write_sql"} {
			t.Run(mode.name+"/"+failure, func(t *testing.T) {
				f := admissionServiceFixture(t, env)
				env.exec("UPDATE runs SET issue_iid=101 WHERE id=$1", f.runID)
				svc := snapshotSvc(env, testParams())
				env.exec("UPDATE workers SET snapshot_register_nonce='display-failure' WHERE id=$1", f.wkr.ID)
				source := seedOperationSource(t, env, f)
				if err := f.svc.RecordCodexAdmissionSignal(env.ctx, f.wkr, f.runID, admissionSignal("rate_limit")); err != nil {
					t.Fatal(err)
				}
				var repoID uuid.UUID
				if err := env.pool.QueryRow(env.ctx, "SELECT repo_id FROM runs WHERE id=$1", f.runID).Scan(&repoID); err != nil {
					t.Fatal(err)
				}
				sibling := env.seedCodexRun(t, f.userID, f.workerID, repoID)
				env.exec("UPDATE runs SET claim_generation=1, issue_iid=102 WHERE id=$1", sibling)
				if err := f.svc.FreezeCodexBinding(env.ctx, f.userID, sibling, f.aliasID, codexAuthModeSubscription); err != nil {
					t.Fatal(err)
				}
				pending := env.seedCodexRun(t, f.userID, f.workerID, repoID)
				env.exec("UPDATE runs SET claim_generation=1 WHERE id=$1", pending)

				badEntry := entry(f.runID, 1, "running", false)
				identity := operationRequest().CodexOperationIdentity
				identity.Source = source
				raw, err := json.Marshal(identity)
				if err != nil {
					t.Fatal(err)
				}
				badEntry.AccountCooldown = raw
				goodEntry := entry(sibling, 1, "running", false)
				raw, err = json.Marshal(operationRequest().CodexOperationIdentity)
				if err != nil {
					t.Fatal(err)
				}
				goodEntry.AccountCooldown = raw
				pendingEntry := entry(pending, 1, "running", true)
				snap := &ActiveSnapshot{RegisterNonce: "display-failure", SnapshotEpoch: 1,
					Active:     []ActiveRunEntry{badEntry, goodEntry, pendingEntry},
					PendingAll: []PendingOutcomeIdentity{{RunID: pending.String(), ClaimGeneration: 1}}}
				applied, err := applySnapshot(t, svc, env, f.wkr.ID, snap, mode.value)
				if err != nil || !applied {
					t.Fatalf("initial snapshot: applied=%v error=%v", applied, err)
				}
				assertDisplay := func(id uuid.UUID, want bool) {
					t.Helper()
					var allNull, complete bool
					if err := env.pool.QueryRow(env.ctx, `SELECT
 cooldown_operation IS NULL AND cooldown_class IS NULL AND cooldown_episode_id IS NULL
 AND cooldown_eligible_at IS NULL AND cooldown_observed_at IS NULL AND cooldown_until IS NULL,
 cooldown_operation IS NOT NULL AND cooldown_class='rate_limit' AND cooldown_episode_id IS NOT NULL
 AND cooldown_eligible_at>cooldown_observed_at AND cooldown_until>now()
 FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2`, f.workerID, id).Scan(&allNull, &complete); err != nil {
						t.Fatal(err)
					}
					if (want && !complete) || (!want && !allNull) {
						t.Fatalf("display for %s: complete=%v allNull=%v want=%v", id, complete, allNull, want)
					}
				}
				assertDisplay(f.runID, true)
				assertDisplay(sibling, true)
				// Make renewal observable without a real-time sleep.
				env.exec(`UPDATE worker_active_runs SET terminal_pending_until=now()-interval '1 second'
 WHERE worker_id=$1 AND run_id=$2`, f.workerID, pending)
				env.exec(`UPDATE worker_pending_outcome_identities SET reported_until=now()+interval '1 second'
 WHERE worker_id=$1 AND run_id=$2`, f.workerID, pending)
				pendingSince, pendingUntil := pendingIdentityTimes(t, env, f.workerID, pending)
				before := operationReadState(t, env, f)
				wkr, err := env.q.GetWorkerByID(env.ctx, f.workerID)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				fault := &cooldownProjectionFault{runID: f.runID, mode: failure}
				wrapped := &cooldownProjectionTx{Tx: tx, fault: fault}
				snap.SnapshotEpoch = 2
				snap.Active[0].Phase = "awaiting_approval"
				applied, err = svc.ReplaceWorkerActiveRuns(env.ctx, store.New(wrapped), wkr, snap, mode.value)
				if err != nil || !applied {
					t.Fatalf("display failure aborted custody: applied=%v error=%v", applied, err)
				}
				if err := tx.Commit(env.ctx); err != nil {
					t.Fatalf("custody transaction left aborted: %v", err)
				}
				if fault.hits != 1 || fault.rollbacks != 1 {
					t.Fatalf("fault not isolated: hits=%d rollbacks=%d", fault.hits, fault.rollbacks)
				}
				assertDisplay(f.runID, false)
				assertDisplay(sibling, true)
				since, until := pendingIdentityTimes(t, env, f.workerID, pending)
				if !since.Equal(pendingSince) || !until.After(pendingUntil) {
					t.Fatal("pending outcome identity age/renewal was lost")
				}
				for _, id := range []uuid.UUID{f.runID, sibling, pending} {
					row, ok := readActiveRun(t, env, f.workerID, id)
					if !ok || row.gen != 1 || row.epoch != 2 {
						t.Fatalf("custody missing/changed: id=%s row=%+v", id, row)
					}
					if id == pending && (!row.terminalPending || !row.untilFuture) {
						t.Fatalf("pending lease not renewed: %+v", row)
					}
					if id == f.runID && row.phase != "awaiting_approval" {
						t.Fatalf("phase not updated: %+v", row)
					}
				}
				if after := operationReadState(t, env, f); after != before {
					t.Fatal("display failure changed run lifecycle, policy or receipt")
				}
				// The next valid refresh recovers display without replacing custody.
				snap.SnapshotEpoch = 3
				applied, err = applySnapshot(t, svc, env, f.workerID, snap, mode.value)
				if err != nil || !applied {
					t.Fatalf("recovery snapshot: applied=%v error=%v", applied, err)
				}
				assertDisplay(f.runID, true)
			})
		}
	}
}
