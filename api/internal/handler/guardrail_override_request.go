package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// validateGuardrailReason is the SHARED reason validator for every #66 guardrail
// override write — the admin per-repo override (SetRepoGuardrailOverride), the member
// "request an override" (RequestGuardrailOverride), and the admin approve/reject
// decision note (M3). Extracting it into one helper is what keeps those paths from
// drifting on what a legal reason is.
//
// It trims, then rejects an empty reason (400), an over-long one (422 —
// maxGuardrailOverrideReasonBytes), or one carrying any unsafe character (400). The
// reason is an audit note M9 renders in a badge, the admin blocked-repos list, the
// admin request queue (a CROSS-USER surface a member's text reaches), and potentially a
// plain-text CLI/log surface, so both C0/C1 control characters (newline, CR, tab, ANSI
// ESC — the Cc set unicode.IsControl covers) AND Unicode format characters (the Cf set:
// bidi overrides U+202A-202E, isolates U+2066-2069, the zero-widths, the BOM) are
// rejected write-side via termsafe.Unsafe. IsControl alone never sees U+202E, so on its
// own it would let a member Trojan-Source-spoof the reason the approving admin reads and
// weighs — the Cf half is the one that matters most here. One check write-side instead
// of trusting every future renderer to escape (PRD #66 M8 audit hardening; issue #1432
// extends it to the member-supplied path). On success it returns the trimmed reason with
// status==0; on rejection it returns the HTTP status and the message the caller emits.
func validateGuardrailReason(raw string) (clean string, status int, msg string) {
	clean = strings.TrimSpace(raw)
	if clean == "" {
		return "", http.StatusBadRequest, "a non-empty reason is required to override the guardrail"
	}
	if status, msg := screenGuardrailText(clean, "reason"); status != 0 {
		return "", status, msg
	}
	return clean, 0, ""
}

// screenGuardrailText is the SHARED length + character screen for every guardrail
// free-text field (the override reason and the admin decision note). It rejects an
// over-long value (422 — maxGuardrailOverrideReasonBytes) and one carrying any unsafe
// C0/C1 control or Unicode-format character (400, via termsafe.Unsafe). It does NOT
// check emptiness: the reason requires a non-empty value while the decision note is
// optional, so each caller owns that policy. label names the field in the message
// ("reason" / "decision note") so one screen serves both without duplicating the
// termsafe/length logic — the reason this exists rather than a copy in each validator.
// Returns status==0 on success, else the HTTP status and message the caller emits.
func screenGuardrailText(clean, label string) (status int, msg string) {
	if len(clean) > maxGuardrailOverrideReasonBytes {
		return http.StatusUnprocessableEntity, label + " is too long"
	}
	for _, ru := range clean {
		if termsafe.Unsafe(ru) {
			return http.StatusBadRequest, label + " must not contain control or formatting characters"
		}
	}
	return 0, ""
}

// validateOptionalGuardrailNote screens the OPTIONAL admin decision note (PRD #1432
// M3). It trims; an empty/absent note is legal and returns an invalid pgtype.Text (no
// note persisted). A NON-empty note gets the SAME length + unsafe-character screen as
// the override reason (screenGuardrailText) — the note is surfaced back to the member
// on the Repos page (OverrideRequestStateDTO.DecisionNote, attached in ListProjects for
// a not-yet-enabled repo), a cross-user surface where admin-authored text is shown to
// the requester, so it MUST get the same termsafe treatment as any other free text that
// crosses that boundary. (The decision notification itself carries only a fixed
// title/body, not the note.) Returns status==0 on success, else the HTTP status and
// message the caller emits.
func validateOptionalGuardrailNote(raw string) (note pgtype.Text, status int, msg string) {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return pgtype.Text{Valid: false}, 0, ""
	}
	if status, msg := screenGuardrailText(clean, "decision note"); status != 0 {
		return pgtype.Text{}, status, msg
	}
	return pgtype.Text{String: clean, Valid: true}, 0, ""
}

