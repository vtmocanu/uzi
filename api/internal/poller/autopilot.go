package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// RunStarter creates an autopilot run through workersvc's shared manual-start
// path. *workersvc.Service satisfies it. Keeping run creation on the workersvc
// side (rather than re-implementing it here) is what makes an autopilot run and a
// manual run share one state machine and one set of gates.
type RunStarter interface {
	CreateAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string) (store.Run, error)
}

// AutopilotLabeler resolves the configured autopilot label. *settings.Cache
// satisfies it; a nil labeler or an empty/errored read falls back to the
// compiled-in default, so a settings blip degrades to the default label rather
// than filtering on an empty one.
type AutopilotLabeler interface {
	AutopilotLabel(ctx context.Context) (string, error)
}

// autopilotStore is the subset of *store.Queries the detector reads and writes.
type autopilotStore interface {
	ListAutopilotCandidateIssues(ctx context.Context, arg store.ListAutopilotCandidateIssuesParams) ([]store.ListAutopilotCandidateIssuesRow, error)
	GetAutopilotConnectionContext(ctx context.Context, connectionID uuid.UUID) (store.GetAutopilotConnectionContextRow, error)
	GetAutopilotTrigger(ctx context.Context, arg store.GetAutopilotTriggerParams) (store.AutopilotTrigger, error)
	UpsertAutopilotTrigger(ctx context.Context, arg store.UpsertAutopilotTriggerParams) error
	HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error)
}

// Autopilot is the poller's post-sync autopilot detector (PRD #19 M4). It is
// stateless and safe for concurrent use across the per-repo sync goroutines: it
// only reads its injected collaborators and touches distinct (repo, issue) rows.
type Autopilot struct {
	q      autopilotStore
	runs   RunStarter
	labels AutopilotLabeler
}

// NewAutopilot builds a detector. q is the store, runs creates the auto_approve
// runs (workersvc), labels resolves the configured autopilot label (settings).
func NewAutopilot(q autopilotStore, runs RunStarter, labels AutopilotLabeler) *Autopilot {
	return &Autopilot{q: q, runs: runs, labels: labels}
}

// detect is the post-sync autopilot hook (review finding B3: detection lives HERE,
// never in forgesvc, whose sync methods are shared with the handlers' CreateIssue
// and manual Refresh and must never spawn runs). It runs after a repo's issue
// cache is fresh, a sibling of the MR-close watcher. For each open
// autopilot-labelled cached issue it decides, once per label application, whether
// to start an auto_approve run for the mapped consenting user or to post one
// explanatory comment, and records the handled label-event id so neither ever
// repeats — even across a FullSync cache eviction.
//
// Every per-issue failure is log-and-skipped (the poller convention): a forge blip
// or a DB error on one issue must not stall the tick or the other issues, and an
// unrecorded event id simply re-evaluates the issue next tick.
func (a *Autopilot) detect(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge) {
	label := a.autopilotLabel(ctx)

	issues, err := a.q.ListAutopilotCandidateIssues(ctx, store.ListAutopilotCandidateIssuesParams{
		RepoID: r.ID,
		Label:  label,
	})
	if err != nil {
		slog.Error("poller: list autopilot candidates", "repo", r.PathWithNamespace, "error", err)
		return
	}
	if len(issues) == 0 {
		return
	}

	// The repo's owner is the only user who can satisfy the "repo connected by that
	// user" consent gate (Decision 4), so the attribution collapses to this one
	// human_username. Fetched once per repo, reused for every candidate.
	cc, err := a.q.GetAutopilotConnectionContext(ctx, r.ConnectionID)
	if err != nil {
		slog.Error("poller: autopilot connection context", "repo", r.PathWithNamespace, "error", err)
		return
	}

	for _, iss := range issues {
		if ctx.Err() != nil {
			return
		}
		a.detectOne(ctx, r, f, label, cc, iss)
	}
}

