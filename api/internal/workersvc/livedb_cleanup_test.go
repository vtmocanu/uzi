package workersvc

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// liveDBTracked is one test's cleanup set on the shared LiveDB: the users it created
// (whose runs, workers and repos go with them) plus the ownerless products and
// agent_templates no user cascade reaches.
type liveDBTracked struct {
	pool      *pgxpool.Pool
	users     []uuid.UUID
	products  []uuid.UUID
	templates []uuid.UUID
}

var (
	liveDBTrackMu sync.Mutex
	liveDBTrack   = map[testing.TB]*liveDBTracked{}
)

// trackLiveDBUser schedules deletion of users this test created, with every run, worker
// and repo they own, when the test ends. Call it right after the insert. The first track
// call for a test registers one t.Cleanup on that pool, so register t.Cleanup(pool.Close)
// before it and any service-stop cleanup after it: LIFO then stops writers, deletes, and
// only then closes the pool.
func trackLiveDBUser(t testing.TB, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	liveDBTrackEntry(t, pool, func(e *liveDBTracked) { e.users = append(e.users, ids...) })
}

// trackLiveDBProduct schedules deletion of an ownerless products row (and its tokens).
func trackLiveDBProduct(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	liveDBTrackEntry(t, pool, func(e *liveDBTracked) { e.products = append(e.products, id) })
}

// trackLiveDBTemplate schedules deletion of an ownerless agent_templates row (user_id
// NULL); a user-scoped template already goes with its user.
func trackLiveDBTemplate(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	liveDBTrackEntry(t, pool, func(e *liveDBTracked) { e.templates = append(e.templates, id) })
}

func liveDBTrackEntry(t testing.TB, pool *pgxpool.Pool, add func(*liveDBTracked)) {
	t.Helper()
	liveDBTrackMu.Lock()
	defer liveDBTrackMu.Unlock()
	e, ok := liveDBTrack[t]
	if !ok {
		e = &liveDBTracked{pool: pool}
		liveDBTrack[t] = e
		t.Cleanup(func() {
			liveDBTrackMu.Lock()
			delete(liveDBTrack, t)
			liveDBTrackMu.Unlock()
			if err := deleteLiveDBTracked(e); err != nil {
				t.Errorf("LiveDB fixture cleanup: %v", err)
			}
		})
	}
	add(e)
}

