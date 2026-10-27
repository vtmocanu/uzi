package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Pure tests of the weekly usage reserve's Go decision core (PRD #3522 M3): the exemption rule
// (reserveEnforced), the credential resolution (reserveHeld) and the ClaimRun parameter derivation.
// The SQL twin of these rules is pinned against them in usage_reserve_livedb_test.go.

var reserveT0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func i16p(v int16) *int16 { return &v }

var (
	tokDefault = uuid.MustParse("00000000-0000-0000-0000-0000000000d1") // owner default, blocked
	tokPoolOK  = uuid.MustParse("00000000-0000-0000-0000-0000000000a1") // pooled, below
	tokPoolHi  = uuid.MustParse("00000000-0000-0000-0000-0000000000a2") // pooled, held
	tokPinOK   = uuid.MustParse("00000000-0000-0000-0000-0000000000b1") // non-pooled, below
	tokPinHi   = uuid.MustParse("00000000-0000-0000-0000-0000000000b2") // non-pooled, held
	aliasHi    = uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
	aliasOK    = uuid.MustParse("00000000-0000-0000-0000-0000000000c2")
	aliasKey   = uuid.MustParse("00000000-0000-0000-0000-0000000000c3")
)

func held(pct int) usagereserve.Verdict {
	return usagereserve.Verdict{State: usagereserve.Held, Pct: pct, Threshold: 70, Reason: usagereserve.HeldReason(pct, 70)}
}

func okV() usagereserve.Verdict { return usagereserve.Verdict{State: usagereserve.OK} }

func unknownV() usagereserve.Verdict {
	return usagereserve.Verdict{State: usagereserve.Unknown, Reason: usagereserve.UnknownReason}
}

// handSnap is a snapshot at threshold 70 with pausing on: a blocked default, a pool of one
// permitted and one blocked token, a permitted and a blocked non-pooled token, two Codex aliases
// and an API-key alias.
func handSnap() *reserveSnap {
	return &reserveSnap{
		userID: uuid.New(), threshold: i16p(70), pauseRunning: true,
		claude: map[uuid.UUID]usagereserve.Verdict{
			tokDefault: held(72), tokPoolOK: okV(), tokPoolHi: held(90), tokPinOK: okV(), tokPinHi: held(80),
		},
		codex:     map[uuid.UUID]usagereserve.Verdict{aliasHi: held(95), aliasOK: okV(), aliasKey: usagereserve.Exempt()},
		pooled:    []uuid.UUID{tokPoolOK, tokPoolHi},
		defaultID: tokDefault,
		judgeMode: BindModeDefault,
	}
}

func TestReserveEnforcedTable(t *testing.T) {
	now := reserveT0
	future := pgconv.Time(now.Add(time.Hour))
	past := pgconv.Time(now.Add(-time.Hour))
	started := pgconv.Time(now.Add(-2 * time.Hour))
	cases := []struct {
		name  string
		run   store.Run
		snap  func(*reserveSnap)
		want  bool
		noOpt bool // snapshot OFF
	}{
		{name: "issue with threshold", run: store.Run{Kind: runkind.Issue}, want: true},
		{name: "every worker kind is enforced", run: store.Run{Kind: runkind.CrossCheck}, want: true},
		{name: "chat is exempt", run: store.Run{Kind: runkind.Chat}, want: false},
		{name: "no threshold", run: store.Run{Kind: runkind.Issue}, noOpt: true, want: false},
		{name: "active override", run: store.Run{Kind: runkind.Issue, ReserveExemptUntil: future}, want: false},
		{name: "expired override", run: store.Run{Kind: runkind.Issue, ReserveExemptUntil: past}, want: true},
		{name: "override ending exactly now is over", run: store.Run{Kind: runkind.Issue, ReserveExemptUntil: pgconv.Time(now)}, want: true},
		{name: "started run, toggle on", run: store.Run{Kind: runkind.Issue, FirstStartedAt: started}, want: true},
		{name: "started run, toggle off", run: store.Run{Kind: runkind.Issue, FirstStartedAt: started},
			snap: func(s *reserveSnap) { s.pauseRunning = false }, want: false},
		{name: "never-started run, toggle off is still gated", run: store.Run{Kind: runkind.Issue},
			snap: func(s *reserveSnap) { s.pauseRunning = false }, want: true},
	}
	for _, c := range cases {
		snap := handSnap()
		if c.snap != nil {
			c.snap(snap)
		}
		if c.noOpt {
			snap = &reserveSnap{userID: snap.userID}
		}
		if got := reserveEnforced(c.run, snap, now); got != c.want {
			t.Errorf("%s: reserveEnforced = %v, want %v", c.name, got, c.want)
		}
	}
}

