package workersvc

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests pin #3150: the per-owner custody admission limit comes ONLY from the
// custody_hold_limit setting (read once per claim through Params.CustodyHoldLimit). Every
// non-default limit is exercised at 4 (below the old compiled-in 8) and 12 (above it), so a claim
// still gated by a constant 8 fails both ways. Each subtest uses a fresh owner (the fixture) so
// custody counts never leak between cases.

func runStatus(t *testing.T, f *custodyContinuationFixture, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// holdsSnapshot is the owner's custody holds as (run, generation, state), in a stable order.
func holdsSnapshot(t *testing.T, f *custodyContinuationFixture) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT coalesce(string_agg(run_id::text||':'||generation||':'||state, ',' ORDER BY id), '')
	  FROM recovery_custody_holds WHERE user_id=$1`, f.userID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *custodyContinuationFixture) pillReason(t *testing.T, run uuid.UUID, th healthThresholds) string {
	t.Helper()
	return f.svc.queuedReason(f.env.ctx, time.Now(), store.ListActiveRunsForHealthRow{ID: run, UserID: f.userID, Kind: "issue"}, th)
}

func TestClaimCustodyLimitFreshBoundaryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, limit := range []int32{4, 12} {
		t.Run(fmt.Sprintf("limit-%d/limit-1 counted holds admit", limit), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			reader := &fakeCustodyLimit{n: limit}
			f.svc.p.CustodyHoldLimit = reader
			f.seedUnrelatedOpenHolds(int(limit) - 1)
			fresh := f.seedRun("queued", 0)
			if r := f.pillReason(t, fresh, thWithLimit(limit)); r == reasonCustodyLimit {
				t.Fatalf("pill says custody limit one below it")
			}
			if got, _ := f.claim(t); got != fresh {
				t.Fatalf("claimed %s, want the fresh run %s (owner one below limit %d)", got, fresh, limit)
			}
			if n := reader.calls.Load(); n != 1 {
				t.Fatalf("setting read %d times for one claim, want 1", n)
			}
		})
		t.Run(fmt.Sprintf("limit-%d/limit counted holds block", limit), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			reader := &fakeCustodyLimit{n: limit}
			f.svc.p.CustodyHoldLimit = reader
			f.seedUnrelatedOpenHolds(int(limit))
			fresh := f.seedRun("queued", 0)
			if r := f.pillReason(t, fresh, thWithLimit(limit)); r != reasonCustodyLimit {
				t.Fatalf("pill reason = %q, want the custody limit reason at the limit", r)
			}
			if got, _ := f.claim(t); got != uuid.Nil {
				t.Fatalf("claimed %s at the limit %d, want blocked", got, limit)
			}
			if s := runStatus(t, f, fresh); s != "queued" {
				t.Fatalf("blocked run status = %s, want queued", s)
			}
			if n := f.openHolds(t); n != int64(limit) {
				t.Fatalf("open holds = %d, want %d unchanged", n, limit)
			}
		})
	}
}

func TestClaimCustodyLimitContinuationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, limit := range []int32{4, 12} {
		t.Run(fmt.Sprintf("limit-%d/own holds limit-1 continue past the cap", limit), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: limit}
			run := f.seedRun("queued", int64(limit-1))
			for g := int64(1); g <= int64(limit-1); g++ {
				f.seedHold(f.userID, run, g, "open")
			}
			f.seedUnrelatedOpenHolds(int(limit)) // owner is over the cap on other runs' holds alone
			if r := f.pillReason(t, run, thWithLimit(limit)); r == reasonCustodyLimit {
				t.Fatalf("pill blocks a continuation-exempt run")
			}
			got, gen := f.claim(t)
			if got != run || gen != int64(limit) {
				t.Fatalf("claimed %s gen %d, want the continuation %s at gen %d", got, gen, run, limit)
			}
		})
		t.Run(fmt.Sprintf("limit-%d/own holds == limit blocked", limit), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: limit}
			run := f.seedRun("queued", int64(limit))
			for g := int64(1); g <= int64(limit); g++ {
				f.seedHold(f.userID, run, g, "open")
			}
			if r := f.pillReason(t, run, thWithLimit(limit)); r != reasonCustodyLimit {
				t.Fatalf("pill reason = %q for a run at its own-hold limit", r)
			}
			if got, _ := f.claim(t); got != uuid.Nil {
				t.Fatalf("claimed %s, want the run holding %d own holds blocked", got, limit)
			}
		})
	}
}

// An unreadable or unwired limit refuses every claim, even for an owner with no holds at all, and
// never falls back to a number. Covers the auto-commit path (no tx), the lease path (tx, no
// snapshot) and the snapshot path.
func TestClaimCustodyLimitUnavailableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	boom := fmt.Errorf("%w: boom", settings.ErrCustodyHoldLimitUnavailable)
	readers := []struct {
		name   string
		reader CustodyHoldLimitReader
		want   error
	}{
		{"read error", &fakeCustodyLimit{err: boom}, settings.ErrCustodyHoldLimitUnavailable},
		{"nil reader", nil, errNoCustodyLimitReader},
		{"configured 4 whose read fails", &fakeCustodyLimit{n: 4, err: boom}, settings.ErrCustodyHoldLimitUnavailable},
	}
	for _, rc := range readers {
		for _, path := range []string{"lease (tx, no snapshot)", "auto-commit (no tx)", "snapshot (tx)"} {
			t.Run(rc.name+"/"+path, func(t *testing.T) {
				f := newCustodyContinuationFixture(t, env)
				f.svc.p.CustodyHoldLimit = rc.reader
				fresh := f.seedRun("queued", 0)
				wkr := f.wkr
				var snap *ActiveSnapshot
				switch path {
				case "auto-commit (no tx)":
					f.svc.txBeginner = nil
				case "snapshot (tx)":
					id := seedSnapshotWorker(t, env, f.userID, "nonce-A")
					wkr = wkrRow(t, env, id)
					snap = &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}
				}
				p, err := f.svc.Claim(env.ctx, wkr, snap)
				if p != nil || !errors.Is(err, rc.want) {
					t.Fatalf("Claim = %+v, %v; want no payload and %v", p, err, rc.want)
				}
				if s := runStatus(t, f, fresh); s != "queued" {
					t.Fatalf("run status = %s, want queued", s)
				}
				if n := f.openHolds(t); n != 0 {
					t.Fatalf("a refused claim opened %d holds", n)
				}
			})
		}
	}
}

// The dedicated cross_check lane never reads the setting: a settings outage cannot strand an
// eligible checker child claimed there. The run lane stays fail-closed, including for a legacy
// run-lane checker child (an accepted #3150 limit).
func TestClaimCustodyLimitCrossCheckLaneLiveDB(t *testing.T) {
	boom := fmt.Errorf("%w: boom", settings.ErrCustodyHoldLimitUnavailable)
	t.Run("cross_check lane claims the child with a failing reader", func(t *testing.T) {
		f := newCheckerCustodyFixture(t, "plan", "claude", "cross_check", false)
		f.seed(t, 8)
		reader := &fakeCustodyLimit{err: boom}
		f.svc.p.CustodyHoldLimit = reader
		p := f.claim(t)
		if p == nil || p.RunID != f.child.String() || p.Kind != "cross_check" {
			t.Fatalf("checker child not claimed on the cross_check lane: %+v", p)
		}
		if n := reader.calls.Load(); n != 0 {
			t.Fatalf("cross_check lane read the setting %d times, want 0", n)
		}
	})
	t.Run("cross_check lane with a nil reader still claims", func(t *testing.T) {
		f := newCheckerCustodyFixture(t, "plan", "claude", "cross_check", false)
		f.seed(t, 8)
		f.svc.p.CustodyHoldLimit = nil
		if p := f.claim(t); p == nil || p.RunID != f.child.String() {
			t.Fatalf("checker child not claimed with a nil reader: %+v", p)
		}
	})
	t.Run("run lane fails closed", func(t *testing.T) {
		f := newCheckerCustodyFixture(t, "plan", "claude", "run", false)
		f.seed(t, 8)
		f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{err: boom}
		p, err := f.svc.Claim(f.env.ctx, wkrRow(t, f.env, f.worker), nil)
		if p != nil || !errors.Is(err, settings.ErrCustodyHoldLimitUnavailable) {
			t.Fatalf("run-lane Claim = %+v, %v; want the unavailable error", p, err)
		}
	})
}

// One Service, a live setting: lowering it below the owner's count blocks the next claim without
// touching any hold or run; raising it admits again.
func TestClaimCustodyLimitChangeTakesEffectLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	reader := &fakeCustodyLimit{n: 12}
	f.svc.p.CustodyHoldLimit = reader
	f.seedUnrelatedOpenHolds(5)

	first := f.seedRun("queued", 0)
	if got, _ := f.claim(t); got != first {
		t.Fatalf("limit 12 with 5 holds: claimed %s, want %s", got, first)
	}

	second := f.seedRun("queued", 0)
	before, beforeFirst := holdsSnapshot(t, f), runStatus(t, f, first)
	reader.n = 4
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("limit lowered to 4 with %d holds: claimed %s, want blocked", f.openHolds(t), got)
	}
	if after := holdsSnapshot(t, f); after != before {
		t.Fatalf("lowering the limit changed holds:\nbefore %s\nafter  %s", before, after)
	}
	if runStatus(t, f, second) != "queued" || runStatus(t, f, first) != beforeFirst {
		t.Fatal("lowering the limit changed a run status")
	}

	reader.n = 12
	if got, _ := f.claim(t); got != second {
		t.Fatalf("limit raised back to 12: claimed %s, want %s", got, second)
	}
}

// The real settings cache end to end: an admin write plus Invalidate (what the PUT does) is seen
// by the very next claim on the same Service.
func TestClaimCustodyLimitRealSettingsCacheLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cache := settings.New(env.q, time.Hour)
	set := func(v int) {
		t.Helper()
		if _, err := env.q.UpsertAppSetting(env.ctx, store.UpsertAppSettingParams{Key: settings.KeyCustodyHoldLimit, Value: strconv.Itoa(v)}); err != nil {
			t.Fatal(err)
		}
		cache.Invalidate()
	}
	t.Cleanup(func() {
		env.exec(`DELETE FROM app_settings WHERE key=$1`, settings.KeyCustodyHoldLimit)
		cache.Invalidate()
	})
	f := newCustodyContinuationFixture(t, env)
	f.svc.p.CustodyHoldLimit = cache
	f.seedUnrelatedOpenHolds(5)
	fresh := f.seedRun("queued", 0)

	set(4)
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("setting 4 with 5 holds: claimed %s, want blocked", got)
	}
	set(12)
	if got, _ := f.claim(t); got != fresh {
		t.Fatalf("setting raised to 12: claimed %s, want %s", got, fresh)
	}
}
