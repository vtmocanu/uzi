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

// PRD #3245 M1: a blank judge_model PUT is the explicit reset. A blank or
// whitespace-only submission stores exactly ” (trim-at-storage), which the admin
// GET reads back as the built-in with source "default".
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one.
func TestUpdateSettingsJudgeModelResetLiveDB(t *testing.T) {
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
		_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'judge_model'`)
	})

	put := func(value string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"settings": map[string]string{settings.KeyJudgeModel: value}})
		rec := httptest.NewRecorder()
		h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", string(body), admin, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %q: status %d, body %s", value, rec.Code, rec.Body.String())
		}
	}
	get := func() (value, source string, defaults map[string]string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
		var resp struct {
			Settings map[string]string `json:"settings"`
			Sources  map[string]string `json:"sources"`
			Defaults map[string]string `json:"defaults"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Settings[settings.KeyJudgeModel], resp.Sources[settings.KeyJudgeModel], resp.Defaults
	}

	for _, blank := range []string{"", "  "} {
		put("haiku")
		if v, src, _ := get(); v != "haiku" || src != "db" {
			t.Fatalf("after pin: value %q source %q, want haiku/db", v, src)
		}
		put(blank)
		var stored string
		if err := pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = 'judge_model'`).Scan(&stored); err != nil {
			t.Fatalf("read stored row: %v", err)
		}
		if stored != "" {
			t.Errorf("PUT %q stored %q, want exactly ''", blank, stored)
		}
		v, src, defaults := get()
		if v != "opus" || src != "default" {
			t.Errorf("after reset %q: value %q source %q, want opus/default", blank, v, src)
		}
		if defaults[settings.KeyJudgeModel] != "opus" {
			t.Errorf("defaults[judge_model] = %q, want opus", defaults[settings.KeyJudgeModel])
		}
	}
}

// PRD #3245: the instance Codex lane. A PUT stores the trimmed value; a blank or
// whitespace-only PUT after a stored value leaves the row ” and the admin GET reads
// back the Codex built-in with source "default".
func TestUpdateSettingsCodexJudgeModelResetLiveDB(t *testing.T) {
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
		_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'codex_judge_model'`)
	})

	put := func(value string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"settings": map[string]string{settings.KeyCodexJudgeModel: value}})
		rec := httptest.NewRecorder()
		h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", string(body), admin, nil))
		return rec.Code
	}
	stored := func() string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = 'codex_judge_model'`).Scan(&v); err != nil {
			t.Fatalf("read stored row: %v", err)
		}
		return v
	}
	get := func() (value, source string, defaults map[string]string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
		var resp struct {
			Settings map[string]string `json:"settings"`
			Sources  map[string]string `json:"sources"`
			Defaults map[string]string `json:"defaults"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Settings[settings.KeyCodexJudgeModel], resp.Sources[settings.KeyCodexJudgeModel], resp.Defaults
	}

	for _, blank := range []string{"", "   "} {
		if code := put(" gpt-6-sol "); code != http.StatusOK {
			t.Fatalf("PUT padded curated id: status %d", code)
		}
		if got := stored(); got != "gpt-6-sol" {
			t.Fatalf("padded PUT stored %q, want trimmed gpt-6-sol", got)
		}
		if v, src, _ := get(); v != "gpt-6-sol" || src != "db" {
			t.Fatalf("after pin: value %q source %q, want gpt-6-sol/db", v, src)
		}
		if code := put(blank); code != http.StatusOK {
			t.Fatalf("PUT %q: status %d", blank, code)
		}
		if got := stored(); got != "" {
			t.Errorf("PUT %q stored %q, want exactly ''", blank, got)
		}
		v, src, defaults := get()
		if v != "gpt-6.1-sol" || src != "default" {
			t.Errorf("after reset %q: value %q source %q, want gpt-6.1-sol/default", blank, v, src)
		}
		if defaults[settings.KeyCodexJudgeModel] != "gpt-6.1-sol" {
			t.Errorf("defaults[codex_judge_model] = %q, want gpt-6.1-sol", defaults[settings.KeyCodexJudgeModel])
		}
	}
	for _, bad := range []string{"opus", "gpt-6-luna", "gpt-7-x", "two words"} {
		if code := put(bad); code != http.StatusBadRequest {
			t.Errorf("PUT %q: status %d, want 400", bad, code)
		}
	}
}
