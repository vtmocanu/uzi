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
// Three properties of this set are load-bearing, all three found by folds that a
// deletion-only control cannot produce:
//
//  1. A phrase that does not NAME ITS OWN TURN is relocation-blind. Deleting a
//     clause is not the only way to lose it: move it into the other wave's bullet
//     and a pin quoting only the clause stays green while the behaviour is gone
//     from the turn it bound. Seven phrases (not seven pins — the unit here is
//     the quoted phrase) failed this across two rounds, each after passing its
//     own deletion fold. The fix is semantic, not length: every phrase below
//     carries a token that states which turn it binds — `after an implementation
//     unit lands`, `submit_plan`, `the plan text`, `you send over the plan`,
//     `during the plan turn`, `re-planning turn`, `the plan you produced`.
//     WHICH FOLD DISCRIMINATES DEPENDS ON WHAT THE PIN CLAIMS, and getting this
//     backwards is how two people fold the same mutation and report opposite
//     verdicts. For a DESCRIPTIVE pin the behaviour is *the template states rule
//     R*, so a relocated sentence still states it: green is correct there and
//     relocation cannot produce a disconfirming answer at all — REVERSION (put
//     the original site back to its un-anchored wording) is the fold that can.
//     For a pin about something the lead must DO AT A PARTICULAR MOMENT, the
//     behaviour is constituted by where the sentence sits, relocation destroys
//     it, and relocation is still required. The tell is inside the phrase: a pin
//     is relocation-proof exactly when nothing in it takes its meaning from a
//     neighbouring sentence.
//     The `anchor` field is that token, and the audit below asserts it is
//     actually inside the phrase. Two rounds of hand-checking missed a
//     freshly-anchored phrase each time; this is a manual property with nothing
//     enforcing it, so it is enforced here — but see property 4 for what that
//     enforcement does and does not cover.
//  2. A constraint the lead must TRANSMIT has to be pinned as a transmission,
//     and the phrase has to NAME ITS RECIPIENTS. A subagent's system prompt is
//     its own template body and the lead cannot alter it, so the dispatch prompt
//     is the only channel; `architect` and `tester` both ship with `Edit, Write`.
//     This pin read `tell each of them …` for two rounds, and `each of them`
//     took its referent from the preceding sentence: relocated, it silently
//     re-resolved to the post-implementation validators while the plan dispatch
//     was told nothing — a behaviour destroyed and a different one created, with
//     the pin green. Naming the recipients (`each validator you send over the
//     plan`) removes the referent rather than anchoring around it, which is what
//     makes this pin expressible as a substring at all.
//  3. NONE OF THIS DETECTS AN INSERTION, and no amount of it ever will.
//     `strings.Contains` is monotone under insertion: adding text can only turn
//     a false into a true, never the reverse. So a paragraph inserted above this
//     one declaring the whole thing optional — "skip it and call `submit_plan`
//     straight away" — neutralises every behaviour here with all pins byte-intact.
//     That is a property of substring-presence as an instrument, not a bad choice
//     of phrases, and no anchoring, widening or region-scoping closes it. The
//     obvious patch, a negative assertion on the inserted wording, is the
//     vacuous-negative trap (it guards a string nothing renders). Anchoring
//     closed RELOCATION; insertion stays open and is caught by reading the diff.
//  4. THE AUDIT IS A SYNTACTIC CONTAINMENT CHECK, and neither semantic property
//     it would need is expressible as a substring relation. One root, two
//     consequences, both open:
//     (a) it cannot check the anchor NAMES A TURN — only that the declared token
//     is present. Measured on this table: `anchor: ""` passes, because
//     `Contains(x, "")` is always true, and so does a vague token like `again`.
//     Applies to every pin; a better-chosen anchor fixes any instance, so this
//     is a quality gap. A turn-token allowlist was considered and REJECTED — it
//     would catch the vague token, leave (b) untouched, and go stale, so the
//     audit would look stronger with the load-bearing hole in place.
//     (b) it cannot check the behaviour is ANCHORABLE AT ALL. For a pin whose
//     phrase carries a context-bound referent, no anchor fixes it. Measured
//     against the previous commit's relay pin, which had a genuine anchor and
//     nothing else changed: relocating that clause left the pin AND every audit
//     assertion green over a template where the plan-turn dispatch is told
//     nothing and the relocated `each of them` re-resolves to the
//     post-implementation validators. That is why the fix here was to delete the
//     referent (property 2) rather than anchor around it. Unfixable inside the
//     anchor model; the model is provisional and #205 replaces it.
//
// Deleting a whole sentence reddens every case living in that sentence, and
// sentence granularity is how anyone actually folds prose: measured, 2 for the
// `submit_plan` sentence, 3 for the long dispatch sentence, 1 for each of the
// other three. Any other multiplicity is a bug, not this trade.
// The spans themselves are pairwise disjoint and each occurs exactly once
// — both audited below, because `strings.Contains` is per-occurrence and two pins
// satisfied by two different occurrences of overlapping text prove less than they
// appear to.
func TestLeadPlanCritiquePhrases(t *testing.T) {
	lead, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("lead builtin missing")
	}
	body := flatten(string(Render(lead)))

	cases := []struct {
		behavior string
		phrase   string
		// anchor names the turn the behavior binds to and MUST appear inside
		// phrase — see property 1 above.
		anchor string
	}{
		{"post-implementation wave is retained and is a REPEAT",
			"Read-only work fans out again after an implementation unit lands",
			"after an implementation unit lands"},
		{"the citation property is due before submit_plan",
			"Before you call `submit_plan`, make the plan carry its own evidence",
			"submit_plan"},
		{"every asserted mechanism is cited by file and line",
			"`submit_plan`, make the plan carry its own evidence: for every mechanism it asserts, name the file that implements it and quote the line",
			"submit_plan"},
		{"the wave reviews the plan text, not a diff",
			"the artifact under review is the plan text, not a diff",
			"the plan text"},
		{"the no-write rule is RELAYED to each dispatched validator",
			"tell each validator you send over the plan that it must not change anything in the worktree",
			"you send over the plan"},
		{"the no-write rule binds the PLAN TURN specifically",
			"an edit made during the plan turn is a change nobody saw when approving it",
			"during the plan turn"},
		{"re-planning re-cites only what changed",
			"On any re-planning turn, re-cite only the mechanisms that changed",
			"re-planning turn"},
		{"the bar is a property of the plan, never of the issue text",
			"follows from the plan you produced — how many mechanisms it asserts — never as a judgement about the issue text",
			"the plan you produced"},
	}
	for _, c := range cases {
		if !strings.Contains(body, c.phrase) {
			t.Errorf("lead template lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}

	// Audit, not a behavior pin: these assert properties OF THE CASE TABLE, so
	// they fail on a badly-written pin rather than on a changed template.
	for _, c := range cases {
		if !strings.Contains(c.phrase, c.anchor) {
			t.Errorf("pin %q is unanchored: phrase %q does not contain its turn anchor %q",
				c.behavior, c.phrase, c.anchor)
		}
		if n := strings.Count(body, c.phrase); n > 1 {
			t.Errorf("pin %q matches %d occurrences; a per-occurrence match cannot say which one satisfied it", c.behavior, n)
		}
	}
	for i, a := range cases {
		for j, b := range cases {
			if i != j && strings.Contains(a.phrase, b.phrase) {
				t.Errorf("pin %q contains pin %q, so the contained one can never fail alone", a.behavior, b.behavior)
			}
		}
	}
	// The 14 pins in TestLeadParallelDispatchPhrases are a separate set and must
	// stay independently falsifiable: an earlier version of the first case here
	// swallowed `send all allocated read-only validators together in one wave`
	// whole, which silently retired that pin.
	for _, c := range cases {
		if strings.Contains(c.phrase, "send all allocated read-only validators together in one wave") {
			t.Errorf("pin %q contains a TestLeadParallelDispatchPhrases phrase, retiring it", c.behavior)
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
