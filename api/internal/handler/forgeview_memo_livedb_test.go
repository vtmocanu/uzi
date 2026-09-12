package handler

// forgeview_memo_livedb_test.go pins the PRD #1255 M2b acceptance bounds for the
// forgememo wiring under the forge-view routes, driving the REAL h.Routes() router with
// an injected COUNTING forge fake:
//
//   1. two concurrent GetPull for one repo+iid issue ONE GetMergeRequestSummary call
//      (singleflight collapse under the tenant-scoped memo);
//   2. two sequential ListPulls inside the TTL issue one per-PR summary call PER PR
//      total (the second list is fully memoised — refs AND every unchanged per-PR
//      summary), because the ref's UpdatedAt change key is stable;
//   3. the list caps the fan-out: ListPulls passes Limit=forgeViewPullsLimit to
//      ListMergeRequestRefs, so the fake receives Limit=30 and the per-PR summary
//      call count is ≤ 30 even against 40 open refs.
//
// The GITHUB-DRIVER-level "61 not 81" cold-list cost bound lives in the forge package
// (github_forgeview_bounds_test.go); this file pins the handler/fake-level bounds.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (cliLiveDB), the
// same harness the M2a auth suite uses — a repo row is needed for repoForRequest.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// countingForgeView is the injected forge for the memo bounds suite: it counts each
// forge-view read and can optionally BLOCK the summary read on a barrier so a test can
// hold the singleflight leader inside its load while a second concurrent request arrives.
// refs, when set, is the ref list ListMergeRequestRefs returns (honouring opts.Limit the
// way the real drivers do); lastRefsLimit records the Limit the handler passed, so a test
// can prove the fan-out cap is applied by capping the refs list, not after it.
type countingForgeView struct {
	forgetest.BaseFake
	refs          []forge.MergeRequestRef
	summaryCalls  atomic.Int64
	refsCalls     atomic.Int64
	checksCalls   atomic.Int64
	reviewCalls   atomic.Int64
	lastRefsLimit atomic.Int64

	entered chan struct{} // a summary load has started (non-blocking send)
	release chan struct{} // unblocks a blocked summary load (closed to release all)
}

func (f *countingForgeView) ListMergeRequestRefs(_ context.Context, _ int64, opts forge.ListMergeRequestsOptions) ([]forge.MergeRequestRef, error) {
	f.refsCalls.Add(1)
	f.lastRefsLimit.Store(int64(opts.Limit))
	refs := f.refs
	if refs == nil {
		refs = []forge.MergeRequestRef{{IID: 7, HeadSHA: "sha7", UpdatedAt: time.Unix(1700000000, 0)}}
	}
	if opts.Limit > 0 && len(refs) > opts.Limit {
		refs = refs[:opts.Limit] // the real drivers cap to Limit; the fake mirrors that
	}
	return refs, nil
}

func (f *countingForgeView) GetMergeRequestSummary(_ context.Context, _ int64, iid int64) (forge.MergeRequestSummary, error) {
	f.summaryCalls.Add(1)
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		<-f.release
	}
	return forge.MergeRequestSummary{IID: iid, Title: "t", HeadSHA: "sha7", State: forge.MRStateOpened}, nil
}

func (f *countingForgeView) ListChecks(context.Context, int64, string) ([]forge.Check, error) {
	f.checksCalls.Add(1)
	return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil
}

func (f *countingForgeView) ListMergeRequestReviews(context.Context, int64, int64) ([]forge.Review, error) {
	f.reviewCalls.Add(1)
	return []forge.Review{{Author: "r", State: "APPROVED"}}, nil
}

