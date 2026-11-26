package capability

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The controller rejects a whole poll snapshot when any worker's roll-evidence
// capability set exceeds its bound (controller/internal/apiclient maxCapabilitySet).
// A registered worker can advertise every name in these vocabularies, so each one
// must stay well under that bound. Fail at three quarters of it, so the next
// additions raise the shared limit before they can blind the controller.
func TestVocabulariesFitControllerPollBound(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "hostedsvc", "testdata", "controller_poll_limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var limits struct {
		MaxCapabilitySet int `json:"max_capability_set"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&limits); err != nil {
		t.Fatal(err)
	}
	if limits.MaxCapabilitySet <= 0 {
		t.Fatalf("max_capability_set = %d, want a positive bound", limits.MaxCapabilitySet)
	}
	ceiling := limits.MaxCapabilitySet * 3 / 4
	for name, size := range map[string]int{"protocol": len(protocolVocabulary), "scheduling": len(vocabulary)} {
		if size > ceiling {
			t.Errorf("%s vocabulary has %d names, over %d (3/4 of the controller poll bound %d): raise max_capability_set in controller_poll_limits.json and maxCapabilitySet in controller/internal/apiclient together",
				name, size, ceiling, limits.MaxCapabilitySet)
		}
	}
}
