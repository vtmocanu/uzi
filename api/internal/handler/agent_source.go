package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Admin agent-source surface (PRD #602 M4). Three endpoints:
//   - GET  /admin/agent-source        (RequireAdminRO) config + status + staged review
//   - POST /admin/agent-source/sync   (cookie-only RequireAdmin) "Sync now"
//   - POST /admin/agent-source/apply  (cookie-only RequireAdmin) approve-and-apply
//
// The GET DTO is the approval surface (web AND CLI), so every untrusted string from
// the source repo is DISPLAY-sanitized before it leaves this handler: the staged
// roles' prompt_body through termsafe.SanitizeTTY (strip control/bidi/ANSI, keep
// \n\t so the markdown still reads), and description/model through termsafe.CellText
// (already Cf-rejected at parse, sanitized here defensively). This is a DISPLAY
// transform only — agentsource.Apply reads the RAW stored body from the DB, so the
// admin approves the true text and sanitization never alters what is written.

type agentSourceConfigDTO struct {
	URL                  string `json:"url"`
	Ref                  string `json:"ref"`
	Folder               string `json:"folder"`
	Enabled              bool   `json:"enabled"`
	Interval             string `json:"interval"`
	CredentialConfigured bool   `json:"credential_configured"`
}

type agentSourceCountsDTO struct {
	Staged  int `json:"staged"`
	Changed int `json:"changed"`
	Failed  int `json:"failed"`
}

type agentSourceStatusDTO struct {
	LastSyncAt     string                `json:"last_sync_at,omitempty"`
	LastSyncSHA    string                `json:"last_sync_sha,omitempty"`
	LastSyncStatus string                `json:"last_sync_status,omitempty"`
	LastSyncError  string                `json:"last_sync_error,omitempty"`
	LastAppliedAt  string                `json:"last_applied_at,omitempty"`
	LastAppliedSHA string                `json:"last_applied_sha,omitempty"`
	Counts         *agentSourceCountsDTO `json:"counts,omitempty"`
	// UpdateAvailable is DERIVED at read time (no egress) from the persisted remote
	// facts + the live config (PRD #702 M4, Decision 6). LatestRef names the newer
	// semver tag when a tag-pinned update is available (empty on a branch "moved"
	// signal); UpdateCheckedAt is the RFC3339 time of the last update check.
	UpdateAvailable bool   `json:"update_available"`
	LatestRef       string `json:"latest_ref,omitempty"`
	UpdateCheckedAt string `json:"update_checked_at,omitempty"`
}

type agentSourceRoleDTO struct {
	Name        string   `json:"name"`
	OK          bool     `json:"ok"`
	Reason      string   `json:"reason,omitempty"`
	Description string   `json:"description,omitempty"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	PromptBody  string   `json:"prompt_body,omitempty"`
	// BodySanitized is true when display-sanitization actually altered PromptBody, i.e.
	// the raw body carries hidden control/bidi/format characters that the sanitized
	// preview drops. The web approval surface flags this so the admin knows the preview
	// differs from the RAW body Apply writes. Not a rejection — an honesty signal.
	BodySanitized bool     `json:"body_sanitized"`
	Notes         []string `json:"notes,omitempty"`
}

type agentSourceDiffDTO struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

type agentSourceStagedDTO struct {
	FetchedAt  string               `json:"fetched_at,omitempty"`
	FetchedSHA string               `json:"fetched_sha"`
	SourceURL  string               `json:"source_url"`
	SourceRef  string               `json:"source_ref"`
	Roles      []agentSourceRoleDTO `json:"roles"`
	Diff       []agentSourceDiffDTO `json:"diff"`
	Counts     agentSourceCountsDTO `json:"counts"`
	// Pending is true when this staged snapshot has NOT yet been applied (its SHA
	// differs from the recorded last-applied SHA). false once Apply has run for it.
	Pending bool `json:"pending"`
}

type agentSourceDTO struct {
	Config agentSourceConfigDTO  `json:"config"`
	Status agentSourceStatusDTO  `json:"status"`
	Staged *agentSourceStagedDTO `json:"staged"` // nil when nothing has been staged yet
}

// GetAgentSource returns the agent-source config + status + the staged snapshot for
// review (PRD #602 M4). Admin read (RequireAdminRO). Read-only: it never triggers a
// sync or an apply.
func (h *Handler) GetAgentSource(w http.ResponseWriter, r *http.Request) {
	dto, err := h.agentSourceDTO(r.Context())
	if err != nil {
		slog.Error("agent-source: build dto", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agent_source": dto})
}

// PostAgentSourceSync runs "Sync now" (PRD #602 M4): the SAME Reconcile the interval
// loop calls (one fn, two triggers). Idempotent via M3's SHA check. Cookie-only admin
// write. Returns the resulting config + status + staged snapshot.
func (h *Handler) PostAgentSourceSync(w http.ResponseWriter, r *http.Request) {
	if h.agentSource == nil {
		httpx.Error(w, http.StatusInternalServerError, "agent source not configured")
		return
	}
	if _, err := h.agentSource.Reconcile(r.Context()); err != nil {
		// Reconcile records every failure in its status and returns nil today; a
		// non-nil error is unexpected, so surface it as a 500 rather than swallow it.
		slog.Error("agent-source: sync now", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "sync failed")
		return
	}
	dto, err := h.agentSourceDTO(r.Context())
	if err != nil {
		slog.Error("agent-source: build dto after sync", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agent_source": dto})
}

type applyAgentSourceRequest struct {
	// ExpectedSHA is the REQUIRED reviewed-snapshot bind: the fetched SHA of the exact
	// staged snapshot the admin approved. Apply re-reads the staged snapshot inside its
	// transaction and refuses (ErrStaleApproval → 409) if that tx-read SHA no longer
	// matches, so an admin can never apply a snapshot that silently changed under them
	// (a concurrent restage) — nor apply blindly with no snapshot in mind. Absent/empty
	// → 400: the admin must approve a specific reviewed snapshot.
	ExpectedSHA string `json:"expected_sha"`
}

// PostAgentSourceApply approves-and-applies the current staged snapshot (PRD #602
// M4): the ONLY path that writes agent_templates from a sync. Cookie-only admin
// write. Nothing reaches a run before this call. Returns the ApplyResult.
func (h *Handler) PostAgentSourceApply(w http.ResponseWriter, r *http.Request) {
	if h.agentSource == nil {
		httpx.Error(w, http.StatusInternalServerError, "agent source not configured")
		return
	}
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req applyAgentSourceRequest
	// A missing body is not an error to DECODE, but expected_sha is required below, so
	// an absent body falls through to the 400. Only a malformed body is a decode error.
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	expectedSHA := strings.TrimSpace(req.ExpectedSHA)
	if expectedSHA == "" {
		httpx.Error(w, http.StatusBadRequest, "expected_sha is required: approve a specific reviewed snapshot")
		return
	}

	// The authoritative bind is IN the apply transaction: Apply re-reads the staged
	// snapshot and compares it to expectedSHA, so there is no TOCTOU window between a
	// handler pre-check and the apply. A mismatch (or nothing staged) is ErrStaleApproval → 409.
	result, err := h.agentSource.Apply(r.Context(), pgconv.UUID(actor.ID), expectedSHA)
	if errors.Is(err, agentsource.ErrStaleApproval) {
		httpx.Error(w, http.StatusConflict, "the staged snapshot changed since you reviewed it; re-review before applying")
		return
	}
	if err != nil {
		slog.Error("agent-source: apply", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "apply failed")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"result": result})
}

// PostAgentSourceUpdateCheck ls-remotes the CONFIGURED source and persists the remote
// facts the GET/status derive against (PRD #702 M4). Cookie-only admin write, mounted
// alongside sync/apply. It mirrors PostAgentSourceSync: run the check, then always
// rebuild and return the refreshed DTO so the badge/checked-at update. On an error
// status nothing was persisted (last-good remote facts stay), and the (PAT-scrubbed)
// message rides in the status where a sync error would — so the web can surface it.
func (h *Handler) PostAgentSourceUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if h.agentSource == nil {
		httpx.Error(w, http.StatusInternalServerError, "agent source not configured")
		return
	}
	res, err := h.agentSource.CheckForUpdate(r.Context())
	if err != nil {
		// CheckForUpdate records every failure in its result and returns nil today; a
		// non-nil error is unexpected, so surface it as a 500 rather than swallow it.
		slog.Error("agent-source: update check", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "update check failed")
		return
	}
	dto, derr := h.agentSourceDTO(r.Context())
	if derr != nil {
		slog.Error("agent-source: build dto after update check", "error", derr)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// On an error status the check persisted nothing, so agentSourceDTO filled
	// LastSyncStatus from the PRIOR sync ("ok", or "" on a never-synced install).
	// Override BOTH fields the web branches on: force LastSyncStatus="error" so the
	// web's `last_sync_status === "error"` branch fires, and surface the message where
	// a sync error would appear (SanitizeTTY defensively, as the sync error is).
	if res.Status == "error" {
		dto.Status.LastSyncStatus = "error"
		if res.Message != "" {
			dto.Status.LastSyncError = termsafe.SanitizeTTY(res.Message)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agent_source": dto})
}

type resolveLatestRequest struct {
	URL string `json:"url"`
}

// PostAgentSourceResolveLatest resolves the newest semver tag for a TYPED, unsaved source
// URL (PRD #702 M2, Decision 5). It is the preset "resolve latest tag" backend: the admin
// has typed a URL in the card but not Saved it, so no sealed credential exists — the
// ls-remote is ANONYMOUS and only works against a public source (the canonical
// github.com/vtmocanu/skills is public).
//
// This is the ONE new outbound-egress path in the PRD, so it is gated exactly like sync:
// cookie-only admin (mounted in the RequireAuth+RequireAdmin group, off the read-only GET
// and off the uza_ CLI token), AND SSRF-rechecked against AGENT_SOURCE_ALLOWED_BASE_URLS
// here — BEFORE any network call, so an off-allowlist URL causes zero egress. Every error
// is a clean, PAT-scrubbed message (the ls-remote helper scrubs before it reaches here).
func (h *Handler) PostAgentSourceResolveLatest(w http.ResponseWriter, r *http.Request) {
	var req resolveLatestRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	url := strings.TrimSpace(req.URL)
	if url == "" {
		httpx.Error(w, http.StatusBadRequest, "url is required")
		return
	}

	// SSRF recheck (Decision 5) BEFORE any network call: an off-allowlist URL is refused
	// here, so it never reaches the ls-remote transport — zero egress. Empty allowlist =
	// deny-all (nothing is allowed until an admin configures AGENT_SOURCE_ALLOWED_BASE_URLS).
	if !h.cfg.AgentSourceBaseURLAllowed(url) {
		httpx.Error(w, http.StatusBadRequest, "source url is not on the AGENT_SOURCE_ALLOWED_BASE_URLS allowlist")
		return
	}

	// Anonymous (no Token): an unsaved typed URL has no saved credential. RedirectAllowed
	// re-checks any redirect target against the same allowlist (fail-closed on nil).
	tag, err := agentsource.ResolveLatestTag(r.Context(), agentsource.CloneOptions{
		CloneURL:        url,
		RedirectAllowed: h.cfg.AgentSourceBaseURLAllowed,
	})
	if err != nil {
		// The helper already PAT-scrubs; keep the client-facing message generic.
		slog.Error("agent-source: resolve latest tag", "error", err)
		httpx.Error(w, http.StatusBadGateway, "could not reach source to resolve latest tag")
		return
	}
	// tag may be "" when the source advertises no valid semver tag; the web treats that
	// as "no semver tags found".
	httpx.JSON(w, http.StatusOK, map[string]any{"latest_ref": tag})
}

// agentSourceDTO assembles the config + status + staged-snapshot view. The staged
// roles' body/description/model are display-sanitized here (see the file header);
// the apply path reads the raw DB body, never this DTO.
func (h *Handler) agentSourceDTO(ctx context.Context) (agentSourceDTO, error) {
	enabled, err := h.settings.AgentSourceEnabled(ctx)
	if err != nil {
		return agentSourceDTO{}, err
	}
	url, _ := h.settings.AgentSourceRepoURL(ctx)
	ref, _ := h.settings.AgentSourceRef(ctx)
	folder, _ := h.settings.AgentSourceFolder(ctx)
	interval, err := h.settings.AgentSourceInterval(ctx)
	if err != nil {
		return agentSourceDTO{}, err
	}
	credConfigured, _ := h.settings.AgentSourceCredentialConfigured(ctx)

	dto := agentSourceDTO{
		Config: agentSourceConfigDTO{
			URL:                  url,
			Ref:                  ref,
			Folder:               folder,
			Enabled:              enabled,
			Interval:             interval.String(),
			CredentialConfigured: credConfigured,
		},
	}

	if st, err := h.settings.AgentSourceStatus(ctx); err == nil {
		dto.Status = agentSourceStatusDTO{
			LastSyncAt:     st.LastSyncAt,
			LastSyncSHA:    st.LastSyncSHA,
			LastSyncStatus: st.LastSyncStatus,
			// PAT-scrubbed at write time; SanitizeTTY defensively before display.
			LastSyncError:  termsafe.SanitizeTTY(st.LastSyncError),
			LastAppliedAt:  st.LastAppliedAt,
			LastAppliedSHA: st.LastAppliedSHA,
		}
		if c, ok := parseSyncCounts(st.CountsJSON); ok {
			dto.Status.Counts = &c
		}
		// Derive "update available" from the persisted remote facts + the live config
		// (PRD #702 M4, Decision 6). This is a PURE computation — it references no
		// ls-remote seam and performs NO network call, so the plain GET stays zero-egress.
		avail, showRef := agentsource.DeriveUpdate(ref, st.LastAppliedSHA, st.LatestRef, st.RemoteTipSHA)
		dto.Status.UpdateAvailable = avail
		dto.Status.LatestRef = showRef
		dto.Status.UpdateCheckedAt = st.UpdateCheckedAt
	}

	staged, serr := h.q.GetAgentSourceStaged(ctx)
	if errors.Is(serr, pgx.ErrNoRows) {
		return dto, nil // nothing staged: Staged stays nil
	}
	if serr != nil {
		return agentSourceDTO{}, serr
	}
	dto.Staged = stagedToDTO(staged, dto.Status.LastAppliedSHA)
	return dto, nil
}

// stagedToDTO decodes a staged snapshot row into its review DTO, DISPLAY-sanitizing
// every untrusted string. pending is derived from lastAppliedSHA.
func stagedToDTO(staged store.AgentSourceStaged, lastAppliedSHA string) *agentSourceStagedDTO {
	out := &agentSourceStagedDTO{
		FetchedSHA: staged.FetchedSha,
		SourceURL:  staged.SourceUrl,
		SourceRef:  staged.SourceRef,
		Pending:    staged.FetchedSha != "" && staged.FetchedSha != lastAppliedSHA,
	}
	if staged.FetchedAt.Valid {
		out.FetchedAt = staged.FetchedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}

	var roles []agentsource.StagedRole
	if len(staged.Roles) > 0 {
		_ = json.Unmarshal(staged.Roles, &roles)
	}
	for _, role := range roles {
		// SanitizeTTY the body once: neutralize control/bidi/ANSI, keep \n\t so the
		// markdown still renders as authored. LOAD-BEARING: the approval surface must
		// not be spoofable by a hostile source's body. Reuse the result for both the
		// preview body and the body_sanitized honesty flag (do not sanitize twice).
		sanitizedBody := termsafe.SanitizeTTY(role.PromptBody)
		r := agentSourceRoleDTO{
			Name: role.Name,
			OK:   role.OK,
			// CellText the routing description + model (single-line, cross-owner surface).
			Reason:      role.Reason,
			Description: termsafe.CellText(role.Description),
			Model:       termsafe.CellText(role.Model),
			Tools:       role.Tools,
			PromptBody:  sanitizedBody,
			// body_sanitized: true only when sanitization changed the body, i.e. the raw
			// body differs from the preview shown. Apply still writes the RAW body.
			BodySanitized: sanitizedBody != role.PromptBody,
			Notes:         role.Notes,
		}
		if role.OK {
			out.Counts.Staged++
		} else {
			out.Counts.Failed++
		}
		out.Roles = append(out.Roles, r)
	}

	var diff []agentsource.DiffEntry
	if len(staged.Diff) > 0 {
		_ = json.Unmarshal(staged.Diff, &diff)
	}
	for _, d := range diff {
		if d.Action != agentsource.DiffUnchanged {
			out.Counts.Changed++
		}
		out.Diff = append(out.Diff, agentSourceDiffDTO{
			Name:   d.Name,
			Action: d.Action,
			Detail: d.Detail,
		})
	}
	return out
}

// parseSyncCounts decodes the engine-stored {"staged":N,"changed":N,"failed":N} blob.
func parseSyncCounts(raw string) (agentSourceCountsDTO, bool) {
	if raw == "" {
		return agentSourceCountsDTO{}, false
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return agentSourceCountsDTO{}, false
	}
	return agentSourceCountsDTO{Staged: m["staged"], Changed: m["changed"], Failed: m["failed"]}, true
}
