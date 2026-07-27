package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// The park computation is PURE, so every case below is an exact-value assertion
// rather than a range check: `now` and the jitter are both parameters.
//
// parkNow is deliberately the same instant as auto_select_test.go's autoNow, so a
// candRow built there means the same thing here.
var parkNow = autoNow

// ms is a worker-reported reset, in the epoch MILLISECONDS the wire carries.
func ms(d time.Duration) *int64 {
	v := parkNow.Add(d).UnixMilli()
	return &v
}

func msRaw(v int64) *int64 { return &v }

func strPtr(s string) *string { return &s }

// parkIn is a park that WILL happen: opted in, budget untouched, a plausible reset
// two hours out, and no pool information at all. Each case below breaks exactly one
// thing, so a failure names what it broke.
func parkIn() limitParkInput {
	return limitParkInput{
		WaitOnLimit:     true,
		LimitWaitCount:  0,
		DeadSecretID:    uuid.New(),
		ReportedResetMs: ms(2 * time.Hour),
		ReportedType:    strPtr("five_hour"),
		Policy:          autoselect.Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute},
		MaxWaits:        5,
		MaxPark:         8 * 24 * time.Hour,
		Jitter:          90 * time.Second,
		Now:             parkNow,
	}
}

// --- the base, and where it comes from -------------------------------------------

func TestDecideLimitParkUsesTheReportedReset(t *testing.T) {
	d := decideLimitPark(parkIn())
	if !d.Park {
		t.Fatalf("did not park: %q", d.Reason)
	}
	want := parkNow.Add(2*time.Hour + 90*time.Second)
	if !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want %v (reported reset + jitter)", d.RetryNotBefore, want)
	}
	if d.LimitResetsAt == nil || !d.LimitResetsAt.Equal(parkNow.Add(2*time.Hour)) {
		t.Fatalf("limit_resets_at = %v, want the worker's report preserved verbatim", d.LimitResetsAt)
	}
}

// TestDecideLimitParkFallsBackWhenNothingKnowsTheReset covers the classifier's
// PRIMARY signal: terminal_reason names the cause outright and carries no timestamp,
// so a limit death with no reset is ordinary rather than malformed.
func TestDecideLimitParkFallsBackWhenNothingKnowsTheReset(t *testing.T) {
	in := parkIn()
	in.ReportedResetMs = nil
	d := decideLimitPark(in)
	if !d.Park {
		t.Fatalf("a limit death with no reported reset must still park, got %q", d.Reason)
	}
	want := parkNow.Add(limitParkFallback + 90*time.Second)
	if !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want %v (fallback + jitter)", d.RetryNotBefore, want)
	}
	if d.LimitResetsAt != nil {
		t.Fatalf("limit_resets_at = %v, want NULL — the server must not invent a reset it "+
			"was never told", d.LimitResetsAt)
	}
}

// 🔴 TestDecideLimitParkRejectsImplausibleResets is the park-loop guard. Every value
// here would, unchecked, produce retry_not_before <= now (or an absurd future), and
// the first kind spins: the next promotion pass requeues the run straight back into
// the exhausted window, it re-parks, and the cycle burns RUN_LIMIT_MAX_WAITS in
// seconds.
func TestDecideLimitParkRejectsImplausibleResets(t *testing.T) {
	for name, raw := range map[string]*int64{
		"zero":                  msRaw(0),
		"negative":              msRaw(-1),
		"far negative":          msRaw(-1_700_000_000_000),
		"epoch 1970":            msRaw(1),
		"seconds read as ms":    msRaw(parkNow.Add(2 * time.Hour).Unix()), // ~1.7e9, three orders too small
		"year 50000 (ms as µs)": msRaw(parkNow.Add(2*time.Hour).UnixMilli() * 1000),
		"int64 max":             msRaw(1<<63 - 1),
	} {
		in := parkIn()
		in.ReportedResetMs = raw
		d := decideLimitPark(in)
		if !d.Park {
			t.Fatalf("%s: refused to park (%q); an implausible timestamp must be DROPPED, "+
				"not turned into a failed run", name, d.Reason)
		}
		if d.LimitResetsAt != nil {
			t.Fatalf("%s: limit_resets_at = %v, want NULL", name, d.LimitResetsAt)
		}
		want := parkNow.Add(limitParkFallback + 90*time.Second)
		if !d.RetryNotBefore.Equal(want) {
			t.Fatalf("%s: retry_not_before = %v, want the fallback %v. A stamp at or before "+
				"`now` is the park LOOP: the next promotion pass requeues the run into the "+
				"same exhausted window and the budget burns in seconds", name, d.RetryNotBefore, want)
		}
	}
}