func uuidp(id uuid.UUID) pgtype.UUID { return pgconv.UUID(id) }

func TestReserveHeldResolution(t *testing.T) {
	now := reserveT0
	autoLane := reserveLane{mode: BindModeAuto}
	defLane := reserveLane{mode: BindModeDefault}
	pinLane := func(id uuid.UUID) reserveLane { return reserveLane{mode: BindModePinned, secretID: &id} }
	override := func(mode string, id *uuid.UUID) store.Run {
		r := store.Run{Kind: runkind.Issue, CredentialOverrideMode: pgtype.Text{String: mode, Valid: true}}
		if id != nil {
			r.CredentialOverrideSecretID = uuidp(*id)
		}
		return r
	}
	codexRun := func(alias *uuid.UUID) store.Run {
		r := store.Run{Kind: runkind.Issue, Harness: harnessCodex}
		if alias != nil {
			r.CodexSecretID = uuidp(*alias)
		}
		return r
	}
	dead := func(r store.Run, id uuid.UUID, retryIn time.Duration) store.Run {
		r.LimitDeadSecretID = uuidp(id)
		r.RetryNotBefore = pgconv.Time(now.Add(retryIn))
		return r
	}
	issue := store.Run{Kind: runkind.Issue}
	cases := []struct {
		name string
		run  store.Run
		lane reserveLane
		snap func(*reserveSnap)
		want bool
		why  string
	}{
		{name: "default lane blocked default", run: issue, lane: defLane, want: true, why: usagereserve.HeldReason(72, 70)},
		{name: "default lane, default ok", run: issue, lane: defLane, snap: func(s *reserveSnap) { s.claude[tokDefault] = okV() }, want: false},
		{name: "pinned lane ok token", run: issue, lane: pinLane(tokPinOK), want: false},
		{name: "pinned lane held token", run: issue, lane: pinLane(tokPinHi), want: true, why: usagereserve.HeldReason(80, 70)},
		{name: "pinned lane NULL id resolves to default", run: issue, lane: reserveLane{mode: BindModePinned}, want: true},
		{name: "auto lane has a permitted pooled token", run: issue, lane: autoLane, want: false},
		{name: "auto lane all pooled blocked", run: issue, lane: autoLane, snap: func(s *reserveSnap) { s.claude[tokPoolOK] = held(71) }, want: true, why: usagereserve.HeldReason(90, 70)},
		{name: "auto lane empty pool is not a reserve hold", run: issue, lane: autoLane, snap: func(s *reserveSnap) { s.pooled = nil }, want: false},
		{name: "auto lane unknown reading holds", run: issue, lane: autoLane, snap: func(s *reserveSnap) {
			s.claude[tokPoolOK] = unknownV()
		}, want: true},
		{name: "auto lane: active exclusion of the only permitted token holds", run: dead(issue, tokPoolOK, time.Hour), lane: autoLane, want: true},
		{name: "auto lane: lapsed exclusion no longer applies", run: dead(issue, tokPoolOK, -time.Minute), lane: autoLane, want: false},
		{name: "auto lane: exclusion of the blocked token changes nothing", run: dead(issue, tokPoolHi, time.Hour), lane: autoLane, want: false},
		{name: "auto lane: exclusion removing the whole pool is pool_wait, not a hold", run: dead(issue, tokPoolOK, time.Hour), lane: autoLane,
			snap: func(s *reserveSnap) { s.pooled = []uuid.UUID{tokPoolOK} }, want: false},
		{name: "override pinned ok beats a blocked worker lane", run: override(BindModePinned, &tokPinOK), lane: defLane, want: false},
		{name: "override pinned held", run: override(BindModePinned, &tokPinHi), lane: pinLane(tokPinOK), want: true},
		{name: "override pinned with a nulled id falls through to the worker", run: override(BindModePinned, nil), lane: pinLane(tokPinOK), want: false},
		{name: "override pinned with a nulled id falls through to a blocked worker", run: override(BindModePinned, nil), lane: defLane, want: true},
		{name: "override default blocked", run: override(BindModeDefault, nil), lane: pinLane(tokPinOK), want: true},
		{name: "override auto permitted beats a blocked default worker", run: override(BindModeAuto, nil), lane: defLane, want: false},
		{name: "override auto exclusion", run: dead(override(BindModeAuto, nil), tokPoolOK, time.Hour), lane: pinLane(tokPinOK), want: true},
		{name: "judge default blocked", run: store.Run{Kind: runkind.Judge}, lane: pinLane(tokPinOK), want: true},
		{name: "judge pinned ok", run: store.Run{Kind: runkind.Judge}, lane: defLane, snap: func(s *reserveSnap) {
			s.judgeMode, s.judgeSecret = BindModePinned, &tokPinOK
		}, want: false},
		{name: "judge pinned held", run: store.Run{Kind: runkind.SelfImprove}, lane: pinLane(tokPinOK), snap: func(s *reserveSnap) {
			s.judgeMode, s.judgeSecret = BindModePinned, &tokPinHi
		}, want: true},
		{name: "judge pinned NULL id is default", run: store.Run{Kind: runkind.Judge}, lane: defLane, snap: func(s *reserveSnap) {
			s.judgeMode = BindModePinned
		}, want: true},
		{name: "judge auto permitted", run: store.Run{Kind: runkind.Judge}, lane: defLane, snap: func(s *reserveSnap) { s.judgeMode = BindModeAuto }, want: false},
		{name: "judge auto pool blocked", run: store.Run{Kind: runkind.Judge}, lane: defLane, snap: func(s *reserveSnap) {
			s.judgeMode = BindModeAuto
			s.claude[tokPoolOK] = held(88)
		}, want: true},
		{name: "judge auto empty pool falls to the default (blocked)", run: store.Run{Kind: runkind.Judge}, lane: defLane, snap: func(s *reserveSnap) {
			s.judgeMode, s.pooled = BindModeAuto, nil
		}, want: true},
		{name: "judge auto empty pool falls to the default (ok)", run: store.Run{Kind: runkind.Judge}, lane: defLane, snap: func(s *reserveSnap) {
			s.judgeMode, s.pooled = BindModeAuto, nil
			s.claude[tokDefault] = okV()
		}, want: false},
		{name: "self_improve ignores the run override", run: func() store.Run {
			r := override(BindModePinned, &tokPinOK)
			r.Kind = runkind.SelfImprove
			return r
		}(), lane: defLane, want: true},
		{name: "codex held alias", run: codexRun(&aliasHi), lane: defLane, want: true, why: usagereserve.HeldReason(95, 70)},
		{name: "codex ok alias", run: codexRun(&aliasOK), lane: defLane, want: false},
		{name: "codex api-key alias is never gated", run: codexRun(&aliasKey), lane: defLane, want: false},
		{name: "codex run with a nulled alias is not held", run: codexRun(nil), lane: defLane, want: false},
		{name: "codex run ignores a blocked Anthropic default", run: codexRun(&aliasOK), lane: defLane, want: false},
		{name: "cross_check checker uses the worker lane", run: store.Run{Kind: runkind.CrossCheck, Harness: harnessClaude}, lane: pinLane(tokPinHi), want: true},
		{name: "exempt run is never held", run: store.Run{Kind: runkind.Issue, ReserveExemptUntil: pgconv.Time(now.Add(time.Hour))}, lane: defLane, want: false},
		{name: "chat is never held", run: store.Run{Kind: runkind.Chat}, lane: defLane, want: false},
		{name: "off snapshot", run: issue, lane: defLane, snap: func(s *reserveSnap) { s.threshold = nil }, want: false},
	}
	for _, c := range cases {
		snap := handSnap()
		if c.snap != nil {
			c.snap(snap)
		}
		d := reserveHeld(c.run, snap, c.lane, now)
		if d.held != c.want {
			t.Errorf("%s: held = %v, want %v (%q)", c.name, d.held, c.want, d.reason)
			continue
		}
		if c.want && c.why != "" && d.reason != c.why {
			t.Errorf("%s: reason = %q, want %q", c.name, d.reason, c.why)
		}
		if c.want && d.reason == "" {
			t.Errorf("%s: a held decision carries no reason", c.name)
		}
	}
}

