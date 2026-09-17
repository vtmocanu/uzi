package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// PRD #1429 M4a (Part A): the schedule per-run harness pin, end-to-end through the
// handlers against a REAL Postgres. These mirror schedules_credential_livedb_test.go's
// #1247 credential_override presence tests exactly, for the twin field: persistence, the
// presence-aware seed-and-keep vs validate/write split, the invalid-enum 400, and the
// write-time enforcement that a pinned-Codex schedule can never ALSO store an Anthropic
// override in the SAME request (the fire-time codexOverrideConflict gate in schedsvc
// documents this as already refused by "the create/edit validator" — these tests are that
// validator).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// scheduleHarnessColumn reads the harness column off a run_schedules row.
func (f scheduleFixture) scheduleHarnessColumn(ctx context.Context, t *testing.T, id string) *string {
	t.Helper()
	var harness *string
	if err := f.pool.QueryRow(ctx,
		`SELECT harness FROM run_schedules WHERE id = $1`, id).Scan(&harness); err != nil {
		t.Fatalf("read harness column: %v", err)
	}
	return harness
}

// TestCreateScheduleHarnessPersistsLiveDB: a create carrying an explicit harness pin
// persists the column and the response DTO carries it.
func TestCreateScheduleHarnessPersistsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"weekly","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","harness":"codex"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.Harness == nil || *dto.Harness != "codex" {
		t.Fatalf("dto harness = %v, want \"codex\"", dto.Harness)
	}
	got := f.scheduleHarnessColumn(ctx, t, dto.ID)
	if got == nil || *got != "codex" {
		t.Fatalf("persisted harness column = %v, want \"codex\"", got)
	}
}

// TestCreateScheduleHarnessAbsentIsNullLiveDB: a create with no harness field leaves the
// column NULL (implicit D11 at fire time), byte-identical to a pre-M4a schedule.
func TestCreateScheduleHarnessAbsentIsNullLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"weekly","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.Harness != nil {
		t.Fatalf("dto harness = %v, want nil (implicit)", dto.Harness)
	}
	if got := f.scheduleHarnessColumn(ctx, t, dto.ID); got != nil {
		t.Fatalf("persisted harness column = %v, want NULL", got)
	}
}

// TestCreateScheduleHarnessInvalid400LiveDB: a harness value outside the closed claude|codex
// enum is a 400 and creates no row.
func TestCreateScheduleHarnessInvalid400LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	_, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","harness":"gpt"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid harness status = %d, want 400", code)
	}
}

// TestScheduleOmittedRetimeKeepsStoredHarnessLiveDB: a retime that omits harness on an
// existing pinned schedule leaves the stored pin unchanged (seed-and-keep); a PRESENT
// explicit null, by contrast, clears it back to implicit.
func TestScheduleOmittedRetimeKeepsStoredHarnessLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","harness":"claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id, _ := uuid.Parse(dto.ID)

	rec := f.patchScheduleRaw(t, f.owner.ID, id, `{"cron_expr":"0 6 * * *"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retime status = %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	if got := f.scheduleHarnessColumn(ctx, t, dto.ID); got == nil || *got != "claude" {
		t.Fatalf("harness after omitted-harness retime = %v, want the stored \"claude\" preserved", got)
	}

	rec = f.patchScheduleRaw(t, f.owner.ID, id, `{"harness":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit clear status = %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	if got := f.scheduleHarnessColumn(ctx, t, dto.ID); got != nil {
		t.Fatalf("harness after explicit null = %v, want NULL (cleared)", got)
	}
}

// TestScheduleHarnessCodexPlusAnthropicOverrideSameRequest422LiveDB: pinning harness=codex
// and an Anthropic credential_override in the SAME create/patch request is a 422 and writes
// no row/column — the write-time enforcement of the invariant schedsvc's codexOverrideConflict
// comment already assumes ("a pinned-Codex schedule can never store an Anthropic override —
// the create/edit validator refuses it"). This exercises resolveScheduleHarness running BEFORE
// resolveScheduleCredentialOverride and feeding it the post-edit harness, not the stale one.
func TestScheduleHarnessCodexPlusAnthropicOverrideSameRequest422LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Create: harness=codex + an override in one request.
	_, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","harness":"codex","credential_override":{"mode":"auto"}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("create harness=codex + override status = %d, want 422", code)
	}

	// Patch: an existing Claude-pinned schedule re-pinned to codex + an override in the same
	// PATCH must be refused the same way (not just accepted because the row was Claude before
	// this edit).
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"y","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","harness":"claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id, _ := uuid.Parse(dto.ID)
	rec := f.patchScheduleRaw(t, f.owner.ID, id, `{"harness":"codex","credential_override":{"mode":"auto"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch harness=codex + override status = %d, want 422 — body %s", rec.Code, rec.Body.String())
	}
	// Neither column moved: the whole request is refused, nothing partially written.
	if got := f.scheduleHarnessColumn(ctx, t, dto.ID); got == nil || *got != "claude" {
		t.Fatalf("harness after refused patch = %v, want the stored \"claude\" untouched", got)
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, dto.ID)
	if mode != nil || secret != nil {
		t.Fatalf("override columns after refused patch = (%v,%v), want both NULL untouched", mode, secret)
	}
}
