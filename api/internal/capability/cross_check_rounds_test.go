package capability

import (
	"reflect"
	"testing"
)

func TestCrossCheckRoundsProtocolVocabularyAndOrder(t *testing.T) {
	if got := FilterProtocol([]string{CrossCheckRoundsV1, CrossCheckV1, CrossCheckRoundsV1, "unknown"}); !reflect.DeepEqual(got, []string{CrossCheckV1, CrossCheckRoundsV1}) {
		t.Fatalf("rounds protocol filter/order: %v", got)
	}
	if got := Filter([]string{CrossCheckRoundsV1}); len(got) != 0 {
		t.Fatalf("protocol leaked to scheduler: %v", got)
	}
}