// TestDecideLimitParkFloorsAPastReset: a reset that has ALREADY passed is not an
// error — it is a window that reopened while the report was in flight. The right
// answer is "retry shortly", never "fail the run", and never a stamp in the past.
func TestDecideLimitParkFloorsAPastReset(t *testing.T) {
	in := parkIn()
	in.ReportedResetMs = ms(-30 * time.Minute) // plausible epoch, but behind us
	d := decideLimitPark(in)
	if !d.Park {
		t.Fatalf("a past reset must still park: %q", d.Reason)
	}
	if !d.RetryNotBefore.After(parkNow) {
		t.Fatalf("retry_not_before = %v, which is not after now (%v)", d.RetryNotBefore, parkNow)
	}
	if want := parkNow.Add(90 * time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want the floor %v (now + jitter)", d.RetryNotBefore, want)
	}
}

// --- Decision 4: the cross-check against the dead credential's own gauge ---------

func TestDecideLimitParkCrossChecksTheGauge(t *testing.T) {
	dead := uuid.New()
	// The gauge says the five-hour window reopens LATER than the worker reported.
	// max() wins: both describe the same window, and promoting before the later of
	// them guarantees a re-park.
	gaugeReset := parkNow.Add(5 * time.Hour)
	c := autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute))
	c.FiveResetsAt = &gaugeReset

	in := parkIn()
	in.DeadSecretID = dead
	in.Candidates = []autoselect.Candidate{c}
	d := decideLimitPark(in)
	if !d.Park {
		t.Fatalf("did not park: %q", d.Reason)
	}
	if want := gaugeReset.Add(90 * time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want %v — max(worker 2h, gauge 5h) + jitter",
			d.RetryNotBefore, want)
	}
}

func TestDecideLimitParkCrossCheckNeverShortensTheWait(t *testing.T) {
	dead := uuid.New()
	early := parkNow.Add(30 * time.Minute) // gauge says sooner than the worker did
	c := autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute))
	c.FiveResetsAt = &early

	in := parkIn()
	in.DeadSecretID = dead
	in.Candidates = []autoselect.Candidate{c}
	d := decideLimitPark(in)
	if want := parkNow.Add(2*time.Hour + 90*time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want the WORKER's later reset %v. The cross-check "+
			"is max(), not the gauge winning: promoting before the later of two statements "+
			"about the same window guarantees a re-park", d.RetryNotBefore, want)
	}
}

