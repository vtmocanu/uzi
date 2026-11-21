package runenv

import (
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

func TestAADKindV2LengthPrefixPreventsAmbiguity(t *testing.T) {
	user, varA, varB := uuid.New(), uuid.New(), uuid.New()
	pairs := [][2]aadV2Case{
		{{user, ScopeUser, nil, true, "A", varA, 1}, {user, ScopeUser, nil, true, "A\x00", varA, 1}},
		{{user, ScopeUser, nil, true, "AB", varA, 1}, {user, ScopeUser, nil, true, "A", varB, 1}},
		{{user, ScopeUser, nil, false, "A", varA, 1}, {user, ScopeUser, nil, true, "A", varA, 1}},
		{{user, ScopeUser, nil, true, "X", varA, 1}, {user, ScopeUser, nil, true, "X", varA, 256}},
	}
	for i, p := range pairs {
		if p[0].kind() == p[1].kind() {
			t.Errorf("pair %d collides", i)
		}
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
