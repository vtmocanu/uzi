package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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

// 🔴 END-TO-END REGRESSION FOR THE ONE DEFECT THIS BRANCH SHIPPED: a transient outage
// used to silence the warning for the rest of the TTL, and the recovery did not
// restore it. Reproduced exactly as it was found — warn, blip, warn — and the third
// command is the assertion.
//
// The store-level test covers the mechanism; this covers the WIRING, which is where
// it actually bit: the hook took its own local `probed` variable rather than what the
// store handed back, so the fix was invisible from the library's own tests.
func TestSkewWarningSurvivesATransientProbeFailure(t *testing.T) {
	withVersion(t, "v0.11.8")
	env := skewEnv(t, nil)

	up := behindServer()
	env.NewClient = func(uzicli.Settings) uzicli.Client { return up }
	_, err1, _ := runCLI(t, env, "run", "list", "--url", skewURL)
	if !strings.Contains(err1, "is behind server 0.14.0") {
		t.Fatalf("baseline command did not warn, so the rest proves nothing:\n%q", err1)
	}

	// The server has a bad moment, and `uzi version` is the amplifier: it writes the
	// cache on every invocation regardless of freshness, so it is the most likely
	// thing to poison it — and the command a user runs when they suspect trouble.
	down := &uzicli.FakeClient{BuildErr: uzicli.Exitf(uzicli.ExitUnreachable, "dial tcp: connection refused")}
	env.NewClient = func(uzicli.Settings) uzicli.Client { return down }
	runCLI(t, env, "version", "--url", skewURL)

	// Server healthy again — but the cache is still fresh, so nothing re-probes. The
	// warning must come back from the preserved reading.
	env.NewClient = func(uzicli.Settings) uzicli.Client { return behindServer() }
	_, err3, _ := runCLI(t, env, "run", "list", "--url", skewURL)
	if !strings.Contains(err3, "is behind server 0.14.0") {
		t.Errorf("a transient probe failure silenced the warning for the rest of the TTL:\n%q", err3)
	}
}

