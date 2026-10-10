package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545 M1: migration 00321's held-publication schema. Every test stands its own
// database, so the CHECKs, the two triggers and the rebuilt recovery_inventory_hold_guard are
// executed by real UPDATEs. A refused guard update is a silent zero-row UPDATE (RETURN NULL),
// a failed CHECK is a 23514, and the tests tell the two apart.

const (
	heldTip    = "1111111111111111111111111111111111111111"
	heldDigest = "2222222222222222222222222222222222222222222222222222222222222222"
)

type heldFixture struct {
	pool   *pgxpool.Pool
	user   uuid.UUID
	repo   uuid.UUID
	worker uuid.UUID
	run    uuid.UUID
	hold   uuid.UUID
	pub    uuid.UUID
}

func heldMigratedPool(t *testing.T, prefix string) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, prefix)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, dsn, pool
}

// heldNewFixture inserts an owner, repo, worker, a failed run at generation 1 and an open hold
// for it. guarded selects recovery_inventory_hold_guard (true) or the CHECK-only backstop (false).
func heldNewFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, guarded bool, status string, failOrigin *string) heldFixture {
	t.Helper()
	f := heldNewHoldFixture(ctx, t, pool, guarded, status, failOrigin)
	f.insertPublication(ctx, t, "created")
	return f
}

// heldNewHoldFixture is heldNewFixture without the publication row.
func heldNewHoldFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, guarded bool, status string, failOrigin *string) heldFixture {
	t.Helper()
	f := heldFixture{pool: pool, user: uuid.New(), worker: uuid.New(), run: uuid.New(), hold: uuid.New(), pub: uuid.New()}
	conn := uuid.New()
	f.repo = uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`,
		f.user, fmt.Sprintf("held-%s@example.com", f.user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections
		(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'github','https://example.com','bot',1,$3)`, conn, f.user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos
		(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)`, f.repo, conn)
	mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status)
		VALUES($1,$2,'held',$3,'online')`, f.worker, f.user, f.worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
		VALUES($1,$2,$3,$4,'issue',1,'held','body','claimed',1)`, f.run, f.user, f.repo, f.worker)
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded)
		VALUES($1,$2,$3,$4,1,'open',$5,'held-worker',$5,$4,$6)`, f.hold, f.user, f.repo, f.run, f.worker, guarded)
	mustExec(ctx, t, pool, `UPDATE runs SET status=$2, fail_origin=$3 WHERE id=$1`, f.run, status, failOrigin)
	return f
}

// insertPublication inserts the (run, generation 1) publication as prepared, as the INSERT guard
// requires, then walks it to the given state through the guard-free setState.
func (f *heldFixture) insertPublication(ctx context.Context, t *testing.T, state string) {
	t.Helper()
	f.insertPreparedAt(ctx, t, f.pub, 1, f.worker)
	if state != "prepared" {
		f.setState(ctx, t, state)
	}
}

// insertPreparedAt inserts a prepared publication row of the fixture's run and hold.
func (f heldFixture) insertPreparedAt(ctx context.Context, t *testing.T, id uuid.UUID, generation int, worker uuid.UUID) {
	t.Helper()
	mustExec(ctx, t, f.pool, `INSERT INTO run_held_publications
		(id,run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
		VALUES($1,$2,$3::bigint,$4,$5,$6,$7,$2,'refs/uzi-held/'||$2::uuid::text||'/'||$3::bigint::text,$8,$9,'prepared')`,
		id, f.run, generation, f.hold, f.user, f.repo, worker, heldTip, heldDigest)
}

