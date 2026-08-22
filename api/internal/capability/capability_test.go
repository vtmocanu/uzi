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

// TestUnmet_SubsetPresent pins the empty result when every required capability is present
// in the effective set — the run is approvable/claimable by that worker (PRD #84 M4 4c).
func TestUnmet_SubsetPresent(t *testing.T) {
	if got := Unmet([]string{Docker}, []string{Docker, JVM}); len(got) != 0 {
		t.Errorf("Unmet(required⊆effective) = %v, want empty", got)
	}
	if got := Unmet(nil, []string{Docker}); len(got) != 0 {
		t.Errorf("Unmet(no requirements) = %v, want empty", got)
	}
	if got := Unmet([]string{Docker, JVM}, []string{JVM, Docker}); len(got) != 0 {
		t.Errorf("Unmet(order-independent subset) = %v, want empty", got)
	}
}

// TestUnmet_MissingNamed pins that a missing required capability is returned by name, in
// stable vocabulary order, deduped.
func TestUnmet_MissingNamed(t *testing.T) {
	if got := Unmet([]string{Docker}, nil); !reflect.DeepEqual(got, []string{Docker}) {
		t.Errorf("Unmet(docker required, none effective) = %v, want [docker]", got)
	}
	if got := Unmet([]string{JVM}, []string{Docker}); !reflect.DeepEqual(got, []string{JVM}) {
		t.Errorf("Unmet(jvm required, docker effective) = %v, want [jvm]", got)
	}
	// Both missing → stable vocabulary order (docker before jvm), regardless of input order.
	if got := Unmet([]string{JVM, Docker}, nil); !reflect.DeepEqual(got, []string{Docker, JVM}) {
		t.Errorf("Unmet(both missing) = %v, want [docker jvm]", got)
	}
	// Duplicate required names collapse to one.
	if got := Unmet([]string{Docker, Docker}, nil); !reflect.DeepEqual(got, []string{Docker}) {
		t.Errorf("Unmet(duplicate required) = %v, want [docker]", got)
	}
}

// TestUnmet_DockerFolded is the load-bearing case for the approval gate: the caller folds
// docker into the effective set when the worker is docker_enabled, so a docker requirement
// against a docker-folded effective set is SATISFIED (empty), matching fn_worker_can_claim.
func TestUnmet_DockerFolded(t *testing.T) {
	// Simulates effectiveOwningWorkerCaps folding docker in for a docker_enabled base worker.
	effective := []string{Docker}
	if got := Unmet([]string{Docker}, effective); len(got) != 0 {
		t.Errorf("Unmet(docker required, docker-folded effective) = %v, want empty", got)
	}
	// Without the fold (a base worker, no docker), the same requirement is unmet.
	if got := Unmet([]string{Docker}, nil); !reflect.DeepEqual(got, []string{Docker}) {
		t.Errorf("Unmet(docker required, base worker) = %v, want [docker]", got)
	}
}

// TestUnmet_DropsUnknownRequired pins that a non-vocabulary name in required is dropped
// (never reported as unmet): required_capabilities is Filter-ed at every write, so an
// unknown name can only be junk and can never be a real, provisionable requirement.
func TestUnmet_DropsUnknownRequired(t *testing.T) {
	if got := Unmet([]string{"gpu"}, nil); len(got) != 0 {
		t.Errorf("Unmet(unknown required) = %v, want empty", got)
	}
	if got := Unmet([]string{"gpu", Docker}, nil); !reflect.DeepEqual(got, []string{Docker}) {
		t.Errorf("Unmet(unknown + docker required) = %v, want [docker]", got)
	}
}
