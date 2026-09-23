package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestUserSettingsLegacyProjectionFollowsEffectiveHarnessLiveDB pins the split
// between D3's legacy input target and the compatibility column's effective
// harness. An unusable Codex pin must not leave a Codex value for an old API
// image to read while new runs select Claude. Clearing the pin with only Codex
// usable still routes a legacy input to Claude, but mirrors Codex for rollback.
func TestUserSettingsLegacyProjectionFollowsEffectiveHarnessLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, wsvc: workersvc.New(q, nil, workersvc.Params{})}
	userID := mkSecretUser(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
		 VALUES (gen_random_uuid(), $1, 'anthropic_token', 'claude', 'ct', 'master')`, userID); err != nil {
		t.Fatalf("seed Anthropic token: %v", err)
	}

	put := func(body string) settingsLanes {
		t.Helper()
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, userID, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: status %d, body %s", body, rec.Code, rec.Body.String())
		}
		return decodeLanes(t, rec.Body.Bytes())
	}
	storedLegacy := func(want string) {
		t.Helper()
		s, err := q.GetUserSettings(ctx, userID)
		if err != nil {
			t.Fatalf("GetUserSettings: %v", err)
		}
		if !s.DefaultModel.Valid || s.DefaultModel.String != want {
			t.Fatalf("stored legacy default_model = %+v, want %q", s.DefaultModel, want)
		}
	}

	grouped := `{"default_harness":"codex","default_claude_model":"opus","default_codex_model":"gpt-6-sol"}`
	got := put(grouped)
	if got.DefaultModel == nil || *got.DefaultModel != "opus" {
		t.Fatalf("Claude-only user with unusable Codex pin: response default_model = %v, want opus", got.DefaultModel)
	}
	storedLegacy("opus") // would be gpt-6-sol before the fix

	mkLinkedCodexAccountH(ctx, t, pool, q, userID, true)
	got = put(grouped)
	if got.DefaultModel == nil || *got.DefaultModel != "gpt-6-sol" {
		t.Fatalf("both harnesses usable with Codex pin: response default_model = %v, want gpt-6-sol", got.DefaultModel)
	}
	storedLegacy("gpt-6-sol")

	if _, err := pool.Exec(ctx,
		`DELETE FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'`, userID); err != nil {
		t.Fatalf("remove Anthropic token: %v", err)
	}
	got = put(`{"default_harness":null,"default_model":"sonnet"}`)
	if got.DefaultClaudeModel == nil || *got.DefaultClaudeModel != "sonnet" {
		t.Fatalf("cleared pin: legacy input did not target Claude lane: %+v", got.DefaultClaudeModel)
	}
	if got.DefaultModel == nil || *got.DefaultModel != "gpt-6-sol" {
		t.Fatalf("Codex-only user after clearing pin: response default_model = %v, want gpt-6-sol", got.DefaultModel)
	}
	storedLegacy("gpt-6-sol")
}
