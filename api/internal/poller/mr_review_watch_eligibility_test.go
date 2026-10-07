package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Author eligibility in the MR review watcher (issue #2347). The multi-tick tests drive detect
// with a fake clock, an in-memory verdict cache and queue, and a scripted forge, and assert
// from the forge's own lookup record how many authors each tick attempted.

const (
	memberAuthor   = int64(11)
	outsiderAuthor = int64(22)
	botAuthor      = int64(136622811)
)

// ew is one eligibility-test rig: a store, a run recorder, a fake clock and a scripted forge.
type ew struct {
	t     *testing.T
	st    *mrwStore
	runs  *mrwRuns
	f     *mrwForge
	d     *MRReviewWatch
	clock time.Time
	set   mrwSettings
}

var ewBase = time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)

func newEW(t *testing.T) *ew {
	t.Helper()
	e := &ew{t: t, clock: ewBase, set: mrwSettings{enabled: true, capVal: 50}}
	e.st = &mrwStore{candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")}, clock: func() time.Time { return e.clock }}
	e.runs = &mrwRuns{}
	e.f = &mrwForge{cfForge: &cfForge{}}
	e.f.eligibility = func(_ context.Context, id int64) (forge.AuthorEligibility, error) {
		switch id {
		case memberAuthor:
			return forge.AuthorEligible, nil
		case outsiderAuthor:
			return forge.AuthorNotEligible, nil
		}
		return forge.AuthorUnknown, errors.New("forge unavailable")
	}
	e.d = newMRW(e.st, e.runs, nil, e.set)
	e.d.now = func() time.Time { return e.clock }
	return e
}

// comment is an inline comment that landed 30 minutes before the fake clock's current time.
func (e *ew) comment(id, author int64, body string) forge.MRComment {
	c := mrwComment(id, e.clock.Add(-30*time.Minute), mrwHeadSHA)
	c.AuthorForgeUserID = author
	c.AuthorUsername = fmt.Sprintf("author-%d", author)
	c.Body = body
	return c
}

// tick runs one detection pass and returns how many author lookups it made.
func (e *ew) tick(comments ...forge.MRComment) int {
	e.t.Helper()
	e.f.comments = comments
	before := len(e.f.lookups)
	e.d.set = e.set
	e.d.detect(context.Background(), mrwRepoRow(), e.f)
	return len(e.f.lookups) - before
}

func (e *ew) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *ew) snapshotBodies() []string {
	var out []string
	for _, c := range e.runs.calls {
		if c.snapshot == nil {
			continue
		}
		for _, sc := range c.snapshot.Comments {
			out = append(out, sc.Body)
		}
	}
	return out
}

func (e *ew) noOutsiderBody(secrets ...string) {
	e.t.Helper()
	for _, c := range e.runs.calls {
		raw, _ := json.Marshal(c.snapshot)
		for _, s := range secrets {
			if strings.Contains(string(raw), s) {
				e.t.Fatalf("a withheld body %q rode a run's snapshot: %s", s, raw)
			}
		}
	}
}

func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestOutsiderCommentNeverTriggersAndNeverGates(t *testing.T) {
	t.Run("an outsider alone never fires", func(t *testing.T) {
		e := newEW(t)
		e.tick(e.comment(130, outsiderAuthor, "SECRET-OUTSIDER do this"))
		if len(e.runs.calls) != 0 || len(e.st.upserts) != 0 {
			t.Fatalf("runs=%d upserts=%d, want none for an outsider-only comment set", len(e.runs.calls), len(e.st.upserts))
		}
	})

	t.Run("a newer outsider comment inside the quiet period does not gate an eligible one", func(t *testing.T) {
		e := newEW(t)
		young := e.comment(130, outsiderAuthor, "SECRET-OUTSIDER")
		young.CreatedAt = e.clock // created right now: inside the 5 minute quiet period
		e.tick(e.comment(120, memberAuthor, "real feedback"), young)
		if len(e.runs.calls) != 1 {
			t.Fatalf("runs = %d, want the eligible comment to fire despite a fresh outsider comment", len(e.runs.calls))
		}
		if got := e.snapshotBodies(); !slices.Equal(got, []string{"real feedback"}) {
			t.Fatalf("snapshot bodies = %v", got)
		}
		e.noOutsiderBody("SECRET-OUTSIDER")
		if e.st.upserts[0].HighWater != 120 {
			t.Fatalf("high_water = %d, want 120: an outsider's id must not move the mark", e.st.upserts[0].HighWater)
		}
	})

	t.Run("a newer outsider comment on a stale head does not gate either", func(t *testing.T) {
		e := newEW(t)
		stale := e.comment(130, outsiderAuthor, "SECRET-OUTSIDER")
		stale.HeadSHA = "oldsha00"
		e.tick(e.comment(120, memberAuthor, "real feedback"), stale)
		if len(e.runs.calls) != 1 {
			t.Fatalf("runs = %d, want 1", len(e.runs.calls))
		}
	})

	t.Run("a withheld and eligible mix advances the mark without re-triggering", func(t *testing.T) {
		e := newEW(t)
		comments := []forge.MRComment{e.comment(120, memberAuthor, "real feedback"), e.comment(130, outsiderAuthor, "SECRET-OUTSIDER")}
		e.tick(comments...)
		e.tick(comments...)
		e.tick(comments...)
		if len(e.runs.calls) != 1 {
			t.Fatalf("runs = %d, want the mix to fire exactly once", len(e.runs.calls))
		}
		if got := e.st.ledgers[mrwRef]; got.HighWater != 120 || got.AttemptCount != 1 {
			t.Fatalf("ledger = %+v, want high_water 120 after one cycle", got)
		}
		e.noOutsiderBody("SECRET-OUTSIDER")
	})
}

