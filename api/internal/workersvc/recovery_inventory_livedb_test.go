package workersvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests exercise service lifecycle callers with inventory-guarded custody.
// Run with UZI_TEST_DATABASE_URL against the throwaway store integration database.
func inventoryLifecycleHold(t *testing.T, e leaseEnv, w, run uuid.UUID, gen int64, guarded bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,$5,'open',$6,'ident',$6,$4,$7)`, id, e.userID, e.repoID, run, gen, w, guarded)
	e.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w, []string{capability.CompletionInterlockV1, capability.RecoveryInventoryV1})
	return id
}

func inventoryLifecycleRecovery(e leaseEnv) *recovery.Service {
	return recovery.New(e.q, e.pool, e.box, recovery.Limits{
		MaxBundleBytes: 1 << 20, MaxCapturesPerClaim: 8, MaxCapturesPerOwner: 16,
		ReadyRetention: time.Hour, RequestDeadline: 10 * time.Second,
	}, nil)
}

func inventoryLifecycleReserve(t *testing.T, e leaseEnv, rs *recovery.Service, w store.Worker, run uuid.UUID, gen int64, key, digest string) uuid.UUID {
	t.Helper()
	res, err := rs.Reserve(e.ctx, w, run, apitypes.RecoveryReserveRequest{
		Generation: &gen, IdempotencyKey: key, SourceSha: "aaaa1111", CoverageDigest: digest,
	})
	if err != nil {
		t.Fatalf("Reserve(%s): %v", key, err)
	}
	return uuid.MustParse(res.CaptureID)
}

func inventoryLifecycleUpload(t *testing.T, e leaseEnv, rs *recovery.Service, w store.Worker, run, cap uuid.UUID) {
	t.Helper()
	body := []byte("aggregate divergent inventory\n")
	sum := sha256.Sum256(body)
	res, err := rs.Upload(e.ctx, w, run, cap, apitypes.RecoveryUploadManifest{
		ByteSize: int64(len(body)), Checksum: hex.EncodeToString(sum[:]), ChunkCount: 1,
	}, bytes.NewReader(body))
	if err != nil || res.State != "available" || !res.ManifestBound {
		t.Fatalf("Upload: %+v %v", res, err)
	}
}

func inventoryLifecycleFinal(gen int64, cap uuid.UUID, digest string) apitypes.RecoveryReleaseRequest {
	return apitypes.RecoveryReleaseRequest{Generation: &gen, FinalDisposition: &apitypes.RecoveryFinalDisposition{
		Kind: "archive", CaptureID: cap.String(), SourceSha: "aaaa1111", CoverageDigest: digest,
	}}
}

func inventoryLifecycleOpen(t *testing.T, e leaseEnv, hold, w, run uuid.UUID) {
	t.Helper()
	var state string
	var liveWorker, liveRun pgtype.UUID
	var final, evidence pgtype.Text
	if err := e.pool.QueryRow(e.ctx, `SELECT state,live_worker_id,live_run_id,final_disposition,release_evidence
 FROM recovery_custody_holds WHERE id=$1`, hold).Scan(&state, &liveWorker, &liveRun, &final, &evidence); err != nil {
		t.Fatal(err)
	}
	if state != "open" || !liveWorker.Valid || liveWorker.Bytes != w || !liveRun.Valid || liveRun.Bytes != run || final.Valid || evidence.Valid {
		t.Fatalf("hold: state=%s worker=%v run=%v final=%v evidence=%v", state, liveWorker, liveRun, final, evidence)
	}
	if !e.workerExists(t, w) || len(e.workerRow(t, w).TokenHash) == 0 {
		t.Fatal("custody worker credential lost")
	}
}

// Completion must commit while both the transactional lease release and the
// postcommit publication release leave guarded inventory available for a later reserve.
func TestRecoveryInventoryCompletionRetainsCustodyLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		lease                time.Duration
		interlocked, earlier bool
	}{
		{"postcommit unavailable", 0, false, false},
		{"fence transaction earlier archive", time.Hour, false, true},
		{"permit transaction unavailable", time.Hour, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(tc.lease, e.pool)
			w, run, iid := e.seedBound(t, "running", 1, tc.interlocked)
			hold := inventoryLifecycleHold(t, e, w, run, 1, true)
			worker := e.workerRow(t, w)
			rs := inventoryLifecycleRecovery(e)
			initial := inventoryLifecycleReserve(t, e, rs, worker, run, 1, "earlier", strings.Repeat("a", 64))
			if tc.earlier {
				inventoryLifecycleUpload(t, e, rs, worker, run, initial)
			}
			if tc.interlocked {
				e.grantPermit(t, svc, w, run, iid)
			}
			res := awaitState(t, e.reportAsync(svc, w, run, completeReq(iid, 1)))
			if res.err != nil || !res.applied || res.run.Status != "completed" || e.runStatusOf(t, run) != "completed" {
				t.Fatalf("completion: %+v err=%v", res, res.err)
			}
			inventoryLifecycleOpen(t, e, hold, w, run)
			if e.leased(t, w) {
				t.Fatal("guarded inventory entered an ephemeral lease")
			}
			if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
				t.Fatal(err)
			}
			inventoryLifecycleOpen(t, e, hold, w, run)
			e.reap(t, tc.lease)
			inventoryLifecycleOpen(t, e, hold, w, run)
			if err := svc.DeleteWorker(e.ctx, e.userID, w); !errors.Is(err, ErrWorkerHasCustody) {
				t.Fatalf("DeleteWorker: %v", err)
			}

			// A ready earlier snapshot is not a final receipt; the final aggregate can
			// still be reserved after the terminal status has landed.
			digest := strings.Repeat("b", 64)
			final := inventoryLifecycleReserve(t, e, rs, worker, run, 1, "final", digest)
			req := inventoryLifecycleFinal(1, final, digest)
			if ack, err := rs.Release(e.ctx, worker, run, req); !errors.Is(err, recovery.ErrManifestConflict) || ack.Released {
				t.Fatalf("unavailable final release: %+v %v", ack, err)
			}
			inventoryLifecycleOpen(t, e, hold, w, run)
			inventoryLifecycleUpload(t, e, rs, worker, run, final)
			ack, err := rs.Release(e.ctx, worker, run, req)
			if err != nil || !ack.Released || ack.HoldsReleased != 1 || e.holdState(t, hold) != "released" {
				t.Fatalf("final release: %+v %v", ack, err)
			}
			if tc.lease == 0 {
				if err := svc.DeleteWorker(e.ctx, e.userID, w); err != nil {
					t.Fatalf("DeleteWorker after final archive: %v", err)
				}
			} else {
				e.reap(t, tc.lease)
			}
			if e.workerExists(t, w) {
				t.Fatal("ephemeral worker survives deletion after exact final archive")
			}
		})
	}
}

// An old generation may archive its final inventory while its successor is
// running; the successor's hold and credential must remain untouched.
func TestRecoveryInventoryOldGenerationFinalWithActiveSuccessorLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(time.Hour, e.pool)
	current, run, _ := e.seedBound(t, "running", 2, false)
	currentHold := inventoryLifecycleHold(t, e, current, run, 2, true)
	old := uuid.New()
	e.exec(`INSERT INTO workers(id,user_id,name,token_hash,template_declared,kind,hosted_size,docker_enabled,ephemeral,
 ephemeral_run_id,status,online_since,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'base','hosted','m',false,true,NULL,'online',now(),now())`, old, e.userID, "old-"+old.String(), old[:])
	oldHold := inventoryLifecycleHold(t, e, old, run, 1, true)
	worker := e.workerRow(t, old)
	rs := inventoryLifecycleRecovery(e)
	digest := strings.Repeat("a", 64)
	cap := inventoryLifecycleReserve(t, e, rs, worker, run, 1, "old-final", digest)
	inventoryLifecycleUpload(t, e, rs, worker, run, cap)
	if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
		t.Fatal(err)
	}
	inventoryLifecycleOpen(t, e, oldHold, old, run)
	ack, err := rs.Release(e.ctx, worker, run, inventoryLifecycleFinal(1, cap, digest))
	if err != nil || !ack.Released || ack.HoldsReleased != 1 || e.holdState(t, oldHold) != "released" {
		t.Fatalf("old-generation final: %+v %v", ack, err)
	}
	inventoryLifecycleOpen(t, e, currentHold, current, run)
	if e.runStatusOf(t, run) != "running" {
		t.Fatal("old final changed active successor status")
	}
	e.reap(t, time.Hour)
	if e.workerExists(t, old) {
		t.Fatal("released old-generation worker was not reaped")
	}
	inventoryLifecycleOpen(t, e, currentHold, current, run)
}

// A delivered worker can have adopted source before forge failure. A park must
// retain its guarded inventory; the existing legacy release remains a control.
func TestRecoveryInventoryForgeParkRetainsAdoptedSourceLiveDB(t *testing.T) {
	for _, guarded := range []bool{true, false} {
		name := "legacy control"
		if guarded {
			name = "guarded adopted source"
		}
		t.Run(name, func(t *testing.T) {
			e := newLeaseEnv(t)
			svc := e.service(0, e.pool)
			w, run, _ := e.seedBound(t, "running", 1, false)
			hold := inventoryLifecycleHold(t, e, w, run, 1, guarded)
			if guarded {
				rs := inventoryLifecycleRecovery(e)
				inventoryLifecycleReserve(t, e, rs, e.workerRow(t, w), run, 1, "adopted-source", strings.Repeat("a", 64))
			}
			res := awaitState(t, e.reportAsync(svc, w, run, StateRequest{
				State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1),
			}))
			if res.err != nil || !res.applied || res.run.Status != "recovery_wait" || e.runStatusOf(t, run) != "recovery_wait" {
				t.Fatalf("forge park: %+v err=%v", res, res.err)
			}
			if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
				t.Fatal(err)
			}
			if guarded {
				inventoryLifecycleOpen(t, e, hold, w, run)
			} else if e.holdState(t, hold) != "released" {
				t.Fatal("legacy park did not release custody")
			}
		})
	}
}

func inventorySettledRelease(gen int64, evidence string) apitypes.RecoveryReleaseRequest {
	return apitypes.RecoveryReleaseRequest{Generation: &gen, ReleaseEvidence: &evidence,
		FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}
}

func (e leaseEnv) openHoldsOfUser(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE user_id=$1 AND state='open'`, e.userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// issue #1924: a guarded generation that parks on a forge failure before any clone adopted
// nothing. The api retains its guarded hold through the park (above), so the worker's settled
// forge_no_output release for that exact generation is what closes it and frees the owner's hold
// slot. The same park with an adopted source is never settled by the worker (it sends no
// release), and the other final dispositions still wait for the generation to end.
func TestRecoveryInventoryForgeParkPreCloneSettledReleaseLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	svc := e.service(0, e.pool)
	w, run, _ := e.seedBound(t, "running", 1, false)
	hold := inventoryLifecycleHold(t, e, w, run, 1, true)
	worker := e.workerRow(t, w)
	rs := inventoryLifecycleRecovery(e)

	// Before the park the generation has not ended: the settled release is refused.
	if _, err := rs.Release(e.ctx, worker, run, inventorySettledRelease(1, "forge_no_output")); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("release before the park: %v", err)
	}
	res := awaitState(t, e.reportAsync(svc, w, run, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1),
	}))
	if res.err != nil || !res.applied || e.runStatusOf(t, run) != "recovery_wait" {
		t.Fatalf("forge park: %+v err=%v", res, res.err)
	}
	inventoryLifecycleOpen(t, e, hold, w, run)
	if got := e.openHoldsOfUser(t); got != 1 {
		t.Fatalf("open holds after park = %d, want 1", got)
	}
	// Only the settled forge_no_output proof may end a parked generation: the publication class and
	// an archive disposition keep the terminal / released / newer-claim gate.
	if _, err := rs.Release(e.ctx, worker, run, inventorySettledRelease(1, "publication")); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("publication release on a parked run: %v", err)
	}
	cap := inventoryLifecycleReserve(t, e, rs, worker, run, 1, "parked-archive", strings.Repeat("c", 64))
	inventoryLifecycleUpload(t, e, rs, worker, run, cap)
	if _, err := rs.Release(e.ctx, worker, run, inventoryLifecycleFinal(1, cap, strings.Repeat("c", 64))); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("archive release on a parked run: %v", err)
	}
	inventoryLifecycleOpen(t, e, hold, w, run)

	ack, err := rs.Release(e.ctx, worker, run, inventorySettledRelease(1, "forge_no_output"))
	if err != nil || !ack.Released || ack.HoldsReleased != 1 || e.holdState(t, hold) != "released" {
		t.Fatalf("settled release after the park: %+v %v", ack, err)
	}
	if again, err := rs.Release(e.ctx, worker, run, inventorySettledRelease(1, "forge_no_output")); err != nil || !again.Released {
		t.Fatalf("replayed settled release must echo: %+v %v", again, err)
	}
	if got := e.openHoldsOfUser(t); got != 0 {
		t.Fatalf("hold slot not freed: %d open", got)
	}
	if e.runStatusOf(t, run) != "recovery_wait" {
		t.Fatal("the settled release changed the parked run")
	}
}

// An untyped (empty-turn) recovery_wait park is not a pre-clone forge park: the settled release
// stays refused there, so only the forge park's cause widens the gate.
func TestRecoveryInventorySettledReleaseRefusedOnUntypedParkLiveDB(t *testing.T) {
	e := newLeaseEnv(t)
	w, run, _ := e.seedBound(t, "running", 1, false)
	hold := inventoryLifecycleHold(t, e, w, run, 1, true)
	e.exec(`UPDATE runs SET status='recovery_wait', recovery_wait_cause=NULL WHERE id=$1`, run)
	rs := inventoryLifecycleRecovery(e)
	if _, err := rs.Release(e.ctx, e.workerRow(t, w), run, inventorySettledRelease(1, "forge_no_output")); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("release on an untyped park: %v", err)
	}
	inventoryLifecycleOpen(t, e, hold, w, run)
}
