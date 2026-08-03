package uzicli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/semver"
)

// The three DELIBERATELY BROKEN reference implementations this suite is measured
// against. They are running code rather than a comment naming which rows matter,
// because a comment goes stale the day someone edits a row and a reference cannot.
//
//   - refNaive     — no normalisation, no guards. The shape everybody writes first.
//   - refUnguarded — normalises BOTH sides but drops the semver.IsValid guards.
//   - refDirection — normalises and guards, but compares `!= 0` instead of `< 0`.
//
// They sit at DIFFERENT STAGES AND FAIL IN OPPOSITE DIRECTIONS, which is the reason
// a suite proving only "it warns when behind" is worthless here: refNaive is INERT
// (it warns on nothing at all), so adding normalisation moves you to refUnguarded,
// which OVER-fires and tells every `go build` binary to run brew.
func refNaive(cli, srv string) bool { return semver.Compare(cli, srv) < 0 }

func refUnguarded(cli, srv string) bool { return semver.Compare(normSemver(cli), normSemver(srv)) < 0 }

func refDirection(cli, srv string) bool {
	c, s := normSemver(cli), normSemver(srv)
	if !semver.IsValid(c) || !semver.IsValid(s) {
		return false
	}
	return semver.Compare(c, s) != 0
}

// skewRow is one measured pair. The three kills* flags are what the row is FOR: a
// row that kills nothing is a documentation pin, not evidence, and is labelled as
// such rather than padded into the count.
type skewRow struct {
	name           string
	cli, srv       string
	want           bool
	killsNaive     bool
	killsUnguarded bool
	killsDirection bool
}

// skewRows are the literal shipped string shapes, not invented ones. The CLI side is
// stamped `v`-prefixed by Formula/uzi-cli.rb; the server side is the BARE wire form
// GET /api/version serves.
//
// 🔴 THE SERVER SIDE IS BARE ON PURPOSE AND IT IS THE WHOLE POINT OF THE FIXTURE.
// A "behind" row written ("v0.11.8","v0.14.0") passes against an implementation that
// forgets to normalise the server side, because the fixture already normalised it —
// so a natural-looking fixture certifies the bug it was written to catch.
var skewRows = []skewRow{
	// The live incident: a brew CLI three minors behind a deployed server.
	{"live incident", "v0.11.8", "0.14.0", true, true, false, false},
	{"unprefixed cli", "0.11.8", "0.14.0", true, true, false, false},
	{"equal", "v0.14.0", "0.14.0", false, false, false, false},
	// SemVer §10: build metadata is comparison-neutral. `+g<sha>` is NOT string
	// equality, so this row pins that the comparison is semantic.
	{"build metadata equal", "v0.14.0", "0.14.0+g2d60c57", false, false, false, false},
	{"behind with build metadata", "v0.14.0", "0.15.0+g1a2b3c4", true, true, false, false},
	// `dev` is root.go's default: every `go build ./cmd/uzi`, every test binary.
	{"dev cli", "dev", "0.14.0", false, false, true, false},
	{"dev server", "v0.14.0", "dev", false, false, false, false},
	{"empty server", "v0.14.0", "", false, false, false, false},
	{"cli ahead", "v0.15.0", "0.14.0", false, false, false, true},
	{"four-part cli", "v0.11.7.1", "0.14.0", false, false, true, false},
	{"four-part server", "v0.14.0", "0.14.0.1", false, false, false, false},
	// SemVer §11.3: a prerelease sorts BELOW its release, so an rc is behind.
	{"prerelease cli", "v0.14.0-rc1", "0.14.0", true, true, false, false},
	{"patch behind", "v0.14.0", "0.14.1", true, true, false, false},
}

