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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSlackDB is a store.DBTX holding one user's Slack linking columns so the
// /me/slack handlers run end to end without a real database. The GET and every
// RETURNING share the same 4-column shape (member, notify, resolved, confirmed).
type fakeSlackDB struct {
	member      pgtype.Text
	notify      bool
	resolved    pgtype.Text
	confirmed   pgtype.Timestamptz
	overrideErr error // returned by the override RETURNING scan (collision case)
}

func (f *fakeSlackDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeSlackDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeSlackDB: Query not used")
}
func (f *fakeSlackDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SET slack_notify"):
		if b, ok := args[0].(bool); ok {
			f.notify = b // SetUserSlackNotify: $1 = notify
		}
	case strings.Contains(sql, "SET slack_member_id"):
		if f.overrideErr != nil {
			return fakeSlackRow{err: f.overrideErr}
		}
		if m, ok := args[0].(pgtype.Text); ok {
			f.member = m // SetUserSlackOverride: $1 = member
		}
		if rv, ok := args[1].(pgtype.Text); ok {
			f.resolved = rv // $2 = resolved
		}
	}
	return fakeSlackRow{member: f.member, notify: f.notify, resolved: f.resolved, confirmed: f.confirmed}
}

type fakeSlackRow struct {
	member    pgtype.Text
	notify    bool
	resolved  pgtype.Text
	confirmed pgtype.Timestamptz
	err       error
}

func (r fakeSlackRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 4 {
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.member
		}
		if p, ok := dest[1].(*bool); ok {
			*p = r.notify
		}
		if p, ok := dest[2].(*pgtype.Text); ok {
			*p = r.resolved
		}
		if p, ok := dest[3].(*pgtype.Timestamptz); ok {
			*p = r.confirmed
		}
	}
	return nil
}

// fakeHandlerLinker records the DM-sending calls the /me/slack endpoints make.
type fakeHandlerLinker struct {
	confirmCalls []string
	testDMCalls  []string
	testDMErr    error
}

func (f *fakeHandlerLinker) SendLinkConfirmation(_ context.Context, slackID, _ string) {
	f.confirmCalls = append(f.confirmCalls, slackID)
}
func (f *fakeHandlerLinker) SendTestDM(_ context.Context, slackID string) error {
	f.testDMCalls = append(f.testDMCalls, slackID)
	return f.testDMErr
}

