package handler

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"

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

// The step B listing gate is per worker: a worker over its bucket is refused while another
// worker still lists.
func TestHeldListAllowForIsPerWorker(t *testing.T) {
	allow := heldListAllowFor(mw.NewLimiter(1, time.Minute, nil))
	a, b := uuid.New(), uuid.New()
	if !allow(a) || allow(a) {
		t.Fatal("worker a: want one listing allowed, then refused")
	}
	if !allow(b) {
		t.Fatal("worker b shares worker a's bucket")
	}
}
