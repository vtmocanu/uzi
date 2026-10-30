package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeCloneSuccessWithoutTransaction(t *testing.T) {
	svc := &Service{}
	applied, err := svc.ReportForgeCloneSuccess(context.Background(), store.Worker{
		ID: uuid.New(), ProtocolCapabilities: []string{ForgeCloneSuccessFeature},
	}, uuid.New(), ForgeCloneSuccessRequest{ClaimGeneration: 1, Incarnation: uuid.New(), Version: 1, ObservationVersion: 1})
	if err == nil || applied {
		t.Fatalf("missing transaction: applied=%v err=%v", applied, err)
	}
}
