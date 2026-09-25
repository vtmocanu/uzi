package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// BeginClaimFinish makes fakeStore a transactional claimFinishBeginner (PRD #1590 M1), so a
// fake-store svc.Claim drives the SAME exact-claim finish logic production runs on the pgx
// transaction. Writes are staged and applied only on Commit, so a rolled-back outcome records
// nothing. The fake models a Claude run (no Codex binding); Codex classification is covered
// by the LiveDB suite.
func (f *fakeStore) BeginClaimFinish(context.Context) (claimFinishTx, error) {
	f.claimFinishBegins++
	return &fakeClaimFinishTx{f: f}, nil
}

type fakeClaimFinishTx struct {
	f       *fakeStore
	pending []func()
}

func (t *fakeClaimFinishTx) GetRunOwnedByWorkerForUpdate(_ context.Context, arg store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error) {
	if len(t.f.claimFinishLockErrs) > 0 {
		err := t.f.claimFinishLockErrs[0]
		t.f.claimFinishLockErrs = t.f.claimFinishLockErrs[1:]
		if err != nil {
			return store.Run{}, err
		}
	}
	if t.f.claimFinishLocked != nil {
		return *t.f.claimFinishLocked, nil
	}
	// ClaimRun's RETURNING row as the claimant left it: status claimed, worker_id the claimant.
	r := t.f.claimRun
	r.ID, r.Status, r.WorkerID = arg.ID, "claimed", arg.WorkerID
	return r, nil
}

func (t *fakeClaimFinishTx) LockCodexAliasForShareNowait(context.Context, store.LockCodexAliasForShareNowaitParams) (store.LockCodexAliasForShareNowaitRow, error) {
	return store.LockCodexAliasForShareNowaitRow{}, pgx.ErrNoRows
}

func (t *fakeClaimFinishTx) LockCodexAccountForShareNowait(context.Context, store.LockCodexAccountForShareNowaitParams) (uuid.UUID, error) {
	return uuid.Nil, pgx.ErrNoRows
}

func (t *fakeClaimFinishTx) GetRunCodexAuthContext(context.Context, uuid.UUID) (store.GetRunCodexAuthContextRow, error) {
	return store.GetRunCodexAuthContextRow{}, pgx.ErrNoRows
}

func (t *fakeClaimFinishTx) LockOpenCustodyHoldsForRunWorkerGeneration(context.Context, store.LockOpenCustodyHoldsForRunWorkerGenerationParams) ([]uuid.UUID, error) {
	return t.f.claimFinishHolds, nil
}

func (t *fakeClaimFinishTx) ReleaseCustodyHoldExact(context.Context, store.ReleaseCustodyHoldExactParams) (int64, error) {
	n := int64(1)
	if t.f.claimReleaseRows != nil {
		n = *t.f.claimReleaseRows
	}
	if n > 0 {
		t.pending = append(t.pending, func() { t.f.claimReleased += int(n) })
	}
	return n, nil
}

func (t *fakeClaimFinishTx) ParkRunCodexAccountUnavailable(_ context.Context, arg store.ParkRunCodexAccountUnavailableParams) (store.Run, error) {
	t.pending = append(t.pending, func() { t.f.claimParked = &arg })
	return store.Run{ID: arg.ID, Status: "recovery_wait"}, nil
}

func (t *fakeClaimFinishTx) RequeueClaimAssemblyExact(_ context.Context, arg store.RequeueClaimAssemblyExactParams) (int64, error) {
	t.pending = append(t.pending, func() { t.f.claimRequeued = &arg })
	return 1, nil
}

func (t *fakeClaimFinishTx) FailClaimAssemblyExact(_ context.Context, arg store.FailClaimAssemblyExactParams) (int64, error) {
	t.pending = append(t.pending, func() { t.f.claimFailed = &arg })
	return 1, nil
}

func (t *fakeClaimFinishTx) Commit(context.Context) error {
	for _, apply := range t.pending {
		apply()
	}
	t.pending = nil
	return nil
}

func (t *fakeClaimFinishTx) Rollback(context.Context) error {
	t.pending = nil
	return nil
}

// plainClaimStore hides fakeStore's BeginClaimFinish: only the Store interface's methods are
// promoted, so a Service over it has neither a pgx transaction nor a transactional store.
type plainClaimStore struct{ Store }

func finishFixture(t *testing.T) (*fakeStore, *Service, store.Run, claimRecoveryIdentity) {
	t.Helper()
	fs := &fakeStore{}
	fs.claimRun = store.Run{ID: uuid.New(), Kind: "issue", ClaimGeneration: 3}
	svc := New(fs, nil, testParams())
	return fs, svc, fs.claimRun, claimRecoveryIdentity{workerID: uuid.New()}
}

