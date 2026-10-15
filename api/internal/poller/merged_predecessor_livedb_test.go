package poller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type mergedPollForge struct {
	forgetest.BaseFake
	syncDeadline bool
	list         func(context.Context, int64) ([]forge.Issue, error)
	reads        int
	read         func(context.Context, int64, int64) (forge.MergeRequest, error)
}

func (f *mergedPollForge) ListIssues(ctx context.Context, project int64, _ forge.ListIssuesOptions) ([]forge.Issue, error) {
	if f.list != nil {
		return f.list(ctx, project)
	}
	if f.syncDeadline {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("issue sync unavailable")
}
func (f *mergedPollForge) GetMergeRequest(ctx context.Context, p, i int64) (forge.MergeRequest, error) {
	f.reads++
	return f.read(ctx, p, i)
}

type mergedRepoStore struct {
	q    *store.Queries
	repo uuid.UUID
}

func (r mergedRepoStore) ListEnabledReposWithConnections(ctx context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	rows, err := r.q.ListEnabledReposWithConnections(ctx)
	var result []store.ListEnabledReposWithConnectionsRow
	for _, row := range rows {
		if row.ID == r.repo {
			result = append(result, row)
		}
	}
	return result, err
}

type mergedPollEnv struct {
	t                     *testing.T
	ctx                   context.Context
	pool                  *pgxpool.Pool
	q                     *store.Queries
	svc                   *forgesvc.Service
	recovery              *recovery.Service
	engine                *Engine
	forge                 *mergedPollForge
	worker                store.Worker
	run, repo, conn, hold uuid.UUID
}

func newMergedPollEnv(t *testing.T) *mergedPollEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run e2e/run-store-it.sh")
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
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	e := &mergedPollEnv{t: t, ctx: ctx, pool: pool, q: store.New(pool), run: uuid.New(), repo: uuid.New(), conn: uuid.New(), hold: uuid.New()}
	e.worker = store.Worker{ID: uuid.New(), UserID: uuid.New(), Name: "merged-poll", ProtocolCapabilities: []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1}}
	e.forge = &mergedPollForge{read: func(ctx context.Context, p, i int64) (forge.MergeRequest, error) {
		if ctx.Err() != nil || p != 42 || i != 7 {
			t.Errorf("exact fresh observation: project=%d iid=%d err=%v", p, i, ctx.Err())
		}
		return forge.MergeRequest{IID: i, State: forge.MRStateMerged}, nil
	}}
	e.svc = forgesvc.NewWithForgeBuilder(e.q, box, time.Second, nil, func(kind forge.Type, url, token string, timeout time.Duration) (forge.Forge, error) {
		if kind != forge.TypeGitLab || url != "https://forge.test" || token != "fresh fixture" || timeout != time.Second {
			t.Errorf("connection snapshot %s %s %s %v", kind, url, token, timeout)
		}
		return e.forge, nil
	})
	sealed, err := e.svc.EncryptToken("fresh fixture")
	if err != nil {
		t.Fatal(err)
	}
	e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", e.worker.UserID, fmt.Sprintf("%s@merged.test", e.worker.UserID))
	e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", e.worker.ID, e.worker.UserID, e.worker.Name, e.worker.ID[:])
	e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge.test','bot',1,$3)", e.conn, e.worker.UserID, sealed)
	e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES($1,$2,42,$3,'https://forge.test/g/r',true)", e.repo, e.conn, e.repo.String())
	e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,status,worker_id,claim_generation,mr_iid,mr_state,created_at) VALUES($1,$2,$3,'prompt','t','d','running',$4,1,7,'merged','2020-01-01')", e.run, e.worker.UserID, e.repo, e.worker.ID)
	e.addHold(e.hold, e.run, e.worker.ID, 1)
	e.recovery = recovery.New(e.q, pool, box, recovery.Limits{MaxBundleBytes: 1 << 20, MaxCapturesPerClaim: 8, MaxCapturesPerOwner: 32, ReadyRetention: time.Hour, RequestDeadline: 10 * time.Second}, nil)
	e.engine = New(e.svc, mergedRepoStore{e.q, e.repo}, time.Minute, 10)
	return e
}
func (e *mergedPollEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}
func (e *mergedPollEnv) addHold(id, run, worker uuid.UUID, gen int64) {
	e.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state) VALUES($1,$2,$3,$4,$5,$6,'original',$6,$4,true,'open')", id, e.worker.UserID, e.repo, run, gen, worker)
}
func (e *mergedPollEnv) state(id uuid.UUID) string {
	e.t.Helper()
	var state string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", id).Scan(&state); err != nil {
		e.t.Fatal(err)
	}
	return state
}
func (e *mergedPollEnv) reserve(key string) uuid.UUID {
	e.t.Helper()
	gen := int64(1)
	r, err := e.recovery.Reserve(e.ctx, e.worker, e.run, apitypes.RecoveryReserveRequest{Generation: &gen, IdempotencyKey: key, SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)})
	if err != nil {
		e.t.Fatal(err)
	}
	return uuid.MustParse(r.CaptureID)
}

