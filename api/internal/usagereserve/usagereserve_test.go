package usagereserve

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

const interval = 5 * time.Minute

func i16(v int16) *int16        { return &v }
func i64(v int64) *int64        { return &v }
func f64(v float64) *float64    { return &v }
func tp(t time.Time) *time.Time { return &t }

func TestStale(t *testing.T) {
	cases := []struct {
		name     string
		synced   time.Time
		valid    bool
		interval time.Duration
		want     bool
	}{
		{"fresh", now.Add(-time.Minute), true, interval, false},
		{"exactly 3x is not stale", now.Add(-3 * interval), true, interval, false},
		{"just past 3x", now.Add(-3*interval - time.Second), true, interval, true},
		{"invalid sync time", now, false, interval, true},
		{"poller disabled", now, true, 0, true},
		{"negative interval", now, true, -time.Second, true},
	}
	for _, c := range cases {
		if got := Stale(c.synced, c.valid, c.interval, now); got != c.want {
			t.Errorf("%s: Stale = %v, want %v", c.name, got, c.want)
		}
	}
}

func claudeFresh(pct int16) ClaudeReading {
	return ClaudeReading{Present: true, SevenDayPct: i16(pct), SyncedAt: tp(now.Add(-time.Minute)),
		SevenDayResetsAt: tp(now.Add(48 * time.Hour))}
}

func TestEvaluateClaude(t *testing.T) {
	cases := []struct {
		name      string
		threshold *int16
		r         ClaudeReading
		interval  time.Duration
		want      State
		wantPct   int
	}{
		{"nil threshold is ok even when high", nil, claudeFresh(99), interval, OK, 0},
		{"nil threshold is ok without reading", nil, ClaudeReading{}, interval, OK, 0},
		{"pct == threshold is held", i16(70), claudeFresh(70), interval, Held, 70},
		{"above threshold held", i16(70), claudeFresh(72), interval, Held, 72},
		{"threshold-1 is ok", i16(70), claudeFresh(69), interval, OK, 69},
		{"threshold 100 held at 100", i16(100), claudeFresh(100), interval, Held, 100},
		{"threshold 100 ok at 99", i16(100), claudeFresh(99), interval, OK, 99},
		{"missing row unknown", i16(70), ClaudeReading{}, interval, Unknown, 0},
		{"nil pct unknown", i16(70), ClaudeReading{Present: true, SyncedAt: tp(now)}, interval, Unknown, 0},
		{"nil synced unknown", i16(70), ClaudeReading{Present: true, SevenDayPct: i16(10)}, interval, Unknown, 0},
		{"stale unknown even when high", i16(70), ClaudeReading{Present: true, SevenDayPct: i16(99),
			SyncedAt: tp(now.Add(-time.Hour))}, interval, Unknown, 0},
		{"poller disabled unknown", i16(70), claudeFresh(10), 0, Unknown, 0},
		{"reset passed, synced before it: unknown", i16(70), ClaudeReading{Present: true, SevenDayPct: i16(99),
			SyncedAt: tp(now.Add(-2 * time.Minute)), SevenDayResetsAt: tp(now.Add(-time.Minute))}, interval, Unknown, 0},
		{"reset passed, synced after it: evaluated", i16(70), ClaudeReading{Present: true, SevenDayPct: i16(3),
			SyncedAt: tp(now.Add(-time.Minute)), SevenDayResetsAt: tp(now.Add(-2 * time.Minute))}, interval, OK, 3},
		{"reset in the future: evaluated", i16(70), claudeFresh(80), interval, Held, 80},
	}
	for _, c := range cases {
		v := EvaluateClaude(c.threshold, c.r, c.interval, now)
		if v.State != c.want {
			t.Errorf("%s: state = %v, want %v (%+v)", c.name, v.State, c.want, v)
			continue
		}
		if c.want != Unknown && c.threshold != nil && v.Pct != c.wantPct {
			t.Errorf("%s: pct = %d, want %d", c.name, v.Pct, c.wantPct)
		}
	}
}

func TestVerdictReasons(t *testing.T) {
	held := EvaluateClaude(i16(70), claudeFresh(72), interval, now)
	if held.Reason != "weekly usage reserve: 72% >= 70%" || held.Threshold != 70 || !held.Fresh || held.ResetsAt == nil {
		t.Errorf("held verdict = %+v", held)
	}
	unk := EvaluateClaude(i16(70), ClaudeReading{}, interval, now)
	if unk.Reason != "weekly usage reserve: weekly usage unknown" || unk.Fresh {
		t.Errorf("unknown verdict = %+v", unk)
	}
	ok := EvaluateClaude(i16(70), claudeFresh(10), interval, now)
	if ok.Reason != "" || !ok.Fresh {
		t.Errorf("ok verdict = %+v", ok)
	}
	if v := Exempt(); v.State != OK || v.Reason != "" {
		t.Errorf("Exempt = %+v", v)
	}
	if OK.String() != "ok" || Held.String() != "held" || Unknown.String() != "unknown" {
		t.Error("State.String mismatch")
	}
}

func win(used float64, seconds int64, resetAt int64) *apitypes.CodexRateLimitWindowDTO {
	return &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(used), LimitWindowSeconds: i64(seconds), ResetAt: i64(resetAt)}
}

func codexAt(buckets ...apitypes.CodexRateLimitBucketDTO) CodexReading {
	return CodexReading{Present: true, Buckets: buckets, LastSuccessAt: tp(now.Add(-time.Minute))}
}

