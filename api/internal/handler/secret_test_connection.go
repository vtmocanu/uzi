package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type secretTestResult struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Display string `json:"display,omitempty"`
}

type anthropicTester interface {
	Usage(context.Context, []byte) (anthropic.Reading, error)
	ProbeTest(context.Context, []byte) (anthropic.Reading, bool, error)
}

type codexSecretTester interface {
	workersvc.CodexIdentityClient
	workersvc.CodexUsageReader
}

type secretTestClients struct {
	anthropic anthropicTester
	openai    *http.Client
	codex     codexSecretTester
}

// These buckets are independent of the login and poll budgets. A per-ID allowance
// bounds repeated spend on one credential; the owner allowance bounds ID hopping.
var secretTestLimiters struct {
	once     sync.Once
	perID    *mw.Limiter
	perOwner *mw.Limiter
}

func allowSecretTest(owner, kind string, id uuid.UUID) bool {
	secretTestLimiters.once.Do(func() {
		secretTestLimiters.perID = mw.NewLimiter(3, time.Minute, nil)
		secretTestLimiters.perOwner = mw.NewLimiter(12, time.Minute, nil)
	})
	if !secretTestLimiters.perOwner.Allow(owner) {
		return false
	}
	return secretTestLimiters.perID.Allow(owner + "|" + kind + "|" + id.String())
}

func (h *Handler) secretTester() *secretTestClients {
	h.secretTestOnce.Do(func() {
		h.secretTestClients = &secretTestClients{
			anthropic: anthropic.New(&http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}),
			openai:    &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
			codex:     codexauth.NewClient(codexauth.WithPerRequestTimeout(10 * time.Second)),
		}
	})
	return h.secretTestClients
}

func (h *Handler) armSecretTestProbe(parent context.Context) (context.Context, context.CancelFunc) {
	const budget = 12 * time.Second
	if h.secretTestProbeContext != nil {
		return h.secretTestProbeContext(parent, budget)
	}
	return context.WithTimeout(parent, budget)
}

