package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// Issue #4395 M1: the pure pool policy (no database).

var (
	poolNow   = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	poolStale = 45 * time.Second
	poolOwner = uuid.MustParse("11111111-1111-4111-8111-111111111111")
)

// readyRow is a pooled, awake, ready, idle member of poolOwner.
func readyRow(n byte) poolRow {
	return poolRow{
		ID: uuid.UUID{n}, UserID: poolOwner, Kind: "hosted", Status: "online",
		LastHeartbeatAt:  pgconv.Time(poolNow.Add(-5 * time.Second)),
		CreatedAt:        pgconv.Time(poolNow.Add(-100 * time.Hour)),
		HibernationState: "none", CleanupPhase: CleanupPhaseNone, MaintenancePhase: "none",
		TemplateDeclared: pgtype.Text{String: "base", Valid: true},
		HostedSize:       pgtype.Text{String: "m", Valid: true},
		DockerEnabled:    pgtype.Bool{Bool: false, Valid: true},
		EffectiveCaps:    []string{"git"}, ProtocolCapabilities: []string{"a"},
		MaxConcurrentRuns: pgtype.Int4{Int32: 2, Valid: true},
		Anchor:            pgconv.Time(poolNow.Add(-10 * time.Hour)),
	}
}

func keyOf(t *testing.T, r poolRow) string {
	t.Helper()
	k, ok := poolKey(r)
	if !ok {
		t.Fatal("expected a pool key")
	}
	return k
}

func TestPoolKeyEqualityAndExclusions(t *testing.T) {
	base := readyRow(1)
	same := func(mut func(*poolRow)) bool {
		o := readyRow(2)
		mut(&o)
		k2, ok := poolKey(o)
		return ok && k2 == keyOf(t, base)
	}
	cases := []struct {
		name string
		mut  func(*poolRow)
		want bool
	}{
		{"identical", func(*poolRow) {}, true},
		{"effective caps order and duplicates", func(r *poolRow) { r.EffectiveCaps = []string{"git", "git"} }, true},
		{"protocol caps duplicates", func(r *poolRow) { r.ProtocolCapabilities = []string{"a", "a"} }, true},
		{"different effective caps", func(r *poolRow) { r.EffectiveCaps = []string{"git", "x"} }, false},
		{"different protocol caps", func(r *poolRow) { r.ProtocolCapabilities = []string{"a", "b"} }, false},
		{"different size", func(r *poolRow) { r.HostedSize = pgtype.Text{String: "l", Valid: true} }, false},
		{"different template", func(r *poolRow) { r.TemplateDeclared = pgtype.Text{String: "other", Valid: true} }, false},
		{"different owner", func(r *poolRow) { r.UserID = uuid.New() }, false},
		{"isolated lane", func(r *poolRow) { r.IsolatedLane = true }, false},
		{"max concurrent runs", func(r *poolRow) { r.MaxConcurrentRuns = pgtype.Int4{Int32: 3, Valid: true} }, false},
		{"max concurrent runs NULL", func(r *poolRow) { r.MaxConcurrentRuns = pgtype.Int4{} }, false},
		{"cross check slots set vs NULL", func(r *poolRow) { r.MaxCrossCheckSlots = pgtype.Int4{Int32: 1, Valid: true} }, false},
		// Same effective capability set, different docker_enabled: docker folds into the set on
		// the docker side, so the sets match here, and only docker_enabled tells them apart.
		{"same effective caps, different docker_enabled", func(r *poolRow) { r.DockerEnabled = pgtype.Bool{Bool: true, Valid: true} }, false},
	}
	for _, c := range cases {
		if got := same(c.mut); got != c.want {
			t.Errorf("%s: same pool = %t, want %t", c.name, got, c.want)
		}
	}

	// NULL max_cross_check_slots matches only NULL.
	a, b := readyRow(3), readyRow(4)
	a.MaxCrossCheckSlots = pgtype.Int4{Int32: 1, Valid: true}
	b.MaxCrossCheckSlots = pgtype.Int4{Int32: 1, Valid: true}
	if keyOf(t, a) != keyOf(t, b) {
		t.Error("equal cross-check slots must share a pool")
	}
	if keyOf(t, a) == keyOf(t, readyRow(5)) {
		t.Error("cross-check slots 1 and NULL must not share a pool")
	}
	if keyOf(t, readyRow(5)) != keyOf(t, readyRow(6)) {
		t.Error("NULL and NULL cross-check slots must share a pool")
	}

	// Different delimiters in a string must not forge equality.
	x, y := readyRow(7), readyRow(8)
	x.TemplateDeclared = pgtype.Text{String: `a"|"b`, Valid: true}
	y.TemplateDeclared = pgtype.Text{String: `a`, Valid: true}
	y.HostedSize = pgtype.Text{String: `b`, Valid: true}
	if keyOf(t, x) == keyOf(t, y) {
		t.Error("key encoding is ambiguous")
	}

	for name, mut := range map[string]func(*poolRow){
		"ephemeral":        func(r *poolRow) { r.Ephemeral = true },
		"not hosted":       func(r *poolRow) { r.Kind = "local" },
		"NULL template":    func(r *poolRow) { r.TemplateDeclared = pgtype.Text{} },
		"NULL hosted size": func(r *poolRow) { r.HostedSize = pgtype.Text{} },
		"NULL docker fact": func(r *poolRow) { r.DockerEnabled = pgtype.Bool{} },
	} {
		r := readyRow(9)
		mut(&r)
		if _, ok := poolKey(r); ok {
			t.Errorf("%s: worker must be in no pool", name)
		}
	}
}

