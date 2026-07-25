package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// The printed-instruction backstop (PRD #98, seam 5; reworked by the M8 Part C design).
//
// THE PROBLEM IT EXISTS FOR. Three instructions were printed by this package telling a user
// what command to run next. None had ever been executed by a test. TWO WERE FALSE:
//
//   - "re-run with --json for the N (run, rec) pair(s)" — the re-run returns settled=0,
//     because the CLI sends no scope and the server's default `open` finds nothing left open
//     after the first call. It also recommended a WRITE as a way to read.
//   - "re-check with `uzi review backlog --bucket all`" — no bucket value can reach what the
//     row cap cut, because `truncated` is computed and the rows sliced before the bucket
//     filter runs. Every bucket truncates identically.
//
// Both PARSED perfectly and both were plausible on the page. Reading them proved nothing;
// only running them did. And both were introduced by commits that were reviewed.
//
// THERE ARE TWO KINDS OF STRING HERE, AND ONE BAR IS WRONG FOR ONE OF THEM. The first
// version of this file applied a single bar — "an entry is a claim that the instruction has
// been EXECUTED" — to every `uzi …` span it could find. Measured against this package, that
// conflated two different things:
//
//   - RUNTIME: emitted at a decision point through Printf/Println/Exitf/…. It tells a user
//     what to do NEXT, so the only thing that validates it is running it and asserting the
//     outcome. `uzi review undo %s %s`, `uzi review show %s`, `uzi repo list`, `uzi login`,
//     `uzi review backlog --run <run-id>`.
//   - HELP: a Short/Long description or a flag's usage text, cross-linking a sibling command.
//     Nobody needs to EXECUTE `uzi skill install` to validate a Short line that mentions it.
//     What such a string can be wrong about is naming a command path that does not exist —
//     which is statically checkable, complete, and stronger than the old bar, since nothing
//     used to verify that `uzi worker set-token` was a real path at all (only that `worker`
//     was a real top-level verb).
//
// Four of the eight original entries' notes misdescribed their own site as a result — two
// help strings were described as printed output, `uzi login`'s hint was attributed to an
// "auth-required path" when it is the device-auth polling loop, and `uzi review backlog`'s
// note described a truncation warning that the extractor could not see at all.
//
// THE KIND IS DERIVED FROM THE AST POSITION AND IS NEVER DECLARED. That is the load-bearing
// property: an author cannot label a runtime instruction "help" to dodge the execution bar,
// because there is no field to label it with. classifyKind walks OUT from the literal and
// takes the first enclosing position it recognises. A position it does not recognise is
// kindUnknown, and kindUnknown FAILS — it must never default to help, or a new emitter
// wrapper would silently buy every future instruction an exemption.
//
// THE BASELINE IS ZERO UNKNOWNS, and that number is what makes a future one meaningful.
// MEASURED when this landed: all 9 lifted candidates resolved to a definite kind. So a
// kindUnknown you hit later is a genuinely NEW emitter, not a gap in the classifier's
// original coverage — and widening `emitters` is therefore a DECISION to record with its
// reason, not a nuisance to clear. It is the one edit that can quietly re-open the hole
// this file exists to close: every string printed through the newly-added function becomes
// classifiable, and if the wrapper is not really an emitter they all become HELP, exempt
// from the execution bar, silently.
//
// WHY A REGISTRY RATHER THAN THREE TESTS. Fixing three strings leaves the seam open: the next
// commit that prints a hint reopens it, and nothing says so. These tests fail the build when
// this package prints a `uzi …` command that has no entry below — so a FOURTH instruction
// cannot land silently. They need no server, which is why they run in `go test ./...` and
// make the omission loud at the moment of writing rather than at review time.
//
// WHAT AN ENTRY MEANS. For a RUNTIME entry, adding a line is a claim that the instruction has
// been EXECUTED and its outcome asserted — not that it looks right — and `where` must name a
// live address (an e2e label or a Go test function) that this file re-checks. An honest
// evidenceNotExecuted entry is LEGAL, GREEN and PERMANENT; the required `reason` is what
// stops it becoming a shrug. For a HELP entry, the claim is only that the path it names
// resolves, and that is checked here in full.

