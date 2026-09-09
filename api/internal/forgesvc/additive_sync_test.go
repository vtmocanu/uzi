package forgesvc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// The additive non-PRD fetch (PRD #102 M6, Decisions 9/11/11a). These are the
// tests the PRD calls the highest-risk change it contains, and the risk is not
// subtle: FullSync deletes every cached issue absent from its keep-set, so a
// keep-set built from one half of a two-half fetch wipes the other half's rows on
// every poll, one minute apart, silently.
//
// Every failure case here fails ONE fetch, never both. A test that can only fail
// them together cannot see the asymmetric mode at all: the union built from a
// successful PRD fetch and a failed open fetch looks exactly like a legitimate
// "there are no non-PRD issues" answer.

const uziLabel = settings.DefaultUziLabel

// labelled builds a forge issue carrying labels, at a given updated_at.
func labelled(iid int64, updated time.Time, labels ...string) forge.Issue {
	return forge.Issue{IID: iid, Title: "t", State: "opened", Labels: labels, UpdatedAt: updated}
}

func keepSet(t *testing.T, st *fakeStore) []int64 {
	t.Helper()
	if len(st.deleteCalls) != 1 {
		t.Fatalf("expected exactly one eviction, got %d", len(st.deleteCalls))
	}
	keep := slices.Clone(st.deleteCalls[0].KeepIids)
	slices.Sort(keep)
	return keep
}

func upsertedIIDs(st *fakeStore) []int64 {
	out := make([]int64, 0, len(st.upserts))
	for _, u := range st.upserts {
		out = append(out, u.ForgeIssueIid)
	}
	slices.Sort(out)
	return out
}

// TestFullSyncFetchesOpenIssuesAdditively pins Decision 9's shape: a SECOND,
// independent fetch rather than a widened first one. The PRD fetch must keep
// state=all (the Closed column depends on it) and keep its label filter; the new
// one must be open-only and carry NO label filter.
func TestFullSyncFetchesOpenIssuesAdditively(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
		openIssues: []forge.Issue{labelled(2, time.Unix(100, 0), "bug")},
	}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(f.listCalls) != 3 {
		t.Fatalf("expected 3 ListIssues calls (uzi + additive open + finding), got %d: %+v", len(f.listCalls), f.listCalls)
	}
	prd, open := f.listCalls[0], f.listCalls[1]
	if prd.State != forge.StateAll {
		t.Errorf("PRD fetch state = %q, want StateAll (closed PRD cards must still reach the Closed column)", prd.State)
	}
	if len(prd.Labels) != 1 || prd.Labels[0] != uziLabel {
		t.Errorf("PRD fetch labels = %v, want [%s]", prd.Labels, uziLabel)
	}
	if open.State != forge.StateOpened {
		t.Errorf("additive fetch state = %q, want StateOpened", open.State)
	}
	if len(open.Labels) != 0 {
		t.Errorf("additive fetch must carry no label filter, got %v", open.Labels)
	}
	// The THIRD fetch is finding-labelled and state=all, mirroring the uzi fetch's shape
	// so a CLOSED finding issue reaches the cache (PRD #1183 Child B): open-only and
	// uzi-only fetches structurally miss it.
	finding := f.findingListCalls()
	if len(finding) != 1 {
		t.Fatalf("expected exactly one finding fetch, got %d: %+v", len(finding), f.listCalls)
	}
	if finding[0].State != forge.StateAll {
		t.Errorf("finding fetch state = %q, want StateAll (closed finding issues must reach the cache)", finding[0].State)
	}
	if len(finding[0].Labels) != 1 || finding[0].Labels[0] != settings.DefaultFindingLabel {
		t.Errorf("finding fetch labels = %v, want [%s]", finding[0].Labels, settings.DefaultFindingLabel)
	}
	if got := upsertedIIDs(st); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("upserted iids = %v, want both halves [1 2]", got)
	}
}