func TestNotEligibleVerdictCostsNoLookupsWithinTTL(t *testing.T) {
	e := newEW(t)
	c := e.comment(130, outsiderAuthor, "SECRET-OUTSIDER")
	if n := e.tick(c); n != 1 {
		t.Fatalf("first tick lookups = %d, want 1", n)
	}
	e.advance(5 * time.Hour)
	if n := e.tick(c); n != 0 {
		t.Fatalf("a not-eligible verdict within the TTL cost %d lookups, want 0", n)
	}
	e.advance(2 * time.Hour)
	if n := e.tick(c); n != 1 {
		t.Fatalf("after the TTL the author must be asked again, lookups = %d", n)
	}
}

func TestUnknownThenRecoveredTriggersOnce(t *testing.T) {
	logs := captureLogs(t, slog.LevelDebug)
	e := newEW(t)
	unknown := int64(77)
	c := e.comment(130, unknown, "feedback from a member whose access was unknown")

	e.tick(c)
	if len(e.runs.calls) != 0 {
		t.Fatalf("an unverifiable author fired a run")
	}
	var warned bool
	for _, rec := range logRecords(t, logs) {
		if rec["msg"] == "poller: mr-rework withheld: permission unknown" {
			warned = true
			if rec["reason"] != "permission_unknown" || rec["count"] != float64(1) || rec["ref"] != mrwRef || rec["attempted"] != float64(1) {
				t.Fatalf("withheld record = %v, want reason permission_unknown, count 1, attempted 1", rec)
			}
		}
	}
	if !warned {
		t.Fatalf("no recorded reason for the withheld comment; logs: %s", logs.String())
	}

	// The forge recovers: the author is now a member.
	e.f.eligibility = func(context.Context, int64) (forge.AuthorEligibility, error) { return forge.AuthorEligible, nil }
	e.tick(c)
	e.tick(c)
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want exactly one after recovery (no re-fire on the third tick)", len(e.runs.calls))
	}
}

func TestPermanentlyUnknownAuthorDoesNotWedgeTheMR(t *testing.T) {
	e := newEW(t)
	stuck := e.comment(5, 77, "from an author the forge never answers for")
	real := e.comment(9, memberAuthor, "real feedback")
	e.tick(stuck, real)
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want the eligible comment to fire although another author is unknown", len(e.runs.calls))
	}
	if got := e.st.ledgers[mrwRef]; got.HighWater != 9 || !slices.Equal(got.PendingUnknownIds, []int64{5}) {
		t.Fatalf("ledger = %+v, want high_water 9 with the unknown comment 5 pending", got)
	}
	// Nothing re-fires while the author stays unknown, and a later eligible comment still fires.
	e.tick(stuck, real)
	e.tick(stuck, real)
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d after quiet ticks, want 1", len(e.runs.calls))
	}
	later := e.comment(15, memberAuthor, "more feedback")
	e.tick(stuck, real, later)
	if len(e.runs.calls) != 2 {
		t.Fatalf("runs = %d, want a later eligible comment to fire", len(e.runs.calls))
	}
	// The author finally resolves eligible: the pending comment fires once and is consumed.
	e.f.eligibility = func(context.Context, int64) (forge.AuthorEligibility, error) { return forge.AuthorEligible, nil }
	e.tick(stuck, real, later)
	e.tick(stuck, real, later)
	if len(e.runs.calls) != 3 {
		t.Fatalf("runs = %d, want the pending comment to fire exactly once when its author resolves", len(e.runs.calls))
	}
	if got := e.st.ledgers[mrwRef].PendingUnknownIds; len(got) != 0 {
		t.Fatalf("pending = %v, want it consumed", got)
	}
	if got := e.snapshotBodies(); !slices.Contains(got, "from an author the forge never answers for") {
		t.Fatalf("the recovered author's comment never reached a snapshot: %v", got)
	}
}

