package workersvc

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1423 live-DB coverage for the NON-TERMINAL conjunct of the advice fence. Completion
// (SetRunCompleted) sets status = 'completed' but leaves worker_id, claim_generation and
// claim_released_at intact, so without `status NOT IN ('completed','failed','cancelled')` in
// the `live` CTE a delayed judge/task-review POST authorized while the advice run was active
// would still match after the run finished and overwrite a newer run's review. These tests
// call the fenced store statements directly (store.Queries), bypassing the service's
// authorize read, so they pin the in-statement predicate itself.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

var adviceTerminalStatuses = []string{"completed", "failed", "cancelled"}

// terminalLane extends adviceLane with a generation-aware direct upsert and a summary probe.
type terminalLane struct {
	adviceLane
	// upsertGen calls the fenced store statement directly; gen nil = unstamped (legacy).
	// label is written to the review summary and to every child row's text field.
	upsertGen func(env codexTestEnv, target, userID, workerID, adviceRunID uuid.UUID, gen *int64, label string) error
	// summary reads the persisted review header's summary_md for target.
	summary func(t *testing.T, env codexTestEnv, target uuid.UUID) string
	// childrenLabelled counts child rows for target carrying label.
	childrenLabelled func(t *testing.T, env codexTestEnv, target uuid.UUID, label string) int
}

func terminalLanes() []terminalLane {
	var out []terminalLane
	for _, l := range adviceLanes() {
		tl := terminalLane{adviceLane: l}
		switch l.insertMarker {
		case "INSERT INTO run_reviews":
			tl.upsertGen = func(env codexTestEnv, target, userID, workerID, adviceRunID uuid.UUID, gen *int64, label string) error {
				recs, _ := json.Marshal([]ReviewRecommendation{{Category: "improve_uzi", Target: label, RationaleMd: "r", Confidence: "high"}})
				_, err := env.q.UpsertRunReviewWithRecommendations(env.ctx, store.UpsertRunReviewWithRecommendationsParams{
					TargetRunID: target, JudgeRunID: pgconv.UUID(adviceRunID), UserID: userID, WorkerID: workerID,
					Verdict: "issues", SummaryMd: label, JudgeModel: "haiku", Status: "complete",
					ProducedByRunID: pgconv.UUID(adviceRunID), ProducedByUserID: pgconv.UUID(userID),
					Recommendations: recs, ClaimGeneration: pgconv.Int8Ptr(gen),
				})
				return err
			}
			tl.summary = func(t *testing.T, env codexTestEnv, target uuid.UUID) string {
				return env.adviceText(t, `SELECT summary_md FROM run_reviews WHERE target_run_id = $1`, target)
			}
			tl.childrenLabelled = func(t *testing.T, env codexTestEnv, target uuid.UUID, label string) int {
				return env.adviceCount(t, `SELECT count(*) FROM review_recommendations rr JOIN run_reviews r ON r.id = rr.review_id
				                           WHERE r.target_run_id = $1 AND rr.target = $2`, target, label)
			}
		case "INSERT INTO task_reviews":
			tl.upsertGen = func(env codexTestEnv, target, userID, workerID, adviceRunID uuid.UUID, gen *int64, label string) error {
				findings, _ := json.Marshal([]TaskReviewFinding{{File: label + ".go", Line: 1, Severity: "error", SummaryMd: "x", RationaleMd: "y"}})
				_, err := env.q.UpsertTaskReviewWithFindings(env.ctx, store.UpsertTaskReviewWithFindingsParams{
					TargetRunID: target, ReviewRunID: pgconv.UUID(adviceRunID), UserID: userID, WorkerID: workerID,
					Status: "complete", SummaryMd: label, Findings: findings, ClaimGeneration: pgconv.Int8Ptr(gen),
				})
				return err
			}
			tl.summary = func(t *testing.T, env codexTestEnv, target uuid.UUID) string {
				return env.adviceText(t, `SELECT summary_md FROM task_reviews WHERE target_run_id = $1`, target)
			}
			tl.childrenLabelled = func(t *testing.T, env codexTestEnv, target uuid.UUID, label string) int {
				return env.adviceCount(t, `SELECT count(*) FROM task_review_findings f JOIN task_reviews r ON r.id = f.review_id
				                           WHERE r.target_run_id = $1 AND f.file = $2`, target, label+".go")
			}
		default:
			panic("terminalLanes: unknown advice lane " + l.name)
		}
		out = append(out, tl)
	}
	return out
}

