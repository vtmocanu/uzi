package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// serveCapturingCredential runs req through RequireUser and returns the credential
// the downstream handler saw.
func serveCapturingCredential(t *testing.T, st fakeAuthStore, req *http.Request) (CLICredential, bool, bool) {
	t.Helper()
	var (
		cred    CLICredential
		credOK  bool
		reached bool
	)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		cred, credOK = CLICredentialFromContext(r.Context())
	})
	rec := httptest.NewRecorder()
	RequireUser(st, authTestCfg())(next).ServeHTTP(rec, req)
	return cred, credOK, reached
}

func TestRequireUserStoresStoredBearerCredential(t *testing.T) {
	for _, scope := range []string{clitoken.ScopeAdminRO, clitoken.ScopeUser} {
		t.Run(scope, func(t *testing.T) {
			tok, hash, _, err := clitoken.Generate(scope)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			row := store.CliToken{ID: uuid.New(), UserID: uuid.New(), TokenHash: hash, Scope: scope}
			st := fakeAuthStore{row: row, user: store.User{ID: row.UserID, IsActive: true, IsAdmin: true}}
			cred, ok, reached := serveCapturingCredential(t, st, bearerRequest(context.Background(), tok))
			if !reached || !ok {
				t.Fatalf("reached=%v credential ok=%v, want both true", reached, ok)
			}
			if cred.TokenID != row.ID || cred.Scope != scope {
				t.Fatalf("credential = %+v, want id %s scope %s", cred, row.ID, scope)
			}
		})
	}
}

// The scope comes from the stored row, not the token prefix: a uza_-prefixed
// secret whose stored row says user must report scope user.
func TestRequireUserCredentialScopeIsStoredNotPrefix(t *testing.T) {
	tok, hash, _, err := clitoken.Generate(clitoken.ScopeAdminRO)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	row := store.CliToken{ID: uuid.New(), UserID: uuid.New(), TokenHash: hash, Scope: clitoken.ScopeUser}
	st := fakeAuthStore{row: row, user: store.User{ID: row.UserID, IsActive: true}}
	cred, ok, _ := serveCapturingCredential(t, st, bearerRequest(context.Background(), tok))
	if !ok || cred.Scope != clitoken.ScopeUser {
		t.Fatalf("credential = %+v ok=%v, want stored scope user", cred, ok)
	}
}

func TestRequireUserCookiePathSetsNoCredential(t *testing.T) {
	uid := uuid.New()
	req, _ := sessionRequest(t, context.Background(), uid)
	st := fakeAuthStore{user: store.User{ID: uid, IsActive: true, IsAdmin: true}}
	_, ok, reached := serveCapturingCredential(t, st, req)
	if !reached {
		t.Fatal("cookie request did not reach next handler")
	}
	if ok {
		t.Fatal("cookie path must not carry a CLI credential")
	}
}
