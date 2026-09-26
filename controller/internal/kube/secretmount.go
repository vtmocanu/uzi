package kube

import (
	"fmt"
	"regexp"
	"strings"
)

// secretMountPathRE is the shape an overridden join-token mount must have: ONE
// directory directly under /run (a container tmpfs the worker never writes to),
// lower-case. That excludes `/`, relative paths, `..`, a trailing slash, anything under
// the persistent /data or /nix volumes, and every system directory, without an
// open-ended deny list.
var secretMountPathRE = regexp.MustCompile(`^/run/[a-z0-9][a-z0-9._-]*$`)

// knownWorkerMountPaths is every mount path a hosted-worker pod renders OTHER than the
// join-token Secret, across every container and posture. A custom secret mount must not
// equal, contain or sit inside any of them: a shared prefix would either mask the token
// with another volume or expose it through a volume another container (dind) also
// mounts. TestKnownWorkerMountPathsCoverRenderedPods fails when a render grows a mount
// this list does not name, so a new volume cannot be forgotten here.
var knownWorkerMountPaths = []string{
	dataMountPath,
	nixMountPath,
	nixSeedMountPath,
	dataSeedMountPath,
	codexCmdCacheDir,
	dindWorkdirDir,
	dindSocketDir,
	dindDataDir,
	dindDataDirRoot,
}

// ValidateSecretMountPath refuses a SecretMountPath override that is malformed or
// overlaps another worker mount. Empty (the default /run/secrets) is always valid.
// Called once at boot, like ValidatePVCCeilings: a bad value would otherwise surface
// only as every hosted worker crash-looping on a missing token.
func ValidateSecretMountPath(cfg RenderConfig) error {
	p := cfg.SecretMountPath
	if p == "" || p == defaultSecretMountPath {
		return nil
	}
	if !secretMountPathRE.MatchString(p) {
		return fmt.Errorf("worker secret mount path %q must be a single lower-case directory directly under /run (for example /run/uzi-secrets)", p)
	}
	for _, m := range knownWorkerMountPaths {
		if pathsOverlap(p, m) {
			return fmt.Errorf("worker secret mount path %q overlaps the worker mount %q", p, m)
		}
	}
	return nil
}

// pathsOverlap reports whether a and b are the same directory or one contains the other.
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
