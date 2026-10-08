package recovery

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerExhaustionCustodyListingsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; lead must execute LiveDB coverage")
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
	q := store.New(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@attention.e2e")
	exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://forge.e2e','bot',1,$3)", conn, owner, []byte{1})
	exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/attention','https://forge.e2e/g/attention','main',true)", repo, conn)
	exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'offline')", worker, owner, worker.String(), worker[:])
	wants := map[uuid.UUID]string{}
	for i, tc := range []struct{ cause, capture, want string }{
		{"worker_requeue_exhausted", "", "source_only"},
		{"worker_requeue_exhausted", "available", "source_only"},
		{"worker_requeue_exhausted", "preparing", "source_only"},
		{"worker_requeue_exhausted", "needs_action", "needs_action"},
		{"provider_outage", "", "active"},
	} {
		run, hold := uuid.New(), uuid.New()
		exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,recovery_wait_cause) VALUES($1,$2,$3,'issue',$4,'t','d','recovery_wait',$5)", run, owner, repo, int64(i+1), tc.cause)
		exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,$4,1,'open',$5,'attention-fixture',$5,$4)", hold, owner, repo, run, worker)
		if tc.capture != "" {
			exec("INSERT INTO recovery_captures(hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,expires_at) VALUES($1,$2,$3,$4,'attention-fixture',$5,$6,$7,now()+interval '1 day')", hold, run, owner, worker, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", uuid.NewString(), tc.capture)
		}
		wants[run] = tc.want
	}
	owners, err := q.ListCustodyHoldsForOwner(ctx, store.ListCustodyHoldsForOwnerParams{UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != len(wants) {
		t.Fatalf("owner listing rows=%d want=%d", len(owners), len(wants))
	}
	for _, row := range owners {
		if got := row.Attention; got != wants[row.RunID] || row.DecisionNeeded != (wants[row.RunID] != "active") {
			t.Fatalf("owner %s attention=%s want=%s cause=%s", row.RunID, got, wants[row.RunID], row.RecoveryWaitCause)
		}
	}
	workers, err := q.ListOpenCustodyHoldsForWorkers(ctx, []uuid.UUID{worker})
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != len(wants) {
		t.Fatalf("worker listing rows=%d want=%d", len(workers), len(wants))
	}
	remaining := map[string]int{"source_only": 3, "needs_action": 1, "active": 1}
	for _, row := range workers {
		got := row.Attention
		if row.WorkerID != worker || remaining[got] == 0 || row.DecisionNeeded != (got != "active") {
			t.Fatalf("unexpected worker listing attention=%s row=%+v", got, row)
		}
		remaining[got]--
	}
	for attention, count := range remaining {
		if count != 0 {
			t.Fatalf("worker listing missing %s", attention)
		}
	}
	counts, err := New(q, nil, nil, Limits{}, nil).CustodyDecisionsByWorker(ctx, []uuid.UUID{worker})
	if err != nil {
		t.Fatal(err)
	}
	if counts[worker] != 4 {
		t.Fatalf("decision count=%d want=4 (three source_only + needs_action)", counts[worker])
	}
}
