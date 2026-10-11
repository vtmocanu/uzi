package handler

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestHeldRemoteForRecovery(t *testing.T) {
	if got := heldRemoteForRecovery(workersvc.ErrHeldRemoteGone); !errors.Is(got, recovery.ErrHeldRemoteGone) {
		t.Fatalf("sentinel mapped to %v", got)
	}
	if got := heldRemoteForRecovery(fmt.Errorf("wrapped: %w", workersvc.ErrHeldRemoteGone)); !errors.Is(got, recovery.ErrHeldRemoteGone) {
		t.Fatalf("wrapped sentinel mapped to %v", got)
	}
	other := errors.New("ssrf gate refused")
	if got := heldRemoteForRecovery(other); got != other || errors.Is(got, recovery.ErrHeldRemoteGone) {
		t.Fatalf("other error mapped to %v", got)
	}
	if got := heldRemoteForRecovery(nil); got != nil {
		t.Fatalf("nil mapped to %v", got)
	}
}
