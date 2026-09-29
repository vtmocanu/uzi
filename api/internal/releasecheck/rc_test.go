package releasecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/settings"
)

func TestExactRCTag(t *testing.T) {
	for _, tc := range []struct {
		tag   string
		valid bool
	}{
		{"v1.2.3-rc.1", true},
		{"v1.2.3-rc.10", true},
		{"1.2.3-rc.1", false},
		{"v1.2.3-rc.01", false},
		{"v1.2.3-rc.1.extra", false},
		{"v1.2.3-rc.1+build", false},
		{"v1.2.3-beta.1", false},
		{"v1.2.3", false},
		{"v1.2.3-rc.x", false},
	} {
		if got := exactRCTag(tc.tag); got != tc.valid {
			t.Errorf("exactRCTag(%q) = %v, want %v", tc.tag, got, tc.valid)
		}
	}
}

func TestRCSelectionAndClear(t *testing.T) {
	list := `[` +
		releaseJSON("v1.2.0-rc.2", "second", "### Security\nfix", "2026-09-02T00:00:00Z", "https://example.test/2") + `,` +
		releaseJSON("v1.2.0-beta.9", "beta", "", "", "") + `,` +
		releaseJSON("v1.2.0-rc.10", "tenth", "notes", "2026-09-10T00:00:00Z", "https://example.test/10") + `,` +
		`{"tag_name":"v9.0.0-rc.1","draft":true},` +
		releaseJSON("v1.2.0-rc.01", "invalid", "", "", "") + `] `
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(list))
			return
		}
		_, _ = w.Write([]byte(releaseJSON("v1.1.0", "stable", "", "", "")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	st := newFakeStore(nil)
	set := &fakeSettings{enabled: true}
	rec := NewReconciler(st, set, nil, nil)
	got, _ := rec.CheckForUpdate(context.Background())
	if got.Status != statusOK || got.Facts.RCTag != "v1.2.0-rc.10" || st.values[settings.KeyReleaseRCTag] != "v1.2.0-rc.10" {
		t.Fatalf("selection = %+v values=%v", got, st.values)
	}
	list = "[]"
	got, _ = rec.CheckForUpdate(context.Background())
	if got.Status != statusOK || st.values[settings.KeyReleaseRCTag] != "" || st.values[settings.KeyReleaseRCBody] != "" {
		t.Fatalf("clear = %+v values=%v", got, st.values)
	}
}

func TestRCFetchFailureKeepsPriorRC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			http.Error(w, "failure", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(releaseJSON("v1.1.0", "stable", "", "", "")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	st := newFakeStore(map[string]string{settings.KeyReleaseRCTag: "v1.0.0-rc.1"})
	set := &fakeSettings{enabled: true}
	got, _ := NewReconciler(st, set, nil, nil).CheckForUpdate(context.Background())
	if got.Status != statusError || !strings.Contains(got.Message, "RC fetch failed") || st.values[settings.KeyReleaseLatestTag] != "v1.1.0" || st.values[settings.KeyReleaseRCTag] != "v1.0.0-rc.1" || set.invalidated.Load() != 1 {
		t.Fatalf("partial failure = %+v values=%v invalidated=%d", got, st.values, set.invalidated.Load())
	}
}
