package handler

import (
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestRecoveryPrerequisiteClosureNotAdvertised(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, held := range []bool{false, true} {
			if slices.Contains(protocolFeatures(active, held), capability.RecoveryPrerequisiteClosureV1) {
				t.Fatal("C1b must not advertise closure FINAL support")
			}
		}
	}
}
