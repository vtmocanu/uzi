package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type fakeModuleData struct {
	err  error
	data []byte
	rev  *int64
	got  []byte
	exp  store.ExpectedRevision
}

func (f *fakeModuleData) Get(context.Context, uuid.UUID, string) ([]byte, *int64, error) {
	return f.data, f.rev, f.err
}

func (f *fakeModuleData) Put(_ context.Context, _ uuid.UUID, _ string, d []byte, e store.ExpectedRevision) (int64, error) {
	f.got, f.exp = d, e
	return 9, f.err
}

func (f *fakeModuleData) Delete(_ context.Context, _ uuid.UUID, _ string, e store.ExpectedRevision) error {
	f.exp = e
	return f.err
}

// moduleSeamRouter mounts the real handlers behind the real root middleware, with an
// authenticated user injected (auth itself is covered by the LiveDB tests).
func moduleSeamRouter(f *fakeModuleData, authed bool) http.Handler {
	h := &Handler{moduleData: f}
	r := chi.NewRouter()
	r.Use(moduleUserDataNoStore)
	r.Route("/api/modules/{module_id}/user-data", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if authed {
					req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
				}
				next.ServeHTTP(w, req)
			})
		})
		r.Get("/", h.GetModuleUserData)
		r.Put("/", h.PutModuleUserData)
		r.Delete("/", h.DeleteModuleUserData)
	})
	return r
}

func TestModuleUserDataSeamMapping(t *testing.T) {
	one := int64(1)
	put := `{"data":1,"expected_revision":null}`
	cases := []struct {
		name, method, path, body string
		err                      error
		want                     int
		wantBody                 string
	}{
		{"get generic", "GET", "/api/modules/m/user-data/", "", errors.New("boom secret"), 500, "internal error"},
		{"put generic", "PUT", "/api/modules/m/user-data/", put, errors.New("boom"), 500, "internal error"},
		{"delete generic", "DELETE", "/api/modules/m/user-data/?expected_revision=1", "", errors.New("boom"), 500, "internal error"},
		{"put uncertain", "PUT", "/api/modules/m/user-data/", put, &store.ModuleDataCommitError{Err: errors.New("x")}, 503, `"outcome unknown"`},
		{"delete uncertain", "DELETE", "/api/modules/m/user-data/?expected_revision=1", "", &store.ModuleDataCommitError{Err: errors.New("x")}, 503, `"outcome unknown"`},
		{"put conflict", "PUT", "/api/modules/m/user-data/", put, &store.ModuleDataConflict{Current: &one}, 409, `{"revision":"1"}`},
		{"put conflict none", "PUT", "/api/modules/m/user-data/", put, &store.ModuleDataConflict{}, 409, `{"revision":null}`},
		{"put cap", "PUT", "/api/modules/m/user-data/", put, store.ErrModuleDataCap, 409, "too many module documents"},
		{"put invalid", "PUT", "/api/modules/m/user-data/", put, store.ErrModuleDataInvalid, 400, "invalid data"},
		{"put nouser", "PUT", "/api/modules/m/user-data/", put, store.ErrModuleDataNoUser, 401, ""},
		{"delete ok", "DELETE", "/api/modules/m/user-data/?expected_revision=1", "", nil, 204, ""},
		{"delete no param", "DELETE", "/api/modules/m/user-data/", "", nil, 400, ""},
		{"bad module", "GET", "/api/modules/Bad/user-data/", "", nil, 400, "invalid module"},
		{"encoded slash", "GET", "/api/modules/a%2Fb/user-data/", "", nil, 400, "invalid module"},
		{"put ok", "PUT", "/api/modules/m/user-data/", put, nil, 200, `{"revision":"9"}`},
		{"method", "PATCH", "/api/modules/m/user-data/", "", nil, 405, ""},
	}
	for _, tc := range cases {
		f := &fakeModuleData{err: tc.err}
		rec := httptest.NewRecorder()
		moduleSeamRouter(f, true).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", tc.name, rec.Header().Get("Cache-Control"))
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s: body %q lacks %q", tc.name, rec.Body.String(), tc.wantBody)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s: store error text leaked: %s", tc.name, rec.Body.String())
		}
	}
}

