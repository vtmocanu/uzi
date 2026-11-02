package codexselect

import (
	"math"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

var (
	t0       = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	interval = 5 * time.Minute
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
func bp(v bool) *bool        { return &v }

func win(used float64, resetAfter int64) *apitypes.CodexRateLimitWindowDTO {
	return &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(used), ResetAfterSeconds: i64(resetAfter)}
}

func reading(buckets ...apitypes.CodexRateLimitBucketDTO) Reading {
	at := t0.Add(-time.Minute)
	return Reading{Present: true, Buckets: buckets, LastSuccessAt: &at}
}

func codexBucket(p, s *apitypes.CodexRateLimitWindowDTO) apitypes.CodexRateLimitBucketDTO {
	return apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: p, Secondary: s}
}

func TestScoreKnownCases(t *testing.T) {
	cases := []struct {
		name      string
		r         Reading
		wantScore float64
		wantReset time.Duration // after LastSuccessAt; 0 = nil
	}{
		{"primary binds", reading(codexBucket(win(30, 1000), win(10, 9000))), 70, 1000 * time.Second},
		{"secondary binds", reading(codexBucket(win(10, 1000), win(60, 9000))), 40, 9000 * time.Second},
		{"single window", reading(codexBucket(nil, win(25, 500))), 75, 500 * time.Second},
		{"over 100 clamps", reading(codexBucket(win(250, 700), nil)), 0, 700 * time.Second},
		{"zero used", reading(codexBucket(win(0, 700), nil)), 100, 700 * time.Second},
		{"tie prefers primary", reading(codexBucket(win(50, 100), win(50, 200))), 50, 100 * time.Second},
		{"feature bucket ignored", reading(
			apitypes.CodexRateLimitBucketDTO{ID: "feature_x", Primary: win(99, 5000)},
			codexBucket(win(20, 500), nil)), 80, 500 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			score, reset, ok := Score(c.r, true, interval, t0)
			if !ok || score != c.wantScore {
				t.Fatalf("score=%v ok=%v, want %v true", score, ok, c.wantScore)
			}
			if reset == nil || !reset.Equal(c.r.LastSuccessAt.Add(c.wantReset)) {
				t.Fatalf("reset=%v, want %v", reset, c.r.LastSuccessAt.Add(c.wantReset))
			}
		})
	}
}

func TestScoreUnknownCases(t *testing.T) {
	stale := t0.Add(-time.Hour)
	pastReset := &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(10), ResetAt: i64(t0.Add(-30 * time.Second).Unix())}
	cases := []struct {
		name  string
		r     Reading
		fresh bool
	}{
		{"meter not fresh", reading(codexBucket(win(10, 5000), nil)), false},
		{"not present", Reading{}, true},
		{"no last success", Reading{Present: true, Buckets: []apitypes.CodexRateLimitBucketDTO{codexBucket(win(10, 5000), nil)}}, true},
		{"stale", Reading{Present: true, LastSuccessAt: &stale, Buckets: []apitypes.CodexRateLimitBucketDTO{codexBucket(win(10, 5000), nil)}}, true},
		{"bucket missing", reading(apitypes.CodexRateLimitBucketDTO{ID: "other", Primary: win(1, 5000)}), true},
		{"no buckets", reading(), true},
		{"no windows", reading(codexBucket(nil, nil)), true},
		{"nil used percent", reading(codexBucket(&apitypes.CodexRateLimitWindowDTO{}, nil)), true},
		{"nil used in secondary only", reading(codexBucket(win(10, 5000), &apitypes.CodexRateLimitWindowDTO{})), true},
		{"NaN", reading(codexBucket(win(math.NaN(), 5000), nil)), true},
		{"+Inf", reading(codexBucket(win(math.Inf(1), 5000), nil)), true},
		{"-Inf", reading(codexBucket(win(math.Inf(-1), 5000), nil)), true},
		{"negative", reading(codexBucket(win(-1, 5000), nil)), true},
		{"pre-reset binding window", reading(codexBucket(pastReset, nil)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if score, reset, ok := Score(c.r, c.fresh, interval, t0); ok || score != 0 || reset != nil {
				t.Fatalf("got (%v, %v, %v), want unknown", score, reset, ok)
			}
		})
	}
}

func TestScorePollerDisabledIsUnknown(t *testing.T) {
	if _, _, ok := Score(reading(codexBucket(win(10, 5000), nil)), true, 0, t0); ok {
		t.Fatal("interval 0 (poller off) must make every reading stale")
	}
}

func TestScoreNoResetStillKnown(t *testing.T) {
	r := reading(codexBucket(&apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(40)}, nil))
	score, reset, ok := Score(r, true, interval, t0)
	if !ok || score != 60 || reset != nil {
		t.Fatalf("got (%v, %v, %v), want (60, nil, true)", score, reset, ok)
	}
}

func TestLimitReachedReadsCodexBucketOnly(t *testing.T) {
	cases := []struct {
		name string
		b    []apitypes.CodexRateLimitBucketDTO
		want bool
	}{
		{"none", nil, false},
		{"limit reached", []apitypes.CodexRateLimitBucketDTO{{ID: "codex", LimitReached: bp(true)}}, true},
		{"not allowed", []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Allowed: bp(false)}}, true},
		{"allowed and not reached", []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Allowed: bp(true), LimitReached: bp(false)}}, false},
		{"unreported signals", []apitypes.CodexRateLimitBucketDTO{{ID: "codex"}}, false},
		{"feature bucket reached", []apitypes.CodexRateLimitBucketDTO{
			{ID: "feature_x", LimitReached: bp(true), Allowed: bp(false)}, {ID: "codex", Allowed: bp(true)}}, false},
	}
	for _, c := range cases {
		if got := LimitReached(c.b); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
