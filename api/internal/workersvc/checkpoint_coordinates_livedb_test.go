package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type coordinateCase struct {
	Name          string           `json:"name"`
	Nodes         []map[string]any `json:"nodes"`
	WorkBranch    *string          `json:"work_branch"`
	CheckpointKey *string          `json:"checkpoint_key"`
}

func readCoordinateCases(t *testing.T) []coordinateCase {
	t.Helper()
	data, err := os.ReadFile("../../../fixtures/checkpoint-branch/coordinates-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []coordinateCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty coordinate fixture")
	}
	return cases
}

// Uses the literal shared oracle for SQL derivation and Go named-pair decoding.
// A complete synthetic runs row also exercises the NEW-row contract without
// requiring missing/cyclic references to pass persistence foreign-key checks.
func TestCheckpointCoordinatesContractLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	owner, _, repo := env.seedCodexInfra(t)
	other, _, otherRepo := env.seedCodexInfra(t)
	for _, c := range readCoordinateCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			for i := 1; i < len(c.Nodes); i++ {
				node := c.Nodes[i]
				userID, repoID := owner, repo
				if node["foreign_owner"] == true {
					userID = other
				}
				if node["foreign_repo"] == true {
					repoID = otherRepo
				}
				// Links are applied after every ancestor exists.
				if _, err := tx.Exec(env.ctx, `INSERT INTO runs
                    (id,user_id,repo_id,kind,branch,issue_title,issue_description,status)
                    VALUES ($1,$2,$3,$4,$5,'coordinates','fixture','completed')`,
					node["id"], userID, repoID, node["kind"], node["branch"]); err != nil {
					t.Fatal(err)
				}
			}
			// The subject itself is passed as a complete row, so links back to it
			// need a persisted placeholder to make a cycle observable.
			root := c.Nodes[0]
			persistedBranch := root["branch"]
			if root["kind"] == "task" && persistedBranch == nil {
				persistedBranch = "uzi/task/" + root["id"].(string)
			}
			persistedIID := root["issue_iid"]
			if (root["kind"] == "issue" || root["kind"] == "self_improve") && persistedIID == nil {
				persistedIID = 42
			}
			if _, err := tx.Exec(env.ctx, `INSERT INTO runs
                (id,user_id,repo_id,kind,branch,issue_iid,issue_title,issue_description,status)
                VALUES ($1,$2,$3,$4,$5,$6,'coordinates','fixture','completed')`,
				root["id"], owner, repo, root["kind"], persistedBranch, persistedIID); err != nil {
				t.Fatal(err)
			}
			for i := 1; i < len(c.Nodes); i++ {
				node := c.Nodes[i]
				if _, err := tx.Exec(env.ctx, `UPDATE runs SET review_target_run_id=$2, then_fix_of_run_id=$3 WHERE id=$1`,
					node["id"], node["review_target_run_id"], node["then_fix_of_run_id"]); err != nil {
					t.Fatal(err)
				}
			}
			input := map[string]any{}
			for k, v := range root {
				input[k] = v
			}
			input["user_id"], input["repo_id"] = owner, repo
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := tx.QueryRow(env.ctx, `SELECT fn_checkpoint_coordinates(jsonb_populate_record(NULL::runs,$1::jsonb))`, encoded).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if c.WorkBranch == nil {
				if len(raw) != 0 {
					t.Fatalf("want no coordinates; got %s", raw)
				}
				return
			}
			var pair CheckpointCoordinates
			if err := json.Unmarshal(raw, &pair); err != nil {
				t.Fatal(err)
			}
			if pair.WorkBranch != *c.WorkBranch || pair.CheckpointKey != *c.CheckpointKey {
				t.Fatalf("got pair (%q,%q)", pair.WorkBranch, pair.CheckpointKey)
			}
			// Execute the generated resolver query too, for a row with no missing links.
			if _, err := tx.Exec(env.ctx, `UPDATE runs SET review_target_run_id=$2,then_fix_of_run_id=$3 WHERE id=$1`,
				root["id"], root["review_target_run_id"], root["then_fix_of_run_id"]); err != nil {
				t.Fatal(err)
			}
			stored, err := store.New(tx).ResolveCheckpointCoordinates(env.ctx, uuid.MustParse(root["id"].(string)))
			if err != nil {
				t.Fatal(err)
			}
			var storedPair CheckpointCoordinates
			if err := json.Unmarshal(stored, &storedPair); err != nil {
				t.Fatal(err)
			}
			if storedPair.WorkBranch != *c.WorkBranch || storedPair.CheckpointKey != *c.CheckpointKey {
				t.Fatalf("stored pair disagrees: %s", stored)
			}
			model := store.Run{ID: pair.RunID, UserID: owner, RepoID: pgconv.UUID(repo), Kind: pair.Kind, CheckpointCoordinates: raw}
			if pair.IssueIID != nil {
				model.IssueIid = pgconv.Int8Ptr(pair.IssueIID)
			}
			decoded, err := frozenCheckpointCoordinates(model)
			if err != nil || decoded.WorkBranch != *c.WorkBranch || decoded.CheckpointKey != *c.CheckpointKey {
				t.Fatalf("Go pair=%+v err=%v", decoded, err)
			}
		})
	}
}