// 🔴 TestDecideLimitParkCrossCheckMapsTheWindow is the clause that stops `max` from
// over-delaying by days. A 5-hour rejection must never be compared against the
// 7-day rollover, and vice versa.
func TestDecideLimitParkCrossCheckMapsTheWindow(t *testing.T) {
	dead := uuid.New()
	five, seven := parkNow.Add(3*time.Hour), parkNow.Add(6*24*time.Hour)

	base := func() autoselect.Candidate {
		c := autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute))
		c.FiveResetsAt, c.SevenResetsAt = &five, &seven
		return c
	}

	for _, tc := range []struct {
		rateLimitType string
		want          time.Time
		why           string
	}{
		{"five_hour", five, "the five-hour column"},
		{"seven_day", seven, "the seven-day column"},
		{"seven_day_opus", seven, "an opus seven-day window is still a seven-day window"},
		{"seven_day_sonnet", seven, "a sonnet seven-day window is still a seven-day window"},
		{"seven_day_overage_included", seven, "an overage-inclusive seven-day window is still seven-day"},
		// No gauge column names these, so there is NO cross-check and the worker's
		// reported reset stands. Falling back to some other window would be a
		// confidently wrong answer, which is worse than none.
		{"overage", parkNow.Add(2 * time.Hour), "overage has no gauge column"},
		{"unknown", parkNow.Add(2 * time.Hour), "unknown names no window at all"},
	} {
		in := parkIn()
		in.DeadSecretID = dead
		in.Candidates = []autoselect.Candidate{base()}
		in.ReportedType = strPtr(tc.rateLimitType)
		d := decideLimitPark(in)
		if want := tc.want.Add(90 * time.Second); !d.RetryNotBefore.Equal(want) {
			t.Fatalf("%s: retry_not_before = %v, want %v (%s)",
				tc.rateLimitType, d.RetryNotBefore, want, tc.why)
		}
	}
}

func TestDecideLimitParkIgnoresAnUnmeasuredGauge(t *testing.T) {
	dead := uuid.New()
	late := parkNow.Add(5 * time.Hour)
	c := autoselectrowCandidate(dead, 90, parkNow.Add(-time.Hour)) // STALE: MaxStaleness is 15m
	c.FiveResetsAt = &late

	in := parkIn()
	in.DeadSecretID = dead
	in.Candidates = []autoselect.Candidate{c}
	d := decideLimitPark(in)
	if want := parkNow.Add(2*time.Hour + 90*time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want the worker's reset %v. A gauge reading that is "+
			"not Measured (stale here) carries no fresh statement about the window and must "+
			"not push the stamp out", d.RetryNotBefore, want)
	}
}

// --- Decision 6e: the pool leg, at the service's own boundary --------------------

func TestDecideLimitParkPromotesEarlyForAPooledAlternative(t *testing.T) {
	dead, alt := uuid.New(), uuid.New()
	in := parkIn()
	in.DeadSecretID = dead
	in.Candidates = []autoselect.Candidate{
		autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute)),
		autoselectrowCandidate(alt, 90, parkNow.Add(-time.Minute)), // eligible ⇒ now
	}
	d := decideLimitPark(in)
	if !d.Park {
		t.Fatalf("did not park: %q", d.Reason)
	}
	if want := parkNow.Add(90 * time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want %v. A second credential with headroom means "+
			"the run can resume on the next tick, so the pool leg must LOWER the stamp "+
			"below the dead credential's 2h reset", d.RetryNotBefore, want)
	}
}

func TestDecideLimitParkPoolLegNeverLengthensTheWait(t *testing.T) {
	dead, alt := uuid.New(), uuid.New()
	// The alternative is below threshold with a reset far beyond the dead credential's.
	altCand := autoselectrowCandidate(alt, 2, parkNow.Add(-time.Minute))
	far := parkNow.Add(70 * time.Hour)
	altCand.FiveResetsAt = &far

	in := parkIn()
	in.DeadSecretID = dead
	in.Candidates = []autoselect.Candidate{
		autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute)),
		altCand,
	}
	d := decideLimitPark(in)
	if want := parkNow.Add(2*time.Hour + 90*time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want %v. NextAvailable is a FLOOR on when something "+
			"is spendable; a distant alternative must never make the wait LONGER than the "+
			"dead credential's own reset", d.RetryNotBefore, want)
	}
}

func TestDecideLimitParkSingleTokenUserDoesNotRegress(t *testing.T) {
	dead := uuid.New()
	in := parkIn()
	in.DeadSecretID = dead
	in.Candidates = []autoselect.Candidate{autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute))}
	d := decideLimitPark(in)
	if want := parkNow.Add(2*time.Hour + 90*time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want the dead credential's own reset %v — the "+
			"single-token case must behave exactly as it did before Decision 6e",
			d.RetryNotBefore, want)
	}
}

