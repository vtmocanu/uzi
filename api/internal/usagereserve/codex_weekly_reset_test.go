package usagereserve

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCodexMainWeeklyReset(t *testing.T) {
	const week, fiveH = 604800, 18000
	readAt := now.Add(-time.Minute)
	soon := now.Add(10 * time.Hour)
	later := now.Add(60 * time.Hour)
	passed := now.Add(-30 * time.Second) // after readAt, before now: reset since the reading

	wk := func(resetAt time.Time) *apitypes.CodexRateLimitWindowDTO {
		return win(40, week, resetAt.Unix())
	}
	noReset := &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(40), LimitWindowSeconds: i64(week)}
	relative := &apitypes.CodexRateLimitWindowDTO{UsedPercent: f64(40), LimitWindowSeconds: i64(week), ResetAfterSeconds: i64(3600)}
	main := func(p, s *apitypes.CodexRateLimitWindowDTO) apitypes.CodexRateLimitBucketDTO {
		return apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: p, Secondary: s}
	}

	cases := []struct {
		name    string
		buckets []apitypes.CodexRateLimitBucketDTO
		want    *time.Time
	}{
		{"weekly in primary", []apitypes.CodexRateLimitBucketDTO{main(wk(soon), nil)}, tp(soon)},
		{"weekly in secondary", []apitypes.CodexRateLimitBucketDTO{main(win(10, fiveH, later.Unix()), wk(soon))}, tp(soon)},
		{"both weekly, primary sooner", []apitypes.CodexRateLimitBucketDTO{main(wk(soon), wk(later))}, tp(soon)},
		{"both weekly, secondary sooner", []apitypes.CodexRateLimitBucketDTO{main(wk(later), wk(soon))}, tp(soon)},
		{"no reset reported", []apitypes.CodexRateLimitBucketDTO{main(noReset, nil)}, nil},
		{"reset_after_seconds relative to the reading", []apitypes.CodexRateLimitBucketDTO{main(relative, nil)}, tp(readAt.Add(time.Hour))},
		{"pre-reset window is skipped", []apitypes.CodexRateLimitBucketDTO{main(wk(passed), nil)}, nil},
		{"pre-reset window skipped, other weekly kept", []apitypes.CodexRateLimitBucketDTO{main(wk(passed), wk(later))}, tp(later)},
		{"non-main bucket ignored", []apitypes.CodexRateLimitBucketDTO{{ID: "codex_other", Primary: wk(soon)}, main(wk(later), nil)}, tp(later)},
		{"only a non-main bucket", []apitypes.CodexRateLimitBucketDTO{{ID: "codex_other", Primary: wk(soon)}}, nil},
		{"5h-only main bucket", []apitypes.CodexRateLimitBucketDTO{main(win(10, fiveH, soon.Unix()), nil)}, nil},
		{"no buckets", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CodexMainWeeklyReset(tc.buckets, readAt, now)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %v, want nil", *got)
			case tc.want != nil && (got == nil || !got.Equal(*tc.want)):
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}
}
