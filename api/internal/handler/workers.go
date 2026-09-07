package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"github.com/vtmocanu/uzi/api/internal/workertmpl"
)

// maxWorkerNameBytes bounds a worker's human label.
const maxWorkerNameBytes = 200

// -------------------------------------------------------------------------
// DTOs
// -------------------------------------------------------------------------

// workerDTO/runDTO/messageDTO moved to the stdlib-only apitypes leaf (PRD #64 M1);
// the mappers below stay here as the store→DTO builders.

// effectiveBindMode maps a worker's stored (mode, id) pair to the mode a client
// should render (PRD #111 M3, D9). It is TOTAL IN BOTH DIRECTIONS, which is the
// whole point: whatever the row holds, the answer it returns agrees with what
// workerSecretID will actually resolve.
//
//   - 'pinned' with NO id reports 'default'. Real and reachable, not defensive:
//     00078's FK nulls workers.anthropic_secret_id when the credential is deleted
//     and deliberately leaves the mode alone (a coupling CHECK is impossible — see
//     00088), so every worker pinned to a token their owner then deletes sits here.
//   - a non-pinned mode WITH an id reports that mode, and the DTO mappers drop the
//     id and label beside it. This direction was missing, and it was not
//     hypothetical: the create path produced exactly this row until M3-BLOCK was
//     fixed, so the API shipped `anthropic_bind_mode:"default"` next to a non-NULL
//     id and label — and WorkersSettings renders "spends <label>" off the id branch,
//     so the picker said pinned while the run record said default, about one worker.
//
// Reporting the raw column would put that same three-line rule in every client, and
// each would have to get both directions right. Applying it once, server-side, is
// the same reasoning as M2's auto_status: one implementation of a rule two surfaces
// read. Being total means the DTO cannot EXPRESS a contradiction even if a future
// writer produces one.
func effectiveBindMode(mode string, secretID pgtype.UUID) string {
	if mode == workersvc.BindModePinned && !secretID.Valid {
		return workersvc.BindModeDefault
	}
	return mode
}

// bindingForMode is effectiveBindMode's other half: the id and label a client should
// see, given the mode that will actually govern. A non-pinned worker shows neither,
// because neither is read on its claim — emitting them invites exactly the
// "picker says pinned, run record says default" split described above.
func bindingForMode(mode string, secretID pgtype.UUID, label string) (*string, *string) {
	if effectiveBindMode(mode, secretID) != workersvc.BindModePinned {
		return nil, nil
	}
	return uuidPtrValue(secretID), textPtrValue(label != "", label)
}

