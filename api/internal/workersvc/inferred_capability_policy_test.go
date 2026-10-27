package workersvc

import (
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestInferredCapabilityPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		persisted, reported, ignored, want []string
	}{
		{"suppressed", nil, []string{"jvm", "docker"}, []string{"jvm"}, []string{"docker"}},
		{"declared wins", []string{"jvm"}, []string{"jvm", "docker"}, []string{"jvm"}, []string{"docker", "jvm"}},
		{"persisted never removed", []string{"docker"}, []string{}, []string{"docker"}, []string{"docker"}},
		{"old run unchanged", []string{"docker"}, []string{"jvm"}, nil, []string{"jvm"}},
		{"absent legacy report", nil, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{RequiredCapabilities: tc.persisted, IgnoredInferredCapabilities: tc.ignored}
			got := applyInferredCapabilityPolicy(run, tc.reported)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}
