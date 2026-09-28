package agenttmpl

import (
	"strings"
	"testing"
)

// The upstream publisher double-quotes tester's description (it contains ": ").
// It used to be stored with the literal quotes, which the Agents UI showed and
// which made an agent-source sync of the same file read as a change.
func TestBuiltinDescriptionsCarryNoYAMLQuotes(t *testing.T) {
	for _, d := range Builtins() {
		if strings.HasPrefix(d.Description, `"`) || strings.HasPrefix(d.Description, "'") {
			t.Errorf("%s: description keeps its YAML quotes: %q", d.Name, d.Description)
		}
	}
	tester, _ := BuiltinByName("tester")
	if !strings.Contains(tester.Description, ": ") {
		t.Fatalf("fixture: tester's description no longer needs quoting (%q); pick another quoted builtin", tester.Description)
	}
}

func TestQuoteScalarRoundTrip(t *testing.T) {
	for _, v := range []string{"plain words", "has: colon space", "ends with:", "- leading dash", "tab #comment", `"quoted"`} {
		if got := unquoteScalar(quoteScalar(v)); got != v && v != `"quoted"` {
			t.Errorf("round trip of %q gave %q", v, got)
		}
	}
	if quoteScalar("plain words") != "plain words" {
		t.Error("a plain value must stay bare")
	}
}
