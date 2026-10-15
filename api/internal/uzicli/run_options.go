package uzicli

import "github.com/vtmocanu/uzi/api/internal/apitypes"

// RunInputOptions carries explicit input bindings, including owner reset consent.
// The legacy SubmitRunInput method remains available for ordinary steering.
type RunInputOptions = apitypes.RunInputRequest

// RunReadOptions selects one bounded audit page. Nil requests the newest page.
type RunReadOptions struct {
	ReconciliationBeforeGateRevision *int64
}
