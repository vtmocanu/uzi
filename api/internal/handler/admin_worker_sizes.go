package handler

// Admin management of hosted worker sizes (PRD #4213 M2, M3): GET /api/admin/worker-sizes,
// PUT /api/admin/worker-sizes/{name} (edit an existing size, or create an absent one),
// DELETE /api/admin/worker-sizes/{name} and POST /api/admin/worker-sizes/{name}/reset.
//
// Every mutation is
// gated on a fresh controller capability report (adminSizesGate) so an edit the controller
// would silently ignore is refused instead, and runs in one transaction under the table's
// advisory lock. Validation is integer arithmetic in workersize.Validate.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersize"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	// maxWorkerSizes is the table's size bound (PRD #4213); the add path enforces it, an edit
	// or a delete never consults it.
	maxWorkerSizes = 16
	// adminSizesAdvertisedVersion is the lowest admin-sizes capability version that honours
	// DesiredWorker.Resources.
	adminSizesAdvertisedVersion = 1
	// workerSizeBodyLimit bounds a size edit body; six integers need far less.
	workerSizeBodyLimit = 4 << 10

	readOnlyNoReport    = "controller_report_missing"
	readOnlyUnsupported = "controller_unsupported"
	readOnlyStale       = "controller_report_stale"
)

// sizeGate is the outcome of the controller capability check.
type sizeGate struct {
	Editable bool
	Code     string
	Reason   string
}

// evaluateAdminSizesGate decides whether a size mutation may be accepted. It is pure: the
// advertisement row, its read error and the clock are inputs. The window is the shared
// workersvc.AdvertisementFresh rule (2x the clamped poll interval on the api receipt clock).
// Check order: a missing report, then a stale one (a stale version proves nothing), then a
// controller that does not advertise support.
func evaluateAdminSizesGate(adv store.GetControllerAdminSizesAdvertisementRow, readErr error, now time.Time) sizeGate {
	if errors.Is(readErr, pgx.ErrNoRows) || (readErr == nil && !adv.AdminSizesVersion.Valid && !adv.AdminSizesAdvertisedAt.Valid) {
		return sizeGate{Code: readOnlyNoReport, Reason: "No controller report has been received yet, so the api cannot tell whether the controller supports admin worker sizes."}
	}
	if readErr != nil {
		return sizeGate{Code: readOnlyNoReport, Reason: "The controller report could not be read."}
	}
	if !adv.AdminSizesAdvertisedAt.Valid ||
		!workersvc.AdvertisementFresh(pgtype.Int4{Int32: adminSizesAdvertisedVersion, Valid: true}, adv.AdminSizesAdvertisedAt,
			adv.PollIntervalSeconds, adminSizesAdvertisedVersion, now) {
		return sizeGate{Code: readOnlyStale, Reason: "The controller report is stale: the controller has not reported recently, so an edit might not be applied."}
	}
	if !adv.AdminSizesVersion.Valid || adv.AdminSizesVersion.Int32 < adminSizesAdvertisedVersion {
		return sizeGate{Code: readOnlyUnsupported, Reason: "The controller does not support admin worker sizes yet (upgrade it); edits would be ignored."}
	}
	return sizeGate{Editable: true}
}

// adminSizesGate reads the advertisement and evaluates the gate. A database failure is
// reported to the caller, which answers 500 rather than guessing.
func (h *Handler) adminSizesGate(ctx context.Context) (sizeGate, error) {
	if h.q == nil {
		return sizeGate{}, errors.New("no store")
	}
	adv, err := h.q.GetControllerAdminSizesAdvertisement(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return sizeGate{}, err
	}
	return evaluateAdminSizesGate(adv, err, h.clock()), nil
}

func specToDTO(s workersize.Spec) apitypes.WorkerSizeSpec {
	return apitypes.WorkerSizeSpec(s)
}

func rowSpec(r store.WorkerSize) workersize.Spec {
	return workersize.Spec{
		CPURequestMillicores: r.CpuRequestMillicores, CPULimitMillicores: r.CpuLimitMillicores,
		MemoryRequestMiB: r.MemoryRequestMib, MemoryLimitMiB: r.MemoryLimitMib,
		DataGiB: r.DataGib, DinDDataGiB: r.DindDataGib,
	}
}

