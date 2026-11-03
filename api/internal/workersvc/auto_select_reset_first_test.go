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
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// #3844 — the reset_first order (the default Policy.Order), from each lane's side.
// autoselect's own tests own the ranking; these prove every lane that runs the
// selector (worker auto claim, run override auto, resume exclusion, judge,
// self_improve) feeds it the SEVEN-DAY reset, so the account whose week resets sooner
// wins even though it has LESS headroom.

// weeklyReset sets the row's seven-day reset d after autoNow. candRow leaves it NULL,
// which reset_first treats as an unknown reset (ranked after every known one), so
// existing fixtures keep ranking on headroom.
func weeklyReset(r store.ListAutoSelectCandidatesRow, d time.Duration) store.ListAutoSelectCandidatesRow {
	r.SevenDayResetsAt = pgtype.Timestamptz{Time: autoNow.Add(d), Valid: true}
	return r
}

// resetFirstPool is two eligible tokens: full (20% headroom, week resets in 2h) and
// empty (90% headroom, week resets in 100h). reset_first picks full; headroom_first
// would pick empty.
func resetFirstPool(f autoFixture) []store.ListAutoSelectCandidatesRow {
	return []store.ListAutoSelectCandidatesRow{
		weeklyReset(candRow(f.fullID, "console-key", true, 20, time.Minute, 0), 2*time.Hour),
		weeklyReset(candRow(f.emptyID, "spare-key", true, 90, time.Minute, 0), 100*time.Hour),
	}
}

func assertRecordedAuto(t *testing.T, f autoFixture, want uuid.UUID, headroom int16) {
	t.Helper()
	rec := onlyRecord(t, f.fs)
	if got := uuid.UUID(rec.AnthropicSecretID.Bytes); got != want {
		t.Fatalf("recorded %v, want %v (the sooner weekly reset)", got, want)
	}
	if rec.AnthropicSelectReason.String != string(autoselect.ReasonAuto) {
		t.Fatalf("reason = %q, want %q", rec.AnthropicSelectReason.String, autoselect.ReasonAuto)
	}
	if !rec.AnthropicHeadroomPct.Valid || rec.AnthropicHeadroomPct.Int16 != headroom {
		t.Fatalf("headroom = %+v, want the raw %d", rec.AnthropicHeadroomPct, headroom)
	}
}

func TestAutoClaimResetFirstPrefersSoonerWeeklyReset(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.autoCandidates = resetFirstPool(f)
	f.claim(t)
	assertRecordedAuto(t, f, f.fullID, 20)
}

// The same pool under an explicit headroom_first policy picks the other token, so the
// test above discriminates the order rather than a fixture accident.
func TestAutoClaimHeadroomFirstPolicyStillPrefersHeadroom(t *testing.T) {
	f := newAutoFixture(t)
	f.svc.p.Autoselect.Order = autoselect.OrderHeadroomFirst
	f.fs.autoCandidates = resetFirstPool(f)
	f.claim(t)
	assertRecordedAuto(t, f, f.emptyID, 90)
}

func TestRunOverrideAutoResetFirstPrefersSoonerWeeklyReset(t *testing.T) {
	f := newAutoFixture(t)
	f.worker.AnthropicBindMode = BindModePinned
	f.worker.AnthropicSecretID = pgtype.UUID{Bytes: f.emptyID, Valid: true}
	f.fs.claimRun.CredentialOverrideMode = pgtype.Text{String: "auto", Valid: true}
	f.fs.autoCandidates = resetFirstPool(f)
	f.claim(t)
	assertRecordedAuto(t, f, f.fullID, 20)
}

