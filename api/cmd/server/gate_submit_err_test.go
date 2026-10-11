package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The Slack adapter translates the plan-gate sentinels so slacksvc can branch on them without a
// workersvc import (PRD #1795 M5 adds the revision mismatch, a pointer error type matched with
// errors.As, wrapped or not).
func TestTranslateGateSubmitErr(t *testing.T) {
	other := errors.New("boom")
	mismatch := &workersvc.GateRevisionMismatchError{Expected: 2, Current: 3}
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"revision mismatch", mismatch, slacksvc.ErrGateRevisionMismatch},
		{"wrapped revision mismatch", fmt.Errorf("submit: %w", mismatch), slacksvc.ErrGateRevisionMismatch},
		{"revise cap", workersvc.ErrReviseCapReached, slacksvc.ErrReviseCapReached},
		{"approval milestones moved", workersvc.ErrApprovalMilestonesMoved, slacksvc.ErrGateRevisionMismatch},
		{"invalid selection", workersvc.ErrInvalidSelection, slacksvc.ErrSelectionRejected},
		{"other passes through", other, other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := translateGateSubmitErr(tc.in); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
				t.Fatalf("translateGateSubmitErr(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
