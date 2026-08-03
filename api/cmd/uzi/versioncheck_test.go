package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

const skewURL = "https://uzi.example"

// skewEnv is fakeEnv plus the three things the check needs before it will do
// anything: the opt-in flag, a config store to cache into, and a throwaway skill
// home so nothing reaches the real ~/.claude.
func skewEnv(t *testing.T, fc uzicli.Client) Env {
	t.Helper()
	env := fakeEnv(fc)
	env.Store = uzicli.NewStore(t.TempDir())
	env.SkillHome = t.TempDir()
	env.CheckServerVersion = true
	return env
}

// behindServer is a server NEWER than the CLI the tests stamp, in the BARE wire
// form GET /api/version actually serves. Writing "v0.14.0" here would pre-normalise
// the server side and certify an implementation that forgets to.
func behindServer() *uzicli.FakeClient {
	return &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: "0.14.0", Founded: "2026-07-03"}}
}

// TestSkewWarningGoesToStderrNotStdout is the POSITIVE CONTROL for this file. A file
// made only of "no warning here" assertions passes against an implementation never
// wired into root.go at all.
//
// Both channels are asserted explicitly. A single "output contains" check covers
// exactly one channel while reading as though it covered both, and the whole reason
// the user chose this design is that --json consumers parse stdout.
func TestSkewWarningGoesToStderrNotStdout(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()

	out, errOut, code := runCLI(t, skewEnv(t, fc), "run", "list", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut, "is behind server 0.14.0") {
		t.Fatalf("stderr carries no skew warning; the hook is not wired:\n%q", errOut)
	}
	if !strings.Contains(errOut, "brew upgrade uzi-cli") {
		t.Errorf("stderr warning is missing the remedy:\n%q", errOut)
	}
	if strings.Contains(out, "is behind") {
		t.Errorf("the warning landed on STDOUT, which corrupts every --json parser:\n%q", out)
	}
	if fc.BuildInfoCalls != 1 {
		t.Errorf("BuildInfoCalls = %d, want 1", fc.BuildInfoCalls)
	}
}

// The property the user's constraint is actually about: stdout stays PARSEABLE.
// Asserted as `json.Unmarshal succeeds` rather than as a negative string check —
// the negative form passes even when the warning lands on stdout in different
// wording, and this repo has a documented case of exactly that vacuity.
func TestSkewWarningLeavesJSONStdoutParseable(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()

	out, errOut, code := runCLI(t, skewEnv(t, fc), "run", "list", "--json", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut, "is behind server 0.14.0") {
		t.Fatalf("stderr carries no warning, so this test proves nothing about stdout:\n%q", errOut)
	}
	var any any
	if err := json.Unmarshal([]byte(out), &any); err != nil {
		t.Fatalf("stdout is not valid JSON with the warning in play: %v\n%q", err, out)
	}
}

// The exit code is pinned DIFFERENTIALLY — the same invocation with the check off
// and on — on a success path AND a not-found path. A hardcoded `== 0` would go stale
// and would say nothing about whether the warning can mask a failing command.
func TestSkewWarningLeavesExitCodeUnchanged(t *testing.T) {
	withVersion(t, "v0.11.8")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"success path", []string{"run", "list"}},
		{"not found path", []string{"run", "get", "00000000-0000-0000-0000-000000000000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "--url", skewURL)

			off := skewEnv(t, behindServer())
			off.CheckServerVersion = false
			_, _, codeOff := runCLI(t, off, args...)

			onFC := behindServer()
			on := skewEnv(t, onFC)
			_, errOn, codeOn := runCLI(t, on, args...)

			if !strings.Contains(errOn, "is behind server") {
				t.Fatalf("the check-on run printed no warning, so the two codes are not being "+
					"compared under different conditions:\n%q", errOn)
			}
			if codeOn != codeOff {
				t.Errorf("exit code changed with the version check on: %d -> %d", codeOff, codeOn)
			}
		})
	}
}