// workerDTOFromWorker builds the DTO from a bare worker row plus its active (non-chat)
// run count and its any-kind busy flag — both computed by the list queries, never
// derivable from a bare Worker row. The register/heartbeat/create paths hold neither
// (a just-registered worker has requeued its orphans and holds nothing), so they pass
// 0/false, exactly as they previously passed busy=false.
// secretLabel is the bound credential's name when the caller knows it (the mint
// and rebind paths do — they just resolved it) and "" otherwise. The bare-row
// callers that pass "" render a bound worker with an id and no label, which is
// honest: this row carries no join to look it up. The list path uses
// workerDTOFromRow, which does have the join.
// cpVersion is the control plane's own release (h.version) — the reference the
// upgrade status is derived against. It is threaded explicitly rather than read off
// the Handler because that is this file's convention for these builders, and because
// it keeps the classification a pure function of its inputs. Passing "" yields
// `unknown`, which is also what a genuinely unstamped build produces.
// pinnedWorkerVersion is the deploy's concrete pinned hosted-worker image tag
// (h.cfg.HostedWorkerVersion / workers.image.tag, PRD #422) — the hosted-worker upgrade
// target, so a worker intentionally pinned behind appVersion is not flagged `outdated`.
// Passing "" falls back to cpVersion (today's behavior for an unconfigured deploy).
//
// NO ROLL SIGNAL on this path, deliberately (PRD #113 M4). The bare-row callers —
// register, heartbeat, create, admin list — hold a worker row with no roll-health
// join, so they classify by version comparison alone. That is honest rather than
// degraded: passing a nil signal says "this row carries no controller report", which
// is exactly what it carries. The per-user LIST path has the join and folds it.
func workerDTOFromWorker(w store.Worker, activeRuns int, busy bool, secretLabel, cpVersion, pinnedWorkerVersion string, now, apiStartedAt time.Time) apitypes.WorkerDTO {
	upgradeStatus, upgradeDetail, upgradeTarget := workersvc.ClassifyUpgradeWithTarget(workersvc.UpgradeInput{
		Reported:            w.Version.String,
		Kind:                w.Kind,
		CPVersion:           cpVersion,
		PinnedWorkerVersion: pinnedWorkerVersion,
		Signal:              nil,
		Now:                 now,
		APIStartedAt:        apiStartedAt,
	}, workersvc.UpgradeParams{})
	bindingID, bindingLabel := bindingForMode(w.AnthropicBindMode, w.AnthropicSecretID, secretLabel)
	return apitypes.WorkerDTO{
		UpgradeStatus:        upgradeStatus,
		UpgradeDetail:        textPtrValue(upgradeDetail != "", upgradeDetail),
		UpgradeTarget:        upgradeTarget,
		AnthropicSecretID:    bindingID,
		AnthropicSecretLabel: bindingLabel,
		AnthropicBindMode:    effectiveBindMode(w.AnthropicBindMode, w.AnthropicSecretID),
		ID:                   w.ID.String(),
		Name:                 w.Name,
		Status:               w.Status,
		Kind:                 w.Kind,
		HostedSize:           textPtrValue(w.HostedSize.Valid, w.HostedSize.String),
		Docker:               boolPtrValue(w.DockerEnabled),
		Ephemeral:            w.Ephemeral,
		Capabilities:         w.Capabilities,
		Busy:                 busy,
		ActiveRuns:           activeRuns,
		MaxConcurrentRuns:    intPtrValue(w.MaxConcurrentRuns),
		TemplateDeclared:     textPtrValue(w.TemplateDeclared.Valid, w.TemplateDeclared.String),
		TemplateReported:     textPtrValue(w.TemplateReported.Valid, w.TemplateReported.String),
		Version:              textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt:      timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		OnlineSince:          timePtr(w.OnlineSince.Valid, w.OnlineSince.Time),
		DrainingSince:        timePtr(w.DrainingSince.Valid, w.DrainingSince.Time),
		CreatedAt:            w.CreatedAt.Time,
		StatsCPUPct:          float4PtrValue(w.StatsCpuPct),
		StatsMemBytes:        int8PtrValue(w.StatsMemBytes),
		StatsMemLimitBytes:   int8PtrValue(w.StatsMemLimitBytes),
		StatsSource:          textPtrValue(w.StatsSource.Valid, w.StatsSource.String),

		StatsDiskNixBytes:       int8PtrValue(w.StatsDiskNixBytes),
		StatsDiskNixTotalBytes:  int8PtrValue(w.StatsDiskNixTotalBytes),
		StatsDiskDataBytes:      int8PtrValue(w.StatsDiskDataBytes),
		StatsDiskDataTotalBytes: int8PtrValue(w.StatsDiskDataTotalBytes),
	}
}

