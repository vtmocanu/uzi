package issuetrackersvc

import (
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// tokenAAD is the additional authenticated data bound into a sealed token:
//
//	"uzi/issue-tracker-token/v1|" + userID + "|" + kind + "|" + baseURL
//
// with userID in canonical lowercase hyphenated form, kind the stored kind
// string, and baseURL the value strictBaseURL returns. The format is frozen:
// changing any part makes every stored ciphertext unreadable, so a new format
// takes a new version prefix and a re-seal migration. Binding the identity in
// means a ciphertext copied to another user's row, or another base URL, fails
// to open instead of sending one user's token to another host.
func tokenAAD(userID uuid.UUID, kind issuetracker.Kind, baseURL string) []byte {
	return []byte("uzi/issue-tracker-token/v1|" + userID.String() + "|" + string(kind) + "|" + baseURL)
}

func sealToken(box *secretbox.Box, userID uuid.UUID, kind issuetracker.Kind, baseURL, token string) ([]byte, error) {
	return box.SealWithAAD([]byte(token), tokenAAD(userID, kind, baseURL))
}

func openToken(box *secretbox.Box, userID uuid.UUID, kind issuetracker.Kind, baseURL string, sealed []byte) (string, error) {
	plain, err := box.OpenWithAAD(sealed, tokenAAD(userID, kind, baseURL))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