// TestFinishRunClaimRefusesWithoutTransaction (PRD #1590 R1): with no pgx transaction and no
// transactional store there is no fenced writer, so finishRunClaim returns an error with no
// payload for a success AND for every assembly outcome, and never reaches the unfenced
// MarkRunFailedByID / RequeueClaimedRunToQueued writers.
func TestFinishRunClaimRefusesWithoutTransaction(t *testing.T) {
	for _, assemblyErr := range []error{nil, errCredentialUnavailable, errToolPackagesRejected,
		errGuardrailBlockedClaim, errVaultLocked, errAutoPoolEmpty, errCustomModelCapabilityMissing} {
		fs, _, run, id := finishFixture(t)
		svc := New(plainClaimStore{fs}, nil, testParams())
		payload, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{RunID: run.ID.String()}, assemblyErr, id)
		if payload != nil || !errors.Is(err, errClaimRecoveryNoTx) {
			t.Fatalf("assembly %v: finishRunClaim = (%v, %v), want (nil, errClaimRecoveryNoTx)", assemblyErr, payload, err)
		}
		if assemblyErr != nil && !errors.Is(err, assemblyErr) {
			t.Fatalf("assembly %v: refusal lost the assembly error: %v", assemblyErr, err)
		}
		if fs.markedFailed != nil || fs.requeuedRun != nil ||
			fs.claimFailed != nil || fs.claimRequeued != nil {
			t.Fatalf("assembly %v: a refused finish wrote the run", assemblyErr)
		}
	}
}

// TestFinishRunClaimCustodyMismatchRollsBack (PRD #1590 D2): an observed hold count other than
// the claim-time expectation rolls the whole transaction back and reports
// errClaimRecoveryCustody with no payload: no release, no fail, no requeue.
func TestFinishRunClaimCustodyMismatchRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capable bool
		holds   int
	}{{"expected 0 actual 1", false, 1}, {"expected 1 actual 0", true, 0}, {"expected 1 actual 2", true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			fs, svc, run, id := finishFixture(t)
			id.recoveryCapable = tc.capable
			for range tc.holds {
				fs.claimFinishHolds = append(fs.claimFinishHolds, uuid.New())
			}
			payload, err := svc.finishRunClaim(context.Background(), run, nil, errCredentialUnavailable, id)
			if payload != nil || !errors.Is(err, errClaimRecoveryCustody) {
				t.Fatalf("finishRunClaim = (%v, %v), want (nil, errClaimRecoveryCustody)", payload, err)
			}
			if fs.claimFailed != nil || fs.claimReleased != 0 || fs.markedFailed != nil {
				t.Fatalf("custody mismatch committed a write: failed=%v released=%d", fs.claimFailed, fs.claimReleased)
			}
		})
	}
}

// TestFinishRunClaimLostClaimIsIdle: a lock that no longer shows this exact claim (another
// status, generation or worker) is idle with nothing written, for a success or a failure.
func TestFinishRunClaimLostClaimIsIdle(t *testing.T) {
	for _, mutate := range []func(*store.Run){
		func(r *store.Run) { r.Status = "cancelled" },
		func(r *store.Run) { r.ClaimGeneration++ },
		func(r *store.Run) { r.WorkerID = pgconv.UUID(uuid.New()) },
	} {
		fs, svc, run, id := finishFixture(t)
		locked := run
		locked.Status, locked.WorkerID = "claimed", pgconv.UUID(id.workerID)
		mutate(&locked)
		fs.claimFinishLocked = &locked
		for _, assemblyErr := range []error{nil, errCredentialUnavailable, errVaultLocked} {
			payload, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{}, assemblyErr, id)
			if payload != nil || err != nil {
				t.Fatalf("lost claim (%v) = (%v, %v), want idle", assemblyErr, payload, err)
			}
		}
		if fs.claimFailed != nil || fs.claimRequeued != nil || fs.claimReleased != 0 {
			t.Fatal("a lost claim was written")
		}
	}
}

// TestFinishRunClaimRetriesLockNotAvailable (PRD #1590 N4): a 55P03 from the NOWAIT locks
// retries the whole transaction a bounded number of times; past the budget the claim returns
// the error with no payload. Any other error is not retried.
func TestFinishRunClaimRetriesLockNotAvailable(t *testing.T) {
	defer func(d time.Duration) { finishRunClaimRetryDelay = d }(finishRunClaimRetryDelay)
	finishRunClaimRetryDelay = time.Millisecond
	busy := &pgconn.PgError{Code: "55P03"}

	fs, svc, run, id := finishFixture(t)
	fs.claimFinishLockErrs = []error{busy, busy}
	want := &ClaimPayload{RunID: run.ID.String()}
	if got, err := svc.finishRunClaim(context.Background(), run, want, nil, id); err != nil || got != want {
		t.Fatalf("two busy attempts then free = (%v, %v), want the payload", got, err)
	}
	if fs.claimFinishBegins != finishRunClaimAttempts {
		t.Fatalf("attempts = %d, want %d", fs.claimFinishBegins, finishRunClaimAttempts)
	}

	fs, svc, run, id = finishFixture(t)
	fs.claimFinishLockErrs = []error{busy, busy, busy, busy}
	if got, err := svc.finishRunClaim(context.Background(), run, want, errCredentialUnavailable, id); got != nil || !isLockNotAvailable(err) {
		t.Fatalf("always busy = (%v, %v), want (nil, 55P03)", got, err)
	}
	if fs.claimFinishBegins != finishRunClaimAttempts || fs.claimFailed != nil {
		t.Fatalf("attempts = %d failed=%v, want %d attempts and no write", fs.claimFinishBegins, fs.claimFailed, finishRunClaimAttempts)
	}

	fs, svc, run, id = finishFixture(t)
	fs.claimFinishLockErrs = []error{&pgconn.PgError{Code: "23505"}}
	if got, err := svc.finishRunClaim(context.Background(), run, want, nil, id); got != nil || err == nil {
		t.Fatalf("non-retryable = (%v, %v), want an error", got, err)
	}
	if fs.claimFinishBegins != 1 {
		t.Fatalf("a non-55P03 error was retried: %d attempts", fs.claimFinishBegins)
	}
}

