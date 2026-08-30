package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
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
// variable, a `date` default-format string, a non-numeric count or a string that is
// not a full SHA is dropped rather than served half-decoded — which is what makes the
// CI version-stamp failure modes (documented in the retired GitLab pipeline's
// publish:api comment) safe by construction.
//
// ALL THREE GATED STAMPS ARE COVERED HERE, and the third was the point of adding it.
// An earlier version of this table had no `commit` column at all while its first row
// was named "unexpanded ci variables" and its doc comment claimed to cover the CI
// failure modes — so the one field that did NOT degrade safely was the one field the
// table structurally could not reach, and the subtest's name is what stopped the next
// reader looking. `commit` had no validity gate then: it was served verbatim, so an
// unexpanded stamp put the literal "$CI_COMMIT_SHA" on the wire.
//
// `version` IS THE FOURTH LDFLAGS VALUE AND IS NOT COVERED, deliberately. It has no
// gate: SetVersion rejects only the empty string, so an unexpanded UZI_VERSION would
// be served verbatim — the exact failure the paragraph above describes for commit,
// still open for version. It is pre-existing rather than introduced by PRD #175, and
// widening this table would imply a gate that does not exist. Named because saying
// "all three stamps" while four values ride the same ldflags line is how the previous
// version of this comment went wrong, and reproducing that here of all places would
// be a poor joke.
func TestVersionEndpointGarbageStampsOmitted(t *testing.T) {
	for _, tc := range []struct{ name, commit, builtAt, commits string }{
		{"unexpanded ci variables", "$CI_COMMIT_SHA", "$CI_JOB_STARTED_AT", "$UZI_COMMIT_COUNT"},
		{"date default format", "not-a-sha", "Mon Jul 27 21:12:38 UTC 2026", "two thousand"},
		{"date only, no time", "", "2026-07-28", "2060.5"},
		{"negative count", "", "", "-1"},
		// A short SHA is the sharpest case: it is a real identifier, just not the one
		// this field promises. Serving it would make BuildInfoDTO.Commit's "full
		// 40-char" claim false and hand a consumer something safe to truncate again.
		{"short sha", "366a282d", "", ""},
		{"39 chars", "366a282d52095312f54b99698b241ac872e2028", "", ""},
		{"41 chars", "366a282d52095312f54b99698b241ac872e202844", "", ""},
		{"40 chars but not hex", "366a282d52095312f54b99698b241ac872e2028z", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
			h.SetBuildInfo(BuildStamp{Commit: tc.commit, BuiltAt: tc.builtAt, Commits: tc.commits})

			body := getVersion(t, h)
			if _, ok := body["commit"]; ok {
				t.Errorf("commit present for %q, want it omitted (raw=%s)", tc.commit, body["commit"])
			}
			if _, ok := body["built_at"]; ok {
				t.Errorf("built_at present for %q, want it omitted (raw=%s)", tc.builtAt, body["built_at"])
			}
			if _, ok := body["commits"]; ok {
				t.Errorf("commits present for %q, want it omitted (raw=%s)", tc.commits, body["commits"])
			}
		})
	}
}

// TestVersionEndpointCommitCasesAccepted pins isFullSHA's charset DECISION, which
// its doc comment argues for and nothing tested: tightening the gate to lowercase
// hex would have passed green, because every other commit fixture in this file is
// lowercase.
//
// Uppercase never arrives in practice — git and $CI_COMMIT_SHA both emit lowercase —
// which is exactly why the case needs a test rather than a reader's trust. Rejecting
// a valid hex SHA over its case would be a surprising failure with no upside, and
// "we never see it" is an argument for cheapness, not for silence.
func TestVersionEndpointCommitCasesAccepted(t *testing.T) {
	const lower = "366a282d52095312f54b99698b241ac872e20284"
	for _, tc := range []struct{ name, commit string }{
		{"lowercase", lower},
		{"uppercase", strings.ToUpper(lower)},
		{"mixed case", "366A282d52095312F54b99698B241ac872E20284"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
			h.SetBuildInfo(BuildStamp{Commit: tc.commit})

			// Served VERBATIM, not normalised: the stamp is an identifier the server
			// passes through, and case-folding it would be the server inventing a
			// value the build never carried.
			wantString(t, getVersion(t, h), "commit", tc.commit)
		})
	}
}