func shippedSpec(name string) (workersize.Spec, bool) {
	for _, s := range builtin.Sizes() {
		if s.Name == name {
			return workersize.Spec{
				CPURequestMillicores: s.CPURequestMillicores, CPULimitMillicores: s.CPULimitMillicores,
				MemoryRequestMiB: s.MemoryRequestMiB, MemoryLimitMiB: s.MemoryLimitMiB,
				DataGiB: s.DataGiB, DinDDataGiB: s.DinDDataGiB,
			}, true
		}
	}
	return workersize.Spec{}, false
}

func adminSizeDTO(r store.WorkerSize) apitypes.AdminWorkerSizeDTO {
	dto := apitypes.AdminWorkerSizeDTO{
		Name: r.Name, WorkerSizeSpec: specToDTO(rowSpec(r)),
		DisplayOrder: r.DisplayOrder, Builtin: r.Builtin, Customized: r.Customized,
		UpdatedAt: r.UpdatedAt.Time,
	}
	if r.Builtin {
		if sh, ok := shippedSpec(r.Name); ok {
			d := specToDTO(sh)
			dto.Shipped = &d
		}
	}
	return dto
}

func warningDTOs(ws []workersize.Warning) []apitypes.WorkerSizeWarningDTO {
	out := make([]apitypes.WorkerSizeWarningDTO, 0, len(ws))
	for _, w := range ws {
		out = append(out, apitypes.WorkerSizeWarningDTO(w))
	}
	return out
}

func advisoryDTOs(as []workersize.Advisory) []apitypes.WorkerSizeAdvisoryDTO {
	out := make([]apitypes.WorkerSizeAdvisoryDTO, 0, len(as))
	for _, a := range as {
		out = append(out, apitypes.WorkerSizeAdvisoryDTO(a))
	}
	return out
}

func ceilingDTO(c builtin.Ceilings, kind workersize.CeilingKind) *apitypes.WorkerSizeCeilingDTO {
	b, ok := workersize.Binding(c, kind)
	if !ok {
		return nil
	}
	return &apitypes.WorkerSizeCeilingDTO{Value: b.Value, ChartPath: b.ChartPath}
}

func ceilingsDTO(c builtin.Ceilings) apitypes.WorkerSizeCeilingsDTO {
	dto := apitypes.WorkerSizeCeilingsDTO{
		CPULimitMillicores: ceilingDTO(c, workersize.CeilingCPULimit),
		MemoryLimitMiB:     ceilingDTO(c, workersize.CeilingMemoryLimit),
		DataGiB:            ceilingDTO(c, workersize.CeilingData),
		DinDDataGiB:        ceilingDTO(c, workersize.CeilingDindData),
		DinDOverridden:     c.DindOverridden,
	}
	if c.TiersKnown {
		tiers := []string{}
		for t := builtin.Tier(0); t < builtin.TierCount; t++ {
			if c.Enabled[t] {
				tiers = append(tiers, t.Name())
			}
		}
		dto.EnabledTiers = tiers
	}
	return dto
}

