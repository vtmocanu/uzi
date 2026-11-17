package workersvc

import (
	"strings"
	"testing"
)

// TestHealthReasonsNameAdminWorkers pins issue #4423: the Docker worker repo allowlist and
// the custody hold limit live on Admin → Workers (#4212), so the three run-health reasons
// that send an admin to them must name that tab, never the old Admin → Instance location.
func TestHealthReasonsNameAdminWorkers(t *testing.T) {
	for name, reason := range map[string]string{
		"reasonRepoNotDockerAllowedAdmin":  reasonRepoNotDockerAllowedAdmin,
		"reasonRepoNotDockerAllowedMember": reasonRepoNotDockerAllowedMember,
		"reasonCustodyLimitUnavailable":    reasonCustodyLimitUnavailable,
	} {
		if !strings.Contains(reason, "Admin → Workers") {
			t.Errorf("%s = %q, want it to name Admin → Workers", name, reason)
		}
		if strings.Contains(reason, "Admin → Instance") {
			t.Errorf("%s = %q, still names Admin → Instance", name, reason)
		}
	}
	if !strings.Contains(reasonRepoNotDockerAllowedAdmin, "Admin → Workers → Docker worker repo allowlist") ||
		!strings.Contains(reasonRepoNotDockerAllowedMember, "Admin → Workers → Docker worker repo allowlist") {
		t.Errorf("docker allowlist reasons must name the Admin → Workers → Docker worker repo allowlist card")
	}
}

// TestQueuedReasonPrecedesCodexAdmissionAcceptsLegacyCustodyText pins the rolling-upgrade
// reader: a run row persisted by an older server still carries the Admin → Instance custody
// text until the next health tick rewrites it, and it must keep the same precedence over
// temporary Codex admission as the new text (issue #4423: no precedence change).
func TestQueuedReasonPrecedesCodexAdmissionAcceptsLegacyCustodyText(t *testing.T) {
	const legacy = "new runs are paused: the custody hold limit setting could not be read; an admin should check Admin → Instance settings"
	if reasonCustodyLimitUnavailableLegacy != legacy {
		t.Fatalf("reasonCustodyLimitUnavailableLegacy = %q, want the pre-#4423 text %q", reasonCustodyLimitUnavailableLegacy, legacy)
	}
	if reasonCustodyLimitUnavailable == legacy {
		t.Fatal("the emitted custody-unavailable reason must not be the legacy text")
	}
	for _, reason := range []string{reasonCustodyLimitUnavailable, legacy} {
		if !QueuedReasonPrecedesCodexAdmission(reason) {
			t.Errorf("QueuedReasonPrecedesCodexAdmission(%q) = false, want true", reason)
		}
	}
}
