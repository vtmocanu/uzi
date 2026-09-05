package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1140 M2 — the JUDGE lane's three-valued bind mode, resolved by judgeChoice.
//
// judgeChoice is the judge-lane peer of claimSecretID and returns the same
// secretChoice, so these own the wiring: which arm runs for which mode, that the auto
// arm reuses the run lane's ranker, and the D4 empty-pool asymmetry (the judge lane
// spends the default recorded pool_empty rather than holding).

// judgeRun is a plain run owned by the fixture's owner, for a direct judgeChoice call.
func (f autoFixture) judgeRun() store.Run {
	return store.Run{ID: f.runID, UserID: f.owner}
}

// TestJudgeChoicePinnedSpendsThePointer: mode pinned + an owned pointer resolves that
// exact credential, reason `judge`, and does NOT run the pool selector.
func TestJudgeChoicePinnedSpendsThePointer(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.judgeBindMode = BindModePinned
	f.fs.judgeSecret = pgtype.UUID{Bytes: f.fullID, Valid: true}

	choice, err := f.svc.judgeChoice(context.Background(), f.judgeRun())
	if err != nil {
		t.Fatalf("judgeChoice: %v", err)
	}
	if choice.secretID == nil || *choice.secretID != f.fullID {
		t.Fatalf("secretID = %v, want the pinned pointer %v", choice.secretID, f.fullID)
	}
	if choice.reason != selectReasonJudge {
		t.Fatalf("reason = %q, want %q", choice.reason, selectReasonJudge)
	}
	if len(f.fs.autoCandidateLookups) != 0 {
		t.Fatalf("a pinned judge lane ran the selector %d time(s); it must not", len(f.fs.autoCandidateLookups))
	}
}

// TestJudgeChoicePinnedNullPointerIsDefault: pinned with a NULL pointer resolves as the
// owner's default (D6), recorded honestly as `default` — staticChoice discards the
// bound reason on a nil id.
func TestJudgeChoicePinnedNullPointerIsDefault(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.judgeBindMode = BindModePinned
	f.fs.judgeSecret = pgtype.UUID{} // NULL, e.g. a deleted token nulled by 00079's FK

	choice, err := f.svc.judgeChoice(context.Background(), f.judgeRun())
	if err != nil {
		t.Fatalf("judgeChoice: %v", err)
	}
	if choice.secretID != nil {
		t.Fatalf("secretID = %v, want nil (default)", choice.secretID)
	}
	if choice.reason != selectReasonDefault {
		t.Fatalf("reason = %q, want %q", choice.reason, selectReasonDefault)
	}
}

// TestJudgeChoiceDefaultIsDefault: mode default resolves the owner's default token,
// reason `default`, and runs no selector.
func TestJudgeChoiceDefaultIsDefault(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.judgeBindMode = BindModeDefault

	choice, err := f.svc.judgeChoice(context.Background(), f.judgeRun())
	if err != nil {
		t.Fatalf("judgeChoice: %v", err)
	}
	if choice.secretID != nil {
		t.Fatalf("secretID = %v, want nil (default)", choice.secretID)
	}
	if choice.reason != selectReasonDefault {
		t.Fatalf("reason = %q, want %q", choice.reason, selectReasonDefault)
	}
	if len(f.fs.autoCandidateLookups) != 0 {
		t.Fatalf("a default judge lane ran the selector %d time(s); it must not", len(f.fs.autoCandidateLookups))
	}
}

