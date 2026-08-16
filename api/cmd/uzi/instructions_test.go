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

	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
//   - RUNTIME: emitted at a decision point through Printf/Println/Exitf/…, or bound to a name
//     on its way there. It tells a user what to do NEXT, so the only thing that validates it
//     is running it and asserting the outcome. `uzi review undo %s %s`, `uzi review show %s`,
//     `uzi repo list`, `uzi login`, `uzi review backlog --run <run-id>`, `uzi auth token`.
//   - HELP: a Short/Long description or a flag's usage text, cross-linking a sibling command.
//     Nobody needs to EXECUTE `uzi worker set-token` to validate a Long line that mentions it.
//     What such a string can be wrong about is naming a command path that does not exist —
//     which is statically checkable, complete, and stronger than the old bar, since nothing
//     used to verify that `uzi worker set-token` was a real path at all (only that `worker`
//     was a real top-level verb).
//
//     (This paragraph used to illustrate HELP with `uzi skill install`. It no longer can:
//     that string ALSO names the hook command uzicli writes into settings.json for Claude
//     Code to execute, so it carries the runtime bar. The example was true for the scan it
//     was written against and false for this one — which is the whole argument for scope
//     being part of the claim.)
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
// MEASURED at `5d5d0be4` across BOTH scanned packages (see instructionScope): 10 distinct
// candidates lifted from 13 sites, every one resolving to a definite kind. So a kindUnknown
// you hit later is a genuinely NEW emitter, not a gap in the classifier's original coverage.
// (The figure this paragraph used to carry was 9 candidates, correct for the one-package scan
// it was written against; the two extra are `uzi auth token` and the second `uzi skill
// install` site, both in api/internal/uzicli.)
//
// 🔴 WHICH EDIT CAN SILENTLY RE-OPEN THE HOLE — CORRECTED, and the old answer was wrong in the
// SAFE direction, which is exactly why it survived review. This paragraph used to say that
// widening `emitters` was the one edit that could quietly exempt strings, because a wrongly
// added wrapper would make everything printed through it HELP. It cannot. `emitters` has ONE
// use site (classifyKind), and that arm can only ever return kindRuntime — the STRICTEST bar.
// Widening `emitters` can only ADD work: strings that were kindUnknown (failing) become
// RUNTIME (needing execution evidence). It is a decision worth recording, but it is not the
// dangerous one, and pointing at it sent a reader to guard the door that does not open.
//
// The knobs that CAN silently exempt, in the order someone would actually reach for them:
//
//  1. classifyKind's kindHelp ARMS AND ITS DEFAULT. The default returns kindUnknown, which
//     FAILS. Change that default to kindHelp — or add a `case` returning kindHelp for a
//     position that is not documentation — and every string in that position is exempt at
//     once, with no entry for anyone to review. `helpFields` and `flagSets` are the same knob
//     wearing different names: both feed kindHelp. This is the one to guard.
//  2. A HUMAN CHOOSING evidenceHelpOnly. It is the only evidence value that requires neither
//     an execution nor a written reason, so it is the cheapest way out of the bar. What stops
//     it today is the derived-kind check below: an entry declared evidenceHelpOnly whose sites
//     classify RUNTIME reddens with "the kind is DERIVED, not declared". So this knob is not
//     usable on its own — it needs knob 1 to have been turned first. That pairing is the
//     mechanism, and it is why knob 1 is not merely the more likely one but the load-bearing one.
//  3. SCOPE — see instructionScope. A package that is not scanned is exempt in full, and
//     nothing in the file announces it. That was true here for three weeks; see below.
//
// WHY A REGISTRY RATHER THAN THREE TESTS. Fixing three strings leaves the seam open: the next
// commit that prints a hint reopens it, and nothing says so. These tests fail the build when a
// SCANNED package prints a `uzi …` command that has no entry below — so a FOURTH instruction
// cannot land silently. They need no server, which is why they run in `go test ./...` and
// make the omission loud at the moment of writing rather than at review time.
//
// 🔴 THAT GUARANTEE WAS FALSE UNTIL `5d5d0be4`, AND THE HOLE WAS SCOPE. The scan was
// `parser.ParseDir(fset, ".", …)` — api/cmd/uzi and nothing else — while
// api/internal/uzicli/client.go already printed a backticked, liftable `uzi auth token` with
// zero entries here, and a brand-new instruction added in that package left all three tests
// green. The guarantee is only ever as wide as instructionScope, so the scope list is part of
// the claim and is stated as such rather than buried in liftAll.
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
		command:  "uzi run list",
		evidence: evidenceE2E,
		where:    "uzi run list --json parses and matches GET /api/runs",
		// ARRIVED FROM PRD #112 ON THE WAVE-3 MERGE, and adapted rather than invented: the
		// entry main added carried `command` and `note` only, predating the `evidence` field,
		// which this tree requires (evidenceUnset is invalid on purpose). The CLAIM below is
		// main's author's, verified here rather than paraphrased; only its EXPRESSION is new.
		//
		// One site emits BOTH this and the `uzi run logs` entry below — `uzi tui`'s non-TTY
		// refusal (tui.go), a single Exitf naming the two scriptable alternatives an agent
		// should use instead of a full-screen UI. Exitf is an emitter, so both derive RUNTIME.
		//
		// VERIFIED, not inherited: the e2e address is a real executed phase, not a comment
		// mentioning one — `pass "uzi run list --json parses and matches GET /api/runs (…)"`.
		// That distinction matters because the address check is a strings.Contains over the
		// harness text, which a comment would satisfy just as well.
		note: "RUNTIME: `uzi tui`'s non-TTY refusal (tui.go) names the two scriptable " +
			"alternatives. EXECUTED against a booted API in e2e/run-e2e.sh: the harness runs " +
			"`uzi run list --json` on the host against the live stack and asserts its run-id SET " +
			"EQUALS GET /api/runs's — so the command works AND its output agrees with the API, " +
			"which is the property the instruction actually promises. Also driven through the " +
			"real cobra parse against a fake client in commands_test.go.",
	},
	{
		command:  "uzi run logs",
		evidence: evidenceGoTest,
		where:    "TestRunLogsFollowStopsOnTerminal",
		// Same site, same adaptation as above.
		//
		// 🔴 MAIN'S NOTE CITED `commands_test.go:692` AND THE LINE HAD DRIFTED — the call is at
		// :732, inside TestRunLogsFollowStopsOnTerminal. The CLAIM is true and the CITATION was
		// stale, which is exactly why evidenceGoTest takes a FUNCTION NAME: a name survives the
		// edit that moves a line. Found by grepping for `"--follow"` rather than reading :692.
		//
		// MAIN'S HONEST LIMIT IS PRESERVED VERBATIM IN SUBSTANCE, because the author who
		// measured it is worth more than a paraphrase: the follow loop is NOT executed against
		// a booted API. e2e covers live streaming through the /api/ws legs instead, so the poll
		// loop's behaviour against a real server is unit-level only.
		note: "RUNTIME: the same `uzi tui` non-TTY refusal (tui.go), as the follow-a-run " +
			"alternative. EXECUTED WITH --follow through the real parse in " +
			"TestRunLogsFollowStopsOnTerminal (commands_test.go), which asserts the follow loop " +
			"drains and terminates on a terminal run. NOT executed with --follow against a " +
			"booted API — the e2e harness uses the /api/ws legs for live streaming, so the poll " +
			"loop against a real server is covered only at the unit level. Stated rather than " +
			"implied, per this file's own rule.",
	},
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
		command:  "uzi token pool",
		evidence: evidenceHelpOnly,
		// ARRIVED WITH PRD #111 M3. `uzi worker set-token --auto` names the command
		// that fills the pool it selects from, because "auto" is inert until a user
		// has opted at least one token in — a help text that omitted it would send
		// someone to a mode that silently does nothing.
		//
		// HELP, derived: the span sits in a cobra Long field (worker.go), which
		// classifyKind reads as documentation rather than an emitter. Nothing runs it
		// and nothing should; the complete bar for a help reference is that the path
		// RESOLVES, and it does — `uzi token pool` is a real subcommand, pinned by
		// TestTokenSubcommands and exercised by four tests in token_test.go.
		note: "HELP: `uzi worker set-token`'s Long help cross-links the command that " +
			"populates the auto-selection pool (worker.go). Never emitted at runtime; the " +
			"path-resolution check is the complete bar.",
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
		command:  "uzi run wait",
		evidence: evidenceHelpOnly,
		// ARRIVED WITH PRD #264 M1. The span sits inside `uzi run wait`'s own Long
		// description (run.go), which names itself twice — once for the bare
		// wait-for-gate-or-end case and once for the D2 narrow-after-approve rule. It is
		// a cobra Long field, so classifyKind derives HELP; nothing emits it at runtime
		// (the loop's stderr lines are transitions like `run r1: running`, not a command
		// to run). The complete bar for a help reference is that the path RESOLVES —
		// `uzi run wait` is a real subcommand, pinned by TestCommandTree and exercised by
		// the run_wait_test.go suite.
		note: "HELP: inside `uzi run wait`'s own Long description (run.go), naming itself " +
			"while explaining the bare-wait default and the D2 narrow-after-approve rule. " +
			"Never emitted at runtime.",
	},

	// ---- RUNTIME: emitted at a decision point. The bar is EXECUTION. --------------------
	{
		command:  "uzi token list",
		evidence: evidenceGoTest,
		where:    "TestTokenPoolUnknownLabelIsUsageError",
		// ARRIVED WITH PRD #111 M2, and the backstop caught it the moment it entered the
		// tree — `uzi token pool` resolves a LABEL client-side, so an unresolvable one is a
		// usage error that has to tell the caller where the valid labels are.
		//
		// RUNTIME, not HELP, and derived rather than chosen: the span sits inside an
		// Exitf argument (token.go), which classifyKind reads as an emitter. The entry is
		// therefore a claim that the command was EXECUTED and its outcome asserted, which
		// is what TestTokenPoolUnknownLabelIsUsageError does: it drives the real cobra parse
		// with a label the fake client's list does not contain and asserts the OUTCOME pair
		// — exit 3 (ExitUsage, not a 404, because the label never reached the server) and
		// NO write attempted (LastPoolSecretID stays empty). The second half is the one
		// worth having: an implementation that sent the label to the server and let it 404
		// would produce the same exit code from the user's side.
		//
		// Its honest limit, stated because the e2e rows set a higher bar: the remedy is
		// executed against a FAKE client, so this proves the argv shape and the refusal, not
		// that `uzi token list` succeeds against a booted API. TestTokenList covers the
		// command itself, also against a fake.
		note: "RUNTIME: `uzi token pool`'s unknown-label refusal (token.go) names the read " +
			"that prints the caller's valid labels. EXECUTED through the real parse in " +
			"TestTokenPoolUnknownLabelIsUsageError, which asserts exit 3 AND that no write " +
			"was sent. Not executed against a booted API.",
	},
	{
		command:  "uzi skill install",
		evidence: evidenceGoTest,
		where:    "TestSkillInstallCommand",
		// TWICE CORRECTED. The first note said "Printed by the skill auto-upgrade warning";
		// the second said HELP, naming only the Short description in skill.go. Both were
		// right about the site they looked at and wrong about the set: the scan reached one
		// package, so the second site did not exist as far as this file was concerned.
		note: "RUNTIME by the strictest-bar rule, from TWO sites lifting the identical text. " +
			"(a) the Short description of `uzi skill install-hook` (cmd/uzi/skill.go) — " +
			"documentation; (b) `hookCommand` (internal/uzicli/skillhook.go), the command " +
			"written into ~/.claude/settings.json for Claude Code to EXECUTE at session start. " +
			"(b) makes a stronger claim than any printed hint does — it does not tell a human to " +
			"run something, it causes a machine to — and until the scope widened it carried no " +
			"bar at all. No split is expressible (both sites lift the same string and " +
			"attribute() keys on the text), so the execution bar governs. EXECUTED by " +
			"TestSkillInstallCommand, which runs `uzi skill install` through runCLI and asserts " +
			"the OUTCOME: the bundled SKILL.md is on disk byte-for-byte. Its honest limit: that " +
			"is the command working, not the HOOK invoking it — the wiring is " +
			"TestSkillInstallHookCommand's, and no test runs the settings.json entry the way " +
			"Claude Code would.",
	},
	{
		command:  "uzi auth token",
		evidence: evidenceGoTest,
		where:    "TestAuthTokenStdin",
		// The entry reviewer-web's Blocking #2 predicted: liftable, printed, and invisible
		// to this file for as long as the scan was one package deep.
		note: "RUNTIME: statusError's 401 message (internal/uzicli/client.go) — the remedy every " +
			"unauthenticated CLI call prints, through Exitf, i.e. STDERR with exit 3 (ExitAuth). " +
			"The literal is bound to `msg` and emitted two lines later, which is why it needed " +
			"the binding arm in classifyKind. EXECUTED by TestAuthTokenStdin: `uzi auth token` " +
			"reads the token from stdin and the OUTCOME asserted is that it is readable back out " +
			"of the credential store, not that it exited 0. Its honest limit, stated because the " +
			"e2e rows set a higher bar: the instruction is run with hand-written argv against a " +
			"fake client, NOT lifted from a real 401's own stderr. Nobody has closed the loop " +
			"from the failing call to the remedy fixing it. Doing so needs a stack, so it belongs " +
			"in e2e/run-e2e.sh next to the other three rows, not here.",
	},

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
		command:  "uzi review backlog --run",
		evidence: evidenceE2E,
		where:    "printed-instruction row: uzi review backlog --run",
		// THE ENTRY TEXT CHANGED WITH THE PRINTED TEXT, and that is the point rather than a
		// side effect. It used to read `uzi review backlog --run <run-id>`, matching a
		// LITERAL the CLI printed — `<run-id>` was a placeholder in the emitted output, not
		// a value substituted at emit time, so "executed verbatim" was undefined for it by
		// construction. The user approved changing the OUTPUT (PRD #98 M8b): the remedy is
		// now one runnable line per settled run, through a real format verb, so the lifted
		// candidate is `uzi review backlog --run %s` and this entry is its command PREFIX.
		//
		// It still attributes correctly against the sibling HELP entry: `uzi review backlog`
		// is a word-boundary prefix of this one, so most-specific-wins routes the runtime
		// candidate here and the flag-usage candidates there. Without this entry the two
		// kinds collapse onto one entry and TestInstructionEvidenceIsWellFormed reddens —
		// measured, not assumed, while making this change.
		note: "RUNTIME: the post-write truncation remedy (runGroupDisposition, review.go), one " +
			"line per run the write settled. EXECUTED in e2e against a 2001-row seed — the only " +
			"arrangement that can reach `truncated` at all, since JudgeBacklogMaxRows is a " +
			"compile-time const with no env override. The row lifts every printed line from " +
			"that command's own stdout, asserts the COUNT first, runs each verbatim, and " +
			"asserts the OUTCOME: the anchored re-read comes back NOT truncated. " +
			"TestReviewTruncationWarningsNameAWorkingRemedy pins that it names --run and NOT " +
			"--bucket (whose uselessness here was measured by lowering the cap to 2), that the " +
			"run id is SUBSTITUTED rather than a placeholder, and that a call settling nothing " +
			"prints no command at all rather than an unrunnable one.",
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
//
// 🔴 THERE IS A SECOND, NARROWER CLASS IN e2e/run-e2e.sh AND THE DIFFERENCE HAS A CONSEQUENCE.
// This one reads SOURCE literals, so it must admit format verbs and placeholders: `%<>-`.
// run_printed_instructions' allowlist reads EMITTED text, which has to be runnable verbatim
// in a shell, so it is `[A-Za-z0-9 ._:/=-]` and admits none of them. The asymmetry is
// correct — but it means an instruction whose EMITTED form carries any character outside the
// runtime class can be REGISTERED here and can never be EXECUTED there: evidenceNotExecuted
// by construction rather than by choice. `%`, `+`, `@`, `,` and `~` are all outside it, so a
// judge target containing one would fail the harness floor with a message about the
// allowlist rather than about the target. Loud rather than silent, and low probability —
// but it is discoverable only by reading both files, so each now points at the other.
var instructionRE = regexp.MustCompile("`(uzi [a-z][a-z0-9 %<>-]*)`|(?m)^\\s*(uzi [a-z][a-z0-9 %<>-]*)")

