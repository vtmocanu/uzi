package agenttmpl

import (
	"strings"
	"testing"
)

func TestBuiltinScratchGuidance(t *testing.T) {
	for _, name := range []string{"coder", "lead", "tester", "reviewer", "auditor", "fact-checker", "web-ux", "ux-designer"} {
		t.Run(name, func(t *testing.T) {
			def, ok := BuiltinByName(name)
			if !ok {
				t.Fatal("missing builtin")
			}
			if !strings.Contains(def.PromptBody, ".uzi/scratch/") {
				t.Error("builtin does not name the run scratch directory")
			}
			for _, stale := range []string{"git worktree add --detach", "./gate-log.XXXXXX", "outside the tracked tree", "it can never be staged", "add the pattern if it is not"} {
				if strings.Contains(def.PromptBody, stale) {
					t.Errorf("builtin retains stale guidance %q", stale)
				}
			}
		})
	}
	for _, name := range []string{"coder", "lead", "tester"} {
		def, _ := BuiltinByName(name)
		if !strings.Contains(def.PromptBody, "mktemp .uzi/scratch/gate-log.XXXXXX") {
			t.Errorf("%s: gate logs must use run scratch", name)
		}
	}
	for _, name := range []string{"reviewer", "auditor", "fact-checker", "tester"} {
		def, _ := BuiltinByName(name)
		for _, want := range []string{"mktemp -d .uzi/scratch/snap.XXXXXX", "set -o pipefail", "git archive \"$sha\" | tar -x -C \"$snap\""} {
			if !strings.Contains(def.PromptBody, want) {
				t.Errorf("%s: missing snapshot instruction %q", name, want)
			}
		}
	}
}
