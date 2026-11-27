// Package operatorkey seals and opens an organization-funded provider key (PRD #4540). The key is
// bound to its definition: the definition's id is authenticated additional data, so a ciphertext
// copied onto another definition's custody row (or opened for another definition) fails to open
// instead of silently serving the wrong credential.
package operatorkey

import (
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// aadPrefix versions the binding. A change to how the key is bound gets a new version here.
const aadPrefix = "uzi/provider-operator-key/v1|"

// AAD is the additional authenticated data for the key of one definition.
func AAD(providerID uuid.UUID) []byte {
	return []byte(aadPrefix + providerID.String())
}

// Seal encrypts plaintext for the definition providerID.
func Seal(box *secretbox.Box, providerID uuid.UUID, plaintext []byte) ([]byte, error) {
	return box.SealWithAAD(plaintext, AAD(providerID))
}

// Open decrypts a value sealed by Seal for the same providerID. A different definition, a truncated
// value or a tampered one is an error. The caller owns the returned plaintext and clears it after use.
func Open(box *secretbox.Box, providerID uuid.UUID, sealed []byte) ([]byte, error) {
	return box.OpenWithAAD(sealed, AAD(providerID))
}
