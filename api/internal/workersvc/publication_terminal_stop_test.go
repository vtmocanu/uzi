package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func termSnapshotJSON(t *testing.T, id uuid.UUID, stopKind *string, branch string) []byte {
	t.Helper()
	b, err := json.Marshal(publicationSnapshot{
		Request: publicationAuthority{ID: id, StopKind: stopKind, Kind: "ci_fix"},
		Nodes:   []publicationNode{{Run: publicationAuthority{ID: id, StopKind: stopKind, Kind: "ci_fix", Branch: &branch}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Only this stop's own stamp is tolerated between the snapshot the answer was based on and the
// reconciliation read.
func TestPublicationSnapshotsEqualModuloStop(t *testing.T) {
	id := uuid.New()
	cancelled, stopped := "cancelled", "stopped"
	before := termSnapshotJSON(t, id, nil, "b")
	cases := []struct {
		name    string
		after   []byte
		stamped bool
		want    bool
	}{
		{"unchanged", termSnapshotJSON(t, id, nil, "b"), false, true},
		{"own stamp", termSnapshotJSON(t, id, &cancelled, "b"), true, true},
		{"foreign cancelled stamp", termSnapshotJSON(t, id, &cancelled, "b"), false, false},
		{"own stamp plus another change", termSnapshotJSON(t, id, &cancelled, "other"), true, false},
		{"stopped is never ours", termSnapshotJSON(t, id, &stopped, "b"), true, false},
	}
	for _, c := range cases {
		got, err := publicationSnapshotsEqualModuloStop(before, c.after, id, c.stamped)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v err %v, want %v", c.name, got, err, c.want)
		}
	}
	if _, err := publicationSnapshotsEqualModuloStop(before, []byte("{"), id, true); err == nil {
		t.Error("malformed snapshot must error")
	}
}
