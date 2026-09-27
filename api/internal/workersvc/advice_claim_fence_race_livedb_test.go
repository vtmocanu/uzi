package workersvc

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1423 live-DB coverage for the three fence properties the basic suite
// (advice_claim_fence_livedb_test.go) does not reach, run against BOTH advice lanes:
//   - advice_run_id: generations are per-run counters, so a stale flight of a terminal judge/
//     review run A at gen G collides with a re-run B (same target, same worker) claimed at G.
//     A post stamping A's id is refused with nothing written; B's id lands.
//   - worker ownership in the statement: a run reassigned to another worker while its claim
//     is unreleased refuses a legacy unstamped post: the service refuses it at authorization
//     (ErrRunNotFound), and the upsert itself refuses it too, which is the half that pins the
//     in-statement worker predicate (the release+reclaim-between-authorize-and-write window).
//   - FOR SHARE: a concurrent, not-yet-committed claim_generation bump blocks the fenced
//     upsert; once it commits, the post re-checks the new row version and is ErrStaleClaim.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// adviceLane abstracts the judge review and task-review advice posts over one shape.
type adviceLane struct {
	name string
	// seedTarget inserts the reviewed run and returns its id.
	seedTarget func(env codexTestEnv, userID, repoID uuid.UUID) uuid.UUID
	// seedAdvice inserts an advice run for target in status, owned by workerID at gen.
	seedAdvice func(env codexTestEnv, userID, repoID, target, workerID uuid.UUID, status string, gen int64) uuid.UUID
	// post drives the service's advice POST.
	post func(env codexTestEnv, svc *Service, wkr store.Worker, target uuid.UUID, claim AdviceClaim) error
	// upsert calls the fenced store statement directly (legacy: no generation).
	upsert func(env codexTestEnv, target, userID, workerID, adviceRunID uuid.UUID) error
	// rows counts the persisted review header + child rows for target.
	rows func(t *testing.T, env codexTestEnv, target uuid.UUID) (headers, children int)
	// insertMarker is a substring of the fenced statement's text, to find it in pg_stat_activity.
	insertMarker string
}

