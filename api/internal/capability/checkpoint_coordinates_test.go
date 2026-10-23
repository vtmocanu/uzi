package capability

import (
	"reflect"
	"testing"
)

func TestTaskCheckpointCoordinatesProtocolOnly(t *testing.T) {
	input := []string{TaskCheckpointCoordinatesV1, TaskCheckpointCoordinatesV1, "unknown"}
	if got := FilterProtocol(input); !reflect.DeepEqual(got, []string{TaskCheckpointCoordinatesV1}) {
		t.Fatalf("registration protocol filter = %v", got)
	}
	if got := Filter(input); len(got) != 0 {
		t.Fatalf("checkpoint protocol became a selectable scheduler capability: %v", got)
	}
}
