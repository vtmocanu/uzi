package releasecheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeSettings is an in-memory SettingsReader: a fixed enabled flag + optional token,
// counting Invalidate calls so a persist is observable.
type fakeSettings struct {
	enabled     bool
	enabledErr  error
	token       string
	tokenErr    error
	interval    time.Duration
	invalidated atomic.Int64
}

func (f *fakeSettings) ReleaseCheckEnabled(context.Context) (bool, error) {
	// Mirror the real accessor's default-ON-on-error behavior so the fail-closed test
	// exercises the same trap: enabled=true is returned ALONGSIDE the error.
	return f.enabled, f.enabledErr
}
func (f *fakeSettings) ReleaseCheckToken(context.Context) (string, error) {
	return f.token, f.tokenErr
}
func (f *fakeSettings) ReleaseCheckInterval(context.Context) (time.Duration, error) {
	return f.interval, nil
}
func (f *fakeSettings) Invalidate() { f.invalidated.Add(1) }

// fakeStore is an in-memory Store recording every UpsertAppSetting, seeded with any
// prior facts so a test can assert last-good survives an error pass.
type fakeStore struct {
	values map[string]string
	writes int
}

func newFakeStore(seed map[string]string) *fakeStore {
	m := map[string]string{}
	for k, v := range seed {
		m[k] = v
	}
	return &fakeStore{values: m}
}

func (s *fakeStore) UpsertAppSetting(_ context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error) {
	s.writes++
	s.values[arg.Key] = arg.Value
	return store.AppSetting{Key: arg.Key, Value: arg.Value}, nil
}

// releaseJSON is a minimal releases/latest payload for the stub server.
func releaseJSON(tag, name, body, published, htmlURL string) string {
	return fmt.Sprintf(`{"tag_name":%q,"name":%q,"body":%q,"published_at":%q,"html_url":%q}`,
		tag, name, body, published, htmlURL)
}

// withBaseURL points the package fetch at u for the duration of the test.
func withBaseURL(t *testing.T, u string) {
	t.Helper()
	prev := baseURL
	baseURL = u
	t.Cleanup(func() { baseURL = prev })
}

// TestCheckForUpdateSuccess: a newer upstream release → status "ok", the six facts
// persisted, the cache invalidated, and the parsed facts derive update_available=true.
func TestCheckForUpdateSuccess(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unauthenticated path sent Authorization = %q, want empty", got)
		}
		_, _ = w.Write([]byte(releaseJSON("v0.15.0", "v0.15.0", "### Security\n- fix",
			"2026-08-20T10:00:00Z", "https://github.com/vtmocanu/uzi/releases/tag/v0.15.0")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)

	st := newFakeStore(nil)
	set := &fakeSettings{enabled: true}
	rec := NewReconciler(st, set, nil, nil)

	res, err := rec.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate err = %v", err)
	}
	if res.Status != statusOK {
		t.Fatalf("Status = %q, want %q (msg=%q)", res.Status, statusOK, res.Message)
	}
	if reqCount.Load() != 1 {
		t.Errorf("server saw %d requests, want 1", reqCount.Load())
	}
	if res.Facts.LatestTag != "v0.15.0" {
		t.Errorf("Facts.LatestTag = %q, want v0.15.0", res.Facts.LatestTag)
	}
	if !UpdateAvailable("0.14.0", res.Facts.LatestTag) {
		t.Error("expected update_available=true from the returned facts")
	}
	if !Security(res.Facts.Body) {
		t.Error("expected security=true from the returned body")
	}
	// All six facts persisted.
	for _, k := range []string{
		settings.KeyReleaseLatestTag, settings.KeyReleaseLatestName, settings.KeyReleaseLatestBody,
		settings.KeyReleaseNotesURL, settings.KeyReleasePublishedAt, settings.KeyReleaseCheckedAt,
	} {
		if _, ok := st.values[k]; !ok {
			t.Errorf("fact %q was not persisted", k)
		}
	}
	if st.values[settings.KeyReleaseCheckedAt] == "" {
		t.Error("checked_at not stamped from the clock")
	}
	if set.invalidated.Load() != 1 {
		t.Errorf("Invalidate called %d times, want 1", set.invalidated.Load())
	}
}

