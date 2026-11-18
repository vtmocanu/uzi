package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4395 M2: the pure warm-wake and origin policy (no database).

const warmIdleAfter = 14 * 24 * time.Hour

// sleeperRow is a pooled member in the given sleeping state whose op was admitted by the
// automatic pass (the owner is active now by default).
func sleeperRow(n byte, state string) poolRow {
	r := readyRow(n)
	r.HibernationState = state
	r.AdmitOrigin = AdmitOriginAutoSurplus
	r.OwnerActiveNow = true
	return r
}

// busyRow is an awake member holding a run: it is not ready, so it is no reserve.
func busyRow(n byte) poolRow {
	r := readyRow(n)
	r.Nonterminal = 1
	r.OwnerActiveNow = true
	return r
}

func TestWakeTargetEligibleEachCondition(t *testing.T) {
	if !wakeTargetEligible(sleeperRow(1, "hibernated")) || !wakeTargetEligible(sleeperRow(1, "preparing")) {
		t.Fatal("an automatically admitted sleeper must be eligible")
	}
	cases := []struct {
		name string
		mut  func(*poolRow)
	}{
		{"awake", func(r *poolRow) { r.HibernationState = "none" }},
		{"waking", func(r *poolRow) { r.HibernationState = "waking" }},
		{"manual admission", func(r *poolRow) { r.AdmitOrigin = AdmitOriginManual }},
		{"legacy or foreign op (no valid marker)", func(r *poolRow) { r.AdmitOrigin = "" }},
		{"manual cordon", func(r *poolRow) { r.ManualCordonedAt = pgconv.Time(poolNow) }},
		{"maintenance fenced", func(r *poolRow) { r.MaintenanceFenced = true }},
		{"maintenance phase", func(r *poolRow) { r.MaintenancePhase = "stopping" }},
		{"wake error", func(r *poolRow) { r.WakeError = pgtype.Text{String: "boom", Valid: true} }},
	}
	for _, c := range cases {
		r := sleeperRow(1, "hibernated")
		c.mut(&r)
		if wakeTargetEligible(r) {
			t.Errorf("%s: eligible, want not", c.name)
		}
	}
	r := sleeperRow(1, "hibernated")
	r.AdmitOrigin = AdmitOriginAutoLong
	if !wakeTargetEligible(r) {
		t.Error("auto_long admission must be eligible")
	}
	r = sleeperRow(1, "hibernated")
	r.MaintenancePhase = "complete" // a finished maintenance is not "under maintenance"
	if !wakeTargetEligible(r) {
		t.Error("a completed maintenance phase must stay eligible")
	}
	// Issue #4442: a draining (non-maintenance) sleeper stays a warm-wake target.
	for _, state := range []string{"hibernated", "preparing"} {
		r = sleeperRow(1, state)
		r.DrainingSince = pgconv.Time(poolNow)
		if !wakeTargetEligible(r) {
			t.Errorf("a draining %s sleeper must stay eligible", state)
		}
	}
}

// Issue #4442: a draining sleeper beside a busy member is picked as the warm-wake target.
func TestDecideWarmDrainingSleeper(t *testing.T) {
	busy, sleeper := busyRow(1), sleeperRow(2, "hibernated")
	sleeper.DrainingSince = pgconv.Time(poolNow.Add(-time.Hour))
	rows := []poolRow{busy, sleeper}
	if id, ok := pickWakeTarget(rows); !ok || id != sleeper.ID {
		t.Fatalf("pickWakeTarget = %v, %v; want the draining sleeper", id, ok)
	}
	if d := decideWarm(rows, sleeper.ID, poolNow, poolStale, warmIdleAfter, true); d.Action != decisionWarmWake || d.Target != sleeper.ID {
		t.Fatalf("decideWarm = %+v, want a warm wake of the draining sleeper", d)
	}
}