// TestFinishRunClaimTransientOutcomes: the three transient assembly errors settle through the
// fenced requeue, pool_wait only for the empty auto pool, and never fail the run.
func TestFinishRunClaimTransientOutcomes(t *testing.T) {
	for _, tc := range []struct {
		err      error
		poolWait bool
	}{{errVaultLocked, false}, {errCustomModelCapabilityMissing, false}, {errAutoPoolEmpty, true}} {
		fs, svc, run, id := finishFixture(t)
		if got, err := svc.finishRunClaim(context.Background(), run, nil, tc.err, id); got != nil || err != nil {
			t.Fatalf("%v: finishRunClaim = (%v, %v), want idle", tc.err, got, err)
		}
		if fs.claimRequeued == nil || fs.claimRequeued.PoolWait != tc.poolWait || fs.claimRequeued.ID != run.ID ||
			fs.claimRequeued.ClaimGeneration != run.ClaimGeneration || fs.claimRequeued.WorkerID != pgconv.UUID(id.workerID) {
			t.Fatalf("%v: requeue = %+v, want exact-claim pool_wait=%v", tc.err, fs.claimRequeued, tc.poolWait)
		}
		if fs.claimFailed != nil || fs.markedFailed != nil || fs.requeuedRun != nil {
			t.Fatalf("%v: a transient outcome failed the run or used an unfenced writer", tc.err)
		}
	}
}

// TestFinishRunClaimReleaseRowCountMismatchRollsBack (PRD #1590 D2): the hold lock saw exactly
// the one expected hold, but the exact release affected 0 (or more than 1) rows. The whole
// transaction rolls back with errClaimRecoveryCustody and no payload: no committed release and
// no fail, requeue or park, for a terminal and for a transient outcome alike.
func TestFinishRunClaimReleaseRowCountMismatchRollsBack(t *testing.T) {
	for _, rows := range []int64{0, 2} {
		for _, assemblyErr := range []error{errCredentialUnavailable, errVaultLocked} {
			fs, svc, run, id := finishFixture(t)
			id.recoveryCapable = true
			fs.claimFinishHolds = []uuid.UUID{uuid.New()}
			fs.claimReleaseRows = &rows
			payload, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{RunID: run.ID.String()}, assemblyErr, id)
			if payload != nil || !errors.Is(err, errClaimRecoveryCustody) {
				t.Fatalf("release rows=%d, %v: finishRunClaim = (%v, %v), want (nil, errClaimRecoveryCustody)", rows, assemblyErr, payload, err)
			}
			if fs.claimReleased != 0 || fs.claimFailed != nil || fs.claimRequeued != nil || fs.claimParked != nil ||
				fs.markedFailed != nil || fs.requeuedRun != nil {
				t.Fatalf("release rows=%d, %v: a mismatched release committed a write: released=%d failed=%v requeued=%v",
					rows, assemblyErr, fs.claimReleased, fs.claimFailed, fs.claimRequeued)
			}
		}
	}
}

// TestFinishRunClaimRetryHonoursCancel (PRD #1590 N4): a caller whose context ends during the
// 55P03 retry wait gets the context's error, not the stale 55P03, and nothing is written.
func TestFinishRunClaimRetryHonoursCancel(t *testing.T) {
	defer func(d time.Duration) { finishRunClaimRetryDelay = d }(finishRunClaimRetryDelay)
	finishRunClaimRetryDelay = time.Hour
	fs, svc, run, id := finishFixture(t)
	fs.claimFinishLockErrs = []error{&pgconn.PgError{Code: "55P03"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload, err := svc.finishRunClaim(ctx, run, nil, errCredentialUnavailable, id)
	if payload != nil || !errors.Is(err, context.Canceled) || isLockNotAvailable(err) {
		t.Fatalf("cancelled retry = (%v, %v), want (nil, context.Canceled) without the 55P03", payload, err)
	}
	if fs.claimFinishBegins != 1 || fs.claimFailed != nil {
		t.Fatalf("attempts=%d failed=%v, want one attempt and no write", fs.claimFinishBegins, fs.claimFailed)
	}
}