// heldRelease attempts the guarded release of the fixture's hold and reports the rows affected.
func (f heldFixture) release(ctx context.Context, t *testing.T, publication uuid.UUID, digest, tip string) (int64, error) {
	t.Helper()
	tag, err := f.pool.Exec(ctx, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
		final_disposition='held_publication',release_evidence='held_publication',final_publication_id=$2,
		final_source_sha=$4,final_coverage_digest=$3,released_at=now() WHERE id=$1`, f.hold, publication, digest, tip)
	return tag.RowsAffected(), err
}

func heldCheckName(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want constraint %s, got %v", want, err)
	}
}

// The tie, converse and shape CHECKs are the backstop for a hold the inventory guard does not
// cover (inventory_guarded=false), and each NULL form must be refused: a plain equality against
// a NULL disposition would pass.
func TestHeldPublicationHoldChecksLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "held_checks_")
	other := uuid.New()
	tests := []struct {
		name  string
		set   string
		check string
	}{
		{"publication id without a disposition", `final_publication_id=$2`, "recovery_custody_holds_held_publication_tie_check"},
		// The shape CHECK fires first on an unguarded hold; the tie CHECK is reached by the cases above.
		{"released disposition without a publication id", `state='released',live_worker_id=NULL,live_run_id=NULL,final_disposition='held_publication',
			release_evidence='held_publication',final_source_sha='` + heldTip + `',final_coverage_digest='` + heldDigest + `',released_at=now()`, "recovery_custody_holds_held_publication_shape_check"},
		{"evidence without a disposition", `release_evidence='held_publication'`, "recovery_custody_holds_held_publication_evidence_check"},
		{"disposition and id with NULL evidence", `final_disposition='held_publication',final_publication_id=$2`, "recovery_custody_holds_held_publication_evidence_check"},
		{"disposition and id with foreign evidence", `final_disposition='held_publication',final_publication_id=$2,release_evidence='archive'`, "recovery_custody_holds_held_publication_evidence_check"},
		{"full tuple on an open hold", `final_disposition='held_publication',final_publication_id=$2,release_evidence='held_publication',
			final_source_sha='` + heldTip + `',final_coverage_digest='` + heldDigest + `',released_at=now()`, "recovery_custody_holds_held_publication_shape_check"},
		{"released without a source sha", `state='released',live_worker_id=NULL,live_run_id=NULL,final_disposition='held_publication',
			final_publication_id=$2,release_evidence='held_publication',final_coverage_digest='` + heldDigest + `',released_at=now()`, "recovery_custody_holds_held_publication_shape_check"},
		{"released while still live", `state='released',final_disposition='held_publication',
			final_publication_id=$2,release_evidence='held_publication',final_source_sha='` + heldTip + `',
			final_coverage_digest='` + heldDigest + `',released_at=now()`, "recovery_custody_holds_held_publication_shape_check"},
		{"complete tuple on an unguarded hold", `state='released',live_worker_id=NULL,live_run_id=NULL,final_disposition='held_publication',
			final_publication_id=$2,release_evidence='held_publication',final_source_sha='` + heldTip + `',
			final_coverage_digest='` + heldDigest + `',released_at=now()`, "recovery_custody_holds_held_publication_shape_check"},
		{"released without a coverage digest", `state='released',live_worker_id=NULL,live_run_id=NULL,final_disposition='held_publication',
			final_publication_id=$2,release_evidence='held_publication',final_source_sha='` + heldTip + `',released_at=now()`, "recovery_custody_holds_held_publication_shape_check"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := heldNewFixture(ctx, t, pool, false, "failed", nil) // one hold per case: a wrongly accepted update cannot poison the next
			_, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET `+tc.set+` WHERE id=$1 AND $2::uuid IS NOT NULL`, f.hold, other)
			heldCheckName(t, err, tc.check)
		})
	}
	// The complete tuple is accepted, so the refusals above are the constraints and not a fixture fault.
	f := heldNewFixture(ctx, t, pool, true, "failed", nil)
	tag, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
		final_disposition='held_publication',final_publication_id=$2,release_evidence='held_publication',
		final_source_sha=$3,final_coverage_digest=$4,released_at=now() WHERE id=$1`, f.hold, f.pub, heldTip, heldDigest)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("complete held release: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

// recovery_inventory_hold_guard's held_publication branch: only a FAILED run of the hold's own
// generation, not push_secret_blocked / worker_residue_blocked, with a matching created
// publication, may release. Every refusal is a zero-row UPDATE.
func TestHeldPublicationHoldGuardLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "held_guard_")
	str := func(s string) *string { return &s }
	t.Run("failed run releases", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 1 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
		// The hold's classification is immutable once released.
		for _, set := range []string{`final_publication_id=gen_random_uuid()`, `final_disposition='archive'`,
			`final_coverage_digest='` + strings.Repeat("3", 64) + `'`} {
			tag, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET `+set+` WHERE id=$1`, f.hold)
			if err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("mutating a released hold with %s: rows=%d err=%v", set, tag.RowsAffected(), err)
			}
		}
	})
	t.Run("failed run with NULL fail_origin releases", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", nil)
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 1 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
	})
	refused := []struct {
		name   string
		status string
		origin *string
	}{
		{"cancelled run", "cancelled", str("agent_failure")},
		{"cancelled run with NULL fail_origin", "cancelled", nil},
		{"push_secret_blocked", "failed", str("push_secret_blocked")},
		{"worker_residue_blocked", "failed", str("worker_residue_blocked")},
		{"completed run", "completed", nil},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			f := heldNewFixture(ctx, t, pool, true, tc.status, tc.origin)
			if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 0 {
				t.Fatalf("release: rows=%d err=%v", n, err)
			}
			assertHoldOpen(ctx, t, pool, f.hold)
		})
	}
	t.Run("stale generation", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		mustExec(ctx, t, pool, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
		assertHoldOpen(ctx, t, pool, f.hold)
	})
	t.Run("publication mismatches", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		if n, err := f.release(ctx, t, f.pub, strings.Repeat("4", 64), heldTip); err != nil || n != 0 {
			t.Fatalf("foreign digest: rows=%d err=%v", n, err)
		}
		if n, err := f.release(ctx, t, f.pub, heldDigest, strings.Repeat("5", 40)); err != nil || n != 0 {
			t.Fatalf("foreign tip: rows=%d err=%v", n, err)
		}
		if n, err := f.release(ctx, t, uuid.New(), heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("unknown publication: rows=%d err=%v", n, err)
		}
		mustExec(ctx, t, pool, `UPDATE run_held_publications SET state='deleted',live_run_id=NULL WHERE id=$1`, f.pub)
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("not-created publication: rows=%d err=%v", n, err)
		}
		assertHoldOpen(ctx, t, pool, f.hold)
	})
	t.Run("run claimed by another worker", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		other := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status)
			VALUES($1,$2,'held-other',$3,'online')`, other, f.user, other[:])
		mustExec(ctx, t, pool, `UPDATE runs SET worker_id=$2 WHERE id=$1`, f.run, other)
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
		assertHoldOpen(ctx, t, pool, f.hold)
	})
	// The publication's identity columns are immutable, so each mismatch is a differently built row.
	// A generation mismatch is also caught by the ref binding (the ref carries the generation), so
	// this case pins the outcome; the worker case is the one only p.worker_id can refuse.
	t.Run("publication of another generation", func(t *testing.T) {
		f := heldNewHoldFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		gen2 := uuid.New()
		f.insertPreparedAt(ctx, t, gen2, 2, f.worker)
		f.pub = gen2
		f.setState(ctx, t, "created")
		if n, err := f.release(ctx, t, gen2, heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("generation-2 publication: rows=%d err=%v", n, err)
		}
		assertHoldOpen(ctx, t, pool, f.hold)
	})
	t.Run("publication of another worker", func(t *testing.T) {
		f := heldNewHoldFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		f.insertPreparedAt(ctx, t, f.pub, 1, uuid.New())
		f.setState(ctx, t, "created")
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("foreign-worker publication: rows=%d err=%v", n, err)
		}
		assertHoldOpen(ctx, t, pool, f.hold)
	})
	t.Run("publication of another hold", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		g := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		if n, err := f.release(ctx, t, g.pub, heldDigest, heldTip); err != nil || n != 0 {
			t.Fatalf("foreign hold's publication: rows=%d err=%v", n, err)
		}
	})
	t.Run("insert is refused", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", str("agent_failure"))
		// Every CHECK is satisfied, so only the BEFORE INSERT guard can decide.
		tag, err := pool.Exec(ctx, `INSERT INTO recovery_custody_holds
			(user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,released_at,
			 final_disposition,final_publication_id,release_evidence,final_source_sha,final_coverage_digest)
			VALUES($1,$2,$3,7,'released',$4,'held-worker',now(),'held_publication',gen_random_uuid(),'held_publication',$5,$6)`,
			f.user, f.repo, f.run, f.worker, heldTip, heldDigest)
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("held INSERT: rows=%d err=%v", tag.RowsAffected(), err)
		}
		tag, err = pool.Exec(ctx, `INSERT INTO recovery_custody_holds
			(user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
			VALUES($1,$2,$3,8,'open',$4,'held-worker')`, f.user, f.repo, f.run, f.worker)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("ordinary INSERT must still work: rows=%d err=%v", tag.RowsAffected(), err)
		}
	})
}

func assertHoldOpen(ctx context.Context, t *testing.T, pool *pgxpool.Pool, hold uuid.UUID) {
	t.Helper()
	var state string
	var pub *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT state, final_publication_id FROM recovery_custody_holds WHERE id=$1`, hold).Scan(&state, &pub); err != nil {
		t.Fatal(err)
	}
	if state != "open" || pub != nil {
		t.Fatalf("hold changed: state=%s publication=%v", state, pub)
	}
}

