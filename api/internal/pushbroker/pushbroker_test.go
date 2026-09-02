package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// These tests prove the M8 publish algorithm against a LOCAL BARE fixture standing
// in for "origin" — the go-git round-trip against a real forge (gitlab.example.com)
// is a manual/e2e step this suite cannot reach. The api image carries no git binary,
// but a TEST may: we build the fixture and its delta packs with the real git, then
// drive the pure-Go broker over a file:// remote (no auth — the file transport takes
// none, so Options carries empty creds).

// gitFixture is a bare "origin" plus a working clone the test commits into.
type gitFixture struct {
	t    *testing.T
	bare string // bare repo path (the "origin")
	work string // working repo path
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	f := &gitFixture{t: t, bare: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work")}
	run(t, "", "git", "init", "--bare", "--initial-branch=main", f.bare)
	run(t, "", "git", "init", "--initial-branch=main", f.work)
	f.git("config", "user.email", "t@example.com")
	f.git("config", "user.name", "t")
	f.git("config", "commit.gpgsign", "false")
	f.git("remote", "add", "origin", f.bare)
	return f
}

// git runs a git subcommand inside the working repo.
func (f *gitFixture) git(args ...string) string {
	f.t.Helper()
	return run(f.t, f.work, "git", args...)
}

// commit writes a file, commits it, and returns the new HEAD sha.
func (f *gitFixture) commit(name, content, msg string) string {
	f.t.Helper()
	writeFile(f.t, filepath.Join(f.work, name), content)
	f.git("add", name)
	f.git("commit", "-m", msg)
	return strings.TrimSpace(f.git("rev-parse", "HEAD"))
}

// pushMain pushes the current main to origin's refs/heads/main.
func (f *gitFixture) pushMain() {
	f.t.Helper()
	f.git("push", "origin", "main:main")
}

// pack builds a self-contained (non-thin) delta pack carrying the objects reachable
// from tip but NOT from base — exactly the shape a worker ships, with base's objects
// coming from origin at apply time.
func (f *gitFixture) pack(tip, base string) []byte {
	f.t.Helper()
	revs := run(f.t, f.work, "git", "rev-list", tip, "^"+base, "--objects")
	cmd := exec.Command("git", "-C", f.work, "pack-objects", "--stdout") //nolint:gosec // G204: test helper invoking the real git against a test-owned temp repo path, not attacker input.
	cmd.Stdin = strings.NewReader(revs)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		f.t.Fatalf("pack-objects: %v\n%s", err, errb.String())
	}
	return out.Bytes()
}

// originRef returns the sha origin has for a ref, or "" if absent.
func (f *gitFixture) originRef(ref string) string {
	f.t.Helper()
	cmd := exec.Command("git", "-C", f.bare, "rev-parse", "--verify", "--quiet", ref) //nolint:gosec // G204: test helper invoking the real git against a test-owned temp repo path, not attacker input.
	out, _ := cmd.Output()                                                            // non-zero exit on a missing ref is expected → ""
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) cloneURL() string { return "file://" + f.bare }

func TestPublishFirstCheckpointCreatesRef(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip := f.commit("b.txt", "one\n", "c1")

	res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL:      f.cloneURL(),
		Branch:        "main",
		DefaultBranch: "main",
		DeclaredTip:   tip,
		Pack:          f.pack(tip, base),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Ref != "refs/uzi-checkpoints/main" {
		t.Fatalf("ref = %q, want refs/uzi-checkpoints/main", res.Ref)
	}
	if got := f.originRef("refs/uzi-checkpoints/main"); got != tip {
		t.Fatalf("origin checkpoint = %q, want tip %q", got, tip)
	}
}

// TestPublishFirstCheckpointOnNeverPushedBranch is the ACTUAL mid-run primary case:
// the agent's branch was NEVER pushed to refs/heads (branchTip zero AND checkpointTip
// zero). Origin only carries the default branch (main); the checkpoint feature branch
// exists nowhere on origin. The pack excludes objects reachable from main's tip (the
// default branch is the exclude boundary), and the publish must create the checkpoint
// ref fresh from nothing. Every OTHER test pushes the branch to heads first, so this
// is the only one exercising the zero-branchTip path.
func TestPublishFirstCheckpointOnNeverPushedBranch(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain() // origin has ONLY main; the feature branch is never pushed to heads

	// Work on a feature branch off main, but never push it.
	f.git("checkout", "-b", "agent/issue-7", base)
	tip := f.commit("b.txt", "one\n", "c1")

	res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL:      f.cloneURL(),
		Branch:        "agent/issue-7",
		DefaultBranch: "main",
		DeclaredTip:   tip,
		Pack:          f.pack(tip, base),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Ref != "refs/uzi-checkpoints/agent/issue-7" {
		t.Fatalf("ref = %q, want refs/uzi-checkpoints/agent/issue-7", res.Ref)
	}
	if got := f.originRef("refs/uzi-checkpoints/agent/issue-7"); got != tip {
		t.Fatalf("origin checkpoint = %q, want tip %q (fresh ref not created)", got, tip)
	}
	// origin's heads must be untouched: the feature branch was NEVER pushed to heads.
	if got := f.originRef("refs/heads/agent/issue-7"); got != "" {
		t.Fatalf("publish wrote refs/heads/agent/issue-7 = %q; it must only touch the checkpoint ns", got)
	}
}

