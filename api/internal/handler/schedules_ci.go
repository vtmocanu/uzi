package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// schedules_ci.go holds the ci_watch (PRD #2865) pieces of the schedule surface: the
// locked config-edit transaction, the repoint refusal, and the episode listing.

const (
	// ciEpisodePageMax is both the default and the maximum page size of the episode
	// listing; it matches the ListCIWatchEpisodes query clamp.
	ciEpisodePageMax = 100
	// ciEpisodeCursorMaxBytes bounds a decoded cursor (a workflow key), 1..200 bytes.
	ciEpisodeCursorMaxBytes = 200
)

// scheduleConfigLock is the transaction a ci_watch config edit runs in. q is bound to
// the transaction; the schedule row lock is held from lockScheduleForConfig until
// commit or rollback.
type scheduleConfigLock struct {
	tx pgx.Tx
	q  *store.Queries
}

// rollback is safe after commit (pgx reports ErrTxClosed, ignored) and uses a context
// that survives request cancellation so the lock is never left to a dead connection.
func (l *scheduleConfigLock) rollback() {
	if l == nil || l.tx == nil {
		return
	}
	_ = l.tx.Rollback(context.WithoutCancel(context.Background()))
}

func (l *scheduleConfigLock) commit(ctx context.Context) error {
	return l.tx.Commit(ctx)
}

// lockScheduleForConfig opens a transaction and takes LockRunScheduleForUser, the same
// owner-scoped row lock every ci_watch fire transition takes, as its FIRST statement.
// It returns the locked row. On failure it has written the HTTP error (404 for an
// absent or foreign schedule) and returns ok=false.
func (h *Handler) lockScheduleForConfig(w http.ResponseWriter, ctx context.Context, userID, id uuid.UUID) (*scheduleConfigLock, store.RunSchedule, bool) {
	if h.pool == nil {
		slog.Error("lock schedule for config: no pool", "schedule", id.String())
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return nil, store.RunSchedule{}, false
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("lock schedule for config: begin", "schedule", id.String(), "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return nil, store.RunSchedule{}, false
	}
	lock := &scheduleConfigLock{tx: tx, q: h.q.WithTx(tx)}
	row, err := lock.q.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: id, UserID: userID})
	if err != nil {
		lock.rollback()
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "schedule not found")
			return nil, store.RunSchedule{}, false
		}
		slog.Error("lock schedule for config: lock", "schedule", id.String(), "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return nil, store.RunSchedule{}, false
	}
	return lock, row, true
}

// ciEpisodesBlockRepoint reports whether repointing the schedule must be refused because
// it already has CI-watch episode history (409). A lookup failure is a 500, never
// treated as "no episodes".
func (h *Handler) ciEpisodesBlockRepoint(ctx context.Context, q *store.Queries, userID, id uuid.UUID) (bool, int, string) {
	has, err := q.HasCIWatchEpisodes(ctx, store.HasCIWatchEpisodesParams{ScheduleID: id, UserID: userID})
	if err != nil {
		slog.Error("check ci watch episodes", "schedule", id.String(), "error", err)
		return true, http.StatusInternalServerError, "internal error"
	}
	if has {
		return true, http.StatusConflict, "this schedule has CI watch history for its current repo and cannot be repointed; delete and recreate it against the new repo"
	}
	return false, 0, ""
}

func encodeCIEpisodeCursor(key string) apitypes.CIEpisodeCursor {
	return apitypes.CIEpisodeCursor(base64.RawURLEncoding.EncodeToString([]byte(key)))
}

// decodeCIEpisodeCursor returns the exclusive workflow key a cursor names; ok=false for
// anything that is not unpadded URL-safe base64 of 1..200 bytes of NUL-free UTF-8.
func decodeCIEpisodeCursor(c string) (string, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(c)
	if err != nil || len(raw) < 1 || len(raw) > ciEpisodeCursorMaxBytes || !utf8.Valid(raw) {
		return "", false
	}
	for _, b := range raw {
		if b == 0 {
			return "", false
		}
	}
	return string(raw), true
}