func TestMergedPredecessorAutomaticPollingLiveDB(t *testing.T) {
	for _, sameWorker := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_worker_%v", sameWorker), func(t *testing.T) {
			e := newMergedPollEnv(t)
			ready := e.reserve("thin")
			body := []byte("thin nonancestor archive bytes")
			sum := sha256.Sum256(body)
			manifest := apitypes.RecoveryUploadManifest{ByteSize: int64(len(body)), Checksum: hex.EncodeToString(sum[:]), ChunkCount: 1, PrerequisiteShas: []string{strings.Repeat("b", 40)}}
			if _, err := e.recovery.Upload(e.ctx, e.worker, e.run, ready, manifest, bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			before, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: ready, RunID: e.run, UserID: e.worker.UserID})
			if err != nil {
				t.Fatal(err)
			}
			var partial []uuid.UUID
			for _, state := range []string{"preparing", "uploading", "needs_action"} {
				id := e.reserve(state)
				partial = append(partial, id)
				e.exec("UPDATE recovery_captures SET state=$2 WHERE id=$1", id, state)
				e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", id, []byte("partial"))
			}
			completedWorker := e.worker.ID
			if !sameWorker {
				completedWorker = uuid.New()
				e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'completed',$3,'online')", completedWorker, e.worker.UserID, completedWorker[:])
			}
			current, later, otherHold, otherRun := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			e.addHold(current, e.run, completedWorker, 2)
			e.addHold(later, e.run, completedWorker, 3)
			e.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,status,claim_generation) VALUES($1,$2,$3,'prompt','t','d','running',2)", otherRun, e.worker.UserID, e.repo)
			e.addHold(otherHold, otherRun, e.worker.ID, 1)
			e.exec("UPDATE runs SET mr_iid=7,mr_state='merged' WHERE id=$1", otherRun) // A shared recorded PR grants no cross-run authority.
			e.exec("UPDATE runs SET status='completed',claim_generation=2,worker_id=$2 WHERE id=$1", e.run, completedWorker)
			aggregateBefore, err := e.recovery.ListHoldsForOwner(e.ctx, e.worker.UserID, true)
			if err != nil {
				t.Fatal(err)
			}
			hotBefore, err := e.q.CountOpenCustodyHoldsForRepo(e.ctx, e.repo)
			if err != nil {
				t.Fatal(err)
			}
			// Central regression control: real PollOnce without its automatic collaborator.
			e.engine.PollOnce(e.ctx)
			if e.state(e.hold) != "open" || e.forge.reads != 0 {
				t.Fatal("disabled collaborator changed G1")
			}
			hooks := 0
			e.recovery.SetCustodySettledHook(func(id uuid.UUID) {
				hooks++
				if id != e.run || e.state(e.hold) != "discarded" {
					t.Error("hook before commit/wrong run")
				}
			})
			e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
			e.engine.PollOnce(e.ctx)
			if e.state(e.hold) != "discarded" || e.forge.reads != 1 || hooks != 1 {
				t.Fatalf("automatic reconciliation state=%s reads=%d hooks=%d", e.state(e.hold), e.forge.reads, hooks)
			}
			for _, id := range []uuid.UUID{current, later, otherHold} {
				if e.state(id) != "open" {
					t.Fatal("unrelated hold changed")
				}
			}
			after, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: ready, RunID: e.run, UserID: e.worker.UserID})
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("available thin archive metadata/TTL changed")
			}
			rr := httptest.NewRecorder()
			if err := e.recovery.Download(e.ctx, rr, e.worker.UserID, e.run, ready); err != nil || !bytes.Equal(rr.Body.Bytes(), body) {
				t.Fatalf("archive bytes: %v", err)
			}
			for _, id := range partial {
				capture, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.worker.UserID})
				chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, id)
				if err != nil || chunkErr != nil || capture.State != "discarded" || len(chunks) != 0 {
					t.Fatal("nonready capture not cleared")
				}
			}
			hotAfter, err := e.q.CountOpenCustodyHoldsForRepo(e.ctx, e.repo)
			if err != nil || hotAfter != hotBefore-1 {
				t.Fatal("hot count not reduced")
			}
			list, err := e.recovery.ListHoldsForOwner(e.ctx, e.worker.UserID, true)
			if err != nil {
				t.Fatal(err)
			}
			if list.Aggregate.OpenHolds != aggregateBefore.Aggregate.OpenHolds-1 || list.Aggregate.AdmissionCountedHolds != aggregateBefore.Aggregate.AdmissionCountedHolds-1 {
				t.Fatal("nonancestor aggregate did not fall")
			}
			for _, h := range list.Holds {
				if h.ID == e.hold.String() {
					t.Fatal("discarded hold in hot list")
				}
			}
			var audit []byte
			if err := e.pool.QueryRow(e.ctx, "SELECT merged_predecessor_discard_binding FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&audit); err != nil {
				t.Fatal(err)
			}
			var b map[string]any
			if err := json.Unmarshal(audit, &b); err != nil {
				t.Fatal(err)
			}
			if b["run_id"] != e.run.String() || b["mr_iid"] != float64(7) || b["completed_generation"] != float64(2) {
				t.Fatalf("audit=%s", audit)
			}
		})
	}
}

