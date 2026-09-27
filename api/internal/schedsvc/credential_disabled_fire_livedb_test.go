package schedsvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// credential_disabled_fire_livedb_test.go pins the schedule half of PRD #1732 D2/D15 through a
// real tick against real Postgres: a due schedule whose stored token pin is disabled, or whose
// pinned harness has no enabled credential, records the benign, ADVANCING credential_disabled
// skip and creates no run (never substituting another token or harness), while an unpinned
// schedule whose owner has disabled every credential follows D15 (no_usable_credential).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func seedDisabledAnthropicToken(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, isDefault bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with, disabled_at, enablement_rev)
	          VALUES ($1, $2, 'anthropic_token', $3, $4, $5, 'master', now(), 1)`,
		id, userID, "tok-"+id.String(), isDefault, []byte("x")); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return id
}

// TestScheduleFireCredentialDisabledSkipsLiveDB: each case fires one due recurring prompt
// schedule through Boot's real tick and asserts the recorded skip, the advance, and no run.
//
// MUTATION: drop the credential_disabled arm of skipReasonForErr; the pinned cases then fall
// through as a transient error (no skip, next_fire_at not advanced) and this test fails.
func TestScheduleFireCredentialDisabledSkipsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, scID uuid.UUID)
		want  SkipReason
	}{
		{"disabled stored pin", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, scID uuid.UUID) {
			// An enabled default exists: the fire must not inherit it.
			if _, err := pool.Exec(ctx, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			          VALUES ($1, $2, 'anthropic_token', 'def', true, $3, 'master')`, uuid.New(), userID, []byte("x")); err != nil {
				t.Fatal(err)
			}
			pin := seedDisabledAnthropicToken(ctx, t, pool, userID, false)
			if _, err := pool.Exec(ctx, `UPDATE run_schedules SET credential_override_mode = 'pinned', credential_override_secret_id = $2 WHERE id = $1`, scID, pin); err != nil {
				t.Fatal(err)
			}
		}, SkipCredentialDisabled},
		{"pinned harness with no enabled credential", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, scID uuid.UUID) {
			seedDisabledAnthropicToken(ctx, t, pool, userID, false)
			if _, err := pool.Exec(ctx, `UPDATE run_schedules SET harness = 'claude' WHERE id = $1`, scID); err != nil {
				t.Fatal(err)
			}
		}, SkipCredentialDisabled},
		{"implicit harness, every credential disabled", func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, _ uuid.UUID) {
			seedDisabledAnthropicToken(ctx, t, pool, userID, false)
		}, SkipNoUsableCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, q, box := openScheduleFireLiveDB(t)
			userID, repoID := scheduleFireUser(ctx, t, pool)
			runs := workersvc.New(q, box, workersvc.Params{})
			runs.SetTxBeginner(pool)
			sched := New(q, runs, nil, nil, nil, nil, time.Minute, nil)
			scID := uuid.New()
			due := time.Now().UTC().Add(-time.Hour)
			insertRecurringPromptSchedule(ctx, t, pool, scID, userID, repoID, due)
			tc.setup(ctx, t, pool, userID, scID)

			sched.Boot(ctx)

			got, err := q.GetRunSchedule(ctx, scID)
			if err != nil {
				t.Fatalf("GetRunSchedule: %v", err)
			}
			if got.Status != "active" || !got.NextFireAt.Valid || !got.NextFireAt.Time.After(due) {
				t.Fatalf("status=%q next_fire_at=%+v, want active and advanced past %v", got.Status, got.NextFireAt, due)
			}
			if reasons := fetchScheduleLastFireReasons(t, got); len(reasons) != 1 || reasons[0] != string(tc.want) {
				t.Fatalf("last_fire skip reasons = %v, want [%q]", reasons, tc.want)
			}
			if n := countPromptRunsForSchedule(ctx, t, pool, scID); n != 0 {
				t.Fatalf("runs created = %d, want 0", n)
			}
		})
	}
}

