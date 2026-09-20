package healthsvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// M2-B check severity + reconciler decision tests. Pure logic with fakes, an injected
// clock and NO database — the live SQL for the new queries is proven in the store package
// (M2-A), the router auth + episode/snooze wiring in the handler package.

// ---- controller.report -----------------------------------------------------

func TestControllerReport(t *testing.T) {
	ts := func(ago time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: fixedNow.Add(-ago), Valid: true}
	}
	tests := []struct {
		name       string
		configured bool
		bootAgo    time.Duration // api boot this long before fixedNow
		report     pgtype.Timestamptz
		reportErr  error
		wantSev    string
		wantSubstr string
	}{
		{"na when hosted not configured", false, time.Hour, ts(5 * time.Second), nil, sevNA, "no hosted workers are configured"},
		{"unknown in the first 5m after boot with no report", true, 2 * time.Minute, pgtype.Timestamptz{}, pgx.ErrNoRows, sevUnknown, "has not reported yet"},
		{"danger when never reported and boot is old", true, 10 * time.Minute, pgtype.Timestamptz{}, pgx.ErrNoRows, sevDanger, "never reported"},
		{"unknown from 3 missed intervals up to 5m", true, time.Hour, ts(40 * time.Second), nil, sevUnknown, "intervals late"},
		{"danger with no report for 5m", true, time.Hour, ts(6 * time.Minute), nil, sevDanger, "not reported for 6m"},
		{"ok when fresh", true, time.Hour, ts(5 * time.Second), nil, sevOK, "controller is reporting"},
		// Restart boot-grace: the singleton persists across a restart, so a stale-but-valid
		// row whose report predates this boot must honour the same grace as the no-row path
		// rather than falling straight to the age-based danger band.
		{"unknown on restart within grace with a stale pre-boot report", true, 2 * time.Minute, ts(10 * time.Minute), nil, sevUnknown, "since api start"},
		{"danger on restart past grace with a stale pre-boot report", true, 6 * time.Minute, ts(20 * time.Minute), nil, sevDanger, "not reported for 20m"},
		// Steady state within the grace window: once a report has arrived AFTER boot the
		// normal age bands apply, so a fresh post-boot report is ok, never spuriously unknown.
		{"ok on restart within grace once a report arrives after boot", true, 2 * time.Minute, ts(5 * time.Second), nil, sevOK, "controller is reporting"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{
				Store:    &fakeStore{controller: tc.report, controllerErr: tc.reportErr},
				Now:      func() time.Time { return fixedNow },
				BootTime: fixedNow.Add(-tc.bootAgo),
			})
			c := svc.checkControllerReport(context.Background(), fixedNow, tc.configured)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(strings.ToLower(c.Summary), strings.ToLower(tc.wantSubstr)) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- loops -----------------------------------------------------------------

// mutClock is a settable clock for driving BeatRegistry Register/Beat at chosen times.
type mutClock struct{ t time.Time }

func (m *mutClock) now() time.Time { return m.t }

func TestLoops(t *testing.T) {
	const interval = time.Minute

	t.Run("na when registry empty", func(t *testing.T) {
		svc := New(Config{Store: &fakeStore{}, Registry: NewBeatRegistry(func() time.Time { return fixedNow })})
		c := svc.checkLoops(fixedNow)
		if c.Severity != sevNA || !strings.Contains(c.Summary, "No background loops") {
			t.Fatalf("got %q / %q, want na", c.Severity, c.Summary)
		}
	})

	t.Run("na when registry nil", func(t *testing.T) {
		svc := New(Config{Store: &fakeStore{}})
		if c := svc.checkLoops(fixedNow); c.Severity != sevNA {
			t.Fatalf("nil registry severity = %q, want na", c.Severity)
		}
	})

	t.Run("ok when a beaten loop is fresh", func(t *testing.T) {
		clk := &mutClock{t: fixedNow}
		reg := NewBeatRegistry(clk.now)
		reg.Register("poller", interval)
		reg.Beat("poller") // beaten at fixedNow
		svc := New(Config{Store: &fakeStore{}, Registry: reg})
		// One interval later: sinceBeat = interval < 3 intervals ⇒ ok.
		c := svc.checkLoops(fixedNow.Add(interval))
		if c.Severity != sevOK || !strings.Contains(c.Summary, "All 1 background loops are ticking") {
			t.Fatalf("got %q / %q, want ok", c.Severity, c.Summary)
		}
	})

	t.Run("warn just past 3 intervals, ok just under", func(t *testing.T) {
		// The warn band is inclusive at 3× the interval (age >= loopBeatWarnIntervals*interval).
		// Hug the boundary: a beat one second PAST 3× warns, one second UNDER stays ok — so an
		// off-by-one in loopBeatWarnIntervals (3→4 or 3→2) cannot slip through as it would at 4×.
		clk := &mutClock{t: fixedNow}
		reg := NewBeatRegistry(clk.now)
		reg.Register("sweeper", interval)
		reg.Beat("sweeper") // beaten at fixedNow
		svc := New(Config{Store: &fakeStore{}, Registry: reg})
		if c := svc.checkLoops(fixedNow.Add(3*interval - time.Second)); c.Severity != sevOK || !strings.Contains(c.Summary, "All 1 background loops are ticking") {
			t.Fatalf("just under 3×: got %q / %q, want ok", c.Severity, c.Summary)
		}
		if c := svc.checkLoops(fixedNow.Add(3*interval + time.Second)); c.Severity != sevWarn || !strings.Contains(c.Summary, "sweeper loop last ticked") {
			t.Fatalf("just past 3×: got %q / %q, want warn", c.Severity, c.Summary)
		}
	})

	t.Run("danger just past 10 intervals, warn just under", func(t *testing.T) {
		// The danger band is inclusive at 10× the interval. Hug it: one second PAST 10× is
		// danger, one second UNDER is still warn (past 3× but under 10×) — pinning
		// loopBeatDangerIntervals against a 10→11 or 10→9 off-by-one.
		clk := &mutClock{t: fixedNow}
		reg := NewBeatRegistry(clk.now)
		reg.Register("lifecycle", interval)
		reg.Beat("lifecycle") // beaten at fixedNow
		svc := New(Config{Store: &fakeStore{}, Registry: reg})
		if c := svc.checkLoops(fixedNow.Add(10*interval - time.Second)); c.Severity != sevWarn || !strings.Contains(c.Summary, "lifecycle loop last ticked") {
			t.Fatalf("just under 10×: got %q / %q, want warn", c.Severity, c.Summary)
		}
		if c := svc.checkLoops(fixedNow.Add(10*interval + time.Second)); c.Severity != sevDanger || !strings.Contains(c.Summary, "lifecycle loop last ticked") {
			t.Fatalf("just past 10×: got %q / %q, want danger", c.Severity, c.Summary)
		}
	})

	t.Run("never-beaten loop escalates past its thresholds", func(t *testing.T) {
		// A REGISTERED loop that has never beaten (LastBeat zero) is unknown only while young;
		// once its RegisteredAt ages past 3× / 10× its interval it must escalate to warn /
		// danger, NOT stay stuck at unknown. Register at a fixed instant, never Beat, then
		// evaluate at chosen ages (age = now - RegisteredAt drives the bands).
		regAt := fixedNow
		mk := func() *Service {
			reg := NewBeatRegistry(func() time.Time { return regAt })
			reg.Register("poller", interval) // registered, never beaten
			return New(Config{Store: &fakeStore{}, Registry: reg})
		}
		if c := mk().checkLoops(regAt.Add(3*interval + time.Second)); c.Severity != sevWarn || !strings.Contains(c.Summary, "poller loop last ticked") {
			t.Fatalf("never-beaten past 3×: got %q / %q, want warn", c.Severity, c.Summary)
		}
		if c := mk().checkLoops(regAt.Add(10*interval + time.Second)); c.Severity != sevDanger || !strings.Contains(c.Summary, "poller loop last ticked") {
			t.Fatalf("never-beaten past 10×: got %q / %q, want danger", c.Severity, c.Summary)
		}
	})

	t.Run("unknown when not beaten and young", func(t *testing.T) {
		clk := &mutClock{t: fixedNow}
		reg := NewBeatRegistry(clk.now)
		reg.Register("poller", interval) // registered, never beaten
		svc := New(Config{Store: &fakeStore{}, Registry: reg})
		// One interval after registration (< 3 intervals): has not ticked yet ⇒ unknown.
		c := svc.checkLoops(fixedNow.Add(interval))
		if c.Severity != sevUnknown || !strings.Contains(c.Summary, "has not ticked yet") {
			t.Fatalf("got %q / %q, want unknown", c.Severity, c.Summary)
		}
	})

	t.Run("an unregistered scheduler is absent from the evidence", func(t *testing.T) {
		clk := &mutClock{t: fixedNow}
		reg := NewBeatRegistry(clk.now)
		// Only two loops registered — the conditional scheduler is NOT (not started, D15).
		reg.Register("poller", interval)
		reg.Register("sweeper", interval)
		reg.Beat("poller")
		reg.Beat("sweeper")
		svc := New(Config{Store: &fakeStore{}, Registry: reg})
		c := svc.checkLoops(fixedNow.Add(interval))
		// "All 2 ..." proves the unregistered scheduler was never counted as a loop.
		if c.Severity != sevOK || !strings.Contains(c.Summary, "All 2 background loops are ticking") {
			t.Fatalf("got %q / %q, want ok over exactly 2 loops", c.Severity, c.Summary)
		}
	})
}

// ---- forge.ciwatch ---------------------------------------------------------

func TestForgeCIWatch(t *testing.T) {
	row := func(path string, refs int64) store.CountEligibleCIWatchRefsPerRepoRow {
		return store.CountEligibleCIWatchRefsPerRepoRow{RepoPath: path, EligibleRefs: refs}
	}
	tests := []struct {
		name       string
		maxRefs    int
		rows       []store.CountEligibleCIWatchRefsPerRepoRow
		wantSev    string
		wantSubstr string
	}{
		{"na when cap is 0", 0, nil, sevNA, "CI watch is disabled"},
		{"ok when every repo fits", 5, []store.CountEligibleCIWatchRefsPerRepoRow{row("a/b", 3), row("c/d", 5)}, sevOK, "fit within the CI watch cap"},
		// Per-repo, never a fleet-wide sum: two repos each at 3 (sum 6 > cap 5) but neither
		// over the cap ⇒ ok. This fails if the check summed across repos.
		{"ok per-repo even when the sum exceeds the cap", 5, []store.CountEligibleCIWatchRefsPerRepoRow{row("a/b", 3), row("c/d", 3)}, sevOK, "fit within the CI watch cap"},
		{"warn when a repo exceeds the cap", 5, []store.CountEligibleCIWatchRefsPerRepoRow{row("a/b", 3), row("busy/repo", 9)}, sevWarn, "1 repo(s) have more run branches"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{
				Store:            &fakeStore{ciwatch: tc.rows},
				Now:              func() time.Time { return fixedNow },
				CIWatchMaxRefs:   tc.maxRefs,
				CIWatchRunWindow: 14 * 24 * time.Hour,
			})
			c := svc.checkForgeCIWatch(context.Background(), fixedNow)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- fleet.roll conjunct via Evaluate --------------------------------------

// TestEvaluateFleetRollConjunct proves controller.report's ok-ness actually reaches
// fleet.roll through Evaluate: a stale roll signal reads `unknown` only when the controller
// is silent, and NOT when the controller reports fresh.
func TestEvaluateFleetRollConjunct(t *testing.T) {
	staleWorker := hostedRow("w1", workersvc.PhaseStuck, rollSignalTTL+time.Minute, "ImagePullBackOff", "agent", "0.84.0")
	base := func(report pgtype.Timestamptz, reportErr error, bootAgo time.Duration) *Service {
		s := New(Config{
			Store:               &fakeStore{workers: []store.ListAllWorkersRow{staleWorker}, controller: report, controllerErr: reportErr},
			Settings:            &fakeSettings{healthEnabled: true},
			SlackState:          func() string { return slacksvc.StateDisabled },
			Now:                 func() time.Time { return fixedNow },
			HostedWorkerVersion: "0.84.0",
			BootTime:            fixedNow.Add(-bootAgo),
		})
		s.probeDB = func(context.Context) dbStat {
			return dbStat{pingDur: time.Millisecond, acquiredConns: 1, maxConns: 20, schemaAtHead: true}
		}
		return s
	}

	t.Run("fresh controller ⇒ fleet.roll not unknown", func(t *testing.T) {
		svc := base(pgtype.Timestamptz{Time: fixedNow.Add(-5 * time.Second), Valid: true}, nil, time.Hour)
		doc, err := svc.Evaluate(context.Background())
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if sev, summary := check(t, doc, "controller.report"); sev != sevOK {
			t.Fatalf("controller.report = %q (%q), want ok", sev, summary)
		}
		if sev, summary := check(t, doc, "fleet.roll"); sev == sevUnknown {
			t.Fatalf("fleet.roll = %q (%q); a stale signal must NOT be unknown when controller.report is ok", sev, summary)
		}
	})

	t.Run("silent controller ⇒ fleet.roll unknown", func(t *testing.T) {
		svc := base(pgtype.Timestamptz{}, pgx.ErrNoRows, 10*time.Minute) // never reported, boot old ⇒ danger
		doc, err := svc.Evaluate(context.Background())
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if sev, _ := check(t, doc, "controller.report"); sev == sevOK {
			t.Fatalf("controller.report = %q, want NOT ok for this case", sev)
		}
		if sev, summary := check(t, doc, "fleet.roll"); sev != sevUnknown {
			t.Fatalf("fleet.roll = %q (%q), want unknown when the roll signal is stale AND controller.report is not ok", sev, summary)
		}
	})
}

// ---- beat registry ---------------------------------------------------------

func TestBeatRegistry(t *testing.T) {
	clk := &mutClock{t: fixedNow}
	reg := NewBeatRegistry(clk.now)

	// Nothing registered yet.
	if snap := reg.Snapshot(); len(snap) != 0 {
		t.Fatalf("empty registry snapshot has %d entries, want 0", len(snap))
	}

	reg.Register("poller", time.Minute)
	reg.Register("sweeper", 15*time.Second)
	// Idempotent: a second Register keeps the first baseline (does not add a row).
	reg.Register("poller", time.Hour)

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(snap))
	}
	// Registration order is stable.
	if snap[0].Name != "poller" || snap[1].Name != "sweeper" {
		t.Fatalf("snapshot order = [%s %s], want [poller sweeper]", snap[0].Name, snap[1].Name)
	}
	if snap[0].Interval != time.Minute {
		t.Fatalf("idempotent Register changed poller's interval to %s, want 1m", snap[0].Interval)
	}
	if !snap[0].RegisteredAt.Equal(fixedNow) {
		t.Fatalf("poller RegisteredAt = %v, want %v", snap[0].RegisteredAt, fixedNow)
	}
	if !snap[0].LastBeat.IsZero() {
		t.Fatalf("poller LastBeat = %v, want zero before any Beat", snap[0].LastBeat)
	}

	// A beat stamps LastBeat from the clock.
	clk.t = fixedNow.Add(30 * time.Second)
	reg.Beat("poller")
	// An unknown-name beat is dropped, never a panic.
	reg.Beat("does-not-exist")

	snap = reg.Snapshot()
	if !snap[0].LastBeat.Equal(fixedNow.Add(30 * time.Second)) {
		t.Fatalf("poller LastBeat after Beat = %v, want %v", snap[0].LastBeat, fixedNow.Add(30*time.Second))
	}
	if !snap[1].LastBeat.IsZero() {
		t.Fatalf("sweeper LastBeat = %v, want zero (never beaten)", snap[1].LastBeat)
	}
}

// ---- episode reconciler ----------------------------------------------------

type fakeEvaluator struct {
	doc Doc
	err error
}

func (f fakeEvaluator) Evaluate(context.Context) (Doc, error) { return f.doc, f.err }

type fakeEpisodeStore struct {
	open       store.GetOpenHealthEpisodeRow
	openErr    error // pgx.ErrNoRows ⇒ none open
	opened     int
	openReturn uuid.UUID
	openRetErr error
	closed     []uuid.UUID

	// M6 fan-out state.
	admins    []uuid.UUID
	adminsErr error
	claimErr  error
	claims    []store.ClaimHealthEpisodeNoticeParams // every claim attempted, in order
	claimed   map[string]bool                        // (episode|user) keys already claimed (first claim inserts, repeats no-op)
}

func (f *fakeEpisodeStore) GetOpenHealthEpisode(context.Context) (store.GetOpenHealthEpisodeRow, error) {
	return f.open, f.openErr
}

// OpenHealthEpisode records the attempt and, on success, REFLECTS the new open episode so the
// NEXT GetOpenHealthEpisode returns it — letting a single fake drive the two-tick debounce
// sequence (opener tick, then the notify tick). openRetErr (e.g. a 23505) returns without
// flipping state, exactly as a losing replica sees.
func (f *fakeEpisodeStore) OpenHealthEpisode(_ context.Context, openedAt pgtype.Timestamptz) (uuid.UUID, error) {
	f.opened++
	if f.openRetErr != nil {
		return uuid.Nil, f.openRetErr
	}
	f.open = store.GetOpenHealthEpisodeRow{ID: f.openReturn, OpenedAt: openedAt}
	f.openErr = nil
	return f.openReturn, nil
}

// CloseHealthEpisode records the close and clears the open state (the re-arm), so a later
// GetOpenHealthEpisode reports pgx.ErrNoRows and a fresh danger opens a new episode.
func (f *fakeEpisodeStore) CloseHealthEpisode(_ context.Context, arg store.CloseHealthEpisodeParams) error {
	f.closed = append(f.closed, arg.ID)
	f.open = store.GetOpenHealthEpisodeRow{}
	f.openErr = pgx.ErrNoRows
	return nil
}
func (f *fakeEpisodeStore) ListAdmins(context.Context) ([]uuid.UUID, error) {
	return f.admins, f.adminsErr
}

// ClaimHealthEpisodeNotice models the real query's :execrows contract: the FIRST claim of a
// (episode, user) slot inserts (returns 1), every repeat is an ON CONFLICT DO NOTHING no-op
// (returns 0) — the atomicity the store live-DB test proves for real.
func (f *fakeEpisodeStore) ClaimHealthEpisodeNotice(_ context.Context, arg store.ClaimHealthEpisodeNoticeParams) (int64, error) {
	if f.claimErr != nil {
		return 0, f.claimErr
	}
	f.claims = append(f.claims, arg)
	if f.claimed == nil {
		f.claimed = map[string]bool{}
	}
	key := arg.EpisodeID.String() + "|" + arg.UserID.String()
	if f.claimed[key] {
		return 0, nil
	}
	f.claimed[key] = true
	return 1, nil
}

// fakeEpisodeNotifier records every notice Notify is asked to send (and can inject an error).
type fakeEpisodeNotifier struct {
	sent []notifysvc.Notification
	err  error
}

func (f *fakeEpisodeNotifier) Notify(_ context.Context, n notifysvc.Notification) (store.Notification, error) {
	f.sent = append(f.sent, n)
	return store.Notification{}, f.err
}

// fakeEpisodeSettings is the reconciler's enablement gate + base-URL seam.
type fakeEpisodeSettings struct {
	enabled    bool
	enabledErr error
	baseURL    string
}

func (f *fakeEpisodeSettings) HealthEnabled(context.Context) (bool, error) {
	return f.enabled, f.enabledErr
}
func (f *fakeEpisodeSettings) PublicBaseURL(context.Context) (string, error) {
	return f.baseURL, nil
}

// newEpisodeReconciler wires the OPEN/CLOSE lifecycle tests: the gate defaults ON and no
// admins are configured, so the danger-and-already-open path reaches notifyAdmins but sends
// nothing. The M6 notice tests build the reconciler directly to drive the fan-out.
func newEpisodeReconciler(status string, st *fakeEpisodeStore) *EpisodeReconciler {
	r := NewEpisodeReconciler(fakeEvaluator{doc: Doc{Status: status}}, st, &fakeEpisodeNotifier{}, &fakeEpisodeSettings{enabled: true}, nil)
	r.now = func() time.Time { return fixedNow }
	return r
}

func TestEpisodeReconciler(t *testing.T) {
	openRow := store.GetOpenHealthEpisodeRow{ID: uuid.New(), OpenedAt: pgtype.Timestamptz{Time: fixedNow, Valid: true}}

	t.Run("danger + none open ⇒ opens", func(t *testing.T) {
		st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New()}
		newEpisodeReconciler(sevDanger, st).Reconcile(context.Background())
		if st.opened != 1 {
			t.Fatalf("opened %d episodes, want 1", st.opened)
		}
		if len(st.closed) != 0 {
			t.Fatalf("closed %d episodes, want 0", len(st.closed))
		}
	})

	t.Run("danger + already open ⇒ no-op", func(t *testing.T) {
		st := &fakeEpisodeStore{open: openRow}
		newEpisodeReconciler(sevDanger, st).Reconcile(context.Background())
		if st.opened != 0 {
			t.Fatalf("opened %d episodes, want 0 (one already open)", st.opened)
		}
		if len(st.closed) != 0 {
			t.Fatalf("closed %d episodes, want 0", len(st.closed))
		}
	})

	t.Run("danger + 23505 on open ⇒ treated as already-open, no error", func(t *testing.T) {
		st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openRetErr: &pgconn.PgError{Code: "23505"}}
		// Must not panic and must not close anything — the race loser just yields.
		newEpisodeReconciler(sevDanger, st).Reconcile(context.Background())
		if st.opened != 1 {
			t.Fatalf("open attempted %d times, want 1", st.opened)
		}
		if len(st.closed) != 0 {
			t.Fatalf("closed %d episodes, want 0", len(st.closed))
		}
	})

	t.Run("not-danger + open ⇒ closes", func(t *testing.T) {
		st := &fakeEpisodeStore{open: openRow}
		newEpisodeReconciler(sevWarn, st).Reconcile(context.Background())
		if len(st.closed) != 1 || st.closed[0] != openRow.ID {
			t.Fatalf("closed = %v, want [%s]", st.closed, openRow.ID)
		}
		if st.opened != 0 {
			t.Fatalf("opened %d episodes, want 0", st.opened)
		}
	})

	t.Run("not-danger + none open ⇒ no-op", func(t *testing.T) {
		st := &fakeEpisodeStore{openErr: pgx.ErrNoRows}
		newEpisodeReconciler(sevOK, st).Reconcile(context.Background())
		if st.opened != 0 || len(st.closed) != 0 {
			t.Fatalf("opened %d / closed %d, want 0/0", st.opened, len(st.closed))
		}
	})
}
