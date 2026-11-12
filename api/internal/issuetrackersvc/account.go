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
//
// It also refuses an id or login that holds token, raw or in one of the escaped
// forms the driver's redactor knows (issuetracker.ContainsToken). Both values
// are stored unsealed and returned to the client, so an issue tracker that
// echoes the token in its account answer must not get it stored in plain text.
// No message carries the value.
func validateAccount(a issuetracker.Account, token string) error {
	if err := validateAccountField(a.ID); err != nil {
		return errors.New("issue tracker returned an invalid account id")
	}
	if err := validateAccountField(a.Login); err != nil {
		return errors.New("issue tracker returned an invalid account login")
	}
	if issuetracker.ContainsToken(a.ID, token) {
		return errors.New("issue tracker returned an account id that contains the token")
	}
	if issuetracker.ContainsToken(a.Login, token) {
		return errors.New("issue tracker returned an account login that contains the token")
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
