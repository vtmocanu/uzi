package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ClaimRun is executed inside a transaction: the hold and guard must be readable
// before commit, and both the claim and hold must disappear on rollback.
func TestRecoveryInventoryClaimLiveDB(t *testing.T) {
	testDSN := os.Getenv("UZI_TEST_DATABASE_URL")
	if testDSN == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, testDSN); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, caps := range [][]string{{"recovery_archive_v1", "recovery_inventory_v1"}, {"recovery_archive_v1", "recovery_inventory_v1_extra"}, {"recovery_archive_v1", "recovery_archive_v2"}} {
		user, worker, run, conn, repo := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("invclaim-%s@example.com", user))
		exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,$3)", conn, user, []byte{1})
		exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)", repo, conn)
		exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])
		exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) VALUES($1,$2,$3,'issue',1,'t','d','queued')", run, user, repo)
		p := store.ClaimRunParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: user, HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}, AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, SpreadCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}, BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, WorkerCaps: []string{}, DockerRepoAllowlist: []uuid.UUID{}, CapabilityAware: true, WorkerProtocolCaps: caps, CustodyHoldLimit: 8, RecoveryCapable: true, WorkerIdentity: worker.String()}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.New(tx).ClaimRun(ctx, p)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		var hold uuid.UUID
		var guarded bool
		if err := tx.QueryRow(ctx, "SELECT id,inventory_guarded FROM recovery_custody_holds WHERE run_id=$1 AND generation=$2", run, claimed.ClaimGeneration).Scan(&hold, &guarded); err != nil {
			t.Fatal(err)
		}
		want := caps[1] == "recovery_inventory_v1"
		if guarded != want {
			t.Fatalf("caps %v guard=%v", caps, guarded)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM recovery_custody_holds WHERE run_id=$1)", run).Scan(&exists); err != nil || exists {
			t.Fatalf("rolled-back hold: %v %v", exists, err)
		}
		claimed, err = store.New(pool).ClaimRun(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if claimed.ID != run || claimed.ClaimGeneration != 1 {
			t.Fatalf("claim: %+v", claimed)
		}
		if err := pool.QueryRow(ctx, "SELECT id FROM recovery_custody_holds WHERE run_id=$1", run).Scan(&hold); err != nil {
			t.Fatal(err)
		}
		q := store.New(pool)
		if want {
			if _, err := pool.Exec(ctx, "DELETE FROM workers WHERE id=$1", worker); err == nil {
				t.Fatal("open guarded hold permitted worker DELETE")
			}
			noAdopt := store.ReleaseClaimCustodyNoAdoptedSourceParams{RunID: run, WorkerID: worker, Generation: 1}
			exec("UPDATE runs SET status='running' WHERE id=$1", run)
			if n, err := q.ReleaseClaimCustodyNoAdoptedSource(ctx, noAdopt); err != nil || n != 0 {
				t.Fatalf("running no-adopt: %d %v", n, err)
			}
			cap, err := q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{RunID: run, UserID: user, OriginalWorkerID: pgtype.UUID{Bytes: worker, Valid: true}, OriginalWorkerIdentity: worker.String(), Generation: 1, SourceSha: "aaaa1111", IdempotencyKey: "earlier"})
			if err != nil {
				t.Fatal(err)
			}
			exec("UPDATE recovery_captures SET state='available',expires_at=now()+interval '1 hour' WHERE id=$1", cap.ID)
			exec("UPDATE runs SET status='completed',branch='agent/issue-1' WHERE id=$1", run)
			exec("UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", worker, run)
			if n, err := q.EnterEphemeralLease(ctx, store.EnterEphemeralLeaseParams{WorkerID: worker, RunID: run}); err != nil || n != 0 {
				t.Fatalf("open hold lease: %d %v", n, err)
			}
			tag, err := pool.Exec(ctx, "UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,release_evidence='publication',released_at=now() WHERE id=$1", hold)
			if err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("OLD release: %v %v", tag, err)
			}
			if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: run, Generation: 1, WorkerID: worker}); err != nil || n != 0 {
				t.Fatalf("exact release: %d %v", n, err)
			}
			if n, err := q.ReleaseCustodyHold(ctx, store.ReleaseCustodyHoldParams{ID: hold}); err != nil || n != 0 {
				t.Fatalf("reconciler release: %d %v", n, err)
			}
			rows, err := q.ListReleasableCustodyHolds(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range rows {
				if h.ID == hold {
					t.Fatal("guarded hold selected by reconciler")
				}
			}
			if yes, err := q.RunHasGuardedInventoryHold(ctx, store.RunHasGuardedInventoryHoldParams{RunID: run, UserID: user, WorkerID: worker, Generation: 1}); err != nil || !yes {
				t.Fatalf("guard query: %v %v", yes, err)
			}
			if _, err := q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{RunID: run, UserID: user, OriginalWorkerID: pgtype.UUID{Bytes: worker, Valid: true}, OriginalWorkerIdentity: worker.String(), Generation: 1, SourceSha: "bbbb2222", IdempotencyKey: "later"}); err != nil {
				t.Fatalf("later SAME generation reserve: %v", err)
			}
			exec("UPDATE runs SET claim_generation=2 WHERE id=$1", run)
			var completed pgtype.Timestamptz
			if err := pool.QueryRow(ctx, "SELECT status_since FROM runs WHERE id=$1", run).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if n, err := q.ReleasePredecessorCustodyHoldByAncestry(ctx, store.ReleasePredecessorCustodyHoldByAncestryParams{HoldID: hold, RunID: run, PredecessorGeneration: 1, SuccessorGeneration: 2, WorkerID: worker, Branch: "agent/issue-1", CompletedSince: completed, PushedSha: "aaaa1111", SourceSha: "aaaa1111", AdoptedSha: "aaaa1111", FinalHeadSha: "aaaa1111"}); err != nil || n != 0 {
				t.Fatalf("predecessor: %d %v", n, err)
			}
			exec("UPDATE runs SET status='running' WHERE id=$1", run)
			successor := uuid.New()
			exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded) VALUES($1,$2,$3,$4,2,'open',$5,$6,$5,$4,true)", successor, user, repo, run, worker, worker.String())
			if n, err := q.ReleasePredecessorCustodyHoldByLiveAncestry(ctx, store.ReleasePredecessorCustodyHoldByLiveAncestryParams{HoldID: hold, RunID: run, UserID: user, PredecessorGeneration: 1, SuccessorGeneration: 2, WorkerID: worker, PublishedSha: "aaaa1111", SourceSha: "aaaa1111", AdoptedSha: "aaaa1111", FinalHeadSha: "aaaa1111", ProvenBranch: "agent/issue-1", CapturedBranch: pgtype.Text{String: "agent/issue-1", Valid: true}, Target: "branch", Kind: "issue", IssueIid: pgtype.Int8{Int64: 1, Valid: true}}); err != nil || n != 0 {
				t.Fatalf("live predecessor: %d %v", n, err)
			}
			var state string
			if err := pool.QueryRow(ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state); err != nil || state != "open" {
				t.Fatalf("guard remains open: %s %v", state, err)
			}
		} else {
			if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: run, Generation: 1, WorkerID: worker}); err != nil || n != 1 {
				t.Fatalf("legacy release: %d %v", n, err)
			}
			exec("UPDATE runs SET status='completed',branch='agent/issue-1' WHERE id=$1", run)
			exec("UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", worker, run)
			if n, err := q.EnterEphemeralLease(ctx, store.EnterEphemeralLeaseParams{WorkerID: worker, RunID: run}); err != nil || n != 1 {
				t.Fatalf("legacy settled lease control: %d %v", n, err)
			}

		}
	}
}

