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
	seen := make(map[string]struct{}, len(in))
	for _, name := range in {
		if _, ok := vocabulary[name]; !ok {
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
