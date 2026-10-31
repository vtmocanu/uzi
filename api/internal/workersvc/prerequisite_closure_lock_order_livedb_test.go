package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type m3Recovery struct {
	svc      *recovery.Service
	worker   store.Worker
	capture  uuid.UUID
	body     []byte
	manifest apitypes.RecoveryUploadManifest
	final    apitypes.RecoveryReleaseRequest
}

func m3RecoveryFixture(t *testing.T, f *prerequisiteExecutorLiveDB, ownerLimit int) *m3Recovery {
	t.Helper()
	var id uuid.UUID
	e := f.env.pool.QueryRow(f.env.ctx, "SELECT worker_id FROM runs WHERE id=$1", f.m.row.RunID).Scan(&id)
	if e != nil {
		t.Fatalf("original run worker: %v", e)
	}
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", id, []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1})
	w, e := f.env.q.GetWorkerByID(f.env.ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	rs := recovery.New(f.env.q, f.env.pool, f.s.box, recovery.Limits{
		MaxBundleBytes: 1 << 20, MaxCapturesPerClaim: 8, MaxCapturesPerOwner: ownerLimit,
		ReadyRetention: time.Hour, RequestDeadline: 10 * time.Second,
	}, nil)
	gen := int64(1)
	digest := strings.Repeat("a", 64)
	res, e := rs.Reserve(f.env.ctx, w, f.m.row.RunID, apitypes.RecoveryReserveRequest{
		Generation: &gen, IdempotencyKey: uuid.NewString(), SourceSha: "aaaa1111", CoverageDigest: digest,
	})
	if e != nil {
		t.Fatal(e)
	}
	id, e = uuid.Parse(res.CaptureID)
	if e != nil {
		t.Fatal(e)
	}
	body := []byte("self-contained sibling FINAL inventory\n")
	sum := sha256.Sum256(body)
	return &m3Recovery{svc: rs, worker: w, capture: id, body: body,
		manifest: apitypes.RecoveryUploadManifest{ByteSize: int64(len(body)), Checksum: hex.EncodeToString(sum[:]), ChunkCount: 1},
		final:    inventoryLifecycleFinal(gen, id, digest)}
}
func (r *m3Recovery) upload(ctx context.Context, f *prerequisiteExecutorLiveDB, body io.Reader) (apitypes.RecoveryCaptureStatusResponse, error) {
	return r.svc.Upload(ctx, r.worker, f.m.row.RunID, r.capture, r.manifest, body)
}
func m3UploadAssert(t *testing.T, r *m3Recovery, up apitypes.RecoveryCaptureStatusResponse) {
	t.Helper()
	if up.State != "available" || !up.ManifestBound || up.CaptureID != r.capture.String() ||
		up.ByteSize == nil || *up.ByteSize != r.manifest.ByteSize || up.Checksum != r.manifest.Checksum {
		t.Fatalf("upload receipt: %+v", up)
	}
}
func m3FinalAssert(t *testing.T, f *prerequisiteExecutorLiveDB, r *m3Recovery, ack apitypes.RecoveryReleaseResponse) {
	t.Helper()
	expected := apitypes.RecoveryReleaseResponse{RunID: f.m.row.RunID.String(), Released: true, HoldsReleased: 1, Generation: r.final.Generation}
	if !reflect.DeepEqual(ack, expected) {
		t.Fatalf("FINAL receipt: %+v", ack)
	}
	h, e := f.env.q.GetFinalInventoryHold(f.env.ctx, store.GetFinalInventoryHoldParams{
		RunID: f.m.row.RunID, UserID: r.worker.UserID, WorkerID: r.worker.ID, Generation: 1,
	})
	if e != nil || h.State != "released" || h.ID != f.m.row.HoldID || h.FinalCaptureID != prerequisiteID(r.capture) ||
		h.FinalDisposition.String != "archive" || h.FinalSourceSha.String != "aaaa1111" ||
		h.FinalCoverageDigest.String != r.final.FinalDisposition.CoverageDigest || h.ReleaseEvidence.String != "archive" ||
		h.LiveRunID.Valid || h.LiveWorkerID.Valid || h.OriginalWorkerIdentity != "held-worker" {
		t.Fatalf("FINAL hold: %+v err=%v", h, e)
	}
	cap, e := f.env.q.GetCaptureForOwner(f.env.ctx, store.GetCaptureForOwnerParams{ID: r.capture, RunID: f.m.row.RunID, UserID: r.worker.UserID})
	if e != nil || cap.State != "available" || !cap.ManifestBound || cap.Checksum.String != r.manifest.Checksum ||
		cap.ByteSize.Int64 != r.manifest.ByteSize || cap.ChunkCount.Int32 != 1 || len(cap.PrerequisiteShas) != 0 ||
		cap.LocalReplicaWorkerID != prerequisiteID(r.worker.ID) || cap.ReadyRetentionSeconds.Int64 != 3600 ||
		cap.HoldID != f.m.row.HoldID || cap.RunID != f.m.row.RunID || cap.SourceSha != "aaaa1111" || cap.CoverageDigest.String != r.final.FinalDisposition.CoverageDigest {
		t.Fatalf("FINAL capture: %+v err=%v", cap, e)
	}
	chunks, e := f.env.q.ListCaptureChunks(f.env.ctx, r.capture)
	if e != nil || len(chunks) != 1 || chunks[0].ChunkIndex != 0 || int64(chunks[0].Length) != r.manifest.ByteSize || len(chunks[0].Sealed) == 0 {
		t.Fatalf("chunk inventory length=%d err=%v", len(chunks), e)
	}
	recorder := httptest.NewRecorder()
	if e = r.svc.Download(f.env.ctx, recorder, r.worker.UserID, f.m.row.RunID, r.capture); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(recorder.Body.Bytes(), r.body) {
		t.Fatal("download bytes differ")
	}
	thin, e := f.env.q.GetCaptureForOwner(f.env.ctx, store.GetCaptureForOwnerParams{ID: f.m.row.CaptureID, RunID: f.m.row.RunID, UserID: r.worker.UserID})
	if e != nil || thin.State != "available" || thin.Checksum.String != f.m.row.Checksum ||
		thin.OriginalWorkerIdentity != "held-worker" || !reflect.DeepEqual(thin.PrerequisiteShas, f.m.row.PrerequisiteShas) {
		t.Fatalf("thin capture changed: %+v err=%v", thin, e)
	}
}
func m3StageBuilder(t *testing.T, ctx context.Context, f *prerequisiteExecutorLiveDB, tx *m3Transactions) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var preparation sync.Once
	f.s.SetForges(&m3Builder{build: func(baseURL string) (forge.Forge, error) {
		var err error
		preparation.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				err = ctx.Err()
			}
		})
		if err != nil {
			return nil, err
		}
		return f.fixtureForge(t, baseURL, &prerequisiteTestForge{compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
			if tx.active.Load() {
				return forge.AncestryUnknown, io.ErrUnexpectedEOF
			}
			return forge.AncestryAncestor, nil
		}})
	}})
	return entered, unblock
}

