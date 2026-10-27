package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Admission fixtures are seeded tasks, workers and prior-refusal state, not
// public task creation or an actual provider refusal. Coordinates are enrolled
// by the production claimCheckpointRun transaction (with the database guard
// enabled); semantic enrollment uses the valid policy from semanticPolicySourceLive.
// The proof exercises production ClaimRun, policy diagnostics and runtime health
// against the same persisted run. Each attempted claim is rolled back.
func TestPolicyHoldAdmissionMirrorsLiveDB(t *testing.T) {
	for _, feature := range []struct {
		name string
		cap  string
	}{
		{"checkpoint_coordinates", capability.TaskCheckpointCoordinatesV1},
		{"semantic_policy_run", capability.CompletionSemanticPublicationV1},
	} {
		t.Run(feature.name, func(t *testing.T) {
			for _, capAware := range []bool{true, false} {
				mode := "scheduling_on"
				if !capAware {
					mode = "scheduling_off"
				}
				t.Run(mode, func(t *testing.T) {
					for _, tc := range []struct {
						name                      string
						policy, feature, combined bool
						wantCount                 int64
						claimA, claimB            bool
					}{
						{"split", true, true, false, 0, false, false},
						{"combined", true, true, true, 1, true, false},
						{"no_policy", false, true, false, 1, false, true},
						{"no_feature", true, false, false, 1, true, false},
						{"ordinary", false, false, false, 2, true, true},
					} {
						t.Run(tc.name, func(t *testing.T) {
							f := newCustodyContinuationFixture(t, setupCodexLiveDB(t))
							f.svc.SetHealthSettings(defaultHealthSettings())
							f.svc.SetCapabilitySettings(fakeCapabilitySettings{on: capAware})
							var runID uuid.UUID
							if feature.cap == capability.TaskCheckpointCoordinatesV1 && tc.feature {
								runID = claimCoordinateTask(t, f).ID
							} else {
								runID = seedCoordinateTask(f)
							}
							refusals := 0
							if tc.policy {
								refusals = 1
							}
							// Requeue without affinity, retaining any immutable enrollment.
							f.env.exec("UPDATE runs SET status='queued',worker_id=NULL,harness='codex',policy_refusal_count=$2 WHERE id=$1", runID, refusals)
							if feature.cap == capability.CompletionSemanticPublicationV1 && tc.feature {
								f.env.exec(`INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,
									'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, runID)
							}
							base := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1}
							aCaps := append(append([]string{}, base...), capability.ProviderPolicyHoldV1)
							if tc.combined {
								aCaps = append(aCaps, feature.cap)
							}
							f.env.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1", f.workerID, aCaps)
							a, err := f.env.q.GetWorkerByID(f.env.ctx, f.workerID)
							if err != nil {
								t.Fatal(err)
							}
							b := (&crFix{env: f.env, user: f.userID}).guardWorker(t, 1, append(base, feature.cap)...)
							run := mustRun(t, f.env, runID)
							var semantic bool
							if err := f.env.pool.QueryRow(f.env.ctx, "SELECT EXISTS(SELECT 1 FROM semantic_planning WHERE run_id=$1)", runID).Scan(&semantic); err != nil {
								t.Fatal(err)
							}
							wantCoordinates := feature.cap == capability.TaskCheckpointCoordinatesV1 && tc.feature
							wantSemantic := feature.cap == capability.CompletionSemanticPublicationV1 && tc.feature
							if (len(run.CheckpointCoordinates) > 0) != wantCoordinates || semantic != wantSemantic ||
								run.PolicyRefusalCount != int32(refusals) || len(run.RequiredCapabilities) != 0 {
								t.Fatal("fixture enrollment/refusal/required-capability preconditions differ")
							}
							t.Logf("persisted fixture coordinates=%v semantic=%v policy_refusals=%d required_capabilities=%v",
								wantCoordinates, semantic, run.PolicyRefusalCount, run.RequiredCapabilities)
							var claimable int64
							for _, worker := range []struct {
								name string
								w    store.Worker
								want bool
							}{{"workerA", a, tc.claimA}, {"workerB", b, tc.claimB}} {
								func() {
									tx, err := f.env.pool.Begin(f.env.ctx)
									if err != nil {
										t.Fatal(err)
									}
									defer func() { _ = tx.Rollback(f.env.ctx) }()
									q := store.New(tx)
									if err := q.OptInCheckpointCoordinates(f.env.ctx); err != nil {
										t.Fatal(err)
									}
									params := claimRunParamsFor(worker.w)
									params.CapabilityAware = capAware
									got, err := q.ClaimRun(f.env.ctx, params)
									if err != nil && !errors.Is(err, pgx.ErrNoRows) {
										t.Fatalf("%s ClaimRun: %v", worker.name, err)
									}
									claimed := err == nil
									if claimed {
										claimable++
									}
									if claimed != worker.want || (claimed && got.ID != runID) {
										t.Errorf("%s ClaimRun claimed=%v run=%s want claimed=%v run=%s", worker.name, claimed, got.ID, worker.want, runID)
									} else {
										t.Logf("PASS %s production ClaimRun claimed=%v (rolled back)", worker.name, claimed)
									}
								}()
							}
							if claimable != tc.wantCount {
								t.Fatalf("production ClaimRun claimable count=%d want=%d", claimable, tc.wantCount)
							}
							now := time.Now()
							n, err := f.env.q.CountOnlineWorkersSatisfyingPolicyHold(f.env.ctx, store.CountOnlineWorkersSatisfyingPolicyHoldParams{
								RunID: runID, HeartbeatCutoff: pgconv.Time(now.Add(-f.svc.p.WorkerHeartbeatStale)),
								AffinityCutoff:        pgconv.Time(now.Add(-f.svc.p.WorkerAffinityCeiling)),
								CrossCheckEvaluatedAt: pgconv.Time(now), CrossCheckAffinityCutoff: pgconv.Time(now.Add(-f.svc.p.WorkerAffinityGrace)),
								StaleRequeueCutoff:  StaleRequeueCutoff(now, f.svc.p.WorkerStaleRequeueGrace),
								DockerRepoAllowlist: []uuid.UUID{}, CodexCuratedModels: codexCuratedModelsSlice(),
								CapabilityAware: capAware, EphemeralLease: LeaseInterval(f.svc.ephemeralLease),
							})
							if err != nil {
								t.Fatal(err)
							}
							if n != claimable {
								t.Errorf("policy diagnostic count=%d want persisted-run ClaimRun claimable count=%d", n, claimable)
							} else {
								t.Logf("PASS policy diagnostic count=%d matches production ClaimRun", n)
							}
							eligible, err := f.svc.WorkerEligibilityForHealth(f.env.ctx, now, runID)
							if err != nil {
								t.Fatal(err)
							}
							if eligible.StrictEligible != claimable {
								t.Errorf("WorkerEligibilityForHealth StrictEligible=%d want ClaimRun claimable count=%d", eligible.StrictEligible, claimable)
							} else {
								t.Logf("PASS WorkerEligibilityForHealth StrictEligible=%d matches production ClaimRun", eligible.StrictEligible)
							}
						})
					}
				})
			}
		})
	}
}
