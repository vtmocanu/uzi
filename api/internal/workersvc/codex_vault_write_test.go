package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func TestCodexMaterialWriteContextDefault(t *testing.T) {
	type requestKey struct{}
	request, cancelRequest := context.WithCancel(context.WithValue(context.Background(), requestKey{}, "request-value"))
	cancelRequest()
	start := time.Now()
	ctx, cancel := (&Service{}).codexMaterialWriteContext(request, 100*time.Millisecond)
	defer cancel()
	if ctx.Err() != nil || ctx.Value(requestKey{}) != "request-value" {
		t.Fatal("material write must detach cancellation and retain request values")
	}
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(start.Add(100*time.Millisecond)) || deadline.After(time.Now().Add(100*time.Millisecond)) {
		t.Fatal("material write did not apply the requested timeout")
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("material write ended with %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("material write timeout never fired")
	}
}

func TestCodexBackgroundWriteBudgetCoversVaultLifecycle(t *testing.T) {
	if codexRecoverySlotBgWriteTimeout <= vault.LifecycleTimeout {
		t.Fatalf("background timeout %s must exceed lifecycle timeout %s", codexRecoverySlotBgWriteTimeout, vault.LifecycleTimeout)
	}
	if codexRecoverySlotWriteTimeout != 5*time.Second {
		t.Fatalf("foreground timeout = %s, want 5s", codexRecoverySlotWriteTimeout)
	}
}

// A consumed retry must remain pending while its result can still be saved.
// In particular, background writes can wait through a full vault conversion;
// applying the short foreground timeout to them prematurely resolves the intent.
func TestCodexReuseInFlightBoundCoversPersistence(t *testing.T) {
	persistence := time.Duration(codexRecoverySlotSyncAttempts)*codexRecoverySlotWriteTimeout +
		time.Duration(codexRecoverySlotBgAttempts)*codexRecoverySlotBgWriteTimeout
	backoffs := time.Duration(codexRecoverySlotSyncAttempts-1+codexRecoverySlotBgAttempts) * codexRecoverySlotRetryBackoff
	minimum := codexRefreshLeaseTTL + persistence + backoffs + codexRecoverySlotWriteTimeout
	if codexRefreshReuseInFlightBound < minimum {
		t.Errorf("reuse bound %s expires before the provider/persistence/terminal budget %s", codexRefreshReuseInFlightBound, minimum)
	}
	admitted := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	stamp := pgtype.Timestamptz{Time: admitted, Valid: true}
	account := store.CodexProviderAccount{CoordState: codexCoordQuarantined}
	intent := store.CodexRefreshIntent{State: codexIntentRotating, CreatedAt: stamp, ProviderErrorAt: stamp, ReuseAttemptedAt: stamp}
	if got := codexRefreshResolution(admitted.Add(minimum-time.Nanosecond), account, intent); got != codexIntentPending {
		t.Errorf("retry with persistence still in budget resolved as %q, want pending", got)
	}
}
