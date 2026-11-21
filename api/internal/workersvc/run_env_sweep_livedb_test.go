package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// PRD #4416 M2: the retired-version sweeper (SweepRunEnvRetired) against a real PostgreSQL. A
// retired secret version is swept once no held run holds a receipt for it. Skipped without
// UZI_TEST_DATABASE_URL; run via ./e2e/run-store-it.sh.

// sweepCase is a terminal run (finished three days ago) that holds a secret receipt for one
// retired version of a variable.
type sweepCase struct {
	fx     *reFix
	run    uuid.UUID
	varID  uuid.UUID
	holder uuid.UUID // the hold row id the custody kinds create
}

func newSweepCase(t *testing.T) *sweepCase {
	t.Helper()
	fx := newREFix(t, true)
	run := fx.queuedRun(t, "issue")
	varID := fx.put(t, "OLD_KEY", true, envSecretValue("old"))
	fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1`, varID)
	fx.env.exec(`INSERT INTO run_env_receipts (run_id,user_id,var_id,rev,scope,name,secret,first_claim_generation,last_claim_generation)
	    VALUES ($1,$2,$3,1,'repo','OLD_KEY',true,1,1)`, run, fx.userID, varID)
	fx.env.exec(`UPDATE runs SET status = 'failed', fail_origin = 'agent_failure', finished_at = now() - interval '3 days',
	    updated_at = now() - interval '3 days', claim_released_at = now() - interval '3 days' WHERE id = $1`, run)
	return &sweepCase{fx: fx, run: run, varID: varID}
}

func (c *sweepCase) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := c.fx.env.pool.QueryRow(c.fx.env.ctx, `SELECT count(*) FROM run_env_vars WHERE var_id = $1`, c.varID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (c *sweepCase) identities(t *testing.T) int {
	t.Helper()
	var n int
	if err := c.fx.env.pool.QueryRow(c.fx.env.ctx, `SELECT count(*) FROM run_env_var_ids WHERE var_id = $1`, c.varID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sweep runs the pass until its owner cursor has been all the way round, so the fixture's owner
// has been looked at whatever other tests left in the shared database. It reports nothing about
// how many rows went: rows of other tests' owners may go too.
func (c *sweepCase) sweep(t *testing.T) {
	t.Helper()
	for range 400 {
		if _, err := c.fx.svc.SweepRunEnvRetired(c.fx.env.ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if c.fx.svc.runEnvSweep.after == uuid.Nil {
			return
		}
	}
	t.Fatal("the sweep cursor never wrapped")
}

func (c *sweepCase) hold(t *testing.T, id uuid.UUID, state string, guarded bool) {
	t.Helper()
	c.fx.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded)
	    VALUES ($1,$2,$3,$4,1,$5,$6,'sweep-fixture',$7)`, id, c.fx.userID, c.fx.repoID, c.run, state, c.fx.workerID, guarded)
}

