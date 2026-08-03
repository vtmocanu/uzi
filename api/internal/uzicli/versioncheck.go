package uzicli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// normSemver puts a version string into the `vX.Y.Z` shape golang.org/x/mod/semver
// requires. It is deliberately the same two-line shape as forge/forgejo.go's
// checkForgejoVersion and workersvc/upgrade.go — a third copy, accepted rather than
// extracted, because extracting it would touch two working packages inside an
// issue-scoped MR. Each copy carries its own discriminating test; this one's is the
// table in versioncheck_test.go.
//
// 🔴 IT IS NOT COSMETIC. The two sides of this comparison ship in DIFFERENT shapes:
// the CLI is stamped `v0.14.0` (Formula/uzi-cli.rb's -X main.version=v#{version})
// and the server serves bare `0.14.0` (apitypes.BuildInfoDTO.Version — "the
// Dockerfile strips a leading v"). x/mod/semver treats every invalid version as
// equal to every other, and sorts an invalid one BELOW a valid one, so without this
// the whole feature is INERT: measured over a 5x5 grid of realistic pairs,
// semver.Compare(cli, server) < 0 fired on 0 of 25 rows — including `v0.1.0`
// against `99.0.0`. The CLI is always the valid operand and the server always the
// invalid one, so the naive gate cannot fire for anyone, ever.
func normSemver(v string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// IsStampedVersion reports whether v normalises to a version this package can
// compare. It exists so the CLI's PersistentPreRun hook can short-circuit on an
// unstamped build BEFORE resolving settings — a `go build ./cmd/uzi` binary carries
// `dev`, and the point of checking it first is that such a build then makes no
// network call and touches no file at all.
//
// Calling SkewWarning would also be silent on `dev`, but only AFTER the probe that
// produced the server's version, which is the cost this predicate removes.
func IsStampedVersion(v string) bool { return semver.IsValid(normSemver(v)) }

// SkewWarning reports whether the CLI is BEHIND the server, and the line to print.
// ok == false means print nothing.
//
// 🔴 THE CALLER MUST SANITIZE serverVersion BEFORE CALLING THIS, AT PRINT TIME.
// serverVersion is attacker-controlled: GET /api/version passes the server's stamp
// through with no constraint (contrast Commit, gated by isFullSHA, and BuiltAt,
// gated by time.Parse), and this string is printed to a TTY on every command. A
// `\r` in it erases uzi's own prefix and makes an attacker sentence appear to come
// from uzi. cmd/uzi's cellText is the sanitizer; it is in package main, which this
// package cannot import, so the guarantee lives at the call site by construction.
// Sanitizing here as well would be free defence-in-depth and is NOT done only
// because the stripper lives on the other side of that import edge.
//
// Every false arm is SILENT — the warning fails CLOSED. That direction is the whole
// difference between a fix and a regression, and it is the opposite of the named
// in-repo precedent: checkForgejoVersion REFUSES on an unparseable version, which is
// right for a feature gate at connect time and wrong here, where "refuse" maps to
// "warn" and would tell every developer running a `go build` binary to
// `brew upgrade`. Copy the SHAPE (re-prefix -> IsValid -> Compare), not the
// disposition.
func SkewWarning(cliVersion, serverVersion string) (string, bool) {
	cli, srv := normSemver(cliVersion), normSemver(serverVersion)
	// Both guards are load-bearing and they guard different populations: the CLI
	// side covers `dev` (every developer build and every test binary), the server
	// side covers a compose server, a pre-PRD-#175 server that sends no version at
	// all, and garbage from a hostile endpoint.
	if !semver.IsValid(cli) || !semver.IsValid(srv) {
		return "", false
	}
	// Ahead-or-equal is never an alert. A dev laptop pointed at a stable deployment
	// is the normal shape here, and there is nothing for the person at the keyboard
	// to do about a server being older than their CLI.
	if semver.Compare(cli, srv) >= 0 {
		return "", false
	}
	// Both versions render VERBATIM as each side reports them (`v0.11.8` against
	// `0.14.0`), normalised only for the comparison above. Normalising for display
	// would make this line disagree with `uzi version`, which prints the CLI's stamp
	// on line one and the server's bare string under `server version`.
	//
	// No `uzi <verb>` span appears in this text, and that is a constraint rather
	// than a style: instructions_test.go's extractor scans this package, lifts any
	// "uzi " + lowercase-letter span, and demands a registry entry asserting the
	// instruction has been EXECUTED. `uzi:` (colon) and `uzi-cli` (hyphen) each miss
	// that class by one character. If a future reword reddens that test, reword
	// again — never register.
	return fmt.Sprintf(
		"uzi: CLI %s is behind server %s; some fields may be missing. Run: brew upgrade uzi-cli",
		cliVersion, serverVersion), true
}

// VersionCheckTTL is how long a recorded probe outcome is reused.
//
// The failure direction is what makes an hour safe, and it is asymmetric: because
// the cache stores the SERVER's version and never the verdict (see
// versionCheckEntry), the CLI-upgrade direction self-heals INSTANTLY — the new
// binary re-reads its own version every run, so `brew upgrade uzi-cli` clears the
// warning on the very next command with no TTL wait. The only staleness a longer TTL
// buys is the other direction: the server is upgraded and we stay quiet for up to an
// hour. Silence-when-we-should-warn, never a false warning.
//
// 🔴 THE EXISTENCE OF A TTL IS THE MITIGATION; ITS VALUE IS NOT. /api/version has no
// rate limiter, so the load argument reads as though a shorter TTL were safer and a
// longer one were the real fix — it is neither. For a 50-agent fleet: no TTL is
// ~90,000 req/h, 1 min ~3,000, 1 h ~50, 24 h ~2. The three orders of magnitude are
// entirely between *no TTL* and *any TTL*; going 1h → 24h buys 24x against an
// already negligible baseline and pays for it with a 24x longer silence window.
//
// ONE VALUE GOVERNS BOTH OUTCOMES, and a shorter TTL for the negative entry is
// specifically rejected. A failed probe costs the full serverProbeTimeout (2s) where
// a success costs tens of milliseconds, so re-probing failures sooner maximises the
// cost of exactly the case it governs — the offline laptop this cache exists for.
const VersionCheckTTL = time.Hour

const (
	versionCheckFile = "version-check.json"
	// maxVersionCheckEntries bounds the map. --url is per-invocation, so a script
	// loop over many endpoints can otherwise grow this file without limit. 16 is not
	// load-bearing; only "not unbounded" is.
	maxVersionCheckEntries = 16
	// maxCachedVersionRunes bounds what a single entry can store. This is a STORAGE
	// bound, NOT the sanitizer — the security control is cellText at print time (see
	// SkewWarning), and a write-time stripper would be bypassed by anything that can
	// write this file directly. It exists because the only ceiling on the wire is
	// client.go's 32 MiB maxRespBytes: a hostile server returning a 1 MiB version
	// string would otherwise put it on disk and re-read it for the whole TTL.
	maxCachedVersionRunes = 256
)

// versionCheckState is the on-disk cache, keyed per server.
//
// KEYED PER SERVER because --url beats $UZI_URL beats the config file, so which
// server a given invocation talks to changes between one command and the next. A
// single blob would apply server A's version to server B — silently and plausibly,
// since both report real version strings — and this warning is a factual claim about
// the server you are talking to. Prod plus the dev cluster is the normal shape here,
// not a corner case.
type versionCheckState struct {
	Servers map[string]versionCheckEntry `json:"servers"`
}

// versionCheckEntry is one server's last probe ATTEMPT.
//
// Two properties, both easy to "optimise" away and each load-bearing:
//
// It records the ATTEMPT, not the success. Version == "" means "we probed and
// learned nothing" and is a FRESH record. Without that, a laptop with UZI_URL set
// and the server unreachable — VPN off, offline, compose stack down, dev cluster
// restarting — takes a cache miss on every single invocation and pays the 2s probe
// timeout before every command, forever. That is strictly worse than doing nothing.
//
// It records the SERVER'S VERSION, never the verdict. A cached `skew: true` is not
// cleared by `brew upgrade uzi-cli`, so the user would be told to upgrade for up to
// a TTL after they did. Recomputing against the live binary's own version each run
// self-heals, because the CLI side is what changed.
//
// CLIVersion is retained for HUMAN FORENSICS and is never read back — freshness keys
// on CheckedAt alone. Mirrors skillState (skill.go), which settled the identical
// question the identical way. Invalidating on a CLI-version change would be the
// right fix for a cache holding a VERDICT and is redundant here for the reason
// above: an observation cache already self-heals on upgrade, instantly. Recording it
// costs nothing and answers "which binary wrote this entry?" when someone is staring
// at the file wondering why they did or did not get a warning.
type versionCheckEntry struct {
	Version    string    `json:"version"`
	CheckedAt  time.Time `json:"checked_at"`
	CLIVersion string    `json:"cli_version,omitempty"`
}

func (s *Store) versionCheckPath() string { return filepath.Join(s.dir, versionCheckFile) }

// versionCheckKey derives the map key for a base URL.
//
// It is a HASH, not the URL, and that is a security property rather than tidiness:
// credentialSafeBase does not strip userinfo, so
// `--url http://alice:hunter2@127.0.0.1:8080` is accepted and served today. No write
// path currently persists a --url base at all (config.toml is written only by
// `uzi login`), so this cache would be the FIRST thing to put one on disk — in a
// 0644 file, following SaveConfig's precedent. Hashing means a password in a URL
// never reaches the filesystem while the per-server keying still works.
//
// The normalisation before hashing is deliberately cheap (trim, drop trailing
// slashes) rather than a net/url round-trip. The asymmetry that justifies it: a
// normalisation MISS costs a cache miss — one extra probe — and never a wrong
// answer, because two spellings of one host are simply two entries each holding
// that host's own truth.
func versionCheckKey(rawURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.TrimSpace(rawURL), "/")))
	return hex.EncodeToString(sum[:])
}

