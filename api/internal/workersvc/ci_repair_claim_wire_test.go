package workersvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ciRepairWireFixture is the golden JSON for a guarded default-branch CI-repair prompt claim
// (PRD #2865). The worker's contract test reads the SAME file, so the guard shape
// {"ci_config_guard": true, "ci_config_paths": [...]} cannot be invented differently by two
// lenient fakes.
const ciRepairWireFixture = "testdata/claim_ci_repair_wire.json"

func ciRepairClaimPayload(t *testing.T, guarded bool) ClaimPayload {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("FORGE-PAT-PLACEHOLDER"))
	sealedTok, _ := box.Seal([]byte("ANTHROPIC-OAUTH-PLACEHOLDER"))
	run := store.Run{
		ID:               uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		RepoID:           pgconv.UUID(uuid.MustParse("22222222-2222-2222-2222-222222222222")),
		Kind:             runkind.Prompt,
		IssueTitle:       "CI repair: nightly-e2e #12",
		IssueDescription: "Diagnose and fix the red default-branch workflow.",
		Status:           "claimed",
		PlanSource:       planSourceAgent,
	}
	if guarded {
		run.CiConfigGuard = true
		run.CiConfigPaths = []string{".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", ".github/workflows/**", ".forgejo/workflows/**"}
	}
	fs := &fakeStore{
		claimRun:           run,
		attributionEnabled: true,
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic: sealedTok,
	}
	svc := New(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker(), nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim: %v (payload=%v)", err, payload)
	}
	return *payload
}

// TestCIRepairClaimWireContract pins the guarded-prompt claim shape against the golden.
func TestCIRepairClaimWireContract(t *testing.T) {
	payload := ciRepairClaimPayload(t, true)
	if !payload.Config.CIConfigGuard || len(payload.Config.CIConfigPaths) != 5 {
		t.Fatalf("a guarded run must carry the guard flag and the floor paths, got guard=%v paths=%v", payload.Config.CIConfigGuard, payload.Config.CIConfigPaths)
	}
	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	path := filepath.FromSlash(ciRepairWireFixture)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: test golden fixture is non-sensitive
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo golden constant
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("guarded CI-repair claim drifted from %s.\n got:\n%s\nwant:\n%s", path, got, want)
	}
}

// TestUnguardedPromptClaimOmitsGuard: an ordinary prompt claim carries neither key, so every
// existing claim stays byte-identical.
func TestUnguardedPromptClaimOmitsGuard(t *testing.T) {
	payload := ciRepairClaimPayload(t, false)
	raw, err := json.Marshal(payload.Config)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["ci_config_guard"]; ok {
		t.Fatal("an unguarded claim must omit ci_config_guard")
	}
	if _, ok := m["ci_config_paths"]; ok {
		t.Fatal("an unguarded claim must omit ci_config_paths")
	}
}
