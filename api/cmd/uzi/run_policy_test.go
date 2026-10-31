package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunGetPolicyHoldCapture(t *testing.T) {
	yes, no := true, false
	head := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name   string
		policy *apitypes.PolicyHoldDTO
		want   string
	}{
		{"published", &apitypes.PolicyHoldDTO{CapturedHead: &head, Published: &yes}, "worker-reported published checkpoint"},
		{"verified local", &apitypes.PolicyHoldDTO{CapturedHead: &head, Published: &no}, "worker-reported verified local checkpoint"},
		{"local only", &apitypes.PolicyHoldDTO{Published: &no}, "local-only/unverified preservation"},
		{"empty head", &apitypes.PolicyHoldDTO{CapturedHead: sp(""), Published: &no}, "local-only/unverified preservation"},
		{"published without head", &apitypes.PolicyHoldDTO{Published: &yes}, "unverified preservation"},
		{"unknown publication", &apitypes.PolicyHoldDTO{CapturedHead: &head}, "unverified preservation"},
		{"missing evidence", nil, "unverified preservation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := heldRun("r1", "codex_policy_hold", time.Now())
			run.PolicyHold = tc.policy
			// Generic capture fields cannot replace this episode's evidence.
			run.CheckpointContainsLatest = &yes
			if run.PolicyHold != nil {
				run.PolicyHold.OutcomeID = "opaque-outcome"
				run.PolicyHold.CorrelationID = sp("opaque-correlation")
				run.PolicyHold.PolicyTag = sp("opaque-provider-text")
			}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			line := lineWith(t, out, "HOLD")
			if !strings.Contains(line, " · "+tc.want+" · ") {
				t.Errorf("capture clause must be exactly %q: %s", tc.want, line)
			}
			for _, want := range []string{"Codex policy hold confirmed by server", tc.want, "Resume and Stop unavailable", "Cancel: uzi run cancel r1"} {
				if !strings.Contains(line, want) {
					t.Errorf("HOLD missing %q: %s", want, line)
				}
			}
			for _, bad := range []string{"opaque-", "uzi run resume", "uzi run stop", "conversation available"} {
				if strings.Contains(out, bad) {
					t.Errorf("unexpected %q: %s", bad, out)
				}
			}
		})
	}
}

func TestRunGetPolicyPendingAndOrdinaryControl(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason, hold, want string
		pending                          bool
	}{
		{"pending", "running", "codex_policy_hold_pending", "", "delivery pending; server hold not confirmed", true},
		{"paused pending", "paused", "codex_policy_hold_pending", "", "delivery pending; server hold not confirmed", true},
		{"terminal pending", "completed", "codex_policy_hold_pending", "", "", false},
		{"failed pending", "failed", "codex_policy_hold_pending", "", "", false},
		{"cancelled pending", "cancelled", "codex_policy_hold_pending", "", "", false},
		{"ordinary pending", "running", "gap_unrecoverable", "", "", false},
		{"ordinary hold", "paused", "", holdBudgetExhausted, "extend: uzi run extend r1 --by 2h", false},
		{"stale policy reason", "running", "", "codex_policy_hold", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := heldRun("r1", tc.hold, time.Now())
			run.Status = tc.status
			if tc.reason != "" {
				run.OutcomePending = &apitypes.OutcomePendingDTO{Reason: tc.reason}
			}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("missing %q: %s", tc.want, out)
			}
			if strings.Contains(out, "POLICY_PENDING") != tc.pending {
				t.Errorf("pending visibility = %t, want %t: %s", strings.Contains(out, "POLICY_PENDING"), tc.pending, out)
			}
			if tc.pending {
				notice := pausedHoldNotice(run)
				if !strings.Contains(notice, tc.want) || strings.Contains(notice, "uzi run resume") {
					t.Errorf("pending notice = %q", notice)
				}
				for _, want := range []string{"Resume and Stop unavailable", "Cancel: uzi run cancel r1"} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %q: %s", want, out)
					}
				}
			}
			if tc.hold != holdBudgetExhausted && (strings.Contains(out, "HOLD ") || strings.Contains(out, "uzi run resume")) {
				t.Errorf("unconfirmed hold or generic resume advice: %s", out)
			}
		})
	}
}

func TestPolicyHoldRenderProfilesAndConfirmation(t *testing.T) {
	for _, color := range []bool{false, true} {
		var buf bytes.Buffer
		t.Setenv("NO_COLOR", "")
		p := uzicli.NewPrinter(&buf, color, false, !color, false)
		run := heldRun("r1", "codex_policy_hold", time.Now())
		run.OutcomePending = &apitypes.OutcomePendingDTO{Reason: apitypes.OutcomePendingPolicyHold}
		if err := renderRunDetail(p, run); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"Codex policy hold confirmed by server", "unverified preservation", "Cancel: uzi run cancel r1"} {
			if !strings.Contains(out, want) {
				t.Errorf("color=%t missing %q: %s", color, want, out)
			}
		}
		if strings.Contains(out, "POLICY_PENDING") {
			t.Errorf("confirmed hold rendered as pending: %s", out)
		}
	}
	for _, status := range []string{"queued", "claimed", "running", "completed", "failed", "cancelled"} {
		run := heldRun("r1", "codex_policy_hold", time.Now())
		run.Status = status
		if row := holdRow(run, time.Now()); row != nil {
			t.Errorf("status %s rendered a confirmed hold: %v", status, row)
		}
	}
}

func TestPolicyHoldNoticeAndSanitization(t *testing.T) {
	run := heldRun("r1\nforged\t\u202e\x1b", "codex_policy_hold", time.Now())
	run.PolicyHold = &apitypes.PolicyHoldDTO{CapturedHead: sp("head\nforged\t\u202e\x1b")}
	for _, text := range []string{strings.Join(holdRow(run, time.Now()), " "), pausedHoldNotice(run)} {
		for _, bad := range []rune{'\n', '\t', '\u202e', '\x1b'} {
			if strings.ContainsRune(text, bad) {
				t.Errorf("control %U in %q", bad, text)
			}
		}
		for _, want := range []string{"Codex policy hold confirmed by server", "Resume and Stop unavailable", "Cancel: uzi run cancel " + cellText(run.ID)} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q: %s", want, text)
			}
		}
		if strings.Contains(text, "uzi run resume") {
			t.Errorf("generic resume advice: %s", text)
		}
	}
	run.OutcomePending = &apitypes.OutcomePendingDTO{Reason: apitypes.OutcomePendingPolicyHold}
	run.HoldReason = nil
	if notice := pausedHoldNotice(run); strings.ContainsAny(notice, "\n\t\u202e\x1b") ||
		!strings.Contains(notice, "Cancel: uzi run cancel "+cellText(run.ID)) {
		t.Errorf("unsafe pending notice: %q", notice)
	}
	ordinary := heldRun("r1", "", time.Now())
	if got := pausedHoldNotice(ordinary); !strings.Contains(got, "resume with uzi run resume r1") {
		t.Errorf("ordinary pause changed: %s", got)
	}
}