// TestReserveClaimParamsArraysNonNil: a NULL array makes `= ANY(...)` NULL, which the SQL's
// COALESCE tolerates only where it was written; the params must never carry a nil slice.
func TestReserveClaimParamsArraysNonNil(t *testing.T) {
	var p store.ClaimRunParams
	reserveClaimParams(&p, &reserveSnap{userID: uuid.New()}, store.Worker{AnthropicBindMode: BindModeDefault}, nil, reserveT0)
	if p.ReserveOn {
		t.Fatal("an OFF snapshot must not enable the gate")
	}
	for name, a := range map[string][]uuid.UUID{
		"override_ended": p.ReserveOverrideEnded, "blocked_codex": p.ReserveBlockedCodex, "permitted": p.ReservePermitted,
		"pooled": p.ReservePooled, "blocked_claude": p.ReserveBlockedClaude, "blocked_workers": p.ReserveBlockedWorkerIds,
	} {
		if a == nil {
			t.Errorf("%s is nil on an OFF snapshot", name)
		}
	}
	snap := handSnap()
	var on store.ClaimRunParams
	reserveClaimParams(&on, snap, store.Worker{AnthropicBindMode: BindModeAuto}, nil, reserveT0)
	if !on.ReserveOn || !on.ReservePauseOn || on.ReserveWorkerMode != BindModeAuto || !on.ReserveOwnerDefaultBlocked {
		t.Fatalf("params = %+v", on)
	}
	if len(on.ReserveBlockedClaude) != 3 || len(on.ReserveBlockedCodex) != 1 || len(on.ReservePermitted) != 1 || len(on.ReservePooled) != 2 {
		t.Fatalf("lists = blocked_claude %v blocked_codex %v permitted %v pooled %v",
			on.ReserveBlockedClaude, on.ReserveBlockedCodex, on.ReservePermitted, on.ReservePooled)
	}
	if on.ReserveBlockedWorkerIds == nil || on.ReserveOverrideEnded == nil {
		t.Fatal("a nil array reached the params")
	}
}