// AdminListWorkerSizes serves GET /api/admin/worker-sizes: every size, the ceilings they are
// validated against, whether the editor may save, and the quota advisories.
func (h *Handler) AdminListWorkerSizes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := workersize.List(ctx, h.q)
	if err != nil {
		slog.Error("list worker sizes", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	gate, err := h.adminSizesGate(ctx)
	if err != nil {
		slog.Error("worker sizes gate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.AdminWorkerSizesDTO{
		Sizes:      make([]apitypes.AdminWorkerSizeDTO, 0, len(rows)),
		Ceilings:   ceilingsDTO(h.cfg.WorkerSizeCeilings),
		Editable:   gate.Editable,
		Advisories: []apitypes.WorkerSizeAdvisoryDTO{},
		MaxSizes:   maxWorkerSizes,
		Refusals:   h.sizeRefusals(ctx),
	}
	if !gate.Editable {
		reason, code := gate.Reason, gate.Code
		out.ReadOnlyReason, out.ReadOnlyCode = &reason, &code
	}
	for _, row := range rows {
		out.Sizes = append(out.Sizes, adminSizeDTO(row))
		res := workersize.Validate(row.Name, rowSpec(row), nil, h.cfg.WorkerSizeCeilings, h.cfg.WorkerSizeQuota)
		out.Advisories = append(out.Advisories, advisoryDTOs(res.Advisories)...)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// maxSizeRefusals bounds the refusals array of the size list.
const (
	maxSizeRefusals    = 50
	maxRefusalTextByte = 256
)

// sizeRefusals lists the hosted workers whose latest FRESH stored roll signal is a stuck
// size_refused or pvc_rejected report, keyed by hosted_size. Freshness is the same API-receipt
// rule the health check and the worker classifier use (workersvc.DefaultControllerSignalTTL on
// roll_observed_at). Always non-nil; a read failure yields an empty list, since the refusals
// are advisory display beside the size table, not part of what an edit depends on.
func (h *Handler) sizeRefusals(ctx context.Context) []apitypes.WorkerSizeRefusalDTO {
	out := []apitypes.WorkerSizeRefusalDTO{}
	if h.q == nil {
		return out
	}
	rows, err := h.q.ListAllWorkers(ctx)
	if err != nil {
		slog.Warn("worker size refusals", "error", err)
		return out
	}
	now := h.clock()
	for _, row := range rows {
		if row.Worker.Kind != "hosted" || !row.Worker.HostedSize.Valid ||
			!row.RollObservedAt.Valid || now.Sub(row.RollObservedAt.Time) > workersvc.DefaultControllerSignalTTL {
			continue
		}
		sig := rollSignalFromAdminRow(row)
		if sig == nil || sig.Phase != workersvc.PhaseStuck {
			continue
		}
		kind := workersvc.StoredBlockingKind(sig)
		if kind != workersvc.BlockingKindSizeRefused && kind != workersvc.BlockingKindPVCRejected {
			continue
		}
		clean := func(s string) string { return termsafe.SanitizeBounded(termsafe.CellText(s), maxRefusalTextByte) }
		out = append(out, apitypes.WorkerSizeRefusalDTO{
			Size: clean(row.Worker.HostedSize.String), WorkerID: row.Worker.ID.String(),
			WorkerName: clean(row.Worker.Name), Kind: kind, Reason: clean(sig.BlockingReason),
			ReportedAt: row.RollObservedAt.Time,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size < out[j].Size
		}
		if out[i].WorkerName != out[j].WorkerName {
			return out[i].WorkerName < out[j].WorkerName
		}
		return out[i].WorkerID < out[j].WorkerID
	})
	if len(out) > maxSizeRefusals {
		out = out[:maxSizeRefusals]
	}
	return out
}

// refuseReadOnly answers 409 for a gated mutation, carrying the stable code and the reason.
func refuseReadOnly(w http.ResponseWriter, g sizeGate) {
	httpx.JSON(w, http.StatusConflict, map[string]string{
		"error":            "worker sizes are read-only: " + g.Reason,
		"reason":           g.Code,
		"read_only_reason": g.Reason,
	})
}

// sizeEditRequest is the PUT body. Pointers so an absent field is a field error, not a zero.
type sizeEditRequest struct {
	CPURequestMillicores *int64 `json:"cpu_request_millicores"`
	CPULimitMillicores   *int64 `json:"cpu_limit_millicores"`
	MemoryRequestMiB     *int64 `json:"memory_request_mib"`
	MemoryLimitMiB       *int64 `json:"memory_limit_mib"`
	DataGiB              *int64 `json:"data_gib"`
	DinDDataGiB          *int64 `json:"dind_data_gib"`
}

func (r sizeEditRequest) spec() (workersize.Spec, map[string]string) {
	missing := map[string]string{}
	val := func(field string, p *int64) int64 {
		if p == nil {
			missing[field] = "is required"
			return 0
		}
		return *p
	}
	s := workersize.Spec{
		CPURequestMillicores: val(workersize.FieldCPURequest, r.CPURequestMillicores),
		CPULimitMillicores:   val(workersize.FieldCPULimit, r.CPULimitMillicores),
		MemoryRequestMiB:     val(workersize.FieldMemRequest, r.MemoryRequestMiB),
		MemoryLimitMiB:       val(workersize.FieldMemLimit, r.MemoryLimitMiB),
		DataGiB:              val(workersize.FieldData, r.DataGiB),
		DinDDataGiB:          val(workersize.FieldDindData, r.DinDDataGiB),
	}
	return s, missing
}

func refuseFields(w http.ResponseWriter, fields map[string]string) {
	httpx.JSON(w, http.StatusBadRequest, map[string]any{
		"error":  "the size has values that cannot be saved",
		"fields": fields,
	})
}

// beginSizeTx opens a transaction and takes the worker_sizes advisory lock as its first
// statement, so the row it then re-reads is the committed state.
func (h *Handler) beginSizeTx(ctx context.Context) (pgx.Tx, error) {
	if h.pool == nil {
		return nil, errors.New("no pool")
	}
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 0)", store.WorkerSizesLockClass); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// AdminPutWorkerSize serves PUT /api/admin/worker-sizes/{name}: edit an existing size, or
// create the size when no row has that name (201). A name that is not a well-formed size name
// is a 400, since nothing could ever be stored under it.
func (h *Handler) AdminPutWorkerSize(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	// The gate precedes everything else: a refused request touches no row and no workload.
	gate, err := h.adminSizesGate(ctx)
	if err != nil {
		slog.Error("worker sizes gate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !gate.Editable {
		refuseReadOnly(w, gate)
		return
	}
	name := chi.URLParam(r, "name")
	if !builtin.WellFormed(name) {
		httpx.Error(w, http.StatusBadRequest, "invalid worker size name: use a lowercase letter then lowercase letters, digits or hyphens, at most 16 characters")
		return
	}
	var req sizeEditRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, workerSizeBodyLimit); err != nil {
		httpx.RespondDecodeError(w, err, "invalid worker size request")
		return
	}
	spec, missing := req.spec()
	if len(missing) > 0 {
		refuseFields(w, missing)
		return
	}

	tx, err := h.beginSizeTx(ctx)
	if err != nil {
		slog.Error("worker size begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	row, err := qtx.LockWorkerSizeForUpdate(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		h.createWorkerSize(ctx, w, tx, user.ID, name, spec)
		return
	}
	if err != nil {
		slog.Error("worker size read", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	prev := rowSpec(row)
	res := workersize.Validate(name, spec, &prev, h.cfg.WorkerSizeCeilings, h.cfg.WorkerSizeQuota)
	if !res.OK() {
		refuseFields(w, res.Fields)
		return
	}
	customized := false
	if row.Builtin {
		shipped, found := shippedSpec(name)
		customized = !found || shipped != spec
	}
	updated, err := qtx.UpdateWorkerSizeValues(ctx, store.UpdateWorkerSizeValuesParams{
		Name:                 name,
		CpuRequestMillicores: spec.CPURequestMillicores, CpuLimitMillicores: spec.CPULimitMillicores,
		MemoryRequestMib: spec.MemoryRequestMiB, MemoryLimitMib: spec.MemoryLimitMiB,
		DataGib: spec.DataGiB, DindDataGib: spec.DinDDataGiB,
		Customized: customized,
		UpdatedBy:  pgtype.UUID{Bytes: user.ID, Valid: true},
	})
	if err != nil {
		slog.Error("worker size update", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("worker size commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("worker size edited", "size", name, "by_user_id", user.ID, "customized", customized)
	httpx.JSON(w, http.StatusOK, apitypes.AdminWorkerSizeWriteDTO{
		Size: adminSizeDTO(updated), Warnings: warningDTOs(res.Warnings), Advisories: advisoryDTOs(res.Advisories),
	})
}

// createWorkerSize is the create arm of PUT, running inside the size transaction after the
// advisory lock is held and the name was found absent. There is no previous row, so no
// disk_lowered warning. The count is its OWN statement read after the lock: the cap is refused
// at 16 existing rows, and only an add consults it.
func (h *Handler) createWorkerSize(ctx context.Context, w http.ResponseWriter, tx pgx.Tx, actor uuid.UUID, name string, spec workersize.Spec) {
	res := workersize.Validate(name, spec, nil, h.cfg.WorkerSizeCeilings, h.cfg.WorkerSizeQuota)
	if !res.OK() {
		refuseFields(w, res.Fields)
		return
	}
	n, err := store.CountWorkerSizes(ctx, tx)
	if err != nil {
		slog.Error("worker size count", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n >= maxWorkerSizes {
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"error":  fmt.Sprintf("at most %d worker sizes", maxWorkerSizes),
			"reason": "max_sizes",
		})
		return
	}
	created, err := store.InsertAdminWorkerSize(ctx, tx, name, spec.CPURequestMillicores, spec.CPULimitMillicores,
		spec.MemoryRequestMiB, spec.MemoryLimitMiB, spec.DataGiB, spec.DinDDataGiB, pgtype.UUID{Bytes: actor, Valid: true})
	if err != nil {
		slog.Error("worker size insert", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("worker size commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("worker size created", "size", name, "by_user_id", actor)
	httpx.JSON(w, http.StatusCreated, apitypes.AdminWorkerSizeWriteDTO{
		Size: adminSizeDTO(created), Warnings: warningDTOs(res.Warnings), Advisories: advisoryDTOs(res.Advisories),
	})
}

// AdminDeleteWorkerSize serves DELETE /api/admin/worker-sizes/{name}: remove an admin-added
// size nothing uses. The guard (builtin, a worker using it, a stored default-size setting
// naming it) is store.DeleteWorkerSizeGuarded, four separate statements under the size
// advisory lock; it never takes the settings advisory lock. A refusal is 409 with
// {error, reason: "builtin"|"in_use", used_by: {workers, settings}}; success is 200
// {deleted: name}.
func (h *Handler) AdminDeleteWorkerSize(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	gate, err := h.adminSizesGate(ctx)
	if err != nil {
		slog.Error("worker sizes gate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !gate.Editable {
		refuseReadOnly(w, gate)
		return
	}
	name := chi.URLParam(r, "name")
	if !builtin.WellFormed(name) {
		httpx.Error(w, http.StatusNotFound, "worker size not found")
		return
	}
	tx, err := h.beginSizeTx(ctx)
	if err != nil {
		slog.Error("worker size begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = store.DeleteWorkerSizeGuarded(ctx, tx, name)
	var refusal *store.WorkerSizeRefusal
	switch {
	case errors.Is(err, store.ErrWorkerSizeNotFound):
		httpx.Error(w, http.StatusNotFound, "worker size not found")
		return
	case errors.As(err, &refusal):
		settingsKeys := refusal.Settings
		if settingsKeys == nil {
			settingsKeys = []string{}
		}
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"error":   refusal.Error(),
			"reason":  refusal.Reason,
			"used_by": map[string]any{"workers": refusal.Workers, "settings": settingsKeys},
		})
		return
	case err != nil:
		slog.Error("worker size delete", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("worker size commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("worker size deleted", "size", name, "by_user_id", user.ID)
	httpx.JSON(w, http.StatusOK, map[string]string{"deleted": name})
}

// AdminResetWorkerSize serves POST /api/admin/worker-sizes/{name}/reset: a builtin size goes
// back to its shipped values with customized = false.
func (h *Handler) AdminResetWorkerSize(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	gate, err := h.adminSizesGate(ctx)
	if err != nil {
		slog.Error("worker sizes gate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !gate.Editable {
		refuseReadOnly(w, gate)
		return
	}
	name := chi.URLParam(r, "name")
	if !builtin.WellFormed(name) {
		httpx.Error(w, http.StatusNotFound, "worker size not found")
		return
	}
	tx, err := h.beginSizeTx(ctx)
	if err != nil {
		slog.Error("worker size begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	row, err := qtx.LockWorkerSizeForUpdate(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "worker size not found")
		return
	}
	if err != nil {
		slog.Error("worker size read", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	var shipped *builtin.Size
	all := builtin.Sizes()
	for i := range all {
		if all[i].Name == name {
			shipped = &all[i]
		}
	}
	if !row.Builtin || shipped == nil {
		httpx.Error(w, http.StatusBadRequest, "only builtin worker sizes can be reset")
		return
	}
	prev := rowSpec(row)
	target, _ := shippedSpec(name)
	// The shipped values must still fit the chart ceilings: a reset that the controller would
	// refuse at render is refused here instead, with the same field errors as an edit.
	res := workersize.Validate(name, target, &prev, h.cfg.WorkerSizeCeilings, h.cfg.WorkerSizeQuota)
	if !res.OK() {
		refuseFields(w, res.Fields)
		return
	}
	n, err := qtx.ResetBuiltinWorkerSize(ctx, store.ResetBuiltinWorkerSizeParams{
		Name:                 name,
		CpuRequestMillicores: shipped.CPURequestMillicores, CpuLimitMillicores: shipped.CPULimitMillicores,
		MemoryRequestMib: shipped.MemoryRequestMiB, MemoryLimitMib: shipped.MemoryLimitMiB,
		DataGib: shipped.DataGiB, DindDataGib: shipped.DinDDataGiB, DisplayOrder: shipped.DisplayOrder,
	})
	if err != nil || n == 0 {
		slog.Error("worker size reset", "error", err, "rows", n)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	updated, err := qtx.GetWorkerSize(ctx, name)
	if err != nil {
		slog.Error("worker size reread", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("worker size commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("worker size reset", "size", name, "by_user_id", user.ID)
	httpx.JSON(w, http.StatusOK, apitypes.AdminWorkerSizeWriteDTO{
		Size: adminSizeDTO(updated), Warnings: warningDTOs(res.Warnings), Advisories: advisoryDTOs(res.Advisories),
	})
}
