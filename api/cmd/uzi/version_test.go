package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// withVersion stamps the package-level ldflags var for one test and restores it.
// Needed because the release value is what the brew constraint is written against
// and the test binary's default is "dev".
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

// fullBuild is a fully-stamped server reply.
func fullBuild() apitypes.BuildInfoDTO {
	commits, uptime := 2107, int64(5400)
	return apitypes.BuildInfoDTO{
		Version:       "0.11.12",
		Founded:       "2026-07-03",
		BuiltAt:       "2026-07-28T09:15:00Z",
		Commit:        "366a282d52095312f54b99698b241ac872e20284",
		Commits:       &commits,
		UptimeSeconds: &uptime,
	}
}

// TestVersionStdoutBeginsWithCLIVersion is the Homebrew release gate, encoded.
//
// scripts/brew-local-test.sh does `case "$out" in "v$version"*)`, and a `case`
// pattern whose only wildcard is trailing is ANCHORED AT THE START of the whole
// captured stdout. That is stricter than Formula/uzi-cli.rb's assert_match, which
// is a substring test — so an output shaped `uzi CLI v1.2.3\n…` passes the formula
// and FAILS the script. Whatever else this command grows, stdout must begin with
// the stamped version.
func TestVersionStdoutBeginsWithCLIVersion(t *testing.T) {
	withVersion(t, "v1.2.3")

	for _, tc := range []struct {
		name string
		fc   *uzicli.FakeClient
		args []string
	}{
		{"no server configured", &uzicli.FakeClient{Build: fullBuild()}, []string{"version"}},
		{"server reachable", &uzicli.FakeClient{Build: fullBuild()}, []string{"version", "--url", "https://uzi.example"}},
		{"server unreachable", &uzicli.FakeClient{BuildErr: errors.New("dial tcp: refused")}, []string{"version", "--url", "https://uzi.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, code := runCLI(t, fakeEnv(tc.fc), tc.args...)
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d, want 0 — `uzi version` must never fail on server state", code)
			}
			if !strings.HasPrefix(out, "v1.2.3") {
				t.Fatalf("stdout must BEGIN with the stamped version (brew-local-test.sh anchors "+
					"its case pattern at the start of stdout); got:\n%q", out)
			}
		})
	}
}

// TestVersionNoServerConfiguredStaysOffline: with no URL, the command prints one
// line and reports nothing about a server — even though the fake would happily
// answer. That is what keeps the brew sandbox instantaneous: the URL is checked
// before a client is built, so there is no connection to time out.
func TestVersionNoServerConfiguredStaysOffline(t *testing.T) {
	withVersion(t, "v1.2.3")
	fc := &uzicli.FakeClient{Build: fullBuild()}

	out, _, code := runCLI(t, fakeEnv(fc), "version")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out, "server") {
		t.Errorf("reported server info with no URL configured — the canned reply was used, "+
			"so the no-URL short-circuit is not in force:\n%s", out)
	}
	if got := strings.TrimSpace(out); got != "v1.2.3" {
		t.Errorf("stdout = %q, want exactly the version line", got)
	}
}

