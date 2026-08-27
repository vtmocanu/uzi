package capability

import "testing"

// TestResolveEphemeralSpec pins the {template, docker} an ephemeral hosted worker is
// provisioned with for a run's required capability set (PRD #529 M1): docker maps to the
// docker_enabled dimension, jvm maps to the "jvm" template, no template-derived capability
// leaves "base", and a capability neither the docker dimension nor any template can provide
// is unprovisionable (error). Size is not resolved here.
func TestResolveEphemeralSpec(t *testing.T) {
	tests := []struct {
		name         string
		required     []string
		wantTemplate string
		wantDocker   bool
		wantErr      bool
	}{
		{name: "docker only -> base+docker", required: []string{Docker}, wantTemplate: "base", wantDocker: true},
		{name: "jvm only -> jvm template", required: []string{JVM}, wantTemplate: "jvm", wantDocker: false},
		{name: "docker and jvm -> jvm+docker", required: []string{Docker, JVM}, wantTemplate: "jvm", wantDocker: true},
		{name: "empty -> base, no docker", required: nil, wantTemplate: "base", wantDocker: false},
		{name: "unprovisionable capability errors", required: []string{"gpu"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTemplate, gotDocker, err := ResolveEphemeralSpec(tc.required)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveEphemeralSpec(%v) = (%q, %v, nil), want error", tc.required, gotTemplate, gotDocker)
				}
				// On error the spec must be the zero spec: an unprovisionable run must not
				// silently provision a base worker.
				if gotTemplate != "" || gotDocker {
					t.Errorf("ResolveEphemeralSpec(%v) on error = (%q, %v), want (\"\", false)", tc.required, gotTemplate, gotDocker)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveEphemeralSpec(%v) unexpected error: %v", tc.required, err)
			}
			if gotTemplate != tc.wantTemplate || gotDocker != tc.wantDocker {
				t.Errorf("ResolveEphemeralSpec(%v) = (%q, %v), want (%q, %v)", tc.required, gotTemplate, gotDocker, tc.wantTemplate, tc.wantDocker)
			}
		})
	}
}
