package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCodexAdmissionFormatting(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 34, 56, 123456789, time.FixedZone("offset", 3600))
	for _, tc := range []struct{ class, want string }{
		{"rate_limit", "waiting: provider reported a rate limit; account admission eligible after 2026-10-27T11:34:56.123456789Z"},
		{"overload", "waiting: provider reported overload; account admission eligible after 2026-10-27T11:34:56.123456789Z"},
		{"unknown", ""},
	} {
		got := CodexAdmissionReason(tc.class, at)
		if got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.class, got, tc.want)
		}
		if IsCodexAdmissionReason(got) != (tc.want != "") {
			t.Fatalf("detector disagrees for %q", got)
		}
	}
	if IsCodexAdmissionReason("waiting: uzi is in maintenance mode") {
		t.Fatal("maintenance is not an admission reason")
	}
}

func TestCodexAdmissionSummaryAndOmission(t *testing.T) {
	for _, value := range []any{RunDTO{}, RunListItemDTO{}, RunSummaryItemDTO{}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "codex_admission_") {
			t.Fatalf("default fields must be omitted: %s", data)
		}
	}
	class := "overload"
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	item := RunListItemDTO{RunDTO: RunDTO{CodexAdmissionClass: &class, CodexAdmissionEligibleAt: &at}}
	summary := RunSummaryOf(item)
	restored := summary.ListItem()
	if summary.CodexAdmissionClass == nil || *summary.CodexAdmissionClass != class ||
		summary.CodexAdmissionEligibleAt == nil || !summary.CodexAdmissionEligibleAt.Equal(at) ||
		restored.CodexAdmissionClass == nil || *restored.CodexAdmissionClass != class ||
		restored.CodexAdmissionEligibleAt == nil || !restored.CodexAdmissionEligibleAt.Equal(at) {
		t.Fatal("summary round-trip lost admission fields")
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"codex_admission_class":"overload"`, `"codex_admission_eligible_at":"2026-10-27T12:00:00Z"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s", want)
		}
	}
}
