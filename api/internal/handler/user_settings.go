package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/theme"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// userSettingsDTO is the current user's own (non-secret) settings: the default
// worker model (PRD #17), the per-user judge model override (PRD #69) and
// run-summary model override (PRD #362 M2), the UI theme override (PRD #21), and
// the sidebar token-meter choice (migration 00123). null default_model means
// inherit (the lead template's model, else the account/SDK default); null
// judge_model / summary_model means inherit the instance value; null theme means
// "use the instance default". sidebar_token_ids lists the NON-default Anthropic
// tokens whose rate meters the user surfaced on the sidebar rail — the default
// token always shows and is never listed; empty means default-only.
type userSettingsDTO struct {
	// DefaultModel is DEPRECATED (PRD #1551 D2/D3): the retained model defaults now live
	// in the two per-harness lanes below. On GET/PUT this legacy field is a PROJECTION of
	// the lane for the effective harness (ResolveSettingsHarness), kept one release as a
	// compatibility field for stale web assets. A legacy-only write is bridged into the
	// target lane (D3); new clients send the explicit lanes instead.
	DefaultModel *string `json:"default_model"`
	// DefaultClaudeModel and DefaultCodexModel are the retained per-harness worker-model
	// lanes (PRD #1551 M1 / D2): each null = inherit. Changing the default harness never
	// clears either lane. A curated Codex id is rejected in the Claude lane and a known
	// Claude alias in the Codex lane (D3 closed-list cross-vocabulary guard).
	DefaultClaudeModel *string `json:"default_claude_model"`
	DefaultCodexModel  *string `json:"default_codex_model"`
	DefaultEffort      *string `json:"default_effort"`
	JudgeModel         *string `json:"judge_model"`
	SummaryModel       *string `json:"summary_model"`
	// Theme is the DEPRECATED legacy single-theme override (PRD #21); kept one
	// release. The appearance override lives in the four fields below (PRD #1167).
	Theme *string `json:"theme"`
	// MrReworkEnabled is the per-user opt-in to the MR review watcher (PRD #700 M5),
	// which ships ON. null means "unset" = inherit the default-ON semantics (a
	// NULL/absent user value is read as enabled); an explicit false is the opt-OUT.
	MrReworkEnabled *bool    `json:"mr_rework_enabled"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
	// SidebarCodexAccountIds lists the LINKED Codex subscription accounts the user
	// surfaced on the sidebar rail (PRD #1209 M1, 00239) — the codex sibling of
	// SidebarTokenIds. The default account always shows and is never listed here; empty
	// means default-only. Populated from the stored uuid[] via uuidStrings, so a NULL
	// column reads as [] (never null), and pruned of stale ids on the GET path.
	SidebarCodexAccountIds []string `json:"sidebar_codex_account_ids"`
	// The four appearance override fields (PRD #1167): the user's RAW per-field
	// overrides (each NULL ⇒ JSON null ⇒ inherit the instance default). These are
	// the stored override values, NOT the resolved appearance — the resolved
	// appearance + defaults live on the session payload (me()), not here.
	AppearanceMode *string `json:"appearance_mode"`
	LightTheme     *string `json:"light_theme"`
	DarkTheme      *string `json:"dark_theme"`
	Typeface       *string `json:"typeface"`
	// DefaultHarness is the user's per-user default harness (PRD #1429 M1 / D3), nullable:
	// null = "no preference", so implicit run creation falls through D11 rather than pinning.
	// A non-null value is one of the closed claude|codex set (validateHarness gates the write).
	DefaultHarness *string `json:"default_harness"`
}

// userSettingsResponse reads the user's settings row (one GetUserSettings query,
// summary_model folded in) and writes the settings body, shared by the GET and PUT
// handlers so the two responses never drift.
//
// sidebarCodexOverride, when non-nil, replaces the stored sidebar_codex_account_ids in the
// response with the caller-supplied set: the GET path passes the array
// PruneUserSidebarCodexAccounts returned so the surfaced set reflects the just-pruned value
// directly (PRD #1209 M1). A nil override uses the value read from GetUserSettings — the PUT
// path, and the GET path's best-effort fallback when the prune UPDATE failed.
func (h *Handler) userSettingsResponse(w http.ResponseWriter, r *http.Request, userID uuid.UUID, sidebarCodexOverride *[]uuid.UUID) {
	s, err := h.q.GetUserSettings(r.Context(), userID)
	if err != nil {
		slog.Error("get user settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	sidebarCodexIDs := s.SidebarCodexAccountIds
	if sidebarCodexOverride != nil {
		sidebarCodexIDs = *sidebarCodexOverride
	}
	// The deprecated legacy default_model is a PROJECTION of the effective harness's lane
	// (PRD #1551 D3), NOT the stored default_model column: GET resolves the effective harness
	// with the SAME D11 resolver run creation uses (honouring a usable stored default_harness),
	// so a stale client reading default_model sees the value its harness would actually run.
	// A zero-credential user resolves to Claude, so the projection never fails on credentials.
	effHarness, err := h.resolveSettingsHarness(r.Context(), userID)
	if err != nil {
		slog.Error("resolve settings harness", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	legacyModel := s.DefaultClaudeModel
	if effHarness == workersvc.HarnessCodex {
		legacyModel = s.DefaultCodexModel
	}
	// summary_model rides the GetUserSettings one-row read (it is the same users row),
	// so the settings surface reads it from `s` — no separate query. GetUserSummaryModel
	// stays the narrow read for issue-run claim assembly, where no other user field is
	// needed (PRD #362 M2).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"settings": userSettingsDTO{
			DefaultModel:           textPtrValue(legacyModel.Valid, legacyModel.String),
			DefaultClaudeModel:     textPtrValue(s.DefaultClaudeModel.Valid, s.DefaultClaudeModel.String),
			DefaultCodexModel:      textPtrValue(s.DefaultCodexModel.Valid, s.DefaultCodexModel.String),
			DefaultEffort:          textPtrValue(s.DefaultEffort.Valid, s.DefaultEffort.String),
			JudgeModel:             textPtrValue(s.JudgeModel.Valid, s.JudgeModel.String),
			SummaryModel:           textPtrValue(s.SummaryModel.Valid, s.SummaryModel.String),
			Theme:                  textPtrValue(s.Theme.Valid, s.Theme.String),
			MrReworkEnabled:        boolPtrValue(s.MrReworkEnabled),
			SidebarTokenIds:        uuidStrings(s.SidebarTokenIds),
			SidebarCodexAccountIds: uuidStrings(sidebarCodexIDs),
			AppearanceMode:         textPtrValue(s.AppearanceMode.Valid, s.AppearanceMode.String),
			LightTheme:             textPtrValue(s.LightTheme.Valid, s.LightTheme.String),
			DarkTheme:              textPtrValue(s.DarkTheme.Valid, s.DarkTheme.String),
			Typeface:               textPtrValue(s.Typeface.Valid, s.Typeface.String),
			DefaultHarness:         textPtrValue(s.DefaultHarness.Valid, s.DefaultHarness.String),
		},
	})
}

// GetMySettings returns the current user's own settings. Session-authenticated
// and own-user only (no admin path — a user's model and theme are theirs).
//
// The GET path ATOMICALLY prunes stale sidebar Codex-account ids first (PRD #1209 M1):
// PruneUserSidebarCodexAccounts drops in ONE UPDATE any id that no longer names a linked
// account (its account was deleted or its last codex_auth alias unlinked), so a since-
// unlinked id never lingers in the surfaced set. It is a single atomic UPDATE — never a
// read-merge-write — so it cannot lose a concurrent SetUserSidebarCodexAccounts.
//
// The prune is BEST-EFFORT: a failed cosmetic sidebar prune must NOT break the core
// settings read (the Claude/anthropic sidebar path has no such failure mode). On success
// the response uses the pruned array Prune returns; on error we log a warning and fall
// back to the currently-stored sidebar_codex_account_ids. The fallback set is still
// correct — the meter/sidebar surfaces render only accounts that are actually linked, so a
// lingering stale id is harmless and gets pruned on the next successful GET.
func (h *Handler) GetMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	pruned, err := h.q.PruneUserSidebarCodexAccounts(r.Context(), user.ID)
	if err != nil {
		slog.Warn("prune user sidebar codex accounts (best effort)", "error", err)
		h.userSettingsResponse(w, r, user.ID, nil)
		return
	}
	h.userSettingsResponse(w, r, user.ID, &pruned)
}

// PutMySettings updates the current user's own settings with PATCH-like
// semantics: a field is applied only when present in the body — absent leaves
// the stored value untouched, present-null clears it. That lets the worker-model
// card and the Appearance theme picker save independently over the one endpoint
// without clobbering each other. default_model is validated by the same
// validateModel used for agent templates (PRD #17); default_effort by the
// closed-enum validateEffort (PRD #617); theme by the same theme registry the
// admin default surface uses (PRD #21), so no pair of write paths can drift.
//
// The handler runs in two phases: it DECODES + VALIDATES every present field
// first, and only then WRITES. A bad field can therefore no longer leave an
// earlier field already persisted — e.g. {theme:"mission", light_theme:"ember"}
// (a dark id in the light slot) is a clean 400 that writes nothing, where the old
// order committed users.theme before the light_theme validation ran.
func (h *Handler) PutMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// RawMessage distinguishes an absent field (nil) from a present null (the
	// bytes `null`); a plain *string cannot, and absent must mean "unchanged".
	var req struct {
		DefaultModel json.RawMessage `json:"default_model"`
		// The two per-harness lanes (PRD #1551 M1 / D2), same absent/null/value tri-state
		// as default_model: absent ⇒ lane unchanged, null ⇒ clear to inherit, a validated
		// value ⇒ set. Cross-vocabulary rejection uses closed lists only (D3).
		DefaultClaudeModel json.RawMessage `json:"default_claude_model"`
		DefaultCodexModel  json.RawMessage `json:"default_codex_model"`
		DefaultEffort      json.RawMessage `json:"default_effort"`
		JudgeModel         json.RawMessage `json:"judge_model"`
		SummaryModel       json.RawMessage `json:"summary_model"`
		Theme              json.RawMessage `json:"theme"`
		MrReworkEnabled    json.RawMessage `json:"mr_rework_enabled"`
		SidebarTokenIds    json.RawMessage `json:"sidebar_token_ids"`
		// The linked Codex accounts on the sidebar rail (PRD #1209 M1): same
		// absent/present tri-state as sidebar_token_ids — absent leaves the stored set,
		// present (a JSON array, or null) replaces it. Unlike sidebar_token_ids, a
		// well-formed id that is not one of the caller's linked accounts is a 400, not a
		// silent drop (the implicit default account is the one exception — excluded, not
		// rejected).
		SidebarCodexAccountIds json.RawMessage `json:"sidebar_codex_account_ids"`
		// The four appearance override fields (PRD #1167), same absent/null/value
		// tri-state as theme: absent ⇒ column unchanged, null ⇒ clear to NULL
		// (inherit), value ⇒ validated then set.
		AppearanceMode json.RawMessage `json:"appearance_mode"`
		LightTheme     json.RawMessage `json:"light_theme"`
		DarkTheme      json.RawMessage `json:"dark_theme"`
		Typeface       json.RawMessage `json:"typeface"`
		// default_harness (PRD #1429 M1 / D3), same absent/null/value tri-state as the model
		// fields: absent ⇒ column unchanged, null ⇒ clear to NULL ("no preference"), a
		// claude|codex value ⇒ validated then set.
		DefaultHarness json.RawMessage `json:"default_harness"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// ---- Phase 1: decode + validate every present field. No column is written
	// until all fields validate.

	var modelVal pgtype.Text
	modelPresent := req.DefaultModel != nil
	if modelPresent {
		var raw *string
		if err := json.Unmarshal(req.DefaultModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid default_model")
			return
		}
		v, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		modelVal = v
	}

	var claudeVal pgtype.Text
	claudePresent := req.DefaultClaudeModel != nil
	if claudePresent {
		var raw *string
		if err := json.Unmarshal(req.DefaultClaudeModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid default_claude_model")
			return
		}
		v, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		// Closed-list cross-vocabulary guard (D3): a curated Codex id must not sit in the
		// Claude lane. No prefix guessing — only the three known Codex ids are rejected.
		if err := rejectCodexIDInClaudeLane("default_claude_model", v); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		claudeVal = v
	}

	var codexVal pgtype.Text
	codexPresent := req.DefaultCodexModel != nil
	if codexPresent {
		var raw *string
		if err := json.Unmarshal(req.DefaultCodexModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid default_codex_model")
			return
		}
		v, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		// Closed-list cross-vocabulary guard (D3): a known Claude alias must not sit in the
		// Codex lane. Only the closed alias set is rejected; a validated custom id passes.
		if err := rejectClaudeAliasInCodexLane("default_codex_model", v); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		codexVal = v
	}

	var effortVal pgtype.Text
	effortPresent := req.DefaultEffort != nil
	if effortPresent {
		var raw *string
		if err := json.Unmarshal(req.DefaultEffort, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid default_effort")
			return
		}
		v, err := validateEffort(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		effortVal = v
	}

	var judgeVal pgtype.Text
	judgePresent := req.JudgeModel != nil
	if judgePresent {
		var raw *string
		if err := json.Unmarshal(req.JudgeModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid judge_model")
			return
		}
		// Same validator as default_model (PRD #69 M2, Decision 4): nil/blank
		// trims to NULL = inherit the instance judge_model; a bad model is a 400.
		v, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		judgeVal = v
	}

	var summaryVal pgtype.Text
	summaryPresent := req.SummaryModel != nil
	if summaryPresent {
		var raw *string
		if err := json.Unmarshal(req.SummaryModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid summary_model")
			return
		}
		// Same validator as judge_model (PRD #362 M2): nil/blank trims to NULL =
		// inherit the instance summary_model; a bad model is a 400.
		v, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		summaryVal = v
	}

	// The legacy theme field (DEPRECATED, PRD #1167) still writes users.theme via
	// SetUserTheme for the deprecated trio's consistency, AND folds into the
	// appearance patch below: a valid theme maps into its polarity slot + mode; a
	// null/blank theme (themeVal.Valid == false) clears users.theme AND the
	// migrated appearance override (00203 backfilled dark_theme + mode from theme),
	// so an old client's "use default" reset actually clears the effective pref.
	var themeVal pgtype.Text
	themePresent := req.Theme != nil
	if themePresent {
		var raw *string
		if err := json.Unmarshal(req.Theme, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid theme")
			return
		}
		v, err := validateTheme(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		themeVal = v
	}

	modePresent, modeDelta, err := decodeAppearanceField(req.AppearanceMode, "appearance_mode", validateAppearanceMode)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	lightPresent, lightDelta, err := decodeAppearanceField(req.LightTheme, "light_theme", validateLightTheme)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	darkPresent, darkDelta, err := decodeAppearanceField(req.DarkTheme, "dark_theme", validateDarkTheme)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	typefacePresent, typefaceDelta, err := decodeAppearanceField(req.Typeface, "typeface", validateTypeface)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	var harnessVal pgtype.Text
	harnessPresent := req.DefaultHarness != nil
	if harnessPresent {
		var raw *string
		if err := json.Unmarshal(req.DefaultHarness, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid default_harness")
			return
		}
		// PRD #1429 M1 / D3: present-null clears to NULL ("no preference"); a claude|codex
		// value sets the pin; anything else is a 400 (validateHarness). Validated in phase 1
		// with the other fields so a bad harness leaves nothing half-written.
		v, err := validateHarness(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		harnessVal = v
	}

	var mrVal pgtype.Bool
	mrPresent := req.MrReworkEnabled != nil
	if mrPresent {
		// PATCH semantics like the model fields: present-bool sets the opt-in,
		// present-null clears back to NULL = the default-ON state (PRD #700 M5). A
		// non-bool body is a 400. There is no closed-enum to validate — a bool is a
		// bool — so the value maps straight to a nullable column.
		var raw *bool
		if err := json.Unmarshal(req.MrReworkEnabled, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid mr_rework_enabled")
			return
		}
		if raw != nil {
			mrVal = pgtype.Bool{Bool: *raw, Valid: true}
		}
	}

	var sidebarIDs []uuid.UUID
	sidebarPresent := req.SidebarTokenIds != nil
	if sidebarPresent {
		var raw *[]string
		if err := json.Unmarshal(req.SidebarTokenIds, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid sidebar_token_ids")
			return
		}
		ids, err := h.validateSidebarTokenIds(r.Context(), user.ID, raw)
		if err != nil {
			// A store fault during the ownership check is OUR failure, not the
			// client's: 500, like every other store-error path in this handler.
			// The 400 arm is reserved for genuine request defects (a non-UUID
			// entry, an oversized list).
			if errors.Is(err, errSidebarStore) {
				httpx.Error(w, http.StatusInternalServerError, "internal error")
			} else {
				httpx.Error(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		sidebarIDs = ids
	}

	var sidebarCodexIDs []uuid.UUID
	sidebarCodexPresent := req.SidebarCodexAccountIds != nil
	if sidebarCodexPresent {
		var raw *[]string
		if err := json.Unmarshal(req.SidebarCodexAccountIds, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid sidebar_codex_account_ids")
			return
		}
		ids, err := h.validateSidebarCodexAccountIds(r.Context(), user.ID, raw)
		if err != nil {
			// Same split as sidebar_token_ids: a store fault is a 500, a genuine request
			// defect (a non-UUID entry, an oversized list, or an id that is not one of the
			// caller's linked accounts) is a 400 with an IDENTICAL message across the
			// unknown / api-key / other-user cases (no existence oracle).
			if errors.Is(err, errCodexSidebarStore) {
				httpx.Error(w, http.StatusInternalServerError, "internal error")
			} else {
				httpx.Error(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		sidebarCodexIDs = ids
	}

	// The harness/model group (PRD #1551 M1 / D1): default_harness, both lanes and the legacy
	// default_model save together in ONE statement. Resolve the target harness, bridge a
	// legacy-only default_model into its lane (or require it to equal an explicit lane), and
	// compute the effective target-lane value the legacy column must mirror — all in phase 1,
	// so a conflict is a clean 400 that writes nothing. groupParams is nil when no grouped
	// field is present.
	groupParams, ok, err := h.buildHarnessModels(r.Context(), user.ID, harnessModelInputs{
		modelPresent:   modelPresent,
		modelVal:       modelVal,
		claudePresent:  claudePresent,
		claudeVal:      claudeVal,
		codexPresent:   codexPresent,
		codexVal:       codexVal,
		harnessPresent: harnessPresent,
		harnessVal:     harnessVal,
	})
	if err != nil {
		switch {
		case errors.Is(err, errHarnessModelConflict):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, errHarnessModelCrossVocab):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("resolve harness models group", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// ---- Phase 2: writes. Every present field has validated, so nothing below
	// leaves the row half-updated on a bad input. Each SetUser* is its own
	// single-column statement (PATCH semantics); the four appearance columns go in
	// ONE atomic conditional UPDATE (no read-merge-write, so two concurrent saves
	// of different fields cannot clobber each other).

	if ok {
		if _, err := h.q.SetUserHarnessModels(r.Context(), groupParams); err != nil {
			slog.Error("set user harness models", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if effortPresent {
		if _, err := h.q.SetUserDefaultEffort(r.Context(), store.SetUserDefaultEffortParams{
			ID:            user.ID,
			DefaultEffort: effortVal,
		}); err != nil {
			slog.Error("set user default effort", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if judgePresent {
		if _, err := h.q.SetUserJudgeModel(r.Context(), store.SetUserJudgeModelParams{
			ID:         user.ID,
			JudgeModel: judgeVal,
		}); err != nil {
			slog.Error("set user judge model", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if summaryPresent {
		if _, err := h.q.SetUserSummaryModel(r.Context(), store.SetUserSummaryModelParams{
			ID:           user.ID,
			SummaryModel: summaryVal,
		}); err != nil {
			slog.Error("set user summary model", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if themePresent {
		if _, err := h.q.SetUserTheme(r.Context(), store.SetUserThemeParams{
			ID:    user.ID,
			Theme: themeVal,
		}); err != nil {
			slog.Error("set user theme", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// Appearance patch (PRD #1167 M2). Build the four per-field (set, value) deltas:
	// an explicit appearance field wins, otherwise the legacy theme folds in — a
	// valid theme into its polarity slot + mode, a cleared theme clearing the
	// migrated dark_theme + mode. Unset fields carry set=false and the statement
	// keeps their current column, so no prior read is needed.
	setMode, mode := modePresent, modeDelta
	setLight, light := lightPresent, lightDelta
	setDark, dark := darkPresent, darkDelta
	setTypeface, typeface := typefacePresent, typefaceDelta
	if themePresent {
		if themeVal.Valid {
			if theme.Polarity(themeVal.String) == theme.PolarityLight {
				if !setLight {
					setLight, light = true, pgtype.Text{String: themeVal.String, Valid: true}
				}
				if !setMode {
					setMode, mode = true, pgtype.Text{String: theme.ModeLight, Valid: true}
				}
			} else {
				if !setDark {
					setDark, dark = true, pgtype.Text{String: themeVal.String, Valid: true}
				}
				if !setMode {
					setMode, mode = true, pgtype.Text{String: theme.ModeDark, Valid: true}
				}
			}
		} else {
			// theme:null ("use default") clears the migrated appearance override so
			// the reset takes effect (finding: legacy reset left the new pref stale).
			if !setMode {
				setMode = true
			}
			if !setDark {
				setDark = true
			}
		}
	}
	if setMode || setLight || setDark || setTypeface {
		if _, err := h.q.SetUserAppearance(r.Context(), store.SetUserAppearanceParams{
			ID:             user.ID,
			SetMode:        setMode,
			AppearanceMode: mode,
			SetLight:       setLight,
			LightTheme:     light,
			SetDark:        setDark,
			DarkTheme:      dark,
			SetTypeface:    setTypeface,
			Typeface:       typeface,
		}); err != nil {
			slog.Error("set user appearance", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if mrPresent {
		if _, err := h.q.SetUserMrReworkEnabled(r.Context(), store.SetUserMrReworkEnabledParams{
			ID:              user.ID,
			MrReworkEnabled: mrVal,
		}); err != nil {
			slog.Error("set user mr rework enabled", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if sidebarPresent {
		if _, err := h.q.SetUserSidebarTokens(r.Context(), store.SetUserSidebarTokensParams{
			ID:              user.ID,
			SidebarTokenIds: sidebarIDs,
		}); err != nil {
			slog.Error("set user sidebar tokens", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if sidebarCodexPresent {
		if _, err := h.q.SetUserSidebarCodexAccounts(r.Context(), store.SetUserSidebarCodexAccountsParams{
			ID:                     user.ID,
			SidebarCodexAccountIds: sidebarCodexIDs,
		}); err != nil {
			slog.Error("set user sidebar codex accounts", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	h.userSettingsResponse(w, r, user.ID, nil)
}

// maxSidebarTokenIds bounds the request list before any per-id work; nobody
// legitimately holds hundreds of tokens, and an unbounded list is an invitation.
const maxSidebarTokenIds = 100

// errSidebarStore marks a store fault inside validateSidebarTokenIds, so the
// caller can answer 500 for it while every other validation error stays a 400.
// The message is what a 500 body would say anyway; the identity is what matters.
var errSidebarStore = errors.New("internal error")

// validateSidebarTokenIds maps the request list to the stored set: nil or empty
// clears back to default-only; each entry must parse as a UUID, and any id that
// is not one of the CALLER'S OWN anthropic_token secrets is silently dropped
// rather than rejected — the web sends ids it read moments ago, and a token
// deleted from another tab must not fail the whole save. Duplicates collapse.
// The default token's id is dropped with the rest of the non-matching set left
// intact if present — storing it would be harmless, but the contract is that
// the stored list holds only non-default extras.
func (h *Handler) validateSidebarTokenIds(ctx context.Context, userID uuid.UUID, raw *[]string) ([]uuid.UUID, error) {
	if raw == nil || len(*raw) == 0 {
		return []uuid.UUID{}, nil
	}
	if len(*raw) > maxSidebarTokenIds {
		return nil, fmt.Errorf("sidebar_token_ids: at most %d entries", maxSidebarTokenIds)
	}
	secrets, err := h.q.ListUserSecretsForKind(ctx, store.ListUserSecretsForKindParams{
		UserID: userID,
		Kind:   "anthropic_token",
	})
	if err != nil {
		slog.Error("list secrets for sidebar-token filter", "error", err)
		return nil, errSidebarStore
	}
	owned := make(map[uuid.UUID]bool, len(secrets))
	for _, s := range secrets {
		// Only non-default tokens are storable; the default always shows.
		if !s.IsDefault {
			owned[s.ID] = true
		}
	}
	ids := make([]uuid.UUID, 0, len(*raw))
	seen := make(map[uuid.UUID]bool, len(*raw))
	for _, v := range *raw {
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("sidebar_token_ids: %q is not a valid id", v)
		}
		if owned[id] && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// errCodexSidebarStore marks a store fault inside validateSidebarCodexAccountIds (the
// codex sibling of errSidebarStore), so the caller answers 500 for it while every genuine
// request defect stays a 400. The message is what a 500 body would say anyway; the
// identity is what matters.
var errCodexSidebarStore = errors.New("internal error")

// errCodexSidebarNotLinked is the SINGLE 400 a well-formed id that is not one of the
// caller's linked Codex accounts produces (PRD #1209 M1). It is one message across all
// three miss cases — an unknown id, an api-key alias (which is 'static', never 'linked'),
// and another user's account — precisely so it is not an existence oracle: the client can
// never tell which of the three a rejected id was.
var errCodexSidebarNotLinked = errors.New("sidebar_codex_account_ids: id is not one of your linked Codex accounts")

// validateSidebarCodexAccountIds maps the request list to the stored set (PRD #1209 M1):
// nil or empty clears back to default-only; each entry must parse as a UUID (a bad one is
// a 400); duplicates collapse; the list is capped at maxSidebarTokenIds. UNLIKE
// validateSidebarTokenIds it does NOT silently drop a non-member — a well-formed id that
// is not one of the caller's LINKED subscription accounts is a 400 (errCodexSidebarNotLinked),
// with an identical message across the unknown / api-key / other-user cases. The ONE
// exception is the implicit default account (the account behind the default linked
// codex_auth alias): it always shows on the rail, so a submitted default id is EXCLUDED
// from the stored extras rather than rejected — the stored set never holds it. Membership
// is checked with the owner-scoped CountLinkedAliasesForCodexAccount (0 for an unknown id,
// an api-key, or a foreign account — the same indistinguishable answer).
func (h *Handler) validateSidebarCodexAccountIds(ctx context.Context, userID uuid.UUID, raw *[]string) ([]uuid.UUID, error) {
	if raw == nil || len(*raw) == 0 {
		return []uuid.UUID{}, nil
	}
	if len(*raw) > maxSidebarTokenIds {
		return nil, fmt.Errorf("sidebar_codex_account_ids: at most %d entries", maxSidebarTokenIds)
	}
	// Resolve the implicit default account once, so a submitted default id is excluded
	// (not rejected) below. uuid.Nil means the user has no default linked codex account.
	defaultAccountID, err := h.defaultCodexAccountID(ctx, userID)
	if err != nil {
		return nil, errCodexSidebarStore
	}
	ids := make([]uuid.UUID, 0, len(*raw))
	seen := make(map[uuid.UUID]bool, len(*raw))
	for _, v := range *raw {
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("sidebar_codex_account_ids: %q is not a valid id", v)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		// The default account is implicit — never stored among the explicit extras — so
		// excluding a submitted default id is correct, and it must NOT 400.
		if defaultAccountID != uuid.Nil && id == defaultAccountID {
			continue
		}
		n, err := h.q.CountLinkedAliasesForCodexAccount(ctx, store.CountLinkedAliasesForCodexAccountParams{
			UserID:            userID,
			ProviderAccountID: pgtype.UUID{Bytes: id, Valid: true},
		})
		if err != nil {
			slog.Error("count linked aliases for codex account", "error", err)
			return nil, errCodexSidebarStore
		}
		if n == 0 {
			return nil, errCodexSidebarNotLinked
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// defaultCodexAccountID resolves the account behind the user's DEFAULT linked codex_auth
// alias (PRD #1209 M1), or uuid.Nil when there is none (no default codex_auth, or it is
// not currently linked to an account). Owner-scoped throughout. Returns a non-nil error
// only on a real store fault; a missing default or an unlinked default is (uuid.Nil, nil).
func (h *Handler) defaultCodexAccountID(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	secretID, err := h.q.GetDefaultUserSecretID(ctx, store.GetDefaultUserSecretIDParams{
		UserID: userID,
		Kind:   store.KindCodexAuth,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, err
	}
	st, err := h.q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: secretID,
		UserID:       userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil
		}
		return uuid.Nil, err
	}
	if st.Status != "linked" || !st.ProviderAccountID.Valid {
		return uuid.Nil, nil
	}
	return uuid.UUID(st.ProviderAccountID.Bytes), nil
}

// uuidStrings renders a stored uuid[] for the DTO; a NULL column arrives as a
// nil slice and reads as the empty (default-only) choice.
func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// validateTheme maps a nullable theme override to its storage type: a nil
// pointer or a blank value becomes NULL (use the instance default); a known
// theme becomes a set pgtype.Text; an unknown theme is rejected. Shares
// theme.Validate with the admin default write path (same discipline as
// validateModel), so a value accepted here is accepted there and vice versa.
func validateTheme(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	v := strings.TrimSpace(*raw)
	if v == "" {
		return pgtype.Text{}, nil
	}
	if err := theme.Validate(v); err != nil {
		return pgtype.Text{}, err
	}
	return pgtype.Text{String: v, Valid: true}, nil
}

// validateHarness maps a nullable default_harness override to its storage type (PRD #1429
// M1 / D3): a nil pointer clears to NULL ("no preference" — implicit run creation falls
// through D11), a "claude"/"codex" value becomes a set pgtype.Text, and anything else is a
// 400. The closed set mirrors the users_default_harness_check CHECK (migration 00226); the
// enum tokens come from workersvc so the API and the resolver cannot drift. Unlike the model
// validators it does NOT trim a blank string to NULL — only JSON null clears — so a bad value
// is a clean reject rather than a silent inherit.
func validateHarness(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	switch *raw {
	case string(workersvc.HarnessClaude), string(workersvc.HarnessCodex):
		return pgtype.Text{String: *raw, Valid: true}, nil
	default:
		return pgtype.Text{}, fmt.Errorf("default_harness: must be one of %s, %s", workersvc.HarnessClaude, workersvc.HarnessCodex)
	}
}

// curatedCodexModels is the closed Codex model vocabulary (PRD #1551 D3), a hand-kept
// mirror of workersvc.codexModels and the web CLAUDE_MODEL_ALIASES sibling. Used ONLY by
// the closed-list cross-vocabulary guard: a value in this set is rejected from the Claude
// lane. Deliberately NOT a prefix check — no `gpt-` guessing.
var curatedCodexModels = map[string]bool{
	"gpt-6-astra": true,
	"gpt-5.6-sol": true,
	"gpt-6-sol":   true,
}

// knownClaudeAliases is the closed Claude alias set the web ModelSelect curates
// (CLAUDE_MODEL_ALIASES: opus, sonnet, haiku, fable). Used ONLY by the cross-vocabulary
// guard: one of these is rejected from the Codex lane. A full custom Claude id (e.g.
// claude-opus-4-8) is NOT in this set and passes — the guard is a closed list, not a
// `claude-` prefix check (D3).
var knownClaudeAliases = map[string]bool{
	"opus":   true,
	"sonnet": true,
	"haiku":  true,
	"fable":  true,
}

// rejectCodexIDInClaudeLane fails when a validated model value is a curated Codex id sitting
// in a Claude-vocabulary field (PRD #1551 D3). A NULL/blank (clear) value never trips it.
func rejectCodexIDInClaudeLane(field string, v pgtype.Text) error {
	if v.Valid && curatedCodexModels[v.String] {
		return fmt.Errorf("%s: %q is a Codex model id and cannot be a Claude model default", field, v.String)
	}
	return nil
}

// rejectClaudeAliasInCodexLane fails when a validated model value is a known Claude alias
// sitting in a Codex-vocabulary field (PRD #1551 D3). A NULL/blank (clear) value never trips
// it, and a validated custom id passes.
func rejectClaudeAliasInCodexLane(field string, v pgtype.Text) error {
	if v.Valid && knownClaudeAliases[v.String] {
		return fmt.Errorf("%s: %q is a Claude model alias and cannot be a Codex model default", field, v.String)
	}
	return nil
}

// errHarnessModelConflict is the 400 when a legacy default_model and its target lane's
// explicit value are both present and disagree (PRD #1551 D3); nothing is written.
var errHarnessModelConflict = errors.New("default_model conflicts with the explicit model lane for the target harness")

// errHarnessModelCrossVocab is the 400 when a legacy default_model would land in a lane whose
// closed vocabulary rejects it (PRD #1551 D3).
var errHarnessModelCrossVocab = errors.New("default_model is not valid for the target harness lane")

// settingsHarnessResolver is the narrow seam over h.wsvc the settings surface uses to resolve
// the effective harness for the legacy default_model projection/write target (PRD #1551 D3).
// *workersvc.Service satisfies it. Kept as an interface so the dependency is explicit and the
// nil-service path (unit tests, pre-wiring) deterministically projects/writes Claude.
type settingsHarnessResolver interface {
	ResolveSettingsHarness(ctx context.Context, userID uuid.UUID) (workersvc.Harness, error)
	ResolveSettingsHarnessWithDefault(ctx context.Context, userID uuid.UUID, proposed *workersvc.Harness) (workersvc.Harness, error)
}

var _ settingsHarnessResolver = (*workersvc.Service)(nil)

// resolveSettingsHarness resolves the effective harness via the D11 resolver on h.wsvc. A nil
// worker service (unit tests, or a Handler constructed before wsvc is wired) deterministically
// resolves to Claude, matching D3's "credential availability never makes settings fail" and the
// zero-credential projection. Only a genuine store error propagates (mapped to 500 by callers).
func (h *Handler) resolveSettingsHarness(ctx context.Context, userID uuid.UUID) (workersvc.Harness, error) {
	if h.wsvc == nil {
		return workersvc.HarnessClaude, nil
	}
	return h.wsvc.ResolveSettingsHarness(ctx, userID)
}

// resolveSettingsHarnessWithDefault projects a proposed harness preference through D11,
// including the usable-credential fallback. The legacy request field may target a different
// lane under D3, but the compatibility column must mirror the harness a run would use after
// the grouped write. A nil worker service retains the unit-test/zero-credential Claude floor.
func (h *Handler) resolveSettingsHarnessWithDefault(ctx context.Context, userID uuid.UUID, proposed pgtype.Text) (workersvc.Harness, error) {
	if h.wsvc == nil {
		return workersvc.HarnessClaude, nil
	}
	var preference *workersvc.Harness
	if proposed.Valid {
		v := workersvc.Harness(proposed.String)
		preference = &v
	}
	return h.wsvc.ResolveSettingsHarnessWithDefault(ctx, userID, preference)
}

// harnessModelInputs is the validated (phase-1) grouped-field state buildHarnessModels folds
// into one SetUserHarnessModels statement.
type harnessModelInputs struct {
	modelPresent   bool
	modelVal       pgtype.Text
	claudePresent  bool
	claudeVal      pgtype.Text
	codexPresent   bool
	codexVal       pgtype.Text
	harnessPresent bool
	harnessVal     pgtype.Text
}

// buildHarnessModels folds the grouped harness/model fields into ONE SetUserHarnessModels
// statement (PRD #1551 M1 / D1, D3). It returns ok=false when no grouped field is present (no
// write). Otherwise it:
//   - resolves the TARGET harness: an explicit non-null default_harness wins with no credential
//     check; an explicit null clears to Claude; an absent harness uses ResolveSettingsHarness
//     (a usable stored default is honoured; zero credentials ⇒ Claude);
//   - bridges a legacy-only default_model into the target lane, or — when that lane's explicit
//     value is ALSO present — requires them equal (errHarnessModelConflict otherwise), and runs
//     the closed-list cross-vocabulary guard against the target lane (errHarnessModelCrossVocab);
//   - projects the post-write effective harness through D11, independently of the legacy
//     request's target lane, then mirrors that lane into default_model for image rollback (D2).
//
// All reads/decisions happen before the single write, so a conflict/cross-vocab 400 writes
// nothing. A store error (resolver or the stored-lane read) is returned raw for a 500.
func (h *Handler) buildHarnessModels(ctx context.Context, userID uuid.UUID, in harnessModelInputs) (store.SetUserHarnessModelsParams, bool, error) {
	if !in.modelPresent && !in.claudePresent && !in.codexPresent && !in.harnessPresent {
		return store.SetUserHarnessModelsParams{}, false, nil
	}

	// Target harness for routing a legacy default_model input (D3). A newly supplied
	// preference is not necessarily usable; the compatibility mirror is resolved below.
	var target workersvc.Harness
	switch {
	case in.harnessPresent && in.harnessVal.Valid:
		target = workersvc.Harness(in.harnessVal.String)
	case in.harnessPresent && !in.harnessVal.Valid:
		target = workersvc.HarnessClaude
	default:
		t, err := h.resolveSettingsHarness(ctx, userID)
		if err != nil {
			return store.SetUserHarnessModelsParams{}, false, err
		}
		target = t
	}

	// Local copies so a legacy bridge can promote a lane to "present" without mutating the
	// caller's phase-1 state.
	claudePresent, claudeVal := in.claudePresent, in.claudeVal
	codexPresent, codexVal := in.codexPresent, in.codexVal

	if in.modelPresent {
		if target == workersvc.HarnessCodex {
			if err := rejectClaudeAliasInCodexLane("default_model", in.modelVal); err != nil {
				return store.SetUserHarnessModelsParams{}, false, fmt.Errorf("%w: %s", errHarnessModelCrossVocab, err)
			}
			if codexPresent {
				if !textEqual(codexVal, in.modelVal) {
					return store.SetUserHarnessModelsParams{}, false, errHarnessModelConflict
				}
			} else {
				codexPresent, codexVal = true, in.modelVal
			}
		} else {
			if err := rejectCodexIDInClaudeLane("default_model", in.modelVal); err != nil {
				return store.SetUserHarnessModelsParams{}, false, fmt.Errorf("%w: %s", errHarnessModelCrossVocab, err)
			}
			if claudePresent {
				if !textEqual(claudeVal, in.modelVal) {
					return store.SetUserHarnessModelsParams{}, false, errHarnessModelConflict
				}
			} else {
				claudePresent, claudeVal = true, in.modelVal
			}
		}
	}

	// Resolve the harness a new run would select AFTER this write. A request with an
	// explicit default_harness uses that proposed preference, including null; an absent
	// field uses the stored preference already resolved as target above. This is distinct
	// from the legacy input's target lane: an unusable Codex pin may still target the
	// Codex lane for that input while new runs and the compatibility mirror use Claude.
	mirror := target
	if in.harnessPresent {
		var err error
		mirror, err = h.resolveSettingsHarnessWithDefault(ctx, userID, in.harnessVal)
		if err != nil {
			return store.SetUserHarnessModelsParams{}, false, err
		}
	}

	// A lane absent from the request may supply either the legacy bridge target or the
	// effective compatibility mirror. Read once when any lane is absent; the grouped web
	// save supplies both and needs no extra read.
	effClaude, effCodex := claudeVal, codexVal
	needStored := !claudePresent || !codexPresent
	if needStored {
		cur, err := h.q.GetUserSettings(ctx, userID)
		if err != nil {
			return store.SetUserHarnessModelsParams{}, false, err
		}
		if !claudePresent {
			effClaude = cur.DefaultClaudeModel
		}
		if !codexPresent {
			effCodex = cur.DefaultCodexModel
		}
	}
	legacyVal := effClaude
	if mirror == workersvc.HarnessCodex {
		legacyVal = effCodex
	}

	return store.SetUserHarnessModelsParams{
		SetHarness:         in.harnessPresent,
		DefaultHarness:     in.harnessVal,
		SetClaude:          claudePresent,
		DefaultClaudeModel: claudeVal,
		SetCodex:           codexPresent,
		DefaultCodexModel:  codexVal,
		SetDefaultModel:    true,
		DefaultModel:       legacyVal,
		ID:                 userID,
	}, true, nil
}

// textEqual reports whether two nullable text values are equal, NULL==NULL included.
func textEqual(a, b pgtype.Text) bool {
	if a.Valid != b.Valid {
		return false
	}
	if !a.Valid {
		return true
	}
	return a.String == b.String
}

// decodeAppearanceField decodes one tri-state appearance field (PRD #1167) into
// its storage delta: raw nil ⇒ absent (present=false, leave the column
// unchanged); JSON null ⇒ present clear (a zero pgtype.Text, Valid=false); a
// string value ⇒ validated then set. A malformed body is a 400 ("invalid
// <field>"); a value that fails the validator surfaces the validator's own
// field-prefixed message so the client sees which field and why. Unlike the
// legacy theme field, only JSON null clears — a blank/whitespace string is a
// value and is rejected by the validator.
func decodeAppearanceField(raw json.RawMessage, field string, validate func(string) error) (bool, pgtype.Text, error) {
	if raw == nil {
		return false, pgtype.Text{}, nil
	}
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return true, pgtype.Text{}, fmt.Errorf("invalid %s", field)
	}
	if v == nil {
		return true, pgtype.Text{}, nil
	}
	if err := validate(*v); err != nil {
		return true, pgtype.Text{}, err
	}
	return true, pgtype.Text{String: *v, Valid: true}, nil
}

// validateAppearanceMode gates the appearance_mode field against the closed
// system|light|dark set, returning the field-prefixed 400 message on a miss.
func validateAppearanceMode(v string) error {
	if !theme.ValidMode(v) {
		return fmt.Errorf("appearance_mode: must be one of %s, %s, %s", theme.ModeSystem, theme.ModeLight, theme.ModeDark)
	}
	return nil
}

// validateLightTheme gates the light_theme field: a known theme whose polarity is
// light. A wrong-polarity or unknown id gets theme.ValidateFor's distinct message,
// prefixed with the field name.
func validateLightTheme(v string) error {
	if err := theme.ValidateFor(theme.PolarityLight, v); err != nil {
		return fmt.Errorf("light_theme: %s", err)
	}
	return nil
}

// validateDarkTheme is the dark-slot companion to validateLightTheme.
func validateDarkTheme(v string) error {
	if err := theme.ValidateFor(theme.PolarityDark, v); err != nil {
		return fmt.Errorf("dark_theme: %s", err)
	}
	return nil
}

// validateTypeface gates the typeface field against the closed system|plex set.
func validateTypeface(v string) error {
	if !theme.ValidTypeface(v) {
		return fmt.Errorf("typeface: must be one of %s, %s", theme.TypefaceSystem, theme.TypefacePlex)
	}
	return nil
}
