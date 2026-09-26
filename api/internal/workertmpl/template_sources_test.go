package workertmpl

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Issue #141: the set of worker templates is written out by hand in several
// places and read automatically from one. The failure the issue describes is a
// silent drift — add a template dir and forget a list, and either the api accepts
// a worker the controller can never render (already gated, see below) or the image
// is never built and the worker lands ImagePullBackOff (NOT gated before this file).
//
// This test makes agent/templates/*/ the single source of truth and asserts every
// hand-written copy agrees with it:
//
//   - Names (this package)                       — TestNamesMatchesTemplateDirs
//   - .github/workflows/ci.yml     build matrix  — TestWorkflowMatricesMatchNames
//   - .github/workflows/release.yml build matrix — TestWorkflowMatricesMatchNames
//   - release.yml assert-worker-pin loop         — TestReleaseScriptWorkerListsMatchNames
//   - release-verify.sh guard-4 loop             — TestReleaseScriptWorkerListsMatchNames
//
// The last two are the hand-written worker lists in the release-train's PUBLISHED-pin
// guards (issue: an unpublished worker image, 2026-09-19). A template in Names but absent
// from one of those loops is never checked for a published image, so the guard silently
// misses it — the same drift class this file exists to catch.
//
// The controller's template→image map is covered transitively via PRD #58's
// cross-module golden api/internal/hostedsvc/testdata/hosted_templates.json: its
// producer api/internal/hostedsvc/template_contract_test.go pins Names into the
// golden, and its consumer controller/internal/preset/preset_contract_test.go
// checks the controller's template→image map covers every name in it. This file
// ties the directory to Names; that pair ties Names to the controller map.
//
// It only READS the workflow files. Deriving the CI matrix from the directory at
// runtime would edit the workflows, which a uzi worker's PAT (no `workflow` scope)
// cannot push; a real drift is a deliberate one-line human edit the CI then gates.
//
// LOCAL RUNS NEED -count=1. Every file this test reads lives OUTSIDE the api module
// root (agent/templates/, .github/workflows/*), so Go's test cache is blind to
// drift in them: `go test ./internal/workertmpl` can serve a cached green after the
// very drift this guards. CI's test:api target already passes -count=1 (the same
// cross-module-cache hazard .claude/rules/go.md documents); pass it locally too.

// repoRoot is three levels up from this package (api/internal/workertmpl); go test
// runs with the package dir as cwd.
const repoRoot = "../../.."

// templateDirs is the ground truth: every directory under agent/templates/ that
// carries a Dockerfile, sorted. agent/test/templates-guardrails.test.ts enforces
// the stronger contract that EVERY directory there has a Dockerfile, so in practice
// the Dockerfile filter here only excludes non-template files (e.g. entrypoint.sh);
// it is kept as defence so a stray Dockerfile-less dir cannot make this test demand
// a matching entry in Names.
func templateDirs(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot, "agent", "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "Dockerfile")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		// An empty set would make every comparison below vacuously pass — the
		// "test that cannot fail" the PRD #58 goldens keep guarding against.
		t.Fatalf("found no template directories under %s", dir)
	}
	sort.Strings(names)
	return names
}

func sortedNames() []string {
	out := slices.Clone(Names)
	sort.Strings(out)
	return out
}

func TestNamesMatchesTemplateDirs(t *testing.T) {
	dirs := templateDirs(t)
	got := sortedNames()
	if !slices.Equal(got, dirs) {
		t.Fatalf("workertmpl.Names drifted from agent/templates/*/.\n"+
			"Names (sorted): %v\ntemplate dirs:  %v\n"+
			"Add or remove the entry in Names to match the directories (and regenerate the "+
			"PRD #58 golden: UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...).", got, dirs)
	}
}

// templateMatrixRe matches an inline (flow-style) matrix list, e.g.
//
//	template: [base, jvm]
//
// which is the form both workflows use. Block style (a `- base` list) would not
// match; TestWorkflowMatricesMatchNames fails loudly on zero matches rather than
// passing, so a reformat that this cannot read is caught, not silently ignored.
var templateMatrixRe = regexp.MustCompile(`(?m)^\s*template:\s*\[([^\]]*)\]\s*$`)

// matrixTemplates returns every flow-style `template: [...]` list found in the
// file, each sorted. Empty slice means none were found.
func matrixTemplates(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reads a fixed repo-relative workflow/script path in a test, not user input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out [][]string
	for _, m := range templateMatrixRe.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, parseFlowList(m[1]))
	}
	return out
}