// detectOne resolves the current autopilot-label application for one issue and, if
// it is a not-yet-handled transition, hands it to handle.
func (a *Autopilot) detectOne(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge, label string, cc store.GetAutopilotConnectionContextRow, iss store.ListAutopilotCandidateIssuesRow) {
	iid := iss.ForgeIssueIid

	events, err := f.ListIssueLabelEvents(ctx, r.ForgeProjectID, iid)
	if err != nil {
		// Already PAT-redacted by the driver. Leave the issue for the next tick.
		slog.Warn("poller: autopilot label events", "repo", r.PathWithNamespace, "issue", iid, "error", err)
		return
	}
	add := lastLabelAdd(events, label)
	if add == nil {
		// The cache still lists the label but the events' latest state for it is a
		// removal (a sync race between the label list and the events): not an
		// application, nothing to trigger. It self-heals when the next sync drops the
		// label from the cache.
		return
	}

	trig, err := a.q.GetAutopilotTrigger(ctx, store.GetAutopilotTriggerParams{RepoID: r.ID, IssueIid: iid})
	switch {
	case err == nil:
		if trig.LastEventID >= add.ID {
			return // transition-once: this application was already handled (Decision 5)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// Never handled: a fresh application.
	default:
		slog.Error("poller: get autopilot trigger", "repo", r.PathWithNamespace, "issue", iid, "error", err)
		return
	}

	a.handle(ctx, r, f, label, cc, iss, add.ID, add.Username)
}

// handle decides the outcome for a fresh label application: swallow (active run),
// start a run (eligible), or record-then-comment (not eligible / no PRD link / too
// large). eventID is the current application's label-event id; adder is who added
// the label.
func (a *Autopilot) handle(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge, label string, cc store.GetAutopilotConnectionContextRow, iss store.ListAutopilotCandidateIssuesRow, eventID int64, adder string) {
	iid := iss.ForgeIssueIid

	// Decision 5: a (re-)application while a run is active is swallowed — no comment,
	// no queued re-run. Consume the event id so it never re-triggers once that run is
	// terminal (retry is a deliberate remove+re-add, which mints a new event id).
	// Checked before eligibility so an active manual run never draws a spurious
	// autopilot comment.
	active, err := a.q.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{RepoID: r.ID, IssueIid: pgtype.Int8{Int64: iid, Valid: true}})
	if err != nil {
		slog.Error("poller: autopilot active-run check", "repo", r.PathWithNamespace, "issue", iid, "error", err)
		return
	}
	if active {
		a.record(ctx, r, iid, eventID)
		return
	}

	// Attribution (Decision 3): the label adder, then the issue author, matched
	// against the repo owner's declared human_username. Consent (Decision 4): the
	// owner opted in and has an Anthropic token. Any miss → record-then-comment.
	author := ""
	if iss.Author.Valid {
		author = iss.Author.String
	}
	if !eligible(cc, adder, author) {
		a.recordThenComment(ctx, r, f, iid, eventID, noEligibleUserComment(label))
		return
	}

	// Eligible: create the run through the SAME path as a manual start (ownership,
	// cached-PRD-issue, PRD-link and one-active-run gates all enforced there), only
	// with auto_approve set. The description is the fresh forge copy, snapshotted onto
	// the run exactly as the manual start path snapshots the description it is given.
	issue, err := f.GetIssue(ctx, r.ForgeProjectID, iid)
	if err != nil {
		// Transient forge error: leave the event unrecorded so the next tick retries.
		slog.Warn("poller: autopilot fetch issue", "repo", r.PathWithNamespace, "issue", iid, "error", err)
		return
	}

	_, err = a.runs.CreateAutopilotRun(ctx, cc.UserID, r.ID, iid, issue.Description)
	switch {
	case err == nil:
		// Create-then-record: a crash before recording leaves the created run active,
		// so the next tick's active-run check swallows the re-detection (no double run)
		// and records the event id then.
		a.record(ctx, r, iid, eventID)
	case errors.Is(err, workersvc.ErrDescriptionTooLarge):
		// The shared createRun cap (unified in M5): the description is too large to
		// snapshot onto an unattended run — explain it instead of running.
		a.recordThenComment(ctx, r, f, iid, eventID, tooLargeComment())
	case errors.Is(err, workersvc.ErrNoPRDLink):
		// The same gate the manual path enforces: an autopilot issue with no prds/*.md
		// link never runs; it gets the explanatory comment instead (PRD invariant).
		a.recordThenComment(ctx, r, f, iid, eventID, noPRDLinkComment(label))
	case errors.Is(err, workersvc.ErrActiveRunExists):
		// A run appeared between the pre-check and here: swallow, same as an active run.
		a.record(ctx, r, iid, eventID)
	default:
		// ErrRepoNotFound/ErrIssueNotFound should not happen (the owner owns the repo
		// and the issue is cached) and a transient DB error should retry: leave the
		// event unrecorded so the next tick re-evaluates.
		slog.Error("poller: autopilot create run", "repo", r.PathWithNamespace, "issue", iid, "error", err)
	}
}