// The publication row's own trigger: identity immutability, the transition matrix, the monotonic
// create_invoked_at, acknowledged-needs-a-released-hold, and the live-pointer CHECK.
func TestHeldPublicationGuardLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "held_pub_")
	state := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT state FROM run_held_publications WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	step := func(f heldFixture, set string) (int64, error) {
		tag, err := pool.Exec(ctx, `UPDATE run_held_publications SET `+set+` WHERE id=$1`, f.pub)
		return tag.RowsAffected(), err
	}
	t.Run("transition matrix", func(t *testing.T) {
		legal := map[string][]string{
			"prepared":       {"invoked"},
			"invoked":        {"created", "refused", "create_unknown"},
			"create_unknown": {"created", "refused"},
			"created":        {"acknowledged", "delete_unknown", "deleted", "abandoned"},
			"acknowledged":   {"delete_unknown", "deleted", "abandoned"},
			"delete_unknown": {"deleted", "abandoned"},
			"refused":        nil,
			"deleted":        nil,
			"abandoned":      nil,
		}
		all := []string{"prepared", "invoked", "created", "create_unknown", "refused", "acknowledged", "delete_unknown", "deleted", "abandoned"}
		for from, tos := range legal {
			for _, to := range all {
				if to == from || to == "acknowledged" {
					continue // the acknowledged edge needs a released hold: tested below
				}
				f := heldNewFixture(ctx, t, pool, false, "failed", nil)
				f.setState(ctx, t, from)
				if to == "prepared" {
					continue // the marker is monotonic: covered by its own subtest
				}
				// Satisfy every CHECK for the target state so only the trigger can refuse.
				live := "run_id"
				if to == "refused" || to == "deleted" || to == "abandoned" {
					live = "NULL"
				}
				set := fmt.Sprintf(`state='%s',live_run_id=%s`, to, live)
				if from == "prepared" {
					set += `,create_invoked_at=now()`
				}
				n, err := step(f, set)
				wantLegal := false
				for _, l := range tos {
					wantLegal = wantLegal || l == to
				}
				if wantLegal {
					if err != nil || n != 1 || state(f.pub) != to {
						t.Errorf("%s -> %s refused: rows=%d err=%v state=%s", from, to, n, err, state(f.pub))
					}
					continue
				}
				if err != nil || n != 0 || state(f.pub) != from {
					t.Errorf("%s -> %s illegal edge not refused by the guard: rows=%d err=%v state=%s", from, to, n, err, state(f.pub))
				}
			}
		}
	})
	t.Run("identity is immutable", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, false, "failed", nil)
		for _, set := range []string{
			`tip='` + strings.Repeat("6", 40) + `'`,
			`coverage_digest='` + strings.Repeat("6", 64) + `'`,
			`generation=2`,
			`hold_id=gen_random_uuid()`,
			`worker_id=gen_random_uuid()`,
			`user_id=gen_random_uuid()`,
			`repo_id=gen_random_uuid()`,
			`run_id=gen_random_uuid()`,
		} {
			n, err := step(f, set)
			if err == nil && n != 0 {
				t.Errorf("identity update %q accepted", set)
			}
		}
		var tip string
		if err := pool.QueryRow(ctx, `SELECT tip FROM run_held_publications WHERE id=$1`, f.pub).Scan(&tip); err != nil || tip != heldTip {
			t.Fatalf("tip changed: %s %v", tip, err)
		}
	})
	t.Run("create_invoked_at is monotonic", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, false, "failed", nil)
		f.setState(ctx, t, "invoked")
		var first string
		if err := pool.QueryRow(ctx, `SELECT create_invoked_at::text FROM run_held_publications WHERE id=$1`, f.pub).Scan(&first); err != nil {
			t.Fatal(err)
		}
		if n, err := step(f, `create_invoked_at=create_invoked_at+interval '1 hour'`); err != nil || n != 0 {
			t.Fatalf("moving the marker: rows=%d err=%v", n, err)
		}
		// Rolling the outcome back is also refused: the marker cannot be erased, and the
		// prepared state it would imply is not reachable.
		if n, err := step(f, `state='prepared',create_invoked_at=NULL`); err == nil && n != 0 {
			t.Fatalf("erasing the marker accepted")
		}
		var after string
		if err := pool.QueryRow(ctx, `SELECT create_invoked_at::text FROM run_held_publications WHERE id=$1`, f.pub).Scan(&after); err != nil || after != first || state(f.pub) != "invoked" {
			t.Fatalf("marker changed: %s -> %s state=%s err=%v", first, after, state(f.pub), err)
		}
	})
	t.Run("acknowledged requires a released hold", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, true, "failed", nil)
		ack := `state='acknowledged',acknowledged_at=now(),expires_at=now()+interval '1 day'`
		if n, err := step(f, ack); err != nil || n != 0 || state(f.pub) != "created" {
			t.Fatalf("ack with an open hold: rows=%d err=%v state=%s", n, err, state(f.pub))
		}
		// A hold released against some other publication does not satisfy it either.
		g := heldNewFixture(ctx, t, pool, true, "failed", nil)
		if n, err := g.release(ctx, t, g.pub, heldDigest, heldTip); err != nil || n != 1 {
			t.Fatalf("release other hold: rows=%d err=%v", n, err)
		}
		if n, err := step(f, ack); err != nil || n != 0 {
			t.Fatalf("ack against another hold's release: rows=%d err=%v", n, err)
		}
		if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 1 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
		if n, err := step(f, ack); err != nil || n != 1 || state(f.pub) != "acknowledged" {
			t.Fatalf("ack after the release: rows=%d err=%v state=%s", n, err, state(f.pub))
		}
		// acknowledged_at is immutable once set.
		if n, err := step(f, `acknowledged_at=acknowledged_at+interval '1 hour'`); err != nil || n != 0 {
			t.Fatalf("moving acknowledged_at: rows=%d err=%v", n, err)
		}
	})
	t.Run("refused clears the live pointer", func(t *testing.T) {
		// closeHold releases the unguarded hold so only the publication row can pin the run.
		closeHold := func(f heldFixture) {
			mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
				release_evidence='publication',released_at=now() WHERE id=$1`, f.hold)
		}
		f := heldNewFixture(ctx, t, pool, false, "failed", nil)
		f.setState(ctx, t, "invoked")
		// Keeping the pointer on a refused row is a CHECK violation.
		_, err := step(f, `state='refused',refusal_reason='create_refused'`)
		heldCheckName(t, err, "run_held_publications_live_pointer_check")
		if n, err := step(f, `state='refused',refusal_reason='create_refused',live_run_id=NULL`); err != nil || n != 1 {
			t.Fatalf("refused with the pointer cleared: rows=%d err=%v", n, err)
		}
		// A refused row never created a ref, so it no longer pins its run; a live one does.
		closeHold(f)
		if _, err := pool.Exec(ctx, `DELETE FROM runs WHERE id=$1`, f.run); err != nil {
			t.Fatalf("a refused publication must not pin the run: %v", err)
		}
		g := heldNewFixture(ctx, t, pool, false, "failed", nil)
		closeHold(g)
		_, err = pool.Exec(ctx, `DELETE FROM runs WHERE id=$1`, g.run)
		heldCheckName(t, err, "run_held_publications_live_run_id_fkey")
	})
	t.Run("unique per run and generation", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, false, "failed", nil)
		_, err := pool.Exec(ctx, `INSERT INTO run_held_publications
			(run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
			VALUES($1::uuid,1,$2,$3,$4,$5,$1::uuid,'refs/uzi-held/'||$1::uuid::text||'/1',$6,$7,'prepared')`,
			f.run, f.hold, f.user, f.repo, f.worker, heldTip, heldDigest)
		heldCheckName(t, err, "run_held_publications_run_generation_key")
		_, err = pool.Exec(ctx, `INSERT INTO run_held_publications
			(run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
			VALUES($1::uuid,2,$2,$3,$4,$5,$1::uuid,'refs/uzi-held/elsewhere',$6,$7,'prepared')`,
			f.run, f.hold, f.user, f.repo, f.worker, heldTip, heldDigest)
		heldCheckName(t, err, "run_held_publications_ref_check")
	})
}

// A terminal row is frozen, the INSERT guard admits only a prepared row, and the DELETE guard
// keeps every row that is or may be the only record of a remote ref.
func TestHeldPublicationLifecycleGuardsLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "held_life_")
	t.Run("terminal rows are frozen", func(t *testing.T) {
		for _, term := range []string{"refused", "deleted", "abandoned"} {
			f := heldNewFixture(ctx, t, pool, false, "failed", nil)
			f.setState(ctx, t, term)
			tag, err := pool.Exec(ctx, `UPDATE run_held_publications SET last_error='late' WHERE id=$1`, f.pub)
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("%s row updated: rows=%d err=%v", term, tag.RowsAffected(), err)
			}
		}
		// A live row still takes a same-state update, so the refusal above is the freeze.
		f := heldNewFixture(ctx, t, pool, false, "failed", nil)
		tag, err := pool.Exec(ctx, `UPDATE run_held_publications SET last_error='ok' WHERE id=$1`, f.pub)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("live same-state update: rows=%d err=%v", tag.RowsAffected(), err)
		}
	})
	t.Run("insert admits only a prepared row", func(t *testing.T) {
		f := heldNewFixture(ctx, t, pool, false, "failed", nil)
		for _, state := range []string{"invoked", "created", "acknowledged", "deleted"} {
			live := "$1"
			if state == "deleted" {
				live = "NULL"
			}
			tag, err := pool.Exec(ctx, `INSERT INTO run_held_publications
				(run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state,
				 create_invoked_at,acknowledged_at,expires_at)
				VALUES($1,9,$2,$3,$4,$5,`+live+`,'refs/uzi-held/'||$1::uuid::text||'/9',$6,$7,'`+state+`',
				 now(),CASE WHEN '`+state+`' IN ('acknowledged','deleted') THEN now() END,
				 CASE WHEN '`+state+`'='acknowledged' THEN now()+interval '1 day' END)`,
				f.run, f.hold, f.user, f.repo, f.worker, heldTip, heldDigest)
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("born-%s row inserted: rows=%d err=%v", state, tag.RowsAffected(), err)
			}
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_held_publications WHERE run_id=$1`, f.run).Scan(&n); err != nil || n != 1 {
			t.Fatalf("rows for the run: %d %v", n, err)
		}
	})
	t.Run("delete keeps live rows", func(t *testing.T) {
		for _, live := range []string{"invoked", "created", "create_unknown", "acknowledged", "delete_unknown"} {
			f := heldNewFixture(ctx, t, pool, false, "failed", nil)
			f.setState(ctx, t, live)
			tag, err := pool.Exec(ctx, `DELETE FROM run_held_publications WHERE id=$1`, f.pub)
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("%s row deleted: rows=%d err=%v", live, tag.RowsAffected(), err)
			}
		}
		for _, safe := range []string{"prepared", "refused", "deleted", "abandoned"} {
			f := heldNewFixture(ctx, t, pool, false, "failed", nil)
			f.setState(ctx, t, safe)
			tag, err := pool.Exec(ctx, `DELETE FROM run_held_publications WHERE id=$1`, f.pub)
			if err != nil || tag.RowsAffected() != 1 {
				t.Errorf("%s row not deletable: rows=%d err=%v", safe, tag.RowsAffected(), err)
			}
		}
	})
}