// TestScheduleFireOnceCredentialDisabledHoldsLiveDB (PRD #1732 D2, journey 8): a due ONE-TIME
// prompt schedule whose stored pin is disabled is held through Boot's real tick: still active,
// still due at the same next_fire_at, no run, and last_fire records the credential_disabled
// skip so the owner sees why it waits. A second held tick does not rewrite the row (no churn).
// Once the owner enables the pin, the next tick fires it and marks it fired. A recurring row
// keeps the advancing skip (above).
//
// MUTATION: drop process()'s holds (pre-fire check and post-fire arm); the once row then takes
// advance's transient arm and records no last_fire, and this test fails. Drop the
// lastFireIsHold check; the second held tick rewrites updated_at, and this test fails.
func TestScheduleFireOnceCredentialDisabledHoldsLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	runs := workersvc.New(q, box, workersvc.Params{})
	runs.SetTxBeginner(pool)
	sched := New(q, runs, nil, nil, nil, nil, time.Minute, nil)
	scID := uuid.New()
	due := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	insertRecurringPromptSchedule(ctx, t, pool, scID, userID, repoID, due)
	if _, err := pool.Exec(ctx, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', 'def', true, $3, 'master')`, uuid.New(), userID, []byte("x")); err != nil {
		t.Fatal(err)
	}
	pin := seedDisabledAnthropicToken(ctx, t, pool, userID, false)
	if _, err := pool.Exec(ctx, `UPDATE run_schedules SET timing = 'once', cron_expr = NULL, run_at = $2,
	          credential_override_mode = 'pinned', credential_override_secret_id = $3 WHERE id = $1`, scID, due, pin); err != nil {
		t.Fatal(err)
	}

	sched.Boot(ctx)

	held := assertOnceHeldLiveDB(ctx, t, q, scID, due)
	if n := countPromptRunsForSchedule(ctx, t, pool, scID); n != 0 {
		t.Fatalf("runs created = %d, want 0 while the pin is disabled", n)
	}
	sched.Boot(ctx)
	if again := assertOnceHeldLiveDB(ctx, t, q, scID, due); !again.UpdatedAt.Time.Equal(held.UpdatedAt.Time) {
		t.Fatalf("a second held tick rewrote the row: updated_at %v -> %v, want unchanged", held.UpdatedAt.Time, again.UpdatedAt.Time)
	}

	if _, err := pool.Exec(ctx, `UPDATE user_secrets SET disabled_at = NULL, enablement_rev = enablement_rev + 1 WHERE id = $1`, pin); err != nil {
		t.Fatal(err)
	}
	sched.Boot(ctx)

	got, err := q.GetRunSchedule(ctx, scID)
	if err != nil {
		t.Fatalf("GetRunSchedule: %v", err)
	}
	if got.Status != "fired" || got.NextFireAt.Valid {
		t.Fatalf("after enable: status=%q next_fire_at=%+v, want fired with no next fire", got.Status, got.NextFireAt)
	}
	if n := countPromptRunsForSchedule(ctx, t, pool, scID); n != 1 {
		t.Fatalf("runs created after enable = %d, want 1", n)
	}
}

// TestScheduleFireOnceHarnessDisabledHoldsWithoutForgeLiveDB (PRD #1732 D2/D15): a due ONE-TIME
// issue schedule pinned to a harness whose credentials are all disabled is held by the pre-fire
// check against real Postgres, before the fire resolves the repo's forge: no GetIssue is made on
// either held tick, and last_fire records credential_disabled. Once a credential is enabled the
// next tick reaches the forge again.
//
// MUTATION: drop process()'s pre-fire ScheduleCredentialDisabled check; each held tick then
// calls GetIssue, and this test fails.
func TestScheduleFireOnceHarnessDisabledHoldsWithoutForgeLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	runs := workersvc.New(q, box, workersvc.Params{})
	runs.SetTxBeginner(pool)
	fb := &fakeBuilder{f: &fakeForge{err: errors.New("forge unreachable in this test")}}
	sched := New(q, runs, fb, nil, nil, nil, time.Minute, nil)
	tok := seedDisabledAnthropicToken(ctx, t, pool, userID, true)
	scID := uuid.New()
	// Boot claims every due row in the shared database, so this row's forge calls are told
	// apart by a unique issue iid, and the row (never fired: the forge errors) is removed after.
	iid := int64(uuid.New().ID()%1_000_000_000) + 1_000_000
	due := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `INSERT INTO run_schedules
	          (id, user_id, repo_id, target, issue_iid, timing, run_at, next_fire_at, timezone, auto_approve, wait_on_limit, enabled, status, origin, harness)
	          VALUES ($1, $2, $3, 'issue', $5, 'once', $4, $4, 'UTC', true, false, true, 'active', 'user', 'claude')`,
		scID, userID, repoID, due, iid); err != nil {
		t.Fatalf("persist once issue schedule: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM run_schedules WHERE id = $1`, scID); err != nil {
			t.Errorf("remove schedule: %v", err)
		}
	})
	getIssueCalls := func() int {
		n := 0
		for _, got := range fb.f.getIID {
			if got == iid {
				n++
			}
		}
		return n
	}

	sched.Boot(ctx)
	sched.Boot(ctx)

	assertOnceHeldLiveDB(ctx, t, q, scID, due)
	if n := getIssueCalls(); n != 0 {
		t.Fatalf("GetIssue calls over two held ticks = %d, want 0", n)
	}

	if _, err := pool.Exec(ctx, `UPDATE user_secrets SET disabled_at = NULL, enablement_rev = enablement_rev + 1 WHERE id = $1`, tok); err != nil {
		t.Fatal(err)
	}
	sched.Boot(ctx)
	if n := getIssueCalls(); n != 1 {
		t.Fatalf("GetIssue calls after enable = %d, want 1 (the row is no longer held)", n)
	}
}

// assertOnceHeldLiveDB asserts a one-time row is held on credential_disabled: still active and
// due at the same next_fire_at, never marked fired, and its last_fire records the skip.
func assertOnceHeldLiveDB(ctx context.Context, t *testing.T, q *store.Queries, scID uuid.UUID, due time.Time) store.RunSchedule {
	t.Helper()
	got, err := q.GetRunSchedule(ctx, scID)
	if err != nil {
		t.Fatalf("GetRunSchedule: %v", err)
	}
	if got.Status != "active" || !got.NextFireAt.Valid || !got.NextFireAt.Time.Equal(due) || got.LastFiredAt.Valid {
		t.Fatalf("held once row: status=%q next_fire_at=%+v last_fired_at=%+v, want active, still due at %v, never fired",
			got.Status, got.NextFireAt, got.LastFiredAt, due)
	}
	if reasons := fetchScheduleLastFireReasons(t, got); len(reasons) != 1 || reasons[0] != string(SkipCredentialDisabled) {
		t.Fatalf("held once row last_fire skip reasons = %v, want [credential_disabled]", reasons)
	}
	return got
}
