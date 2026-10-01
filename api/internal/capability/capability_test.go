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

// TestFilterProtocol_DropsUnknownsKeepsVocabulary proves the PROTOCOL gate keeps ONLY
// the protocol vocabulary (today completion_interlock_v1) and DROPS scheduler-vocabulary
// names (docker/jvm) and arbitrary strings — the two vocabularies are separate on purpose
// (PRD #1226 M1 D2), so a scheduler capability must never survive the protocol filter.
func TestFilterProtocol_DropsUnknownsKeepsVocabulary(t *testing.T) {
	got := FilterProtocol([]string{"docker", "completion_interlock_v1", "rm -rf", "jvm", "COMPLETION_INTERLOCK_V1"})
	want := []string{"completion_interlock_v1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilterProtocol dropped/kept wrong names: got %v, want %v", got, want)
	}
}

func TestFilterProtocol_DedupesStableOrderAndEmpty(t *testing.T) {
	if got := FilterProtocol([]string{"completion_interlock_v1", "completion_interlock_v1"}); !reflect.DeepEqual(got, []string{"completion_interlock_v1"}) {
		t.Errorf("FilterProtocol(dupes) = %v, want [completion_interlock_v1]", got)
	}
	if got := FilterProtocol(nil); len(got) != 0 {
		t.Errorf("FilterProtocol(nil) = %v, want empty", got)
	}
	if got := FilterProtocol([]string{"docker", "jvm", "gpu"}); len(got) != 0 {
		t.Errorf("FilterProtocol(no protocol names) = %v, want empty", got)
	}
}

