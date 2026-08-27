package autoselect

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// now is a fixed instant. Every fixture below is expressed relative to it, because
// Classify takes `now` as a parameter precisely so no test has to reason about the
// wall clock.
var now = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute, InflightPenalty: 3}
}

func i16(v int16) *int16 { return &v }
func at(d time.Duration) *time.Time {
	t := now.Add(d)
	return &t
}

// pooled is a candidate that passes every gate: opted in, with a fresh reading of
// both windows. Each case below breaks exactly ONE thing, so a failure names the
// gate it broke rather than a whole fixture.
func pooled() Candidate {
	return Candidate{
		SecretID:      uuid.New(),
		Label:         "console-key",
		AutoEligible:  true,
		HasReading:    true,
		FiveHourPct:   i16(20),
		SevenDayPct:   i16(10),
		FiveResetsAt:  at(time.Hour),
		SevenResetsAt: at(72 * time.Hour),
		SyncedAt:      at(-time.Minute),
	}
}

func TestClassifyStatuses(t *testing.T) {
	notPooled := pooled()
	notPooled.AutoEligible = false

	noReading := pooled()
	noReading.HasReading = false
	noReading.SyncedAt = nil
	noReading.FiveHourPct, noReading.SevenDayPct = nil, nil

	// A row exists but only ONE window was measured. D12: a reading that cannot
	// produce a headroom number is excluded rather than defaulted to some
	// assumed-full value, which would make an unmeasured token look like the
	// emptiest one in the pool and win every ranking.
	halfMeasured := pooled()
	halfMeasured.FiveHourPct = nil

	stale := pooled()
	stale.SyncedAt = at(-time.Hour)

	below := pooled()
	below.FiveHourPct, below.SevenDayPct = i16(90), i16(50) // headroom 10 < 15

	for _, tc := range []struct {
		name     string
		c        Candidate
		want     Status
		measured bool
		headroom int
	}{
		{"opted in, fresh, above the floor", pooled(), StatusEligible, true, 80},
		{"not opted in", notPooled, StatusNotPooled, false, 0},
		{"no gauge row at all", noReading, StatusNoReading, false, 0},
		{"gauge row with a NULL window", halfMeasured, StatusUnmeasured, false, 0},
		{"reading aged out", stale, StatusStale, false, 0},
		{"fresh but below the floor", below, StatusBelowThreshold, true, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.c, testPolicy(), now)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q", got.Status, tc.want)
			}
			if got.Measured != tc.measured {
				t.Fatalf("measured = %v, want %v", got.Measured, tc.measured)
			}
			if got.Headroom != tc.headroom {
				t.Fatalf("headroom = %d, want %d", got.Headroom, tc.headroom)
			}
		})
	}
}

// TestClassifyGateOrder pins that the gates answer the MOST specific problem, not
// merely a true one. A candidate that is simultaneously un-pooled, unreadable and
// ancient could honestly be called stale; saying so would send a user to look at
// their poller when the actual answer is "you never opted this token in".
func TestClassifyGateOrder(t *testing.T) {
	broken := Candidate{AutoEligible: false, HasReading: false, SyncedAt: at(-99 * time.Hour)}
	if got := Classify(broken, testPolicy(), now).Status; got != StatusNotPooled {
		t.Fatalf("status = %q, want %q — the opt-in gate must answer first", got, StatusNotPooled)
	}
	broken.AutoEligible = true
	if got := Classify(broken, testPolicy(), now).Status; got != StatusNoReading {
		t.Fatalf("status = %q, want %q — a token with no row cannot be 'stale'", got, StatusNoReading)
	}
}

// TestClassifyBindingWindow: headroom is the min of the two windows, because they
// are a conjunction. A token at 10% of its 5-hour allowance and 98% of its 7-day one
// has 2 points of usable capacity, not 46 — and an implementation that averaged, or
// read only the near-term window, would rank it as one of the emptiest credentials
// the user holds.
func TestClassifyBindingWindow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		five, seven int16
		want        int
	}{
		{"seven-day binds", 10, 98, 2},
		{"five-hour binds", 98, 10, 2},
		{"equal", 40, 40, 60},
		{"empty token", 0, 0, 100},
		{"exhausted token", 100, 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := pooled()
			c.FiveHourPct, c.SevenDayPct = i16(tc.five), i16(tc.seven)
			got := Classify(c, Policy{MinHeadroom: 0, MaxStaleness: time.Hour}, now)
			if got.Headroom != tc.want {
				t.Fatalf("headroom = %d, want %d", got.Headroom, tc.want)
			}
		})
	}
}

