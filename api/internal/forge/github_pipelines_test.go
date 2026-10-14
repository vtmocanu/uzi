package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	gh "github.com/google/go-github/v92/github"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
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

// TestGitHubLatestPipelineNilRunMapsToErrNoPipeline is the issue-74 M2 pin: a
// hostile forge returning {"workflow_runs":[null],"total_count":1} decodes to a
// slice with a nil *WorkflowRun, which passes the len==0 guard. go-github's Get*
// accessors are nil-safe, so without the guard toGitHubPipeline would not panic
// (unlike gitlab/forgejo) — it would return a phantom zero-ID Pipeline with a nil
// error, which this test catches as the errors.Is check failing. With the guard the
// driver returns ErrNoPipeline.
func TestGitHubLatestPipelineNilRunMapsToErrNoPipeline(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{nil}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	if _, err := d.LatestPipeline(context.Background(), 7, "feature"); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a one-element run list with a null entry must map to ErrNoPipeline (no panic), got %v", err)
	}
}

// TestGitHubLatestMRPipelineNilRunMapsToErrNoPipeline is the issue-74 M2 pin for the
// MR path: the PR resolves with a head sha, then the runs endpoint returns a null
// entry. The nil-element guard returns ErrNoPipeline rather than a phantom zero-ID
// pipeline (go-github's Get* accessors are nil-safe, so this path returns a bad
// value rather than panicking as gitlab/forgejo would).
func TestGitHubLatestMRPipelineNilRunMapsToErrNoPipeline(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 4, "state": "open",
				"head": map[string]any{"sha": "headsha999"},
			})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{nil}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	if _, err := d.LatestMRPipeline(context.Background(), 7, 4); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a one-element MR run list with a null entry must map to ErrNoPipeline (no panic), got %v", err)
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