func adviceLanes() []adviceLane {
	return []adviceLane{
		{
			name: "judge review",
			seedTarget: func(env codexTestEnv, userID, repoID uuid.UUID) uuid.UUID {
				id := uuid.New()
				env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
				          VALUES ($1, $2, $3, 'issue', 1423, 't', 'd', 'completed')`, id, userID, repoID)
				return id
			},
			seedAdvice: func(env codexTestEnv, userID, _, target, workerID uuid.UUID, status string, gen int64) uuid.UUID {
				id := uuid.New()
				env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id, worker_id, claim_generation)
				          VALUES ($1, $2, 'judge', 't', 'd', $3, $4, $5, $6)`, id, userID, status, target, workerID, gen)
				return id
			},
			post: func(env codexTestEnv, svc *Service, wkr store.Worker, target uuid.UUID, claim AdviceClaim) error {
				_, err := svc.PostReview(env.ctx, wkr, target, ReviewSubmission{
					Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete",
					Recommendations: []ReviewRecommendation{{Category: "improve_uzi", Target: "x", RationaleMd: "r", Confidence: "high"}},
				}, claim)
				return err
			},
			upsert: func(env codexTestEnv, target, userID, workerID, adviceRunID uuid.UUID) error {
				recs, _ := json.Marshal([]ReviewRecommendation{{Category: "improve_uzi", Target: "x", RationaleMd: "r", Confidence: "high"}})
				_, err := env.q.UpsertRunReviewWithRecommendations(env.ctx, store.UpsertRunReviewWithRecommendationsParams{
					TargetRunID: target, JudgeRunID: pgconv.UUID(adviceRunID), UserID: userID, WorkerID: workerID,
					Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete",
					ProducedByRunID: pgconv.UUID(adviceRunID), ProducedByUserID: pgconv.UUID(userID),
					Recommendations: recs, ClaimGeneration: pgtype.Int8{},
				})
				return err
			},
			rows: func(t *testing.T, env codexTestEnv, target uuid.UUID) (int, int) {
				return env.adviceCount(t, `SELECT count(*) FROM run_reviews WHERE target_run_id = $1`, target),
					env.adviceCount(t, `SELECT count(*) FROM review_recommendations rr JOIN run_reviews r ON r.id = rr.review_id
					                     WHERE r.target_run_id = $1`, target)
			},
			insertMarker: "INSERT INTO run_reviews",
		},
		{
			name: "task review",
			seedTarget: func(env codexTestEnv, userID, repoID uuid.UUID) uuid.UUID {
				id := uuid.New()
				env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_requested, issue_title, issue_description, status, auto_approve)
				          VALUES ($1, $2, $3, 'task', $4, 'main', true, 't', 'd', 'completed', true)`, id, userID, repoID, "uzi/task/"+id.String())
				return id
			},
			seedAdvice: func(env codexTestEnv, userID, repoID, target, workerID uuid.UUID, status string, gen int64) uuid.UUID {
				id := uuid.New()
				env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at,
				                            auto_approve, open_mr, review_requested, issue_title, issue_description, status, worker_id, claim_generation)
				          VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), true, false, false, 'review', '', $6, $7, $8)`,
					id, userID, repoID, "uzi/task/"+target.String(), target, status, workerID, gen)
				return id
			},
			post: func(env codexTestEnv, svc *Service, wkr store.Worker, target uuid.UUID, claim AdviceClaim) error {
				return svc.PostTaskReview(env.ctx, wkr, target, TaskReviewSubmission{Status: "complete", SummaryMd: "s",
					Findings: []TaskReviewFinding{{File: "a.go", Line: 1, Severity: "error", SummaryMd: "x", RationaleMd: "y"}}}, claim)
			},
			upsert: func(env codexTestEnv, target, userID, workerID, adviceRunID uuid.UUID) error {
				findings, _ := json.Marshal([]TaskReviewFinding{{File: "a.go", Line: 1, Severity: "error", SummaryMd: "x", RationaleMd: "y"}})
				_, err := env.q.UpsertTaskReviewWithFindings(env.ctx, store.UpsertTaskReviewWithFindingsParams{
					TargetRunID: target, ReviewRunID: pgconv.UUID(adviceRunID), UserID: userID, WorkerID: workerID,
					Status: "complete", SummaryMd: "s", Findings: findings, ClaimGeneration: pgtype.Int8{},
				})
				return err
			},
			rows: func(t *testing.T, env codexTestEnv, target uuid.UUID) (int, int) {
				return env.adviceCount(t, `SELECT count(*) FROM task_reviews WHERE target_run_id = $1`, target),
					env.adviceCount(t, `SELECT count(*) FROM task_review_findings f JOIN task_reviews r ON r.id = f.review_id
					                     WHERE r.target_run_id = $1`, target)
			},
			insertMarker: "INSERT INTO task_reviews",
		},
	}
}

// TestAdviceRunIDFenceLiveDB: judge/review run A is terminal, re-run B (same target, same
// worker) is active at the SAME generation. A's stale flight stamps A's id and the matching
// generation: refused, nothing written. B's own post lands.
func TestAdviceRunIDFenceLiveDB(t *testing.T) {
	for _, lane := range adviceLanes() {
		t.Run(lane.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			svc := New(env.q, env.box, testParams())
			_, capable := adviceWorkers(workerID, userID)
			const gen int64 = 1

			target := lane.seedTarget(env, userID, repoID)
			runA := lane.seedAdvice(env, userID, repoID, target, workerID, "completed", gen)
			runB := lane.seedAdvice(env, userID, repoID, target, workerID, "running", gen)

			err := lane.post(env, svc, capable, target, AdviceClaim{Generation: i64(gen), RunID: &runA})
			if !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("stale run A post (gen %d collides with B) err = %v, want ErrStaleClaim", gen, err)
			}
			if h, c := lane.rows(t, env, target); h != 0 || c != 0 {
				t.Fatalf("stale run A post persisted headers=%d children=%d, want 0/0", h, c)
			}

			if err := lane.post(env, svc, capable, target, AdviceClaim{Generation: i64(gen), RunID: &runB}); err != nil {
				t.Fatalf("run B's own post: %v", err)
			}
			if h, c := lane.rows(t, env, target); h != 1 || c != 1 {
				t.Fatalf("run B post persisted headers=%d children=%d, want 1/1", h, c)
			}
		})
	}
}