// TestVersionEndpointWhitespaceStampsTrimmed: a stamp that arrived with surrounding
// whitespace still parses. Belt-and-braces on a value that crosses a YAML string, a
// shell word-split and a Docker build arg before it reaches the linker.
//
// The commit fixture is a full 40-char SHA and must stay one: trimming is what has to
// happen BEFORE the length gate, so a padded-but-valid SHA is exactly the case that
// proves the two steps compose. (This test previously used "abc123", which pinned
// "any non-empty string is a valid commit" as behaviour.)
func TestVersionEndpointWhitespaceStampsTrimmed(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetBuildInfo(BuildStamp{
		Commit:  " 366a282d52095312f54b99698b241ac872e20284 ",
		BuiltAt: " 2026-07-28T09:15:00Z\n",
		Commits: " 7 ",
	})

	body := getVersion(t, h)
	wantString(t, body, "commit", "366a282d52095312f54b99698b241ac872e20284")
	wantString(t, body, "built_at", "2026-07-28T09:15:00Z")
	if string(body["commits"]) != "7" {
		t.Fatalf("commits = %s, want 7", body["commits"])
	}
}

// TestVersionEndpointZeroCommitsPresent: "0" renders as present-not-omitted, the same
// property uptime_seconds is asserted for. Unreachable in practice — a repo with no
// commits cannot build this binary — but Commits is a pointer for exactly this reason
// and the assertion is what stops someone "simplifying" it to a bare int with
// omitempty, which would silently drop the value.
func TestVersionEndpointZeroCommitsPresent(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetBuildInfo(BuildStamp{Commits: "0"})

	body := getVersion(t, h)
	raw, ok := body["commits"]
	if !ok {
		t.Fatalf("commits missing for a stamped \"0\", want it present (body=%v)", body)
	}
	if string(raw) != "0" {
		t.Fatalf("commits = %s, want 0", raw)
	}
}

// TestVersionEndpointStampedPrdCounts is the release-image case for the PRD counts
// (#245): both prds_done and prds_open present and carried as JSON numbers. Mirrors
// the commit-count coverage — the counts follow the exact `commits` path (computed in
// CI, stamped via ldflags), so they render the same way.
func TestVersionEndpointStampedPrdCounts(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetBuildInfo(BuildStamp{PrdsDone: "80", PrdsOpen: "32"})

	body := getVersion(t, h)
	for k, want := range map[string]int{"prds_done": 80, "prds_open": 32} {
		raw, ok := body[k]
		if !ok {
			t.Fatalf("%s missing for a stamped build, want it present (body=%v)", k, body)
		}
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("%s is not a JSON number: %v (raw=%s)", k, err, raw)
		}
		if n != want {
			t.Fatalf("%s = %d, want %d", k, n, want)
		}
	}
}

// TestVersionEndpointUnstampedPrdCountsOmitted: on an un-stamped build — every laptop,
// every compose stack, every MR validation image — both PRD counts are ABSENT, not
// zero. The same unknown-beats-wrong rule the rest of this endpoint obeys.
func TestVersionEndpointUnstampedPrdCountsOmitted(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)

	body := getVersion(t, h)
	if _, ok := body["prds_done"]; ok {
		t.Errorf("prds_done present on an un-stamped build, want it omitted (raw=%s)", body["prds_done"])
	}
	if _, ok := body["prds_open"]; ok {
		t.Errorf("prds_open present on an un-stamped build, want it omitted (raw=%s)", body["prds_open"])
	}
}

