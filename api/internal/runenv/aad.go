// Package runenv holds the rules and the sealing context for run environment
// variables (PRD #4416): who may name a variable, what a value may hold, how many
// a scope may carry, and which context binds a sealed value. It is a leaf: it
// imports neither the vault nor the store, so both can depend on it.
package runenv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

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

// AAD versions stored in run_env_vars.aad_version. M1 rows are version 1; every row
// written from M2 on is version 2, which also binds the variable id and the revision.
const (
	AADVersion1 = 1
	AADVersion2 = 2
)

// aadDomainV2 separates the version 2 context from version 1 and from every other
// vault kind.
const aadDomainV2 = "uzi-run-env-v2"

// ErrUnknownAADVersion is returned by AADFor for a version this build cannot rebuild.
var ErrUnknownAADVersion = errors.New("runenv: unknown AAD version")

func appendLenPrefixed(b []byte, field string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(field))) //nolint:gosec // G115: every field is bounded (a name to 128 bytes, ids to 36).
	return append(b, field...)
}

// AADKindV2 returns the version 2 sealing context. Every field is length-prefixed
// (uint32 big-endian length, then the bytes), so no field can bleed into the next, and
// the context binds the variable id and the revision as well: a ciphertext cannot be
// moved between versions of one variable (a stale secret copied over a rotated row), as
// well as between names, repos and scopes.
//
// Layout: "uzi-run-env-v2" followed by length-prefixed scope, repo id (empty for the user
// scope), secret flag ("1"|"0"), name, var id and the decimal revision.
func AADKindV2(scope string, repoID *uuid.UUID, secret bool, name string, varID uuid.UUID, rev int) string {
	repo := ""
	if repoID != nil {
		repo = repoID.String()
	}
	flag := "0"
	if secret {
		flag = "1"
	}
	b := make([]byte, 0, len(aadDomainV2)+len(scope)+len(repo)+len(name)+96)
	b = append(b, aadDomainV2...)
	b = appendLenPrefixed(b, scope)
	b = appendLenPrefixed(b, repo)
	b = appendLenPrefixed(b, flag)
	b = appendLenPrefixed(b, name)
	b = appendLenPrefixed(b, varID.String())
	b = appendLenPrefixed(b, strconv.Itoa(rev))
	return string(b)
}

// AADFor returns the sealing context of a row written under aadVersion. An unknown
// version is an error, never a guess: a row that cannot name its context cannot be opened.
func AADFor(aadVersion int, scope string, repoID *uuid.UUID, secret bool, name string, varID uuid.UUID, rev int) (string, error) {
	switch aadVersion {
	case AADVersion1:
		return AADKind(scope, repoID, secret, name), nil
	case AADVersion2:
		return AADKindV2(scope, repoID, secret, name, varID, rev), nil
	}
	return "", fmt.Errorf("%w: %d", ErrUnknownAADVersion, aadVersion)
}
