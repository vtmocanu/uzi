package handler

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// recordingForge is a forge.Forge (via the embedded fakeUserForge stubs) whose ListLabels
// and EnsureLabels are the two methods the sweep-label guardrail exercises. It returns a
// canned label set from ListLabels and RECORDS every EnsureLabels call so a test can assert
// the exact labels the CONFIRM primitive sent to the forge. No network, no DB.
type recordingForge struct {
	*fakeUserForge
	labels      []forge.Label   // what ListLabels returns
	listErr     error           // a forge read failure to model
	ensureErr   error           // a forge write failure to model
	ensuredWith [][]forge.Label // one entry per EnsureLabels call, in order
}

func (f *recordingForge) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return f.labels, f.listErr
}

func (f *recordingForge) EnsureLabels(_ context.Context, _ int64, labels []forge.Label) error {
	f.ensuredWith = append(f.ensuredWith, labels)
	return f.ensureErr
}

// TestMissingLabels: the WARN diff returns exactly the requested labels absent from the
// repo, order-preserving, deduped, blanks dropped, and CASE-SENSITIVE — matching how the
// forge drivers decide label existence in EnsureLabels (an exact-name lookup).
func TestMissingLabels(t *testing.T) {
	existing := []forge.Label{{Name: "bug"}, {Name: "Planned"}}
	cases := []struct {
		name      string
		requested []string
		want      []string
	}{
		{"one missing among present", []string{"bug", "needs-triage", "Planned"}, []string{"needs-triage"}},
		{"all present", []string{"bug", "Planned"}, []string{}},
		{"case difference is missing", []string{"Bug", "planned"}, []string{"Bug", "planned"}},
		{"blanks dropped", []string{"  ", "", "bug"}, []string{}},
		{"trimmed then matched", []string{"  bug  "}, []string{}},
		{"dedup missing", []string{"x", "x", "y"}, []string{"x", "y"}},
		{"empty request", nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingLabels(existing, tc.requested)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("missingLabels(%v) = %v, want %v", tc.requested, got, tc.want)
			}
		})
	}
}

// TestEnsureRepoLabels: the CONFIRM primitive calls f.EnsureLabels with the requested
// labels (deduped, non-blank) and reports them as ensured. Asserts the fake recorded the
// call and the exact label set.
func TestEnsureRepoLabels(t *testing.T) {
	f := &recordingForge{fakeUserForge: &fakeUserForge{}}
	ensured, err := ensureRepoLabels(context.Background(), f, 42, []string{"bug", " Planned ", "bug", ""})
	if err != nil {
		t.Fatalf("ensureRepoLabels: %v", err)
	}
	if want := []string{"bug", "Planned"}; !reflect.DeepEqual(ensured, want) {
		t.Fatalf("ensured = %v, want %v", ensured, want)
	}
	if len(f.ensuredWith) != 1 {
		t.Fatalf("EnsureLabels calls = %d, want 1", len(f.ensuredWith))
	}
	wantLabels := []forge.Label{{Name: "bug"}, {Name: "Planned"}}
	if !reflect.DeepEqual(f.ensuredWith[0], wantLabels) {
		t.Fatalf("EnsureLabels was called with %v, want %v", f.ensuredWith[0], wantLabels)
	}
}

// TestEnsureRepoLabelsEmpty: an empty (or all-blank) request makes NO forge call and
// reports nothing ensured.
func TestEnsureRepoLabelsEmpty(t *testing.T) {
	f := &recordingForge{fakeUserForge: &fakeUserForge{}}
	ensured, err := ensureRepoLabels(context.Background(), f, 42, []string{"", "   "})
	if err != nil {
		t.Fatalf("ensureRepoLabels: %v", err)
	}
	if len(ensured) != 0 {
		t.Fatalf("ensured = %v, want empty", ensured)
	}
	if len(f.ensuredWith) != 0 {
		t.Fatalf("EnsureLabels should not have been called, got %d calls", len(f.ensuredWith))
	}
}

// TestEnsureRepoLabelsForgeError: a forge write failure propagates (the handler maps it to
// a 502), and nothing is reported as ensured.
func TestEnsureRepoLabelsForgeError(t *testing.T) {
	f := &recordingForge{fakeUserForge: &fakeUserForge{}, ensureErr: errors.New("forge down")}
	ensured, err := ensureRepoLabels(context.Background(), f, 42, []string{"bug"})
	if err == nil {
		t.Fatal("expected the forge error to propagate")
	}
	if ensured != nil {
		t.Fatalf("ensured = %v, want nil on error", ensured)
	}
}