func ciEpisodeDTO(e store.CiWatchEpisode) apitypes.CIEpisodeDTO {
	d := apitypes.CIEpisodeDTO{
		WorkflowKey: e.WorkflowKey,
		Attempts:    int(e.Attempts),
		UpdatedAt:   e.UpdatedAt.Time,
	}
	if e.LastHandledRunID.Valid {
		v := e.LastHandledRunID.Int64
		d.LastHandledRunID = &v
	}
	if e.ObservedCreatedAt.Valid {
		t := e.ObservedCreatedAt.Time
		d.ObservedCreatedAt = &t
	}
	if e.ObservedUpdatedAt.Valid {
		t := e.ObservedUpdatedAt.Time
		d.ObservedUpdatedAt = &t
	}
	if e.ObservedRunID.Valid {
		v := e.ObservedRunID.Int64
		d.ObservedRunID = &v
	}
	if e.LastSignature.Valid {
		v := e.LastSignature.String
		d.LastSignature = &v
	}
	if e.HaltedReason.Valid {
		v := e.HaltedReason.String
		d.HaltedReason = &v
	}
	return d
}

// ciEpisodePage reads one owner-scoped page of episodes after afterKey (nil = first
// page) holding at most limit entries. It fills a next cursor only when a further row
// exists, found by probing one row past the page's last key.
func (h *Handler) ciEpisodePage(ctx context.Context, userID, id uuid.UUID, afterKey *string, limit int) (apitypes.CIEpisodePage, error) {
	after := pgtype.Text{}
	if afterKey != nil {
		after = pgtype.Text{String: *afterKey, Valid: true}
	}
	rows, err := h.q.ListCIWatchEpisodes(ctx, store.ListCIWatchEpisodesParams{
		ScheduleID: id, UserID: userID, AfterKey: after,
		PageSize: pgtype.Int4{Int32: int32(limit), Valid: true}, //nolint:gosec // G115: limit is validated to [1, ciEpisodePageMax].
	})
	if err != nil {
		return apitypes.CIEpisodePage{}, err
	}
	page := apitypes.CIEpisodePage{Episodes: make([]apitypes.CIEpisodeDTO, 0, len(rows))}
	for _, e := range rows {
		page.Episodes = append(page.Episodes, ciEpisodeDTO(e))
	}
	if len(rows) == limit {
		last := rows[len(rows)-1].WorkflowKey
		more, err := h.q.ListCIWatchEpisodes(ctx, store.ListCIWatchEpisodesParams{
			ScheduleID: id, UserID: userID, AfterKey: pgtype.Text{String: last, Valid: true},
			PageSize: pgtype.Int4{Int32: 1, Valid: true},
		})
		if err != nil {
			return apitypes.CIEpisodePage{}, err
		}
		if len(more) > 0 {
			c := encodeCIEpisodeCursor(last)
			page.NextCursor = &c
		}
	}
	return page, nil
}

// ListScheduleCIEpisodes pages a ci_watch schedule's episode state
// (GET /api/schedules/{id}/ci-episodes?cursor=&limit=). The schedule is owner-scoped
// (404 for a foreign or absent id); a malformed cursor or limit is a 400.
func (h *Handler) ListScheduleCIEpisodes(w http.ResponseWriter, r *http.Request) {
	user, id, ok := h.scheduleParam(w, r)
	if !ok {
		return
	}
	var afterKey *string
	if c := r.URL.Query().Get("cursor"); c != "" {
		key, valid := decodeCIEpisodeCursor(c)
		if !valid {
			httpx.Error(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		afterKey = &key
	}
	limit := ciEpisodePageMax
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ciEpisodePageMax {
			httpx.Error(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(ciEpisodePageMax))
			return
		}
		limit = n
	}
	if _, err := h.q.GetRunScheduleForUser(r.Context(), store.GetRunScheduleForUserParams{ID: id, UserID: user.ID}); err != nil {
		httpx.Error(w, http.StatusNotFound, "schedule not found")
		return
	}
	page, err := h.ciEpisodePage(r.Context(), user.ID, id, afterKey, limit)
	if err != nil {
		slog.Error("list ci watch episodes", "schedule", id.String(), "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}