// execNoTriggers runs a fixture write with the table guards off, for a transition the guards
// reserve to the recovery code paths (the fixture only needs the resulting state).
func (c *sweepCase) execNoTriggers(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.fx.env.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// execOptIn runs a publication-resume write under the API opt-in the guard trigger requires.
func (c *sweepCase) execOptIn(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.fx.env.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('uzi.publication_resume_api', 'v1', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Every custody form the hold predicate names keeps the version while it is live and releases it
// when it settles. A row for the same run in the settled form must not keep it.
func TestRunEnvSweepKeepsVersionsOfHeldRunsLiveDB(t *testing.T) {
	tip := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	cases := []struct {
		name    string
		hold    func(t *testing.T, c *sweepCase)
		release func(t *testing.T, c *sweepCase)
	}{
		{"open custody hold",
			func(t *testing.T, c *sweepCase) { c.hold(t, c.holder, "open", false) },
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE recovery_custody_holds SET state='released', released_at=now() WHERE id=$1`, c.holder)
			}},
		{"live capture",
			func(t *testing.T, c *sweepCase) {
				c.hold(t, c.holder, "released", false)
				c.fx.env.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
				    VALUES ($1,$2,$3,$4,'fixture',$5,'k','available')`, c.holder, c.holder, c.run, c.fx.userID, tip)
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE recovery_captures SET state='expired' WHERE id=$1`, c.holder)
			}},
		{"live held publication",
			func(t *testing.T, c *sweepCase) {
				c.hold(t, c.holder, "released", false)
				c.fx.env.exec(`INSERT INTO run_held_publications(run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state,create_invoked_at)
				    VALUES ($1,1,$2,$3,$4,$5,$1,$6,$7,$8,'prepared',NULL)`,
					c.run, c.holder, c.fx.userID, c.fx.repoID, c.fx.workerID, "refs/uzi-held/"+c.run.String()+"/1", tip, digest)
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE run_held_publications SET state='invoked', create_invoked_at=now() WHERE run_id=$1`, c.run)
				c.fx.env.exec(`UPDATE run_held_publications SET state='refused', live_run_id=NULL, refusal_reason='create_refused' WHERE run_id=$1`, c.run)
			}},
		{"terminal preservation",
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`INSERT INTO run_terminal_preservations(run_id,user_id,repo_id,ref,lifetime_at_bind,state)
				    VALUES ($1,$2,$3,$4,'issue','bound')`, c.run, c.fx.userID, c.fx.repoID, pushbroker.TerminalRef(c.run))
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE run_terminal_preservations SET state='not_published' WHERE run_id=$1`, c.run)
			}},
		{"pending salvage",
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`INSERT INTO run_salvage(run_id,user_id,repo_id,forge_type,branch,tip,live_run_id,state)
				    VALUES ($1,$2,$3,'github','agent/issue-7',$4,$1,'pending')`, c.run, c.fx.userID, c.fx.repoID, tip)
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE run_salvage SET state='unavailable', live_run_id=NULL WHERE run_id=$1`, c.run)
			}},
		{"live prerequisite closure",
			func(t *testing.T, c *sweepCase) {
				c.hold(t, c.holder, "open", true)
				c.fx.env.exec(`INSERT INTO recovery_captures
				    (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,prerequisite_shas,reserved_bytes,coverage_digest,expires_at)
				    VALUES ($1,$1,$2,$3,$4,'sweep-fixture',$5,'k','available',true,3,$6,1,ARRAY[$5]::text[],3,$6,now()+interval '1 day')`,
					c.holder, c.run, c.fx.userID, c.fx.workerID, tip, digest)
				if _, err := c.fx.env.q.InsertPrerequisiteClosure(c.fx.env.ctx, c.holder); err != nil {
					t.Fatal(err)
				}
				// Neither the hold nor the capture holds the run any more; only the closure does.
				c.execNoTriggers(t, `UPDATE recovery_captures SET state='expired' WHERE id=$1`, c.holder)
				c.execNoTriggers(t, `UPDATE recovery_custody_holds SET state='released', released_at=now(), live_worker_id=NULL, live_run_id=NULL WHERE id=$1`, c.holder)
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE recovery_prerequisite_closures SET state='refused' WHERE capture_id=$1`, c.holder)
			}},
		{"active publication resume",
			func(t *testing.T, c *sweepCase) {
				c.execOptIn(t, `INSERT INTO run_publication_resume(run_id,enrollment_seq,claim_generation,intent,state)
				    VALUES ($1,1,1,$2::json,'active')`, c.run, `{"run_id":"`+c.run.String()+`"}`)
			},
			func(t *testing.T, c *sweepCase) {
				c.execOptIn(t, `UPDATE run_publication_resume SET state='completed' WHERE run_id=$1`, c.run)
			}},
		{"checkpoint retention",
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state)
				    VALUES ($1,$2,$3,'agent/issue-7','refs/uzi-checkpoints/agent/issue-7',$4,'retained')`, c.run, c.fx.userID, c.fx.repoID, tip)
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`UPDATE checkpoint_retentions SET state='deleted', settled_at=now() WHERE run_id=$1`, c.run)
			}},
		{"unaccounted checkpoint push",
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES($1,'agent/issue-7','ref',$2)`, c.run, tip)
			},
			func(t *testing.T, c *sweepCase) {
				c.fx.env.exec(`DELETE FROM checkpoint_publish_attempts WHERE run_id=$1`, c.run)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSweepCase(t)
			c.holder = uuid.New()
			tc.hold(t, c)
			c.sweep(t)
			if c.rows(t) != 1 {
				t.Fatalf("held (%s): rows=%d; want the version kept", tc.name, c.rows(t))
			}
			tc.release(t, c)
			c.sweep(t)
			if c.rows(t) != 0 {
				t.Fatalf("released (%s): rows=%d; want the version deleted", tc.name, c.rows(t))
			}
			// The identity stays while a receipt still names the variable.
			if c.identities(t) != 1 {
				t.Fatalf("the identity went while a receipt still names the variable")
			}
			c.sweep(t)
			if c.rows(t) != 0 || c.identities(t) != 1 {
				t.Fatalf("a re-run changed the settled state: rows=%d identities=%d", c.rows(t), c.identities(t))
			}
		})
	}
}

// A run that is not terminal, or finished less than 24 hours ago, holds its versions.
func TestRunEnvSweepGraceWindowLiveDB(t *testing.T) {
	c := newSweepCase(t)
	for _, age := range []string{"1 hour", "23 hours"} {
		c.fx.env.exec(`UPDATE runs SET finished_at = now() - $2::interval, updated_at = now() - $2::interval WHERE id = $1`, c.run, age)
		c.sweep(t)
		if c.rows(t) != 1 {
			t.Fatalf("finished %s ago: rows=%d; want the version kept", age, c.rows(t))
		}
	}
	c.fx.env.exec(`UPDATE runs SET finished_at = now() - interval '25 hours', updated_at = now() - interval '25 hours' WHERE id = $1`, c.run)
	c.fx.env.exec(`UPDATE runs SET status = 'running', finished_at = NULL, fail_origin = NULL WHERE id = $1`, c.run)
	c.sweep(t)
	if c.rows(t) != 1 {
		t.Fatalf("a running run: rows=%d; want the version kept", c.rows(t))
	}
	c.fx.env.exec(`UPDATE runs SET status = 'failed', fail_origin = 'agent_failure', finished_at = now() - interval '25 hours' WHERE id = $1`, c.run)
	c.sweep(t)
	if c.rows(t) != 0 {
		t.Fatalf("finished 25 hours ago: rows=%d; want the version deleted", c.rows(t))
	}
}

// A pending-deletion version, a version with only a plain receipt and a version with no receipt
// are swept; a version a held run receipted by a different revision does not keep this one.
func TestRunEnvSweepWithoutSecretReceiptLiveDB(t *testing.T) {
	c := newSweepCase(t)
	other := c.fx.put(t, "PLAIN_OLD", true, envSecretValue("plain-receipt"))
	c.fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1`, other)
	c.fx.env.exec(`INSERT INTO run_env_receipts (run_id,user_id,var_id,rev,scope,name,secret,first_claim_generation,last_claim_generation)
	    VALUES ($1,$2,$3,1,'repo','PLAIN_OLD',false,1,1)`, c.run, c.fx.userID, other)
	pending := c.fx.put(t, "PENDING_ONE", true, envSecretValue("pending"))
	c.fx.env.exec(`UPDATE run_env_vars SET pending_delete_at = now() WHERE var_id = $1`, pending)
	live := c.fx.put(t, "LIVE_ONE", true, envSecretValue("live"))
	// The held run receipted this version, so the sweep keeps it: make it held, then check.
	c.fx.env.exec(`UPDATE runs SET finished_at = now() - interval '1 hour' WHERE id = $1`, c.run)
	c.sweep(t)
	count := func(varID uuid.UUID) (rows, idents int) {
		if err := c.fx.env.pool.QueryRow(c.fx.env.ctx, `SELECT (SELECT count(*) FROM run_env_vars WHERE var_id=$1), (SELECT count(*) FROM run_env_var_ids WHERE var_id=$1)`, varID).Scan(&rows, &idents); err != nil {
			t.Fatal(err)
		}
		return rows, idents
	}
	// The plain-receipted version goes (its identity stays: the receipt names it); the pending
	// one with no receipt goes with its identity.
	if rows, idents := count(other); rows != 0 || idents != 1 {
		t.Fatalf("plain-receipted version: rows=%d identities=%d; want 0 and 1", rows, idents)
	}
	if rows, idents := count(pending); rows != 0 || idents != 0 {
		t.Fatalf("unreceipted pending version: rows=%d identities=%d; want 0 and 0", rows, idents)
	}
	if c.rows(t) != 1 {
		t.Fatal("the secret-receipted version of a held run was deleted")
	}
	var liveRows int
	if err := c.fx.env.pool.QueryRow(c.fx.env.ctx, `SELECT count(*) FROM run_env_vars WHERE var_id = $1 AND retired_at IS NULL`, live).Scan(&liveRows); err != nil || liveRows != 1 {
		t.Fatalf("a live version was touched: %d %v", liveRows, err)
	}
}