// TestMySecret validates one owned, enabled credential. The result only contains
// fixed vocabulary; provider bodies and transport errors are never returned.
func (h *Handler) TestMySecret(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	kind := chi.URLParam(r, "kind")
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil || (kind != store.KindAnthropicToken && kind != store.KindCodexAuth && kind != store.KindOpenAIAPIKey) {
		httpx.Error(w, http.StatusNotFound, "secret not found")
		return
	}
	before, err := h.q.GetSecretEnablement(r.Context(), store.GetSecretEnablementParams{ID: id, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && before.Kind != kind {
		httpx.Error(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if before.DisabledAt.Valid {
		httpx.JSON(w, http.StatusOK, secretTestResult{Status: "inconclusive", Reason: "generic"})
		return
	}
	if !allowSecretTest(user.ID.String(), kind, id) {
		httpx.Error(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	ctx, cancel := h.armSecretTestProbe(r.Context())
	defer cancel()
	result := secretTestResult{Status: "inconclusive", Reason: "generic"}
	switch kind {
	case store.KindAnthropicToken:
		result = h.testAnthropicSecret(ctx, user.ID, id, before.EnablementRev)
	case store.KindOpenAIAPIKey:
		result = h.testOpenAISecret(ctx, user.ID, id)
	case store.KindCodexAuth:
		result = h.testCodexSecret(ctx, user.ID, id)
	}
	// Re-read on a fresh context: the provider calls may have spent the probe
	// budget, and an expired ctx here would mislabel a committed verdict superseded.
	readCtx, readCancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer readCancel()
	after, err := h.q.GetSecretEnablement(readCtx, store.GetSecretEnablementParams{ID: id, UserID: user.ID})
	if err != nil || after.Kind != kind || after.DisabledAt.Valid || after.EnablementRev != before.EnablementRev || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		result = secretTestResult{Status: "inconclusive", Reason: "superseded"}
	}
	httpx.JSON(w, http.StatusOK, result)
}

func (h *Handler) testAnthropicSecret(ctx context.Context, userID, id uuid.UUID, rev int64) secretTestResult {
	row, err := h.q.GetAnthropicTokenToPoll(ctx, store.GetAnthropicTokenToPollParams{UserID: userID, SecretID: pgconv.UUID(id)})
	if err != nil || row.EnablementRev != rev {
		return secretTestResult{Status: "inconclusive", Reason: "superseded"}
	}
	plain, err := secretopen.OpenSealed(h.vault, h.box, userID, store.KindAnthropicToken, row.SealedWith, row.Ciphertext)
	if errors.Is(err, secretopen.ErrVaultLocked) {
		return secretTestResult{Status: "inconclusive", Reason: "vault_locked"}
	}
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	return testAnthropicOutcome(ctx, h.secretTester().anthropic, plain, h.cfg.UsageProbe,
		func(reading anthropic.Reading) secretTestResult { return h.anthropicTestSuccess(ctx, row, reading) },
		func() secretTestResult { return h.anthropicTestClear(ctx, row) },
		func() secretTestResult { return h.anthropicTestRejected(ctx, row) })
}

func testAnthropicOutcome(ctx context.Context, client anthropicTester, plain []byte, usageProbe bool,
	success func(anthropic.Reading) secretTestResult, clear, reject func() secretTestResult) secretTestResult {
	reading, err := client.Usage(ctx, plain)
	if err == nil {
		return success(reading)
	}
	var providerErr *anthropic.Error
	if !errors.As(err, &providerErr) || providerErr.Kind != anthropic.KindHTTP {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	if !usageProbe {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	reading, hasGauge, err := client.ProbeTest(ctx, plain)
	if err == nil {
		if hasGauge {
			return success(reading)
		}
		return clear()
	}
	if errors.As(err, &providerErr) && providerErr.Kind == anthropic.KindHTTP &&
		(providerErr.Status == http.StatusUnauthorized || providerErr.Status == http.StatusForbidden) {
		return reject()
	}
	return secretTestResult{Status: "inconclusive", Reason: "generic"}
}

func (h *Handler) anthropicTestClear(ctx context.Context, row store.GetAnthropicTokenToPollRow) secretTestResult {
	if h.pool == nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	// A successful status-only probe clears the rejection marker without inventing
	// a gauge reading. The same enablement and success-generation fences apply.
	tag, err := h.pool.Exec(ctx, `UPDATE user_secrets SET anthropic_rejected_at = NULL,
		anthropic_success_generation = anthropic_success_generation + 1
		WHERE id = $1 AND user_id = $2 AND kind = 'anthropic_token'
		AND disabled_at IS NULL AND enablement_rev = $3
		AND anthropic_success_generation = $4`, row.ID, row.UserID, row.EnablementRev, row.AnthropicSuccessGeneration)
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	if tag.RowsAffected() == 0 {
		return secretTestResult{Status: "inconclusive", Reason: "superseded"}
	}
	return secretTestResult{Status: "ok"}
}

func (h *Handler) anthropicTestRejected(ctx context.Context, row store.GetAnthropicTokenToPollRow) secretTestResult {
	n, err := h.q.MarkAnthropicTokenRejected(ctx, store.MarkAnthropicTokenRejectedParams{
		UserSecretID: row.ID, UserID: row.UserID, EnablementRev: row.EnablementRev,
		AnthropicSuccessGeneration: row.AnthropicSuccessGeneration,
	})
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	if n == 0 {
		return secretTestResult{Status: "inconclusive", Reason: "superseded"}
	}
	return secretTestResult{Status: "rejected"}
}

func (h *Handler) anthropicTestSuccess(ctx context.Context, row store.GetAnthropicTokenToPollRow, reading anthropic.Reading) secretTestResult {
	n, err := h.q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID: row.ID, UserID: row.UserID, EnablementRev: row.EnablementRev,
		FiveHourPct:      pgtype.Int2{Int16: int16(reading.FiveHour.Pct), Valid: true}, //nolint:gosec // G115: a rate-limit percentage (0-100), far within int16 range
		FiveHourResetsAt: pgconv.TimePtr(reading.FiveHour.ResetsAt),
		SevenDayPct:      pgtype.Int2{Int16: int16(reading.SevenDay.Pct), Valid: true}, //nolint:gosec // G115: a rate-limit percentage (0-100), far within int16 range
		SevenDayResetsAt: pgconv.TimePtr(reading.SevenDay.ResetsAt),
		Source:           pgconv.Text(reading.Source), SyncedAt: pgconv.Time(time.Now().UTC()),
	})
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	if n == 0 {
		return secretTestResult{Status: "inconclusive", Reason: "superseded"}
	}
	return secretTestResult{Status: "ok"}
}

func (h *Handler) testOpenAISecret(ctx context.Context, userID, id uuid.UUID) secretTestResult {
	plain, err := secretopen.OpenByIDOfKind(ctx, h.q, h.vault, h.box, userID, id, store.KindOpenAIAPIKey)
	if errors.Is(err, secretopen.ErrVaultLocked) {
		return secretTestResult{Status: "inconclusive", Reason: "vault_locked"}
	}
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	return probeOpenAIModels(ctx, h.secretTester().openai, plain)
}

func probeOpenAIModels(ctx context.Context, client *http.Client, plain []byte) secretTestResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	req.Header.Set("Authorization", "Bearer "+string(plain))
	resp, err := client.Do(req)
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var models struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		const maxModelsResponseBytes = 2 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseBytes+1))
		if err != nil || len(body) > maxModelsResponseBytes || json.Unmarshal(body, &models) != nil ||
			models.Object != "list" || models.Data == nil {
			return secretTestResult{Status: "inconclusive", Reason: "generic"}
		}
		for _, model := range models.Data {
			if model.ID == "" || model.Object != "model" {
				return secretTestResult{Status: "inconclusive", Reason: "generic"}
			}
		}
		return secretTestResult{Status: "ok", Display: "models endpoint accessible"}
	case resp.StatusCode == http.StatusUnauthorized:
		return secretTestResult{Status: "rejected"}
	case resp.StatusCode == http.StatusForbidden:
		return secretTestResult{Status: "permission_denied"}
	default:
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
}

func (h *Handler) testCodexSecret(ctx context.Context, userID, id uuid.UUID) secretTestResult {
	st, err := h.q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: id, UserID: userID})
	if err != nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	client := h.secretTester().codex
	if st.Status == "staging" || st.Status == "failed" {
		if h.pool == nil {
			return secretTestResult{Status: "inconclusive", Reason: "generic"}
		}
		reconciler := workersvc.NewCodexReconciler(h.q, h.vault, h.box, client, h.pool)
		err = reconciler.ReconcileCodexAuthIdentity(ctx, userID, id)
		result := secretTestResult{Status: "ok"}
		if err != nil {
			var authErr *codexauth.AuthError
			switch {
			case errors.As(err, &authErr) && (authErr.StatusCode == 401 || authErr.StatusCode == 403):
				result = secretTestResult{Status: "rejected"}
			case errors.Is(err, secretopen.ErrVaultLocked):
				result = secretTestResult{Status: "inconclusive", Reason: "vault_locked"}
			default:
				result = secretTestResult{Status: "inconclusive", Reason: "generic"}
			}
		}
		fresh, readErr := h.q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: id, UserID: userID})
		if readErr != nil || fresh.MaterialRevision != st.MaterialRevision ||
			(fresh.Status != st.Status && fresh.Status != "linked" && fresh.Status != "failed") {
			return secretTestResult{Status: "inconclusive", Reason: "superseded"}
		}
		return result
	}
	if st.Status != "linked" || !st.ProviderAccountID.Valid || h.wsvc == nil {
		return secretTestResult{Status: "inconclusive", Reason: "generic"}
	}
	status, reason := h.wsvc.TestCodexLinkedAccount(ctx, userID, uuid.UUID(st.ProviderAccountID.Bytes), client)
	fresh, err := h.q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: id, UserID: userID})
	if err != nil || fresh.MaterialRevision != st.MaterialRevision || fresh.Status != st.Status || fresh.ProviderAccountID != st.ProviderAccountID {
		return secretTestResult{Status: "inconclusive", Reason: "superseded"}
	}
	return secretTestResult{Status: status, Reason: reason}
}