// --quiet suppresses the WORK, not merely the print. Asserting BuildInfoCalls == 0
// is what separates those two claims; a stderr-is-empty assertion alone passes
// against an implementation that probes every command and stays silent.
func TestSkewWarningQuietSuppressesTheProbe(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()

	_, errOut, code := runCLI(t, skewEnv(t, fc), "run", "list", "--quiet", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(errOut, "is behind") {
		t.Errorf("--quiet still printed the warning:\n%q", errOut)
	}
	if fc.BuildInfoCalls != 0 {
		t.Errorf("BuildInfoCalls = %d, want 0 — --quiet must skip the probe, not just the print", fc.BuildInfoCalls)
	}
}

func TestSkewWarningEnvVarDisablesTheProbe(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()
	t.Setenv("UZI_VERSION_CHECK", "0")

	_, errOut, code := runCLI(t, skewEnv(t, fc), "run", "list", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(errOut, "is behind") {
		t.Errorf("UZI_VERSION_CHECK=0 still printed the warning:\n%q", errOut)
	}
	if fc.BuildInfoCalls != 0 {
		t.Errorf("BuildInfoCalls = %d, want 0", fc.BuildInfoCalls)
	}
}

// A `go build ./cmd/uzi` binary carries `dev`. It must make NO network call and
// touch NO file — that is what keeps this off the local dev loop and out of every
// existing test, and it is the difference between the correct implementation and the
// half-fixed one that tells every developer to run brew.
func TestSkewWarningDevBuildDoesNotProbe(t *testing.T) {
	// Deliberately no withVersion: `version` stays "dev".
	fc := behindServer()

	_, errOut, code := runCLI(t, skewEnv(t, fc), "run", "list", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(errOut, "is behind") {
		t.Errorf("a dev build was told to brew upgrade:\n%q", errOut)
	}
	if fc.BuildInfoCalls != 0 {
		t.Errorf("BuildInfoCalls = %d, want 0 — the unstamped short-circuit must precede the probe", fc.BuildInfoCalls)
	}
}

// No store means no home directory, so nowhere to cache — and an uncached probe on
// every command is the thing the design exists to avoid.
func TestSkewWarningNilStoreDoesNotProbe(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()
	env := skewEnv(t, fc)
	env.Store = nil

	if _, _, code := runCLI(t, env, "run", "list", "--url", skewURL); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.BuildInfoCalls != 0 {
		t.Errorf("BuildInfoCalls = %d, want 0", fc.BuildInfoCalls)
	}
}

func TestSkewWarningNoURLDoesNotProbe(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()

	// No --url, and runCLI clears $UZI_URL.
	if _, _, code := runCLI(t, skewEnv(t, fc), "run", "list"); code == uzicli.ExitOK {
		t.Log("run list without a URL returned 0")
	}
	if fc.BuildInfoCalls != 0 {
		t.Errorf("BuildInfoCalls = %d, want 0 — the no-URL check must precede the client build", fc.BuildInfoCalls)
	}
}

// The only test that proves the cache is WIRED TO THE HOOK rather than merely
// correct in isolation: two commands over one store, one probe, warning both times.
// The second half matters as much as the first — a cache that suppressed the warning
// on the cache hit would satisfy the call count and defeat the feature.
func TestSkewWarningCacheHitAcrossCommands(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()
	env := skewEnv(t, fc)

	_, err1, _ := runCLI(t, env, "run", "list", "--url", skewURL)
	_, err2, _ := runCLI(t, env, "run", "list", "--url", skewURL)

	if fc.BuildInfoCalls != 1 {
		t.Errorf("BuildInfoCalls = %d across two commands, want 1", fc.BuildInfoCalls)
	}
	for i, e := range []string{err1, err2} {
		if !strings.Contains(e, "is behind server 0.14.0") {
			t.Errorf("command %d printed no warning:\n%q", i+1, e)
		}
	}
}

// A FAILED probe is cached too. Without that, a laptop with a URL configured and the
// server unreachable pays the probe timeout before every single command, forever —
// strictly worse than not having the feature.
func TestSkewWarningCachesAFailedProbe(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := &uzicli.FakeClient{BuildErr: uzicli.Exitf(uzicli.ExitUnreachable, "dial tcp: connection refused")}
	env := skewEnv(t, fc)

	for range 3 {
		runCLI(t, env, "run", "list", "--url", skewURL)
	}
	if fc.BuildInfoCalls != 1 {
		t.Errorf("BuildInfoCalls = %d across three commands, want 1 — a failed probe was not cached", fc.BuildInfoCalls)
	}
}

// Two servers, two truths. An unkeyed cache would apply one server's version to the
// other, silently and plausibly, and this warning is a factual claim about the server
// you are talking to.
func TestSkewWarningCacheIsPerServer(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()
	env := skewEnv(t, fc)

	runCLI(t, env, "run", "list", "--url", "https://a.example")
	runCLI(t, env, "run", "list", "--url", "https://b.example")

	if fc.BuildInfoCalls != 2 {
		t.Errorf("BuildInfoCalls = %d, want 2 — the second server reused the first's record", fc.BuildInfoCalls)
	}
}

// `uzi version` is RELOCATED, not exempt: it warns inline from the probe it was
// already making. One probe, not two, and stdout still begins with the CLI version
// (the Homebrew release constraint).
func TestSkewWarningVersionCommandWarnsInlineWithoutDoubleProbing(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()

	out, errOut, code := runCLI(t, skewEnv(t, fc), "version", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.BuildInfoCalls != 1 {
		t.Errorf("BuildInfoCalls = %d, want 1 — the hook and the command both probed", fc.BuildInfoCalls)
	}
	if !strings.Contains(errOut, "is behind server 0.14.0") {
		t.Errorf("`uzi version` printed no skew warning:\n%q", errOut)
	}
	if !strings.HasPrefix(out, "v0.11.8") {
		t.Errorf("stdout must still BEGIN with the stamped version; got:\n%q", out)
	}
	if strings.Contains(out, "is behind") {
		t.Errorf("the warning landed on stdout, breaking scripts/brew-local-test.sh:\n%q", out)
	}
}

// And it warms the cache from that live probe, so the next command hits it.
func TestSkewWarningVersionCommandWarmsTheCache(t *testing.T) {
	withVersion(t, "v0.11.8")
	fc := behindServer()
	env := skewEnv(t, fc)

	runCLI(t, env, "version", "--url", skewURL)
	_, errOut, _ := runCLI(t, env, "run", "list", "--url", skewURL)

	if fc.BuildInfoCalls != 1 {
		t.Errorf("BuildInfoCalls = %d, want 1 — `uzi version` did not warm the shared cache", fc.BuildInfoCalls)
	}
	if !strings.Contains(errOut, "is behind server 0.14.0") {
		t.Errorf("the following command printed no warning:\n%q", errOut)
	}
}

// The exempt commands, end to end. `logout` and `auth token` are the two that make
// NO network call today: the route is unauthenticated but the REQUEST carries the
// bearer token regardless, so probing there would ship the credential on the way to
// deleting it.
func TestSkewWarningExemptCommandsDoNotProbe(t *testing.T) {
	withVersion(t, "v0.11.8")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"skill status", []string{"skill", "status"}},
		{"logout", []string{"logout"}},
		{"completion request", []string{cobra.ShellCompRequestCmd, "run", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := behindServer()
			args := append(append([]string{}, tc.args...), "--url", skewURL)
			_, errOut, _ := runCLI(t, skewEnv(t, fc), args...)
			if fc.BuildInfoCalls != 0 {
				t.Errorf("BuildInfoCalls = %d, want 0", fc.BuildInfoCalls)
			}
			if strings.Contains(errOut, "is behind") {
				t.Errorf("an exempt command warned:\n%q", errOut)
			}
		})
	}
}

// The exemption predicate, over the REAL command tree. The end-to-end test above
// cannot distinguish "exempt" from "cobra never reached the hook for this command",
// which is exactly the case for the completion RPC.
func TestExemptFromVersionCheck(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))

	exempt := [][]string{
		{"version"},
		{"skill"}, {"skill", "status"}, {"skill", "install"},
		{"logout"},
		{"auth", "token"},
	}
	notExempt := [][]string{
		{"run"}, {"run", "list"}, {"run", "get"},
		{"login"},
		{"whoami"},
		{"auth", "status"},
		// `uzi token list` shares a leaf name with `uzi auth token` and must NOT
		// inherit its exemption.
		{"token"}, {"token", "list"},
	}
	for _, path := range exempt {
		if c := findPath(t, root, path); !exemptFromVersionCheck(c) {
			t.Errorf("%v should be exempt", path)
		}
	}
	for _, path := range notExempt {
		if c := findPath(t, root, path); exemptFromVersionCheck(c) {
			t.Errorf("%v should NOT be exempt", path)
		}
	}
	// Cobra's completion machinery is added during Execute, so name-check it
	// directly rather than looking it up in a tree that has not run.
	for _, name := range []string{cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd, "completion"} {
		if !exemptFromVersionCheck(&cobra.Command{Use: name}) {
			t.Errorf("%q should be exempt", name)
		}
	}
}

