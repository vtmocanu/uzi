package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests cover the actual SQL claim/requeue lifecycle and service terminal
// transitions/classification. They do not exercise scheduler or forge behavior.
// setupInterlockLiveDB applies the real migrations and skips without a throwaway DB.
const orphanReclaimRef = "agent/issue-1810"
const orphanReclaimMRIID int64 = 1810

func orphanReclaimWorker(t *testing.T, e interlockLiveDB) store.Worker {
	t.Helper()
	id := uuid.New()
	name := "orphan-reclaim-" + id.String()
	e.exec(t, `INSERT INTO workers
		(id, user_id, name, token_hash, status, last_heartbeat_at,
		 protocol_capabilities, capabilities, draining_since, isolated_lane, ephemeral,
		 pending_overflow, pending_overflow_until)
		VALUES ($1, $2, $3, $4, 'online', now(),
		 '{recovery_archive_v1}', '{}', NULL, false, false, false, NULL)`,
		id, e.userID, name, id[:])
	return store.Worker{
		ID: id, UserID: e.userID, Name: name, Status: "online",
		ProtocolCapabilities: []string{"recovery_archive_v1"}, Capabilities: []string{},
	}
}

func orphanReclaimParams(e interlockLiveDB, w store.Worker) store.ClaimRunParams {
	p := e.claimParams(w.ID, w.ProtocolCapabilities, true)
	p.WorkerIdentity = workerIdentity(w)
	p.RecoveryCapable = true
	p.CustodyHoldLimit = 8 // All scenarios remain below the positive owner custody cap.
	p.WorkerCaps = w.Capabilities
	p.ClaimantDraining = w.DrainingSince.Valid
	p.WorkerIsolatedLane = w.IsolatedLane
	p.IsEphemeral = w.Ephemeral
	p.RequestActiveIds = []uuid.UUID{}
	p.RequestActiveGens = []int64{}
	p.SnapshotFreshCutoff = p.HeartbeatCutoff
	return p
}

