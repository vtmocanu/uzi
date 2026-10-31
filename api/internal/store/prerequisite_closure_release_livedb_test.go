package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The prerequisiteFixture capture is created with these values.
var (
	closureSource = strings.Repeat("a", 40)
	closureDigest = strings.Repeat("a", 64)
)

func closureReleaseParams(f prerequisiteFixture, closure uuid.UUID) store.ReleaseFinalClosureHoldParams {
	return store.ReleaseFinalClosureHoldParams{FinalCaptureID: f.capture, FinalSourceSha: closureSource,
		FinalCoverageDigest: closureDigest, ClosureID: closure, ID: f.hold, RunID: f.run, UserID: f.user,
		Generation: 1, WorkerID: f.worker}
}

// closureStamp marks the capture as FINAL-protected without going through a query, so a
// negative case varies exactly one other predicate.
func closureStamp(t *testing.T, ctx context.Context, f prerequisiteFixture) {
	t.Helper()
	mustExec(ctx, t, f.pool, "UPDATE recovery_captures SET local_replica_worker_id=$2,ready_retention_seconds=3600 WHERE id=$1",
		f.capture, f.worker)
}

// closureReady is a pinned closure whose capture was protected by the real query.
func closureReady(t *testing.T, ctx context.Context, f prerequisiteFixture) prerequisiteFixture {
	t.Helper()
	prerequisitePin(t, ctx, f)
	n, err := store.New(f.pool).ProtectFinalClosureCapture(ctx, store.ProtectFinalClosureCaptureParams{
		WorkerID: f.worker, RetentionSeconds: 3600, ID: f.capture, HoldID: f.hold, SourceSha: closureSource,
		CoverageDigest: prerequisiteText(closureDigest), ClosureID: f.closure.ID})
	prerequisiteRows(t, 1, n, err)
	return f
}

func closureHoldSnapshot(t *testing.T, ctx context.Context, f prerequisiteFixture) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(ctx, "SELECT to_jsonb(h)::text FROM recovery_custody_holds h WHERE id=$1", f.hold).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// closureRefused fails unless a write was refused by 0 rows or by a guard/constraint error.
func closureRefused(t *testing.T, what string, n int64, err error) {
	t.Helper()
	if err != nil {
		// The merged predecessor guard raises P0001 for any discard it does not recognise.
		var p *pgconn.PgError
		if errors.As(err, &p) && p.Code == "P0001" {
			return
		}
		prerequisiteError(t, err)
		return
	}
	if n != 0 {
		t.Fatalf("%s: affected %d rows, want a refusal", what, n)
	}
}

func closureRelease(t *testing.T, ctx context.Context, f prerequisiteFixture, closure uuid.UUID) {
	t.Helper()
	n, err := store.New(f.pool).ReleaseFinalClosureHold(ctx, closureReleaseParams(f, closure))
	if err != nil || n != 0 {
		t.Fatalf("release must be refused with 0 rows: rows=%d err=%v", n, err)
	}
}

