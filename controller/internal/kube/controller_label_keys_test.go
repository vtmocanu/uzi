package kube

import (
	"slices"
	"testing"
)

// config.controllerLabelKeys mirrors objectLabels' keys as literals (config cannot
// import kube). Pin the literal list here so renaming a provenance label cannot leave
// config accepting an operator label that overrides it.
func TestControllerLabelKeysMatchObjectLabels(t *testing.T) {
	want := []string{"app.kubernetes.io/component", "app.kubernetes.io/managed-by", "app.kubernetes.io/name", "uzi.dev/hosted-worker-id"}
	var got []string
	for k := range objectLabels("abc") {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("objectLabels keys = %v; update controllerLabelKeys in internal/config and this list together", got)
	}
}