func TestSkewWarning(t *testing.T) {
	for _, r := range skewRows {
		t.Run(r.name, func(t *testing.T) {
			msg, ok := SkewWarning(r.cli, r.srv)
			if ok != r.want {
				t.Fatalf("SkewWarning(%q, %q) ok = %v, want %v (msg %q)", r.cli, r.srv, ok, r.want, msg)
			}
			if !ok {
				if msg != "" {
					t.Errorf("silent verdict still produced a message: %q", msg)
				}
				return
			}
			// Assert on CONTENT, not shape: both versions must appear VERBATIM as
			// passed in (so the live-incident row carries `v0.11.8` and `0.14.0`,
			// not two normalised strings), and the remedy must be the one documented
			// install path.
			for _, want := range []string{r.cli, r.srv, "brew upgrade uzi-cli"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
			if strings.Contains(msg, "\n") {
				t.Errorf("message must be a single line, got %q", msg)
			}
		})
	}
}

// TestSkewWarningDifferential is the test that makes the table above evidence rather
// than decoration: each broken reference must disagree with `want` on EXACTLY the
// rows flagged for it. Exactly, not "at least N" — a count cannot see WHICH row it
// lost, and the rows that discriminate are not the ones a careful person writes
// first.
func TestSkewWarningDifferential(t *testing.T) {
	refs := []struct {
		name string
		fn   func(string, string) bool
		flag func(skewRow) bool
	}{
		{"naive", refNaive, func(r skewRow) bool { return r.killsNaive }},
		{"unguarded", refUnguarded, func(r skewRow) bool { return r.killsUnguarded }},
		{"direction", refDirection, func(r skewRow) bool { return r.killsDirection }},
	}
	for _, ref := range refs {
		t.Run(ref.name, func(t *testing.T) {
			var flagged int
			for _, r := range skewRows {
				disagrees := ref.fn(r.cli, r.srv) != r.want
				if want := ref.flag(r); disagrees != want {
					t.Errorf("row %q: reference %q disagrees=%v, table says %v",
						r.name, ref.name, disagrees, want)
				}
				if ref.flag(r) {
					flagged++
				}
			}
			// The vacuity guard. Without it, flipping every flag to false leaves a
			// differential that passes over a fixture discriminating nothing.
			if flagged == 0 {
				t.Errorf("no row kills reference %q: the fixture cannot detect that regression", ref.name)
			}
		})
	}
}

// TestSkewWarningNaiveIsInert pins the fact the whole design turns on, separately
// from the per-row differential above: against this project's real string shapes the
// un-normalised comparison does not merely get some rows wrong — it warns on NOTHING.
// Stated as its own assertion because "wrong on some pairs" and "cannot fire for
// anyone, ever" call for different amounts of alarm.
func TestSkewWarningNaiveIsInert(t *testing.T) {
	clis := []string{"v0.1.0", "v0.11.8", "v0.14.0", "v1.0.0", "v0.14.0-rc1"}
	srvs := []string{"0.14.0", "0.15.0", "1.0.0", "99.0.0", "0.14.1"}
	var fired, rows int
	for _, c := range clis {
		for _, s := range srvs {
			rows++
			if refNaive(c, s) {
				fired++
				t.Errorf("naive comparison fired on (%q, %q) — the premise has changed", c, s)
			}
		}
	}
	if rows == 0 {
		t.Fatal("empty grid")
	}
	t.Logf("naive comparison fired on %d of %d realistic pairs", fired, rows)
}

func TestIsStampedVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"dev", false},
		{"", false},
		{"v0.14.0", true},
		{"0.14.0", true},
		{" 0.14.0 ", true},
		{"v0.14.0-rc1", true},
		{"v0.14.0+g2d60c57", true},
		{"0.11.7.1", false},
	} {
		if got := IsStampedVersion(tc.in); got != tc.want {
			t.Errorf("IsStampedVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

const testURL = "https://uzi.example.com"

func TestVersionCacheRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", now); err != nil {
		t.Fatalf("record: %v", err)
	}
	v, fresh := s.CachedServerVersion(testURL, now.Add(time.Minute), VersionCheckTTL)
	if !fresh || v != "0.14.0" {
		t.Fatalf("got (%q, %v), want (\"0.14.0\", true)", v, fresh)
	}
}

func TestVersionCacheExpires(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if v, fresh := s.CachedServerVersion(testURL, now, VersionCheckTTL); fresh {
		t.Fatalf("a 2h-old record read fresh under a 1h TTL (version %q)", v)
	}
}

// A future timestamp is NOT fresh: the clock moved backwards, or the file was copied
// from another machine. Trusting it would suppress the warning until that future
// instant arrives.
func TestVersionCacheRejectsFutureTimestamp(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", now.Add(time.Hour)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if v, fresh := s.CachedServerVersion(testURL, now, VersionCheckTTL); fresh {
		t.Fatalf("a future-dated record read fresh (version %q)", v)
	}
}

// The one test a single-blob cache fails. Two servers, two truths; applying one
// server's version to the other would be silent and plausible, since both report
// real version strings.
func TestVersionCacheIsKeyedPerServer(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion("https://a.example", "0.14.0", "v0.11.8", now); err != nil {
		t.Fatalf("record: %v", err)
	}
	if v, fresh := s.CachedServerVersion("https://b.example", now, VersionCheckTTL); fresh {
		t.Fatalf("server B read server A's record: (%q, %v)", v, fresh)
	}
	if v, fresh := s.CachedServerVersion("https://a.example", now, VersionCheckTTL); !fresh || v != "0.14.0" {
		t.Fatalf("server A lost its own record: (%q, %v)", v, fresh)
	}
}

