package termsafe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeTTYSplitSecretFixture pins the display half of fixtures/split-secret-redaction: the
// worker redacts run-owned secrets split by invisible separators (agent/test/redact.test.ts pins
// that half on the same cases), and this proves against the PRODUCTION renderer that the raw split
// value would re-join on screen while the worker's redacted value never displays the secret.
//
// fixtures/ sits above the api module, so a fixture-only edit moves no Go cache key; the gate's
// -count=1 is what re-runs this.
func TestSanitizeTTYSplitSecretFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "split-secret-redaction", "cases.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		Secret string `json:"secret"`
		Cases  []struct {
			Name         string `json:"name"`
			RejoinsOnTTY bool   `json:"rejoins_on_tty"`
			Input        string `json:"input"`
			Redacted     string `json:"redacted"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if fx.Secret == "" || len(fx.Cases) == 0 {
		t.Fatal("fixture has no secret or no cases")
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if got := strings.Contains(SanitizeTTY(c.Input), fx.Secret); got != c.RejoinsOnTTY {
				t.Errorf("SanitizeTTY(input) displays the secret = %v, fixture says %v", got, c.RejoinsOnTTY)
			}
			if strings.Contains(SanitizeTTY(c.Redacted), fx.Secret) {
				t.Errorf("the worker-redacted value displays the secret: %q", SanitizeTTY(c.Redacted))
			}
		})
	}
}
