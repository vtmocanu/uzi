package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
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
