package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4512 (ADR-4007 T3a), migrations 00501 (fire_epoch trigger) and 00502 (the per-fire
// progress tables). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh). Every name ends LiveDB so the store-IT sweep selects it.

type onceFireFx struct {
	ctx             context.Context
	pool            *pgxpool.Pool
	q               *store.Queries
	user            uuid.UUID
	repo, otherRepo uuid.UUID
	dsn             string
}

func newOnceFireFx(t *testing.T) *onceFireFx {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "once_fire_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &onceFireFx{ctx: ctx, pool: pool, q: store.New(pool), user: uuid.New(), repo: uuid.New(), otherRepo: uuid.New(), dsn: dsn}
	conn := uuid.New()
	f.exec(t, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, f.user, f.user.String()+"@example.test")
	f.exec(t, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES ($1,$2,'gitlab','https://forge.e2e','bot',1,'x')`, conn, f.user)
	for i, r := range []uuid.UUID{f.repo, f.otherRepo} {
		f.exec(t, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled)
			VALUES ($1,$2,$3,$4,$5,true)`, r, conn, 100+i, fmt.Sprintf("g/r%d", i), fmt.Sprintf("https://forge.e2e/g/r%d", i))
	}
	return f
}

func (f *onceFireFx) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *onceFireFx) epoch(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	var e int64
	if err := f.pool.QueryRow(f.ctx, `SELECT fire_epoch FROM run_schedules WHERE id=$1`, id).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *onceFireFx) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func futureTS() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
}