// instructionKind is where a printed span SITS, derived from the AST. Never declared by an
// author; see classifyKind.
type instructionKind int

const (
	kindUnknown instructionKind = iota // MUST fail — an unrecognised emitter is not an exemption
	kindHelp                           // a Short/Long/Example/Use field, or a flag's usage text
	kindRuntime                        // an argument to a print/exit emitter
)

func (k instructionKind) String() string {
	switch k {
	case kindHelp:
		return "HELP"
	case kindRuntime:
		return "RUNTIME"
	default:
		return "UNKNOWN"
	}
}

// evidenceKind is what backs a RUNTIME entry's claim. evidenceUnset is the zero value on
// purpose: a newly-added entry that forgets to choose FAILS rather than inheriting the
// weakest option.
type evidenceKind int

const (
	evidenceUnset       evidenceKind = iota // invalid — a new entry must choose
	evidenceHelpOnly                        // the entry is a HELP reference; the execution bar does not apply
	evidenceNotExecuted                     // RUNTIME, honestly declared unexecuted. LEGAL and GREEN; needs a reason
	evidenceGoTest                          // executed from a Go test; `where` is the test function NAME
	evidenceE2E                             // executed in e2e/run-e2e.sh; `where` is the row's label string
)

// knownInstruction is one printed command the package may emit. `command` is matched against
// a lifted candidate on a WORD boundary (see matchesCommand) — flags, format verbs and
// placeholders after it do not need restating, but `uzi review backlogger` is NOT absorbed by
// `uzi review backlog`.
//
// Each candidate is attributed to the LONGEST matching entry. That is what lets the same verb
// carry both kinds: `uzi review backlog` is a flag-usage reference (HELP) while
// `uzi review backlog --run <run-id>` is a Println at a truncation decision point (RUNTIME),
// and without most-specific attribution the runtime candidate would also land on the help
// entry and make its derived kind ambiguous.
type knownInstruction struct {
	command  string
	evidence evidenceKind
	where    string // an e2e row label, or a Go test function name
	reason   string // REQUIRED when evidence == evidenceNotExecuted
	note     string
}