// TestFullSyncDoesNotThreadTheRunEligibleSet is PRD #196's non-change, guarded from
// the risk table: the run-eligibility gate now accepts an admin-configured SET of
// labels (default {PRD, bug}), but the sync fetch must keep passing the PRIMARY label
// ALONE. ListIssuesOptions.Labels is ANDed (forge.go:307-310), so handing it {PRD,
// bug} would fetch only issues carrying BOTH — zero on a repo where the sets are
// disjoint — and FullSync would then evict the entire cached backlog.
//
// The assertion is that the label-filtered fetch carries EXACTLY ONE label and it is
// the primary. That single-label property is the cheapest thing that fails the moment
// the eligible set (two labels by default) is threaded in here by a later cleanup.
func TestFullSyncDoesNotThreadTheRunEligibleSet(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
		openIssues: []forge.Issue{labelled(2, time.Unix(100, 0), "bug")},
	}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(f.listCalls) != 3 {
		t.Fatalf("expected 3 ListIssues calls, got %d: %+v", len(f.listCalls), f.listCalls)
	}
	prd := f.listCalls[0]
	if len(prd.Labels) != 1 || prd.Labels[0] != uziLabel {
		t.Fatalf("label-filtered fetch labels = %v, want exactly [%s] — the run-eligible SET must never reach the ANDed sync fetch", prd.Labels, uziLabel)
	}
	// The finding fetch is likewise a single-label query: the run-eligible SET must never
	// reach EITHER label-filtered fetch (ListIssuesOptions.Labels is ANDed).
	finding := f.findingListCalls()
	if len(finding) != 1 || len(finding[0].Labels) != 1 || finding[0].Labels[0] != settings.DefaultFindingLabel {
		t.Fatalf("finding fetch labels = %+v, want exactly [%s]", finding, settings.DefaultFindingLabel)
	}
}

// TestFullSyncKeepSetIsTheUnion is the one that matters. Built from the PRD fetch
// alone the keep-set omits every non-PRD row the second fetch just wrote, and
// DeleteIssuesNotIn removes them — every poll.
func TestFullSyncKeepSetIsTheUnion(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel), labelled(3, time.Unix(100, 0), uziLabel)},
		openIssues: []forge.Issue{labelled(2, time.Unix(100, 0), "bug"), labelled(4, time.Unix(100, 0))},
	}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if got := keepSet(t, st); !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Fatalf("keep-set = %v, want the union [1 2 3 4]; a PRD-only keep-set evicts the non-PRD rows every poll", got)
	}
}

// TestFullSyncDiscardsPRDRowsFromTheOpenFetch pins the ADDITIVE half of Decision
// 9: the unfiltered open fetch necessarily re-returns the open PRD issues, and
// those belong to the PRD path. Without the discard an issue is written twice per
// sync, the second time from the later snapshot.
func TestFullSyncDiscardsPRDRowsFromTheOpenFetch(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	shared := labelled(1, time.Unix(100, 0), uziLabel, "bug")
	f := &fakeForge{
		issues:     []forge.Issue{shared},
		openIssues: []forge.Issue{shared, labelled(2, time.Unix(100, 0), "bug")},
	}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if got := upsertedIIDs(st); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("upserted iids = %v, want [1 2] — issue 1 is in BOTH fetches and must be written once", got)
	}
	if got := keepSet(t, st); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("keep-set = %v, want [1 2] with no duplicate for the shared issue", got)
	}
}

// TestFullSyncEvictsNothingWhenEitherFetchFails is Decision 11's fail-closed rule,
// exercised against EACH fetch on its own. The PRD-fetch case is the pre-M6
// behaviour; the open-fetch case is new and is the one that would otherwise look
// like a legitimately empty non-PRD set and delete the whole backlog.
func TestFullSyncEvictsNothingWhenEitherFetchFails(t *testing.T) {
	boom := errors.New("forge is down")
	cases := []struct {
		name string
		fake *fakeForge
	}{
		{"PRD fetch fails", &fakeForge{
			listErr:    boom,
			openIssues: []forge.Issue{labelled(2, time.Unix(100, 0), "bug")},
		}},
		{"additive open fetch fails", &fakeForge{
			issues:  []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
			openErr: boom,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			svc := newTestService(st)

			marks, err := svc.FullSync(context.Background(), uuid.New(), 7, tc.fake)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the forge error surfaced (no soft-fail, no log-and-continue)", err)
			}
			if len(st.deleteCalls) != 0 {
				t.Errorf("a half-failed fetch must evict NOTHING, got %+v", st.deleteCalls)
			}
			// BOTH fields, because FullSync reports observations for the poller to
			// Advance into its own pair and the zero pair is what makes that fold a
			// no-op. The two rows reach that zero pair by DIFFERENT routes, and only
			// the second is a real test of the rule: when the PRD fetch fails
			// FullSync returns before the open fetch is ever issued, so Open is zero
			// trivially; when the OPEN fetch fails the PRD fetch has already come
			// back with rows, and reporting its mark would advance the caller past a
			// window whose rows were never written, since neither half's upserts ran.
			if !marks.PRD.IsZero() || !marks.Open.IsZero() {
				t.Errorf("marks = %+v, want the zero pair so the poller keeps the marks it had", marks)
			}
		})
	}
}