// TestPublishFirstCheckpointAfterDefaultAdvanced is the issue-#1009 regression: the
// agent's branch was NEVER pushed to refs/heads, AND origin's default branch has
// advanced since the worker cloned. The worker's non-thin pack excludes everything
// reachable from the branch-point (base == D_old), but origin's main now points at
// D_new, so a depth-1 fetch pulls only D_new's snapshot — D_old is absent from the
// broker's storer. remote.PushContext recomputes its send-set by walking from the tip
// and excluding D_new, hits the branch-point's excluded parent D_old (not in the
// storer), and fails LOCALLY with "object not found" before any bytes leave — even
// though the REMOTE holds D_old and could resolve the pack. The manual receive-pack
// forward ships the pack verbatim and lets the remote do reachability, so the
// checkpoint is created.
func TestPublishFirstCheckpointAfterDefaultAdvanced(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base") // D_old — the pack's exclude boundary
	f.pushMain()

	// Work on a feature branch off base and build the worker's non-thin pack (^base),
	// but never push the branch to heads.
	f.git("checkout", "-b", "agent/issue-9", base)
	tip := f.commit("b.txt", "one\n", "c1")
	pack := f.pack(tip, base)

	// Origin's default advances to D_new on a DIFFERENT path than the feature branch
	// touched, so D_new descends D_old (D_old stays reachable on origin) but the
	// feature tip does not descend D_new.
	f.git("checkout", "main")
	f.commit("main-only.txt", "advanced\n", "main advance")
	f.pushMain() // origin main = D_new != D_old = base

	res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL:      f.cloneURL(),
		Branch:        "agent/issue-9",
		DefaultBranch: "main",
		DeclaredTip:   tip,
		Pack:          pack,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Ref != "refs/uzi-checkpoints/agent/issue-9" {
		t.Fatalf("ref = %q, want refs/uzi-checkpoints/agent/issue-9", res.Ref)
	}
	if got := f.originRef("refs/uzi-checkpoints/agent/issue-9"); got != tip {
		t.Fatalf("origin checkpoint = %q, want tip %q", got, tip)
	}
	// origin's heads must be untouched: the feature branch was NEVER pushed to heads.
	if got := f.originRef("refs/heads/agent/issue-9"); got != "" {
		t.Fatalf("publish wrote refs/heads/agent/issue-9 = %q; it must only touch the checkpoint ns", got)
	}
}

// TestPublishRejectsPackBomb proves the header-only budget pre-pass refuses a pack
// whose DECLARED object length exceeds the per-object cap, WITHOUT inflating it and
// WITHOUT pushing. The pack is a single highly-compressible ~40 MiB blob (over the
// 32 MiB per-object cap); on the wire it is tiny, so a naive apply would inflate 40
// MiB into the storer. Here it must come back ErrPackTooLarge and leave origin
// unchanged.
func TestPublishRejectsPackBomb(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()

	// A ~40 MiB highly-compressible blob (zeros): declared inflated length > 32 MiB
	// per-object cap, but a minuscule compressed pack.
	big := make([]byte, 40<<20)
	bomb := f.commit("big.bin", string(big), "bomb")
	packBytes := f.pack(bomb, base)
	if len(packBytes) > 1<<20 {
		t.Fatalf("bomb pack is %d bytes; expected it to compress tiny", len(packBytes))
	}

	_, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL:      f.cloneURL(),
		Branch:        "main",
		DefaultBranch: "main",
		DeclaredTip:   bomb,
		Pack:          packBytes,
	})
	if err == nil || !errors.Is(err, pushbroker.ErrPackTooLarge) {
		t.Fatalf("err = %v, want ErrPackTooLarge", err)
	}
	// No push happened: origin never gained the checkpoint ref.
	if got := f.originRef("refs/uzi-checkpoints/main"); got != "" {
		t.Fatalf("a rejected bomb still moved the checkpoint: origin = %q", got)
	}
}