// TestCheckForUpdateEqualNotAvailable: equal versions → update_available=false, the
// FALSE state (distinct from the unchecked/disabled state), still Status "ok".
func TestCheckForUpdateEqualNotAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseJSON("v0.14.0", "v0.14.0", "### Added\n- x",
			"2026-08-20T10:00:00Z", "https://example.test/r")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)

	rec := NewReconciler(newFakeStore(nil), &fakeSettings{enabled: true}, nil, nil)
	res, err := rec.CheckForUpdate(context.Background())
	if err != nil || res.Status != statusOK {
		t.Fatalf("CheckForUpdate = %q, %v; want ok", res.Status, err)
	}
	if UpdateAvailable("0.14.0", res.Facts.LatestTag) {
		t.Error("equal versions must derive update_available=false")
	}
}

// TestCheckForUpdateDisabled: master toggle OFF → Status "disabled", the server sees
// ZERO requests, nothing persisted, cache not invalidated.
func TestCheckForUpdateDisabled(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount.Add(1)
		_, _ = w.Write([]byte(releaseJSON("v9.9.9", "x", "", "2026-08-20T10:00:00Z", "https://x")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)

	st := newFakeStore(nil)
	set := &fakeSettings{enabled: false}
	rec := NewReconciler(st, set, nil, nil)

	res, err := rec.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate err = %v", err)
	}
	if res.Status != statusDisabled {
		t.Fatalf("Status = %q, want %q", res.Status, statusDisabled)
	}
	if reqCount.Load() != 0 {
		t.Errorf("disabled check made %d http requests, want 0", reqCount.Load())
	}
	if st.writes != 0 || len(st.values) != 0 {
		t.Errorf("disabled check persisted %d writes, want 0", st.writes)
	}
	if set.invalidated.Load() != 0 {
		t.Errorf("disabled check invalidated the cache %d times, want 0", set.invalidated.Load())
	}
}

// TestCheckForUpdateEnableReadErrorFailsClosed: an error reading the master toggle →
// Status "disabled", the server sees ZERO requests, nothing persisted, cache not
// invalidated. The master gate is the air-gap/privacy gate (PRD #836 D2), so a
// transient cache-read error must never cause egress — even though the accessor
// defaults to enabled=true on that error.
func TestCheckForUpdateEnableReadErrorFailsClosed(t *testing.T) {
	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount.Add(1)
		_, _ = w.Write([]byte(releaseJSON("v9.9.9", "x", "", "2026-08-20T10:00:00Z", "https://x")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)

	st := newFakeStore(nil)
	// enabled=true PLUS a read error: the accessor's default-ON behavior must NOT let
	// the check proceed to egress.
	set := &fakeSettings{enabled: true, enabledErr: fmt.Errorf("cache read failed")}
	rec := NewReconciler(st, set, nil, nil)

	res, err := rec.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate err = %v", err)
	}
	if res.Status != statusDisabled {
		t.Fatalf("Status = %q, want %q (an errored enable read must fail closed)", res.Status, statusDisabled)
	}
	if reqCount.Load() != 0 {
		t.Errorf("enable-read-error check made %d http requests, want 0", reqCount.Load())
	}
	if st.writes != 0 || len(st.values) != 0 {
		t.Errorf("enable-read-error check persisted %d writes, want 0", st.writes)
	}
	if set.invalidated.Load() != 0 {
		t.Errorf("enable-read-error check invalidated the cache %d times, want 0", set.invalidated.Load())
	}
}

