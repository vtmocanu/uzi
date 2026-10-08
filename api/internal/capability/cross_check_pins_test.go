package capability

import (
	"reflect"
	"testing"
)

func TestCrossCheckPinsProtocolSeparation(t *testing.T) {
	got := FilterProtocol([]string{CrossCheckPinsV1, CrossCheckV1, CrossCheckPinsV1, "unknown"})
	if !reflect.DeepEqual(got, []string{CrossCheckV1, CrossCheckPinsV1}) {
		t.Fatalf("protocol order = %v", got)
	}
	if len(Filter([]string{CrossCheckPinsV1})) != 0 || len(SelfReportable([]string{CrossCheckPinsV1})) != 0 {
		t.Fatal("checker pin protocol leaked into repo selectable capabilities")
	}
}
