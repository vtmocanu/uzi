package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIterationParkFailsClosedWithoutTransaction(t *testing.T) {
	s := &Service{}
	_, applied, _, err := s.ReportIterationPark(context.Background(), store.Worker{}, uuid.New(),
		IterationParkEvidence{ClaimGeneration: 1, IterationCount: 5, MaxIterations: 5})
	if applied || !errors.Is(err, ErrIterationParkAuthority) {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
}

func TestIterationParkClaimWire(t *testing.T) {
	raw, err := json.Marshal(ClaimConfig{IterationParkVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["iteration_park_version"] != float64(1) {
		t.Fatalf("claim config=%s", raw)
	}
	raw, err = json.Marshal(ClaimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// A zero version can still represent the older API wire shape.
	var old map[string]any
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if _, exists := old["iteration_park_version"]; exists {
		t.Fatalf("zero version emitted: %s", raw)
	}
}