func TestReserveHeldErrorIsErrReserveHeld(t *testing.T) {
	err := error(&reserveHeldError{reason: usagereserve.HeldReason(72, 70)})
	if !errors.Is(err, errReserveHeld) {
		t.Fatal("reserveHeldError must match errReserveHeld")
	}
	if got := reserveHeldReasonOf(errors.Join(errors.New("wrap"), err)); got != usagereserve.HeldReason(72, 70) {
		t.Fatalf("reason through a wrap = %q", got)
	}
	if reserveHeldReasonOf(errVaultLocked) != "" {
		t.Fatal("a non-reserve error carries no reserve reason")
	}
	// A held claim is TRANSIENT and must not be rewritten by the Claude checker mapping, so a held
	// checker stays queued and expires on its own deadline.
	checker := store.Run{Kind: runkind.CrossCheck, Harness: harnessClaude}
	if got := claudeCheckerClaimError(checker, err); !errors.Is(got, errReserveHeld) || errors.Is(got, errCheckerPinUnavailable) {
		t.Fatalf("claudeCheckerClaimError rewrote a reserve hold: %v", got)
	}
}

// TestReserveSnapshotOffWithoutReserveStore: a service whose store cannot serve the reserve reads
// (every fake Store) reads as OFF without touching the store.
func TestReserveSnapshotOffWithoutReserveStore(t *testing.T) {
	svc := New(&fakeStore{}, nil, testParams())
	snap, err := svc.reserveSnapshot(context.Background(), uuid.New(), reserveT0)
	if err != nil || snap.on() {
		t.Fatalf("snapshot = (%+v, %v), want an OFF snapshot", snap, err)
	}
	gate, err := svc.reserveGateFor(context.Background(), store.Run{UserID: uuid.New(), Kind: runkind.Issue})
	if err != nil || gate != nil {
		t.Fatalf("gate = (%v, %v), want nil", gate, err)
	}
	if err := (*reserveGate)(nil).checkChoice(secretChoice{}); err != nil {
		t.Fatalf("a nil gate refused a choice: %v", err)
	}
}

