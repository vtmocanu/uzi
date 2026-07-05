package handler

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// userSkill builds a user-scope skill owned by owner.
func userSkill(owner uuid.UUID) store.Skill {
	return store.Skill{Scope: "user", UserID: pgUUID(owner)}
}

func TestValidateSkillFields(t *testing.T) {
	const maxBytes = 100
	base := skillWriteRequest{Description: "does a thing.", Body: "# Playbook\n\nsteps.\n"}

	f, err := validateSkillFields(base, maxBytes)
	if err != nil {
		t.Fatalf("valid skill rejected: %v", err)
	}
	if f.description != "does a thing." || f.body != base.Body {
		t.Errorf("fields not carried through: %+v", f)
	}

	rejects := map[string]skillWriteRequest{
		"empty description":      {Description: "  ", Body: "b\n"},
		"newline in description": {Description: "legit\ndescription: evil", Body: "b\n"},
		"cr in description":      {Description: "legit\rx", Body: "b\n"},
		"tab in description":     {Description: "a\tb", Body: "b\n"},
		"empty body":             {Description: "d.", Body: "   "},
		"oversized body":         {Description: "d.", Body: strings.Repeat("x", maxBytes+1)},
		"full token in body":     {Description: "d.", Body: "key " + "sk-ant-api03-" + strings.Repeat("A", 80) + "\n"},
		"full token in descr":    {Description: "sk-ant-api03-" + strings.Repeat("A", 80), Body: "b\n"},
	}
	for name, req := range rejects {
		if _, err := validateSkillFields(req, maxBytes); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	// A body exactly at the cap is allowed; a description mentioning the token
	// format (not a full token) stays legal.
	if _, err := validateSkillFields(skillWriteRequest{Description: "d.", Body: strings.Repeat("x", maxBytes)}, maxBytes); err != nil {
		t.Errorf("body at the cap should be allowed: %v", err)
	}
	if _, err := validateSkillFields(skillWriteRequest{Description: "tokens start with sk-ant-.", Body: "b\n"}, maxBytes); err != nil {
		t.Errorf("token format mention should be allowed: %v", err)
	}
}

// TestAuthorizeSkillWrite is the core write-authz matrix: builtin/global are
// admin-only, a user skill is owner-only, and a non-owner non-admin sees 404
// (existence hidden) while an admin who cannot edit a user skill sees 403.
func TestAuthorizeSkillWrite(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	other := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}

	builtin := store.Skill{Scope: "builtin"}
	global := store.Skill{Scope: "global"}
	mine := userSkill(owner.ID)

	cases := []struct {
		name       string
		actor      store.User
		skill      store.Skill
		wantOK     bool
		wantStatus int
	}{
		{"admin edits builtin", admin, builtin, true, 0},
		{"user edits builtin", owner, builtin, false, 403},
		{"admin edits global", admin, global, true, 0},
		{"user edits global", owner, global, false, 403},
		{"owner edits own user skill", owner, mine, true, 0},
		{"admin edits others user skill", admin, mine, false, 403},
		{"other user edits user skill", other, mine, false, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok := authorizeSkillWrite(c.actor, c.skill)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok && status != c.wantStatus {
				t.Errorf("status = %d, want %d", status, c.wantStatus)
			}
		})
	}
}

// TestAllocatableRules pins which skills may be allocated as a shared row vs a
// user's own overlay. Shared: builtin/global only. Mine: builtin/global or the
// caller's own user skill — never another user's private skill.
func TestAllocatableRules(t *testing.T) {
	actor := store.User{ID: uuid.New()}
	otherID := uuid.New()

	builtin := store.Skill{Scope: "builtin"}
	global := store.Skill{Scope: "global"}
	mine := userSkill(actor.ID)
	theirs := userSkill(otherID)

	if !allocatableAsShared(builtin) || !allocatableAsShared(global) {
		t.Error("builtin/global must be shared-allocatable")
	}
	if allocatableAsShared(mine) || allocatableAsShared(theirs) {
		t.Error("user skills must not be shared-allocatable")
	}

	if !allocatableAsMine(builtin, actor) || !allocatableAsMine(global, actor) {
		t.Error("builtin/global must be mine-allocatable")
	}
	if !allocatableAsMine(mine, actor) {
		t.Error("own user skill must be mine-allocatable")
	}
	if allocatableAsMine(theirs, actor) {
		t.Error("another user's private skill must NOT be mine-allocatable")
	}
}
