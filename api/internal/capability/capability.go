// Package capability is the server-owned vocabulary of worker capabilities and
// the template→capabilities map (PRD #84 M1). A capability is a coarse,
// scheduler-relevant fact about what a worker CAN run — today the v1 vocabulary
// is exactly {docker, jvm}. It is deliberately a code constant, not a DB table or
// free-form worker text: the set is small, reviewed like any code, and the
// server must be the sole authority on which names are legal so a hostile or
// garbled worker report cannot smuggle a novel "capability" past the matcher a
// later milestone will build on top of workers.capabilities.
//
// Two sources feed a worker's stored set, unioned then Filter-ed at register:
//   - TEMPLATE-DERIVED: implied by the worker's image template (jvm → {jvm}).
//   - SELF-REPORTED: what the worker announces it can reach (today only
//     {docker}, meaning a rootless-DinD daemon is reachable). docker is a valid
//     vocabulary member even though no template implies it.
package capability

import "fmt"

const (
	// Docker means a container daemon (rootless DinD sidecar) is reachable. It is
	// worker-SELF-REPORTED, never template-derived.
	Docker = "docker"
	// JVM means a Java runtime is present. It is TEMPLATE-derived (the jvm
	// template) and not something a worker self-reports.
	JVM = "jvm"
)

// vocabulary is the v1 closed set of legal capability names. Filter drops
// anything not in here; nothing outside this map is ever persisted or surfaced.
var vocabulary = map[string]struct{}{
	Docker: {},
	JVM:    {},
}

// order fixes Filter's stable output order (vocabulary is a map, so its own
// iteration order is not stable). Keep in lockstep with vocabulary.
var order = []string{Docker, JVM}