func TestPickWakeTargetOrdering(t *testing.T) {
	at := func(h int) pgtype.Timestamptz { return pgconv.Time(poolNow.Add(-time.Duration(h) * time.Hour)) }
	prep, hibRecent, hibOld := sleeperRow(1, "preparing"), sleeperRow(2, "hibernated"), sleeperRow(3, "hibernated")
	prep.Anchor, hibRecent.Anchor, hibOld.Anchor = at(20), at(3), at(30)
	if got, ok := pickWakeTarget([]poolRow{hibRecent, hibOld, prep}); !ok || got != prep.ID {
		t.Fatalf("target = %v %t, want the preparing member even with the oldest anchor", got, ok)
	}
	if got, _ := pickWakeTarget([]poolRow{hibOld, hibRecent}); got != hibRecent.ID {
		t.Fatalf("target = %v, want the most recently used hibernated member", got)
	}
	// Equal anchors: oldest created_at first, then lowest id.
	hibOld.Anchor = hibRecent.Anchor
	hibOld.CreatedAt = at(300)
	if got, _ := pickWakeTarget([]poolRow{hibRecent, hibOld}); got != hibOld.ID {
		t.Fatalf("target on anchor tie = %v, want the oldest %v", got, hibOld.ID)
	}
	hibOld.CreatedAt = hibRecent.CreatedAt
	if got, _ := pickWakeTarget([]poolRow{hibRecent, hibOld}); got != hibRecent.ID {
		t.Fatalf("target on full tie = %v, want the lowest id %v", got, hibRecent.ID)
	}
	// An ineligible member is skipped even with the best rank.
	prep.AdmitOrigin = AdmitOriginManual
	if got, _ := pickWakeTarget([]poolRow{prep, hibOld}); got != hibOld.ID {
		t.Fatalf("target = %v, want the eligible hibernated member", got)
	}
	if _, ok := pickWakeTarget([]poolRow{prep}); ok {
		t.Fatal("no eligible sleeper must give no target")
	}
}

func TestOwnerActiveWithin(t *testing.T) {
	r := readyRow(1)
	if ownerActiveWithin(r, poolNow, warmIdleAfter) {
		t.Fatal("an owner with no runs at all is not active")
	}
	r.OwnerAnchor = pgconv.Time(poolNow.Add(-warmIdleAfter + time.Minute))
	if !ownerActiveWithin(r, poolNow, warmIdleAfter) {
		t.Fatal("an owner whose last activity is inside the window is active")
	}
	r.OwnerAnchor = pgconv.Time(poolNow.Add(-warmIdleAfter))
	if ownerActiveWithin(r, poolNow, warmIdleAfter) {
		t.Fatal("an owner whose last activity is exactly idle_after ago is long-idle (anchor <= cutoff), not active")
	}
	r.OwnerActiveNow = true
	if !ownerActiveWithin(r, poolNow, warmIdleAfter) {
		t.Fatal("a non-terminal run makes the owner active now whatever the anchor says")
	}
}