// closureRaw is the release as plain SQL, so each argument can be varied independently.
func closureRaw(ctx context.Context, f prerequisiteFixture, disposition string, capture, evidence, closure any) (int64, error) {
	tag, err := f.pool.Exec(ctx, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
        final_disposition=$2,final_capture_id=$3,final_source_sha=$4,final_coverage_digest=$5,
        release_evidence=$6,final_closure_id=$7,released_at=now() WHERE id=$1`,
		f.hold, disposition, capture, closureSource, closureDigest, evidence, closure)
	return tag.RowsAffected(), err
}

func TestPrerequisiteClosureReleaseLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "closure_release_")
	f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
	q := store.New(pool)
	n, err := q.ReleaseFinalClosureHold(ctx, closureReleaseParams(f, f.closure.ID))
	prerequisiteRows(t, 1, n, err)
	var evidence, disposition string
	var capture, closure uuid.UUID
	var state string
	if err = pool.QueryRow(ctx, `SELECT state,release_evidence,final_disposition,final_capture_id,final_closure_id
        FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&state, &evidence, &disposition, &capture, &closure); err != nil {
		t.Fatal(err)
	}
	if state != "released" || evidence != "prerequisite_closure" || disposition != "archive" ||
		capture != f.capture || closure != f.closure.ID {
		t.Fatalf("released shape: %s %s %s %s %s", state, evidence, disposition, capture, closure)
	}
	if c := prerequisiteRead(t, ctx, f); c.State != "pinned" {
		t.Fatalf("release changed the closure: %s", c.State)
	}
	// A replay is a no-op.
	closureRelease(t, ctx, f, f.closure.ID)
	// FINAL evidence is immutable, and the closure it names cannot be deleted.
	before := closureHoldSnapshot(t, ctx, f)
	for name, set := range map[string]string{
		"clear id":      "final_closure_id=NULL",
		"clear both":    "final_closure_id=NULL,release_evidence='archive'",
		"other id":      "final_closure_id=gen_random_uuid()",
		"other capture": "final_capture_id=gen_random_uuid()",
	} {
		tag, err := pool.Exec(ctx, "UPDATE recovery_custody_holds SET "+set+" WHERE id=$1", f.hold)
		closureRefused(t, name, tag.RowsAffected(), err)
	}
	_, err = pool.Exec(ctx, "DELETE FROM recovery_prerequisite_closures WHERE id=$1", f.closure.ID)
	prerequisiteError(t, err)
	// Policy: a released closure hold cannot be re-dispositioned into a discard that
	// drops or keeps the id. This is unreachable from production code, which never
	// discards a released hold; the guard still pins it.
	for name, set := range map[string]string{
		"owner_discard": "state='discarded',release_evidence='owner_discard'",
		"merged_predecessor_discard": `state='discarded',release_evidence='merged_predecessor_discard',
            merged_predecessor_discard_binding='{}'::jsonb`,
	} {
		tag, err := pool.Exec(ctx, "UPDATE recovery_custody_holds SET "+set+" WHERE id=$1", f.hold)
		closureRefused(t, name, tag.RowsAffected(), err)
	}
	if after := closureHoldSnapshot(t, ctx, f); after != before {
		t.Fatalf("FINAL evidence changed:\n%s\n%s", before, after)
	}
}

func TestPrerequisiteClosureReleaseNegativesLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "closure_release_neg_")
	q := store.New(pool)
	check := func(t *testing.T, f prerequisiteFixture, closure uuid.UUID) {
		t.Helper()
		before := closureHoldSnapshot(t, ctx, f)
		closureRelease(t, ctx, f, closure)
		if after := closureHoldSnapshot(t, ctx, f); after != before {
			t.Fatalf("refused release changed the hold:\n%s\n%s", before, after)
		}
	}
	t.Run("wrong id", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		check(t, f, uuid.New())
	})
	t.Run("another hold's closure", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		g := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		check(t, f, g.closure.ID)
	})
	t.Run("another capture's closure on the same hold", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		second := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO recovery_captures
            SELECT (jsonb_populate_record(NULL::recovery_captures,to_jsonb(c) ||
                jsonb_build_object('id',$2::uuid,'idempotency_key',$2::uuid::text))).*
            FROM recovery_captures c WHERE id=$1`, f.capture, second)
		other := f
		other.capture = second
		var err error
		if other.closure, err = q.InsertPrerequisiteClosure(ctx, second); err != nil {
			t.Fatal(err)
		}
		prerequisitePin(t, ctx, other)
		check(t, f, other.closure.ID)
	})
	for _, state := range []string{"pending", "invoked", "create_unknown", "refused", "foreign", "lost"} {
		t.Run("closure "+state, func(t *testing.T) {
			f := prerequisiteNew(t, ctx, pool)
			prerequisiteSetState(t, ctx, f, state)
			closureStamp(t, ctx, f)
			check(t, f, f.closure.ID)
		})
	}
	t.Run("closure absent_since set", func(t *testing.T) {
		f := prerequisiteNew(t, ctx, pool)
		prerequisiteSetState(t, ctx, f, "pinned") // records an absence
		if c := prerequisiteRead(t, ctx, f); c.State != "pinned" || !c.AbsentSince.Valid {
			t.Fatalf("fixture: %+v", c)
		}
		closureStamp(t, ctx, f)
		check(t, f, f.closure.ID)
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		mustExec(ctx, t, pool, "UPDATE recovery_captures SET checksum=repeat('e',64) WHERE id=$1", f.capture)
		check(t, f, f.closure.ID)
	})
	t.Run("prerequisite mismatch", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		mustExec(ctx, t, pool, "UPDATE recovery_captures SET prerequisite_shas=ARRAY[repeat('b',40)] WHERE id=$1", f.capture)
		check(t, f, f.closure.ID)
	})
	t.Run("protection missing", func(t *testing.T) {
		f := prerequisiteNew(t, ctx, pool)
		prerequisitePin(t, ctx, f)
		check(t, f, f.closure.ID)
	})
	t.Run("capture expired", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		mustExec(ctx, t, pool, "UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1", f.capture)
		check(t, f, f.closure.ID)
	})
	t.Run("run still owns the generation", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		mustExec(ctx, t, pool, "UPDATE runs SET status='claimed' WHERE id=$1", f.run)
		check(t, f, f.closure.ID)
	})
	// Owner-only parks never release this way; other causes still do.
	for _, cause := range []string{"worker_requeue_exhausted", "worker_memory_pressure",
		"retained_recovery_exhausted", "retained_recovery_adoption_failed", "forge_unreachable"} {
		t.Run("recovery_wait "+cause, func(t *testing.T) {
			f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
			mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=2,worker_id=NULL,status='recovery_wait',recovery_wait_cause=$2 WHERE id=$1",
				f.run, cause)
			if cause == "forge_unreachable" {
				n, err := q.ReleaseFinalClosureHold(ctx, closureReleaseParams(f, f.closure.ID))
				prerequisiteRows(t, 1, n, err)
				return
			}
			check(t, f, f.closure.ID)
		})
	}
	t.Run("plain SQL control", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		n, err := closureRaw(ctx, f, "archive", f.capture, "prerequisite_closure", f.closure.ID)
		prerequisiteRows(t, 1, n, err)
	})
	t.Run("plain SQL variations", func(t *testing.T) {
		for name, vary := range map[string]func(f prerequisiteFixture) (int64, error){
			"closure evidence without id": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "archive", f.capture, "prerequisite_closure", nil)
			},
			"id with archive evidence": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "archive", f.capture, "archive", f.closure.ID)
			},
			"id with no evidence": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "archive", f.capture, nil, f.closure.ID)
			},
			"id with settled disposition": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "settled", nil, "prerequisite_closure", f.closure.ID)
			},
			"id with null capture": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "archive", nil, "prerequisite_closure", f.closure.ID)
			},
			"nonexistent id with null capture": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "archive", nil, "prerequisite_closure", uuid.New())
			},
			"nonexistent id": func(f prerequisiteFixture) (int64, error) {
				return closureRaw(ctx, f, "archive", f.capture, "prerequisite_closure", uuid.New())
			},
			"another hold's closure": func(f prerequisiteFixture) (int64, error) {
				g := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
				return closureRaw(ctx, f, "archive", f.capture, "prerequisite_closure", g.closure.ID)
			},
			"owner_discard carrying id": func(f prerequisiteFixture) (int64, error) {
				tag, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,
                    live_run_id=NULL,release_evidence='owner_discard',final_closure_id=$2,released_at=now() WHERE id=$1`,
					f.hold, f.closure.ID)
				return tag.RowsAffected(), err
			},
			"merged_predecessor_discard carrying id": func(f prerequisiteFixture) (int64, error) {
				tag, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,
                    live_run_id=NULL,release_evidence='merged_predecessor_discard',
                    merged_predecessor_discard_binding='{}'::jsonb,final_closure_id=$2,released_at=now() WHERE id=$1`,
					f.hold, f.closure.ID)
				return tag.RowsAffected(), err
			},
		} {
			t.Run(name, func(t *testing.T) {
				f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
				before := closureHoldSnapshot(t, ctx, f)
				n, err := vary(f)
				closureRefused(t, name, n, err)
				if after := closureHoldSnapshot(t, ctx, f); after != before {
					t.Fatalf("refused write changed the hold:\n%s\n%s", before, after)
				}
			})
		}
	})
	t.Run("id on a non-guarded hold", func(t *testing.T) {
		donor := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		u := heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
		tag, err := pool.Exec(ctx, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
            final_disposition='archive',release_evidence='prerequisite_closure',final_closure_id=$2,released_at=now() WHERE id=$1`,
			u.hold, donor.closure.ID)
		closureRefused(t, "non-guarded hold", tag.RowsAffected(), err)
	})
	t.Run("INSERT carrying closure evidence", func(t *testing.T) {
		f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
		insert := func(generation int, extra string) (int64, error) {
			tag, err := pool.Exec(ctx, `INSERT INTO recovery_custody_holds
                SELECT (jsonb_populate_record(NULL::recovery_custody_holds,to_jsonb(h) ||
                    jsonb_build_object('id',gen_random_uuid(),'generation',$2::int`+extra+`))).*
                FROM recovery_custody_holds h WHERE id=$1`, f.hold, generation)
			return tag.RowsAffected(), err
		}
		// Control: the same copy without closure columns inserts.
		// A hold copy carries the old generation's run, so a failure here is the fixture.
		n, err := insert(2, "")
		prerequisiteRows(t, 1, n, err)
		n, err = insert(3, `,'final_closure_id','`+f.closure.ID.String()+`'`)
		closureRefused(t, "INSERT with id", n, err)
		n, err = insert(4, `,'release_evidence','prerequisite_closure'`)
		closureRefused(t, "INSERT with evidence", n, err)
	})
}