func TestMemberReadyPerGate(t *testing.T) {
	if !memberReady(readyRow(1), poolNow, poolStale) {
		t.Fatal("the baseline row must be ready")
	}
	cases := []struct {
		name string
		mut  func(*poolRow)
	}{
		{"offline", func(r *poolRow) { r.Status = "offline" }},
		{"stale heartbeat", func(r *poolRow) { r.LastHeartbeatAt = pgconv.Time(poolNow.Add(-poolStale)) }},
		{"no heartbeat", func(r *poolRow) { r.LastHeartbeatAt = pgtype.Timestamptz{} }},
		{"preparing", func(r *poolRow) { r.HibernationState = "preparing" }},
		{"hibernated", func(r *poolRow) { r.HibernationState = "hibernated" }},
		{"waking", func(r *poolRow) { r.HibernationState = "waking" }},
		{"unparseable hibernation state", func(r *poolRow) { r.HibernationState = "" }},
		{"manual cordon", func(r *poolRow) { r.ManualCordonedAt = pgconv.Time(poolNow) }},
		{"draining", func(r *poolRow) { r.DrainingSince = pgconv.Time(poolNow) }},
		{"maintenance fenced", func(r *poolRow) { r.MaintenanceFenced = true }},
		{"maintenance requested", func(r *poolRow) { r.MaintenancePhase = "requested" }},
		{"maintenance recycling", func(r *poolRow) { r.MaintenancePhase = "recycling" }},
		{"non-terminal run", func(r *poolRow) { r.Nonterminal = 1 }},
		{"pending overflow closure", func(r *poolRow) { r.PendingOverflowUntil = pgconv.Time(poolNow.Add(time.Minute)) }},
		{"cleanup requested", func(r *poolRow) { r.CleanupPhase = CleanupPhaseRequested }},
		{"cleanup started", func(r *poolRow) { r.CleanupPhase = CleanupPhaseStarted }},
		{"cleanup unknown", func(r *poolRow) { r.CleanupPhase = CleanupPhaseUnknown }},
		{"cleanup phase unparseable", func(r *poolRow) { r.CleanupPhase = "" }},
		{"memory authority lost", func(r *poolRow) { r.MemoryAuthorityLostAt = pgconv.Time(poolNow.Add(-time.Hour)) }},
		{"ephemeral", func(r *poolRow) { r.Ephemeral = true }},
	}
	for _, c := range cases {
		r := readyRow(1)
		c.mut(&r)
		if memberReady(r, poolNow, poolStale) {
			t.Errorf("%s: member must not be ready", c.name)
		}
	}
	if memberReady(readyRow(1), poolNow, 0) {
		t.Error("an unconfigured heartbeat stale window must fail closed")
	}
	// An expired overflow closure, a settled cleanup and a held custody do not block readiness.
	r := readyRow(1)
	r.PendingOverflowUntil = pgconv.Time(poolNow.Add(-time.Second))
	r.CleanupPhase = CleanupPhaseSettled
	if !memberReady(r, poolNow, poolStale) {
		t.Error("an expired closure and a settled cleanup must stay ready")
	}
}