// loadVersionCheckState reads the cache, treating absent and corrupt alike as empty.
//
// A corrupt file is NOT deleted: the next successful write replaces it atomically,
// and an unreadable cache must never become an error a user sees. Mirrors
// LoadConfig's missing-file tolerance and the skill installer's recordedSHA.
func (s *Store) loadVersionCheckState() *versionCheckState {
	st := &versionCheckState{Servers: map[string]versionCheckEntry{}}
	b, err := os.ReadFile(s.versionCheckPath())
	if err != nil {
		return st
	}
	var got versionCheckState
	if err := json.Unmarshal(b, &got); err != nil || got.Servers == nil {
		return st
	}
	st.Servers = got.Servers
	return st
}

// CachedServerVersion returns the recorded server version for url and whether the
// record is fresh at now.
//
// version may be "" on a fresh record — that is the negative-cache case (the last
// probe failed) and the caller must NOT re-probe. `now` is a parameter rather than
// time.Now() inside so the clock cases are testable without a clock interface.
//
// A checked_at in the FUTURE is NOT fresh. The clock moved backwards, or the file
// was copied from another machine; either way the entry is untrustworthy and the
// right move is to re-probe and overwrite rather than to trust it until it "expires"
// at some point in the future.
func (s *Store) CachedServerVersion(url string, now time.Time, ttl time.Duration) (string, bool) {
	if s == nil {
		return "", false
	}
	e, ok := s.loadVersionCheckState().Servers[versionCheckKey(url)]
	if !ok || e.CheckedAt.After(now) || now.Sub(e.CheckedAt) >= ttl {
		return "", false
	}
	return e.Version, true
}