// TestCheckForUpdateErrorPreservesLastGood: upstream returns HTTP 500 → Status
// "error", NOTHING persisted, and the previously-persisted (last-good) facts survive.
func TestCheckForUpdateErrorPreservesLastGood(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)

	lastGood := map[string]string{
		settings.KeyReleaseLatestTag: "v0.13.0",
		settings.KeyReleaseNotesURL:  "https://example.test/prev",
	}
	st := newFakeStore(lastGood)
	set := &fakeSettings{enabled: true}
	rec := NewReconciler(st, set, nil, nil)

	res, err := rec.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate err = %v", err)
	}
	if res.Status != statusError {
		t.Fatalf("Status = %q, want %q", res.Status, statusError)
	}
	if st.writes != 0 {
		t.Errorf("error pass persisted %d writes, want 0 (last-good must be preserved)", st.writes)
	}
	if st.values[settings.KeyReleaseLatestTag] != "v0.13.0" {
		t.Errorf("last-good tag = %q, want v0.13.0 (must survive an error)", st.values[settings.KeyReleaseLatestTag])
	}
	if set.invalidated.Load() != 0 {
		t.Errorf("error pass invalidated the cache %d times, want 0", set.invalidated.Load())
	}
}

// TestScrubToken covers scrubToken directly: a token embedded in a non-nil error is
// replaced with REDACTED, a nil error stays nil, and an empty token returns the error
// unchanged.
func TestScrubToken(t *testing.T) {
	const token = "ghp_secret_token_xyz789"

	// (a) A non-nil error whose message contains the token has it redacted.
	in := fmt.Errorf("dial failed for %s: connection refused", token)
	out := scrubToken(in, token)
	if out == nil {
		t.Fatalf("scrubToken returned nil for a non-nil error")
	}
	if strings.Contains(out.Error(), token) {
		t.Errorf("scrubbed error still contains the token: %q", out.Error())
	}
	if !strings.Contains(out.Error(), "REDACTED") {
		t.Errorf("scrubbed error missing REDACTED marker: %q", out.Error())
	}

	// (b) A nil error in yields nil out.
	if got := scrubToken(nil, token); got != nil {
		t.Errorf("scrubToken(nil, token) = %v, want nil", got)
	}

	// (c) An empty token returns the original error value unchanged.
	orig := fmt.Errorf("dial failed for %s: connection refused", token)
	if got := scrubToken(orig, ""); got != orig {
		t.Errorf("scrubToken(err, \"\") = %v, want the original error value unchanged", got)
	}
}

// TestCheckForUpdateSendsBearerToken: a configured token → Authorization: Bearer
// <token> is sent (verifies the Authorization Bearer header only; token scrubbing is
// covered by TestScrubToken).
func TestCheckForUpdateSendsBearerToken(t *testing.T) {
	const token = "ghp_release_check_token_abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(releaseJSON("v0.15.0", "x", "", "2026-08-20T10:00:00Z", "https://x")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)

	rec := NewReconciler(newFakeStore(nil), &fakeSettings{enabled: true, token: token}, nil, nil)
	res, err := rec.CheckForUpdate(context.Background())
	if err != nil || res.Status != statusOK {
		t.Fatalf("CheckForUpdate = %q, %v; want ok", res.Status, err)
	}
}

// TestCheckForUpdateUnreachable: an unreachable endpoint → Status "error", nothing
// persisted (a connection failure, not an HTTP status).
func TestCheckForUpdateUnreachable(t *testing.T) {
	// A closed server's URL is unreachable.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	withBaseURL(t, url)

	st := newFakeStore(nil)
	rec := NewReconciler(st, &fakeSettings{enabled: true}, nil, nil)
	res, err := rec.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate err = %v", err)
	}
	if res.Status != statusError {
		t.Fatalf("Status = %q, want %q", res.Status, statusError)
	}
	if st.writes != 0 {
		t.Errorf("unreachable pass persisted %d writes, want 0", st.writes)
	}
}
