package workersvc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4395 M1: surplus hibernation over a real database. Skipped without
// UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh). Outcomes are asserted per seeded worker
// (state, reason, blocker). A test also asserts HibernationAutoPass's returned count where that
// count is the property under test (the shared per-pass cap) or a cheap cross-check beside the
// per-worker assertions; the count is never the only evidence.

// poolSettings is autoSettings with the surplus switches under test's control.
type poolSettings struct {
	*autoSettings
	mu          sync.Mutex
	surplus     bool
	surplusIdle time.Duration
}

func (p *poolSettings) WorkerHibernationSurplus(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.surplus, nil
}

func (p *poolSettings) WorkerHibernationSurplusIdleAfter(context.Context) (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.surplusIdle, nil
}

func (p *poolSettings) setSurplus(on bool) {
	p.mu.Lock()
	p.surplus = on
	p.mu.Unlock()
}

type poolFx struct {
	*autoFx
	ps *poolSettings
}

const poolSurplusIdle = 2 * time.Hour

func newPoolFx(t *testing.T) *poolFx {
	t.Helper()
	a := newAutoFx(t)
	ps := &poolSettings{autoSettings: a.set, surplus: true, surplusIdle: poolSurplusIdle}
	a.svc.SetHibernationAutoSettings(ps)
	return &poolFx{autoFx: a, ps: ps}
}

// memberOf seeds a registered, ready, idle persistent hosted worker of user whose only activity
// anchor (created_at) is `ago` before the clock. All members share one pool key.
func (p *poolFx) memberOf(user uuid.UUID, ago time.Duration) uuid.UUID {
	p.t.Helper()
	id, pod := uuid.New(), uuid.New()
	p.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared,
	            registered_pod_uid, docker_enabled, max_concurrent_runs, created_at, last_heartbeat_at)
	            VALUES ($1, $2, $3, $4, 'online', 'hosted', 'm', 'base', $5, false, 2, $6, $7)`,
		id, user, "w-"+id.String(), id[:], pod, p.clock.Add(-ago), p.clock)
	return id
}

func (p *poolFx) member(ago time.Duration) uuid.UUID { return p.memberOf(p.user, ago) }

// owner seeds one more owner with its repository; its rows go with the user.
func (p *poolFx) owner() uuid.UUID {
	p.t.Helper()
	user, _, _ := p.env.seedCodexInfra(p.t)
	p.t.Cleanup(func() { _, _ = p.env.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, user) })
	return user
}

// beat refreshes the heartbeat of every awake worker of every owner this fixture seeded.
func (p *poolFx) beat(ids ...uuid.UUID) {
	p.t.Helper()
	for _, id := range ids {
		p.env.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id = $1`, id, p.clock)
	}
}

// pass advances the clock past the pacing gap, refreshes the heartbeats of ids, and runs a pass.
func (p *poolFx) pass(ids ...uuid.UUID) int64 {
	p.t.Helper()
	p.advance(2 * time.Minute)
	p.beat(ids...)
	n, err := p.runPass()
	if err != nil {
		p.t.Fatalf("pass: %v", err)
	}
	return n
}

func (p *poolFx) wantStates(want map[uuid.UUID]string) {
	p.t.Helper()
	for id, state := range want {
		if got := p.state(id); got != state {
			p.t.Fatalf("worker %s state = %q, want %q", id, got, state)
		}
	}
}

// Example 1: two equal workers, both idle and ready. At the surplus threshold - 1m nothing; at
// threshold + 1m the less recently used one is preparing with the surplus reason and the other
// stays awake. No hibernation event row is written.
func TestHibernationSurplusExample1LiveDB(t *testing.T) {
	p := newPoolFx(t)
	older := p.member(poolSurplusIdle - 2*time.Minute) // idle 1h58m at seed
	newer := p.member(poolSurplusIdle - 62*time.Minute)

	p.advance(time.Minute) // older idle 1h59m: threshold - 1m
	p.beat(older, newer)
	if n, err := p.runPass(); err != nil || n != 0 {
		t.Fatalf("pass at threshold - 1m = %d, %v; want 0", n, err)
	}
	p.wantStates(map[uuid.UUID]string{older: "none", newer: "none"})

	p.advance(2 * time.Minute) // older idle 2h01m: threshold + 1m
	p.beat(older, newer)
	if n, err := p.runPass(); err != nil || n != 1 {
		t.Fatalf("pass at threshold + 1m = %d, %v; want 1", n, err)
	}
	p.wantStates(map[uuid.UUID]string{older: "preparing", newer: "none"})
	w := p.row(older)
	if got := w.HibernationReason.String; got != "automatic: surplus in pool, idle 2h" {
		t.Fatalf("reason = %q", got)
	}
	if p.events(older, "hibernate") != 0 {
		t.Fatal("a surplus admission must insert no hibernation event row")
	}
	// The keeper stays awake on later passes: the other member is preparing, no keeper remains
	// for it, so it is not surplus however long it idles.
	p.advance(3 * time.Hour)
	p.beat(newer)
	if n, err := p.runPass(); err != nil || n != 0 {
		t.Fatalf("later pass = %d, %v; want 0", n, err)
	}
	p.wantStates(map[uuid.UUID]string{older: "preparing", newer: "none"})
}

