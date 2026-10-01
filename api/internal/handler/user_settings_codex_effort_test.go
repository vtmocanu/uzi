package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPutMySettingsCodexEffortIsolation(t *testing.T) {
	db := &fakeSettingsDB{effort: pgtype.Text{String: "high", Valid: true}, codexEffort: pgtype.Text{String: "low", Valid: true}}
	h := &Handler{q: store.New(db)}
	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewBufferString(body))))
		return rec
	}
	for _, body := range []string{`{"default_codex_effort":" medium "}`, `{"default_effort":"max"}`} {
		if rec := put(body); rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
	}
	if db.effort.String != "max" || db.codexEffort.String != "medium" {
		t.Fatalf("separate writes clobbered lanes: %+v", db)
	}
	for _, body := range []string{`{"default_codex_effort":null}`, `{"default_codex_effort":"  "}`} {
		rec := put(body)
		if rec.Code != 200 || db.codexEffort.Valid || db.effort.String != "max" {
			t.Fatalf("clear failed: %d %+v", rec.Code, db)
		}
		var response struct {
			Settings map[string]any `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		value, present := response.Settings["default_codex_effort"]
		if !present || value != nil {
			t.Fatalf("missing nullable Codex field: %s", rec.Body)
		}
	}
}

func TestPutMySettingsRejectsCodexEffortBeforeWrites(t *testing.T) {
	for _, value := range []string{`"ultra"`, `"HIGH"`, `12`, `false`, `{"level":"high"}`} {
		db := &fakeSettingsDB{effort: pgtype.Text{String: "high", Valid: true}, codexEffort: pgtype.Text{String: "low", Valid: true}}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewBufferString(`{"default_effort":"max","default_codex_effort":`+value+`}`))))
		if rec.Code != 400 {
			t.Fatalf("invalid %s status=%d", value, rec.Code)
		}
		if db.effort.String != "high" || db.codexEffort.String != "low" {
			t.Fatalf("rejected patch changed preferences: %+v", db)
		}
	}
}
