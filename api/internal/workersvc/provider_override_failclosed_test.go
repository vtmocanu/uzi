package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestRefuseOverrideForProviderOwnerFailsClosedWithoutASourceReader: a query surface that cannot read
// the owner's provider source must not read as "no provider source" (that would let a vendor
// credential override ride an unchecked, possibly provider-sourced owner).
func TestRefuseOverrideForProviderOwnerFailsClosedWithoutASourceReader(t *testing.T) {
	err := refuseOverrideForProviderOwner(context.Background(), struct{}{}, uuid.New())
	if err == nil {
		t.Fatal("a surface with no provider source reader was treated as having no provider source")
	}
	if errors.Is(err, ErrCredentialOverrideProviderSource) {
		t.Fatalf("err = %v: an unreadable source is an internal failure, not the provider-source refusal", err)
	}
}