// failing authors are unknown forever; ids start well above the member and outsider.
func failingAuthors(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(10000 + i)
	}
	return out
}

func TestFlood200PlusErroringCandidatesAheadOfOneEligibleAuthor(t *testing.T) {
	e := newEW(t)
	failing := failingAuthors(250)
	var comments []forge.MRComment
	for i, a := range failing {
		c := e.comment(int64(100+i), a, "SECRET-UNVERIFIED-"+fmt.Sprint(a))
		c.CreatedAt = e.clock.Add(-time.Hour + time.Duration(i)*time.Second) // oldest first
		comments = append(comments, c)
	}
	eligible := e.comment(1000, memberAuthor, "the feedback that matters")
	eligible.CreatedAt = e.clock.Add(-30 * time.Minute)
	comments = append(comments, eligible)

	// Σ of attempts on authors ahead of the eligible one, tick by tick, from the forge's own record.
	ahead := len(failing)
	var attemptedTotal int
	for tick := 1; tick <= 6 && len(e.runs.calls) == 0; tick++ {
		before := len(e.f.lookups)
		e.tick(comments...)
		for _, id := range e.f.lookups[before:] {
			if id != memberAuthor {
				attemptedTotal++
			}
		}
		// The eligible author is reached on the first tick whose attempts begin after the others ahead of it
		// have all been attempted once.
		wantFired := attemptedTotal >= ahead && slices.Contains(e.f.lookups[before:], memberAuthor)
		if got := len(e.runs.calls) == 1; got != wantFired {
			t.Fatalf("tick %d: fired=%t, want %t (attempted %d of %d ahead)", tick, got, wantFired, attemptedTotal, ahead)
		}
		if n := len(e.f.lookups) - before; n > 200 {
			t.Fatalf("tick %d made %d lookups, want at most the 200 distinct-author budget", tick, n)
		}
	}
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want exactly one: the eligible author starved behind the flood", len(e.runs.calls))
	}
	e.noOutsiderBody("SECRET-UNVERIFIED")
	if got := e.snapshotBodies(); !slices.Equal(got, []string{"the feedback that matters"}) {
		t.Fatalf("snapshot bodies = %d entries, want only the eligible comment", len(got))
	}
	// And it must not fire again on the same comments.
	e.tick(comments...)
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d after another tick, want no re-fire", len(e.runs.calls))
	}

	// The OLDEST pending author (comment 100) now resolves eligible: its id must still be pending
	// after the flood, and it fires on a later tick (the 200-lookup budget spans a few ticks).
	oldest := failing[0]
	e.f.eligibility = func(_ context.Context, id int64) (forge.AuthorEligibility, error) {
		if id == oldest || id == memberAuthor {
			return forge.AuthorEligible, nil
		}
		return forge.AuthorUnknown, errors.New("forge unavailable")
	}
	for tick := 1; tick <= 6 && len(e.runs.calls) < 2; tick++ {
		e.tick(comments...)
	}
	if len(e.runs.calls) != 2 {
		t.Fatalf("runs = %d, want the older pending author's comment to fire once it resolves eligible", len(e.runs.calls))
	}
	if got := e.snapshotBodies(); !slices.Contains(got, "SECRET-UNVERIFIED-"+fmt.Sprint(oldest)) {
		t.Fatalf("the older author's comment never reached a snapshot: %v", got)
	}
}