func workerDTOFromRow(w store.ListWorkersByUserRow, cpVersion, pinnedWorkerVersion string, now, apiStartedAt time.Time) apitypes.WorkerDTO {
	upgradeStatus, upgradeDetail, upgradeTarget := workersvc.ClassifyUpgradeWithTarget(workersvc.UpgradeInput{
		Reported:            w.Version.String,
		Kind:                w.Kind,
		CPVersion:           cpVersion,
		PinnedWorkerVersion: pinnedWorkerVersion,
		Signal:              rollSignalFromRow(w),
		Now:                 now,
		APIStartedAt:        apiStartedAt,
	}, workersvc.UpgradeParams{})
	rowBindingID, rowBindingLabel := bindingForMode(
		w.AnthropicBindMode, w.AnthropicSecretID, w.AnthropicSecretLabel.String)
	return apitypes.WorkerDTO{
		UpgradeStatus: upgradeStatus,
		UpgradeDetail: textPtrValue(upgradeDetail != "", upgradeDetail),
		UpgradeTarget: upgradeTarget,
		// Only meaningful for a failed upgrade, and only the list path has the join that
		// carries them at all.
		UpgradeBlockingContainer: textPtrValue(upgradeStatus == workersvc.UpgradeStatusUpgradeFailed && w.RollBlockingContainer.Valid, w.RollBlockingContainer.String),
		UpgradeBlockingReason:    textPtrValue(upgradeStatus == workersvc.UpgradeStatusUpgradeFailed && w.RollBlockingReason.Valid, w.RollBlockingReason.String),
		UpgradeLastExitCode:      int32PtrValue(upgradeStatus == workersvc.UpgradeStatusUpgradeFailed && w.RollLastExitCode.Valid, w.RollLastExitCode.Int32),
		AnthropicSecretID:        rowBindingID,
		AnthropicSecretLabel:     rowBindingLabel,
		AnthropicBindMode:        effectiveBindMode(w.AnthropicBindMode, w.AnthropicSecretID),
		ID:                       w.ID.String(),
		Name:                     w.Name,
		Status:                   w.Status,
		Kind:                     w.Kind,
		HostedSize:               textPtrValue(w.HostedSize.Valid, w.HostedSize.String),
		Docker:                   boolPtrValue(w.DockerEnabled),
		Ephemeral:                w.Ephemeral,
		Capabilities:             w.Capabilities,
		Busy:                     w.Busy,
		ActiveRuns:               int(w.ActiveRuns),
		MaxConcurrentRuns:        intPtrValue(w.MaxConcurrentRuns),
		TemplateDeclared:         textPtrValue(w.TemplateDeclared.Valid, w.TemplateDeclared.String),
		TemplateReported:         textPtrValue(w.TemplateReported.Valid, w.TemplateReported.String),
		Version:                  textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt:          timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		OnlineSince:              timePtr(w.OnlineSince.Valid, w.OnlineSince.Time),
		DrainingSince:            timePtr(w.DrainingSince.Valid, w.DrainingSince.Time),
		CreatedAt:                w.CreatedAt.Time,
		StatsCPUPct:              float4PtrValue(w.StatsCpuPct),
		StatsMemBytes:            int8PtrValue(w.StatsMemBytes),
		StatsMemLimitBytes:       int8PtrValue(w.StatsMemLimitBytes),
		StatsSource:              textPtrValue(w.StatsSource.Valid, w.StatsSource.String),

		StatsDiskNixBytes:       int8PtrValue(w.StatsDiskNixBytes),
		StatsDiskNixTotalBytes:  int8PtrValue(w.StatsDiskNixTotalBytes),
		StatsDiskDataBytes:      int8PtrValue(w.StatsDiskDataBytes),
		StatsDiskDataTotalBytes: int8PtrValue(w.StatsDiskDataTotalBytes),
	}
}

// rollSignalFromRow lifts the LEFT-JOINed roll-health columns into the classifier's
// input, or nil when no report exists for this worker.
//
// The nil is the important half: absent is a distinct state from any phase value, and
// it is what lets a report DECAY — the api falls back to version comparison rather
// than keeping the last thing the controller said forever. A zero-valued struct here
// instead of nil would read as a signal with an empty phase and an epoch timestamp.
//
// controller_reported_at is not among the columns selected, so it cannot reach this
// struct and therefore cannot reach freshness. That is structural, not a rule.
func rollSignalFromRow(w store.ListWorkersByUserRow) *workersvc.RollSignal {
	if !w.RollPhase.Valid || !w.RollObservedAt.Valid {
		return nil
	}
	sig := &workersvc.RollSignal{
		Phase:             w.RollPhase.String,
		ObservedAt:        w.RollObservedAt.Time,
		PodPhase:          w.RollPodPhase.String,
		BlockingContainer: w.RollBlockingContainer.String,
		BlockingReason:    w.RollBlockingReason.String,
		RolledTag:         w.RollWorkerImageTag.String,
	}
	if w.RollRestartCount.Valid {
		sig.RestartCount = w.RollRestartCount.Int32
	}
	if w.RollLastExitCode.Valid {
		code := w.RollLastExitCode.Int32
		sig.LastExitCode = &code
	}
	if w.RollPhaseSince.Valid {
		t := w.RollPhaseSince.Time
		sig.PhaseSince = &t
	}
	if w.RollUpgradingSince.Valid {
		t := w.RollUpgradingSince.Time
		sig.UpgradingSince = &t
	}
	return sig
}

// -------------------------------------------------------------------------
// Worker management (session-authenticated)
// -------------------------------------------------------------------------

