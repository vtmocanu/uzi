package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Codex catalog default on the settings codex cell (issue #3251): the cell always carries
// it, Claude never does, GET and PUT share the writer, and a read failure degrades to unknown.

type fakeCatalogReader struct {
	got workersvc.CatalogDefault
	err error
	// models records the model argument of every read.
	models []string
}

func (f *fakeCatalogReader) CodexCatalogDefault(_ context.Context, _ uuid.UUID, model string) (workersvc.CatalogDefault, error) {
	f.models = append(f.models, model)
	return f.got, f.err
}

// leadPinSettingsDB is the settings fake with the lead template's Codex pin on the
// ListUserCrossCheckPins rows (the shared fake leaves the template columns NULL).
type leadPinSettingsDB struct {
	*fakeSettingsDB
	pin pgtype.Text
}

func (f *leadPinSettingsDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := f.fakeSettingsDB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if nr, ok := rows.(*fakeNotifRows); ok {
		for i, scan := range nr.scans {
			nr.scans[i] = func(dest ...any) error {
				if err := scan(dest...); err != nil {
					return err
				}
				*dest[9].(*pgtype.Text) = f.pin
				return nil
			}
		}
	}
	return rows, nil
}

type catalogSettingsBody struct {
	Settings struct {
		EffortSettings []struct {
			Harness        string `json:"harness"`
			CatalogDefault *struct {
				State          string     `json:"state"`
				Effort         *string    `json:"effort"`
				Model          *string    `json:"model"`
				RuntimeVersion *string    `json:"runtime_version"`
				ObservedAt     *time.Time `json:"observed_at"`
			} `json:"catalog_default"`
		} `json:"effort_settings"`
	} `json:"settings"`
}

func decodeCatalogSettings(t *testing.T, rec *httptest.ResponseRecorder) catalogSettingsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var b catalogSettingsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Settings.EffortSettings) != 2 || b.Settings.EffortSettings[0].Harness != "claude" || b.Settings.EffortSettings[1].Harness != "codex" {
		t.Fatalf("effort_settings = %+v, want claude then codex", b.Settings.EffortSettings)
	}
	return b
}

func TestEffortSettingCellsCatalogDefaultCodexOnly(t *testing.T) {
	cd := &apitypes.CatalogDefaultDTO{State: "unknown"}
	cells := effortSettingCells(nil, nil, cd)
	if cells[0].CatalogDefault != nil {
		t.Errorf("claude cell carries catalog_default: %+v", cells[0].CatalogDefault)
	}
	if cells[1].CatalogDefault != cd {
		t.Errorf("codex cell catalog_default = %+v, want the passed value", cells[1].CatalogDefault)
	}
}

func TestSettingsGetAndPutCarryCatalogDefaultOnCodexCellOnly(t *testing.T) {
	observed := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reader := &fakeCatalogReader{got: workersvc.CatalogDefault{
		State: workersvc.CatalogStateKnown, Effort: "low", Model: "gpt-6.1-sol", RuntimeVersion: "0.164.0", ObservedAt: observed}}
	h := &Handler{q: store.New(&fakeSettingsDB{}), catalogDefaults: reader}

	getRec := httptest.NewRecorder()
	h.GetMySettings(getRec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	putRec := httptest.NewRecorder()
	h.PutMySettings(putRec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewBufferString(`{"default_codex_effort":"runtime"}`))))

	for name, rec := range map[string]*httptest.ResponseRecorder{"GET": getRec, "PUT": putRec} {
		b := decodeCatalogSettings(t, rec)
		if b.Settings.EffortSettings[0].CatalogDefault != nil {
			t.Errorf("%s: claude cell carries catalog_default", name)
		}
		cd := b.Settings.EffortSettings[1].CatalogDefault
		if cd == nil || cd.State != "known" || cd.Effort == nil || *cd.Effort != "low" || cd.Model == nil || *cd.Model != "gpt-6.1-sol" ||
			cd.RuntimeVersion == nil || *cd.RuntimeVersion != "0.164.0" || cd.ObservedAt == nil || !cd.ObservedAt.Equal(observed) {
			t.Errorf("%s: codex catalog_default = %+v", name, cd)
		}
	}
	// No lane and no lead pin: the provider default is the asked model, on both paths.
	if len(reader.models) != 2 || reader.models[0] != agenttmpl.DefaultCodexModel || reader.models[1] != agenttmpl.DefaultCodexModel {
		t.Errorf("models asked = %v, want the provider default twice", reader.models)
	}
}

