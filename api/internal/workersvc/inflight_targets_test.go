package workersvc

// issue #297 M3: regression fixture (2026-08-10) for the self_improve in-flight
// avoid-set that M1 attached to the claim payload. It pins two things a green build
// alone does not: (1) that a self_improve claim reads ListActiveRunsAll and carries
// one coordinate line per active run on the SAME repo, excluding itself and other
// repos, and (2) that an empty set stays OFF the wire (omitempty). The active-runs
// table is an in-code fake (fakeStore.activeRunsAll), so there is no cross-module
// -count=1 concern — the rows are constructed per test, not cached anywhere.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// selfImproveClaimFixture builds the fakeStore + worker + box scaffold a self_improve
// Claim needs to SUCCEED, modelled on TestSelfImproveClaimFollowsJudgeBinding. The
// caller sets fs.claimRun.RepoID and fs.activeRunsAll before invoking svc.Claim.
func selfImproveClaimFixture(t *testing.T, selfRunID uuid.UUID, repoID uuid.UUID) (*fakeStore, *Service, store.Worker) {
	t.Helper()

	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-INFLIGHT-abcdef1234567890"))
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-inflight-abcde"))
	sealedJudge, _ := box.Seal([]byte("anthropic-JUDGE-inflight-abcdefgh"))

	owner := uuid.New()
	judgeID, workerID := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: selfRunID, UserID: owner, Kind: runkind.SelfImprove, Status: "claimed",
			RepoID:   pgconv.UUID(repoID),
			IssueIid: pgtype.Int8{Int64: 1, Valid: true}, IssueTitle: "improve uzi", IssueDescription: "d",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/uzi", RepoPath: "g/uzi",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:     sealedDefault,
		judgeSecret:   pgtype.UUID{Bytes: judgeID, Valid: true},
		judgeBindMode: BindModePinned,
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			judgeID:  {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedJudge, SealedWith: store.SealedWithMaster},
			workerID: {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedDefault, SealedWith: store.SealedWithMaster},
		},
	}
	svc := New(fs, box, testParams())
	wkr := store.Worker{
		ID: uuid.New(), UserID: owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: workerID, Valid: true},
	}
	return fs, svc, wkr
}

func mustMilestonesJSON(t *testing.T, ms ...apitypes.Milestone) []byte {
	t.Helper()
	b, err := json.Marshal(ms)
	if err != nil {
		t.Fatalf("marshal milestones: %v", err)
	}
	return b
}

// TestSelfImproveClaimCarriesInflightTargets pins that a self_improve claim carries one
// coordinate line per active run on the SAME repo — excluding the self_improve run itself
// and runs on any OTHER repo — and that the assembled line surfaces the issue iid and the
// frozen milestone title so the picker can steer clear of overlapping work (issue #297).
func TestSelfImproveClaimCarriesInflightTargets(t *testing.T) {
	selfRunID := uuid.New()
	repoID, otherRepoID := uuid.New(), uuid.New()

	fs, svc, wkr := selfImproveClaimFixture(t, selfRunID, repoID)
	fs.activeRunsAll = []store.ListActiveRunsAllRow{
		// (a) an active issue run on the SAME repo — the only entry that must survive.
		{Run: store.Run{
			ID: uuid.New(), RepoID: pgconv.UUID(repoID), Kind: "issue", Status: "running",
			IssueIid:         pgtype.Int8{Int64: 293, Valid: true},
			IssueTitle:       "Self-improvement picker skips in-progress work",
			MilestonesFrozen: mustMilestonesJSON(t, apitypes.Milestone{ID: "m3", Title: "Graceful-skip for deadcode:web/deadcode:agent when knip absent"}),
		}},
		// (b) the self_improve run itself — same id as claimRun, must be excluded.
		{Run: store.Run{
			ID: selfRunID, RepoID: pgconv.UUID(repoID), Kind: runkind.SelfImprove, Status: "claimed",
		}},
		// (c) a run on a DIFFERENT repo — must be excluded.
		{Run: store.Run{
			ID: uuid.New(), RepoID: pgconv.UUID(otherRepoID), Kind: "issue", Status: "running",
			IssueIid:   pgtype.Int8{Int64: 999, Valid: true},
			IssueTitle: "unrelated",
		}},
	}

	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a self_improve payload, got idle")
	}

	if got := len(payload.InflightTargets); got != 1 {
		t.Fatalf("InflightTargets has %d entries, want exactly 1 (self and other-repo excluded): %#v", got, payload.InflightTargets)
	}
	entry := payload.InflightTargets[0]
	if !strings.Contains(entry, "#293") {
		t.Errorf("in-flight line %q must carry the issue iid #293", entry)
	}
	if !strings.Contains(entry, "Graceful-skip for deadcode") {
		t.Errorf("in-flight line %q must carry the frozen milestone title substring", entry)
	}
	if strings.Contains(entry, "999") || strings.Contains(entry, "unrelated") {
		t.Errorf("in-flight line %q leaked the other-repo run (#999 / unrelated), which must be excluded", entry)
	}

	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(wire), `"inflight_targets"`) {
		t.Errorf("a non-empty avoid-set must serialize the inflight_targets key; wire = %s", wire)
	}
}

// TestSelfImproveClaimOmitsEmptyInflightTargets pins the omitempty half: with nothing in
// flight the field is nil and never reaches the wire, so an older worker is unaffected and
// the common no-overlap case adds no bytes (issue #297).
func TestSelfImproveClaimOmitsEmptyInflightTargets(t *testing.T) {
	selfRunID := uuid.New()
	repoID := uuid.New()

	fs, svc, wkr := selfImproveClaimFixture(t, selfRunID, repoID)
	fs.activeRunsAll = nil

	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a self_improve payload, got idle")
	}
	if payload.InflightTargets != nil {
		t.Errorf("InflightTargets must be nil for an empty active-runs set, got %#v", payload.InflightTargets)
	}

	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(wire), "inflight_targets") {
		t.Errorf("an empty avoid-set must be omitted from the wire; wire = %s", wire)
	}
}
