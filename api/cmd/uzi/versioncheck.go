package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// versionCheckEnabled reports whether this invocation may probe and warn.
//
// Three independent off switches, and they are NOT redundant:
//
//   - env.CheckServerVersion is the injection seam. DefaultEnv sets it true;
//     fakeEnv leaves it false, which is what keeps every pre-existing command test
//     from suddenly calling FakeClient.BuildInfo. Mirrors AutoUpgradeSkill exactly.
//   - --quiet is the user's "suppress non-essential output", and a version warning
//     is non-essential by definition. Note it suppresses the WORK, not just the
//     print: this gate sits above the probe.
//   - UZI_VERSION_CHECK=0 is the documented escape hatch, same `== "0"` test as
//     UZI_SKILL_AUTO_UPGRADE so the two behave identically for anyone who learns one.
func versionCheckEnabled(env Env, gf *globalFlags) bool {
	return env.CheckServerVersion && !gf.quiet && os.Getenv("UZI_VERSION_CHECK") != "0"
}

// exemptFromVersionCheck reports whether cmd must not trigger a version probe. It
// walks ancestors, the same shape as underSkillCmd.
//
// Each exemption has a reason and none of them is "it felt chatty":
//
//   - `version` is RELOCATED, not silenced. PersistentPreRun runs BEFORE RunE, so a
//     CACHED warning would print `behind server 0.13.0` on stderr and then stdout
//     would print `server version 0.14.0` from that command's own live probe — a
//     visible self-contradiction inside one invocation. It warns inline instead
//     (see version.go), from the probe it was already making.
//   - the `skill` subtree already has this exemption for the auto-upgrade hook.
//     These verbs touch only local files, and `uzi skill install` is machine-invoked
//     at EVERY Claude Code session start by the opt-in hook, where an extra stderr
//     line is pure noise in an agent's context.
//   - `__complete` / `__completeNoDesc` are cobra's shell-completion RPC, invoked on
//     every TAB. A 2s stall there is unacceptable and stderr during completion
//     corrupts the display in some shells.
//   - `completion` emits a script that is `eval`'d from a shell rc file, so the
//     warning would print at every shell start.
//   - `logout` and `auth token` are the two commands that make NO network call today
//     (both are pure env.Store operations). The route is unauthenticated but the
//     REQUEST carries `Authorization: Bearer uzc_…` regardless — newRequest attaches
//     it to every request whose client holds a token. So probing here would ship the
//     credential on the way to deleting it, and would make `uzi auth token` — built
//     so a credential never lands on argv — emit a request carrying the PREVIOUS
//     credential. `logout`'s own Short says it "does not revoke it server-side",
//     which a probe would quietly contradict.
//
// `uzi login` is deliberately NOT exempt: it is already on the network (device-auth
// flow), so it introduces no new credential path.
//
// `--help`, `--version` and a bare non-runnable parent need no exemption at all —
// cobra returns above the PersistentPreRun loop for each (command.go:918-993). One
// consequence worth knowing: `uzi --version` never warns while `uzi version` does.
func exemptFromVersionCheck(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "skill", "version", "completion", "logout",
			cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		case "token":
			// `uzi auth token` only. `uzi token list` is a different, network-bound
			// command that happens to share the leaf name.
			if p := c.Parent(); p != nil && p.Name() == "auth" {
				return true
			}
		}
	}
	return false
}