var knownInstructions = []knownInstruction{
	// ---- HELP: references inside help text. The bar is that the path RESOLVES. ----------
	{
		command:  "uzi worker set-token",
		evidence: evidenceHelpOnly,
		// This entry arrived from PRD #104 on the landing merge (2026-07-21), because the
		// backstop did what it was built to do: it failed the build on the FOURTH
		// instruction, the one nobody had written yet, at the moment it entered the tree.
		//
		// Its first note claimed it was "NOT EXECUTED as a literal instruction" and pointed
		// at worker_test.go for the argv shape. Both halves were answering the wrong
		// question: this is a cross-reference in `uzi token`'s Long help, not something the
		// CLI ever tells a user to run at a decision point. What it CAN be wrong about is
		// naming a path that does not exist, and that is now checked in full.
		note: "HELP: `uzi token`'s Long help cross-links the write verb that is web-only from " +
			"the CLI's side (token.go). Nothing executes it, and nothing should — the path " +
			"resolution check is the complete bar for a help reference.",
	},
	{
		command:  "uzi review backlog",
		evidence: evidenceHelpOnly,
		// CORRECTED. The old note said "Printed by both truncation warnings". It is not:
		// the span the extractor lifts here comes from the --category/--target flag USAGE
		// text (addCoordFlags, review.go). The truncation remedy is a different, longer
		// string that lifts as its own candidate — and before the character class was
		// widened to %<> the extractor could not see it at all, so the note described a site
		// that was invisible to the very test it was written for.
		note: "HELP: the --category/--target flag usage names the read that prints the values " +
			"to pass (addCoordFlags, review.go). See the separate `uzi review backlog --run " +
			"<run-id>` entry for the RUNTIME truncation remedy. TestBacklogBucketUsageMatchesServerEnum " +
			"and TestReviewBacklogRunAnchorForwarded cover the flags themselves.",
	},
	{
		command:  "uzi run inputs",
		evidence: evidenceHelpOnly,
		// CORRECTED. The old note said "Printed by run.go after submitting a steer". It is
		// not printed at all — it is inside `uzi run inputs`'s own Long description, where
		// it names itself while explaining what a chat run's queue contains.
		note: "HELP: inside `uzi run inputs`'s own Long description (run.go), naming itself " +
			"while explaining what a chat run's steer queue holds. Never emitted at runtime.",
	},
	{
		command:  "uzi skill install",
		evidence: evidenceHelpOnly,
		// CORRECTED. The old note said "Printed by the skill auto-upgrade warning". It is
		// the Short description of `uzi skill install-hook`, saying what the hook it
		// installs will run.
		note: "HELP: the Short description of `uzi skill install-hook` (skill.go), naming the " +
			"command the installed SessionStart hook runs. Never emitted at runtime.",
	},

	// ---- RUNTIME: emitted at a decision point. The bar is EXECUTION. --------------------
	{
		command:  "uzi review undo",
		evidence: evidenceE2E,
		where:    "printed-instruction row: uzi review undo",
		note: "RUNTIME: runGroupDisposition prints one undo address per settled member " +
			"(review.go). EXECUTED in e2e: a coordinate seeded on TWO reviews is group-dismissed, " +
			"BOTH printed addresses are lifted from that command's own stdout and run verbatim, " +
			"and the outcome asserted — zero disposition rows left on the coordinate and the " +
			"global triage `todo` back to its pre-dismiss value. The count is asserted before any " +
			"address is used, so a single-address regression cannot pass as a green.",
	},
	{
		command:  "uzi review show",
		evidence: evidenceE2E,
		where:    "printed-instruction row: uzi review show",
		note: "RUNTIME: resolveRecID's refresh hint on a no-match (review.go), through Exitf — " +
			"so it lands on STDERR with a non-zero exit, and the e2e row captures both. " +
			"EXECUTED in e2e: the hint is lifted from that stderr and run verbatim; the outcome " +
			"asserted is that the refreshed read names the coordinate seeded on that run's review, " +
			"not merely that it exited 0. TestReviewResolveUnknownID still pins that the hint appears.",
	},
	{
		command:  "uzi repo list",
		evidence: evidenceE2E,
		where:    "printed-instruction row: uzi repo list",
		// CORRECTED. The old note said "Printed by run.go when a repo id is needed", which was
		// vague enough to be unfalsifiable; it is the --repo-missing usage error on `run create`.
		note: "RUNTIME: `uzi run create`'s --repo-missing usage error names where to get a repo id " +
			"(run.go), through Exitf — STDERR, exit 2. EXECUTED in e2e: the instruction is lifted " +
			"from that stderr and run verbatim, and the outcome asserted is that its output names " +
			"the harness's enabled repo id, not that it exited 0.",
	},
	{
		command:  "uzi review backlog --run <run-id>",
		evidence: evidenceNotExecuted,
		reason: "Reachable, but only from a TRUNCATED backlog: the remedy is printed on the " +
			"post-write re-read's `truncated` flag, and JudgeBacklogMaxRows is a compile-time " +
			"const, so the only arrangement that reaches it is a 2001-row seed. That seed is " +
			"PRD #98 M8b's (Part B/B4) and is not designed yet. Executing this against an " +
			"untruncated backlog would assert nothing about the remedy — it would be `backlog " +
			"--run` working, which TestReviewBacklogRunAnchorForwarded already pins.",
		note: "RUNTIME: the post-write truncation remedy (runGroupDisposition, review.go). " +
			"TestReviewTruncationWarningsNameAWorkingRemedy pins that it names --run and NOT " +
			"--bucket, whose uselessness here was measured by lowering the cap to 2.",
	},
	{
		command:  "uzi login",
		evidence: evidenceNotExecuted,
		// CORRECTED, and this is the entry the old note was most wrong about. "Printed by the
		// auth-required path" describes a different path with different reachability; the
		// string is emitted from inside the device-authorization POLLING LOOP.
		reason: "PERMANENTLY unreachable from a harness, and this is a real reason rather than an " +
			"inherited gap. `uzi login` declares no flags; it is a device-authorization flow, and " +
			"the hint fires from inside the polling loop on a terminal or timed-out approval. " +
			"Executing it verbatim means driving a browser approval, which no e2e row can arrange. " +
			"An honest permanent declaration is the correct landing state here, not a TODO.",
		note: "RUNTIME: the device-auth polling loop's retry hint, on a terminal poll status and " +
			"on expiry (login.go), both through Exitf.",
	},
}