// TestAdviceWorkerOwnershipFenceLiveDB: the advice run is reassigned to another worker while
// its claim is unreleased. The original worker's legacy (unstamped) post writes nothing,
// through the service AND at the fenced upsert itself, which is what closes a reassignment
// landing between the service's authorize read and the write.
func TestAdviceWorkerOwnershipFenceLiveDB(t *testing.T) {
	for _, lane := range adviceLanes() {
		t.Run(lane.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			svc := New(env.q, env.box, testParams())
			legacy, _ := adviceWorkers(workerID, userID)
			otherWorker := uuid.New()
			env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
				otherWorker, userID, "w-"+otherWorker.String(), otherWorker[:])

			target := lane.seedTarget(env, userID, repoID)
			advice := lane.seedAdvice(env, userID, repoID, target, workerID, "running", adviceLiveGen)
			// Sanity: before the reassignment the same legacy statement lands.
			if err := lane.upsert(env, target, userID, workerID, advice); err != nil {
				t.Fatalf("owned legacy upsert: %v", err)
			}
			env.exec(`DELETE FROM run_reviews WHERE target_run_id = $1`, target)
			env.exec(`DELETE FROM task_reviews WHERE target_run_id = $1`, target)

			env.exec(`UPDATE runs SET worker_id = $1 WHERE id = $2`, otherWorker, advice)

			if err := lane.post(env, svc, legacy, target, AdviceClaim{}); !errors.Is(err, ErrRunNotFound) {
				t.Fatalf("legacy post after reassignment err = %v, want ErrRunNotFound (refused at authorization)", err)
			}
			if err := lane.upsert(env, target, userID, workerID, advice); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("fenced upsert by the previous worker err = %v, want pgx.ErrNoRows (nothing written)", err)
			}
			if h, c := lane.rows(t, env, target); h != 0 || c != 0 {
				t.Fatalf("reassigned-run posts persisted headers=%d children=%d, want 0/0", h, c)
			}
		})
	}
}

// TestAdviceFenceForShareLiveDB pins the FOR SHARE in the `live` CTE: a reclaim's
// `claim_generation + 1` UPDATE is held open in tx A; the post (stamped with the generation
// that is still committed) must BLOCK on the run row lock rather than pass on its snapshot,
// and once A commits it must re-check the new row version and refuse with ErrStaleClaim.
// Without FOR SHARE the post never blocks and lands, so this test goes red.
func TestAdviceFenceForShareLiveDB(t *testing.T) {
	for _, lane := range adviceLanes() {
		t.Run(lane.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			svc := New(env.q, env.box, testParams())
			_, capable := adviceWorkers(workerID, userID)

			target := lane.seedTarget(env, userID, repoID)
			advice := lane.seedAdvice(env, userID, repoID, target, workerID, "running", adviceStaleGen)

			txA, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatalf("begin tx A: %v", err)
			}
			defer func() { _ = txA.Rollback(env.ctx) }()
			var pidA int32
			if err := txA.QueryRow(env.ctx, `SELECT pg_backend_pid()`).Scan(&pidA); err != nil {
				t.Fatalf("tx A pid: %v", err)
			}
			if _, err := txA.Exec(env.ctx, `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`, advice); err != nil {
				t.Fatalf("tx A bump: %v", err)
			}

			done := make(chan error, 1)
			go func() {
				done <- lane.post(env, svc, capable, target, AdviceClaim{Generation: i64(adviceStaleGen), RunID: &advice})
			}()

			// Wait (condition-based, bounded) until the post's backend is blocked by tx A.
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
					t.Fatalf("the post finished without blocking on tx A's uncommitted bump (err = %v): "+
						"the live CTE did not lock the advice run row (FOR SHARE missing?)", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the post never blocked on tx A within 30s")
				}
				time.Sleep(20 * time.Millisecond)
			}

			if err := txA.Commit(env.ctx); err != nil {
				t.Fatalf("commit tx A: %v", err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("post after the committed bump err = %v, want ErrStaleClaim", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the post did not finish within 30s of tx A committing")
			}
			if h, c := lane.rows(t, env, target); h != 0 || c != 0 {
				t.Fatalf("post across the bump persisted headers=%d children=%d, want 0/0", h, c)
			}
		})
	}
}
