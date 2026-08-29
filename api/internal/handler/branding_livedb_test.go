package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #685 M1 live-DB proof for the instance-branding endpoints. The public read, the
// logo bytes route, the admin upload/delete and the settings→branding round-trip are
// hand-written Go over generated code + a real settings cache, so their risk is
// invisible to a fake: whether the upsert round-trips the BYTEA, whether the ETag/304
// path works, whether a PUT /admin/settings reflects on the next GET /api/branding, and
// — the R1 guard — whether the public JSON exposes ONLY the branding keys.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func brandingHandler(t *testing.T) (*Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run against a throwaway Postgres for live-DB coverage")
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
	// The branding tables are singleton-keyed (branding_assets by slot, the six
	// app_settings keys by key), so state persists across runs and across tests
	// sharing this DB. Reset it so each test starts from the unbranded default.
	if _, err := pool.Exec(ctx, `DELETE FROM branding_assets`); err != nil {
		t.Fatalf("reset branding_assets: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM app_settings WHERE key LIKE 'app_logo\_%' OR key LIKE 'brand\_%'`); err != nil {
		t.Fatalf("reset branding settings: %v", err)
	}
	h := &Handler{
		pool:     pool,
		q:        store.New(pool),
		settings: settings.New(store.New(pool), time.Minute),
	}
	return h, pool
}