// TestJudgeChoiceAutoPicksHigherHeadroom: mode auto runs the SAME ranker the run lane
// uses (D7) and picks the higher-headroom pooled token, reason `auto` with the raw
// headroom — the exact behaviour an issue run gets.
//
// MUTATION THIS CATCHES: the auto arm returning the static default instead of
// autoChoice → nil id and reason `default`, reddening on both.
func TestJudgeChoiceAutoPicksHigherHeadroom(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.judgeBindMode = BindModeAuto
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.fullID, "console-key", true, 20, time.Minute, 0),
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
	}

	choice, err := f.svc.judgeChoice(context.Background(), f.judgeRun())
	if err != nil {
		t.Fatalf("judgeChoice: %v", err)
	}
	if choice.secretID == nil || *choice.secretID != f.emptyID {
		t.Fatalf("secretID = %v, want the higher-headroom pooled token %v", choice.secretID, f.emptyID)
	}
	if choice.reason != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want %q", choice.reason, autoselect.ReasonAuto)
	}
	if choice.headroom == nil || *choice.headroom != 90 {
		t.Fatalf("headroom = %v, want 90", choice.headroom)
	}
	// The query was scoped to the run owner, once.
	if len(f.fs.autoCandidateLookups) != 1 || f.fs.autoCandidateLookups[0] != f.owner {
		t.Fatalf("candidate lookups = %v, want exactly one for %v", f.fs.autoCandidateLookups, f.owner)
	}
}

// TestJudgeChoiceAutoEmptyPoolIsPoolEmpty: mode auto on a genuinely empty pool returns
// a nil-id choice carrying reason pool_empty (D4) — NOT staticChoice's `default`, and
// NOT errAutoPoolEmpty (the judge lane does not hold).
func TestJudgeChoiceAutoEmptyPoolIsPoolEmpty(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.judgeBindMode = BindModeAuto
	f.fs.autoCandidates = nil // empty pool

	choice, err := f.svc.judgeChoice(context.Background(), f.judgeRun())
	if err != nil {
		t.Fatalf("judgeChoice returned an error on an empty pool; the judge lane must NOT hold (D4): %v", err)
	}
	if choice.secretID != nil {
		t.Fatalf("secretID = %v, want nil so openAnthropic resolves the default", choice.secretID)
	}
	if choice.reason != string(autoselect.ReasonPoolEmpty) {
		t.Fatalf("reason = %q, want %q — a reason-only check is why the full-path test below also asserts the recorded id",
			choice.reason, autoselect.ReasonPoolEmpty)
	}
	if choice.headroom != nil {
		t.Fatalf("headroom = %v, want NULL", choice.headroom)
	}
}

// judgeAutoFixture builds a JUDGE-run claim fixture in `auto` mode, so the full record
// path (judgeChoice → openWithAutoRetry → recordRunCredential) can be driven through
// Claim exactly as production would.
func newJudgeAutoFixture(t *testing.T) autoFixture {
	t.Helper()
	f := newAutoFixture(t)
	f.fs.claimRun.Kind = runkind.Judge
	f.fs.judgeBindMode = BindModeAuto
	return f
}

// TestJudgeAutoEmptyPoolSpendsDefaultRecordedPoolEmpty is D4 through the full claim
// path: an auto judge lane whose pool is empty spends the OWNER'S DEFAULT token and
// records reason pool_empty with NULL headroom — the run completes rather than holding.
// It pins BOTH the reason and the recorded id, because a reason-only check cannot tell
// D4's fallback from an ordinary `default`.
func TestJudgeAutoEmptyPoolSpendsDefaultRecordedPoolEmpty(t *testing.T) {
	f := newJudgeAutoFixture(t)
	f.fs.autoCandidates = nil // empty pool

	payload := f.claim(t)
	if payload == nil {
		t.Fatal("an auto judge run with an empty pool went idle; D4 says it spends the default and completes")
	}
	if f.fs.poolWaitHeld != nil {
		t.Fatalf("the judge run was held in pool_wait (%v); D4 says the judge lane does NOT hold", f.fs.poolWaitHeld)
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("the judge run was failed terminally (%v); D4 says it spends the default", f.fs.markedFailed)
	}
	rec := onlyRecord(t, f.fs)
	// The recorded id is the OWNER DEFAULT, opened by id (D8), NOT a pooled token.
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fs.defaultCredID() {
		t.Fatalf("recorded %v, want the owner default %v — D4's fallback spends the default",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.fs.defaultCredID())
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonPoolEmpty) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonPoolEmpty)
	}
	if rec.AnthropicHeadroomPct.Valid {
		t.Fatalf("headroom = %+v, want NULL — nothing was measured on an empty pool", rec.AnthropicHeadroomPct)
	}
}

