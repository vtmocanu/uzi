package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Reason codes of the two 500s PUT /api/me/model-settings distinguishes from a plain
// "internal error", so a client can tell "the write failed" from "the write may have happened".
const (
	reasonSettingsCommitUnknown       = "settings_commit_unknown"
	reasonSettingsSavedReadbackFailed = "settings_saved_readback_failed"
	msgSettingsCommitUnknown          = "the save may or may not have been applied; check with `uzi settings get`"
	msgSettingsSavedReadbackFailed    = "settings saved; reading them back failed"

	// reasonSettingsEffectiveReadbackFailed is the effective_error of a 200 whose settings were
	// saved and read back but whose effective-settings prediction could not be computed.
	reasonSettingsEffectiveReadbackFailed = "settings_effective_readback_failed"
)

const (
	// maxUnknownFieldErrors caps the unknown-key entries in the 400 "errors" array.
	maxUnknownFieldErrors = 5
	// maxEchoBytes bounds every caller-supplied string echoed into that array.
	maxEchoBytes = 64
)

// clipEcho returns s cut to at most maxEchoBytes bytes on a rune boundary.
func clipEcho(s string) string {
	if len(s) <= maxEchoBytes {
		return s
	}
	n := maxEchoBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// modelSettingsAllowlist is the closed set of keys PUT /api/me/model-settings accepts. Anything
// else, including the legacy default_model, appearance and every consent switch, is a 400
// naming the key.
var modelSettingsAllowlist = map[string]struct{}{
	"default_harness":          {},
	"planning_claude_model":    {},
	"planning_claude_effort":   {},
	"planning_codex_model":     {},
	"planning_codex_effort":    {},
	"default_claude_model":     {},
	"default_codex_model":      {},
	"default_effort":           {},
	"default_codex_effort":     {},
	"judge_model":              {},
	"codex_judge_model":        {},
	"judge_runs_on":            {},
	"summary_model":            {},
	"codex_summary_model":      {},
	"task_review_model":        {},
	"codex_task_review_model":  {},
	"task_review_effort":       {},
	"codex_task_review_effort": {},
	"cross_check_pins":         {},
}

// modelSettingsField is one decoded tri-state field: absent (present=false) leaves the column,
// present with an invalid val clears it to NULL, present with a valid val sets it.
type modelSettingsField struct {
	present bool
	val     pgtype.Text
}

// modelSettingsWrite is the validated phase-1 state writeModelSettings applies.
type modelSettingsWrite struct {
	claude, codex, effort, codexEffort      modelSettingsField
	judge, codexJudge, judgeRunsOn          modelSettingsField
	summary, codexSummary                   modelSettingsField
	taskReview, codexTaskReview             modelSettingsField
	taskReviewEffort, codexTaskReviewEffort modelSettingsField
	harness                                 modelSettingsField
	planningClaudeModel, planningClaudeEffort,
	planningCodexModel, planningCodexEffort modelSettingsField
	// crossCheckPins are the decoded cross_check_pins patches (collectCrossCheckPins); empty when
	// the key is absent or an empty list.
	crossCheckPins []store.PatchUserCrossCheckPinParams
}

func (w modelSettingsWrite) any() bool {
	if len(w.crossCheckPins) > 0 {
		return true
	}
	for _, f := range []modelSettingsField{w.claude, w.codex, w.effort, w.codexEffort,
		w.judge, w.codexJudge, w.judgeRunsOn, w.summary, w.codexSummary, w.taskReview, w.codexTaskReview, w.taskReviewEffort, w.codexTaskReviewEffort,
		w.harness, w.planningClaudeModel, w.planningClaudeEffort, w.planningCodexModel, w.planningCodexEffort} {
		if f.present {
			return true
		}
	}
	return false
}

// modelSettingsError is one entry of the 400 "errors" array. Stage, Harness and Member are set
// only for a cross_check_pins cell defect (Member is "model" or "effort"; a list-shape defect has
// none of them).
type modelSettingsError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Stage   string `json:"stage,omitempty"`
	Harness string `json:"harness,omitempty"`
	Member  string `json:"member,omitempty"`
}

