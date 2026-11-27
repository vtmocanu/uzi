package workersvc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #4416 review: a transient failure reading the owner's vault state (secretopen.ErrReaderFailure)
// is a retryable database error, never a "history unavailable" park; a missing or invalid vault
// still fails closed as a park; a stale seal and a locked vault keep their own classes.
func TestMapRunEnvOpenErrorClassifies(t *testing.T) {
	transient := fmt.Errorf("%w: %w", secretopen.ErrReaderFailure, errors.New("connection reset"))
	if got := mapRunEnvOpenError(transient); errors.Is(got, errRunEnvHistoryUnavailable) || !errors.Is(got, secretopen.ErrReaderFailure) {
		t.Fatalf("reader failure: got %v, want it passed through, not a park", got)
	}
	for name, err := range map[string]error{
		"not configured": fmt.Errorf("%w: %w", secretopen.ErrReaderFailure, vault.ErrNotConfigured),
		"invalid state":  fmt.Errorf("%w: %w", secretopen.ErrReaderFailure, vault.ErrInvalidState),
		"undecryptable":  secretopen.ErrUndecryptable,
	} {
		if got := mapRunEnvOpenError(err); !errors.Is(got, errRunEnvHistoryUnavailable) {
			t.Fatalf("%s: got %v, want errRunEnvHistoryUnavailable", name, got)
		}
	}
	if got := mapRunEnvOpenError(store.ErrVaultSealStale); !errors.Is(got, store.ErrVaultSealStale) {
		t.Fatalf("stale: got %v", got)
	}
	if got := mapRunEnvOpenError(fmt.Errorf("x: %w", secretopen.ErrVaultLocked)); !errors.Is(got, errVaultLocked) {
		t.Fatalf("locked: got %v", got)
	}
}