// TestVersionReportsServerBuildInfo: every stamped coordinate reaches stdout.
func TestVersionReportsServerBuildInfo(t *testing.T) {
	withVersion(t, "v1.2.3")
	fc := &uzicli.FakeClient{Build: fullBuild()}

	out, _, code := runCLI(t, fakeEnv(fc), "version", "--url", "https://uzi.example")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"0.11.12",
		"366a282d52095312f54b99698b241ac872e20284", // full SHA, untruncated
		"2026-07-28T09:15:00Z",
		"2107",
		"2026-07-03",
		"1h30m0s", // 5400s
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestVersionServerUnreachableIsSilent: a failed probe prints no server lines and
// no error. The CLI's own version is the answer to `uzi version`; the server is a
// bonus, and a stack trace about a refused connection would be noise on a command
// people run to check what they have installed.
func TestVersionServerUnreachableIsSilent(t *testing.T) {
	withVersion(t, "v1.2.3")
	fc := &uzicli.FakeClient{BuildErr: errors.New("dial tcp 10.0.0.1:443: connect: connection refused")}

	out, errOut, code := runCLI(t, fakeEnv(fc), "version", "--url", "https://uzi.example")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(out); got != "v1.2.3" {
		t.Errorf("stdout = %q, want exactly the version line", got)
	}
	if strings.Contains(errOut, "refused") {
		t.Errorf("leaked the probe failure to stderr:\n%s", errOut)
	}
}

// TestVersionJSONWrapsServerUnderKey pins OQ-B's resolved shape: top-level
// `version` keeps its exact pre-#175 meaning — the CLI's own ldflags stamp — so
// existing --json parsers are untouched, and the server nests under `server`.
func TestVersionJSONWrapsServerUnderKey(t *testing.T) {
	withVersion(t, "v1.2.3")
	fc := &uzicli.FakeClient{Build: fullBuild()}

	out, _, code := runCLI(t, fakeEnv(fc), "version", "--json", "--url", "https://uzi.example")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}

	var got struct {
		Version string                 `json:"version"`
		Server  *apitypes.BuildInfoDTO `json:"server"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v (out=%s)", err, out)
	}
	if got.Version != "v1.2.3" {
		t.Errorf("top-level version = %q, want the CLI's own stamp %q", got.Version, "v1.2.3")
	}
	if got.Server == nil {
		t.Fatal("server key missing")
	}
	if got.Server.Version != "0.11.12" || got.Server.Commit != "366a282d52095312f54b99698b241ac872e20284" {
		t.Errorf("server payload wrong: %+v", *got.Server)
	}
	if got.Server.Commits == nil || *got.Server.Commits != 2107 {
		t.Errorf("server commits = %v, want 2107", got.Server.Commits)
	}
}

// TestVersionJSONPreservesUnknownFields is the reason M4 re-marshals the SHARED
// apitypes DTO instead of a CLI-local struct.
//
// An unstamped server omits commit/built_at/commits, and those must stay ABSENT
// here rather than reappear as "" and 0. A local struct with plain int fields would
// typecheck, look correct, and silently convert "we don't know" into "the value is
// zero" — undoing the distinction the server side spent a pointer to preserve. The
// assertion is on the raw KEYS because a typed decode cannot tell absent from zero,
// which is the same reason the server's own tests decode into a map.
func TestVersionJSONPreservesUnknownFields(t *testing.T) {
	withVersion(t, "v1.2.3")
	// A dev server: version + founded only, exactly what an unstamped build serves.
	fc := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: "dev", Founded: "2026-07-03"}}

	out, _, code := runCLI(t, fakeEnv(fc), "version", "--json", "--url", "https://uzi.example")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}

	var envelope struct {
		Server map[string]json.RawMessage `json:"server"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode: %v (out=%s)", err, out)
	}
	for _, absent := range []string{"commit", "built_at", "commits", "uptime_seconds"} {
		if raw, ok := envelope.Server[absent]; ok {
			t.Errorf("server.%s present as %s for an unstamped server, want it OMITTED — "+
				"a CLI-local struct with plain fields would do exactly this", absent, raw)
		}
	}
	for _, present := range []string{"version", "founded"} {
		if _, ok := envelope.Server[present]; !ok {
			t.Errorf("server.%s missing", present)
		}
	}
}

// TestVersionJSONOmitsServerKeyWhenUnreachable: no server, no key. An explicit
// null would make a consumer handle a third state for no benefit.
func TestVersionJSONOmitsServerKeyWhenUnreachable(t *testing.T) {
	withVersion(t, "v1.2.3")
	fc := &uzicli.FakeClient{BuildErr: errors.New("dial tcp: refused")}

	out, _, code := runCLI(t, fakeEnv(fc), "version", "--json", "--url", "https://uzi.example")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v (out=%s)", err, out)
	}
	if _, ok := got["server"]; ok {
		t.Errorf("server key present with an unreachable server, want it omitted:\n%s", out)
	}
	if _, ok := got["version"]; !ok {
		t.Errorf("version key missing:\n%s", out)
	}
}
