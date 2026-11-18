package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func providerRun(name *string, usage *apitypes.UsageDTO) apitypes.RunDTO {
	id := "11111111-1111-4111-8111-111111111111"
	return apitypes.RunDTO{
		ID: "run-detail", Kind: "issue", Status: "completed", Harness: "claude", IssueTitle: "t",
		ProviderID: &id, ProviderName: name, Usage: usage,
	}
}

func unreportedUsage() *apitypes.UsageDTO {
	return &apitypes.UsageDTO{CostStatus: "unreported", InputTokens: 300, OutputTokens: 200}
}

// `uzi run get` names the provider (as plain text) instead of a token, and reads the cost as
// unknown for the custom provider rather than as $0 or the generic "unavailable".
func TestRunGetProviderRun(t *testing.T) {
	name := "corp " + hostileANSI + hostileBidi
	out := renderDetail(t, providerRun(&name, unreportedUsage()))
	if !strings.Contains(out, "PROVIDER") || !strings.Contains(out, "corp") {
		t.Errorf("provider row missing:\n%s", out)
	}
	if strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("a provider run must not render a token row:\n%s", out)
	}
	if !strings.Contains(out, "cost unknown (custom provider)") || strings.Contains(out, "$") || strings.Contains(out, "cost unavailable") {
		t.Errorf("cost must read unknown (custom provider), never a dollar figure:\n%s", out)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, '\u202e') {
		t.Errorf("provider name reached the terminal raw:\n%q", out)
	}

	// The provider was deleted: the id survives, the name is null.
	out = renderDetail(t, providerRun(nil, unreportedUsage()))
	if !strings.Contains(out, "PROVIDER") || !strings.Contains(out, "(deleted)") {
		t.Errorf("deleted provider not shown:\n%s", out)
	}

	// A vendor run is unchanged: no provider row, the generic unavailable wording.
	vendor := providerRun(nil, unreportedUsage())
	vendor.ProviderID = nil
	out = renderDetail(t, vendor)
	if strings.Contains(out, "PROVIDER") || strings.Contains(out, "custom provider") || !strings.Contains(out, "cost unavailable") {
		t.Errorf("vendor unreported run changed:\n%s", out)
	}
}

func TestTUIProviderRunCost(t *testing.T) {
	name := "corp"
	run := providerRun(&name, unreportedUsage())
	run.Status = "running"

	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	m.width, m.height = 100, 40
	m = applyDetail(m, run, nil)
	spend := stripANSI(m.renderSpend(0))
	if !strings.Contains(spend, "Unknown") || !strings.Contains(spend, "custom provider") || strings.Contains(spend, "API-equivalent") || strings.Contains(spend, "$") {
		t.Errorf("spend block: %q", spend)
	}
	header := stripANSI(strings.Join(m.detailHeaderLines(), "\n"))
	if !strings.Contains(header, "unknown") || strings.Contains(header, "$") {
		t.Errorf("header: %q", header)
	}

	// Board: the credential column names the provider, the cost cell is never a dollar figure or blank.
	board := tuiTestModel(t, &uzicli.FakeClient{}, "")
	item := apitypes.RunListItemDTO{RunDTO: run}
	if got := board.boardCredLabel(item); got != "corp" {
		t.Errorf("credential column = %q, want the provider name", got)
	}
	if s := strings.TrimSpace(stripANSI(board.boardCostSeg(item, nil, boardCostWidth))); s != "n/a" {
		t.Errorf("board cost = %q, want n/a", s)
	}
	item.ProviderName = nil
	if got := board.boardCredLabel(item); got != "(deleted)" {
		t.Errorf("deleted provider credential column = %q", got)
	}

	// A hostile name never reaches the board raw.
	hostile := hostileANSI + hostileBidi
	item.ProviderName = &hostile
	if got := board.boardCredLabel(item); strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, '\u202e') {
		t.Errorf("hostile provider name reached the board raw: %q", got)
	}

	// A vendor run in the same model keeps the Anthropic wording.
	vendor := run
	vendor.ProviderID = nil
	mv := tuiTestModel(t, &uzicli.FakeClient{}, "run-detail")
	mv.width, mv.height = 100, 40
	mv = applyDetail(mv, vendor, nil)
	if s := stripANSI(mv.renderSpend(0)); !strings.Contains(s, "Unavailable") || !strings.Contains(s, "API-equivalent") {
		t.Errorf("vendor spend changed: %q", s)
	}
}
