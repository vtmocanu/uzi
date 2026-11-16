package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexSwitchStoppedAuthorityBounds(t *testing.T) {
	for _, value := range []int64{-1, 0, 9007199254740992} {
		for _, field := range []string{"generation", "epoch"} {
			generation, epoch := int64(7), int64(41)
			if field == "generation" {
				generation = value
			} else {
				epoch = value
			}
			req := StateRequest{State: stateCodexSwitchStopped, ClaimGeneration: &generation, ExpectedEpoch: &epoch, RegisterNonce: "nonce"}
			_, applied, err := (&Service{}).SetState(context.Background(), store.Worker{}, uuid.New(), req)
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatal("unsafe integer reached mutation authority", field, value, err)
			}
		}
	}
}
