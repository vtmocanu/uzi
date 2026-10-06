package recovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Invalid final dispositions must fail before opening a database transaction.
func TestReleaseRejectsInvalidFinalInventory(t *testing.T) {
	generation := int64(1)
	publication := "publication"
	noWork := "no_work"
	cases := []struct {
		name string
		req  apitypes.RecoveryReleaseRequest
	}{
		{"missing generation", apitypes.RecoveryReleaseRequest{FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}, ReleaseEvidence: &publication}},
		{"unknown kind", apitypes.RecoveryReleaseRequest{Generation: &generation, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "no_work", CoverageDigest: emptyInventoryDigest}}},
		{"noncanonical digest", apitypes.RecoveryReleaseRequest{Generation: &generation, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "archive", CoverageDigest: strings.Repeat("A", 64)}}},
		{"settled nonempty digest", apitypes.RecoveryReleaseRequest{Generation: &generation, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: strings.Repeat("a", 64)}, ReleaseEvidence: &publication}},
		{"settled generic no work", apitypes.RecoveryReleaseRequest{Generation: &generation, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}, ReleaseEvidence: &noWork}},
		{"settled source reference", apitypes.RecoveryReleaseRequest{Generation: &generation, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", SourceSha: "abcd", CoverageDigest: emptyInventoryDigest}, ReleaseEvidence: &publication}},
		{"archive missing identity", apitypes.RecoveryReleaseRequest{Generation: &generation, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "archive", CoverageDigest: emptyInventoryDigest}}},
	}
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.RecoveryInventoryV1}}
	svc := &Service{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Release(context.Background(), wkr, uuid.New(), tc.req)
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("Release = %v, want bad request", err)
			}
		})
	}
}
