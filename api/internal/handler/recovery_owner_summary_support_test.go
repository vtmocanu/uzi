package handler

// This support and recovery_owner_summary_blackbox_livedb_test.go intentionally
// use only baseline APIs. Copy both to the baseline for the missing-metadata proof.
import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type ownerSummaryFixture struct {
	h                                 *Handler
	router                            http.Handler
	pool                              *pgxpool.Pool
	owner, run, hold, worker, capture uuid.UUID
}

func newOwnerSummaryFixture(t *testing.T) *ownerSummaryFixture {
	t.Helper()
	ephemeralPriorityDatabase(t)
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	cliMustExec(t, pool, "UPDATE forge_connections SET forge_type='github',base_url='https://github.com' WHERE id=$1", conn)
	worker := custodySeedWorker(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 3728, true)
	run := rmSeedRun(t, pool, owner, repo, "failed")
	hold := ownerSummarySeedHold(t, pool, owner, run, worker)
	f := &ownerSummaryFixture{h: h, router: router, pool: pool, owner: owner, run: run, hold: hold, worker: worker}
	f.capture = f.seedCapture(t, true)
	if _, err := store.New(pool).InsertPrerequisiteClosure(context.Background(), f.capture); err != nil {
		t.Fatal(err)
	}
	return f
}

// Guarded provenance is fixed at insertion; the hold guard rejects later changes.
func ownerSummarySeedHold(t *testing.T, pool *pgxpool.Pool, owner, run, worker uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds
	(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
	SELECT $1,$2,r.repo_id,r.id,1,'open',$4,'cw-ident',$4,r.id,true FROM runs r WHERE r.id=$3`,
		id, owner, run, worker)
	return id
}

func (f *ownerSummaryFixture) seedCapture(t *testing.T, thin bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	prereqs := []string{}
	if thin {
		prereqs = []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	}
	cliMustExec(t, f.pool, `INSERT INTO recovery_captures
	(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,prerequisite_shas,expires_at)
	VALUES ($1,$2,$3,$4,$5,'cw-ident','aaaa1111',$6,'available',true,3,repeat('a',64),$7,now()+interval '7 days')`,
		id, f.hold, f.run, f.owner, f.worker, id.String(), prereqs)
	return id
}