// TestClassifyThresholdIsInclusive: `>= MinHeadroom` is eligible, one point below is
// not. Pinned because off-by-one at a threshold is invisible in every fixture that
// does not sit exactly on it.
func TestClassifyThresholdIsInclusive(t *testing.T) {
	p := testPolicy() // MinHeadroom 15
	exact := pooled()
	exact.FiveHourPct, exact.SevenDayPct = i16(85), i16(0) // headroom exactly 15
	if got := Classify(exact, p, now); got.Status != StatusEligible {
		t.Fatalf("headroom == MinHeadroom classified %q, want eligible", got.Status)
	}
	oneUnder := pooled()
	oneUnder.FiveHourPct, oneUnder.SevenDayPct = i16(86), i16(0) // headroom 14
	if got := Classify(oneUnder, p, now); got.Status != StatusBelowThreshold {
		t.Fatalf("headroom == MinHeadroom-1 classified %q, want below_threshold", got.Status)
	}
}

// TestClassifyStalenessBoundary: strictly OLDER than MaxStaleness is stale; exactly
// at the boundary is not. Same reason as the threshold above.
func TestClassifyStalenessBoundary(t *testing.T) {
	p := testPolicy() // MaxStaleness 15m
	exact := pooled()
	exact.SyncedAt = at(-15 * time.Minute)
	if got := Classify(exact, p, now); got.Status != StatusEligible {
		t.Fatalf("a reading exactly MaxStaleness old classified %q, want eligible", got.Status)
	}
	older := pooled()
	older.SyncedAt = at(-15*time.Minute - time.Nanosecond)
	if got := Classify(older, p, now); got.Status != StatusStale {
		t.Fatalf("a reading older than MaxStaleness classified %q, want stale", got.Status)
	}
}

// TestClassifyPollerDisabled is R2 as a first-class case, not an accident of
// arithmetic. UZI_AUTOSELECT_MAX_STALENESS defaults to 3× the poll interval, so
// disabling the poller (UZI_USAGE_POLL_INTERVAL=0, which the e2e overlay sets)
// makes it 0 — and with nothing refreshing the gauge, EVERY token must classify
// stale so auto degrades to the worker's non-auto binding. A reading from one
// nanosecond ago must not survive it.
func TestClassifyPollerDisabled(t *testing.T) {
	c := pooled()
	c.SyncedAt = at(-time.Nanosecond)
	for _, staleness := range []time.Duration{0, -time.Second} {
		p := testPolicy()
		p.MaxStaleness = staleness
		if got := Classify(c, p, now); got.Status != StatusStale {
			t.Fatalf("MaxStaleness=%v classified a one-nanosecond-old reading as %q, want stale", staleness, got.Status)
		}
	}
}

// TestClassifyIsTotal: any input yields an Eligibility, including the zero value.
// Classify is called per row on a user-facing list, so a panic here is a 500 on the
// settings page.
func TestClassifyIsTotal(t *testing.T) {
	if got := Classify(Candidate{}, Policy{}, time.Time{}); got.Status != StatusNotPooled {
		t.Fatalf("zero candidate classified %q", got.Status)
	}
	// Pooled, claims a reading, but every column is nil — the shape a query that
	// projected HasReading independently of the columns could produce.
	lying := Candidate{AutoEligible: true, HasReading: true}
	if got := Classify(lying, testPolicy(), now); got.Status != StatusNoReading {
		t.Fatalf("HasReading with a nil SyncedAt classified %q, want no_reading", got.Status)
	}
}

// TestClassifyIgnoresInFlight is the contract the M2 ↔ M4 differential rests on:
// InFlight is the ranker's herd-control bias and is NOT part of the gate, so the
// settings list — whose query legitimately yields 0 — and the ranking query must
// classify the same token identically. If this ever fails, the two surfaces can
// disagree about eligibility for a reason neither of them displays.
func TestClassifyIgnoresInFlight(t *testing.T) {
	base := pooled()
	busy := pooled()
	busy.SecretID = base.SecretID
	busy.InFlight = 99
	if Classify(base, testPolicy(), now) != Classify(busy, testPolicy(), now) {
		t.Fatal("InFlight changed the classification; it must affect ranking only, never the gate")
	}
}

// TestPackageImportsStayPure is the design constraint stated as a test, because it
// is the one a later refactor is most likely to erode and the erosion is silent: the
// first `import "context"` or `import ".../store"` here turns every test above into
// an integration test and moves the ranking behind a database. Purity is what lets
// M6 assert the whole ranking from hand-written fixtures.
//
// Asserted over the package's own source rather than by convention, so it fails at
// the moment the import is added rather than whenever someone notices.
func TestPackageImportsStayPure(t *testing.T) {
	allowed := map[string]bool{`"time"`: true, `"github.com/google/uuid"`: true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, imp := range f.Imports {
			if !allowed[imp.Path.Value] {
				path, _ := strconv.Unquote(imp.Path.Value)
				t.Errorf("%s imports %q. This package is PURE by design — no context, no store, "+
					"no pgtype, no clock — which is what keeps the ranking unit-testable without a "+
					"database. If the new dependency is genuinely needed, that is a design change: "+
					"move the impure part into the caller (workersvc) instead.", name, path)
			}
		}
	}
}
