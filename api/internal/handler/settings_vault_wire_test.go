package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Delegate unrelated reads to the real DBTX (including authentication in the
// LiveDB PUT test). Only the new count read fails.
type settingsCountFailureDB struct{ store.DBTX }

func (db settingsCountFailureDB) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "-- name: CountUnprotectedUsers :one") {
		return fakeScanRow{func(...any) error { return errors.New("count unavailable") }}
	}
	return db.DBTX.QueryRow(ctx, query, args...)
}

func TestGetSettingsUnprotectedCount(t *testing.T) {
	h := newSettingsHandler()
	h.q = store.New(fakeCountDB{count: 7})
	rec := httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got.UnprotectedUsers != 7 {
		t.Fatalf("settings=%d %s err=%v", rec.Code, rec.Body.String(), err)
	}
	if got.Settings[settings.KeyVaultPolicy] != "required" || got.Sources[settings.KeyVaultPolicy] != "default" ||
		got.Defaults[settings.KeyVaultPolicy] != "required" {
		t.Fatalf("default policy exposure=%+v", got)
	}
}

func TestGetSettingsCountFailure(t *testing.T) {
	h := newSettingsHandler()
	h.q = store.New(settingsCountFailureDB{})
	rec := httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "unprotected_users") {
		t.Fatalf("count error must not become a zero count: %d %s", rec.Code, rec.Body.String())
	}
}