// TestForgeViewMemoConcurrentDetailLiveDB pins bound (1): two concurrent GetPull for one
// repo+iid issue exactly one GetMergeRequestSummary call. The first request is held
// inside the summary load on a barrier so the second arrives while the first is the
// singleflight leader; releasing the barrier lets both finish sharing one load. A memo
// that is not wired (or under-scoped) would call the summary twice.
func TestForgeViewMemoConcurrentDetailLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	fake := &countingForgeView{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.forgeFactory = func(string, string, []byte) (forge.Forge, error) { return fake, nil }

	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)
	enabled := rmSeedRepo(t, pool, connID, 1255101, true)
	path := "/api/repos/" + enabled.String() + "/pulls/7"

	var wg sync.WaitGroup
	wg.Add(2)
	codes := make([]int, 2)
	go func() { defer wg.Done(); codes[0] = bearerReq(router, http.MethodGet, path, uzc).Code }()

	// Wait for the leader to enter the (blocked) summary load, then fire the second
	// request so it collapses onto the in-flight singleflight rather than starting a
	// fresh load.
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		close(fake.release)
		t.Fatal("timed out waiting for the first GetPull to enter the summary load")
	}
	go func() { defer wg.Done(); codes[1] = bearerReq(router, http.MethodGet, path, uzc).Code }()
	close(fake.release) // release the leader; both requests now complete

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for both GetPull requests to finish")
	}

	if got := fake.summaryCalls.Load(); got != 1 {
		t.Fatalf("GetMergeRequestSummary called %d times, want 1 (singleflight collapses two concurrent detail requests)", got)
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, code)
		}
	}
}

// TestForgeViewMemoListPullsUnchangedLiveDB pins bound (2): two sequential ListPulls
// inside the TTL window issue one per-PR summary call PER PR total. The second list hits
// the memo for the refs call AND for every unchanged PR's summary (the ref UpdatedAt
// change key is stable across the two lists), so no PR is re-enriched.
func TestForgeViewMemoListPullsUnchangedLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	upd := time.Unix(1700000123, 0).UTC()
	fake := &countingForgeView{refs: []forge.MergeRequestRef{
		{IID: 11, HeadSHA: "a", UpdatedAt: upd},
		{IID: 22, HeadSHA: "b", UpdatedAt: upd},
	}}
	h.forgeFactory = func(string, string, []byte) (forge.Forge, error) { return fake, nil }

	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)
	enabled := rmSeedRepo(t, pool, connID, 1255102, true)
	path := "/api/repos/" + enabled.String() + "/pulls"

	for i := 0; i < 2; i++ {
		if rec := bearerReq(router, http.MethodGet, path, uzc); rec.Code != http.StatusOK {
			t.Fatalf("ListPulls #%d = %d, want 200\nbody: %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := fake.summaryCalls.Load(); got != 2 {
		t.Fatalf("GetMergeRequestSummary called %d times, want 2 (once per PR; the second list is fully memoised)", got)
	}
	if got := fake.refsCalls.Load(); got != 1 {
		t.Fatalf("ListMergeRequestRefs called %d times, want 1 (the second list's refs are memoised too)", got)
	}
}

// TestForgeViewMemoListPullsFanoutCapLiveDB pins bound (3): the list caps the per-PR
// fan-out. Against 40 open refs, ListPulls passes Limit=forgeViewPullsLimit to
// ListMergeRequestRefs (the fake records it), the fake returns ≤ that many, and the
// per-PR summary call count is ≤ forgeViewPullsLimit — never one per open PR.
func TestForgeViewMemoListPullsFanoutCapLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	refs := make([]forge.MergeRequestRef, 40)
	for i := range refs {
		refs[i] = forge.MergeRequestRef{IID: int64(i + 1), HeadSHA: "s", UpdatedAt: time.Unix(1700000000, 0)}
	}
	fake := &countingForgeView{refs: refs}
	h.forgeFactory = func(string, string, []byte) (forge.Forge, error) { return fake, nil }

	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)
	enabled := rmSeedRepo(t, pool, connID, 1255103, true)
	path := "/api/repos/" + enabled.String() + "/pulls"

	if rec := bearerReq(router, http.MethodGet, path, uzc); rec.Code != http.StatusOK {
		t.Fatalf("ListPulls = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if got := fake.lastRefsLimit.Load(); got != int64(forgeViewPullsLimit) {
		t.Fatalf("ListMergeRequestRefs received Limit=%d, want %d (the handler applies the cap by capping the refs list)", got, forgeViewPullsLimit)
	}
	if got := fake.summaryCalls.Load(); got > int64(forgeViewPullsLimit) {
		t.Fatalf("GetMergeRequestSummary called %d times, want ≤ %d (the fan-out is capped at the refs limit)", got, forgeViewPullsLimit)
	}
}