// TestHealthReserveHoldBypassesAgeGateAndSuppressesNudge: a queued run the weekly reserve holds is
// explained from its first tick (no 10m wait) with the persisted reserve text, and the per-run Slack
// nudge is suppressed (the line is the owner's own); the intake pause still outranks it.
func TestHealthReserveHoldBypassesAgeGateAndSuppressesNudge(t *testing.T) {
	reason := usagereserve.HeldReason(72, 70)
	r := runRow("queued")
	r.StatusSince = ago(5 * time.Second) // far younger than the 10m queued threshold
	r.ReserveHoldReason = reason
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1: a reserve-held young run is flagged immediately", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker || w.HealthReason.String != reason {
		t.Fatalf("health = (%q, %q), want waiting_worker with the reserve reason", w.Health, w.HealthReason.String)
	}
	if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("nudge: notified_at=%v broadcast=%v, want suppressed", w.HealthNotifiedAt, b.healthNudges)
	}

	// Control: the same young run WITHOUT the marker stays ok (the age gate is unchanged).
	plain := runRow("queued")
	plain.StatusSince = ago(5 * time.Second)
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{plain}}
	if n := healthSvc(fs2, defaultHealthSettings()).detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("control run changed = %d, want 0", n)
	}

	// An old run: the reserve reason outranks the fleet rungs (no online worker would otherwise win).
	old := runRow("queued")
	old.StatusSince = ago(time.Hour)
	old.ReserveHoldReason = reason
	fs3 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{old}, onlineWorkers: 0}
	healthSvc(fs3, defaultHealthSettings()).detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs3, old.ID).HealthReason.String; got != reason {
		t.Fatalf("old run reason = %q, want the reserve reason", got)
	}

	// Maintenance outranks the reserve.
	paused := runRow("queued")
	paused.StatusSince = ago(time.Hour)
	paused.ReserveHoldReason = reason
	paused.IntakePauseHeld = true
	fs4 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{paused}}
	healthSvc(fs4, defaultHealthSettings()).detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs4, paused.ID).HealthReason.String; got != reasonMaintenance {
		t.Fatalf("paused run reason = %q, want maintenance", got)
	}
}

func TestIsReserveHoldReason(t *testing.T) {
	for _, s := range []string{usagereserve.HeldReason(1, 1), usagereserve.UnknownReason} {
		if !isReserveHoldReason(s) {
			t.Errorf("%q is a reserve reason", s)
		}
	}
	for _, s := range []string{"", reasonMaintenance, reasonNoWorker} {
		if isReserveHoldReason(s) {
			t.Errorf("%q is not a reserve reason", s)
		}
	}
}