// TestIncrementalSyncFailsClosedOnEitherFetch is Decision 11a's asymmetric-failure
// rule. Returning nil with an advanced mark after one fetch failed is what makes
// the next pass skip the failed path's window until the next full reconcile, so
// the assertion is on the RETURNED MARKS, not only on the error.
//
// The caller's two marks DIFFER deliberately, and BOTH are asserted. With per-path
// marks the tempting half-measure is to hold only the failed fetch's mark and let
// the successful one advance; that is unsound, because both fetches complete before
// any upsert runs, so a half-failure means NEITHER path's rows were written. A pair
// of equal caller marks would also let a `Marks{hwm, hwm}` collapse pass.
func TestIncrementalSyncFailsClosedOnEitherFetch(t *testing.T) {
	boom := errors.New("forge is down")
	start := Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0)}
	cases := []struct {
		name string
		fake *fakeForge
	}{
		{"PRD fetch fails", &fakeForge{
			listErr:    boom,
			openIssues: []forge.Issue{labelled(2, time.Unix(9000, 0), "bug")},
		}},
		{"additive open fetch fails", &fakeForge{
			issues:  []forge.Issue{labelled(1, time.Unix(9000, 0), uziLabel)},
			openErr: boom,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			svc := newTestService(st)

			got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, tc.fake, start)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the forge error surfaced", err)
			}
			if !got.PRD.Equal(start.PRD) || !got.Open.Equal(start.Open) {
				t.Fatalf("marks = %+v, want the WHOLE pair held at %+v — advancing past a window whose rows were never written makes the skip permanent until the reconcile", got, start)
			}
		})
	}
}

// TestEachFetchCarriesItsOwnMark is the wire-level half of issue #177: on the
// incremental path each fetch's lower bound comes from ITS OWN mark. The caller's
// two marks DIFFER, which is the whole point — the test this replaced passed one
// `start` to both and so certified a single-mark implementation just as happily as
// a correct one. It also catches a field swap, which no assertion on the returned
// pair can see.
func TestEachFetchCarriesItsOwnMark(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{}
	// All THREE marks DIFFER, so a field swap among the fetches' lower bounds fails here
	// rather than reading as "each carried a bound".
	start := Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0), Finding: time.Unix(700, 0)}

	if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(f.listCalls) != 3 {
		t.Fatalf("expected 3 ListIssues calls, got %d", len(f.listCalls))
	}
	prd, open, finding := f.prdListCalls(), f.openListCalls(), f.findingListCalls()
	if len(prd) != 1 || len(open) != 1 || len(finding) != 1 {
		t.Fatalf("expected one uzi, one open and one finding fetch, got %d/%d/%d: %+v", len(prd), len(open), len(finding), f.listCalls)
	}
	if prd[0].UpdatedAfter == nil || !prd[0].UpdatedAfter.Equal(start.PRD) {
		t.Errorf("PRD fetch updated_after = %v, want %v (its OWN mark)", prd[0].UpdatedAfter, start.PRD)
	}
	if open[0].UpdatedAfter == nil || !open[0].UpdatedAfter.Equal(start.Open) {
		t.Errorf("open fetch updated_after = %v, want %v (its OWN mark)", open[0].UpdatedAfter, start.Open)
	}
	if finding[0].UpdatedAfter == nil || !finding[0].UpdatedAfter.Equal(start.Finding) {
		t.Errorf("finding fetch updated_after = %v, want %v (its OWN mark)", finding[0].UpdatedAfter, start.Finding)
	}
}