// Both lock orders must resolve without a child->parent wait. SQL NOWAIT
// contention consumes all three bounded attempts without leaving custody behind.
func TestCheckpointCoordinatesAncestorConcurrencyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mutation := range []string{"UPDATE runs SET branch='feature/drift' WHERE id=$1", "DELETE FROM runs WHERE id=$1"} {
		for _, first := range []string{"ancestor", "enrollment"} {
			t.Run(first+"/"+mutation, func(t *testing.T) {
				f := newCustodyContinuationFixture(t, env)
				env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
					[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
				root, child := uuid.New(), uuid.New()
				branch := "uzi/task/" + root.String()
				env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,issue_title,issue_description,status)
                    VALUES($1,$2,$3,'task',$4,'root','fixture','completed')`, root, f.userID, f.repoID, branch)
				env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,then_fix_of_run_id,dispatched_at,issue_title,issue_description,status)
                    VALUES($1,$2,$3,'task',$4,$5,now(),'child','fixture','queued')`, child, f.userID, f.repoID, branch, root)
				claimTx, err := env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = claimTx.Rollback(env.ctx) }()
				ancestorTx, err := env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = ancestorTx.Rollback(env.ctx) }()
				if _, err := store.New(claimTx).GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
					t.Fatal(err)
				}
				if first == "ancestor" {
					if _, err := ancestorTx.Exec(env.ctx, mutation, root); err != nil {
						t.Fatal(err)
					}
					_, err := f.svc.claimCheckpointRun(env.ctx, claimTx, coordinateClaimParams(f))
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("contended claim: %v", err)
					}
					var holds int
					if err := claimTx.QueryRow(env.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", child).Scan(&holds); err != nil {
						t.Fatal(err)
					}
					if holds != 0 {
						t.Fatal("contended admission leaked custody")
					}
					var status string
					var generation int64
					var coordinates []byte
					if err := claimTx.QueryRow(env.ctx, "SELECT status,claim_generation,checkpoint_coordinates FROM runs WHERE id=$1", child).Scan(&status, &generation, &coordinates); err != nil {
						t.Fatal(err)
					}
					if status != "queued" || generation != 0 || len(coordinates) != 0 {
						t.Fatal("contended admission leaked claim/enrollment")
					}
					if err := ancestorTx.Rollback(env.ctx); err != nil {
						t.Fatal(err)
					}
					run, err := f.svc.claimCheckpointRun(env.ctx, claimTx, coordinateClaimParams(f))
					if err != nil || run.ID != child || len(run.CheckpointCoordinates) == 0 {
						t.Fatalf("retry run=%s err=%v", run.ID, err)
					}
				} else {
					run, err := f.svc.claimCheckpointRun(env.ctx, claimTx, coordinateClaimParams(f))
					if err != nil || run.ID != child || len(run.CheckpointCoordinates) == 0 {
						t.Fatalf("claim run=%s err=%v", run.ID, err)
					}
					if _, err := ancestorTx.Exec(env.ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
						t.Fatal(err)
					}
					_, err = ancestorTx.Exec(env.ctx, mutation, root)
					requireCoordinateSQLState(t, err, "55P03")
					_ = ancestorTx.Rollback(env.ctx)
					if err := claimTx.Commit(env.ctx); err != nil {
						t.Fatal(err)
					}
					if mutation == "DELETE FROM runs WHERE id=$1" {
						// Cascading then-fix deletion reaches the child's live
						// custody FK and must refuse the whole parent delete.
						_, err := env.pool.Exec(env.ctx, mutation, root)
						requireCoordinateSQLState(t, err, "23503")
						if mustRun(t, env, child).ID != child || mustRun(t, env, root).ID != root {
							t.Fatal("refused cascade removed lineage")
						}
						if f.ownOpenHolds(t, child) != 1 {
							t.Fatal("refused cascade lost custody")
						}
						return
					}
					// Updating an ancestor cannot change the frozen provenance.
					env.exec(mutation, root)
					persisted := mustRun(t, env, child)
					pair, err := frozenCheckpointCoordinates(persisted)
					if err != nil || pair == nil || pair.WorkBranch != branch {
						t.Fatalf("frozen pair=%+v err=%v", pair, err)
					}
					raw, err := env.q.ResolveCheckpointCoordinates(env.ctx, child)
					if err != nil || string(raw) != string(run.CheckpointCoordinates) {
						t.Fatalf("frozen SQL=%s err=%v", raw, err)
					}
				}
			})
		}
	}
}