func TestVersionCacheKeyNormalisesTrailingSlash(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion("https://x.example/", "0.14.0", "v0.11.8", now); err != nil {
		t.Fatalf("record: %v", err)
	}
	if v, fresh := s.CachedServerVersion("https://x.example", now, VersionCheckTTL); !fresh || v != "0.14.0" {
		t.Fatalf("got (%q, %v), want a hit on the slash-free spelling", v, fresh)
	}
}

// The negative cache. A fresh record holding "" means "we probed and learned
// nothing"; without it an offline laptop pays the probe timeout on every command.
func TestVersionCacheRecordsFailedProbe(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion(testURL, "", "v0.11.8", now); err != nil {
		t.Fatalf("record: %v", err)
	}
	v, fresh := s.CachedServerVersion(testURL, now.Add(time.Minute), VersionCheckTTL)
	if !fresh {
		t.Fatal("a failed probe was not cached; every command would re-probe")
	}
	if v != "" {
		t.Fatalf("version = %q, want empty", v)
	}
}

// 🔴 THE REGRESSION TEST FOR THE ONE DEFECT THIS BRANCH SHIPPED AND THEN FIXED.
//
// A failed probe used to write "" over a real reading. Because an empty-and-fresh
// entry is indistinguishable from a real one to the caller, every later command took
// the cache-hit path, never re-probed, and printed nothing — for up to a full TTL
// AFTER the server recovered. Silence, not a wrong answer, which is why nothing
// caught it.
//
// Both halves are asserted, because the fix is only correct if it keeps BOTH: the
// known-good version survives the failure, AND checked_at still moves so the entry
// stays fresh and no re-probe storm replaces the outage.
func TestVersionCacheFailedProbeKeepsTheLastKnownVersion(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()

	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", now); err != nil {
		t.Fatalf("record success: %v", err)
	}
	// The probe fails a minute later.
	eff, err := s.RecordServerVersion(testURL, "", "v0.11.8", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if eff != "0.14.0" {
		t.Errorf("returned effective version %q, want %q — a transient failure erased a good reading", eff, "0.14.0")
	}
	v, fresh := s.CachedServerVersion(testURL, now.Add(2*time.Minute), VersionCheckTTL)
	if v != "0.14.0" {
		t.Errorf("cached version %q, want %q — the outage poisoned the entry", v, "0.14.0")
	}
	if !fresh {
		t.Error("entry is not fresh after a failed probe; every command would re-probe")
	}
}

// The other half of the same rule: with NO prior reading, a failed probe still
// records the negative entry. This is the offline-laptop case and it must not
// regress while fixing the one above.
func TestVersionCacheFailedProbeWithNoPriorStaysEmpty(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	eff, err := s.RecordServerVersion(testURL, "", "v0.11.8", now)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if eff != "" {
		t.Errorf("returned %q from a first-contact failure, want empty", eff)
	}
	v, fresh := s.CachedServerVersion(testURL, now.Add(time.Minute), VersionCheckTTL)
	if !fresh || v != "" {
		t.Fatalf("got (%q, %v), want (\"\", true) — the negative cache is gone", v, fresh)
	}
}

