// Package runenv holds the rules and the sealing context for run environment
// variables (PRD #4416): who may name a variable, what a value may hold, how many
// a scope may carry, and which context binds a sealed value. It is a leaf: it
// imports neither the vault nor the store, so both can depend on it.
package runenv

import (
	"encoding/binary"
	"errors"

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

// aadDomainV2 is the domain tag of the version-2 context.
const aadDomainV2 = "uzi-run-env-v2"

// AAD versions, as stored in run_env_vars.aad_version.
const (
	AADVersion1 int16 = 1
	AADVersion2 int16 = 2
)

// ErrUnknownAADVersion is returned by AADFor for a version it does not know.
var ErrUnknownAADVersion = errors.New("runenv: unknown aad version")

// AADKindV2 returns the version-2 sealing context. It binds, next to the vault seam's
// own owner id, every field of AADKind plus the owner id, the variable id and the
// version number, so a ciphertext cannot be moved to another version of the same
// variable, to another variable or to another owner.
//
// Layout: the domain tag, then each field as uint32-BE(len) followed by its bytes, in the
// order owner id (canonical text), scope, repo id (empty for the user scope), secret flag
// ("1" or "0"), name, var id (canonical text), rev (uint32-BE). Every field is length
// prefixed, so no two distinct field tuples encode to the same bytes.
func AADKindV2(userID uuid.UUID, scope string, repoID *uuid.UUID, secret bool, name string, varID uuid.UUID, rev int32) string {
	repo := ""
	if repoID != nil {
		repo = repoID.String()
	}
	flag := "0"
	if secret {
		flag = "1"
	}
	var revBytes [4]byte
	binary.BigEndian.PutUint32(revBytes[:], uint32(rev)) //nolint:gosec // G115: rev is a positive counter (table CHECK rev >= 1).
	b := make([]byte, 0, len(aadDomainV2)+len(scope)+len(repo)+len(name)+128)
	b = append(b, aadDomainV2...)
	for _, field := range []string{userID.String(), scope, repo, flag, name, varID.String(), string(revBytes[:])} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(field))) //nolint:gosec // G115: every field is bounded (names to 128 bytes, ids to 36).
		b = append(b, field...)
	}
	return string(b)
}

// AADFor returns the sealing context for one stored row, selected by its aad_version.
// Every seal and open of a run environment value goes through it, so a row sealed under
// version 1 keeps opening under version 1 while every new seal uses version 2.
func AADFor(aadVersion int16, userID uuid.UUID, scope string, repoID *uuid.UUID, secret bool, name string, varID uuid.UUID, rev int32) (string, error) {
	switch aadVersion {
	case AADVersion1:
		return AADKind(scope, repoID, secret, name), nil
	case AADVersion2:
		return AADKindV2(userID, scope, repoID, secret, name, varID, rev), nil
	default:
		return "", ErrUnknownAADVersion
	}
}
