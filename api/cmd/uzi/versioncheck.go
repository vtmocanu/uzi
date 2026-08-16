package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
// 🔴 THE MEMBERSHIP RULE IS "MAKES NO NETWORK CALL OF ITS OWN", NOT A LIST.
// The list below is the rule's current solution, not its definition, and stating it
// as a list is how the third member got missed: `logout` and `auth token` came from
// an audit, and `uzi auth status` has exactly the same property and was named by
// nobody until it was looked for on purpose.
//
// Probing from a command that otherwise makes no call is not merely surprising, it
// moves a CREDENTIAL. The /api/version ROUTE is unauthenticated, but the REQUEST
// carries `Authorization: Bearer uzc_…` regardless — newRequest attaches the token to
// every request whose client holds one and does not special-case this route. So a
// probe on `uzi logout` ships the credential on the way to deleting it, and
// contradicts that command's own Short ("does not revoke it server-side"); a probe on
// `uzi auth token` — built so a credential never lands on argv — emits a request
// carrying the PREVIOUS credential.
//
// 🔴 AND DO NOT RE-DERIVE THE SET WITH THE OBVIOUS GREP. `git grep -F 'env.client('`
// misses `uzi login`, which builds its client directly (`login.go:53`
// `c := env.NewClient(s)`), so that grep alone reports login as local-only and would
// exempt it for a reason that looks right. It is on the network (device-auth flow)
// and is NOT exempt. A file-level grep fails in the other direction too: `auth.go`
// appears in the `env.client(` list, but that one site belongs to `whoami`, which
// merely lives in the same file — both of `auth`'s own verbs are local.
//
// The derivation, per RunE rather than per file: every command file has as many
// client constructions as RunE bodies EXCEPT `auth.go` (3 RunE / 1 client — whoami),
// `login.go` (2 / 1 — login), and `skill.go` (4 / 0). That accounts for every
// local-only command in the tree, so the set below is exhaustive as of this commit.
// Re-run both greps when adding a command; a NEW command is not exempt by default,
// which is the safe direction.
//
// The remaining exemptions are not about credentials:
//
//   - `version` is RELOCATED, not silenced. PersistentPreRun runs BEFORE RunE, so a
//     CACHED warning would print `behind server 0.13.0` on stderr and then stdout
//     would print `server version 0.14.0` from that command's own live probe — a
//     visible self-contradiction inside one invocation. It warns inline instead
//     (see version.go), from the probe it was already making.
//   - the `skill` subtree already has this exemption for the auto-upgrade hook, and
//     `uzi skill install` is machine-invoked at EVERY Claude Code session start by
//     the opt-in hook, where an extra stderr line is pure noise in an agent context.
//   - `__complete` / `__completeNoDesc` are cobra's shell-completion RPC, invoked on
//     every TAB. A 2s stall there is unacceptable and stderr during completion
//     corrupts the display in some shells.
//   - `completion` emits a script that is `eval`'d from a shell rc file, so the
//     warning would print at every shell start.
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
		case "token", "status":
			// `uzi auth token` / `uzi auth status` ONLY. Both leaf names are reused
			// elsewhere in the tree by commands that do call the API — `uzi token
			// list` and `uzi skill status` — so the parent test is what keeps this
			// from over-matching. Enumerating the leaves rather than the whole
			// `auth` subtree also means a future network-bound `uzi auth <verb>` is
			// not silently exempted.
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
		var probed string
		if info := serverBuildInfo(cmd.Context(), env, gf); info != nil {
			probed = info.Version
		}
		// Records the ATTEMPT: an empty probe result is the negative cache entry that
		// stops an offline laptop re-probing on every command. The error is ignored
		// deliberately — a read-only $HOME must not break `uzi run list --json`.
		//
		// Take the RETURNED version, not `probed`. On a failed probe the store keeps
		// the last known-good reading and hands it back, so a transient outage does
		// not silence this warning for the rest of the TTL — see RecordServerVersion,
		// where that rule lives so both call sites inherit it.
		serverVersion, _ = env.Store.RecordServerVersion(s.URL, probed, version, now)
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
//
// 🔴 IT READS THE CACHE AS WELL AS WRITING IT, AND THAT IS THE POINT OF THE READ.
// This command probes live on every invocation — it has to, since it PRINTS the
// server's build info — so it was the one command that wrote the cache and never
// consulted it. That made `uzi version`, the command a user runs precisely when they
// suspect something is wrong, the one that stayed silent about skew exactly when the
// server was having a bad moment. Taking RecordServerVersion's returned version means
// a failed live probe falls back to the last known-good reading instead of to nothing.
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
			serverVersion, _ = env.Store.RecordServerVersion(s.URL, serverVersion, version, time.Now())
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
//
// 🔴 HOW MUCH ATTACKER TEXT CAN REACH stderr IS DECIDED IN run.go, NOT HERE.
// Measured: a server version carrying 150 characters of SemVer build metadata (157
// total) is printed, so ~157 characters of attacker-chosen text land on a line
// prefixed `uzi:` — stripped of controls and Cf by cellText, so spacing and letters,
// never a cursor effect. Above compactText's 200-char cap the truncation appends
// U+2026, which is outside SemVer's build-metadata charset, the version stops parsing
// and the warning vanishes entirely. That silence is therefore a property of a
// COSMETIC CONSTANT in another file: raise or remove that cap and this bound moves
// with it, with nothing here to notice.
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