func TestEvaluateCodex(t *testing.T) {
	future := now.Add(48 * time.Hour).Unix()
	const week, fiveH = 604800, 18000
	cases := []struct {
		name string
		r    CodexReading
		want State
		pct  int
	}{
		{"7d in secondary", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: win(10, fiveH, future), Secondary: win(75, week, future)}), Held, 75},
		{"7d in primary (swapped)", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: win(75, week, future), Secondary: win(10, fiveH, future)}), Held, 75},
		{"5h high, 7d low: ok", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: win(99, fiveH, future), Secondary: win(20, week, future)}), OK, 20},
		{"two weekly buckets: highest decides", codexAt(
			apitypes.CodexRateLimitBucketDTO{ID: "codex", Secondary: win(20, week, future)},
			apitypes.CodexRateLimitBucketDTO{ID: "codex_other", Primary: win(80, week, future)}), Held, 80},
		{"equal to threshold held", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Secondary: win(70, week, future)}), Held, 70},
		{"69.9 not held", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Secondary: win(69.9, week, future)}), OK, 70},
		{"no weekly window unknown", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: win(99, fiveH, future)}), Unknown, 0},
		{"no buckets unknown", codexAt(), Unknown, 0},
		{"weekly window without percent unknown", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex",
			Secondary: &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: i64(week), ResetAt: i64(future)}}), Unknown, 0},
		{"missing row unknown", CodexReading{}, Unknown, 0},
		{"stale unknown", CodexReading{Present: true, LastSuccessAt: tp(now.Add(-time.Hour)),
			Buckets: []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Secondary: win(99, week, future)}}}, Unknown, 0},
		{"no last success unknown", CodexReading{Present: true,
			Buckets: []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Secondary: win(99, week, future)}}}, Unknown, 0},
		{"pre-reset reading unknown", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex",
			Secondary: win(99, week, now.Add(-30*time.Second).Unix())}), Unknown, 0},
		{"reset_after_seconds relative to last success: pre-reset", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex",
			Secondary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(99), LimitWindowSeconds: i64(week), ResetAfterSeconds: i64(10)}}), Unknown, 0},
		{"reset_after_seconds in the future: evaluated", codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex",
			Secondary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(99), LimitWindowSeconds: i64(week), ResetAfterSeconds: i64(3600)}}), Held, 99},
	}
	for _, c := range cases {
		v := EvaluateCodex(i16(70), c.r, interval, now)
		if v.State != c.want {
			t.Errorf("%s: state = %v, want %v (%+v)", c.name, v.State, c.want, v)
			continue
		}
		if c.want != Unknown && v.Pct != c.pct {
			t.Errorf("%s: pct = %d, want %d", c.name, v.Pct, c.pct)
		}
	}
	hi := codexAt(apitypes.CodexRateLimitBucketDTO{ID: "codex", Secondary: win(99, week, future)})
	if v := EvaluateCodex(nil, hi, interval, now); v.State != OK {
		t.Errorf("nil threshold = %+v, want ok", v)
	}
	if v := EvaluateCodex(i16(70), hi, 0, now); v.State != Unknown {
		t.Errorf("poller disabled = %+v, want unknown", v)
	}
}

func TestClaudeResetObserved(t *testing.T) {
	anchor := now.Add(72 * time.Hour)
	cases := []struct {
		name   string
		anchor time.Time
		prior  *int16
		fresh  *time.Time
		pct    int
		want   bool
	}{
		{"arm A: moved a week", anchor, i16(80), tp(anchor.Add(7 * 24 * time.Hour)), 0, true},
		{"arm A: moved just past margin", anchor, nil, tp(anchor.Add(time.Hour + time.Second)), 90, true},
		{"jitter: moved exactly the margin", anchor, i16(80), tp(anchor.Add(time.Hour)), 80, false},
		{"jitter: moved 30m", anchor, i16(50), tp(anchor.Add(30 * time.Minute)), 50, false},
		{"arm B: unmoved, 5 -> 1", anchor, i16(5), tp(anchor), 1, true},
		{"arm B: prior below floor", anchor, i16(4), tp(anchor), 0, false},
		{"arm B: fresh above ceil", anchor, i16(50), tp(anchor), 2, false},
		{"arm B: boundary moved backward", anchor, i16(50), tp(anchor.Add(-time.Minute)), 0, false},
		{"arm B: nil fresh boundary", anchor, i16(50), nil, 0, false},
		{"arm B: nil prior pct", anchor, nil, tp(anchor), 0, false},
		{"zero anchor never observes", time.Time{}, i16(50), tp(anchor), 0, false},
	}
	for _, c := range cases {
		if got := ClaudeResetObserved(c.anchor, c.prior, c.fresh, c.pct); got != c.want {
			t.Errorf("%s: = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCodexResetObserved(t *testing.T) {
	const old = int64(1_900_000_000)
	cases := []struct {
		name       string
		fresh      int64
		oldP, newP *float64
		want       bool
	}{
		{"forward past margin", old + 3601, f64(50), f64(50), true},
		{"forward exactly margin", old + 3600, f64(50), f64(50), false},
		{"cleared on unmoved boundary", old, f64(5), f64(1), true},
		{"unmoved, prior below floor", old, f64(4.9), f64(0), false},
		{"unmoved, fresh above ceil", old, f64(50), f64(1.1), false},
		{"unmoved, nil percent", old, nil, f64(0), false},
		{"unmoved, NaN percent", old, f64(5), f64(nan()), false},
		{"backward", old - 5, f64(50), f64(0), false},
	}
	for _, c := range cases {
		if got := CodexResetObserved(old, c.fresh, c.oldP, c.newP); got != c.want {
			t.Errorf("%s: = %v, want %v", c.name, got, c.want)
		}
	}
}

func nan() float64 { z := 0.0; return z / z }
