package hostedsvc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PRD #58 Decision 1: the api gets ZERO kube-apiserver access, and
// `automountServiceAccountToken: false` stays. The primary enforcement is
// structural — the controller is a separate Go module
// (gitlab.example.com/vtmocanu/uzi/controller), so no package under api/ can
// import a kube client without someone first adding it to api/go.mod, which fails
// the build until they do and shows up as a reviewable go.mod/go.sum diff when
// they try.
//
// These tests are the belt to that suspenders. The module boundary stops an
// ACCIDENTAL import; it does not stop a deliberate `go get k8s.io/client-go` in
// api/, which would look like an ordinary dependency bump in review. The auditor
// lists any kube client under api/ as a blocking finding, so the invariant gets a
// gate that fails loudly rather than relying on a reviewer noticing a line in
// go.sum.
//
// It lives in hostedsvc because that is the package a kube client would most
// plausibly creep into (it is the api's hosted-worker code), but it asserts over
// the WHOLE api module.

// kubeModulePrefixes are the module paths that would mean the api gained a
// kube-apiserver client. sigs.k8s.io/controller-runtime is included because it is
// the other obvious way to reach an apiserver.
var kubeModulePrefixes = []string{
	"k8s.io/client-go",
	"k8s.io/api",
	"k8s.io/apimachinery",
	"k8s.io/kubectl",
	"sigs.k8s.io/controller-runtime",
}

// apiModuleRoot returns the api module's directory (this file lives two levels
// under it).
func apiModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve api module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("api go.mod not found at %s: %v", root, err)
	}
	return root
}

// The api's own module file must never require a kube client.
func TestAPIGoModHasNoKubeClient(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(apiModuleRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read api go.mod: %v", err)
	}
	for _, prefix := range kubeModulePrefixes {
		if strings.Contains(string(raw), prefix) {
			t.Fatalf("api/go.mod requires %q — PRD #58 Decision 1: the api gets zero kube-apiserver access. "+
				"Kube clients belong in the controller module (controller/go.mod), which is why it is a separate module.", prefix)
		}
	}
}

// The stronger form: nothing the api module actually BUILDS may transitively
// depend on a kube client. go.mod alone would miss an indirect dependency pulled
// in by something else.
func TestAPIBuildGraphHasNoKubeClient(t *testing.T) {
	root := apiModuleRoot(t)
	// -deps walks the full transitive import graph of every package in the module.
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = root
	// Linked worktrees trip Go's VCS stamping; this mirrors the documented local
	// build flag and never reaches a committed file.
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	out, err := cmd.Output()
	if err != nil {
		// In CI this is a hard failure: silently downgrading a Decision 1 gate to the
		// weaker go.mod substring check is exactly how an invariant rots. Locally a
		// toolchain/network hiccup only skips, so a developer offline on a plane is not
		// blocked by a check the pipeline will run anyway.
		if os.Getenv("CI") != "" {
			t.Fatalf("go list -deps failed in CI (%v); the Decision 1 build-graph gate must not be skipped here", err)
		}
		t.Skipf("go list -deps unavailable (%v); TestAPIGoModHasNoKubeClient still enforces the invariant", err)
	}
	for _, pkg := range strings.Split(string(out), "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}
		for _, prefix := range kubeModulePrefixes {
			if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
				t.Fatalf("the api's build graph reaches %q (via package %q) — PRD #58 Decision 1: the api gets zero kube-apiserver access.", prefix, pkg)
			}
		}
	}
}