// CreateWorker issues a worker and returns its join token exactly once. The
// token is shown here and never again (only its hash is stored), so the client
// must surface it to the user immediately.
func (h *Handler) CreateWorker(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Name string `json:"name"`
		// Template is the worker template the user picks at issuance (PRD #18).
		// Optional: empty ⇒ no declared choice (stored NULL). When present it must
		// be a known curated template — validated against the registry so an
		// arbitrary string can't land in the declared column.
		Template string `json:"template"`
		// AnthropicToken is the LABEL of the credential this worker should spend
		// (PRD #104 M3), optional: empty ⇒ unbound ⇒ the owner's default. A label,
		// not an id, because this is the surface a human types into. An unknown
		// label is a 400 rather than a worker minted against a token that does not
		// exist and would only fail at its first claim.
		AnthropicToken string `json:"anthropic_token"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxWorkerNameBytes {
		httpx.Error(w, http.StatusBadRequest, "name must be non-empty and at most 200 characters")
		return
	}
	// BEHAVIOUR CHANGE, #169: names that POST fine today start 400ing. `uzi admin
	// workers` prints this name beside a DIFFERENT user's owner_email, so an ESC or a
	// bidi override here is terminal control injection into another user's session, and
	// an embedded newline forges a row in a table an admin reads to make decisions. The
	// rule is termsafe's, which is the SAME rule the CLI renderer strips by — so a name
	// that gets past this displays exactly as it was typed, and one that would not is
	// refused rather than silently rewritten into something the user never chose.
	if err := termsafe.Validate("name", name); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	template := strings.TrimSpace(req.Template)
	if template != "" && !workertmpl.Valid(template) {
		httpx.Error(w, http.StatusBadRequest, "unknown worker template")
		return
	}

	tokenLabel := strings.TrimSpace(req.AnthropicToken)
	wkr, token, err := h.wsvc.CreateWorker(r.Context(), user.ID, name, template, tokenLabel)
	if err != nil {
		if errors.Is(err, workersvc.ErrUnknownSecretLabel) {
			httpx.Error(w, http.StatusBadRequest, "no Anthropic token with that label")
			return
		}
		slog.Error("create worker", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"worker": workerDTOFromWorker(wkr, 0, false, tokenLabel, h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt),
		"token":  token,
	})
}

