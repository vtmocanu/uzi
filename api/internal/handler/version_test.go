package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
)

// getVersion drives GET /api/version and returns the response as a raw key→value
// map. Deliberately NOT a typed decode: this endpoint's contract is that unknown
// fields are OMITTED rather than zero-valued (PRD #175), and a typed decode cannot
// tell an absent key from a present-but-empty one.
func getVersion(t *testing.T, h *Handler) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Version(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/version = %d, want 200", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

// wantKeys fails unless the response carries exactly these top-level keys.
func wantKeys(t *testing.T, body map[string]json.RawMessage, want ...string) {
	t.Helper()
	got := make([]string, 0, len(body))
	for k := range body {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("key set mismatch\n got: %v\nwant: %v", got, want)
	}
}

// wantString fails unless key holds exactly want.
func wantString(t *testing.T, body map[string]json.RawMessage, key, want string) {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		t.Fatalf("%q missing from response", key)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%q is not a string: %v (raw=%s)", key, err, raw)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

// TestVersionEndpointVersionKey pins the ONE part of this response that predates PRD
// #175 and must survive it byte for byte. web/src/pages/WorkersSettings.tsx feeds
// this key to PRD #113's worker upgrade classification, so a rename or a reshape is a
// coordinated change across the SPA and a feature that gates fleet upgrades — not a
// refactor. SetVersion's own semantics are pinned here too: an empty stamp (the
// plain-`go build` case) must not clobber whatever it already holds.
func TestVersionEndpointVersionKey(t *testing.T) {
	// Decoding through the pre-#175 shape, exactly as version_test did before the
	// widening and as any older consumer still does. Widening is additive: this must
	// keep working unchanged.
	get := func(h *Handler) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.Version(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/version = %d, want 200", rec.Code)
		}
		var body struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
		}
		return body.Version
	}

	// New defaults to "dev" — an un-stamped local/compose build.
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	if got := get(h); got != "dev" {
		t.Fatalf("default version = %q, want %q", got, "dev")
	}

	// A stamped value is reported verbatim (the release tag, incl. any leading v).
	h.SetVersion("v1.2.3")
	if got := get(h); got != "v1.2.3" {
		t.Fatalf("after SetVersion = %q, want %q", got, "v1.2.3")
	}

	// An empty stamp (ldflags var unset) leaves the last value untouched.
	h.SetVersion("")
	if got := get(h); got != "v1.2.3" {
		t.Fatalf("after SetVersion(\"\") = %q, want it unchanged (%q)", got, "v1.2.3")
	}
}

// TestVersionEndpointUnstamped is the COMMON case — every laptop, every compose
// stack, every MR validation image — and the one the omit-never-zero rule exists for.
// The assertion is on the key SET, not on values: `"commit": ""` would pass a
// value-based check while telling the consumer this build has an empty commit rather
// than an unknown one.
func TestVersionEndpointUnstamped(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)

	body := getVersion(t, h)
	wantKeys(t, body, "version", "founded", "uptime_seconds")
	wantString(t, body, "version", "dev")
	wantString(t, body, "founded", "2026-07-03")
}

// TestVersionEndpointStamped is the release-image case: every coordinate present and
// carried verbatim, with the full 40-char SHA untruncated (display truncation is the
// consumer's job — the served value stays greppable and linkable).
func TestVersionEndpointStamped(t *testing.T) {
	const sha = "366a282d52095312f54b99698b241ac872e20284"

	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetVersion("0.11.12")
	h.SetBuildInfo(BuildStamp{Commit: sha, BuiltAt: "2026-07-28T09:15:00Z", Commits: "2060"})

	body := getVersion(t, h)
	wantKeys(t, body, "version", "founded", "built_at", "commit", "commits", "uptime_seconds")
	wantString(t, body, "version", "0.11.12")
	wantString(t, body, "founded", "2026-07-03")
	wantString(t, body, "built_at", "2026-07-28T09:15:00Z")
	wantString(t, body, "commit", sha)
	if len(sha) != 40 {
		t.Fatalf("fixture broken: commit fixture is %d chars, want the full 40", len(sha))
	}

	var commits int
	if err := json.Unmarshal(body["commits"], &commits); err != nil {
		t.Fatalf("commits is not a JSON number: %v (raw=%s)", err, body["commits"])
	}
	if commits != 2060 {
		t.Fatalf("commits = %d, want 2060", commits)
	}
}

// TestVersionEndpointBuiltAtCanonicalised: a stamp carrying a numeric offset is
// re-formatted to UTC, so one wire spelling reaches every consumer whatever CI
// produced. Same instant, canonical rendering.
func TestVersionEndpointBuiltAtCanonicalised(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetBuildInfo(BuildStamp{BuiltAt: "2026-07-28T11:15:00+02:00"})

	wantString(t, getVersion(t, h), "built_at", "2026-07-28T09:15:00Z")
}

// TestVersionEndpointGarbageStampsOmitted: unknown beats wrong. An unexpanded CI
// variable, a `date` default-format string, or a non-numeric commit count is dropped
// rather than served half-decoded — which is what makes the two CI failure modes in
// .gitlab-ci.yml's publish:api comment safe by construction.
func TestVersionEndpointGarbageStampsOmitted(t *testing.T) {
	for _, tc := range []struct{ name, builtAt, commits string }{
		{"unexpanded ci variables", "$CI_JOB_STARTED_AT", "$UZI_COMMITS"},
		{"date default format", "Mon Jul 27 21:12:38 UTC 2026", "two thousand"},
		{"date only, no time", "2026-07-28", "2060.5"},
		{"negative count", "", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
			h.SetBuildInfo(BuildStamp{BuiltAt: tc.builtAt, Commits: tc.commits})

			body := getVersion(t, h)
			if _, ok := body["built_at"]; ok {
				t.Errorf("built_at present for %q, want it omitted (raw=%s)", tc.builtAt, body["built_at"])
			}
			if _, ok := body["commits"]; ok {
				t.Errorf("commits present for %q, want it omitted (raw=%s)", tc.commits, body["commits"])
			}
		})
	}
}

