package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeMarginDB answers SetUserWeeklyWarningMargin from memory, recording every statement it saw so a
// test can prove what was (and was not) written.
type fakeMarginDB struct {
	writes []marginWrite
}

type marginWrite struct {
	sql string
	id  uuid.UUID
	pts int16
}

func (f *fakeMarginDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeMarginDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeMarginDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	w := marginWrite{sql: sql}
	// sqlc orders the parameters by first appearance: margin_pts, then id.
	w.pts, _ = args[0].(int16)
	w.id, _ = args[1].(uuid.UUID)
	f.writes = append(f.writes, w)
	return fakeScanRow{func(dest ...any) error {
		*dest[0].(*int16) = w.pts
		return nil
	}}
}

func putMargin(h *Handler, sessionID uuid.UUID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/me/weekly-warning-margin", strings.NewReader(body))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()
	h.SetWeeklyWarningMargin(rec, req)
	return rec
}

func TestSetWeeklyWarningMarginValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		pts  int16
	}{
		{"lower bound 0 turns the early warnings off", `{"margin_pts":0}`, 200, 0},
		{"typical", `{"margin_pts":15}`, 200, 15},
		{"upper bound 50", `{"margin_pts":50}`, 200, 50},
		{"51", `{"margin_pts":51}`, 400, 0},
		{"negative", `{"margin_pts":-1}`, 400, 0},
		{"fraction", `{"margin_pts":1.5}`, 400, 0},
		{"whole number written as a fraction still rejected", `{"margin_pts":15.0}`, 400, 0},
		{"exponent", `{"margin_pts":1e1}`, 400, 0},
		{"string", `{"margin_pts":"15"}`, 400, 0},
		{"null", `{"margin_pts":null}`, 400, 0},
		{"bool", `{"margin_pts":true}`, 400, 0},
		{"huge", `{"margin_pts":99999999999999999999}`, 400, 0},
		{"missing field", `{}`, 400, 0},
		{"unknown field", `{"margin_pts":15,"extra":1}`, 400, 0},
		{"a body user id is not accepted", `{"margin_pts":15,"user_id":"` + uuid.NewString() + `"}`, 400, 0},
		{"wrong field name", `{"margin":15}`, 400, 0},
		{"trailing value", `{"margin_pts":15} {"margin_pts":20}`, 400, 0},
		{"array", `[15]`, 400, 0},
		{"bare number", `15`, 400, 0},
		{"empty body", ``, 400, 0},
		{"not json", `nope`, 400, 0},
	}
	for _, c := range cases {
		sessionID := uuid.New()
		db := &fakeMarginDB{}
		h := &Handler{q: store.New(db)}
		rec := putMargin(h, sessionID, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			continue
		}
		if c.want != http.StatusOK {
			if len(db.writes) != 0 {
				t.Errorf("%s: store was written on a rejected request: %+v", c.name, db.writes)
			}
			continue
		}
		if len(db.writes) != 1 || db.writes[0].id != sessionID || db.writes[0].pts != c.pts {
			t.Errorf("%s: writes = %+v, want one write of %d for the session user %v", c.name, db.writes, c.pts, sessionID)
		}
		var out struct {
			Pts *int `json:"weekly_warning_margin_pts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Pts == nil || *out.Pts != int(c.pts) {
			t.Errorf("%s: response = %s, want weekly_warning_margin_pts %d", c.name, rec.Body.String(), c.pts)
		}
	}
}

// TestSetWeeklyWarningMarginTouchesOnlyTheMargin pins that the write is one UPDATE of the one column,
// scoped by the session user's id, so no other setting can be changed or cleared by it.
func TestSetWeeklyWarningMarginTouchesOnlyTheMargin(t *testing.T) {
	db := &fakeMarginDB{}
	h := &Handler{q: store.New(db)}
	if rec := putMargin(h, uuid.New(), `{"margin_pts":20}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(db.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(db.writes))
	}
	sql := strings.Join(strings.Fields(db.writes[0].sql), " ")
	if !strings.Contains(sql, "UPDATE users SET weekly_warning_margin_pts = $1 WHERE id = $2") || strings.Count(sql, "=") != 2 {
		t.Fatalf("statement = %q; want a single-column UPDATE keyed on the user id", sql)
	}
}

func TestSetWeeklyWarningMarginRequiresUser(t *testing.T) {
	db := &fakeMarginDB{}
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodPut, "/api/me/weekly-warning-margin", strings.NewReader(`{"margin_pts":15}`))
	rec := httptest.NewRecorder()
	h.SetWeeklyWarningMargin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session user: status = %d, want 401", rec.Code)
	}
	if len(db.writes) != 0 {
		t.Fatalf("an unauthenticated request wrote: %+v", db.writes)
	}
}

type noUserMarginDB struct{ fakeMarginDB }

func (noUserMarginDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

func TestSetWeeklyWarningMarginMissingUserRowIs401(t *testing.T) {
	h := &Handler{q: store.New(&noUserMarginDB{})}
	rec := putMargin(h, uuid.New(), `{"margin_pts":15}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing user row: status = %d, want 401", rec.Code)
	}
}

// TestGetMySettingsCarriesTheWarningMargin: the margin round-trips through GET /api/me/settings,
// including 0 (off), which must not be dropped as an empty value.
func TestGetMySettingsCarriesTheWarningMargin(t *testing.T) {
	for _, want := range []int16{0, 15, 50} {
		h := &Handler{q: store.New(&fakeSettingsDB{weeklyMargin: want})}
		rec := httptest.NewRecorder()
		h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var raw struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		got, ok := raw.Settings["weekly_warning_margin_pts"]
		if !ok || string(got) != strings.TrimSpace(string(mustJSON(t, int(want)))) {
			t.Errorf("margin %d: weekly_warning_margin_pts = %s (present %v)", want, got, ok)
		}
	}
}
