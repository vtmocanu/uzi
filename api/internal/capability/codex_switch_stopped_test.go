package capability

import (
	"reflect"
	"testing"
)

func TestCodexSwitchStoppedProtocolOnly(t *testing.T) {
	if got := FilterProtocol([]string{CodexSwitchStoppedV1, CodexSwitchStoppedV1}); !reflect.DeepEqual(got, []string{CodexSwitchStoppedV1}) {
		t.Fatal(got)
	}
	if len(Filter([]string{CodexSwitchStoppedV1})) != 0 || len(SelfReportable([]string{CodexSwitchStoppedV1})) != 0 {
		t.Fatal("stopped protocol leaked into scheduler capabilities")
	}
}
