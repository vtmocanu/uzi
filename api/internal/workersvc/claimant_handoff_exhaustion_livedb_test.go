package workersvc

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh).
//
// Issue #3645 (claimant handoff). A claimant that takes the agent's RetainedRecoveryStop
// pre-seed sends no terminal report and leaves the active-run registry, so the worker's
// heartbeat snapshot omits the run. These tests prove, through the real writers only, that
// such a run ends owner-held at requeue exhaustion: recovery_wait / worker_requeue_exhausted
// with every custody hold still open. The only custody fact involved is the open hold that
// ClaimRun itself opens in its `hold` CTE; nothing here inserts a capture, FINAL, checkpoint,
// uncertainty fact or custody release.
//
// Real writers driven: Service.Claim (-> ClaimRun), Service.SetState (the agent's first
// `running` report, sent at the start of RunRunner.phaseClone before any stop), and
// Service.Heartbeat with a snapshot that omits the run (FrozenFail/FrozenRequeueRunsMissingFromSnapshot).
// The one raw statement per cycle shifts status_since into the past so the run is beyond
// the missing-run fence; it changes no custody or recovery fact.

const exhaustionNonce = "claimant-handoff-nonce"

type claimantExhaustion struct {
	*custodyContinuationFixture
	guarded bool
	epoch   int64
}

// newClaimantExhaustion builds a fresh owner whose worker advertises a recovery-capable
// protocol set: recovery_inventory_v1 (+ v2) for a guarded claim, recovery_archive_v2 alone
// for a legacy claim, and applies RUN_MAX_REQUEUES = maxRequeues.
func newClaimantExhaustion(t *testing.T, env codexTestEnv, guarded bool, maxRequeues int) *claimantExhaustion {
	t.Helper()
	f := newCustodyContinuationFixture(t, env)
	f.svc.p.RunMaxRequeues = maxRequeues
	caps := []string{capability.RecoveryArchiveV2}
	if guarded {
		caps = append(caps, capability.RecoveryInventoryV1)
	}
	f.wkr.ProtocolCapabilities = caps
	env.exec(`UPDATE workers SET protocol_capabilities=$2, snapshot_register_nonce=$3, snapshot_epoch=0,
	          status='online', last_heartbeat_at=now() WHERE id=$1`, f.workerID, caps, exhaustionNonce)
	return &claimantExhaustion{custodyContinuationFixture: f, guarded: guarded}
}

// seedKind inserts a queued run of the given kind at claim_generation 0 with no custody row.
func (c *claimantExhaustion) seedKind(t *testing.T, kind string) uuid.UUID {
	t.Helper()
	if kind == "issue" {
		return c.seedRun("queued", 0)
	}
	// mr_rework hangs off a completed source issue run on the same branch and MR.
	c.nextIID++
	source := uuid.New()
	branch := "agent/issue-" + source.String()[:8]
	c.env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,branch,mr_iid,claim_generation,finished_at)
	            VALUES ($1,$2,$3,'issue',$4,'t','d','completed',$5,77,1,now())`, source, c.userID, c.repoID, c.nextIID, branch)
	run := uuid.New()
	c.env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,pipeline_ref,mr_iid,branch,target_run_id,issue_title,issue_description,status,claim_generation)
	            VALUES ($1,$2,$3,'mr_rework',$4,77,$4,$5,'t','d','queued',0)`, run, c.userID, c.repoID, branch, source)
	return run
}

type holdRow struct {
	generation int64
	state      string
	guarded    bool
	original   uuid.UUID
}