func TestPrerequisiteClosureReleaseZeroPrerequisiteLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "closure_release_zero_")
	q := store.New(pool)
	f := heldNewHoldFixture(ctx, t, pool, true, "failed", nil)
	capture := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures
        (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,
         state,manifest_bound,coverage_digest,expires_at)
        VALUES($1,$2,$3,$4,$5,'held-worker',repeat('a',40),'zero','available',true,repeat('a',64),now()+interval '1 day')`,
		capture, f.hold, f.run, f.user, f.worker)
	n, err := q.ProtectFinalInventoryCapture(ctx, store.ProtectFinalInventoryCaptureParams{WorkerID: f.worker,
		RetentionSeconds: 3600, ID: capture, HoldID: f.hold, SourceSha: closureSource,
		CoverageDigest: prerequisiteText(closureDigest)})
	prerequisiteRows(t, 1, n, err)
	// A capture without prerequisites cannot take the closure path.
	n, err = q.ReleaseFinalClosureHold(ctx, store.ReleaseFinalClosureHoldParams{FinalCaptureID: capture,
		FinalSourceSha: closureSource, FinalCoverageDigest: closureDigest, ClosureID: uuid.New(), ID: f.hold,
		RunID: f.run, UserID: f.user, Generation: 1, WorkerID: f.worker})
	prerequisiteRows(t, 0, n, err)
	n, err = q.ReleaseFinalInventoryHold(ctx, store.ReleaseFinalInventoryHoldParams{ID: f.hold, RunID: f.run,
		UserID: f.user, WorkerID: f.worker, Generation: 1, FinalDisposition: "archive",
		FinalCaptureID: prerequisiteUUID(capture), FinalSourceSha: prerequisiteText(closureSource),
		FinalCoverageDigest: closureDigest, ReleaseEvidence: "archive"})
	prerequisiteRows(t, 1, n, err)
	var closure pgtype.UUID
	if err = pool.QueryRow(ctx, "SELECT final_closure_id FROM recovery_custody_holds WHERE id=$1", f.hold).Scan(&closure); err != nil || closure.Valid {
		t.Fatalf("plain archive must not carry a closure: %v %v", closure, err)
	}
}

func TestPrerequisiteFinalListingReservationLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "closure_final_listing_")
	q := store.New(pool)
	reserve := func(f prerequisiteFixture, pass pgtype.UUID, generation int32) error {
		_, err := q.ReservePrerequisiteFinalListing(ctx, store.ReservePrerequisiteFinalListingParams{
			PassID: pass, ID: f.closure.ID, Generation: generation})
		return err
	}
	pinned := func(t *testing.T) prerequisiteFixture {
		f := prerequisiteNew(t, ctx, pool)
		prerequisitePin(t, ctx, f)
		return f
	}
	t.Run("reserves a pass and leaves the schedule alone", func(t *testing.T) {
		f := pinned(t)
		// The audit is not due: the listing is independent of the audit schedule.
		mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET audit_next_at=now()+interval '1 day' WHERE id=$1", f.closure.ID)
		before := prerequisiteRead(t, ctx, f)
		pass := prerequisiteUUID(uuid.New())
		got, err := q.ReservePrerequisiteFinalListing(ctx, store.ReservePrerequisiteFinalListingParams{
			PassID: pass, ID: f.closure.ID, Generation: before.CleanupAttempts})
		if err != nil {
			t.Fatal(err)
		}
		if got.LastAuditPassID != pass {
			t.Fatalf("pass not reserved: %+v", got.LastAuditPassID)
		}
		if got.AuditNextAt != before.AuditNextAt || got.Attempts != before.Attempts ||
			got.CleanupAttempts != before.CleanupAttempts {
			t.Fatalf("schedule changed: %+v -> %+v", before, got)
		}
		want := before
		want.LastAuditPassID = pass
		want.UpdatedAt = got.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected change:\n%+v\n%+v", want, got)
		}
		// The same pass cannot be reserved twice; a new one can.
		if err = reserve(f, pass, before.CleanupAttempts); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("same pass twice: %v", err)
		}
		if err = reserve(f, prerequisiteUUID(uuid.New()), before.CleanupAttempts); err != nil {
			t.Fatalf("fresh pass: %v", err)
		}
	})
	refusals := map[string]func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32){
		"wrong generation": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			return pinned(t), prerequisiteUUID(uuid.New()), 1
		},
		"zero pass id": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			return pinned(t), prerequisiteUUID(uuid.Nil), 0
		},
		"null pass id": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			return pinned(t), pgtype.UUID{}, 0
		},
		"absent_since set": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			f := prerequisiteNew(t, ctx, pool)
			prerequisiteSetState(t, ctx, f, "pinned")
			return f, prerequisiteUUID(uuid.New()), 0
		},
		"not pinned": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			return prerequisiteNew(t, ctx, pool), prerequisiteUUID(uuid.New()), 0
		},
		"capture expired": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			f := pinned(t)
			mustExec(ctx, t, pool, "UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1", f.capture)
			return f, prerequisiteUUID(uuid.New()), 0
		},
		"capture not available": func(t *testing.T) (prerequisiteFixture, pgtype.UUID, int32) {
			f := pinned(t)
			mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='expired' WHERE id=$1", f.capture)
			return f, prerequisiteUUID(uuid.New()), 0
		},
	}
	for name, setup := range refusals {
		t.Run(name, func(t *testing.T) {
			f, pass, generation := setup(t)
			before := prerequisiteRead(t, ctx, f)
			if err := reserve(f, pass, generation); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("want ErrNoRows, got %v", err)
			}
			if after := prerequisiteRead(t, ctx, f); !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed the closure:\n%+v\n%+v", before, after)
			}
		})
	}
}

func closureHoldRows(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(h) - 'final_closure_id' ORDER BY id)::text,'[]')
        FROM recovery_custody_holds h`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPrerequisiteClosureReleaseMigrationRoundTripLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "closure_release_rt_")
	if err := store.MigrateTo(ctx, dsn, 388); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	prerequisiteSchemaVersion(t, ctx, pool, 388)
	heldNewHoldFixture(ctx, t, pool, true, "failed", nil)
	heldNewHoldFixture(ctx, t, pool, false, "failed", nil)
	released := heldNewHoldFixture(ctx, t, pool, true, "completed", nil)
	mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
        final_disposition='settled',release_evidence='publication',
        final_coverage_digest='e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
        released_at='2026-01-02 03:04:05+00' WHERE id=$1`, released.hold)
	mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
        final_disposition='no_adopted_source',release_evidence='no_adopted_source',
        released_at='2026-01-02 03:04:05+00' WHERE id=$1`, heldNewHoldFixture(ctx, t, pool, false, "failed", nil).hold)
	want := closureHoldRows(t, ctx, pool)
	if want == "[]" {
		t.Fatal("no hold rows were seeded")
	}
	for round := 0; round < 2; round++ {
		if err = store.MigrateTo(ctx, dsn, 389); err != nil {
			t.Fatal(err)
		}
		prerequisiteSchemaVersion(t, ctx, pool, 389)
		if got := closureHoldRows(t, ctx, pool); got != want {
			t.Fatalf("Up changed hold rows:\n%s\n%s", want, got)
		}
		var ids int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM recovery_custody_holds WHERE final_closure_id IS NOT NULL").Scan(&ids); err != nil || ids != 0 {
			t.Fatalf("Up invented closure ids: %d %v", ids, err)
		}
		if err = store.MigrateDownTo(ctx, dsn, 388); err != nil {
			t.Fatal(err)
		}
		prerequisiteSchemaVersion(t, ctx, pool, 388)
		if got := closureHoldRows(t, ctx, pool); got != want {
			t.Fatalf("Down changed hold rows:\n%s\n%s", want, got)
		}
		var cols int
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
            WHERE table_name='recovery_custody_holds' AND column_name='final_closure_id'`).Scan(&cols); err != nil || cols != 0 {
			t.Fatalf("Down left the column: %d %v", cols, err)
		}
	}
	if err = store.MigrateTo(ctx, dsn, 389); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 389)
}

func TestPrerequisiteClosureReleaseDownRefusalLiveDB(t *testing.T) {
	ctx, dsn, pool := prerequisiteMigratedPool(t, "closure_release_down_")
	if err := store.MigrateDownTo(ctx, dsn, 389); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 389)
	f := closureReady(t, ctx, prerequisiteNew(t, ctx, pool))
	// A pinned closure without release evidence rolls back and forward cleanly.
	if err := store.MigrateDownTo(ctx, dsn, 388); err != nil {
		t.Fatalf("Down with an unreleased closure: %v", err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 388)
	if err := store.MigrateTo(ctx, dsn, 389); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 389)
	n, err := store.New(pool).ReleaseFinalClosureHold(ctx, closureReleaseParams(f, f.closure.ID))
	prerequisiteRows(t, 1, n, err)
	before := closureHoldSnapshot(t, ctx, f)
	err = store.MigrateDownTo(ctx, dsn, 388)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
		t.Fatalf("Down must refuse with SQLSTATE 55000, got %v", err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 389)
	if after := closureHoldSnapshot(t, ctx, f); after != before {
		t.Fatalf("refused Down changed the hold:\n%s\n%s", before, after)
	}
	if c := prerequisiteRead(t, ctx, f); c.State != "pinned" {
		t.Fatalf("refused Down changed the closure: %s", c.State)
	}
}
