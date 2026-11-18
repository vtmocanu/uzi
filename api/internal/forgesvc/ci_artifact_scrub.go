package forgesvc

import (
	"errors"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgecred"
	"strings"
)

// CIArtifactScrubber returns an exact connection-secret scrubber without exposing
// the decrypted credential to the evidence collector. Callers also apply the
// known-token-shape scrub before persisting metadata or content.
func (s *Service) CIArtifactScrubber(ciphertext []byte) (func(string) string, error) {
	plain, err := forgecred.Open(s.box, ciphertext)
	if err != nil {
		return nil, errors.New("CI artifact credential unavailable")
	}
	secret := string(plain)
	if secret == "" {
		return nil, errors.New("CI artifact credential unavailable")
	}
	return func(text string) string {
		for _, value := range forge.ConnectionSecretValues(secret) {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
		return text
	}, nil
}