// Vocabulary returns the v1 closed set of legal capability names — today exactly
// {docker, jvm} — in stable order. This package is the SINGLE SOURCE OF TRUTH for
// that vocabulary: the web mirror (web/src/lib/capabilityVocabulary.ts) is pinned
// against it by a golden (api/internal/capability/testdata/vocabulary.json), so the
// two cannot drift silently. The returned slice is a fresh copy the caller may
// mutate; the package's own order slice is never exposed.
func Vocabulary() []string {
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// selfReportable is the subset of the vocabulary a worker is allowed to
// SELF-REPORT. Today it is exactly {docker}: JVM is TEMPLATE-derived and must
// never be accepted from a worker's own report, or a base worker could spoof a
// jvm capability it does not have. SelfReportable enforces this before the
// self-report side is unioned with the template-derived caps.
var selfReportable = map[string]struct{}{
	Docker: {},
}

// SelfReportable returns the members of in that a worker is permitted to
// SELF-REPORT — today only docker — DROPPING everything else (including the
// template-derived jvm), deduped, in stable vocabulary order. It is the gate the
// self-report side passes through before it is unioned with template-derived
// caps at register, so a worker cannot smuggle a template-only capability past
// the scheduler by announcing it.
func SelfReportable(in []string) []string {
	return filterOrdered(in, selfReportable, order)
}

// filterOrdered keeps only the names present in allowed, de-duplicated and emitted in
// the fixed order given by order. (Shared body for Filter/SelfReportable/FilterTools.)
func filterOrdered(in []string, allowed map[string]struct{}, order []string) []string {
	seen := make(map[string]struct{}, len(in))
	for _, name := range in {
		if _, ok := allowed[name]; !ok {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for _, name := range order {
		if _, ok := seen[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// templateCaps maps a worker-template registry name (workertmpl.Names) to the
// capabilities that template implies. A template absent from this map — or the
// empty string — contributes nothing (returns {}), which is the correct answer
// for the base template and for an unknown/older reported template alike: the
// map is authoritative and an omission is "no implied capability", never an
// error.
var templateCaps = map[string][]string{
	"base": {},
	"jvm":  {JVM},
}

// TemplateCapabilities returns the capabilities implied by a worker image
// template. An unknown or empty template returns an empty slice (never nil,
// never an error). The returned slice is a fresh copy the caller may mutate.
func TemplateCapabilities(template string) []string {
	caps, ok := templateCaps[template]
	if !ok {
		return []string{}
	}
	out := make([]string, len(caps))
	copy(out, caps)
	return out
}

// Filter returns the members of in that are in the vocabulary, DROPPING unknowns
// silently (never an error), deduped, in stable vocabulary order. It is the
// single gate every capability set passes through before it is stored or
// surfaced, so the server — not the worker — decides what names are legal.
func Filter(in []string) []string {
	return filterOrdered(in, vocabulary, order)
}

// Unmet returns the members of required that are NOT present in effective — the
// capabilities a run needs that a given (already-folded) effective worker set does not
// satisfy. It is the pure, offline-checkable core of the PRD #84 M4 4c approval gate: the
// caller folds the owning worker's effective caps (capabilities ∪ {docker if
// docker_enabled}, the SAME fold fn_worker_can_claim applies) and passes them here, so a
// docker-folded effective set yields no `docker` in the result. Output is deduped and in
// stable vocabulary order; a `required` name outside the vocabulary is dropped (it is not
// a legal capability and can never be a real unmet requirement — required_capabilities is
// Filter-ed at every write, so this never discards a live requirement). An empty result
// means required ⊆ effective, i.e. the run is claimable/approvable by that worker.
func Unmet(required, effective []string) []string {
	have := make(map[string]struct{}, len(effective))
	for _, name := range effective {
		have[name] = struct{}{}
	}
	need := make(map[string]struct{}, len(required))
	for _, name := range required {
		if _, ok := have[name]; !ok {
			need[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(need))
	for _, name := range order {
		if _, ok := need[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// ResolveEphemeralSpec maps a run's required (already Filter-ed) capability set to the
// {template, docker} an ephemeral hosted worker must be provisioned with (PRD #529 M1).
// docker maps to the docker_enabled dimension (not a template); every other capability
// must be provided by a template (jvm -> "jvm"), else "base". The template is derived
// from templateCaps — the source of truth for which template implies which capability —
// so a future template addition is picked up without editing this function. Returns an
// error if a required capability is satisfiable by neither the docker dimension nor any
// template — it cannot be provisioned. Size is NOT resolved here (it is a configured
// default set by the create path). Pure and DB-free: capability is a leaf package.
func ResolveEphemeralSpec(required []string) (template string, docker bool, err error) {
	template = "base"
	for _, name := range required {
		if name == Docker {
			docker = true
			continue
		}
		if t, ok := templateProviding(name); ok {
			template = t
			continue
		}
		return "", false, fmt.Errorf("capability %q is not provisionable by the docker dimension or any worker template", name)
	}
	return template, docker, nil
}

// templateProviding returns the (non-base) worker template whose templateCaps entry
// includes cap, deriving the mapping from templateCaps so a new template is picked up
// automatically. The base template implies nothing, so it never satisfies a capability
// and is skipped; ok is false when no template provides cap.
func templateProviding(cap string) (template string, ok bool) {
	for name, caps := range templateCaps {
		if name == "base" {
			continue
		}
		for _, c := range caps {
			if c == cap {
				return name, true
			}
		}
	}
	return "", false
}

// EffectiveWorkerCaps folds a worker's docker-enabled flag INTO its stored capability
// set — the worker's caps plus `docker` when dockerEnabled — the Go mirror of SQL's
// fn_effective_worker_caps (migration 00151) and the SINGLE source of that fold on the Go
// side (issue #512 M5). effectiveOwningWorkerCaps calls it so the PRD #84 M4 4c approval
// gate evaluates the identical set fn_worker_can_claim folds at claim time. It returns a
// NEW slice and never mutates caps. Like the SQL fold it is NON-DEDUP: `docker` is appended
// unconditionally when dockerEnabled, even if caps already contains it — matching the SQL
// `||` array concat, so the two produce byte-identical multisets. (Callers that need a set
// membership, e.g. Unmet, treat the result as a set anyway, so the duplicate is harmless.)
func EffectiveWorkerCaps(caps []string, dockerEnabled bool) []string {
	effective := append([]string{}, caps...)
	if dockerEnabled {
		effective = append(effective, Docker)
	}
	return effective
}

// Tool names are the PROVISIONABLE toolchain families the plan-time inference
// (PRD #84 M4) emits on the awaiting_approval report — the languages/runtimes a
// worker could install for the run, as distinct from the non-provisionable
// capabilities above. Unlike capabilities they are DISPLAY-ONLY in v1: they never
// gate a claim (there is no fn_worker_can_claim clause for them). The server still
// owns the name set, and every reported tool set passes through FilterTools before
// storage, so a garbled or hostile worker report cannot smuggle an arbitrary string
// into the run's requirement panel.
const (
	ToolGo     = "go"
	ToolNode   = "node"
	ToolPython = "python"
	ToolRust   = "rust"
	ToolJVM    = "jvm"
)

// toolVocabulary is the v1 closed set of legal provisionable-tool names. FilterTools
// drops anything not in here; nothing outside this map is ever persisted or surfaced.
var toolVocabulary = map[string]struct{}{
	ToolGo:     {},
	ToolNode:   {},
	ToolPython: {},
	ToolRust:   {},
	ToolJVM:    {},
}

// toolOrder fixes FilterTools's stable output order (toolVocabulary is a map, so its
// own iteration order is not stable). Keep in lockstep with toolVocabulary.
var toolOrder = []string{ToolGo, ToolNode, ToolPython, ToolRust, ToolJVM}

// FilterTools returns the members of in that are in the tool vocabulary, DROPPING
// unknowns silently (never an error), deduped, in stable vocabulary order. It mirrors
// Filter and is the single gate every provisionable-tool set passes through before it
// is stored or surfaced, so the server — not the worker — decides what tool names are
// legal even though the set never gates scheduling.
func FilterTools(in []string) []string {
	return filterOrdered(in, toolVocabulary, toolOrder)
}
