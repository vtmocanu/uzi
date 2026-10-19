package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Each invocation owns a database: goose rollback must never touch another test's rows.
func TestPolicyCandidateCrossCheckLiveDB(t *testing.T) {
	base := os.Getenv("UZI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := OpenPool(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "cdr_candidate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	version := migrationVersionByName(t, "policy_candidate_cross_check")
	previous := migrationVersionByName(t, "policy_stop_resolution")
	if err := MigrateTo(ctx, dsn, previous); err != nil {
		t.Fatal(err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := New(pool)
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, worker := uuid.New(), uuid.New()
	exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	exec(t, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'candidate',$3,'online')", worker, owner, worker[:])
	newRun := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(t, "INSERT INTO runs(id,user_id,kind,status,harness,issue_title,issue_description,worker_id,claim_generation) VALUES($1,$2,'chat','paused','codex','fixture','fixture',$3,7)", id, owner, worker)
		t.Cleanup(func() { exec(t, "DELETE FROM runs WHERE id=$1", id) })
		return id
	}
	// This row predates the new column.
	historical := uuid.New()
	exec(t, "INSERT INTO runs(id,user_id,kind,status,harness,issue_title,issue_description) VALUES($1,$2,'chat','paused','codex','historical','fixture')", historical, owner)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	t.Run("latest_nullable_and_clean_down_up", func(t *testing.T) {
		var nullable string
		if err := pool.QueryRow(ctx, "SELECT is_nullable FROM information_schema.columns WHERE table_schema='public' AND table_name='runs' AND column_name='policy_candidate_cross_check_id'").Scan(&nullable); err != nil || nullable != "YES" {
			t.Fatalf("nullable=%q err=%v", nullable, err)
		}
		var selected pgtype.UUID
		if err := pool.QueryRow(ctx, "SELECT policy_candidate_cross_check_id FROM runs WHERE id=$1", historical).Scan(&selected); err != nil || selected.Valid {
			t.Fatalf("historical selection=%v err=%v", selected, err)
		}
		if err := MigrateDownTo(ctx, dsn, previous); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='runs' AND column_name='policy_candidate_cross_check_id')").Scan(&exists); err != nil || exists {
			t.Fatalf("column survived clean Down: exists=%v err=%v", exists, err)
		}
		if err := MigrateTo(ctx, dsn, version); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "SELECT policy_candidate_cross_check_id FROM runs WHERE id=$1", historical).Scan(&selected); err != nil || selected.Valid {
			t.Fatalf("round-trip selection=%v err=%v", selected, err)
		}
	})
	exec(t, "DELETE FROM runs WHERE id=$1", historical)
	scope := func(t *testing.T, run uuid.UUID) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'scope','ceiling') RETURNING id", run).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	accept := func(t *testing.T, run uuid.UUID) {
		t.Helper()
		exec(t, "UPDATE runs SET policy_stop_resolution=jsonb_build_object('version',1,'scope_input_id',$2::bigint) WHERE id=$1", run, scope(t, run))
	}
	selectedIs := func(t *testing.T, run uuid.UUID, want pgtype.UUID) {
		t.Helper()
		var got pgtype.UUID
		if err := pool.QueryRow(ctx, "SELECT policy_candidate_cross_check_id FROM runs WHERE id=$1", run).Scan(&got); err != nil || got != want {
			t.Fatalf("selected identity=%v want=%v err=%v", got, want, err)
		}
	}
	newCrossCheck := func(t *testing.T, run uuid.UUID, round int) uuid.UUID {
		t.Helper()
		id, child := uuid.New(), newRun(t)
		exec(t, "INSERT INTO cross_checks(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,verdict,reason_class,findings,deadline_at,checker_harness,checker_model,checker_model_source) VALUES($1,$2,$3,'plan',$5,7,'candidate','[]','s','base',$4,'revise','revise','[]',now(),'codex','fixture','worker default')", id, run, child, []byte{1}, round)
		return id
	}
	for _, guard := range []string{"current", "wrong_worker", "wrong_generation", "released", "accepted_stop"} {
		t.Run("query_"+guard, func(t *testing.T) {
			run := newRun(t)
			first, second := newCrossCheck(t, run, 1), newCrossCheck(t, run, 2)
			args := SetPolicyCandidateCrossCheckAssociationParams{CrossCheckID: first, LeadRunID: run, WorkerID: worker, ClaimGeneration: 7}
			n, err := q.SetPolicyCandidateCrossCheckAssociation(ctx, args)
			if err != nil || n != 1 {
				t.Fatalf("initial association rows=%d err=%v", n, err)
			}
			selectedIs(t, run, pgtype.UUID{Bytes: first, Valid: true})
			args.CrossCheckID = second
			want := int64(0)
			switch guard {
			case "current":
				want = 1
			case "wrong_worker":
				args.WorkerID = uuid.New()
			case "wrong_generation":
				args.ClaimGeneration++
			case "released":
				exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
			case "accepted_stop":
				accept(t, run)
			}
			n, err = q.SetPolicyCandidateCrossCheckAssociation(ctx, args)
			if err != nil || n != want {
				t.Fatalf("guard %s rows=%d want=%d err=%v", guard, n, want, err)
			}
			if want == 1 {
				first = second
			}
			selectedIs(t, run, pgtype.UUID{Bytes: first, Valid: true})
		})
	}
	for _, transition := range []string{"null_to_uuid", "uuid_to_uuid", "uuid_to_null"} {
		t.Run("accepted_stop_immutable_"+transition, func(t *testing.T) {
			run := newRun(t)
			var before, after pgtype.UUID
			if transition != "null_to_uuid" {
				before = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			}
			if transition != "uuid_to_null" {
				after = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			}
			exec(t, "UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, before)
			accept(t, run)
			_, err := pool.Exec(ctx, "UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, after)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != "immutable policy stop resolution" {
				t.Fatalf("expected immutability rejection, got %v", err)
			}
			selectedIs(t, run, before)
		})
	}
	for _, evidence := range []string{"association_only", "source_only", "accepted_stop", "input_settlement", "cross_check_settlement"} {
		t.Run("down_refuses_"+evidence, func(t *testing.T) {
			run := newRun(t)
			switch evidence {
			case "association_only":
				// A dangling UUID is intentionally legal here. This is schema evidence,
				// not authority for a downstream manifest, which must refuse it.
				exec(t, "UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
			case "source_only":
				exec(t, "UPDATE runs SET policy_revision_source='{\"kind\":\"human\",\"input_id\":1}' WHERE id=$1", run)
			case "accepted_stop":
				accept(t, run)
			case "input_settlement":
				exec(t, "INSERT INTO run_user_inputs(run_id,kind,body,disposition,settled_at,superseded_by_stop_input_id) VALUES($1,'revise_plan','feedback','superseded_by_stop',now(),$2)", run, scope(t, run))
			case "cross_check_settlement":
				cc := newCrossCheck(t, run, 1)
				exec(t, "UPDATE cross_checks SET revision_disposition='superseded_by_stop',revision_settled_at=now(),revision_superseded_by_stop_input_id=$2 WHERE id=$1", cc, scope(t, run))
			}
			err := MigrateDownTo(ctx, dsn, previous)
			if err == nil || !strings.Contains(err.Error(), "policy candidate or stop evidence exists; refusing lossy rollback") {
				t.Fatalf("expected evidence refusal, got %v", err)
			}
			var column, trigger bool
			if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='runs' AND column_name='policy_candidate_cross_check_id'), EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='runs'::regclass AND tgname='runs_policy_stop_resolution' AND tgenabled='O')").Scan(&column, &trigger); err != nil || !column || !trigger {
				t.Fatalf("failed Down lost column/trigger: column=%v trigger=%v err=%v", column, trigger, err)
			}
			if evidence == "association_only" {
				// Exercise the surviving trigger rather than trusting its catalog name.
				accept(t, run)
				_, err := pool.Exec(ctx, "UPDATE runs SET policy_candidate_cross_check_id=NULL WHERE id=$1", run)
				if err == nil || !strings.Contains(err.Error(), "immutable policy stop resolution") {
					t.Fatalf("failed Down lost association immutability: %v", err)
				}
			}
		})
	}
}
