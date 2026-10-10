package workersvc

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Count every proof entry, including summary reads, without racing the report goroutine.
type serializedPublicationForge struct {
	*publicationForge
	calls atomic.Int32
}

func (f *serializedPublicationForge) GetMergeRequestSummary(ctx context.Context, project, mr int64) (forge.MergeRequestSummary, error) {
	f.calls.Add(1)
	return f.publicationForge.GetMergeRequestSummary(ctx, project, mr)
}

type serializedPublicationFixture struct {
	e         interlockLiveDB
	svc       *Service
	worker    store.Worker
	run, hold uuid.UUID
	req       StateRequest
	proof     *serializedPublicationForge
}

func seedSerializedPublication(t *testing.T, e interlockLiveDB, permit bool) serializedPublicationFixture {
	t.Helper()
	caps := []string{capability.RecoveryCompletedPublicationV1, "completion_interlock_v1"}
	wid := e.seedWorker(t, caps)
	var run uuid.UUID
	if permit {
		run = e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	} else {
		run = e.seedLegacyRunningRun(t, wid)
	}
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
	var iid int64
	if err := e.pool.QueryRow(e.ctx, "SELECT issue_iid FROM runs WHERE id=$1", run).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	branch, head := agentIssueBranch(iid), strings.Repeat("a", 40)
	gen, mr := int64(1), int64(7)
	hold := uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
	svc := e.permitService(t)
	svc.SetEphemeralLease(0)
	w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}
	req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
	if permit {
		req.Head = &head
		p, err := svc.RequestCompletionPermit(e.ctx, w, run, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head})
		if err != nil || !p.Granted {
			t.Fatalf("grant permit: %+v %v", p, err)
		}
	}
	proof := &serializedPublicationForge{publicationForge: &publicationForge{t: t, projectID: 1, mrIID: 7, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: forge.AncestryAncestor}}
	svc.SetForges(settleUnitBuilder{f: proof})
	return serializedPublicationFixture{e: e, svc: svc, worker: w, run: run, hold: hold, req: req, proof: proof}
}

func (f serializedPublicationFixture) assertUnstamped(t *testing.T, status string) {
	t.Helper()
	var actual string
	var headNull, identityNull, receiptNull bool
	err := f.e.pool.QueryRow(f.e.ctx, `SELECT r.status,r.completion_final_head IS NULL,h.completion_identity IS NULL,h.completed_publication_receipt IS NULL
 FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id WHERE h.id=$1`, f.hold).Scan(&actual, &headNull, &identityNull, &receiptNull)
	if err != nil || actual != status || !headNull || !identityNull || !receiptNull {
		t.Fatalf("unstamped: status=%s headNull=%t identityNull=%t receiptNull=%t err=%v", actual, headNull, identityNull, receiptNull, err)
	}
}

func TestCompletedPublicationPersistentWorkerLockLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprintf("permit_%t", permit), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			e.ctx = ctx
			f := seedSerializedPublication(t, e, permit)
			holder, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rollbackHolder := func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = holder.Rollback(cleanup)
			}
			defer rollbackHolder()
			var pid int
			if err = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if _, err = holder.Exec(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE", f.worker.ID); err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result StateReportResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				defer close(done)
				result, err := f.svc.SetStateReportWithReconciliation(ctx, f.worker, f.run, f.req)
				done <- outcome{result, err}
			}()
			// Cleanup cancels the bounded report, unlocks the holder, and joins the goroutine even on Fatal.
			var joined bool
			defer func() {
				cancel()
				rollbackHolder()
				if !joined {
					<-done
				}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err = e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case out := <-done:
					joined = true
					t.Fatalf("report finished before observable worker block: %+v %v", out.result, out.err)
				case <-ctx.Done():
					t.Fatal("no observable worker block: ", ctx.Err())
				case <-ticker.C:
				}
			}
			f.assertUnstamped(t, "running")
			if f.proof.calls.Load() != 0 {
				t.Fatal("forge proof ran while worker was locked")
			}
			probe, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, probeErr := probe.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", f.run)
			rollbackErr := probe.Rollback(ctx)
			if probeErr != nil || rollbackErr != nil {
				t.Fatalf("worker must lock BEFORE run: probe=%v rollback=%v", probeErr, rollbackErr)
			}
			if err = holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			out := <-done
			joined = true
			if out.err != nil || !out.result.Applied || out.result.CompletedPublicationReceipt == nil || f.proof.calls.Load() != 1 {
				t.Fatalf("released completion: %+v %v calls=%d", out.result, out.err, f.proof.calls.Load())
			}
			var stamped, identity, receipt, unleased bool
			if err = e.pool.QueryRow(ctx, `SELECT r.completion_final_head=$2,h.completion_identity IS NOT NULL,h.completed_publication_receipt IS NOT NULL,w.lease_since IS NULL
 FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id JOIN workers w ON w.id=r.worker_id WHERE h.id=$1`, f.hold, *f.req.CompletionFinalHead).Scan(&stamped, &identity, &receipt, &unleased); err != nil || !stamped || !identity || !receipt || !unleased {
				t.Fatalf("stamp/lease: %t %t %t %t %v", stamped, identity, receipt, unleased, err)
			}
		})
	}
}

