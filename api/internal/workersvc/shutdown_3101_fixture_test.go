package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Shutdown3101Fixture exposes existing live fixtures to the external-package HTTP
// tests, which can import handler without introducing a workersvc import cycle.
type Shutdown3101Fixture struct {
	Service                                      *Service
	Queries                                      *store.Queries
	Pool                                         *pgxpool.Pool
	Box                                          *secretbox.Box
	RunID                                        uuid.UUID
	Token, Capability, AccessToken, RefreshToken string
	ProviderUserID, AccountID                    string
	Generation                                   int64
	Mutate                                       func(string)
	DuringRelease                                func(string)
	DuringPublish                                func(string)
	AssertCommitted                              func(string)
}

func shutdown3101Bearer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, worker uuid.UUID) string {
	t.Helper()
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE workers SET token_hash=$2 WHERE id=$1", worker, hash); err != nil {
		t.Fatal(err)
	}
	return token
}

func NewShutdown3101CodexFixture(t *testing.T, apiKey bool) Shutdown3101Fixture {
	t.Helper()
	e := setupCodexLiveDB(t)
	var f refreshFixture
	if apiKey {
		key := newAPIKeyFixture(t, e)
		f = refreshFixture{userID: key.userID, workerID: key.workerID, runID: key.runID,
			svc: key.svc, wkr: key.wkr, accessToken: key.accessToken}
	} else {
		f = newRefreshFixture(t, e, &fakeRefreshClient{})
	}
	e.exec("UPDATE runs SET status='running' WHERE id=$1", f.runID)
	f.svc.SetTxBeginner(e.pool)
	capw := e.mintCap(t, f.runID, f.workerID)
	mutate := func(kind string) {
		switch kind {
		case "queued":
			e.exec("UPDATE runs SET stop_kind='cancelled' WHERE id=$1", f.runID)
		case "terminal":
			e.exec("UPDATE runs SET status='cancelled',finished_at=now() WHERE id=$1", f.runID)
		case "epoch":
			e.exec("UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1 WHERE id=$1", f.runID)
		case "released":
			e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID)
		case "worker":
			e.exec("UPDATE runs SET worker_id=NULL WHERE id=$1", f.runID)
		default:
			t.Fatalf("unknown mutation %q", kind)
		}
	}
	return Shutdown3101Fixture{
		Service: f.svc, Queries: e.q, Pool: e.pool, Box: e.box, RunID: f.runID,
		Token: shutdown3101Bearer(t, e.ctx, e.pool, f.workerID), Capability: capw,
		AccessToken: f.accessToken, RefreshToken: f.prevRefreshToken, ProviderUserID: f.providerUserID, AccountID: f.workspaceAcctID,
		Mutate: mutate,
		DuringRelease: func(kind string) {
			fired := false
			hook := func() {
				if !fired {
					fired = true
					mutate(kind)
				}
			}
			if apiKey {
				f.svc.q = shutdown3101KeyReadStore{Queries: e.q, afterRead: hook}
			} else {
				f.svc.q = releaseHookStore{Queries: e.q, onAccountRead: hook}
			}
		},
		AssertCommitted: func(want string) {
			acct := e.mustAccount(t, f.userID, f.accountID)
			if acct.Generation != 1 || e.accountBlob(t, f.userID, f.accountID).AccessToken != want {
				t.Fatal("rotation must remain durably committed despite this run's release refusal")
			}
		},
	}
}

// shutdown3101KeyReadStore observes the real encrypted key read, then changes
// authority before the public ReleaseCodexCredential recheck.
type shutdown3101KeyReadStore struct {
	*store.Queries
	afterRead func()
}

func (q shutdown3101KeyReadStore) GetUserSecretCiphertextByID(ctx context.Context, p store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	row, err := q.Queries.GetUserSecretCiphertextByID(ctx, p)
	if err == nil {
		q.afterRead()
	}
	return row, err
}

// shutdown3101PreparedStore changes real authority after unsent evidence has been
// inserted. RecordLiveCheckpointPublishAttempt's second call must recheck the DB.
type shutdown3101PreparedStore struct {
	*store.Queries
	afterPrepare func()
}

func (q *shutdown3101PreparedStore) RecordLiveCheckpointPublishAttempt(ctx context.Context, p store.RecordLiveCheckpointPublishAttemptParams) (uuid.UUID, error) {
	id, err := q.Queries.RecordLiveCheckpointPublishAttempt(ctx, p)
	if err == nil && q.afterPrepare != nil {
		fn := q.afterPrepare
		q.afterPrepare = nil
		fn()
	}
	return id, err
}

func NewShutdown3101CheckpointFixture(t *testing.T) Shutdown3101Fixture {
	t.Helper()
	f := newLiveAttemptFix(t)
	r, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatal(err)
	}
	// The real origin starts with main only; no fabricated checkpoint tip is owned.
	mutate := func(kind string) {
		switch kind {
		case "running":
			// Positive control: the same live admission and recheck keep authority.
		case "terminal":
			f.e.exec(t, "UPDATE runs SET status='cancelled',finished_at=now() WHERE id=$1", f.oldRun)
		case "generation":
			f.e.exec(t, "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", f.oldRun)
		case "released":
			f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.oldRun)
		case "worker":
			f.e.exec(t, "UPDATE runs SET worker_id=$2 WHERE id=$1", f.oldRun, f.newWorker)
		default:
			t.Fatalf("unknown mutation %q", kind)
		}
	}
	return Shutdown3101Fixture{
		Service: f.svc1, Queries: f.e.q, Pool: f.e.pool, Box: f.svc1.box, RunID: f.oldRun, Generation: r.ClaimGeneration,
		Token:  shutdown3101Bearer(t, f.e.ctx, f.e.pool, uuid.UUID(r.WorkerID.Bytes)),
		Mutate: mutate,
		DuringPublish: func(kind string) {
			f.svc1.q = &shutdown3101PreparedStore{Queries: f.e.q, afterPrepare: func() { mutate(kind) }}
		},
	}
}
