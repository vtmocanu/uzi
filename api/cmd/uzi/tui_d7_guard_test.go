package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// d7Sanitizers are the calls that make an untrusted string safe to draw. Markdown and
// Plain are tuiRenderer methods; the rest are the shared package-main helpers the
// layout decision exists to keep reachable.

// The D7 structural guard, in the spirit of TestNoServerDeps: a build failure rather
// than a review nit.
//
// WHY IT EXISTS. Choosing `package main` over a tui/ subpackage bought reachability of
// sanitizeTTY and the other ~25 shared helpers, and the cost was that D6/D7's "cannot
// drift" stopped being a compile property and became a review property. Glamour itself
// is safe — tuiRenderer.md is unexported, so nothing can reach the markdown renderer
// without passing through Markdown(), which sanitizes first. A plain strings.Builder is
// not safe: nothing structurally stops a future `sb.WriteString(rv.SummaryMd)`.
//
// WHAT IT ACTUALLY CHECKS, stated precisely because a guard that advertises more than
// it holds is worse than none: a mention of an untrusted DTO field must be enclosed by
// a sanitizer if it is either
//
//	(a) an argument to a WRITER (WriteString, Sprintf, lipgloss Render, …), or
//	(b) an operand of a string CONCATENATION.
//
// Data plumbing is not a draw, so a struct-field copy, a map-key build, a comparison
// and a selector function's return are all untouched.
//
// Clause (b) is not decoration — it was added because mutating clause (a) alone proved
// insufficient. The dominant shape in these files is `head := x + rec.Target + y`
// followed by a write of `head`, so the mention sits in an assignment, not inside the
// writer call. A guard with only clause (a) stayed GREEN when rec.Target was drawn
// raw in exactly the place a contributor would add a new field. Concatenating
// untrusted text is always a draw here; nothing plumbs data with `+`.
//
// I FIRST WROTE THE STRICTER RULE — every syntactic mention must sit inside a sanitizer —
// on the theory that it would also catch `s := rv.SummaryMd; sb.WriteString(s)`. It
// produced FOUR false positives and ZERO true ones on correct code: two struct-field
// copies in the laneFrame converters, laneLabel's `return f.AgentLabel`, and a
// coordKey() map-key build whose sibling draw was already sanitized. A guard with that
// ratio trains people to add exceptions until it means nothing, so it is not the rule.
//
// WHAT IT DOES NOT CATCH, measured rather than guessed. Four mutations were run
// against it; three redden and the fourth does not:
//
//	A  sb.WriteString(rv.SummaryMd)                    → caught (writer argument)
//	B  head := x + rec.Target + y                      → caught (concatenation)
//	C  Render(l.Label) in the lane rail                → caught (internal field name)
//	D  name := l.Role   … later drawn via `name`       → NOT CAUGHT
//
// D is the indirection gap and it is not closable here: making a bare assignment a
// trigger re-flags the laneFrame converters, which copy these fields as plumbing. So
// the honest boundary is "a mention that is written or concatenated in place", and a
// contributor who lands the value in a local first defeats it. That is why this guard
// supplements the render tests (which drive real model state through Update/View and
// assert on the output) rather than replacing them — those catch D, this catches the
// shape that reaches review most often.
//
// Also not caught: a helper in another file returning the field, a whole struct
// rendered by %v, and anything outside tui_*.go.
//
// So this is a PATTERN, not an identity. It is scoped to the shape a contributor
// actually writes when adding a field to a panel — a direct draw — and it is worth
// having on that basis and on no stronger one. The indirection gap is real and is the
// reason this does not replace the render tests.

// d7UntrustedFields are the DTO fields carrying model-authored or forge-authored free
// text that the TUI draws. Closed enums (Verdict, Category, Confidence, Status, Kind)
// are deliberately absent: they are safe to branch on, and D7's rule for them is
// "branch only on these", which is a different property that the render tests pin.
// It lists BOTH the wire names and the INTERNAL struct names the TUI copies them into.
// That second half was found by mutation: the lane rail draws agentLane.Label and
// agentLane.Role, which carry AgentLabel and Agent from the wire under different names,
// so a DTO-keyed list alone left the entire rail unguarded and a raw draw there stayed
// green. Any future struct that carries untrusted text under a new field name has to be
// added here — that is this guard's standing weakness, and it is why the render tests
// remain the primary defence rather than this.
var d7UntrustedFields = []string{
	// internal (tui_lanes.go)
	"Label",
	"Role",
	"Agent",
	// wire (apitypes)
	"SummaryMd",
	"RationaleMd",
	"Target",
	"IssueTitle",
	"IssueDescription",
	"AgentLabel",
	"HealthReason",
	"FailureReason",
	// OwnerEmail is forge-authored and shown on the admin board (PRD #325 M2, B1). It
	// must be drawn only through a sanitizer (renderer.Plain); the clean-fixture
	// screenshots cannot catch a raw draw, so this guard + the hostile-OwnerEmail render
	// test in TestTUIViewsStripControlBytesFromUntrustedText are the defence.
	"OwnerEmail",
}

