package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// brandingUploadReq builds a PUT /api/admin/branding/logo/{slot} request carrying an
// admin user, the slot path param, a Content-Type and a raw body — the shape
// PutBrandingLogo reads. No DB is touched by the rejection paths below (the handler
// returns before the Upsert), so these run without UZI_TEST_DATABASE_URL.
func brandingUploadReq(slot, contentType string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/admin/branding/logo/"+slot, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	ctx := mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsActive: true, IsAdmin: true})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slot", slot)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

// The upload handler's TYPE and SIZE gates fire before any DB touch, so they are
// exercised here without a live Postgres. The accept-at/under-cap path (which does
// upsert) lives in the live-DB suite.
func TestPutBrandingLogoRejectsBeforeDB(t *testing.T) {
	h := &Handler{}
	cases := []struct {
		name        string
		slot        string
		contentType string
		body        []byte
		wantStatus  int
	}{
		{"unknown slot", "sidebar", "image/png", []byte("x"), http.StatusNotFound},
		{"disallowed type gif", "app", "image/gif", []byte("x"), http.StatusUnsupportedMediaType},
		{"disallowed type json", "brand", "application/json", []byte("x"), http.StatusUnsupportedMediaType},
		{"empty body", "app", "image/png", []byte{}, http.StatusBadRequest},
		{"one byte over cap", "app", "image/png", bytes.Repeat([]byte("a"), maxBrandingLogoBytes+1), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.PutBrandingLogo(rec, brandingUploadReq(tc.slot, tc.contentType, tc.body))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// A Content-Type with a charset parameter still matches the allowlist (the media type
// is parsed out) — this must not 415 a legitimate SVG upload. It returns before the DB
// only when the SIZE gate also passes; here we send an over-cap body so the handler
// gets past the type check and rejects on size, proving the type was accepted.
func TestPutBrandingLogoParsesMediaTypeParams(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	body := bytes.Repeat([]byte("a"), maxBrandingLogoBytes+1)
	h.PutBrandingLogo(rec, brandingUploadReq("brand", "image/svg+xml; charset=utf-8", body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (media type with charset should pass the type gate; body=%s)",
			rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "image/png") {
		t.Errorf("error mentioned the type allowlist, want the size error: %s", rec.Body.String())
	}
}

func TestGetBrandingLogoRejectsUnknownSlot(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/branding/logo/sidebar", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slot", "sidebar")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.GetBrandingLogo(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown slot", rec.Code)
	}
}
