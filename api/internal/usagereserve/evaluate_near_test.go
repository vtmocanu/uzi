package usagereserve

import (
	"math"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestEvaluateNearStates(t *testing.T) {
	reset := now.Add(48 * time.Hour)
	cases := []struct {
		name      string
		threshold *int16
		margin    int
		w         WeeklyReading
		state     NearState
		limit     int
		pct       int
	}{
		{"no reserve below margin", nil, 15, WeeklyReading{OK: true, Pct: 84.9, ResetsAt: &reset}, NearBelow, 100, 84},
		{"no reserve exactly at limit minus margin", nil, 15, WeeklyReading{OK: true, Pct: 85, ResetsAt: &reset}, NearNear, 100, 85},
		{"no reserve near", nil, 15, WeeklyReading{OK: true, Pct: 86, ResetsAt: &reset}, NearNear, 100, 86},
		{"no reserve just under 100", nil, 15, WeeklyReading{OK: true, Pct: 99.9, ResetsAt: &reset}, NearNear, 100, 99},
		{"no reserve at 100", nil, 15, WeeklyReading{OK: true, Pct: 100, ResetsAt: &reset}, NearAt, 100, 100},
		{"reserve moves the limit: below", i16(80), 15, WeeklyReading{OK: true, Pct: 64, ResetsAt: &reset}, NearBelow, 80, 64},
		{"reserve moves the limit: near", i16(80), 15, WeeklyReading{OK: true, Pct: 65, ResetsAt: &reset}, NearNear, 80, 65},
		{"reserve moves the limit: at", i16(80), 15, WeeklyReading{OK: true, Pct: 80, ResetsAt: &reset}, NearAt, 80, 80},
		{"reserve moves the limit: past", i16(80), 15, WeeklyReading{OK: true, Pct: 95, ResetsAt: &reset}, NearAt, 80, 95},
		{"unrounded compare: 79.9 is not at an 80 reserve", i16(80), 15, WeeklyReading{OK: true, Pct: 79.9, ResetsAt: &reset}, NearNear, 80, 79},
		{"margin 0 has no near state below the limit", nil, 0, WeeklyReading{OK: true, Pct: 99, ResetsAt: &reset}, NearBelow, 100, 99},
		{"margin 0 still reports at", nil, 0, WeeklyReading{OK: true, Pct: 100, ResetsAt: &reset}, NearAt, 100, 100},
		{"negative margin acts as 0", nil, -5, WeeklyReading{OK: true, Pct: 99, ResetsAt: &reset}, NearBelow, 100, 99},
		{"clamped above 100", nil, 15, WeeklyReading{OK: true, Pct: 140, ResetsAt: &reset}, NearAt, 100, 100},
		{"margin 50 reaches half", nil, 50, WeeklyReading{OK: true, Pct: 50, ResetsAt: &reset}, NearNear, 100, 50},
		{"zero percent", nil, 15, WeeklyReading{OK: true, Pct: 0, ResetsAt: &reset}, NearBelow, 100, 0},
		{"unknown reading", nil, 15, WeeklyReading{}, NearUnknown, 100, 0},
		{"unknown reading keeps the reserve limit", i16(70), 15, WeeklyReading{}, NearUnknown, 70, 0},
		{"unknown ignores a stray percent", nil, 15, WeeklyReading{OK: false, Pct: 99}, NearUnknown, 100, 0},
		{"no reset known is still near", nil, 15, WeeklyReading{OK: true, Pct: 90}, NearNear, 100, 90},
	}
	for _, c := range cases {
		got := EvaluateNear(c.threshold, c.margin, c.w)
		if got.State != c.state || got.EffectiveLimit != c.limit || got.Pct != c.pct {
			t.Errorf("%s: got state=%v limit=%d pct=%d, want state=%v limit=%d pct=%d",
				c.name, got.State, got.EffectiveLimit, got.Pct, c.state, c.limit, c.pct)
		}
		if got.ResetsAt != c.w.ResetsAt && c.state != NearUnknown {
			t.Errorf("%s: ResetsAt = %v, want %v", c.name, got.ResetsAt, c.w.ResetsAt)
		}
		if c.state == NearUnknown && got.ResetsAt != nil {
			t.Errorf("%s: unknown verdict carried a reset", c.name)
		}
	}
}

func TestEvaluateNearOverClaudeReadings(t *testing.T) {
	synced := now.Add(-time.Minute)
	future := now.Add(24 * time.Hour)
	cases := []struct {
		name  string
		r     ClaudeReading
		state NearState
	}{
		{"fresh near", ClaudeReading{Present: true, SevenDayPct: i16(88), SevenDayResetsAt: &future, SyncedAt: &synced}, NearNear},
		{"fresh below", ClaudeReading{Present: true, SevenDayPct: i16(20), SevenDayResetsAt: &future, SyncedAt: &synced}, NearBelow},
		{"fresh at", ClaudeReading{Present: true, SevenDayPct: i16(100), SevenDayResetsAt: &future, SyncedAt: &synced}, NearAt},
		{"stale is unknown", ClaudeReading{Present: true, SevenDayPct: i16(99), SevenDayResetsAt: &future, SyncedAt: tp(now.Add(-4 * interval))}, NearUnknown},
		{"no row is unknown", ClaudeReading{}, NearUnknown},
		{"no utilization is unknown", ClaudeReading{Present: true, SevenDayResetsAt: &future, SyncedAt: &synced}, NearUnknown},
		{"pre-reset is unknown", ClaudeReading{Present: true, SevenDayPct: i16(99), SevenDayResetsAt: tp(now.Add(-time.Minute)), SyncedAt: tp(now.Add(-time.Hour))}, NearUnknown},
	}
	for _, c := range cases {
		if got := EvaluateNear(nil, 15, ClaudeWeekly(c.r, interval, now)); got.State != c.state {
			t.Errorf("%s: state = %v, want %v", c.name, got.State, c.state)
		}
	}
}

func TestEvaluateNearOverCodexReadings(t *testing.T) {
	last := now.Add(-time.Minute)
	read := func(buckets ...apitypes.CodexRateLimitBucketDTO) CodexReading {
		return CodexReading{Present: true, Buckets: buckets, LastSuccessAt: &last}
	}
	fiveHour := &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(99), LimitWindowSeconds: i64(18000), ResetAfterSeconds: i64(60)}
	nan := f64(math.NaN())
	cases := []struct {
		name      string
		r         CodexReading
		threshold *int16
		state     NearState
		pct       int
	}{
		{"primary weekly", read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: weeklyWin(88, 3600)}), nil, NearNear, 88},
		{"secondary weekly", read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: fiveHour, Secondary: weeklyWin(91, 3600)}), nil, NearNear, 91},
		{"five hour window never counts", read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: fiveHour}), nil, NearUnknown, 0},
		{"highest of several weekly windows decides",
			read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(30, 3600)}, apitypes.CodexRateLimitBucketDTO{ID: "b", Secondary: weeklyWin(90, 7200)}),
			nil, NearNear, 90},
		{"several windows, highest at the limit", read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(30, 3600), Secondary: weeklyWin(100, 3600)}), nil, NearAt, 100},
		{"over 100 is clamped", read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(140, 3600)}), nil, NearAt, 100},
		{"reserve is the limit", read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(72, 3600)}), i16(80), NearNear, 72},
		{"invalid sibling window rejects the reading",
			read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(95, 3600), Secondary: &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: i64(WeeklyWindowSeconds), ResetAfterSeconds: i64(60)}}),
			nil, NearUnknown, 0},
		{"NaN window rejects the reading",
			read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(95, 3600), Secondary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: nan, LimitWindowSeconds: i64(WeeklyWindowSeconds), ResetAfterSeconds: i64(60)}}),
			nil, NearUnknown, 0},
		{"negative window rejects the reading", read(apitypes.CodexRateLimitBucketDTO{ID: "a", Primary: weeklyWin(-1, 3600)}), nil, NearUnknown, 0},
		{"stale reading", CodexReading{Present: true, LastSuccessAt: tp(now.Add(-4 * interval)), Buckets: []apitypes.CodexRateLimitBucketDTO{{ID: "a", Primary: weeklyWin(95, 3600)}}}, nil, NearUnknown, 0},
		{"absent reading", CodexReading{}, nil, NearUnknown, 0},
	}
	for _, c := range cases {
		cw := CodexWeekly(c.r, interval, now)
		got := EvaluateNear(c.threshold, 15, cw.Weekly())
		if got.State != c.state || got.Pct != c.pct {
			t.Errorf("%s: got state=%v pct=%d, want state=%v pct=%d", c.name, got.State, got.Pct, c.state, c.pct)
		}
	}
}

func TestEvaluateNearAgreesWithTheReserveAtTheLimit(t *testing.T) {
	// At exactly the reserve threshold the reserve holds and the near verdict says at: one rule.
	synced := now.Add(-time.Minute)
	future := now.Add(24 * time.Hour)
	for _, pct := range []int16{69, 70, 71} {
		r := ClaudeReading{Present: true, SevenDayPct: i16(pct), SevenDayResetsAt: &future, SyncedAt: &synced}
		held := EvaluateClaude(i16(70), r, interval, now).State == Held
		at := EvaluateNear(i16(70), 15, ClaudeWeekly(r, interval, now)).State == NearAt
		if held != at {
			t.Errorf("pct %d: reserve held=%v but near at=%v", pct, held, at)
		}
	}
}
