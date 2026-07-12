package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeNotifDB is a store.DBTX that answers the notifications inbox queries from
// fixed in-memory results, keyed off SQL substrings, so the handlers run end to
// end (scope check → store → DTO → respond) without a database. The list Query
// captures the LIMIT/OFFSET it was handed so the page clamp is observable, and
// mark-read returns pgx.ErrNoRows when markRow is nil to model a non-owned id.
type fakeNotifDB struct {
	unread    int64
	userTotal int64
	allTotal  int64
	userRows  []store.Notification
	allRows   []store.ListAllNotificationsRow
	markRow   *store.Notification

	listLim int32
	listOff int32
}

func (f *fakeNotifDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeNotifDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "JOIN users") {
		scans := make([]func(...any) error, 0, len(f.allRows))
		for _, r := range f.allRows {
			scans = append(scans, scanAllNotification(r))
		}
		return &fakeNotifRows{scans: scans}, nil
	}
	// ListNotificationsForUser: Query(ctx, sql, UserID, Off, Lim).
	if len(args) == 3 {
		if v, ok := args[1].(int32); ok {
			f.listOff = v
		}
		if v, ok := args[2].(int32); ok {
			f.listLim = v
		}
	}
	scans := make([]func(...any) error, 0, len(f.userRows))
	for _, r := range f.userRows {
		scans = append(scans, scanNotification(r))
	}
	return &fakeNotifRows{scans: scans}, nil
}

func (f *fakeNotifDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "read_at IS NULL"):
		return fakeScanRow{scanInt64(f.unread)}
	case strings.Contains(sql, "UPDATE notifications"):
		if f.markRow == nil {
			return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
		}
		return fakeScanRow{scanNotificationRow(*f.markRow)}
	case strings.Contains(sql, "count(*) FROM notifications") && strings.Contains(sql, "WHERE user_id"):
		return fakeScanRow{scanInt64(f.userTotal)}
	case strings.Contains(sql, "count(*) FROM notifications"):
		return fakeScanRow{scanInt64(f.allTotal)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

type fakeScanRow struct{ scan func(dest ...any) error }

func (r fakeScanRow) Scan(dest ...any) error { return r.scan(dest...) }

func scanInt64(v int64) func(dest ...any) error {
	return func(dest ...any) error {
		if p, ok := dest[0].(*int64); ok {
			*p = v
		}
		return nil
	}
}

func scanNotification(n store.Notification) func(dest ...any) error { return scanNotificationRow(n) }

func scanNotificationRow(n store.Notification) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = n.ID
		*dest[1].(*uuid.UUID) = n.UserID
		*dest[2].(*string) = n.Kind
		*dest[3].(*[]byte) = n.Payload
		*dest[4].(*pgtype.UUID) = n.RunID
		*dest[5].(*pgtype.UUID) = n.ReviewID
		*dest[6].(*pgtype.Timestamptz) = n.ReadAt
		*dest[7].(*pgtype.Timestamptz) = n.CreatedAt
		return nil
	}
}

func scanAllNotification(n store.ListAllNotificationsRow) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = n.ID
		*dest[1].(*uuid.UUID) = n.UserID
		*dest[2].(*string) = n.Kind
		*dest[3].(*[]byte) = n.Payload
		*dest[4].(*pgtype.UUID) = n.RunID
		*dest[5].(*pgtype.UUID) = n.ReviewID
		*dest[6].(*pgtype.Timestamptz) = n.ReadAt
		*dest[7].(*pgtype.Timestamptz) = n.CreatedAt
		*dest[8].(*string) = n.OwnerEmail
		*dest[9].(*pgtype.Text) = n.OwnerDisplayName
		return nil
	}
}

type fakeNotifRows struct {
	scans []func(dest ...any) error
	i     int
}

func (r *fakeNotifRows) Close()                                       {}
func (r *fakeNotifRows) Err() error                                   { return nil }
func (r *fakeNotifRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeNotifRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeNotifRows) Next() bool                                   { r.i++; return r.i <= len(r.scans) }
func (r *fakeNotifRows) Scan(dest ...any) error                       { return r.scans[r.i-1](dest...) }
func (r *fakeNotifRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeNotifRows) RawValues() [][]byte                          { return nil }
func (r *fakeNotifRows) Conn() *pgx.Conn                              { return nil }

// notifUser attaches a session user (own-user / admin scope) to a request.
func notifUser(req *http.Request, id uuid.UUID, admin bool) *http.Request {
	return req.WithContext(mw.ContextWithUser(req.Context(),
		store.User{ID: id, IsAdmin: admin, IsActive: true}))
}

type notifListResp struct {
	Notifications []struct {
		ID      string          `json:"id"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
		ReadAt  *string         `json:"read_at"`
		Owner   *struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"owner"`
	} `json:"notifications"`
	Unread int64 `json:"unread"`
	Total  int64 `json:"total"`
}

func notif(id, user uuid.UUID, kind string) store.Notification {
	return store.Notification{
		ID: id, UserID: user, Kind: kind,
		Payload:   []byte(`{"k":"v"}`),
		CreatedAt: pgtype.Timestamptz{Valid: true},
	}
}

