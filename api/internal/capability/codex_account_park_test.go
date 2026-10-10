package capability

import (
	"reflect"
	"testing"
)

func TestCodexAccountParkProtocolVocabulary(t *testing.T) {
	got := FilterProtocol([]string{CodexAccountParkV1, CodexHarnessV1, CodexAccountParkV1, "unknown"})
	if !reflect.DeepEqual(got, []string{CodexHarnessV1, CodexAccountParkV1}) {
		t.Fatalf("protocol filter: %v", got)
	}
	if len(Filter([]string{CodexAccountParkV1})) != 0 {
		t.Fatal("protocol token became a scheduler capability")
	}
}