// instructionRE lifts a candidate from a printed string: either backtick-quoted, or a line
// that is itself runnable (the form the undo listing uses). Both are the shapes an
// instruction is allowed to take — anything else is unliftable and would defeat a test that
// must execute the printed text rather than a hand-written copy.
//
// A candidate is only an INSTRUCTION if its second word is a real command. That check runs
// against the live cobra tree rather than a word list, which is what keeps prose out: the
// root command's own Long description begins "uzi drives the factory from the terminal", and
// a naive line-anchored regex reports it as an instruction to run `uzi drives`. Deriving the
// verb set from the tree also means a renamed command cannot leave this extractor stale.
//
// THE CHARACTER CLASS CARRIES `%`, `<` AND `>`, AND DELIBERATELY NOT `|` OR `/`. Without the
// first three the extractor was blind to the shape a real instruction takes: measured against
// this package, `uzi review show %s` and `uzi review backlog --run <run-id>` lifted NOTHING,
// and `  uzi review undo %s %s` truncated at the first `%`. `uzi review show` was registered
// only because a human happened to type it — had they not, the registration check would have
// stayed green. Adding `%<>` lifts those three and, measured package-wide, ZERO false
// positives.
//
// Adding `|` and `/` on top buys exactly one more span, `uzi review resolve|dismiss
// --category/--target` (review.go), and that span is an ALTERNATION that is not runnable as
// printed. Registering it would assert that someone executed a string nobody can. Excluding
// it needs no special case: the alternation character is itself the marker that a span is a
// reference rather than a command.
//
// The false-positive load is carried by the cobra-verb filter, not by the class — the root's
// own "uzi drives the factory" is rejected under both the narrow and the widened class,
// because `drives` is not a verb in the tree.
var instructionRE = regexp.MustCompile("`(uzi [a-z][a-z0-9 %<>-]*)`|(?m)^\\s*(uzi [a-z][a-z0-9 %<>-]*)")

// emitters are the calls whose string arguments reach a user at runtime. A call NOT in this
// set is not a classification — classifyKind keeps walking outward, so a literal wrapped in a
// Sprintf inside a Println still classifies RUNTIME. A genuinely unrecognised position stays
// kindUnknown and FAILS; see C11 of the design note.
var emitters = map[string]bool{
	"Print": true, "Printf": true, "Println": true,
	"Fprint": true, "Fprintf": true, "Fprintln": true,
	"Exitf": true, "Errorf": true,
}

// helpFields are the cobra.Command fields whose contents are documentation.
var helpFields = map[string]bool{"Short": true, "Long": true, "Example": true, "Use": true}

// flagSets are the pflag accessors a usage string is registered through
// (`cmd.Flags().String(name, def, usage)`).
var flagSets = map[string]bool{"Flags": true, "PersistentFlags": true, "LocalFlags": true}

// calleeName is the bare function name of a call, whether qualified (uzicli.Exitf, p.Println)
// or not.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// isFlagRegistration reports whether a call is `<x>.Flags().Something(...)`.
func isFlagRegistration(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	innerSel, ok := inner.Fun.(*ast.SelectorExpr)
	return ok && flagSets[innerSel.Sel.Name]
}