// instructionScope is every package this backstop scans, relative to api/cmd/uzi. IT IS PART
// OF THE GUARANTEE, not an implementation detail: an unscanned package is exempt in full and
// silently, which is knob 3 above and was a live hole until `5d5d0be4`.
//
// api/internal/uzicli is in scope because it is the CLI's own client library and it PRINTS —
// statusError builds the messages every non-2xx exit carries, `uzi auth token` among them.
// Adding a package here is cheap and safe (it can only add candidates); leaving one out is
// neither, and nothing fails when you do.
var instructionScope = []string{".", "../../internal/uzicli"}

// emitters are the calls whose string arguments reach a user at runtime. A call NOT in this
// set is not a classification — classifyKind keeps walking outward, so a literal wrapped in a
// Sprintf inside a Println still classifies RUNTIME. A genuinely unrecognised position stays
// kindUnknown and FAILS; see C11 of the design note.
//
// Widening this set is safe by construction: its only arm returns kindRuntime, the strictest
// bar. See the corrected residual-risk paragraph at the top of this file — this is NOT the
// knob that can grant a silent exemption, and it was named as such for three weeks.
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
		case *ast.AssignStmt, *ast.ValueSpec:
			// A command string BOUND TO A NAME — a local variable, or a package-level
			// const/var — rather than handed straight to an emitter or sitting in a help
			// field. THE STRICTEST BAR APPLIES, and this arm exists because a binding is
			// precisely where an instruction goes to hide from a position-based classifier.
			//
			// The header comment already promised that indirection was handled ("a literal
			// wrapped in a Sprintf inside a Println still classifies RUNTIME"), and that was
			// only ever true of EXPRESSION nesting. A variable binding broke the chain, so
			// MEASURED at `5d5d0be4`, `uzicli/client.go`'s
			//
			//	msg = "authentication required or token invalid — run `uzi auth token` …"
			//	…
			//	return Exitf(ExitAuth, "%s", msg)
			//
			// derived kindUnknown despite reaching the user through Exitf two lines later —
			// no enclosing CallExpr, so nothing to recognise. Same for the package const
			// `uzicli/skillhook.go`'s hookCommand, which is not printed at all: it is written
			// into ~/.claude/settings.json for Claude Code to EXECUTE, which is a stronger
			// claim than any printed hint makes and had no bar on it whatsoever.
			//
			// WHY THIS ARM CANNOT GRANT AN EXEMPTION, which is the only property that matters
			// for a new classifier arm: it returns kindRuntime and nothing else. It can only
			// move a string from kindUnknown (build fails, author must decide) to kindRuntime
			// (needs execution evidence). Contrast a kindHelp arm, which is knob 1.
			//
			// The cost, stated because it is real: a `const` holding help prose that happens
			// to name a command classifies RUNTIME here even though a Long field is its only
			// consumer, and its author must then restructure or carry runtime evidence. That
			// is a false POSITIVE on the strict side. There are none in the tree today, and
			// erring strict is the correct direction for this file — a false positive costs
			// an argument, a false negative costs the guarantee.
			return kindRuntime
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
	commands := topLevelCommands(t)
	found := map[string]*candidate{}
	for _, dir := range instructionScope {
		//nolint:staticcheck // SA1019: parser.ParseDir (deprecated since go1.25) is adequate for this
		// test-only scan of a single build-tag-free package dir; a go/packages migration is out of scope
		// for PRD #264 and this edit only registers a new command's instruction below.
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		// POSITIVE CONTROL PER DIRECTORY. A moved or renamed package makes ParseDir return an
		// empty map with NO error, and this loop would then scan it to zero candidates while
		// every test stayed green — the same false-green shape as a live-DB suite that skips.
		// The scope list is part of the guarantee, so a scope entry that contributes no source
		// must fail rather than vanish.
		scanned := 0
		for _, pkg := range pkgs {
			for name, file := range pkg.Files {
				// Test files may legitimately quote a command while asserting on it.
				if strings.HasSuffix(name, "_test.go") {
					continue
				}
				scanned++
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
		if scanned == 0 {
			t.Fatalf("instructionScope names %q, which yielded no non-test Go source. The package "+
				"moved or was renamed; an entry that silently contributes nothing is worse than a "+
				"missing one, because the guarantee still reads as covering it.", dir)
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

	// REGISTRY WELL-FORMEDNESS, before anything is attributed. Both checks guard the ONE
	// place the most-specific-wins tie-break can be silent (architect's ruling 1.3), and
	// neither fires today — measured while adding them.
	//
	// A DUPLICATE `command` is the single exception to "longest wins is never ambiguous":
	// two entries whose command strings are byte-equal have equal length, attribute()'s
	// strict `>` keeps the FIRST, and the loser surfaces only through
	// TestRegisteredInstructionsAreStillPrinted as "no lifted candidate matches any more —
	// remove the entry". That diagnosis is wrong in the way that costs the most time: the
	// code is not gone, a duplicate ate it.
	//
	// SURROUNDING WHITESPACE breaks matchesCommand outright. `entry+" "` on an entry that
	// already ends in a space needs two spaces in the candidate, so every candidate for that
	// entry silently stops matching — which presents as the same wrong diagnosis.
	seenCommand := map[string]int{}
	for i, k := range knownInstructions {
		if strings.TrimSpace(k.command) != k.command {
			t.Errorf("knownInstructions[%d] command %q carries surrounding whitespace. matchesCommand "+
				"appends a single space to test the word boundary, so this entry can never match any "+
				"candidate — and it would report as 'no lifted candidate matches any more', which names "+
				"the wrong cause.", i, k.command)
		}
		if first, dup := seenCommand[k.command]; dup {
			t.Errorf("knownInstructions[%d] duplicates the command of entry %d (%q). attribute() picks the "+
				"LONGEST match with a strict >, so equal-length entries silently resolve to the first and "+
				"the second reports as 'no lifted candidate matches any more' — the code is not gone, a "+
				"duplicate ate it. Merge them, or make the texts distinguishable.", i, first, k.command)
			continue
		}
		seenCommand[k.command] = i
	}

	// Derived kind per entry, from the candidates attributed to it.
	//
	// `oneText` records that a SINGLE candidate text carried more than one kind. That is the
	// one shape the "split it into two entries" remedy below cannot address, because attribute()
	// keys on the candidate TEXT: two entries with the same command string are forbidden (they
	// make the most-specific tie-break silent), so there is no split to make. See the
	// same-text arm below.
	kinds := map[int]map[instructionKind][]string{}
	oneText := map[int]bool{}
	for cmd, c := range found {
		i := attribute(cmd)
		if i < 0 {
			continue // reported by TestPrintedInstructionsAreRegistered
		}
		if kinds[i] == nil {
			kinds[i] = map[instructionKind][]string{}
		}
		if len(c.kinds) > 1 {
			oneText[i] = true
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
		if len(byKind) > 1 && !oneText[i] {
			// TWO DIFFERENT candidate texts landed on this entry and disagree. That IS
			// splittable — the texts differ, so a longer entry can take the runtime one and
			// most-specific-wins routes it there. This is the arm the architect's ruling 1.2
			// relies on to show first-match and all-match attribution are unsatisfiable
			// against a registry C0 requires; it is unchanged.
			t.Errorf("%q is printed from BOTH help and runtime positions (%v). Split it into two "+
				"entries — the two kinds carry different bars and one entry cannot honestly hold both.",
				k.command, byKind)
			continue
		}
		// THE STRICTEST BAR PRESENT GOVERNS. With one kind this is that kind, which is all it
		// has ever been. It differs only for the same-text case above: ONE command string
		// emitted from both a documentation position and a runtime position, where no split is
		// expressible and the honest rule is that a string printed at a decision point must be
		// executed regardless of also appearing in help text. Live today for
		// `uzi skill install` — a Short description in skill.go AND the hook command uzicli
		// writes into settings.json for Claude Code to run.
		derived := kindHelp
		if _, runtime := byKind[kindRuntime]; runtime {
			derived = kindRuntime
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

		// THE PATH MUST RESOLVE in the live cobra tree. Nothing used to check this — only that
		// the SECOND word was a real top-level verb, so `uzi worker set-token` was never
		// verified past `worker`.
		//
		// Applied to EVERY entry whose command is a pure path, not only to the help ones. It
		// was help-only while evidenceHelpOnly was the only kind that could hold a bare path;
		// `uzi skill install` moving to a runtime bar would otherwise have silently dropped its
		// path check, which is a regression hiding inside a fix. It also gains `uzi repo list`,
		// `uzi review show`, `uzi review undo` and `uzi login` — all of which could have been
		// renamed out from under their entries with nothing here noticing.
		if isCommandPath(k.command) {
			assertCommandPathResolves(t, root, k.command)
		}

		switch k.evidence {
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

// isCommandPath reports whether every word past `uzi` could name a command — no flags, no
// format verbs, no placeholders. Only those entries can be looked up in the cobra tree;
// `uzi review backlog --run <run-id>` carries an argument and is deliberately excluded rather
// than special-cased, since a partial resolve would assert less than it appears to.
func isCommandPath(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) < 2 || fields[0] != "uzi" {
		return false
	}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") || strings.ContainsAny(f, "%<>") {
			return false
		}
	}
	return true
}

// assertCommandPathResolves checks that every word of a command-path reference past `uzi` names
// a real command in the tree, with nothing left over.
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
		t.Errorf("registered instruction %q does not resolve in the cobra tree: %v", ref, err)
		return
	}
	if len(rest) != 0 {
		t.Errorf("registered instruction %q does not resolve: %q is not a command under %q (the tree "+
			"stopped at %q). For a HELP entry this is the one thing it can be wrong about and the "+
			"complete bar; for a RUNTIME entry it is the cheap half — the command was renamed out "+
			"from under a string that still tells a user to run it.",
			ref, rest, cmd.CommandPath(), cmd.CommandPath())
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
