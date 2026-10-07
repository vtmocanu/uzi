package poller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #2347: the watcher's decision rests on a ledger read taken before it lists the review
// comments. These tests move the ledger during the listing and check, against a real Postgres
// and the real workersvc create, that the create re-validates under its lock: another cycle that
// consumed the comments or spent the cap is not duplicated or overrun.

type mrwLive struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	svc    *workersvc.Service
	owner  uuid.UUID
	repo   uuid.UUID
	source uuid.UUID
}

func newMRWLive(t *testing.T) *mrwLive {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set a throwaway database: UZI_TEST_DATABASE_URL=<throwaway DSN> go test -buildvcs=false -race -count=1 -p 1 -v -run 'TestMRRework.*LiveDB$' ./internal/poller")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	l := &mrwLive{ctx: ctx, pool: pool, q: store.New(pool), owner: uuid.New(), repo: uuid.New(), source: uuid.New()}
	l.svc = workersvc.New(l.q, nil, workersvc.Params{})
	l.svc.SetTxBeginner(pool)
	conn := uuid.New()
	l.exec(t, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", l.owner, fmt.Sprintf("%s@2347.test", l.owner))
	l.exec(t, "INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://forge.test','bot',$3,$4)", conn, l.owner, mrwBotID, []byte{1})
	l.exec(t, "INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES ($1,$2,42,$3,'https://forge.test/g/r','main',true)", l.repo, conn, l.repo.String())
	l.exec(t, "INSERT INTO user_secrets (user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,'anthropic_token','default',true,$2,'master')", l.owner, []byte{2})
	l.exec(t, "INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,status,created_at) VALUES ($1,$2,$3,'issue',7,'t','d',$4,$5,'opened','completed','2020-01-01')", l.source, l.owner, l.repo, mrwRef, mrwMrIID)
	l.exec(t, "INSERT INTO pipeline_statuses (repo_id,ref,pipeline_id,sha,status,web_url,synced_at) VALUES ($1,$2,7001,$3,'success','https://forge.test/p','2020-01-01')", l.repo, mrwRef, mrwHeadSHA)
	return l
}

func (l *mrwLive) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := l.pool.Exec(l.ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// seedLedger writes the ledger as an earlier consumed cycle left it.
func (l *mrwLive) seedLedger(t *testing.T, attempts int, highWater int64) {
	t.Helper()
	if err := l.q.UpsertMRReworkLedger(l.ctx, store.UpsertMRReworkLedgerParams{RepoID: l.repo, Ref: mrwRef, HighWater: highWater}); err != nil {
		t.Fatal(err)
	}
	l.exec(t, "UPDATE mr_rework_ledger SET attempt_count=$3 WHERE repo_id=$1 AND ref=$2", l.repo, mrwRef, attempts)
}

func (l *mrwLive) ledger(t *testing.T) store.MrReworkLedger {
	t.Helper()
	got, err := l.q.GetMRReworkLedger(l.ctx, store.GetMRReworkLedgerParams{RepoID: l.repo, Ref: mrwRef})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (l *mrwLive) reworkRuns(t *testing.T) int {
	t.Helper()
	var n int
	if err := l.pool.QueryRow(l.ctx, "SELECT count(*) FROM runs WHERE kind='mr_rework' AND repo_id=$1", l.repo).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (l *mrwLive) detect(capVal int, f *mrwForge) {
	row := mrwRepoRow()
	row.ID = l.repo
	NewMRReviewWatch(l.q, l.svc, poolQueue{l.pool}, nil, mrwSettings{enabled: true, capVal: capVal}, capVal, 0).detect(l.ctx, row, f)
}

// A manual rework that consumes the comment and completes while the watcher is listing: the
// terminal run no longer blocks the watcher's create, so only the recheck stops the duplicate.
func TestMRReworkAutoDoesNotReconsumeACompletedManualCycleLiveDB(t *testing.T) {
	l := newMRWLive(t)
	l.seedLedger(t, 1, 150)
	f := landedForge(mrwComment(190, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), mrwHeadSHA))
	f.onList = func() {
		f.onList = nil
		snap := &workersvc.ReviewCommentsSnapshot{Comments: []workersvc.ReviewCommentSnapshot{{ID: 190, AuthorUsername: "human", Body: "please tighten this", ReviewState: "inline"}}}
		if _, err := l.svc.CreateManualMRReworkRun(l.ctx, l.owner, l.repo, mrwRef, mrwMrIID, l.source, "t", "d", snap, workersvc.ReviewPlan{MaxActionableID: 190}, nil); err != nil {
			t.Error(err)
			return
		}
		l.exec(t, "UPDATE runs SET status='completed' WHERE kind='mr_rework' AND repo_id=$1", l.repo)
	}

	l.detect(5, f)

	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d, want 1 (the manual one): the watcher must not rework the consumed comment", n)
	}
	if got := l.ledger(t); got.AttemptCount != 1 || got.HighWater != 190 {
		t.Fatalf("ledger = %+v, want attempt_count 1 (manual advance does not count) and high_water 190", got)
	}
}

// A concurrent cycle spends the last attempt while the watcher is listing, so the watcher's
// pre-listing count (1 of 2) is stale. A newer eligible comment is still not reworked past the cap.
func TestMRReworkAutoRespectsACapSpentDuringTheListingLiveDB(t *testing.T) {
	l := newMRWLive(t)
	l.seedLedger(t, 1, 150)
	f := landedForge(
		mrwComment(190, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), mrwHeadSHA),
		mrwComment(200, time.Date(2020, 1, 1, 0, 0, 1, 0, time.UTC), mrwHeadSHA),
	)
	f.onList = func() {
		f.onList = nil
		// The other cycle: its run came and went, and it advanced the count to the cap.
		l.exec(t, "UPDATE mr_rework_ledger SET attempt_count=2, high_water=190 WHERE repo_id=$1 AND ref=$2", l.repo, mrwRef)
	}

	l.detect(2, f)

	if n := l.reworkRuns(t); n != 0 {
		t.Fatalf("mr_rework runs = %d, want 0: the cap was already spent", n)
	}
	if got := l.ledger(t); got.AttemptCount != 2 || got.HighWater != 190 {
		t.Fatalf("ledger = %+v, want attempt_count 2 and high_water 190 unchanged", got)
	}
}