// Example 3: a Docker and a non-Docker worker of one owner are different pools: neither is
// surplus.
func TestHibernationSurplusExample3DockerPoolsLiveDB(t *testing.T) {
	p := newPoolFx(t)
	plain, docker := p.member(5*time.Hour), p.member(6*time.Hour)
	p.env.exec(`UPDATE workers SET docker_enabled = true WHERE id = $1`, docker)
	p.pass(plain, docker)
	p.wantStates(map[uuid.UUID]string{plain: "none", docker: "none"})
	// Equal effective capabilities (docker folds into the set) but different docker_enabled stay
	// in different pools.
	p.env.exec(`UPDATE workers SET capabilities = '{docker}' WHERE id = $1`, plain)
	p.pass(plain, docker)
	p.wantStates(map[uuid.UUID]string{plain: "none", docker: "none"})
	// Control: two Docker workers are one pool.
	other := p.member(7 * time.Hour)
	p.env.exec(`UPDATE workers SET docker_enabled = true WHERE id = $1`, other)
	p.pass(plain, docker, other)
	p.wantStates(map[uuid.UUID]string{plain: "none", other: "preparing", docker: "none"})
}

// No surplus when the other member is busy, in another pool, or not ready through any gate.
func TestHibernationSurplusNoKeeperLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(p *poolFx, other uuid.UUID)
		noBeat bool // the other member's heartbeat is left to go stale
	}{
		{name: "busy", mut: func(p *poolFx, other uuid.UUID) { p.runOf(runkind.Issue, "running", &other) }},
		{name: "other pool by size", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET hosted_size = 'l' WHERE id = $1`, other)
		}},
		{name: "other pool by protocol caps", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, other, []string{capability.WorkerMemoryPressureV1})
		}},
		{name: "other pool by slots", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET max_concurrent_runs = 3 WHERE id = $1`, other)
		}},
		{name: "stale heartbeat", mut: func(*poolFx, uuid.UUID) {}, noBeat: true},
		{name: "manual cordon", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET manual_cordoned_at = $2, manual_cordoned_by = $3, manual_cordoned_by_label = 'admin' WHERE id = $1`,
				other, p.clock, p.user)
		}},
		{name: "draining", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET draining_since = $2 WHERE id = $1`, other, p.clock)
		}},
		{name: "maintenance fenced", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET maintenance_fenced = true WHERE id = $1`, other)
		}},
		{name: "pending overflow", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET pending_overflow_until = $2 WHERE id = $1`, other, p.clock.Add(time.Hour))
		}},
		{name: "memory authority lost", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET memory_authority_lost_at = $2 WHERE id = $1`, other, p.clock.Add(-time.Hour))
		}},
		{name: "offline", mut: func(p *poolFx, other uuid.UUID) {
			p.env.exec(`UPDATE workers SET status = 'offline' WHERE id = $1`, other)
		}},
		{name: "manually hibernated", mut: func(p *poolFx, other uuid.UUID) { p.hibernate(other) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			cand, other := p.member(6*time.Hour), p.member(5*time.Hour)
			c.mut(p, other)
			beat := []uuid.UUID{cand, other}
			if c.noBeat {
				beat = []uuid.UUID{cand}
			}
			p.pass(beat...)
			w := p.row(cand)
			if w.HibernationState != "none" || w.HibernationReason.Valid {
				t.Fatalf("candidate state %q reason %+v, want untouched awake", w.HibernationState, w.HibernationReason)
			}
			wantNotBlocked(t, w)
		})
	}
	t.Run("control: a ready idle keeper admits", func(t *testing.T) {
		p := newPoolFx(t)
		cand, other := p.member(6*time.Hour), p.member(5*time.Hour)
		p.pass(cand, other)
		p.wantStates(map[uuid.UUID]string{cand: "preparing", other: "none"})
	})
}

// Shared gates do not decide: a keeper that holds custody is still the keeper, and the candidate
// is admitted.
func TestHibernationSurplusSharedGatesLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.member(6*time.Hour), p.member(5*time.Hour)
	p.seedOpenCustodyHold(keeper)
	p.pass(cand, keeper)
	p.wantStates(map[uuid.UUID]string{cand: "preparing", keeper: "none"})
}

// With the active-snapshot kill switch on, memory authority is not persisted: a pool of
// memory-capable members gets no surplus decision; a pool without the capabilities does.
func TestHibernationSurplusMemoryAuthorityUnknownLiveDB(t *testing.T) {
	caps := []string{capability.WorkerMemoryPressureV1, capability.WorkerMemoryAuthorityClaimV1}
	for _, tc := range []struct {
		name     string
		capable  bool
		disabled bool
		want     string
	}{
		{"capable, kill switch off", true, false, "preparing"},
		{"capable, kill switch on", true, true, "none"},
		{"not capable, kill switch on", false, true, "preparing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPoolFx(t)
			p.svc.SetActiveSnapshotDisabled(tc.disabled)
			cand, keeper := p.member(6*time.Hour), p.member(5*time.Hour)
			if tc.capable {
				p.env.exec(`UPDATE workers SET protocol_capabilities = $1 WHERE id = ANY($2)`, caps, []uuid.UUID{cand, keeper})
			}
			p.pass(cand, keeper)
			p.wantStates(map[uuid.UUID]string{cand: tc.want, keeper: "none"})
		})
	}
}

// Example 5, first half: with the surplus setting off the pass is today's: an equal idle pair is
// left alone below the long threshold, and a worker idle past it is admitted with the long reason.
func TestHibernationSurplusOffIsTodayLiveDB(t *testing.T) {
	p := newPoolFx(t)
	p.ps.setSurplus(false)
	a, b := p.member(6*time.Hour), p.member(5*time.Hour)
	old := p.member(400 * 24 * time.Hour)
	if n := p.pass(a, b, old); n != 1 {
		t.Fatalf("pass = %d, want 1 (only the long-idle worker)", n)
	}
	p.wantStates(map[uuid.UUID]string{a: "none", b: "none", old: "preparing"})
	if got := p.row(old).HibernationReason.String; got != "automatic: no run activity for 14d" {
		t.Fatalf("reason = %q", got)
	}
}

// In surplus mode the long rule still applies: an unregistered (unpooled) worker and a lone
// pooled one idle past the long threshold are admitted exactly as today.
func TestHibernationSurplusLongRuleStillAppliesLiveDB(t *testing.T) {
	p := newPoolFx(t)
	unpooled := p.member(400 * 24 * time.Hour)
	p.env.exec(`UPDATE workers SET docker_enabled = NULL WHERE id = $1`, unpooled)
	owner2 := p.owner()
	lone := p.memberOf(owner2, 400*24*time.Hour)
	p.pass(unpooled, lone)
	p.wantStates(map[uuid.UUID]string{unpooled: "preparing", lone: "preparing"})
	for _, id := range []uuid.UUID{unpooled, lone} {
		if got := p.row(id).HibernationReason.String; got != "automatic: no run activity for 14d" {
			t.Fatalf("reason = %q", got)
		}
	}
}

// A candidate the list saw unpooled (alone in its pool) that became pooled before its attempt (an
// equal-key sibling registered) is not admitted on the per-worker clock without the owner lock: the
// attempt changes nothing and the next pass decides it as a pooled worker.
func TestHibernationSurplusStaleUnpooledCandidateSkippedLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand := p.member(400 * 24 * time.Hour)
	sibling := uuid.Nil
	p.svc.autoPassAfterList = func() { sibling = p.member(time.Hour) }
	if n := p.pass(cand); n != 0 {
		t.Fatalf("pass with the candidate pooled after the list = %d, want 0", n)
	}
	if sibling == uuid.Nil {
		t.Fatal("the after-list hook never ran")
	}
	p.wantStates(map[uuid.UUID]string{cand: "none"})
	p.svc.autoPassAfterList = nil
	if n := p.pass(cand, sibling); n != 1 {
		t.Fatalf("control pass (listed as pooled) = %d, want 1", n)
	}
	p.wantStates(map[uuid.UUID]string{cand: "preparing"})
}

// Refusals are recorded as today, do not use the owner's one change, and the owner's next
// candidate is tried.
func TestHibernationSurplusRefusalRecordedLiveDB(t *testing.T) {
	p := newPoolFx(t)
	first, second, keeper := p.member(9*time.Hour), p.member(8*time.Hour), p.member(5*time.Hour)
	p.seedOpenCustodyHold(first)
	p.pass(first, second, keeper)
	w := p.row(first)
	if w.HibernationState != "none" || w.HibernationBlockedReason.String != BlockedCustodyHold {
		t.Fatalf("first: state %q blocked %+v, want none/custody_hold", w.HibernationState, w.HibernationBlockedReason)
	}
	p.wantStates(map[uuid.UUID]string{second: "preparing", keeper: "none"})
}

// The candidate is never the keeper: when the most recently used member is itself a candidate and
// ranks first, it stays awake even though the older member refuses. The older member is tried
// first (oldest anchor), is refused by an open custody hold (custody does not affect readiness)
// and records its blocker; the recent one is decided none, never hibernated with the older one as
// "keeper".
func TestHibernationSurplusCandidateNeverKeeperLiveDB(t *testing.T) {
	p := newPoolFx(t)
	older, recent := p.member(9*time.Hour), p.member(5*time.Hour)
	p.seedOpenCustodyHold(older)
	p.pass(older, recent)
	w := p.row(older)
	if w.HibernationState != "none" || w.HibernationBlockedReason.String != BlockedCustodyHold {
		t.Fatalf("older: state %q blocked %+v, want none/custody_hold", w.HibernationState, w.HibernationBlockedReason)
	}
	r := p.row(recent)
	if r.HibernationState != "none" || r.HibernationReason.Valid {
		t.Fatalf("recent: state %q reason %+v, want untouched awake", r.HibernationState, r.HibernationReason)
	}
	wantNotBlocked(t, r)
}

// Budgets: one state change per owner per pass; the shared cap; owners served oldest-first.
func TestHibernationSurplusBudgetsLiveDB(t *testing.T) {
	t.Run("one change per owner per pass", func(t *testing.T) {
		p := newPoolFx(t)
		a, b, c := p.member(10*time.Hour), p.member(9*time.Hour), p.member(8*time.Hour)
		if n := p.pass(a, b, c); n != 1 {
			t.Fatalf("pass 1 = %d, want 1", n)
		}
		p.wantStates(map[uuid.UUID]string{a: "preparing", b: "none", c: "none"})
		if n := p.pass(b, c); n != 1 {
			t.Fatalf("pass 2 = %d, want 1", n)
		}
		p.wantStates(map[uuid.UUID]string{a: "preparing", b: "preparing", c: "none"})
		if n := p.pass(c); n != 0 {
			t.Fatalf("pass 3 = %d, want 0: the last member is the keeper", n)
		}
		p.wantStates(map[uuid.UUID]string{c: "none"})
	})

	t.Run("shared cap of ten, oldest owner first", func(t *testing.T) {
		p := newPoolFx(t)
		const owners = HibernationAutoAdmissionsPerPass + 2
		cands := make([]uuid.UUID, owners)
		keepers := make([]uuid.UUID, owners)
		var all []uuid.UUID
		for i := range owners {
			o := p.owner()
			// Owner i's candidate is (owners - i) minutes older: owner 0 waited longest.
			cands[i] = p.memberOf(o, 20*time.Hour+time.Duration(owners-i)*time.Minute)
			keepers[i] = p.memberOf(o, 5*time.Hour)
			all = append(all, cands[i], keepers[i])
		}
		if n := p.pass(all...); n != HibernationAutoAdmissionsPerPass {
			t.Fatalf("pass 1 admitted %d, want the cap %d", n, HibernationAutoAdmissionsPerPass)
		}
		for i := range owners {
			want := "preparing"
			if i >= HibernationAutoAdmissionsPerPass { // the two youngest owners wait a pass
				want = "none"
			}
			p.wantStates(map[uuid.UUID]string{cands[i]: want, keepers[i]: "none"})
		}
		if n := p.pass(all...); n != 2 {
			t.Fatalf("pass 2 admitted %d, want the remaining 2", n)
		}
		for i := range owners {
			p.wantStates(map[uuid.UUID]string{cands[i]: "preparing", keepers[i]: "none"})
		}
	})
}

// A lock held elsewhere skips that owner for the pass and never blocks another owner; the
// skipped owner is decided on a later pass.
func TestHibernationSurplusLockTimeoutLiveDB(t *testing.T) {
	p := newPoolFx(t)
	p.svc.ownerLockTimeout = 200 * time.Millisecond
	busyOwner, freeOwner := p.owner(), p.owner()
	busyCand := p.memberOf(busyOwner, 30*time.Hour) // older: served first
	busyKeep := p.memberOf(busyOwner, 5*time.Hour)
	freeCand := p.memberOf(freeOwner, 20*time.Hour)
	freeKeep := p.memberOf(freeOwner, 5*time.Hour)
	all := []uuid.UUID{busyCand, busyKeep, freeCand, freeKeep}

	tx, err := p.env.pool.Begin(p.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback(p.env.ctx)
		}
	}
	defer release()
	if _, err := tx.Exec(p.env.ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.HibernationOwnerLockClass, store.HibernationOwnerLockObjID(busyOwner)); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	n := p.pass(all...)
	elapsed := time.Since(start)
	if n != 1 {
		t.Fatalf("pass = %d, want 1 (the free owner)", n)
	}
	if elapsed < 200*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("pass took %v, want a bounded wait of about the 200ms lock timeout", elapsed)
	}
	p.wantStates(map[uuid.UUID]string{busyCand: "none", busyKeep: "none", freeCand: "preparing", freeKeep: "none"})

	// The candidate row lock is bounded the same way.
	release()
	rowTx, err := p.env.pool.Begin(p.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rowTx.Rollback(p.env.ctx) }()
	if _, err := rowTx.Exec(p.env.ctx, `SELECT 1 FROM workers WHERE id = $1 FOR UPDATE`, busyCand); err != nil {
		t.Fatal(err)
	}
	if n := p.pass(busyKeep, freeCand, freeKeep); n != 0 { // busyCand is row-locked: no heartbeat write
		t.Fatalf("pass with a row lock held = %d, want 0", n)
	}
	p.wantStates(map[uuid.UUID]string{busyCand: "none", busyKeep: "none", freeCand: "preparing", freeKeep: "none"})
	_ = rowTx.Rollback(p.env.ctx)
	if n := p.pass(all...); n != 1 {
		t.Fatalf("pass after release = %d, want 1", n)
	}
	p.wantStates(map[uuid.UUID]string{busyCand: "preparing", busyKeep: "none"})
}

// The race the owner lock exists for. One owner, a two-member pool A, B, both idle past the
// threshold. Two Service instances with different clocks decide at the same time: at S1's clock A
// is the ready keeper and B the candidate; at S2's later clock A's heartbeat is stale, so B is the
// keeper and A the candidate. They lock distinct worker rows, so only the owner advisory lock
// serializes them; without it both are admitted and the owner has no awake worker.
//
// The test is event-driven: S1 waits in its after-decide hook until S2 has either reached its own
// decision (the unlocked outcome) or is observed waiting on the advisory lock in pg_locks (the
// locked outcome), with a 10s safety timeout that fails the test.
func TestHibernationSurplusOwnerLockConcurrencyLiveDB(t *testing.T) {
	p := newPoolFx(t)
	hb := p.clock // heartbeat of A; B's is a stale window later
	stale := testParams().WorkerHeartbeatStale
	a := p.member(5 * time.Hour) // more recent anchor: the keeper whenever it is ready
	b := p.member(6 * time.Hour)
	p.env.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id = $1`, a, hb)
	p.env.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id = $1`, b, hb.Add(stale))

	t1 := hb.Add(stale / 4)       // A is fresh
	t2 := hb.Add(stale + stale/2) // A is stale, B (heartbeat hb+stale) is fresh
	if err := p.env.q.UpsertControllerHibernationAdvertisement(p.env.ctx, store.UpsertControllerHibernationAdvertisementParams{
		AdvertisedAt: pgtype.Timestamptz{Time: t2, Valid: true}, HibernationVersion: pgtype.Int4{Int32: 1, Valid: true},
		PollIntervalSeconds: pgtype.Int4{Int32: 300, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	newSvc := func(now time.Time) *Service {
		s := New(p.env.q, p.env.box, testParams())
		s.SetTxBeginner(p.env.pool)
		s.SetVault(p.env.vlt)
		s.SetHibernationSettings(p.set)
		s.SetHibernationAutoSettings(p.ps)
		s.SetClock(func() time.Time { return now })
		return s
	}
	s1, s2 := newSvc(t1), newSvc(t2)
	params := func(now time.Time) surplusPassParams {
		return surplusPassParams{
			now: now, idleAfter: autoIdleAfter, threshold: poolSurplusIdle,
			surplusCutoff: now.Add(-poolSurplusIdle), longCutoff: now.Add(-autoIdleAfter),
			recentCutoff: now.Add(-HibernationAutoRefusalCooldown),
		}
	}

	var s2pid atomic.Int32
	s2.poolPassBeforeOwnerLock = func(pid int32) { s2pid.Store(pid) }
	s2Decided := make(chan struct{}, 1)
	s2.poolPassAfterDecide = func() { s2Decided <- struct{}{} }
	s1Decided := make(chan struct{})
	var releasedBy string
	s1.poolPassAfterDecide = func() {
		close(s1Decided)
		deadline := time.After(10 * time.Second)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-s2Decided:
				releasedBy = "s2_decided"
				return
			case <-deadline:
				t.Error("S2 neither reached its decision nor waited on the owner lock within 10s")
				releasedBy = "timeout"
				return
			case <-tick.C:
				pid := s2pid.Load()
				if pid == 0 {
					continue
				}
				var waiting bool
				err := p.env.pool.QueryRow(p.env.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
				    WHERE locktype = 'advisory' AND NOT granted AND pid = $1)`, pid).Scan(&waiting)
				if err != nil {
					t.Errorf("pg_locks: %v", err)
					releasedBy = "error"
					return
				}
				if waiting {
					releasedBy = "s2_blocked_on_lock"
					return
				}
			}
		}
	}

	type outcome struct {
		res workResult
		err error
	}
	run := func(s *Service, id uuid.UUID, now time.Time) chan outcome {
		ch := make(chan outcome, 1)
		go func() {
			res, err := s.poolAttempt(p.env.ctx, poolWork{id: id, owner: p.user, anchor: now.Add(-6 * time.Hour), pooled: true, surplus: true}, params(now))
			ch <- outcome{res, err}
		}()
		return ch
	}
	c1 := run(s1, b, t1) // at S1's clock A is the ready keeper, B the candidate
	select {
	case <-s1Decided:
	case <-time.After(10 * time.Second):
		t.Fatal("S1 never reached its decision")
	}
	c2 := run(s2, a, t2) // at S2's clock B is the ready keeper, A the candidate
	o1, o2 := <-c1, <-c2
	if o1.err != nil || o2.err != nil {
		t.Fatalf("attempts: %v, %v", o1.err, o2.err)
	}
	t.Logf("S1 released by %s; S1 admitted=%t, S2 admitted=%t", releasedBy, o1.res.admitted, o2.res.admitted)

	preparing := 0
	for _, id := range []uuid.UUID{a, b} {
		if p.state(id) == "preparing" {
			preparing++
		}
	}
	if preparing != 1 {
		t.Fatalf("%d of the pool's 2 workers are preparing, want exactly 1 (S1 released by %s)", preparing, releasedBy)
	}
	if releasedBy != "s2_blocked_on_lock" {
		t.Fatalf("S1 released by %q, want s2_blocked_on_lock: S2 must wait on the owner lock", releasedBy)
	}
}

// The surplus setting is re-read on the locked row: a pass whose candidate list was read while
// the setting was on admits nothing once it is switched off before the decision is written.
func TestHibernationSurplusRecheckLiveDB(t *testing.T) {
	p := newPoolFx(t)
	cand, keeper := p.member(6*time.Hour), p.member(5*time.Hour)
	p.svc.autoPassAfterList = func() { p.ps.setSurplus(false) }
	if n := p.pass(cand, keeper); n != 0 {
		t.Fatalf("pass with the setting switched off after the list = %d, want 0", n)
	}
	p.wantStates(map[uuid.UUID]string{cand: "none", keeper: "none"})
	p.svc.autoPassAfterList = nil
	p.ps.setSurplus(true)
	if n := p.pass(cand, keeper); n != 1 {
		t.Fatalf("control pass = %d, want 1", n)
	}
	p.wantStates(map[uuid.UUID]string{cand: "preparing", keeper: "none"})
}
