package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// The printed-instruction backstop (PRD #98 review, seam 5).
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
// WHY A REGISTRY RATHER THAN THREE TESTS. Fixing three strings leaves the seam open: the next
// commit that prints a hint reopens it, and nothing says so. This test fails the build when
// this package prints a `uzi …` command that has no entry below — so a FOURTH instruction
// cannot land silently. It is a grep and a set difference, which is why it can live here
// instead of waiting for the e2e stack: it needs no server, and it makes the omission loud at
// the moment of writing rather than at review time.
//
// WHAT AN ENTRY MEANS. Adding a line here is a claim that the instruction has been EXECUTED
// and its outcome asserted — not that it looks right. Record where. If you cannot yet execute
// it, say so in the note; an honest "not yet executed" entry is a known gap, whereas no entry
// at all is an invisible one.

// knownInstruction is one printed command the package may emit, with the evidence that it
// works. `command` is matched as a prefix against what the source prints, so flags and format
// verbs after it do not need restating.
type knownInstruction struct {
	command string
	note    string
}

var knownInstructions = []knownInstruction{
	{
		command: "uzi worker set-token",
		// NOT PRD #98's command — it arrived from PRD #104 on the landing merge (2026-07-21),
		// and this entry exists because the backstop did exactly what it was built to do:
		// failed the build on the FOURTH instruction, the one nobody had written yet, at the
		// moment it entered the tree rather than at review time. It came from another PRD,
		// which is a better outcome than the one designed for, not a worse one.
		//
		// HONEST STATUS: NOT EXECUTED AS A LITERAL INSTRUCTION. Registered per this file's own
		// rule that a stated gap beats an invisible one. What IS covered, verified rather than
		// assumed: `worker set-token <id> <label>` and `--default` are both driven through the
		// real cobra parse into a fake client in worker_test.go:56-82, which asserts the
		// (id, label) pair reaching SetWorkerToken and that the output names the token. So the
		// argv shape this string tells the user to type is pinned.
		// What is NOT: nobody has run this string verbatim against a booted API and asserted
		// the worker's binding actually moved. That is PRD #104's to close, not this MR's —
		// registering it here without saying so would be the precise lie the registry exists
		// to prevent.
		note: "Printed in `uzi token`'s Long help (token.go). Argv shape executed through the real " +
			"parse against a fake client (worker_test.go:56-82: id+label and --default, both " +
			"asserting the pair reaching SetWorkerToken). NOT yet executed against a booted API — " +
			"no test asserts the binding moved. Arrived from PRD #104 at the landing merge; " +
			"closing the gap belongs to that PRD.",
	},
	{
		command: "uzi review undo",
		note: "Printed per settled member by runGroupDisposition. Executed: the (run, rec) pair is " +
			"fed straight to DeleteDisposition in TestBulkDispositionSettledIsAWorkingUndoAddressLiveDB " +
			"and the row asserted gone; the argument shape is pinned by TestReviewGroupHumanPointsAtTheUndoAddresses. " +
			"NOT YET run against a booted API as literal argv — the service beneath and the parse above are covered.",
	},
	{
		command: "uzi review backlog",
		note: "Printed by both truncation warnings as the way to narrow below the row cap. It names " +
			"--run, which is the ONLY filter applied before the cap; TestReviewTruncationWarningsNameAWorkingRemedy " +
			"pins that it does NOT name --bucket, whose uselessness here was measured by lowering the cap to 2. " +
			"TestReviewBacklogRunAnchorForwarded pins that the flag reaches the wire.",
	},
	{
		command: "uzi review show",
		note: "Printed by resolveRecID's refresh hint. Exercised by TestReviewResolveUnknownID, which " +
			"asserts the hint appears on a no-match; the command itself is the package's own read verb, " +
			"covered by TestReviewShow*.",
	},
	// ---- surfaced BY this backstop on its first run, and registered honestly ----------
	//
	// These four predate PRD #98 and none has been executed from its printed form. They are
	// listed so the gap is VISIBLE rather than invisible — which is the whole point of the
	// registry — not because they have been verified. Do not read an entry here as evidence;
	// read the note. Anyone touching these paths should upgrade the note by running them.
	{
		command: "uzi login",
		note:    "NOT EXECUTED. Printed by the auth-required path in login.go. Pre-existing; inherited gap.",
	},
	{
		command: "uzi repo list",
		note:    "NOT EXECUTED. Printed by run.go when a repo id is needed. Pre-existing; inherited gap.",
	},
	{
		command: "uzi run inputs",
		note:    "NOT EXECUTED. Printed by run.go after submitting a steer. Pre-existing; inherited gap.",
	},
	{
		command: "uzi skill install",
		note:    "NOT EXECUTED. Printed by the skill auto-upgrade warning. Pre-existing; inherited gap.",
	},
}

// instructionRE lifts a candidate from a printed string: either backtick-quoted, or a line
// that is itself runnable (the form the revert listing uses). Both are the shapes an
// instruction is allowed to take — anything else is unliftable and would defeat a test that
// must execute the printed text rather than a hand-written copy.
//
// A candidate is only an INSTRUCTION if its second word is a real command. That check runs
// against the live cobra tree rather than a word list, which is what keeps prose out: the
// root command's own Long description begins "uzi drives the factory from the terminal", and
// a naive line-anchored regex reports it as an instruction to run `uzi drives`. Deriving the
// verb set from the tree also means a renamed command cannot leave this extractor stale.
var instructionRE = regexp.MustCompile("`(uzi [a-z][a-z0-9 -]*)`|(?m)^\\s*(uzi [a-z][a-z0-9 -]*)")

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

func TestPrintedInstructionsAreRegistered(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/uzi: %v", err)
	}

	commands := topLevelCommands(t)
	found := map[string]token.Position{}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			// Test files may legitimately quote a command while asserting on it.
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				for _, cmd := range liftInstructions(val, commands) {
					found[cmd] = fset.Position(lit.Pos())
				}
				return true
			})
		}
	}

	if len(found) == 0 {
		// Without this the test passes by parsing nothing — the same vacuity as a glob that
		// matches no files. This package definitely prints instructions.
		t.Fatal("no printed `uzi …` instructions found at all; the extractor is broken, not the code")
	}

	var unregistered []string
	for cmd, pos := range found {
		matched := false
		for _, k := range knownInstructions {
			if strings.HasPrefix(cmd, k.command) {
				matched = true
				break
			}
		}
		if !matched {
			unregistered = append(unregistered, cmd+"  ("+pos.String()+")")
		}
	}
	sort.Strings(unregistered)
	for _, u := range unregistered {
		t.Errorf("printed instruction %q has no entry in knownInstructions.\n"+
			"    Add one — and adding it is a claim that you EXECUTED the command and asserted its\n"+
			"    outcome, not that it reads correctly. Two of the three instructions this package\n"+
			"    shipped parsed perfectly and were false.", u)
	}
}

// The registry must not rot in the other direction either: an entry whose command nothing
// prints any more is a claim about code that no longer exists, and it would quietly absorb a
// future instruction that happens to share its prefix.
func TestRegisteredInstructionsAreStillPrinted(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/uzi: %v", err)
	}
	var all strings.Builder
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						all.WriteString(v)
						all.WriteString("\n")
					}
				}
				return true
			})
		}
	}
	for _, k := range knownInstructions {
		if !strings.Contains(all.String(), k.command) {
			t.Errorf("knownInstructions lists %q, which this package no longer prints — remove the entry "+
				"rather than leaving a claim about code that is gone", k.command)
		}
	}
}