// The completed issue is only the prerequisite target of the queued MR rework.
// Every rework ownership/status/generation transition uses ClaimRun or the service.
func orphanReclaimTarget(t *testing.T, e interlockLiveDB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(t, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		VALUES ($1, $2, $3, 'issue', 1810, 'target', 'target', 'completed')`,
		id, e.userID, e.repoID)
	return id
}

func orphanReclaimQueued(t *testing.T, e interlockLiveDB, target uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(t, `INSERT INTO runs
		(id, user_id, repo_id, kind, status, mr_iid, target_run_id, pipeline_ref,
		 issue_title, issue_description)
		VALUES ($1, $2, $3, 'mr_rework', 'queued', $4, $5, $6, 'rework', 'rework')`,
		id, e.userID, e.repoID, orphanReclaimMRIID, target, orphanReclaimRef)
	return id
}

func orphanReclaimClaim(t *testing.T, e interlockLiveDB, p store.ClaimRunParams, id uuid.UUID, gen int64) store.Run {
	t.Helper()
	r, err := e.q.ClaimRun(e.ctx, p)
	if err != nil {
		t.Fatalf("ClaimRun generation %d: %v", gen, err)
	}
	if r.ID != id || r.Status != "claimed" || r.ClaimGeneration != gen ||
		r.WorkerID != p.WorkerID || r.Branch.Valid ||
		!r.PipelineRef.Valid || r.PipelineRef.String != orphanReclaimRef {
		t.Fatalf("claim identity/branch/generation: %+v", r)
	}
	return r
}

// Requeue preserves the previous worker and generation. First prove that a live A
// still blocks B, then move only B's cutoff beyond the updated_at read from SQL.
// There are no snapshots or overflow closures in these fixtures.
func orphanReclaimRequeue(t *testing.T, e interlockLiveDB, a, b store.Worker, id uuid.UUID, gen int64) store.ClaimRunParams {
	t.Helper()
	ids, err := e.q.RequeueWorkerRuns(e.ctx, store.RequeueWorkerRunsParams{
		WorkerID: pgconv.UUID(a.ID), MaxRequeues: 3,
	})
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("RequeueWorkerRuns within budget: ids=%v err=%v", ids, err)
	}
	var status string
	var owner uuid.UUID
	var generation int64
	var requeues int32
	var updated time.Time
	if err := e.pool.QueryRow(e.ctx,
		`SELECT status, worker_id, claim_generation, requeue_count, updated_at FROM runs WHERE id = $1`, id).
		Scan(&status, &owner, &generation, &requeues, &updated); err != nil {
		t.Fatalf("read requeued run: %v", err)
	}
	if status != "queued" || owner != a.ID || generation != gen || requeues != 1 {
		t.Fatalf("requeue changed ownership/generation or missed budget: %s %s gen=%d requeues=%d",
			status, owner, generation, requeues)
	}
	p := orphanReclaimParams(e, b)
	if _, err := e.q.ClaimRun(e.ctx, p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("B must be affinity-blocked while A's heartbeat is fresh: %v", err)
	}
	p.AffinityCutoff = pgtype.Timestamptz{Time: updated.Add(time.Microsecond), Valid: true}
	return p
}

type orphanReclaimHold struct {
	ID, UserID, RepoID, RunID, OriginalWorkerID uuid.UUID
	Generation                                  int64
	State, OriginalIdentity                     string
	LiveWorkerID, LiveRunID                     pgtype.UUID
	CreatedAt, UpdatedAt, ReleasedAt            pgtype.Timestamptz
	ReleaseEvidence                             pgtype.Text
}

func orphanReclaimReadHold(t *testing.T, e interlockLiveDB, runID uuid.UUID, gen int64) orphanReclaimHold {
	t.Helper()
	var h orphanReclaimHold
	// No LIMIT: Query collects the exact generation's rows so duplicates cannot hide.
	rows, err := e.pool.Query(e.ctx, `SELECT id, user_id, repo_id, run_id, generation, state,
		original_worker_id, original_worker_identity, live_worker_id, live_run_id,
		created_at, updated_at, released_at, release_evidence
		FROM recovery_custody_holds WHERE run_id = $1 AND generation = $2`, runID, gen)
	if err != nil {
		t.Fatalf("read exact custody hold: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&h.ID, &h.UserID, &h.RepoID, &h.RunID, &h.Generation, &h.State,
			&h.OriginalWorkerID, &h.OriginalIdentity, &h.LiveWorkerID, &h.LiveRunID,
			&h.CreatedAt, &h.UpdatedAt, &h.ReleasedAt, &h.ReleaseEvidence); err != nil {
			t.Fatalf("scan custody hold: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate custody holds: %v", err)
	}
	if n != 1 {
		t.Fatalf("run %s generation %d: got %d holds, want exactly one", runID, gen, n)
	}
	return h
}

func orphanReclaimOpenHold(t *testing.T, e interlockLiveDB, r store.Run, w store.Worker) orphanReclaimHold {
	t.Helper()
	h := orphanReclaimReadHold(t, e, r.ID, r.ClaimGeneration)
	if h.ID == uuid.Nil || h.UserID != e.userID || h.RepoID != e.repoID || h.RunID != r.ID ||
		h.Generation != r.ClaimGeneration || h.State != "open" ||
		h.OriginalWorkerID != w.ID || h.OriginalIdentity != workerIdentity(w) ||
		h.LiveWorkerID != pgconv.UUID(w.ID) || h.LiveRunID != pgconv.UUID(r.ID) ||
		!h.CreatedAt.Valid || !h.UpdatedAt.Valid || h.ReleasedAt.Valid || h.ReleaseEvidence.Valid {
		t.Fatalf("ClaimRun must auto-create the exact open hold with provenance/live pointers: %+v", h)
	}
	return h
}

func orphanReclaimUnchanged(t *testing.T, e interlockLiveDB, holds ...orphanReclaimHold) {
	t.Helper()
	for _, want := range holds {
		if got := orphanReclaimReadHold(t, e, want.RunID, want.Generation); got != want {
			t.Fatalf("custody hold changed: got %+v want %+v", got, want)
		}
	}
	var captures int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM recovery_captures WHERE user_id = $1`, e.userID).Scan(&captures); err != nil {
		t.Fatalf("read captures: %v", err)
	}
	if captures != 0 {
		t.Fatalf("lifecycle/classification created %d captures without an upload", captures)
	}
}