func TestDecideWarm(t *testing.T) {
	warm := func(rows []poolRow, target uuid.UUID) poolDecision {
		return decideWarm(rows, target, poolNow, poolStale, warmIdleAfter, true)
	}
	busy, sleeper := busyRow(1), sleeperRow(2, "hibernated")
	if d := warm([]poolRow{busy, sleeper}, sleeper.ID); d.Action != decisionWarmWake || d.Target != sleeper.ID {
		t.Fatalf("busy keeper plus an auto sleeper = %+v, want a warm wake of the sleeper", d)
	}
	cases := []struct {
		name string
		rows func() ([]poolRow, uuid.UUID)
		why  string
	}{
		{"an awake ready idle member is the reserve", func() ([]poolRow, uuid.UUID) {
			return []poolRow{readyRow(1), sleeperRow(2, "hibernated")}, uuid.UUID{2}
		}, noneReserveExists},
		{"a healthy waking member counts toward the reserve", func() ([]poolRow, uuid.UUID) {
			w := sleeperRow(3, "waking")
			return []poolRow{busyRow(1), sleeperRow(2, "hibernated"), w}, uuid.UUID{2}
		}, noneWakingHealthy},
		{"a pending demand wake on any member", func() ([]poolRow, uuid.UUID) {
			p := sleeperRow(3, "hibernated")
			p.PendingWakeDemand = true
			return []poolRow{busyRow(1), sleeperRow(2, "hibernated"), p}, uuid.UUID{2}
		}, nonePendingDemand},
		{"owner inactive past idle_after", func() ([]poolRow, uuid.UUID) {
			b, s := busyRow(1), sleeperRow(2, "hibernated")
			b.OwnerActiveNow, s.OwnerActiveNow = false, false
			s.OwnerAnchor = pgconv.Time(poolNow.Add(-warmIdleAfter - time.Hour))
			return []poolRow{b, s}, s.ID
		}, noneOwnerInactive},
		{"paused instance intake", func() ([]poolRow, uuid.UUID) {
			s := sleeperRow(2, "hibernated")
			s.IntakePaused = true
			return []poolRow{busyRow(1), s}, s.ID
		}, noneSharedGate},
		{"manually hibernated member", func() ([]poolRow, uuid.UUID) {
			s := sleeperRow(2, "hibernated")
			s.AdmitOrigin = AdmitOriginManual
			return []poolRow{busyRow(1), s}, s.ID
		}, noneNotEligible},
		{"not the best target", func() ([]poolRow, uuid.UUID) {
			a, b := sleeperRow(2, "hibernated"), sleeperRow(3, "preparing")
			return []poolRow{busyRow(1), a, b}, a.ID
		}, noneNotBestTarget},
		{"a lone pool member", func() ([]poolRow, uuid.UUID) {
			return []poolRow{sleeperRow(2, "hibernated")}, uuid.UUID{2}
		}, nonePoolTooSmall},
		{"another pool is not a member", func() ([]poolRow, uuid.UUID) {
			o := busyRow(1)
			o.HostedSize = pgtype.Text{String: "l", Valid: true}
			return []poolRow{o, sleeperRow(2, "hibernated")}, uuid.UUID{2}
		}, nonePoolTooSmall},
		{"an awake candidate is not a sleeper", func() ([]poolRow, uuid.UUID) {
			return []poolRow{busyRow(1), busyRow(2)}, uuid.UUID{2}
		}, noneNotSleeper},
		{"unknown candidate", func() ([]poolRow, uuid.UUID) { return []poolRow{busyRow(1)}, uuid.UUID{9} }, noneNotCandidate},
	}
	for _, c := range cases {
		rows, target := c.rows()
		if d := warm(rows, target); d.Action != decisionNone || d.Why != c.why {
			t.Errorf("%s: %+v, want none/%s", c.name, d, c.why)
		}
	}

	// A waking member with a wake_error does not count: another sleeper is chosen.
	failed := sleeperRow(3, "waking")
	failed.WakeError = pgtype.Text{String: "boom", Valid: true}
	if d := warm([]poolRow{busyRow(1), sleeperRow(2, "hibernated"), failed}, uuid.UUID{2}); d.Action != decisionWarmWake {
		t.Fatalf("a waking member with a wake_error must not suppress: %+v", d)
	}
	// Readiness gates that make an awake idle member not ready leave the pool without a reserve.
	for name, mut := range map[string]func(*poolRow){
		"stale heartbeat": func(r *poolRow) { r.LastHeartbeatAt = pgconv.Time(poolNow.Add(-time.Hour)) },
		"cordon":          func(r *poolRow) { r.ManualCordonedAt = pgconv.Time(poolNow) },
		"memory lost":     func(r *poolRow) { r.MemoryAuthorityLostAt = pgconv.Time(poolNow.Add(-time.Hour)) },
	} {
		k := readyRow(1)
		k.OwnerActiveNow = true
		mut(&k)
		if d := warm([]poolRow{k, sleeperRow(2, "hibernated")}, uuid.UUID{2}); d.Action != decisionWarmWake {
			t.Errorf("%s: a not-ready awake member is no reserve: %+v", name, d)
		}
	}
	caps := []string{capability.WorkerMemoryPressureV1, capability.WorkerMemoryAuthorityClaimV1}
	// A memory-capable awake idle member whose memory authority is unknown (no receipt, or an
	// expired one) is no reserve, while one with a fresh held receipt is (issue #4395, PRD D16).
	for _, tc := range []struct {
		name string
		held pgtype.Timestamptz
		want string
	}{
		{"fresh receipt", pgconv.Time(poolNow.Add(-5 * time.Second)), decisionNone},
		{"no receipt", pgtype.Timestamptz{}, decisionWarmWake},
		{"expired receipt", pgconv.Time(poolNow.Add(-time.Hour)), decisionWarmWake},
	} {
		k, sl := readyRow(1), sleeperRow(2, "hibernated")
		k.OwnerActiveNow = true
		k.ProtocolCapabilities, sl.ProtocolCapabilities = caps, caps
		k.MemoryAuthorityHeldAt = tc.held
		if d := warm([]poolRow{k, sl}, sl.ID); d.Action != tc.want {
			t.Errorf("capable keeper, %s: %+v, want %s", tc.name, d, tc.want)
		}
	}
	// With memory authority unpersisted, a memory-capable pool has unknown readiness: no decision.
	b, s := busyRow(1), sleeperRow(2, "hibernated")
	b.ProtocolCapabilities, s.ProtocolCapabilities = caps, caps
	if d := decideWarm([]poolRow{b, s}, s.ID, poolNow, poolStale, warmIdleAfter, false); d.Why != noneMemoryAuthUnsure {
		t.Fatalf("unpersisted memory authority = %+v", d)
	}
	if d := decideWarm([]poolRow{b, s}, s.ID, poolNow, poolStale, warmIdleAfter, true); d.Action != decisionWarmWake {
		t.Fatalf("persisted memory authority = %+v", d)
	}
}

