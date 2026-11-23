package usagereserve

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func weeklyWin(used float64, resetAfter int64) *apitypes.CodexRateLimitWindowDTO {
	return &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(used), LimitWindowSeconds: i64(WeeklyWindowSeconds), ResetAfterSeconds: i64(resetAfter)}
}

func TestClaudeWeeklyPolicy(t *testing.T) {
	synced := now.Add(-time.Minute)
	future := now.Add(24 * time.Hour)
	cases := []struct {
		name   string
		r      ClaudeReading
		wantOK bool
		want   float64
	}{
		{"fresh", ClaudeReading{Present: true, SevenDayPct: i16(42), SevenDayResetsAt: &future, SyncedAt: &synced}, true, 42},
		{"no row", ClaudeReading{}, false, 0},
		{"no utilization", ClaudeReading{Present: true, SevenDayResetsAt: &future, SyncedAt: &synced}, false, 0},
		{"no sync time", ClaudeReading{Present: true, SevenDayPct: i16(42)}, false, 0},
		{"stale", ClaudeReading{Present: true, SevenDayPct: i16(42), SyncedAt: tp(now.Add(-4 * interval))}, false, 0},
		{"pre-reset", ClaudeReading{Present: true, SevenDayPct: i16(42), SevenDayResetsAt: tp(now.Add(-time.Minute)), SyncedAt: tp(now.Add(-time.Hour))}, false, 0},
	}
	for _, c := range cases {
		got := ClaudeWeekly(c.r, interval, now)
		if got.OK != c.wantOK || got.Pct != c.want {
			t.Errorf("%s: ClaudeWeekly = %+v, want ok=%v pct=%v", c.name, got, c.wantOK, c.want)
		}
		// The threshold evaluation is built on the same policy: usable iff not Unknown.
		thr := int16(50)
		if v := EvaluateClaude(&thr, c.r, interval, now); (v.State != Unknown) != c.wantOK {
			t.Errorf("%s: EvaluateClaude state %v disagrees with ClaudeWeekly OK=%v", c.name, v.State, c.wantOK)
		}
	}
	if got := ClaudeWeekly(ClaudeReading{Present: true, SevenDayPct: i16(42), SevenDayResetsAt: &future, SyncedAt: &synced}, interval, now); got.ResetsAt == nil || !got.ResetsAt.Equal(future) {
		t.Errorf("ResetsAt = %v, want %v", got.ResetsAt, future)
	}
}

func TestCodexWeeklyPolicyAndWindows(t *testing.T) {
	last := now.Add(-time.Minute)
	read := func(buckets ...apitypes.CodexRateLimitBucketDTO) CodexReading {
		return CodexReading{Present: true, Buckets: buckets, LastSuccessAt: &last}
	}
	fiveHour := &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(99), LimitWindowSeconds: i64(18000), ResetAfterSeconds: i64(60)}
	t.Run("lists every weekly window with bucket and slot, highest decides", func(t *testing.T) {
		got := CodexWeekly(read(
			apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: fiveHour, Secondary: weeklyWin(30, 3600)},
			apitypes.CodexRateLimitBucketDTO{ID: "other", Primary: weeklyWin(120, 7200)},
		), interval, now)
		if !got.OK || got.Pct != 100 {
			t.Fatalf("got %+v, want OK with the clamped highest window 100", got)
		}
		if len(got.Windows) != 2 {
			t.Fatalf("windows = %+v, want the two weekly windows (the 5h window is not weekly)", got.Windows)
		}
		if w := got.Windows[0]; w.BucketID != "codex" || w.Slot != "secondary" || w.Pct != 30 || w.ResetsAt == nil {
			t.Errorf("window 0 = %+v", w)
		}
		if w := got.Windows[1]; w.BucketID != "other" || w.Slot != "primary" || w.Pct != 100 {
			t.Errorf("window 1 = %+v", w)
		}
		if want := last.Add(7200 * time.Second); got.ResetsAt == nil || !got.ResetsAt.Equal(want) {
			t.Errorf("ResetsAt = %v, want the highest window's reset %v", got.ResetsAt, want)
		}
	})
	t.Run("an invalid weekly window makes the whole reading not OK", func(t *testing.T) {
		bad := &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: i64(WeeklyWindowSeconds), ResetAfterSeconds: i64(60)}
		got := CodexWeekly(read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: weeklyWin(10, 3600), Secondary: bad}), interval, now)
		if got.OK || len(got.Windows) != 0 {
			t.Errorf("got %+v, want a not-OK reading with no windows (fail closed)", got)
		}
	})
	t.Run("stale, missing and window-less readings are not OK", func(t *testing.T) {
		old := now.Add(-time.Hour)
		for name, r := range map[string]CodexReading{
			"missing":    {},
			"stale":      {Present: true, LastSuccessAt: &old, Buckets: []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: weeklyWin(10, 60)}}},
			"no weekly":  read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: fiveHour}),
			"pre-reset":  read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(10), LimitWindowSeconds: i64(WeeklyWindowSeconds), ResetAt: i64(now.Add(-30 * time.Second).Unix())}}),
			"no success": {Present: true},
		} {
			if got := CodexWeekly(r, interval, now); got.OK {
				t.Errorf("%s: got %+v, want not OK", name, got)
			}
			thr := int16(50)
			if v := EvaluateCodex(&thr, r, interval, now); v.State != Unknown {
				t.Errorf("%s: EvaluateCodex state = %v, want Unknown", name, v.State)
			}
		}
	})
	t.Run("EvaluateCodex keeps its Weekly windows", func(t *testing.T) {
		thr := int16(50)
		v := EvaluateCodex(&thr, read(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: weeklyWin(60, 3600), Secondary: weeklyWin(20, 7200)}), interval, now)
		if v.State != Held || len(v.Weekly) != 2 || v.Weekly[0].Pct != 60 || v.Weekly[1].Pct != 20 {
			t.Errorf("verdict = %+v", v)
		}
	})
}
