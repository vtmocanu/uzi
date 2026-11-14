package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// cleanup_stopped is proven by the exact stopped pod uid, so the Deployment uid is optional
// there; every other transition still requires it, and a present value must be a uuid.
func TestValidateHibernationTransitionDeploymentUID(t *testing.T) {
	pod, op, cleanup := uuid.NewString(), uuid.NewString(), uuid.NewString()
	mk := func(transition, dep string) HibernationTransition {
		return HibernationTransition{OpID: op, Transition: transition, DeploymentUID: dep, ResourceVersion: "7", PodUID: pod, CleanupOpID: cleanup}
	}
	cases := []struct {
		name string
		t    HibernationTransition
		ok   bool
	}{
		{"cleanup_stopped without deployment uid", mk(TransitionCleanupStopped, ""), true},
		{"cleanup_stopped with deployment uid", mk(TransitionCleanupStopped, uuid.NewString()), true},
		{"cleanup_stopped with malformed deployment uid", mk(TransitionCleanupStopped, "nope"), false},
		{"scaling_down without deployment uid", mk(TransitionScalingDown, ""), false},
		{"scaled_down without deployment uid", mk(TransitionScaledDown, ""), false},
		{"ready without deployment uid", mk(TransitionReady, ""), false},
		{"refused without deployment uid", mk(TransitionRefused, ""), false},
		{"ready with deployment uid", mk(TransitionReady, uuid.NewString()), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := c.t
			tr.Reason = "why"
			_, _, _, err := validateHibernationTransition(&tr)
			if c.ok {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var ve *HibernationValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a validation error", err)
			}
		})
	}
}