// TestVersionEndpointWhitespaceStampsTrimmed: a stamp that arrived with surrounding
// whitespace still parses. Belt-and-braces on a value that crosses a YAML string, a
// shell word-split and a Docker build arg before it reaches the linker.
func TestVersionEndpointWhitespaceStampsTrimmed(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetBuildInfo(BuildStamp{Commit: " abc123 ", BuiltAt: " 2026-07-28T09:15:00Z\n", Commits: " 7 "})

	body := getVersion(t, h)
	wantString(t, body, "commit", "abc123")
	wantString(t, body, "built_at", "2026-07-28T09:15:00Z")
	if string(body["commits"]) != "7" {
		t.Fatalf("commits = %s, want 7", body["commits"])
	}
}

// TestVersionEndpointZeroStartedAtOmitsUptime is the hazard PRD #175 M1 calls out and
// the reason uptime_seconds is a pointer. Many tests in this package construct a
// Handler as a struct literal rather than through New (see clock()), leaving startedAt
// the zero time — where now-startedAt is roughly two millennia. A build-info endpoint
// claiming ~63 billion seconds of uptime is worse than one that says nothing.
func TestVersionEndpointZeroStartedAtOmitsUptime(t *testing.T) {
	h := &Handler{version: "dev"}
	if !h.startedAt.IsZero() {
		t.Fatal("fixture broken: a struct-literal Handler must have a zero startedAt")
	}

	body := getVersion(t, h)
	wantKeys(t, body, "version", "founded")
	if _, ok := body["uptime_seconds"]; ok {
		t.Fatalf("uptime_seconds present on a zero-startedAt handler (raw=%s)", body["uptime_seconds"])
	}
}

// TestVersionEndpointUptime: with a real startedAt the field is present and counts
// forward, driven through the same injected clock the upgrade classification uses.
// Zero is asserted as PRESENT rather than omitted — that is exactly the value a bare
// int64 with omitempty would have swallowed.
func TestVersionEndpointUptime(t *testing.T) {
	start := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	now := start

	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.startedAt = start
	h.now = func() time.Time { return now }

	uptime := func() int64 {
		t.Helper()
		body := getVersion(t, h)
		raw, ok := body["uptime_seconds"]
		if !ok {
			t.Fatalf("uptime_seconds missing (body=%v)", body)
		}
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("uptime_seconds is not a JSON number: %v (raw=%s)", err, raw)
		}
		return n
	}

	if got := uptime(); got != 0 {
		t.Fatalf("uptime at t=0 = %d, want 0 present (not omitted)", got)
	}

	now = start.Add(90*time.Minute + 500*time.Millisecond)
	if got := uptime(); got != 5400 {
		t.Fatalf("uptime after 90m = %d, want 5400 (truncated to whole seconds)", got)
	}

	// A clock injected BEHIND startedAt reports the floor. Negative is never a truer
	// answer than zero, and it is not a shape any consumer should have to handle.
	now = start.Add(-time.Hour)
	if got := uptime(); got != 0 {
		t.Fatalf("uptime with a clock behind startedAt = %d, want 0", got)
	}
}

// TestVersionEndpointCarriesNothingPrivate is the standing trust assertion, not a
// value check. GET /api/version is unauthenticated AND unrate-limited, and in k8s it
// is reachable through an ingress published at path `/` with no auth annotation, so
// every key below is world-readable to anyone who can reach the deployment. Build-info
// endpoints conventionally leak hostnames, environment, paths and dependency
// inventories; this one carries none, so the key set is closed and adding to it is a
// deliberate act rather than a slip.
//
// Read the map as two classes, because they are not the same claim. Most entries are
// ALREADY public — the fact exists elsewhere in the open, and serving it here reveals
// nothing new. `uptime_seconds` is a considered DISCLOSURE: a runtime fact about this
// process, published because it is worth real debugging time and discloses no
// identity, topology or schedule. If you are adding a key, say which class it is in
// and why; if it is neither, it does not belong in this response.
func TestVersionEndpointCarriesNothingPrivate(t *testing.T) {
	public := map[string]bool{
		"version":  true, // already public: == the image tag, which is in the chart
		"founded":  true, // already public: a const date
		"built_at": true, // already public: when a public image was built
		"commit":   true, // already public: a SHA that is in the repo
		"commits":  true, // already public: a count over that repo
		// DISCLOSURE, not already-public. See the Version handler for the decision
		// and for what would require re-deciding it (an /about page, a signed-out
		// footer — any new surface widens the audience by default).
		"uptime_seconds": true,
	}

	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetVersion("0.11.12")
	h.SetBuildInfo(BuildStamp{
		Commit:  "366a282d52095312f54b99698b241ac872e20284",
		BuiltAt: "2026-07-28T09:15:00Z",
		Commits: "2060",
	})

	for k := range getVersion(t, h) {
		if !public[k] {
			t.Errorf("GET /api/version carries %q, which is not on the public-by-construction list. "+
				"This route is unauthenticated and unrate-limited: either the field is already public "+
				"(image tag, repo commit) and belongs on the list with a reason, or it does not belong "+
				"in this response at all.", k)
		}
	}
}
