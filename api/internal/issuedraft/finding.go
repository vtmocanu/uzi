package issuedraft

import (
	"strconv"
	"strings"
)

// FindingDraftInput is the fully-resolved, DB-free input to RenderFinding (PRD #333 M4, D4).
// Title/Description/Location are UNTRUSTED agent-authored text (already inert at rest from the
// M2 ingest sanitiser, re-fenced here defensively at the write boundary). RepoPath is
// forge-controlled; RunShortID/RunKind/IssueIID are server/enum/integer provenance — none is
// worker free text, so the provenance footer interpolates them without a fence.
type FindingDraftInput struct {
	Title       string
	Description string
	Location    string

	RepoPath   string // path_with_namespace of the finding's repo (forge-controlled)
	RunShortID string // short id of the run that reported the finding (server-derived)
	RunKind    string // runs.kind of the run that reported the finding (provenance), e.g. issue, ci_fix, prompt, self_improve
	IssueIID   int64  // 0 when the reporting run has no issue iid
}

// FindingDraft is RenderFinding's output: the title + body that seed the editable draft, the
// standalone inert location the panel shows, and the provenance footer.
type FindingDraft struct {
	Title       string
	Description string
	Location    string
	Provenance  string
}

// RenderFinding templates the deterministic finding issue draft (PRD #333 D4). It is the
// finding-specific SIBLING of Render — Render is judge-hardcoded (judge headings,
// CategoryLabel over judge enums, an Input of judge fields) and MUST NOT be reused here.
// RenderFinding instead ROUTES each untrusted field through the matching field-level
// sanitiser this package owns:
//
//   - Title       → SanitizeTitle                     (single-line, defanged leading "/")
//   - Description → FenceBlock, then SanitizeFiledBody (breakout-proof fence, unfenced-/
//     strip, secret scrub over the whole body)
//   - Location    → SafeInlineCode                    (breakout-proof inline code span)
//
// and appends a provenance footer built only from server/enum/integer values. The file
// endpoint (M5) resolves the text to file from the stored, already-sanitised finding row, and
// re-runs these same passes on any user edit — this draft is a UX convenience, not the
// security boundary (D4, mirroring Render's Decision-10 contract).
func RenderFinding(in FindingDraftInput) FindingDraft {
	title := SanitizeTitle(in.Title)
	location := SafeInlineCode(in.Location)
	provenance := findingProvenance(in)

	var b strings.Builder
	b.WriteString("## The bug\n\n")
	b.WriteString(FenceBlock(in.Description))
	b.WriteString("\n## Where\n\n")
	b.WriteString("- Location: ")
	b.WriteString(location)
	b.WriteString("\n")
	if strings.TrimSpace(in.RepoPath) != "" {
		// RepoPath is forge-controlled, but fenced inline defensively anyway (the same
		// discipline Render applies to JudgeModel) so a breakout never rests solely on an
		// external constraint.
		b.WriteString("- Repo: ")
		b.WriteString(SafeInlineCode(in.RepoPath))
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	b.WriteString(provenance)

	return FindingDraft{
		Title:       title,
		Description: SanitizeFiledBody(b.String()),
		Location:    location,
		Provenance:  provenance,
	}
}

// findingProvenance builds the deterministic footer "Found by uzi run <shortID> while working
// <kind> #<iid>" (PRD #333 D4). Every interpolated value is server-derived, an enum, or an
// integer — never agent free text — so it needs no fence. It names how the (attacker-
// influencable) text above was produced and states that the fenced body is agent-authored and
// unverified, matching Render's footer contract.
func findingProvenance(in FindingDraftInput) string {
	who := "a uzi run"
	if in.RunShortID != "" {
		who = "uzi run " + in.RunShortID
	}
	working := ""
	if in.IssueIID > 0 {
		kind := in.RunKind
		if kind == "" {
			kind = "run"
		}
		working = " while working " + kind + " #" + strconv.FormatInt(in.IssueIID, 10)
	}
	return "Found by " + who + working +
		". This bug was noticed off-task during that run; the description above is " +
		"agent-authored and unverified, and is fenced (not blockquoted) so links, image " +
		"beacons and quick-actions render inert."
}
