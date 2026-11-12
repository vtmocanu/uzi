package workersvc

import (
	"strings"
	"testing"
	"time"
)

// A worker whose hibernation_state is not none reports neither a roll nor a version
// verdict (PRD #4205), even with a fresh stuck signal and an outdated version.
func TestClassifyUpgradeHibernationPrecedence(t *testing.T) {
	now := time.Now()
	signal := &RollSignal{Phase: PhaseStuck, ObservedAt: now, BlockingReason: "ImagePullBackOff"}
	for _, state := range []string{"preparing", "hibernated"} {
		status, detail := ClassifyUpgrade(UpgradeInput{
			Reported: "0.1.0", Kind: "hosted", CPVersion: "0.84.0", PinnedWorkerVersion: "0.84.0",
			Signal: signal, Now: now, APIStartedAt: now, Hibernation: state,
		}, UpgradeParams{})
		if status == UpgradeStatusOutdated || status == UpgradeStatusUpgrading || status == UpgradeStatusUpgradeFailed {
			t.Errorf("%s: status %q leaked a roll/version verdict", state, status)
		}
		if want := map[string]string{"preparing": "hibernating"}[state]; want != "" && !strings.Contains(detail, want) {
			t.Errorf("%s: detail %q does not name %q", state, detail, want)
		}
	}
	// A waking worker has a pod coming up: normal classification, so upgrade_failed shows.
	if status, _ := ClassifyUpgrade(UpgradeInput{
		Reported: "0.1.0", Kind: "hosted", CPVersion: "0.84.0", PinnedWorkerVersion: "0.84.0",
		Signal: signal, Now: now, APIStartedAt: now, Hibernation: "waking",
	}, UpgradeParams{}); status != UpgradeStatusUpgradeFailed {
		t.Errorf("waking: status %q, want upgrade_failed (not short-circuited)", status)
	}
	// "none" and "" keep the ordinary classification.
	for _, state := range []string{"", "none"} {
		status, _ := ClassifyUpgrade(UpgradeInput{
			Reported: "0.1.1", Kind: "external", CPVersion: "0.84.0", Now: now, APIStartedAt: now.Add(-24 * time.Hour), Hibernation: state,
		}, UpgradeParams{})
		if status != UpgradeStatusOutdated {
			t.Errorf("hibernation %q: status %q, want outdated", state, status)
		}
	}
}

func TestSanitizeHibernationText(t *testing.T) {
	long := strings.Repeat("a", 500) + " b"
	if _, tooLong := sanitizeHibernationText(long); !tooLong {
		t.Error("502-rune reason accepted")
	}
	exact := strings.Repeat("é", 500)
	if got, tooLong := sanitizeHibernationText(exact); tooLong || got != exact {
		t.Errorf("500-rune reason refused or altered (tooLong=%v len=%d)", tooLong, len([]rune(got)))
	}
	got, tooLong := sanitizeHibernationText("ok\x1b[31m red\x1b[0m \u202eevil\u2066 done")
	if tooLong {
		t.Error("short reason refused")
	}
	for _, bad := range []string{"\x1b", "\u202e", "\u2066"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitized %q still contains %q", got, bad)
		}
	}
	if !strings.Contains(got, "ok") || !strings.Contains(got, "done") {
		t.Errorf("sanitized %q lost visible text", got)
	}
}
