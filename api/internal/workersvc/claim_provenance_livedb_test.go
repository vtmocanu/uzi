package workersvc

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4476 M4: a Codex run that was claimed before may be claimed again only by the worker
// recorded as its last Codex claimant for the CURRENT generation. These tests drive the REAL
// claim path (Service.Claim -> ClaimRun) and the real health pass against a real Postgres, and
// prove a refused claim leaves the run, its capability and its custody exactly as they were.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; every name ends LiveDB.

var provenanceCaps = []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1}

const (
	provHeldNamedA       = "held for worker %s: wait for it or cancel the run; only the worker that last ran this Codex run may resume it"
	provHeldUnavailable  = "held: predecessor provenance unavailable; cancel the run; no worker may resume this Codex run"
	provenanceWorkerName = "prov-worker-"
)

// claimProvFix is a queued, never-claimed Codex subscription run plus three Codex-capable workers.
type claimProvFix struct {
	env              codexTestEnv
	svc              *Service
	userID, repoID   uuid.UUID
	runID            uuid.UUID
	workerA, workerB uuid.UUID
	workerC          uuid.UUID
	nameA, nameB     string
}

func newClaimProvFix(t *testing.T, env codexTestEnv) *claimProvFix {
	t.Helper()
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	seedDefaultAnthropicToken(t, env, f.userID)
	fx := &claimProvFix{env: env, userID: f.userID, runID: f.runID, workerA: f.workerID}
	fx.repoID = uuid.UUID(mustRun(t, env, f.runID).RepoID.Bytes)
	// Never claimed: queued at generation 0, no worker, no capability, no provenance.
	env.exec(`UPDATE runs SET status = 'queued', status_since = now(), worker_id = NULL, claim_generation = 0,
		codex_cap_hash = NULL WHERE id = $1`, f.runID)
	fx.nameA = provenanceWorkerName + "A-" + f.workerID.String()[:8]
	env.exec(`UPDATE workers SET name = $2, protocol_capabilities = $3, last_heartbeat_at = now(),
		snapshot_register_nonce = 'nonce-A', snapshot_epoch = 0 WHERE id = $1`, fx.workerA, fx.nameA, provenanceCaps)
	fx.workerB = fx.insertWorker(t, "B")
	fx.workerC = fx.insertWorker(t, "C")
	fx.nameB = provenanceWorkerName + "B-" + fx.workerB.String()[:8]
	env.exec(`UPDATE workers SET name = $2 WHERE id = $1`, fx.workerB, fx.nameB)
	fx.svc = New(env.q, env.box, testParams())
	fx.svc.SetVault(env.vlt)
	fx.svc.SetTxBeginner(env.pool)
	fx.svc.healthSettings = defaultHealthSettings()
	return fx
}

func (fx *claimProvFix) insertWorker(t *testing.T, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	fx.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities,
		last_heartbeat_at, snapshot_register_nonce, snapshot_epoch)
		VALUES ($1, $2, $3, $4, 'online', $5, now(), $6, 0)`,
		id, fx.userID, provenanceWorkerName+label+"-"+id.String()[:8], id[:], provenanceCaps, "nonce-"+label)
	return id
}

// claimer reads the worker row with the Codex capability set Claim sees.
func (fx *claimProvFix) claimer(t *testing.T, id uuid.UUID) store.Worker {
	t.Helper()
	fx.env.exec(`UPDATE workers SET last_heartbeat_at = now(), status = 'online' WHERE id = $1`, id)
	w := wkrRow(t, fx.env, id)
	w.ProtocolCapabilities = provenanceCaps
	return w
}

func (fx *claimProvFix) claim(t *testing.T, id uuid.UUID) *ClaimPayload {
	t.Helper()
	p, err := fx.svc.Claim(fx.env.ctx, fx.claimer(t, id), nil)
	if err != nil {
		t.Fatalf("Claim as %s: %v", id, err)
	}
	return p
}

// firstClaimByA claims the fresh run as worker A and proves provenance (A, 1) is recorded.
func (fx *claimProvFix) firstClaimByA(t *testing.T) {
	t.Helper()
	p := fx.claim(t, fx.workerA)
	if p == nil {
		t.Fatalf("A's first claim delivered no payload for run %s", fx.runID)
	}
	if p.Secrets.Codex == nil {
		t.Fatalf("A's first claim has no Codex block: run=%s generation=%d", p.RunID, p.ClaimGeneration)
	}
	r := mustRun(t, fx.env, fx.runID)
	if r.Status != "claimed" || r.ClaimGeneration != 1 || r.WorkerID != pgconv.UUID(fx.workerA) {
		t.Fatalf("after A's claim: status=%s gen=%d worker=%v", r.Status, r.ClaimGeneration, r.WorkerID)
	}
	fx.assertProvenance(t, fx.workerA, 1)
}

func (fx *claimProvFix) assertProvenance(t *testing.T, worker uuid.UUID, gen int64) {
	t.Helper()
	var w pgtype.UUID
	var g pgtype.Int8
	if err := fx.env.pool.QueryRow(fx.env.ctx,
		`SELECT claim_provenance_worker_id, claim_provenance_generation FROM runs WHERE id = $1`, fx.runID).Scan(&w, &g); err != nil {
		t.Fatal(err)
	}
	if w != pgconv.UUID(worker) || !g.Valid || g.Int64 != gen {
		t.Fatalf("provenance = (%v, %v), want (%s, %d)", w, g, worker, gen)
	}
}

// claimSnapshot is everything a refused claim must leave untouched.
type claimSnapshot struct {
	Generation int64
	CapHash    string
	Epoch      int64
	Holds      string
	CredEpochs string
	Msgs       string
}

func (fx *claimProvFix) snapshot(t *testing.T) claimSnapshot {
	t.Helper()
	r := mustRun(t, fx.env, fx.runID)
	var holds string
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(
		id, generation, original_worker_id, live_worker_id, live_run_id, state) ORDER BY id)::text, '[]')
		FROM recovery_custody_holds WHERE run_id = $1`, fx.runID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	var cred, msgs string
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(
		run_id, claim_generation, secret_id, label, select_reason, applied_at) ORDER BY claim_generation)::text, '[]')
		FROM run_credential_epochs WHERE run_id = $1`, fx.runID).Scan(&cred); err != nil {
		t.Fatal(err)
	}
	// Message payloads may contain secrets; compare only identity and routing metadata.
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(
		id, seq, kind, agent, agent_instance, claim_generation, created_at, credential_epoch_ordinal) ORDER BY seq)::text, '[]')
		FROM run_messages WHERE run_id = $1`, fx.runID).Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	return claimSnapshot{Generation: r.ClaimGeneration, CapHash: fmt.Sprintf("%x", r.CodexCapHash), Epoch: r.CodexClaimEpoch,
		Holds: holds, CredEpochs: cred, Msgs: msgs}
}