func TestAHangingLookupFiresOnTheFirstTick(t *testing.T) {
	e := newEW(t)
	e.d.lookupTimeout = 30 * time.Millisecond
	hanger := int64(88)
	e.f.eligibility = func(ctx context.Context, id int64) (forge.AuthorEligibility, error) {
		switch id {
		case memberAuthor:
			return forge.AuthorEligible, nil
		case hanger:
			<-ctx.Done() // blocks until the per-lookup timeout cancels it
			return forge.AuthorUnknown, ctx.Err()
		}
		return forge.AuthorUnknown, errors.New("forge unavailable")
	}
	hung := e.comment(100, hanger, "SECRET-HUNG-AUTHOR")
	hung.CreatedAt = e.clock.Add(-time.Hour)
	start := time.Now()
	e.tick(hung, e.comment(120, memberAuthor, "real feedback"))
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("tick took %s: the hanging lookup was not bounded by its own timeout", took)
	}
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want the eligible author to fire on the first tick", len(e.runs.calls))
	}
	e.noOutsiderBody("SECRET-HUNG-AUTHOR")
}

func TestEligibleAuthorIsRetriedBeforeBrandNewArrivals(t *testing.T) {
	e := newEW(t)
	e.d.maxLookups = 3
	// The eligible author's first lookup fails (a blip); it works from the second tick on.
	var blip = true
	e.f.eligibility = func(_ context.Context, id int64) (forge.AuthorEligibility, error) {
		if id == memberAuthor {
			if blip {
				return forge.AuthorUnknown, errors.New("blip")
			}
			return forge.AuthorEligible, nil
		}
		return forge.AuthorUnknown, errors.New("forge unavailable")
	}
	old := func(i int, author int64, body string) forge.MRComment {
		c := e.comment(int64(100+i), author, body)
		c.CreatedAt = e.clock.Add(-time.Hour + time.Duration(i)*time.Second)
		return c
	}
	comments := []forge.MRComment{old(0, memberAuthor, "real feedback"), old(1, 201, "a"), old(2, 202, "b"), old(3, 203, "c")}
	e.tick(comments...) // attempts the member, 201, 202 (cap 3): all unknown; 203 not reached
	if len(e.runs.calls) != 0 {
		t.Fatal("fired on the tick the eligible author's lookup failed")
	}
	blip = false
	// Brand-new unverifiable authors arrive every tick; they must queue behind everyone already waiting.
	var arrivals []int64
	for tick := 2; tick <= 6 && len(e.runs.calls) == 0; tick++ {
		a := int64(300 + tick)
		arrivals = append(arrivals, a)
		comments = append(comments, old(10+tick, a, "newcomer"))
		before := len(e.f.lookups)
		e.tick(comments...)
		order := e.f.lookups[before:]
		if mi := slices.Index(order, memberAuthor); mi >= 0 {
			for _, id := range order[:mi] {
				if slices.Contains(arrivals, id) {
					t.Fatalf("tick %d: newcomer %d was attempted before the waiting eligible author: %v", tick, id, order)
				}
			}
		}
	}
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want the eligible author to fire once within the bound", len(e.runs.calls))
	}
}

func TestSharedEvidenceTimeoutThenAnswerFiresOnceThenNeverAgain(t *testing.T) {
	logs := captureLogs(t, slog.LevelWarn)
	e := newEW(t)
	e.d.lookupTimeout = 20 * time.Millisecond
	hang := true
	e.f.eligibility = func(ctx context.Context, id int64) (forge.AuthorEligibility, error) {
		if hang {
			<-ctx.Done()
			return forge.AuthorUnknown, ctx.Err()
		}
		return forge.AuthorEligible, nil
	}
	comments := []forge.MRComment{e.comment(100, 41, "one"), e.comment(101, 42, "two"), e.comment(102, 43, "three")}
	e.tick(comments...)
	if len(e.runs.calls) != 0 {
		t.Fatal("fired while every lookup timed out")
	}
	var reasoned bool
	for _, rec := range logRecords(t, logs) {
		if rec["msg"] == "poller: mr-rework withheld: permission unknown" && rec["reason"] == "permission_unknown" && rec["count"] == float64(3) {
			reasoned = true
		}
	}
	if !reasoned {
		t.Fatalf("no permission_unknown record for the all-withheld tick: %s", logs.String())
	}
	hang = false
	e.tick(comments...)
	e.tick(comments...)
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want exactly one on the answering tick and none after", len(e.runs.calls))
	}
}