type m3PausedReader struct {
	ctx     context.Context
	first   bool
	entered chan struct{}
	resume  <-chan struct{}
	body    io.Reader
}

func (r *m3PausedReader) Read(p []byte) (int, error) {
	if !r.first {
		r.first = true
		close(r.entered)
		select {
		case <-r.resume:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	return r.body.Read(p)
}

// FINAL has already acquired worker/run/hold before S2 is released from the
// post-preflight builder. The external capture lock is the only FINAL blocker.
func TestPrerequisiteFinalBeforeS2RefusesArchiveLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	recovery := m3RecoveryFixture(t, f, 16)
	up, e := recovery.upload(ctx, f, bytes.NewReader(recovery.body))
	if e != nil {
		t.Fatal(e)
	}
	m3UploadAssert(t, recovery, up)
	observer := m3Observer(t, ctx, f)
	tx := &m3Transactions{f: f}
	f.s.SetPrerequisiteStore(f.env.q, tx.transaction)
	remote := m3RemoteFor(t, f, tx)
	entered, release := m3StageBuilder(t, ctx, f, tx)
	defer release()
	sweep := m3Start(t, ctx, f.s.SweepPrerequisiteClosures)
	m3Wait(t, ctx, entered)
	holder, e := f.env.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = holder.Rollback(c)
	})
	var locked uuid.UUID
	if e = holder.QueryRow(ctx, "SELECT id FROM recovery_captures WHERE id=$1 FOR UPDATE", recovery.capture).Scan(&locked); e != nil {
		t.Fatal(e)
	}
	final := m3Start(t, ctx, func(c context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return recovery.svc.Release(c, recovery.worker, f.m.row.RunID, recovery.final)
	})
	finalPID := m3LockWait(t, ctx, observer, int32(holder.Conn().PgConn().PID()), "GetFinalInventoryCapture") //nolint:gosec // PostgreSQL PID fits int32.
	release()
	m3LockWait(t, ctx, observer, finalPID, "LockPrerequisiteRun")
	// No extra rendezvous while S2's actual 500ms timeout is ticking.
	if e = holder.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	ack := m3Join(t, ctx, final)
	m3Join(t, ctx, sweep)
	m3FinalAssert(t, f, recovery, ack)
	row := f.readLiveDB(t)
	_, commands := remote.snapshot()
	if row.State != "refused" || row.Reason != "archive_unavailable" || row.Attempts != 1 ||
		row.InvokedAt.Valid || row.InvokedDeadline.Valid || row.PinTip.Valid || row.LiveRunID.Valid || tx.committed.Load() || len(commands) != 0 ||
		remote.ioInTX.Load() || tx.orderViolation.Load() {
		t.Fatalf("S2 state=%s reason=%s attempts=%d invoked=%t live=%t commands=%d txIO=%t order=%t",
			row.State, row.Reason, row.Attempts, row.InvokedAt.Valid, row.LiveRunID.Valid, len(commands), remote.ioInTX.Load(), tx.orderViolation.Load())
	}
}

