package oidc

import (
	"context"
	"testing"

	"golang.org/x/oauth2"
)

// TestExchangeGroupsClaimParsing exercises the tolerant groups-claim parse end to
// end through a real signed token (PRD #55 Decision 1). present is true ONLY for a
// JSON array of strings (empty array included, an authoritative empty membership);
// absent, JSON null, and every non-string-array shape are fail-safe (nil, false).
func TestExchangeGroupsClaimParsing(t *testing.T) {
	cases := []struct {
		name        string
		set         bool
		val         any
		wantPresent bool
		wantGroups  []string
	}{
		{"absent", false, nil, false, nil},
		{"null", true, nil, false, nil},
		{"empty array", true, []any{}, true, nil},
		{"array of strings", true, []any{"uzi-admins", "staff"}, true, []string{"uzi-admins", "staff"}},
		{"single-element array", true, []any{"uzi-admins"}, true, []string{"uzi-admins"}},
		{"string not array", true, "uzi-admins", false, nil},
		{"mixed array", true, []any{"uzi-admins", 7}, false, nil},
		{"number", true, 42, false, nil},
		{"object", true, map[string]any{"role": "admin"}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeIDP(t, "uzi-client")
			defer f.srv.Close()
			f.nonce = "n"
			f.groupsSet = tc.set
			f.groups = tc.val

			id, err := f.provider().Exchange(context.Background(), "code", oauth2.GenerateVerifier(), "n")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if id.GroupsClaimPresent != tc.wantPresent {
				t.Errorf("GroupsClaimPresent = %v, want %v", id.GroupsClaimPresent, tc.wantPresent)
			}
			if !equalStrings(id.Groups, tc.wantGroups) {
				t.Errorf("Groups = %v, want %v", id.Groups, tc.wantGroups)
			}
		})
	}
}

// TestExchangeGroupsCustomClaimName: the claim name is dynamic — a non-default name
// (e.g. "roles") is looked up when threaded as GroupsClaim, and the default "groups"
// key is then ignored.
func TestExchangeGroupsCustomClaimName(t *testing.T) {
	f := newFakeIDP(t, "uzi-client")
	defer f.srv.Close()
	f.nonce = "n"
	f.groupsClaimName = "roles"
	f.groupsSet = true
	f.groups = []any{"platform"}

	id, err := f.provider().Exchange(context.Background(), "code", oauth2.GenerateVerifier(), "n")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !id.GroupsClaimPresent {
		t.Fatal("GroupsClaimPresent = false, want true for a present custom claim")
	}
	if want := []string{"platform"}; !equalStrings(id.Groups, want) {
		t.Errorf("Groups = %v, want %v", id.Groups, want)
	}
}

// TestExchangeGroupsClaimDisabledWhenNameEmpty: an empty GroupsClaim disables group
// parsing entirely even when the token carries a "groups" array (fail-safe default
// for the dormant/unconfigured case).
func TestExchangeGroupsClaimDisabledWhenNameEmpty(t *testing.T) {
	f := newFakeIDP(t, "uzi-client")
	defer f.srv.Close()
	f.nonce = "n"
	f.groupsClaimName = "" // provider() threads this as GroupsClaim="" => parsing off
	f.groupsSet = true
	f.groups = []any{"uzi-admins"}

	id, err := f.provider().Exchange(context.Background(), "code", oauth2.GenerateVerifier(), "n")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.GroupsClaimPresent {
		t.Error("GroupsClaimPresent = true with an empty GroupsClaim, want false")
	}
	if len(id.Groups) != 0 {
		t.Errorf("Groups = %v, want nil with an empty GroupsClaim", id.Groups)
	}
}

// equalStrings compares two string slices by length + element, treating nil and an
// empty slice as equal (the present-but-empty membership is distinguished by
// GroupsClaimPresent, not by nil-ness).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