// TestPerPathMarksAdvanceIndependently is the core of issue #177, on both sync
// paths. The two fetches' maxima DIFFER and the later fetch's is the larger, which
// is what discriminates every broken shape at once: the old shared min returns
// {1000, 1000}, a naive shared max returns {5000, 5000}, and any single-mark
// implementation returns two equal fields whatever it picks.
//
// The soundness the old min() protected is not lost, it is dissolved: the PRD mark
// stays at 1000 <= tA, so a PRD issue updated between the two calls is still
// returned by the next PRD fetch. Only the OPEN mark takes the later fetch's
// evidence, and it owns nothing that fetch did not see.
func TestPerPathMarksAdvanceIndependently(t *testing.T) {
	prdMax := time.Unix(1000, 0)
	openMax := time.Unix(5000, 0)
	newFake := func() *fakeForge {
		return &fakeForge{
			issues:     []forge.Issue{labelled(1, prdMax, uziLabel)},
			openIssues: []forge.Issue{labelled(2, openMax, "bug")},
		}
	}
	assert := func(t *testing.T, got Marks) {
		t.Helper()
		if !got.PRD.Equal(prdMax) {
			t.Errorf("PRD mark = %v, want %v (its OWN fetch's max) — taking the open fetch's later evidence steps past a PRD issue updated between the two calls", got.PRD, prdMax)
		}
		if !got.Open.Equal(openMax) {
			t.Errorf("Open mark = %v, want %v (its OWN fetch's max) — clamping it to the PRD fetch is the shared-mark stall issue #177 is about", got.Open, openMax)
		}
	}

	t.Run("FullSync", func(t *testing.T) {
		got, err := newTestService(&fakeStore{}).FullSync(context.Background(), uuid.New(), 7, newFake())
		if err != nil {
			t.Fatalf("FullSync: %v", err)
		}
		assert(t, got)
	})

	t.Run("IncrementalSync", func(t *testing.T) {
		// Caller marks differ so a returned pair that collapsed onto one field
		// cannot pass by coincidence.
		start := Marks{PRD: time.Unix(1, 0), Open: time.Unix(2, 0)}
		got, err := newTestService(&fakeStore{}).IncrementalSync(context.Background(), uuid.New(), 7, newFake(), start)
		if err != nil {
			t.Fatalf("IncrementalSync: %v", err)
		}
		assert(t, got)
	})
}

// TestSyncCountsDiscardedRowsTowardsTheOpenMark: the open fetch's mark is read off
// its RAW result, before the PRD-labelled rows are dropped. Those rows still witness
// when the forge was read, and computing the bound after the discard would stall the
// Open mark on any repo whose only open issues carry the PRD label — which is the
// same permanent stall issue #177 fixes, arriving through the other door.
//
// BOTH sync paths, because both derive their Open mark this way and both maxUpdatedAt's
// doc and this comment state the raw-result property unconditionally. The predecessor
// covered IncrementalSync only, which left FullSync free to compute its Open mark from
// the post-withoutLabel slice with the whole suite still green.
func TestSyncCountsDiscardedRowsTowardsTheOpenMark(t *testing.T) {
	prdMax := time.Unix(5000, 0)
	openMax := time.Unix(3000, 0)
	newFake := func() *fakeForge {
		return &fakeForge{
			issues: []forge.Issue{labelled(1, prdMax, uziLabel)},
			// Every row here is discarded by the PRD filter, so the FILTERED max is
			// zero while the raw one is not.
			openIssues: []forge.Issue{labelled(1, openMax, uziLabel)},
		}
	}
	assert := func(t *testing.T, got Marks) {
		t.Helper()
		if !got.Open.Equal(openMax) {
			t.Errorf("Open mark = %v, want %v — the discarded row is still evidence of when the open fetch ran; a post-withoutLabel max gives zero and never advances", got.Open, openMax)
		}
		if !got.PRD.Equal(prdMax) {
			t.Errorf("PRD mark = %v, want %v — its own fetch is untouched by what the open one discarded", got.PRD, prdMax)
		}
	}

	t.Run("FullSync", func(t *testing.T) {
		got, err := newTestService(&fakeStore{}).FullSync(context.Background(), uuid.New(), 7, newFake())
		if err != nil {
			t.Fatalf("FullSync: %v", err)
		}
		assert(t, got)
	})

	t.Run("IncrementalSync", func(t *testing.T) {
		// The caller's Open mark differs from openMax, so "advanced to 3000" and
		// "left alone" are distinguishable outcomes.
		start := Marks{PRD: time.Unix(1, 0), Open: time.Unix(2, 0)}
		got, err := newTestService(&fakeStore{}).IncrementalSync(context.Background(), uuid.New(), 7, newFake(), start)
		if err != nil {
			t.Fatalf("IncrementalSync: %v", err)
		}
		assert(t, got)
	})
}