// writeModelSettingsFieldError renders a single field-naming 400: {"error": "...", "field": "..."}.
// PUT /api/me/model-settings answers with writeModelSettingsErrors instead; this one remains for
// the agent-template pins route.
func writeModelSettingsFieldError(w http.ResponseWriter, field, message string) {
	httpx.JSON(w, http.StatusBadRequest, map[string]string{"error": message, "field": field})
}

// writeModelSettingsErrors renders the 400 for a rejected request: the FIRST error stays in the
// flat "error" and "field" keys (the shape every earlier client reads), and every error, first
// included, is listed in "errors". errs must not be empty.
func writeModelSettingsErrors(w http.ResponseWriter, errs []modelSettingsError) {
	httpx.JSON(w, http.StatusBadRequest, map[string]any{
		"error":  errs[0].Message,
		"field":  errs[0].Field,
		"errors": errs,
	})
}

// PutMyModelSettings updates the caller's own model and effort defaults (PRD #3264 M1) with the
// PutMySettings tri-state (absent leaves the column, null clears it, a value is validated and
// set), but over a closed allowlist and in ONE transaction: every present field is written or
// none is. It shares the per-field decoders with PutMySettings (settings_decode.go), so the two
// write paths accept and reject identically.
//
// Order: decode and validate everything, collecting every defect (nothing written; a 400 lists
// them in "errors", unknown keys capped at maxUnknownFieldErrors, the first also in "error" and "field"), resolve the settings harness (the
// same accepted pre-write race as PutMySettings), then Begin, lock the users row, write, commit,
// and only then read the settings back, then the effective settings, for the response
// {"settings": ..., "effective": ...}.
//
// Failure model:
//   - any error before Commit rolls back: a validation defect is a 400, anything else a 500
//     "internal error";
//   - a Commit error is a 500 with reason settings_commit_unknown, because the outcome is not
//     known to this process;
//   - a failed post-commit readback is a 500 with reason settings_saved_readback_failed: the
//     write is durable;
//   - a failed effective-settings readback after a good settings readback is still a 200, with
//     "effective": null and "effective_error": settings_effective_readback_failed.
func (h *Handler) PutMyModelSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req map[string]json.RawMessage
	if err := httpx.DecodeJSONStrict(r, &req); err != nil || req == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	in, errs := decodeModelSettingsRequest(req, user.ID)
	if len(errs) > 0 {
		writeModelSettingsErrors(w, errs)
		return
	}

	if in.any() {
		if err := h.writeModelSettings(r.Context(), user.ID, in); err != nil {
			var cu commitUnknownError
			if errors.As(err, &cu) {
				slog.Error("model settings commit outcome unknown", "error", err, "user", user.ID)
				httpx.ErrorReason(w, http.StatusInternalServerError, msgSettingsCommitUnknown, reasonSettingsCommitUnknown)
				return
			}
			slog.Error("write model settings", "error", err, "user", user.ID)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// Post-commit readback: the same {"settings": ...} body GET /api/me/settings returns.
	build := h.buildUserSettingsDTO
	if h.modelSettingsReadback != nil {
		build = func(r *http.Request, id uuid.UUID, _ *[]uuid.UUID) (userSettingsDTO, error) {
			return h.modelSettingsReadback(r, id)
		}
	}
	dto, err := build(r, user.ID, nil)
	if err != nil {
		httpx.ErrorReason(w, http.StatusInternalServerError, msgSettingsSavedReadbackFailed, reasonSettingsSavedReadbackFailed)
		return
	}

	// The effective-settings prediction is read after the settings, still after the commit. Its
	// failure never turns the saved write into a failed one: the body keeps the settings and
	// reports effective null plus effective_error.
	body := map[string]any{"settings": dto}
	if eff, err := h.readModelSettingsEffective(r.Context(), user.ID); err != nil {
		slog.Error("model settings effective readback", "error", err, "user", user.ID)
		body["effective"] = nil
		body["effective_error"] = reasonSettingsEffectiveReadbackFailed
	} else {
		body["effective"] = eff
	}
	httpx.JSON(w, http.StatusOK, body)
}

// readModelSettingsEffective computes the effective-settings body GET /api/me/effective-settings
// returns, through the modelSettingsEffectiveReadback seam when a test set one. Without a worker
// service the prediction cannot be built, which is a failure here, not a panic.
func (h *Handler) readModelSettingsEffective(ctx context.Context, userID uuid.UUID) (apitypes.EffectiveSettingsDTO, error) {
	if h.modelSettingsEffectiveReadback != nil {
		return h.modelSettingsEffectiveReadback(ctx, userID)
	}
	if h.wsvc == nil {
		return apitypes.EffectiveSettingsDTO{}, errors.New("no worker service")
	}
	return h.buildEffectiveSettings(ctx, userID)
}

// decodeModelSettingsRequest is phase 1 of PUT /api/me/model-settings: it checks the keys against
// the allowlist, then decodes and validates every present field, collecting EVERY defect instead
// of stopping at the first. An unknown key is reported before any decoding (the first maxUnknownFieldErrors in sorted order, each clipped).
// Otherwise fields are decoded in PutMySettings' order, with default_harness inserted where
// PutMySettings decodes it (just before judge_runs_on) and cross_check_pins last, so the first
// error is the one PutMySettings would report. The returned write is meaningful only when no
// error is returned; nothing is written either way.
func decodeModelSettingsRequest(req map[string]json.RawMessage, userID uuid.UUID) (modelSettingsWrite, []modelSettingsError) {
	keys := make([]string, 0, len(req))
	for k := range req {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []modelSettingsError
	for _, k := range keys {
		if _, allowed := modelSettingsAllowlist[k]; !allowed {
			// The response must stay small whatever the caller sends: at most
			// maxUnknownFieldErrors entries (the first ones in sorted order, no marker entry), and
			// each key is echoed clipped (clipEcho).
			if len(errs) == maxUnknownFieldErrors {
				break
			}
			ck := clipEcho(k)
			errs = append(errs, modelSettingsError{Field: ck, Message: "unknown field " + ck})
		}
	}
	if len(errs) > 0 {
		return modelSettingsWrite{}, errs
	}

	var in modelSettingsWrite
	effort := func(field string) func(json.RawMessage) (pgtype.Text, *settingsFieldError) {
		return func(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
			return decodeDefaultEffortField(raw, field)
		}
	}
	steps := []struct {
		key    string
		decode func(json.RawMessage) (pgtype.Text, *settingsFieldError)
		into   *modelSettingsField
	}{
		{"default_claude_model", decodeDefaultClaudeModelField, &in.claude},
		{"default_codex_model", decodeDefaultCodexModelField, &in.codex},
		{"default_effort", effort("default_effort"), &in.effort},
		{"default_codex_effort", effort("default_codex_effort"), &in.codexEffort},
		{"judge_model", decodeJudgeModelField, &in.judge},
		{"codex_judge_model", decodeCodexJudgeModelField, &in.codexJudge},
		{"summary_model", decodeSummaryModelField, &in.summary},
		{"codex_summary_model", decodeCodexSummaryModelField, &in.codexSummary},
		{"task_review_model", decodeTaskReviewModelField, &in.taskReview},
		{"codex_task_review_model", decodeCodexTaskReviewModelField, &in.codexTaskReview},
		{"task_review_effort", effort("task_review_effort"), &in.taskReviewEffort},
		{"codex_task_review_effort", effort("codex_task_review_effort"), &in.codexTaskReviewEffort},
		{"default_harness", decodeDefaultHarnessField, &in.harness},
		{"judge_runs_on", decodeJudgeRunsOnField, &in.judgeRunsOn},
		{"planning_claude_model", decodePlanningClaudeModelField, &in.planningClaudeModel},
		{"planning_claude_effort", decodePlanningEffortField, &in.planningClaudeEffort},
		{"planning_codex_model", decodePlanningCodexModelField, &in.planningCodexModel},
		{"planning_codex_effort", decodePlanningCodexEffortField, &in.planningCodexEffort},
	}
	for _, st := range steps {
		raw, present := req[st.key]
		if !present {
			continue
		}
		v, ferr := st.decode(raw)
		if ferr != nil {
			errs = append(errs, modelSettingsError{Field: ferr.Field, Message: ferr.Message})
			continue
		}
		*st.into = modelSettingsField{present: true, val: v}
	}
	// cross_check_pins decodes last, as in PutMySettings, with the same semantics: an array of at
	// most four unique stage/harness cells, omitted members unchanged, a null member clears it,
	// a top-level null is rejected. Every pin defect is field cross_check_pins.
	if raw, present := req["cross_check_pins"]; present {
		patches, pinErrs := collectCrossCheckPins(raw, userID)
		for _, pe := range pinErrs {
			errs = append(errs, modelSettingsError{Field: "cross_check_pins", Message: pe.Err.Error(),
				Stage: pe.Stage, Harness: pe.Harness, Member: pe.Member})
		}
		in.crossCheckPins = patches
	}
	if len(errs) > 0 {
		return modelSettingsWrite{}, errs
	}
	return in, nil
}

// commitUnknownError marks a tx.Commit failure: the outcome is not known to this process.
type commitUnknownError struct{ err error }

func (e commitUnknownError) Error() string { return "commit: " + e.err.Error() }
func (e commitUnknownError) Unwrap() error { return e.err }

// writeModelSettings applies a validated write in one transaction. The harness is resolved
// BEFORE Begin (it reads through the worker service's own connections and must not run while
// the users row is locked), the same pre-write read PutMySettings does. Inside the
// transaction the users row is locked first, then every write goes through the transaction's
// queries, so a failure at any statement rolls all of them back.
//
// Invariant (see the header of model_settings_livedb_test.go; asserted by
// TestModelSettingsWrittenColumnsAreNotKeyColumnsLiveDB): none of the columns written below is users.id
// or a unique or foreign-key-referenced column of users, so the FOR NO KEY UPDATE row lock is
// the same strength as the lock each UPDATE takes on its own and conflicts with exactly the
// other settings writers.
func (h *Handler) writeModelSettings(ctx context.Context, userID uuid.UUID, in modelSettingsWrite) error {
	// The compatibility mirror (users.default_model) follows the harness a run would use AFTER
	// this write: a request carrying default_harness projects that proposed preference through
	// D11 (an explicit null is the zero pgtype.Text, so no preference), exactly as
	// buildHarnessModels does; a lane-only request uses the stored preference.
	var mirror workersvc.Harness
	switch {
	case in.harness.present:
		resolved, err := h.resolveSettingsHarnessWithDefault(ctx, userID, in.harness.val)
		if err != nil {
			return err
		}
		mirror = resolved
	case in.claude.present || in.codex.present:
		resolved, err := h.resolveSettingsHarness(ctx, userID)
		if err != nil {
			return err
		}
		mirror = resolved
	}

	begin := h.modelSettingsBeginTx
	if begin == nil {
		if h.pool == nil {
			return errors.New("model settings transaction unavailable")
		}
		begin = h.pool.Begin
	}
	tx, err := begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := h.applyModelSettings(ctx, tx, userID, in, mirror); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return commitUnknownError{err: err}
	}
	return nil
}

func (h *Handler) applyModelSettings(ctx context.Context, tx pgx.Tx, userID uuid.UUID, in modelSettingsWrite, mirror workersvc.Harness) error {
	q := h.q.WithTx(tx)
	if _, err := q.LockUserSettingsRow(ctx, userID); err != nil {
		return err
	}
	if in.harness.present || in.claude.present || in.codex.present {
		// harnessModelParams is the shared harness/lane/mirror builder. buildHarnessModels is not
		// used: this route has no legacy default_model input to bridge, and the mirror harness
		// is resolved before Begin. The harness itself is written by the same statement.
		params, _, err := harnessModelParams(ctx, q, userID,
			harnessModelInputs{harnessPresent: in.harness.present, harnessVal: in.harness.val},
			in.claude.present, in.claude.val, in.codex.present, in.codex.val, mirror)
		if err != nil {
			return err
		}
		if _, err := q.SetUserHarnessModels(ctx, params); err != nil {
			return err
		}
	}
	if in.effort.present {
		if _, err := q.SetUserDefaultEffort(ctx, store.SetUserDefaultEffortParams{ID: userID, DefaultEffort: in.effort.val}); err != nil {
			return err
		}
	}
	if in.codexEffort.present {
		if _, err := q.SetUserDefaultCodexEffort(ctx, store.SetUserDefaultCodexEffortParams{ID: userID, DefaultCodexEffort: in.codexEffort.val}); err != nil {
			return err
		}
	}
	if in.judge.present {
		if _, err := q.SetUserJudgeModel(ctx, store.SetUserJudgeModelParams{ID: userID, JudgeModel: in.judge.val}); err != nil {
			return err
		}
	}
	if in.codexJudge.present {
		if _, err := q.SetUserCodexJudgeModel(ctx, store.SetUserCodexJudgeModelParams{ID: userID, CodexJudgeModel: in.codexJudge.val}); err != nil {
			return err
		}
	}
	if in.judgeRunsOn.present {
		if _, err := q.SetUserJudgeRunsOn(ctx, store.SetUserJudgeRunsOnParams{ID: userID, JudgeRunsOn: in.judgeRunsOn.val}); err != nil {
			return err
		}
	}
	if in.summary.present {
		if _, err := q.SetUserSummaryModel(ctx, store.SetUserSummaryModelParams{ID: userID, SummaryModel: in.summary.val}); err != nil {
			return err
		}
	}
	if in.codexSummary.present {
		if _, err := q.SetUserCodexSummaryModel(ctx, store.SetUserCodexSummaryModelParams{ID: userID, CodexSummaryModel: in.codexSummary.val}); err != nil {
			return err
		}
	}
	if in.taskReview.present {
		if _, err := q.SetUserTaskReviewModel(ctx, store.SetUserTaskReviewModelParams{ID: userID, TaskReviewModel: in.taskReview.val}); err != nil {
			return err
		}
	}
	if in.codexTaskReview.present {
		if _, err := q.SetUserCodexTaskReviewModel(ctx, store.SetUserCodexTaskReviewModelParams{ID: userID, CodexTaskReviewModel: in.codexTaskReview.val}); err != nil {
			return err
		}
	}
	if in.taskReviewEffort.present {
		if _, err := q.SetUserTaskReviewEffort(ctx, store.SetUserTaskReviewEffortParams{ID: userID, TaskReviewEffort: in.taskReviewEffort.val}); err != nil {
			return err
		}
	}
	if in.codexTaskReviewEffort.present {
		if _, err := q.SetUserCodexTaskReviewEffort(ctx, store.SetUserCodexTaskReviewEffortParams{ID: userID, CodexTaskReviewEffort: in.codexTaskReviewEffort.val}); err != nil {
			return err
		}
	}
	if in.planningClaudeModel.present || in.planningClaudeEffort.present {
		if _, err := q.SetUserPlanningClaude(ctx, store.SetUserPlanningClaudeParams{
			ID:                   userID,
			SetModel:             in.planningClaudeModel.present,
			PlanningClaudeModel:  in.planningClaudeModel.val,
			SetEffort:            in.planningClaudeEffort.present,
			PlanningClaudeEffort: in.planningClaudeEffort.val,
		}); err != nil {
			return err
		}
	}
	if in.planningCodexModel.present || in.planningCodexEffort.present {
		if _, err := q.SetUserPlanningCodex(ctx, store.SetUserPlanningCodexParams{
			ID:                  userID,
			SetModel:            in.planningCodexModel.present,
			PlanningCodexModel:  in.planningCodexModel.val,
			SetEffort:           in.planningCodexEffort.present,
			PlanningCodexEffort: in.planningCodexEffort.val,
		}); err != nil {
			return err
		}
	}
	// The pin cells are written last: the users row lock above is taken first, then the cell
	// rows in (stage, harness) order, the order every pin writer uses.
	return saveCrossCheckPinsTx(ctx, q, in.crossCheckPins)
}
