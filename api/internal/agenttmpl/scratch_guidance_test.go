package agenttmpl

import (
	"strings"
	"testing"
)

// staleScratchGuidance lists phrases that contradict a uzi worker (PRD #1719): a
// nested worktree it refuses, a gate log outside run scratch, a tracked .gitignore
// edit, and the false claim that an ignored file cannot be staged. Every builtin
// body must be free of them. The upstream role library keeps its bodies free of
// them too (PRD #1849 M1), which is what lets the builtins copy it verbatim.
var staleScratchGuidance = []string{
	"git worktree add --detach",
	"./gate-log.XXXXXX",
	"outside the tracked tree",
	"it can never be staged",
	"add the pattern if it is not",
}

// flattenWS collapses every whitespace run to one space, so a phrase wrapped
// across lines in a body is still found.
func flattenWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestBuiltinsCarryNoStaleScratchGuidance(t *testing.T) {
	for _, def := range Builtins() {
		t.Run(def.Name, func(t *testing.T) {
			body := flattenWS(def.PromptBody)
			for _, stale := range staleScratchGuidance {
				if strings.Contains(body, stale) {
					t.Errorf("builtin retains guidance a uzi worker contradicts: %q", stale)
				}
			}
		})
	}
}

// The uzi runtime facts (scratch path, gate-log and snapshot recipe) reach every
// subagent through the worker append (agent/src/prompt.ts RUN_SCRATCH_GUIDANCE;
// pinned in agent/test/agents.test.ts and codex-render.test.ts). The lead body is
// uzi-only and still names the path itself.
func TestLeadNamesRunScratch(t *testing.T) {
	def, ok := BuiltinByName("lead")
	if !ok {
		t.Fatal("missing lead builtin")
	}
	for _, want := range []string{".uzi/scratch/", "mktemp .uzi/scratch/gate-log.XXXXXX"} {
		if !strings.Contains(def.PromptBody, want) {
			t.Errorf("lead: missing %q", want)
		}
	}
}
