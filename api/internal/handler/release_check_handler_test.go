package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/releasecheck"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeReleaseReconciler is a handler-test double for the release-check reconciler: it
// records the call count and returns a canned Result, without any HTTP machinery.
type fakeReleaseReconciler struct {
	calls int
	res   releasecheck.Result
	err   error
}

func (f *fakeReleaseReconciler) CheckForUpdate(context.Context) (releasecheck.Result, error) {
	f.calls++
	return f.res, f.err
}

// releaseCheckHandler builds a struct-literal Handler whose release-check reads resolve
// through an in-memory settings cache, with a fixed clock and the given reconciler.
func releaseCheckHandler(version string, rec ReleaseCheckReconciler, rows ...store.AppSetting) *Handler {
	return &Handler{
		version:      version,
		settings:     settings.New(&settingsStore{rows: rows}, time.Minute),
		now:          func() time.Time { return time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) },
		releaseCheck: rec,
	}
}

func decodeReleaseCheck(t *testing.T, rec *httptest.ResponseRecorder) apitypes.ReleaseCheckStatusDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ReleaseCheck apitypes.ReleaseCheckStatusDTO `json:"release_check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return resp.ReleaseCheck
}

// TestGetReleaseCheckReturnsPersistedFacts: the admin GET reflects the persisted facts
// (Body included — admin-only), the derived signals, the running version, the toggles,
// and an "ok" status once a check has run.
func TestGetReleaseCheckReturnsPersistedFacts(t *testing.T) {
	h := releaseCheckHandler("0.11.0", nil,
		store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
		store.AppSetting{Key: settings.KeyReleaseCheckBannerEnabled, Value: "false"},
		store.AppSetting{Key: settings.KeyReleaseCheckInterval, Value: "6h"},
		store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v0.12.0"},
		store.AppSetting{Key: settings.KeyReleaseLatestName, Value: "v0.12.0"},
		store.AppSetting{Key: settings.KeyReleaseLatestBody, Value: "### Security\n- CVE fix"},
		store.AppSetting{Key: settings.KeyReleaseNotesURL, Value: "https://github.com/vtmocanu/uzi/releases/tag/v0.12.0"},
		store.AppSetting{Key: settings.KeyReleasePublishedAt, Value: "2026-08-29T10:00:00Z"},
		store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T11:00:00Z"},
	)

	rec := httptest.NewRecorder()
	h.GetReleaseCheck(rec, httptest.NewRequest(http.MethodGet, "/api/admin/release-check", nil))
	dto := decodeReleaseCheck(t, rec)

	if !dto.Enabled || dto.BannerEnabled {
		t.Errorf("toggles = enabled:%v banner:%v, want enabled:true banner:false", dto.Enabled, dto.BannerEnabled)
	}
	if dto.RunningVersion != "0.11.0" {
		t.Errorf("running_version = %q, want 0.11.0", dto.RunningVersion)
	}
	if dto.LatestTag != "v0.12.0" {
		t.Errorf("latest_tag = %q, want v0.12.0", dto.LatestTag)
	}
	if dto.Body != "### Security\n- CVE fix" {
		t.Errorf("body = %q, want the raw markdown (admin-only surface)", dto.Body)
	}
	if !dto.UpdateAvailable {
		t.Error("update_available = false, want true (0.11.0 < v0.12.0)")
	}
	if !dto.Security {
		t.Error("security = false, want true (### Security body)")
	}
	if dto.Status != "ok" {
		t.Errorf("status = %q, want ok", dto.Status)
	}
}

// TestGetReleaseCheckStatusStates: status reads "disabled" when the master toggle is
// off and "never" when enabled but no check has run.
func TestGetReleaseCheckStatusStates(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		h := releaseCheckHandler("0.11.0", nil,
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "false"})
		rec := httptest.NewRecorder()
		h.GetReleaseCheck(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if got := decodeReleaseCheck(t, rec).Status; got != "disabled" {
			t.Errorf("status = %q, want disabled", got)
		}
	})
	t.Run("never", func(t *testing.T) {
		h := releaseCheckHandler("0.11.0", nil,
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"})
		rec := httptest.NewRecorder()
		h.GetReleaseCheck(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if got := decodeReleaseCheck(t, rec).Status; got != "never" {
			t.Errorf("status = %q, want never", got)
		}
	})
}

// TestPostReleaseCheckTriggersReconciler: "Check now" calls CheckForUpdate exactly once
// and returns the refreshed DTO.
func TestPostReleaseCheckTriggersReconciler(t *testing.T) {
	rec := &fakeReleaseReconciler{res: releasecheck.Result{Status: "ok"}}
	h := releaseCheckHandler("0.11.0", rec,
		store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
		store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v0.12.0"},
		store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T11:00:00Z"},
	)

	w := httptest.NewRecorder()
	h.PostReleaseCheck(w, httptest.NewRequest(http.MethodPost, "/api/admin/release-check", nil))
	dto := decodeReleaseCheck(t, w)

	if rec.calls != 1 {
		t.Errorf("CheckForUpdate called %d times, want 1", rec.calls)
	}
	if !dto.UpdateAvailable {
		t.Error("refreshed dto update_available = false, want true")
	}
}

// TestPostReleaseCheckNilReconciler: an unset reconciler yields a clean 500, not a panic.
func TestPostReleaseCheckNilReconciler(t *testing.T) {
	h := releaseCheckHandler("0.11.0", nil,
		store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"})
	w := httptest.NewRecorder()
	h.PostReleaseCheck(w, httptest.NewRequest(http.MethodPost, "/x", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a nil reconciler", w.Code)
	}
}

// TestPostReleaseCheckErrorScrubbed: an error status forces status "error" and surfaces
// the reason with control bytes stripped (SanitizeTTY), never raw.
func TestPostReleaseCheckErrorScrubbed(t *testing.T) {
	rec := &fakeReleaseReconciler{res: releasecheck.Result{Status: "error", Message: "boom \x1b[31mred\x1b[0m"}}
	h := releaseCheckHandler("0.11.0", rec,
		store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"})

	w := httptest.NewRecorder()
	h.PostReleaseCheck(w, httptest.NewRequest(http.MethodPost, "/x", nil))
	dto := decodeReleaseCheck(t, w)

	if dto.Status != "error" {
		t.Errorf("status = %q, want error", dto.Status)
	}
	if dto.Message == "" {
		t.Fatal("error message empty, want the scrubbed reason")
	}
	if strings.ContainsRune(dto.Message, '\x1b') {
		t.Errorf("error message carries a raw ESC control byte: %q", dto.Message)
	}
}

// TestReleaseCheckBannerSnoozedDerivation pins banner_snoozed as a pure read-time
// derivation over the persisted snooze tag vs latest_tag (PRD #836 M6): false with no
// snooze tag, true when the snooze tag equals the current latest_tag, and false again
// once a NEWER latest_tag lands (the tag no longer matches → auto-expiry).
func TestReleaseCheckBannerSnoozedDerivation(t *testing.T) {
	base := []store.AppSetting{
		{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
		{Key: settings.KeyReleaseLatestTag, Value: "v0.5.0"},
		{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T11:00:00Z"},
	}
	get := func(t *testing.T, h *Handler) apitypes.ReleaseCheckStatusDTO {
		t.Helper()
		rec := httptest.NewRecorder()
		h.GetReleaseCheck(rec, httptest.NewRequest(http.MethodGet, "/api/admin/release-check", nil))
		return decodeReleaseCheck(t, rec)
	}

	t.Run("no snooze tag → false", func(t *testing.T) {
		h := releaseCheckHandler("0.4.0", nil, base...)
		if get(t, h).BannerSnoozed {
			t.Error("banner_snoozed = true with no snooze tag, want false")
		}
	})
	t.Run("snooze tag matches latest → true", func(t *testing.T) {
		rows := append(append([]store.AppSetting{}, base...),
			store.AppSetting{Key: settings.KeyReleaseBannerSnoozeTag, Value: "v0.5.0"})
		if !get(t, releaseCheckHandler("0.4.0", nil, rows...)).BannerSnoozed {
			t.Error("banner_snoozed = false when snooze tag == latest_tag, want true")
		}
	})
	t.Run("stale snooze tag after a newer release → false", func(t *testing.T) {
		// latest_tag advanced to v0.6.0 but the snooze is still pinned to v0.5.0.
		rows := []store.AppSetting{
			{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
			{Key: settings.KeyReleaseLatestTag, Value: "v0.6.0"},
			{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T11:00:00Z"},
			{Key: settings.KeyReleaseBannerSnoozeTag, Value: "v0.5.0"},
		}
		if get(t, releaseCheckHandler("0.4.0", nil, rows...)).BannerSnoozed {
			t.Error("banner_snoozed = true after a newer release, want false (auto-expiry)")
		}
	})
}

// TestReleaseCheckAdminGate: both endpoints are admin-gated by their route middleware —
// a non-admin gets 403, an admin passes through. The routes mount GetReleaseCheck behind
// RequireAdminRO and PostReleaseCheck behind RequireAdmin (see handler.Routes).
func TestReleaseCheckAdminGate(t *testing.T) {
	h := releaseCheckHandler("0.11.0", &fakeReleaseReconciler{},
		store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"})

	nonAdmin := store.User{IsAdmin: false, IsActive: true}
	admin := store.User{IsAdmin: true, IsActive: true}

	cases := []struct {
		name   string
		gated  http.Handler
		method string
	}{
		{"GET RequireAdminRO", mw.RequireAdminRO(http.HandlerFunc(h.GetReleaseCheck)), http.MethodGet},
		{"POST RequireAdmin", mw.RequireAdmin(http.HandlerFunc(h.PostReleaseCheck)), http.MethodPost},
		// PRD #836 M6: the snooze write is cookie-only RequireAdmin. With no latest_tag
		// seeded the admin path is a no-op 200 (nothing to snooze, no store touch), which
		// is exactly what proves the gate without needing a live DB.
		{"POST snooze RequireAdmin", mw.RequireAdmin(http.HandlerFunc(h.PostReleaseCheckSnooze)), http.MethodPost},
	}
	for _, tc := range cases {
		t.Run(tc.name+" forbids a non-admin", func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/admin/release-check", nil).
				WithContext(mw.ContextWithUser(context.Background(), nonAdmin))
			tc.gated.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("non-admin got %d, want 403", w.Code)
			}
		})
		t.Run(tc.name+" admits an admin", func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/admin/release-check", nil).
				WithContext(mw.ContextWithUser(context.Background(), admin))
			tc.gated.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("admin got %d, want 200 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}
