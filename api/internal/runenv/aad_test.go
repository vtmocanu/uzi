package runenv

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestAADVersionsDiffer(t *testing.T) {
	repo := uuid.New()
	varID := uuid.New()
	v1, err := AADFor(AADVersion1, ScopeRepo, &repo, true, "API_KEY", varID, 3)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := AADFor(AADVersion2, ScopeRepo, &repo, true, "API_KEY", varID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("v1 and v2 contexts are equal")
	}
	if v1 != AADKind(ScopeRepo, &repo, true, "API_KEY") {
		t.Fatal("AADFor(1) is not AADKind")
	}
}

func TestAADV2BindsEveryField(t *testing.T) {
	repo, other := uuid.New(), uuid.New()
	varID := uuid.New()
	base := AADKindV2(ScopeRepo, &repo, true, "API_KEY", varID, 1)
	cases := map[string]string{
		"rev":    AADKindV2(ScopeRepo, &repo, true, "API_KEY", varID, 2),
		"var_id": AADKindV2(ScopeRepo, &repo, true, "API_KEY", uuid.New(), 1),
		"repo":   AADKindV2(ScopeRepo, &other, true, "API_KEY", varID, 1),
		"scope":  AADKindV2(ScopeUser, nil, true, "API_KEY", varID, 1),
		"secret": AADKindV2(ScopeRepo, &repo, false, "API_KEY", varID, 1),
		"name":   AADKindV2(ScopeRepo, &repo, true, "API_KEY2", varID, 1),
	}
	for field, got := range cases {
		if got == base {
			t.Errorf("changing %s does not change the v2 context", field)
		}
	}
	// Length prefixes: moving a byte across a field boundary must change the context.
	a := AADKindV2(ScopeRepo, nil, true, "AB", varID, 1)
	b := AADKindV2(ScopeRepo, nil, true, "A", varID, 1)
	if a == b {
		t.Fatal("name length not bound")
	}
}

func TestAADForRejectsUnknownVersion(t *testing.T) {
	for _, v := range []int{0, 3, -1} {
		if _, err := AADFor(v, ScopeUser, nil, false, "X", uuid.New(), 1); !errors.Is(err, ErrUnknownAADVersion) {
			t.Errorf("version %d: err = %v, want ErrUnknownAADVersion", v, err)
		}
	}
}
