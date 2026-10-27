package workersvc

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

//go:embed testdata/task-checkpoint-legacy/*.sql testdata/task-checkpoint-legacy/*.json
var historicalCoordinateFixtures embed.FS

// Fixture SQL is the archived generated literal, not today's sqlc query. Exec
// deliberately discards RETURNING rows, whose shape predates the new column.
func historicalCoordinateQuery(t *testing.T, name string, params any) (string, []any) {
	t.Helper()
	dir := "testdata/task-checkpoint-legacy"
	sql, err := historicalCoordinateFixtures.ReadFile(path.Join(dir, "legacy-"+name+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := historicalCoordinateFixtures.ReadFile(path.Join(dir, "legacy-"+name+"-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		SourceSHA string   `json:"source_sha"`
		Bindings  []string `json:"bindings"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.SourceSHA != "03d273fde1901d4185ac7590bae4b6a9f78ef8ea" {
		t.Fatalf("unexpected historical source %q", metadata.SourceSHA)
	}
	if name == "ReadoptRunsFromSnapshot" {
		return string(sql), []any{params}
	}
	value := reflect.ValueOf(params)
	args := make([]any, len(metadata.Bindings))
	for i, field := range metadata.Bindings {
		v := value.FieldByName(field)
		if !v.IsValid() || !v.CanInterface() {
			t.Fatalf("historical binding %d %q missing", i+1, field)
		}
		args[i] = v.Interface()
	}
	if len(args) == 0 {
		t.Fatal("empty historical parameter binding")
	}
	return string(sql), args
}

// Snapshot complete persisted rows, including closed custody records: a count of
// open holds alone misses a changed pre-existing hold or a new generation-2 hold.
func coordinateAuthoritySnapshot(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := env.pool.QueryRow(env.ctx, `SELECT jsonb_build_object(
 'run', (SELECT to_jsonb(r) FROM runs r WHERE id=$1),
 'custody', COALESCE((SELECT jsonb_agg(to_jsonb(h) ORDER BY h.generation,h.id)
 FROM recovery_custody_holds h WHERE run_id=$1),'[]'::jsonb))::text`, id).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCheckpointCoordinatesArchivedAdmissionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, name := range []string{"ClaimRun", "ReadoptRunsFromSnapshot", "FrozenReadoptRunsFromSnapshot"} {
		phases := []string{"claim"}
		if name != "ClaimRun" {
			phases = []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"}
		}
		for _, phase := range phases {
			for _, capable := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/capable_%v", name, phase, capable), func(t *testing.T) {
					f := newCustodyContinuationFixture(t, env)
					run := claimCoordinateTask(t, f)
					env.exec("UPDATE runs SET status='queued' WHERE id=$1", run.ID)
					caps := []string{capability.RecoveryArchiveV1}
					if capable {
						caps = append(caps, capability.TaskCheckpointCoordinatesV1)
					}
					env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, caps)
					if name != "ClaimRun" {
						env.exec(`INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
      VALUES($1,$2,$3,$4,false,1,now())`, f.workerID, run.ID, run.ClaimGeneration, phase)
					}
					before := coordinateAuthoritySnapshot(t, env, run.ID)
					tx, err := env.pool.Begin(env.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(env.ctx) }()
					q := store.New(tx)
					if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
						t.Fatal(err)
					}
					// No OptInCheckpointCoordinates call: this is an old API process.
					var params any = f.workerID
					if name == "ClaimRun" {
						p := coordinateClaimParams(f)
						p.WorkerProtocolCaps = caps
						params = p
					}
					if name == "FrozenReadoptRunsFromSnapshot" {
						locks, err := captureWorkerRecoveryLocks(env.ctx, q, f.workerID, []validatedEntry{{runID: run.ID}}, validatedFinalizeResume{})
						if err != nil {
							t.Fatal(err)
						}
						ledger, parents, err := locks.parameters()
						if err != nil {
							t.Fatal(err)
						}
						params = store.FrozenReadoptRunsFromSnapshotParams{WorkerID: f.workerID, FrozenTargets: ledger, LockedParentIds: parents}
					}
					sql, args := historicalCoordinateQuery(t, name, params)
					sp, err := tx.Begin(env.ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = sp.Exec(env.ctx, sql, args...)
					if name == "ClaimRun" {
						// Migration 00355 extends workers. The archived explicit projection
						// no longer casts to that composite, before enrollment is checked.
						// Current SQL without opt-in still requires 23514 in
						// TestCheckpointCoordinatesClaimCompatibilityLiveDB/historical_API.
						requireCoordinateSQLState(t, err, "42846")
					} else {
						requireCoordinateSQLState(t, err, "23514")
					}
					if err := sp.Rollback(env.ctx); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(env.ctx); err != nil {
						t.Fatal(err)
					}
					if after := coordinateAuthoritySnapshot(t, env, run.ID); after != before {
						t.Fatal("historical refusal changed persisted run/binding/generation/descriptor/custody")
					}
				})
			}
		}
	}
}

// The composite mismatch affects ordinary intake too, not just enrolled tasks.
func TestArchivedClaimRunRejectsOrdinaryCandidateLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	id := f.seedRun("queued", 0)
	run := mustRun(t, env, id)
	if run.Kind != "issue" || len(run.CheckpointCoordinates) != 0 {
		t.Fatal("control must be an unenrolled ordinary issue")
	}
	before := coordinateAuthoritySnapshot(t, env, id)
	params := coordinateClaimParams(f)
	sql, args := historicalCoordinateQuery(t, "ClaimRun", params)
	_, err := env.pool.Exec(env.ctx, sql, args...)
	requireCoordinateSQLState(t, err, "42846")
	if after := coordinateAuthoritySnapshot(t, env, id); after != before {
		t.Fatal("archived ordinary claim changed persisted authority")
	}

	// Current SQL can claim the same candidate: ordinary eligibility is intact.
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	q := store.New(tx)
	if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
		t.Fatal(err)
	}
	claimed, err := q.ClaimRun(env.ctx, params)
	if err != nil || claimed.ID != id || claimed.ClaimGeneration != run.ClaimGeneration+1 {
		t.Fatalf("current ordinary claim=%s generation=%d err=%v", claimed.ID, claimed.ClaimGeneration, err)
	}
	if err := tx.Rollback(env.ctx); err != nil {
		t.Fatal(err)
	}
	if after := coordinateAuthoritySnapshot(t, env, id); after != before {
		t.Fatal("ordinary control rollback changed persisted authority")
	}
}