// settleCountsAsManual / conservativeWakeTerm table: the Go side of the parity the LiveDB test
// checks against the SQL term.
func TestConservativeWakeTermTable(t *testing.T) {
	settle := pgconv.Time(poolNow.Add(-time.Hour))
	manualAt := pgconv.Time(poolNow.Add(-5 * time.Hour))
	op := uuid.New()
	mk := func(valid bool, admit, wake string) *store.Worker {
		w := &store.Worker{LastWakeSettledAt: settle, LastManualWakeSettledAt: manualAt}
		w.HibernationOpID = hibernationUUID(op)
		if valid {
			w.HibernationOriginOpID = hibernationUUID(op)
		} else {
			w.HibernationOriginOpID = hibernationUUID(uuid.New())
		}
		w.HibernationAdmitOrigin = pgconv.TextOrNull(admit)
		w.HibernationWakeOrigin = pgconv.TextOrNull(wake)
		return w
	}
	cases := []struct {
		name     string
		w        *store.Worker
		wantTerm pgtype.Timestamptz
	}{
		{"no markers (legacy)", mk(false, "", ""), settle},
		{"foreign op id reads legacy even with warm marker", mk(false, "", WakeOriginWarm), settle},
		{"valid warm wake", mk(true, "", WakeOriginWarm), manualAt},
		{"valid system wake", mk(true, "", WakeOriginSystem), manualAt},
		{"valid manual wake", mk(true, "", WakeOriginManual), settle},
		{"valid demand wake", mk(true, "", WakeOriginDemand), settle},
		{"valid admitted op that has not woken", mk(true, AdmitOriginAutoSurplus, ""), manualAt},
		{"valid op with neither origin reads legacy", mk(true, "", ""), settle},
	}
	for _, c := range cases {
		if got := conservativeWakeTerm(c.w); got != c.wantTerm {
			t.Errorf("%s: term = %v, want %v", c.name, got, c.wantTerm)
		}
	}
	// A never-manual worker whose only settle is warm has no term at all.
	w := mk(true, "", WakeOriginWarm)
	w.LastManualWakeSettledAt = pgtype.Timestamptz{}
	if got := conservativeWakeTerm(w); got.Valid {
		t.Errorf("warm-only history = %v, want no term", got)
	}
}

func TestGroupByOwnerWarmFirst(t *testing.T) {
	o1 := uuid.UUID{1}
	at := func(h int) time.Time { return poolNow.Add(-time.Duration(h) * time.Hour) }
	groups := groupByOwner([]poolWork{
		{id: uuid.UUID{10}, owner: o1, anchor: at(9)},
		{id: uuid.UUID{11}, owner: o1, anchor: at(2), warm: true},
	})
	if len(groups) != 1 || groups[0][0].id != (uuid.UUID{11}) {
		t.Fatalf("a warm wake must come first within its owner: %+v", groups)
	}
}