func findPath(t *testing.T, root *cobra.Command, path []string) *cobra.Command {
	t.Helper()
	c := root
	for _, name := range path {
		next := findCmd(c, name)
		if next == nil {
			t.Fatalf("command %v not found in the tree (at %q)", path, name)
		}
		c = next
	}
	return c
}

// 🔴 THE DISCRIMINATING SANITIZER FIXTURE, AND ITS INPUT IS NOT THE OBVIOUS ONE.
//
// Most attack payloads make the version string INVALID semver, so SkewWarning is
// silent and the payload never reaches stderr at all — a test built from those
// passes against a completely unsanitized implementation. What gets through is
// exactly a payload whose TRIMMED form is valid semver, because normSemver calls
// strings.TrimSpace: `\r`, `\n` and `\t` are unicode.IsSpace, so they are trimmed
// for the COMPARISON and survive into the PRINTED string.
//
// `\r` is the sharpest of them. Mid-line it returns the cursor to column 0 and the
// rest of the message overwrites uzi's own prefix, so an attacker sentence appears
// to come from uzi.
func TestSkewWarningSanitizesTheServerString(t *testing.T) {
	withVersion(t, "v0.11.8")

	for _, tc := range []struct {
		name    string
		version string
	}{
		{"trailing CR", "0.14.0\r"},
		{"leading CR", "\r0.14.0"},
		{"trailing newline", "0.14.0\n"},
		{"trailing tab", "0.14.0\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: tc.version, Founded: "2026-07-03"}}
			_, errOut, _ := runCLI(t, skewEnv(t, fc), "run", "list", "--url", skewURL)
			if !strings.Contains(errOut, "is behind server 0.14.0") {
				t.Fatalf("no warning printed, so this test proves nothing about sanitization:\n%q", errOut)
			}
			assertNoControlChars(t, errOut)
			// ONE line. A newline is spared by sanitizeTTY and skipped by
			// assertNoControlChars (it is the line separator), so without this the
			// newline row is a pin rather than evidence — an unsanitized `0.14.0\n`
			// splits the warning in two, leaving a second line that reads as a
			// standalone sentence uzi never wrote.
			if got := len(nonEmptyLines(errOut)); got != 1 {
				t.Errorf("stderr is %d lines, want exactly 1:\n%q", got, errOut)
			}
		})
	}
}

