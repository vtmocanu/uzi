package capability

import (
	"reflect"
	"testing"
)

func TestCodexProviderProtocolVocabulary(t *testing.T) {
	if CodexProviderV1 != "codex_provider_v1" {
		t.Fatal("wire name drift")
	}
	got := FilterProtocol([]string{CodexProviderV1, ClaudeProviderV1, CodexProviderV1, "unknown"})
	if !reflect.DeepEqual(got, []string{ClaudeProviderV1, CodexProviderV1}) {
		t.Fatalf("protocol order = %v", got)
	}
	if len(Filter([]string{CodexProviderV1})) != 0 || len(SelfReportable([]string{CodexProviderV1})) != 0 {
		t.Fatal("provider protocol leaked into scheduler vocabulary")
	}
}

func TestCodexProviderCheckerProtocolVocabulary(t *testing.T) {
	if CodexProviderCheckerV1 != "codex_provider_checker_v1" {
		t.Fatal("wire name drift")
	}
	got := FilterProtocol([]string{CodexProviderCheckerV1, CodexProviderJudgeV1, CodexProviderCheckerV1})
	if !reflect.DeepEqual(got, []string{CodexProviderJudgeV1, CodexProviderCheckerV1}) {
		t.Fatalf("protocol order = %v", got)
	}
	if len(Filter([]string{CodexProviderCheckerV1})) != 0 || len(SelfReportable([]string{CodexProviderCheckerV1})) != 0 {
		t.Fatal("checker protocol leaked into scheduler vocabulary")
	}
}
