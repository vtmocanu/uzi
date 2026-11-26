package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// goldenWorkerWithCapabilities returns a one-worker poll body built from the api's
// evidence-bearing golden, with the named roll-evidence capability set replaced by n
// distinct, well-formed names.
func goldenWorkerWithCapabilities(t *testing.T, key string, n int) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "controller_poll_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Workers []map[string]any `json:"workers"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	worker := golden.Workers[0]
	evidence, ok := worker["roll_evidence"].(map[string]any)
	if !ok {
		t.Fatal("golden worker 0 carries no roll_evidence")
	}
	set := make([]string, n)
	for i := range set {
		set[i] = fmt.Sprintf("capability_%03d_v1", i)
	}
	evidence[key] = set
	body, err := json.Marshal(map[string]any{"workers": []map[string]any{worker}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func decodeBody(body []byte) (int, error) {
	got, err := decodePoll(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)})
	return len(got.Workers), err
}

// A worker that advertises more than 64 protocol capabilities must not make the
// controller drop the whole fleet snapshot (rc.26: 65 names, every poll rejected).
func TestDecodePollAcceptsSixtyFiveProtocolCapabilities(t *testing.T) {
	n, err := decodeBody(goldenWorkerWithCapabilities(t, "protocol_capabilities", 65))
	if err != nil || n != 1 {
		t.Fatalf("decodePoll rejected a worker with 65 protocol capabilities: workers=%d err=%v", n, err)
	}
}

func TestDecodePollCapabilitySetBoundary(t *testing.T) {
	for _, key := range []string{"scheduling_capabilities", "protocol_capabilities"} {
		t.Run(key, func(t *testing.T) {
			if n, err := decodeBody(goldenWorkerWithCapabilities(t, key, maxCapabilitySet)); err != nil || n != 1 {
				t.Fatalf("a set of exactly %d names must decode: workers=%d err=%v", maxCapabilitySet, n, err)
			}
			if n, err := decodeBody(goldenWorkerWithCapabilities(t, key, maxCapabilitySet+1)); err == nil || n != 0 {
				t.Fatalf("a set of %d names must reject the snapshot: workers=%d err=%v", maxCapabilitySet+1, n, err)
			}
		})
	}
}

// The api's vocabulary test reads the same limit, so the two modules cannot drift.
func TestCapabilitySetBoundMatchesSharedLimit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "controller_poll_limits.json"))
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
	if limits.MaxCapabilitySet != maxCapabilitySet {
		t.Fatalf("controller maxCapabilitySet = %d, shared limit = %d", maxCapabilitySet, limits.MaxCapabilitySet)
	}
}