// liveDBCleanupSteps run in order in one transaction. $1 binds the step's tracked set
// (users unless named otherwise); every row reached hangs off those ids.
var liveDBCleanupSteps = []struct {
	name, sql string
	set       liveDBSet
}{
	// Captures RESTRICT their hold, so they go first. A closure pins its capture and
	// cannot be deleted, so a pinned capture is left (none exists on the shared DB).
	{"recovery_captures", `DELETE FROM recovery_captures c
		WHERE (c.user_id = ANY($1) OR c.run_id IN (SELECT id FROM runs WHERE user_id = ANY($1)))
		  AND NOT EXISTS (SELECT 1 FROM recovery_prerequisite_closures pc WHERE pc.capture_id = c.id)`, liveDBUsers},
	// held_publication_delete_guard silently skips a lane-state row, so move those to an
	// inert terminal state first, through the transitions held_publication_guard allows
	// (neutraliseHeldRows' SQL).
	{"neutralise held publications (deleted)", `UPDATE run_held_publications SET state='deleted', live_run_id=NULL,
		deleted_at=COALESCE(deleted_at, now()), next_attempt_at=NULL, updated_at=now()
		WHERE (user_id = ANY($1) OR run_id IN (SELECT id FROM runs WHERE user_id = ANY($1)))
		  AND state IN ('created','acknowledged','delete_unknown')`, liveDBUsers},
	{"neutralise held publications (refused)", `UPDATE run_held_publications SET state='refused', live_run_id=NULL,
		refusal_reason='create_refused', next_attempt_at=NULL, updated_at=now()
		WHERE (user_id = ANY($1) OR run_id IN (SELECT id FROM runs WHERE user_id = ANY($1)))
		  AND state IN ('invoked','create_unknown')`, liveDBUsers},
	{"run_salvage", `DELETE FROM run_salvage WHERE user_id = ANY($1)
		OR run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))
		OR live_run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))`, liveDBUsers},
	{"checkpoint_retentions", `DELETE FROM checkpoint_retentions WHERE user_id = ANY($1)
		OR run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))`, liveDBUsers},
	{"checkpoint_publish_attempts", `DELETE FROM checkpoint_publish_attempts
		WHERE run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))`, liveDBUsers},
	{"worker_cordon_events", `DELETE FROM worker_cordon_events WHERE owner_id = ANY($1)
		OR worker_id IN (SELECT id FROM workers WHERE user_id = ANY($1))`, liveDBUsers},
	// As memoryFixtureCleanup: remove attempts before the run cascade, avoiding the
	// ON DELETE SET NULL updates and their snapshot shape checks.
	{"cross_checks", `DELETE FROM cross_checks
		WHERE lead_run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))
		   OR checker_run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))`, liveDBUsers},
	// Holds and publications RESTRICT each other (hold_id, final_publication_id), so one
	// statement deletes both; the FK checks run at its end. A hold carrying a merged
	// predecessor binding (its guard raises) or pinned by a closure stays: none exists on
	// the shared DB, and if one ever RESTRICTs a tracked run the transaction fails loudly.
	{"recovery_custody_holds + run_held_publications", `WITH h AS (
		DELETE FROM recovery_custody_holds rh
		WHERE (rh.user_id = ANY($1)
		    OR rh.live_run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))
		    OR rh.live_worker_id IN (SELECT id FROM workers WHERE user_id = ANY($1)))
		  AND rh.merged_predecessor_discard_binding IS NULL
		  AND NOT EXISTS (SELECT 1 FROM recovery_prerequisite_closures pc WHERE pc.hold_id = rh.id)
		RETURNING rh.id)
	DELETE FROM run_held_publications p
	WHERE p.user_id = ANY($1)
	   OR p.run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))
	   OR p.hold_id IN (SELECT id FROM h)`, liveDBUsers},
	// run_milestone_reconciliations.owner_id is NO ACTION: delete the runs (cascading
	// the reconciliation through run_id) before their owner.
	{"runs", `DELETE FROM runs WHERE user_id = ANY($1)`, liveDBUsers},
	// Cascades workers, forge_connections, repos, user_secrets and the rest.
	{"users", `DELETE FROM users WHERE id = ANY($1)`, liveDBUsers},
	// After the users: their tokens, job origins and allocations are already gone.
	{"product_tokens", `DELETE FROM product_tokens WHERE product_id = ANY($1)`, liveDBProducts},
	{"products", `DELETE FROM products WHERE id = ANY($1)`, liveDBProducts},
	{"agent_templates", `DELETE FROM agent_templates WHERE id = ANY($1)`, liveDBTemplates},
}

type liveDBSet int

const (
	liveDBUsers liveDBSet = iota
	liveDBProducts
	liveDBTemplates
)