// assertRefused proves worker id gets no claim and that nothing the claim would have changed moved.
func (fx *claimProvFix) assertRefused(t *testing.T, id uuid.UUID) {
	t.Helper()
	before := fx.snapshot(t)
	p := fx.claim(t, id)
	if p != nil {
		t.Fatalf("worker %s was handed a claim for a held Codex run (codex secrets: %v)", id, p.Secrets.Codex != nil)
	}
	if after := fx.snapshot(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused claim changed the run:\nbefore %+v\nafter  %+v", before, after)
	}
	if st := mustRun(t, fx.env, fx.runID).Status; st != "queued" {
		t.Fatalf("status after refused claim = %s, want queued", st)
	}
}

// assertHealth runs the real health pass and returns the stored reason.
func (fx *claimProvFix) assertHealth(t *testing.T, want string) {
	t.Helper()
	fx.svc.detectRunHealth(fx.env.ctx, time.Now().Add(time.Hour))
	r := mustRun(t, fx.env, fx.runID)
	if r.Health != healthWaitingWorker || r.HealthReason.String != want {
		t.Fatalf("health = %q reason = %q, want %q / %q", r.Health, r.HealthReason.String, healthWaitingWorker, want)
	}
}

// ---- A. rejected-claim scenarios ----------------------------------------------------------

