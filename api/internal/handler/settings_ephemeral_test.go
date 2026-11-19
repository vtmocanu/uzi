package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4212: the ephemeral/hosted worker settings through the real admin PUT.

// An invalid value is refused with a 400 naming the key, before any write, and the
// effective value the admin GET reports is unchanged.
func TestUpdateSettingsEphemeralRejectsInvalid(t *testing.T) {
	admin := adminUser()
	cases := []struct{ key, value string }{
		{settings.KeyEphemeralMaxPerUser, "21"},
		{settings.KeyEphemeralMaxPerUser, "-1"},
		{settings.KeyEphemeralMaxPerUser, "abc"},
		{settings.KeyEphemeralDefaultSize, "XL"},
		{settings.KeyHostedDefaultSize, "has space"},
		{settings.KeyEphemeralLease, "3h"},
		{settings.KeyEphemeralLease, "-1m"},
		{settings.KeyEphemeralLease, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			h := newSettingsHandler()
			body, _ := json.Marshal(map[string]any{"settings": map[string]string{tc.key: tc.value}})
			rec := httptest.NewRecorder()
			h.UpdateSettings(rec, putSettings(&admin, string(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.key) {
				t.Fatalf("400 body %q should name the key %q", rec.Body.String(), tc.key)
			}
			av, err := h.settings.AdminView(context.Background())
			if err != nil {
				t.Fatalf("AdminView: %v", err)
			}
			if av.Values[tc.key] != settings.Defaults[tc.key] || av.Sources[tc.key] != "default" {
				t.Fatalf("value after rejected PUT = %q (%s), want unchanged default %q",
					av.Values[tc.key], av.Sources[tc.key], settings.Defaults[tc.key])
			}
		})
	}
}

// A key fixed by the environment overlay refuses a write with a 409.
func TestUpdateSettingsEphemeralEnvLockedKeyConflicts(t *testing.T) {
	admin := adminUser()
	cache := settings.New(&settingsStore{}, time.Minute)
	cache.ConfigureSecrets(slackTestBox(t), map[string]string{
		settings.KeyEphemeralMaxPerUser:  "3",
		settings.KeyEphemeralDefaultSize: "s",
		settings.KeyEphemeralLease:       "30m",
	})
	h := &Handler{q: store.New(fakeCountDB{}), settings: cache}
	for key, value := range map[string]string{
		settings.KeyEphemeralMaxPerUser:  "4",
		settings.KeyEphemeralDefaultSize: "m",
		settings.KeyEphemeralLease:       "1h",
	} {
		body, _ := json.Marshal(map[string]any{"settings": map[string]string{key: value}})
		rec := httptest.NewRecorder()
		h.UpdateSettings(rec, putSettings(&admin, string(body)))
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409 (body %s)", key, rec.Code, rec.Body.String())
		}
	}
	// hosted_default_size has no env var, so main never overlays it.
	if cache.IsEnvSourced(settings.KeyHostedDefaultSize) {
		t.Error("hosted_default_size must not be env-locked")
	}
}

// A blank or whitespace-only PUT is the reset: the row is stored as exactly ""
// (trim-at-storage) and the admin GET reads back the default with source "default".
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one.
func TestUpdateSettingsEphemeralResetLiveDB(t *testing.T) {
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
	// PRD #4213 M3: a provision and a default-size write now check the size row exists.
	if err := store.ReconcileBuiltinWorkerSizes(ctx, pool); err != nil {
		t.Fatalf("seed builtin sizes: %v", err)
	}

	admin := mkSecretUser(t, pool)
	h := &Handler{
		pool:     pool,
		q:        store.New(pool),
		box:      newHandlerTestBox(t),
		settings: settings.New(store.New(pool), time.Minute),
	}
	keys := []string{settings.KeyEphemeralMaxPerUser, settings.KeyEphemeralDefaultSize,
		settings.KeyHostedDefaultSize, settings.KeyEphemeralLease}
	pinned := map[string]string{
		settings.KeyEphemeralMaxPerUser:  "5",
		settings.KeyEphemeralDefaultSize: "s",
		settings.KeyHostedDefaultSize:    "m",
		settings.KeyEphemeralLease:       "30m",
	}
	t.Cleanup(func() {
		for _, k := range keys {
			_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = $1`, k)
		}
	})

	put := func(key, value string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"settings": map[string]string{key: value}})
		rec := httptest.NewRecorder()
		h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", string(body), admin, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s=%q: status %d, body %s", key, value, rec.Code, rec.Body.String())
		}
	}
	get := func(key string) (value, source string) {
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
		return resp.Settings[key], resp.Sources[key]
	}

	for _, key := range keys {
		for _, blank := range []string{"", "   "} {
			put(key, pinned[key])
			if v, src := get(key); v != pinned[key] || src != "db" {
				t.Fatalf("%s after pin: value %q source %q, want %q/db", key, v, src, pinned[key])
			}
			put(key, blank)
			var stored string
			if err := pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, key).Scan(&stored); err != nil {
				t.Fatalf("read stored row: %v", err)
			}
			if stored != "" {
				t.Errorf("%s: PUT %q stored %q, want exactly ''", key, blank, stored)
			}
			if v, src := get(key); v != settings.Defaults[key] || src != "default" {
				t.Errorf("%s after reset %q: value %q source %q, want %q/default", key, blank, v, src, settings.Defaults[key])
			}
		}
	}
}