func TestMergedPredecessorOrdinaryDeadlineLiveDB(t *testing.T) {
	e := newMergedPollEnv(t)
	e.exec("UPDATE runs SET status='completed',claim_generation=2 WHERE id=$1", e.run)
	e.forge.syncDeadline = true
	e.engine.interval = time.Second
	e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
	e.engine.PollOnce(e.ctx)
	if e.state(e.hold) != "discarded" || e.forge.reads != 1 {
		t.Fatal("ordinary deadline prevented independent reconciliation")
	}
}

func TestMergedPredecessorBindingDriftDuringReadLiveDB(t *testing.T) {
	for _, field := range []string{"run_owner", "repo", "connection", "connection_owner", "project", "forge", "base_url", "mr", "generation", "status"} {
		t.Run(field, func(t *testing.T) {
			e := newMergedPollEnv(t)
			e.exec("UPDATE runs SET status='completed',claim_generation=2 WHERE id=$1", e.run)
			e.forge.read = func(ctx context.Context, p, i int64) (forge.MergeRequest, error) {
				switch field {
				case "run_owner", "connection_owner":
					owner := uuid.New()
					e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@drift.test")
					if field == "run_owner" {
						e.exec("UPDATE runs SET user_id=$2 WHERE id=$1", e.run, owner)
					} else {
						e.exec("UPDATE forge_connections SET user_id=$2 WHERE id=$1", e.conn, owner)
					}
				case "repo":
					repo := uuid.New()
					e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES($1,$2,43,$3,'https://forge.test/other',true)", repo, e.conn, repo.String())
					e.exec("UPDATE runs SET repo_id=$2 WHERE id=$1", e.run, repo)
				case "connection":
					conn := uuid.New()
					e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://other.test','bot',1,$3)", conn, e.worker.UserID, []byte{1})
					e.exec("UPDATE repos SET connection_id=$2 WHERE id=$1", e.repo, conn)
				case "project":
					e.exec("UPDATE repos SET forge_project_id=43 WHERE id=$1", e.repo)
				case "forge":
					e.exec("UPDATE forge_connections SET forge_type='github' WHERE id=$1", e.conn)
				case "base_url":
					e.exec("UPDATE forge_connections SET base_url='https://other.test' WHERE id=$1", e.conn)
				case "mr":
					e.exec("UPDATE runs SET mr_iid=8 WHERE id=$1", e.run)
				case "generation":
					e.exec("UPDATE runs SET claim_generation=3 WHERE id=$1", e.run)
				case "status":
					e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)
				}
				return forge.MergeRequest{IID: i, State: forge.MRStateMerged}, nil
			}
			e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
			e.engine.PollOnce(e.ctx)
			if e.forge.reads != 1 || e.state(e.hold) != "open" {
				t.Fatal("binding drift was accepted")
			}
		})
	}
}
