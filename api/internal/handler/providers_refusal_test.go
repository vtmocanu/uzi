package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWriteProviderCreateRefusalMapsTheTypedErrors pins the stable reason codes of the provider
// refusals run creation can return (PRD #4320 module 4), wrapped the way callers wrap them.
func TestWriteProviderCreateRefusalMapsTheTypedErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"unusable (typed)", fmt.Errorf("create: %w", &workersvc.ProviderSourceUnusableError{Reason: workersvc.SourceUnusableRetired}), "provider_source_unusable"},
		{"unusable (sentinel)", workersvc.ErrProviderSourceUnusable, "provider_source_unusable"},
		{"lane", fmt.Errorf("x: %w", workersvc.ErrProviderLaneUnsupported), "provider_lane_unsupported"},
		{"unmapped", &workersvc.ProviderModelUnmappedError{Model: "fable", Source: "run"}, "provider_model_unmapped"},
		{"stale", fmt.Errorf("%w: rotated", workersvc.ErrProviderBindingStale), "provider_binding_stale"},
		{"destination", workersvc.ErrProviderDestinationNotAllowed, "provider_destination_not_allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !writeProviderCreateRefusal(rec, c.err) {
				t.Fatal("not mapped")
			}
			if rec.Code != http.StatusConflict || providerReason(t, rec) != c.reason {
				t.Fatalf("got %d %s, want 409 %s", rec.Code, rec.Body.String(), c.reason)
			}
		})
	}
	rec := httptest.NewRecorder()
	if writeProviderCreateRefusal(rec, errors.New("other")) || rec.Body.Len() != 0 {
		t.Fatal("an unrelated error must not be written")
	}
}
