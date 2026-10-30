package vault

import (
	"encoding/base64"
	"github.com/google/uuid"
	"strings"
)

// CookieValue encodes exactly v1.<UUID>.<32-byte raw-base64url key>. It is secret
// transport material, never a JSON response. HTTP attributes belong to M2.
func (k *BrowserKey) CookieValue() (string, error) {
	if k == nil || k.slot == uuid.Nil {
		return "", ErrInvalidState
	}
	return "v1." + k.slot.String() + "." + base64.RawURLEncoding.EncodeToString(k.key[:]), nil
}

// DecodeBrowserKey is strict and bounded before splitting/allocating. Browser
// identity travels separately; restore never trusts identity from this cookie.
func DecodeBrowserKey(value string) (*BrowserKey, error) {
	if len(value) != 83 {
		return nil, ErrInvalidState
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, ErrInvalidState
	}
	id, err := uuid.Parse(parts[1])
	if err != nil || id == uuid.Nil || id.String() != parts[1] {
		return nil, ErrInvalidState
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	defer wipe(raw)
	if err != nil || len(raw) != 32 {
		return nil, ErrInvalidState
	}
	k := &BrowserKey{slot: id}
	copy(k.key[:], raw)
	return k, nil
}