func TestCheckpointCoordinatesCapabilityRefreshConcurrencyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, first := range []string{"refresh", "admission"} {
		t.Run(first, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			seedCoordinateTask(f)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.TaskCheckpointCoordinatesV1})
			admission, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = admission.Rollback(env.ctx) }()
			refresh, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = refresh.Rollback(env.ctx) }()
			if first == "refresh" {
				if _, err := refresh.Exec(env.ctx, "UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", f.workerID); err != nil {
					t.Fatal(err)
				}
				if _, err := admission.Exec(env.ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
					t.Fatal(err)
				}
				_, err := store.New(admission).GetWorkerForUpdate(env.ctx, f.workerID)
				requireCoordinateSQLState(t, err, "55P03")
				_ = admission.Rollback(env.ctx)
				if err := refresh.Commit(env.ctx); err != nil {
					t.Fatal(err)
				}
				admission, err = env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = admission.Rollback(env.ctx) }()
				worker, err := store.New(admission).GetWorkerForUpdate(env.ctx, f.workerID)
				if err != nil {
					t.Fatal(err)
				}
				if len(worker.ProtocolCapabilities) != 0 {
					t.Fatal("worker reread missed committed refresh")
				}
				params := coordinateClaimParams(f)
				params.WorkerProtocolCaps = worker.ProtocolCapabilities
				params.RecoveryCapable = false
				run, err := f.svc.claimCheckpointRun(env.ctx, admission, params)
				if err != nil || len(run.CheckpointCoordinates) != 0 {
					t.Fatalf("legacy claim enrollment=%s err=%v", run.CheckpointCoordinates, err)
				}
			} else {
				if _, err := store.New(admission).GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
					t.Fatal(err)
				}
				run, err := f.svc.claimCheckpointRun(env.ctx, admission, coordinateClaimParams(f))
				if err != nil || len(run.CheckpointCoordinates) == 0 {
					t.Fatalf("admission enrollment=%s err=%v", run.CheckpointCoordinates, err)
				}
				if _, err := refresh.Exec(env.ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
					t.Fatal(err)
				}
				_, err = refresh.Exec(env.ctx, "UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", f.workerID)
				requireCoordinateSQLState(t, err, "55P03")
			}
		})
	}
}

