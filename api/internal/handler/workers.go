package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workertmpl"
)

// textPtrValue returns a pointer to s when valid, else nil — the JSON-null vs
// value convention the DTOs use for nullable text columns.
func textPtrValue(valid bool, s string) *string {
	if !valid {
		return nil
	}
	return &s
}

// uuidPtrValue renders a nullable uuid column as its string form, or nil. Used for
// the worker's Anthropic binding (PRD #104 M3), where NULL is the meaningful
// "unbound, spends the owner's default" state and must serialize as JSON null.
func uuidPtrValue(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// timePtr returns a pointer to t when valid, else nil.
func timePtr(valid bool, t time.Time) *time.Time {
	if !valid {
		return nil
	}
	return &t
}

// intPtrValue returns a pointer to the int value of a nullable int4 column when
// valid, else nil — the JSON-null vs value convention for the worker's advertised
// max_concurrent_runs (PRD #42).
func intPtrValue(i pgtype.Int4) *int {
	if !i.Valid {
		return nil
	}
	v := int(i.Int32)
	return &v
}

// float4PtrValue / int8PtrValue apply the same JSON-null vs value convention to the
// worker's nullable stats columns (PRD #49): a NULL column becomes a JSON null so the
// UI shows nothing rather than a fabricated 0.
func float4PtrValue(f pgtype.Float4) *float64 {
	if !f.Valid {
		return nil
	}
	v := float64(f.Float32)
	return &v
}

func int8PtrValue(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	v := i.Int64
	return &v
}

// boolPtrValue applies the JSON-null vs value convention to a nullable bool column
// (worker.docker_enabled, PRD #83 M3): NULL → JSON null (docker not applicable to an
// external worker), else the stored true/false.
func boolPtrValue(b pgtype.Bool) *bool {
	if !b.Valid {
		return nil
	}
	v := b.Bool
	return &v
}

// maxWorkerNameBytes bounds a worker's human label.
const maxWorkerNameBytes = 200

// runInputKinds is the accepted steering-input set (mirrors the DB CHECK).
var runInputKinds = map[string]bool{
	"follow_up": true, "approve_plan": true, "reject_plan": true, "cancel": true, "revise_plan": true,
}

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
//
// NO ROLL SIGNAL on this path, deliberately (PRD #113 M4). The bare-row callers —
// register, heartbeat, create, admin list — hold a worker row with no roll-health
// join, so they classify by version comparison alone. That is honest rather than
// degraded: passing a nil signal says "this row carries no controller report", which
// is exactly what it carries. The per-user LIST path has the join and folds it.
func workerDTOFromWorker(w store.Worker, activeRuns int, busy bool, secretLabel, cpVersion string, now, apiStartedAt time.Time) apitypes.WorkerDTO {
	upgradeStatus, upgradeDetail, upgradeTarget := workersvc.ClassifyUpgradeWithTarget(workersvc.UpgradeInput{
		Reported:     w.Version.String,
		Kind:         w.Kind,
		CPVersion:    cpVersion,
		Signal:       nil,
		Now:          now,
		APIStartedAt: apiStartedAt,
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
		Busy:                 busy,
		ActiveRuns:           activeRuns,
		MaxConcurrentRuns:    intPtrValue(w.MaxConcurrentRuns),
		TemplateDeclared:     textPtrValue(w.TemplateDeclared.Valid, w.TemplateDeclared.String),
		TemplateReported:     textPtrValue(w.TemplateReported.Valid, w.TemplateReported.String),
		Version:              textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt:      timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		CreatedAt:            w.CreatedAt.Time,
		StatsCPUPct:          float4PtrValue(w.StatsCpuPct),
		StatsMemBytes:        int8PtrValue(w.StatsMemBytes),
		StatsMemLimitBytes:   int8PtrValue(w.StatsMemLimitBytes),
		StatsSource:          textPtrValue(w.StatsSource.Valid, w.StatsSource.String),
	}
}

func workerDTOFromRow(w store.ListWorkersByUserRow, cpVersion string, now, apiStartedAt time.Time) apitypes.WorkerDTO {
	upgradeStatus, upgradeDetail, upgradeTarget := workersvc.ClassifyUpgradeWithTarget(workersvc.UpgradeInput{
		Reported:     w.Version.String,
		Kind:         w.Kind,
		CPVersion:    cpVersion,
		Signal:       rollSignalFromRow(w),
		Now:          now,
		APIStartedAt: apiStartedAt,
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
		Busy:                     w.Busy,
		ActiveRuns:               int(w.ActiveRuns),
		MaxConcurrentRuns:        intPtrValue(w.MaxConcurrentRuns),
		TemplateDeclared:         textPtrValue(w.TemplateDeclared.Valid, w.TemplateDeclared.String),
		TemplateReported:         textPtrValue(w.TemplateReported.Valid, w.TemplateReported.String),
		Version:                  textPtrValue(w.Version.Valid, w.Version.String),
		LastHeartbeatAt:          timePtr(w.LastHeartbeatAt.Valid, w.LastHeartbeatAt.Time),
		CreatedAt:                w.CreatedAt.Time,
		StatsCPUPct:              float4PtrValue(w.StatsCpuPct),
		StatsMemBytes:            int8PtrValue(w.StatsMemBytes),
		StatsMemLimitBytes:       int8PtrValue(w.StatsMemLimitBytes),
		StatsSource:              textPtrValue(w.StatsSource.Valid, w.StatsSource.String),
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

// int32PtrValue applies the JSON-null vs value convention to a nullable int4 whose zero
// is meaningful — an exit code of 0 and "never terminated" are different facts.
func int32PtrValue(valid bool, v int32) *int32 {
	if !valid {
		return nil
	}
	return &v
}

func runToDTO(r store.Run) apitypes.RunDTO {
	dto := apitypes.RunDTO{
		ID:               r.ID.String(),
		Kind:             r.Kind,
		IssueTitle:       r.IssueTitle,
		IssueDescription: r.IssueDescription,
		Title:            textPtrValue(r.Title.Valid, r.Title.String),
		Status:           r.Status,
		RequeueCount:     r.RequeueCount,
		IterationCount:   r.IterationCount,
		AutoApprove:      r.AutoApprove,
		Branch:           textPtrValue(r.Branch.Valid, r.Branch.String),
		MrWebURL:         textPtrValue(r.MrWebUrl.Valid, r.MrWebUrl.String),
		MrState:          textPtrValue(r.MrState.Valid, r.MrState.String),
		FailureReason:    textPtrValue(r.FailureReason.Valid, r.FailureReason.String),
		StopKind:         textPtrValue(r.StopKind.Valid, r.StopKind.String),
		Health:           r.Health,
		HealthReason:     textPtrValue(r.HealthReason.Valid, r.HealthReason.String),
		HealthSince:      timePtr(r.HealthSince.Valid, r.HealthSince.Time),
		PlanMd:           textPtrValue(r.PlanMd.Valid, r.PlanMd.String),
		PipelineRef:      textPtrValue(r.PipelineRef.Valid, r.PipelineRef.String),
		FixVerdict:       textPtrValue(r.FixVerdict.Valid, r.FixVerdict.String),
		ClaimedAt:        timePtr(r.ClaimedAt.Valid, r.ClaimedAt.Time),
		StartedAt:        timePtr(r.StartedAt.Valid, r.StartedAt.Time),
		FinishedAt:       timePtr(r.FinishedAt.Valid, r.FinishedAt.Time),
		CreatedAt:        r.CreatedAt.Time,
		UpdatedAt:        r.UpdatedAt.Time,
		// PRD #35 usage-limit park. Mapped INDEPENDENTLY of each other, like the
		// PRD #111 credential fields below: WaitOnLimit is set on every run from
		// creation while the other four stay null/zero until a first park, so a run
		// legitimately carries the opt-in with no park data. Never branch on the group.
		WaitOnLimit:    r.WaitOnLimit,
		LimitResetsAt:  timePtr(r.LimitResetsAt.Valid, r.LimitResetsAt.Time),
		RetryNotBefore: timePtr(r.RetryNotBefore.Valid, r.RetryNotBefore.Time),
		LimitWaitCount: r.LimitWaitCount,
		RateLimitType:  textPtrValue(r.RateLimitType.Valid, r.RateLimitType.String),
	}
	if r.RepoID.Valid {
		s := uuid.UUID(r.RepoID.Bytes).String()
		dto.RepoID = &s
	}
	if r.ResumeOfRunID.Valid {
		s := uuid.UUID(r.ResumeOfRunID.Bytes).String()
		dto.ResumeOfRunID = &s
	}
	if r.IssueIid.Valid {
		v := r.IssueIid.Int64
		dto.IssueIID = &v
	}
	if r.WorkerID.Valid {
		s := uuid.UUID(r.WorkerID.Bytes).String()
		dto.WorkerID = &s
	}
	if r.MrIid.Valid {
		v := r.MrIid.Int64
		dto.MrIID = &v
	}
	// PRD #111 M1. Mapped INDEPENDENTLY, not as a pair: the FK nulls the id when the
	// credential is deleted while the snapshotted label stays, so a historical run
	// legitimately carries a label with no id and the UI still names the account.
	if r.AnthropicSecretID.Valid {
		s := uuid.UUID(r.AnthropicSecretID.Bytes).String()
		dto.AnthropicSecretID = &s
	}
	dto.AnthropicSecretLabel = textPtrValue(r.AnthropicSecretLabel.Valid, r.AnthropicSecretLabel.String)
	// PRD #111 M5. Mapped independently of BOTH fields above, for the same reason and
	// one more: the reason is present on every M1-era run (all three lanes write one)
	// while the headroom is present only on an auto pick, so a run legitimately
	// carries a reason with no headroom. Rendering must branch on each, never on the
	// pair.
	dto.AnthropicSelectReason = textPtrValue(r.AnthropicSelectReason.Valid, r.AnthropicSelectReason.String)
	if r.AnthropicHeadroomPct.Valid {
		// SMALLINT 0..100 with a CHECK, widened to int for the wire: JSON has one
		// number type and a *int16 would only invite a client to think the range is
		// meaningful to it. The range lives in the database and in autoselect, not here.
		v := int(r.AnthropicHeadroomPct.Int16)
		dto.AnthropicHeadroomPct = &v
	}
	// PRD #37. A decode error should be impossible (the API validates every write
	// and both columns carry a jsonb_typeof CHECK); it is logged and treated as
	// "not reported" rather than failing the read of an otherwise-fine run.
	if agents, err := workersvc.DecodeRepoAgents(r.RepoAgents); err != nil {
		slog.Error("decode run repo agents", "run_id", r.ID, "error", err)
	} else {
		dto.RepoAgents = agents
	}
	if excl, err := workersvc.DecodeExclusions(r.AgentExclusions); err != nil {
		slog.Error("decode run agent exclusions", "run_id", r.ID, "error", err)
	} else {
		dto.AgentExclusions = excl
	}
	dto.AgentSource = textPtrValue(r.AgentSource.Valid, r.AgentSource.String)
	// The failing pipeline's URL rides the frozen snapshot, not a column (the
	// pipeline cache row is transient). Best-effort decode; a ci_fix run always has
	// one, an issue run has no snapshot.
	if url := failureSnapshotWebURL(r.FailureSnapshot); url != "" {
		dto.PipelineWebURL = &url
	}
	return dto
}

// failureSnapshotWebURL pulls just the pipeline web URL out of a run's
// failure_snapshot jsonb for the run-view header link. Returns "" for an issue run
// (no snapshot) or a malformed one.
func failureSnapshotWebURL(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var snap struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return ""
	}
	return snap.WebURL
}

func messageToDTO(m store.RunMessage) apitypes.MessageDTO {
	return apitypes.MessageDTO{
		Seq:           m.Seq,
		Kind:          m.Kind,
		Agent:         textPtrValue(m.Agent.Valid, m.Agent.String),
		AgentInstance: textPtrValue(m.AgentInstance.Valid, m.AgentInstance.String),
		AgentLabel:    textPtrValue(m.AgentLabel.Valid, m.AgentLabel.String),
		Payload:       json.RawMessage(m.Payload),
		CreatedAt:     m.CreatedAt.Time,
	}
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
		"worker": workerDTOFromWorker(wkr, 0, false, tokenLabel, h.version, h.clock(), h.startedAt),
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
		out = append(out, workerDTOFromRow(row, h.version, h.clock(), h.startedAt))
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid worker id")
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid worker id")
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
	httpx.JSON(w, http.StatusOK, map[string]any{"worker": workerDTOFromWorker(wkr, 0, false, token.label, h.version, h.clock(), h.startedAt)})
}

// -------------------------------------------------------------------------
// Runs (session-authenticated)
// -------------------------------------------------------------------------

// CreateRun queues an agent run from a card. The issue must be a cached PRD
// issue with a PRD link in a repo the user owns. The issue description is
// snapshotted from the forge (the source of truth) at queue time — the board
// never stores descriptions, so the browser has none to send, and fetching it
// here keeps the run self-contained and authoritative.
func (h *Handler) CreateRun(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	user, _ := mw.UserFromContext(r.Context())
	var req struct {
		IssueIID int64 `json:"issue_iid"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IssueIID <= 0 {
		httpx.Error(w, http.StatusBadRequest, "issue_iid must be a positive integer")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	issue, err := f.GetIssue(r.Context(), repo.ForgeProjectID, req.IssueIID)
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not read the issue from the forge: "+err.Error())
		return
	}

	// PRDLESS bypass (PRD #22 Decision 3): compute allowWithoutPRD from the fresh
	// forge snapshot's labels and the prdless settings, then thread it into the
	// shared createRun gate. Reading the fresh labels (not the cache) means a
	// just-added label works immediately, without waiting for a poller cycle;
	// matching is exact, like board column labels. Settings reads are best-effort
	// (the accessors return the default alongside a cold error): a settings blip
	// falls back to the default label / enabled=true, and an unlabeled issue is
	// still gated.
	prdlessEnabled, _ := h.settings.PrdlessEnabled(r.Context())
	prdlessLabel, _ := h.settings.PrdlessLabel(r.Context())
	allowWithoutPRD := prdlessEnabled && slices.Contains(issue.Labels, prdlessLabel)

	// The description cap is enforced inside CreateRun (shared with the autopilot
	// path), surfaced here as ErrDescriptionTooLarge → 422.
	run, err := h.wsvc.CreateRun(r.Context(), user.ID, repo.ID, req.IssueIID, issue.Description, allowWithoutPRD)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRepoNotFound):
			httpx.Error(w, http.StatusNotFound, "repo not found")
		case errors.Is(err, workersvc.ErrIssueNotFound):
			httpx.Error(w, http.StatusNotFound, "issue not found on this repo's board")
		case errors.Is(err, workersvc.ErrDescriptionTooLarge):
			httpx.Error(w, http.StatusUnprocessableEntity, "issue description is too large to run")
		case errors.Is(err, workersvc.ErrNoPRDLink):
			// Extend the hint with the escape-hatch label only when the feature is
			// enabled instance-wide, so a strict-regime instance never advertises it.
			msg := "issue has no PRD link; add a prds/*.md link before starting a run"
			if prdlessEnabled {
				msg = fmt.Sprintf("issue has no PRD link; add a prds/*.md link (or the %s label) before starting a run", prdlessLabel)
			}
			httpx.Error(w, http.StatusUnprocessableEntity, msg)
		case errors.Is(err, workersvc.ErrActiveRunExists):
			httpx.Error(w, http.StatusConflict, "a run is already in progress for this issue")
		case errors.Is(err, workersvc.ErrBranchInUse):
			// Cross-kind exclusion (PRD #6): a ci_fix run is already holding this
			// issue's agent branch/worktree.
			httpx.Error(w, http.StatusConflict, "a CI-fix run is already working this issue's branch; cancel it before starting an issue run")
		default:
			slog.Error("create run", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run)})
}

// GetRun returns one run visible to the viewer (owner, or any run for an admin).
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := h.wsvc.GetRunForViewer(r.Context(), user.ID, user.IsAdmin, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	dto := runToDTO(run)
	// PRD #65 D2: stamp the run's forge for the run-view MR/PR noun. Best-effort and
	// only for a repo-ful run (chat runs have no repo, hence no MR affordance): a
	// lookup error leaves forge_type "" (the web defaults to GitLab's noun), never
	// failing the read of an otherwise-fine run.
	if run.RepoID.Valid {
		if ft, err := h.q.GetForgeTypeForRepo(r.Context(), uuid.UUID(run.RepoID.Bytes)); err != nil {
			slog.Error("resolve run forge type", "run_id", run.ID, "error", err)
		} else {
			dto.ForgeType = ft
		}
	}
	// PRD #37 M4-fix: resolve the owner's OWN-source roster here, on the detail read,
	// so the plan-gate picker sources its "My agent templates" chips from exactly the
	// roster the approve validator + worker use (allocation-resolved, lead stripped).
	// Best-effort: a lookup error leaves own_agents null (the picker degrades to no own
	// chips) rather than failing the read of an otherwise-fine run.
	if own, err := h.wsvc.OwnAgentRoster(r.Context(), run.UserID); err != nil {
		slog.Error("resolve own agent roster", "run_id", run.ID, "error", err)
	} else {
		dto.OwnAgents = own
	}
	// Attach the run's usage totals (PRD #40). No row → no usage: leave dto.Usage nil
	// (a pre-feature run shows nothing). Best-effort like OwnAgents above: a lookup
	// error must not fail the read of an otherwise-fine run.
	if u, err := h.wsvc.RunUsageTotal(r.Context(), run.ID); err == nil {
		dto.Usage = &apitypes.UsageDTO{
			InputTokens:         u.InputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
			OutputTokens:        u.OutputTokens,
			CostUSD:             numericToFloat(u.CostUsd),
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get run usage total", "run_id", run.ID, "error", err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": dto})
}

// ListRunMessages returns a run's persisted messages after ?after=<seq> (default
// 0), the replay source a reconnecting browser reads before going live. Visible
// to the run's owner or an admin.
func (h *Handler) ListRunMessages(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	after := int32(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			httpx.Error(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = int32(n)
	}
	msgs, err := h.wsvc.ListRunMessagesForViewer(r.Context(), user.ID, user.IsAdmin, id, after)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("list run messages", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageToDTO(m))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": out})
}

// CreateRunInput submits a steering input (approve/reject/follow-up/cancel). A
// cancel or plan rejection against a run with no live poller is applied
// server-side; anything else is queued for the worker.
func (h *Handler) CreateRunInput(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req apitypes.RunInputRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !runInputKinds[req.Kind] {
		httpx.Error(w, http.StatusBadRequest, "kind must be one of follow_up, approve_plan, reject_plan, cancel, revise_plan")
		return
	}

	res, err := h.wsvc.SubmitInput(r.Context(), user.ID, id, req.Kind, req.Body, req.Selection)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		case errors.Is(err, workersvc.ErrReviseCapReached):
			httpx.Error(w, http.StatusConflict, "plan revision limit reached")
		case errors.Is(err, workersvc.ErrInvalidSelection):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("submit run input", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	resp := apitypes.RunInputResponse{ServerSide: res.ServerSide}
	// A follow_up write returns the created row (PRD #95 S2) so the web's optimistic
	// queue entry adopts the real id + timestamp. follow_up is never server-side (only
	// cancel/reject are), so res.ID/CreatedAt are the freshly-inserted row here.
	if req.Kind == "follow_up" {
		id := res.ID
		createdAt := res.CreatedAt
		resp.ID = &id
		resp.CreatedAt = &createdAt
	}
	httpx.JSON(w, http.StatusAccepted, resp)
}

// steerInputToDTO maps a follow_up run_user_inputs row to the web/CLI steer-queue DTO
// (PRD #95). Delivery status is derived client-side from consumed_at.
func steerInputToDTO(i store.RunUserInput) apitypes.SteerInputDTO {
	return apitypes.SteerInputDTO{
		ID:         i.ID,
		Body:       textPtrValue(i.Body.Valid, i.Body.String),
		CreatedAt:  i.CreatedAt.Time,
		ConsumedAt: timePtr(i.ConsumedAt.Valid, i.ConsumedAt.Time),
	}
}

// ListRunInputs returns a run's follow_up steer queue (newest-first) with delivery
// status (PRD #95). RequireUser (so a CLI token works — Decision 8) and OWNER-ONLY:
// the run is resolved via GetRunByIDForUser, so a non-owner — including an admin_ro
// token on another user's run — gets 404, never an error banner. This closes a real
// read leak (follow-ups are never mirrored into run_messages). A non-owner viewer's
// queue card therefore renders empty/silent.
func (h *Handler) ListRunInputs(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	rows, err := h.wsvc.ListFollowUpInputs(r.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("list run inputs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.SteerInputDTO, 0, len(rows))
	for _, i := range rows {
		out = append(out, steerInputToDTO(i))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"inputs": out})
}
