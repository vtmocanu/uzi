package workersvc

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Proposal filing caps and the never-sweepable label invariant (PRD #929 M2/D3).
const (
	// ProposalTitleMaxBytes bounds the filed issue's title. A forge title is a single
	// line; 200 bytes is generous for an idea headline and keeps a hostile worker from
	// smuggling a wall of text into the title field.
	ProposalTitleMaxBytes = 200
	// ProposalBodyMaxBytes bounds the filed issue's body, reusing the report_md cap
	// (clampWireReportMd → ReviewSummaryMaxBytes) so proposal bodies and report-only
	// summaries share one untrusted-text ceiling.
	ProposalBodyMaxBytes = ReviewSummaryMaxBytes

	// proposalLabelBase is the scoped marker label (D2). The label set filed with a
	// proposal issue is EXACTLY one of {proposalLabelBase} or {proposalLabelBase +
	// proposalLabelSep + <catalog-slug>} — constructed here from constants, NEVER from
	// model output. It carries no `uzi`, no sweep selector (Planned/bug/refactor), no
	// bot assignment: a filed proposal is never sweep-eligible at creation (D3), so an
	// unattended sweep can never auto-implement a half-formed idea. This invariant is
	// tested (TestMaybeFileProposal...) and must not be widened.
	proposalLabelBase = "proposal"
	proposalLabelSep  = "::"
)

// ProposalLabel builds the scoped marker label for a proposal-shaped schedule (PRD #929
// D2): the bare proposalLabelBase for a slugless (user) schedule, else
// proposalLabelBase::<catalogSlug>. It is the SINGLE source of the label string so the fire
// side (schedsvc, which lists issues carrying it to build the dedup digest) and this filing
// side query and file the SAME label — they can never drift. The label is built purely from
// the constants above plus the given slug; it never carries `uzi`, a sweep selector, or a
// bot assignment (D3).
func ProposalLabel(catalogSlug string) string {
	if catalogSlug == "" {
		return proposalLabelBase
	}
	return proposalLabelBase + proposalLabelSep + catalogSlug
}

// catalogSlugString reads a schedule's catalog slug as a plain string, "" when NULL/invalid.
func catalogSlugString(sched store.RunSchedule) string {
	if sched.CatalogSlug.Valid {
		return sched.CatalogSlug.String
	}
	return ""
}

// clampWireProposal narrows the worker's proposal declaration to what may be filed.
// It returns nil unless the run is a SCHEDULED PROMPT run (defense-in-depth kind gate;
// the deeper output-mode/target gates run in maybeFileProposal once the schedule is
// loaded) carrying a non-empty title. Title and Body are untrusted worker output — the
// agent may have seen repo secrets during the run — so each is control-char/format
// stripped, secret-scrubbed, and ONLY THEN length-bounded (see scrubThenBound: the cap
// is applied LAST so the scrubber sees the whole field; a credential straddling the cap
// must not be truncated into an unmatched prefix that leaks into the filed forge issue).
//
// Like the other clampWire* helpers this DROPS (returns nil), never errors: it feeds a
// best-effort side effect on the terminal `completed` report and must not be able to
// fail a worker's report on a technicality.
func clampWireProposal(run store.Run, p *ProposalPayload) *ProposalPayload {
	if p == nil {
		return nil
	}
	if run.Kind != runkind.Prompt || !run.ScheduleID.Valid {
		slog.Warn("dropping proposal: not a scheduled prompt run", "run_id", run.ID, "kind", run.Kind)
		return nil
	}
	title := scrubThenBound(p.Title, ProposalTitleMaxBytes)
	if title == "" {
		return nil
	}
	body := scrubThenBound(p.Body, ProposalBodyMaxBytes)
	return &ProposalPayload{Title: title, Body: body}
}

// scrubThenBound sanitizes the FULL untrusted field, secret-scrubs the whole value, and
// ONLY THEN applies the byte cap. The order is a security property: secretscrub.Scrub
// matches a credential only when it sees the WHOLE token, so truncating first can leave a
// sub-match-length prefix that Scrub misses, leaking it into the filed forge issue (a
// public artifact). The first SanitizeBounded is effectively unbounded — a cap past the
// input length never truncates, since sanitizing only removes runes — so no cut happens
// before Scrub; the second applies the real, rune-safe cap to the already-scrubbed text.
func scrubThenBound(s string, max int) string {
	sanitized := termsafe.SanitizeBounded(s, len(s)+1)
	return termsafe.SanitizeBounded(secretscrub.Scrub(sanitized), max)
}

// maybeFileProposal files a scheduled issues-mode prompt run's proposal as a forge
// issue on run completion (PRD #929 M2, Design C: the agent conveys the idea via the
// completion report; the api does the forge write — the agent never gains a
// forge-write tool, D1). It is BEST-EFFORT and NEVER returns/propagates an error: the
// forge issue is a side effect of a terminal `completed` report, and a failure here
// must never fail that report (exactly like maybeEnqueueJudge). Every failure
// log-and-returns.
//
// Filing carries EXACTLY the scoped marker label proposalLabelBase[::<slug>] (D3): the
// issue is never sweep-eligible at creation — a human promotes it by adding labels.
func (s *Service) maybeFileProposal(ctx context.Context, run store.Run, proposal *ProposalPayload) {
	// Cheap-first gates, each a silent early return.
	if proposal == nil {
		return
	}
	if s.forges == nil {
		slog.Warn("file proposal: forges unavailable", "run_id", run.ID)
		return
	}
	if run.Status != "completed" {
		return
	}
	if run.Kind != runkind.Prompt {
		return
	}
	if !run.ScheduleID.Valid {
		return
	}

	sched, err := s.q.GetRunSchedule(ctx, run.ScheduleID.Bytes)
	if err != nil {
		slog.Warn("file proposal: load schedule", "run_id", run.ID, "schedule_id", run.ScheduleID.Bytes, "error", err)
		return
	}

	// Resolve output mode via the shared resolver (PRD #929 M3) so this filing side and the
	// fire side (schedsvc) interpret a NULL output_mode identically — a NULL row resolves to
	// the CATALOG default, not a hardcoded "mr", so a catalog that ships `output: issues`
	// files here too. Only issues-mode prompt schedules file a proposal; a payload on a
	// non-issues run is defense-in-depth filed nothing (M4 only makes issues-mode agents
	// emit one).
	mode := schedtmpl.ResolveOutputMode(sched.OutputMode.String, sched.OutputMode.Valid, catalogSlugString(sched))
	if mode != "issues" {
		return
	}
	if sched.Target != "prompt" {
		return
	}

	// The label set is EXACTLY one constant-built marker (D3, via ProposalLabel — the SAME
	// helper schedsvc queries with for the dedup digest). Never `uzi`, never a sweep
	// selector, never a bot assignment, never anything from model output.
	label := ProposalLabel(catalogSlugString(sched))
	labels := []string{label}

	repo, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: run.RepoID.Bytes, UserID: run.UserID})
	if err != nil {
		slog.Warn("file proposal: load repo", "run_id", run.ID, "error", err)
		return
	}
	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Warn("file proposal: build forge", "run_id", run.ID, "error", err)
		return
	}

	// Audit row first (pending), then the forge write, then stamp confirmed with the
	// iid — mirroring the issue_proposals shape ConfirmProposalForUser uses (PRD #929
	// reuses that store shape). A failure to insert the pending row before the issue
	// exists is fatal to filing (log+return); a failure to stamp AFTER the issue was
	// created only logs — the forge issue already exists and must not be re-filed.
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		slog.Warn("file proposal: marshal labels", "run_id", run.ID, "error", err)
		return
	}
	prop, err := s.q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{
		RunID:       run.ID,
		RepoID:      run.RepoID.Bytes,
		Title:       proposal.Title,
		Description: proposal.Body,
		Labels:      labelsJSON,
	})
	if err != nil {
		slog.Warn("file proposal: create audit row", "run_id", run.ID, "error", err)
		return
	}

	created, err := f.CreateIssue(ctx, repo.ForgeProjectID, proposal.Title, proposal.Body, labels)
	if err != nil {
		// err is already PAT-redacted by the driver. The pending audit row stays for
		// diagnosis; nothing was filed.
		slog.Warn("file proposal: create issue", "run_id", run.ID, "proposal", prop.ID, "error", err)
		return
	}

	if _, err := s.q.StampFiledProposal(ctx, store.StampFiledProposalParams{
		ID:  prop.ID,
		Iid: pgtype.Int8{Int64: created.IID, Valid: true},
	}); err != nil {
		// The issue exists; only the audit stamp failed. Log and move on — never error.
		slog.Warn("file proposal: stamp confirmed", "run_id", run.ID, "proposal", prop.ID, "issue_iid", created.IID, "error", err)
	}
	slog.Info("filed scheduled proposal as forge issue",
		"run_id", run.ID, "schedule_id", sched.ID, "issue_iid", created.IID, "label", label)
}
