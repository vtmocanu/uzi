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
// its slice sorted by name and TestBuiltinsSetIsExactlyTwelve compares index-for-index.
var builtinNames = []string{
	"architect", "auditor", "coder", "documenter", "fact-checker",
	"lead", "researcher", "reviewer", "spec-keeper", "tester", "ux-designer", "web-ux",
}

func TestBuiltinsSetIsExactlyTwelve(t *testing.T) {
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
//
// It additionally holds the corpus to WHITESPACE HYGIENE on the frontmatter
// values, which is a comparison invariant rather than a cosmetic one (issue #201
// M4a). The write path trims description (validateTemplateFields) and this
// parser does not (strings.Cut on ": "), so a builtin shipping a padded value
// would seed a row that flips to the trimmed form on the first no-op save and
// then reports drift with nothing changed. Fixing it at the source keeps
// SameContent free of a trim that would hide whitespace-only edits forever.
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
		// Whitespace hygiene on every frontmatter value. Asserted per field
		// rather than over the raw file so the message names the offender.
		for _, f := range []struct{ field, value string }{
			{"name", def.Name},
			{"description", def.Description},
			{"model", def.Model},
		} {
			if f.value != strings.TrimSpace(f.value) {
				t.Errorf("builtin %q has padded %s %q: the write path trims it, so this row "+
					"would report drift after a no-op save", def.Name, f.field, f.value)
			}
		}
		for _, tool := range def.Tools {
			if tool != strings.TrimSpace(tool) {
				t.Errorf("builtin %q has a padded tool name %q", def.Name, tool)
			}
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
//
// THESE FOURTEEN ARE WHOLE-BODY AND ARE NOT REGION-SCOPED. `splitLeadRegions`
// further down this file scopes the OTHER eight pins to their section of the
// template; it does not apply here, so a phrase below satisfies its assertion
// from anywhere in `lead.md`. Measured: moving one of these constraints out of
// its bullet and up into the plan-turn paragraph — across the boundary that
// test splits on — leaves both sets green. Narrow in practice, since these
// phrases nearly all live in the bullets and have nowhere misleading to go, but
// do not read this file's region machinery as covering this test.
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
		{"read-only validators review an immutable commit range, not a mutating tree", "over the immutable range `<base>..<sha>`"},
		{"integration gate overlaps the read-only wave rather than serializing ahead of it", "overlapped with the read-only wave you just dispatched, never serialized ahead of it"},
		{"integration gate keeps blocking authority over the commit", "The gate keeps full blocking authority over the commit"},
		{"declared scope map goes to the review wave", "include the declared scope map when you dispatch the review wave"},
		{"when in doubt, run serially", "run them serially"},
		{"sequential-by-nature work stays serial", "stays serial"},
		// Pointer handover (2026-08-04). Same contract class as the bullets above:
		// the fan-out depends on the lead handing over what it already found, so a
		// reword that drops one of these silently restores the N-validators-re-grep
		// cost the rule exists to remove.
		{"lead hands over locations it already found", "Hand over the locations you have: name the files"},
		{"citations quote the line, not just its number", "quote the line as well as giving its number"},
		{"pointer set is labelled exhaustive or starting point", "Say whether that list is exhaustive or a starting point"},
		{"the set of locations is itself a claim", "the set of locations is itself a claim"},
		{"locations, never conclusions", "Give locations, not conclusions"},
		// Verifier fan-out discipline (PRD #319 M4). Complements the per-unit
		// read-only review wave rather than duplicating it: a verifier must not be
		// spent re-reading a coordinate the lead already confirmed itself.
		{"no re-dispatch to re-read a coordinate the lead already confirmed", "Do not dispatch a verifier to re-read a coordinate you already opened and confirmed yourself"},
	}
	for _, c := range cases {
		if !strings.Contains(body, c.phrase) {
			t.Errorf("lead template lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}
}

// ---------------------------------------------------------------------------
// Region-scoped phrase pins for `lead` (issue #205). What follows splits the
// rendered template in two and asserts each pinned phrase against its own half,
// so a rule MOVED between the plan-turn paragraph and the post-implementation
// bullet reds instead of satisfying its pin from the wrong section.
//
// Three sentences carry more weight than their length suggests, and none of
// them is an assertion: `leadRegionBoundary` and the two landmarks below are
// ordinary prose of `lead.md`, and editing any one of them fatals every
// assertion here. THE WARNING CANNOT LIVE NEXT TO THE PROSE IT PROTECTS:
// `Render` writes `PromptBody` verbatim (render.go) and that body IS the system
// prompt shipped to every user's lead agent, so a "do not reword, a test
// depends on this" note in `lead.md` would be read by the agent as an
// instruction; and frontmatter is no escape either, since `parse` rejects any
// unknown key (builtins.go, `unknown frontmatter key`). Hence it is here, and
// hence the length of what follows.
// ---------------------------------------------------------------------------

// leadRegionBoundary splits the rendered `lead` template into its two halves:
// everything before it states what the lead must do BEFORE `submit_plan`, and
// everything after it is the parallel-dispatch bullet list, whose first bullet
// is the wave that runs AFTER an implementation unit lands.
//
// The boundary is also pinned, from the other direction, by
// TestLeadParallelDispatchPhrases' first case (which quotes it without the
// trailing colon). So rewording it cannot quietly move this split without also
// reddening that set.
const leadRegionBoundary = "Dispatch independent subagents in parallel in a single turn:"

// Landmarks for guard clause 3. Three properties, each of which a fold showed
// was needed:
//
//   - NOT a phrase any case pins, so a guard failure and a behavior failure are
//     never the same message;
//   - asserted BOTH ways — present in its own region, absent from the other.
//     The positive half matters as much as the negative: a bare "the plan region
//     must not contain the bullet landmark" goes vacuous the moment the landmark
//     is reworded, which is the vacuous-negative trap wearing a guard's clothes;
//   - taken from the region's EDGE, not its middle. A first draft used a
//     mid-region landmark for the bullet side and measured blind to a boundary
//     that moved DOWN past the first bullet: the landmark stayed put, the guard
//     passed, and the plan region silently absorbed the first bullet, so a plan
//     phrase relocated into that bullet would still have satisfied its plan
//     case. The plan landmark is therefore the last clause of the plan region
//     and the bullet landmark is inside the first bullet.
const (
	leadPlanLandmark   = "which you do not control"
	leadBulletLandmark = "Do not name a fixed reviewer-then-auditor pair"
)

// splitLeadRegions cuts the flattened template in two and refuses to return an
// untrustworthy split.
//
// EVERY CLAUSE BELOW IS LOAD-BEARING AND CLAUSE 3 IS THE ONE THAT LOOKS
// REMOVABLE. The naive form of this function — a bare `strings.Cut` — is
// STRICTLY WORSE than the whole-body assertions it replaces, because
// `strings.Cut` on a missing separator returns `(whole, "", false)`:
//
//   - the plan region silently becomes the ENTIRE body, so every plan
//     assertion reverts to whole-body semantics, still passing, no longer
//     scoped, and nothing says so;
//   - the bullet region becomes empty, so its one assertion reds.
//
// One loud, correct-looking red about the bullet case, concealing seven quietly
// disarmed ones. That is the gate lying rather than a claim lying, which is
// worse than the blindness it replaces because the blindness was at least
// written down.
//
//	clause 1 — exactly one boundary. Catches an absent boundary and a duplicated
//	           one. Both make the split undefined rather than wrong.
//	clause 2 — neither region degenerate. Catches splits clause 1 admits: a
//	           boundary that survives at the very top or very bottom.
//	clause 3 — no cross-contamination. Catches a boundary that MOVED rather than
//	           vanished, where the count is still 1 and both sizes are still
//	           plausible. Deleting it as belt-and-braces is the suggestion to
//	           expect; what it costs is measured below rather than asserted.
//
// Clause 3 is worth its keep for a narrower reason than "nothing else catches a
// moved boundary", and the difference is worth stating because the wider claim
// is what a reader would assume. Folded both directions:
//
//   - boundary moved UP, above the plan paragraph: without clause 3 the suite
//     still reds — seven `PLAN region lost …` errors — so it is DETECTED either
//     way. What clause 3 buys there is the message. Seven reds say the template
//     lost seven behaviors, which is false; one Fatal says the boundary moved,
//     which is true, and sends the reader to the one thing that changed.
//   - boundary moved DOWN, past the first bullet: this is the one that matters,
//     and the cost is measured rather than predicted. Without clause 3 the only
//     red is the bullet case — correct-looking and misleading, since the
//     ordering sentence is still in the template — while the plan region has
//     silently GROWN to include the first bullet. Folded on that tree: move the
//     boundary down AND relocate the citation clause into the first bullet, and
//     the citation case does NOT red. It matched from inside the bullet. One
//     red pointing at the wrong thing, over a scope that quietly loosened, with
//     a relocation going undetected underneath it — which is the exact class
//     this whole change exists to close. With clause 3 present the same tree
//     stops at `guard 3`, before any assertion is read.
//
// So: clause 3 is not the sole detector of a moved boundary. It is the clause
// that keeps a red honest, and the one that stops a region growing back into
// the whole-body semantics this change replaced when the boundary lands past
// the bullet landmark. Both are worth a Fatal, and the second is the one that
// would otherwise undo the change.
//
// There is a THIRD boundary position and no guard clause covers it: PARTWAY
// down, past the ordering sentence but before the bullet landmark. Measured,
// zero guards fire — the count is 1, both regions are ample, and each landmark
// is still on its own side — and the run reds with a message that is FALSE:
// `BULLET region lost … missing phrase "Read-only work fans out per unit"`,
// while that phrase is still in the template, one line above the boundary.
// Detection holds (rc=1 either way), so this is a MESSAGE gap rather
// than a detection gap — but it is the reason the sentence above says "past the
// bullet landmark" instead of claiming every downward move.
//
// And the thing that closes that window is a BEHAVIOR PIN, not the guard: the
// bullet region carries exactly one case, and its phrase is the first sentence
// after the boundary, so any move further down than that reds it. Retire or
// reword that pin and the partway window widens with nothing structural behind
// it. A reader looking at the bullet case sees a behavior assertion; it is also
// load-bearing for the split.
//
// CONTROLLING CLAUSE 3 NEEDS A DUPLICATION FOLD, NOT A MOVED BOUNDARY. Each
// landmark is checked present-in-its-own-region first and leaked-into-the-other
// second, and the first is a Fatalf — so a moved boundary ALWAYS trips the
// presence branch and the leak branch is unreachable that way. Measured: every
// boundary-move fold reports `no longer contains its landmark`, never a leak.
// Anyone controlling the leak branch by moving the boundary will therefore
// conclude it is dead code and delete it. Duplicate a landmark into the other
// region instead — measured, that reports `leaked into the other region` with
// the boundary count still 1, which is the fold that proves the branch live.
//
// All three are Fatalf rather than Errorf on purpose. Once the split is
// untrustworthy the assertions below report on regions that are not the regions
// they name, and under Errorf a broken
// split prints all eight results anyway — a screenful of authoritative-looking
// output computed from a split already known to be invalid, with the one line
// that explains it scrolled off the top. Stop at the guard; nothing below it
// means anything until the split is fixed.
func splitLeadRegions(t *testing.T, body string) (plan, bullet string) {
	t.Helper()

	if n := strings.Count(body, leadRegionBoundary); n != 1 {
		t.Fatalf("guard 1: region boundary %q occurs %d times, want exactly 1 — "+
			"the split every assertion in this test depends on is undefined",
			leadRegionBoundary, n)
	}
	plan, bullet, _ = strings.Cut(body, leadRegionBoundary)
	const floor = 400
	if len(plan) < floor || len(bullet) < floor {
		t.Fatalf("guard 2: degenerate split, plan=%d bullet=%d bytes (want both >= %d)",
			len(plan), len(bullet), floor)
	}
	for _, c := range []struct {
		name     string
		landmark string
		in       string
		notIn    string
		inName   string
	}{
		{"plan", leadPlanLandmark, plan, bullet, "plan"},
		{"bullet", leadBulletLandmark, bullet, plan, "bullet"},
	} {
		if !strings.Contains(c.in, c.landmark) {
			t.Fatalf("guard 3: the %s region no longer contains its landmark %q — "+
				"either the boundary moved or the landmark was reworded; fix this before "+
				"reading any assertion below", c.inName, c.landmark)
		}
		if strings.Contains(c.notIn, c.landmark) {
			t.Fatalf("guard 3: the %s landmark %q leaked into the other region — the "+
				"boundary moved, and the regions no longer mean what they are named",
				c.name, c.landmark)
		}
	}
	return plan, bullet
}

// TestLeadPlanCritiquePhrases pins the plan-time half of the lead's dispatch
// contract (issue #197): before `submit_plan` the plan must cite the code it
// asserts, and the wave that produces those citations reads the plan text
// rather than a diff and writes nothing.
//
// EACH PHRASE IS ASSERTED AGAINST ITS OWN REGION, NOT THE WHOLE TEMPLATE
// (issue #205). Moving a clause ACROSS THE BOUNDARY — out of the plan paragraph
// and into the post-implementation bullet, or back — takes it out of the region
// its case asserts, so the case reds by construction, whatever context the
// phrase does or does not carry.
//
// "By construction" reaches exactly that far and no further. The plan region is
// everything BEFORE the boundary — the frontmatter and both intro paragraphs,
// not the plan-critique paragraph — so a clause moved UP within the region is
// not moved at all as far as these assertions are concerned. Measured: the plan
// region is 1655 bytes against the paragraph's 889, leaving 766 bytes of room
// above it, and hoisting the relay clause into the intro reworded as "as a
// general habit" gives rc=0, no guard and no red, with the clause out of the
// section that gives it meaning. Closing that needs a second cut, and the
// paragraph's opening sentence is itself a pinned phrase, so a naive
// `strings.Cut` on it would remove it from the region and red its own case. Not
// attempted here.
//
// It replaces a per-phrase anchoring scheme, and the history is the argument
// for the replacement. Anchoring required every phrase to quote a token naming
// its own turn; that is a manual property with nothing checking it was applied,
// and it was missed in three consecutive rounds — each time on phrases anchored
// in the round before, once on the very pin guarding the previous round's
// blocking fix. Three rounds of careful people missing it is the signature of a
// discipline, not of a defect. The region carries the position now, so the
// phrase only has to carry the meaning: the phrases below total 376 BYTES
// against the 666 of the anchored ones they replace, none contains another, and
// each occurs exactly once. (Raw figures rather than a percentage, and bytes
// rather than "characters", for one reason: this shrink is −44% or −43%
// depending on whether it is taken in bytes or runes, because the anchored set
// carried two em dashes and so counts 666 bytes against 662 runes. Each
// percentage is that unit's own division rounded once — no single number rounds
// to both. The ambiguous unit diverges on exactly one of the two figures, since
// this set carries no em dash and is 376 either way, and two figures in two
// files read as a disagreement to whoever meets them apart.)
//
// It cannot LOSE detection relative to the whole-body form it replaces, and
// that is a proof rather than a measurement: each region is a substring of the
// body, so `Contains(region, p)` implies `Contains(body, p)`, and every fold
// the old form reddened on this set reddens here too. Worth stating because it
// bounds the question "does scoping open a new class?" — the split is the
// ENTIRE new exposure, not one item among several. What that exposure is, is
// the third item below.
//
// FOUR THINGS THIS DOES NOT DO. The first is a deliberate semantic change; the
// second is a hard limit of every substring instrument, not a choice; the third
// and the first are both what the split costs; the fourth is an open gap in this
// same file that this change makes HARDER to see:
//
//  1. IT IS A STRICTER CONTRACT THAN BEFORE, and that is a real semantic change
//     rather than a refactor. A relocated-but-still-present rule is stated
//     SOMEWHERE in the template; this set treats being in the wrong section as a
//     failure. Since #197 is precisely about which turn the wave happens in,
//     placement is part of the contract here — but a future reader should meet
//     that as a decision, not discover it from a red.
//  2. IT DOES NOT DETECT AN INSERTION, and no substring-presence instrument
//     ever will. `strings.Contains` is monotone under insertion: if a phrase is
//     in a region it is in every superstring of that region. Measured under this
//     region-scoped form and under the whole-body form it replaces, with the
//     same result — a paragraph inserted above the plan paragraph calling the
//     whole thing "an optional pre-flight … skip it entirely and call
//     `submit_plan` straight away" neutralises every behavior here and the suite
//     passes, rc=0. Regions closed relocation; they did not close the problem.
//     Do not patch it with a negative assertion on the inserted wording — that
//     goes vacuous the moment the copy changes and then guards nothing forever.
//     Real coverage needs a semantic check on the rendered prompt, which is a
//     different instrument and a separate piece of work. Until then, insertion
//     is caught by reading the diff and by nothing in this file.
//  3. IT ADDS THREE PROSE DEPENDENCIES THAT NO BEHAVIOR PIN PROTECTS, and this
//     is the whole of what scoping costs. `leadRegionBoundary` and the two
//     landmarks are ordinary sentences of the template, and a benign edit to any
//     one of them fatals all eight assertions at the guard. The sharpest is
//     punctuation: change the boundary's trailing colon to a period and
//     `guard 1` reports `occurs 0 times`, while TestLeadParallelDispatchPhrases'
//     pin on that same sentence stays GREEN, because it quotes the sentence
//     without the colon. One character, identical behavior, whole instrument
//     down. All three fail CLOSED and name themselves, which is what makes this
//     a cost rather than a hazard — but the whole-body form did not have it, and
//     it is invisible from reading the case table, because the three strings sit
//     in constants that read as configuration rather than as assertions.
//  4. IT DOES NOT SCOPE TestLeadParallelDispatchPhrases, WHICH STILL ASSERTS
//     AGAINST THE WHOLE BODY. Same file, same template, fourteen pins, and the
//     class stays open for them: measured, moving a parallel-implementer
//     constraint out of its bullet and up into the plan paragraph — across the
//     very boundary this test introduces — leaves both sets green at rc=0.
//     A PIN ADDED TO THAT SET FROM NOW ON WILL BE WHOLE-BODY, and this file's
//     own `splitLeadRegions` is what will make its author think otherwise: the
//     class #205 exists to close, re-entering through the set this change did
//     not touch. The exposure is narrow today — those phrases nearly all live
//     in the bullets and have nowhere misleading to go, which is why
//     documenting was judged enough — but that is a property of the phrases
//     currently in the set, not of the set. Scoping them is its own piece of
//     work if it is worth doing at all.
func TestLeadPlanCritiquePhrases(t *testing.T) {
	lead, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("lead builtin missing")
	}
	body := flatten(string(Render(lead)))
	plan, bullet := splitLeadRegions(t, body)

	type pin struct {
		behavior string
		phrase   string
	}
	// Everything the lead owes BEFORE it calls `submit_plan`.
	planCases := []pin{
		{"the citation property is due before submit_plan", "Before you call `submit_plan`"},
		{"every asserted mechanism is cited by file and line", "name the file that implements it and quote the line"},
		{"the wave reviews the plan text, not a diff", "the artifact under review is the plan text, not a diff"},
		{"the no-write rule is RELAYED to each dispatched validator", "tell each validator you send over the plan that it must not change anything"},
		{"the no-write rule exists to protect the approval gate", "a change nobody saw when approving it"},
		{"re-planning re-cites only what changed", "re-cite only the mechanisms that changed"},
		{"the bar is a property of the plan, never of the issue text", "never as a judgement about the issue text"},
		{"exact literal tokens from the scope are carried verbatim into the plan", "carry those literal tokens verbatim into the plan"},
	}
	// The dispatch of the read-only wave, which now happens PER UNIT rather than
	// as an end-of-run barrier (PRD #215 M2). Kept as its own region so that a
	// plan-turn clause moved down here, or this one moved up, reds rather than
	// satisfying the other region's case.
	//
	// This case is a POSITIVE pin on the single surviving dispatch procedure
	// (PRD #215 M3): the read-only lane has exactly one procedure and this bullet
	// states it. It does NOT catch a paragraph inserted alongside it that
	// contradicts it — `strings.Contains` is monotone under insertion, the same
	// gap documented at length above for the plan cases — so behavior under the
	// reworded prompt is measured by a live run (PRD #215 M7), not asserted here.
	//
	// THIS ONE CASE IS ALSO LOAD-BEARING FOR THE SPLIT, which is not visible
	// from here and is why it is repeated here rather than only at the guard.
	// Its phrase is the first sentence after the boundary, so it is what reds
	// when the boundary moves PARTWAY down into this bullet — a position no
	// guard clause covers (see splitLeadRegions). Retire it, reword it, or add
	// a second bullet case ahead of it, and that window widens with nothing
	// structural behind it.
	bulletCases := []pin{
		{"read-only lane has exactly one dispatch procedure, stated in the bullet", "Read-only work fans out per unit"},
	}

	for _, c := range planCases {
		if !strings.Contains(plan, c.phrase) {
			t.Errorf("PLAN region lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}
	for _, c := range bulletCases {
		if !strings.Contains(bullet, c.phrase) {
			t.Errorf("BULLET region lost behavior %q: missing phrase %q", c.behavior, c.phrase)
		}
	}

	// Audits, not behavior pins: these assert properties OF THE CASE TABLE, so
	// they red on a badly-written pin rather than on a changed template.
	all := append(append([]pin{}, planCases...), bulletCases...)
	for _, c := range all {
		if n := strings.Count(body, c.phrase); n > 1 {
			t.Errorf("pin %q matches %d occurrences of the template; a per-occurrence "+
				"match cannot say which one satisfied it", c.behavior, n)
		}
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && strings.Contains(a.phrase, b.phrase) {
				t.Errorf("pin %q contains pin %q, so the contained one can never fail alone",
					a.behavior, b.behavior)
			}
		}
	}
	// TestLeadParallelDispatchPhrases is a separate set and must stay
	// independently falsifiable: an earlier version of the bullet case here
	// swallowed one of its phrases whole, silently retiring it.
	for _, c := range all {
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
		Version:     4,
		Description: "an example.",
		Model:       "sonnet",
		Tools:       []string{"Bash", "Read", "Grep"},
		PromptBody:  "Body.\n",
	})
	want := "---\nname: example\nversion: 4\ndescription: an example.\ntools: Bash, Read, Grep\nmodel: sonnet\n---\n\nBody.\n"
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

// TestParseRejectsBadVersion pins that parse accepts only a positive-integer
// version and rejects malformed or non-positive values. parse is
// package-internal, so this test lives in package agenttmpl and feeds it raw
// frontmatter+body bytes directly.
func TestParseRejectsBadVersion(t *testing.T) {
	mk := func(version string) []byte {
		return []byte("---\nname: example\nversion: " + version +
			"\ndescription: an example.\n---\n\nBody.\n")
	}

	for _, bad := range []string{"0", "-1", "abc"} {
		if _, err := parse(mk(bad)); err == nil {
			t.Errorf("parse(version: %s) returned nil error, want a rejection", bad)
		}
	}

	def, err := parse(mk("3"))
	if err != nil {
		t.Fatalf("parse(version: 3) unexpected error: %v", err)
	}
	if def.Version != 3 {
		t.Errorf("parse(version: 3) gave Version = %d, want 3", def.Version)
	}
}
