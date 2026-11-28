package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analyticsprecompute"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4771: the background scheduler against a real database. These live in the handler
// package because the end-to-end test needs the handler's builder and router, and the package
// already carries the LiveDB harness.

func apSchedPublications(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT publications FROM analytics_precomputed_state WHERE id`).Scan(&n); err != nil {
		t.Fatalf("read publications: %v", err)
	}
	return n
}

func apSchedResetState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	cliMustExec(t, pool, `DELETE FROM analytics_precomputed_docs`)
	cliMustExec(t, pool, `UPDATE analytics_precomputed_state SET failure_count = 0, last_failure_at = NULL WHERE id`)
}

// Two schedulers on one database share the failure backoff and the stored generation: A's recorded
// failure keeps B from building, and once A publishes, B sees a fresh set and does not build.
// Mutation caught: a scheduler that keeps its backoff only in memory (B would build at once), or one
// that decides freshness without reading the stored set (B would rebuild after A's publish).
func TestAnalyticsSchedulerSharedStateLiveDB(t *testing.T) {
	f := newAnFixture(t, apYear)
	apSchedResetState(t, f.pool)
	t.Cleanup(func() { apSchedResetState(t, f.pool) })

	const section = "ap-sched-shared"
	keys := []string{"k1", "k2"}
	base := time.Now().UTC().Truncate(time.Microsecond)
	var aClock, bClock time.Time
	aClock, bClock = base, base.Add(time.Minute)

	aFails := true
	aCalls, bCalls := 0, 0
	docs := []store.AnalyticsPrecomputedDoc{{Key: "k1", Document: []byte(`{"k":1}`)}, {Key: "k2", Document: []byte(`{"k":2}`)}}
	a := analyticsprecompute.NewWithClock(f.pool, 4*time.Hour, []analyticsprecompute.Section{{Name: section, Keys: keys,
		Build: func(context.Context, *store.AnalyticsSnapshot, time.Time) ([]store.AnalyticsPrecomputedDoc, error) {
			aCalls++
			if aFails {
				return nil, errors.New("a: injected build failure")
			}
			return docs, nil
		}}}, func() time.Time { return aClock })
	b := analyticsprecompute.NewWithClock(f.pool, 4*time.Hour, []analyticsprecompute.Section{{Name: section, Keys: keys,
		Build: func(context.Context, *store.AnalyticsSnapshot, time.Time) ([]store.AnalyticsPrecomputedDoc, error) {
			bCalls++
			return docs, nil
		}}}, func() time.Time { return bClock })
	run := func(s *analyticsprecompute.Scheduler) {
		t.Helper()
		if n, err := s.Pass(context.Background()); err != nil || n != 0 {
			t.Fatalf("Pass = (%d, %v), want (0, nil): a build start is not a row", n, err)
		}
		s.Wait()
	}
	status := func() store.AnalyticsPrecomputeState {
		t.Helper()
		st, err := store.ReadAnalyticsPrecomputeStatus(context.Background(), f.pool, section, keys)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	run(a)
	if st := status(); aCalls != 1 || st.FailureCount != 1 || !st.LastFailureAt.Equal(base) || st.Present != 0 {
		t.Fatalf("after A's failure: calls %d status %+v, want one recorded failure at %v and nothing stored", aCalls, st, base)
	}

	run(b)
	if bCalls != 0 {
		t.Fatal("B built inside A's recorded backoff")
	}

	aClock, aFails = base.Add(5*time.Minute), false
	run(a)
	st := status()
	if aCalls != 2 || st.Present != 2 || st.FailureCount != 0 || !st.OldestGeneratedAt.Equal(aClock) {
		t.Fatalf("after A's success: calls %d status %+v", aCalls, st)
	}

	bClock = base.Add(time.Hour)
	run(b)
	if bCalls != 0 {
		t.Fatal("B rebuilt a set that A published within the interval")
	}
}

// The stored set follows a delete and the setting end to end, with the handler's own builder:
// build and serve; a committed user delete empties the set (pending, never the old generation);
// the setting off answers the old 400 and writes nothing; re-enabled it stays pending until the
// scheduler builds again.
// The deleted user has no dependents, so the plain DELETE succeeds and fires the trigger.
// Mutation caught: serving that ignores an emptied set (the old generation would be served after
// the delete), or a request at setting 0 that publishes.
func TestAnalyticsSchedulerEndToEndLiveDB(t *testing.T) {
	f := newAnFixture(t, apYear)
	apSeed(f)
	apSchedResetState(t, f.pool)
	t.Cleanup(func() { apSchedResetState(t, f.pool) })
	f.h.cfg.AnalyticsSnapshotInterval = 4 * time.Hour
	admin := f.user(true)
	const path = "/api/admin/analytics/spend?window=all"

	sched := analyticsprecompute.NewWithClock(f.pool, 4*time.Hour, []analyticsprecompute.Section{{
		Name: AllTimeSpendSection, Keys: AllTimeSpendKeys(), Build: BuildAllTimeSpendDocs,
	}}, func() time.Time { return f.now })
	build := func() {
		t.Helper()
		if n, err := sched.Pass(context.Background()); err != nil || n != 0 {
			t.Fatalf("Pass = (%d, %v), want (0, nil): a build start is not a row", n, err)
		}
		sched.Wait()
	}

	if rec := f.cookie(admin, path); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before the first build: %d %s", rec.Code, rec.Body.String())
	}
	build()
	rec := f.cookie(admin, path)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"snapshot":`) {
		t.Fatalf("after the build: %d %.200s", rec.Code, rec.Body.String())
	}
	st, err := store.ReadAnalyticsPrecomputeStatus(context.Background(), f.pool, AllTimeSpendSection, AllTimeSpendKeys())
	if err != nil || st.Present != int64(len(AllTimeSpendKeys())) || !st.OldestGeneratedAt.Equal(f.now) {
		t.Fatalf("stored status = %+v (%v), want every variant generated at %v", st, err, f.now)
	}

	// A committed user delete empties the set.
	doomed := f.user(false)
	publications := apSchedPublications(t, f.pool)
	build() // the set is fresh: no build, no publish
	if got := apSchedPublications(t, f.pool); got != publications {
		t.Fatalf("a fresh set was published again: publications %v -> %v", publications, got)
	}
	cliMustExec(t, f.pool, `DELETE FROM users WHERE id = $1`, doomed)
	if rec := f.cookie(admin, path); rec.Code != http.StatusServiceUnavailable || rec.Result().Header.Get("Retry-After") != "60" {
		t.Fatalf("after the delete: %d %v %s, want pending", rec.Code, rec.Result().Header, rec.Body.String())
	}

	// Setting off: the old 400, and a request writes nothing. No scheduler exists at 0.
	f.h.cfg.AnalyticsSnapshotInterval = 0
	if rec := f.cookie(admin, path); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgAnalyticsWindowAllAllUsers) {
		t.Fatalf("setting off: %d %s", rec.Code, rec.Body.String())
	}
	if got := apSchedPublications(t, f.pool); got != publications {
		t.Fatalf("publications moved from %d to %d across a request with the setting off", publications, got)
	}

	// Re-enabled: pending until the scheduler builds, never the old generation.
	f.h.cfg.AnalyticsSnapshotInterval = 4 * time.Hour
	if rec := f.cookie(admin, path); rec.Code != http.StatusServiceUnavailable || rec.Result().Header.Get("Retry-After") != "60" {
		t.Fatalf("re-enabled: %d %s, want pending", rec.Code, rec.Body.String())
	}
	build()
	if rec := f.cookie(admin, path); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"snapshot":`) {
		t.Fatalf("after the rebuild: %d %.200s", rec.Code, rec.Body.String())
	}
	if got := apSchedPublications(t, f.pool); got != publications+1 {
		t.Fatalf("publications = %d, want %d", got, publications+1)
	}
}
