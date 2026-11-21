package runenv

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type aadV2Case struct {
	user   uuid.UUID
	scope  string
	repo   *uuid.UUID
	secret bool
	name   string
	varID  uuid.UUID
	rev    int32
}

func (c aadV2Case) kind() string {
	return AADKindV2(c.user, c.scope, c.repo, c.secret, c.name, c.varID, c.rev)
}

func TestAADKindV2DiffersFromV1(t *testing.T) {
	repo := uuid.New()
	c := aadV2Case{uuid.New(), ScopeRepo, &repo, true, "TOKEN", uuid.New(), 1}
	if c.kind() == AADKind(c.scope, c.repo, c.secret, c.name) {
		t.Fatal("v2 context equals the v1 context")
	}
}

func TestAADKindV2EveryFieldChangesTheContext(t *testing.T) {
	repo, otherRepo := uuid.New(), uuid.New()
	base := aadV2Case{uuid.New(), ScopeRepo, &repo, true, "TOKEN", uuid.New(), 3}
	variants := map[string]aadV2Case{}
	add := func(label string, mut func(c *aadV2Case)) {
		c := base
		mut(&c)
		variants[label] = c
	}
	add("user", func(c *aadV2Case) { c.user = uuid.New() })
	add("scope", func(c *aadV2Case) { c.scope = ScopeUser; c.repo = nil })
	add("repo", func(c *aadV2Case) { c.repo = &otherRepo })
	add("secret", func(c *aadV2Case) { c.secret = false })
	add("name", func(c *aadV2Case) { c.name = "TOKEN2" })
	add("var", func(c *aadV2Case) { c.varID = uuid.New() })
	add("rev", func(c *aadV2Case) { c.rev = 4 })
	baseKind := base.kind()
	if baseKind != base.kind() {
		t.Fatal("v2 context is not deterministic")
	}
	for label, c := range variants {
		if c.kind() == baseKind {
			t.Errorf("changing %s left the v2 context unchanged", label)
		}
	}
}

// Without the per-field length prefixes these tuples concatenate to the same bytes: the scope
// "user" followed by a repo id, against the scope "user<repo id>" with no repo. Each pair is
// checked to collide when prefixes are left out, so the test fails if AADKindV2 ever drops them.
func TestAADKindV2LengthPrefixPreventsAmbiguity(t *testing.T) {
	user, varID, repo := uuid.New(), uuid.New(), uuid.New()
	unprefixed := func(c aadV2Case) string {
		r := ""
		if c.repo != nil {
			r = c.repo.String()
		}
		flag := "0"
		if c.secret {
			flag = "1"
		}
		var rev [4]byte
		binary.BigEndian.PutUint32(rev[:], uint32(c.rev)) //nolint:gosec // G115: the fixture revisions are small positive counters.
		return c.user.String() + c.scope + r + flag + c.name + c.varID.String() + string(rev[:])
	}
	pairs := [][2]aadV2Case{
		{{user, "user", &repo, true, "A", varID, 1}, {user, "user" + repo.String(), nil, true, "A", varID, 1}},
		{{user, "", &repo, true, "A", varID, 1}, {user, repo.String(), nil, true, "A", varID, 1}},
	}
	for i, p := range pairs {
		if unprefixed(p[0]) != unprefixed(p[1]) {
			t.Fatalf("pair %d does not collide without length prefixes: the test would prove nothing", i)
		}
		if p[0].kind() == p[1].kind() {
			t.Errorf("pair %d collides under AADKindV2", i)
		}
	}
	// Other single-field neighbours stay distinct too.
	varB := uuid.New()
	for i, p := range [][2]aadV2Case{
		{{user, ScopeUser, nil, true, "A", varID, 1}, {user, ScopeUser, nil, true, "A\x00", varID, 1}},
		{{user, ScopeUser, nil, false, "A", varID, 1}, {user, ScopeUser, nil, true, "A", varID, 1}},
		{{user, ScopeUser, nil, true, "X", varID, 1}, {user, ScopeUser, nil, true, "X", varID, 256}},
		{{user, ScopeUser, nil, true, "X", varID, 1}, {user, ScopeUser, nil, true, "X", varB, 1}},
	} {
		if p[0].kind() == p[1].kind() {
			t.Errorf("neighbour pair %d collides", i)
		}
	}
}

// The exact layout is a contract with every stored version-2 ciphertext: a change to it makes
// existing rows undecryptable. The expected bytes are written out by hand, field by field.
func TestAADKindV2GoldenBytes(t *testing.T) {
	user := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	repo := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	varID := uuid.MustParse("00000000-0000-4000-8000-000000000003")
	want := "uzi-run-env-v2" +
		"\x00\x00\x00\x24" + "00000000-0000-4000-8000-000000000001" +
		"\x00\x00\x00\x04" + "repo" +
		"\x00\x00\x00\x24" + "00000000-0000-4000-8000-000000000002" +
		"\x00\x00\x00\x01" + "1" +
		"\x00\x00\x00\x05" + "TOKEN" +
		"\x00\x00\x00\x24" + "00000000-0000-4000-8000-000000000003" +
		"\x00\x00\x00\x04" + "\x00\x00\x00\x07"
	if got := AADKindV2(user, ScopeRepo, &repo, true, "TOKEN", varID, 7); got != want {
		t.Errorf("repo scope layout changed:\n got %q\nwant %q", got, want)
	}
	wantUser := "uzi-run-env-v2" +
		"\x00\x00\x00\x24" + "00000000-0000-4000-8000-000000000001" +
		"\x00\x00\x00\x04" + "user" +
		"\x00\x00\x00\x00" +
		"\x00\x00\x00\x01" + "0" +
		"\x00\x00\x00\x01" + "N" +
		"\x00\x00\x00\x24" + "00000000-0000-4000-8000-000000000003" +
		"\x00\x00\x00\x04" + "\x01\x02\x03\x04"
	if got := AADKindV2(user, ScopeUser, nil, false, "N", varID, 0x01020304); got != wantUser {
		t.Errorf("user scope layout changed:\n got %q\nwant %q", got, wantUser)
	}
}

func TestAADForSelectsByVersion(t *testing.T) {
	user, varID, repo := uuid.New(), uuid.New(), uuid.New()
	v1, err := AADFor(AADVersion1, user, ScopeRepo, &repo, true, "N", varID, 2)
	if err != nil || v1 != AADKind(ScopeRepo, &repo, true, "N") {
		t.Fatalf("version 1: %q %v", v1, err)
	}
	v2, err := AADFor(AADVersion2, user, ScopeRepo, &repo, true, "N", varID, 2)
	if err != nil || v2 != AADKindV2(user, ScopeRepo, &repo, true, "N", varID, 2) {
		t.Fatalf("version 2: %q %v", v2, err)
	}
	if v1 == v2 {
		t.Fatal("versions select the same context")
	}
	for _, bad := range []int16{0, 3, -1} {
		if _, err := AADFor(bad, user, ScopeRepo, &repo, true, "N", varID, 2); !errors.Is(err, ErrUnknownAADVersion) {
			t.Errorf("version %d: err = %v", bad, err)
		}
	}
}