// A resuming run excludes the credential it just parked on. That token has the soonest
// reset, so the next-soonest eligible token is picked, not the emptiest.
func TestAutoResumeExclusionResetFirstPicksNextSoonest(t *testing.T) {
	f := newAutoFixture(t)
	dead := uuid.New() // soonest reset; excluded, never opened
	f.fs.claimRun.LimitDeadSecretID = pgtype.UUID{Bytes: dead, Valid: true}
	f.fs.claimRun.RetryNotBefore = pgtype.Timestamptz{Time: autoNow.Add(time.Hour), Valid: true}
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		weeklyReset(candRow(dead, "dead-key", true, 50, time.Minute, 0), time.Hour),
		weeklyReset(candRow(f.emptyID, "spare-key", true, 30, time.Minute, 0), 5*time.Hour),
		weeklyReset(candRow(uuid.New(), "other-key", true, 95, time.Minute, 0), 50*time.Hour),
	}
	f.claim(t)
	assertRecordedAuto(t, f, f.emptyID, 30)
}

// The weekly usage reserve is a pre-filter, not a ranking input: a pooled token it blocks is
// dropped before reset_first ranks, so the soonest-reset token being blocked yields the
// next-soonest unblocked one (reason auto), not the blocked one.
func TestAutoChoiceResetFirstReserveDropsBlockedSoonestReset(t *testing.T) {
	f := newAutoFixture(t)
	blocked := uuid.New() // soonest weekly reset, blocked by the reserve
	next := f.fullID      // next-soonest, unblocked
	f.fs.autoCandidates = []store.ListAutoSelectCandidatesRow{
		weeklyReset(candRow(blocked, "blocked-key", true, 80, time.Minute, 0), time.Hour),
		weeklyReset(candRow(next, "next-key", true, 40, time.Minute, 0), 5*time.Hour),
		weeklyReset(candRow(f.emptyID, "late-key", true, 95, time.Minute, 0), 100*time.Hour),
	}
	snap := &reserveSnap{
		userID: f.owner,
		claude: map[uuid.UUID]usagereserve.Verdict{
			blocked: held(90), next: okV(), f.emptyID: okV(),
		},
		pooled: []uuid.UUID{blocked, next, f.emptyID},
	}
	ctx := (&reserveGate{snap: snap, now: autoNow}).attach(context.Background())

	choice, err := f.svc.autoChoice(ctx, store.Run{UserID: f.owner})
	if err != nil {
		t.Fatalf("autoChoice: %v", err)
	}
	if choice.secretID == nil || *choice.secretID != next {
		t.Fatalf("secretID = %v, want the next-soonest unblocked token %v", choice.secretID, next)
	}
	if choice.reason != string(autoselect.ReasonAuto) || choice.headroom == nil || *choice.headroom != 40 {
		t.Fatalf("reason %q headroom %v, want auto and the raw 40", choice.reason, choice.headroom)
	}
}

func TestJudgeChoiceAutoResetFirstPrefersSoonerWeeklyReset(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.judgeBindMode = BindModeAuto
	f.fs.autoCandidates = resetFirstPool(f)
	choice, err := f.svc.judgeChoice(context.Background(), f.judgeRun())
	if err != nil {
		t.Fatalf("judgeChoice: %v", err)
	}
	if choice.secretID == nil || *choice.secretID != f.fullID {
		t.Fatalf("secretID = %v, want the sooner-weekly-reset token %v", choice.secretID, f.fullID)
	}
	if choice.reason != string(autoselect.ReasonAuto) || choice.headroom == nil || *choice.headroom != 20 {
		t.Fatalf("reason %q headroom %v, want auto and the raw 20", choice.reason, choice.headroom)
	}
}

func TestSelfImproveAutoResetFirstPrefersSoonerWeeklyReset(t *testing.T) {
	f := newAutoFixture(t)
	f.fs.claimRun.Kind = runkind.SelfImprove
	f.fs.judgeBindMode = BindModeAuto
	f.fs.autoCandidates = resetFirstPool(f)
	f.worker = store.Worker{
		ID: uuid.New(), UserID: f.owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: f.fs.defaultCredID(), Valid: true},
	}
	f.claim(t)
	assertRecordedAuto(t, f, f.fullID, 20)
}
