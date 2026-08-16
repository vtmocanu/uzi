package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

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