func TestPickKeeperOrdering(t *testing.T) {
	a, b, c := readyRow(1), readyRow(2), readyRow(3)
	a.Anchor = pgconv.Time(poolNow.Add(-5 * time.Hour))
	b.Anchor = pgconv.Time(poolNow.Add(-3 * time.Hour)) // most recent surplus anchor
	c.Anchor = pgconv.Time(poolNow.Add(-4 * time.Hour))
	got, ok := pickKeeper([]poolRow{a, b, c}, poolNow, poolStale)
	if !ok || got != b.ID {
		t.Fatalf("keeper = %v %t, want the most recently used %v", got, ok, b.ID)
	}
	if got, _ := pickKeeper([]poolRow{a, c}, poolNow, poolStale); got != c.ID {
		t.Fatalf("keeper without b = %v, want %v", got, c.ID)
	}
	// Equal anchors: oldest created_at, then lowest id.
	a.Anchor, b.Anchor = c.Anchor, c.Anchor
	a.CreatedAt = pgconv.Time(poolNow.Add(-300 * time.Hour))
	if got, _ := pickKeeper([]poolRow{c, b, a}, poolNow, poolStale); got != a.ID {
		t.Fatalf("keeper on anchor tie = %v, want the oldest %v", got, a.ID)
	}
	a.CreatedAt = c.CreatedAt
	if got, _ := pickKeeper([]poolRow{c, b, a}, poolNow, poolStale); got != a.ID {
		t.Fatalf("keeper on full tie = %v, want the lowest id %v", got, a.ID)
	}
	// A not-ready member is skipped even with the best anchor.
	b.Anchor = pgconv.Time(poolNow.Add(-time.Hour))
	b.Status = "offline"
	if got, _ := pickKeeper([]poolRow{a, b}, poolNow, poolStale); got != a.ID {
		t.Fatalf("keeper = %v, want the ready one %v", got, a.ID)
	}
	if _, ok := pickKeeper([]poolRow{b}, poolNow, poolStale); ok {
		t.Fatal("no ready member must give no keeper")
	}
}

