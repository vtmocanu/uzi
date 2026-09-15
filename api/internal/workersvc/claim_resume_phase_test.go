package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestResumePhaseFor is the pure-unit table for the PRD #1247 M5 (D13) resume_phase
// derivation. It builds store.Run values directly (no DB) so the ORDERED table in
// resumePhaseFor is pinned independent of the live-DB assembly path: an open question wins
// over an approved plan (a mid-run clarification, PRD #88), an approved plan resumes
// implementing, an unapproved submitted plan with a resumable session resumes the gate, and
// everything else (no plan, no session, empty/whitespace fields) falls through to "".
func TestResumePhaseFor(t *testing.T) {
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	cases := []struct {
		name         string
		run          store.Run
		planApproved bool
		want         string
	}{
		{
			// open_question_id is checked FIRST and unconditionally, so it wins even when the
			// plan is independently approved (a mid-run clarification on an autopilot run).
			name:         "open question wins even with planApproved true",
			run:          store.Run{OpenQuestionID: text("q-1"), PlanMd: text("# plan"), SessionID: text("sess")},
			planApproved: true,
			want:         "awaiting_input",
		},
		{
			name:         "plan and approved resumes implementing",
			run:          store.Run{PlanMd: text("# plan"), SessionID: text("sess")},
			planApproved: true,
			want:         "implementing",
		},
		{
			// An APPROVED interactive run collapses into implementing by design: a released
			// awaiting_followup run is indistinguishable from implementing once requeued
			// (ReleaseCredentialSwitch records no source state, open_followup_id ratchets), so
			// it must land in the implementing bucket and let the worker's existing
			// interactive-resume logic drive the idle-for-follow-up behaviour.
			name:         "approved interactive run collapses to implementing (awaiting_followup collapse)",
			run:          store.Run{PlanMd: text("# plan"), SessionID: text("sess"), Interactive: true},
			planApproved: true,
			want:         "implementing",
		},
		{
			name:         "plan unapproved with session resumes the gate",
			run:          store.Run{PlanMd: text("# plan"), SessionID: text("sess")},
			planApproved: false,
			want:         "awaiting_approval",
		},
		{
			// A gate restore needs a resumable session; without one the worker cannot restore
			// the gate, so this falls through to "" (re-plan).
			name:         "plan unapproved without session falls through",
			run:          store.Run{PlanMd: text("# plan")},
			planApproved: false,
			want:         "",
		},
		{
			name:         "no plan is a fresh run",
			run:          store.Run{SessionID: text("sess")},
			planApproved: false,
			want:         "",
		},
		{
			// A whitespace-only plan_md is treated as no plan (the same TrimSpace guard the
			// self_improve open-MR path uses), so it never resumes a gate.
			name:         "whitespace-only plan is not a plan",
			run:          store.Run{PlanMd: text("   \n\t "), SessionID: text("sess")},
			planApproved: false,
			want:         "",
		},
		{
			// A present-but-whitespace open_question_id must not be read as awaiting_input.
			name:         "whitespace-only open question is not awaiting_input",
			run:          store.Run{OpenQuestionID: text("  "), PlanMd: text("# plan"), SessionID: text("sess")},
			planApproved: true,
			want:         "implementing",
		},
		{
			// A present-but-whitespace session_id is not a resumable session, so an unapproved
			// plan cannot resume the gate (the same TrimSpace guard the plan/open-question arms use).
			name:         "whitespace-only session is not a resumable gate",
			run:          store.Run{PlanMd: text("# plan"), SessionID: text("  ")},
			planApproved: false,
			want:         "",
		},
		{
			name:         "empty run falls through",
			run:          store.Run{},
			planApproved: false,
			want:         "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumePhaseFor(tc.run, tc.planApproved); got != tc.want {
				t.Fatalf("resumePhaseFor = %q, want %q", got, tc.want)
			}
		})
	}
}
