package handler

// Admin editing of hosted worker sizes (PRD #4213 M2): GET /api/admin/worker-sizes,
// PUT /api/admin/worker-sizes/{name} and POST /api/admin/worker-sizes/{name}/reset.
//
// M2 edits and resets EXISTING rows only; adding and deleting sizes is M3. Every mutation is
// gated on a fresh controller capability report (adminSizesGate) so an edit the controller
// would silently ignore is refused instead, and runs in one transaction under the table's
// advisory lock. Validation is integer arithmetic in workersize.Validate.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersize"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	// maxWorkerSizes is the table's size bound (PRD #4213); M3's add path enforces it.
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

// AdminPutWorkerSize serves PUT /api/admin/worker-sizes/{name}: edit an existing size.
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
		httpx.Error(w, http.StatusNotFound, "worker size not found")
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
		httpx.Error(w, http.StatusNotFound, "worker size not found")
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