func TestDecideSurplus(t *testing.T) {
	cutoff := poolNow.Add(-2 * time.Hour)
	cand, keeper := readyRow(1), readyRow(2)
	cand.Anchor = pgconv.Time(poolNow.Add(-5 * time.Hour))
	keeper.Anchor = pgconv.Time(poolNow.Add(-3 * time.Hour))
	decide := func(rows ...poolRow) poolDecision {
		return decideSurplus(rows, cand.ID, poolNow, poolStale, cutoff, true)
	}
	if d := decide(cand, keeper); d.Action != decisionAutoSurplus || d.Keeper != keeper.ID {
		t.Fatalf("decision = %+v, want auto_surplus with keeper %v", d, keeper.ID)
	}
	if d := decide(cand); d.Action != decisionNone || d.Why != noneNoKeeper {
		t.Fatalf("a lone worker: %+v", d)
	}
	other := keeper
	other.HostedSize = pgtype.Text{String: "l", Valid: true}
	if d := decide(cand, other); d.Action != decisionNone {
		t.Fatalf("a member of another pool must not be the keeper: %+v", d)
	}
	busy := keeper
	busy.Nonterminal = 1
	if d := decide(cand, busy); d.Action != decisionNone {
		t.Fatalf("a busy member must not be the keeper: %+v", d)
	}
	asleep := keeper
	asleep.HibernationState = "preparing"
	if d := decide(cand, asleep); d.Action != decisionNone {
		t.Fatalf("a preparing member must not be the keeper: %+v", d)
	}
	// The candidate must itself be idle past the cutoff, awake and pooled.
	young := cand
	young.Anchor = pgconv.Time(cutoff.Add(time.Minute))
	if d := decideSurplus([]poolRow{young, keeper}, young.ID, poolNow, poolStale, cutoff, true); d.Action != decisionNone || d.Why != noneNotCandidate {
		t.Fatalf("a candidate idle less than the threshold must be refused as not a candidate: %+v", d)
	}
	atCutoff := cand
	atCutoff.Anchor = pgconv.Time(cutoff)
	recentKeeper := keeper
	recentKeeper.Anchor = pgconv.Time(cutoff.Add(time.Hour)) // ranks above the candidate
	if d := decideSurplus([]poolRow{atCutoff, recentKeeper}, atCutoff.ID, poolNow, poolStale, cutoff, true); d.Action != decisionAutoSurplus {
		t.Fatalf("a candidate idle exactly the threshold: %+v", d)
	}
	unpooled := cand
	unpooled.DockerEnabled = pgtype.Bool{}
	if d := decide(unpooled, keeper); d.Action != decisionNone || d.Why != noneNoPool {
		t.Fatalf("an unregistered worker: %+v", d)
	}
	if d := decideSurplus([]poolRow{keeper}, cand.ID, poolNow, poolStale, cutoff, true); d.Why != noneNotCandidate {
		t.Fatalf("an unknown candidate: %+v", d)
	}
	// Shared gates are not inputs: the rows carry no vault, custody or intake-pause field, so a
	// keeper that is ready is the keeper regardless of them (PRD D17).
}

// The candidate is never the keeper: when it ranks first among all ready idle members it is not
// admitted with the next member standing in as keeper.
func TestDecideSurplusCandidateRankingFirstIsNone(t *testing.T) {
	cutoff := poolNow.Add(-2 * time.Hour)
	recent, older := readyRow(1), readyRow(2)
	recent.Anchor = pgconv.Time(poolNow.Add(-3 * time.Hour))
	older.Anchor = pgconv.Time(poolNow.Add(-9 * time.Hour))
	rows := []poolRow{recent, older}
	if d := decideSurplus(rows, recent.ID, poolNow, poolStale, cutoff, true); d.Action != decisionNone || d.Why != noneNoKeeper {
		t.Fatalf("the top-ranked candidate must stay: %+v", d)
	}
	if d := decideSurplus(rows, older.ID, poolNow, poolStale, cutoff, true); d.Action != decisionAutoSurplus || d.Keeper != recent.ID {
		t.Fatalf("the older member is surplus with the recent one as keeper: %+v", d)
	}
}

// TestDecideSurplusMemoryAuthorityUnknown: with the active-snapshot kill switch on, memory
// authority is never persisted, so a pool of memory-capable members has unknown readiness and no
// decision is made; a pool without the capabilities is decided as usual.
func TestDecideSurplusMemoryAuthorityUnknown(t *testing.T) {
	cutoff := poolNow.Add(-2 * time.Hour)
	cand, keeper := readyRow(1), readyRow(2)
	cand.Anchor = pgconv.Time(poolNow.Add(-5 * time.Hour))
	keeper.Anchor = pgconv.Time(poolNow.Add(-3 * time.Hour)) // ranks first, so the candidate is not the keeper
	caps := []string{capability.WorkerMemoryPressureV1, capability.WorkerMemoryAuthorityClaimV1}
	cand.ProtocolCapabilities, keeper.ProtocolCapabilities = caps, caps
	if d := decideSurplus([]poolRow{cand, keeper}, cand.ID, poolNow, poolStale, cutoff, false); d.Action != decisionNone || d.Why != noneMemoryAuthUnsure {
		t.Fatalf("memory-capable pool with authority unpersisted: %+v", d)
	}
	if d := decideSurplus([]poolRow{cand, keeper}, cand.ID, poolNow, poolStale, cutoff, true); d.Action != decisionAutoSurplus {
		t.Fatalf("memory-capable pool with authority persisted: %+v", d)
	}
	// Only one of the two capabilities: the claim gate judges nothing, authority is not unknown.
	half := []string{capability.WorkerMemoryPressureV1}
	cand.ProtocolCapabilities, keeper.ProtocolCapabilities = half, half
	if d := decideSurplus([]poolRow{cand, keeper}, cand.ID, poolNow, poolStale, cutoff, false); d.Action != decisionAutoSurplus {
		t.Fatalf("pressure-only pool with authority unpersisted: %+v", d)
	}
	// Neither member is marked not-ready: the memory-capable keeper is still ready, so the
	// policy cannot cascade warm wakes from this mode.
	cand.ProtocolCapabilities, keeper.ProtocolCapabilities = caps, caps
	if !memberReady(keeper, poolNow, poolStale) {
		t.Fatal("authority-unknown members must not be marked not ready")
	}
}

