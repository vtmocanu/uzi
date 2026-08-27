package planpolicy

import "testing"

func TestScreen(t *testing.T) {
	tests := []struct {
		name        string
		plan        string
		wantTarget  string
		wantMatched bool
	}{
		// Rule 1: cloud instance metadata endpoints.
		{
			name:        "imds ipv4 literal",
			plan:        "curl http://169.254.169.254/latest/meta-data/",
			wantTarget:  "cloud instance metadata endpoint",
			wantMatched: true,
		},
		{
			name:        "gcp metadata dns",
			plan:        "GET http://metadata.google.internal/computeMetadata/v1/",
			wantTarget:  "cloud instance metadata endpoint",
			wantMatched: true,
		},
		{
			name:        "gcp metadata dns mixed case",
			plan:        "resolve Metadata.Google.Internal for the token",
			wantTarget:  "cloud instance metadata endpoint",
			wantMatched: true,
		},

		// Rule 2: kube-apiserver ClusterIP.
		{
			name:        "apiserver clusterip literal",
			plan:        "hit the apiserver at 10.96.0.1:443",
			wantTarget:  "kube-apiserver ClusterIP",
			wantMatched: true,
		},

		// Rule 3: in-pod service-account token mount.
		{
			name:        "service account token mount",
			plan:        "read /var/run/secrets/kubernetes.io/serviceaccount/token",
			wantTarget:  "in-pod service-account token mount",
			wantMatched: true,
		},
		{
			name:        "service account token mount mixed case",
			plan:        "cat /VAR/run/Secrets/Kubernetes.IO/ServiceAccount/token",
			wantTarget:  "in-pod service-account token mount",
			wantMatched: true,
		},
		{
			// /var/run is an FHS symlink to /run — the bare /run form is the
			// more canonical spelling of the same in-pod file and must match.
			name:        "service account token mount canonical run path",
			plan:        "read /run/secrets/kubernetes.io/serviceaccount/token",
			wantTarget:  "in-pod service-account token mount",
			wantMatched: true,
		},

		// Boundary negatives — the \b anchors must reject longer numbers.
		{
			name:        "apiserver trailing digit",
			plan:        "10.96.0.10",
			wantTarget:  "",
			wantMatched: false,
		},
		{
			name:        "apiserver leading digit",
			plan:        "110.96.0.1",
			wantTarget:  "",
			wantMatched: false,
		},
		{
			name:        "apiserver embedded in sentence",
			plan:        "version 10.96.0.15 of the tool",
			wantTarget:  "",
			wantMatched: false,
		},
		{
			name:        "imds trailing digit",
			plan:        "169.254.169.2540",
			wantTarget:  "",
			wantMatched: false,
		},
		{
			name:        "imds leading digit",
			plan:        "1169.254.169.254",
			wantTarget:  "",
			wantMatched: false,
		},

		// Deliberately-allowed content must stay clean.
		{
			name:        "kubernetes default svc allowed",
			plan:        "the service is reachable at kubernetes.default.svc inside the cluster",
			wantTarget:  "",
			wantMatched: false,
		},
		{
			name:        "aws credentials path allowed",
			plan:        "load profile from ~/.aws/credentials as documented",
			wantTarget:  "",
			wantMatched: false,
		},

		// Benign realistic multi-paragraph plan.
		{
			name: "benign multi-paragraph plan",
			plan: `Refactor the token store.

First, extract the retry helper from internal/store/queries.go into a new
helper function withBackoff(). Add a table-driven test in queries_test.go
covering the transient-error path.

Then wire the new helper into SubmitInput and run go test ./internal/store/...
to confirm nothing regressed. Update the CHANGELOG under [Unreleased].`,
			wantTarget:  "",
			wantMatched: false,
		},

		// Empty input.
		{
			name:        "empty string",
			plan:        "",
			wantTarget:  "",
			wantMatched: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, matched := Screen(tt.plan)
			if matched != tt.wantMatched {
				t.Errorf("Screen(%q) matched = %v, want %v", tt.plan, matched, tt.wantMatched)
			}
			if target != tt.wantTarget {
				t.Errorf("Screen(%q) target = %q, want %q", tt.plan, target, tt.wantTarget)
			}
		})
	}
}
