package apitypes

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes/apitypestest"
)

func TestWorkerRecoveryWireKeys(t *testing.T) {
	var dto RunDTO
	apitypestest.Populate(&dto)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var run map[string]json.RawMessage
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}
	assertKeys := func(raw json.RawMessage, want []string) map[string]json.RawMessage {
		t.Helper()
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(got))
		for key := range got {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		sort.Strings(want)
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("keys=%v want=%v", keys, want)
		}
		return got
	}
	recovery := assertKeys(run["worker_recovery"], []string{"episode", "automatic_requeue_limit", "episode_used", "episode_remaining", "evidence"})
	assertKeys(recovery["evidence"], []string{"checkpoint_tip", "available_capture", "publication_uncertain", "capture_uncertain", "custody_uncertain", "unknown", "recorded_at"})
	if _, ok := run["requeue_episode_baseline"]; ok {
		t.Fatal("internal baseline leaked")
	}
}