// TestDecideLimitParkWithNoRecordedCredentialSkipsBothLegs: a pre-PRD-#111 run, or
// one whose credential recording failed. Without the exclusion id, leg 1 would fire
// on the dead credential's OWN stale-but-eligible reading and promote the run
// instantly into the window it just exhausted.
func TestDecideLimitParkWithNoRecordedCredentialSkipsBothLegs(t *testing.T) {
	gaugeLate := parkNow.Add(5 * time.Hour)
	c := autoselectrowCandidate(uuid.New(), 90, parkNow.Add(-time.Minute))
	c.FiveResetsAt = &gaugeLate

	in := parkIn()
	in.DeadSecretID = uuid.Nil
	in.Candidates = []autoselect.Candidate{c}
	d := decideLimitPark(in)
	if want := parkNow.Add(2*time.Hour + 90*time.Second); !d.RetryNotBefore.Equal(want) {
		t.Fatalf("retry_not_before = %v, want the worker's reset %v. With no credential "+
			"recorded there is nothing to exclude, so BOTH pool legs and the cross-check "+
			"must be skipped", d.RetryNotBefore, want)
	}
}

// --- the three failures, which are 200s carrying `failed`, never refusals ---------

func TestDecideLimitParkOptOutFailsWithAComposedReason(t *testing.T) {
	in := parkIn()
	in.WaitOnLimit = false
	d := decideLimitPark(in)
	if d.Park {
		t.Fatal("parked a run whose owner opted out")
	}
	if !strings.Contains(d.Reason, "(five_hour)") || !strings.Contains(d.Reason, "resets at") {
		t.Fatalf("reason = %q, want the server-composed sentence naming the window and the "+
			"reset", d.Reason)
	}
}

func TestDecideLimitParkBudgetExhausted(t *testing.T) {
	in := parkIn()
	in.LimitWaitCount = 5 // == MaxWaits
	d := decideLimitPark(in)
	if d.Park {
		t.Fatal("parked past RUN_LIMIT_MAX_WAITS")
	}
	if !strings.Contains(d.Reason, "budget exhausted") {
		t.Fatalf("reason = %q, want it to name the exhausted retry budget", d.Reason)
	}

	// One below the cap still parks — the boundary is >=, so an off-by-one here either
	// wastes the last retry or grants one too many.
	in.LimitWaitCount = 4
	if d := decideLimitPark(in); !d.Park {
		t.Fatalf("refused to park at count=4 with MaxWaits=5: %q", d.Reason)
	}
}

// TestDecideLimitParkMaxWaitsZeroNeverParks is the operator's off switch: it must
// land on the FIRST limit, reproducing today's fail-immediately behaviour with a
// better reason rather than parking once and then stopping.
func TestDecideLimitParkMaxWaitsZeroNeverParks(t *testing.T) {
	in := parkIn()
	in.MaxWaits = 0
	if d := decideLimitPark(in); d.Park {
		t.Fatal("RUN_LIMIT_MAX_WAITS=0 parked a run; it is the never-park switch")
	}
}

func TestDecideLimitParkCeiling(t *testing.T) {
	in := parkIn()
	in.ReportedResetMs = ms(9 * 24 * time.Hour) // beyond the 8d MaxPark
	d := decideLimitPark(in)
	if d.Park {
		t.Fatal("parked further out than RUN_LIMIT_MAX_PARK; waiting is not free — a parked " +
			"run holds its issue's one-active lock and its worker's disk")
	}
	if !strings.Contains(d.Reason, "maximum park") {
		t.Fatalf("reason = %q, want it to name the ceiling", d.Reason)
	}
}

// --- the composed sentence -------------------------------------------------------