// TestPerPathMarksZeroCases pins what an EMPTY fetch means, at the Sync level where
// the semantics now live — its predecessor unit-tested the shared-mark combine
// helper, which this change deletes outright. An empty result is not evidence: it
// moves its own mark not at all, and constrains the other mark not at all.
//
// The first row IS issue #177. Against the old shared mark the whole pair stays at
// 500, because the combine returned prdMax whenever either input was zero — so a
// repo with open issues and no PRD-labelled ones re-read its entire open set every
// single poll, forever.
func TestPerPathMarksZeroCases(t *testing.T) {
	cases := []struct {
		name        string
		prd, open   []forge.Issue
		start, want Marks
	}{
		{
			// Equal caller marks are deliberate HERE and nowhere else in this table:
			// they make "PRD did not move, Open did" readable as a single pair. A
			// field swap still fails, since the wanted fields differ.
			name:  "#177: an empty PRD fetch must not stall the open mark",
			prd:   nil,
			open:  []forge.Issue{labelled(2, time.Unix(5000, 0), "bug")},
			start: Marks{PRD: time.Unix(500, 0), Open: time.Unix(500, 0)},
			want:  Marks{PRD: time.Unix(500, 0), Open: time.Unix(5000, 0)},
		},
		{
			name:  "both fetches empty: neither mark moves",
			prd:   nil,
			open:  nil,
			start: Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0)},
			want:  Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0)},
		},
		{
			name:  "an empty open fetch must not stall the PRD mark",
			prd:   []forge.Issue{labelled(1, time.Unix(1000, 0), uziLabel)},
			open:  nil,
			start: Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0)},
			want:  Marks{PRD: time.Unix(1000, 0), Open: time.Unix(900, 0)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			svc := newTestService(st)
			f := &fakeForge{issues: tc.prd, openIssues: tc.open}

			got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, tc.start)
			if err != nil {
				t.Fatalf("IncrementalSync: %v", err)
			}
			if !got.PRD.Equal(tc.want.PRD) {
				t.Errorf("PRD mark = %v, want %v", got.PRD, tc.want.PRD)
			}
			if !got.Open.Equal(tc.want.Open) {
				t.Errorf("Open mark = %v, want %v", got.Open, tc.want.Open)
			}
		})
	}
}

// TestWithoutLabelMatchesExactly: the discard has to classify an issue the same way
// the forge's own label filter did, so it is exact and case-sensitive like every
// other label comparison in this package.
func TestWithoutLabelMatchesExactly(t *testing.T) {
	in := []forge.Issue{
		labelled(1, time.Unix(1, 0), uziLabel),
		labelled(2, time.Unix(1, 0), "bug"),
		labelled(3, time.Unix(1, 0)),
		labelled(4, time.Unix(1, 0), "prd"), // different case: a different label
		labelled(5, time.Unix(1, 0), "bug", uziLabel),
	}
	var got []int64
	for _, is := range withoutLabel(in, uziLabel) {
		got = append(got, is.IID)
	}
	if !slices.Equal(got, []int64{2, 3, 4}) {
		t.Fatalf("withoutLabel kept %v, want [2 3 4]", got)
	}
}