func TestRecoveryInventoryNoAdoptLiveDB(t *testing.T) {
	// Existing ClaimRun coverage above proves genuine claim creation. This focused
	// fixture isolates the pre-payload helper's status/capture guards.
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, status := range []string{"claimed", "running", "claimed-with-capture"} {
		user, worker, run, hold := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := pool.Exec(ctx, sql, args...); err != nil {
				t.Fatal(err)
			}
		}
		exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("noadopt-%s@example.com", user))
		exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])
		conn, repo := uuid.New(), uuid.New()
		exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,$3)", conn, user, []byte{1})
		exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)", repo, conn)
		runStatus := status
		if status == "claimed-with-capture" {
			runStatus = "claimed"
		}
		exec("INSERT INTO runs(id,user_id,repo_id,issue_iid,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,$5,1,'issue','t','d',$3,$4,1)", run, user, runStatus, worker, repo)
		exec("INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state) VALUES($1,$2,$3,1,$4,'ident',$4,$3,true,'open')", hold, user, run, worker)
		if status == "claimed-with-capture" {
			exec("INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state) VALUES($1,$2,$3,$4,$5,'ident','aaaa1111','source','preparing')", uuid.New(), hold, run, user, worker)
		}
		n, err := store.New(pool).ReleaseClaimCustodyNoAdoptedSource(ctx, store.ReleaseClaimCustodyNoAdoptedSourceParams{RunID: run, WorkerID: worker, Generation: 1})
		want := int64(0)
		if status == "claimed" {
			want = 1
		}
		if err != nil || n != want {
			t.Fatalf("%s no-adopt: %d %v", status, n, err)
		}
	}
}
