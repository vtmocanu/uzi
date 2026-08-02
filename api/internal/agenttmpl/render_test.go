package agenttmpl

import (
	"strings"
	"testing"
)

// builtinNames is the exact set of roles the product ships and PRD #4 depends on
// existing. The lead orchestrator template (PRD #17) is one of them; architect and
// researcher were added from the agent-team role library (design-before-code and
// read-only context-gathering roles the product previously lacked). This is the
// single source of truth: builtins are the embedded builtins/*.md files, no
// longer mirrored from .claude/agents/. Keep this list sorted — Builtins() returns
// its slice sorted by name and TestBuiltinsSetIsExactlyEleven compares index-for-index.
var builtinNames = []string{
	"architect", "auditor", "coder", "documenter", "fact-checker",
	"lead", "researcher", "reviewer", "spec-keeper", "tester", "web-ux",
}

func TestBuiltinsSetIsExactlyEleven(t *testing.T) {
	got := Builtins()
	if len(got) != len(builtinNames) {
		t.Fatalf("got %d builtins, want %d", len(got), len(builtinNames))
	}
	for i, name := range builtinNames {
		if got[i].Name != name {
			t.Errorf("builtin[%d] = %q, want %q (should be sorted)", i, got[i].Name, name)
		}
	}
}

// TestBuiltinsParseAndValid replaces the removed golden byte-match tests
// (TestRenderBuiltinsByteMatch / TestEmbeddedCopiesMatchRepo, which pinned the
// builtins to .claude/agents/*.md). Now that builtins/ is the single source of
// truth, the guarantee is that every embedded file parses and carries sane
// frontmatter: non-empty name/description, unique names, a non-empty body, and
// a well-formed model alias when one is set.
func TestBuiltinsParseAndValid(t *testing.T) {
	seen := make(map[string]bool)
	for _, def := range Builtins() {
		if def.Name == "" {
			t.Errorf("builtin has empty name: %+v", def)
		}
		if def.Description == "" {
			t.Errorf("builtin %q has empty description", def.Name)
		}
		if strings.TrimSpace(def.PromptBody) == "" {
			t.Errorf("builtin %q has empty prompt body", def.Name)
		}
		if seen[def.Name] {
			t.Errorf("duplicate builtin name %q", def.Name)
		}
		seen[def.Name] = true
		// Validate the model against the shared, authoritative rule (no separate
		// test-local copy that could drift from the server validator).
		if _, err := ValidateModel(def.Model); err != nil {
			t.Errorf("builtin %q has an invalid model %q: %v", def.Name, def.Model, err)
		}
	}
}

// TestBuiltinsRoundTripRender pins the parse->Render round-trip byte-for-byte
// against the embedded source, replacing the old golden test's byte-match (which
// compared against .claude/agents/). Render of the parsed definition must
// reproduce the on-disk builtin exactly, so the renderer stays the canonical
// serializer for what boot-seeds into the database.
func TestBuiltinsRoundTripRender(t *testing.T) {
	for _, name := range builtinNames {
		t.Run(name, func(t *testing.T) {
			def, ok := BuiltinByName(name)
			if !ok {
				t.Fatalf("builtin %q not found", name)
			}
			want, err := builtinFS.ReadFile("builtins/" + name + ".md")
			if err != nil {
				t.Fatalf("read embedded builtin: %v", err)
			}
			if got := Render(def); string(got) != string(want) {
				t.Errorf("Render(%q) does not round-trip the embedded file\n--- got ---\n%s\n--- want ---\n%s",
					name, got, want)
			}
		})
	}
}

// TestLeadBuiltin pins the shipped lead orchestrator template: it exists, runs
// on opus, and inherits all tools (no tools line — the lead needs the full
// toolset, same null-tools contract the coder uses). The worker routes it to the
// main thread by name convention; the agent side asserts the name matches its
// LEAD_NAME_RE.
func TestLeadBuiltin(t *testing.T) {
	lead, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("lead builtin missing")
	}
	if lead.Model != "opus" {
		t.Errorf("lead.Model = %q, want %q", lead.Model, "opus")
	}
	if len(lead.Tools) != 0 {
		t.Errorf("lead.Tools = %v, want empty (inherit all)", lead.Tools)
	}
	if strings.Contains(string(Render(lead)), "\ntools:") {
		t.Error("rendered lead must not contain a tools line")
	}
}