// 🔴 THE OTHER HALF OF THE N-1 FIX, AND THE SUITE COULD NOT SEE IT.
//
// TestSkewWarningSurvivesATransientProbeFailure above drives its outage through the
// `uzi version` call site and then reads a FRESH cache, so the HOOK's own
// `serverVersion, _ = env.Store.RecordServerVersion(...)` is never exercised where
// its return value matters. Folding that line back to discarding the return left the
// entire suite green while silently losing the warning.
//
// The scenario that exercises it is the most ordinary instance of N-1 there is —
// the offline laptop the morning after:
//
//	STALE cache (TTL expired) + FAILED probe + a prior good reading
//
// Stale, so the hook probes rather than taking the cache-hit branch; failed, so the
// probe yields nothing; prior good reading, so there is something for the store to
// hand back that the hook must then USE rather than discard.
func TestSkewWarningStaleCachePlusFailedProbeKeepsWarning(t *testing.T) {
	withVersion(t, "v0.11.8")

	st := uzicli.NewStore(t.TempDir())
	// A good reading from two hours ago — stale under the 1h TTL.
	if _, err := st.RecordServerVersion(skewURL, "0.14.0", "v0.11.8", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// PRECONDITION, asserted rather than assumed. Were this entry still fresh the hook
	// would take the cache-hit branch and this test would pass while proving nothing —
	// the exact vacuity it exists to avoid.
	if v, fresh := st.CachedServerVersion(skewURL, time.Now(), uzicli.VersionCheckTTL); fresh {
		t.Fatalf("precondition failed: seeded entry still fresh (%q) — the probe path is not exercised", v)
	}

	down := &uzicli.FakeClient{BuildErr: uzicli.Exitf(uzicli.ExitUnreachable, "dial tcp: connection refused")}
	env := skewEnv(t, down)
	env.Store = st

	_, errOut, _ := runCLI(t, env, "run", "list", "--url", skewURL)
	if down.BuildInfoCalls != 1 {
		t.Errorf("BuildInfoCalls = %d, want 1 — a stale entry must be re-probed", down.BuildInfoCalls)
	}
	if !strings.Contains(errOut, "is behind server 0.14.0") {
		t.Errorf("stale cache + failed probe + prior good reading lost the warning:\n%q", errOut)
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
		{"auth status", []string{"auth", "status"}},
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

	// The membership rule is "makes no network call of its own". `auth status` is the
	// third member and was named by nobody until the rule was applied deliberately —
	// it is `resolveSettings` plus a print, with no client at all.
	exempt := [][]string{
		{"version"},
		{"skill"}, {"skill", "status"}, {"skill", "install"},
		{"logout"},
		{"auth", "token"}, {"auth", "status"},
	}
	notExempt := [][]string{
		{"run"}, {"run", "list"}, {"run", "get"},
		// `uzi login` builds its client with env.NewClient directly rather than
		// env.client, so the obvious grep reports it as local-only. It is on the
		// network (device-auth flow) and must stay unexempt.
		{"login"},
		// whoami merely LIVES in auth.go; it is the one client site in that file.
		{"whoami"},
		// `uzi token list` shares a leaf name with `uzi auth token` and must NOT
		// inherit its exemption.
		{"token"}, {"token", "list"},
		{"repo"}, {"repo", "list"},
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
// The four executed attack payloads (ESC[2J, a `\r` with a payload after it, OSC 8,
// U+202E) all make the version string INVALID semver, so SkewWarning is silent and
// none of them reaches stderr on THIS path at all. A fixture built from those passes
// against a completely unsanitized warning — they belong at the serverRows sink
// below, which has no validity guard and where all four do land.
//
// TWO classes get through here, and both were found by running the comparison rather
// than by reading it:
//
//  1. A payload whose TRIMMED form is valid semver. normSemver calls
//     strings.TrimSpace, and `\r`, `\n` and `\t` are unicode.IsSpace — so they are
//     stripped for the COMPARISON and survive verbatim into the PRINTED string.
//     `\r` is the sharpest: mid-line it returns the cursor to column 0 and the rest
//     of the message overwrites uzi's own prefix, so an attacker sentence appears to
//     come from uzi.
//  2. Unbounded length. SemVer build metadata is `[0-9A-Za-z-]` with NO length limit,
//     so `0.14.0+` followed by a megabyte of `A` is genuinely VALID semver, is
//     genuinely behind, and reaches the message in full.
//
// So the validity guard is not a sanitizer and must never be mistaken for one: it
// happens to reject one family, rejects neither of these, and protects only because
// it precedes the interpolation — a statement-ordering property a refactor loses in
// silence. cellText stays unconditional.
func TestSkewWarningSanitizesTheServerString(t *testing.T) {
	withVersion(t, "v0.11.8")

	for _, tc := range []struct {
		name    string
		version string
		// wantWarning is false for the unbounded row and that is not a weaker
		// assertion, it is a different one. cellText's 200-char cap runs BEFORE the
		// comparison, so the truncated string is no longer valid semver and the
		// verdict is silence — a strictly safer outcome than a truncated warning.
		// What that row pins is the byte count, and the mutation control (remove
		// cellText) turns it into a one-megabyte line on stderr.
		wantWarning bool
	}{
		{"trailing CR", "0.14.0\r", true},
		{"leading CR", "\r0.14.0", true},
		{"trailing newline", "0.14.0\n", true},
		{"trailing tab", "0.14.0\t", true},
		// 🔴 THE TRIM HALF OF THE CLASS. Every row above is ALSO stripped by
		// sanitizeTTY (they are unicode.IsControl), so all of them stay green with
		// the trimming removed entirely — the guard they appear to pin is in fact
		// unpinned by them. U+2028 (Zl) and U+00A0 (Zs) are NEITHER IsControl nor
		// Cf, so sanitizeTTY leaves them alone and TrimSpace is the only thing
		// standing between them and stderr. These two rows are the ones that
		// discriminate.
		{"trailing line separator", "0.14.0\u2028", true},
		{"trailing no-break space", "0.14.0\u00a0", true},
		// Valid semver by construction — the validity guard cannot help here.
		{"unbounded build metadata", "0.14.0+" + strings.Repeat("A", 1<<20), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: tc.version, Founded: "2026-07-03"}}
			_, errOut, _ := runCLI(t, skewEnv(t, fc), "run", "list", "--url", skewURL)
			// Matched on the PREFIX, not on "is behind server 0.14.0": the server
			// string is interpolated VERBATIM, so a leading-control payload renders
			// as "behind server \r0.14.0" and the fuller pattern misses a warning
			// that was printed. The row still caught its mutation with the fuller
			// pattern -- but it reported "no warning printed" when one had been,
			// which sends the next reader after the wrong mechanism.
			warned := strings.Contains(errOut, "is behind server")
			if warned != tc.wantWarning {
				t.Fatalf("warning printed = %v, want %v:\n%.200q", warned, tc.wantWarning, errOut)
			}
			assertNoControlChars(t, errOut)
			// ONE line. A newline is spared by sanitizeTTY and skipped by
			// assertNoControlChars (it is the line separator), so without this the
			// newline row is a pin rather than evidence — an unsanitized `0.14.0\n`
			// splits the warning in two, leaving a second line that reads as a
			// standalone sentence uzi never wrote.
			if got := len(nonEmptyLines(errOut)); got > 1 {
				t.Errorf("stderr is %d lines, want at most 1:\n%q", got, errOut)
			}
			// 🔴 THIS IS NOT WHAT CATCHES THE BUILD-METADATA ROW, despite reading
			// like it. That row aborts at the wantWarning t.Fatalf above when the
			// cap is removed, so this line never executes for the one input that
			// could exceed 4096 -- wantWarning is the real catcher. It is not dead
			// either: lowering the threshold to > 0 reddens every row that warns --
			// SIX of them here, at 96 bytes each, and not unbounded_build_metadata,
			// whose stderr is empty. So it runs only for rows that can never trip
			// it. (The tester measured FOUR for the same control at a4a18c0d; both
			// are right for their tree -- the two Zl/Zs rows landed after it ran.)
			// Kept as a cheap backstop for a future row that warns AND is long,
			// which is a combination no current row has.
			if len(errOut) > 4096 {
				t.Errorf("stderr is %d bytes; the server string reached it unbounded", len(errOut))
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

// 🔴 PINS A SAFETY PROPERTY THAT IS EMERGENT FROM STATEMENT ORDERING, WITH NOTHING
// ELSE HOLDING IT.
//
// compactText truncates with `s[:200]` — 200 BYTES, not runes — so a multi-byte rune
// straddling that boundary is cut in half and the tail is an orphan continuation
// byte. The output is nevertheless valid UTF-8, but ONLY because cellText runs its
// outer strings.Map AFTER that slice, and Map re-encodes the orphan as U+FFFD.
//
// Swap those two steps, or call compactText alone on this path, and uzi emits
// invalid UTF-8 derived from attacker-chosen input — with every existing assertion
// still green, because the bytes carry no control character and sit on one line.
// That is what this test exists for; it is not a duplicate of the control-character
// tests above.
//
// The payload puts the boundary inside a 3-byte rune deliberately: 199 ASCII bytes
// then U+20AC, so the slice keeps one byte of it.
func TestVersionCommandOutputStaysValidUTF8(t *testing.T) {
	withVersion(t, "v1.2.3")
	payload := strings.Repeat("A", 199) + strings.Repeat("€", 4)
	fc := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: payload, Founded: "2026-07-03"}}

	out, _, code := runCLI(t, skewEnv(t, fc), "version", "--url", skewURL)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "server version") {
		t.Fatalf("the payload never reached the render path, so this proves nothing:\n%.120q", out)
	}
	if !utf8.ValidString(out) {
		t.Errorf("stdout is not valid UTF-8 — the byte-slice truncation cut a rune and "+
			"nothing re-encoded the orphan:\n%.120q", out)
	}
}

// --json stays BYTE-EXACT, and the reason is the DESTINATION rather than the encoder:
// those bytes go to a parser, and sanitizing them would corrupt the payload an agent
// decodes. (encoding/json escapes C0 and U+2028/29 but NOT DEL, the C1 controls,
// U+202E or the zero-widths — so "json escapes what matters" is not the reason and
// must not be written down as one.) This pins that the sanitizer sits on the human
// render path only.
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

// assertNoControlChars fails if s carries anything a terminal would act on or that
// would silently restructure the line: C0/C1 controls, DEL, Unicode format characters
// (the bidi overrides among them), and any whitespace other than the plain ASCII
// space and the newline that separates lines.
//
// 🔴 THE WHITESPACE ARM IS NOT TIDINESS — it is what gives the U+2028 and U+00A0 rows
// any power at all. Those two are Zl/Zs: sanitizeTTY does not touch them, so they are
// held ONLY by TrimSpace, and a control-character-only assertion cannot see one
// surviving. Neither can the one-line check, since Go splits lines on \n and a
// U+2028 is not one.
//
// Every string this is used on is ASCII prose plus a version, so the plain space is
// the only whitespace that legitimately appears.
func assertNoControlChars(t *testing.T, s string) {
	t.Helper()
	for i, r := range s {
		if r == '\n' || r == ' ' {
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || unicode.IsSpace(r) {
			t.Errorf("control or exotic-whitespace character %U at byte %d survived into output:\n%q", r, i, s)
			return
		}
	}
}
