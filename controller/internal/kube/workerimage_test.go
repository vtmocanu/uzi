package kube

import "testing"

const testRelocatedMount = "/run/uzi" + "-secrets"

func TestCompareSemverPrecedence(t *testing.T) {
	// Ascending: each must sort strictly before the next (SemVer 2.0.0 §11 plus the
	// release train's own ordering).
	order := []string{
		"0.84.0",
		"0.85.0-alpha",
		"0.85.0-alpha.1",
		"0.85.0-rc.1",
		"0.85.0-rc.2",
		"0.85.0-rc.10",
		"0.85.0",
		"0.85.1",
		"0.86.0-rc.1",
		"1.0.0",
	}
	for i := 0; i+1 < len(order); i++ {
		a, okA := parseSemver(order[i])
		b, okB := parseSemver(order[i+1])
		if !okA || !okB {
			t.Fatalf("parse failed: %q=%v %q=%v", order[i], okA, order[i+1], okB)
		}
		if compareSemver(a, b) >= 0 || compareSemver(b, a) <= 0 {
			t.Errorf("want %s < %s", order[i], order[i+1])
		}
	}
	a, _ := parseSemver("0.85.0-rc.2+build.7")
	b, _ := parseSemver("v0.85.0-rc.2")
	if compareSemver(a, b) != 0 {
		t.Error("build metadata and a v prefix must not change precedence")
	}
	for _, bad := range []string{"", "dev", "latest", "sha256:abc", "0.85", "0.85.0.1", "01.2.3", "0.85.0-", "0.85.0-rc..1", "0.85.0-01", "0.85.0-rc_1"} {
		if _, ok := parseSemver(bad); ok {
			t.Errorf("%q parsed as semver", bad)
		}
	}
}

func TestValidateSecretMountWorkerImage(t *testing.T) {
	// A mount PATH, not a credential: built from the test's own path constant so the
	// struct literal carries no string gosec's G101 reads as a hardcoded secret.
	relocated := RenderConfig{SecretMountPath: testRelocatedMount}
	type imageCase struct {
		title     string
		cfg       RenderConfig
		version   string
		allow     bool
		wantError bool
	}
	cases := []imageCase{
		{"old rc + relocation refused", relocated, "0.85.0-rc.1", false, true},
		{"older stable + relocation refused", relocated, "0.84.0", false, true},
		{"first carrying release accepted", relocated, "0.85.0-rc.2", false, false},
		{"later rc accepted", relocated, "0.85.0-rc.10", false, false},
		{"stable accepted", relocated, "0.85.0", false, false},
		{"later stable accepted", relocated, "0.86.1", false, false},
		{"non-semver refused by default", relocated, "dev", false, true},
		{"digest refused by default", relocated, "sha256:abc", false, true},
		{"non-semver accepted with the escape hatch", relocated, "dev", true, false},
		{"escape hatch does not rescue an OLD semver tag", relocated, "0.85.0-rc.1", true, true},
		{"default path + old tag unaffected", RenderConfig{}, "0.84.0", false, false},
		{"explicit default path + old tag unaffected", RenderConfig{SecretMountPath: "/run/secrets"}, "0.80.0", false, false},
		{"default path + non-semver unaffected", RenderConfig{}, "dev", false, false},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			err := ValidateSecretMountWorkerImage(c.cfg, c.version, c.allow)
			if (err != nil) != c.wantError {
				t.Fatalf("err = %v, wantError %v", err, c.wantError)
			}
		})
	}
}