func TestFairProgressBoundWithTwoLookupsPerTick(t *testing.T) {
	// R_0 = 2 authors ahead of the eligible one and A_t = 2: the eligible author is not attempted on
	// tick 1 and is attempted on tick 2, because the two attempted authors go to the back.
	e := newEW(t)
	e.d.maxLookups = 2
	mk := func(i int, author int64, body string, age time.Duration) forge.MRComment {
		c := e.comment(int64(100+i), author, body)
		c.CreatedAt = e.clock.Add(-age)
		return c
	}
	comments := []forge.MRComment{mk(1, 201, "y1", 3*time.Hour), mk(2, 202, "y2", 2*time.Hour), mk(3, memberAuthor, "x", time.Hour)}

	if n := e.tick(comments...); n != 2 {
		t.Fatalf("tick 1 lookups = %d, want 2", n)
	}
	if slices.Contains(e.f.lookups, memberAuthor) {
		t.Fatalf("the eligible author was attempted on tick 1: %v", e.f.lookups)
	}
	if len(e.runs.calls) != 0 {
		t.Fatal("fired before the eligible author was reached")
	}
	before := len(e.f.lookups)
	e.tick(comments...)
	if e.f.lookups[before] != memberAuthor {
		t.Fatalf("tick 2 attempted %v first, want the eligible author at the front", e.f.lookups[before:])
	}
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want the eligible author to fire on tick 2", len(e.runs.calls))
	}
}

func TestAnotherMRDoesNotReorderThisMRsQueue(t *testing.T) {
	e := newEW(t)
	refB := "agent/issue-8"
	candB := mrwCand("success")
	candB.Ref = pgtype.Text{String: refB, Valid: true}
	candB.MrIid = pgtype.Int8{Int64: 56, Valid: true}
	e.st.candidates = []store.ListMRReworkCandidatesRow{mrwCand("success"), candB}

	var comments []forge.MRComment
	for i, a := range []int64{401, 402, 403} {
		c := e.comment(int64(100+i), a, "x")
		c.CreatedAt = e.clock.Add(-time.Hour + time.Duration(i)*time.Second)
		comments = append(comments, c)
	}
	e.f.comments = comments
	e.d.maxLookups = 2
	e.d.detect(context.Background(), mrwRepoRow(), e.f)

	// Both MRs share the author ids but own separate queues: each saw the same two attempts.
	orderA := e.st.ras().Order(mrwRepoID, mrwRef)
	orderB := e.st.ras().Order(mrwRepoID, refB)
	// After one tick with a cap of 2: the unreached author keeps the front, the attempted two go behind it.
	want := []int64{403, 401, 402}
	if !slices.Equal(orderA, want) || !slices.Equal(orderB, want) {
		t.Fatalf("queues A=%v B=%v, want each %v independently", orderA, orderB, want)
	}
	// A tick for MR B alone leaves A's order untouched.
	e.st.candidates = []store.ListMRReworkCandidatesRow{candB}
	e.d.detect(context.Background(), mrwRepoRow(), e.f)
	if got := e.st.ras().Order(mrwRepoID, mrwRef); !slices.Equal(got, want) {
		t.Fatalf("MR A's queue changed while only MR B was processed: %v", got)
	}
	if got := e.st.ras().Order(mrwRepoID, refB); slices.Equal(got, want) {
		t.Fatalf("MR B's queue did not advance: %v", got)
	}
}

func TestTrustedBotSkipsLookupAndItsSummaryNeverTriggers(t *testing.T) {
	trusted := []settings.TrustedBot{{BaseURL: "https://github.com", ForgeUserID: botAuthor}}

	t.Run("an inline finding fires with no lookup", func(t *testing.T) {
		e := newEW(t)
		e.set.bots = trusted
		row := mrwRepoRow()
		row.BaseUrl = "https://github.com"
		finding := e.comment(120, botAuthor, "guard nil here")
		e.f.comments = []forge.MRComment{finding}
		e.d.set = e.set
		e.d.detect(context.Background(), row, e.f)
		if len(e.runs.calls) != 1 || len(e.f.lookups) != 0 {
			t.Fatalf("runs=%d lookups=%v, want the allowlisted bot's finding to fire with no lookup", len(e.runs.calls), e.f.lookups)
		}
	})

	t.Run("a summary note never triggers", func(t *testing.T) {
		e := newEW(t)
		e.set.bots = trusted
		row := mrwRepoRow()
		row.BaseUrl = "https://github.com"
		note := mrwSummaryComment(130, e.clock.Add(-30*time.Minute), "coderabbitai", "Please also rename X before merging.")
		note.AuthorForgeUserID = botAuthor
		e.f.comments = []forge.MRComment{note}
		e.d.set = e.set
		e.d.detect(context.Background(), row, e.f)
		if len(e.runs.calls) != 0 || len(e.st.upserts) != 0 {
			t.Fatalf("runs=%d upserts=%d, want an allowlisted bot's summary to be inert", len(e.runs.calls), len(e.st.upserts))
		}
	})
}

