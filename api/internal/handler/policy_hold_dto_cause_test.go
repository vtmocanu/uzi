package handler

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func policyDTORun(mutate func(*store.Run)) store.Run {
	r := store.Run{
		Status: "paused", HoldReason: pgconv.Text("codex_policy_hold"), ClaimGeneration: 3,
		PolicyOutcomeID: pgconv.UUID(uuid.New()), PolicyContinuationMode: pgconv.Text("same_conversation"),
		PolicyPreserveRequired: true, PolicyApplyingWorkerID: pgconv.UUID(uuid.New()),
		PolicyApplyingGeneration: pgtype.Int8{Int64: 3, Valid: true}, PolicySourceGeneration: pgtype.Int8{Int64: 3, Valid: true},
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func TestPolicyHoldDTOPinsEveryCauseAndNull(t *testing.T) {
	causes := []string{"refused", "attempt_path", "saved_work_missing", "session_missing", "transcript_unknown",
		"provider_resume_rejected", "pending_conversion", "fresh_start_uncertain"}
	for _, cause := range causes {
		dto := policyHoldDTO(policyDTORun(func(r *store.Run) { r.PolicyHoldCause = pgconv.Text(cause) }))
		if dto == nil || dto.HoldCause == nil || *dto.HoldCause != cause {
			t.Fatalf("cause %q projected as %+v", cause, dto)
		}
	}
	if dto := policyHoldDTO(policyDTORun(nil)); dto == nil || dto.HoldCause != nil {
		t.Fatalf("NULL cause must project as null, got %+v", dto)
	}
	if policyHoldDTO(store.Run{}) != nil {
		t.Fatal("a run without an episode must have no policy_hold")
	}
}

func TestPolicyHoldDTOResumeModes(t *testing.T) {
	resolved := func(r *store.Run) { r.PolicyResolvedAt = pgtype.Timestamptz{Valid: true} }
	for name, tc := range map[string]struct {
		mutate func(*store.Run)
		want   []string
	}{
		"unresolved session-compatible hold": {nil, []string{"preserve"}},
		"unresolved attempt-path hold": {func(r *store.Run) { r.PolicyContinuationMode = pgconv.Text("fresh_from_saved_work") },
			[]string{"fresh_from_saved_work"}},
		"resolved episode":      {resolved, []string{}},
		"queued after resume":   {func(r *store.Run) { resolved(r); r.Status = "queued"; r.HoldReason = pgtype.Text{} }, []string{}},
		"not paused":            {func(r *store.Run) { r.Status = "running" }, []string{}},
		"other hold reason":     {func(r *store.Run) { r.HoldReason = pgconv.Text("budget_exhausted") }, []string{}},
		"unknown lane (legacy)": {func(r *store.Run) { r.PolicyContinuationMode = pgtype.Text{} }, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			dto := policyHoldDTO(policyDTORun(tc.mutate))
			if dto == nil || dto.ResumeModes == nil || !reflect.DeepEqual(dto.ResumeModes, tc.want) {
				t.Fatalf("resume_modes = %#v, want %#v (never null)", dto.ResumeModes, tc.want)
			}
		})
	}
}

func TestPolicyHoldDTORecoveryFacts(t *testing.T) {
	head, other := "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	for name, tc := range map[string]struct {
		mutate             func(*store.Run)
		required, verified bool
	}{
		"no required head":     {nil, false, false},
		"required, unverified": {func(r *store.Run) { r.PolicyRequiredHead = pgconv.Text(head) }, true, false},
		"verified this claim": {func(r *store.Run) {
			r.PolicyRequiredHead = pgconv.Text(head)
			r.PolicyRecoveryVerifiedGeneration = pgtype.Int8{Int64: 3, Valid: true}
			r.PolicyRecoveryVerifiedHead = pgconv.Text(head)
		}, true, true},
		"verified an earlier claim": {func(r *store.Run) {
			r.PolicyRequiredHead = pgconv.Text(head)
			r.PolicyRecoveryVerifiedGeneration = pgtype.Int8{Int64: 2, Valid: true}
			r.PolicyRecoveryVerifiedHead = pgconv.Text(head)
		}, true, false},
		"verified a different head": {func(r *store.Run) {
			r.PolicyRequiredHead = pgconv.Text(head)
			r.PolicyRecoveryVerifiedGeneration = pgtype.Int8{Int64: 3, Valid: true}
			r.PolicyRecoveryVerifiedHead = pgconv.Text(other)
		}, true, false},
		"verification without a required head": {func(r *store.Run) {
			r.PolicyRecoveryVerifiedGeneration = pgtype.Int8{Int64: 3, Valid: true}
			r.PolicyRecoveryVerifiedHead = pgconv.Text(head)
		}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			dto := policyHoldDTO(policyDTORun(tc.mutate))
			if dto.RecoveryRequired != tc.required || dto.RecoveryVerifiedThisClaim != tc.verified {
				t.Fatalf("required=%v verified=%v, want %v/%v", dto.RecoveryRequired, dto.RecoveryVerifiedThisClaim, tc.required, tc.verified)
			}
		})
	}
}