func TestLimitFailureReasonOmitsWhatItDoesNotKnow(t *testing.T) {
	at := time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct{ got, want string }{
		"both": {
			limitFailureReason(strPtr("five_hour"), &at, ""),
			"Anthropic usage limit (five_hour) reached; resets at 2026-07-27T16:00:00Z",
		},
		"type only":  {limitFailureReason(strPtr("seven_day"), nil, ""), "Anthropic usage limit (seven_day) reached"},
		"reset only": {limitFailureReason(nil, &at, ""), "Anthropic usage limit reached; resets at 2026-07-27T16:00:00Z"},
		"neither":    {limitFailureReason(nil, nil, ""), "Anthropic usage limit reached"},
		"detail": {
			limitFailureReason(strPtr("overage"), nil, "budget spent"),
			"Anthropic usage limit (overage) reached; budget spent",
		},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s: %q, want %q — each part is OMITTED rather than defaulted when it is "+
				"unknown, so the sentence never claims a fact the server does not have",
				name, tc.got, tc.want)
		}
	}
}

// 🔴 TestDecideLimitParkNeverLeaksWorkerText is the criterion the design brief says
// would otherwise be false: the enum lives server-side, so no worker-controlled byte
// reaches a rendered failure reason.
func TestDecideLimitParkNeverLeaksWorkerText(t *testing.T) {
	for _, hostile := range []string{
		"five_hour<script>alert(1)</script>",
		"'); DROP TABLE runs; --",
		strings.Repeat("A", 5009),
		"five_hour\x00\r\n",
	} {
		in := parkIn()
		in.WaitOnLimit = false // take the failure path, which is what renders the string
		in.ReportedType = &hostile
		d := decideLimitPark(in)
		if strings.Contains(d.Reason, "script") || strings.Contains(d.Reason, "DROP") ||
			strings.Contains(d.Reason, "AAA") || strings.ContainsAny(d.Reason, "\x00\r\n") {
			t.Fatalf("worker text reached the composed reason: %q", d.Reason)
		}
		if !strings.Contains(d.Reason, "(unknown)") {
			t.Fatalf("reason = %q, want the coerced (unknown)", d.Reason)
		}
	}
}

// --- the service seam ------------------------------------------------------------

// autoselectrowCandidate builds a fresh, pooled, measurable candidate at a given
// headroom. Local rather than reused from autoselect's tests, which are in another
// package.
//
// 🔴 BOTH RESETS ARE LEFT NIL, deliberately, and this DIVERGES from candRow's
// convention of deriving the reset from the headroom. That convention is right for
// the ranker's tests, where the reset is only a tie-break, and it is a trap here: it
// made the DEAD credential's gauge claim a five-hour window reopening in ninety
// hours, so max(worker, gauge) legitimately answered 90h and two cases failed with
// the cross-check working exactly as designed. Every case that wants a gauge reading
// sets the reset it means; every case that does not gets no cross-check and no pool
// contribution, which is the "each fixture breaks exactly one thing" property.
func autoselectrowCandidate(id uuid.UUID, headroom int, syncedAt time.Time) autoselect.Candidate {
	five := int16(100 - headroom)
	seven := int16(0)
	return autoselect.Candidate{
		SecretID: id, Label: "tok", AutoEligible: true, HasReading: true,
		FiveHourPct: &five, SevenDayPct: &seven, SyncedAt: &syncedAt,
	}
}

func limitParkFixture(t *testing.T, run store.Run) (*fakeStore, *Service, store.Worker) {
	t.Helper()
	fs := &fakeStore{runOwned: run}
	p := autoParams()
	p.RunLimitMaxWaits = 5
	p.RunLimitMaxPark = 8 * 24 * time.Hour
	svc := New(fs, newBox(t), p)
	svc.now = func() time.Time { return parkNow }
	return fs, svc, store.Worker{ID: uuid.New(), UserID: run.UserID}
}

func runningRun(waitOnLimit bool) store.Run {
	return store.Run{
		ID: uuid.New(), UserID: uuid.New(), Kind: "issue", Status: "running",
		IssueTitle: "t", IssueDescription: "d", WaitOnLimit: waitOnLimit,
	}
}