func TestFailClosedPaths(t *testing.T) {
	t.Run("an unreadable trusted bot list skips the repo", func(t *testing.T) {
		e := newEW(t)
		e.set.botsErr = errors.New("settings down")
		e.tick(e.comment(120, memberAuthor, "feedback"))
		if len(e.runs.calls) != 0 || len(e.f.lookups) != 0 || len(e.st.evicts) != 0 {
			t.Fatalf("runs=%d lookups=%v evicts=%d, want the whole repo tick skipped", len(e.runs.calls), e.f.lookups, len(e.st.evicts))
		}
	})
	t.Run("a failed queue admission fires nothing", func(t *testing.T) {
		e := newEW(t)
		e.st.ras().AdmitErr = errors.New("db down")
		e.tick(e.comment(120, memberAuthor, "feedback"))
		if len(e.runs.calls) != 0 || len(e.f.lookups) != 0 {
			t.Fatalf("runs=%d lookups=%v, want the tick to stop before any lookup", len(e.runs.calls), e.f.lookups)
		}
	})
	t.Run("an unreadable verdict cache fires nothing", func(t *testing.T) {
		e := newEW(t)
		e.st.ras().VerdictListErr = errors.New("db down")
		e.tick(e.comment(120, memberAuthor, "feedback"))
		if len(e.runs.calls) != 0 {
			t.Fatalf("runs=%d, want none", len(e.runs.calls))
		}
	})
}

func TestEvictionRunsNextToLedgerReconcile(t *testing.T) {
	e := newEW(t)
	e.st.candidates = nil
	repo := mrwRepoID
	ctx := context.Background()
	// A verdict from 7 hours ago and a queue row untouched for 8 days.
	if err := e.st.UpsertReviewAuthorVerdict(ctx, store.UpsertReviewAuthorVerdictParams{RepoID: repo, ForgeUserID: 5, NotEligibleAt: pgtype.Timestamptz{Time: e.clock.Add(-7 * time.Hour), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.ras().MutateReviewAuthorQueue(ctx, repo, "agent/issue-old", func(q workersvc.ReviewAuthorQueueOps) error {
		return q.AdmitReviewAuthors(ctx, store.AdmitReviewAuthorsParams{RepoID: repo, Ref: "agent/issue-old", ForgeUserIds: []int64{9}})
	}); err != nil {
		t.Fatal(err)
	}
	e.advance(8 * 24 * time.Hour)
	e.tick()
	if v := e.st.ras().Verdicts(repo); len(v) != 0 {
		t.Fatalf("expired verdicts survived the tick: %v", v)
	}
	if q := e.st.ras().Order(repo, "agent/issue-old"); len(q) != 0 {
		t.Fatalf("a week-old queue row survived the tick: %v", q)
	}
}

// A concurrent writer stores pending id 170 while this tick is listing the comments, so the
// list lacks it. GitHub and Forgejo number comment types from separate sequences, so the list
// also holds a LARGER id (190): "absent and below the largest fetched id" cannot tell a gone
// comment from one the list predates. The ledger is read before the listing, so 170 (absent from
// the row this tick planned against) is never mistaken for gone.
func TestStaleFetchLeavesConcurrentlyStoredPendingIDAlone(t *testing.T) {
	e := newEW(t)
	e.st.ledgers = map[string]store.MrReworkLedger{
		mrwRef: {RepoID: mrwRepoRow().ID, Ref: mrwRef, HighWater: 150},
	}
	e.f.onList = func() {
		cur := e.st.ledgers[mrwRef]
		cur.PendingUnknownIds = []int64{170}
		cur.HighWater = 165
		e.st.ledgers[mrwRef] = cur
	}
	old := e.comment(120, outsiderAuthor, "old outsider note")
	fresh := e.comment(190, memberAuthor, "eligible feedback")
	e.tick(old, fresh)
	if len(e.runs.calls) != 1 {
		t.Fatalf("runs = %d, want the eligible comment to fire", len(e.runs.calls))
	}
	if got := e.st.ledgers[mrwRef].PendingUnknownIds; !slices.Equal(got, []int64{170}) {
		t.Fatalf("pending = %v, want 170 kept: the listing never saw it", got)
	}
}
