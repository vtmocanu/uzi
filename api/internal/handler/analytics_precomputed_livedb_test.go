package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4771: stored all-time Spend documents against a real database. Calendar year 2060 is
// this file's own, as the fixture's header explains.

const apYear = 2060

func apSeed(f *anFixture) {
	f.t.Helper()
	u1, u2 := f.user(false), f.user(false)
	r1, _ := f.repoOwnedBy(u1, "one")
	r2, _ := f.repoOwnedBy(u2, "two")
	f.run(u1, r1, f.now.AddDate(0, 0, -40), "1.25")
	f.run(u1, r1, f.now.AddDate(0, 0, -20), "2.50")
	f.run(u2, r2, f.now.AddDate(0, 0, -10), "4.00")
}

// apLive computes the live Spend body of one variant on snapshot s at now, exactly as the stored
// document is defined: the JSON the live handler would write for that request.
func apLive(t *testing.T, ctx context.Context, s *store.AnalyticsSnapshot, split, breakdown string, now time.Time) []byte {
	t.Helper()
	req, msg := parseAnalyticsRequest(url.Values{"window": {"all"}, "split": {split}, "breakdown": {breakdown}}, true, analytics.SectionSpend, uuid.Nil, true)
	if msg != "" {
		t.Fatalf("parse %s/%s: %s", split, breakdown, msg)
	}
	dto, err := (&analyticsDeps{tzLoader: time.LoadLocation}).spend(ctx, s, req, now)
	if err != nil {
		t.Fatalf("live spend %s/%s: %v", split, breakdown, err)
	}
	rec := httptest.NewRecorder()
	httpx.JSON(rec, http.StatusOK, dto)
	return rec.Body.Bytes()
}

// apSplitBreakdown splits a variant key back into its parameters.
func apSplitBreakdown(key string) (split, breakdown string) {
	v, _ := url.ParseQuery(key)
	return v.Get("split"), v.Get("breakdown")
}

func apOpenSession(t *testing.T, f *anFixture) *store.AnalyticsPrecomputeSession {
	t.Helper()
	s, ok, err := store.OpenAnalyticsPrecomputeSession(context.Background(), f.pool)
	if err != nil || !ok {
		t.Fatalf("open precompute session = (%v, %v)", ok, err)
	}
	t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	return s
}

func apCloseOK(t *testing.T, s *store.AnalyticsPrecomputeSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if unlocked, err := s.Close(ctx); err != nil || !unlocked {
		t.Fatalf("Close = (%v, %v)", unlocked, err)
	}
}