// checkRun builds one check-runs API entry. appSlug/suiteID/headSHA are optional
// (empty string / zero omit the sub-object or field).
func checkRun(id int, name, status string, conclusion any, htmlURL, appSlug string, suiteID int, suiteHeadSHA string) map[string]any {
	cr := map[string]any{
		"id": id, "name": name, "status": status, "conclusion": conclusion, "html_url": htmlURL,
	}
	if appSlug != "" {
		cr["app"] = map[string]any{"slug": appSlug}
	}
	if suiteID != 0 || suiteHeadSHA != "" {
		cs := map[string]any{}
		if suiteID != 0 {
			cs["id"] = suiteID
		}
		if suiteHeadSHA != "" {
			cs["head_sha"] = suiteHeadSHA
		}
		cr["check_suite"] = cs
	}
	return cr
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

// Both entry points must prefer the maximum Actions suite and preserve older
// check-run-derived native recovery when the preferred response is empty.
func TestGitHubActionsSuiteRecoveryOrder(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name        string
		runApp      string
		runSuite    int
		noRuns      bool
		noSuites    bool
		suiteID     int
		preferred   string
		older       string
		wantQueries []string
		wantID      int64
		wantStatus  string
		wantURL     string
		wantError   bool
		wantNone    bool
	}{
		{name: "newest-native", runApp: "github-actions", runSuite: 100, suiteID: 200, preferred: "native", older: "native", wantQueries: []string{"200"}, wantID: 2000, wantStatus: "failure", wantURL: "https://github.com/acme/widgets/actions/runs/2000"},
		{name: "newest-native-success", runApp: "github-actions", runSuite: 100, suiteID: 200, preferred: "native-success", older: "error", wantQueries: []string{"200"}, wantID: 2000, wantStatus: "success", wantURL: "https://github.com/acme/widgets/actions/runs/2000"},
		{name: "older-native", runApp: "github-actions", runSuite: 100, suiteID: 200, older: "native", wantQueries: []string{"200", "100"}, wantID: 1000, wantStatus: "success", wantURL: "https://github.com/acme/widgets/actions/runs/1000"},
		{name: "preferred-error", runApp: "github-actions", runSuite: 100, suiteID: 200, preferred: "error", older: "native", wantQueries: []string{"200"}, wantError: true},
		{name: "older-error", runApp: "github-actions", runSuite: 100, suiteID: 200, older: "error", wantQueries: []string{"200", "100"}, wantError: true},
		{name: "same-suite", runApp: "github-actions", runSuite: 200, suiteID: 200, wantQueries: []string{"200"}, wantID: 200, wantStatus: "success", wantURL: "check-url"},
		{name: "absent-run-suite", runApp: "github-actions", suiteID: 200, wantQueries: []string{"200"}, wantID: 200, wantStatus: "success", wantURL: "check-url"},
		{name: "non-actions-run", runApp: "external-ci", runSuite: 100, suiteID: 200, wantQueries: []string{"200"}, wantID: 200, wantStatus: "success", wantURL: "check-url"},
		{name: "zero-run-suite", runApp: "github-actions", suiteID: 200, wantQueries: []string{"200"}, wantID: 200, wantStatus: "success", wantURL: "check-url"},
		{name: "absent-runs", noRuns: true, suiteID: 200, wantQueries: []string{"200"}, wantNone: true},
		{name: "both-empty", runApp: "github-actions", runSuite: 100, suiteID: 200, wantQueries: []string{"200", "100"}, wantID: 200, wantStatus: "success", wantURL: "check-url"},
		{name: "nil-response", runApp: "github-actions", runSuite: 100, suiteID: 200, preferred: "null", older: "native", wantQueries: []string{"200", "100"}, wantID: 1000, wantStatus: "success", wantURL: "https://github.com/acme/widgets/actions/runs/1000"},
		{name: "nil-first", runApp: "github-actions", runSuite: 100, suiteID: 200, preferred: "nil-first", older: "native", wantQueries: []string{"200", "100"}, wantID: 1000, wantStatus: "success", wantURL: "https://github.com/acme/widgets/actions/runs/1000"},
		{name: "no-eligible", runApp: "external-ci", runSuite: 100, noSuites: true, wantID: 100, wantStatus: "success", wantURL: "check-url"},
		{name: "no-checks", noRuns: true, noSuites: true, wantNone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mr := range []bool{false, true} {
				name := "branch"
				if mr {
					name = "MR"
				}
				t.Run(name, func(t *testing.T) {
					var queries []string
					checkRef := "feature"
					if mr {
						checkRef = sha
					}
					m := newMockGitHub(t, map[string]http.HandlerFunc{
						"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
							writeJSON(w, map[string]any{"number": 4, "head": map[string]any{"sha": sha, "ref": "feature"}})
						},
						"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, r *http.Request) {
							suite := r.URL.Query().Get("check_suite_id")
							if suite == "" {
								if mr && r.URL.Query().Get("head_sha") != sha {
									t.Error("initial MR workflow query must use head SHA")
								}
								if !mr && r.URL.Query().Get("branch") != "feature" {
									t.Error("initial branch workflow query must use branch ref")
								}
								writeJSON(w, map[string]any{"workflow_runs": []any{}})
								return
							}
							queries = append(queries, suite)
							if r.URL.Query().Get("per_page") != "1" {
								t.Error("recovery must request one workflow")
							}
							response := tc.preferred
							id, status := 2000, "failure"
							if suite == "100" {
								response = tc.older
								id, status = 1000, "success"
							} else if suite != "200" {
								t.Errorf("unexpected suite query %q", suite)
							}
							switch response {
							case "error":
								w.WriteHeader(http.StatusInternalServerError)
								writeJSON(w, map[string]any{"message": "suite lookup failed"})
							case "native", "native-success":
								if response == "native-success" {
									status = "success"
								}
								writeJSON(w, map[string]any{"workflow_runs": []any{map[string]any{
									"id": id, "head_sha": sha, "head_branch": "feature",
									"status": "completed", "conclusion": status,
									"html_url": fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
								}}})
							case "null":
								writeJSON(w, nil)
							case "nil-first":
								writeJSON(w, map[string]any{"workflow_runs": []any{nil}})
							default:
								writeJSON(w, map[string]any{"workflow_runs": []any{}})
							}
						},
						"/repos/acme/widgets/commits/" + checkRef + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
							runs := []map[string]any{}
							if !tc.noRuns {
								cr := checkRun(11, "ci", "completed", "success", "check-url", tc.runApp, tc.runSuite, sha)
								cr["head_sha"] = sha
								switch tc.name {
								case "absent-run-suite":
									delete(cr, "check_suite")
								case "zero-run-suite":
									cr["check_suite"] = map[string]any{"id": 0, "head_sha": sha}
								}
								runs = append(runs, cr)
							}
							writeJSON(w, map[string]any{"check_runs": runs})
						},
						"/repos/acme/widgets/commits/" + sha + "/check-suites": func(w http.ResponseWriter, _ *http.Request) {
							suites := []map[string]any{}
							if !tc.noSuites {
								suites = append(suites, map[string]any{"id": tc.suiteID, "app": map[string]any{"slug": "github-actions"}, "head_sha": sha})
							}
							writeJSON(w, map[string]any{"check_suites": suites})
						},
						"/repos/acme/widgets/commits/feature/check-suites": func(w http.ResponseWriter, _ *http.Request) {
							if !tc.noRuns {
								t.Error("branch suites query must be pinned to check-run SHA")
							}
							suites := []map[string]any{}
							if !tc.noSuites {
								suites = append(suites, map[string]any{"id": tc.suiteID, "app": map[string]any{"slug": "github-actions"}, "head_sha": sha})
							}
							writeJSON(w, map[string]any{"check_suites": suites})
						},
					})
					d := newGitHubDriver(t, m, "dummy-token")
					var p Pipeline
					var err error
					if mr {
						p, err = d.LatestMRPipeline(context.Background(), 7, 4)
					} else {
						p, err = d.LatestPipeline(context.Background(), 7, "feature")
					}
					if tc.wantError {
						if err == nil || errors.Is(err, ErrNoPipeline) || !strings.Contains(err.Error(), "latest pipeline from checks") || !strings.Contains(err.Error(), "suite lookup failed") {
							t.Errorf("want wrapped workflow lookup error, got pipeline %+v, error %v", p, err)
						}
						if p != (Pipeline{}) {
							t.Errorf("error must not return synthesized pipeline: %+v", p)
						}
					} else if tc.wantNone {
						if !errors.Is(err, ErrNoPipeline) {
							t.Errorf("want ErrNoPipeline, got %+v, %v", p, err)
						}
					} else if err != nil || p.ID != tc.wantID || p.SHA != sha || p.Status != tc.wantStatus || p.WebURL != tc.wantURL {
						t.Errorf("pipeline = %+v, error = %v; want ID %d SHA %s status %s URL %s", p, err, tc.wantID, sha, tc.wantStatus, tc.wantURL)
					}
					if !reflect.DeepEqual(queries, tc.wantQueries) {
						t.Errorf("suite queries = %v, want %v", queries, tc.wantQueries)
					}
				})
			}
		})
	}
}