func TestSettingsCatalogDefaultVariesAndNone(t *testing.T) {
	reader := &fakeCatalogReader{got: workersvc.CatalogDefault{
		State: workersvc.CatalogStateVaries, Effort: "none", Model: "gpt-6.1-sol", RuntimeVersion: "0.163.0", ObservedAt: time.Now()}}
	h := &Handler{q: store.New(&fakeSettingsDB{}), catalogDefaults: reader}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	cd := decodeCatalogSettings(t, rec).Settings.EffortSettings[1].CatalogDefault
	if cd == nil || cd.State != "varies" || cd.Effort == nil || *cd.Effort != "none" {
		t.Fatalf("catalog_default = %+v, want varies with effort none", cd)
	}
}

func TestSettingsCatalogDefaultReadErrorDegradesToUnknown(t *testing.T) {
	reader := &fakeCatalogReader{err: errors.New("catalog store down")}
	h := &Handler{q: store.New(&fakeSettingsDB{}), catalogDefaults: reader}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := httptest.NewRecorder()
		var body *bytes.Buffer
		if method == http.MethodPut {
			body = bytes.NewBufferString(`{"default_codex_effort":"runtime"}`)
		} else {
			body = &bytes.Buffer{}
		}
		req := authed(httptest.NewRequest(method, "/api/me/settings", body))
		if method == http.MethodPut {
			h.PutMySettings(rec, req)
		} else {
			h.GetMySettings(rec, req)
		}
		cd := decodeCatalogSettings(t, rec).Settings.EffortSettings[1].CatalogDefault
		if cd == nil || cd.State != "unknown" || cd.Effort != nil || cd.Model != nil || cd.RuntimeVersion != nil || cd.ObservedAt != nil {
			t.Errorf("%s: catalog_default = %+v, want a bare unknown", method, cd)
		}
	}
}

func TestSettingsCatalogDefaultNilServiceIsUnknown(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	b := decodeCatalogSettings(t, rec)
	if b.Settings.EffortSettings[0].CatalogDefault != nil {
		t.Errorf("claude cell carries catalog_default")
	}
	if cd := b.Settings.EffortSettings[1].CatalogDefault; cd == nil || cd.State != "unknown" {
		t.Fatalf("codex catalog_default = %+v, want unknown (always present)", cd)
	}
}

func TestSettingsCatalogDefaultAsksTheWorkersCodexModel(t *testing.T) {
	cases := []struct {
		name string
		lane pgtype.Text
		pin  pgtype.Text
		want string
	}{
		{"lane wins over the lead pin", pgtype.Text{String: "gpt-6-sol", Valid: true}, pgtype.Text{String: "gpt-6-astra", Valid: true}, "gpt-6-sol"},
		{"lead role-model pin used with no lane", pgtype.Text{}, pgtype.Text{String: "gpt-6-astra", Valid: true}, "gpt-6-astra"},
		{"non-role pin ignored", pgtype.Text{}, pgtype.Text{String: "not-a-codex-role-model", Valid: true}, agenttmpl.DefaultCodexModel},
		{"nothing set is the provider default", pgtype.Text{}, pgtype.Text{}, agenttmpl.DefaultCodexModel},
	}
	if !agenttmpl.CodexRoleModels["gpt-6-astra"] || !agenttmpl.CodexRoleModels["gpt-6-sol"] {
		t.Fatal("fixture models are not Codex role models any more; pick current ones")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := &fakeCatalogReader{}
			db := &leadPinSettingsDB{fakeSettingsDB: &fakeSettingsDB{codexModel: c.lane}, pin: c.pin}
			h := &Handler{q: store.New(db), catalogDefaults: reader}
			rec := httptest.NewRecorder()
			h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
			decodeCatalogSettings(t, rec)
			if len(reader.models) != 1 || reader.models[0] != c.want {
				t.Fatalf("models asked = %v, want [%s]", reader.models, c.want)
			}
		})
	}
}

func TestCrossCheckPinSettingsReturnsLeadCodexPin(t *testing.T) {
	db := &leadPinSettingsDB{fakeSettingsDB: &fakeSettingsDB{}, pin: pgtype.Text{String: "gpt-6-astra", Valid: true}}
	h := &Handler{q: store.New(db)}
	cells, pin, err := h.crossCheckPinSettings(context.Background(), uuid.New())
	if err != nil || len(cells) != 4 {
		t.Fatalf("cells=%d err=%v", len(cells), err)
	}
	if !pin.Valid || pin.String != "gpt-6-astra" {
		t.Fatalf("lead pin = %+v, want gpt-6-astra", pin)
	}
}
