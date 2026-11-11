// Package forgecred separates GitHub App key envelopes from legacy PAT storage.
// An older API must fail to decrypt an App envelope, rather than send its PEM
// as a bearer token after a rollback.
package forgecred

import (
	"bytes"
	"errors"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

const appPrefix = "uzi-ghapp-v1:"
const appAAD = "uzi/forge/github-app/v1"

// Seal preserves the legacy PAT format and domain-separates App key documents.
func Seal(box *secretbox.Box, value string) ([]byte, error) {
	if box == nil {
		return nil, errors.New("forge credential encryption unavailable")
	}
	if !forge.IsGitHubAppCredential(value) {
		return box.Seal([]byte(value))
	}
	sealed, err := box.SealWithAAD([]byte(value), []byte(appAAD))
	if err != nil {
		return nil, err
	}
	return append([]byte(appPrefix), sealed...), nil
}

// Open recognizes only the versioned App envelope or ordinary PAT ciphertext.
// The fixed AAD also prevents removing the clear prefix to make an App key
// decryptable by a legacy API.
func Open(box *secretbox.Box, ciphertext []byte) ([]byte, error) {
	if box == nil {
		return nil, errors.New("forge credential decryption unavailable")
	}
	if bytes.HasPrefix(ciphertext, []byte(appPrefix)) {
		return box.OpenWithAAD(ciphertext[len(appPrefix):], []byte(appAAD))
	}
	plain, err := box.Open(ciphertext)
	if err != nil {
		return nil, err
	}
	if forge.IsGitHubAppCredential(string(plain)) {
		return nil, errors.New("unversioned GitHub App credential; reconnect the App")
	}
	return plain, nil
}
