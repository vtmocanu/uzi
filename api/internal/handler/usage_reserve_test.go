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
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeUsageReserveDB answers SetUserUsageReserve from memory, capturing the arguments the
// handler passed and echoing them back as the RETURNING row (the last two users columns are
// weekly_reserve_pct and weekly_reserve_pause_running).
type fakeUsageReserveDB struct {
	called bool
	id     uuid.UUID
	pct    pgtype.Int2
	pause  bool
}

func (f *fakeUsageReserveDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeUsageReserveDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeUsageReserveDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "UPDATE users SET weekly_reserve_pct") {
		return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
	}
	f.called = true
	f.id, _ = args[0].(uuid.UUID)
	f.pct, _ = args[1].(pgtype.Int2)
	f.pause, _ = args[2].(bool)
	return fakeScanRow{func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = f.id
		}
		if p, ok := dest[len(dest)-2].(*pgtype.Int2); ok {
			*p = f.pct
		}
		if p, ok := dest[len(dest)-1].(*bool); ok {
			*p = f.pause
		}
		return nil
	}}
}

func putUsageReserve(h *Handler, sessionID uuid.UUID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/me/usage-reserve", strings.NewReader(body))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()
	h.SetUsageReserve(rec, req)
	return rec
}

func TestSetUsageReserveValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    int
		wantPct int // -1 = NULL
		pause   bool
	}{
		{"null threshold turns it off", `{"threshold_pct":null,"pause_running":true}`, 200, -1, true},
		{"lower bound 1", `{"threshold_pct":1,"pause_running":true}`, 200, 1, true},
		{"upper bound 100", `{"threshold_pct":100,"pause_running":false}`, 200, 100, false},
		{"typical", `{"threshold_pct":70,"pause_running":true}`, 200, 70, true},
		{"zero", `{"threshold_pct":0,"pause_running":true}`, 400, 0, false},
		{"101", `{"threshold_pct":101,"pause_running":true}`, 400, 0, false},
		{"negative", `{"threshold_pct":-5,"pause_running":true}`, 400, 0, false},
		{"fraction", `{"threshold_pct":1.5,"pause_running":true}`, 400, 0, false},
		{"string", `{"threshold_pct":"70","pause_running":true}`, 400, 0, false},
		{"huge", `{"threshold_pct":99999999999999999999,"pause_running":true}`, 400, 0, false},
		{"threshold absent", `{"pause_running":true}`, 400, 0, false},
		{"pause_running absent", `{"threshold_pct":70}`, 400, 0, false},
		{"pause_running null", `{"threshold_pct":70,"pause_running":null}`, 400, 0, false},
		{"pause_running string", `{"threshold_pct":70,"pause_running":"yes"}`, 400, 0, false},
		{"not json", `nope`, 400, 0, false},
	}
	for _, c := range cases {
		sessionID := uuid.New()
		db := &fakeUsageReserveDB{}
		h := &Handler{q: store.New(db)}
		rec := putUsageReserve(h, sessionID, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			continue
		}
		if c.want != 200 {
			if db.called {
				t.Errorf("%s: store was written on a rejected request", c.name)
			}
			continue
		}
		if db.id != sessionID {
			t.Errorf("%s: wrote id %v, want the session id %v", c.name, db.id, sessionID)
		}
		if c.wantPct < 0 && db.pct.Valid || c.wantPct >= 0 && (!db.pct.Valid || int(db.pct.Int16) != c.wantPct) {
			t.Errorf("%s: stored pct = %+v, want %d", c.name, db.pct, c.wantPct)
		}
		if db.pause != c.pause {
			t.Errorf("%s: stored pause_running = %v, want %v", c.name, db.pause, c.pause)
		}
		var out struct {
			User struct {
				ID           string `json:"id"`
				Pct          *int   `json:"weekly_reserve_pct"`
				PauseRunning bool   `json:"weekly_reserve_pause_running"`
			} `json:"user"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if out.User.ID != sessionID.String() || out.User.PauseRunning != c.pause ||
			(c.wantPct < 0) != (out.User.Pct == nil) || (out.User.Pct != nil && *out.User.Pct != c.wantPct) {
			t.Errorf("%s: response = %s", c.name, rec.Body.String())
		}
	}
}

func TestSetUsageReserveRequiresUser(t *testing.T) {
	h := &Handler{q: store.New(&fakeUsageReserveDB{})}
	req := httptest.NewRequest(http.MethodPut, "/api/me/usage-reserve", strings.NewReader(`{"threshold_pct":70,"pause_running":true}`))
	rec := httptest.NewRecorder()
	h.SetUsageReserve(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session user: status = %d, want 401", rec.Code)
	}
}