// TestSetStateLimitWaitParks walks the whole arm: the report is validated, the
// candidates are fetched, the stamp is computed and the park query is called with it.
func TestSetStateLimitWaitParks(t *testing.T) {
	run := runningRun(true)
	fs, svc, wkr := limitParkFixture(t, run)
	fs.setLimitWaitRows = 1

	reset := parkNow.Add(2 * time.Hour)
	rows := reset.UnixMilli()
	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "limit_wait", LimitResetsAt: &rows, RateLimitType: strPtr("five_hour"),
	})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if !applied {
		t.Fatal("applied = false for a park the store accepted")
	}
	if fs.setLimitWait == nil {
		t.Fatal("SetRunLimitWait was never called")
	}
	got := *fs.setLimitWait
	if !got.RetryNotBefore.Valid {
		t.Fatal("retry_not_before was not stamped")
	}
	if lo, hi := reset.Add(limitParkJitterMin), reset.Add(limitParkJitterMax); got.RetryNotBefore.Time.Before(lo) || got.RetryNotBefore.Time.After(hi) {
		t.Fatalf("retry_not_before = %v, want within [%v, %v] — the reported reset plus the "+
			"jitter window", got.RetryNotBefore.Time, lo, hi)
	}
	if !got.LimitResetsAt.Valid || !got.LimitResetsAt.Time.Equal(reset) {
		t.Fatalf("limit_resets_at = %v, want the report %v", got.LimitResetsAt, reset)
	}
	if got.RateLimitType.String != "five_hour" {
		t.Fatalf("rate_limit_type = %q", got.RateLimitType.String)
	}
	if fs.setFailed != nil {
		t.Fatalf("a successful park also failed the run: %+v", fs.setFailed)
	}
}

// 🔴 TestSetStateLimitWaitCoercesAnOptOutToFailed is §7.5, and the shape is the
// point: an opt-out report must be FAILED, never refused. A refusal is 0 rows →
// 409, which the worker treats as "the server already moved on" and stops — so the
// park would vanish with no reason, no feed line, and the run rotting at `running`
// until RUN_TIMEOUT.
func TestSetStateLimitWaitCoercesAnOptOutToFailed(t *testing.T) {
	run := runningRun(false) // opted OUT
	fs, svc, wkr := limitParkFixture(t, run)

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "limit_wait", LimitResetsAt: ms(2 * time.Hour), RateLimitType: strPtr("five_hour"),
	})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if !applied {
		t.Fatal("applied = false — an opt-out report was REFUSED rather than coerced. That " +
			"maps to 409, which the worker reads as success, so the report vanishes and the " +
			"run rots at `running` until RUN_TIMEOUT")
	}
	if fs.setLimitWait != nil {
		t.Fatal("parked a run whose owner opted out")
	}
	if fs.setFailed == nil {
		t.Fatal("the run was neither parked nor failed")
	}
	if reason := fs.setFailed.FailureReason.String; !strings.Contains(reason, "(five_hour)") {
		t.Fatalf("failure_reason = %q, want the SERVER-composed sentence", reason)
	}
}

func TestSetStateLimitWaitBudgetExhaustedFails(t *testing.T) {
	run := runningRun(true)
	run.LimitWaitCount = 5
	fs, svc, wkr := limitParkFixture(t, run)

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "limit_wait"})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if !applied || fs.setFailed == nil {
		t.Fatalf("applied=%v setFailed=%+v; an exhausted budget must FAIL the run as a 200 "+
			"carrying status failed, not refuse it", applied, fs.setFailed)
	}
	if fs.setLimitWait != nil {
		t.Fatal("parked past the budget")
	}
}

// TestSetStateLimitWaitGuardRefusalStaysARefusal is the other half: when the SQL's
// own guard rejects (not `running`, wrong worker, a judge run, a re-delivery), 0
// rows must reach the handler as applied=false → 409. The worker then cleans up,
// because its carve-out keys off the returned STATUS, not off applied.
func TestSetStateLimitWaitGuardRefusalStaysARefusal(t *testing.T) {
	run := runningRun(true)
	fs, svc, wkr := limitParkFixture(t, run)
	fs.setLimitWaitRows = 0 // the guard refused

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "limit_wait"})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if applied {
		t.Fatal("a 0-row park reported applied = true")
	}
	if fs.setFailed != nil {
		t.Fatalf("a guard refusal also failed the run: %+v — the two outcomes must stay "+
			"distinguishable, or a cancelled run gets clobbered", fs.setFailed)
	}
}

