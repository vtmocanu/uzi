package kube

import (
	"fmt"
	"strconv"
	"strings"
)

// MinRelocatableMountWorkerTag is the first worker image release whose entrypoint and
// guardrails follow a relocated join-token Secret (issue #1761). An OLDER image still
// STARTS with a relocated Secret, because its config reads the configured
// UZI_WORKER_TOKEN_FILE and its entrypoint's /run/secrets check is skipped when that file
// is absent, but its guardrails deny only /run/secrets/, so the agent's commands would not
// be screened for the new directory. Refusing that pairing at boot is the only thing that
// prevents it: requiring the knob alone does not.
const MinRelocatableMountWorkerTag = "0.85.0-rc.2"

// ValidateSecretMountWorkerImage refuses a relocated join-token Secret paired with a worker
// image that predates MinRelocatableMountWorkerTag. The default mount path is never
// affected. A tag that is not semver (a digest, `dev`, a branch name) cannot be compared,
// so it is refused too unless allowUnversioned is set: the operator's explicit statement
// that the image carries the change.
func ValidateSecretMountWorkerImage(cfg RenderConfig, workerTag string, allowUnversioned bool) error {
	if cfg.SecretMountPath == "" || cfg.SecretMountPath == defaultSecretMountPath {
		return nil
	}
	v, ok := parseSemver(workerTag)
	if !ok {
		if allowUnversioned {
			return nil
		}
		return fmt.Errorf("worker secret mount path %q needs a worker image >= %s, but the worker image tag %q is not a semver version; set UZI_WORKER_SECRET_MOUNT_ALLOW_UNVERSIONED_IMAGE=true (chart: workers.secretMountPathAllowUnversionedImage) only if that image carries the relocated-Secret support", cfg.SecretMountPath, MinRelocatableMountWorkerTag, workerTag)
	}
	min, _ := parseSemver(MinRelocatableMountWorkerTag)
	if compareSemver(v, min) < 0 {
		return fmt.Errorf("worker secret mount path %q needs a worker image >= %s (its guardrails must know the relocated Secret directory), but workers.image.tag is %q", cfg.SecretMountPath, MinRelocatableMountWorkerTag, workerTag)
	}
	return nil
}

type semver struct {
	core [3]uint64
	pre  []string
}

// parseSemver parses a SemVer 2.0.0 version, with an optional leading `v` (the repo's
// tags carry none, but a `v` prefix is a common operator spelling). Build metadata is
// accepted and ignored, as precedence requires.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if !validIdents(s[i+1:], false) {
			return semver{}, false
		}
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
		if !validIdents(pre, true) {
			return semver{}, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		if !isNumericIdent(p) {
			return semver{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return semver{}, false
		}
		v.core[i] = n
	}
	if pre != "" {
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

// isNumericIdent: digits only, and no leading zero unless it is exactly "0".
func isNumericIdent(p string) bool {
	if p == "" || (len(p) > 1 && p[0] == '0') {
		return false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validIdents(s string, prerelease bool) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		allDigits := true
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9':
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
				allDigits = false
			default:
				return false
			}
		}
		if prerelease && allDigits && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// compareSemver implements SemVer 2.0.0 precedence: core numerically, then a version
// with a prerelease sorts BEFORE the same core without one, then prerelease identifiers
// left to right (numeric < alphanumeric; numerics numerically; strings lexically; a
// shorter list sorts first when all shared identifiers are equal).
func compareSemver(a, b semver) int {
	for i := 0; i < 3; i++ {
		if a.core[i] != b.core[i] {
			if a.core[i] < b.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := compareIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func compareIdent(x, y string) int {
	xn, xNum := numericValue(x)
	yn, yNum := numericValue(y)
	switch {
	case xNum && yNum:
		switch {
		case xn < yn:
			return -1
		case xn > yn:
			return 1
		}
		return 0
	case xNum:
		return -1
	case yNum:
		return 1
	}
	return strings.Compare(x, y)
}

func numericValue(s string) (uint64, bool) {
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}