// Upload owns hold/capture; S2 owns run KEY SHARE and waits on that hold;
// FINAL must then wait for S2's run lock. Only observed PostgreSQL waits release
// the upload, so a timeout or retry cannot masquerade as lock-order evidence.
func TestPrerequisiteSiblingUploadS2ThenFinalLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	recovery := m3RecoveryFixture(t, f, 16)
	observer := m3Observer(t, ctx, f)
	tx := &m3Transactions{f: f}
	f.s.SetPrerequisiteStore(f.env.q, tx.transaction)
	remote := m3RemoteFor(t, f, tx)
	entered, release := m3StageBuilder(t, ctx, f, tx)
	defer release()
	sweep := m3Start(t, ctx, f.s.SweepPrerequisiteClosures)
	m3Wait(t, ctx, entered)
	reading, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	upload := m3Start(t, ctx, func(c context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
		return recovery.upload(c, f, &m3PausedReader{ctx: c, entered: reading, resume: resume, body: bytes.NewReader(recovery.body)})
	})
	m3Wait(t, ctx, reading)
	release()
	s2PID := m3LockWait(t, ctx, observer, 0, "LockPrerequisiteHold")
	final := m3Start(t, ctx, func(c context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return recovery.svc.Release(c, recovery.worker, f.m.row.RunID, recovery.final)
	})
	m3LockWait(t, ctx, observer, s2PID, "SELECT status, claim_generation, worker_id")
	unblock()
	m3UploadAssert(t, recovery, m3Join(t, ctx, upload))
	ack := m3Join(t, ctx, final)
	m3Join(t, ctx, sweep)
	m3FinalAssert(t, f, recovery, ack)
	row := f.readLiveDB(t)
	_, commands := remote.snapshot()
	if row.State != "pinned" || row.Attempts != 1 || !row.InvokedAt.Valid ||
		row.LiveRunID != prerequisiteID(f.m.row.RunID) || len(commands) != 1 ||
		commands[0] != [3]string{strings.Repeat("0", 40), prerequisiteTestTip, row.PinRef} ||
		!tx.committed.Load() || tx.s2PID.Load() != s2PID || remote.ioInTX.Load() ||
		remote.sendBeforeCommit.Load() || tx.orderViolation.Load() {
		t.Fatalf("S2 state=%s attempts=%d commands=%d ownTxIO=%t earlySend=%t order=%t",
			row.State, row.Attempts, len(commands), remote.ioInTX.Load(), remote.sendBeforeCommit.Load(), tx.orderViolation.Load())
	}
	// This tests the recorded AVAILABLE state boundary with released custody.
	// It deliberately does not run the recovery expiry sweeper; the seeded 24h
	// expiry coincides with AuditNextAt and is not an expiry-sweep claim.
	f.advanceLiveDB(t, row.AuditNextAt.Time.Sub(f.m.now))
	fresh := m3Fresh(f)
	beforeRequests, _ := remote.snapshot()
	m3Sweep(t, ctx, fresh)
	after := f.readLiveDB(t)
	afterRequests, afterCommands := remote.snapshot()
	m3AssertFrozen(t, row, after)
	if after.State != "pinned" || after.CleanupAttempts != 0 || afterRequests <= beforeRequests || len(afterCommands) != 1 ||
		!after.AuditNextAt.Time.After(f.m.now) {
		t.Fatal("AVAILABLE audit failed to retain pin without cleanup")
	}
	var state, holdState string
	if e := f.env.pool.QueryRow(ctx, "SELECT c.state,h.state FROM recovery_captures c JOIN recovery_custody_holds h ON h.id=c.hold_id WHERE c.id=$1", row.CaptureID).Scan(&state, &holdState); e != nil || state != "available" || holdState != "released" {
		t.Fatalf("audit state boundary: capture=%s hold=%s err=%v", state, holdState, e)
	}
}

// Compile-time seam checks are declarations, not runtime proof.
var _ PrerequisiteStore = (*m3TxQueries)(nil)