// TestAClosedNonPRDIssueIsNeitherRefreshedNorKept pins the window docs/board.md
// documents and the Board.tsx comment describes — and it exists because that comment
// described it WRONGLY for a while, claiming the card renders no button.
//
// The mechanism, which is what makes the claim checkable: once a non-PRD issue
// closes, NEITHER fetch returns it. The PRD fetch is label-filtered and it carries
// no PRD label; the additive fetch is StateOpened and it is no longer open. So the
// cached row is never re-upserted — its state stays 'opened', which is why the card
// keeps rendering as an open, promotable card rather than sliding into Closed — and
// it is absent from the union keep-set, so the next FullSync evicts it.
//
// Both halves are asserted, because either alone tells a misleading story: "not
// upserted" without "evicted" reads as a leak, and "evicted" without "not upserted"
// reads as an orderly close.
func TestAClosedNonPRDIssueIsNeitherRefreshedNorKept(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)

	// The forge's state AFTER the issue closed: the PRD fetch still returns the PRD
	// issue, and the open fetch no longer returns issue 2.
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
		openIssues: []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
	}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	// Never re-upserted: nothing wrote issue 2, so its cached state is untouched and
	// still says 'opened'. That staleness is the documented window, not a bug.
	if got := upsertedIIDs(st); slices.Contains(got, int64(2)) {
		t.Fatalf("upserted %v — a closed non-PRD issue must not be re-upserted; neither fetch can return it", got)
	}

	// And evicted: absent from the union keep-set, so the reconcile removes it.
	keep := keepSet(t, st)
	if slices.Contains(keep, int64(2)) {
		t.Fatalf("keep-set %v still holds the closed non-PRD issue; the window must END at the reconcile", keep)
	}
	if !slices.Contains(keep, int64(1)) {
		t.Fatalf("keep-set %v dropped the PRD issue, which is a different bug entirely", keep)
	}
}

// ── PRD #1183 Child B: the additive finding fetch ─────────────────────────────
//
// The third fetch mirrors the uzi fetch (finding label, state=all) and exists to close
// one structural gap: a CLOSED finding-labelled issue is returned by NEITHER the uzi
// fetch (it is not uzi-labelled) NOR the open fetch (it is closed), so before it the
// row was evicted on close and forgesvc.SyncFindingIssueCloses never observed the
// open→closed edge. These tests pin the fetch's shape, its additive upsert/keep, its
// fail-closed contract (Decision 11, extended from two fetches to three), and the
// defensive skip when the finding label collides with the uzi label.

// findingClosed builds a CLOSED forge issue carrying the default finding marker label
// (and no uzi label) at a given updated_at — the exact shape the third fetch exists to
// catch and the first two structurally miss.
func findingClosed(iid int64, updated time.Time) forge.Issue {
	return forge.Issue{IID: iid, Title: "t", State: "closed", Labels: []string{settings.DefaultFindingLabel}, UpdatedAt: updated}
}

// TestFullSyncFetchesFindingIssuesAdditively is the finding twin of
// TestFullSyncFetchesOpenIssuesAdditively: the third fetch observes a closed finding
// issue, upserts it as closed (so the close-sync can read state='closed'), and keeps it
// in the union so the reconcile does not evict it. Its mark advances to the fetch's max.
func TestFullSyncFetchesFindingIssuesAdditively(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{
		issues:        []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
		openIssues:    []forge.Issue{labelled(2, time.Unix(100, 0), "bug")},
		findingIssues: []forge.Issue{findingClosed(9, time.Unix(300, 0))},
	}

	got, err := svc.FullSync(context.Background(), uuid.New(), 7, f)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if ids := upsertedIIDs(st); !slices.Equal(ids, []int64{1, 2, 9}) {
		t.Fatalf("upserted iids = %v, want [1 2 9] — the closed finding issue must reach the cache", ids)
	}
	if keep := keepSet(t, st); !slices.Contains(keep, int64(9)) {
		t.Fatalf("keep-set %v dropped the closed finding issue; the third fetch must keep it cached, not evict it on close", keep)
	}
	// The row is written with state='closed' — the exact snapshot SyncFindingIssueCloses
	// keys the open→closed edge on.
	var wroteClosed bool
	for _, u := range st.upserts {
		if u.ForgeIssueIid == 9 {
			wroteClosed = u.State == "closed"
		}
	}
	if !wroteClosed {
		t.Errorf("issue 9 must be upserted with state='closed'; the close-sync edge depends on it")
	}
	if !got.Finding.Equal(time.Unix(300, 0)) {
		t.Errorf("Finding mark = %v, want %v (its OWN fetch's max)", got.Finding, time.Unix(300, 0))
	}
}