func orphanReclaimState(t *testing.T, e interlockLiveDB, svc *Service, w store.Worker, id uuid.UUID, gen int64, state string) {
	t.Helper()
	req := StateRequest{State: state, ClaimGeneration: &gen}
	if state == "completed" {
		iid := orphanReclaimMRIID
		req.MrIID = &iid // SetRunCompleted assigns mr_iid; retain the valid rework shape.
	}
	r, applied, err := svc.SetState(e.ctx, w, id, req) // Branch omitted: preserve SQL NULL.
	if err != nil || !applied || r.Status != state || r.ClaimGeneration != gen ||
		r.WorkerID != pgconv.UUID(w.ID) || r.Branch.Valid ||
		!r.PipelineRef.Valid || r.PipelineRef.String != orphanReclaimRef {
		t.Fatalf("SetState(%s) identity/generation: applied=%v run=%+v err=%v", state, applied, r, err)
	}
}

func TestOrphanReclaimSQLLifecycleAndServiceTerminalLiveDB(t *testing.T) {
	for _, terminal := range []string{"failed", "completed"} {
		t.Run(terminal, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			a, b := orphanReclaimWorker(t, e), orphanReclaimWorker(t, e)
			target := orphanReclaimTarget(t, e)
			ownerID := orphanReclaimQueued(t, e, target)

			first := orphanReclaimClaim(t, e, orphanReclaimParams(e, a), ownerID, 1)
			ah := orphanReclaimOpenHold(t, e, first, a)
			orphanReclaimUnchanged(t, e, ah)

			bp := orphanReclaimRequeue(t, e, a, b, ownerID, 1)
			orphanReclaimUnchanged(t, e, ah)
			second := orphanReclaimClaim(t, e, bp, ownerID, 2)
			bh := orphanReclaimOpenHold(t, e, second, b)
			if bh.ID == ah.ID {
				t.Fatal("B's generation-2 hold reused A's generation-1 hold")
			}
			orphanReclaimUnchanged(t, e, ah, bh)
			orphanReclaimState(t, e, svc, b, ownerID, 2, "running")
			orphanReclaimUnchanged(t, e, ah, bh)
			orphanReclaimState(t, e, svc, b, ownerID, 2, terminal)
			if terminal == "completed" {
				released := orphanReclaimReadHold(t, e, ownerID, 2)
				if released.State != "released" || released.LiveWorkerID.Valid ||
					released.LiveRunID.Valid || !released.ReleasedAt.Valid ||
					!released.ReleaseEvidence.Valid || released.ReleaseEvidence.String != "publication" {
					t.Fatalf("completed B hold must release only its live pointers: %+v", released)
				}
				want := bh
				want.State, want.LiveWorkerID, want.LiveRunID = "released", pgtype.UUID{}, pgtype.UUID{}
				want.UpdatedAt, want.ReleasedAt = released.UpdatedAt, released.ReleasedAt
				want.ReleaseEvidence = pgtype.Text{String: "publication", Valid: true}
				if released != want {
					t.Fatalf("release changed B's immutable provenance: got %+v want %+v", released, want)
				}
				bh = released
			}
			orphanReclaimUnchanged(t, e, ah, bh)

			// Only now can a successor share repo/ref without violating the unique
			// nonterminal rework index. It is a fresh run, hence generation 1.
			claimantID := orphanReclaimQueued(t, e, target)
			orphanReclaimUnchanged(t, e, ah, bh)
			claimant := orphanReclaimClaim(t, e, orphanReclaimParams(e, a), claimantID, 1)
			ch := orphanReclaimOpenHold(t, e, claimant, a)
			orphanReclaimUnchanged(t, e, ah, bh, ch)
			identity, err := svc.RunOrphanClassification(e.ctx, a, claimantID, ownerID)
			if err != nil {
				t.Fatalf("classify B's terminal owner from A's successor: %v", err)
			}
			if identity.Status != terminal || identity.RepoID != e.repoID ||
				identity.Kind != "mr_rework" || identity.Branch != nil ||
				identity.PipelineRef == nil || *identity.PipelineRef != orphanReclaimRef ||
				identity.IssueIID != nil || identity.PipelineID != nil {
				t.Fatalf("terminal owner identity with NULL branch/ref fallback: %+v", identity)
			}
			orphanReclaimUnchanged(t, e, ah, bh, ch)

			// A legitimate requeue/reclaim removes A's authorization anchor even
			// though its old custody and the same-user/repo owner still exist.
			bp = orphanReclaimRequeue(t, e, a, b, claimantID, 1)
			orphanReclaimUnchanged(t, e, ah, bh, ch)
			reassigned := orphanReclaimClaim(t, e, bp, claimantID, 2)
			rh := orphanReclaimOpenHold(t, e, reassigned, b)
			orphanReclaimUnchanged(t, e, ah, bh, ch, rh)
			if _, err := svc.RunOrphanClassification(e.ctx, a, claimantID, ownerID); !errors.Is(err, ErrRunNotOwned) {
				t.Fatalf("A classified after claimant reassigned to B: %v", err)
			}
			orphanReclaimUnchanged(t, e, ah, bh, ch, rh)
		})
	}
}

