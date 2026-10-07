package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCodexLimitProviderReasons(t *testing.T) {
	reset := parkNow.Add(2 * time.Hour)
	for _, provider := range []struct{ harness, name string }{{"codex", "Codex"}, {"claude", "Anthropic"}} {
		t.Run(provider.harness, func(t *testing.T) {
			want := provider.name + " usage limit (seven_day) reached; resets at " + reset.UTC().Format(time.RFC3339)
			in := parkIn()
			in.Harness, in.ReportedType = provider.harness, strPtr("seven_day")
			in.WaitOnLimit = false
			if got := decideLimitPark(in).Reason; got != want {
				t.Fatalf("opt-out = %q, want %q", got, want)
			}
			in.WaitOnLimit, in.MaxWaits = true, 0
			if got := decideLimitPark(in).Reason; got != want+"; usage-limit retry budget exhausted after 0 attempt(s)" {
				t.Fatalf("budget = %q", got)
			}
			in.MaxWaits, in.MaxPark = 5, time.Hour
			if got := decideLimitPark(in).Reason; got != want+"; the window reopens further out than the 1h0m0s maximum park" {
				t.Fatalf("max park = %q", got)
			}
			for _, state := range []string{"failed", "limit_wait", "recovery_wait"} {
				run := runningRun(false)
				run.Harness = provider.harness
				fs, svc, w := limitParkFixture(t, run)
				req := StateRequest{State: state, RateLimitType: strPtr("seven_day"), LimitResetsAt: ms(2 * time.Hour)}
				if state == "recovery_wait" {
					req.RecoveryCause = strPtr("forge_unreachable")
				}
				if _, applied, err := svc.SetState(context.Background(), w, run.ID, req); err != nil || !applied {
					t.Fatalf("%s: applied=%v err=%v", state, applied, err)
				}
				if fs.setFailed == nil || fs.setFailed.FailureReason.String != want {
					t.Fatalf("%s failure = %+v, want %q", state, fs.setFailed, want)
				}
			}
			req := StateRequest{FailureReason: strPtr("ordinary\x00failure")}
			got := limitAwareFailureReason(provider.harness, req)
			if !got.Valid || got.String != "ordinaryfailure" {
				t.Fatalf("ordinary failure = %+v", got)
			}
			req.RateLimitType = strPtr("seven_day<script>")
			req.LimitResetsAt = ms(2 * time.Hour)
			if got := limitAwareFailureReason(provider.harness, req).String; strings.Contains(got, "script") || got != provider.name+" usage limit (unknown) reached; resets at "+reset.UTC().Format(time.RFC3339) {
				t.Fatalf("sanitized reason = %q", got)
			}
		})
	}
}
