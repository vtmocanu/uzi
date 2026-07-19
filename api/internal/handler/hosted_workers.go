package handler

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/hostedsvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersize"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workertmpl"
)

// Hosted worker provisioning (PRD #58 M2).
//
// There is no hosted DELETE here, and that is the design: DELETE /api/workers/{id}
// (workers.go) already implements Decision 11 verbatim — 409 while the worker holds
// a non-terminal run, 404 for a foreign id, and the hosted_worker_tokens FK cascade
// destroys any pending sealed token with the row. Its kind-blindness is the point;
// a hosted-only delete would duplicate the refusal rule and drift from it. The
// controller then observes the worker gone from its poll and tears the cluster
// objects down.

// errHostedQuotaExceeded is the quota refusal: the user already holds their full
// allowance of hosted workers. Raised by provisionHostedWorker's count, under the
// lock that makes that count trustworthy.
var errHostedQuotaExceeded = errors.New("hosted worker quota exceeded")

// ProvisionHostedWorker creates a hosted worker: a worker whose container the
// CONTROLLER runs in the cluster (PRD #58). It mints the join token, but — unlike
// CreateWorker — it NEVER returns it.
//
// That asymmetry is Decision 3 and is the whole point of this endpoint's shape. A
// hand-run worker's token has exactly one consumer: the human who pastes it into
// their `docker run`, so CreateWorker shows it once. A hosted worker's token has
// exactly one legitimate consumer: the controller, which collects it from the
// desired-state poll and writes it into the worker's k8s Secret. The user is never
// in that path, so putting the plaintext in this response would create a second
// copy with no reader — the one thing "delivered once, never at rest in plaintext
// server-side" exists to prevent. The token's whole lifetime is inside
// provisionHostedWorker below, which returns no token at all.
func (h *Handler) ProvisionHostedWorker(w http.ResponseWriter, r *http.Request) {
	// Feature kill-switch first, before the body is even read — the shape Register
	// uses (auth.go): a well-formed request the policy forbids is 403, not 400, and
	// a malformed body under a disabled feature must not leak that the body was the
	// problem. This is also the ONLY thing standing between a flag-off instance and
	// a sealed token in its database (M2 constraint 2).
	if !h.cfg.WorkerHostingEnabled {
		httpx.Error(w, http.StatusForbidden, "worker hosting is disabled on this instance")
		return
	}
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Name string `json:"name"`
		// Template and Size are both MANDATORY here, unlike CreateWorker's optional
		// template. ck_workers_hosted_metadata forbids NULL for either on a hosted
		// row, and the meaning differs: for an external worker a NULL template means
		// "we don't know what image you will run", which is legitimate; for a hosted
		// one WE run the image, so a silent default would pick it for the user
		// (Decision 7 makes the type a deliberate choice).
		Template string `json:"template"`
		Size     string `json:"size"`
		// Docker opts the worker into a rootless-DinD sidecar (PRD #83 M3), so its
		// agent can run docker/docker compose. A pod-shape DIMENSION orthogonal to
		// template (Decision 1: docker is not a template), which is why it is a bool
		// here and not another template name. Absent → false → no sidecar; the k8s
		// controller renders the privileged DinD sidecar only when it is true, in the
		// dedicated privileged-tier namespace.
		Docker bool `json:"docker"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	template := strings.TrimSpace(req.Template)
	if !workertmpl.Valid(template) {
		// Membership, not workertmpl.WellFormed. The weaker check exists for a
		// worker's SELF-REPORTED template, where an unknown-but-well-formed name is
		// the drift signal the UI badges; here the value selects an image the
		// controller will run, so free text must never reach a pod spec.
		httpx.Error(w, http.StatusBadRequest, "unknown worker template")
		return
	}
	size := strings.TrimSpace(req.Size)
	if !workersize.Valid(size) {
		httpx.Error(w, http.StatusBadRequest, "unknown worker size")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = derivedHostedWorkerName(template, size)
	}
	if len(name) > maxWorkerNameBytes {
		httpx.Error(w, http.StatusBadRequest, "name must be at most 200 characters")
		return
	}

	// Read the quota STRICTLY: a cold-cache error is a 500, never a fallback. The
	// settings cache's TTL means this can be up to SETTINGS_CACHE_TTL stale after an
	// admin lowers the quota; that is accepted and benign — the quota gates NEW
	// provisions and never removes existing workers, so a slightly stale pass is
	// indistinguishable from a request that arrived a moment earlier.
	quota, err := h.settings.HostedWorkerQuota(r.Context())
	if err != nil {
		slog.Error("hosted worker quota setting", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if quota <= 0 {
		// Policy, not state: the admin has turned self-service off (Decision 8's "0
		// disables"), and no amount of deleting workers makes this request succeed.
		// That is what separates it from the 409 below.
		httpx.Error(w, http.StatusForbidden, "self-service worker provisioning is disabled")
		return
	}

	wkr, err := h.provisionHostedWorker(r.Context(), user.ID, name, template, size, req.Docker, quota)
	if err != nil {
		if errors.Is(err, errHostedQuotaExceeded) {
			// State, not policy: the user can delete a hosted worker and retry. Mirrors
			// ErrWorkerHasActiveRuns → 409.
			httpx.Error(w, http.StatusConflict, fmt.Sprintf("hosted worker quota reached (%d)", quota))
			return
		}
		// Never wrap the token into an error: nothing on this path may log or return
		// plaintext, including a failed seal.
		slog.Error("provision hosted worker", "user_id", user.ID.String(), "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// No "token" field, deliberately — see this handler's doc comment. The DTO is the
	// same one the worker list returns, so the caller sees kind/hosted_size and can
	// render the row it just created.
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"worker": workerDTOFromWorker(wkr, 0, false),
	})
}

// derivedHostedWorkerName builds a display name for a provision that supplied none
// (the M5 dialog collects type + size, but workers.name is NOT NULL). Names are not
// unique-constrained and the controller never reads one — its desired-state poll
// deliberately does not select the name, since object names derive from the uuid —
// so a derived, non-unique name is safe and purely cosmetic. Size upper-cases for
// display only; the stored/wire value stays lowercase.
func derivedHostedWorkerName(template, size string) string {
	return fmt.Sprintf("%s (%s)", template, strings.ToUpper(size))
}

// provisionHostedWorker is the provision transaction: lock, count, insert, seal,
// commit — in that order, and the order is the whole design.
//
// It is deliberately the same shape as createUserFirstAdmin (auth.go), which
// solves the identical race: take pg_advisory_xact_lock as the transaction's first
// statement, then do a count-then-insert that is only sound because the lock is
// held. Under READ COMMITTED the count alone is a TOCTOU — two concurrent
// provisions both read N-1 and both insert. The lock, not the count and not any
// cleverness in the SQL, is the mechanism. An earlier draft pushed the check into a
// guarded `INSERT … WHERE count < quota`; that was dropped because it has the same
// hole while reading as though it closed it. Keeping the decision here, in Go,
// means the safety property reads top-to-bottom in one function and the refusal can
// name the real count.
//
// It returns (store.Worker, error) and NO TOKEN, deliberately. The plaintext's
// entire lifetime is this function body, so the HTTP layer above literally cannot
// put it in a response even by a future caller's mistake — Decision 3's "never at
// rest in plaintext server-side" enforced by shape rather than by discipline.
//
// The two writes below MUST stay in this one transaction (the auditor constraint
// pinned in the PRD's M2 bullet). MarkHostedWorkerTokenDelivered destroys the parked
// buffer under `AND token_hash = @proved_token_hash` — the hash the registering pod
// actually proved — and that predicate is correct only while "the hash I proved is
// still current" means "the parked ciphertext is the token I proved". Those are the
// same statement only because token_hash and the ciphertext are written together
// here. Split them and nothing fails loudly: a worker ends up holding a token whose
// plaintext was never queued for anyone, or a buffer outlives the hash it belongs to.
func (h *Handler) provisionHostedWorker(ctx context.Context, userID uuid.UUID, name, template, size string, docker bool, quota int) (store.Worker, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return store.Worker{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	qtx := h.q.WithTx(tx)

	// FIRST statement, before the count it protects. Serializes this USER's
	// provisions (registration's lock is global; this one is keyed per user, the
	// only difference). Held until the transaction ends; there is no unlock to
	// forget. Move it below the count and the count is worthless again.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.HostedProvisionLockClass, hostedProvisionLockObjID(userID)); err != nil {
		return store.Worker{}, err
	}

	n, err := qtx.CountHostedWorkersForUser(ctx, userID)
	if err != nil {
		return store.Worker{}, err
	}
	if n >= int64(quota) {
		// Refuse before minting anything: no token is generated, so there is no
		// plaintext to seal and nothing to roll back but the lock.
		return store.Worker{}, fmt.Errorf("%w: holds %d of %d", errHostedQuotaExceeded, n, quota)
	}

	token, hash, err := jointoken.Generate()
	if err != nil {
		return store.Worker{}, err
	}

	// Both are non-empty by the gates above, and ck_workers_hosted_metadata requires
	// them NOT NULL on a hosted row — hence Valid: true unconditionally, unlike
	// CreateWorker's "empty → NULL" for an external worker's optional template.
	wkr, err := qtx.CreateHostedWorker(ctx, store.CreateHostedWorkerParams{
		UserID:           userID,
		Name:             name,
		TokenHash:        hash,
		TemplateDeclared: pgtype.Text{String: template, Valid: true},
		HostedSize:       pgtype.Text{String: size, Valid: true},
		// Explicit true/false (Valid always), never NULL: on a hosted row a false is a
		// real "no sidecar", and the controller renders exactly what this says. Only
		// external rows (created via CreateWorker) leave the column NULL.
		DockerEnabled: pgtype.Bool{Bool: docker, Valid: true},
	})
	if err != nil {
		return store.Worker{}, err
	}

	// The co-write. SealJoinToken is a free function over hostedsvc.Store precisely
	// so it can take qtx and land in THIS transaction (M1 shaped it that way for
	// this call site). Sealing via hostedsvc — never box.Seal directly — is what
	// binds the ciphertext to this worker's AAD; a direct seal would fail closed
	// later, at the controller poll's OpenWithAAD, with the worker already created.
	if err := hostedsvc.SealJoinToken(ctx, qtx, h.box, wkr.ID, token); err != nil {
		return store.Worker{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return store.Worker{}, err
	}
	return wkr, nil
}

// hostedProvisionLockObjID derives the objid half of the per-user advisory lock
// from a user's uuid. A uuid's leading bytes are random, so two users can collide
// here; the consequence is that two unrelated provisions serialize for a moment,
// which costs latency and never correctness.
func hostedProvisionLockObjID(userID uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(userID[:4])) //nolint:gosec // wraparound is fine: this is a lock key, not a number
}

// HostedConfig reports whether hosting is available and the current per-user quota,
// so the SPA can hide everything hosted when it is off (Decision 12) rather than
// discovering it from a 403. Session-authenticated; it exposes only operator-set
// policy, which is the same thing ForgeConfig does with the SSRF allowlist.
//
// The type and size LISTS are deliberately not here: this repo's pattern for a
// curated registry the UI must render is a web-side mirror constant
// (web/src/lib/workerTemplates.ts mirrors workertmpl.Names), and M5 adds the size
// list the same way.
func (h *Handler) HostedConfig(w http.ResponseWriter, r *http.Request) {
	// The quota is reported even when hosting is off. It is operator-set policy
	// either way, and a client that reads enabled=false must not render a provision
	// affordance regardless of the number beside it.
	quota, err := h.settings.HostedWorkerQuota(r.Context())
	if err != nil {
		slog.Error("hosted worker quota setting", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"enabled": h.cfg.WorkerHostingEnabled,
		"quota":   quota,
	})
}
