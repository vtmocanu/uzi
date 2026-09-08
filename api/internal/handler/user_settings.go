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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/theme"
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
	DefaultModel  *string `json:"default_model"`
	DefaultEffort *string `json:"default_effort"`
	JudgeModel    *string `json:"judge_model"`
	SummaryModel  *string `json:"summary_model"`
	// Theme is the DEPRECATED legacy single-theme override (PRD #21); kept one
	// release. The appearance override lives in the four fields below (PRD #1167).
	Theme *string `json:"theme"`
	// MrReworkEnabled is the per-user opt-in to the MR review watcher (PRD #700 M5),
	// which ships ON. null means "unset" = inherit the default-ON semantics (a
	// NULL/absent user value is read as enabled); an explicit false is the opt-OUT.
	MrReworkEnabled *bool    `json:"mr_rework_enabled"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
	// The four appearance override fields (PRD #1167): the user's RAW per-field
	// overrides (each NULL ⇒ JSON null ⇒ inherit the instance default). These are
	// the stored override values, NOT the resolved appearance — the resolved
	// appearance + defaults live on the session payload (me()), not here.
	AppearanceMode *string `json:"appearance_mode"`
	LightTheme     *string `json:"light_theme"`
	DarkTheme      *string `json:"dark_theme"`
	Typeface       *string `json:"typeface"`
}

// userSettingsResponse reads the user's settings row (one GetUserSettings query,
// summary_model folded in) and writes the settings body, shared by the GET and PUT
// handlers so the two responses never drift.
func (h *Handler) userSettingsResponse(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	s, err := h.q.GetUserSettings(r.Context(), userID)
	if err != nil {
		slog.Error("get user settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// summary_model rides the GetUserSettings one-row read (it is the same users row),
	// so the settings surface reads it from `s` — no separate query. GetUserSummaryModel
	// stays the narrow read for issue-run claim assembly, where no other user field is
	// needed (PRD #362 M2).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"settings": userSettingsDTO{
			DefaultModel:    textPtrValue(s.DefaultModel.Valid, s.DefaultModel.String),
			DefaultEffort:   textPtrValue(s.DefaultEffort.Valid, s.DefaultEffort.String),
			JudgeModel:      textPtrValue(s.JudgeModel.Valid, s.JudgeModel.String),
			SummaryModel:    textPtrValue(s.SummaryModel.Valid, s.SummaryModel.String),
			Theme:           textPtrValue(s.Theme.Valid, s.Theme.String),
			MrReworkEnabled: boolPtrValue(s.MrReworkEnabled),
			SidebarTokenIds: uuidStrings(s.SidebarTokenIds),
			AppearanceMode:  textPtrValue(s.AppearanceMode.Valid, s.AppearanceMode.String),
			LightTheme:      textPtrValue(s.LightTheme.Valid, s.LightTheme.String),
			DarkTheme:       textPtrValue(s.DarkTheme.Valid, s.DarkTheme.String),
			Typeface:        textPtrValue(s.Typeface.Valid, s.Typeface.String),
		},
	})
}

// GetMySettings returns the current user's own settings. Session-authenticated
// and own-user only (no admin path — a user's model and theme are theirs).
func (h *Handler) GetMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	h.userSettingsResponse(w, r, user.ID)
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
		DefaultModel    json.RawMessage `json:"default_model"`
		DefaultEffort   json.RawMessage `json:"default_effort"`
		JudgeModel      json.RawMessage `json:"judge_model"`
		SummaryModel    json.RawMessage `json:"summary_model"`
		Theme           json.RawMessage `json:"theme"`
		MrReworkEnabled json.RawMessage `json:"mr_rework_enabled"`
		SidebarTokenIds json.RawMessage `json:"sidebar_token_ids"`
		// The four appearance override fields (PRD #1167), same absent/null/value
		// tri-state as theme: absent ⇒ column unchanged, null ⇒ clear to NULL
		// (inherit), value ⇒ validated then set.
		AppearanceMode json.RawMessage `json:"appearance_mode"`
		LightTheme     json.RawMessage `json:"light_theme"`
		DarkTheme      json.RawMessage `json:"dark_theme"`
		Typeface       json.RawMessage `json:"typeface"`
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

	// ---- Phase 2: writes. Every present field has validated, so nothing below
	// leaves the row half-updated on a bad input. Each SetUser* is its own
	// single-column statement (PATCH semantics); the four appearance columns go in
	// ONE atomic conditional UPDATE (no read-merge-write, so two concurrent saves
	// of different fields cannot clobber each other).

	if modelPresent {
		if _, err := h.q.SetUserDefaultModel(r.Context(), store.SetUserDefaultModelParams{
			ID:           user.ID,
			DefaultModel: modelVal,
		}); err != nil {
			slog.Error("set user default model", "error", err)
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

	h.userSettingsResponse(w, r, user.ID)
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
