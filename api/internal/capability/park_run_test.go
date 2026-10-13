package capability

import (
	"reflect"
	"testing"
)

func TestFilterProtocolParkRun(t *testing.T) {
	got := FilterProtocol([]string{InputReceiptsV1, ParkRunV1, "unknown", WallParkV1, ParkRunV1})
	want := []string{WallParkV1, ParkRunV1, InputReceiptsV1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterProtocol = %v, want %v", got, want)
	}
}