// classifyKind derives the kind of a string literal from its enclosing syntax, walking OUT
// from the literal and taking the first position it recognises. Innermost wins, which is what
// makes an Exitf inside a cobra RunE classify RUNTIME rather than being swallowed by the
// surrounding command literal.
//
// stack is the ancestor chain with the literal itself last.
func classifyKind(stack []ast.Node) instructionKind {
	for i := len(stack) - 2; i >= 0; i-- {
		switch n := stack[i].(type) {
		case *ast.CallExpr:
			if emitters[calleeName(n)] {
				return kindRuntime
			}
			if isFlagRegistration(n) {
				return kindHelp
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok && helpFields[id.Name] {
				return kindHelp
			}
		}
	}
	return kindUnknown
}

// topLevelCommands reads the real command names off the built tree.
func topLevelCommands(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, c := range newRootCmd(fakeEnv(&uzicli.FakeClient{})).Commands() {
		out[c.Name()] = true
	}
	if len(out) == 0 {
		t.Fatal("the command tree is empty; the extractor below would classify everything as prose")
	}
	return out
}

// liftInstructions returns the printed commands in a string, skipping prose.
func liftInstructions(val string, commands map[string]bool) []string {
	var out []string
	for _, m := range instructionRE.FindAllStringSubmatch(val, -1) {
		cmd := strings.TrimSpace(m[1] + m[2])
		fields := strings.Fields(cmd)
		if len(fields) < 2 || !commands[fields[1]] {
			continue // prose that merely starts with the binary name
		}
		out = append(out, cmd)
	}
	return out
}

// candidate is one lifted instruction text, with every site that produces it.
type candidate struct {
	kinds map[instructionKind][]string // kind -> "file:line" positions
}

func (c *candidate) positions() []string {
	var all []string
	for k, ps := range c.kinds {
		for _, p := range ps {
			all = append(all, k.String()+" @ "+p)
		}
	}
	sort.Strings(all)
	return all
}

// inspectWithStack is ast.Inspect with the ancestor chain kept, so a literal can be
// classified by where it SITS rather than by what it says.
func inspectWithStack(root ast.Node, fn func(n ast.Node, stack []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		fn(n, stack)
		return true
	})
}

// liftAll computes THE lift set — once — for both directions of the check.
//
// Computing it once is the point, not a tidy-up. The two directions used to disagree: the
// staleness check was a strings.Contains over raw literals, which returns true for
// `uzi review show` while the registration scan (running the real regex) saw nothing at all.
// Same concept, two definitions, and only the blind one gated the build. Contains would also
// keep an entry alive on the strength of the command name appearing in a COMMENT, since it
// never asked whether anything prints it.
func liftAll(t *testing.T) map[string]*candidate {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/uzi: %v", err)
	}
	commands := topLevelCommands(t)
	found := map[string]*candidate{}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			// Test files may legitimately quote a command while asserting on it.
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			inspectWithStack(file, func(n ast.Node, stack []ast.Node) {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					return
				}
				lifted := liftInstructions(val, commands)
				if len(lifted) == 0 {
					return
				}
				kind := classifyKind(stack)
				pos := fset.Position(lit.Pos()).String()
				for _, cmd := range lifted {
					c := found[cmd]
					if c == nil {
						c = &candidate{kinds: map[instructionKind][]string{}}
						found[cmd] = c
					}
					c.kinds[kind] = append(c.kinds[kind], pos)
				}
			})
		}
	}
	if len(found) == 0 {
		// Without this the tests pass by parsing nothing — the same vacuity as a glob that
		// matches no files. This package definitely prints instructions.
		t.Fatal("no printed `uzi …` instructions found at all; the extractor is broken, not the code")
	}
	return found
}

// matchesCommand is the entry↔candidate match, on a WORD boundary.
//
// A bare strings.HasPrefix absorbed `uzi review backlog-export --all`, `uzi review backlogger`
// and `uzi review showdown` into existing entries — a new instruction could land silently by
// happening to share a prefix with a registered one, which is the exact failure the registry
// exists to prevent. Requiring the next character to be a space closes all three while every
// legitimate match survives, including the longer forms the widened class now lifts
// (`uzi review show %s` still matches the `uzi review show` entry).
func matchesCommand(cmd, entry string) bool {
	return cmd == entry || strings.HasPrefix(cmd, entry+" ")
}

// attribute returns the index of the LONGEST entry matching cmd, or -1.
func attribute(cmd string) int {
	best := -1
	for i, k := range knownInstructions {
		if !matchesCommand(cmd, k.command) {
			continue
		}
		if best < 0 || len(k.command) > len(knownInstructions[best].command) {
			best = i
		}
	}
	return best
}