func TestModuleUserDataUnauthenticatedContext(t *testing.T) {
	rec := httptest.NewRecorder()
	moduleSeamRouter(&fakeModuleData{}, false).ServeHTTP(rec, httptest.NewRequest("GET", "/api/modules/m/user-data/", nil))
	if rec.Code != 401 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestModuleUserDataExpectedRevisionMapping(t *testing.T) {
	cases := []struct {
		query string
		want  store.ExpectedRevision
	}{
		{"12", store.ExpectedRevision{Kind: store.ExpectMatch, Value: 12}},
		{"", store.ExpectedRevision{Kind: store.ExpectNeverMatch}},
		{"007", store.ExpectedRevision{Kind: store.ExpectNeverMatch}},
		{"+7", store.ExpectedRevision{Kind: store.ExpectNeverMatch}},
		{"99999999999999999999", store.ExpectedRevision{Kind: store.ExpectNeverMatch}},
	}
	for _, tc := range cases {
		f := &fakeModuleData{}
		rec := httptest.NewRecorder()
		moduleSeamRouter(f, true).ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/modules/m/user-data/?expected_revision="+strings.ReplaceAll(tc.query, "+", "%2B"), nil))
		if f.exp != tc.want {
			t.Errorf("delete %q: exp %+v, want %+v", tc.query, f.exp, tc.want)
		}
	}
	f := &fakeModuleData{}
	body := `{"data":[0e-400],"expected_revision":"5"}`
	moduleSeamRouter(f, true).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", "/api/modules/m/user-data/", strings.NewReader(body)))
	if f.exp != (store.ExpectedRevision{Kind: store.ExpectMatch, Value: 5}) || string(f.got) != "[0]" {
		t.Errorf("put: exp %+v data %q", f.exp, f.got)
	}
	f = &fakeModuleData{}
	moduleSeamRouter(f, true).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", "/api/modules/m/user-data/", strings.NewReader(`{"data":1,"expected_revision":""}`)))
	if f.exp.Kind != store.ExpectNeverMatch {
		t.Errorf("empty string revision kind = %d", f.exp.Kind)
	}
}

func TestModuleUserDataGetShape(t *testing.T) {
	rev := int64(42)
	f := &fakeModuleData{data: []byte(`{"a":[1,2]}`), rev: &rev}
	rec := httptest.NewRecorder()
	moduleSeamRouter(f, true).ServeHTTP(rec, httptest.NewRequest("GET", "/api/modules/m/user-data/", nil))
	if got := rec.Body.String(); got != `{"data":{"a":[1,2]},"revision":"42"}`+"\n" {
		t.Errorf("body %q", got)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", rec.Header())
	}
	rec = httptest.NewRecorder()
	moduleSeamRouter(&fakeModuleData{}, true).ServeHTTP(rec, httptest.NewRequest("GET", "/api/modules/m/user-data/", nil))
	if rec.Body.String() != `{"data":null,"revision":null}`+"\n" {
		t.Errorf("empty body %q", rec.Body.String())
	}
}

type failingSessionStore struct{}

func (failingSessionStore) GetUserByID(context.Context, uuid.UUID) (store.User, error) {
	return store.User{}, errors.New("connection refused")
}

// TestModuleUserDataNoStoreOnAuthLayerFailure proves the root middleware covers a
// response the auth layer writes before any handler runs.
func TestModuleUserDataNoStoreOnAuthLayerFailure(t *testing.T) {
	cfg := config.Config{JWTSecret: []byte("0123456789abcdef0123456789abcdef"), AuthTokenTTL: time.Hour}
	tok, err := auth.IssueToken(cfg.JWTSecret, uuid.New().String(), 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := moduleUserDataNoStore(mw.RequireAuth(failingSessionStore{}, cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached")
	})))
	req := httptest.NewRequest("GET", "/api/modules/m/user-data/", nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: tok}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/other", nil))
	if rec.Header().Get("Cache-Control") != "" {
		t.Errorf("non-module path got Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
}
