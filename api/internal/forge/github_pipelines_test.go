package forge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitHubActionsStatusFold pins item-9 / D8: while not completed the neutral
// status is the run `status`; once completed it is the `conclusion`. Stored raw.
func TestGitHubActionsStatusFold(t *testing.T) {
	for _, tc := range []struct {
		status     string
		conclusion any // string or nil
		want       string
	}{
		{"in_progress", nil, "in_progress"},
		{"queued", nil, "queued"},
		{"completed", "failure", "failure"},
		{"completed", "success", "success"},
		{"completed", "timed_out", "timed_out"},
	} {
		t.Run(tc.status+"/"+asStr(tc.conclusion), func(t *testing.T) {
			run := map[string]any{
				"id": 900, "head_branch": "feature", "head_sha": "deadbeef",
				"status": tc.status, "conclusion": tc.conclusion,
				"html_url": "https://github.com/acme/widgets/actions/runs/900",
			}
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []map[string]any{run}})
				},
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
			p, err := d.LatestPipeline(context.Background(), 7, "feature")
			if err != nil {
				t.Fatalf("LatestPipeline: %v", err)
			}
			if p.Status != tc.want {
				t.Fatalf("status=%q conclusion=%v → %q, want %q", tc.status, tc.conclusion, p.Status, tc.want)
			}
			if p.ID != 900 || p.SHA != "deadbeef" {
				t.Errorf("run fields not mapped: %+v", p)
			}
		})
	}
}

func TestGitHubLatestPipelineNoneIsErrNoPipeline(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	if _, err := d.LatestPipeline(context.Background(), 7, "feature"); err != ErrNoPipeline {
		t.Fatalf("no runs must be ErrNoPipeline, got %v", err)
	}
}

// TestGitHubLatestMRPipelineNoHeadIsError pins that a PR with no resolvable head
// is a real error, NOT ErrNoPipeline (mirrors the Forgejo driver).
func TestGitHubLatestMRPipelineNoHeadIsError(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 4, "state": "open"}) // no head
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	_, err := d.LatestMRPipeline(context.Background(), 7, 4)
	if err == nil || err == ErrNoPipeline {
		t.Fatalf("a headless PR must be a real error, not ErrNoPipeline; got %v", err)
	}
}

func TestGitHubListPipelineJobs(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs/900/jobs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "jobs": []map[string]any{
				{"id": 1, "name": "build", "status": "completed", "conclusion": "success", "html_url": "u1"},
				{"id": 2, "name": "test", "status": "in_progress", "conclusion": nil, "html_url": "u2"},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	jobs, err := d.ListPipelineJobs(context.Background(), 7, 900)
	if err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	if jobs[0].Status != "success" || jobs[1].Status != "in_progress" {
		t.Fatalf("job status fold wrong: %+v", jobs)
	}
}

// TestGitHubJobLogTailFromEnd pins item-10: JobLogTail follows the 302 to the
// plain-text blob URL and truncates to maxBytes from the END.
func TestGitHubJobLogTailFromEnd(t *testing.T) {
	const fullLog = "line one\nline two\nthe failing tail line\n"
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(fullLog))
	}))
	t.Cleanup(blob.Close)

	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/55/logs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", blob.URL+"/log?token=presigned-secret")
			w.WriteHeader(http.StatusFound)
		},
	})
	// Loopback http blob host: relax the SSRF guard (test harness only).
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
	d.allowInsecureLogHost = true

	const maxBytes = 20
	tail, err := d.JobLogTail(context.Background(), 7, 55, maxBytes)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if len(tail) > maxBytes {
		t.Fatalf("tail exceeds maxBytes: %d > %d", len(tail), maxBytes)
	}
	if !strings.HasSuffix(fullLog, tail) {
		t.Fatalf("tail is not the END of the log: %q", tail)
	}
	if !strings.Contains(tail, "failing tail line") {
		t.Errorf("tail should carry the concluding lines: %q", tail)
	}
}

// TestGitHubJobLogTailByteBoundsOversizedBlob is the issue-74 M1 regression pin for
// GitHub, whose blob GET was ALREADY transfer-bounded (fetchJobLog reads through
// io.LimitReader(maxTraceBytes+1)). A blob body LARGER than maxTraceBytes (16 MiB)
// must trip the fail-closed ceiling and return an error with an empty tail: the
// LimitReader stops the transfer, and the len>maxTraceBytes check errors rather than
// returning the truncated body.
func TestGitHubJobLogTailByteBoundsOversizedBlob(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("A", maxTraceBytes+1024)))
	}))
	t.Cleanup(blob.Close)

	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/55/logs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", blob.URL+"/log?token=presigned-secret")
			w.WriteHeader(http.StatusFound)
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
	d.allowInsecureLogHost = true

	tail, err := d.JobLogTail(context.Background(), 7, 55, 32*1024)
	if err == nil {
		t.Fatal("an oversized blob body must trip the ceiling error, got nil")
	}
	if tail != "" {
		t.Fatalf("the ceiling error must return an empty tail, got %d bytes", len(tail))
	}
}