func TestNotificationsRequireAuth(t *testing.T) {
	h := &Handler{} // no user in context ⇒ 401 before any store access
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"list", h.ListNotifications, httptest.NewRequest(http.MethodGet, "/api/notifications", nil)},
		{"unread", h.UnreadNotificationCount, httptest.NewRequest(http.MethodGet, "/api/notifications/unread_count", nil)},
		{"mark", h.MarkNotificationRead, httptest.NewRequest(http.MethodPost, "/api/notifications/x/read", nil)},
	} {
		rec := httptest.NewRecorder()
		tc.call(rec, tc.req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: status = %d, want 401", tc.name, rec.Code)
		}
	}
}

func TestListNotificationsOwnScope(t *testing.T) {
	user := uuid.New()
	db := &fakeNotifDB{
		unread:    2,
		userTotal: 3,
		userRows:  []store.Notification{notif(uuid.New(), user, "judge_review")},
	}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	h.ListNotifications(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/notifications", nil), user, false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out notifListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Unread != 2 || out.Total != 3 {
		t.Fatalf("envelope unread=%d total=%d, want 2/3", out.Unread, out.Total)
	}
	if len(out.Notifications) != 1 || out.Notifications[0].Kind != "judge_review" {
		t.Fatalf("notifications = %+v, want one judge_review row", out.Notifications)
	}
	if out.Notifications[0].Owner != nil {
		t.Fatalf("own-scope rows must not carry an owner block")
	}
	if string(out.Notifications[0].Payload) != `{"k":"v"}` {
		t.Fatalf("payload forwarded as %s, want raw jsonb", out.Notifications[0].Payload)
	}
}

func TestListNotificationsAllRequiresAdmin(t *testing.T) {
	user := uuid.New()
	db := &fakeNotifDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	h.ListNotifications(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/notifications?all=1", nil), user, false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin ?all=1: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListNotificationsAllViewCarriesOwner(t *testing.T) {
	admin := uuid.New()
	owner := uuid.New()
	db := &fakeNotifDB{
		unread:   0,
		allTotal: 1,
		allRows: []store.ListAllNotificationsRow{{
			ID: uuid.New(), UserID: owner, Kind: "judge_review",
			Payload: []byte(`{}`), CreatedAt: pgtype.Timestamptz{Valid: true},
			OwnerEmail: "owner@example.com",
		}},
	}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	h.ListNotifications(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/notifications?all=1", nil), admin, true))

	if rec.Code != http.StatusOK {
		t.Fatalf("admin ?all=1: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out notifListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Notifications) != 1 || out.Notifications[0].Owner == nil {
		t.Fatalf("admin all-view must include an owner block, got %+v", out.Notifications)
	}
	if out.Notifications[0].Owner.Email != "owner@example.com" || out.Notifications[0].Owner.ID != owner.String() {
		t.Fatalf("owner = %+v, want the row's owner", out.Notifications[0].Owner)
	}
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1 (all-view count)", out.Total)
	}
}

func TestListNotificationsPageClampAndValidation(t *testing.T) {
	user := uuid.New()

	// A request over the max page size is clamped, not rejected.
	db := &fakeNotifDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	h.ListNotifications(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/notifications?limit=500&offset=7", nil), user, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.listLim != maxNotifLimit {
		t.Fatalf("limit clamped to %d, want %d", db.listLim, maxNotifLimit)
	}
	if db.listOff != 7 {
		t.Fatalf("offset = %d, want 7", db.listOff)
	}

	for _, bad := range []string{"limit=0", "limit=abc", "offset=-1", "offset=x"} {
		rec := httptest.NewRecorder()
		h.ListNotifications(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/notifications?"+bad, nil), user, false))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestUnreadNotificationCount(t *testing.T) {
	user := uuid.New()
	h := &Handler{q: store.New(&fakeNotifDB{unread: 5})}
	rec := httptest.NewRecorder()
	h.UnreadNotificationCount(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/notifications/unread_count", nil), user, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Unread int64 `json:"unread"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Unread != 5 {
		t.Fatalf("unread = %d, want 5", out.Unread)
	}
}

// markReq builds a POST /notifications/{id}/read carrying the chi route param, so
// chi.URLParam(r,"id") resolves without mounting the full router.
func markReq(id string, user uuid.UUID, admin bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/"+id+"/read", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return notifUser(req, user, admin)
}

func TestMarkNotificationReadOwn(t *testing.T) {
	user := uuid.New()
	id := uuid.New()
	row := notif(id, user, "judge_review")
	row.ReadAt = pgtype.Timestamptz{Valid: true}
	h := &Handler{q: store.New(&fakeNotifDB{markRow: &row})}

	rec := httptest.NewRecorder()
	h.MarkNotificationRead(rec, markReq(id.String(), user, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Notification struct {
			ReadAt *string `json:"read_at"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Notification.ReadAt == nil {
		t.Fatalf("marked row should carry a read_at timestamp")
	}
}

func TestMarkNotificationReadCrossUserDenied(t *testing.T) {
	// markRow nil ⇒ the (id, user_id) UPDATE matched no row (not owned / unknown)
	// ⇒ the query returns ErrNoRows ⇒ 404, indistinguishable from an unknown id.
	h := &Handler{q: store.New(&fakeNotifDB{markRow: nil})}
	rec := httptest.NewRecorder()
	h.MarkNotificationRead(rec, markReq(uuid.New().String(), uuid.New(), false))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user mark-read: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarkNotificationReadBadID(t *testing.T) {
	h := &Handler{q: store.New(&fakeNotifDB{})}
	rec := httptest.NewRecorder()
	h.MarkNotificationRead(rec, markReq("not-a-uuid", uuid.New(), false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status = %d, want 400", rec.Code)
	}
}
