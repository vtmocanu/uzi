package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The API golden currently omits the additive manual fields. Parse it strictly
// both as emitted and with the new fields, without changing the API-owned golden.
func TestManualCordonStrictGolden(t *testing.T) {
	raw, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, added := range []bool{false, true} {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		var workers []map[string]json.RawMessage
		if err := json.Unmarshal(envelope["workers"], &workers); err != nil {
			t.Fatal(err)
		}
		if added {
			workers[0]["manual_cordoned_at"] = json.RawMessage(`"2026-01-02T03:04:05Z"`)
			workers[0]["effective_cordoned"] = json.RawMessage("true")
			workers[1]["effective_cordoned"] = json.RawMessage("false")
			envelope["workers"], err = json.Marshal(workers)
			if err != nil {
				t.Fatal(err)
			}
		}
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		var got PollResponse
		if err := dec.Decode(&got); err != nil {
			t.Fatal(err)
		}
		w := got.Workers[0]
		if !added {
			if w.ManualCordonedAt != nil || w.EffectiveCordoned != nil {
				t.Fatal("omission acquired a cordon")
			}
		} else {
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			if w.ManualCordonedAt == nil || !w.ManualCordonedAt.Equal(at) ||
				w.EffectiveCordoned == nil || !*w.EffectiveCordoned ||
				got.Workers[1].EffectiveCordoned == nil || *got.Workers[1].EffectiveCordoned ||
				got.Workers[1].ManualCordonedAt != nil {
				t.Fatalf("fields conflated: %+v", got.Workers)
			}
		}
	}
}