func TestNewestActionsSuiteIDFromChecks(t *testing.T) {
	run := func(id int64, app string) *gh.CheckRun {
		return &gh.CheckRun{App: &gh.App{Slug: new(app)}, CheckSuite: &gh.CheckSuite{ID: new(id)}}
	}
	suite := func(id int64, app string) *gh.CheckSuite {
		return &gh.CheckSuite{ID: new(id), App: &gh.App{Slug: new(app)}}
	}
	for _, tc := range []struct {
		name   string
		runs   []*gh.CheckRun
		suites []*gh.CheckSuite
		want   int64
		ok     bool
	}{
		{"suites-newer", []*gh.CheckRun{run(100, "github-actions")}, []*gh.CheckSuite{suite(200, "github-actions")}, 200, true},
		{"runs-newer", []*gh.CheckRun{run(200, "github-actions")}, []*gh.CheckSuite{suite(100, "github-actions")}, 200, true},
		{"equal", []*gh.CheckRun{run(200, "github-actions")}, []*gh.CheckSuite{suite(200, "github-actions")}, 200, true},
		{"runs-only", []*gh.CheckRun{run(100, "github-actions")}, nil, 100, true},
		{"suites-only", nil, []*gh.CheckSuite{suite(200, "github-actions")}, 200, true},
		{"ascending", []*gh.CheckRun{run(50, "github-actions"), run(100, "github-actions")}, []*gh.CheckSuite{suite(150, "github-actions"), suite(200, "github-actions")}, 200, true},
		{"reversed", []*gh.CheckRun{run(100, "github-actions"), run(50, "github-actions")}, []*gh.CheckSuite{suite(200, "github-actions"), suite(150, "github-actions")}, 200, true},
		{"app-guards", []*gh.CheckRun{run(900, "other"), run(800, ""), run(100, "github-actions")}, []*gh.CheckSuite{suite(700, "other"), suite(600, ""), suite(200, "github-actions")}, 200, true},
		{"zero-and-nil", []*gh.CheckRun{nil, {}, run(0, "github-actions")}, []*gh.CheckSuite{nil, {}, suite(0, "github-actions")}, 0, false},
		{"no-eligible", []*gh.CheckRun{run(100, "other")}, []*gh.CheckSuite{suite(200, "other")}, 0, false},
		{"empty", nil, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := newestActionsSuiteIDFromChecks(tc.runs, tc.suites)
			if id != tc.want || ok != tc.ok {
				t.Fatalf("got (%d, %v), want (%d, %v)", id, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestGitHubLatestMRPipelineRecoversActionsRunFromChecks is the reported repro
// (issue #1005): the head_sha-filtered workflow-runs list is empty, but the head
// commit's check-runs belong to a github-actions check-suite. The driver must
// re-query workflow-runs by that check_suite_id and return the NATIVE workflow run
// (its id in the workflow-run space, so ListPipelineJobs/JobLogTail keep working) —
// NOT ErrNoPipeline. FAILS on the pre-fix code (which returned ErrNoPipeline).
func TestGitHubLatestMRPipelineRecoversActionsRunFromChecks(t *testing.T) {
	const headSHA = "headsha999"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
		},
		// One path serves BOTH the head_sha (empty) and the check_suite_id (the run)
		// queries — branch on the query string.
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("check_suite_id") == "555" {
				writeJSON(w, map[string]any{"total_count": 1, "workflow_runs": []map[string]any{{
					"id": 900, "head_branch": "feature", "head_sha": headSHA,
					"status": "completed", "conclusion": "failure",
					"html_url": "https://github.com/acme/widgets/actions/runs/900",
				}}})
				return
			}
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 3, "check_runs": []map[string]any{
				checkRun(11, "linux failure", "completed", "failure", "u-linux", "github-actions", 555, headSHA),
				checkRun(12, "macos failure", "completed", "failure", "u-macos", "github-actions", 555, headSHA),
				checkRun(13, "ready success", "completed", "success", "u-ready", "github-actions", 555, headSHA),
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	p, err := d.LatestMRPipeline(context.Background(), 7, 4)
	if err != nil {
		t.Fatalf("LatestMRPipeline: %v", err)
	}
	if p.ID != 900 {
		t.Errorf("recovery must return the native workflow-run id 900, got %d", p.ID)
	}
	if p.SHA != headSHA {
		t.Errorf("SHA = %q, want %q", p.SHA, headSHA)
	}
	if !pipelinestatus.IsFailed(p.Status) {
		t.Errorf("status %q must classify as failed", p.Status)
	}
	if p.WebURL != "https://github.com/acme/widgets/actions/runs/900" {
		t.Errorf("WebURL = %q, want the native run url", p.WebURL)
	}
}

// TestGitHubLatestMRPipelineSynthesizesExternalCI covers external (non-Actions) CI:
// the check-runs belong to some_ci, not github-actions, so no workflow run is
// recoverable. The driver must SYNTHESIZE a neutral pipeline from the check-runs
// (status "failure", id = newest check-suite id, sha = head, WebURL = the failed
// check-run's page) rather than return ErrNoPipeline.
func TestGitHubLatestMRPipelineSynthesizesExternalCI(t *testing.T) {
	const headSHA = "extsha42"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 2, "check_runs": []map[string]any{
				checkRun(21, "lint", "completed", "success", "u-lint", "some-ci", 777, headSHA),
				checkRun(22, "build", "completed", "failure", "u-build-failed", "some-ci", 777, headSHA),
			}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-suites": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 2, "check_suites": []map[string]any{
				{"id": 770, "head_sha": headSHA},
				{"id": 777, "head_sha": headSHA},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	p, err := d.LatestMRPipeline(context.Background(), 7, 4)
	if err != nil {
		t.Fatalf("LatestMRPipeline: %v", err)
	}
	if p.Status != "failure" || !pipelinestatus.IsFailed(p.Status) {
		t.Errorf("status = %q, want failure (IsFailed)", p.Status)
	}
	if p.SHA != headSHA {
		t.Errorf("SHA = %q, want %q", p.SHA, headSHA)
	}
	if p.ID != 777 {
		t.Errorf("ID = %d, want the newest check-suite id 777", p.ID)
	}
	if p.WebURL != "u-build-failed" {
		t.Errorf("WebURL = %q, want the failed check-run's html_url", p.WebURL)
	}
}

// TestGitHubLatestMRPipelineSynthesisAllGreen: every external check-run succeeded →
// synthesized status "success" (IsSuccess).
func TestGitHubLatestMRPipelineSynthesisAllGreen(t *testing.T) {
	const headSHA = "greensha"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 2, "check_runs": []map[string]any{
				checkRun(31, "lint", "completed", "success", "u1", "some-ci", 888, headSHA),
				checkRun(32, "test", "completed", "success", "u2", "some-ci", 888, headSHA),
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	p, err := d.LatestMRPipeline(context.Background(), 7, 4)
	if err != nil {
		t.Fatalf("LatestMRPipeline: %v", err)
	}
	if p.Status != "success" || !pipelinestatus.IsSuccess(p.Status) {
		t.Errorf("status = %q, want success (IsSuccess)", p.Status)
	}
}

// TestGitHubLatestMRPipelineSynthesisAllNeutral: every external check-run is
// neutral/skipped (no failure, no pending, no attention, NO real success) → the
// synthesized status must be "neutral", which is NEITHER IsFailed nor IsSuccess. This
// pins the Edit-2 fix: the old `default: return "success"` false-greened such a head
// through mr_rework's GATE 1 (IsSuccess), an asymmetry with the native workflow-run
// path (which stores "neutral"/"skipped" verbatim, also not IsSuccess).
func TestGitHubLatestMRPipelineSynthesisAllNeutral(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []map[string]any
	}{
		{"all-skipped", []map[string]any{
			checkRun(61, "lint", "completed", "skipped", "u1", "some-ci", 950, "neutsha"),
			checkRun(62, "test", "completed", "skipped", "u2", "some-ci", 950, "neutsha"),
		}},
		{"all-neutral", []map[string]any{
			checkRun(63, "lint", "completed", "neutral", "u3", "some-ci", 951, "neutsha"),
			checkRun(64, "test", "completed", "neutral", "u4", "some-ci", 951, "neutsha"),
		}},
		{"neutral+skipped", []map[string]any{
			checkRun(65, "lint", "completed", "neutral", "u5", "some-ci", 952, "neutsha"),
			checkRun(66, "test", "completed", "skipped", "u6", "some-ci", 952, "neutsha"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const headSHA = "neutsha"
			runs := tc.runs
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
				},
				"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
				},
				"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"total_count": len(runs), "check_runs": runs})
				},
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
			p, err := d.LatestMRPipeline(context.Background(), 7, 4)
			if err != nil {
				t.Fatalf("LatestMRPipeline: %v", err)
			}
			if pipelinestatus.IsSuccess(p.Status) {
				t.Errorf("status %q must NOT be IsSuccess (all-neutral must not false-green)", p.Status)
			}
			if pipelinestatus.IsFailed(p.Status) {
				t.Errorf("status %q must NOT be IsFailed (nothing actually failed)", p.Status)
			}
			if p.Status != "neutral" {
				t.Errorf("status = %q, want neutral", p.Status)
			}
		})
	}
}

// TestGitHubLatestMRPipelineSynthesisPrecedence pins the combine precedence for the
// attention/pending mixes: neither classifies as failed or success.
func TestGitHubLatestMRPipelineSynthesisPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		second     map[string]any // the non-success check-run
		wantStatus string
	}{
		{"success+action_required", checkRun(42, "deploy", "completed", "action_required", "u", "some-ci", 900, "mixsha"), "action_required"},
		{"success+in_progress", checkRun(42, "build", "in_progress", nil, "u", "some-ci", 900, "mixsha"), "in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const headSHA = "mixsha"
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
				},
				"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
				},
				"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"total_count": 2, "check_runs": []map[string]any{
						checkRun(41, "lint", "completed", "success", "u", "some-ci", 900, headSHA),
						tc.second,
					}})
				},
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
			p, err := d.LatestMRPipeline(context.Background(), 7, 4)
			if err != nil {
				t.Fatalf("LatestMRPipeline: %v", err)
			}
			if p.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", p.Status, tc.wantStatus)
			}
			if pipelinestatus.IsFailed(p.Status) || pipelinestatus.IsSuccess(p.Status) {
				t.Errorf("status %q must be neither failed nor success", p.Status)
			}
		})
	}
}

