package workersvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// PRD #4320 module 5: a vendor claim is byte-identical to the pre-provider wire. These goldens were
// captured from the BASE behaviour (the tree before the provider claim branch existed), by running
// this test with UPDATE_GOLDEN=1 against that tree, and are asserted unchanged afterwards: no
// `provider` key anywhere, `anthropic_oauth_token` present as before. The claim is assembled by the
// REAL path (Claim / ClaimChat against Postgres); volatile ids and the token fixtures embed random
// UUIDs, which are normalized to <uuid> so the rest of the bytes are compared exactly.
// Run via ./e2e/run-store-it.sh (skipped without UZI_TEST_DATABASE_URL).

var goldenUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// goldenIID normalizes the issue iid the fixture derives from the run id's first four bytes.
var goldenIID = regexp.MustCompile(`"issue_iid": [0-9]+`)

func normalizeClaimGolden(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	s := goldenUUID.ReplaceAllString(string(raw), "<uuid>")
	s = goldenIID.ReplaceAllString(s, `"issue_iid": 0`)
	return []byte(s + "\n")
}

func checkClaimGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: a test golden under testdata
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: fixed testdata path
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if string(want) != string(got) {
		t.Fatalf("vendor claim wire changed (%s).\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func TestVendorIssueClaimGoldenLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.queuedRun(t, "issue")
	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim = (%v, %v)", payload != nil, err)
	}
	if payload.Secrets.Provider != nil || payload.Secrets.AnthropicOAuthToken == "" {
		t.Fatalf("a vendor claim must carry its token and no provider: %+v", payload.Secrets)
	}
	got := normalizeClaimGolden(t, payload)
	if strings.Contains(string(got), `"provider"`) {
		t.Fatalf("a vendor claim carries a provider key:\n%s", got)
	}
	checkClaimGolden(t, "claim_vendor_issue_wire.json", got)
}

func TestVendorChatClaimGoldenLiveDB(t *testing.T) {
	fx := newCDFix(t)
	chatID := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title, trigger_source, harness, status)
	    VALUES ($1, $2, 'chat', 'hi', 'hi', 'hi', 'chat', 'claude', 'queued')`, chatID, fx.userID)
	payload, err := fx.svc.ClaimChat(fx.env.ctx, fx.worker(t))
	if err != nil || payload == nil {
		t.Fatalf("ClaimChat = (%v, %v)", payload != nil, err)
	}
	if payload.Secrets.Provider != nil || payload.Secrets.AnthropicOAuthToken == "" {
		t.Fatalf("a vendor chat claim must carry its token and no provider: %+v", payload.Secrets)
	}
	got := normalizeClaimGolden(t, payload)
	if strings.Contains(string(got), `"provider"`) {
		t.Fatalf("a vendor chat claim carries a provider key:\n%s", got)
	}
	checkClaimGolden(t, "claim_vendor_chat_wire.json", got)
}