// TestVersionEndpointGarbagePrdCountsOmitted: unknown beats wrong for the PRD counts
// too. An unexpanded CI variable, a non-numeric value or a negative count is dropped
// rather than served — the same guard `commits` uses (strconv.Atoi, err == nil && n >=
// 0). The CI numeric shape-guard rejects most of these before the stamp, but the
// server refuses them regardless.
func TestVersionEndpointGarbagePrdCountsOmitted(t *testing.T) {
	for _, tc := range []struct{ name, prdsDone, prdsOpen string }{
		{"unexpanded ci variables", "$UZI_PRDS_DONE", "$UZI_PRDS_OPEN"},
		{"non-numeric", "eighty", "thirty-two"},
		{"negative", "-1", "-5"},
		{"fractional", "80.0", "32.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
			h.SetBuildInfo(BuildStamp{PrdsDone: tc.prdsDone, PrdsOpen: tc.prdsOpen})

			body := getVersion(t, h)
			if _, ok := body["prds_done"]; ok {
				t.Errorf("prds_done present for %q, want it omitted (raw=%s)", tc.prdsDone, body["prds_done"])
			}
			if _, ok := body["prds_open"]; ok {
				t.Errorf("prds_open present for %q, want it omitted (raw=%s)", tc.prdsOpen, body["prds_open"])
			}
		})
	}
}

// TestVersionEndpointHalfPresentPrdCounts: the two counts parse INDEPENDENTLY on the
// server — a stamped one renders whether or not its sibling did. The both-or-neither
// rule the PRD describes is a CONSUMER guarantee (the web row and `uzi version` require
// both), and it holds because the two are computed in one CI step and travel together;
// the server itself does not couple them, so this pins the honest per-field behaviour.
func TestVersionEndpointHalfPresentPrdCounts(t *testing.T) {
	t.Run("only done stamped", func(t *testing.T) {
		h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
		h.SetBuildInfo(BuildStamp{PrdsDone: "80"})

		body := getVersion(t, h)
		if _, ok := body["prds_done"]; !ok {
			t.Errorf("prds_done omitted when stamped alone, want it present (body=%v)", body)
		}
		if _, ok := body["prds_open"]; ok {
			t.Errorf("prds_open present when only done was stamped (raw=%s)", body["prds_open"])
		}
	})
	t.Run("only open stamped", func(t *testing.T) {
		h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
		h.SetBuildInfo(BuildStamp{PrdsOpen: "32"})

		body := getVersion(t, h)
		if _, ok := body["prds_open"]; !ok {
			t.Errorf("prds_open omitted when stamped alone, want it present (body=%v)", body)
		}
		if _, ok := body["prds_done"]; ok {
			t.Errorf("prds_done present when only open was stamped (raw=%s)", body["prds_done"])
		}
	})
}

