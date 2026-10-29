package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPrerequisiteReservationNullDeadlinesLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_null_")
	for _, cleanup := range []bool{false, true} {
		name := "preparation"
		if cleanup {
			name = "cleanup"
		}
		t.Run(name, func(t *testing.T) {
			f := prerequisiteNew(t, ctx, pool)
			statement := "UPDATE recovery_prerequisite_closures SET attempts=attempts+1 WHERE id=$1"
			if cleanup {
				prerequisitePin(t, ctx, f)
				mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
				statement = "UPDATE recovery_prerequisite_closures SET cleanup_attempts=cleanup_attempts+1 WHERE id=$1"
			}
			_, err := pool.Exec(ctx, statement, f.closure.ID)
			if err == nil {
				t.Fatalf("NULL %s deadline admitted a generation", name)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
				t.Fatalf("NULL %s deadline: want SQLSTATE 55000, got %v", name, err)
			}
			c := prerequisiteRead(t, ctx, f)
			if (!cleanup && (c.Attempts != 0 || c.AttemptDeadline.Valid)) ||
				(cleanup && (c.CleanupAttempts != 0 || c.CleanupDeadline.Valid)) {
				t.Fatalf("rejected NULL reservation changed row: %+v", c)
			}
		})
	}
}

func prerequisitePreparedMarker(t *testing.T, ctx context.Context, f prerequisiteFixture) store.MarkPrerequisiteInvokedParams {
	t.Helper()
	var connection uuid.UUID
	if err := f.pool.QueryRow(ctx, "SELECT connection_id FROM repos WHERE id=$1", f.repo).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	return store.MarkPrerequisiteInvokedParams{
		ID: f.closure.ID, Generation: prerequisiteRead(t, ctx, f).Attempts, SendSeconds: 2,
		PinTip: prerequisiteText(heldTip), ConnectionID: prerequisiteUUID(connection),
		ForgeKind: prerequisiteText("github"), NormalizedBaseUrl: prerequisiteText("https://example.com"),
		ProjectID: pgtype.Int8{Int64: 1, Valid: true}, DefaultBranch: prerequisiteText("main"),
	}
}

func TestPrerequisiteMarkerPredicateFencesLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_marker_fences_")
	for _, scenario := range []string{"generation", "expired_lease", "connection", "forge_kind", "base_url", "project", "default_branch", "closed_hold", "unavailable_capture"} {
		t.Run(scenario, func(t *testing.T) {
			f := prerequisiteNew(t, ctx, pool)
			prerequisiteReserve(t, ctx, f, 60)
			args := prerequisitePreparedMarker(t, ctx, f)
			switch scenario {
			case "generation":
				args.Generation++
			case "expired_lease":
				prerequisiteAdvance(t, ctx, pool, 61*time.Second)
			case "connection":
				args.ConnectionID = prerequisiteUUID(uuid.New())
			case "forge_kind":
				mustExec(ctx, t, pool, "UPDATE forge_connections SET forge_type='gitlab' WHERE id=$1", uuid.UUID(args.ConnectionID.Bytes))
			case "base_url":
				mustExec(ctx, t, pool, "UPDATE forge_connections SET base_url='https://drift.example.com' WHERE id=$1", uuid.UUID(args.ConnectionID.Bytes))
			case "project":
				mustExec(ctx, t, pool, "UPDATE repos SET forge_project_id=2 WHERE id=$1", f.repo)
			case "default_branch":
				mustExec(ctx, t, pool, "UPDATE repos SET default_branch='other' WHERE id=$1", f.repo)
			case "closed_hold":
				n, err := store.New(pool).DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: f.hold, RunID: f.run, UserID: f.user, ReleaseEvidence: prerequisiteText("owner_discard")})
				prerequisiteRows(t, 1, n, err)
			case "unavailable_capture":
				mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
			}
			before := prerequisiteRead(t, ctx, f)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := store.New(tx)
			if _, err = q.LockPrerequisiteRun(ctx, f.run); err != nil {
				t.Fatal(err)
			}
			if _, err = q.LockPrerequisiteHold(ctx, f.hold); err != nil {
				t.Fatal(err)
			}
			if _, err = q.LockPrerequisiteCapture(ctx, f.capture); err != nil {
				t.Fatal(err)
			}
			if _, err = q.LockPrerequisiteClosure(ctx, f.closure.ID); err != nil {
				t.Fatal(err)
			}
			_, markerErr := q.MarkPrerequisiteInvoked(ctx, args)
			if !errors.Is(markerErr, pgx.ErrNoRows) {
				t.Fatalf("%s marker: want pgx.ErrNoRows, got %v", scenario, markerErr)
			}
			after, readErr := q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: f.capture, UserID: f.user})
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("%s rejected marker changed transactional pending evidence: %v", scenario, readErr)
			}
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrerequisiteAbsenceTenMinuteFenceLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_absence_boundary_")
	f := prerequisiteNew(t, ctx, pool)
	prerequisitePin(t, ctx, f)
	q := store.New(pool)
	first := prerequisiteAudit(t, ctx, f)
	n, err := q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: f.closure.ID, Generation: 0, PassID: first})
	prerequisiteRows(t, 1, n, err)
	prerequisiteAdvance(t, ctx, pool, 9*time.Minute)
	// A fresh reserved different pass makes the time fence the only failing predicate.
	early := prerequisiteAudit(t, ctx, f)
	before := prerequisiteRead(t, ctx, f)
	n, err = q.ConfirmPrerequisiteLost(ctx, store.ConfirmPrerequisiteLostParams{ID: f.closure.ID, Generation: 0, PassID: early})
	prerequisiteRows(t, 0, n, err)
	if after := prerequisiteRead(t, ctx, f); !reflect.DeepEqual(before, after) {
		t.Fatal("early absence confirmation changed evidence")
	}
	// Exercise the trigger independently of the query's zero-row time predicate.
	assertRawLossRefused := func(pass pgtype.UUID, label string) {
		t.Helper()
		_, rawErr := pool.Exec(ctx, "UPDATE recovery_prerequisite_closures SET state='lost',live_run_id=NULL,reason='pin_lost',absent_since=NULL,absence_pass_id=NULL,last_audit_pass_id=$2 WHERE id=$1", f.closure.ID, pass)
		var pgErr *pgconn.PgError
		if !errors.As(rawErr, &pgErr) || pgErr.Code != "55000" || !strings.Contains(rawErr.Error(), "later confirming pass after ten minutes") {
			t.Fatalf("%s loss bypassed absence trigger: %v", label, rawErr)
		}
	}
	assertRawLossRefused(early, "early")
	prerequisiteAdvance(t, ctx, pool, time.Minute)
	assertRawLossRefused(first, "same-pass")
	boundary := prerequisiteAudit(t, ctx, f)
	n, err = q.ConfirmPrerequisiteLost(ctx, store.ConfirmPrerequisiteLostParams{ID: f.closure.ID, Generation: 0, PassID: boundary})
	prerequisiteRows(t, 1, n, err)
	if c := prerequisiteRead(t, ctx, f); c.State != "lost" || c.LiveRunID.Valid || c.Reason != "pin_lost" {
		t.Fatalf("ten-minute confirmation failed: %+v", c)
	}
}
