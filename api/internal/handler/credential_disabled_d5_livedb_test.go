package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// credential_disabled_d5_livedb_test.go pins PRD #1732 D5 and D15 on the create and schedule
// surfaces against real Postgres: an explicit pin onto a disabled token is refused with a 409
// naming Settings and nothing is written (no run, no schedule, unchanged override columns), and
// an explicit harness whose credentials are all disabled is refused (422
// no_credential_for_harness, naming Settings) with no run and no fallback. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// TestCreateRunDisabledCredentialRefusedLiveDB.
//
// MUTATION: drop the meta.Disabled check in validateCredentialOverrideOn; the create then
// persists a run pinned to the disabled token (201) and this test fails.
func TestCreateRunDisabledCredentialRefusedLiveDB(t *testing.T) {
	ctx := context.Background()
	t.Run("pinned disabled token is 409", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5731, 7, protClean)
		seedOwnedAnthropicToken(ctx, t, f, f.owner.ID, "enabled-"+uuid.NewString())
		pin := seedOwnedAnthropicToken(ctx, t, f, f.owner.ID, "parked-"+uuid.NewString())
		mustExecT(ctx, t, f.pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, pin)
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "pinned", "secret_id": pin.String()},
		})
		assertSettings409(t, w)
		assertNoRun(ctx, t, f, repoID)
	})
	t.Run("explicit harness with every credential disabled is refused", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5732, 7, protClean)
		// Every Anthropic token the owner holds (the fixture's default included) is disabled,
		// and the slot has no default (D4).
		mustExecT(ctx, t, f.pool, `UPDATE user_secrets SET disabled_at = now(), is_default = false
		    WHERE user_id = $1 AND kind = 'anthropic_token'`, f.owner.ID)
		w := f.createRunWithOverride(t, repoID, map[string]any{"issue_iid": 7, "harness": "claude"})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
		}
		for _, want := range []string{"no_credential_for_harness", "Settings"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("body = %s, want it to carry %q", w.Body.String(), want)
			}
		}
		assertNoRun(ctx, t, f, repoID)
	})
}

// TestScheduleDisabledCredentialRefusedLiveDB: a schedule create or edit pinning a disabled
// token is a 409 and writes nothing.
//
// MUTATION: drop the meta.Disabled check in validateCredentialOverrideOn; the create persists
// the disabled pin (201) and this test fails.
func TestScheduleDisabledCredentialRefusedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	pin := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "parked")
	mustExecT(ctx, t, f.pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, pin)
	count := func() int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM run_schedules WHERE user_id = $1`, f.owner.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	pinned := `"credential_override":{"mode":"pinned","secret_id":"` + pin.String() + `"}`
	if _, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"weekly","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC",`+pinned+`}`); code != http.StatusConflict {
		t.Fatalf("create status = %d, want 409", code)
	}
	if n := count(); n != before {
		t.Fatalf("schedules = %d, want %d: a refused create wrote a row", n, before)
	}

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"weekly","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("baseline create status = %d, want 201", code)
	}
	rec := f.patchScheduleRaw(t, f.owner.ID, uuid.MustParse(dto.ID), `{"prompt":"edited",`+pinned+`}`)
	assertSettings409(t, rec)
	if mode, secret := f.scheduleOverrideColumns(ctx, t, dto.ID); mode != nil || secret != nil {
		t.Fatalf("override columns after a refused edit = (%v,%v), want unchanged NULL", mode, secret)
	}
	var prompt string
	if err := f.pool.QueryRow(ctx, `SELECT prompt FROM run_schedules WHERE id = $1`, dto.ID).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if prompt != "weekly" {
		t.Fatalf("prompt = %q after a refused edit, want the edit not applied", prompt)
	}
}
