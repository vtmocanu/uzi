package issuetrackersvc

import (
	"errors"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// maxAccountField bounds the id and login a driver may report, in bytes.
const maxAccountField = 255

// validateAccount refuses an account whose id or login is empty, too long, or
// holds a control, format (Cf) or line/paragraph separator character. The
// values come from a remote server and are stored and shown to the user, so a
// hostile one must not smuggle in invisible or bidirectional text.
func validateAccount(a issuetracker.Account) error {
	if err := validateAccountField(a.ID); err != nil {
		return errors.New("issue tracker returned an invalid account id")
	}
	if err := validateAccountField(a.Login); err != nil {
		return errors.New("issue tracker returned an invalid account login")
	}
	return nil
}

func validateAccountField(s string) error {
	if s == "" || len(s) > maxAccountField {
		return errors.New("empty or too long")
	}
	for _, r := range s {
		if issuetracker.InvisibleRune(r) {
			return errors.New("forbidden character")
		}
	}
	return nil
}
