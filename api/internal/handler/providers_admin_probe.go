package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/operatorkey"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Admin Test of an organization key (PRD #4540 M1): POST /api/admin/providers/{id}/test dials the
// definition's own gateway with its organization key (two steps: a bounded request and a forced
// single-tool request, exactly as the per-user Test) and records the verdict as evidence for that exact
// key and revision. The key is opened for the call and the opened buffer is zeroed after it; the header
// strings built by providers.Probe and request copies are Go strings and are not cleared. The response,
// the audit line and every error carry the two verdicts and nothing from the gateway.
//
// Steps: (1) capture the definition and its key's identity in one read; (2) admission (allowlist, profile,
// then the per-definition limiter; the audit boundary starts here); (3) read and open the sealed key;
// (4) re-read the capture and refuse if anything it covers changed; (5) extend the write deadline;
// (6) dial; (7) record the evidence in a short transaction that locks the definition and then the key.

// Outcomes of an admitted admin Test, recorded on the admin_test audit line.
const (
	probeOutcomeKeyUnavailable    = "key_unavailable"
	probeOutcomeKeyReadError      = "key_read_error"
	probeOutcomeOpenFailed        = "open_failed"
	probeOutcomeRecheckError      = "recheck_error"
	probeOutcomeChangedBeforeDial = "changed_before_dial"
	probeOutcomeRefusedNotAllowed = "refused_not_allowed"
	probeOutcomeProbeFailed       = "probe_failed"
	probeOutcomeRecorded          = "probed_recorded"
	probeOutcomeStale             = "probed_stale"
	probeOutcomeStoreError        = "probed_store_error"
)

const (
	errStaleProbeMessage = "the organization key or provider changed while it was being tested; test it again"
)

// operatorProbeStore is the set of store reads and writes the admin Test performs. *store.Queries
// implements it; a test wraps it (Handler.adminProbeWrap) to inject an error or a pause at one of them.
type operatorProbeStore interface {
	GetOperatorProviderForTest(ctx context.Context, id uuid.UUID) (store.GetOperatorProviderForTestRow, error)
	GetOperatorProviderKeyForTest(ctx context.Context, arg store.GetOperatorProviderKeyForTestParams) (store.GetOperatorProviderKeyForTestRow, error)
	LockOperatorProviderForEvidence(ctx context.Context, arg store.LockOperatorProviderForEvidenceParams) (store.LockOperatorProviderForEvidenceRow, error)
	LockOperatorProviderKeyForEvidence(ctx context.Context, arg store.LockOperatorProviderKeyForEvidenceParams) (store.LockOperatorProviderKeyForEvidenceRow, error)
	UpsertOperatorProviderKeyTestResult(ctx context.Context, arg store.UpsertOperatorProviderKeyTestResultParams) error
}

func (h *Handler) operatorProbeStore(q *store.Queries) operatorProbeStore {
	if h.adminProbeWrap != nil {
		return h.adminProbeWrap(q)
	}
	return q
}

// sameProbeTuple reports whether the recheck still describes what was captured: the same revision and
// destination, the same key row at the same key_rev, still enabled and not retired. A rename is not part
// of it.
func sameProbeTuple(a, b store.GetOperatorProviderForTestRow) bool {
	return a.CurrentRevision == b.CurrentRevision && a.BaseUrl == b.BaseUrl && a.AuthStyle == b.AuthStyle &&
		a.Protocol == b.Protocol && bytes.Equal(a.ModelMap, b.ModelMap) &&
		a.KeyID == b.KeyID && a.KeyRev == b.KeyRev &&
		a.KeyDisabledAt.Valid == b.KeyDisabledAt.Valid && a.RetiredAt.Valid == b.RetiredAt.Valid
}

