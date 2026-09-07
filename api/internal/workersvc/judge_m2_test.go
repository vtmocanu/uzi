package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #69 M2, Decision 5: judge-claim model resolution is user-value-wins. The run
// owner's per-user judge_model overrides the instance judge_model for their own
// judge runs; NULL/blank inherits the instance value; a user-row read error falls
// back to the instance value best-effort (logged) and NEVER sends an empty model.

// TestJudgeClaimUserModelOverridesInstance: a set per-user judge_model wins over the
// instance judge_model on the assembled claim.
func TestJudgeClaimUserModelOverridesInstance(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:   judgeRun(uid, target),
		anthropic:  sealedTok,
		judgeModel: pgtype.Text{String: "opus", Valid: true},
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != "opus" {
		t.Errorf("JudgeModel = %v, want the per-user override opus", payload.JudgeModel)
	}
}

// TestJudgeClaimNullUserModelInheritsInstance: a NULL per-user judge_model inherits
// the instance judge_model.
func TestJudgeClaimNullUserModelInheritsInstance(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:  judgeRun(uid, target),
		anthropic: sealedTok,
		// judgeModel left zero (NULL/inherit).
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != "haiku" {
		t.Errorf("JudgeModel = %v, want the inherited instance haiku", payload.JudgeModel)
	}
}

// TestJudgeClaimBlankUserModelInheritsInstance: a blank (whitespace) per-user
// judge_model is treated as inherit, not as an empty model.
func TestJudgeClaimBlankUserModelInheritsInstance(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:   judgeRun(uid, target),
		anthropic:  sealedTok,
		judgeModel: pgtype.Text{String: "   ", Valid: true},
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != "haiku" {
		t.Errorf("JudgeModel = %v, want the inherited instance haiku (blank ⇒ inherit)", payload.JudgeModel)
	}
}

// TestJudgeClaimUserModelReadErrorFallsBackToInstance: a user-row read error must
// NOT fail the claim and must NOT send an empty model — it falls back to the
// instance value best-effort (logged).
func TestJudgeClaimUserModelReadErrorFallsBackToInstance(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:      judgeRun(uid, target),
		anthropic:     sealedTok,
		judgeModelErr: errors.New("user row read failed"),
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("Claim must not fail on a user judge-model read error: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a claim payload despite the user judge-model read error")
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != "haiku" {
		t.Errorf("JudgeModel = %v, want the instance fallback haiku (never an empty model)", payload.JudgeModel)
	}
}

// issue #1157: the judge lane now carries the run owner's per-user default reasoning
// effort on the assembled claim, inheriting the uzi default `xhigh` when the owner has
// not chosen (NULL), mirroring the run/chat lanes. It is best-effort: a read error must
// NOT fail the judge claim (audit H2's no-spurious-fail posture).

// TestJudgeClaimNullDefaultEffortUsesUziDefault: a NULL per-user default_effort resolves
// to the uzi default xhigh on the claim, and serializes as default_effort:"xhigh".
func TestJudgeClaimNullDefaultEffortUsesUziDefault(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:  judgeRun(uid, target),
		anthropic: sealedTok,
		// defaultEffort left zero (NULL) ⇒ the uzi default xhigh.
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != "xhigh" {
		t.Errorf("Config.DefaultEffort = %v, want the uzi default xhigh", payload.Config.DefaultEffort)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(payload): %v", err)
	}
	if !strings.Contains(string(b), `"default_effort":"xhigh"`) {
		t.Errorf("marshaled claim missing default_effort:xhigh: %s", b)
	}
}

// TestJudgeClaimExplicitDefaultEffortIsCarried: a set per-user default_effort is carried
// through onto the judge claim verbatim.
func TestJudgeClaimExplicitDefaultEffortIsCarried(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:      judgeRun(uid, target),
		anthropic:     sealedTok,
		defaultEffort: pgconv.TextOrNull("high"),
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != "high" {
		t.Errorf("Config.DefaultEffort = %v, want the owner's explicit high", payload.Config.DefaultEffort)
	}
}

// TestJudgeClaimDefaultEffortReadErrorFallsBackToXhigh: a user-row read error must NOT
// fail the judge claim and falls back to the uzi default xhigh best-effort, mirroring
// TestJudgeClaimUserModelReadErrorFallsBackToInstance.
func TestJudgeClaimDefaultEffortReadErrorFallsBackToXhigh(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:         judgeRun(uid, target),
		anthropic:        sealedTok,
		defaultEffortErr: errors.New("db down"),
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("Claim must not fail on a user default-effort read error: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a claim payload despite the user default-effort read error")
	}
	if payload.Config.DefaultEffort == nil || *payload.Config.DefaultEffort != "xhigh" {
		t.Errorf("Config.DefaultEffort = %v, want the uzi default xhigh fallback", payload.Config.DefaultEffort)
	}
}
