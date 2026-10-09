package releasecheck

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The same store supplies cache snapshots and receives the reconciler's atomic
// writes. Tests inspect it only once the Runner requests its next blocked wait.
func (s *fakeStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	rows := make([]store.AppSetting, 0, len(s.values))
	for k, v := range s.values {
		rows = append(rows, store.AppSetting{Key: k, Value: v})
	}
	return rows, nil
}

func TestScheduledReconcilerOutcomes(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		enabled            bool
		enableErr          error
		stableFail, rcFail bool
		requests           int64
		persisted          bool
	}{
		{name: "disabled"},
		{name: "enable read error", enabled: true, enableErr: errors.New("read failed")},
		{name: "stable failure", enabled: true, stableFail: true, requests: 1},
		{name: "success", enabled: true, requests: 2, persisted: true},
		{name: "RC failure", enabled: true, rcFail: true, requests: 2, persisted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if (tc.stableFail && r.URL.Path == "/repos/vtmocanu/uzi/releases/latest") ||
					(tc.rcFail && r.URL.Path == "/repos/vtmocanu/uzi/releases") {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
					_, _ = w.Write([]byte("[" + releaseJSON("v2.1.0-rc.1", "candidate", "RC notes", "", "") + "]"))
					return
				}
				_, _ = w.Write([]byte(releaseJSON("v2.0.0", "stable", "stable notes", "", "")))
			}))
			defer srv.Close()
			withBaseURL(t, srv.URL)
			oldStamp := now.Add(-7 * time.Hour).Format(time.RFC3339)
			st := newFakeStore(map[string]string{
				settings.KeyReleaseCheckedAt: oldStamp,
				settings.KeyReleaseLatestTag: "v1.0.0", settings.KeyReleaseRCTag: "v1.1.0-rc.1",
			})
			set := &fakeSettings{enabled: tc.enabled, enabledErr: tc.enableErr, interval: 6 * time.Hour, checkedAt: oldStamp}
			rec := NewReconciler(st, set, func() time.Time { return now }, quietLogger())
			rn := NewRunner(rec, set, quietLogger())
			w := newManualWait()
			rn.now = func() time.Time { return now }
			rn.wait = w.wait
			stop := startRunner(t, rn)
			first := w.next(t)
			if first.delay != time.Minute {
				t.Fatalf("first wait=%v, want 1m", first.delay)
			}
			close(first.release)
			if got := w.next(t).delay; got != 6*time.Hour {
				t.Fatalf("second wait=%v, want 6h", got)
			}
			stop()
			if requests.Load() != tc.requests {
				t.Fatalf("HTTP requests=%d, want %d", requests.Load(), tc.requests)
			}
			if tc.persisted {
				if st.values[settings.KeyReleaseCheckedAt] != now.Format(time.RFC3339) ||
					st.values[settings.KeyReleaseLatestTag] != "v2.0.0" || set.invalidated.Load() != 1 {
					t.Fatalf("persisted values=%v invalidations=%d", st.values, set.invalidated.Load())
				}
				wantRC := "v2.1.0-rc.1"
				if tc.rcFail {
					wantRC = "v1.1.0-rc.1"
				}
				if st.values[settings.KeyReleaseRCTag] != wantRC {
					t.Fatalf("RC=%q, want %q", st.values[settings.KeyReleaseRCTag], wantRC)
				}
			} else if st.writes != 0 || st.values[settings.KeyReleaseCheckedAt] != oldStamp ||
				st.values[settings.KeyReleaseLatestTag] != "v1.0.0" || set.invalidated.Load() != 0 {
				t.Fatalf("unexpected write: values=%v writes=%d invalidations=%d", st.values, st.writes, set.invalidated.Load())
			}
		})
	}
}

func TestScheduledCheckRefreshesRealCache(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/repos/vtmocanu/uzi/releases" {
			_, _ = w.Write([]byte("[" + releaseJSON("v2.1.0-rc.2", "new candidate", "new RC notes", "", "") + "]"))
			return
		}
		_, _ = w.Write([]byte(releaseJSON("v2.0.0", "new stable", "new notes", "", "")))
	}))
	defer srv.Close()
	withBaseURL(t, srv.URL)
	oldStamp := now.Add(-7 * time.Hour).Format(time.RFC3339)
	st := newFakeStore(map[string]string{
		settings.KeyReleaseCheckEnabled: "true", settings.KeyReleaseCheckInterval: "6h",
		settings.KeyReleaseCheckedAt: oldStamp, settings.KeyReleaseLatestTag: "v1.0.0",
		settings.KeyReleaseRCTag: "v1.1.0-rc.1",
	})
	cache := settings.New(st, time.Hour)
	old, err := cache.ReleaseStatus(context.Background())
	if err != nil || old.LatestTag != "v1.0.0" || old.RCTag != "v1.1.0-rc.1" || old.CheckedAt != oldStamp {
		t.Fatalf("warm snapshot=%+v, %v", old, err)
	}
	rec := NewReconciler(st, cache, func() time.Time { return now }, quietLogger())
	rn := NewRunner(rec, cache, quietLogger())
	if rn.bootDelay != time.Minute || rn.now == nil || rn.wait == nil {
		t.Fatal("constructor scheduling defaults missing")
	}
	w := newManualWait()
	rn.wait = w.wait
	rn.now = func() time.Time { return now }
	stop := startRunner(t, rn)
	first := w.next(t)
	if first.delay != time.Minute {
		t.Fatalf("first wait=%v, want 1m", first.delay)
	}
	close(first.release)
	if got := w.next(t).delay; got != 6*time.Hour {
		t.Fatalf("second wait=%v, want 6h", got)
	}
	stop()
	got, err := cache.ReleaseStatus(context.Background())
	if err != nil || got.LatestTag != "v2.0.0" || got.LatestName != "new stable" ||
		got.RCTag != "v2.1.0-rc.2" || got.RCName != "new candidate" ||
		got.CheckedAt != now.Format(time.RFC3339) {
		t.Fatalf("post-check cache snapshot=%+v, %v", got, err)
	}
	stamp, err := cache.ReleaseCheckedAt(context.Background())
	if err != nil || stamp != now.Format(time.RFC3339) {
		t.Fatalf("post-check checked-at=%q, %v", stamp, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("HTTP requests=%d, want 2", requests.Load())
	}
}
