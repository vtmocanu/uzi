package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type storedCheckerPinStore struct{ *healthFakeStore }

func (storedCheckerPinStore) GetCrossCheckPinSnapshot(context.Context, store.GetCrossCheckPinSnapshotParams) (store.GetCrossCheckPinSnapshotRow, error) {
	return store.GetCrossCheckPinSnapshotRow{
		Stage: "plan", CheckerHarness: pgtype.Text{String: "claude", Valid: true},
		Model: pgtype.Text{String: "gpt-6-luna", Valid: true},
	}, nil
}

func TestStoredClaudeCheckerLunaPinPreflight(t *testing.T) {
	userID := uuid.New()
	svc := &Service{q: storedCheckerPinStore{}}
	got, err := svc.preflightCrossCheckPins(context.Background(),
		store.Worker{UserID: userID, ProtocolCapabilities: []string{capability.CrossCheckPinsV1}},
		store.Run{UserID: userID, Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model == nil || *got.Model != "gpt-6-luna" || got.ModelSource != "pin" {
		t.Fatalf("stored checker pin lost: %+v", got)
	}
}