// TestPublishAcceptsLegitDeltaPack proves the delta-aware budget does NOT reject a
// pack that legitimately CONTAINS a delta with a small target size. A large file is
// committed twice with tiny edits so git deltifies the second blob against the first
// (both land in the non-thin pack), then Publish must succeed and advance the
// checkpoint — guarding against a budget that accidentally refuses all deltas.
func TestPublishAcceptsLegitDeltaPack(t *testing.T) {
	f := newGitFixture(t)
	// A base commit whose objects are the exclude boundary (fetched from origin), then
	// TWO commits that both mutate a large, highly-similar file so the pack carries
	// both blob versions and git deltas the second against the first.
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	f.commit("big.txt", bigText("one"), "v1")
	tip := f.commit("big.txt", bigText("two"), "v2")

	pack := f.pack(tip, base)
	if !packHasDelta(t, pack) {
		t.Fatalf("test pack carries no delta object; the legit-delta case is not being exercised")
	}

	res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL:      f.cloneURL(),
		Branch:        "main",
		DefaultBranch: "main",
		DeclaredTip:   tip,
		Pack:          pack,
	})
	if err != nil {
		t.Fatalf("Publish rejected a legit delta pack: %v", err)
	}
	if res.Ref != "refs/uzi-checkpoints/main" {
		t.Fatalf("ref = %q", res.Ref)
	}
	if got := f.originRef("refs/uzi-checkpoints/main"); got != tip {
		t.Fatalf("checkpoint did not advance: origin = %q, want %q", got, tip)
	}
}

// bigText builds a large, mostly-fixed body with a small variable marker so two
// versions are near-identical — the input git reliably deltifies.
func bigText(marker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "marker: %s\n", marker)
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "line %d: the quick brown fox jumps over the lazy dog\n", i)
	}
	return b.String()
}

// packHasDelta reports whether pack contains at least one OFS/REF delta object, so a
// "legit delta" test can assert it is actually exercising a delta.
func packHasDelta(t *testing.T, pack []byte) bool {
	t.Helper()
	sc := packfile.NewScanner(bytes.NewReader(pack))
	_, n, err := sc.Header()
	if err != nil {
		t.Fatalf("scan header: %v", err)
	}
	for i := uint32(0); i < n; i++ {
		h, err := sc.NextObjectHeader()
		if err != nil {
			t.Fatalf("scan object header: %v", err)
		}
		if h.Type == plumbing.OFSDeltaObject || h.Type == plumbing.REFDeltaObject {
			return true
		}
		if _, _, err := sc.NextObject(io.Discard); err != nil {
			t.Fatalf("scan object body: %v", err)
		}
	}
	return false
}

func TestPublishAdvancesCheckpointNonForced(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip1 := f.commit("b.txt", "one\n", "c1")
	if _, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DeclaredTip: tip1, Pack: f.pack(tip1, base),
	}); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	tip2 := f.commit("c.txt", "two\n", "c2")
	res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DeclaredTip: tip2, Pack: f.pack(tip2, tip1),
	})
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if res.Ref != "refs/uzi-checkpoints/main" {
		t.Fatalf("ref = %q", res.Ref)
	}
	if got := f.originRef("refs/uzi-checkpoints/main"); got != tip2 {
		t.Fatalf("checkpoint did not advance: origin = %q, want %q", got, tip2)
	}
}

// TestPublishSameTipTwiceReportsSuccess is the PRD #1030 M1 resume scenario: a run
// resumed on a cold worker with lastPublishedTip reset and NO new commits re-declares
// the tip origin's checkpoint ref already holds. Re-publishing the SAME tip must be a
// genuine SUCCESS (nil error → Published=true at the caller), NOT ErrNotDescendant —
// otherwise the worker emits a misleading "checkpoint publish skipped: not_descendant"
// feed line every interval and never advances lastPublishedTip. Before the fix the
// strict-descendant check ran first and returned ErrNotDescendant (base == declared.Hash),
// so the already-up-to-date short-circuit was dead code; this pins it reachable.
func TestPublishSameTipTwiceReportsSuccess(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip := f.commit("b.txt", "one\n", "c1")

	opts := pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DefaultBranch: "main", DeclaredTip: tip, Pack: f.pack(tip, base),
	}
	if _, err := pushbroker.Publish(context.Background(), opts); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	ref := "refs/uzi-checkpoints/main"
	if got := f.originRef(ref); got != tip {
		t.Fatalf("checkpoint = %q, want tip %q", got, tip)
	}

	// Re-publish the SAME tip; origin's checkpoint ref already equals it.
	res, err := pushbroker.Publish(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Publish of same tip = %v; want success (not a not_descendant skip)", err)
	}
	if res.Ref != ref {
		t.Fatalf("ref = %q, want %q", res.Ref, ref)
	}
	if got := f.originRef(ref); got != tip {
		t.Fatalf("checkpoint changed on same-tip re-publish: got %q, want unchanged %q", got, tip)
	}
}

