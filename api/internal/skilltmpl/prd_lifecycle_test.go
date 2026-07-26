package skilltmpl_test

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/skilltmpl"
)

const prdLifecycle = "prd-lifecycle"

func prdLifecycleDef(t *testing.T) skilltmpl.Definition {
	t.Helper()
	def, ok := skilltmpl.BuiltinByName(prdLifecycle)
	if !ok {
		t.Fatalf("%s is not a shipped builtin skill", prdLifecycle)
	}
	return def
}

// The skill must parse and ship. The init-time check in allocations.go already
// makes a map entry without a SKILL.md a boot panic; this is the other direction.
func TestPRDLifecycleBuiltinShips(t *testing.T) {
	def := prdLifecycleDef(t)
	if strings.TrimSpace(def.Description) == "" {
		t.Error("description is what the model routes on; it must not be empty")
	}
	if strings.ContainsAny(def.Description, "\r\n") {
		t.Error("description must be a single line")
	}
	if strings.TrimSpace(def.Body) == "" {
		t.Error("empty body")
	}
}

// It must carry a default allocation, or it reaches nobody — an unallocated
// builtin is not in the run union at all, which is the gap M2 closed.
func TestPRDLifecycleIsDefaultAllocated(t *testing.T) {
	targets := skilltmpl.DefaultAllocationsFor(prdLifecycle)
	if len(targets) == 0 {
		t.Fatal("prd-lifecycle has no default allocation; it would reach no agent")
	}
	want := map[string]bool{"lead": true, "reviewer": true}
	for _, tmpl := range targets {
		if !want[tmpl] {
			t.Errorf("unexpected default target %q", tmpl)
		}
		delete(want, tmpl)
	}
	for missing := range want {
		t.Errorf("expected %q among the default targets; got %v", missing, targets)
	}
}

// TestPRDLifecycleBodyCreatesDoneDirBeforeMoving is a CHANGE DETECTOR, and
// deliberately so.
//
// READ THIS BEFORE "FIXING" A FAILURE HERE. If you reddened this by tidying the
// skill body, you have not broken a string test — you have re-armed a known
// exit-128. `git mv prds/x.md prds/done/x.md` fails with
//
//	fatal: renaming 'prds/x.md' failed: No such file or directory
//
// when `prds/done/` does not exist, and git does not track empty directories, so
// it does not exist in ANY repo that has never archived a PRD. That is first use
// in every such repo, and it fires at the very END of a run, after all the work.
// It is the headline behaviour of this whole PRD failing on its first outing.
// Reproduced in a throwaway repo: bare `git mv` exits 128, `mkdir -p` then
// `git mv` exits 0.
//
// A model reads prose, not assertions, so no stronger mechanical check exists
// here — the real proof is the mandated manual run. This pins that the instruction
// is at least PRESENT.
func TestPRDLifecycleBodyCreatesDoneDirBeforeMoving(t *testing.T) {
	body := prdLifecycleDef(t).Body
	mkdir := strings.Index(body, "mkdir -p prds/done")
	if mkdir < 0 {
		t.Fatal("the skill body must tell the agent to `mkdir -p prds/done` before `git mv`; " +
			"without it the move exits 128 on first use in every repo that has never archived a PRD")
	}
	mv := strings.Index(body, "git mv")
	if mv < 0 {
		t.Fatal("the skill body must show the `git mv` that performs the move")
	}
	if mkdir > mv {
		t.Error("`mkdir -p prds/done` must come BEFORE `git mv`; after it is exactly the exit-128 case")
	}
}

// The judgment Decision 4 says to keep from prd-update-progress, and the rules
// Decisions 2 and 3 add. Named individually so a failure says which one went.
func TestPRDLifecycleBodyCarriesTheLoadBearingRules(t *testing.T) {
	body := prdLifecycleDef(t).Body
	for _, c := range []struct{ what, needle string }{
		{"Decision 4: scan every unchecked item", "- [ ]"},
		{"Decision 4: evidence-based completion", "direct evidence"},
		{"Decision 2: partial completion does not move the file", "leave the file where it is"},
		{"Decision 2: already under done/ is a no-op", "Already under `prds/done/`"},
		{"Decision 3: the reviewer checks the PRD diff", "Check the PRD diff"},
		{"Decision 3: state honestly that this is prompt-level", "prompt-level instruction"},
		{"Decision 5: no linked PRD is a no-op", "no-op"},
		{"M4: declare the moved path", "prd_done_path"},
	} {
		if !strings.Contains(body, c.needle) {
			t.Errorf("%s: expected the body to contain %q", c.what, c.needle)
		}
	}
}

// Stripped per Decision 4: these belong to the human loop or duplicate machinery
// uzi already owns, and carrying them would fight the run's own workflow.
func TestPRDLifecycleBodyOmitsTheStrippedSteps(t *testing.T) {
	body := prdLifecycleDef(t).Body
	for _, c := range []struct{ what, needle string }{
		{"git-log archaeology (the agent just did the work)", "git log"},
		{"the /prd-next handoff", "prd-next"},
		{"stage-the-world (the run commits its own change set)", "git add ."},
		{"a second human gate (uzi has exactly one, the plan gate)", "wait for user"},
	} {
		if strings.Contains(body, c.needle) {
			t.Errorf("%s: the body must not contain %q (Decision 4 strips it)", c.what, c.needle)
		}
	}
}