// apBuild builds the full generation on one RunBuild call and returns it with its counter.
func apBuild(t *testing.T, s *store.AnalyticsPrecomputeSession, now time.Time) ([]store.AnalyticsPrecomputedDoc, int64) {
	t.Helper()
	var docs []store.AnalyticsPrecomputedDoc
	var counter int64
	err := s.RunBuild(context.Background(), func(ctx context.Context, snap *store.AnalyticsSnapshot, c int64) error {
		counter = c
		var berr error
		docs, berr = BuildAllTimeSpendDocs(ctx, snap, now)
		return berr
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return docs, counter
}

func apSetHooks(t *testing.T, before, after func(i int, key string) error) {
	t.Helper()
	allTimeSpendBeforeVariant, allTimeSpendAfterVariant = before, after
	t.Cleanup(func() { allTimeSpendBeforeVariant, allTimeSpendAfterVariant = nil, nil })
}

// One transaction per generation: a write that commits after variant 1 must change none of the
// 25 documents, and every document equals the live computation on the same snapshot handle.
// Mutation caught: RunBuild's IsoLevel RepeatableRead changed to ReadCommitted (store
// analytics_precomputed.go), after which variant 25 (split=user x breakdown=user) would include
// the new user and differ from the pre-write bytes.
func TestAllTimeSpendBuildOneSnapshotLiveDB(t *testing.T) {
	f := newAnFixture(t, apYear)
	apSeed(f)
	keys := AllTimeSpendKeys()

	pre := make([][]byte, len(keys))
	for i, k := range keys {
		split, breakdown := apSplitBreakdown(k)
		err := store.RunAnalyticsSnapshot(context.Background(), f.pool, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
			pre[i] = apLive(t, ctx, s, split, breakdown, f.now)
			return nil
		})
		if err != nil {
			t.Fatalf("pre-write snapshot %s: %v", k, err)
		}
	}

	wrote := false
	apSetHooks(t, nil, func(i int, _ string) error {
		if i != 1 {
			return nil
		}
		// A new user and repo with a costed run commit on a separate pool connection mid-build.
		nu := f.user(false)
		nr, _ := f.repoOwnedBy(nu, "late")
		f.run(nu, nr, f.now.AddDate(0, 0, -2), "9.00")
		wrote = true
		return nil
	})

	sess := apOpenSession(t, f)
	var docs []store.AnalyticsPrecomputedDoc
	err := sess.RunBuild(context.Background(), func(ctx context.Context, snap *store.AnalyticsSnapshot, _ int64) error {
		var berr error
		docs, berr = BuildAllTimeSpendDocs(ctx, snap, f.now)
		if berr != nil {
			return berr
		}
		for i, k := range keys {
			split, breakdown := apSplitBreakdown(k)
			if live := apLive(t, ctx, snap, split, breakdown, f.now); !bytes.Equal(live, append(slices.Clone(docs[i].Document), '\n')) {
				t.Errorf("variant %d %s: document differs from the live computation on the same snapshot", i+1, k)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunBuild: %v", err)
	}
	apCloseOK(t, sess)
	if !wrote {
		t.Fatal("the mid-build write never ran")
	}
	if len(docs) != len(keys) {
		t.Fatalf("%d docs, want %d", len(docs), len(keys))
	}
	for i, d := range docs {
		if d.Key != keys[i] {
			t.Fatalf("doc %d key %q, want %q", i, d.Key, keys[i])
		}
		if !bytes.Equal(append(slices.Clone(d.Document), '\n'), pre[i]) {
			t.Fatalf("variant %d %s: built document differs from the pre-write bytes", i+1, d.Key)
		}
	}

	// The write is visible to a fresh snapshot, so the equality above is not vacuous.
	last := len(keys) - 1
	split, breakdown := apSplitBreakdown(keys[last])
	err = store.RunAnalyticsSnapshot(context.Background(), f.pool, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
		if bytes.Equal(apLive(t, ctx, s, split, breakdown, f.now), pre[last]) {
			t.Error("a fresh snapshot after the write equals the pre-write bytes: the write changed nothing")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A failure at variant 13 returns no documents and an error naming that variant.
func TestAllTimeSpendBuildFailureReturnsNoDocsLiveDB(t *testing.T) {
	f := newAnFixture(t, apYear)
	apSeed(f)
	ctx := context.Background()
	sess := apOpenSession(t, f)

	boom := errors.New("injected failure")
	apSetHooks(t, func(i int, _ string) error {
		if i == 13 {
			return boom
		}
		return nil
	}, nil)
	var docs []store.AnalyticsPrecomputedDoc
	var berr error
	rerr := sess.RunBuild(ctx, func(ctx context.Context, snap *store.AnalyticsSnapshot, _ int64) error {
		docs, berr = BuildAllTimeSpendDocs(ctx, snap, f.now)
		return berr
	})
	if !errors.Is(rerr, boom) || !errors.Is(berr, boom) {
		t.Fatalf("RunBuild err = %v, build err = %v, want the injected failure", rerr, berr)
	}
	if docs != nil {
		t.Fatalf("a failed build returned %d documents", len(docs))
	}
	if want := AllTimeSpendKeys()[12]; !strings.Contains(berr.Error(), want) {
		t.Fatalf("error %q does not name variant 13 %q", berr, want)
	}
	apCloseOK(t, sess)
}

// The router serves each variant's stored document: minus the snapshot member it equals the live
// body at the same instant, its dimensions match the request, and one generation shares one
// generated_at.
// Mutation: serving that reads a fixed key returns variant 1 for every request.
func TestAllTimeSpendServedFromStoredSetLiveDB(t *testing.T) {
	f := newAnFixture(t, apYear)
	apSeed(f)
	f.h.cfg.AnalyticsSnapshotInterval = 4 * time.Hour
	admin := f.user(true)
	ctx := context.Background()
	keys := AllTimeSpendKeys()

	// Before any publish the answer is pending.
	cliMustExec(t, f.pool, `DELETE FROM analytics_precomputed_docs`)
	rec := f.cookie(admin, "/api/admin/analytics/spend?window=all")
	if rec.Code != http.StatusServiceUnavailable || rec.Result().Header.Get("Retry-After") != "60" || rec.Result().Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("before publish: %d %v %s", rec.Code, rec.Result().Header, rec.Body.String())
	}

	sess := apOpenSession(t, f)
	docs, counter := apBuild(t, sess, f.now)
	if err := sess.Publish(ctx, AllTimeSpendSection, counter, f.now, docs); err != nil {
		t.Fatalf("publish: %v", err)
	}
	apCloseOK(t, sess)

	for _, k := range keys {
		split, breakdown := apSplitBreakdown(k)
		path := "/api/admin/analytics/spend?window=all&split=" + split + "&breakdown=" + breakdown
		rec := f.cookie(admin, path)
		if rec.Code != http.StatusOK || rec.Result().Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("%s: %d %v %s", k, rec.Code, rec.Result().Header, rec.Body.String())
		}
		body := rec.Body.Bytes()
		idx := bytes.LastIndex(body, []byte(`,"snapshot":`))
		if idx < 0 {
			t.Fatalf("%s: no snapshot member in %.200s", k, body)
		}
		stripped := append(slices.Clone(body[:idx]), '}', '\n')
		var live []byte
		err := store.RunAnalyticsSnapshot(ctx, f.pool, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
			live = apLive(t, ctx, s, split, breakdown, f.now)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stripped, live) {
			t.Fatalf("%s: served body minus snapshot differs from the live body", k)
		}
		var dto apitypes.AnalyticsSpendSnapshotDTO
		if err := json.Unmarshal(body, &dto); err != nil {
			t.Fatalf("%s: decode: %v", k, err)
		}
		if dto.Split.Dimension != split || dto.Breakdown.Dimension != breakdown {
			t.Fatalf("%s: dimensions %q/%q", k, dto.Split.Dimension, dto.Breakdown.Dimension)
		}
		if !dto.Snapshot.GeneratedAt.Equal(f.now) || dto.Snapshot.Stale {
			t.Fatalf("%s: snapshot %+v, want generated_at %v and not stale", k, dto.Snapshot, f.now)
		}
		row, err := f.h.q.GetAnalyticsPrecomputedDoc(ctx, store.GetAnalyticsPrecomputedDocParams{Section: AllTimeSpendSection, VariantKey: k})
		if err != nil || !row.GeneratedAt.Time.Equal(dto.Snapshot.GeneratedAt) {
			t.Fatalf("%s: stored generated_at %v (%v) differs from served %v", k, row.GeneratedAt.Time, err, dto.Snapshot.GeneratedAt)
		}
	}

	// Options list the all-time window for an admin all-users scope only with the feature on.
	if got := anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options")).Windows; !slices.Equal(got, []string{"7d", "30d", "90d", "all"}) {
		t.Fatalf("admin all-users windows with the setting on = %v", got)
	}
	f.h.cfg.AnalyticsSnapshotInterval = 0
	if got := anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options")).Windows; !slices.Equal(got, []string{"7d", "30d", "90d"}) {
		t.Fatalf("admin all-users windows with the setting off = %v", got)
	}
	if rec := f.cookie(admin, "/api/admin/analytics/spend?window=all"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgAnalyticsWindowAllAllUsers) {
		t.Fatalf("setting off: %d %s", rec.Code, rec.Body.String())
	}
}

// Credentials with a stored generation present: nothing reaches the stored read without an
// admin credential, and no 4xx body carries a snapshot.
func TestAllTimeSpendCredentialsLiveDB(t *testing.T) {
	f := newAnFixture(t, apYear)
	apSeed(f)
	f.h.cfg.AnalyticsSnapshotInterval = 4 * time.Hour
	ctx := context.Background()
	admin, member := f.user(true), f.user(false)

	sess := apOpenSession(t, f)
	docs, counter := apBuild(t, sess, f.now)
	if err := sess.Publish(ctx, AllTimeSpendSection, counter, f.now, docs); err != nil {
		t.Fatalf("publish: %v", err)
	}
	apCloseOK(t, sess)

	reads := 0
	f.h.analyticsPrecomputedDoc = func(ctx context.Context, section, key string) ([]byte, time.Time, error) {
		reads++
		row, err := f.h.q.GetAnalyticsPrecomputedDoc(ctx, store.GetAnalyticsPrecomputedDocParams{Section: section, VariantKey: key})
		return row.Document, row.GeneratedAt.Time, err
	}

	const path = "/api/admin/analytics/spend?window=all"
	adminRO := cliMintToken(t, f.pool, admin, clitoken.ScopeAdminRO)
	memberToken := cliMintToken(t, f.pool, member, clitoken.ScopeUser)
	memberJWT, adminJWT := cliMintJWT(t, f.pool, member), cliMintJWT(t, f.pool, admin)

	denied := map[string]*httptest.ResponseRecorder{
		"unauthenticated": bearerReq(f.router, http.MethodGet, path, ""),
		"member cookie":   cookieReq(t, f.router, http.MethodGet, path, memberJWT, ""),
		"member bearer":   bearerReq(f.router, http.MethodGet, path, memberToken),
	}
	want := map[string]int{"unauthenticated": http.StatusUnauthorized, "member cookie": http.StatusForbidden, "member bearer": http.StatusForbidden}
	for name, rec := range denied {
		if rec.Code != want[name] || strings.Contains(rec.Body.String(), "snapshot") {
			t.Fatalf("%s: %d %s, want %d without a snapshot member", name, rec.Code, rec.Body.String(), want[name])
		}
	}
	if reads != 0 {
		t.Fatalf("%d stored reads happened for denied requests", reads)
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"admin cookie":   cookieReq(t, f.router, http.MethodGet, path, adminJWT, ""),
		"admin_ro token": bearerReq(f.router, http.MethodGet, path, adminRO),
	} {
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"snapshot":`) {
			t.Fatalf("%s: %d %.200s", name, rec.Code, rec.Body.String())
		}
	}
	if reads != 2 {
		t.Fatalf("%d stored reads for two admin requests, want 2", reads)
	}
}