// TestAdviceTerminalStatusFenceLiveDB: an advice run that reached a terminal status while
// keeping the same worker_id, the same claim_generation and claim_released_at NULL refuses a
// late post (stamped with that generation, and unstamped) with pgx.ErrNoRows, leaving the
// previously written review and its child rows unchanged. The positive control is the same
// run, same worker, same generation, still non-terminal: the post lands.
func TestAdviceTerminalStatusFenceLiveDB(t *testing.T) {
	for _, lane := range terminalLanes() {
		for _, terminal := range adviceTerminalStatuses {
			t.Run(lane.name+"/"+terminal, func(t *testing.T) {
				env := setupCodexLiveDB(t)
				userID, workerID, repoID := env.seedCodexInfra(t)
				const gen int64 = 3

				target := lane.seedTarget(env, userID, repoID)
				advice := lane.seedAdvice(env, userID, repoID, target, workerID, "running", gen)

				// Positive control: non-terminal, same worker, same generation lands (stamped and unstamped).
				if err := lane.upsertGen(env, target, userID, workerID, advice, i64(gen), "live-stamped"); err != nil {
					t.Fatalf("non-terminal stamped upsert: %v", err)
				}
				if err := lane.upsertGen(env, target, userID, workerID, advice, nil, "live"); err != nil {
					t.Fatalf("non-terminal unstamped upsert: %v", err)
				}
				if h, c := lane.rows(t, env, target); h != 1 || c != 1 || lane.summary(t, env, target) != "live" {
					t.Fatalf("non-terminal post: headers=%d children=%d summary=%q, want 1 / 1 / live",
						h, c, lane.summary(t, env, target))
				}

				// The run finishes the way SetRunCompleted/fail/cancel leave it: only status moves.
				env.exec(`UPDATE runs SET status = $1 WHERE id = $2`, terminal, advice)
				var (
					gotWorker   uuid.UUID
					gotGen      int64
					gotReleased bool
				)
				if err := env.pool.QueryRow(env.ctx,
					`SELECT worker_id, claim_generation, claim_released_at IS NOT NULL FROM runs WHERE id = $1`, advice,
				).Scan(&gotWorker, &gotGen, &gotReleased); err != nil {
					t.Fatalf("read advice run: %v", err)
				}
				if gotWorker != workerID || gotGen != gen || gotReleased {
					t.Fatalf("precondition: worker=%v gen=%d released=%v, want %v / %d / false", gotWorker, gotGen, gotReleased, workerID, gen)
				}

				for _, tc := range []struct {
					label string
					gen   *int64
				}{{"late-stamped", i64(gen)}, {"late-unstamped", nil}} {
					if err := lane.upsertGen(env, target, userID, workerID, advice, tc.gen, tc.label); !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("%s upsert on a %s advice run err = %v, want pgx.ErrNoRows", tc.label, terminal, err)
					}
					if n := lane.childrenLabelled(t, env, target, tc.label); n != 0 {
						t.Fatalf("%s upsert on a %s advice run inserted %d child rows, want 0", tc.label, terminal, n)
					}
				}
				if h, c := lane.rows(t, env, target); h != 1 || c != 1 {
					t.Fatalf("late posts on a %s run changed row counts: headers=%d children=%d, want 1/1", terminal, h, c)
				}
				if s := lane.summary(t, env, target); s != "live" {
					t.Fatalf("late posts on a %s run changed the summary to %q, want live", terminal, s)
				}
				if n := lane.childrenLabelled(t, env, target, "live"); n != 1 {
					t.Fatalf("late posts on a %s run removed the live child row: %d, want 1", terminal, n)
				}
			})
		}
	}
}

// TestAdviceTerminalStatusForShareLiveDB: a terminal-status UPDATE is held open in tx A; the
// stamped upsert must block on the advice run's row lock (FOR SHARE) and, once A commits,
// re-check the new row version and write nothing (pgx.ErrNoRows).
func TestAdviceTerminalStatusForShareLiveDB(t *testing.T) {
	for _, lane := range terminalLanes() {
		t.Run(lane.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			const gen int64 = 3

			target := lane.seedTarget(env, userID, repoID)
			advice := lane.seedAdvice(env, userID, repoID, target, workerID, "running", gen)

			txA, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatalf("begin tx A: %v", err)
			}
			defer func() { _ = txA.Rollback(env.ctx) }()
			var pidA int32
			if err := txA.QueryRow(env.ctx, `SELECT pg_backend_pid()`).Scan(&pidA); err != nil {
				t.Fatalf("tx A pid: %v", err)
			}
			if _, err := txA.Exec(env.ctx, `UPDATE runs SET status = 'completed' WHERE id = $1`, advice); err != nil {
				t.Fatalf("tx A complete: %v", err)
			}

			done := make(chan error, 1)
			go func() {
				done <- lane.upsertGen(env, target, userID, workerID, advice, i64(gen), "raced")
			}()

			deadline := time.Now().Add(30 * time.Second)
			for {
				var blocked int
				if err := env.pool.QueryRow(env.ctx,
					`SELECT count(*) FROM pg_stat_activity
					  WHERE $1 = ANY(pg_blocking_pids(pid)) AND position($2 in query) > 0`,
					pidA, lane.insertMarker).Scan(&blocked); err != nil {
					t.Fatalf("poll pg_stat_activity: %v", err)
				}
				if blocked > 0 {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("the upsert finished without blocking on tx A's uncommitted completion (err = %v)", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the upsert never blocked on tx A within 30s")
				}
				time.Sleep(20 * time.Millisecond)
			}

			if err := txA.Commit(env.ctx); err != nil {
				t.Fatalf("commit tx A: %v", err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("upsert after the committed completion err = %v, want pgx.ErrNoRows", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the upsert did not finish within 30s of tx A committing")
			}
			if h, c := lane.rows(t, env, target); h != 0 || c != 0 {
				t.Fatalf("upsert across the completion persisted headers=%d children=%d, want 0/0", h, c)
			}
		})
	}
}