func TestOrphanReclaimOwnerScopeDeniedLiveDB(t *testing.T) {
	for _, scope := range []string{"wrong-user", "wrong-repo"} {
		t.Run(scope, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			a := orphanReclaimWorker(t, e)
			target := orphanReclaimTarget(t, e)
			foreign := setupInterlockLiveDB(t)
			if scope == "wrong-user" {
				foreign.repoID = e.repoID // Isolate user mismatch; repo must match the anchor.
			} else {
				foreign.userID = e.userID // Isolate repo mismatch; user must match the anchor.
			}
			b := orphanReclaimWorker(t, foreign)
			foreignTarget := orphanReclaimTarget(t, foreign)
			ownerID := orphanReclaimQueued(t, foreign, foreignTarget)
			owner := orphanReclaimClaim(t, foreign, orphanReclaimParams(foreign, b), ownerID, 1)
			oh := orphanReclaimOpenHold(t, foreign, owner, b)
			orphanReclaimUnchanged(t, foreign, oh)
			orphanReclaimState(t, foreign, svc, b, ownerID, 1, "running")
			orphanReclaimUnchanged(t, foreign, oh)
			orphanReclaimState(t, foreign, svc, b, ownerID, 1, "failed")
			orphanReclaimUnchanged(t, foreign, oh)

			// The repo/ref unique index is independent of user, so seed the
			// anchor only after the wrong-user owner has become terminal.
			claimantID := orphanReclaimQueued(t, e, target)
			orphanReclaimUnchanged(t, foreign, oh)
			claimant := orphanReclaimClaim(t, e, orphanReclaimParams(e, a), claimantID, 1)
			ch := orphanReclaimOpenHold(t, e, claimant, a)
			orphanReclaimUnchanged(t, e, ch)
			orphanReclaimUnchanged(t, foreign, oh)

			// Positive control proves A still holds a valid anchor before the
			// cross-scope OWNER lookup. Only that lookup must be denied.
			if _, err := svc.RunOrphanClassification(e.ctx, a, claimantID, claimantID); err != nil {
				t.Fatalf("valid claimant authorization anchor: %v", err)
			}
			if _, err := svc.RunOrphanClassification(e.ctx, a, claimantID, ownerID); !errors.Is(err, ErrRunNotOwned) {
				t.Fatalf("%s OWNER classification: %v, want ErrRunNotOwned", scope, err)
			}
			orphanReclaimUnchanged(t, e, ch)
			orphanReclaimUnchanged(t, foreign, oh)
		})
	}
}