// d7Writers are the calls that put a string on the screen. lipgloss's Render is one:
// it styles and returns text that is then written.
var d7Writers = map[string]bool{
	"WriteString": true, "Write": true, "WriteRune": true, "WriteByte": true,
	"Sprintf": true, "Sprint": true, "Sprintln": true,
	"Printf": true, "Println": true, "Fprintf": true, "Fprintln": true,
	"Render": true,
}

var d7Sanitizers = map[string]bool{
	"sanitizeTTY": true,
	"cellText":    true,
	"compactText": true,
	"capCell":     true,
	"Markdown":    true,
	"Plain":       true,
}

func TestD7UntrustedFieldsNeverReachAWriterUnsanitized(t *testing.T) {
	files, err := filepath.Glob("tui_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var srcs []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			srcs = append(srcs, f)
		}
	}
	if len(srcs) == 0 {
		// Without this the test passes by parsing nothing — the same vacuity as a glob
		// that matches no files.
		t.Fatal("no tui_*.go source files found; the guard is scanning nothing")
	}

	fset := token.NewFileSet()
	var violations []string
	sanitizedMentions := 0

	for _, src := range srcs {
		f, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		// Walk with a stack so each mention can be asked what encloses it.
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)

			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !d7Field(sel.Sel.Name) {
				return true
			}
			switch {
			case enclosedByCall(stack, d7Sanitizers):
				sanitizedMentions++
			case !enclosedByCall(stack, d7Writers) && !enclosedByConcat(stack):
				// Not a draw: a struct-field copy, a map-key build, a comparison, a
				// selector function's return. Plumbing, not rendering.
			default:
				violations = append(violations,
					fset.Position(sel.Pos()).String()+": ."+sel.Sel.Name)
			}
			return true
		})
	}

	// A positive control on the INSTRUMENT: if the walk found no sanitized mentions at
	// all, it is not looking at the render paths and its silence means nothing.
	if sanitizedMentions == 0 {
		t.Fatal("the guard found no SANITIZED mentions of any untrusted field; it is not observing the render paths, so a clean result proves nothing")
	}

	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("D7: untrusted field mentioned outside a sanitizer at %s\n"+
			"    Every draw of model- or forge-authored text must pass through one of: sanitizeTTY, cellText, compactText, capCell, renderer.Markdown, renderer.Plain.\n"+
			"    sanitizeTTY BEFORE Glamour, never after — reversing it strips Glamour's own escapes and prints literal SGR text.\n"+
			"    If this mention genuinely renders nothing (a comparison), the guard already allows that shape; anything else needs a sanitizer.", v)
	}
}

func d7Field(name string) bool {
	for _, f := range d7UntrustedFields {
		if f == name {
			return true
		}
	}
	return false
}

// enclosedByCall reports whether any enclosing node is a call to one of names. Walking
// the whole enclosing stack (rather than only the immediate parent) is what makes
// concatenation safe to reason about: in sb.WriteString("x " + Plain(f.Target)) the
// mention is nested inside a BinaryExpr, and both the writer and the sanitizer still
// enclose it.
func enclosedByCall(stack []ast.Node, names map[string]bool) bool {
	for _, n := range stack {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			continue
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if names[fn.Name] {
				return true
			}
		case *ast.SelectorExpr:
			if names[fn.Sel.Name] {
				return true
			}
		}
	}
	return false
}

// enclosedByConcat reports whether the mention is an operand of a `+`. In this package
// string concatenation is always building a line for the screen; nothing moves data
// with it.
func enclosedByConcat(stack []ast.Node) bool {
	for _, n := range stack {
		if bin, ok := n.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
			return true
		}
	}
	return false
}