// setState forces the fixture's publication into state with the columns that state's CHECKs need,
// through the guard-free path (the trigger is disabled for the statement).
func (f heldFixture) setState(ctx context.Context, t *testing.T, state string) {
	t.Helper()
	live := "live_run_id=run_id"
	if state == "refused" || state == "deleted" || state == "abandoned" {
		live = "live_run_id=NULL"
	}
	marker := "create_invoked_at=COALESCE(create_invoked_at,now())"
	if state == "prepared" {
		marker = "create_invoked_at=NULL"
	}
	extra := ""
	switch state {
	case "acknowledged":
		extra = ",acknowledged_at=now(),expires_at=now()+interval '1 day'"
	case "delete_unknown", "deleted", "abandoned":
		extra = ",acknowledged_at=now()"
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, sql := range []string{
		`ALTER TABLE run_held_publications DISABLE TRIGGER held_publication_guard`,
		fmt.Sprintf(`UPDATE run_held_publications SET state='%s',%s,%s%s WHERE id='%s'`, state, live, marker, extra, f.pub),
		`ALTER TABLE run_held_publications ENABLE TRIGGER held_publication_guard`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Down refuses while a publication row may own a remote ref, then removes every held object and
// restores the narrower schema; re-applying Up works.
func TestHeldPublicationRollbackLiveDB(t *testing.T) {
	ctx, dsn, pool := heldMigratedPool(t, "held_rollback_")
	f := heldNewFixture(ctx, t, pool, true, "failed", nil)
	if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 1 {
		t.Fatalf("release: rows=%d err=%v", n, err)
	}
	mustExec(ctx, t, pool, `UPDATE run_held_publications SET state='acknowledged',acknowledged_at=now(),
		expires_at=now()+interval '1 day' WHERE id=$1`, f.pub)
	if err := store.MigrateDownTo(ctx, dsn, 320); err == nil {
		t.Fatal("Down accepted a live acknowledged publication")
	}
	var applied bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('run_held_publications') IS NOT NULL`).Scan(&applied); err != nil || !applied {
		t.Fatalf("refused Down changed the schema: %v %v", applied, err)
	}
	mustExec(ctx, t, pool, `UPDATE run_held_publications SET state='deleted',live_run_id=NULL,deleted_at=now() WHERE id=$1`, f.pub)
	if err := store.MigrateDownTo(ctx, dsn, 320); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var column, table, guards int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_name='recovery_custody_holds' AND column_name='final_publication_id'),
		(SELECT count(*) FROM pg_class WHERE relname='run_held_publications'),
		(SELECT count(*) FROM pg_proc WHERE proname LIKE 'held_publication_%')`).Scan(&column, &table, &guards); err != nil || column+table+guards != 0 {
		t.Fatalf("held objects remain: column=%d table=%d functions=%d err=%v", column, table, guards, err)
	}
	var body, enabled string
	if err := pool.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE oid='recovery_inventory_hold_guard'::regproc`).Scan(&body); err != nil || strings.Contains(body, "held_publication") {
		t.Fatalf("restored guard still knows held_publication: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger
		WHERE tgrelid='recovery_custody_holds'::regclass AND tgname='recovery_inventory_hold_guard'`).Scan(&enabled); err != nil || enabled != "O" {
		t.Fatalf("inventory guard disabled after Down: %q %v", enabled, err)
	}
	var disposition, evidence, source *string
	if err := pool.QueryRow(ctx, `SELECT final_disposition, release_evidence, final_source_sha
		FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&disposition, &evidence, &source); err != nil ||
		disposition != nil || evidence != nil || source != nil {
		t.Fatalf("held classification survived Down: %v %v %v err=%v", disposition, evidence, source, err)
	}
	_, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET final_disposition='held_publication' WHERE id=$1`, f.hold)
	heldCheckName(t, err, "recovery_custody_holds_final_disposition_check")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("re-up: %v", err)
	}
}
