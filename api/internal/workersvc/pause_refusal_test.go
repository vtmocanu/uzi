package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPauseRefusalReason pins the three 409 classes a 0-row CreatePauseInput collapses into
// (PRD #1190 M1): a non-running run names its status (→ ErrPauseNotRunning), and a
// disallowed kind/interactivity names why per kind (→ ErrPauseNotSupported). The handler
// surfaces the .Msg verbatim, so the exact sentences are pinned here.
func TestPauseRefusalReason(t *testing.T) {
	tests := []struct {
		name     string
		run      store.Run
		sentinel error
		wantMsg  string
	}{
		{
			name:     "not running names the status",
			run:      store.Run{Status: "awaiting_approval", Kind: runkind.Issue},
			sentinel: ErrPauseNotRunning,
			wantMsg:  "run is awaiting_approval; the clock is already stopped",
		},
		{
			name:     "chat run",
			run:      store.Run{Status: "running", Kind: runkind.Chat},
			sentinel: ErrPauseNotSupported,
			wantMsg:  "chat runs already park between turns",
		},
		{
			name:     "interactive task",
			run:      store.Run{Status: "running", Kind: runkind.Task, Interactive: true},
			sentinel: ErrPauseNotSupported,
			wantMsg:  "interactive tasks park after each turn",
		},
		{
			name:     "judge run",
			run:      store.Run{Status: "running", Kind: runkind.Judge},
			sentinel: ErrPauseNotSupported,
			wantMsg:  "judge, mr_rework and ci_fix runs are short and finish on their own",
		},
		{
			name:     "ci_fix run",
			run:      store.Run{Status: "running", Kind: runkind.CIFix},
			sentinel: ErrPauseNotSupported,
			wantMsg:  "judge, mr_rework and ci_fix runs are short and finish on their own",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := pauseRefusalReason(tc.run)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.sentinel)
			}
			if err.Error() != tc.wantMsg {
				t.Fatalf("message = %q, want %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
