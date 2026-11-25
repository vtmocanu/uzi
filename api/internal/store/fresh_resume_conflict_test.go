package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The 23505 mapping of a fresh resume insert is by constraint name: only the UNIQUE(run_id, request_id)
// key is a request conflict (request_completed); the per-run pending/claimed and seeded partial unique
// indexes never are.
func TestIsFreshResumeRequestConflictByConstraintName(t *testing.T) {
	unique := func(constraint string) error {
		return &pgconn.PgError{Code: "23505", ConstraintName: constraint}
	}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"request key", unique("run_fresh_resumes_run_request_key"), true},
		{"wrapped request key", fmt.Errorf("insert: %w", unique("run_fresh_resumes_run_request_key")), true},
		{"one active index", unique("run_fresh_resumes_one_active_idx"), false},
		{"one seeded index", unique("run_fresh_resumes_one_seeded_idx"), false},
		{"another unique violation", unique("some_other_key"), false},
		{"request key name, other code", &pgconn.PgError{Code: "23503", ConstraintName: "run_fresh_resumes_run_request_key"}, false},
		{"not a pg error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		if got := IsFreshResumeRequestConflict(c.err); got != c.want {
			t.Errorf("%s: IsFreshResumeRequestConflict = %v, want %v", c.name, got, c.want)
		}
	}
}