// deleteLiveDBTracked runs every step in one transaction; any error rolls the whole
// cleanup back and is returned naming the step (the pg error names its constraint or
// trigger). It deliberately never nulls repos.guardrail_override_by: that column records
// the admin actor, so a test that writes it clears its own pointer.
func deleteLiveDBTracked(e *liveDBTracked) error {
	if len(e.users)+len(e.products)+len(e.templates) == 0 {
		return nil
	}
	// t.Context() is already cancelled when cleanups run.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sets := map[liveDBSet][]uuid.UUID{
		liveDBUsers: nonNilUUIDs(e.users), liveDBProducts: nonNilUUIDs(e.products), liveDBTemplates: nonNilUUIDs(e.templates),
	}
	users := sets[liveDBUsers]
	for _, s := range liveDBCleanupSteps {
		if _, err := tx.Exec(ctx, s.sql, sets[s.set]); err != nil {
			return &liveDBCleanupError{step: s.name, err: err}
		}
		if s.name == "neutralise held publications (refused)" {
			// The guard refuses an illegal transition silently; a row still in a lane
			// state would then be skipped silently by the delete guard.
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_held_publications
				WHERE (user_id = ANY($1) OR run_id IN (SELECT id FROM runs WHERE user_id = ANY($1)))
				  AND state = ANY($2::text[])`, users, heldLaneStates).Scan(&n); err != nil {
				return &liveDBCleanupError{step: "count lane publications", err: err}
			}
			if n != 0 {
				return &liveDBCleanupError{step: s.name, err: fmt.Errorf("%d held publications still in a cleanup-lane state", n)}
			}
		}
	}
	return tx.Commit(ctx)
}

type liveDBCleanupError struct {
	step string
	err  error
}

func (e *liveDBCleanupError) Error() string { return e.step + ": " + e.err.Error() }
func (e *liveDBCleanupError) Unwrap() error { return e.err }

// nonNilUUIDs binds an empty array rather than NULL, so ANY($n) matches nothing.
func nonNilUUIDs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

// liveDBRowCount counts rows of table whose column is any of ids.
func liveDBRowCount(t *testing.T, e codexTestEnv, table, column string, ids []uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+table+` WHERE `+column+` = ANY($1)`, ids).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Issue #4154: every shared-DB fixture shape deletes its own users, runs, workers, repos
// and custody rows (and its ownerless products and templates) when its test ends, while
// an untracked control user's rows stay.
func TestLiveDBFixtureCleanupRemovesOwnRowsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	controlUser, controlWorker, controlRepo := env.seedCodexInfra(t)
	controlRun := env.seedCodexRun(t, controlUser, controlWorker, controlRepo)

	var users, runs, workers, repos, holds, products, templates []uuid.UUID
	run := func(name string, body func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, body) {
			t.Fatalf("subtest %s failed", name)
		}
	}

	run("interlock", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		w := e.seedWorker(t, nil)
		users, workers, repos = append(users, e.userID), append(workers, w), append(repos, e.repoID)
		runs = append(runs, e.seedActiveRunOwnedBy(t, w))
	})

	run("seedUser with guarded hold and milestone reconciliation", func(t *testing.T) {
		e := setupCodexLiveDB(t)
		u, w, repo := e.seedCodexInfra(t)
		r := e.seedCodexRun(t, u, w, repo)
		hold := uuid.New()
		// live_run_id / live_worker_id RESTRICT the run and worker deletes.
		e.exec(`INSERT INTO recovery_custody_holds
			(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
			VALUES ($1,$2,$3,$4,1,'open',$5,'cleanup-ident',$5,$4,true)`, hold, u, repo, r, w)
		var input int64
		if err := e.pool.QueryRow(e.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'approve_plan','') RETURNING id", r).Scan(&input); err != nil {
			t.Fatal(err)
		}
		// owner_id is NO ACTION: it blocks the user delete unless the run goes first.
		e.exec(`INSERT INTO run_milestone_reconciliations(id,run_id,owner_id,gate_revision,presentation_id,
			gate_digest,reset_state_digest,worker_id,claim_generation,reason,consent,before_state,after_state,
			applied_input_id,stop_cutoff)
			VALUES($1,$2,$3,1,$4,$5,'fixture',$6,1,'fixture','{}','{}','{}',$7,0)`,
			uuid.New(), r, u, uuid.New(), []byte{1}, w, input)
		users, workers, repos = append(users, u), append(workers, w), append(repos, repo)
		runs, holds = append(runs, r), append(holds, hold)
	})

	run("held publication cycle", func(t *testing.T) {
		c := newCleanupFix(t)
		pub := c.created()
		// Releasing the hold against the row sets final_publication_id: hold and
		// publication now RESTRICT each other.
		c.acknowledge(pub)
		var final uuid.UUID
		if err := c.e.pool.QueryRow(c.e.ctx, `SELECT final_publication_id FROM recovery_custody_holds WHERE id=$1`, c.hold).Scan(&final); err != nil || final != pub.ID {
			t.Fatalf("final_publication_id = %v, %v; want %v", final, err, pub.ID)
		}
		users, workers, repos = append(users, c.e.userID), append(workers, c.worker.ID), append(repos, c.e.repoID)
		runs, holds = append(runs, c.run), append(holds, c.hold)
	})

	run("own pool", func(t *testing.T) {
		pool, err := store.OpenPool(context.Background(), os.Getenv("UZI_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		// Registered before the track call, so the delete runs while the pool is open.
		t.Cleanup(pool.Close)
		u := uuid.New()
		if _, err := pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("cleanup-%s@e2e", u)); err != nil {
			t.Fatal(err)
		}
		trackLiveDBUser(t, pool, u)
		users = append(users, u)
	})

	run("job product and ownerless template", func(t *testing.T) {
		e := setupJobLiveDB(t, 0)
		u := e.seedJobUser(t)
		prod, _ := e.seedProduct(t, u, []string{"research"})
		tmpl := uuid.New()
		e.exec(`INSERT INTO agent_templates (id, name, description, prompt_body) VALUES ($1, $2, 'd', 'b')`, tmpl, "cleanup-"+tmpl.String())
		trackLiveDBTemplate(t, e.pool, tmpl)
		users, products, templates = append(users, u), append(products, prod), append(templates, tmpl)
	})

	run("writer stopped before delete", func(t *testing.T) {
		e := setupCodexLiveDB(t)
		u, w, repo := uuid.New(), uuid.New(), uuid.New()
		conn := uuid.New()
		e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u, fmt.Sprintf("cleanup-%s@e2e", u))
		e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, u, []byte("x"))
		e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			VALUES ($1, $2, 1, $3, $4, 'main', true)`, repo, conn, "g/"+repo.String(), "https://forge.e2e/g/"+repo.String())
		e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`, w, u, "w-"+w.String(), w[:])

		stop, done, first := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var stopped atomic.Bool
		var writeErr error
		// Order is the contract under test: track, then the probe, then the stop. LIFO
		// runs stop/join, the probe, and only then the delete.
		trackLiveDBUser(t, e.pool, u)
		t.Cleanup(func() {
			if !stopped.Load() {
				t.Errorf("tracked rows were deleted while the fixture's writer was still running")
			}
		})
		t.Cleanup(func() {
			close(stop)
			<-done
			if writeErr != nil {
				t.Errorf("writer: %v", writeErr)
			}
		})
		go func() {
			defer close(done)
			defer stopped.Store(true)
			for iid := int64(1); ; iid++ {
				if _, err := e.pool.Exec(context.Background(), `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
					VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed')`, uuid.New(), u, repo, iid); err != nil {
					writeErr = err
					return
				}
				if iid == 1 {
					close(first)
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
		select {
		case <-first:
		case <-done:
			t.Fatalf("writer never inserted: %v", writeErr)
		}
		users, workers, repos = append(users, u), append(workers, w), append(repos, repo)
	})

	for _, c := range []struct {
		table, column string
		ids           []uuid.UUID
	}{
		{"users", "id", users}, {"runs", "user_id", users}, {"runs", "id", runs},
		{"workers", "id", workers}, {"repos", "id", repos},
		{"recovery_custody_holds", "id", holds}, {"recovery_custody_holds", "user_id", users},
		{"run_held_publications", "user_id", users},
		{"products", "id", products}, {"product_tokens", "product_id", products},
		{"agent_templates", "id", templates},
	} {
		if n := liveDBRowCount(t, env, c.table, c.column, c.ids); n != 0 {
			t.Errorf("%d %s rows (by %s) survived their tests' cleanup", n, c.table, c.column)
		}
	}
	for _, c := range []struct {
		table, column string
		id            uuid.UUID
	}{
		{"users", "id", controlUser}, {"workers", "id", controlWorker},
		{"repos", "id", controlRepo}, {"runs", "id", controlRun},
	} {
		if n := liveDBRowCount(t, env, c.table, c.column, []uuid.UUID{c.id}); n != 1 {
			t.Errorf("untracked control %s row: count %d, want 1", c.table, n)
		}
	}
}