// TestPublishNeverForcesDivergedCheckpoint pins the never-forced invariant at the
// Publish level: if origin's checkpoint ref is moved OUT-OF-BAND to a commit X the
// declared tip does not descend, Publish must skip (ErrNotDescendant) AND leave origin's
// ref STILL pointing at X — never force-moving a diverged checkpoint. (The pure wire-CAS
// race between fetch and forward is not reproducible in the file:// harness; this covers
// the reachable local strict-descendant + never-clobber guarantee.)
func TestPublishNeverForcesDivergedCheckpoint(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip1 := f.commit("b.txt", "one\n", "c1")

	// Create the checkpoint ref at tip1.
	if _, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DefaultBranch: "main", DeclaredTip: tip1, Pack: f.pack(tip1, base),
	}); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	ref := "refs/uzi-checkpoints/main"
	if got := f.originRef(ref); got != tip1 {
		t.Fatalf("checkpoint = %q, want tip1 %q", got, tip1)
	}

	// Out of band, move origin's checkpoint ref to a DIVERGENT commit X (a sibling off
	// base that does not descend tip1) via a force-push against the bare — simulating a
	// concurrent/rogue mover. The force-push also transfers X's objects into origin.
	f.git("checkout", "-b", "fork", base)
	x := f.commit("x.txt", "divergent\n", "x")
	f.git("push", "origin", "--force", "fork:"+ref)
	if got := f.originRef(ref); got != x {
		t.Fatalf("out-of-band setup failed: checkpoint = %q, want X %q", got, x)
	}

	// Declare a tip that descends tip1 but NOT X.
	f.git("checkout", "main")
	tip2 := f.commit("c.txt", "two\n", "c2")

	_, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DefaultBranch: "main", DeclaredTip: tip2, Pack: f.pack(tip2, tip1),
	})
	if err == nil {
		t.Fatal("Publish accepted a tip that does not descend the diverged checkpoint; want ErrNotDescendant")
	}
	if !isNotDescendant(err) {
		t.Fatalf("err = %v, want ErrNotDescendant", err)
	}
	// CRUCIAL never-forced assertion: the diverged checkpoint ref was NOT clobbered.
	if got := f.originRef(ref); got != x {
		t.Fatalf("diverged checkpoint was force-moved: got %q, want unchanged X %q", got, x)
	}
}

func TestPublishRejectsNonDescendantAndDoesNotMoveRef(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip1 := f.commit("b.txt", "one\n", "c1")
	if _, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DeclaredTip: tip1, Pack: f.pack(tip1, base),
	}); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	// A sibling commit off base that does NOT descend the current checkpoint (tip1).
	f.git("checkout", "-b", "fork", base)
	fork := f.commit("d.txt", "fork\n", "fork")

	_, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DeclaredTip: fork, Pack: f.pack(fork, base),
	})
	if err == nil {
		t.Fatal("Publish accepted a non-descendant tip; want ErrNotDescendant")
	}
	if !isNotDescendant(err) {
		t.Fatalf("err = %v, want ErrNotDescendant", err)
	}
	if got := f.originRef("refs/uzi-checkpoints/main"); got != tip1 {
		t.Fatalf("checkpoint moved on a rejected publish: origin = %q, want unchanged %q", got, tip1)
	}
}

func TestPublishTipMissingFromPack(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	// Advance and produce a pack for tip, but declare a DIFFERENT (never-shipped)
	// tip: a well-formed sha not present after applying the pack.
	tip := f.commit("b.txt", "one\n", "c1")
	absent := "0123456789abcdef0123456789abcdef01234567"
	_, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "main", DeclaredTip: absent, Pack: f.pack(tip, base),
	})
	if err == nil || !errors.Is(err, pushbroker.ErrTipMissing) {
		t.Fatalf("err = %v, want ErrTipMissing", err)
	}
}

func isNotDescendant(err error) bool { return errors.Is(err, pushbroker.ErrNotDescendant) }

// run executes a command (optionally in dir) and returns stdout, failing the test
// on a non-zero exit with stderr attached.
func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // G204: generic test command runner; name/args are test-controlled constants, not attacker input.
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