// TestSetStateLimitWaitSkipsTheCandidateQueryWithoutACredential: no recorded
// credential means nothing to exclude, so the pool legs are skipped entirely — and
// the query that feeds them is not even issued.
func TestSetStateLimitWaitSkipsTheCandidateQueryWithoutACredential(t *testing.T) {
	run := runningRun(true) // AnthropicSecretID left invalid
	fs, svc, wkr := limitParkFixture(t, run)
	fs.setLimitWaitRows = 1

	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "limit_wait"}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if len(fs.autoCandidateLookups) != 0 {
		t.Fatalf("fetched the candidate pool %d time(s) for a run with no recorded "+
			"credential; without an exclusion id the pool cannot be used at all",
			len(fs.autoCandidateLookups))
	}
}

// TestSetStateFailedComposesTheLimitReasonServerSide is §7.8. The worker's own text
// is REPLACED when the structured fields ride along, so the enum never lives on the
// untrusted side of the wire.
func TestSetStateFailedComposesTheLimitReasonServerSide(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	worker := "usage limit (i-made-this-up<script>) reached"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &worker,
		LimitResetsAt: ms(2 * time.Hour), RateLimitType: strPtr("nonsense"),
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got := fs.setFailed.FailureReason.String
	if strings.Contains(got, "script") || strings.Contains(got, "i-made-this-up") {
		t.Fatalf("failure_reason = %q — the worker composed it", got)
	}
	if !strings.Contains(got, "(unknown)") {
		t.Fatalf("failure_reason = %q, want the coerced (unknown)", got)
	}
}

// TestSetStateFailedWithoutLimitFieldsIsUntouched is what keeps every OTHER failure
// path in the product byte-identical.
func TestSetStateFailedWithoutLimitFieldsIsUntouched(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	worker := "clone failed: exit 128"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &worker,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if got := fs.setFailed.FailureReason.String; got != worker {
		t.Fatalf("failure_reason = %q, want the worker's own text %q passed through unchanged",
			got, worker)
	}
}

// --- the sweeper pass ------------------------------------------------------------

func TestSweepPromotesLimitWaitRuns(t *testing.T) {
	promoted := []store.PromoteLimitWaitRunsRow{
		{ID: uuid.New(), UserID: uuid.New(), Status: "queued"},
		{ID: uuid.New(), UserID: uuid.New(), Status: "queued"},
	}
	fs := &fakeStore{promotedLimitWait: promoted}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }
	bc := &parkBroadcaster{}
	svc.SetBroadcaster(bc)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.LimitPromoted != 2 {
		t.Fatalf("LimitPromoted = %d, want 2", res.LimitPromoted)
	}
	if len(fs.promoteLimitWaitAt) != 1 {
		t.Fatalf("the promotion pass ran %d times, want exactly 1", len(fs.promoteLimitWaitAt))
	}
	if !fs.promoteLimitWaitAt[0].Valid || !fs.promoteLimitWaitAt[0].Time.Equal(parkNow) {
		t.Fatalf("promotion clock = %v, want the sweep's own now %v — a promotion gate read "+
			"off a different clock than the rest of the pass is how a run promotes early",
			fs.promoteLimitWaitAt[0], parkNow)
	}
	for _, r := range promoted {
		if !bc.sawState(r.ID, "queued") {
			t.Fatalf("promotion of %s was not broadcast; a resumed run would sit invisible "+
				"in the board's In Progress column until something else moved it", r.ID)
		}
	}
}

// --- the two positive guards that genuinely needed editing -----------------------