// TestFullSyncEvictsNothingWhenFindingFetchFails extends Decision 11's fail-closed rule
// to the third fetch: a failed finding fetch drives NO eviction and NO upsert, and
// FullSync returns the zero Marks so the caller holds the marks it had. Without the
// return-before-eviction guard the union would be built from two of three fetches and
// wipe whatever the finding fetch owns.
func TestFullSyncEvictsNothingWhenFindingFetchFails(t *testing.T) {
	boom := errors.New("forge is down")
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)},
		openIssues: []forge.Issue{labelled(2, time.Unix(100, 0), "bug")},
		findingErr: boom,
	}

	marks, err := svc.FullSync(context.Background(), uuid.New(), 7, f)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the finding-fetch error surfaced (no soft-fail)", err)
	}
	if len(st.deleteCalls) != 0 {
		t.Errorf("a failed finding fetch must evict NOTHING, got %+v", st.deleteCalls)
	}
	if len(st.upserts) != 0 {
		t.Errorf("a failed finding fetch must upsert NOTHING (all fetches complete before any write), got %+v", st.upserts)
	}
	if !marks.PRD.IsZero() || !marks.Open.IsZero() || !marks.Finding.IsZero() {
		t.Errorf("marks = %+v, want the zero Marks so the poller keeps the marks it had", marks)
	}
}

// TestIncrementalSyncFailsClosedOnFindingFetch is Decision 11a's asymmetric-failure rule
// for the third fetch: a failed finding fetch returns the CALLER'S whole Marks unchanged,
// so the next pass does not skip a window whose rows never reached the cache.
func TestIncrementalSyncFailsClosedOnFindingFetch(t *testing.T) {
	boom := errors.New("forge is down")
	st := &fakeStore{}
	svc := newTestService(st)
	start := Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0), Finding: time.Unix(700, 0)}
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(9000, 0), uziLabel)},
		openIssues: []forge.Issue{labelled(2, time.Unix(9000, 0), "bug")},
		findingErr: boom,
	}

	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the finding-fetch error surfaced", err)
	}
	if !got.PRD.Equal(start.PRD) || !got.Open.Equal(start.Open) || !got.Finding.Equal(start.Finding) {
		t.Fatalf("marks = %+v, want the WHOLE trio held at %+v — advancing past an unwritten window makes the skip permanent until the reconcile", got, start)
	}
	if len(st.upserts) != 0 {
		t.Errorf("no path's rows may be written when a fetch fails, got %+v", st.upserts)
	}
}

// TestIncrementalSyncAdvancesFindingMark: the finding fetch's mark advances to its own
// batch max, independently of the other two (which return nothing here and hold at the
// caller's values). The closed finding issue is upserted so the close-sync can read it.
func TestIncrementalSyncAdvancesFindingMark(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{findingIssues: []forge.Issue{findingClosed(9, time.Unix(4000, 0))}}
	start := Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0), Finding: time.Unix(1000, 0)}

	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if !got.Finding.Equal(time.Unix(4000, 0)) {
		t.Errorf("Finding mark = %v, want its fetch's max %v", got.Finding, time.Unix(4000, 0))
	}
	if !got.PRD.Equal(start.PRD) || !got.Open.Equal(start.Open) {
		t.Errorf("PRD/Open marks moved on an empty fetch: got %+v, want held at %+v", got, start)
	}
	if ids := upsertedIIDs(st); !slices.Equal(ids, []int64{9}) {
		t.Errorf("upserted iids = %v, want [9] (the closed finding issue reaching the cache)", ids)
	}
}

// TestFullSyncSkipsFindingFetchWhenLabelEqualsUzi is the defensive skip: ValidateMerged
// forbids finding_label == uzi_label, but if a misconfig slipped through the finding
// fetch would be a pure duplicate of the state=all uzi fetch. FullSync issues only the
// two base fetches and leaves the Finding mark zero (an unissued fetch is no evidence).
func TestFullSyncSkipsFindingFetchWhenLabelEqualsUzi(t *testing.T) {
	st := &fakeStore{}
	svc := New(st, nil, time.Second, fakeLabels{uzi: uziLabel, finding: uziLabel})
	f := &fakeForge{issues: []forge.Issue{labelled(1, time.Unix(100, 0), uziLabel)}}

	got, err := svc.FullSync(context.Background(), uuid.New(), 7, f)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(f.listCalls) != 2 {
		t.Fatalf("expected only 2 ListIssues calls when finding==uzi (third skipped), got %d: %+v", len(f.listCalls), f.listCalls)
	}
	if !got.Finding.IsZero() {
		t.Errorf("Finding mark = %v, want zero — the skipped fetch is no evidence", got.Finding)
	}
}
