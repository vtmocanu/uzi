package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// FileFinding files a forge issue from one incidental finding (PRD #333 M5, D4/D5). It is the
// human-gated forge write: mounted on RequireUser (CLI-reachable via Bearer; CSRF preserved
// on the browser cookie path) behind forgeLimiter.PerUserMiddleware, mirroring FileIssue.
// Authorization is owner-scoped in one
// step — GetIncidentalFinding((id, user.ID)) gives the coordinate (user_id, repo_id, location)
// AND proves ownership (a foreign/unknown id is a 404, no existence oracle) — plus
// caller-owns-repo (GetRepoForUser) to WRITE.
//
// Text is resolved from the STORED, already-sanitised finding row by default (D4); the request
// body's title/description are accepted only as edits and are re-run through the same
// write-boundary sanitisers, never trusted to have preserved their inertness. Labels are
// assembled server-side (D5): the config-overridable marker (settings.FindingLabel) is unioned
// with the user's sanitised, capped selection, marker always present.
//
// Ordering is claim-first (D4): ClaimFindingForFiling (open→filing, a guarded UPDATE) so of two
// concurrent POSTs on one coordinate exactly one reaches the forge and the loser is a 409; then
// EnsureLabels the marker BEFORE CreateIssue (D5/R5 — Forgejo resolves label ids and errors on
// an unknown name); then CreateIssue on the caller's own connection; then SettleFindingFiled.
// Any post-claim, pre-CreateIssue failure REVERTS the claim (filing→open, re-fileable). A settle
// failure AFTER a successful CreateIssue is created-with-warning: the forge issue is real, so we
// NEVER revert (would orphan it) and NEVER retry (mirrors settleFiledIssue).
//
// Note the transient `filing` status window (M4 carry-over, deliberate): between the claim and
// the settle/revert the coordinate is neither `open` nor `filed`, so it is invisible to the
// to_file bucket and the open_count badge for that brief window and reappears as `open` on
// revert or moves to `filed` on settle. M7's web will render a "filing…" state for it.
func (h *Handler) FileFinding(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	findingID, ok := httpx.PathUUID(w, r, "id", "finding")
	if !ok {
		return
	}
	var req apitypes.FileFindingRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx := r.Context()

	// (1) Owner-scoped load: the (id, user_id) match IS the ownership check. A foreign or
	// unknown id is a 404 — the same not-found the draft read returns, no existence oracle.
	// This row is the source of the coordinate AND the default filed text (D4).
	finding, err := h.q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: findingID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "finding not found")
			return
		}
		slog.Error("file finding: get finding", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// (2) Caller-owns-repo (write side): GetRepoForUser is user_id-scoped, so a repo the
	// caller does not own/connect is a 404 — a plain user cannot file against someone else's
	// repo, and it also yields the forge connection to build the driver.
	repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: finding.RepoID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("file finding: load repo", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// (3) Resolve the filed title/description (D4). The default is the deterministic template
	// over the STORED row (agent text never rides in raw — RenderFinding routes each field
	// through the matching sanitiser). A body field is a user EDIT: re-run it through the same
	// write-boundary sanitiser, never trust it to be inert. The draft is built by the SAME
	// buildFindingDraft the M4 preview (GetFindingIssueDraft) uses, so what the user previewed
	// is byte-identical to what an omitted body files.
	draft := h.buildFindingDraft(ctx, finding)

	title := draft.Title
	if req.Title != nil {
		title = issuedraft.SanitizeTitle(*req.Title)
	}
	if title == "" {
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty")
		return
	}

	description := draft.Description
	if req.Description != nil {
		if len(*req.Description) > workersvc.MaxIssueDescriptionBytes {
			httpx.Error(w, http.StatusBadRequest, "description is too large")
			return
		}
		description = issuedraft.SanitizeFiledBody(*req.Description)
	}

	// (4) Labels are assembled server-side (D5): the config-overridable marker unioned with the
	// user's sanitised, capped selection, marker ALWAYS first and present. The API gives the
	// client no way to attach a trigger label that skips the marker.
	//
	// LABEL POSTURE (auditor question, confirmed): the ONLY inputs are the server marker and the
	// human's req.Labels. The agent-authored suggestions stored on finding.Labels are NOT read
	// here, so they never auto-attach — they are surfaced only as an editable seed in the M4
	// draft (GetFindingIssueDraft), and attach solely when the repo owner explicitly keeps them
	// in the body. A user-chosen run-eligible label is therefore the owner's own deliberate
	// action, not an agent-injected one.
	marker, _ := h.settings.FindingLabel(ctx)
	filedLabels := assembleFindingLabels(marker, req.Labels)

	// (5) Claim-first (D4): a guarded open→filing UPDATE. 0 rows affected means the coordinate
	// is already filed or being filed (the single row is not `open`), so exactly one of two
	// concurrent POSTs wins and the loser is a 409 — never a duplicate forge issue.
	rows, err := h.q.ClaimFindingForFiling(ctx, store.ClaimFindingForFilingParams{
		UserID:   finding.UserID,
		RepoID:   finding.RepoID,
		Location: finding.Location,
	})
	if err != nil {
		slog.Error("file finding: claim", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if rows == 0 {
		httpx.Error(w, http.StatusConflict, "this finding is already filed or being filed")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		h.revertFindingClaim(ctx, finding)
		slog.Error("file finding: build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// EnsureLabels the marker BEFORE CreateIssue (D5/R5): Forgejo resolves label names to ids
	// and errors on an unknown one, so the marker must pre-exist. A failure here reverts the
	// claim and is a 502 (an upstream forge problem), not a duplicate issue.
	if err := f.EnsureLabels(ctx, repo.ForgeProjectID, []forge.Label{{Name: marker}}); err != nil {
		h.revertFindingClaim(ctx, finding)
		httpx.Error(w, http.StatusBadGateway, "could not ensure the finding label on the forge: "+err.Error())
		return
	}

	created, err := f.CreateIssue(ctx, repo.ForgeProjectID, title, description, filedLabels)
	if err != nil {
		// The forge rejected the write: the claim reverts, nothing is persisted, the finding
		// stays open and re-fileable. err is already PAT-redacted by the driver.
		h.revertFindingClaim(ctx, finding)
		httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
		return
	}

	// Forge-first done. Settle the coordinate (filing→filed, stamp the iid). A settle failure or
	// 0-rows AFTER a successful CreateIssue is created-with-warning — the real issue exists, so
	// NEVER revert (would orphan it) and NEVER retry (mirrors settleFiledIssue).
	warning := h.settleFiledFinding(ctx, finding, created.IID, created.WebURL)

	// The response is the exported apitypes.IncidentalFindingFileResultDTO (PRD #1183 M3 folds the
	// handler-local fileFindingResponse twin into it): wire-identical to what shipped before.
	httpx.JSON(w, http.StatusCreated, apitypes.IncidentalFindingFileResultDTO{
		Issue:   apitypes.IncidentalFindingFiledIssueDTO{IID: created.IID, WebURL: created.WebURL, Title: created.Title},
		Warning: warning,
	})
}

// buildFindingDraft builds the deterministic finding issue draft (D4) over the STORED row. It is
// the ONE shared draft path used by BOTH the M4 preview (GetFindingIssueDraft) and the M5 default
// filed text (FileFinding), so what the user previews is byte-identical to what an omitted body
// files. Everything is resolved OWNER-SCOPED by finding.UserID (which is the authenticated user at
// both call sites — the finding was loaded via the (id, user_id) owner-scoped GetIncidentalFinding):
// the provenance footer's run kind / issue iid, and the repo path. Both lookups are display-only —
// a miss just drops the corresponding line, never fails the draft. A body edit replaces
// title/description before either is written.
func (h *Handler) buildFindingDraft(ctx context.Context, finding store.IncidentalFinding) issuedraft.FindingDraft {
	var runKind string
	var issueIID int64
	if run, rerr := h.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: finding.RunID, UserID: finding.UserID}); rerr == nil {
		runKind = run.Kind
		if run.IssueIid.Valid {
			issueIID = run.IssueIid.Int64
		}
	}
	repoPath := ""
	if repo, rerr := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: finding.RepoID, UserID: finding.UserID}); rerr == nil {
		repoPath = repo.PathWithNamespace
	}
	return issuedraft.RenderFinding(issuedraft.FindingDraftInput{
		Title:       finding.Title,
		Description: finding.DescriptionMd,
		Location:    finding.Location,
		RepoPath:    repoPath,
		RunShortID:  shortID(finding.RunID),
		RunKind:     runKind,
		IssueIID:    issueIID,
	})
}

// revertFindingClaim best-effort reverts a claim (filing→open) after a post-claim, pre-CreateIssue
// failure so the coordinate is fileable again. A revert error is logged but never changes the
// response the user already gets for the underlying failure — the guarded UPDATE only touches a
// `filing` row, so it can never disturb a concurrently-settled `filed` one.
func (h *Handler) revertFindingClaim(ctx context.Context, finding store.IncidentalFinding) {
	if _, err := h.q.RevertFindingFiling(ctx, store.RevertFindingFilingParams{
		UserID:   finding.UserID,
		RepoID:   finding.RepoID,
		Location: finding.Location,
	}); err != nil {
		slog.Error("file finding: revert claim after failure", "location", finding.Location, "error", err)
	}
}

// settleFiledFinding closes a won claim with the created issue iid (filing→filed). It runs only
// AFTER a successful CreateIssue, so a non-empty return is created-with-warning: the forge issue
// is real, and the caller NEVER reverts or retries on it. Empty on a clean settle; a warning when
// the settle errored or matched 0 rows.
//
// On a settle failure the coordinate is LEFT `filing` (never reverted — that would orphan the
// real forge issue). It does NOT self-heal on the next report: ReopenDispositionOnHashMismatch
// guards status IN ('filed','dismissed'), so a re-report of a `filing` coordinate is a no-op. The
// stranded-filing sweeper (main.go `finding_filing_claims_stranded`, clamped by
// IssueFilingStuckTimeout to >= 2x ForgeHTTPTimeout) is what recovers it: on a later tick it
// resets the coordinate to `open` so the user can re-file — accepting the rare duplicate that a
// re-file of an already-created issue produces (documented on SweepStrandedFilingFindings).
func (h *Handler) settleFiledFinding(ctx context.Context, finding store.IncidentalFinding, iid int64, webURL string) string {
	const warnUnlinked = "The issue was created on the forge, but recording it in uzi failed; the finding may still show as unfiled until it reconciles."
	rows, err := h.q.SettleFindingFiled(ctx, store.SettleFindingFiledParams{
		FiledIssueIid: pgtype.Int8{Int64: iid, Valid: true},
		FiledIssueUrl: webURL,
		UserID:        finding.UserID,
		RepoID:        finding.RepoID,
		Location:      finding.Location,
	})
	if err != nil {
		slog.Warn("file finding: settle", "location", finding.Location, "error", err)
		return warnUnlinked
	}
	if rows == 0 {
		return warnUnlinked
	}
	return ""
}

// assembleFindingLabels builds the filed label set (D5): the server marker FIRST and always
// present, unioned with the user's sanitised, capped selection, deduplicated. Each user label is
// rendered inert (control/bidi strip + 64-byte bound) and dropped when it is empty after
// sanitising, a comma (the forge label-list separator, matching settings.ValidateLabel), or a
// duplicate. The count is capped at MaxProposalLabels so a runaway body cannot balloon the set.
// The marker is never dropped and never de-duplicated away, so it rides on every filed issue.
func assembleFindingLabels(marker string, userLabels []string) []string {
	out := make([]string, 0, len(userLabels)+1)
	seen := make(map[string]struct{}, len(userLabels)+1)
	if marker != "" {
		out = append(out, marker)
		seen[marker] = struct{}{}
	}
	for _, raw := range userLabels {
		if len(out) >= workersvc.MaxProposalLabels {
			break
		}
		clean := termsafe.SanitizeBounded(raw, 64)
		if clean == "" || containsComma(clean) {
			continue
		}
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func containsComma(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return true
		}
	}
	return false
}

// findingDismissReasons is the closed set of dismissal reasons (mirrors the judge's
// recommendation_dispositions reasons and the finding_dispositions CHECK).
var findingDismissReasons = map[string]struct{}{
	"wont_do":      {},
	"not_an_issue": {},
}

// DismissFinding triages one finding coordinate to `dismissed` with a reason (PRD #333 M5). It is
// a LOCAL write — no forge call, no token spend — so it mounts on RequireUser WITHOUT the forge
// limiter (a local write; CLI-reachable via Bearer, CSRF preserved on the cookie path), beside
// the M4 reads. Owner-scoped via GetIncidentalFinding (a foreign/unknown id is a
// 404). The DismissFinding query is guarded to status='open' (the to_file bucket only shows open
// rows): a 0-row result on a filed/filing coordinate is a 409 — the user reverts/handles the
// filing first — and dismiss-from-open-only keeps a dismissed bug from silently reopening a
// forge-linked one.
func (h *Handler) DismissFinding(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	findingID, ok := httpx.PathUUID(w, r, "id", "finding")
	if !ok {
		return
	}
	var req apitypes.DismissFindingRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := findingDismissReasons[req.Reason]; !ok {
		httpx.Error(w, http.StatusBadRequest, "reason must be wont_do or not_an_issue")
		return
	}
	ctx := r.Context()

	// Owner-scoped load: (id, user_id) match is the ownership check → 404 on foreign/unknown.
	finding, err := h.q.GetIncidentalFinding(ctx, store.GetIncidentalFindingParams{ID: findingID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "finding not found")
			return
		}
		slog.Error("dismiss finding: get finding", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	rows, err := h.q.DismissFinding(ctx, store.DismissFindingParams{
		DismissReason: pgtype.Text{String: req.Reason, Valid: true},
		UserID:        finding.UserID,
		RepoID:        finding.RepoID,
		Location:      finding.Location,
	})
	if err != nil {
		slog.Error("dismiss finding: dismiss", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if rows == 0 {
		// The guard is status='open', so a 0-row result is any non-open state: filed, mid-filing,
		// or already dismissed. All three are named so the message is accurate for each.
		httpx.Error(w, http.StatusConflict, "cannot dismiss (already filed, being filed, or already dismissed)")
		return
	}

	httpx.JSON(w, http.StatusOK, apitypes.DismissFindingResultDTO{Status: "dismissed", Reason: req.Reason})
}

// bulkDismissMaxIDs bounds one BulkDismissFindings request. Over it the handler 400s rather than
// letting an unbounded id list drive one statement — the same "bound them where they are written"
// rule the judge fan-out and the close-edge scan follow.
const bulkDismissMaxIDs = 100

// BulkDismissFindings triages up to bulkDismissMaxIDs owned OPEN coordinates to `dismissed` with one
// shared reason in a SINGLE statement (PRD #1183 M3). It is a LOCAL write — no forge call, no token
// spend — mounted on RequireUser WITHOUT the forge limiter beside DismissFinding. Owner-scoped by the
// query's user_id filter and guarded to status='open', so a foreign id or a non-open coordinate is
// SKIPPED SILENTLY (the judge fan-out's "settle what you can" shape): Updated reports how many rows
// actually moved, which can be less than len(ids). The 100-id cap is a 400 above the limit; an
// unparseable id is a 400. Returns {updated, findings} with the re-read rows.
func (h *Handler) BulkDismissFindings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req apitypes.BulkDismissFindingsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := findingDismissReasons[req.Reason]; !ok {
		httpx.Error(w, http.StatusBadRequest, "reason must be wont_do or not_an_issue")
		return
	}
	if len(req.IDs) > bulkDismissMaxIDs {
		httpx.Error(w, http.StatusBadRequest, "too many ids (max 100)")
		return
	}
	ids := make([]uuid.UUID, 0, len(req.IDs))
	for _, raw := range req.IDs {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid finding id")
			return
		}
		ids = append(ids, parsed)
	}

	rows, err := h.q.BulkDismissFindings(r.Context(), store.BulkDismissFindingsParams{
		Reason: pgtype.Text{String: req.Reason, Valid: true},
		UserID: user.ID,
		Ids:    ids,
	})
	if err != nil {
		slog.Error("bulk dismiss findings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := apitypes.BulkDismissFindingsResultDTO{
		Updated:  len(rows),
		Findings: make([]apitypes.IncidentalFindingDTO, 0, len(rows)),
	}
	for _, row := range rows {
		out.Findings = append(out.Findings, findingDispositionDTO(row))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// UndoDismissFinding reopens a dismissed coordinate (dismissed → open, PRD #1183 M3): DELETE
// /api/findings/{id}/dismiss, keyed on the disposition id (a dismissed coordinate may carry no
// evidence row to key by, which is why this and BulkDismissFindings key on the disposition id, not
// the evidence id the file/single-dismiss POSTs use). Owner-scoped and guarded to status='dismissed'
// by the query, so an undo of a non-dismissed or foreign coordinate matches zero rows → 404. On
// success it returns the reopened coordinate so the client can reconcile the row in place. A LOCAL
// write: RequireUser, no forge limiter.
func (h *Handler) UndoDismissFinding(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dispositionID, ok := httpx.PathUUID(w, r, "id", "finding")
	if !ok {
		return
	}

	row, err := h.q.UndoDismissFinding(r.Context(), store.UndoDismissFindingParams{
		UserID: user.ID,
		ID:     dispositionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No owned dismissed coordinate at this id — an unknown/foreign id or a coordinate that
			// is not currently dismissed. Both are a 404, leaking no existence oracle.
			httpx.Error(w, http.StatusNotFound, "no dismissed finding to undo")
			return
		}
		slog.Error("undo dismiss finding", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, findingDispositionDTO(row))
}

// findingDispositionDTO projects a re-read finding_dispositions row onto the wire DTO for the
// bulk-dismiss and undo responses. Unlike the backlog mapper it has no repo_path, seen_in_runs,
// evidence_preview or occurrences to carry (a raw disposition row holds none of them) — the client
// reconciles by disposition_id + status. FindingID stays nil for the same reason.
func findingDispositionDTO(d store.FindingDisposition) apitypes.IncidentalFindingDTO {
	dto := apitypes.IncidentalFindingDTO{
		DispositionID: d.ID.String(),
		Location:      d.Location,
		RepoID:        d.RepoID.String(),
		Status:        d.Status,
		LastTitle:     d.LastTitle,
		FiledIssueURL: d.FiledIssueUrl,
	}
	if d.DismissReason.Valid {
		dto.DismissReason = d.DismissReason.String
	}
	if d.SetVia.Valid {
		dto.SetVia = d.SetVia.String
	}
	if d.FiledIssueIid.Valid {
		iid := d.FiledIssueIid.Int64
		dto.FiledIssueIID = &iid
	}
	if d.ResolvedAt.Valid {
		ts := d.ResolvedAt.Time
		dto.ResolvedAt = &ts
	}
	return dto
}
