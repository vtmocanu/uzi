package releasecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/settings"
)

func TestRCSelectionAndClear(t *testing.T) {
	list := `[` +
		releaseJSON("v1.2.0-rc.2", "second", "### Security\nfix", "2026-09-02T00:00:00Z", "https://example.test/2") + `,` +
		releaseJSON("v1.2.0-beta.9", "beta", "", "", "") + `,` +
		releaseJSON("v1.2.0-rc.10", "tenth", "notes", "", "https://example.test/10") + `,` +
		releaseJSON("v9.0.0-rc.0", "zero is not published", "", "", "") + `,` +
		`{"tag_name":"v9.0.0-rc.1","draft":true},` +
		releaseJSON("v1.2.0-rc.01", "invalid", "", "", "") + `] `
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			if r.URL.RawQuery != "per_page=20" {
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
	if got.Partial || got.Status != statusOK || got.Facts.RCTag != "v1.2.0-rc.10" || st.values[settings.KeyReleaseRCTag] != "v1.2.0-rc.10" {
		t.Fatalf("selection = %+v values=%v", got, st.values)
	}
	list = "[]"
	got, _ = rec.CheckForUpdate(context.Background())
	if got.Partial || got.Status != statusOK || st.values[settings.KeyReleaseRCTag] != "" || st.values[settings.KeyReleaseRCBody] != "" {
		t.Fatalf("clear = %+v values=%v", got, st.values)
	}
}

func TestRCListAcceptsResponseAboveLatestLimit(t *testing.T) {
	notes := strings.Repeat("n", maxReleaseBodyBytes+1)
	list := "[" + releaseJSON("v1.2.0-rc.2", "candidate", notes, "", "") + "]"
	if len(list) <= maxReleaseBodyBytes || len(list) >= 4<<20 {
		t.Fatalf("list fixture has %d bytes, want between 1 and 4 MiB", len(list))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(list))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	rel, err := fetchLatestRC(context.Background(), newHTTPClient(), "")
	if err != nil || rel.TagName != "v1.2.0-rc.2" || rel.Body != notes {
		t.Fatalf("large valid RC page = tag %q, body bytes %d, err %v", rel.TagName, len(rel.Body), err)
	}
}

func TestStableLatestKeepsOneMiBLimit(t *testing.T) {
	stable := releaseJSON("v1.2.0", "stable", strings.Repeat("n", maxReleaseBodyBytes+1), "", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(stable))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	_, err := fetchLatest(context.Background(), newHTTPClient(), "")
	if err == nil || !strings.Contains(err.Error(), "response exceeds 1 MiB") {
		t.Fatalf("large stable response error = %v, want 1 MiB bound", err)
	}
}

func TestRCFetchRejectsOversizeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			_, _ = w.Write([]byte(strings.Repeat("x", maxReleaseListBodyBytes+1)))
			return
		}
		_, _ = w.Write([]byte(releaseJSON("v1.1.0", "stable", "", "", "")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	st := newFakeStore(map[string]string{settings.KeyReleaseRCTag: "v1.0.0-rc.1"})
	set := &fakeSettings{enabled: true}
	got, _ := NewReconciler(st, set, nil, nil).CheckForUpdate(context.Background())
	if got.Status != statusError || !strings.Contains(got.Message, "response exceeds 4 MiB") ||
		st.values[settings.KeyReleaseLatestTag] != "v1.1.0" || st.values[settings.KeyReleaseRCTag] != "v1.0.0-rc.1" {
		t.Fatalf("oversize RC response = %+v values=%v", got, st.values)
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
	if !got.Partial || got.Facts.LatestTag != "v1.1.0" || got.Status != statusError || !strings.Contains(got.Message, "RC fetch failed") || st.values[settings.KeyReleaseLatestTag] != "v1.1.0" || st.values[settings.KeyReleaseRCTag] != "v1.0.0-rc.1" || set.invalidated.Load() != 1 {
		t.Fatalf("partial failure = %+v values=%v invalidated=%d", got, st.values, set.invalidated.Load())
	}
}

func TestRCWriteFailureKeepsFreshSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			_, _ = w.Write([]byte("[" + releaseJSON("v1.2.0-rc.2", "new RC", "### Security\nnew", "", "https://example.test/new") + "]"))
			return
		}
		_, _ = w.Write([]byte(releaseJSON("v1.1.0", "new stable", "new body", "", "")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	prior := map[string]string{
		settings.KeyReleaseLatestTag: "v1.0.0", settings.KeyReleaseLatestBody: "old body",
		settings.KeyReleaseRCTag: "v1.1.0-rc.1", settings.KeyReleaseRCBody: "old RC body",
		settings.KeyReleaseCheckedAt: "2026-09-01T00:00:00Z",
	}
	st := newFakeStore(prior)
	st.failKey = settings.KeyReleaseRCBody
	set := &fakeSettings{enabled: true}
	got, _ := NewReconciler(st, set, nil, nil).CheckForUpdate(context.Background())
	if got.Partial || got.Status != statusError || set.invalidated.Load() != 0 {
		t.Fatalf("failed write = %+v invalidated=%d", got, set.invalidated.Load())
	}
	// A new cache would read the database map, rather than the reconciler's old cache.
	for key, want := range prior {
		if st.values[key] != want {
			t.Errorf("fresh read %s = %q, want %q", key, st.values[key], want)
		}
	}
	if len(st.values) != len(prior) {
		t.Errorf("fresh read has extra keys: %v", st.values)
	}
}

func TestNullResponseIsFetchFailure(t *testing.T) {
	for _, nullPath := range []string{"/repos/vtmocanu/uzi/releases/latest", "/repos/vtmocanu/uzi/releases"} {
		t.Run(nullPath, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == nullPath {
					_, _ = w.Write([]byte(" null \n"))
				} else {
					_, _ = w.Write([]byte(releaseJSON("v1.1.0", "stable", "new", "", "")))
				}
			}))
			defer srv.Close()
			withBaseURL(t, srv.URL)
			st := newFakeStore(map[string]string{settings.KeyReleaseLatestTag: "v1.0.0", settings.KeyReleaseRCTag: "v1.1.0-rc.1"})
			set := &fakeSettings{enabled: true}
			got, _ := NewReconciler(st, set, nil, nil).CheckForUpdate(context.Background())
			if got.Status != statusError || st.values[settings.KeyReleaseRCTag] != "v1.1.0-rc.1" {
				t.Fatalf("null response = %+v values=%v", got, st.values)
			}
			if nullPath == "/repos/vtmocanu/uzi/releases/latest" && (st.values[settings.KeyReleaseLatestTag] != "v1.0.0" || set.invalidated.Load() != 0) {
				t.Fatalf("stable null wrote facts: %v", st.values)
			}
			if nullPath == "/repos/vtmocanu/uzi/releases" && (st.values[settings.KeyReleaseLatestTag] != "v1.1.0" || set.invalidated.Load() != 1) {
				t.Fatalf("RC null did not retain stable update: %v", st.values)
			}
		})
	}
}

func TestRCFetchAndPersistFailureIsNotPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			http.Error(w, "failure", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(releaseJSON("v1.1.0", "stable", "new body", "", "")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	prior := map[string]string{
		settings.KeyReleaseLatestTag: "v1.0.0",
		settings.KeyReleaseRCTag:     "v1.0.0-rc.1",
		settings.KeyReleaseCheckedAt: "2026-09-01T00:00:00Z",
	}
	st := newFakeStore(prior)
	st.failKey = settings.KeyReleaseLatestBody
	set := &fakeSettings{enabled: true}
	got, err := NewReconciler(st, set, nil, nil).CheckForUpdate(context.Background())
	if err != nil || got.Partial || got.Status != statusError || !strings.Contains(got.Message, "persist remote facts failed") || set.invalidated.Load() != 0 {
		t.Fatalf("RC and persistence failure = %+v err=%v invalidated=%d", got, err, set.invalidated.Load())
	}
	for key, want := range prior {
		if st.values[key] != want {
			t.Errorf("cached fact %s = %q, want %q", key, st.values[key], want)
		}
	}
	if len(st.values) != len(prior) {
		t.Errorf("failed persistence added facts: %v", st.values)
	}
}