// AdminTestSharedProvider is POST /api/admin/providers/{id}/test.
func (h *Handler) AdminTestSharedProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}

	// No instance secret box means no key can be opened. Refused before the capture and admission, so no
	// audit line is written (as AdminPutOperatorKey does for its seal).
	if h.box == nil {
		internalProviderError(w, "open organization key for test", errors.New("no instance secret box"))
		return
	}

	// 1. Capture. A refusal here makes no outbound request and writes no audit line.
	st := h.operatorProbeStore(h.q)
	snap, err := st.GetOperatorProviderForTest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		internalProviderError(w, "get organization-key provider for test", err)
		return
	}
	switch {
	case snap.RetiredAt.Valid:
		writeProviderError(w, errProviderRetired)
		return
	case !snap.KeyID.Valid:
		httpx.Error(w, http.StatusConflict, "the provider has no organization key; set one before testing")
		return
	case snap.KeyDisabledAt.Valid:
		httpx.Error(w, http.StatusConflict, "the organization key is disabled; enable it before testing")
		return
	}

	// 2. Admission. The stored URL having left the allowlist is the typed refusal; the probe re-checks
	// it again before it dials.
	entry, allowed := h.cfg.ProviderBaseURLAllowed(snap.BaseUrl)
	if !allowed {
		writeBaseURLRefusal(w)
		return
	}
	var models providers.ModelMap
	if json.Unmarshal(snap.ModelMap, &models) != nil || providers.ValidateProfile(providers.EngineClaude, snap.Protocol, snap.AuthStyle, models) != nil {
		httpx.Error(w, http.StatusConflict, "the provider profile is invalid")
		return
	}
	model := providerHaikuModel(snap.ModelMap)
	if model == "" {
		httpx.Error(w, http.StatusConflict, "the provider has no probe model")
		return
	}
	prober, limiter := h.providerProbe()
	release, _, retryAfter, admitted := limiter.Acquire("admin-test:" + id.String())
	if !admitted {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		httpx.ErrorReason(w, http.StatusTooManyRequests, "too many provider tests; try again later", reasonProviderProbeRateLimited)
		return
	}
	defer release()

	// The audit boundary: exactly one admin_test line per admitted request, with one outcome and the
	// identity of the key tested, never the key.
	outcome := ""
	auth, tool := "", ""
	defer func() {
		extra := []any{"outcome", outcome, "key_id", uuid.UUID(snap.KeyID.Bytes).String(), "key_rev", snap.KeyRev.Int64, "revision", snap.CurrentRevision}
		if outcome == probeOutcomeRecorded || outcome == probeOutcomeStale || outcome == probeOutcomeStoreError {
			extra = append(extra, "auth_result", auth, "tool_result", tool)
		}
		auditProvider("admin_test", actor.ID, id, extra...)
	}()

	// 3. Key read and open. The sink read matches only the exact key row captured, while it is enabled.
	sealed, err := st.GetOperatorProviderKeyForTest(r.Context(), store.GetOperatorProviderKeyForTestParams{
		ID: snap.KeyID.Bytes, ProviderID: id, KeyRev: snap.KeyRev.Int64,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		outcome = probeOutcomeKeyUnavailable
		httpx.Error(w, http.StatusConflict, errStaleProbeMessage)
		return
	case err != nil:
		outcome = probeOutcomeKeyReadError
		internalProviderError(w, "read organization key for test", err)
		return
	}
	key, err := operatorkey.Open(h.box, id, sealed.Ciphertext)
	if err != nil {
		outcome = probeOutcomeOpenFailed
		internalProviderError(w, "open organization key for test", errors.New("the sealed key did not open"))
		return
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()

	// 4. Re-read the capture after the open and before any network request: a change to the revision,
	// destination, key or its enablement refuses the Test, so a new destination never gets the old key.
	// Edits after this point can only leave an in-flight probe using the key and endpoint it captured.
	again, err := st.GetOperatorProviderForTest(r.Context(), id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		outcome = probeOutcomeChangedBeforeDial
		httpx.Error(w, http.StatusConflict, errStaleProbeMessage)
		return
	case err != nil:
		outcome = probeOutcomeRecheckError
		internalProviderError(w, "recheck organization-key provider for test", err)
		return
	case !sameProbeTuple(snap, again):
		outcome = probeOutcomeChangedBeforeDial
		httpx.Error(w, http.StatusConflict, errStaleProbeMessage)
		return
	}

	// 5. The listener's 15 s WriteTimeout would cut a probe (two steps of up to 20 s) short.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2*providers.DefaultProbeStepTimeout + providerProbeResponseGrace))

	// 6. Dial. The probe follows no redirects; it classifies every failure and returns an error only for
	// a destination that is not allowed, before any I/O.
	res, err := prober.Probe(r.Context(), providers.ProbeTarget{Protocol: snap.Protocol, BaseURL: entry, AuthStyle: snap.AuthStyle, Model: model, Key: key})
	if errors.Is(err, providers.ErrBaseURLNotAllowed) {
		outcome = probeOutcomeRefusedNotAllowed
		writeBaseURLRefusal(w)
		return
	}
	if err != nil {
		// Defensive: Prober.Probe documents only ErrBaseURLNotAllowed as an error return.
		outcome = probeOutcomeProbeFailed
		internalProviderError(w, "organization-key provider probe", err)
		return
	}
	auth, tool = res.Auth, res.Tool
	if h.adminProbeAfterDial != nil {
		h.adminProbeAfterDial()
	}

	// 7. Evidence.
	written, err := h.recordOperatorProbeEvidence(r.Context(), actor.ID, id, snap, res)
	if err != nil {
		outcome = probeOutcomeStoreError
		internalProviderError(w, "store organization-key probe result", err)
		return
	}
	if !written {
		outcome = probeOutcomeStale
		httpx.Error(w, http.StatusConflict, errStaleProbeMessage)
		return
	}
	// The outcome and the response follow the write alone: a change that commits after it may hide the
	// evidence from a later admin read, but this Test did record it. probed_at is the handler's clock at
	// the write (the stored value is the database's now()).
	outcome = probeOutcomeRecorded
	httpx.JSON(w, http.StatusOK, map[string]*apitypes.ProviderProbeDTO{"probe": {
		AuthResult: res.Auth, ToolResult: res.Tool, ProbedAt: time.Now().UTC(),
	}})
}

// recordOperatorProbeEvidence stores the Test result for the exact tuple the probe ran against, in one
// short transaction AFTER the probe returned (no lock spans the network call): the definition FOR SHARE
// at the probed revision, then the key FOR SHARE at the probed key_rev, then the upsert. A key replace,
// disable or delete, an edit or a retirement that committed first leaves a lock with no row, so a late
// result writes nothing and the caller reports the conflict; one still in flight is waited for and
// re-checked.
func (h *Handler) recordOperatorProbeEvidence(ctx context.Context, actorID, providerID uuid.UUID, snap store.GetOperatorProviderForTestRow, res providers.ProbeResult) (bool, error) {
	if h.pool == nil {
		return false, store.ErrVaultTransactionRequired
	}
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	q := h.operatorProbeStore(store.New(tx))
	if _, err := q.LockOperatorProviderForEvidence(ctx, store.LockOperatorProviderForEvidenceParams{ID: providerID, Revision: snap.CurrentRevision}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err := q.LockOperatorProviderKeyForEvidence(ctx, store.LockOperatorProviderKeyForEvidenceParams{
		KeyID: snap.KeyID.Bytes, ProviderID: providerID, KeyRev: snap.KeyRev.Int64,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if err := q.UpsertOperatorProviderKeyTestResult(ctx, store.UpsertOperatorProviderKeyTestResultParams{
		ProviderID: providerID, Revision: snap.CurrentRevision, KeyID: snap.KeyID.Bytes, KeyRev: snap.KeyRev.Int64,
		AuthResult: res.Auth, ToolResult: pgconv.Text(res.Tool), ProbedBy: actorID,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