// TestJudgeAutoRetriesOntoAnotherPooledTokenWhenThePickWillNotOpen is the D14 retry
// reaching the JUDGE lane (PRD #1140 M2). Before the openWithAutoRetry extraction the
// judge lane opened directly, so an auto judge pick that would not decrypt failed the
// judge run terminally while the identical run-lane pick floored onto another pooled
// token. Now the judge lane re-floors once and records open_failed with the second
// pooled token's id.
func TestJudgeAutoRetriesOntoAnotherPooledTokenWhenThePickWillNotOpen(t *testing.T) {
	f := newJudgeAutoFixture(t)
	// The selector's pick (emptyID, headroom 90) will not open; fullID (headroom 20) is
	// the openable second pooled token the floor-retry must fall to.
	f.fs.byIDSecrets[f.emptyID] = store.GetUserSecretCiphertextByIDRow{
		UserID: f.owner, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("not-sealed-by-this-box"), SealedWith: store.SealedWithMaster,
	}
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
		candRow(f.fullID, "console-key", true, 20, time.Minute, 0),
	}

	payload := f.claim(t)
	if payload == nil {
		t.Fatal("an undecryptable auto judge pick went idle; the second pooled token was openable — the retry did not reach the judge lane")
	}
	if f.fs.markedFailed != nil {
		t.Fatalf("the judge run was failed terminally (%v); the D14 retry must reach the judge lane too", f.fs.markedFailed)
	}
	// Two opens: the failing pick, then the SECOND POOLED token — never the default.
	if len(f.fs.byIDLookups) != 2 || f.fs.byIDLookups[0].ID != f.emptyID || f.fs.byIDLookups[1].ID != f.fullID {
		t.Fatalf("by-id opens = %+v, want the pick %v then the second pooled token %v", f.fs.byIDLookups, f.emptyID, f.fullID)
	}
	if f.fs.byIDLookups[1].ID == f.fs.defaultCredID() {
		t.Fatal("the judge floor-retry opened the owner default — the auto lane must NEVER spend the non-pooled default")
	}
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.fullID {
		t.Fatalf("recorded %v, want the floored pooled token %v", uuid.UUID(rec.AnthropicSecretID.Bytes), f.fullID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonOpenFailed) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonOpenFailed)
	}
}

// TestSelfImproveAutoFollowsThePool: a self_improve run whose owner is in judge `auto`
// mode resolves the POOL (not the claiming worker's mode), through the same judgeChoice
// the judge lane uses — this is the run-lane arm of claimSecretID routing to judgeChoice.
func TestSelfImproveAutoFollowsThePool(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.claimRun.Kind = runkind.SelfImprove
	f.fs.judgeBindMode = BindModeAuto
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		candRow(f.fullID, "console-key", true, 20, time.Minute, 0),
		candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0),
	}
	// The claiming worker is pinned to a DIFFERENT credential, to prove the judge-lane
	// auto pool wins over the worker's own mode for a self_improve run.
	f.worker = store.Worker{
		ID: uuid.New(), UserID: f.owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: f.fs.defaultCredID(), Valid: true},
	}

	f.claim(t)
	rec := onlyRecord(t, f.fs)
	if uuid.UUID(rec.AnthropicSecretID.Bytes) != f.emptyID {
		t.Fatalf("recorded %v, want the pool's higher-headroom token %v — self_improve follows the judge pool, not the worker's pin",
			uuid.UUID(rec.AnthropicSecretID.Bytes), f.emptyID)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonAuto)
	}
	// It is still a repo-ful run-lane claim, not a judge claim.
	if !f.fs.claimCtxCalled {
		t.Fatal("a self_improve claim must still take the ordinary repo-ful claim path")
	}
}