func TestHasLivePollerIsFalseForAParkedRun(t *testing.T) {
	// A parked run KEEPS its worker_id for affinity, and that worker keeps
	// heartbeating for its other runs — so both existing conditions are false and,
	// without the limit_wait clause, a cancel would be enqueued for a poller that
	// will never read it again.
	wkrID := uuid.New()
	fs := &fakeStore{workerByID: store.Worker{
		ID: wkrID, LastHeartbeatAt: pgtype.Timestamptz{Time: parkNow, Valid: true},
	}}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }

	parked := store.Run{ID: uuid.New(), Status: "limit_wait", WorkerID: pgtype.UUID{Bytes: wkrID, Valid: true}}
	live, err := svc.hasLivePoller(context.Background(), parked)
	if err != nil {
		t.Fatalf("hasLivePoller: %v", err)
	}
	if live {
		t.Fatal("a parked run reported a live poller. Its worker is alive but is NOT polling " +
			"this run, so a cancel enqueued here sits unconsumed until the promotion pass — " +
			"potentially for days")
	}

	// Control: the same worker, the same heartbeat, a RUNNING run — still live. Without
	// this half, a mutation that returned false unconditionally would pass.
	running := parked
	running.Status = "running"
	if live, err := svc.hasLivePoller(context.Background(), running); err != nil || !live {
		t.Fatalf("running run: live=%v err=%v, want live", live, err)
	}
}

// parkBroadcaster records run IDS alongside statuses, which fakeBroadcaster does not.
// The pairing is what the promotion assertion needs: "two `queued` events happened"
// would pass against a pass that published the wrong runs.
type parkBroadcaster struct {
	states []struct { //nolint:govet // test-local pair
		id     uuid.UUID
		status string
	}
}

func (b *parkBroadcaster) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (b *parkBroadcaster) PublishState(id uuid.UUID, status string) {
	b.states = append(b.states, struct {
		id     uuid.UUID
		status string
	}{id, status})
}
func (b *parkBroadcaster) PublishHealth(uuid.UUID, string, string, bool) {}
func (b *parkBroadcaster) PublishInput(uuid.UUID)                        {}

func (b *parkBroadcaster) sawState(id uuid.UUID, status string) bool {
	for _, s := range b.states {
		if s.id == id && s.status == status {
			return true
		}
	}
	return false
}

// --- the claim payload's two new fields ------------------------------------------

// TestClaimCarriesWaitOnLimitAndPlanApproved pins Decision 6b's derivation at the
// point it is DERIVED. plan_approved tells the worker to skip the Phase-1 planning
// turn and implement plan_md, so a payload that is true too often is a gate bypass —
// which is why the false cases matter more than the true one.
func TestClaimCarriesWaitOnLimitAndPlanApproved(t *testing.T) {
	for name, tc := range map[string]struct {
		autoApprove   bool
		humanApproved bool
		waitOnLimit   bool
		wantApproved  bool
	}{
		"neither":             {false, false, false, false},
		"human approved":      {false, true, false, true},
		"autopilot":           {true, false, false, true},
		"both":                {true, true, true, true},
		"opted in, not gated": {false, false, true, false},
	} {
		f := newAutoFixture(t)
		f.fs.claimRun.AutoApprove = tc.autoApprove
		f.fs.claimRun.WaitOnLimit = tc.waitOnLimit
		f.fs.claimCtx.HumanPlanApproved = tc.humanApproved

		payload := f.claim(t)
		if payload.PlanApproved != tc.wantApproved {
			t.Fatalf("%s: plan_approved = %v, want %v. It is auto_approve OR a consumed "+
				"approve_plan input, and a false positive makes a resumed run implement an "+
				"UNREVIEWED plan", name, payload.PlanApproved, tc.wantApproved)
		}
		if payload.WaitOnLimit != tc.waitOnLimit {
			t.Fatalf("%s: wait_on_limit = %v, want the ROW's value %v — it is re-read on "+
				"every claim so a toggle flipped mid-flight takes effect on the next resume",
				name, payload.WaitOnLimit, tc.waitOnLimit)
		}
	}
}
