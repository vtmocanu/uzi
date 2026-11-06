package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeUserForge is a forge.Forge whose only meaningful method is UserExists; the
// rest are unused by the save-path helper under test. It lets humanUsernameWarning
// be exercised without a live forge (the handler's DB-touching wrapper is covered
// by the e2e stack; a live-Postgres handler harness does exist — hostedLiveDB in
// hosted_provision_livedb_test.go, opt-in via UZI_TEST_DATABASE_URL / run-store-it.sh —
// but this save-path helper does not need one).
type fakeUserForge struct {
	forgetest.BaseFake
	exists bool
	err    error
}

func (f *fakeUserForge) UserExists(context.Context, string) (bool, error) {
	return f.exists, f.err
}

// The save path is verified-or-warned (PRD #19 Decision 3): an existing user is
// clean, a missing user warns, and a lookup failure warns rather than blocking.
func TestHumanUsernameWarning(t *testing.T) {
	if w := humanUsernameWarning(context.Background(), &fakeUserForge{exists: true}, "alice"); w != "" {
		t.Errorf("existing user should produce no warning, got %q", w)
	}
	if w := humanUsernameWarning(context.Background(), &fakeUserForge{exists: false}, "ghost"); w != usernameNotFoundWarning {
		t.Errorf("missing user warning = %q, want not-found warning", w)
	}
	if w := humanUsernameWarning(context.Background(), &fakeUserForge{err: errors.New("forge down")}, "alice"); w != usernameUnverifiedWarning {
		t.Errorf("lookup error warning = %q, want unverified warning", w)
	}
}

// isUniqueViolation drives the human_username collision → 409 mapping: only a
// SQLSTATE 23505 counts, so a different DB error surfaces as a 500 (not a
// misleading 409) and a nil error is never a violation.
// CreateConnection never guesses the forge (issue #3785): a missing or blank
// forge_type is a 400 that names the supported values, where it used to default
// to GitLab. A valid type passes the check and stops at the next guard (the
// empty token), which proves the type itself was accepted without a forge call.
func TestCreateConnectionRequiresForgeType(t *testing.T) {
	const required = "forge_type is required; supported values: gitlab, forgejo, github"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing", `{"base_url":"https://gitlab.example.com","token":"pat"}`, required},
		{"empty", `{"forge_type":"","base_url":"https://gitlab.example.com","token":"pat"}`, required},
		{"blank", `{"forge_type":"   ","base_url":"https://gitlab.example.com","token":"pat"}`, required},
		{"unknown", `{"forge_type":"bitbucket","base_url":"https://gitlab.example.com","token":"pat"}`, "unsupported forge type; supported values: gitlab, forgejo, github"},
		{"gitlab", `{"forge_type":"gitlab","base_url":"https://gitlab.example.com"}`, "a bot token is required"},
		{"forgejo", `{"forge_type":"forgejo","base_url":"https://gitlab.example.com"}`, "a bot token is required"},
		{"github", `{"forge_type":"github","base_url":"https://gitlab.example.com"}`, "a bot token is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/forge/connections", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
			rec := httptest.NewRecorder()
			(&Handler{}).CreateConnection(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if body.Error != tc.want {
				t.Fatalf("error = %q, want %q", body.Error, tc.want)
			}
		})
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Error("23505 must be a unique violation")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23503"}) {
		t.Error("a foreign-key violation (23503) is not a unique violation")
	}
	if isUniqueViolation(errors.New("some other error")) {
		t.Error("a plain error is not a unique violation")
	}
	if isUniqueViolation(nil) {
		t.Error("nil is not a unique violation")
	}
}
