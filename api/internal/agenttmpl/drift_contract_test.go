package agenttmpl_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// The agent-template drift-predicate cross-language contract (issue #223 item 3).
// This is the GO HALF; the TS half is web/src/lib/agentTemplateDriftContract.test.ts.
// Neither reads the other: each folds the SAME (shipped, stored) case table with its
// OWN production drift predicate and compares against the SAME recorded expectation,
// so a failure names the side that drifted. fixtures/run-usage/ (issue #195) is the
// in-repo precedent for the shape.
//
// WHAT THIS PINS. "Has this row drifted from the definition this binary ships?" has
// three implementations and compare.go's own doc comment names their divergence as the
// hazard it exists to prevent: agenttmpl.SameContent (this half), mockApi.ts's
// sameContent and agentTemplates.ts's driftedColumns (the TS half). No divergence can be
// CONSTRUCTED today -- all three fold null/"" and null/[] identically, compare tools
// order-sensitively and never trim -- which is exactly why the agreement is pinned to a
// shared artifact before a fourth consumer makes a future divergence consequential.
//
// The Go half has no column notion, so it reads `differs` only: SameContent(shipped,
// stored) must equal !differs. The `columns` field pins driftedColumns on the TS side.
//
// 🔴 RUN THIS PACKAGE WITH -count=1 AFTER ANY FIXTURE-ONLY EDIT. driftFixture points
// ABOVE api/, so every byte of the fixture is outside this module and contributes
// NOTHING to this package's cache key -- cmd/go's own rule is "Do not recheck files
// outside the module, GOPATH, or GOROOT root". A gutted fixture therefore prints
// "ok (cached)". The vitest half has no such cache and needs no flag, so the two halves
// are NOT symmetric. task test:api / gate:api already carry -count=1.
const driftFixture = "../../../fixtures/agent-template-drift/cases.json"

type driftContent struct {
	Description string   `json:"description"`
	Model       string   `json:"model"`
	Tools       []string `json:"tools"`
	PromptBody  string   `json:"prompt_body"`
}

func (c driftContent) definition() agenttmpl.Definition {
	// Name is left zero: SameContent ignores it (it is the lookup key), and a JSON
	// null model decodes to "" -- the same inherit sentinel the shipped side uses.
	return agenttmpl.Definition{
		Description: c.Description,
		Model:       c.Model,
		Tools:       c.Tools,
		PromptBody:  c.PromptBody,
	}
}

type driftExpected struct {
	Differs bool     `json:"differs"`
	Columns []string `json:"columns"`
}

type driftCase struct {
	Name     string        `json:"name"`
	Shipped  driftContent  `json:"shipped"`
	Stored   driftContent  `json:"stored"`
	Expected driftExpected `json:"expected"`
}

type driftTable struct {
	Note  string      `json:"note"`
	Cases []driftCase `json:"cases"`
}

func loadDriftTable(t *testing.T) driftTable {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(driftFixture))
	if err != nil {
		// A missing or unreadable fixture is a FATAL, never a skip: this contract
		// asserts nothing without it and a skip would look identical to a pass.
		t.Fatalf("drift fixture unreadable: %v", err)
	}
	var table driftTable
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatalf("drift fixture is not valid JSON: %v", err)
	}
	return table
}

func TestSameContentAgreesWithDriftFixture(t *testing.T) {
	table := loadDriftTable(t)
	if len(table.Cases) == 0 {
		t.Fatal("drift fixture holds no cases -- it asserts nothing")
	}
	for _, c := range table.Cases {
		t.Run(c.Name, func(t *testing.T) {
			same := agenttmpl.SameContent(c.Shipped.definition(), c.Stored.definition())
			if same == c.Expected.Differs {
				t.Errorf("SameContent=%v but fixture says differs=%v", same, c.Expected.Differs)
			}
		})
	}
}

// TestDriftFixtureIsWellFormed guards the fixture itself so a "tidied" edit fatals
// instead of quietly weakening the contract: every case's boolean `differs` must match
// its `columns` list being non-empty, and the load-bearing discriminating cases (which
// are the whole point -- an implementation that sorted tools, trimmed a field or treated
// null distinctly from empty would redden exactly these) must remain present.
func TestDriftFixtureIsWellFormed(t *testing.T) {
	table := loadDriftTable(t)
	names := map[string]bool{}
	for _, c := range table.Cases {
		names[c.Name] = true
		if c.Expected.Differs != (len(c.Expected.Columns) > 0) {
			t.Errorf("case %q: differs=%v but columns=%v -- the two must agree",
				c.Name, c.Expected.Differs, c.Expected.Columns)
		}
	}
	for _, required := range []string{
		"identical",
		"description-trailing-space-not-trimmed",
		"model-null-vs-empty-agree",
		"model-whitespace-not-trimmed",
		"tools-reorder",
		"tools-null-vs-empty-agree",
		"prompt-body-trailing-newline-not-trimmed",
		"all-four-differ-in-display-order",
	} {
		if !names[required] {
			t.Errorf("load-bearing case %q is missing -- do not tidy the fixture", required)
		}
	}
}
