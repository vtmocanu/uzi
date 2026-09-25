package workersvc

import (
	"errors"
	"testing"
)

func TestJudgeCodexClaimErrorPreservesMintIdentity(t *testing.T) {
	minted := &codexMintedClaimError{cause: ErrCodexAccountQuarantined, epoch: 7, hash: []byte("claim-hash")}
	wrapped := wrapJudgeCodexClaimError(minted)
	if !errors.Is(wrapped, errCredentialUnavailable) || !errors.Is(wrapped, ErrCodexAccountQuarantined) {
		t.Fatalf("judge error lost terminal class or quarantine cause: %v", wrapped)
	}
	var got *codexMintedClaimError
	if !errors.As(wrapped, &got) || got.epoch != minted.epoch || string(got.hash) != string(minted.hash) {
		t.Fatalf("judge error lost minted capability identity: %#v", got)
	}
	if !errors.Is(wrapJudgeCodexClaimError(errCodexMintAmbiguous), errCodexMintAmbiguous) {
		t.Fatal("judge error lost ambiguous mint marker")
	}
}