func TestPrintedInstructionsAreRegistered(t *testing.T) {
	found := liftAll(t)

	var unregistered []string
	for cmd, c := range found {
		if attribute(cmd) < 0 {
			unregistered = append(unregistered, cmd+"  ("+strings.Join(c.positions(), ", ")+")")
		}
	}
	sort.Strings(unregistered)
	for _, u := range unregistered {
		t.Errorf("printed instruction %q has no entry in knownInstructions.\n"+
			"    Add one. If the site is a print/exit emitter, the entry is a RUNTIME claim that you\n"+
			"    EXECUTED the command and asserted its outcome — not that it reads correctly. Two of\n"+
			"    the three instructions this package shipped parsed perfectly and were false. If the\n"+
			"    site is help text, the entry is evidenceHelpOnly and the bar is that its path resolves.\n"+
			"    You do not choose which: the kind is derived from where the string sits.", u)
	}
}

// The registry must not rot in the other direction either: an entry whose command nothing
// prints any more is a claim about code that no longer exists, and it would quietly absorb a
// future instruction that happens to share its prefix.
//
// This runs over the SAME lift set as the registration direction — see liftAll for why that
// symmetry is the fix and not a refactor.
func TestRegisteredInstructionsAreStillPrinted(t *testing.T) {
	found := liftAll(t)

	matched := map[int]bool{}
	for cmd := range found {
		if i := attribute(cmd); i >= 0 {
			matched[i] = true
		}
	}
	for i, k := range knownInstructions {
		if !matched[i] {
			t.Errorf("knownInstructions lists %q, which no lifted candidate matches any more — remove "+
				"the entry rather than leaving a claim about code that is gone. (Matching is on a word "+
				"boundary and most-specific-wins, so a longer entry shadowing this one would also show "+
				"up here.)", k.command)
		}
	}
}