func (c *claimantExhaustion) holds(t *testing.T, run uuid.UUID) []holdRow {
	t.Helper()
	rows, err := c.env.pool.Query(c.env.ctx, `SELECT generation,state,inventory_guarded,original_worker_id
	  FROM recovery_custody_holds WHERE run_id=$1 ORDER BY generation`, run)
	if err != nil {
		t.Fatalf("read holds: %v", err)
	}
	defer rows.Close()
	var out []holdRow
	for rows.Next() {
		var h holdRow
		if err := rows.Scan(&h.generation, &h.state, &h.guarded, &h.original); err != nil {
			t.Fatalf("scan hold: %v", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("holds rows: %v", err)
	}
	return out
}

// claimAndRun performs a real Service.Claim as wkr, requires it to return run at wantGen, proves
// the precondition (ClaimRun itself opened an open hold at that generation, with inventory_guarded
// matching the claim kind), then sends the claimant's first `running` report and moves
// status_since past the missing-run fence.
func (c *claimantExhaustion) claimAndRun(t *testing.T, wkr store.Worker, run uuid.UUID, wantGen int64, guarded bool) {
	t.Helper()
	if wantGen == 1 {
		if got := c.holds(t, run); len(got) != 0 {
			t.Fatalf("fixture pre-seeded custody holds %+v; only ClaimRun may open them", got)
		}
	}
	p, err := c.svc.Claim(c.env.ctx, wkr, nil)
	if err != nil {
		t.Fatalf("Claim gen %d: %v", wantGen, err)
	}
	if p == nil || p.RunID != run.String() || p.ClaimGeneration != wantGen {
		t.Fatalf("Claim gen %d = %+v, want run %s generation %d", wantGen, p, run, wantGen)
	}
	hs := c.holds(t, run)
	if int64(len(hs)) != wantGen {
		t.Fatalf("holds after claim gen %d = %+v, want %d", wantGen, hs, wantGen)
	}
	last := hs[len(hs)-1]
	if last.generation != wantGen || last.state != "open" || last.original != wkr.ID || last.guarded != guarded {
		t.Fatalf("hold ClaimRun opened = %+v, want open generation %d worker %s inventory_guarded=%v", last, wantGen, wkr.ID, guarded)
	}
	gen := wantGen
	if _, applied, err := c.svc.SetState(c.env.ctx, wkr, run, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("running report gen %d: applied=%v err=%v", wantGen, applied, err)
	}
	c.env.exec(`UPDATE runs SET status_since=now()-interval '3 hours' WHERE id=$1`, run)
}

// omitHeartbeat is the real Heartbeat carrying a valid, empty active-run snapshot.
func (c *claimantExhaustion) omitHeartbeat(t *testing.T, workerID uuid.UUID) {
	t.Helper()
	c.epoch++
	_, err := c.svc.Heartbeat(c.env.ctx, hbWorker(t, c.env, workerID), nil, nil,
		&ActiveSnapshot{SnapshotEpoch: c.epoch, RegisterNonce: exhaustionNonce, Active: []ActiveRunEntry{}})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

type runFacts struct {
	status, cause string
	released      bool
	requeues      int64
	evidence      []byte // runs.worker_recovery_evidence as JSON text; nil when NULL
}

func (c *claimantExhaustion) facts(t *testing.T, run uuid.UUID) runFacts {
	t.Helper()
	var f runFacts
	if err := c.env.pool.QueryRow(c.env.ctx, `SELECT status,COALESCE(recovery_wait_cause,''),claim_released_at IS NOT NULL,requeue_count,
	  worker_recovery_evidence::text
	  FROM runs WHERE id=$1`, run).Scan(&f.status, &f.cause, &f.released, &f.requeues, &f.evidence); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return f
}

// requireCustodyUncertainEvidence proves the exhaustion classifier read the evidence successfully
// and saw only the open custody hold: a failed evidence read parks with Unknown instead.
func requireCustodyUncertainEvidence(t *testing.T, f runFacts) {
	t.Helper()
	if f.evidence == nil {
		t.Fatalf("worker_recovery_evidence is NULL at exhaustion: %+v", f)
	}
	var ev store.WorkerRecoveryEvidence
	if err := json.Unmarshal(f.evidence, &ev); err != nil {
		t.Fatalf("decode worker_recovery_evidence %s: %v", f.evidence, err)
	}
	if !ev.CustodyUncertain || ev.Unknown || ev.AvailableCapture || ev.CaptureUncertain || ev.PublicationUncertain || ev.CheckpointTip != nil {
		t.Fatalf("worker_recovery_evidence = %s, want custody_uncertain=true and every other signal false/null", f.evidence)
	}
}

func (c *claimantExhaustion) requireAllHoldsOpen(t *testing.T, run uuid.UUID, want int) {
	t.Helper()
	hs := c.holds(t, run)
	if len(hs) != want {
		t.Fatalf("holds = %+v, want %d (one per generation claimed)", hs, want)
	}
	for _, h := range hs {
		if h.state != "open" {
			t.Fatalf("hold %+v is not open; custody must survive the claimant stop and every requeue", h)
		}
	}
}

// Matrix: claim kind x run kind x RUN_MAX_REQUEUES. The legacy rows (recovery_archive_v2 only)
// are API-only: they exercise an older image's claim against this API and are not evidence about
// the candidate worker.
func claimantHandoffMatrix(t *testing.T, env codexTestEnv) {
	for _, claimKind := range []struct {
		name    string
		guarded bool
	}{{"guarded", true}, {"legacy_api_only", false}} {
		for _, kind := range []string{"issue", "mr_rework"} {
			for _, maxRequeues := range []int{0, 3} {
				name := claimKind.name + "/" + kind + fmt.Sprintf("/max%d", maxRequeues)
				t.Run(name, func(t *testing.T) {
					c := newClaimantExhaustion(t, env, claimKind.guarded, maxRequeues)
					run := c.seedKind(t, kind)
					for gen := int64(1); ; gen++ {
						c.claimAndRun(t, c.wkr, run, gen, claimKind.guarded)
						// The claimant stopped on the retained source: no report, only an omitted heartbeat.
						c.omitHeartbeat(t, c.workerID)
						f := c.facts(t, run)
						if gen <= int64(maxRequeues) {
							if f.status != "queued" || f.requeues != gen || f.released {
								t.Fatalf("after omission %d: %+v, want queued with requeue_count %d", gen, f, gen)
							}
							c.requireAllHoldsOpen(t, run, int(gen))
							continue
						}
						if f.status != "recovery_wait" || f.cause != "worker_requeue_exhausted" || !f.released {
							t.Fatalf("at the cap: %+v, want recovery_wait/worker_requeue_exhausted with the claim released", f)
						}
						requireCustodyUncertainEvidence(t, f)
						c.requireAllHoldsOpen(t, run, int(gen))
						return
					}
				})
			}
		}
	}
}

// (a) An owner Cancel stamped between requeues ends cancelled, and the cancel does not release
// custody: every generation's hold stays open.
func claimantHandoffOwnerCancel(t *testing.T, env codexTestEnv) {
	// maxRequeues 3: the cancel lands mid-budget. maxRequeues 1: generation 2's omission is the
	// exhausting cycle, so the stamped cancel must win over worker_requeue_exhausted.
	for _, guarded := range []bool{true, false} {
		for _, maxRequeues := range []int{3, 1} {
			name := "guarded"
			if !guarded {
				name = "legacy_api_only"
			}
			name += fmt.Sprintf("/max%d", maxRequeues)
			t.Run(name, func(t *testing.T) {
				c := newClaimantExhaustion(t, env, guarded, maxRequeues)
				run := c.seedKind(t, "issue")
				c.claimAndRun(t, c.wkr, run, 1, guarded)
				c.omitHeartbeat(t, c.workerID)
				if f := c.facts(t, run); f.status != "queued" || f.requeues != 1 {
					t.Fatalf("first omission: %+v, want queued", f)
				}
				c.claimAndRun(t, c.wkr, run, 2, guarded)
				// The owner's Cancel verdict stamps runs.stop_kind while the run is still running.
				if _, err := c.env.q.CreateStopVerdictInput(c.env.ctx, store.CreateStopVerdictInputParams{
					RunID: run, Kind: "cancel", StopKind: pgtype.Text{String: "cancelled", Valid: true},
				}); err != nil {
					t.Fatalf("CreateStopVerdictInput(cancel): %v", err)
				}
				c.omitHeartbeat(t, c.workerID)
				f := c.facts(t, run)
				if f.status != "cancelled" {
					t.Fatalf("after owner cancel and omission: %+v, want cancelled", f)
				}
				c.requireAllHoldsOpen(t, run, 2)
			})
		}
	}
}

// (b) An ownership change: a different worker re-claims at a new generation. The first worker's
// hold stays open, the new worker's hold is opened, and exhaustion on the second worker is
// owner-held with both holds open.
func claimantHandoffOwnershipChange(t *testing.T, env codexTestEnv) {
	for _, guarded := range []bool{true, false} {
		name := "guarded"
		if !guarded {
			name = "legacy_api_only"
		}
		t.Run(name, func(t *testing.T) {
			c := newClaimantExhaustion(t, env, guarded, 1)
			run := c.seedKind(t, "issue")
			first := c.workerID
			c.claimAndRun(t, c.wkr, run, 1, guarded)
			c.omitHeartbeat(t, first)
			if f := c.facts(t, run); f.status != "queued" || f.requeues != 1 {
				t.Fatalf("first worker omission: %+v, want queued", f)
			}

			second := uuid.New()
			c.env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities,snapshot_register_nonce,snapshot_epoch)
			            VALUES ($1,$2,'second',$3,'online',now(),$4,$5,0)`, second, c.userID, second[:], c.wkr.ProtocolCapabilities, exhaustionNonce)
			// The first worker stops heartbeating, so the queued run is free for the second worker.
			c.env.exec(`UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1`, first)
			wkr2 := hbWorker(t, c.env, second)
			c.claimAndRun(t, wkr2, run, 2, guarded)
			hs := c.holds(t, run)
			if len(hs) != 2 || hs[0].original != first || hs[0].state != "open" || hs[1].original != second || hs[1].state != "open" {
				t.Fatalf("holds after ownership change = %+v, want gen1 open on %s and gen2 open on %s", hs, first, second)
			}
			c.omitHeartbeat(t, second)
			f := c.facts(t, run)
			if f.status != "recovery_wait" || f.cause != "worker_requeue_exhausted" || !f.released {
				t.Fatalf("exhaustion on the new owner: %+v, want recovery_wait/worker_requeue_exhausted", f)
			}
			requireCustodyUncertainEvidence(t, f)
			c.requireAllHoldsOpen(t, run, 2)
		})
	}
}

// TestClaimantHandoffExhaustionLiveDB is the single entry point: the matrix plus the two extra rows.
func TestClaimantHandoffExhaustionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	t.Run("matrix", func(t *testing.T) { claimantHandoffMatrix(t, env) })
	t.Run("owner_cancel_between_requeues", func(t *testing.T) { claimantHandoffOwnerCancel(t, env) })
	t.Run("ownership_change", func(t *testing.T) { claimantHandoffOwnershipChange(t, env) })
}
