// Package runenv holds the rules and the sealing context for run environment
// variables (PRD #4416): who may name a variable, what a value may hold, how many
// a scope may carry, and which context binds a sealed value. It is a leaf: it
// imports neither the vault nor the store, so both can depend on it.
package runenv

import (
	"encoding/binary"

	"github.com/google/uuid"
)

// Scope tags used in the sealing context and in the rows' repo_id nullability.
const (
	ScopeRepo = "repo"
	ScopeUser = "user"
)

// aadDomain is the domain tag that separates this material from every other
// vault kind.
const aadDomain = "uzi-run-env-v1"

// AADKind returns the "kind" string the vault seam (Seal, Open, OpenAtEpoch and
// the conversion path) binds a ciphertext to, next to the owner id. It encodes the
// domain tag, the scope tag, the repo id (empty for the user scope), the secret
// flag and the length-prefixed name, so a ciphertext cannot be moved between
// names, repos or scopes, and a secret cannot be relabelled plain.
//
// Layout, with 0x00 as the separator:
//
//	"uzi-run-env-v1" 0x00 scope 0x00 repoID 0x00 ("1"|"0") 0x00 uint32-BE(len(name)) name
func AADKind(scope string, repoID *uuid.UUID, secret bool, name string) string {
	repo := ""
	if repoID != nil {
		repo = repoID.String()
	}
	flag := "0"
	if secret {
		flag = "1"
	}
	b := make([]byte, 0, len(aadDomain)+len(scope)+len(repo)+len(name)+12)
	b = append(b, aadDomain...)
	b = append(b, 0)
	b = append(b, scope...)
	b = append(b, 0)
	b = append(b, repo...)
	b = append(b, 0)
	b = append(b, flag...)
	b = append(b, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(name))) //nolint:gosec // G115: names are validated to at most 128 bytes before they reach a seal.
	b = append(b, name...)
	return string(b)
}
