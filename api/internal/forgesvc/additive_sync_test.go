package forgesvc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
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

const prdLabel = settings.DefaultPRDLabel

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
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), prdLabel)},
		openIssues: []forge.Issue{labelled(2, time.Unix(100, 0), "bug")},
	}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(f.listCalls) != 2 {
		t.Fatalf("expected 2 ListIssues calls (PRD + additive), got %d: %+v", len(f.listCalls), f.listCalls)
	}
	prd, open := f.listCalls[0], f.listCalls[1]
	if prd.State != forge.StateAll {
		t.Errorf("PRD fetch state = %q, want StateAll (closed PRD cards must still reach the Closed column)", prd.State)
	}
	if len(prd.Labels) != 1 || prd.Labels[0] != prdLabel {
		t.Errorf("PRD fetch labels = %v, want [%s]", prd.Labels, prdLabel)
	}
	if open.State != forge.StateOpened {
		t.Errorf("additive fetch state = %q, want StateOpened", open.State)
	}
	if len(open.Labels) != 0 {
		t.Errorf("additive fetch must carry no label filter, got %v", open.Labels)
	}
	if got := upsertedIIDs(st); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("upserted iids = %v, want both halves [1 2]", got)
	}
}

// TestFullSyncKeepSetIsTheUnion is the one that matters. Built from the PRD fetch
// alone the keep-set omits every non-PRD row the second fetch just wrote, and
// DeleteIssuesNotIn removes them — every poll.
func TestFullSyncKeepSetIsTheUnion(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), prdLabel), labelled(3, time.Unix(100, 0), prdLabel)},
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
	shared := labelled(1, time.Unix(100, 0), prdLabel, "bug")
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
			issues:  []forge.Issue{labelled(1, time.Unix(100, 0), prdLabel)},
			openErr: boom,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			svc := newTestService(st)

			hwm, err := svc.FullSync(context.Background(), uuid.New(), 7, tc.fake)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the forge error surfaced (no soft-fail, no log-and-continue)", err)
			}
			if len(st.deleteCalls) != 0 {
				t.Errorf("a half-failed fetch must evict NOTHING, got %+v", st.deleteCalls)
			}
			if !hwm.IsZero() {
				t.Errorf("hwm = %v, want the zero time so the poller keeps the mark it had", hwm)
			}
		})
	}
}

// TestIncrementalSyncFailsClosedOnEitherFetch is Decision 11a's asymmetric-failure
// rule. Returning nil with an advanced mark after one fetch failed is what makes
// the next pass skip the failed path's window until the next full reconcile, so
// the assertion is on the RETURNED MARK, not only on the error.
func TestIncrementalSyncFailsClosedOnEitherFetch(t *testing.T) {
	boom := errors.New("forge is down")
	start := time.Unix(500, 0)
	cases := []struct {
		name string
		fake *fakeForge
	}{
		{"PRD fetch fails", &fakeForge{
			listErr:    boom,
			openIssues: []forge.Issue{labelled(2, time.Unix(9000, 0), "bug")},
		}},
		{"additive open fetch fails", &fakeForge{
			issues:  []forge.Issue{labelled(1, time.Unix(9000, 0), prdLabel)},
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
			if !got.Equal(start) {
				t.Fatalf("hwm = %v, want it held at %v — advancing past a window one fetch never read makes the skip permanent until the reconcile", got, start)
			}
		})
	}
}

// TestSyncBoundsBothFetchesByTheHWM: the additive fetch is unbounded only on a
// FullSync. On the incremental path it must carry the same lower bound as the PRD
// fetch, or every poll re-reads the repo's entire open set.
func TestSyncBoundsBothFetchesByTheHWM(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{}
	start := time.Unix(500, 0)

	if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(f.listCalls) != 2 {
		t.Fatalf("expected 2 ListIssues calls, got %d", len(f.listCalls))
	}
	for i, c := range f.listCalls {
		if c.UpdatedAfter == nil || !c.UpdatedAfter.Equal(start) {
			t.Errorf("call %d updated_after = %v, want %v", i, c.UpdatedAfter, start)
		}
	}
}

