package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4784: per-user JSON documents for runtime web modules.

// maxModuleUserDataBody is the PUT body cap, wrapper included.
const maxModuleUserDataBody = 65536

// moduleIDRe mirrors MODULE_ID_RE in web/src/modules/manifest.ts.
var moduleIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// moduleUserDataNoStore marks every /api/modules/ response no-store. It runs at the
// router root because chi answers 405 for a method missing from a route's methodMap
// before any subrouter middleware, and an auth-layer 503 or 401 must carry it too.
func moduleUserDataNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/modules/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// moduleUserDataStore is the storage seam of the module document routes.
type moduleUserDataStore interface {
	Get(ctx context.Context, userID uuid.UUID, moduleID string) ([]byte, *int64, error)
	Put(ctx context.Context, userID uuid.UUID, moduleID string, data []byte, exp store.ExpectedRevision) (int64, error)
	Delete(ctx context.Context, userID uuid.UUID, moduleID string, exp store.ExpectedRevision) error
}

type poolModuleUserData struct {
	q    *store.Queries
	pool *pgxpool.Pool
}

func (p poolModuleUserData) Get(ctx context.Context, u uuid.UUID, m string) ([]byte, *int64, error) {
	return store.GetModuleUserDataDoc(ctx, p.q, u, m)
}

func (p poolModuleUserData) Put(ctx context.Context, u uuid.UUID, m string, d []byte, e store.ExpectedRevision) (int64, error) {
	return store.PutModuleUserData(ctx, p.pool, u, m, d, e)
}

func (p poolModuleUserData) Delete(ctx context.Context, u uuid.UUID, m string, e store.ExpectedRevision) error {
	return store.DeleteModuleUserData(ctx, p.pool, u, m, e)
}

func (h *Handler) moduleStore() moduleUserDataStore {
	if h.moduleData != nil {
		return h.moduleData
	}
	return poolModuleUserData{q: h.q, pool: h.pool}
}

// moduleRequest authenticates context and validates the module id; it writes the
// failure response itself. The id check comes first.
func moduleRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	id := chi.URLParam(r, "module_id")
	if !moduleIDRe.MatchString(id) {
		httpx.Error(w, http.StatusBadRequest, "invalid module")
		return uuid.Nil, "", false
	}
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return uuid.Nil, "", false
	}
	return user.ID, id, true
}

// parseExpectedRevision maps a client revision string: only the canonical decimal
// form of an int64 can match; anything else can never match.
func parseExpectedRevision(s string) store.ExpectedRevision {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(v, 10) != s {
		return store.ExpectedRevision{Kind: store.ExpectNeverMatch}
	}
	return store.ExpectedRevision{Kind: store.ExpectMatch, Value: v}
}

type moduleRevisionBody struct {
	Revision *string `json:"revision"`
}

func revString(p *int64) *string {
	if p == nil {
		return nil
	}
	s := strconv.FormatInt(*p, 10)
	return &s
}

// moduleStoreError maps a store error to a response. Shared by PUT and DELETE.
func moduleStoreError(w http.ResponseWriter, op string, err error) {
	var conflict *store.ModuleDataConflict
	var commit *store.ModuleDataCommitError
	switch {
	case errors.As(err, &conflict):
		httpx.JSON(w, http.StatusConflict, moduleRevisionBody{Revision: revString(conflict.Current)})
	case errors.Is(err, store.ErrModuleDataCap):
		httpx.Error(w, http.StatusConflict, "too many module documents")
	case errors.Is(err, store.ErrModuleDataInvalid):
		httpx.Error(w, http.StatusBadRequest, "invalid data")
	case errors.Is(err, store.ErrModuleDataNoUser):
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
	case errors.As(err, &commit):
		slog.Error("module user data: commit outcome unknown", "op", op, "error", err)
		httpx.Error(w, http.StatusServiceUnavailable, "outcome unknown")
	default:
		slog.Error("module user data", "op", op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// GetModuleUserData returns the caller's document and revision for a module. The
// body is assembled by hand: the stored value may nest 10,000 deep, which the
// stdlib encoder refuses once wrapped.
func (h *Handler) GetModuleUserData(w http.ResponseWriter, r *http.Request) {
	userID, moduleID, ok := moduleRequest(w, r)
	if !ok {
		return
	}
	data, rev, err := h.moduleStore().Get(r.Context(), userID, moduleID)
	if err != nil {
		moduleStoreError(w, "get", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if rev == nil {
		_, _ = io.WriteString(w, "{\"data\":null,\"revision\":null}\n")
		return
	}
	for _, part := range [][]byte{[]byte(`{"data":`), data, []byte(`,"revision":"` + strconv.FormatInt(*rev, 10) + "\"}\n")} {
		if _, err := w.Write(part); err != nil { //nolint:gosec // G705: application/json with nosniff, set above.
			slog.Error("module user data: write response", "error", err)
			return
		}
	}
}

// PutModuleUserData stores the caller's document under an optimistic-concurrency
// precondition.
func (h *Handler) PutModuleUserData(w http.ResponseWriter, r *http.Request) {
	userID, moduleID, ok := moduleRequest(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxModuleUserDataBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	doc, err := scanModuleDoc(body)
	if err != nil {
		var de *moduleDocError
		if errors.As(err, &de) {
			httpx.Error(w, http.StatusBadRequest, de.Msg)
			return
		}
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	exp := store.ExpectedRevision{Kind: store.ExpectCreate}
	if doc.expectedRevision != nil {
		exp = parseExpectedRevision(*doc.expectedRevision)
	}
	rev, err := h.moduleStore().Put(r.Context(), userID, moduleID, doc.data, exp)
	if err != nil {
		moduleStoreError(w, "put", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"revision": strconv.FormatInt(rev, 10)})
}

// DeleteModuleUserData deletes the caller's document when ?expected_revision matches.
func (h *Handler) DeleteModuleUserData(w http.ResponseWriter, r *http.Request) {
	userID, moduleID, ok := moduleRequest(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if !q.Has("expected_revision") {
		httpx.Error(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	if err := h.moduleStore().Delete(r.Context(), userID, moduleID, parseExpectedRevision(q.Get("expected_revision"))); err != nil {
		moduleStoreError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