// RecordServerVersion stores the outcome of a probe ATTEMPT and returns the server
// version now IN EFFECT for url — the probed version when the probe succeeded, the
// preserved last-known-good when it failed, and "" when neither exists.
//
// 🔴 A FAILED PROBE MOVES checked_at AND DOES NOT CLEAR A KNOWN-GOOD VERSION. Getting
// this wrong is not a missed warning, it is a SILENT one for the full TTL, and it was
// live on this branch: a probe failure wrote "" over a real reading, and because an
// empty-and-fresh entry is indistinguishable from a real one to the caller, every
// later command took the cache-hit path, never re-probed, and printed nothing — for up
// to an hour AFTER the server came back. Measured: warn / (server down) / silent, with
// the server healthy again by the third command.
//
// Both properties survive, which is why the fix is here rather than at either caller.
// The TTL still suppresses re-probing, because checked_at moves on every attempt — so
// an offline laptop still pays one 2s probe per hour and not one per command. And the
// last real reading survives the outage, so the warning stays correct through it.
//
// The preserved version is kept regardless of how OLD the entry it came from is. A
// month offline therefore keeps warning against a month-old reading, which is
// deliberate: the value only moves when the SERVER is upgraded, and this comparison is
// recomputed against the live binary every run, so the direction that self-heals is
// the one users act on.
//
// The returned error is expected to be IGNORED by the CLI: a read-only $HOME must
// never break `uzi run list --json`. The effective version is returned even when the
// write fails, because it is correct for THIS invocation either way.
//
// cliVersion is taken as a PARAMETER rather than read here because this package has
// no access to the binary's ldflags stamp — the same reason NewSkillInstaller takes
// it. It is written and never read; see versionCheckEntry.
func (s *Store) RecordServerVersion(url, version, cliVersion string, now time.Time) (string, error) {
	if s == nil {
		return version, errors.New("no config store")
	}
	if r := []rune(version); len(r) > maxCachedVersionRunes {
		version = string(r[:maxCachedVersionRunes])
	}
	if r := []rune(cliVersion); len(r) > maxCachedVersionRunes {
		cliVersion = string(r[:maxCachedVersionRunes])
	}
	st := s.loadVersionCheckState()
	key := versionCheckKey(url)
	if version == "" {
		// Failed probe: keep whatever version we last learned for this server.
		if prev, ok := st.Servers[key]; ok {
			version = prev.Version
		}
	}
	st.Servers[key] = versionCheckEntry{
		Version:    version,
		CheckedAt:  now.UTC(),
		CLIVersion: cliVersion,
	}
	pruneVersionCheck(st.Servers)
	b, err := json.Marshal(st)
	if err != nil {
		return version, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return version, err
	}
	// 0644, like config.toml: this file holds a public version string and a hash.
	// Written through writeFileAtomic rather than os.WriteFile — the rename REPLACES
	// a symlink instead of following it, which a hand-rolled write would not.
	return version, writeFileAtomic(s.versionCheckPath(), b, 0o644)
}

// pruneVersionCheck drops the oldest entries until at most maxVersionCheckEntries
// remain.
func pruneVersionCheck(m map[string]versionCheckEntry) {
	if len(m) <= maxVersionCheckEntries {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Newest first; ties broken by key so the eviction is deterministic rather than
	// map-order dependent (which would make the test flaky rather than wrong).
	sort.Slice(keys, func(i, j int) bool {
		a, b := m[keys[i]].CheckedAt, m[keys[j]].CheckedAt
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.After(b)
	})
	for _, k := range keys[maxVersionCheckEntries:] {
		delete(m, k)
	}
}
