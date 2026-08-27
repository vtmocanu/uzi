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
	raw, err := os.ReadFile(path)
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