func TestCodexProvenanceRefusesOtherWorkerAfterRequeueLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		back func(t *testing.T, fx *claimProvFix) // brings the claimed run back to queued
	}{
		{"stale requeue", func(t *testing.T, fx *claimProvFix) {
			fx.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '2 hours', status = 'offline' WHERE id = $1`, fx.workerA)
			rows, err := fx.env.q.RequeueRunsOfStaleWorkers(fx.env.ctx, store.RequeueRunsOfStaleWorkersParams{
				MaxRequeues: 5, Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range rows {
				found = found || r.ID == fx.runID
			}
			if !found {
				t.Fatal("RequeueRunsOfStaleWorkers did not requeue the run")
			}
		}},
		{"resume nulls worker", func(t *testing.T, fx *claimProvFix) {
			fx.env.exec(`UPDATE runs SET status = 'paused', status_since = now() - interval '5 minutes',
				started_at = now() - interval '10 minutes', claim_released_at = now(),
				released_worker_id = worker_id WHERE id = $1`, fx.runID)
			if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, store.ResumePausedRunParams{
				ID: fx.runID, UserID: fx.userID, GlobalTimeoutSeconds: 7200}); err != nil {
				t.Fatalf("ResumePausedRun: %v", err)
			}
			if r := mustRun(t, fx.env, fx.runID); r.WorkerID.Valid {
				t.Fatalf("resume left worker_id = %v, want NULL", r.WorkerID)
			}
		}},
		{"account unavailable foreign holder", func(t *testing.T, fx *claimProvFix) {
			claimRecoveryHold(t, fx.env, fx.runID, fx.workerC, 2)
			r, err := fx.env.q.ParkRunCodexAccountUnavailable(fx.env.ctx, store.ParkRunCodexAccountUnavailableParams{
				ID: fx.runID, WorkerID: pgconv.UUID(fx.workerA), ClaimGeneration: 1})
			if err != nil {
				t.Fatal(err)
			}
			if r.WorkerID != pgconv.UUID(fx.workerC) {
				t.Fatalf("park rewrote worker_id to %v, want foreign holder C %s", r.WorkerID, fx.workerC)
			}
			// The account recovers: the run is queued again, worker_id still the foreign holder.
			fx.env.exec(`UPDATE runs SET status = 'queued', recovery_wait_cause = NULL, status_since = now() WHERE id = $1`, fx.runID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newClaimProvFix(t, env)
			fx.firstClaimByA(t)
			tc.back(t, fx)
			if st := mustRun(t, env, fx.runID).Status; st != "queued" {
				t.Fatalf("precondition: status = %s, want queued", st)
			}
			fx.assertProvenance(t, fx.workerA, 1) // the requeue path never touches provenance

			fx.assertRefused(t, fx.workerB)
			// Even the worker the run last sat on (C in the park case) is not the recorded one.
			fx.assertRefused(t, fx.workerC)
			fx.assertHealth(t, fmt.Sprintf(provHeldNamedA, fx.nameA))

			// Positive control: the recorded worker still resumes it, at generation 2. The park case
			// pins the queue to foreign holder C until the affinity ceiling passes, so age the episode
			// past it: the control then proves provenance, not the pin, is what admits A.
			env.exec(`UPDATE runs SET status_since = now() - interval '2 hours', updated_at = now() - interval '2 hours' WHERE id = $1`, fx.runID)
			p := fx.claim(t, fx.workerA)
			if p == nil || p.Secrets.Codex == nil {
				t.Fatal("recorded worker A was refused its own run")
			}
			r := mustRun(t, env, fx.runID)
			if r.ClaimGeneration != 2 || r.Status != "claimed" || r.WorkerID != pgconv.UUID(fx.workerA) {
				t.Fatalf("after A's resume: gen=%d status=%s worker=%v", r.ClaimGeneration, r.Status, r.WorkerID)
			}
			fx.assertProvenance(t, fx.workerA, 2)
		})
	}
}

// A legacy row: claimed before this change, so generation >= 1 with no provenance at all. No worker
// is admitted, A included, and the health pass says so without inventing a worker name.
func TestCodexProvenanceLegacyNullHoldsEveryWorkerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newClaimProvFix(t, env)
	// Seeding the generation directly leaves provenance NULL and does not fire the guard trigger.
	env.exec(`UPDATE runs SET claim_generation = 1, worker_id = $2 WHERE id = $1`, fx.runID, fx.workerA)
	var w pgtype.UUID
	var g pgtype.Int8
	if err := env.pool.QueryRow(env.ctx, `SELECT claim_provenance_worker_id, claim_provenance_generation FROM runs WHERE id = $1`, fx.runID).Scan(&w, &g); err != nil || w.Valid || g.Valid {
		t.Fatalf("seed produced provenance (%v, %v) err=%v", w, g, err)
	}
	fx.assertRefused(t, fx.workerA)
	fx.assertRefused(t, fx.workerB)
	fx.assertHealth(t, provHeldUnavailable)
}

// ---- judge lane ------------------------------------------------------------------------------

// A judge run is claimed through the same ClaimRun lane, so a judge held for A is refused to B and
// B's refused claim mints nothing.
func TestCodexProvenanceJudgeRunHeldForRecordedWorkerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newClaimProvFix(t, env)
	target := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		VALUES ($1, $2, $3, 'issue', 2, 't', 'd', 'completed', $4)`, target, fx.userID, fx.repoID, fx.workerA)
	env.exec(`UPDATE runs SET kind = 'judge', issue_iid = NULL, repo_id = NULL, target_run_id = $2 WHERE id = $1`, fx.runID, target)

	fx.firstClaimByA(t)
	// The judge run comes back to queued (worker lost): affinity to A has lapsed, provenance has not.
	env.exec(`UPDATE runs SET status = 'queued', status_since = now() - interval '2 hours', codex_cap_hash = NULL,
		updated_at = now() - interval '2 hours' WHERE id = $1`, fx.runID)
	fx.assertRefused(t, fx.workerB)
	fx.assertHealth(t, fmt.Sprintf(provHeldNamedA, fx.nameA))

	if p := fx.claim(t, fx.workerA); p == nil || p.Secrets.Codex == nil {
		t.Fatal("recorded worker A was refused its own judge run")
	}
	fx.assertProvenance(t, fx.workerA, 2)
}