func TestSurplusThresholdClamp(t *testing.T) {
	for _, c := range []struct{ surplus, idle, want time.Duration }{
		{2 * time.Hour, 336 * time.Hour, 2 * time.Hour},
		{336 * time.Hour, 24 * time.Hour, 24 * time.Hour},
		{time.Hour, time.Hour, time.Hour},
	} {
		if got := surplusThreshold(c.surplus, c.idle); got != c.want {
			t.Errorf("surplusThreshold(%v, %v) = %v, want %v", c.surplus, c.idle, got, c.want)
		}
	}
}

func TestSurplusReasonAndSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour:    "automatic: surplus in pool, idle 2h",
		48 * time.Hour:   "automatic: surplus in pool, idle 2d",
		90 * time.Minute: "automatic: surplus in pool, idle 1h30m0s",
		15 * time.Minute: "automatic: surplus in pool, idle 15m0s",
	} {
		if got := surplusHibernationReason(d); got != want {
			t.Errorf("reason(%v) = %q, want %q", d, got, want)
		}
	}
	if got := autoHibernationReason(336 * time.Hour); got != "automatic: no run activity for 14d" {
		t.Errorf("long reason changed: %q", got)
	}
}

func TestOwnerLockTimeoutDefaultsToFiveSeconds(t *testing.T) {
	if HibernationOwnerLockTimeout != 5*time.Second {
		t.Fatalf("HibernationOwnerLockTimeout = %v, want 5s", HibernationOwnerLockTimeout)
	}
	s := &Service{}
	if got := s.hibernationOwnerLockTimeout(); got != 5*time.Second {
		t.Fatalf("default owner lock timeout = %v, want 5s", got)
	}
	s.ownerLockTimeout = 200 * time.Millisecond
	if got := s.hibernationOwnerLockTimeout(); got != 200*time.Millisecond {
		t.Fatalf("override = %v", got)
	}
}

func TestGroupByOwnerFairnessOrder(t *testing.T) {
	o1, o2, o3 := uuid.UUID{1}, uuid.UUID{2}, uuid.UUID{3}
	at := func(h int) time.Time { return poolNow.Add(-time.Duration(h) * time.Hour) }
	groups := groupByOwner([]poolWork{
		{id: uuid.UUID{10}, owner: o1, anchor: at(5)},
		{id: uuid.UUID{11}, owner: o1, anchor: at(9)},
		{id: uuid.UUID{20}, owner: o2, anchor: at(8)},
		{id: uuid.UUID{30}, owner: o3, anchor: at(20)},
		{id: uuid.UUID{21}, owner: o2, anchor: at(7)},
	})
	var order []string
	for _, g := range groups {
		var ids []string
		for _, w := range g {
			ids = append(ids, string('0'+rune(w.id[0]/10))+string('0'+rune(w.id[0]%10)))
		}
		order = append(order, strings.Join(ids, ","))
	}
	// o3 waited longest, then o1 (oldest 9h), then o2 (oldest 8h); within an owner oldest first.
	if got, want := strings.Join(order, " | "), "30 | 11,10 | 20,21"; got != want {
		t.Fatalf("group order = %q, want %q", got, want)
	}
}
