package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerExhaustionRegisterSameIncarnationClaimLiveDB(t *testing.T) {
	for _, transactional := range []bool{false, true} {
		for _, priorNull := range []bool{false, true} {
			for _, attested := range []bool{false, true} {
				name := "ordinary"
				if transactional {
					name = "frozen"
				}
				if priorNull {
					name += "/null"
				}
				if attested {
					name += "/attested"
				}
				t.Run(name, func(t *testing.T) {
					env := setupCodexLiveDB(t)
					user, _, repo := env.seedCodexInfra(t)
					worker := seedSnapshotWorker(t, env, user, "old-A")
					if priorNull {
						env.exec("UPDATE workers SET snapshot_register_nonce=NULL WHERE id=$1", worker)
					}
					run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, 2)
					env.exec("UPDATE runs SET checkpoint_tip=$2, started_at=now()-interval '1 minute',budget_wall_seconds=300,finalize_resume_generation=2,codex_cap_hash=$3,codex_claim_epoch=41 WHERE id=$1", run, strings.Repeat("b", 40), []byte("known capability"))
					svc := snapshotSvc(env, testParams())
					if !transactional {
						svc.txBeginner = nil
					}
					staleAuth := hbWorker(t, env, worker)
					// Authentication's copy is deliberately stale; the actual row owns old-A/NULL.
					staleAuth.SnapshotRegisterNonce = pgtype.Text{String: "stale-auth-copy", Valid: true}
					var snapshot *ActiveSnapshot
					if attested {
						snapshot = finalizeSnap(false, fin(run, 2))
					}
					registered, nonce, err := svc.Register(env.ctx, staleAuth, "fixture", "", nil, nil, []string{capability.RecoveryArchiveV1}, snapshot)
					if err != nil {
						t.Fatal(err)
					}
					if nonce == "" || nonce == "old-A" || registered.SnapshotRegisterNonce.String != nonce {
						t.Fatalf("nonce not rotated: %q", nonce)
					}
					held := exhaustionRun(t, env, run)
					if held.Status != "recovery_wait" || len(held.CodexCapHash) != 0 || held.CodexClaimEpoch != 42 {
						t.Fatalf("park/revocation status=%s hash=%v epoch=%d", held.Status, held.CodexCapHash, held.CodexClaimEpoch)
					}
					if _, err = env.q.ResumeWorkerRecoveryEpisode(env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: run, UserID: user, GlobalTimeoutSeconds: 3600}); err != nil {
						t.Fatal(err)
					}
					tx, err := env.pool.Begin(env.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(env.ctx) }()
					q := env.q.WithTx(tx)
					claimant, err := q.GetWorkerForUpdate(env.ctx, worker)
					if err != nil {
						t.Fatal(err)
					}
					now := time.Now()
					claimed, err := q.ClaimRun(env.ctx, store.ClaimRunParams{WorkerID: pgconv.UUID(worker), UserID: user, AffinityCutoff: pgconv.Time(now.Add(-2 * time.Hour)), SpreadCutoff: pgconv.Time(now.Add(-9 * time.Second)), HeartbeatCutoff: pgconv.Time(now.Add(-45 * time.Second)), IsDockerWorker: true, DockerRepoAllowlist: []uuid.UUID{repo}, RecoveryCapable: true, WorkerProtocolCaps: claimant.ProtocolCapabilities, WorkerIdentity: "fixture"})
					if err != nil || claimed.ID != run {
						t.Fatalf("fresh registered B blocked by released nonce: released=%v current=%q claim=%s err=%v", held.ReleasedWorkerNonce, nonce, claimed.ID, err)
					}
					if err = tx.Commit(env.ctx); err != nil {
						t.Fatal(err)
					}
					if held.ReleasedWorkerNonce.Valid == priorNull || (!priorNull && held.ReleasedWorkerNonce.String != "old-A") {
						t.Fatalf("released nonce did not capture actual old row: %v", held.ReleasedWorkerNonce)
					}
					live, err := env.q.RunUsageFenceLiveLocked(env.ctx, store.RunUsageFenceLiveLockedParams{RunID: run, WorkerID: pgconv.UUID(worker), ClaimGeneration: pgtype.Int8{Int64: 2, Valid: true}})
					if err != nil || live {
						t.Fatalf("old generation report accepted: live=%v err=%v", live, err)
					}
				})
			}
		}
	}
}