// A structurally valid file whose map is null. loadVersionCheckState handles it, but
// the corrupt-file test above covers only UNPARSEABLE bytes, so this arm was unpinned.
func TestVersionCacheNullServersMapIsAMiss(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, versionCheckFile), []byte(`{"servers":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL); fresh {
		t.Fatalf("null map read fresh: (%q, %v)", v, fresh)
	}
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", time.Now()); err != nil {
		t.Fatalf("record over a null map: %v", err)
	}
	if v, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL); !fresh || v != "0.14.0" {
		t.Fatalf("got (%q, %v) after writing over a null map", v, fresh)
	}
}

func TestVersionCacheCorruptFileIsAMiss(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, versionCheckFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL); fresh {
		t.Fatalf("corrupt cache read fresh: (%q, %v)", v, fresh)
	}
	// And a write over a corrupt file must still succeed.
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", time.Now()); err != nil {
		t.Fatalf("record over corrupt file: %v", err)
	}
	if v, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL); !fresh || v != "0.14.0" {
		t.Fatalf("got (%q, %v) after replacing a corrupt file", v, fresh)
	}
}

func TestVersionCacheIsBounded(t *testing.T) {
	s := NewStore(t.TempDir())
	base := time.Now().Add(-time.Minute)
	const n = maxVersionCheckEntries + 1
	for i := range n {
		// Ascending timestamps, so entry 0 is the oldest and is the one evicted.
		if _, err := s.RecordServerVersion(fmt.Sprintf("https://h%02d.example", i), "0.14.0", "v0.11.8", base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(), versionCheckFile))
	if err != nil {
		t.Fatal(err)
	}
	var st versionCheckState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("cache is not valid JSON: %v", err)
	}
	if len(st.Servers) > maxVersionCheckEntries {
		t.Errorf("cache holds %d entries, want at most %d", len(st.Servers), maxVersionCheckEntries)
	}
	now := time.Now()
	if _, fresh := s.CachedServerVersion("https://h00.example", now, VersionCheckTTL); fresh {
		t.Error("the OLDEST entry survived eviction")
	}
	if _, fresh := s.CachedServerVersion(fmt.Sprintf("https://h%02d.example", n-1), now, VersionCheckTTL); !fresh {
		t.Error("the NEWEST entry was evicted")
	}
}

// The key is a HASH, not the URL. credentialSafeBase does not strip userinfo, and
// this is the first write path that would persist a --url base at all — into a 0644
// file. A password in a URL must never reach the filesystem.
func TestVersionCacheDoesNotPersistTheURL(t *testing.T) {
	s := NewStore(t.TempDir())
	const hostile = "http://alice:hunter2@127.0.0.1:8080"
	if _, err := s.RecordServerVersion(hostile, "0.14.0", "v0.11.8", time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(), versionCheckFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "alice", "127.0.0.1"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("cache file leaks %q: %s", leak, b)
		}
	}
	// It must still work as a key.
	if v, fresh := s.CachedServerVersion(hostile, time.Now(), VersionCheckTTL); !fresh || v != "0.14.0" {
		t.Fatalf("hashed key lost its record: (%q, %v)", v, fresh)
	}
}

// cli_version is written for human forensics and NEVER read back: freshness keys on
// checked_at alone. Both halves are asserted, because the pair is the whole claim —
// recording it without keying on it is exactly what skillState decided for the skill
// sidecar, and an observation cache self-heals on upgrade without it.
func TestVersionCacheRecordsCLIVersionButDoesNotKeyOnIt(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", now); err != nil {
		t.Fatalf("record: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(), versionCheckFile))
	if err != nil {
		t.Fatal(err)
	}
	var st versionCheckState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	e, ok := st.Servers[versionCheckKey(testURL)]
	if !ok {
		t.Fatal("no entry recorded")
	}
	if e.CLIVersion != "v0.11.8" {
		t.Errorf("cli_version = %q, want %q", e.CLIVersion, "v0.11.8")
	}
	// A DIFFERENT CLI reading the same entry must still get a hit. If freshness ever
	// keys on cli_version, this goes red — and the feature would then re-probe on
	// every upgrade for no benefit, since recomputing already self-heals.
	if v, fresh := s.CachedServerVersion(testURL, now.Add(time.Minute), VersionCheckTTL); !fresh || v != "0.14.0" {
		t.Fatalf("got (%q, %v) — freshness appears to key on cli_version", v, fresh)
	}
}

func TestVersionCacheFileMode(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir(), versionCheckFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode %04o, want 0644", perm)
	}
}

// A 1 MiB version string is reachable: client.go's 32 MiB maxRespBytes is the only
// ceiling on the wire. It must not land on disk and be re-read for a whole TTL. This
// bound is STORAGE, not sanitization — the security control is at print time.
func TestVersionCacheBoundsAHugeVersion(t *testing.T) {
	s := NewStore(t.TempDir())
	huge := strings.Repeat("A", 1<<20)
	if _, err := s.RecordServerVersion(testURL, huge, "v0.11.8", time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir(), versionCheckFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 4096 {
		t.Errorf("cache file is %d bytes; the huge version was stored in full", fi.Size())
	}
	v, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL)
	if !fresh {
		t.Fatal("record was not stored at all")
	}
	if len([]rune(v)) > maxCachedVersionRunes {
		t.Errorf("read back %d runes, want at most %d", len([]rune(v)), maxCachedVersionRunes)
	}
}

// A nil *Store is the no-home-dir case, and every other caller in this package
// tolerates it. Calling these two must not panic.
func TestVersionCacheNilStore(t *testing.T) {
	var s *Store
	if v, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL); fresh || v != "" {
		t.Errorf("nil store returned (%q, %v)", v, fresh)
	}
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", time.Now()); err == nil {
		t.Error("nil store recorded without error")
	}
}

func TestVersionCacheWriteFailureIsReported(t *testing.T) {
	// A store rooted under a regular FILE: MkdirAll cannot create the directory.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(filepath.Join(blocker, "uzi"))
	if _, err := s.RecordServerVersion(testURL, "0.14.0", "v0.11.8", time.Now()); err == nil {
		t.Error("write into an unusable directory returned nil; the caller could not tell")
	}
	// And the read side degrades to a miss rather than an error.
	if _, fresh := s.CachedServerVersion(testURL, time.Now(), VersionCheckTTL); fresh {
		t.Error("read from an unusable directory reported fresh")
	}
}