// Identities go once nothing references them: no row and no receipt, plain or secret.
func TestRunEnvSweepDeletesUnreferencedIdentitiesLiveDB(t *testing.T) {
	c := newSweepCase(t)
	// Settle the run so the version goes; the receipt still names the variable.
	c.sweep(t)
	if c.rows(t) != 0 || c.identities(t) != 1 {
		t.Fatalf("rows=%d identities=%d; want the version gone and the identity kept while a receipt names it", c.rows(t), c.identities(t))
	}
	c.fx.env.exec(`DELETE FROM run_env_receipts WHERE run_id = $1`, c.run)
	c.sweep(t)
	if c.identities(t) != 0 {
		t.Fatal("the orphan identity stayed")
	}
	// A variable deleted outright leaves an identity with no work in the version scan.
	gone := c.fx.put(t, "GONE_ONE", false, "x")
	c.fx.env.exec(`DELETE FROM run_env_vars WHERE var_id = $1`, gone)
	c.sweep(t)
	var left int
	if err := c.fx.env.pool.QueryRow(c.fx.env.ctx, `SELECT count(*) FROM run_env_var_ids WHERE var_id = $1`, gone).Scan(&left); err != nil || left != 0 {
		t.Fatalf("identity of the deleted variable: %d %v", left, err)
	}
}

// A busy owner (its users row held by a writer) is skipped, not waited for and not failed.
func TestRunEnvSweepSkipsBusyOwnerLiveDB(t *testing.T) {
	c := newSweepCase(t)
	ctx := c.fx.env.ctx
	tx, err := c.fx.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE users SET default_effort='low' WHERE id=$1`, c.fx.userID); err != nil {
		t.Fatal(err)
	}
	c.sweep(t)
	if c.rows(t) != 1 {
		t.Fatalf("busy owner: rows=%d; want it skipped", c.rows(t))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	c.sweep(t)
	if c.rows(t) != 0 {
		t.Fatalf("free owner: rows=%d; want the version deleted", c.rows(t))
	}
}
