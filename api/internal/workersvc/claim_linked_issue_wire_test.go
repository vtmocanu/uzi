package workersvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// linkedIssueWireFixture is the server side of the cross-side contract for a linked
// repo's issue run (PRD #4173): agent/test/claim-issue-key-contract.test.ts parses the
// same file, so the worker's idea of issue_key cannot drift from the server's.
const linkedIssueWireFixture = "testdata/claim_linked_issue_wire.json"

// issueClaimPayload runs a real Claim of an issue run on a repo whose link carries
// projectShort (NULL when unlinked), so the golden reflects the server's actual assembly.
func issueClaimPayload(t *testing.T, projectShort pgtype.Text) ClaimPayload {
	return claimPayloadOfKind(t, runkind.Issue, projectShort)
}

// claimPayloadOfKind is issueClaimPayload for any run kind. Every kind gets a valid iid, so a
// non-issue kind is kept from reading as linked by its kind and not by a missing iid.
func claimPayloadOfKind(t *testing.T, kind string, projectShort pgtype.Text) ClaimPayload {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("FORGE-PAT-PLACEHOLDER"))
	sealedTok, _ := box.Seal([]byte("ANTHROPIC-OAUTH-PLACEHOLDER"))
	iid := pgtype.Int8{Int64: 12, Valid: true}
	fs := &fakeStore{
		claimRun: store.Run{
			ID:               uuid.MustParse("33333333-3333-3333-3333-333333333333"),
			RepoID:           pgconv.UUID(uuid.MustParse("22222222-2222-2222-2222-222222222222")),
			Kind:             kind,
			IssueIid:         iid,
			IssueTitle:       "Add the thing",
			IssueDescription: "A card from the issue tracker.",
			Status:           "claimed",
			PlanSource:       planSourceAgent,
		},
		attributionEnabled: true,
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
			IssueTrackerProjectShort: projectShort,
		},
		anthropic: sealedTok,
	}
	svc := newOffModeService(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker(), nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim: %v (payload=%v)", err, payload)
	}
	return *payload
}

func TestLinkedIssueClaimWireContract(t *testing.T) {
	payload := issueClaimPayload(t, pgtype.Text{String: "ABC", Valid: true})
	if payload.IssueKey != "ABC-12" || payload.IssueIID == nil || *payload.IssueIID != 12 {
		t.Fatalf("issue_key = %q iid = %v, want ABC-12 and 12", payload.IssueKey, payload.IssueIID)
	}

	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	path := filepath.FromSlash(linkedIssueWireFixture)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: test golden fixture is non-sensitive and 0644 keeps it readable in the repo
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: path is a fixed in-repo golden constant, not attacker-controlled
	if err != nil {
		t.Fatalf("read golden (run `UPDATE_GOLDEN=1 go test` to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("linked issue claim wire shape drifted from %s.\nIf intended, regenerate with UPDATE_GOLDEN=1 and update the worker-side contract test to match.\n--- got ---\n%s", linkedIssueWireFixture, got)
	}
}

// issue_key is omitted, never null and never "", for every claim that is not an issue run
// on a linked repo: an unlinked issue run, a linked repo's non-issue run, and a link whose
// short name is blank.
func TestIssueKeyIsOmittedFromEveryUnlinkedClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		short pgtype.Text
		kind  string
	}{
		"unlinked issue run":       {short: pgtype.Text{}, kind: runkind.Issue},
		"blank short name":         {short: pgtype.Text{String: "", Valid: true}, kind: runkind.Issue},
		"linked repo's ci_fix run": {short: pgtype.Text{String: "ABC", Valid: true}, kind: runkind.CIFix},
	} {
		t.Run(name, func(t *testing.T) {
			payload := claimPayloadOfKind(t, tc.kind, tc.short)
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatal(err)
			}
			if v, present := wire["issue_key"]; present {
				t.Fatalf("issue_key present on the wire: %s", v)
			}
		})
	}
}
