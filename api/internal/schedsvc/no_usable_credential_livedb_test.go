package schedsvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// no_usable_credential_livedb_test.go is the regression for the review fix to PRD #1429 M2's
// fire-time D11 resolution: a schedule fire for an owner with NO usable credential (no
// Anthropic token AND no usable Codex credential) must record the new, BENIGN, ADVANCING
// SkipNoUsableCredential skip — not fall through skipReasonForErr's default arm as a
// TRANSIENT error. Before this fix a real tick never advanced such a schedule's
// next_fire_at, so it re-fired (and re-hit the forge/DB) every tick forever: a tick-storm.
// It builds a real *workersvc.Service over a real Postgres (the exact RunCreator the
// scheduler fires through in production) so D11's credential-availability read is genuine,
// not a fake. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every name ends LiveDB so the store-IT sweep selects it.

// insertRecurringPromptSchedule persists a real, DUE recurring prompt schedule (next_fire_at
// in the past, hourly cron), the minimal shape a genuine tick() can claim via
// ClaimDueSchedules and fire+advance through process() — as opposed to nullHarnessOverrideSchedule
// (harness_schedule_fire_livedb_test.go), which RunNow can fire from an in-memory row alone.
func insertRecurringPromptSchedule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, userID, repoID uuid.UUID, dueAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO run_schedules
	          (id, user_id, repo_id, target, prompt, timing, cron_expr, next_fire_at, timezone, auto_approve, wait_on_limit, enabled, status, origin)
	          VALUES ($1, $2, $3, 'prompt', 'do the thing', 'recurring', '0 * * * *', $4, 'UTC', false, false, true, 'active', 'user')`,
		id, userID, repoID, dueAt); err != nil {
		t.Fatalf("persist recurring run_schedules row: %v", err)
	}
}

// fetchScheduleLastFireReasons unmarshals the recorded last_fire.skips[].reason values off a
// schedule row, or nil if last_fire is NULL/unset.
func fetchScheduleLastFireReasons(t *testing.T, sc store.RunSchedule) []string {
	t.Helper()
	if len(sc.LastFire) == 0 {
		return nil
	}
	var rec struct {
		Skips []struct {
			Reason string `json:"reason"`
		} `json:"skips"`
	}
	if err := json.Unmarshal(sc.LastFire, &rec); err != nil {
		t.Fatalf("unmarshal last_fire: %v", err)
	}
	reasons := make([]string, 0, len(rec.Skips))
	for _, s := range rec.Skips {
		reasons = append(reasons, s.Reason)
	}
	return reasons
}

// TestScheduleFireNoUsableCredentialSkipsAndAdvancesLiveDB is the review-fix regression: a
// schedule whose owner has NEITHER a usable Anthropic token NOR a usable Codex credential
// fires, records SkipNoUsableCredential, creates NO run, and — the load-bearing assertion —
// ADVANCES next_fire_at, exactly like the existing benign skips (SkipVaultLocked et al.).
// Before the fix, ErrNoUsableCredential fell through skipReasonForErr's default arm as
// transient, so advance() left next_fire_at untouched in the past and the very next tick
// would refire the same doomed candidate: a tick-storm with a forge round-trip per attempt.
func TestScheduleFireNoUsableCredentialSkipsAndAdvancesLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	// Deliberately seed NO Anthropic token and NO Codex credential for userID: D11 resolves
	// workersvc.ErrNoUsableCredential (neither harness usable, no explicit selection).

	runs := workersvc.New(q, box, workersvc.Params{})
	sched := New(q, runs, nil, nil, nil, nil, time.Minute, nil)

	scID := uuid.New()
	before := time.Now().UTC()
	due := before.Add(-time.Hour) // due an hour ago, so a real tick claims it
	insertRecurringPromptSchedule(ctx, t, pool, scID, userID, repoID, due)

	// Boot runs one real tick: ClaimDueSchedules -> process -> fireOne -> advance, the exact
	// production path a tick-storm would hammer.
	sched.Boot(ctx)

	got, err := q.GetRunSchedule(ctx, scID)
	if err != nil {
		t.Fatalf("GetRunSchedule: %v", err)
	}

	if got.Status != "active" {
		t.Fatalf("status = %q, want active (a benign skip must not park the schedule)", got.Status)
	}
	if !got.NextFireAt.Valid || !got.NextFireAt.Time.After(due) {
		t.Fatalf("next_fire_at = %+v, want it ADVANCED past the due instant %v (the tick-storm regression: an unmapped ErrNoUsableCredential leaves this untouched)", got.NextFireAt, due)
	}

	reasons := fetchScheduleLastFireReasons(t, got)
	if len(reasons) != 1 || reasons[0] != string(SkipNoUsableCredential) {
		t.Fatalf("last_fire skip reasons = %v, want exactly [%q]", reasons, SkipNoUsableCredential)
	}

	if n := countPromptRunsForSchedule(ctx, t, pool, scID); n != 0 {
		t.Fatalf("runs created for schedule = %d, want 0 (no usable credential must create no run)", n)
	}
}