// overrideRequestStateDTO maps a guardrail_override_requests row to the member-facing
// OverrideRequestStateDTO (PRD #1432). Shared by RequestGuardrailOverride's response and
// ListProjects' per-repo enrichment so the two never drift on how a stored row becomes
// the state the owner sees. DecidedAt / DecisionNote stay nil while the request is still
// pending — the pgtype .Valid discriminator is the only source, never a zero value.
func overrideRequestStateDTO(row store.GuardrailOverrideRequest) apitypes.OverrideRequestStateDTO {
	dto := apitypes.OverrideRequestStateDTO{
		Status:    row.Status,
		Reason:    row.Reason,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.DecidedAt.Valid {
		t := row.DecidedAt.Time
		dto.DecidedAt = &t
	}
	if row.DecisionNote.Valid {
		s := row.DecisionNote.String
		dto.DecisionNote = &s
	}
	return dto
}

// RequestGuardrailOverride is the member "ask an admin to allow this blocked repo
// through the #66 guardrail" write (PRD #1432). It is OWNER-SCOPED and cookie-only
// (mounted in the RequireAuth repos group, mirroring SetRepoEnabled's posture): a member
// who could not enable a repo because the live guard refused it records a request an
// admin later approves or rejects (M3). It NEVER sets an override and NEVER enables the
// repo — only the admin write (SetRepoGuardrailOverride) and enable (SetRepoEnabled) do.
//
// It re-runs the LIVE guard with Overridden:false (never the stored report, D2), so the
// persisted findings snapshot is current and waivability is enforced server-side, not
// trusted from the client:
//   - a repo the live guard does NOT block → 409 (nothing to request; enable it
//     directly);
//   - a refusal whose block set is not fully waivable (protection_unreadable — the
//     fail-closed "protection could not be verified" case, which an admin override can
//     never waive, D8/D3) → 422 with "waivable": false and NO row persisted, so a member
//     never opens a request an admin could not honour;
//   - otherwise the SeverityBlock findings are snapshotted onto an upserted pending
//     request (one open request per repo — a re-request refreshes the same row).
func (h *Handler) RequestGuardrailOverride(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	var req setGuardrailOverrideRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	reason, status, msg := validateGuardrailReason(req.Reason)
	if status != 0 {
		httpx.Error(w, status, msg)
		return
	}

	// Same owner-scoped preflight as SetRepoEnabled: a non-owned/unknown id is a 404,
	// resolved before any forge read.
	row, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("get repo for override request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Run the live guard with Overridden:false so the findings are the RAW refusal set,
	// exactly what SetRepoEnabled evaluates (forge.go), never a stored or overridden
	// view. This is the same GuardInput shape the enable gate builds.
	res := h.pcheck.GuardRepo(r.Context(), privcheck.GuardInput{
		ForgeType:       row.ForgeType,
		BaseURL:         row.BaseUrl,
		TokenCiphertext: row.TokenCiphertext,
		Repo: privcheck.Repo{
			ID:             row.ID.String(),
			Path:           row.PathWithNamespace,
			ForgeProjectID: row.ForgeProjectID,
			DefaultBranch:  row.DefaultBranch.String,
		},
		Overridden: false,
	})
	if !res.Blocked {
		// Not blocked → there is nothing to request; the member can enable directly.
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"error": "this repo is not blocked by the guardrail — enable it directly.",
		})
		return
	}
	if !privcheck.AllBlocksWaivable(res.Findings) {
		// A non-waivable block (protection_unreadable) can never be cleared by an admin
		// override (D8/D3), so refuse here rather than opening a doomed request.
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      "this refusal cannot be waived by an admin override (branch protection could not be verified); fix protection on the forge, then retry Enable.",
			"violations": blockFindingMessages(res.Findings),
			"waivable":   false,
		})
		return
	}

	// Snapshot the coded SeverityBlock findings that caused the refusal for the
	// findings jsonb column — audit/display only, no gate reads it back. The stored
	// shape is exactly the GuardrailFindingDTO the admin queue (M3) reads back.
	findingsJSON, err := json.Marshal(blockFindingDTOs(res.Findings))
	if err != nil {
		slog.Error("marshal override request findings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	reqRow, err := h.q.UpsertGuardrailOverrideRequest(r.Context(), store.UpsertGuardrailOverrideRequestParams{
		RepoID:      id,
		RequestedBy: user.ID,
		Reason:      reason,
		Findings:    findingsJSON,
	})
	if err != nil {
		slog.Error("upsert guardrail override request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"override_request": overrideRequestStateDTO(reqRow)})
}

// guardrailOverrideDecidedKind is the notifications.kind for the inbox row a member
// gets when an admin approves or rejects their guardrail-override request (PRD #1432
// M3). The notifications table's kind + payload jsonb is generic (PRD #60), so a new
// kind is free text needing no migration; the payload carries the rendered title/body,
// the repo, and the decision. (issue #1432.)
const guardrailOverrideDecidedKind = "guardrail_override_decided"

// guardrailOverrideRequestDTO maps a ListPendingGuardrailOverrideRequestsRow to the
// admin-queue wire DTO (PRD #1432 M3). The findings jsonb was stored by M2 as a
// marshaled []apitypes.GuardrailFindingDTO, so it unmarshals into that type directly;
// a malformed or null snapshot must NEVER nil the slice (the field is non-omitempty and
// the TS type is a non-nullable array) or fail the row — the queue is the primary
// payload — so it falls back to an empty (never nil) list.
func guardrailOverrideRequestDTO(row store.ListPendingGuardrailOverrideRequestsRow) apitypes.GuardrailOverrideRequestDTO {
	findings := []apitypes.GuardrailFindingDTO{}
	if len(row.Findings) > 0 {
		if err := json.Unmarshal(row.Findings, &findings); err != nil || findings == nil {
			findings = []apitypes.GuardrailFindingDTO{}
		}
	}
	return apitypes.GuardrailOverrideRequestDTO{
		ID:         row.ID.String(),
		RepoID:     row.RepoID.String(),
		RepoPath:   row.PathWithNamespace,
		OwnerID:    row.OwnerID.String(),
		OwnerEmail: row.OwnerEmail,
		ForgeType:  row.ForgeType,
		Reason:     row.Reason,
		Findings:   findings,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt.Time,
	}
}

// decideGuardrailOverrideRequestBody is the OPTIONAL POST body for approve/reject: an
// admin's note rendered back to the requester. Absent/empty is allowed.
type decideGuardrailOverrideRequestBody struct {
	DecisionNote string `json:"decision_note"`
}

// decideGuardrailOverrideRequest is the shared body of ApproveGuardrailOverrideRequest
// and RejectGuardrailOverrideRequest (PRD #1432 M3). status is "approved" or "rejected";
// applyOverride is true only for approve. It settles the PENDING request single-shot,
// and — on approve — sets the EXISTING #66 per-repo override with the member's own
// reason, the deciding admin as actor, and now(). It NEVER enables the repo (the member
// retries Enable so the live guard re-runs against the current forge state). The actor
// is the SESSION user, never the body (audit).
func (h *Handler) decideGuardrailOverrideRequest(w http.ResponseWriter, r *http.Request, status string, applyOverride bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "request")
	if !ok {
		return
	}

	// The body is OPTIONAL: an empty/absent body (io.EOF from the decoder) is not an
	// error, it just means "no note". Any other decode error is a malformed body → 400.
	var body decideGuardrailOverrideRequestBody
	if err := httpx.DecodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	notePg, nstatus, nmsg := validateOptionalGuardrailNote(body.DecisionNote)
	if nstatus != 0 {
		httpx.Error(w, nstatus, nmsg)
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("decide guardrail override: begin tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	req, err := qtx.DecideGuardrailOverrideRequest(ctx, store.DecideGuardrailOverrideRequestParams{
		ID:           id,
		Status:       status,
		DecidedBy:    user.ID,
		DecisionNote: notePg,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The `AND status = 'pending'` guard matched no row: the id is either unknown
			// (404) or already decided (409). Disambiguate with a plain get so a
			// double-decide reads as a conflict, not a not-found.
			if _, gerr := h.q.GetGuardrailOverrideRequest(ctx, id); errors.Is(gerr, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusNotFound, "override request not found")
			} else {
				httpx.Error(w, http.StatusConflict, "this override request has already been decided")
			}
			return
		}
		slog.Error("decide guardrail override request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	if applyOverride {
		// Reuse the EXISTING audited override write (SetRepoGuardrailOverride): it sets
		// the per-repo override with the member's reason + admin actor + now(), but it
		// NEVER enables the repo. The repo may have been deleted between the request and
		// this approval → pgx.ErrNoRows → 409 (rollback via defer).
		if _, err := qtx.SetRepoGuardrailOverride(ctx, store.SetRepoGuardrailOverrideParams{
			ID:                      req.RepoID,
			GuardrailOverrideReason: pgtype.Text{String: req.Reason, Valid: true},
			GuardrailOverrideBy:     pgconv.UUID(user.ID),
			GuardrailOverrideAt:     pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusConflict, "the repository no longer exists")
				return
			}
			slog.Error("decide guardrail override: set repo override", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("decide guardrail override: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Best-effort notify AFTER the durable commit; a notify error never fails the
	// decision (the request is already settled).
	h.notifyGuardrailOverrideDecision(ctx, req, status)

	httpx.JSON(w, http.StatusOK, map[string]any{"request": overrideRequestStateDTO(req)})
}

// ApproveGuardrailOverrideRequest is the admin "approve this member override request"
// write (PRD #1432 M3). Admin-only, mounted in the admin WRITE group (RequireAuth +
// RequireAdmin), so it is cookie-only — a uza_ Bearer 401s before the handler. Approval
// sets the EXISTING #66 per-repo override (SetRepoGuardrailOverride) with the member's
// own reason, the admin as actor, and now(); it does NOT enable the repo. The member
// then retries Enable, which re-runs the live guard so the override is weighed against
// the current forge state rather than a stale snapshot.
func (h *Handler) ApproveGuardrailOverrideRequest(w http.ResponseWriter, r *http.Request) {
	h.decideGuardrailOverrideRequest(w, r, "approved", true)
}

// RejectGuardrailOverrideRequest is the admin "reject this member override request"
// write (PRD #1432 M3). Admin-only, same admin WRITE-group posture as Approve (cookie-
// only; a uza_ Bearer 401s before the handler). It settles the pending request as
// rejected and notifies the requester; it sets no override and never touches the repo.
func (h *Handler) RejectGuardrailOverrideRequest(w http.ResponseWriter, r *http.Request) {
	h.decideGuardrailOverrideRequest(w, r, "rejected", false)
}

// notifyGuardrailOverrideDecision fires the best-effort inbox notification a member
// gets when an admin settles their override request (PRD #1432 M3). Nil-safe: no
// notifier wired ⇒ no-op (precedent notifyReviewReady). The repo path is a best-effort
// lookup — a lookup miss degrades the body to "your repository" rather than dropping
// the notification. A delivery error is logged, never returned: the decision is already
// committed and must not fail on a notify error. Slack:nil ⇒ inbox-only (precedent the
// run-failure notifier).
func (h *Handler) notifyGuardrailOverrideDecision(ctx context.Context, req store.GuardrailOverrideRequest, status string) {
	if h.notifier == nil {
		return
	}
	repoPath := ""
	if rp, err := h.q.GetRepoByID(ctx, req.RepoID); err == nil {
		repoPath = rp.PathWithNamespace
	}
	repoLabel := repoPath
	if repoLabel == "" {
		repoLabel = "your repository"
	}
	var title, bodyText string
	switch status {
	case "approved":
		title = "Guardrail override approved"
		bodyText = "An instance admin approved your request to allow " + repoLabel + " through the guardrail. Retry Enable on your Repos page — the live guard runs again."
	default: // "rejected"
		title = "Guardrail override rejected"
		bodyText = "An instance admin rejected your request to allow " + repoLabel + " through the guardrail."
	}
	if _, err := h.notifier.Notify(ctx, notifysvc.Notification{
		UserID: req.RequestedBy,
		Kind:   guardrailOverrideDecidedKind,
		Payload: map[string]any{
			"title":     title,
			"body":      bodyText,
			"repo_path": repoPath,
			"repo_id":   req.RepoID.String(),
			"decision":  status,
		},
		Slack: nil,
	}); err != nil {
		slog.Error("notify guardrail override decision", "error", err)
	}
}
