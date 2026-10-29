package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeOriginSchemaLifecycleLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "forge_origin_")
	if err := store.MigrateTo(ctx, dsn, 374); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, other, worker, otherWorker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{user, other} {
		mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", u, u.String()+"@example.com")
	}
	for _, p := range [][2]uuid.UUID{{user, worker}, {other, otherWorker}} {
		mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'w',$3)", p[1], p[0], p[1][:])
	}
	conn1, conn2, connOther := uuid.New(), uuid.New(), uuid.New()
	repo1, repo2, repoOther := uuid.New(), uuid.New(), uuid.New()
	for i, p := range [][3]uuid.UUID{{conn1, user, repo1}, {conn2, user, repo2}, {connOther, other, repoOther}} {
		mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
            VALUES($1,$2,'gitlab',$3,'bot',1,'x')`, p[0], p[1], "https://api"+string(rune('a'+i))+".example")
		mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url)
            VALUES($1,$2,1,'group/repo','https://git.example/group/repo')`, p[2], p[0])
	}
	// Seed actual in-flight claims before M1 exists, including an origin that
	// this new telemetry normalizer cannot represent. Rollout must not reauthorize them.
	legacy, legacyUnknown := uuid.New(), uuid.New()
	for _, p := range [][2]uuid.UUID{{legacy, repo1}, {legacyUnknown, repo2}} {
		mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
            VALUES ($1,$2,$3,'issue',1,'legacy','','claimed',$4,1)`, p[0], user, p[1], worker)
	}
	mustExec(ctx, t, pool, "UPDATE repos SET web_url='http://git.example/group/repo' WHERE id=$1", repo2)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	for _, id := range []uuid.UUID{legacy, legacyUnknown} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM run_forge_origins WHERE run_id=$1", id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rollout fabricated snapshot: %d, %v", n, err)
		}
		if _, err := q.GetRunClaimContext(ctx, id); err != nil {
			t.Fatalf("legacy claimed context: %v", err)
		}
		// Existing running-generation users of GetRunClaimContext also keep working.
		mustExec(ctx, t, pool, "UPDATE runs SET status='running' WHERE id=$1", id)
		if _, err := q.GetRunClaimContext(ctx, id); err != nil {
			t.Fatalf("legacy running context: %v", err)
		}
		mustExec(ctx, t, pool, "UPDATE runs SET status='claimed' WHERE id=$1", id)
	}
	// A new generation is no longer historical absence, even on the same run.
	mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", legacyUnknown)
	if _, err := q.GetRunClaimContext(ctx, legacyUnknown); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new unknown generation context: %v", err)
	}
	refused, err := q.RunForgeOriginClaimRefused(ctx, legacyUnknown)
	if err != nil || !refused {
		t.Fatalf("unknown classification: %v, %v", refused, err)
	}
	mustExec(ctx, t, pool, "UPDATE repos SET web_url='https://git.example/group/repo' WHERE id=$1", repo2)
	seed := func(u, w uuid.UUID, origin string) uuid.UUID {
		t.Helper()
		var inc uuid.UUID
		err := pool.QueryRow(ctx, `INSERT INTO forge_origin_cooldowns
            (user_id,worker_id,origin,cooled_until,last_reason,observed_at)
            VALUES($1,$2,$3,now()-interval '1 day','transient_clone_fetch_exhausted',now()) RETURNING incarnation`, u, w, origin).Scan(&inc)
		if err != nil {
			t.Fatal(err)
		}
		return inc
	}
	count := func(u uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM forge_origin_cooldowns WHERE user_id=$1", u).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	first := seed(user, worker, "https://git.example")
	seed(other, otherWorker, "https://git.example")
	seed(user, worker, "https://git.example:8443")
	// Expiry and worker registration facts do not prune or reset identity.
	mustExec(ctx, t, pool, "UPDATE workers SET status='online',version='reregistered' WHERE id=$1", worker)
	if count(user) != 2 {
		t.Fatal("registration/expiry removed current state")
	}
	// A second connection to the same actual origin keeps its single shared key.
	mustExec(ctx, t, pool, "DELETE FROM repos WHERE id=$1", repo1)
	if count(user) != 1 {
		t.Fatal("prune did not retain shared origin or remove obsolete port")
	}
	var kept uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT incarnation FROM forge_origin_cooldowns WHERE user_id=$1", user).Scan(&kept); err != nil || kept != first {
		t.Fatalf("shared key incarnation changed: %v, %v", kept, err)
	}
	mustExec(ctx, t, pool, "DELETE FROM forge_connections WHERE id=$1", conn2)
	if count(user) != 0 || count(other) != 1 {
		t.Fatal("last-reference deletion crossed owner scope or retained obsolete key")
	}
	// Recreated current records always get a fresh incarnation, even for an
	// identical owner/worker/origin tuple.
	fresh := seed(user, worker, "https://git.example")
	if fresh == first {
		t.Fatal("deletion/recreation reused incarnation")
	}
	// The schema itself rejects malformed/empty origins and foreign workers.
	for _, origin := range []string{"", "garbage", "https://example.com:bad"} {
		if _, err := pool.Exec(ctx, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
            VALUES($1,$2,$3,now(),'transient_clone_fetch_exhausted',now())`, user, worker, origin); err == nil {
			t.Fatalf("accepted malformed origin %q", origin)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
        VALUES($1,$2,'https://foreign.example',now(),'transient_clone_fetch_exhausted',now())`, user, otherWorker); err == nil {
		t.Fatal("accepted foreign worker")
	}
	mustExec(ctx, t, pool, "DELETE FROM workers WHERE id=$1", worker)
	if count(user) != 0 {
		t.Fatal("worker deletion retained cooldown")
	}
	mustExec(ctx, t, pool, "DELETE FROM users WHERE id=$1", other)
	if count(other) != 0 {
		t.Fatal("owner deletion retained cooldown")
	}
}