// onceSweep inserts a once label sweep and moves it to state (active, error or fired). Leaving
// active never meets the trigger's predicate, so the returned row still sits at epoch 0.
func (f *onceFireFx) onceSweep(t *testing.T, state string) store.RunSchedule {
	t.Helper()
	sc, err := f.q.CreateRunSchedule(f.ctx, store.CreateRunScheduleParams{
		UserID: f.user, RepoID: f.repo, Target: "sweep", Timing: "once", RunAt: futureTS(), NextFireAt: futureTS(),
		Timezone: "UTC", AutoApprove: true, Enabled: true, Labels: []byte(`["uzi"]`),
	})
	if err != nil {
		t.Fatalf("create once sweep: %v", err)
	}
	if state != "active" {
		f.exec(t, `UPDATE run_schedules SET status=$2 WHERE id=$1`, sc.ID, state)
	}
	if e := f.epoch(t, sc.ID); e != 0 {
		t.Fatalf("fixture epoch = %d, want 0 (leaving active must not bump)", e)
	}
	got, err := f.q.GetRunSchedule(f.ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *onceFireFx) recurringSweep(t *testing.T, state string) store.RunSchedule {
	t.Helper()
	sc, err := f.q.CreateRunSchedule(f.ctx, store.CreateRunScheduleParams{
		UserID: f.user, RepoID: f.repo, Target: "sweep", Timing: "recurring", CronExpr: pgtype.Text{String: "*/5 * * * *", Valid: true},
		NextFireAt: futureTS(), Timezone: "UTC", AutoApprove: true, Enabled: true, Labels: []byte(`["uzi"]`),
	})
	if err != nil {
		t.Fatalf("create recurring sweep: %v", err)
	}
	if state != "active" {
		f.exec(t, `UPDATE run_schedules SET status=$2 WHERE id=$1`, sc.ID, state)
	}
	got, err := f.q.GetRunSchedule(f.ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// updateParams restates a row as the UpdateRunSchedule arguments (a config edit that keeps
// every field), for a case to mutate.
func updateParams(s store.RunSchedule) store.UpdateRunScheduleParams {
	return store.UpdateRunScheduleParams{
		CiWorkflows: s.CiWorkflows, CiArtifacts: s.CiArtifacts, Target: s.Target, RepoID: s.RepoID, IssueIid: s.IssueIid,
		Labels: s.Labels, Prompt: s.Prompt, Timing: s.Timing, CronExpr: s.CronExpr, RunAt: s.RunAt, Timezone: s.Timezone,
		NextFireAt: s.NextFireAt, AutoApprove: s.AutoApprove, WaitOnLimit: s.WaitOnLimit, MrReworkEnabled: s.MrReworkEnabled,
		MaxIssues: s.MaxIssues, Guidance: s.Guidance, Model: s.Model, OutputMode: s.OutputMode,
		RemoveLabelOnDispatch: s.RemoveLabelOnDispatch, OverrideSubagentModel: s.OverrideSubagentModel,
		CapacityLimit: s.CapacityLimit, CapacityRoomNeeded: s.CapacityRoomNeeded, Harness: s.Harness,
		CredentialOverrideMode: s.CredentialOverrideMode, CredentialOverrideSecretID: s.CredentialOverrideSecretID,
		Customized: s.Customized, ID: s.ID, UserID: s.UserID,
	}
}

// legacyUpdateRunSchedule is the UpdateRunSchedule text of base 8be78a548 (before #4512), run
// as raw SQL: what an older binary executes against the migrated schema. Parameters follow the
// query's $n order (sqlc numbers them by first appearance).
const legacyUpdateRunSchedule = `UPDATE run_schedules
SET ci_workflows = $1, ci_artifacts = $2,
    target        = $3,
    repo_id       = $4,
    issue_iid     = $5,
    labels        = $6,
    prompt        = $7,
    timing        = $8,
    cron_expr     = $9,
    run_at        = $10,
    timezone      = $11,
    next_fire_at  = $12,
    auto_approve  = $13,
    wait_on_limit = $14,
    mr_rework_enabled = $15,
    max_issues    = $16,
    guidance      = $17,
    model         = $18,
    output_mode   = $19,
    remove_label_on_dispatch = $20,
    override_subagent_model = $21,
    capacity_limit = $22,
    capacity_room_needed = $23,
    harness       = $24,
    credential_override_mode = $25,
    credential_override_secret_id = $26,
    customized    = $27,
    status        = 'active',
    updated_at    = now()
WHERE id = $28 AND user_id = $29`

func (f *onceFireFx) legacyUpdate(t *testing.T, p store.UpdateRunScheduleParams) {
	t.Helper()
	f.exec(t, legacyUpdateRunSchedule, p.CiWorkflows, p.CiArtifacts, p.Target, p.RepoID, p.IssueIid, p.Labels, p.Prompt, p.Timing,
		p.CronExpr, p.RunAt, p.Timezone, p.NextFireAt, p.AutoApprove, p.WaitOnLimit, p.MrReworkEnabled, p.MaxIssues, p.Guidance,
		p.Model, p.OutputMode, p.RemoveLabelOnDispatch, p.OverrideSubagentModel, p.CapacityLimit, p.CapacityRoomNeeded, p.Harness,
		p.CredentialOverrideMode, p.CredentialOverrideSecretID, p.Customized, p.ID, p.UserID)
}

// TestOnceFireEpochTriggerMatrixLiveDB pins the trigger's exact predicate (migration 00501):
// rows in states active, error and fired crossed with every writer of run_schedules.
func TestOnceFireEpochTriggerMatrixLiveDB(t *testing.T) {
	f := newOnceFireFx(t)
	otherTS := pgtype.Timestamptz{Time: time.Now().Add(48 * time.Hour), Valid: true}

	type c struct {
		name string
		// bump is whether the writer must bump the epoch of a once row that starts in state.
		bump  func(state string) bool
		apply func(t *testing.T, s store.RunSchedule)
	}
	never := func(string) bool { return false }
	always := func(string) bool { return true }
	// revival: the status arm fires only on a real transition into active.
	revival := func(state string) bool { return state != "active" }
	// All four guarded writers (queries/once_fire.sql, credential_disabled.sql) require
	// status='active' on the row at the claimed epoch 0 (every matrix row is a once row), so the write lands (1 row) only on
	// an active row and an error/fired row is skipped (0 rows): "no bump" is then not vacuous.
	wantGuardedRows := func(t *testing.T, name string, s store.RunSchedule, got int64) {
		t.Helper()
		want := int64(0)
		if s.Status == "active" {
			want = 1
		}
		if got != want {
			t.Fatalf("%s on a %s once row matched %d rows, want %d", name, s.Status, got, want)
		}
	}

	cases := []c{
		{"enabled-only pause", never, func(t *testing.T, s store.RunSchedule) {
			if _, err := f.q.SetRunScheduleEnabled(f.ctx, store.SetRunScheduleEnabledParams{ID: s.ID, UserID: s.UserID, Enabled: false}); err != nil {
				t.Fatal(err)
			}
		}},
		{"enabled-only resume", never, func(t *testing.T, s store.RunSchedule) {
			f.exec(t, `UPDATE run_schedules SET enabled=false WHERE id=$1`, s.ID)
			if _, err := f.q.SetRunScheduleEnabled(f.ctx, store.SetRunScheduleEnabledParams{ID: s.ID, UserID: s.UserID, Enabled: true}); err != nil {
				t.Fatal(err)
			}
		}},
		{"sibling group bookkeeping", never, func(t *testing.T, s store.RunSchedule) {
			if _, err := f.q.CoalesceScheduleSiblingGroup(f.ctx, store.CoalesceScheduleSiblingGroupParams{
				NewGroup: pgtype.UUID{Bytes: uuid.New(), Valid: true}, ID: s.ID, UserID: s.UserID}); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE run_schedules SET sibling_group_id = NULL WHERE id=$1`, s.ID)
		}},
		{"held write", never, func(t *testing.T, s store.RunSchedule) {
			n, err := f.q.RecordScheduleHeldFire(f.ctx, store.RecordScheduleHeldFireParams{ID: s.ID, LastFire: []byte(`{"matched":0}`), FireEpoch: 0})
			if err != nil {
				t.Fatal(err)
			}
			wantGuardedRows(t, "RecordScheduleHeldFire", s, n)
		}},
		{"partial write", never, func(t *testing.T, s store.RunSchedule) {
			n, err := f.q.RecordOnceFirePartial(f.ctx, store.RecordOnceFirePartialParams{ID: s.ID, LastFire: []byte(`{"matched":1}`), FireEpoch: 0})
			if err != nil {
				t.Fatal(err)
			}
			wantGuardedRows(t, "RecordOnceFirePartial", s, n)
		}},
		{"guarded advance", never, func(t *testing.T, s store.RunSchedule) {
			n, err := f.q.AdvanceOnceScheduleGuarded(f.ctx, store.AdvanceOnceScheduleGuardedParams{
				ID: s.ID, FireEpoch: 0, Status: "fired", LastFire: []byte(`{"matched":1}`),
				LastFiredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
			if err != nil {
				t.Fatal(err)
			}
			wantGuardedRows(t, "AdvanceOnceScheduleGuarded", s, n)
		}},
		{"guarded park", never, func(t *testing.T, s store.RunSchedule) {
			n, err := f.q.ParkScheduleGuarded(f.ctx, store.ParkScheduleGuardedParams{ID: s.ID, FireEpoch: 0})
			if err != nil {
				t.Fatal(err)
			}
			wantGuardedRows(t, "ParkScheduleGuarded", s, n)
		}},
		{"secret delete SET NULL", never, func(t *testing.T, s store.RunSchedule) {
			secret := uuid.New()
			f.exec(t, `INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token',$3,'\x01','master')`, secret, f.user, "l-"+secret.String())
			f.exec(t, `UPDATE run_schedules SET credential_override_mode='pinned', credential_override_secret_id=$2 WHERE id=$1`, s.ID, secret)
			f.exec(t, `DELETE FROM user_secrets WHERE id=$1`, secret)
			var set pgtype.UUID
			if err := f.pool.QueryRow(f.ctx, `SELECT credential_override_secret_id FROM run_schedules WHERE id=$1`, s.ID).Scan(&set); err != nil || set.Valid {
				t.Fatalf("secret delete must null the pin: %v %v", set, err)
			}
		}},
		{"config edit keeping run_at (UpdateRunSchedule)", revival, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.Guidance = pgtype.Text{String: "same slot", Valid: true}
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"run_at change", always, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.RunAt = otherTS
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"target change", always, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.Target, p.IssueIid, p.Labels = "issue", pgtype.Int8{Int64: 5, Valid: true}, nil
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"repo change", always, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.RepoID = f.otherRepo
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"timing change once to recurring", always, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.Timing, p.CronExpr, p.RunAt = "recurring", pgtype.Text{String: "*/5 * * * *", Valid: true}, pgtype.Timestamptz{}
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
		}},
		// The legacy pre-#4512 text: an older binary's writes still bump, on revival or a re-arm only.
		{"legacy UpdateRunSchedule text, config edit keeping run_at", revival, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.Guidance = pgtype.Text{String: "legacy", Valid: true}
			f.legacyUpdate(t, p)
		}},
		{"legacy UpdateRunSchedule text, run_at change", always, func(t *testing.T, s store.RunSchedule) {
			p := updateParams(s)
			p.RunAt = otherTS
			f.legacyUpdate(t, p)
		}},
	}
	for _, state := range []string{"active", "error", "fired"} {
		for _, tc := range cases {
			t.Run(state+"/"+tc.name, func(t *testing.T) {
				s := f.onceSweep(t, state)
				tc.apply(t, s)
				want := int64(0)
				if tc.bump(state) {
					want = 1
				}
				if got := f.epoch(t, s.ID); got != want {
					t.Fatalf("fire_epoch after %q on a %s once row = %d, want %d", tc.name, state, got, want)
				}
			})
		}
	}

	t.Run("a second re-arm bumps again", func(t *testing.T) {
		s := f.onceSweep(t, "fired")
		for want := int64(1); want <= 2; want++ {
			p := updateParams(s)
			p.RunAt = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(want+30) * time.Hour), Valid: true}
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
			if got := f.epoch(t, s.ID); got != want {
				t.Fatalf("epoch after re-arm %d = %d", want, got)
			}
		}
	})

	t.Run("recurring rows keep the epoch", func(t *testing.T) {
		for _, state := range []string{"active", "error", "fired"} {
			s := f.recurringSweep(t, state)
			p := updateParams(s)
			p.CronExpr = pgtype.Text{String: "0 * * * *", Valid: true} // a revival too, from error and fired
			if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
				t.Fatal(err)
			}
			f.legacyUpdate(t, p)
			if got := f.epoch(t, s.ID); got != 0 {
				t.Fatalf("recurring-to-recurring edit from %s moved the epoch to %d", state, got)
			}
		}
	})

	t.Run("recurring to once conversion bumps", func(t *testing.T) {
		s := f.recurringSweep(t, "active")
		p := updateParams(s)
		p.Timing, p.CronExpr, p.RunAt = "once", pgtype.Text{}, futureTS()
		if _, err := f.q.UpdateRunSchedule(f.ctx, p); err != nil {
			t.Fatal(err)
		}
		if got := f.epoch(t, s.ID); got != 1 {
			t.Fatalf("recurring-to-once epoch = %d, want 1", got)
		}
	})

	t.Run("ResetDefaultSchedule on an error-parked default row does not bump", func(t *testing.T) {
		d, err := f.q.CreateDefaultSchedule(f.ctx, store.CreateDefaultScheduleParams{
			UserID: f.user, RepoID: f.repo, Target: "sweep", CatalogSlug: pgtype.Text{String: "once-fire-default", Valid: true},
			CronExpr: pgtype.Text{String: "0 3 * * *", Valid: true}, Timezone: "UTC", NextFireAt: futureTS(), AutoApprove: true,
		})
		if err != nil {
			t.Fatalf("create default: %v", err)
		}
		f.exec(t, `UPDATE run_schedules SET status='error' WHERE id=$1`, d.ID)
		reset, err := f.q.ResetDefaultSchedule(f.ctx, store.ResetDefaultScheduleParams{
			CronExpr: pgtype.Text{String: "0 4 * * *", Valid: true}, Timezone: "UTC", AutoApprove: true, NextFireAt: futureTS(), ID: d.ID, UserID: f.user})
		if err != nil {
			t.Fatalf("reset default: %v", err)
		}
		if reset.Status != "active" || reset.FireEpoch != 0 {
			t.Fatalf("reset default: status=%q epoch=%d, want active/0 (a recurring default never meets the once guard)", reset.Status, reset.FireEpoch)
		}
	})

	t.Run("INSERT starts at epoch 0", func(t *testing.T) {
		s := f.onceSweep(t, "active")
		if s.FireEpoch != 0 {
			t.Fatalf("new row epoch = %d", s.FireEpoch)
		}
	})
}

