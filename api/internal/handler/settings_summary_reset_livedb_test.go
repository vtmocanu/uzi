package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3248: a blank or whitespace-only summary_model / codex_summary_model PUT is
// the explicit reset. It stores exactly ” (trim-at-storage), which the admin GET
// reads back as the built-in with source "default". A bad Codex-lane value is a 400.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one.
func TestUpdateSettingsSummaryModelResetLiveDB(t *testing.T) {
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

	admin := mkSecretUser(t, pool)
	h := &Handler{
		pool:     pool,
		q:        store.New(pool),
		box:      newHandlerTestBox(t),
		settings: settings.New(store.New(pool), time.Minute),
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key IN ('summary_model', 'codex_summary_model')`)
	})

	for _, tc := range []struct{ key, pin, builtin string }{
		{settings.KeySummaryModel, "sonnet", "haiku"},
		{settings.KeyCodexSummaryModel, "gpt-6-astra", "gpt-6-luna"},
	} {
		put := func(value string) int {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"settings": map[string]string{tc.key: value}})
			rec := httptest.NewRecorder()
			h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", string(body), admin, nil))
			return rec.Code
		}
		get := func() (value, source string) {
			t.Helper()
			rec := httptest.NewRecorder()
			h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
			var resp struct {
				Settings map[string]string `json:"settings"`
				Sources  map[string]string `json:"sources"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return resp.Settings[tc.key], resp.Sources[tc.key]
		}
		for _, blank := range []string{"", "   "} {
			if code := put(tc.pin); code != http.StatusOK {
				t.Fatalf("%s: PUT %q: status %d", tc.key, tc.pin, code)
			}
			if v, src := get(); v != tc.pin || src != "db" {
				t.Fatalf("%s after pin: value %q source %q, want %s/db", tc.key, v, src, tc.pin)
			}
			if code := put(blank); code != http.StatusOK {
				t.Fatalf("%s: PUT %q: status %d, want 200", tc.key, blank, code)
			}
			var stored string
			if err := pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, tc.key).Scan(&stored); err != nil {
				t.Fatalf("%s: read stored row: %v", tc.key, err)
			}
			if stored != "" {
				t.Errorf("%s: PUT %q stored %q, want exactly ''", tc.key, blank, stored)
			}
			if v, src := get(); v != tc.builtin || src != "default" {
				t.Errorf("%s after reset %q: value %q source %q, want %s/default", tc.key, blank, v, src, tc.builtin)
			}
		}
	}
	if code := func() int {
		rec := httptest.NewRecorder()
		h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", `{"settings":{"codex_summary_model":"opus"}}`, admin, nil))
		return rec.Code
	}(); code != http.StatusBadRequest {
		t.Errorf("codex_summary_model=opus: status %d, want 400", code)
	}
}
