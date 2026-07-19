package settings

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// The docker-worker repo allowlist (PRD #89 M-allow): a comma-separated list of repo
// UUIDs. Empty is the fail-closed "no repos trusted" value.

func TestValidateRepoAllowlist(t *testing.T) {
	a, b := uuid.New().String(), uuid.New().String()
	ok := []string{
		"",                  // empty = fail-closed, a valid value (not a rejection)
		"  ",                // whitespace-only collapses to empty
		a,                   // one repo
		a + "," + b,         // two repos (the comma ValidateLabel would have rejected)
		" " + a + " , " + b, // surrounding whitespace tolerated
		a + ",",             // trailing separator / empty token skipped
	}
	for _, v := range ok {
		if err := Validate(KeyDockerRepoAllowlist, v); err != nil {
			t.Errorf("Validate(docker_repo_allowlist, %q) = %v, want nil", v, err)
		}
	}
	bad := []string{
		"not-a-uuid",
		a + ",garbage",
		"PRD", // a valid label, an invalid allowlist
		"12345",
		a + " " + b, // space-separated, not comma-separated → the second token is invalid
	}
	for _, v := range bad {
		if err := Validate(KeyDockerRepoAllowlist, v); err == nil {
			t.Errorf("Validate(docker_repo_allowlist, %q) = nil, want a rejection", v)
		}
	}
}

// The same regression the hosted-quota test pins: without its explicit Validate case
// the key falls through to ValidateLabel. ValidateLabel accepts a single non-empty
// ≤64-char string but REJECTS a comma — so a two-repo allowlist could never be saved,
// while a bogus single token like "PRD" would be accepted and then silently parse to
// an empty (fail-closed) list on read. This test fails if the case is removed.
func TestValidateRepoAllowlistDoesNotFallThroughToLabelRules(t *testing.T) {
	a, b := uuid.New().String(), uuid.New().String()
	// A legitimate two-repo allowlist: contains a comma, which ValidateLabel rejects.
	if err := Validate(KeyDockerRepoAllowlist, a+","+b); err != nil {
		t.Fatalf("Validate(docker_repo_allowlist, two repos) = %v — the key fell through to "+
			"ValidateLabel, which rejects the comma a multi-repo allowlist requires", err)
	}
	// A perfectly good label that is not a UUID must be rejected here, not accepted.
	if err := Validate(KeyDockerRepoAllowlist, "PRD"); err == nil {
		t.Fatal("Validate(docker_repo_allowlist, \"PRD\") = nil — the key fell through to " +
			"ValidateLabel, so a non-UUID would be accepted and then silently read back as " +
			"an empty (fail-closed) allowlist")
	}
}

func TestDockerRepoAllowlistAccessor(t *testing.T) {
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()

	c := New(&fakeStore{rows: []store.AppSetting{row(KeyDockerRepoAllowlist, a.String()+","+b.String())}}, time.Minute)
	got, err := c.DockerRepoAllowlist(ctx)
	if err != nil {
		t.Fatalf("DockerRepoAllowlist: %v", err)
	}
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("DockerRepoAllowlist = %v, want [%s %s]", got, a, b)
	}
}

// An absent row (and the compiled-in default) is the empty, fail-closed allowlist:
// a NON-NIL empty slice, so the claim param encodes as a Postgres array, never NULL.
func TestDockerRepoAllowlistDefaultIsEmptyNonNil(t *testing.T) {
	got, err := New(&fakeStore{}, time.Minute).DockerRepoAllowlist(context.Background())
	if err != nil {
		t.Fatalf("DockerRepoAllowlist: %v", err)
	}
	if got == nil {
		t.Fatal("DockerRepoAllowlist default = nil, want a non-nil empty slice (so it encodes as {} not NULL)")
	}
	if len(got) != 0 {
		t.Errorf("DockerRepoAllowlist default = %v, want empty (fail-closed)", got)
	}
}

// Junk-tolerance on READ mirrors the other accessors: a hand-edited row with a bad
// token skips only that token rather than erroring the whole claim. Validate is the
// real gate on the way in; this is the backstop for what Validate cannot reach.
func TestDockerRepoAllowlistIsJunkTolerantOnRead(t *testing.T) {
	good := uuid.New()
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyDockerRepoAllowlist, "garbage,"+good.String())}}, time.Minute)
	got, err := c.DockerRepoAllowlist(context.Background())
	if err != nil {
		t.Fatalf("DockerRepoAllowlist: %v", err)
	}
	if len(got) != 1 || got[0] != good {
		t.Errorf("DockerRepoAllowlist(garbage,%s) = %v, want [%s]", good, got, good)
	}
}

func TestDockerRepoAllowlistKeyKnownAndInDefaults(t *testing.T) {
	if !Known(KeyDockerRepoAllowlist) {
		t.Error("Known(docker_repo_allowlist) = false — the admin settings PUT would reject it")
	}
	if _, ok := Defaults[KeyDockerRepoAllowlist]; !ok {
		t.Error("Defaults[docker_repo_allowlist] missing — All/AdminView would not surface it")
	}
	// Not a secret: it is operator-set policy the admin page renders in the clear.
	if IsSecret(KeyDockerRepoAllowlist) {
		t.Error("IsSecret(docker_repo_allowlist) = true, want false")
	}
}