func getBranding(t *testing.T, h *Handler) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GetBranding(rec, httptest.NewRequest(http.MethodGet, "/api/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/branding: code %d, body %s", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode /api/branding: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

// A fresh DB returns the unbranded default JSON AND — the R1 guard — contains ONLY the
// nine branding fields, never public_base_url or any other settings key.
func TestBrandingPublicReadDefaultsAndAllowlistLiveDB(t *testing.T) {
	h, _ := brandingHandler(t)
	m := getBranding(t, h)

	wantKeys := []string{
		"app_logo_mode", "app_logo_preset", "app_logo_present", "app_logo_keep_name",
		"brand_mode", "brand_company", "brand_placement", "brand_plaque",
		"brand_logo_present",
	}
	gotKeys := make([]string, 0, len(m))
	for k := range m {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("public branding keys = %v, want exactly %v", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("public branding keys = %v, want exactly %v (R1 leak guard)", gotKeys, wantKeys)
		}
	}
	// A representative non-branding settings key must never appear.
	if _, leaked := m["public_base_url"]; leaked {
		t.Error("GET /api/branding leaked public_base_url — R1 allowlist breached")
	}

	// Default (unbranded) values.
	assertJSON(t, m, "app_logo_mode", `"default"`)
	assertJSON(t, m, "app_logo_preset", `""`)
	assertJSON(t, m, "app_logo_keep_name", `true`)
	assertJSON(t, m, "brand_mode", `"none"`)
	assertJSON(t, m, "brand_company", `""`)
	assertJSON(t, m, "brand_placement", `"below"`)
	assertJSON(t, m, "brand_plaque", `false`)
	assertJSON(t, m, "app_logo_present", `false`)
	assertJSON(t, m, "brand_logo_present", `false`)
}

func assertJSON(t *testing.T, m map[string]json.RawMessage, key, want string) {
	t.Helper()
	if got := string(m[key]); got != want {
		t.Errorf("%s = %s, want %s", key, got, want)
	}
}

// The admin upload accepts each allowed type at and under the 256 KiB cap, the bytes
// round-trip through BYTEA, the public logo route serves them with a strong ETag, a
// second request with If-None-Match is a 304, and a delete reverts to 404. Presence in
// GET /api/branding flips with the upload/delete.
func TestBrandingLogoUploadServeDeleteLiveDB(t *testing.T) {
	h, pool := brandingHandler(t)
	admin := mkSecretUser(t, pool)

	// Before any upload: the logo route 404s and presence is false.
	rec := httptest.NewRecorder()
	h.GetBrandingLogo(rec, userReq(http.MethodGet, "/api/branding/logo/app", "", admin, map[string]string{"slot": "app"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("logo GET before upload: code %d, want 404", rec.Code)
	}
	if got := string(getBranding(t, h)["app_logo_present"]); got != "false" {
		t.Fatalf("app_logo_present before upload = %s, want false", got)
	}

	cases := []struct {
		slot        string
		contentType string
		size        int
	}{
		{"app", "image/png", maxBrandingLogoBytes},         // at the cap
		{"brand", "image/webp", maxBrandingLogoBytes - 10}, // under the cap
		{"app", "image/svg+xml", 128},                      // svg, replaces the app png
	}
	for _, tc := range cases {
		body := bytes.Repeat([]byte{'z'}, tc.size)
		rec = httptest.NewRecorder()
		h.PutBrandingLogo(rec, brandingUploadReqUser(tc.slot, tc.contentType, body, admin))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %s %s (%d bytes): code %d, body %s", tc.slot, tc.contentType, tc.size, rec.Code, rec.Body.String())
		}

		// Serve it back: 200, right content-type, exact bytes, a strong ETag.
		rec = httptest.NewRecorder()
		h.GetBrandingLogo(rec, userReq(http.MethodGet, "/api/branding/logo/"+tc.slot, "", admin, map[string]string{"slot": tc.slot}))
		if rec.Code != http.StatusOK {
			t.Fatalf("logo GET %s: code %d, body %s", tc.slot, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.contentType {
			t.Errorf("logo GET %s Content-Type = %q, want %q", tc.slot, ct, tc.contentType)
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("logo GET %s returned %d bytes, want %d", tc.slot, rec.Body.Len(), tc.size)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("logo GET %s: no ETag", tc.slot)
		}
		if cc := rec.Header().Get("Cache-Control"); cc == "" {
			t.Errorf("logo GET %s: no Cache-Control", tc.slot)
		}
		// Defense-in-depth headers on the 200 path (admin-uploaded bytes, svg allowed).
		if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
			t.Errorf("logo GET %s X-Content-Type-Options = %q, want nosniff", tc.slot, xcto)
		}
		// Route-local CSP sandboxes a directly-navigated SVG so its inline scripts/SMIL
		// cannot execute in the app origin (nosniff does not cover that vector).
		if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox; default-src 'none'" {
			t.Errorf("logo GET %s Content-Security-Policy = %q, want \"sandbox; default-src 'none'\"", tc.slot, csp)
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
			t.Errorf("logo GET %s Content-Disposition = %q, want inline", tc.slot, cd)
		}

		// If-None-Match with the same ETag is a 304 with no body.
		req := userReq(http.MethodGet, "/api/branding/logo/"+tc.slot, "", admin, map[string]string{"slot": tc.slot})
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		h.GetBrandingLogo(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("logo GET %s with matching If-None-Match: code %d, want 304", tc.slot, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 for %s carried a body of %d bytes", tc.slot, rec.Body.Len())
		}
	}

	// Both slots now present.
	m := getBranding(t, h)
	assertJSON(t, m, "app_logo_present", `true`)
	assertJSON(t, m, "brand_logo_present", `true`)

	// Delete the app slot → 404 again and presence false; brand stays.
	rec = httptest.NewRecorder()
	h.DeleteBrandingLogo(rec, userReq(http.MethodDelete, "/api/admin/branding/logo/app", "", admin, map[string]string{"slot": "app"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete app logo: code %d, body %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.GetBrandingLogo(rec, userReq(http.MethodGet, "/api/branding/logo/app", "", admin, map[string]string{"slot": "app"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("logo GET app after delete: code %d, want 404", rec.Code)
	}
	m = getBranding(t, h)
	assertJSON(t, m, "app_logo_present", `false`)
	assertJSON(t, m, "brand_logo_present", `true`)
}

// A branding config key set through PUT /admin/settings reflects on the very next GET
// /api/branding (the cache is invalidated on write).
func TestBrandingConfigReflectsSettingsPutLiveDB(t *testing.T) {
	h, pool := brandingHandler(t)
	admin := mkSecretUser(t, pool)

	assertJSON(t, getBranding(t, h), "brand_mode", `"none"`)

	body, _ := json.Marshal(map[string]any{
		"settings": map[string]string{
			"brand_mode":    "text",
			"brand_company": "Acme, Inc.",
		},
	})
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, userReq(http.MethodPut, "/api/admin/settings", string(body), admin, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /admin/settings: code %d, body %s", rec.Code, rec.Body.String())
	}

	m := getBranding(t, h)
	assertJSON(t, m, "brand_mode", `"text"`)
	assertJSON(t, m, "brand_company", `"Acme, Inc."`)
}

// brandingUploadReqUser is brandingUploadReq with a caller-supplied user id so the
// live-DB upsert's updated_by FK references a row that exists.
func brandingUploadReqUser(slot, contentType string, body []byte, user uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/admin/branding/logo/"+slot, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	ctx := mw.ContextWithUser(req.Context(), store.User{ID: user, IsActive: true, IsAdmin: true})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slot", slot)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}