type ledgerFx struct {
	*onceFireFx
	sched store.RunSchedule
}

func (f *onceFireFx) ledger(t *testing.T) ledgerFx {
	t.Helper()
	s := f.onceSweep(t, "active")
	f.exec(t, `INSERT INTO schedule_once_fires (schedule_id, fire_epoch, effective_cap) VALUES ($1, 0, 2)`, s.ID)
	return ledgerFx{f, s}
}

func (l ledgerFx) insertRun(t *testing.T, iid int64, runUUID uuid.UUID) error {
	t.Helper()
	_, err := l.pool.Exec(l.ctx, `INSERT INTO schedule_once_fire_runs (schedule_id, fire_epoch, repo_id, issue_iid, issue_title, web_url, run_uuid)
		VALUES ($1, 0, $2, $3, 'title', 'https://forge.e2e/i', $4)`, l.sched.ID, l.repo, iid, runUUID)
	return err
}

// TestOnceFireLedgerSchemaLiveDB pins the ledger's constraints and cascades (migration 00502).
func TestOnceFireLedgerSchemaLiveDB(t *testing.T) {
	f := newOnceFireFx(t)

	t.Run("run delete leaves the ledger row intact", func(t *testing.T) {
		l := f.ledger(t)
		runID := uuid.New()
		f.exec(t, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description) VALUES ($1,$2,$3,96,'t','d')`, runID, f.user, f.repo)
		if err := l.insertRun(t, 96, runID); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `DELETE FROM runs WHERE id=$1`, runID)
		rows, err := f.q.ListOnceFireProgress(f.ctx, store.ListOnceFireProgressParams{ScheduleID: l.sched.ID, FireEpoch: 0})
		if err != nil || len(rows) != 1 {
			t.Fatalf("ledger after run delete: %v %v", rows, err)
		}
		if rows[0].RunUuid != runID || rows[0].IssueTitle != "title" || rows[0].WebUrl != "https://forge.e2e/i" || rows[0].IssueIid != 96 {
			t.Fatalf("ledger identity changed by the run delete: %+v", rows[0])
		}
	})

	t.Run("an UPDATE of an identity column is refused, others are not", func(t *testing.T) {
		l := f.ledger(t)
		if err := l.insertRun(t, 1, uuid.New()); err != nil {
			t.Fatal(err)
		}
		// A header at the target epoch exists, so the composite FK cannot be what refuses the
		// fire_epoch change: only the immutability trigger can.
		f.exec(t, `INSERT INTO schedule_once_fires (schedule_id, fire_epoch, effective_cap) VALUES ($1, 7, 2)`, l.sched.ID)
		other := f.ledger(t) // a second schedule with an epoch-0 header, the target of the schedule_id change
		refused := func(set string, args ...any) {
			t.Helper()
			_, err := f.pool.Exec(f.ctx, `UPDATE schedule_once_fire_runs SET `+set+` WHERE schedule_id=$1 AND issue_iid=1`, append([]any{l.sched.ID}, args...)...)
			var pe *pgconn.PgError
			// The trigger's own RAISE EXCEPTION: SQLSTATE P0001 and its message, not an FK or CHECK error.
			if !errors.As(err, &pe) || pe.Code != "P0001" || !strings.Contains(pe.Message, "identity columns are immutable") {
				t.Errorf("UPDATE SET %s: want the immutability trigger's P0001, got %v", set, err)
			}
		}
		refused(`issue_iid = 2`)
		refused(`run_uuid = gen_random_uuid()`)
		refused(`repo_id = $2`, f.otherRepo)
		refused(`fire_epoch = 7`)
		refused(`schedule_id = $2`, other.sched.ID)
		f.exec(t, `UPDATE schedule_once_fire_runs SET issue_title = 'renamed' WHERE schedule_id=$1`, l.sched.ID)
	})

	t.Run("the nil run uuid and a duplicate issue are refused", func(t *testing.T) {
		l := f.ledger(t)
		if err := l.insertRun(t, 1, uuid.Nil); err == nil {
			t.Fatal("a nil run_uuid must be refused by the CHECK")
		}
		if err := l.insertRun(t, 1, uuid.New()); err != nil {
			t.Fatal(err)
		}
		err := l.insertRun(t, 1, uuid.New())
		if err == nil || sqlState(err) != "23505" || !containsConstraint(err, "schedule_once_fire_runs_pkey") {
			t.Fatalf("a second row for the same issue: want 23505 on schedule_once_fire_runs_pkey, got %v", err)
		}
	})

	t.Run("a negative cap is refused, NULL is unlimited", func(t *testing.T) {
		s := f.onceSweep(t, "active")
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO schedule_once_fires (schedule_id, fire_epoch, effective_cap) VALUES ($1, 0, -1)`, s.ID); err == nil {
			t.Fatal("a negative effective_cap must be refused")
		}
		f.exec(t, `INSERT INTO schedule_once_fires (schedule_id, fire_epoch) VALUES ($1, 0)`, s.ID)
		got, err := f.q.GetOnceFireCap(f.ctx, store.GetOnceFireCapParams{ScheduleID: s.ID, FireEpoch: 0})
		if err != nil || got.Valid {
			t.Fatalf("NULL cap must read back invalid (unlimited): %v %v", got, err)
		}
	})

	t.Run("a ledger row needs its header", func(t *testing.T) {
		s := f.onceSweep(t, "active")
		_, err := f.pool.Exec(f.ctx, `INSERT INTO schedule_once_fire_runs (schedule_id, fire_epoch, repo_id, issue_iid, issue_title, web_url, run_uuid)
			VALUES ($1, 3, $2, 1, 't', 'u', gen_random_uuid())`, s.ID, f.repo)
		if err == nil {
			t.Fatal("a ledger row without a header must be refused")
		}
	})

	t.Run("schedule delete cascades the header and ledger", func(t *testing.T) {
		l := f.ledger(t)
		if err := l.insertRun(t, 1, uuid.New()); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `DELETE FROM run_schedules WHERE id=$1`, l.sched.ID)
		if n := f.count(t, `SELECT (SELECT count(*) FROM schedule_once_fires WHERE schedule_id=$1) + (SELECT count(*) FROM schedule_once_fire_runs WHERE schedule_id=$1)`, l.sched.ID); n != 0 {
			t.Fatalf("%d rows left after the schedule delete", n)
		}
	})

	t.Run("repo delete cascades the header and ledger", func(t *testing.T) {
		repo2 := uuid.New()
		var conn uuid.UUID
		if err := f.pool.QueryRow(f.ctx, `SELECT connection_id FROM repos WHERE id=$1`, f.repo).Scan(&conn); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES ($1,$2,900,'g/del','https://forge.e2e/g/del',true)`, repo2, conn)
		sc, err := f.q.CreateRunSchedule(f.ctx, store.CreateRunScheduleParams{
			UserID: f.user, RepoID: repo2, Target: "sweep", Timing: "once", RunAt: futureTS(), NextFireAt: futureTS(),
			Timezone: "UTC", Enabled: true, Labels: []byte(`["uzi"]`)})
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, `INSERT INTO schedule_once_fires (schedule_id, fire_epoch, effective_cap) VALUES ($1, 0, 2)`, sc.ID)
		f.exec(t, `INSERT INTO schedule_once_fire_runs (schedule_id, fire_epoch, repo_id, issue_iid, issue_title, web_url, run_uuid)
			VALUES ($1, 0, $2, 1, 't', 'u', gen_random_uuid())`, sc.ID, repo2)
		f.exec(t, `DELETE FROM repos WHERE id=$1`, repo2)
		if n := f.count(t, `SELECT (SELECT count(*) FROM run_schedules WHERE id=$1) + (SELECT count(*) FROM schedule_once_fires WHERE schedule_id=$1) + (SELECT count(*) FROM schedule_once_fire_runs WHERE schedule_id=$1)`, sc.ID); n != 0 {
			t.Fatalf("%d rows left after the repo delete", n)
		}
	})

	t.Run("the guard trigger and the immutability trigger are attached", func(t *testing.T) {
		if n := f.count(t, `SELECT count(*) FROM pg_trigger WHERE tgrelid='schedule_once_fire_runs'::regclass AND NOT tgisinternal
			AND tgname IN ('schedule_once_fire_runs_awaiting_first_enable_guard','schedule_once_fire_runs_identity_immutable')`); n != 2 {
			t.Fatalf("triggers attached = %d, want 2", n)
		}
	})
}

func containsConstraint(err error, name string) bool {
	return err != nil && strings.Contains(err.Error(), name)
}

// TestOnceFireQueriesLiveDB drives the progress queries and the guarded writers directly.
func TestOnceFireQueriesLiveDB(t *testing.T) {
	f := newOnceFireFx(t)
	l := f.ledger(t)

	// Header insert is idempotent and the first cap wins.
	if err := f.q.InsertOnceFireHeader(f.ctx, store.InsertOnceFireHeaderParams{ScheduleID: l.sched.ID, FireEpoch: 0, EffectiveCap: pgtype.Int4{Int32: 9, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.q.GetOnceFireCap(f.ctx, store.GetOnceFireCapParams{ScheduleID: l.sched.ID, FireEpoch: 0}); err != nil || !got.Valid || got.Int32 != 2 {
		t.Fatalf("stored cap = %+v %v, want 2 (an existing header wins)", got, err)
	}
	if _, err := f.q.GetOnceFireCap(f.ctx, store.GetOnceFireCapParams{ScheduleID: l.sched.ID, FireEpoch: 5}); err != pgx.ErrNoRows {
		t.Fatalf("no header: want ErrNoRows, got %v", err)
	}
	// Ledger order is by issue, whatever the insert order.
	for _, iid := range []int64{98, 96, 97} {
		if err := f.q.InsertOnceFireRun(f.ctx, store.InsertOnceFireRunParams{ScheduleID: l.sched.ID, FireEpoch: 0, RepoID: f.repo, IssueIid: iid,
			IssueTitle: fmt.Sprintf("t%d", iid), WebUrl: "u", RunUuid: uuid.New()}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.q.ListOnceFireProgress(f.ctx, store.ListOnceFireProgressParams{ScheduleID: l.sched.ID, FireEpoch: 0})
	if err != nil || len(rows) != 3 || rows[0].IssueIid != 96 || rows[1].IssueIid != 97 || rows[2].IssueIid != 98 {
		t.Fatalf("ListOnceFireProgress = %+v %v", rows, err)
	}
	if n, err := f.q.CountOnceFireRuns(f.ctx, store.CountOnceFireRunsParams{ScheduleID: l.sched.ID, FireEpoch: 0}); err != nil || n != 3 {
		t.Fatalf("count = %d %v", n, err)
	}
	if ok, err := f.q.OnceFireRunExists(f.ctx, store.OnceFireRunExistsParams{ScheduleID: l.sched.ID, FireEpoch: 0, IssueIid: 97}); err != nil || !ok {
		t.Fatalf("exists(97) = %v %v", ok, err)
	}
	if ok, err := f.q.OnceFireRunExists(f.ctx, store.OnceFireRunExistsParams{ScheduleID: l.sched.ID, FireEpoch: 1, IssueIid: 97}); err != nil || ok {
		t.Fatalf("exists(97) in another epoch = %v %v", ok, err)
	}

	// Seed history so a partial write must preserve content, not just an empty array.
	previous := map[string]any{"matched": float64(1), "tag": "previous"}
	if _, err := f.pool.Exec(f.ctx, `UPDATE run_schedules SET recent_fires=$2::jsonb WHERE id=$1`, l.sched.ID, `[{"matched":1,"tag":"previous"}]`); err != nil {
		t.Fatal(err)
	}
	readHistory := func() (store.RunSchedule, []map[string]any) {
		t.Helper()
		got, err := f.q.GetRunSchedule(f.ctx, l.sched.ID)
		if err != nil {
			t.Fatal(err)
		}
		var history []map[string]any
		if err := json.Unmarshal(got.RecentFires, &history); err != nil {
			t.Fatalf("decode recent_fires: %v", err)
		}
		return got, history
	}
	// Guarded writers write nothing for a stale epoch or a non-active row.
	stale := store.RecordOnceFirePartialParams{ID: l.sched.ID, FireEpoch: 7, LastFire: []byte(`{"matched":1}`)}
	if n, err := f.q.RecordOnceFirePartial(f.ctx, stale); err != nil || n != 0 {
		t.Fatalf("partial with a stale epoch wrote %d rows (%v)", n, err)
	}
	if n, err := f.q.RecordOnceFirePartial(f.ctx, store.RecordOnceFirePartialParams{ID: l.sched.ID, FireEpoch: 0, LastFire: []byte(`{"matched":3}`)}); err != nil || n != 1 {
		t.Fatalf("partial wrote %d rows (%v)", n, err)
	}
	if _, history := readHistory(); !reflect.DeepEqual(history, []map[string]any{previous}) {
		t.Fatalf("the partial write must preserve recent_fires, got %+v", history)
	}
	adv := store.AdvanceOnceScheduleGuardedParams{ID: l.sched.ID, FireEpoch: 7, Status: "fired", LastFire: []byte(`{"matched":3}`), LastFiredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	if n, err := f.q.AdvanceOnceScheduleGuarded(f.ctx, adv); err != nil || n != 0 {
		t.Fatalf("advance with a stale epoch wrote %d rows (%v)", n, err)
	}
	adv.FireEpoch = 0
	if n, err := f.q.AdvanceOnceScheduleGuarded(f.ctx, adv); err != nil || n != 1 {
		t.Fatalf("advance wrote %d rows (%v)", n, err)
	}
	if got, history := readHistory(); got.Status != "fired" || !reflect.DeepEqual(history, []map[string]any{{"matched": float64(3)}, previous}) {
		t.Fatalf("advance must set status fired and prepend exactly once: %q %+v", got.Status, history)
	}
	// Once fired, a second advance, a partial and a park all match nothing.
	if n, err := f.q.AdvanceOnceScheduleGuarded(f.ctx, adv); err != nil || n != 0 {
		t.Fatalf("advance on a fired row wrote %d rows (%v)", n, err)
	}
	if n, err := f.q.RecordOnceFirePartial(f.ctx, store.RecordOnceFirePartialParams{ID: l.sched.ID, FireEpoch: 0, LastFire: []byte(`{}`)}); err != nil || n != 0 {
		t.Fatalf("partial on a fired row wrote %d rows (%v)", n, err)
	}
	if n, err := f.q.ParkScheduleGuarded(f.ctx, store.ParkScheduleGuardedParams{ID: l.sched.ID, FireEpoch: 0}); err != nil || n != 0 {
		t.Fatalf("park on a fired row wrote %d rows (%v)", n, err)
	}

	// Park: stale epoch writes nothing, the claimed one parks (any timing).
	for _, mk := range []func(*testing.T, string) store.RunSchedule{f.onceSweep, f.recurringSweep} {
		s := mk(t, "active")
		if n, err := f.q.ParkScheduleGuarded(f.ctx, store.ParkScheduleGuardedParams{ID: s.ID, FireEpoch: 3}); err != nil || n != 0 {
			t.Fatalf("park with a stale epoch wrote %d rows (%v)", n, err)
		}
		if n, err := f.q.ParkScheduleGuarded(f.ctx, store.ParkScheduleGuardedParams{ID: s.ID, FireEpoch: 0}); err != nil || n != 1 {
			t.Fatalf("park wrote %d rows (%v)", n, err)
		}
	}

	// The recurring guarded advance keeps the cadence for epoch 0 and refuses another epoch.
	rec := f.recurringSweep(t, "active")
	radv := store.AdvanceScheduleGuardedParams{ID: rec.ID, FireEpoch: 4, Status: "active", LastFire: []byte(`{"matched":0}`),
		LastFiredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, NextFireAt: futureTS()}
	if n, err := f.q.AdvanceScheduleGuarded(f.ctx, radv); err != nil || n != 0 {
		t.Fatalf("recurring advance with a stale epoch wrote %d rows (%v)", n, err)
	}
	radv.FireEpoch = 0
	if n, err := f.q.AdvanceScheduleGuarded(f.ctx, radv); err != nil || n != 1 {
		t.Fatalf("recurring advance wrote %d rows (%v)", n, err)
	}

	// RecordScheduleHeldFire now carries the epoch guard.
	hold := f.onceSweep(t, "active")
	if n, err := f.q.RecordScheduleHeldFire(f.ctx, store.RecordScheduleHeldFireParams{ID: hold.ID, LastFire: []byte(`{"matched":0}`), FireEpoch: 2}); err != nil || n != 0 {
		t.Fatalf("held write with a stale epoch wrote %d rows (%v)", n, err)
	}
	if n, err := f.q.RecordScheduleHeldFire(f.ctx, store.RecordScheduleHeldFireParams{ID: hold.ID, LastFire: []byte(`{"matched":0}`), FireEpoch: 0}); err != nil || n != 1 {
		t.Fatalf("held write wrote %d rows (%v)", n, err)
	}
}

// TestOnceFireMigrationDownUpLiveDB runs 00502's and 00501's Down and Up again, in order.
func TestOnceFireMigrationDownUpLiveDB(t *testing.T) {
	f := newOnceFireFx(t)
	count := func(sql string) int { return f.count(t, sql) }
	objects := `SELECT (SELECT count(*) FROM pg_class WHERE relname IN ('schedule_once_fires','schedule_once_fire_runs'))
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'schedule_once_fire_runs_identity_immutable')`
	epochObjects := `SELECT (SELECT count(*) FROM pg_trigger WHERE tgname = 'run_schedules_fire_epoch')
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'run_schedules_bump_fire_epoch')
		+ (SELECT count(*) FROM information_schema.columns WHERE table_name='run_schedules' AND column_name='fire_epoch')`
	if count(objects) != 3 || count(epochObjects) != 3 {
		t.Fatalf("objects before Down: %d %d", count(objects), count(epochObjects))
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 501); err != nil {
		t.Fatalf("down to 501: %v", err)
	}
	if n := count(objects); n != 0 {
		t.Fatalf("progress objects left after 00502's Down: %d", n)
	}
	if n := count(epochObjects); n != 3 {
		t.Fatalf("00502's Down must leave 00501's objects: %d", n)
	}
	if err := store.MigrateDownTo(f.ctx, f.dsn, 500); err != nil {
		t.Fatalf("down to 500: %v", err)
	}
	if n := count(epochObjects); n != 0 {
		t.Fatalf("epoch objects left after 00501's Down: %d", n)
	}
	if err := store.Migrate(f.ctx, f.dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(objects) != 3 || count(epochObjects) != 3 {
		t.Fatal("objects missing after the re-Up")
	}
}