// TestSyncAdvancesHWMByTheEarlierFetch is Decision 11a's two-fetch window race,
// both fetches succeeding. The fixture is deliberately one where the two maxima
// DIFFER and the later fetch's is the larger — with equal maxima, min and max
// return the same value and a broken implementation passes.
func TestSyncAdvancesHWMByTheEarlierFetch(t *testing.T) {
	prdMax := time.Unix(1000, 0)
	openMax := time.Unix(5000, 0)

	t.Run("FullSync", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{
			issues:     []forge.Issue{labelled(1, prdMax, prdLabel)},
			openIssues: []forge.Issue{labelled(2, openMax, "bug")},
		}
		got, err := svc.FullSync(context.Background(), uuid.New(), 7, f)
		if err != nil {
			t.Fatalf("FullSync: %v", err)
		}
		if !got.Equal(prdMax) {
			t.Fatalf("hwm = %v, want %v (the EARLIER fetch's max) — max() here steps past a PRD issue updated between the two calls", got, prdMax)
		}
	})

	t.Run("IncrementalSync", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{
			issues:     []forge.Issue{labelled(1, prdMax, prdLabel)},
			openIssues: []forge.Issue{labelled(2, openMax, "bug")},
		}
		got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, time.Unix(1, 0))
		if err != nil {
			t.Fatalf("IncrementalSync: %v", err)
		}
		if !got.Equal(prdMax) {
			t.Fatalf("hwm = %v, want %v (the EARLIER fetch's max)", got, prdMax)
		}
	})
}

// TestSyncCountsDiscardedRowsTowardsTheHWM: the open fetch's contribution is read
// off its RAW result, before the PRD-labelled rows are dropped. Those rows still
// witness when the forge was read, and computing the bound after the discard would
// stall the mark on any repo whose only open issues carry the PRD label.
func TestSyncCountsDiscardedRowsTowardsTheHWM(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	prdMax := time.Unix(5000, 0)
	f := &fakeForge{
		issues: []forge.Issue{labelled(1, prdMax, prdLabel)},
		// Every row here is discarded by the PRD filter, so the FILTERED max is zero
		// while the raw one is not.
		openIssues: []forge.Issue{labelled(1, time.Unix(3000, 0), prdLabel)},
	}

	want := time.Unix(3000, 0)
	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("hwm = %v, want %v — the discarded row is still evidence of when the open fetch ran", got, want)
	}
}

// TestAdvanceHWMZeroCases pins the asymmetry the fetch ORDER creates, which is the
// part of advanceHWM a reader is most likely to "simplify" into a symmetric min.
func TestAdvanceHWMZeroCases(t *testing.T) {
	early := time.Unix(1000, 0)
	late := time.Unix(5000, 0)
	cases := []struct {
		name                  string
		prdMax, openMax, want time.Time
	}{
		{"both empty: no advance", time.Time{}, time.Time{}, time.Time{}},
		{"open fetch empty constrains nothing", early, time.Time{}, early},
		{"PRD fetch empty means no advance at all", time.Time{}, late, time.Time{}},
		{"both present: the smaller wins", late, early, early},
		{"both present, PRD smaller", early, late, early},
		{"equal", early, early, early},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := advanceHWM(tc.prdMax, tc.openMax); !got.Equal(tc.want) {
				t.Fatalf("advanceHWM(%v, %v) = %v, want %v", tc.prdMax, tc.openMax, got, tc.want)
			}
		})
	}
}

// TestWithoutLabelMatchesExactly: the discard has to classify an issue the same way
// the forge's own label filter did, so it is exact and case-sensitive like every
// other label comparison in this package.
func TestWithoutLabelMatchesExactly(t *testing.T) {
	in := []forge.Issue{
		labelled(1, time.Unix(1, 0), prdLabel),
		labelled(2, time.Unix(1, 0), "bug"),
		labelled(3, time.Unix(1, 0)),
		labelled(4, time.Unix(1, 0), "prd"), // different case: a different label
		labelled(5, time.Unix(1, 0), "bug", prdLabel),
	}
	var got []int64
	for _, is := range withoutLabel(in, prdLabel) {
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
		issues:     []forge.Issue{labelled(1, time.Unix(100, 0), prdLabel)},
		openIssues: []forge.Issue{labelled(1, time.Unix(100, 0), prdLabel)},
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
