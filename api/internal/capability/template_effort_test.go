package capability

import (
	"reflect"
	"testing"
)

func TestTemplateEffortProtocolSeparation(t *testing.T) {
	got := FilterProtocol([]string{TemplateEffortV1, CrossCheckV1, TemplateEffortV1, "unknown"})
	if !reflect.DeepEqual(got, []string{CrossCheckV1, TemplateEffortV1}) {
		t.Fatalf("protocol order = %v", got)
	}
	if len(Filter([]string{TemplateEffortV1})) != 0 || len(SelfReportable([]string{TemplateEffortV1})) != 0 {
		t.Fatal("template effort protocol leaked into repo selectable capabilities")
	}
}