// TestFilterProtocol_SeparateFromSchedulerVocabulary pins the invariant that the protocol
// vocabulary and the scheduler vocabulary do not overlap: no Vocabulary() member survives
// FilterProtocol, and CompletionInterlockV1 is not a Filter (scheduler) member. This is
// what keeps a protocol string out of the web capability picker.
func TestFilterProtocol_SeparateFromSchedulerVocabulary(t *testing.T) {
	if got := FilterProtocol(Vocabulary()); len(got) != 0 {
		t.Errorf("FilterProtocol(scheduler vocabulary) = %v, want empty (vocabularies must not overlap)", got)
	}
	if got := Filter([]string{CompletionInterlockV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", CompletionInterlockV1, got)
	}
}

// TestFilterProtocol_KeepsCodexHarnessV1 pins PRD #1332 M5A (D3): the Codex-harness protocol
// capability is a member of the protocol vocabulary (so a worker's self-reported codex_harness_v1
// survives registration and reaches workers.protocol_capabilities, where the fail-closed claim gate
// reads it), and is NOT a scheduler capability (so it never leaks into Vocabulary() or the web
// picker). This is the DB-free half of the vocabulary-removal calibration: removing CodexHarnessV1
// from protocolVocabulary makes FilterProtocol DROP it here (and the capable-worker claim LiveDB test
// then fails because Register stores nothing).
func TestFilterProtocol_KeepsCodexHarnessV1(t *testing.T) {
	if got := FilterProtocol([]string{CodexHarnessV1}); !reflect.DeepEqual(got, []string{CodexHarnessV1}) {
		t.Errorf("FilterProtocol(%q) = %v, want it KEPT (missing from the protocol vocabulary?)", CodexHarnessV1, got)
	}
	if got := Filter([]string{CodexHarnessV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", CodexHarnessV1, got)
	}
	if got := SelfReportable([]string{CodexHarnessV1}); len(got) != 0 {
		t.Errorf("SelfReportable(%q) = %v, want empty (codex_harness_v1 is not a scheduler self-report)", CodexHarnessV1, got)
	}
}

func TestFilterProtocol_KeepsCodexCompletionInterlockV1(t *testing.T) {
	if got := FilterProtocol([]string{CodexCompletionInterlockV1, CodexHarnessV1}); !reflect.DeepEqual(got, []string{CodexHarnessV1, CodexCompletionInterlockV1}) {
		t.Fatalf("FilterProtocol = %v", got)
	}
	if got := Filter([]string{CodexCompletionInterlockV1}); len(got) != 0 {
		t.Fatalf("scheduler vocabulary admitted protocol capability: %v", got)
	}
}

// TestFilterProtocol_KeepsCodexCustomModelV1 pins PRD #1551 M4 (D6): the custom-Codex-model
// protocol capability is a member of the protocol vocabulary (so a worker's self-reported
// codex_custom_model_v1 survives registration and reaches workers.protocol_capabilities, where the
// fail-closed custom-model claim gate reads it), and is NOT a scheduler capability (so it never
// leaks into Vocabulary() or the web picker). This is the DB-free half of the vocabulary-removal
// calibration: removing CodexCustomModelV1 from protocolVocabulary makes FilterProtocol DROP it
// here (and the capable-worker custom-model claim LiveDB test then fails because Register stores
// nothing).
func TestFilterProtocol_KeepsCodexCustomModelV1(t *testing.T) {
	if got := FilterProtocol([]string{CodexCustomModelV1}); !reflect.DeepEqual(got, []string{CodexCustomModelV1}) {
		t.Errorf("FilterProtocol(%q) = %v, want it KEPT (missing from the protocol vocabulary?)", CodexCustomModelV1, got)
	}
	if got := Filter([]string{CodexCustomModelV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", CodexCustomModelV1, got)
	}
	if got := SelfReportable([]string{CodexCustomModelV1}); len(got) != 0 {
		t.Errorf("SelfReportable(%q) = %v, want empty (codex_custom_model_v1 is not a scheduler self-report)", CodexCustomModelV1, got)
	}
	// It survives alongside codex_harness_v1 in stable protocolOrder (a real worker advertises both).
	if got := FilterProtocol([]string{CodexCustomModelV1, CodexHarnessV1}); !reflect.DeepEqual(got, []string{CodexHarnessV1, CodexCustomModelV1}) {
		t.Errorf("FilterProtocol([custom, harness]) = %v, want [%q %q] in protocolOrder", got, CodexHarnessV1, CodexCustomModelV1)
	}
}

// TestFilterProtocol_KeepsAdviceClaimFenceV1 pins issue #1423: advice_claim_fence_v1 survives
// registration (the advice-post stamping requirement reads it off workers.protocol_capabilities),
// is not a scheduler capability, and orders after gate_revision_v1 in protocolOrder.
func TestFilterProtocol_KeepsAdviceClaimFenceV1(t *testing.T) {
	if got := FilterProtocol([]string{AdviceClaimFenceV1, CredentialSwitchV1}); !reflect.DeepEqual(got, []string{CredentialSwitchV1, AdviceClaimFenceV1}) {
		t.Errorf("FilterProtocol([advice fence, credential switch]) = %v, want [%q %q]", got, CredentialSwitchV1, AdviceClaimFenceV1)
	}
	if got := Filter([]string{AdviceClaimFenceV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", AdviceClaimFenceV1, got)
	}
}

// TestFilterProtocol_KeepsJobRunnerV1 pins PRD #1908 D-A: job_runner_v1 survives registration (the
// job claim clause reads it off workers.protocol_capabilities), is not a scheduler capability,
// and orders last in protocolOrder.
func TestFilterProtocol_KeepsJobRunnerV1(t *testing.T) {
	if got := FilterProtocol([]string{JobRunnerV1, AdviceClaimFenceV1}); !reflect.DeepEqual(got, []string{AdviceClaimFenceV1, JobRunnerV1}) {
		t.Errorf("FilterProtocol([job runner, advice fence]) = %v, want [%q %q]", got, AdviceClaimFenceV1, JobRunnerV1)
	}
	if got := Filter([]string{JobRunnerV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", JobRunnerV1, got)
	}
	if got := SelfReportable([]string{JobRunnerV1}); len(got) != 0 {
		t.Errorf("SelfReportable(%q) = %v, want empty", JobRunnerV1, got)
	}
}

// TestFilterProtocol_KeepsJobFilesV1 pins PRD #1909 M1: job_files_v1, the rollout gate for
// new-protocol jobs, survives registration (ClaimRun's job clause reads it off
// workers.protocol_capabilities), is not a scheduler capability, and orders right after
// job_runner_v1 in protocolOrder. Removing it from protocolVocabulary makes FilterProtocol drop
// it, and a worker could then never claim a new job.
func TestFilterProtocol_KeepsJobFilesV1(t *testing.T) {
	if got := FilterProtocol([]string{JobFilesV1, JobRunnerV1, AdviceClaimFenceV1}); !reflect.DeepEqual(got, []string{AdviceClaimFenceV1, JobRunnerV1, JobFilesV1}) {
		t.Errorf("FilterProtocol([job files, job runner, advice fence]) = %v, want [%q %q %q]", got, AdviceClaimFenceV1, JobRunnerV1, JobFilesV1)
	}
	if got := Filter([]string{JobFilesV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", JobFilesV1, got)
	}
	if got := SelfReportable([]string{JobFilesV1}); len(got) != 0 {
		t.Errorf("SelfReportable(%q) = %v, want empty", JobFilesV1, got)
	}
	if JobFilesV1 != "job_files_v1" {
		t.Errorf("JobFilesV1 = %q: the SQL clauses and agent/src/worker.ts spell it job_files_v1", JobFilesV1)
	}
	if JobProtocolFiles != 2 {
		t.Errorf("JobProtocolFiles = %d: the runs_job_protocol_check migration allows >= 2 and CreateJobRun stamps this", JobProtocolFiles)
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

// TestEffectiveWorkerCaps pins the docker fold EffectiveWorkerCaps applies — the Go mirror
// of SQL fn_effective_worker_caps (migration 00151, single source since #512 M5). It is
// NON-DEDUP by design (`docker` is appended unconditionally when dockerEnabled), matching
// the SQL `||` array concat so the two produce byte-identical multisets, and it must never
// mutate its input slice.
func TestEffectiveWorkerCaps(t *testing.T) {
	cases := []struct {
		name   string
		caps   []string
		docker bool
		want   []string
	}{
		{"nil-no-docker", nil, false, []string{}},
		{"nil-docker", nil, true, []string{Docker}},
		{"jvm-no-docker", []string{JVM}, false, []string{JVM}},
		{"jvm-docker", []string{JVM}, true, []string{JVM, Docker}},
		// Non-dedup: a worker already carrying `docker` yields two, matching the SQL fold.
		{"docker-docker", []string{Docker}, true, []string{Docker, Docker}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveWorkerCaps(tc.caps, tc.docker)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("EffectiveWorkerCaps(%v, %v) = %v, want %v", tc.caps, tc.docker, got, tc.want)
			}
		})
	}
}

// TestEffectiveWorkerCaps_DoesNotMutateInput pins that the returned slice is fresh: appending
// docker must not write into (or alias) the caller's capabilities slice.
func TestEffectiveWorkerCaps_DoesNotMutateInput(t *testing.T) {
	in := []string{JVM}
	got := EffectiveWorkerCaps(in, true)
	if !reflect.DeepEqual(in, []string{JVM}) {
		t.Errorf("EffectiveWorkerCaps mutated its input: in = %v, want [jvm]", in)
	}
	if len(got) != 2 || got[0] != JVM || got[1] != Docker {
		t.Errorf("EffectiveWorkerCaps([jvm], true) = %v, want [jvm docker]", got)
	}
}

// TestFilterProtocol_KeepsIsolatedFetchV1 pins PRD #1906 M5: isolated_fetch_v1 survives
// FilterProtocol (so a lane worker's advertisement is stored and ClaimRun's lane clause can
// read it), last in the stable order, and is never a scheduler capability.
func TestFilterProtocol_KeepsIsolatedFetchV1(t *testing.T) {
	if got := FilterProtocol([]string{IsolatedFetchV1, AdviceClaimFenceV1}); !reflect.DeepEqual(got, []string{AdviceClaimFenceV1, IsolatedFetchV1}) {
		t.Errorf("FilterProtocol([isolated fetch, advice fence]) = %v, want [%q %q]", got, AdviceClaimFenceV1, IsolatedFetchV1)
	}
	if got := Filter([]string{IsolatedFetchV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty (a protocol cap must not be a scheduler cap)", IsolatedFetchV1, got)
	}
}

// TestFilterProtocol_KeepsIsolatedJobV1 pins PRD #1976 M1: isolated_job_v1 survives
// FilterProtocol right after isolated_fetch_v1, is never a scheduler or self-reportable
// capability, and never appears in the user-facing Vocabulary().
func TestFilterProtocol_KeepsIsolatedJobV1(t *testing.T) {
	if IsolatedJobV1 != "isolated_job_v1" {
		t.Fatalf("IsolatedJobV1 = %q, want isolated_job_v1", IsolatedJobV1)
	}
	got := FilterProtocol([]string{IsolatedJobV1, IsolatedFetchV1, AdviceClaimFenceV1, "bogus"})
	want := []string{AdviceClaimFenceV1, IsolatedFetchV1, IsolatedJobV1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilterProtocol = %v, want %v", got, want)
	}
	if got := Filter([]string{IsolatedJobV1}); len(got) != 0 {
		t.Errorf("Filter(%q) = %v, want empty", IsolatedJobV1, got)
	}
	if got := SelfReportable([]string{IsolatedJobV1}); len(got) != 0 {
		t.Errorf("SelfReportable(%q) = %v, want empty", IsolatedJobV1, got)
	}
	for _, v := range Vocabulary() {
		if v == IsolatedJobV1 {
			t.Errorf("Vocabulary() contains %q, a protocol capability", v)
		}
	}
}
