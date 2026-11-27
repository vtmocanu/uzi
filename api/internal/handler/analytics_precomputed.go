package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4771: the admin all-users all-time Spend response, built in the background and served
// from a stored document. This file holds the serving path and the builder; the scheduler that
// calls BuildAllTimeSpendDocs lives elsewhere.

// AllTimeSpendSection is the section name under which the all-time Spend documents are stored.
const AllTimeSpendSection = "spend"

// analyticsPrecomputedReadTimeout bounds the one-row read of a stored document.
const analyticsPrecomputedReadTimeout = 2 * time.Second

// allTimeSpendKey is the variant key of one stored document.
func allTimeSpendKey(split, breakdown string) string {
	return "split=" + split + "&breakdown=" + breakdown
}

// AllTimeSpendKeys lists the variant keys of one generation: every split against every breakdown
// an admin all-users request may name, split-major. It derives from the request's own choice
// lists, so a dimension added there is built here without a second edit.
func AllTimeSpendKeys() []string {
	r := analyticsRequest{allUsers: true}
	var keys []string
	for _, split := range r.splitChoices() {
		for _, breakdown := range r.breakdownChoices() {
			keys = append(keys, allTimeSpendKey(split, breakdown))
		}
	}
	return keys
}

// analyticsSnapshotStale reports whether a document generated at generatedAt is older than twice
// the build interval at now (exactly twice is not stale).
func analyticsSnapshotStale(now, generatedAt time.Time, interval time.Duration) bool {
	return now.After(generatedAt.Add(interval).Add(interval))
}

// isNonEmptyJSONObject reports whether b starts with '{', ends with '}' and has something other
// than whitespace between them, so a member can be appended after a comma.
func isNonEmptyJSONObject(b []byte) bool {
	if len(b) < 3 || b[0] != '{' || b[len(b)-1] != '}' {
		return false
	}
	return len(bytes.TrimSpace(b[1:len(b)-1])) > 0
}

func (h *Handler) readAllTimeSpendDoc(ctx context.Context, key string) ([]byte, time.Time, error) {
	if h.analyticsPrecomputedDoc != nil {
		return h.analyticsPrecomputedDoc(ctx, AllTimeSpendSection, key)
	}
	row, err := h.q.GetAnalyticsPrecomputedDoc(ctx, store.GetAnalyticsPrecomputedDocParams{Section: AllTimeSpendSection, VariantKey: key})
	if err != nil {
		return nil, time.Time{}, err
	}
	return row.Document, row.GeneratedAt.Time, nil
}

// serveAllTimeSpend answers an admin all-users all-time Spend request from the stored document of
// its (split, breakdown) variant. It reads one row and takes no analytics admission slot.
func (h *Handler) serveAllTimeSpend(w http.ResponseWriter, r *http.Request, req analyticsRequest) {
	w.Header().Set("Cache-Control", "private, no-store")
	ctx, cancel := context.WithTimeout(r.Context(), analyticsPrecomputedReadTimeout)
	defer cancel()
	doc, generatedAt, err := h.readAllTimeSpendDoc(ctx, allTimeSpendKey(req.split, req.breakdown))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		w.Header().Set("Retry-After", "60")
		httpx.Error(w, http.StatusServiceUnavailable, "analytics snapshot pending, retry later")
		return
	case err != nil:
		if errors.Is(store.ClassifyAnalyticsErr(err), store.ErrAnalyticsTimeout) && r.Context().Err() == nil {
			slog.Warn("analytics: stored document read timed out", "error", err)
			w.Header().Set("Retry-After", "1")
			httpx.Error(w, http.StatusServiceUnavailable, "analytics query timed out, retry shortly")
			return
		}
		if r.Context().Err() != nil {
			slog.Debug("analytics: request cancelled", "error", err)
			return
		}
		slog.Error("analytics: read stored document", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	trimmed := bytes.TrimSpace(doc)
	if !isNonEmptyJSONObject(trimmed) {
		slog.Error("analytics: stored document is not a non-empty JSON object", "len", len(doc))
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	stale := analyticsSnapshotStale(h.clock().UTC(), generatedAt, h.cfg.AnalyticsSnapshotInterval)
	meta, err := json.Marshal(apitypes.AnalyticsSnapshotMetaDTO{GeneratedAt: generatedAt.UTC(), Stale: stale})
	if err != nil {
		slog.Error("analytics: encode snapshot member", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body := make([]byte, 0, len(trimmed)+len(meta)+16)
	body = append(body, trimmed[:len(trimmed)-1]...)
	body = append(body, `,"snapshot":`...)
	body = append(body, meta...)
	body = append(body, "}\n"...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// Test-only hooks of BuildAllTimeSpendDocs (nil in production). The index is 1-based: variant i of
// len(AllTimeSpendKeys()). beforeVariant runs before the variant is built, afterVariant after it
// was built and encoded; a returned error fails the whole build.
var (
	allTimeSpendBeforeVariant func(i int, key string) error
	allTimeSpendAfterVariant  func(i int, key string) error
)

// BuildAllTimeSpendDocs builds every variant of the admin all-users all-time Spend response from
// one snapshot s at the instant now, in AllTimeSpendKeys order. Any failure returns an error and no
// documents, so a partial generation is never handed to a publisher. now is the build's single
// reference instant (UTC, microsecond precision).
func BuildAllTimeSpendDocs(ctx context.Context, s *store.AnalyticsSnapshot, now time.Time) ([]store.AnalyticsPrecomputedDoc, error) {
	now = now.UTC().Truncate(time.Microsecond)
	deps := &analyticsDeps{tzLoader: time.LoadLocation}
	r := analyticsRequest{allUsers: true}
	keys := AllTimeSpendKeys()
	docs := make([]store.AnalyticsPrecomputedDoc, 0, len(keys))
	i := 0
	for _, split := range r.splitChoices() {
		for _, breakdown := range r.breakdownChoices() {
			i++
			key := allTimeSpendKey(split, breakdown)
			if hook := allTimeSpendBeforeVariant; hook != nil {
				if err := hook(i, key); err != nil {
					return nil, fmt.Errorf("all-time spend %s: %w", key, err)
				}
			}
			req, msg := parseAnalyticsRequest(url.Values{"window": {"all"}, "split": {split}, "breakdown": {breakdown}}, true, analytics.SectionSpend, uuid.Nil, true)
			if msg != "" || !req.precomputed {
				return nil, fmt.Errorf("all-time spend %s: variant is not a precomputed request (%q)", key, msg)
			}
			dto, err := deps.spend(ctx, s, req, now)
			if err != nil {
				return nil, fmt.Errorf("all-time spend %s: %w", key, err)
			}
			doc, err := json.Marshal(dto)
			if err != nil {
				return nil, fmt.Errorf("all-time spend %s: encode: %w", key, err)
			}
			docs = append(docs, store.AnalyticsPrecomputedDoc{Key: key, Document: doc})
			if hook := allTimeSpendAfterVariant; hook != nil {
				if err := hook(i, key); err != nil {
					return nil, fmt.Errorf("all-time spend %s: %w", key, err)
				}
			}
		}
	}
	return docs, nil
}