// ListWorkers returns the current user's workers with derived busy status.
func (h *Handler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.wsvc.ListWorkers(r.Context(), user.ID)
	if err != nil {
		slog.Error("list workers", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.WorkerDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, workerDTOFromRow(row, h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"workers": out})
}

// AdminListWorkers returns every worker with its owner email and busy status
// (admin-only, gated by RequireAdmin on the route).
func (h *Handler) AdminListWorkers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.wsvc.ListAllWorkers(r.Context())
	if err != nil {
		slog.Error("admin list workers", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.AdminWorkerDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, apitypes.AdminWorkerDTO{
			WorkerDTO:  workerDTOFromWorker(row.Worker, int(row.ActiveRuns), row.Busy, "", h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt),
			OwnerEmail: row.OwnerEmail,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"workers": out})
}

// DeleteWorker revokes a worker owned by the current user.
func (h *Handler) DeleteWorker(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	if err := h.wsvc.DeleteWorker(r.Context(), user.ID, id); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrWorkerNotFound):
			httpx.Error(w, http.StatusNotFound, "worker not found")
		case errors.Is(err, workersvc.ErrWorkerHasActiveRuns):
			httpx.Error(w, http.StatusConflict, "worker has active runs; cancel them before deleting it")
		default:
			slog.Error("delete worker", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PatchWorker rebinds a worker to one of its owner's Anthropic credentials, or
// clears the binding back to the owner's default (PRD #104 M3, D1).
//
// RequireUser rather than cookie-only (D8): unlike POST /workers, which MINTS a
// join token whose claim yields a decrypted credential, this only re-points a
// worker between two credentials the caller ALREADY owns. It grants the caller no
// access they did not have — but that argument holds only while the ownership
// check does, which is why the composite FK backs it independently of this handler.
//
// The body names a token by LABEL, the thing a human knows. `null` (or "") clears
// the binding; an OMITTED key leaves it alone. Distinguishing those two is why the
// field is a json.RawMessage rather than a *string — see parseTokenField, where a
// nil *string would have collapsed "clear" and "don't touch" into one request.
//
// The rebind lands on the worker's NEXT claim. No restart, no re-minted join token.
func (h *Handler) PatchWorker(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	var req struct {
		AnthropicToken json.RawMessage `json:"anthropic_token"`
		// PRD #111 M3. Optional, and its absence is not a clear — see below.
		AnthropicBindMode *string `json:"anthropic_bind_mode"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token, ok := parseTokenField(req.AnthropicToken)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "anthropic_token must be a token label, null, or omitted")
		return
	}
	// The MODE the caller is asking for, derived when they did not say (PRD #111 M3).
	//
	// Deriving rather than requiring is what keeps every pre-#111 client working
	// unchanged: a label has always meant "pin to this" and null "use my default",
	// which are exactly `pinned` and `default`. So the existing web PATCH and
	// `uzi worker set-token` keep their meaning with no version negotiation, and the
	// derivation is the same rule 00088's backfill applies to existing rows.
	mode := workersvc.BindModePinned
	if token.label == "" {
		mode = workersvc.BindModeDefault
	}
	if req.AnthropicBindMode != nil {
		mode = *req.AnthropicBindMode
		if !workersvc.ValidBindMode(mode) {
			httpx.Error(w, http.StatusBadRequest, "anthropic_bind_mode must be one of: default, pinned, auto")
			return
		}
		// A mode and a token that contradict each other are REFUSED, not silently
		// reconciled. Either choice of winner spends a credential the caller did not
		// ask for on some request, and "auto" arriving with a leftover label is the
		// realistic shape of a half-updated client — exactly when a quiet reconcile
		// does the most damage.
		switch {
		case mode == workersvc.BindModePinned && token.label == "":
			httpx.Error(w, http.StatusBadRequest, "anthropic_bind_mode=pinned requires a token label in anthropic_token")
			return
		case mode != workersvc.BindModePinned && token.label != "":
			httpx.Error(w, http.StatusBadRequest, "anthropic_token must be null when anthropic_bind_mode is default or auto")
			return
		}
	}
	// An omitted key is NOT a clear. Today this route carries only the binding, so a
	// body without it asks for nothing and is a client bug worth naming — but the
	// rule is the load-bearing part: PATCH means "change what I named", and the day
	// this body grows a second field, absent-means-clear would wipe a user's binding
	// every time someone renamed a worker. Answering 400 rather than 200-with-no-op
	// avoids inventing a read path just to echo an unchanged worker back.
	//
	// `anthropic_bind_mode` alone satisfies it: a caller switching a worker to auto
	// has named what they are changing, and requiring a redundant `"anthropic_token":
	// null` beside it would be ceremony, not safety.
	if !token.present && req.AnthropicBindMode == nil {
		httpx.Error(w, http.StatusBadRequest,
			"anthropic_token or anthropic_bind_mode is required; pass anthropic_token null to use your default token")
		return
	}

	var secretID *uuid.UUID
	if token.label != "" {
		resolved, rerr := h.wsvc.ResolveTokenLabel(r.Context(), user.ID, token.label)
		if rerr != nil {
			if errors.Is(rerr, workersvc.ErrUnknownSecretLabel) {
				httpx.Error(w, http.StatusBadRequest, "no Anthropic token with that label")
				return
			}
			slog.Error("resolve anthropic token label", "error", rerr)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		secretID = &resolved
	}

	wkr, err := h.wsvc.SetWorkerAnthropicToken(r.Context(), user.ID, id, mode, secretID)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrInvalidBindMode):
			// Unreachable through this handler (ValidBindMode ran above), and mapped
			// anyway: the service is the layer that owns the vocabulary, so a future
			// caller that skips the check gets a 400 naming the problem rather than a
			// 500 from 00088's CHECK.
			httpx.Error(w, http.StatusBadRequest, "anthropic_bind_mode must be one of: default, pinned, auto")
		case errors.Is(err, workersvc.ErrWorkerNotFound), errors.Is(err, workersvc.ErrSecretNotOwned):
			// Both are 404, and deliberately the same 404: distinguishing them would
			// tell a caller which of the two ids they guessed happens to exist.
			httpx.Error(w, http.StatusNotFound, "worker not found")
		default:
			slog.Error("set worker anthropic token", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"worker": workerDTOFromWorker(wkr, 0, false, token.label, h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt)})
}
