package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fixtures/ sits above the api module, so a fixture-only edit is invisible to the test
// cache: run this package with -count=1 after editing cases.json.
type effortDefaultsFixture struct {
	Ordinary []struct {
		ID      string  `json:"id"`
		Harness string  `json:"harness"`
		Stored  *string `json:"stored"`
		Expect  struct {
			Mode           string  `json:"mode"`
			ResolvedEffort *string `json:"resolved_effort"`
			Source         string  `json:"source"`
		} `json:"expect"`
	} `json:"ordinary"`
	Validators []struct {
		ID       string  `json:"id"`
		Input    string  `json:"input"`
		Accepted bool    `json:"accepted"`
		Stored   *string `json:"stored"`
	} `json:"validators"`
	Checker []struct {
		ID             string  `json:"id"`
		Family         string  `json:"family"`
		Stage          string  `json:"stage"`
		OrdinaryStored *string `json:"ordinary_stored"`
		Pin            *string `json:"pin"`
		Expect         struct {
			Effort string `json:"effort"`
			Source string `json:"source"`
		} `json:"expect"`
	} `json:"checker"`
}

func readEffortDefaults(t *testing.T) effortDefaultsFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/effort-defaults/cases.json")
	if err != nil {
		t.Fatalf("fixture unreadable: %v", err)
	}
	var f effortDefaultsFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Ordinary) == 0 || len(f.Validators) == 0 || len(f.Checker) == 0 {
		t.Fatal("cases.json has an empty section")
	}
	return f
}

// markCase records a fixture case ID as exercised and fails on an empty or duplicate ID.
func markCase(t *testing.T, seen map[string]bool, id string) {
	t.Helper()
	if id == "" || seen[id] {
		t.Fatalf("fixture case ID %q is empty or duplicated", id)
	}
	seen[id] = true
}