// TestVersionEndpointZeroPrdCountsPresent: a real "0" renders as present-not-omitted,
// exactly like commits and uptime_seconds. A brand-new project with no completed PRDs
// has a KNOWN done count of 0, which is not the same as "unknown"; the pointer +
// `n >= 0` guard (not `> 0`) is what keeps that distinction on the wire.
func TestVersionEndpointZeroPrdCountsPresent(t *testing.T) {
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetBuildInfo(BuildStamp{PrdsDone: "0", PrdsOpen: "0"})

	body := getVersion(t, h)
	for _, k := range []string{"prds_done", "prds_open"} {
		raw, ok := body[k]
		if !ok {
			t.Fatalf("%s missing for a stamped \"0\", want it present (body=%v)", k, body)
		}
		if string(raw) != "0" {
			t.Fatalf("%s = %s, want 0", k, raw)
		}
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

// buildInfoJSONKeys returns every top-level JSON key apitypes.BuildInfoDTO can emit,
// by walking its struct tags.
//
// Reading TAGS rather than a marshalled response is the entire point of this helper,
// and the reason the guard below was rewritten. A rendered response contains only the
// keys its fixture happened to populate, so an `omitempty` field left at its zero
// value is INVISIBLE to any check over a response — and `omitempty` on an optional
// field is not an unlucky choice a mutation would have to contrive, it is what this
// endpoint's omit-never-zero rule REQUIRES of every new optional field. So the gap
// pointed the same way the house style does. A walk over the type sees a field whether
// or not anything set it.
//
// It recurses into embedded structs carrying no json name because encoding/json
// PROMOTES their exported fields to the top level: an embedded type's `secret` tag
// becomes a top-level `secret` key, while a flat field walk sees one field with an
// empty tag and reports `""`. The flat walk does still fail — `""` is not on any
// allowlist — but it fails naming the wrong thing, and a reader who sees a test
// complaining about an empty string diagnoses a broken test. Recursing makes the gate
// name the key that actually leaked. Not hypothetical in this package:
// RunListItemDTO embeds RunDTO and AdminWorkerDTO embeds WorkerDTO, so composing an
// authenticated shape into this public one is the idiomatic move here and therefore
// the likeliest way a leak arrives.
func buildInfoJSONKeys(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue // explicitly off the wire (note: `json:"-,"` names a field "-")
		}
		name, _, _ := strings.Cut(tag, ",")

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}

		// Embedded fields are handled BEFORE the unexported check, and that order is
		// the whole correctness of this helper. encoding/json's typeFields skips an
		// unexported field only when it is NOT an embedded struct — its own comment
		// reads "Do not ignore embedded fields of unexported struct types since they
		// may have exported fields". So `struct{ leaky }` promotes leaky's exported
		// fields onto the wire even though `leaky` is unexported, and a walk that
		// tests IsExported first skips the embed and reports nothing.
		//
		// Not a hypothetical ordering nit: the first version of this helper had the
		// check the other way round and MISSED an embedded unexported struct carrying
		// a `tls_cert_path`. The response-level pass in the caller is what caught it.
		if f.Anonymous {
			if name == "" && ft.Kind() == reflect.Struct {
				keys = append(keys, buildInfoJSONKeys(t, ft)...)
				continue
			}
			if !f.IsExported() && ft.Kind() != reflect.Struct {
				continue // encoding/json ignores embedded unexported non-struct types
			}
			// A named embed, or an embedded exported non-struct type: one key, under
			// the tag name or the type name. Reported even in the odd unexported-and-
			// named case — over-reporting fails loudly, under-reporting hides a leak.
			if name == "" {
				name = f.Name
			}
			keys = append(keys, name)
			continue
		}

		if !f.IsExported() {
			continue // unexported and not embedded: encoding/json never emits it
		}
		if name == "" {
			name = f.Name // untagged: encoding/json uses the Go field name verbatim
		}
		keys = append(keys, name)
	}
	return keys
}

