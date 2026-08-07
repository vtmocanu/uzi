package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// judgeRun is a claimed judge run row as ClaimRun would return it: repo-less,
// issue-less, pointing at the reviewed run via target_run_id.
func judgeRun(userID, targetID uuid.UUID) store.Run {
	return store.Run{
		ID:               uuid.New(),
		UserID:           userID,
		Kind:             RunKindJudge,
		Status:           "claimed",
		TargetRunID:      pgUUID(targetID),
		IssueTitle:       "Judge of run " + targetID.String(),
		IssueDescription: "retrospective",
	}
}

// TestClaimJudgeSkipsRepoJoinAndPAT is the core Decision 1+3 contract: assembling a
// judge claim NEVER queries the repo/forge join (GetRunClaimContext) and NEVER
// decrypts a bot PAT — the claim carries only the Anthropic token and the reviewed
// run's id. A judge with a token but a vanished/absent forge connection still
// claims fine (audit H2).
func TestClaimJudgeSkipsRepoJoinAndPAT(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid := uuid.New()
	target := uuid.New()
	fs := &fakeStore{
		claimRun:  judgeRun(uid, target),
		anthropic: sealedTok,
		// No claimCtx set: if assembly touched GetRunClaimContext it would still
		// return a zero row, so claimCtxCalled is the real assertion below.
	}
	svc := New(fs, box, testParams())

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a judge claim payload for a user with an Anthropic token and no forge connection")
	}
	if fs.claimCtxCalled {
		t.Error("judge assembly must never query the repo/forge join (GetRunClaimContext)")
	}
	if payload.Kind != RunKindJudge {
		t.Errorf("Kind = %q, want judge", payload.Kind)
	}
	if payload.TargetRunID == nil || *payload.TargetRunID != target.String() {
		t.Errorf("TargetRunID = %v, want %s", payload.TargetRunID, target)
	}
	if payload.Secrets.AnthropicOAuthToken != "anthropic-judge-token-abcdef1234567890" {
		t.Error("judge claim must carry the decrypted Anthropic token")
	}
	if payload.Secrets.ForgePAT != "" || payload.Secrets.ForgeUsername != "" {
		t.Errorf("judge claim must carry NO forge credential, got pat=%q user=%q", payload.Secrets.ForgePAT, payload.Secrets.ForgeUsername)
	}
	if payload.Repo.ID != "" || payload.Repo.CloneURL != "" {
		t.Errorf("judge claim must carry no repo, got %+v", payload.Repo)
	}
}

// TestClaimJudgeCarriesKnownImproveUziTargets pins issue #232's write-side fix: a judge
// claim carries the run owner's existing improve_uzi target menu (frequency-ranked,
// canonical, capped) so the judge reuses an exact coordinate instead of inventing new
// phrasing. It asserts the menu is delivered verbatim, that the lookup is owner-scoped and
// capped at judgeKnownTargetsCap, and — the second sub-case — that a user with no
// improve_uzi history yields a nil menu that is OMITTED from the wire (omitempty).
func TestClaimJudgeCarriesKnownImproveUziTargets(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid := uuid.New()

	// ── populated menu ──
	menu := []string{"worker git identity setup", "shellcheck", "flaky e2e retry"}
	fs := &fakeStore{
		claimRun:     judgeRun(uid, uuid.New()),
		anthropic:    sealedTok,
		knownTargets: menu,
	}
	svc := New(fs, box, testParams())

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if got := payload.KnownImproveUziTargets; len(got) != len(menu) {
		t.Fatalf("KnownImproveUziTargets = %v, want %v", got, menu)
	}
	for i := range menu {
		if payload.KnownImproveUziTargets[i] != menu[i] {
			t.Fatalf("KnownImproveUziTargets[%d] = %q, want %q", i, payload.KnownImproveUziTargets[i], menu[i])
		}
	}
	// The lookup must be owner-scoped (the claimed run's owner) and capped.
	if fs.knownTargetsParams == nil {
		t.Fatal("ListKnownImproveUziTargetsForUser was never called")
	}
	if fs.knownTargetsParams.UserID != uid {
		t.Errorf("menu lookup user = %s, want the run owner %s", fs.knownTargetsParams.UserID, uid)
	}
	if fs.knownTargetsParams.Lim != judgeKnownTargetsCap {
		t.Errorf("menu lookup lim = %d, want the cap %d", fs.knownTargetsParams.Lim, judgeKnownTargetsCap)
	}
	// It rides the wire under the agreed field name.
	if raw, _ := json.Marshal(payload); !strings.Contains(string(raw), `"known_improve_uzi_targets"`) {
		t.Errorf("populated menu must appear on the wire, got: %s", raw)
	}

	// ── empty menu is omitted from the wire ──
	fsEmpty := &fakeStore{claimRun: judgeRun(uid, uuid.New()), anthropic: sealedTok}
	svcEmpty := New(fsEmpty, box, testParams())
	empty, err := svcEmpty.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || empty == nil {
		t.Fatalf("Claim (empty): payload=%v err=%v", empty, err)
	}
	if empty.KnownImproveUziTargets != nil {
		t.Errorf("no improve_uzi history must yield a nil menu, got %v", empty.KnownImproveUziTargets)
	}
	if raw, _ := json.Marshal(empty); strings.Contains(string(raw), "known_improve_uzi_targets") {
		t.Errorf("an empty menu must be omitted from the wire (omitempty), got: %s", raw)
	}
}

// TestClaimJudgeSurvivesKnownTargetsLookupError pins the best-effort posture (issue #232):
// the menu is an optimization, so a lookup failure must WARN and proceed with a nil menu,
// never fail the claim — mirroring judgeSignal. Without this a transient DB hiccup on an
// optional read would abort an otherwise-fine judge claim.
func TestClaimJudgeSurvivesKnownTargetsLookupError(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid := uuid.New()
	fs := &fakeStore{
		claimRun:        judgeRun(uid, uuid.New()),
		anthropic:       sealedTok,
		knownTargetsErr: errors.New("db down"),
	}
	svc := New(fs, box, testParams())

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("a known-targets lookup error must NOT fail the claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a judge claim payload despite the menu lookup error")
	}
	if payload.KnownImproveUziTargets != nil {
		t.Errorf("a failed menu lookup must leave the menu nil, got %v", payload.KnownImproveUziTargets)
	}
}

// TestClaimJudgeWireCarriesNoPATValue marshals a real assembled judge claim and
// asserts the wire contains an EMPTY forge_pat (the judge rides the ordinary
// ClaimPayload, so the key is present but never a credential) while the Anthropic
// token IS present — the "no forge_pat present" wire assertion of Decision 1.
func TestClaimJudgeWireCarriesNoPATValue(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-nopat-abcdef1234567890"))
	uid := uuid.New()
	fs := &fakeStore{claimRun: judgeRun(uid, uuid.New()), anthropic: sealedTok}
	svc := New(fs, box, testParams())

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(raw)
	// The secret bytes must never be on the wire.
	if strings.Contains(js, "bot-pat") {
		t.Fatalf("judge wire leaked a bot PAT: %s", js)
	}
	if !strings.Contains(js, `"forge_pat":""`) {
		t.Fatalf("judge wire must carry an empty forge_pat, got: %s", js)
	}
	if !strings.Contains(js, "anthropic_oauth_token") {
		t.Fatalf("judge wire must carry the anthropic token, got: %s", js)
	}
}
