package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
)

// modelSettingsAllowlist is the closed set of keys PUT /api/me/model-settings accepts. Anything
// else, including default_harness, the legacy default_model, appearance and every consent
// switch, is a 400 naming the key.
var modelSettingsAllowlist = map[string]struct{}{
	"default_claude_model": {},
	"default_codex_model":  {},
	"default_effort":       {},
	"default_codex_effort": {},
	"judge_model":          {},
	"codex_judge_model":    {},
	"judge_runs_on":        {},
	"summary_model":        {},
	"codex_summary_model":  {},
	"cross_check_pins":     {},
}

// modelSettingsField is one decoded tri-state field: absent (present=false) leaves the column,
// present with an invalid val clears it to NULL, present with a valid val sets it.
type modelSettingsField struct {
	present bool
	val     pgtype.Text
}

// modelSettingsWrite is the validated phase-1 state writeModelSettings applies.
type modelSettingsWrite struct {
	claude, codex, effort, codexEffort modelSettingsField
	judge, codexJudge, judgeRunsOn     modelSettingsField
	summary, codexSummary              modelSettingsField
	// crossCheckPins are the decoded cross_check_pins patches (decodeCrossCheckPins); empty when
	// the key is absent or an empty list.
	crossCheckPins []store.PatchUserCrossCheckPinParams
}

func (w modelSettingsWrite) any() bool {
	if len(w.crossCheckPins) > 0 {
		return true
	}
	for _, f := range []modelSettingsField{w.claude, w.codex, w.effort, w.codexEffort,
		w.judge, w.codexJudge, w.judgeRunsOn, w.summary, w.codexSummary} {
		if f.present {
			return true
		}
	}
	return false
}

// writeModelSettingsFieldError renders a field-naming 400: {"error": "...", "field": "..."}.
func writeModelSettingsFieldError(w http.ResponseWriter, field, message string) {
	httpx.JSON(w, http.StatusBadRequest, map[string]string{"error": message, "field": field})
}

// PutMyModelSettings updates the caller's own model and effort defaults (PRD #3264 M1) with the
// PutMySettings tri-state (absent leaves the column, null clears it, a value is validated and
// set), but over a closed allowlist and in ONE transaction: every present field is written or
// none is. It shares the per-field decoders with PutMySettings (settings_decode.go), so the two
// write paths accept and reject identically.
//
// Order: decode and validate everything (nothing written), resolve the settings harness (the
// same accepted pre-write race as PutMySettings), then Begin, lock the users row, write, commit,
// and only then read the settings back for the response.
//
// Failure model:
//   - any error before Commit rolls back: a validation defect is a 400, anything else a 500
//     "internal error";
//   - a Commit error is a 500 with reason settings_commit_unknown, because the outcome is not
//     known to this process;
//   - a failed post-commit readback is a 500 with reason settings_saved_readback_failed: the
//     write is durable.
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
	keys := make([]string, 0, len(req))
	for k := range req {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, allowed := modelSettingsAllowlist[k]; !allowed {
			writeModelSettingsFieldError(w, k, "unknown field "+k)
			return
		}
	}

	// ---- Phase 1: decode + validate every present field in PutMySettings' order, so the
	// first error is the one PutMySettings would report. Nothing is written.
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
		{"judge_runs_on", decodeJudgeRunsOnField, &in.judgeRunsOn},
	}
	for _, st := range steps {
		raw, present := req[st.key]
		if !present {
			continue
		}
		v, ferr := st.decode(raw)
		if ferr != nil {
			writeModelSettingsFieldError(w, ferr.Field, ferr.Message)
			return
		}
		*st.into = modelSettingsField{present: true, val: v}
	}
	// cross_check_pins decodes last, as in PutMySettings, with the same semantics: an array of at
	// most four unique stage/harness cells, omitted members unchanged, a null member clears it,
	// a top-level null is rejected.
	if raw, present := req["cross_check_pins"]; present {
		patches, err := decodeCrossCheckPins(raw, user.ID)
		if err != nil {
			writeModelSettingsFieldError(w, "cross_check_pins", err.Error())
			return
		}
		in.crossCheckPins = patches
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
	httpx.JSON(w, http.StatusOK, map[string]any{"settings": dto})
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
	var mirror workersvc.Harness
	if in.claude.present || in.codex.present {
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
	if in.claude.present || in.codex.present {
		// harnessModelParams is the shared lane/mirror builder. buildHarnessModels is not used:
		// it needs default_harness/default_model input this route has none of, and the
		// harness is resolved before Begin.
		params, _, err := harnessModelParams(ctx, q, userID, harnessModelInputs{},
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
	// The pin cells are written last: the users row lock above is taken first, then the cell
	// rows in (stage, harness) order, the order every pin writer uses.
	return saveCrossCheckPinsTx(ctx, q, in.crossCheckPins)
}