// TestVersionEndpointCarriesNothingPrivate is the standing trust assertion, not a
// value check. GET /api/version is unauthenticated AND unrate-limited, and in k8s it
// is by default reachable through an ingress published at path `/` with no auth
// annotation, so every key below is world-readable to anyone who can reach the
// deployment. Build-info endpoints conventionally leak hostnames, environment, paths
// and dependency inventories; this one carries none, so the key set is closed and
// adding to it is a deliberate act rather than a slip.
//
// Read the map as two classes, because they are not the same claim. Most entries are
// ALREADY public — the fact exists elsewhere in the open, and serving it here reveals
// nothing new. `uptime_seconds` is a considered DISCLOSURE: a runtime fact about this
// process, published because it is worth real debugging time and discloses no
// identity, topology or schedule. If you are adding a key, say which class it is in
// and why; if it is neither, it does not belong in this response.
//
// The assertion runs over the TYPE's tags and not over a rendered response. The
// earlier version ranged over one response's keys and could therefore only reject keys
// that were PRESENT — three agents independently defeated it with an `omitempty` field
// populated from config (`oidc_issuer`, `db_host`, `tls_cert_path`), each of which
// rendered nothing under the `config.Config{}` every fixture here uses, and passed.
// The response-level pass is kept below because it costs nothing and guards this
// check's own premise: if the handler ever stops returning the DTO (a hand-built map,
// say), the tag walk would pass vacuously while the response check would not.
func TestVersionEndpointCarriesNothingPrivate(t *testing.T) {
	// One list, and the reason is the value rather than a comment so it cannot drift
	// away from the key it justifies.
	public := map[string]string{
		"version":   "already public: == the image tag, which is in the chart",
		"founded":   "already public: a const date",
		"built_at":  "already public: when a public image was built",
		"commit":    "already public: a SHA that is in the repo",
		"commits":   "already public: a count over that repo",
		"prds_done": "already public: a count over that repo (completed PRDs)",
		"prds_open": "already public: a count over that repo (active PRDs)",
		"uptime_seconds": "DISCLOSURE, not already-public: a runtime fact about this process. " +
			"See the Version handler for the decision and for what would require re-deciding " +
			"it (an /about page, a signed-out footer — any new surface widens the audience).",
		// PRD #836 M3. The tag walk records the named `latest` object as the single key
		// "latest" and does NOT recurse into its inner fields (version/name/… are
		// public-by-construction and unaudited here — see buildInfoJSONKeys). Exactly
		// these three top-level keys are added; the body/notes are admin-only and never
		// ride this endpoint.
		"latest":           "already public: the newest upstream release, published on github.com/vtmocanu/uzi",
		"update_available": "already public: a boolean derived from the public latest tag vs the public running version",
		"far_behind":       "already public: a boolean derived from the same public version delta (D4 heuristic)",
	}

	got := buildInfoJSONKeys(t, reflect.TypeOf(apitypes.BuildInfoDTO{}))
	if len(got) == 0 {
		t.Fatal("tag walk found no keys — the guard would pass vacuously; fix the walk, not this line")
	}

	seen := make(map[string]bool, len(got))
	for _, k := range got {
		seen[k] = true
		if _, ok := public[k]; !ok {
			t.Errorf("BuildInfoDTO can emit %q, which is not on the public-by-construction list.\n"+
				"GET /api/version is unauthenticated and unrate-limited, so this field would be "+
				"world-readable. Either it is already public (the image tag, a commit in the repo) "+
				"or a considered disclosure — in which case add it to the map WITH its reason and "+
				"record that reason where it is enforced, in Version's doc comment — or it does not "+
				"belong in this response at all.\n"+
				"If %q is an empty string, the field is an embedded struct: encoding/json promotes "+
				"its fields to the top level, so recurse into it or give it a json name.", k, k)
		}
	}

	// The reverse direction: an allowlist entry with no field behind it is a stale
	// reason, and a stale reason is what makes the next addition look pre-approved.
	for k := range public {
		if !seen[k] {
			t.Errorf("the public list carries %q but BuildInfoDTO has no such key — remove the "+
				"entry rather than leaving a reason for a field that no longer exists", k)
		}
	}

	// Premise guard: the handler must still be returning that type.
	h := New(nil, nil, config.Config{}, nil, nil, nil, nil, nil, nil)
	h.SetVersion("0.11.12")
	h.SetBuildInfo(BuildStamp{
		Commit:   "366a282d52095312f54b99698b241ac872e20284",
		BuiltAt:  "2026-07-28T09:15:00Z",
		Commits:  "2060",
		PrdsDone: "80",
		PrdsOpen: "32",
	})
	for k := range getVersion(t, h) {
		if _, ok := public[k]; !ok {
			t.Errorf("the rendered response carries %q, which the tag walk did not report. "+
				"TWO causes, and the second is the one that has actually happened here: either "+
				"the handler stopped serving apitypes.BuildInfoDTO, or the tag walk under-reports "+
				"— buildInfoJSONKeys missed an embedded unexported struct once already, by testing "+
				"IsExported before handling Anonymous fields. Check the walk before the handler.", k)
		}
	}
}