// maybeWarnVersionSkew warns on stderr when this binary is older than the server it
// is about to talk to. Best-effort in the same sense as maybeAutoUpgradeSkill: never
// fatal, never blocking, every failure silent.
//
// The exit code is untouched STRUCTURALLY rather than by assertion: this runs from
// PersistentPreRun (not PersistentPreRunE), which has no error return, so nothing it
// does can reach ExitCodeFor.
//
// Order matters and the two early returns are the reason this stays off the local
// dev loop entirely:
//
//	the unstamped-build check comes FIRST, so a `go build ./cmd/uzi` binary makes no
//	network call and touches no file;
//	the no-URL check comes before any client is built, so an unconfigured CLI costs
//	nothing — the same reasoning serverBuildInfo already documents for the brew
//	sandbox.
func maybeWarnVersionSkew(cmd *cobra.Command, env Env, gf *globalFlags) {
	if !versionCheckEnabled(env, gf) || exemptFromVersionCheck(cmd) {
		return
	}
	if !uzicli.IsStampedVersion(version) {
		return
	}
	// No store means no home directory, hence nowhere to cache — and an uncached
	// probe on every command is exactly what the cached design exists to avoid, so
	// skip the check entirely rather than degrade to it.
	if env.Store == nil {
		return
	}
	s, err := resolveSettings(env, gf)
	if err != nil || strings.TrimSpace(s.URL) == "" {
		return
	}

	now := time.Now()
	serverVersion, fresh := env.Store.CachedServerVersion(s.URL, now, uzicli.VersionCheckTTL)
	if !fresh {
		serverVersion = ""
		if info := serverBuildInfo(cmd.Context(), env, gf); info != nil {
			serverVersion = info.Version
		}
		// Records the ATTEMPT: an empty version is the negative cache entry that
		// stops an offline laptop re-probing on every command. The error is ignored
		// deliberately — a read-only $HOME must not break `uzi run list --json`.
		_ = env.Store.RecordServerVersion(s.URL, serverVersion, now)
	}
	warnVersionSkew(env, serverVersion)
}

// recordAndWarnVersionSkew is `uzi version`'s half: it warms the shared cache from
// that command's OWN live probe and prints the same warning the hook would have.
//
// It exists so the exemption costs nothing. `uzi version` already fetches the
// server's build info, so routing it through the cache means the pair
// (`uzi version`, then any other command) makes ONE network call, not two — and the
// warning a user sees is derived from the freshest possible reading rather than from
// a cache entry that could disagree with what the same screen is about to print.
//
// srv == nil is the every-failure case serverBuildInfo documents; it still records,
// because a failed probe is an observation worth caching (see versionCheckEntry).
func recordAndWarnVersionSkew(env Env, gf *globalFlags, srv *apitypes.BuildInfoDTO) {
	if !versionCheckEnabled(env, gf) {
		return
	}
	var serverVersion string
	if srv != nil {
		serverVersion = srv.Version
	}
	if env.Store != nil {
		if s, err := resolveSettings(env, gf); err == nil && strings.TrimSpace(s.URL) != "" {
			_ = env.Store.RecordServerVersion(s.URL, serverVersion, time.Now())
		}
	}
	warnVersionSkew(env, serverVersion)
}

// warnVersionSkew renders and prints the skew line, or prints nothing.
//
// 🔴 THIS IS WHERE THE SERVER'S STRING IS SANITIZED, AND IT IS AT PRINT TIME ON
// PURPOSE — after the cache read, never at fetch or at cache-write. The cache file
// is a plain file with no integrity protection, so anything able to write it
// controls this text with no network involved; a write-time sanitizer would be
// bypassed by exactly that path. Treat the cache as precisely as untrusted as the
// network response.
//
// The sink is real, not theoretical: GET /api/version passes the server's stamp
// through with no constraint at all (contrast Commit, gated by isFullSHA, and
// BuiltAt, gated by time.Parse), and a `\r` in it erases uzi's own prefix so an
// attacker sentence appears to come from uzi. cellText strips C0/C1/DEL and the
// Unicode format characters (bidi overrides among them) and caps the result, which
// closes the injection and the unbounded-length problem in one call. The precedent
// is run.go's RateLimitType, sanitized even though the server allowlists it to an
// enum, because "server-controlled today" is exactly the assumption that rots.
//
// Only the SERVER's string goes through it. The CLI's own version is a compile-time
// ldflags stamp, so it is not attacker-controlled and passing it through would only
// obscure that asymmetry.
//
// Print with fmt.Fprintf, NOT uzicli.Printer: govulncheck traces GO-2026-5970
// (x/text infinite loop on invalid input) through Printer.Println, and this path
// takes the most hostile string in the CLI. The plain path does not reach it.
func warnVersionSkew(env Env, serverVersion string) {
	if serverVersion == "" {
		return
	}
	if msg, ok := uzicli.SkewWarning(version, cellText(serverVersion)); ok {
		// Write error dropped explicitly: this whole path is best-effort and must
		// never affect the command it runs before.
		_, _ = fmt.Fprintln(env.Stderr, msg)
	}
}
