package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestValidateErrIsInput(t *testing.T) {
	for _, c := range []string{"22P02", "22P05", "22021", "22003", "54001"} {
		if !validateErrIsInput(&pgconn.PgError{Code: c}) {
			t.Errorf("code %s should be input error", c)
		}
		if !validateErrIsInput(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: c})) {
			t.Errorf("wrapped code %s should be input error", c)
		}
	}
	for _, c := range []string{"57014", "53200", "XX000", "08006", "40001"} {
		if validateErrIsInput(&pgconn.PgError{Code: c}) {
			t.Errorf("code %s must not be input error", c)
		}
	}
	if validateErrIsInput(errors.New("plain")) || validateErrIsInput(nil) {
		t.Error("non-pg errors must not be input errors")
	}
}
