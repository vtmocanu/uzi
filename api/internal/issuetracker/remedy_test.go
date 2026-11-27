package issuetracker

import (
	"errors"
	"fmt"
	"testing"
)

func TestWithRemedy(t *testing.T) {
	if WithRemedy(nil, "x") != nil {
		t.Error("WithRemedy(nil) is not nil")
	}
	err := fmt.Errorf("move: %w", WithRemedy(ErrStateMachineField, "fix it in the tracker"))
	if !errors.Is(err, ErrStateMachineField) {
		t.Error("errors.Is does not see the sentinel through the remedy")
	}
	if got := RemedyOf(err); got != "fix it in the tracker" {
		t.Errorf("RemedyOf = %q", got)
	}
	if err.Error() != "move: "+ErrStateMachineField.Error() {
		t.Errorf("Error() = %q: the remedy must not change the message", err.Error())
	}
	if got := RemedyOf(ErrStateMachineField); got != "" {
		t.Errorf("RemedyOf(no remedy) = %q, want empty", got)
	}
}
