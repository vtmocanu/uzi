package capability

import (
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workertmpl"
)

// TestTemplateCapabilities_TruthTable pins the implied capabilities for EVERY
// entry in the worker-template registry, so adding a template without deciding
// its capabilities reddens here rather than silently returning {}.
func TestTemplateCapabilities_TruthTable(t *testing.T) {
	want := map[string][]string{
		"base": {},
		"jvm":  {JVM},
	}
	for _, name := range workertmpl.Names {
		exp, ok := want[name]
		if !ok {
			t.Fatalf("workertmpl.Names has %q with no expected capability set in this truth table; decide its capabilities", name)
		}
		got := TemplateCapabilities(name)
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("TemplateCapabilities(%q) = %v, want %v", name, got, exp)
		}
	}
}

func TestTemplateCapabilities_UnknownAndEmpty(t *testing.T) {
	for _, name := range []string{"", "does-not-exist", "gpu"} {
		got := TemplateCapabilities(name)
		if len(got) != 0 {
			t.Errorf("TemplateCapabilities(%q) = %v, want empty", name, got)
		}
		if got == nil {
			t.Errorf("TemplateCapabilities(%q) returned nil, want empty non-nil slice", name)
		}
	}
}

func TestFilter_DropsUnknownsKeepsVocabulary(t *testing.T) {
	got := Filter([]string{"gpu", "docker", "rm -rf", "jvm", "DOCKER"})
	want := []string{"docker", "jvm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter dropped/kept wrong names: got %v, want %v", got, want)
	}
}

func TestFilter_DedupesStableOrder(t *testing.T) {
	// Input order jvm-before-docker with duplicates; output is vocabulary order,
	// deduped.
	got := Filter([]string{"jvm", "docker", "jvm", "docker"})
	want := []string{"docker", "jvm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter = %v, want %v", got, want)
	}
}

func TestFilter_EmptyAndAllUnknown(t *testing.T) {
	if got := Filter(nil); len(got) != 0 {
		t.Errorf("Filter(nil) = %v, want empty", got)
	}
	if got := Filter([]string{"gpu", "tpu"}); len(got) != 0 {
		t.Errorf("Filter(all-unknown) = %v, want empty", got)
	}
}

func TestFilterTools_DropsUnknownsKeepsVocabulary(t *testing.T) {
	// Capability names (docker) and arbitrary strings are NOT tools and must drop;
	// only the provisionable toolchain families survive.
	got := FilterTools([]string{"docker", "go", "rm -rf", "node", "GO", "python", "rust", "jvm"})
	want := []string{"go", "node", "python", "rust", "jvm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilterTools dropped/kept wrong names: got %v, want %v", got, want)
	}
}

func TestFilterTools_DedupesStableOrder(t *testing.T) {
	// Input order is scrambled with duplicates; output is vocabulary order, deduped.
	got := FilterTools([]string{"rust", "go", "node", "go", "rust", "jvm", "node"})
	want := []string{"go", "node", "rust", "jvm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilterTools = %v, want %v", got, want)
	}
}

func TestFilterTools_EmptyAndAllUnknown(t *testing.T) {
	if got := FilterTools(nil); len(got) != 0 {
		t.Errorf("FilterTools(nil) = %v, want empty", got)
	}
	if got := FilterTools([]string{"docker", "cobol", "haskell"}); len(got) != 0 {
		t.Errorf("FilterTools(all-unknown) = %v, want empty", got)
	}
}

// TestSelfReportable_DropsTemplateAndUnknown proves the self-report gate keeps
// ONLY docker: jvm is template-derived and must never survive a worker's own
// report, and an unknown name is dropped like Filter drops it.
func TestSelfReportable_DropsTemplateAndUnknown(t *testing.T) {
	got := SelfReportable([]string{"jvm", "docker", "gpu"})
	want := []string{"docker"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SelfReportable dropped/kept wrong names: got %v, want %v", got, want)
	}
}

func TestSelfReportable_DedupesStableOrderAndEmpty(t *testing.T) {
	if got := SelfReportable([]string{"docker", "docker"}); !reflect.DeepEqual(got, []string{"docker"}) {
		t.Errorf("SelfReportable(dupes) = %v, want [docker]", got)
	}
	if got := SelfReportable(nil); len(got) != 0 {
		t.Errorf("SelfReportable(nil) = %v, want empty", got)
	}
	if got := SelfReportable([]string{"jvm", "gpu"}); len(got) != 0 {
		t.Errorf("SelfReportable(no self-reportable names) = %v, want empty", got)
	}
}