// assertExercised fails unless the exercised IDs are exactly the fixture's IDs.
func assertExercised(t *testing.T, want []string, seen map[string]bool) {
	t.Helper()
	for _, id := range want {
		if !seen[id] {
			t.Errorf("fixture case %q was not exercised", id)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("exercised %d case IDs, fixture has %d", len(seen), len(want))
	}
}

func TestEffortDefaultsFixtureOrdinary(t *testing.T) {
	f := readEffortDefaults(t)
	seen := map[string]bool{}
	var ids []string
	for _, c := range f.Ordinary {
		markCase(t, seen, c.ID)
		ids = append(ids, c.ID)
		r := agenttmpl.ResolveHarnessEffort(c.Harness, c.Stored)
		if r.Mode != c.Expect.Mode || r.Source != c.Expect.Source {
			t.Errorf("%s: resolution = %+v, want mode %s source %s", c.ID, r, c.Expect.Mode, c.Expect.Source)
		}
		if (c.Expect.ResolvedEffort == nil) != (r.Level == "") || (c.Expect.ResolvedEffort != nil && *c.Expect.ResolvedEffort != r.Level) {
			t.Errorf("%s: level = %q, want %v", c.ID, r.Level, c.Expect.ResolvedEffort)
		}
		// The settings DTO projection: the cell for this harness carries the same outcome.
		var cells []apitypes.EffortSettingCellDTO
		if c.Harness == "codex" {
			cells = effortSettingCells(nil, c.Stored)
		} else {
			cells = effortSettingCells(c.Stored, nil)
		}
		if len(cells) != 2 || cells[0].Harness != "claude" || cells[1].Harness != "codex" {
			t.Fatalf("%s: cells = %+v, want claude then codex", c.ID, cells)
		}
		cell := cells[0]
		if c.Harness == "codex" {
			cell = cells[1]
		}
		if cell.Mode != c.Expect.Mode || cell.Source != c.Expect.Source ||
			cell.DefaultEffort != "medium" || cell.DefaultSource != "builtin" ||
			!reflect.DeepEqual(cell.ResolvedEffort, c.Expect.ResolvedEffort) || !reflect.DeepEqual(cell.Effort, c.Stored) {
			t.Errorf("%s: cell = %+v", c.ID, cell)
		}
	}
	assertExercised(t, ids, seen)
}

func TestEffortDefaultsFixtureValidators(t *testing.T) {
	f := readEffortDefaults(t)
	seen := map[string]bool{}
	var ids []string
	for _, c := range f.Validators {
		markCase(t, seen, c.ID)
		ids = append(ids, c.ID)
		input := c.Input
		got, err := validateDefaultEffort(&input)
		if c.Accepted != (err == nil) {
			t.Errorf("%s: accepted = %v (err %v), want %v", c.ID, err == nil, err, c.Accepted)
		}
		if c.Accepted {
			want := pgtype.Text{}
			if c.Stored != nil {
				want = pgtype.Text{String: *c.Stored, Valid: true}
			}
			if got != want {
				t.Errorf("%s: stored = %+v, want %+v", c.ID, got, want)
			}
		}
	}
	assertExercised(t, ids, seen)
}

// The checker cases document that ordinary effort is not an input of the checker
// resolver: ResolveCrossCheck takes only the pin.
func TestEffortDefaultsFixtureChecker(t *testing.T) {
	f := readEffortDefaults(t)
	seen := map[string]bool{}
	var ids []string
	for _, c := range f.Checker {
		markCase(t, seen, c.ID)
		ids = append(ids, c.ID)
		r := agenttmpl.ResolveCrossCheck(c.Family, nil, c.Pin, nil, nil)
		if r.Effort != c.Expect.Effort || r.EffortSource != c.Expect.Source {
			t.Errorf("%s: checker = %q/%q, want %q/%q", c.ID, r.Effort, r.EffortSource, c.Expect.Effort, c.Expect.Source)
		}
		if c.Pin == nil && r.EffortSource != agenttmpl.CrossCheckDefaultSource {
			t.Errorf("%s: unpinned source = %q", c.ID, r.EffortSource)
		}
	}
	assertExercised(t, ids, seen)
}

func TestSettingsRuntimeEffortRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	type resp struct {
		Settings struct {
			DefaultEffort      *string                         `json:"default_effort"`
			DefaultCodexEffort *string                         `json:"default_codex_effort"`
			EffortSettings     []apitypes.EffortSettingCellDTO `json:"effort_settings"`
		} `json:"settings"`
	}
	decode := func(rec *httptest.ResponseRecorder) resp {
		t.Helper()
		var r resp
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	rec := httptest.NewRecorder()
	h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings",
		bytes.NewBufferString(`{"default_effort":" runtime ","default_codex_effort":"high"}`))))
	put := decode(rec)
	if db.effort.String != "runtime" || !db.effort.Valid {
		t.Fatalf("stored = %+v, want runtime", db.effort)
	}
	rec = httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	get := decode(rec)
	for name, r := range map[string]resp{"PUT": put, "GET": get} {
		if r.Settings.DefaultEffort == nil || *r.Settings.DefaultEffort != "runtime" {
			t.Errorf("%s default_effort = %v, want runtime", name, r.Settings.DefaultEffort)
		}
		cells := r.Settings.EffortSettings
		if len(cells) != 2 || cells[0].Harness != "claude" || cells[1].Harness != "codex" {
			t.Fatalf("%s effort_settings = %+v", name, cells)
		}
		if cells[0].Mode != "runtime_default" || cells[0].ResolvedEffort != nil || cells[0].Source != "user_default" {
			t.Errorf("%s claude cell = %+v", name, cells[0])
		}
		if cells[1].Mode != "explicit" || cells[1].ResolvedEffort == nil || *cells[1].ResolvedEffort != "high" {
			t.Errorf("%s codex cell = %+v", name, cells[1])
		}
	}
}

func TestPutMySettingsRejectsUltraButAcceptsRuntimeCodex(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewBufferString(body))))
		return rec
	}
	if rec := put(`{"default_codex_effort":"runtime"}`); rec.Code != http.StatusOK || db.codexEffort.String != "runtime" {
		t.Fatalf("runtime: status=%d stored=%+v", rec.Code, db.codexEffort)
	}
	for _, body := range []string{`{"default_effort":"ultra"}`, `{"default_codex_effort":"ultra"}`} {
		if rec := put(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
	if db.effort.Valid || db.codexEffort.String != "runtime" {
		t.Fatalf("rejected PUT changed state: %+v", db)
	}
}
