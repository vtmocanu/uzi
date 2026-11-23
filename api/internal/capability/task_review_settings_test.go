package capability

import (
	"slices"
	"testing"
)

func TestTaskReviewSettingsProtocol(t *testing.T) {
	const name = "task_review_settings_v1"
	if TaskReviewSettingsV1 != name {
		t.Fatal(TaskReviewSettingsV1)
	}
	got := FilterProtocol([]string{TaskReviewSettingsV1, CrossCheckV1, TemplateEffortV1, name, "unknown"})
	want := []string{CrossCheckV1, TemplateEffortV1, name}
	if !slices.Equal(got, want) {
		t.Fatalf("protocol order/allowlist/dedup: %v", got)
	}
	if len(Filter([]string{name})) != 0 || len(SelfReportable([]string{name})) != 0 || slices.Contains(Vocabulary(), name) {
		t.Fatal("protocol name entered scheduler vocabulary")
	}
	if _, ok := protocolVocabulary[name]; !ok || !slices.Contains(protocolOrder, name) {
		t.Fatal("name missing from vocabulary or order")
	}
}
