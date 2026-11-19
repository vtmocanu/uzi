// Package runenv holds the rules and the sealing context for run environment
// variables (PRD #4416): who may name a variable, what a value may hold, how many
// a scope may carry, and which context binds a sealed value. It is a leaf: it
// imports neither the vault nor the store, so both can depend on it.
package runenv

import (
	"encoding/binary"
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

// aadDomainV2 is the domain tag of the version-2 context. It differs from aadDomain, so a v2
// string can never equal a v1 string and no ciphertext opens under the other generation's context.
const aadDomainV2 = "uzi-run-env-v2"

// AAD generations (the aad_version column).
const (
	AADVersion1 int16 = 1
	AADVersion2 int16 = 2
)

// appendField appends one length-prefixed field: uint32-BE length, then the bytes.
func appendField(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // G115: every field is a name, a tag or a uuid string, far below 4 GiB.
	return append(b, s...)
}

// AADKindV2 is AADKind plus the variable id and the revision, so a ciphertext cannot be copied
// from one version of a variable into another (a rev-1 ciphertext in the rev-2 row no longer
// opens). Every field is length-prefixed, which makes the encoding injective without separators.
//
// Layout: "uzi-run-env-v2" then field(scope) field(repoID or "") field("1"|"0") field(varID)
// field(decimal rev) field(name), each field being uint32-BE length followed by its bytes.
func AADKindV2(scope string, repoID *uuid.UUID, secret bool, name string, varID uuid.UUID, rev int32) string {
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
	b = appendField(b, scope)
	b = appendField(b, repo)
	b = appendField(b, flag)
	b = appendField(b, varID.String())
	b = appendField(b, strconv.FormatInt(int64(rev), 10))
	b = appendField(b, name)
	return string(b)
}

// Row is the part of a run_env_vars row the sealing context is built from. RepoID nil is the
// user scope.
type Row struct {
	AADVersion int16
	RepoID     *uuid.UUID
	Secret     bool
	Name       string
	VarID      uuid.UUID
	Rev        int32
}

// AADFor returns the vault "kind" a row's value is sealed under, by its aad_version. It is the one
// helper every seal and open site uses, so no site can pick the wrong generation. A row under any
// version other than 1 is read as version 2, the generation every new seal writes (the column's
// CHECK admits only 1 and 2).
func AADFor(r Row) string {
	scope := ScopeUser
	if r.RepoID != nil {
		scope = ScopeRepo
	}
	if r.AADVersion == AADVersion1 {
		return AADKind(scope, r.RepoID, r.Secret, r.Name)
	}
	return AADKindV2(scope, r.RepoID, r.Secret, r.Name, r.VarID, r.Rev)
}