func assertPublicationDowngrade(t *testing.T, e interlockLiveDB, permit, applies bool) {
	t.Helper()
	f := seedSerializedPublication(t, e, permit)
	e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY['completion_interlock_v1']::text[] WHERE id=$1", f.worker.ID)
	result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, f.req)
	if err != nil || result.Applied != applies || result.CompletedPublicationReceipt != nil {
		t.Fatalf("downgrade permit=%t applied=%t: %+v %v", permit, applies, result, err)
	}
	status := "running"
	if applies {
		status = "completed"
	}
	f.assertUnstamped(t, status)
	if f.proof.calls.Load() != 0 {
		t.Fatal("downgrade invoked forge proof")
	}
	if permit {
		var consumed bool
		if err = e.pool.QueryRow(e.ctx, "SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id=$1", f.run).Scan(&consumed); err != nil || consumed != applies {
			t.Fatalf("downgrade permit consumed=%t want=%t err=%v", consumed, applies, err)
		}
	}
}
func TestCompletedPublicationCapabilityDowngradeLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprintf("permit_%t", permit), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			e.ctx = ctx
			assertPublicationDowngrade(t, e, permit, true)
		})
	}
}

// Only this private database is migrated backwards; the shared fixture is never downgraded.
func isolatedPublicationDB(t *testing.T) (context.Context, string, interlockLiveDB) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	config, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "publication_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop private database: %v", err)
		}
	})
	config.Path = "/" + name
	isolated := config.String()
	if err = store.MigrateTo(ctx, isolated, 319); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, isolated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	iid := int64(1)
	e := interlockLiveDB{ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New(), nextIID: &iid}
	conn := uuid.New()
	e.exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", e.userID, "publication-"+e.userID.String()+"@e2e")
	e.exec(t, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, e.userID, []byte("x"))
	e.exec(t, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/interlock','https://forge.e2e/g/interlock','main',true)`, e.repoID, conn)
	return ctx, isolated, e
}

func TestCompletedPublicationDowngradeMigrationLiveDB(t *testing.T) {
	ctx, dsn, e := isolatedPublicationDB(t)
	preserved := seedSerializedPublication(t, e, true)
	result, err := preserved.svc.SetStateReportWithReconciliation(ctx, preserved.worker, preserved.run, preserved.req)
	if err != nil || result.CompletedPublicationReceipt == nil {
		t.Fatalf("seed existing receipt: %+v %v", result, err)
	}
	read := func() ([]byte, []byte) {
		t.Helper()
		var identity, receipt []byte
		if err := e.pool.QueryRow(ctx, "SELECT completion_identity,completed_publication_receipt FROM recovery_custody_holds WHERE id=$1", preserved.hold).Scan(&identity, &receipt); err != nil {
			t.Fatal(err)
		}
		return identity, receipt
	}
	identity, receipt := read()
	for step, version := range []int64{319, 320, 319, 320} {
		if step > 0 {
			if version == 319 {
				err = store.MigrateDownTo(ctx, dsn, version)
			} else {
				err = store.MigrateTo(ctx, dsn, version)
			}
			if err != nil {
				t.Fatalf("migration step %d: %v", step, err)
			}
		}
		t.Run(fmt.Sprintf("step_%d_version_%d", step, version), func(t *testing.T) {
			assertPublicationDowngrade(t, e, false, true)
			assertPublicationDowngrade(t, e, true, version == 320)
			afterIdentity, afterReceipt := read()
			if !bytes.Equal(identity, afterIdentity) || !bytes.Equal(receipt, afterReceipt) {
				t.Fatal("migration altered existing identity or receipt")
			}
		})
	}
}