// The pre-existing sink, which is where every payload lands regardless of semver
// validity: `uzi version` prints the server's build info verbatim. Four executed
// attack shapes plus an unbounded string.
func TestVersionCommandSanitizesServerBuildInfo(t *testing.T) {
	withVersion(t, "v1.2.3")

	for _, tc := range []struct {
		name    string
		version string
	}{
		{"erase display", "0.14.0\x1b[2J\x1b[H"},
		{"line overwrite", "0.14.0\rWARNING: run curl evil.example/x to fix"},
		{"osc 8 hyperlink", "0.14.0\x1b]8;;https://evil.example\x07click\x1b]8;;\x07"},
		// Escaped rather than literal: a raw U+202E in source visually REVERSES the
		// rest of the line in every editor, so the file reads as something other
		// than what it is. staticcheck ST1018 flags it for exactly that reason.
		{"bidi override", "0.14.0\u202egnp.exe"},
		{"unbounded", strings.Repeat("A", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: tc.version, Founded: "2026-07-03"}}
			out, _, code := runCLI(t, skewEnv(t, fc), "version", "--url", skewURL)
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			assertNoControlChars(t, out)
			if len(out) > 4096 {
				t.Errorf("stdout is %d bytes; the server string is unbounded", len(out))
			}
		})
	}
}

// --json stays BYTE-EXACT: the structural encoder escapes what matters and agents
// decode it verbatim, so sanitizing there would corrupt payloads. This pins that the
// sanitizer sits on the human render path only.
func TestVersionJSONStaysUnsanitized(t *testing.T) {
	withVersion(t, "v1.2.3")
	const raw = "0.14.0\rspoof"
	fc := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: raw, Founded: "2026-07-03"}}

	out, _, code := runCLI(t, skewEnv(t, fc), "version", "--json", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got versionOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%q", err, out)
	}
	if got.Server == nil || got.Server.Version != raw {
		t.Fatalf("--json must carry the server string verbatim; got %+v", got.Server)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// assertNoControlChars fails if s carries anything a terminal would act on: C0/C1
// controls, DEL, or a Unicode format character (the bidi overrides among them).
// Newline is the line separator and tab is spared by sanitizeTTY by design; cellText
// folds tab to a space, so neither should reach a cell here.
func assertNoControlChars(t *testing.T, s string) {
	t.Helper()
	for i, r := range s {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			t.Errorf("control character %U at byte %d survived into output:\n%q", r, i, s)
			return
		}
	}
}
