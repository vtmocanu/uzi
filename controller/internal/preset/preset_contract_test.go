package preset

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The CONSUMER half of PRD #58's two cross-module goldens, and the thing that makes
// them gates rather than promises.
//
// M2 shipped the size golden's producer (api/internal/workersize.Names IS the
// golden) and recorded that it stays INERT until this file parses it: until then,
// editing one module's list and not the other was silently legal — which is the
// entire failure the golden exists to stop. M3 ships both halves of the template
// golden and this consumer for both.
//
// Reaching across the tree mirrors protocol_contract_test.go next door (and
// agent/test/claim-skills-contract.test.ts before it): the two modules share no Go
// code by design, so a file on disk is what joins them.
func goldenPath(name string) string {
	return filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", name)
}

func readGolden(t *testing.T, file, field string) []string {
	t.Helper()
	raw, err := os.ReadFile(goldenPath(file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc map[string][]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	names, ok := doc[field]
	if !ok {
		t.Fatalf("%s has no %q field", file, field)
	}
	if len(names) == 0 {
		// A golden that parsed to nothing would make every assertion below vacuously
		// pass — the "test that cannot fail" this PRD's Decision Log keeps catching.
		t.Fatalf("%s carries no %s", file, field)
	}
	return names
}

// Every size the api will ever put on the wire must resolve here. This is the gate:
// add a name to workersize.Names (and thus to the golden) without a preset entry
// and this goes red.
//
// VERIFIED BY MUTATION, not by inspection (this PRD's Decision Log records that a
// test that cannot fail is not a gate): adding "xxl" to the golden fails this test
// with "the api ships size "xxl" ...", and deleting the "l" entry from `sizes`
// fails it likewise.
func TestPresetTableCoversEveryAPISize(t *testing.T) {
	for _, name := range readGolden(t, "hosted_sizes.json", "sizes") {
		if _, ok := sizes[name]; !ok {
			t.Errorf("the api ships size %q and this controller's preset table does not carry it. "+
				"A worker provisioned at that size would never have a pod rendered: it would sit pending "+
				"until its join token expired, visible only as a worker that never came online.", name)
		}
	}
}

// Every template the api will ever put on the wire must resolve to an image.
func TestTemplateImageMapCoversEveryAPITemplate(t *testing.T) {
	for _, name := range readGolden(t, "hosted_templates.json", "templates") {
		if _, ok := templateImages[name]; !ok {
			t.Errorf("the api ships template %q and this controller's image map does not carry it. "+
				"A worker provisioned with that template would never have a pod rendered. "+
				"Adding an entry is not enough on its own: M6's CI must actually publish that image.", name)
		}
	}
}

// The reverse direction, which is a different failure and a milder one: a preset the
// api cannot provision is dead code, not a stranded worker. Worth failing anyway —
// it is always either a half-finished change or a typo, and both are cheaper to see
// here than to puzzle over later.
func TestPresetTablesCarryNothingTheAPICannotProvision(t *testing.T) {
	assertNoExtras(t, "size", readGolden(t, "hosted_sizes.json", "sizes"), SizeNames())
	assertNoExtras(t, "template", readGolden(t, "hosted_templates.json", "templates"), TemplateNames())
}

func assertNoExtras(t *testing.T, kind string, golden, table []string) {
	t.Helper()
	known := map[string]bool{}
	for _, n := range golden {
		known[n] = true
	}
	var extra []string
	for _, n := range table {
		if !known[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("this controller carries %s preset(s) %v the api's registry does not ship, so nothing can ever "+
			"provision one. Either the api's registry is missing them or these are leftovers.", kind, extra)
	}
}

// Resolve must reject an unknown name with a TYPED miss rather than a zero Spec.
// The type is what lets the reconcile loop tell deployment skew (log, skip
// rendering, KEEP the worker desired) from a real failure — and "keep the worker
// desired" is the difference between one unrendered worker and a torn-down fleet.
func TestResolveReturnsATypedMissForUnknownNames(t *testing.T) {
	r, err := NewResolver("harbor.example.com/uzi", "v1.2.3")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	for _, tc := range []struct{ template, size, field string }{
		{"python", "m", "template"}, // a template a newer api might ship
		{"base", "xxl", "size"},     // a size a newer api might ship
		{"", "", "template"},        // empty is unknown, never a default
	} {
		spec, err := r.Resolve(tc.template, tc.size)
		if err == nil {
			t.Fatalf("Resolve(%q, %q) = %+v, want an error", tc.template, tc.size, spec)
		}
		if !IsUnknown(err) {
			t.Fatalf("Resolve(%q, %q) error is not a preset miss: %v", tc.template, tc.size, err)
		}
		var u *UnknownError
		if !errors.As(err, &u) {
			t.Fatalf("Resolve(%q, %q): cannot extract the typed miss", tc.template, tc.size)
		}
		if u.Field != tc.field {
			t.Errorf("Resolve(%q, %q) blamed %q, want %q", tc.template, tc.size, u.Field, tc.field)
		}
		if spec != (Spec{}) {
			t.Errorf("Resolve(%q, %q) returned a non-zero Spec alongside its error", tc.template, tc.size)
		}
	}
}

func TestResolveBuildsTheImageReferenceFromConfig(t *testing.T) {
	// A trailing slash on the repo is the kind of thing a values file grows; it must
	// not produce a double slash in an image reference.
	r, err := NewResolver("harbor.example.com/gitlab/vtmocanu/uzi/", "v0.4.0")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	spec, err := r.Resolve("jvm", "l")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "harbor.example.com/gitlab/vtmocanu/uzi/agent-jvm:v0.4.0"; spec.Image != want {
		t.Errorf("Image = %q, want %q", spec.Image, want)
	}
	if spec.Size.CPULimit.String() != "4" || spec.Size.MemoryLimit.String() != "8Gi" {
		t.Errorf("size l = %+v, want the approved l quantities", spec.Size)
	}
}

// The repo and the tag are required, because both defaults that suggest themselves
// are worse than a refusal: an empty repo silently means Docker Hub, and a "latest"
// tag means a fleet running an unknown release.
func TestNewResolverRequiresRepoAndTag(t *testing.T) {
	for _, tc := range []struct{ repo, tag string }{
		{"", "v1"},
		{"harbor.example.com/uzi", ""},
		{"   ", "v1"},
		{"harbor.example.com/uzi", "  "},
	} {
		if _, err := NewResolver(tc.repo, tc.tag); err == nil {
			t.Errorf("NewResolver(%q, %q) = nil error, want a refusal", tc.repo, tc.tag)
		}
	}
}

// /nix does not vary by size or by template — it is measured byte-identical across
// base and jvm, which is exactly why Decision 7's correction moved it out of the
// preset table. Pin that: a per-size nix value creeping back in is a table with one
// repeated number, and M6's display golden inherits the same shape.
func TestNixSizeIsFlatAcrossEveryPresetAndTemplate(t *testing.T) {
	r, err := NewResolver("harbor.example.com/uzi", "v1")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	for _, template := range TemplateNames() {
		for _, size := range SizeNames() {
			spec, err := r.Resolve(template, size)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", template, size, err)
			}
			if spec.NixSize.Cmp(nixSize) != 0 {
				t.Errorf("Resolve(%q, %q).NixSize = %s, want the flat %s", template, size, spec.NixSize.String(), nixSize.String())
			}
		}
	}
}

// Burstable, not Guaranteed, and requests strictly below limits on every preset.
// Guaranteed contradicts the PRD's own fleet arithmetic and would strand a full
// memory limit per IDLE worker (measured idle: 148 MiB) on a shared cluster.
func TestEveryPresetIsBurstable(t *testing.T) {
	for name, s := range sizes {
		if s.CPURequest.Cmp(s.CPULimit) >= 0 {
			t.Errorf("preset %q: cpu request %s is not below limit %s", name, s.CPURequest.String(), s.CPULimit.String())
		}
		if s.MemoryRequest.Cmp(s.MemoryLimit) >= 0 {
			t.Errorf("preset %q: memory request %s is not below limit %s", name, s.MemoryRequest.String(), s.MemoryLimit.String())
		}
	}
}

// The measured floor, pinned as a property rather than as a comment. A real Claude
// Agent SDK run peaks at 676 MiB (cgroup memory.peak, whole live capstone run), so
// a preset whose memory REQUEST sits below that would make hosted workers the first
// thing kubelet evicts under node pressure — the failure is invisible in a version
// with no pod-phase status in the UI.
//
// This bounds the AGENT, not the user's build, which is unmeasured (the e2e repo is
// a single-commit fake and compiles nothing). Do not read a green run here as
// "every preset is big enough".
func TestEveryPresetRequestsMoreMemoryThanTheMeasuredAgentPeak(t *testing.T) {
	peak := resource.MustParse("676Mi")
	for name, s := range sizes {
		if s.MemoryRequest.Cmp(peak) <= 0 {
			t.Errorf("preset %q requests %s, at or below the measured 676 MiB real-run peak: "+
				"kubelet evicts pods exceeding their requests first", name, s.MemoryRequest.String())
		}
	}
}