func coordinateClaimParams(f *custodyContinuationFixture) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		WorkerID: pgconv.UUID(f.workerID), UserID: f.userID,
		WorkerProtocolCaps: []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1},
		RecoveryCapable:    true, WorkerIdentity: "coordinate-test",
		AffinityCutoff:        pgconv.Time(now.Add(-time.Hour)),
		SpreadCutoff:          pgconv.Time(now.Add(-time.Hour)),
		HeartbeatCutoff:       pgconv.Time(now.Add(-time.Minute)),
		BackgroundGraceCutoff: pgconv.Time(now.Add(-time.Hour)),
		CrossCheckEvaluatedAt: pgconv.Time(now),
	}
}

func requireCoordinateSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error=%v, want SQLSTATE %s", err, code)
	}
}

// Real ClaimRun + enrollment + custody savepoint, historical API fail-closed,
// terminal NEW, no ancestor re-read, and first-write/lost-ACK persistence.
func TestCheckpointCoordinatesClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
		[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
	runID := seedCoordinateTask(f)
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if _, err = store.New(tx).GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
		t.Fatal(err)
	}
	run, err := f.svc.claimCheckpointRun(env.ctx, tx, coordinateClaimParams(f))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := checkpointCoordinatesForWorker(store.Worker{ProtocolCapabilities: []string{capability.TaskCheckpointCoordinatesV1}}, run)
	if err != nil || pair == nil || pair.WorkBranch != "uzi/task/"+runID.String() || pair.CheckpointKey != pair.WorkBranch {
		t.Fatalf("pair=%+v err=%v", pair, err)
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Fatal(err)
	}
	persisted := mustRun(t, env, runID)
	if string(persisted.CheckpointCoordinates) != string(run.CheckpointCoordinates) {
		t.Fatal("descriptor not persisted before response")
	}
	env.exec("UPDATE runs SET status='queued' WHERE id=$1", runID)
	// New worker capability does not grant authority to an old API statement.
	_, err = env.pool.Exec(env.ctx, "UPDATE runs SET status='running' WHERE id=$1", runID)
	requireCoordinateSQLState(t, err, "23514")
	env.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", f.workerID)
	guarded, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.New(guarded).OptInCheckpointCoordinates(env.ctx); err != nil {
		t.Fatal(err)
	}
	_, err = guarded.Exec(env.ctx, "UPDATE runs SET status='running' WHERE id=$1", runID)
	requireCoordinateSQLState(t, err, "23514")
	_ = guarded.Rollback(env.ctx)
	for _, sql := range []string{
		"UPDATE runs SET checkpoint_coordinates=NULL WHERE id=$1",
		"UPDATE runs SET branch='feature/tamper' WHERE id=$1",
		"UPDATE runs SET user_id=gen_random_uuid() WHERE id=$1",
		"UPDATE runs SET repo_id=gen_random_uuid() WHERE id=$1",
		"UPDATE runs SET kind='prompt' WHERE id=$1",
		"UPDATE runs SET issue_iid=42 WHERE id=$1",
		"UPDATE runs SET review_target_run_id=gen_random_uuid() WHERE id=$1",
		"UPDATE runs SET then_fix_of_run_id=gen_random_uuid() WHERE id=$1",
		"UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1",
	} {
		_, err := env.pool.Exec(env.ctx, sql, runID)
		requireCoordinateSQLState(t, err, "23514")
	}
	env.exec("UPDATE runs SET status='failed',checkpoint_tip=$2 WHERE id=$1", runID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	var branch, ref, state string
	if err := env.pool.QueryRow(env.ctx, "SELECT branch,ref,state FROM checkpoint_retentions WHERE run_id=$1", runID).Scan(&branch, &ref, &state); err != nil {
		t.Fatal(err)
	}
	if branch != pair.CheckpointKey || ref != "refs/uzi-checkpoints/"+pair.CheckpointKey || state != "retained" {
		t.Fatalf("retention=%q %q %q", branch, ref, state)
	}
}