// record persists the handled label-event id (transition-once + never-re-comment
// dedup). Best-effort: a write failure only means the issue is re-evaluated next
// tick, and the active-run / event-id guards make that re-evaluation a no-op.
func (a *Autopilot) record(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, iid, eventID int64) error {
	err := a.q.UpsertAutopilotTrigger(ctx, store.UpsertAutopilotTriggerParams{
		RepoID:      r.ID,
		IssueIid:    iid,
		LastEventID: eventID,
	})
	if err != nil {
		slog.Error("poller: record autopilot trigger", "repo", r.PathWithNamespace, "issue", iid, "error", err)
	}
	return err
}

// recordThenComment records the handled event id FIRST, then posts one explanatory
// comment (Decision 6): a crash between the two loses one comment rather than ever
// double-posting on a later tick. If the dedup record does not persist, no comment
// is posted (a comment without its record could double-post next tick).
func (a *Autopilot) recordThenComment(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge, iid, eventID int64, body string) {
	if a.record(ctx, r, iid, eventID) != nil {
		return
	}
	if _, err := f.CreateIssueNote(ctx, r.ForgeProjectID, iid, body); err != nil {
		// Already PAT-redacted by the driver. The event id is recorded, so this comment
		// is simply lost (never retried) — the accepted record-then-comment trade.
		slog.Warn("poller: autopilot comment", "repo", r.PathWithNamespace, "issue", iid, "error", err)
	}
}

// autopilotLabel resolves the configured autopilot label, falling back to the
// compiled-in default when unconfigured or on a settings read error (mirrors
// forgesvc.prdLabel — the accessor already returns the default alongside a cold
// error, so this is best-effort by design).
func (a *Autopilot) autopilotLabel(ctx context.Context) string {
	if a.labels != nil {
		if l, _ := a.labels.AutopilotLabel(ctx); l != "" {
			return l
		}
	}
	return settings.DefaultAutopilotLabel
}

// eligible reports whether the repo owner may run this issue under autopilot: they
// opted in, they have an Anthropic token, and their declared human_username is the
// label adder or (fallback) the issue author. The adder is preferred per Decision 3,
// but since the "repo connected by that user" gate admits only the repo owner, the
// resolved user is the owner either way — so a plain match against the owner's
// username on adder-then-author is exactly the ordered resolution.
func eligible(cc store.GetAutopilotConnectionContextRow, adder, author string) bool {
	if !cc.AutopilotEnabled || !cc.HasAnthropicToken {
		return false
	}
	if !cc.HumanUsername.Valid || cc.HumanUsername.String == "" {
		return false
	}
	uname := cc.HumanUsername.String
	if uname == adder {
		return true
	}
	return author != "" && uname == author
}

// lastLabelAdd returns the most recent event that ADDS label, or nil if the latest
// event touching label is a removal (or there is none). GitLab returns resource
// label events oldest-first with globally-monotonic ids, so the last matching entry
// is the label's current state and its id orders "which application" for dedup.
func lastLabelAdd(events []forge.LabelEvent, label string) *forge.LabelEvent {
	var last *forge.LabelEvent
	for i := range events {
		if events[i].LabelName == label {
			last = &events[i]
		}
	}
	if last == nil || last.Action != "add" {
		return nil
	}
	return last
}

// Comment bodies. These are posted to the forge (user-facing), so they avoid em
// dashes and spell out the retry gesture (remove + re-add the label, Decision 5).

func noEligibleUserComment(label string) string {
	return fmt.Sprintf(
		"**Autopilot did not start a run.**\n\n"+
			"No uzi user with autopilot enabled is mapped to the person who added the `%s` label or to the issue author. "+
			"To run this issue with autopilot, connect your forge in uzi, set your forge username on that connection, and enable autopilot in your settings, then remove and re-add the `%s` label.",
		label, label)
}

func noPRDLinkComment(label string) string {
	return fmt.Sprintf(
		"**Autopilot did not start a run.**\n\n"+
			"This issue has no PRD link. Add a link to a `prds/*.md` file in the description, then remove and re-add the `%s` label to retry.",
		label)
}

func tooLargeComment() string {
	return "**Autopilot did not start a run.**\n\n" +
		"The issue description is too large to run. Shorten it, then remove and re-add the label to retry."
}
