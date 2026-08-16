package skilltmpl

import "fmt"

// Default allocations for builtin skills (PRD #72 M2).
//
// An allocation is what makes a skill reachable AT ALL. `ListRunSkillAllocations`
// builds the run union from `agent_skill_allocations`, so a builtin with no
// allocation row reaches nobody — not the subagents it was scoped to, and not the
// lead either (the lead receives the whole union, but an unallocated skill is not
// in it). Builtin agent TEMPLATES have auto-seeded their global-default row since
// PRD #18 M7 (`SeedSharedTemplateAllocationByName`); skills shipped without the
// equivalent, which is why `docs/skills.md` told admins to click allocate by hand.
//
// Why this map lives in Go and not in the SKILL.md frontmatter (Decision 8):
// `Definition` carries Name/Description/Body and `Parse` rejects every other
// frontmatter key as an authoring error. That strictness is worth keeping — a key
// like `allowed-tools` must never reach the model through an authoring channel —
// and a default allocation is uzi's product decision rather than authored data.
//
// The VALUES are agent-template names (`agent_templates.name`), not skill names.
// A name here that matches no template is not a compile error, so two things
// guard it: a test in this package pins every entry against the shipped builtin
// templates, and the reconciler warns at runtime on a seed that inserted no row
// (Decision 9) — the second is what covers a template an ADMIN deleted, which no
// test can see.
var defaultAllocations = map[string][]string{
	// PRD #72 M3. `lead` is the semantic owner (it holds the PRD file and calls
	// signal_done) and is guaranteed to exist under either agent source, since the
	// lead always comes from the claim payload. But allocating to `lead` is NOT
	// what delivers the skill to it: the lead is the main thread and receives the
	// whole run union, so ANY allocation puts this in its context. What the entry
	// buys is union membership at all (without which it reaches nobody) plus, on an
	// own-template run, per-subagent scoping for `reviewer`, who is told to check
	// the PRD diff. On a repo-source run the reviewer half is ignored — every repo
	// subagent gets the whole surviving set (M1) — so it is an own-run control.
	//
	// This entry and builtins/prd-lifecycle/SKILL.md MUST land together: the init
	// check above panics on a key with no shipped skill, so splitting them across
	// commits does not boot.
	"prd-lifecycle": {"lead", "reviewer"},
}

// The KEYS must name shipped builtin skills, and this is enforced at package
// init — a panic, matching what this package already does for a builtin whose
// SKILL.md fails to parse or whose directory disagrees with its frontmatter name.
// The map is compile-time data, so this can only fire on a build where a
// developer made the mistake; it cannot depend on runtime state, and it fails
// identically in CI, in tests, and at boot.
//
// It matters exactly one milestone out. PRD #72 M3 must add
// `builtins/prd-lifecycle/SKILL.md` and its map entry IN THE SAME COMMIT. With
// only a test guarding this, splitting them across commits would leave an entry
// that silently never fires — the failure mode this whole milestone exists to
// remove. With the panic, the split does not boot, which is the loud version.
//
// The VALUES cannot be checked here: agent-template names live in `agenttmpl`,
// and importing it would add a production dependency to this package purely to
// catch a developer error. They are pinned by a package-external test instead
// (allocations_test.go), where the import costs nothing, backed at runtime by
// the reconciler's zero-row warning — which is also the only layer that can see
// a template an ADMIN deleted.
// Called from skilltmpl.go's init AFTER the builtins are parsed. NOT its own
// init(): Go runs a package's init functions in FILENAME order, and
// allocations.go sorts before skilltmpl.go — so an init() here would read an
// empty `builtins` and panic on a perfectly healthy tree. (Measured, not
// reasoned: that is exactly what the first version of this check did.)
func validateDefaultAllocations() {
	shipped := make(map[string]bool, len(builtins))
	for _, d := range builtins {
		shipped[d.Name] = true
	}
	for name, targets := range defaultAllocations {
		if !shipped[name] {
			panic(fmt.Sprintf(
				"skilltmpl: defaultAllocations names %q, which is not a shipped builtin skill "+
					"(add builtins/%s/SKILL.md in the same commit as the map entry, or drop the entry)", name, name))
		}
		if len(targets) == 0 {
			panic(fmt.Sprintf("skilltmpl: defaultAllocations[%q] has no target templates; drop the entry instead", name))
		}
	}
}

// DefaultAllocationsFor returns the agent-template names a builtin skill is
// allocated to by default, or nil when it declares none. The reconciler calls
// this ONLY for a skill it just inserted, so an admin who later removes a default
// keeps it removed (Decision 9, mirroring ReconcileBuiltinTemplates' `n > 0`
// gate). The returned slice is a copy, so callers cannot mutate package state.
func DefaultAllocationsFor(name string) []string {
	src, ok := defaultAllocations[name]
	if !ok {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// DefaultAllocationNames returns the builtin skill names that declare a default
// allocation. Test-facing: it lets the pin over the shipped templates enumerate
// the map without exporting it (an exported map would be mutable by any caller).
func DefaultAllocationNames() []string {
	out := make([]string, 0, len(defaultAllocations))
	for name := range defaultAllocations {
		out = append(out, name)
	}
	return out
}