// Task fixtures must satisfy the real task shape and dispatch predicates.
func seedCoordinateTask(f *custodyContinuationFixture) uuid.UUID {
	id := uuid.New()
	f.env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,dispatched_at,issue_title,issue_description,status)
        VALUES($1,$2,$3,'task',$4,now(),'task','fixture','queued')`, id, f.userID, f.repoID, "uzi/task/"+id.String())
	return id
}

func TestCheckpointCoordinatesLegacyClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, kind := range []string{"issue", "self_improve"} {
		t.Run(kind, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := f.seedRun("queued", 0)
			env.exec("UPDATE runs SET kind=$2,branch='feature/legacy' WHERE id=$1", id, kind)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			q := store.New(tx)
			if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			run, err := f.svc.claimCheckpointRun(env.ctx, tx, coordinateClaimParams(f))
			if err != nil || run.ID != id || len(run.CheckpointCoordinates) != 0 {
				t.Fatalf("legacy claim=%+v err=%v", run, err)
			}
			// Even direct SQL enrollment must preserve the legacy kind contract.
			raw, err := q.EnrollCheckpointCoordinates(env.ctx, store.EnrollCheckpointCoordinatesParams{RunID: id, WorkerID: f.workerID, Generation: run.ClaimGeneration})
			if err != nil || len(raw) != 0 {
				t.Fatalf("legacy SQL enrollment=%s err=%v", raw, err)
			}
			derived, err := q.ResolveCheckpointCoordinates(env.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var pair CheckpointCoordinates
			if err := json.Unmarshal(derived, &pair); err != nil {
				t.Fatal(err)
			}
			want, ok := checkpointBranch(kind, id, run.IssueIid)
			if !ok || pair.WorkBranch != want || pair.CheckpointKey != want {
				t.Fatalf("legacy key=%+v want=%q", pair, want)
			}
			if err := tx.Rollback(env.ctx); err != nil {
				t.Fatal(err)
			}
			persisted := mustRun(t, env, id)
			if persisted.Status != "queued" || persisted.ClaimGeneration != 0 || len(persisted.CheckpointCoordinates) != 0 || f.ownOpenHolds(t, id) != 0 {
				t.Fatal("legacy rollback residue")
			}
		})
	}
}

func claimCoordinateTask(t *testing.T, f *custodyContinuationFixture) store.Run {
	t.Helper()
	id := seedCoordinateTask(f)
	env := f.env
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
		[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if _, err := store.New(tx).GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
		t.Fatal(err)
	}
	run, err := f.svc.claimCheckpointRun(env.ctx, tx, coordinateClaimParams(f))
	if err != nil || run.ID != id || len(run.CheckpointCoordinates) == 0 {
		t.Fatalf("claim=%s coordinates=%s err=%v", run.ID, run.CheckpointCoordinates, err)
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCheckpointCoordinatesReadoptionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, frozen := range []bool{false, true} {
		for _, mode := range []string{"compatible", "incompatible", "historical API"} {
			t.Run(fmt.Sprintf("frozen_%v/%s", frozen, mode), func(t *testing.T) {
				f := newCustodyContinuationFixture(t, env)
				run := claimCoordinateTask(t, f)
				env.exec("UPDATE runs SET status='queued' WHERE id=$1", run.ID)
				env.exec(`INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
                    VALUES($1,$2,$3,'running',false,1,now())`, f.workerID, run.ID, run.ClaimGeneration)
				if mode == "incompatible" {
					env.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", f.workerID)
				}
				tx, err := env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(env.ctx) }()
				q := store.New(tx)
				if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
					t.Fatal(err)
				}
				if mode != "historical API" {
					if err := q.OptInCheckpointCoordinates(env.ctx); err != nil {
						t.Fatal(err)
					}
				}
				count := 0
				if frozen {
					locks, lockErr := captureWorkerRecoveryLocks(env.ctx, q, f.workerID, []validatedEntry{{runID: run.ID}}, validatedFinalizeResume{})
					if lockErr != nil {
						t.Fatal(lockErr)
					}
					ledger, parents, parameterErr := locks.parameters()
					if parameterErr != nil {
						t.Fatal(parameterErr)
					}
					rows, readErr := q.FrozenReadoptRunsFromSnapshot(env.ctx, store.FrozenReadoptRunsFromSnapshotParams{WorkerID: f.workerID, FrozenTargets: ledger, LockedParentIds: parents})
					count, err = len(rows), readErr
				} else {
					rows, readErr := q.ReadoptRunsFromSnapshot(env.ctx, f.workerID)
					count, err = len(rows), readErr
				}
				if mode == "historical API" {
					requireCoordinateSQLState(t, err, "23514")
					_ = tx.Rollback(env.ctx)
				} else {
					want := 1
					if mode == "incompatible" {
						want = 0
					}
					if err != nil || count != want {
						t.Fatalf("readopt count=%d want=%d err=%v", count, want, err)
					}
					if err := tx.Commit(env.ctx); err != nil {
						t.Fatal(err)
					}
				}
				got := mustRun(t, env, run.ID)
				wantStatus := "queued"
				if mode == "compatible" {
					wantStatus = "running"
				}
				if got.Status != wantStatus || got.ClaimGeneration != run.ClaimGeneration || string(got.CheckpointCoordinates) != string(run.CheckpointCoordinates) || f.ownOpenHolds(t, run.ID) != 1 {
					t.Fatal("readoption changed descriptor, generation or custody")
				}
			})
		}
	}
}

func TestCheckpointCoordinatesClaimCompatibilityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []string{"compatible", "incompatible", "historical API", "old task"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			var prior store.Run
			if mode == "old task" {
				prior = mustRun(t, env, seedCoordinateTask(f))
			} else {
				prior = claimCoordinateTask(t, f)
				env.exec("UPDATE runs SET status='queued' WHERE id=$1", prior.ID)
			}
			params := coordinateClaimParams(f)
			if mode == "incompatible" || mode == "old task" {
				env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.RecoveryArchiveV1})
				params.WorkerProtocolCaps = []string{capability.RecoveryArchiveV1}
			}
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			q := store.New(tx)
			if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			var got store.Run
			if mode == "historical API" {
				got, err = q.ClaimRun(env.ctx, params)
			} else {
				got, err = f.svc.claimCheckpointRun(env.ctx, tx, params)
			}
			switch mode {
			case "historical API":
				requireCoordinateSQLState(t, err, "23514")
			case "incompatible":
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("incompatible claim: %v", err)
				}
			default:
				if err != nil || got.ID != prior.ID || got.ClaimGeneration != prior.ClaimGeneration+1 {
					t.Fatalf("claim=%s generation=%d err=%v", got.ID, got.ClaimGeneration, err)
				}
				if mode == "old task" && len(got.CheckpointCoordinates) != 0 {
					t.Fatal("old worker task converted")
				}
				if mode == "compatible" && string(got.CheckpointCoordinates) != string(prior.CheckpointCoordinates) {
					t.Fatal("reclaim changed descriptor")
				}
			}
			_ = tx.Rollback(env.ctx)
			persisted := mustRun(t, env, prior.ID)
			wantHolds := int64(1)
			if mode == "old task" {
				wantHolds = 0
			}
			if persisted.Status != "queued" || persisted.ClaimGeneration != prior.ClaimGeneration || string(persisted.CheckpointCoordinates) != string(prior.CheckpointCoordinates) || f.ownOpenHolds(t, prior.ID) != wantHolds {
				t.Fatal("claim refusal/rollback residue")
			}
		})
	}
}

func TestCheckpointCoordinatesTerminalNewLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []string{"new branch and tip", "unsupported lineage", "unknown write"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := seedCoordinateTask(f)
			env.exec("UPDATE runs SET branch='feature/before' WHERE id=$1", id)
			branch := "uzi/task/" + id.String()
			var tip any = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if mode == "unsupported lineage" {
				branch = "feature/unsupported"
			}
			if mode == "unknown write" {
				tip = nil
			}
			env.exec("UPDATE runs SET status='failed',branch=$2,checkpoint_tip=$3 WHERE id=$1", id, branch, tip)
			var keys []string
			rows, err := env.pool.Query(env.ctx, "SELECT branch FROM checkpoint_retentions WHERE run_id=$1", id)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var key string
				if err := rows.Scan(&key); err != nil {
					t.Fatal(err)
				}
				keys = append(keys, key)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if mode == "new branch and tip" {
				if len(keys) != 1 || keys[0] != branch {
					t.Fatalf("NEW retention keys=%v", keys)
				}
			} else if len(keys) != 0 {
				t.Fatalf("unsupported/unknown became durable: %v", keys)
			}
		})
	}
}

func TestCheckpointCoordinatesGuardedDownLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	run := claimCoordinateTask(t, f)
	sqlDB := stdlib.OpenDBFromPool(env.pool)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("../store/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.ApplyVersion(env.ctx, 352, false)
	if err == nil || !strings.Contains(err.Error(), "cannot downgrade with enrolled checkpoint coordinates") {
		t.Fatalf("guarded Down: %v", err)
	}
	persisted := mustRun(t, env, run.ID)
	if string(persisted.CheckpointCoordinates) != string(run.CheckpointCoordinates) {
		t.Fatal("failed Down changed enrolled coordinates")
	}
	// Exercise the resolver after rollback of the failed migration as well.
	if _, err := env.q.ResolveCheckpointCoordinates(env.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointCoordinatesServiceClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	id := seedCoordinateTask(f)
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
		[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
	// RequireWorker's stale snapshot is deliberately missing the new capability;
	// admission must re-read the locked worker and assembly must use that row.
	payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
	if err != nil || payload == nil || payload.CheckpointCoordinates == nil {
		t.Fatalf("claim payload=%+v err=%v", payload, err)
	}
	persisted := mustRun(t, env, id)
	if payload.RunID != id.String() || string(persisted.CheckpointCoordinates) == "" || f.ownOpenHolds(t, id) != 1 {
		t.Fatal("response preceded durable descriptor/custody")
	}
	pair, err := frozenCheckpointCoordinates(persisted)
	if err != nil || pair.WorkBranch != payload.CheckpointCoordinates.WorkBranch || pair.CheckpointKey != payload.CheckpointCoordinates.CheckpointKey {
		t.Fatalf("persisted pair=%+v err=%v", pair, err)
	}
}

func TestCheckpointCoordinatesPlacementLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	run := claimCoordinateTask(t, f)
	env.exec("UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", f.userID)
	env.exec("UPDATE runs SET status='queued',required_capabilities='{}' WHERE id=$1", run.ID)
	now := time.Now()
	params := store.ListUnplaceableQueuedRunsForEphemeralParams{
		AffinityCutoff:        pgconv.Time(now.Add(-time.Hour)),
		CrossCheckEvaluatedAt: pgconv.Time(now), CrossCheckAffinityCutoff: pgconv.Time(now.Add(-time.Hour)),
		DockerRepoAllowlist: []uuid.UUID{}, CodexCuratedModels: codexCuratedModelsSlice(),
		BackgroundGraceCutoff: pgconv.Time(now.Add(-time.Hour)), MaxPerUser: 10, MaxRows: 1000,
	}
	for _, capable := range []bool{false, true} {
		protocols := []string{capability.RecoveryArchiveV1}
		if capable {
			protocols = append(protocols, capability.TaskCheckpointCoordinatesV1)
		}
		env.exec("UPDATE workers SET protocol_capabilities=$2,status='online',last_heartbeat_at=now() WHERE id=$1", f.workerID, protocols)
		counts, err := f.svc.WorkerEligibilityForHealth(env.ctx, now, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if capable {
			if counts.StrictEligible != 1 || counts.DiagnosticSuitable != 1 {
				t.Fatalf("capable health=%+v", counts)
			}
		} else if counts.StrictEligible != 0 || counts.DiagnosticSuitable != 0 {
			t.Fatalf("incompatible health=%+v", counts)
		}
		rows, err := env.q.ListUnplaceableQueuedRunsForEphemeral(env.ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.ID == run.ID {
				found = true
			}
		}
		if found == capable {
			t.Fatalf("protocol-only demand found=%v capable=%v", found, capable)
		}
	}
}