// TestInstructionEvidenceIsWellFormed checks the registry against the tree in the four ways
// that are statically decidable, plus the derived-kind consistency that makes the whole
// mechanism undodgeable.
//
// WHAT THIS PROVES AND WHAT IT DOES NOT. It proves an evidence claim's ADDRESS is live — that
// the named e2e row label is still in the harness, that the named Go test still exists. It
// does NOT prove the named row asserts anything. That is the same honest boundary the query
// inventory test carries, and it is stated here rather than left for a reader to discover.
func TestInstructionEvidenceIsWellFormed(t *testing.T) {
	found := liftAll(t)

	// Derived kind per entry, from the candidates attributed to it.
	kinds := map[int]map[instructionKind][]string{}
	for cmd, c := range found {
		i := attribute(cmd)
		if i < 0 {
			continue // reported by TestPrintedInstructionsAreRegistered
		}
		if kinds[i] == nil {
			kinds[i] = map[instructionKind][]string{}
		}
		for k, ps := range c.kinds {
			kinds[i][k] = append(kinds[i][k], ps...)
		}
	}

	// POSITIVE CONTROL for the e2e address check. A moved or renamed harness must redden this
	// test, not silently satisfy it by making every `where` unfindable in an empty string.
	// Deliberately NOT asserted: "at least one entry is evidenceE2E" — that would have failed
	// before the first row landed, which is how a useful check gets deleted.
	e2eScript, err := os.ReadFile("../../../e2e/run-e2e.sh")
	if err != nil {
		t.Fatalf("cannot read e2e/run-e2e.sh (%v) — the evidence addresses below would all be "+
			"unverifiable, and an unverifiable check must fail rather than pass quietly", err)
	}
	if len(e2eScript) == 0 {
		t.Fatal("e2e/run-e2e.sh is empty; every evidenceE2E address would be trivially unfindable")
	}
	e2e := string(e2eScript)

	goTestFuncs := goFuncNames(t, "../..")
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))

	for i, k := range knownInstructions {
		byKind := kinds[i]
		if len(byKind) == 0 {
			continue // reported by TestRegisteredInstructionsAreStillPrinted
		}
		if _, bad := byKind[kindUnknown]; bad {
			// Never default to help: a new emitter wrapper would otherwise buy every future
			// instruction printed through it a silent exemption from the execution bar.
			t.Errorf("%q is printed from a position this file cannot classify (%v). Teach classifyKind "+
				"the new emitter or help field — do NOT let it fall through to help, which is the one "+
				"way a runtime instruction can dodge the execution bar.", k.command, byKind[kindUnknown])
			continue
		}
		if len(byKind) > 1 {
			t.Errorf("%q is printed from BOTH help and runtime positions (%v). Split it into two "+
				"entries — the two kinds carry different bars and one entry cannot honestly hold both.",
				k.command, byKind)
			continue
		}
		var derived instructionKind
		for kk := range byKind {
			derived = kk
		}

		switch k.evidence {
		case evidenceUnset:
			t.Errorf("%q declares no evidence kind. The zero value is invalid on purpose: a new entry "+
				"must choose, rather than inheriting the weakest option by forgetting.", k.command)
		case evidenceHelpOnly:
			if derived != kindHelp {
				t.Errorf("%q is declared evidenceHelpOnly but it is printed from a RUNTIME position "+
					"(%v). The kind is DERIVED, not declared — a runtime instruction cannot be relabelled "+
					"out of the execution bar.", k.command, byKind)
			}
		default:
			if derived != kindRuntime {
				t.Errorf("%q carries a runtime evidence claim but it is HELP text (%v). Help references "+
					"are not executed; use evidenceHelpOnly and let the path check be the bar.",
					k.command, byKind)
			}
		}

		switch k.evidence {
		case evidenceHelpOnly:
			// The complete bar for a help reference: the path it names must RESOLVE in the live
			// cobra tree. Nothing used to check this — only that the SECOND word was a real
			// top-level verb, so `uzi worker set-token` was never verified past `worker`.
			assertCommandPathResolves(t, root, k.command)
		case evidenceNotExecuted:
			if strings.TrimSpace(k.reason) == "" {
				t.Errorf("%q is declared NOT EXECUTED with no reason. An honest gap is legal, green and "+
					"permanent here — the required reason is the only thing standing between that and a "+
					"shrug.", k.command)
			}
		case evidenceE2E:
			if strings.TrimSpace(k.where) == "" {
				t.Errorf("%q claims e2e evidence with no `where` label", k.command)
			} else if !strings.Contains(e2e, k.where) {
				t.Errorf("%q claims e2e evidence at %q, which no longer appears in e2e/run-e2e.sh. The "+
					"row was renamed or removed; the claim outlived its address.", k.command, k.where)
			}
		case evidenceGoTest:
			if strings.TrimSpace(k.where) == "" {
				t.Errorf("%q claims Go-test evidence with no `where` function name", k.command)
			} else if !goTestFuncs[k.where] {
				t.Errorf("%q claims Go-test evidence in %s, which does not exist under api/. The claim "+
					"outlived its address.", k.command, k.where)
			}
		}

		if strings.TrimSpace(k.note) == "" {
			t.Errorf("%q has an empty note; the note is what a reader calibrates the entry by", k.command)
		}
	}
}

// assertCommandPathResolves checks that every word of a help reference past `uzi` names a real
// command in the tree, with nothing left over.
func assertCommandPathResolves(t *testing.T, root *cobra.Command, ref string) {
	t.Helper()
	fields := strings.Fields(ref)
	if len(fields) < 2 || fields[0] != "uzi" {
		t.Errorf("help reference %q is not a `uzi <path>` command path", ref)
		return
	}
	path := fields[1:]
	cmd, rest, err := root.Find(path)
	if err != nil {
		t.Errorf("help reference %q does not resolve in the cobra tree: %v", ref, err)
		return
	}
	if len(rest) != 0 {
		t.Errorf("help reference %q does not resolve: %q is not a command under %q (the tree stopped "+
			"at %q). A help string naming a path that does not exist is the one thing this kind of "+
			"entry CAN be wrong about.", ref, rest, cmd.CommandPath(), cmd.CommandPath())
	}
}

// goFuncNames collects every `func Name(` declared under dir, so an evidenceGoTest address can
// be checked without a build.
func goFuncNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // an unparsable file is the compiler's problem, not this test's
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				out[fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for Go test names: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("found no Go functions under %s; every evidenceGoTest address would be unverifiable", dir)
	}
	return out
}