func decodeSlack(t *testing.T, body []byte) slackLinkDTO {
	t.Helper()
	var resp struct {
		Slack slackLinkDTO `json:"slack"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode slack response %s: %v", body, err)
	}
	return resp.Slack
}

func TestSlackEndpointsRequireAuth(t *testing.T) {
	h := &Handler{} // no user in context ⇒ 401 before any store/linker access
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"GET", h.GetMySlack, httptest.NewRequest(http.MethodGet, "/api/me/slack", nil)},
		{"notify", h.PutMySlackNotify, httptest.NewRequest(http.MethodPut, "/api/me/slack/notify", strings.NewReader(`{"notify":false}`))},
		{"override", h.PutMySlackOverride, httptest.NewRequest(http.MethodPut, "/api/me/slack/override", strings.NewReader(`{"member_id":"U1"}`))},
		{"test-dm", h.PostMySlackTestDM, httptest.NewRequest(http.MethodPost, "/api/me/slack/test-dm", nil)},
	} {
		rec := httptest.NewRecorder()
		tc.call(rec, tc.req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: status = %d, want 401", tc.name, rec.Code)
		}
	}
}

func TestGetMySlackDerivesState(t *testing.T) {
	cases := []struct {
		name      string
		resolved  pgtype.Text
		confirmed pgtype.Timestamptz
		want      string
	}{
		{"unlinked", pgtype.Text{}, pgtype.Timestamptz{}, "unlinked"},
		{"pending", pgtype.Text{String: "U1", Valid: true}, pgtype.Timestamptz{}, "pending"},
		{"confirmed", pgtype.Text{String: "U1", Valid: true}, pgtype.Timestamptz{Valid: true}, "confirmed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{q: store.New(&fakeSlackDB{notify: true, resolved: tc.resolved, confirmed: tc.confirmed})}
			rec := httptest.NewRecorder()
			h.GetMySlack(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/slack", nil)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if got := decodeSlack(t, rec.Body.Bytes()); got.State != tc.want {
				t.Fatalf("state = %q, want %q", got.State, tc.want)
			}
		})
	}
}

func TestPutMySlackNotifyRoundTrip(t *testing.T) {
	db := &fakeSlackDB{notify: true}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/slack/notify", strings.NewReader(`{"notify":false}`)))
	h.PutMySlackNotify(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.notify {
		t.Errorf("notify not persisted as false: %+v", db.notify)
	}
	if got := decodeSlack(t, rec.Body.Bytes()); got.Notify {
		t.Errorf("response notify = true, want false")
	}
}

func TestPutMySlackNotifyRequiresField(t *testing.T) {
	h := &Handler{q: store.New(&fakeSlackDB{})}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/slack/notify", strings.NewReader(`{}`)))
	h.PutMySlackNotify(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a missing notify field", rec.Code)
	}
}

func TestPutMySlackOverrideSetSendsConfirmDM(t *testing.T) {
	db := &fakeSlackDB{notify: true}
	linker := &fakeHandlerLinker{}
	h := &Handler{q: store.New(db), slackLinker: linker}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/slack/override", strings.NewReader(`{"member_id":"U9"}`)))
	h.PutMySlackOverride(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.member.Valid || db.member.String != "U9" || !db.resolved.Valid || db.resolved.String != "U9" {
		t.Fatalf("override not stored to member+resolved U9: member=%+v resolved=%+v", db.member, db.resolved)
	}
	if len(linker.confirmCalls) != 1 || linker.confirmCalls[0] != "U9" {
		t.Fatalf("a set override must re-send the confirmation DM to the new target: %v", linker.confirmCalls)
	}
	if got := decodeSlack(t, rec.Body.Bytes()); got.State != "pending" {
		t.Errorf("state after override = %q, want pending (must confirm)", got.State)
	}
}

func TestPutMySlackOverrideCollisionIs409(t *testing.T) {
	db := &fakeSlackDB{overrideErr: &pgconn.PgError{Code: "23505"}}
	linker := &fakeHandlerLinker{}
	h := &Handler{q: store.New(db), slackLinker: linker}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/slack/override", strings.NewReader(`{"member_id":"U9"}`)))
	h.PutMySlackOverride(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 on a unique-index collision; body=%s", rec.Code, rec.Body.String())
	}
	if len(linker.confirmCalls) != 0 {
		t.Errorf("a rejected override must not send a confirmation DM: %v", linker.confirmCalls)
	}
}

func TestPutMySlackOverrideClearSkipsConfirmDM(t *testing.T) {
	db := &fakeSlackDB{member: pgtype.Text{String: "U9", Valid: true}, resolved: pgtype.Text{String: "U9", Valid: true}}
	linker := &fakeHandlerLinker{}
	h := &Handler{q: store.New(db), slackLinker: linker}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/slack/override", strings.NewReader(`{"member_id":null}`)))
	h.PutMySlackOverride(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.member.Valid || db.resolved.Valid {
		t.Fatalf("clear must null both member and resolved: member=%+v resolved=%+v", db.member, db.resolved)
	}
	if len(linker.confirmCalls) != 0 {
		t.Errorf("clearing an override must not DM anyone: %v", linker.confirmCalls)
	}
}

func TestPutMySlackOverrideRejectsGarbage(t *testing.T) {
	h := &Handler{q: store.New(&fakeSlackDB{}), slackLinker: &fakeHandlerLinker{}}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/slack/override", strings.NewReader(`{"member_id":"has space"}`)))
	h.PutMySlackOverride(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed member id", rec.Code)
	}
}

func TestPostTestDMWithoutResolvedIs400(t *testing.T) {
	h := &Handler{q: store.New(&fakeSlackDB{notify: true}), slackLinker: &fakeHandlerLinker{}}
	rec := httptest.NewRecorder()
	h.PostMySlackTestDM(rec, authed(httptest.NewRequest(http.MethodPost, "/api/me/slack/test-dm", nil)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when there is no resolved id", rec.Code)
	}
}

func TestPostTestDMSends(t *testing.T) {
	db := &fakeSlackDB{notify: true, resolved: pgtype.Text{String: "U9", Valid: true}}
	linker := &fakeHandlerLinker{}
	h := &Handler{q: store.New(db), slackLinker: linker}
	rec := httptest.NewRecorder()
	h.PostMySlackTestDM(rec, authed(httptest.NewRequest(http.MethodPost, "/api/me/slack/test-dm", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(linker.testDMCalls) != 1 || linker.testDMCalls[0] != "U9" {
		t.Fatalf("test DM must go to the resolved id: %v", linker.testDMCalls)
	}
}

func TestPostTestDMLinkerErrorIs502(t *testing.T) {
	db := &fakeSlackDB{notify: true, resolved: pgtype.Text{String: "U9", Valid: true}}
	h := &Handler{q: store.New(db), slackLinker: &fakeHandlerLinker{testDMErr: errors.New("slack down")}}
	rec := httptest.NewRecorder()
	h.PostMySlackTestDM(rec, authed(httptest.NewRequest(http.MethodPost, "/api/me/slack/test-dm", nil)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the Slack send fails", rec.Code)
	}
}

func TestGetAdminSlackStatus(t *testing.T) {
	h := &Handler{}
	h.SetSlackStatus(func() string { return "connected" })
	rec := httptest.NewRecorder()
	h.GetAdminSlackStatus(rec, httptest.NewRequest(http.MethodGet, "/api/admin/slack/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		SlackStatus string `json:"slack_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SlackStatus != "connected" {
		t.Fatalf("slack_status = %q, want connected", resp.SlackStatus)
	}
}