// TestGitHubLatestMRPipelineHonestNoCI: empty workflow-runs AND empty check-runs AND
// empty check-suites → still ErrNoPipeline. The check-runs endpoint MUST be queried
// (asserted via checkRunsHit) so this genuinely exercises the new path — on the
// pre-fix code the endpoint is never hit.
func TestGitHubLatestMRPipelineHonestNoCI(t *testing.T) {
	const headSHA = "nocish"
	checkRunsHit := false
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			checkRunsHit = true
			writeJSON(w, map[string]any{"total_count": 0, "check_runs": []map[string]any{}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-suites": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "check_suites": []map[string]any{}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	if _, err := d.LatestMRPipeline(context.Background(), 7, 4); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("no workflow runs AND no checks must be ErrNoPipeline, got %v", err)
	}
	if !checkRunsHit {
		t.Fatal("the fix must consult the check-runs endpoint before giving up")
	}
}

// TestGitHubLatestPipelineBranchSynthesizesFromChecks is the branch-path (LatestPipeline)
// analog: empty branch workflow-runs + failing external check-runs whose head SHA is
// carried only on the associated check-suite. The driver must resolve the REAL head
// commit SHA from the suite (mr_rework staleness gate) and synthesize a failed pipeline.
func TestGitHubLatestPipelineBranchSynthesizesFromChecks(t *testing.T) {
	const branchHead = "branchhead999"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		// Branch path keys the check APIs on the branch ref "feature".
		"/repos/acme/widgets/commits/feature/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			// No top-level head_sha on the run; the SHA lives on its check-suite.
			writeJSON(w, map[string]any{"total_count": 1, "check_runs": []map[string]any{
				checkRun(51, "e2e", "completed", "failure", "u-e2e-failed", "some-ci", 1001, branchHead),
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	p, err := d.LatestPipeline(context.Background(), 7, "feature")
	if err != nil {
		t.Fatalf("LatestPipeline: %v", err)
	}
	if p.SHA != branchHead {
		t.Errorf("SHA = %q, want the resolved branch-head %q", p.SHA, branchHead)
	}
	if !pipelinestatus.IsFailed(p.Status) {
		t.Errorf("status %q must classify as failed", p.Status)
	}
	if p.ID != 1001 {
		t.Errorf("ID = %d, want the check-suite id 1001", p.ID)
	}
}

// TestGitHubLatestMRPipelineRecoversActionsRunFromSuiteOnly is the FINDING-2 pin: the
// head_sha-filtered workflow-runs list is empty AND the head's check-RUNS list is empty,
// but a github-actions check-SUITE is listed for the head. ACTIONS-RECOVERY must resolve
// the Actions suite id from the SUITES list (not only from check-runs), re-query
// workflow-runs by that suite id, and return the NATIVE workflow run — NOT ErrNoPipeline.
// FAILS on the pre-fix code, which read the suite id from check-runs only, skipped
// recovery, and returned ErrNoPipeline (len(runs)==0).
func TestGitHubLatestMRPipelineRecoversActionsRunFromSuiteOnly(t *testing.T) {
	const headSHA = "suiteonly777"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/4": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 4, "state": "open", "head": map[string]any{"sha": headSHA, "ref": "feature"}})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("check_suite_id") == "666" {
				writeJSON(w, map[string]any{"total_count": 1, "workflow_runs": []map[string]any{{
					"id": 900, "head_branch": "feature", "head_sha": headSHA,
					"status": "completed", "conclusion": "failure",
					"html_url": "https://github.com/acme/widgets/actions/runs/900",
				}}})
				return
			}
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		// Check-RUNS empty: the recoverable Actions suite is exposed ONLY via check-suites.
		"/repos/acme/widgets/commits/" + headSHA + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "check_runs": []map[string]any{}})
		},
		"/repos/acme/widgets/commits/" + headSHA + "/check-suites": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 1, "check_suites": []map[string]any{
				{"id": 666, "head_sha": headSHA, "app": map[string]any{"slug": "github-actions"}},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	p, err := d.LatestMRPipeline(context.Background(), 7, 4)
	if err != nil {
		t.Fatalf("LatestMRPipeline: %v", err)
	}
	if p.ID != 900 {
		t.Errorf("recovery must return the native workflow-run id 900, got %d", p.ID)
	}
	if p.SHA != headSHA {
		t.Errorf("SHA = %q, want %q", p.SHA, headSHA)
	}
	if !pipelinestatus.IsFailed(p.Status) {
		t.Errorf("status %q must classify as failed", p.Status)
	}
	if p.WebURL != "https://github.com/acme/widgets/actions/runs/900" {
		t.Errorf("WebURL = %q, want the native run url", p.WebURL)
	}
}

// TestGitHubLatestPipelineBranchPinsSuitesToCheckRunsCommit is the FINDING-1 pin: on the
// branch path the check-runs and check-suites fetches both key on a branch NAME, so a
// push landing between them makes check-runs describe commit A and check-suites commit B.
// The synthesized pipeline must be pinned to the check-RUNS commit — its SHA and suite id
// come from commit A — and the check-suites request must be made against the RESOLVED
// SHA, not the branch name. FAILS on the pre-fix code, which fetched suites by the branch
// ref and let commit B's newer suite id leak into the synthesized pipeline.
func TestGitHubLatestPipelineBranchPinsSuitesToCheckRunsCommit(t *testing.T) {
	const commitA = "commitaaaa111" // the commit the check-runs describe
	const commitB = "commitbbbb222" // the branch head after a mid-fetch push
	var suitesReqRef string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
		// Check-RUNS keyed on the branch ref describe commit A (external CI → synthesis).
		"/repos/acme/widgets/commits/feature/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"total_count": 1, "check_runs": []map[string]any{
				checkRun(51, "e2e", "completed", "failure", "u-e2e-failed", "some-ci", 1000, commitA),
			}})
		},
		// Branch-ref check-suites: what the BUGGY code queries — the branch advanced, so
		// this describes commit B and carries a NEWER suite id (2000).
		"/repos/acme/widgets/commits/feature/check-suites": func(w http.ResponseWriter, _ *http.Request) {
			suitesReqRef = "feature"
			writeJSON(w, map[string]any{"total_count": 1, "check_suites": []map[string]any{
				{"id": 2000, "head_sha": commitB},
			}})
		},
		// Resolved-SHA check-suites: what the FIXED code queries — commit A's suite (1000).
		"/repos/acme/widgets/commits/" + commitA + "/check-suites": func(w http.ResponseWriter, _ *http.Request) {
			suitesReqRef = commitA
			writeJSON(w, map[string]any{"total_count": 1, "check_suites": []map[string]any{
				{"id": 1000, "head_sha": commitA},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	p, err := d.LatestPipeline(context.Background(), 7, "feature")
	if err != nil {
		t.Fatalf("LatestPipeline: %v", err)
	}
	if suitesReqRef != commitA {
		t.Errorf("check-suites must be fetched by the resolved SHA %q, got %q", commitA, suitesReqRef)
	}
	if p.SHA != commitA {
		t.Errorf("SHA = %q, want the check-runs commit %q", p.SHA, commitA)
	}
	if p.ID != 1000 {
		t.Errorf("ID = %d, want commit A's suite id 1000 (not commit B's 2000)", p.ID)
	}
}

func asStr(v any) string {
	if v == nil {
		return "nil"
	}
	return v.(string)
}
