package capability

import (
	"reflect"
	"testing"
)

func TestCodexRuntimeIsProtocolOnly(t *testing.T) {
	if got := FilterProtocol([]string{CodexRuntimeV2, CodexHarnessV1}); !reflect.DeepEqual(got, []string{CodexHarnessV1, CodexRuntimeV2}) {
		t.Fatalf("older worker capabilities changed: %v", got)
	}
	if got := FilterProtocol([]string{CodexRuntimeV3, CodexRuntimeV2, CodexHarnessV1, CodexRuntimeV3, CodexRuntimeV2}); !reflect.DeepEqual(got, []string{CodexHarnessV1, CodexRuntimeV2, CodexRuntimeV3}) {
		t.Fatalf("current runtime dropped or duplicated: %v", got)
	}
	if got := Filter([]string{CodexRuntimeV2, CodexRuntimeV3}); len(got) != 0 {
		t.Fatalf("protocol leaked into scheduler vocabulary: %v", got)
	}
	if got := SelfReportable([]string{CodexRuntimeV2, CodexRuntimeV3}); len(got) != 0 {
		t.Fatalf("protocol leaked into tool capabilities: %v", got)
	}
}