// TestGitHubJobLogTailNoAuthHeader pins item-16: the blob GET carries NO
// Authorization header (the pre-signed URL must not receive the PAT).
func TestGitHubJobLogTailNoAuthHeader(t *testing.T) {
	var blobAuth string
	blobHit := false
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobHit = true
		blobAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok log body"))
	}))
	t.Cleanup(blob.Close)

	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/55/logs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", blob.URL+"/log?token=presigned")
			w.WriteHeader(http.StatusFound)
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
	d.allowInsecureLogHost = true

	if _, err := d.JobLogTail(context.Background(), 7, 55, 0); err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if !blobHit {
		t.Fatal("blob was never fetched")
	}
	if blobAuth != "" {
		t.Fatalf("the blob GET must carry NO Authorization header, got %q", blobAuth)
	}
}

// TestGitHubJobLogTailRefusesFurtherRedirect pins item-16: a FURTHER redirect
// from the blob host is not followed.
func TestGitHubJobLogTailRefusesFurtherRedirect(t *testing.T) {
	secondHit := false
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHit = true
		_, _ = w.Write([]byte("should never be read"))
	}))
	t.Cleanup(second.Close)
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", second.URL+"/next")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(blob.Close)

	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/55/logs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", blob.URL+"/log")
			w.WriteHeader(http.StatusFound)
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
	d.allowInsecureLogHost = true

	_, err := d.JobLogTail(context.Background(), 7, 55, 0)
	if err == nil {
		t.Fatal("a further redirect from the blob host must not be followed (expected error)")
	}
	if secondHit {
		t.Fatal("the driver followed a further redirect cross-origin")
	}
}

// TestGitHubJobLogTailRejectsPrivateHost pins item-16/R5: a redirect to a
// loopback/private/non-https host is refused WITHOUT fetching the log.
func TestGitHubJobLogTailRejectsPrivateHost(t *testing.T) {
	blobHit := false
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		blobHit = true
		_, _ = w.Write([]byte("internal secret log"))
	}))
	t.Cleanup(blob.Close)

	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/55/logs": func(w http.ResponseWriter, _ *http.Request) {
			// blob.URL is http://127.0.0.1:port — loopback AND non-https, both of which
			// the production guard rejects.
			w.Header().Set("Location", blob.URL+"/log?token=presigned")
			w.WriteHeader(http.StatusFound)
		},
	})
	// Default driver: allowInsecureLogHost stays false, so the guard is ACTIVE.
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	_, err := d.JobLogTail(context.Background(), 7, 55, 0)
	if err == nil {
		t.Fatal("a loopback/non-https blob host must be refused")
	}
	if blobHit {
		t.Fatal("the log must NOT be fetched from a rejected host")
	}
	// The error must never carry the raw pre-signed URL (its query holds a secret).
	if strings.Contains(err.Error(), "presigned") {
		t.Fatalf("error leaked the pre-signed URL query: %q", err.Error())
	}
}

// TestIsDisallowedLogIP pins the second-hop SSRF host allowlist (R5), including
// RFC 6598 CGNAT (100.64.0.0/10), which net.IP.IsPrivate does NOT cover but which
// fronts internal services in some cloud/k8s fabrics.
func TestIsDisallowedLogIP(t *testing.T) {
	for _, tc := range []struct {
		ip         string
		disallowed bool
	}{
		{"127.0.0.1", true},         // loopback
		{"::1", true},               // loopback v6
		{"169.254.169.254", true},   // link-local (cloud metadata)
		{"10.0.0.1", true},          // RFC 1918
		{"192.168.1.1", true},       // RFC 1918
		{"100.64.1.1", true},        // CGNAT (RFC 6598)
		{"100.127.255.254", true},   // CGNAT upper edge
		{"::ffff:100.64.1.1", true}, // IPv4-mapped CGNAT
		{"0.0.0.0", true},           // unspecified
		{"224.0.0.1", true},         // multicast
		{"140.82.112.3", false},     // a real public GitHub-range address
		{"100.63.255.255", false},   // just below CGNAT
		{"100.128.0.0", false},      // just above CGNAT
	} {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tc.ip)
		}
		if got := isDisallowedLogIP(ip); got != tc.disallowed {
			t.Errorf("isDisallowedLogIP(%s) = %v, want %v", tc.ip, got, tc.disallowed)
		}
	}
}

func asStr(v any) string {
	if v == nil {
		return "nil"
	}
	return v.(string)
}