// parseFlowList splits a flow-style list body ("base, jvm") into sorted, trimmed
// elements. Interior empties are NOT dropped: a malformed list like `[base,,jvm]`
// keeps the empty element so it fails the equality check (an empty-named template
// would build agent/templates//Dockerfile). Only a wholly empty body ("[]") yields
// no elements.
func parseFlowList(body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	items := strings.Split(body, ",")
	for i := range items {
		items[i] = strings.Trim(items[i], " \t\"'")
	}
	sort.Strings(items)
	return items
}

func TestWorkflowMatricesMatchNames(t *testing.T) {
	want := sortedNames()
	for _, wf := range []string{
		filepath.Join(repoRoot, ".github", "workflows", "ci.yml"),
		filepath.Join(repoRoot, ".github", "workflows", "release.yml"),
	} {
		matrices := matrixTemplates(t, wf)
		if len(matrices) == 0 {
			t.Fatalf("%s: found no `template: [...]` build matrix. Either the per-template "+
				"image build was removed (drop this assertion) or the matrix was reformatted to "+
				"a form this guard cannot read — do not let it pass silently.", wf)
		}
		for _, got := range matrices {
			if !slices.Equal(got, want) {
				t.Fatalf("%s: build matrix drifted from workertmpl.Names.\n"+
					"matrix (sorted): %v\nNames (sorted):  %v\n"+
					"A template in Names but not the matrix is never built: its image is missing "+
					"and the worker lands ImagePullBackOff. Update the matrix to match.", wf, got, want)
			}
		}
	}
}

// workerListRe matches a shell `for <var> in <bare words>; do` loop carrying a trailing
// `# ...workertmpl...` marker, e.g.
//
//	for t in base jvm; do  # worker templates; workertmpl.Names (pinned by ...)
//	for img in agent-base agent-jvm; do  # worker images agent-<name>; workertmpl.Names ...
//
// The marker is the anchor: it singles these worker lists out from the OTHER bare-word
// `for` loops in the same files (release-verify.sh's own `for img in api web controller
// agent-base agent-jvm` images loop, release.yml's `for t in "${VERSION}" ...` tag-verify
// loop — the latter's tokens are not bare words anyway), so a template rename that forgets
// one of these lists reddens here, the guarantee TestWorkflowMatricesMatchNames gives the
// build matrix. Bare-word body only (`[A-Za-z0-9 _-]`), so a `${...}` list never matches.
var workerListRe = regexp.MustCompile(`(?m)^[ \t]*for[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+in[ \t]+([A-Za-z0-9 _-]+?)[ \t]*;[ \t]*do\b[^\n]*#[^\n]*workertmpl`)

// markedForLists returns every marked `for ... in ...; do` word list in the file, each
// sorted. Empty slice means none were found.
func markedForLists(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reads a fixed repo-relative workflow/script path in a test, not user input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out [][]string
	for _, m := range workerListRe.FindAllStringSubmatch(string(raw), -1) {
		fields := strings.Fields(m[1])
		sort.Strings(fields)
		out = append(out, fields)
	}
	return out
}

// TestReleaseScriptWorkerListsMatchNames pins the release-train's PUBLISHED-pin guard
// lists to workertmpl.Names: release.yml's assert-worker-pin loop (template short names)
// and release-verify.sh's guard-4 loop (agent-<name> image repos).
func TestReleaseScriptWorkerListsMatchNames(t *testing.T) {
	names := sortedNames()
	agentNames := make([]string, len(names))
	for i, n := range names {
		agentNames[i] = "agent-" + n
	}
	sort.Strings(agentNames)

	cases := []struct {
		path string
		want []string
	}{
		{filepath.Join(repoRoot, ".github", "workflows", "release.yml"), names},
		{filepath.Join(repoRoot, ".agents", "skills", "uzi-release", "scripts", "release-verify.sh"), agentNames},
	}
	for _, c := range cases {
		lists := markedForLists(t, c.path)
		if len(lists) == 0 {
			t.Fatalf("%s: found no marked `for ... in ...; do  # ...workertmpl...` worker list. "+
				"Either the worker-pin guard was removed (drop this assertion) or its loop lost the "+
				"marker comment this guard keys on — do not let it pass silently.", c.path)
		}
		for _, got := range lists {
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s: worker list drifted from workertmpl.Names.\n"+
					"list (sorted): %v\nwant (sorted): %v\n"+
					"A template in Names but not this list is never checked for a published image: "+
					"the unpublished-worker guard would miss it. Update the list to match.", c.path, got, c.want)
			}
		}
	}
}
