package workersvc

import (
	"context"
	"errors"
	"strings"
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
	// The route's JSON decoding cannot deliver invalid UTF-8 (encoding/json
	// substitutes U+FFFD), so the service entry is where this bound is reachable.
	for _, nonce := range []string{"", "\xff", "a\x00b", strings.Repeat("n", 129)} {
		generation, epoch := int64(7), int64(41)
		req := StateRequest{State: stateCodexSwitchStopped, ClaimGeneration: &generation, ExpectedEpoch: &epoch, RegisterNonce: nonce}
		_, applied, err := (&Service{}).SetState(context.Background(), store.Worker{}, uuid.New(), req)
		if applied || !errors.Is(err, ErrInvalidState) {
			t.Fatalf("unsafe nonce %q reached mutation authority: %v", nonce, err)
		}
	}
}