// TestCoderInheritsAllTools pins the confirmed gotcha: coder.md carries no tools
// line, so its builtin must inherit all tools (empty Tools) while still setting
// a model.
func TestCoderInheritsAllTools(t *testing.T) {
	coder, ok := BuiltinByName("coder")
	if !ok {
		t.Fatal("coder builtin missing")
	}
	if len(coder.Tools) != 0 {
		t.Errorf("coder.Tools = %v, want empty (inherit all)", coder.Tools)
	}
	if coder.Model != "opus" {
		t.Errorf("coder.Model = %q, want %q", coder.Model, "opus")
	}
	rendered := string(Render(coder))
	if strings.Contains(rendered, "\ntools:") {
		t.Error("rendered coder must not contain a tools line")
	}
	if !strings.Contains(rendered, "\nmodel: opus\n") {
		t.Error("rendered coder must contain the model line")
	}
}

// flatten collapses every run of whitespace (including the body's hard line
// wraps) to a single space, so a phrase assertion matches the prose regardless
// of where a line break happens to fall. Without this, a load-bearing phrase
// that straddles a wrap point would look "missing" to a naive substring test.
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestLeadParallelDispatchPhrases pins the load-bearing behaviors of the lead's
// parallel-dispatch prose (PRD #43 M1), one assertion per behavior so a future
// reword that silently drops a behavior fails loudly. These are the contract
// the run's fan-out depends on, not incidental wording.
func TestLeadParallelDispatchPhrases(t *testing.T) {
	lead, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("lead builtin missing")
	}
	body := flatten(string(Render(lead)))

	cases := []struct {
		behavior string
		phrase   string
	}{
		{"parallel dispatch in a single turn", "Dispatch independent subagents in parallel in a single turn"},
		{"read-only validators fan out in one wave, allocation-agnostic", "send all allocated read-only validators together in one wave"},
		{"implementation fans out only with no dependency between units", "units with no dependency between them"},
		{"disjoint ownership at the package or module level", "disjoint ownership at the package or module level"},
		{"never the same package, project, or shared file", "never touch the same Go package, the same TypeScript project, or any shared file"},
		{"shared-wiring exclusion list is enumerated", "go.mod, go.sum, lockfiles, generated code, routers and registration files, compose or config files"},
		{"same coder may be invoked several times in parallel", "The same coder subagent may be invoked several times in parallel"},
		{"explicit non-overlapping file scope per implementer", "an explicit, non-overlapping list of files and directories it owns"},
		{"parallel implementers do not commit or run repo-wide gates", "tell it not to commit and not to run repo-wide build or test commands"},
		{"lead diffs against the last commit and confirms declared scopes", "diff the working tree against the last commit and confirm only the declared scopes changed"},
		{"lead commits once and runs the gate once itself", "commit once, run the quality gates once yourself"},
		{"declared scope map goes to the review wave", "include the declared scope map when you dispatch the review wave"},
		{"when in doubt, run serially", "run them serially"},
		{"sequential-by-nature work stays serial", "stays serial"},
	}
	for _, c := range cases {
		if !strings.Contains(body, c.phrase) {
			t.Errorf("lead template lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}
}

// TestLeadPlanCritiquePhrases pins the plan-time half of the lead's dispatch
// contract (issue #197): the plan must cite the code it asserts, and the wave
// that produces those citations runs before `submit_plan`, reads the plan text
// rather than a diff, and writes nothing.
//
// It is a SEPARATE case set from TestLeadParallelDispatchPhrases on purpose.
// The ordering this issue changed was entirely unpinned — `after an
// implementation unit lands` appeared nowhere in this file — so a mutant that
// deleted the post-implementation ordering and replaced it with a plan-time
// wave kept all 14 of those pins green by reusing the one clause they share
// (`send all allocated read-only validators together in one wave`), which is
// deliberately prefix-agnostic. The first case below fixes the wave that
// happens AFTER implementation; the rest fix the one that happens BEFORE
// `submit_plan`, so no single sentence can satisfy both meanings.
//
// Two properties of this set are load-bearing and were both found the hard way,
// by folds a deletion-only control cannot produce:
//
//  1. A phrase with no anchor to a wave is RELOCATION-BLIND. Deleting a clause
//     is not the only way to lose it: move it into the other wave's bullet and
//     every pin naming only the clause stays green while the behaviour is gone
//     from the turn it bound. Measured on four separate phrases here, each of
//     which passed a deletion control and then failed a relocation one. So every
//     case below quotes enough context to name its wave — the ordering case
//     carries the bullet's own opening, the plan-turn cases carry `Before you
//     call submit_plan`, `over the plan in the same turn`, `an edit made during
//     the plan turn`, or the sentence they follow.
//     Consequence, deliberate and not an oversight: the spans are NOT pairwise
//     disjoint. Two anchors are the tail of the previous case (the citation
//     phrase carries `make the plan carry its own evidence:`, the revise phrase
//     carries `is a change nobody saw when approving it.`), so deleting either
//     anchor sentence reddens two cases rather than one. Relocation is what the
//     overlap buys, and it is worth more here than a clean one-fold-one-red
//     table.
//  2. A constraint the lead must TRANSMIT has to be pinned as a transmission.
//     A subagent's system prompt is its own template body and the lead cannot
//     alter it, so the dispatch prompt is the only channel; `architect` and
//     `tester` both ship with `Edit, Write`. The pin is therefore on `tell each
//     of them the wave must not change anything`, which the un-relayed wording
//     (`That wave must not change anything in the worktree`) does not satisfy.
func TestLeadPlanCritiquePhrases(t *testing.T) {
	lead, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("lead builtin missing")
	}
	body := flatten(string(Render(lead)))

	cases := []struct {
		behavior string
		phrase   string
	}{
		{"post-implementation wave is retained and is a REPEAT", "Read-only work fans out again after an implementation unit lands: send all allocated read-only validators together in one wave"},
		{"the citation property is anchored on submit_plan", "Before you call `submit_plan`, make the plan carry its own evidence"},
		{"every asserted mechanism is cited by file and line", "make the plan carry its own evidence: for every mechanism it asserts, name the file that implements it and quote the line"},
		{"the plan-turn wave reviews the plan text, not a diff", "over the plan in the same turn. Say in each dispatch that the artifact under review is the plan text, not a diff"},
		{"the no-write rule is RELAYED to each dispatched validator", "tell each of them the wave must not change anything in the worktree"},
		{"the no-write rule binds the PLAN TURN specifically", "an edit made during the plan turn is a change nobody saw when approving it"},
		{"a revise turn re-cites only what the revision changed", "is a change nobody saw when approving it. On a revise turn, re-cite only the mechanisms your revision changed"},
		{"the bar is a property of the plan, never of the issue text", "never as a judgement about the issue text"},
	}
	for _, c := range cases {
		if !strings.Contains(body, c.phrase) {
			t.Errorf("lead template lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}
}

// TestCoderParallelModeContract pins the coder's parallel-mode contract (PRD #43
// M1): the hard file-scope boundary, stop-and-report on out-of-scope or shared
// files, no commit in parallel mode, and gate only what it exclusively owns.
func TestCoderParallelModeContract(t *testing.T) {
	coder, ok := BuiltinByName("coder")
	if !ok {
		t.Fatal("coder builtin missing")
	}
	body := flatten(string(Render(coder)))

	cases := []struct {
		behavior string
		phrase   string
	}{
		{"may run as one of several parallel coders", "dispatched as one of several coders working in parallel"},
		{"assigned file scope is a hard boundary", "treat it as a hard boundary"},
		{"stop and report on out-of-scope need", "stop and report that instead of editing it"},
		{"shared files are called out as out-of-scope", "including shared files like go.mod, lockfiles, generated code, or wiring and registration files"},
		{"no git commit in parallel mode", "In parallel mode do not run `git commit`"},
		{"no gate/build/test unless it covers only exclusively-owned code", "do not run gate, build, or test commands unless they cover only code you exclusively own"},
		{"lead integrates, commits, and runs the repo-wide gate", "the lead integrates, commits, and runs the repo-wide gate"},
	}
	for _, c := range cases {
		if !strings.Contains(body, c.phrase) {
			t.Errorf("coder template lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}
}

// TestRenderFieldOrderAndOmission pins the fixed field order and the omit-when-
// empty behaviour for a synthetic definition covering all four fields.
func TestRenderFieldOrderAndOmission(t *testing.T) {
	full := Render(Definition{
		Name:        "example",
		Description: "an example.",
		Model:       "sonnet",
		Tools:       []string{"Bash", "Read", "Grep"},
		PromptBody:  "Body.\n",
	})
	want := "---\nname: example\ndescription: an example.\ntools: Bash, Read, Grep\nmodel: sonnet\n---\n\nBody.\n"
	if string(full) != want {
		t.Errorf("full render mismatch\n got: %q\nwant: %q", full, want)
	}

	inherit := Render(Definition{
		Name:        "bare",
		Description: "no tools, no model.",
		PromptBody:  "Body.\n",
	})
	wantInherit := "---\nname: bare\ndescription: no tools, no model.\n---\n\nBody.\n"
	if string(inherit) != wantInherit {
		t.Errorf("inherit render mismatch\n got: %q\nwant: %q", inherit, wantInherit)
	}
}
