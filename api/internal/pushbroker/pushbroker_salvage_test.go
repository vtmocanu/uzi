package pushbroker_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
)

// PRD #1867: CreateSalvageRef (a CREATE-ONLY copy of a failed run's checkpoint tip into
// the run-scoped refs/uzi-salvage/<run-id>) and the salvage-only CAS DeleteRef, against
// the same file:// bare fixture the publish tests use. go-git's file:// transport execs
// the REAL git-receive-pack, so the packless create, the report-status and every hook
// below are real git behaviour.
//
// The branch checkpoint ref and refs/uzi-recovery/<run-id> belong to PR #1819's
// retention (PRD #1810): every test asserts salvage leaves both exactly as they were.

const salvageBranch = "agent/issue-5"

var salvageRunID = uuid.MustParse("8f0c2a4e-1b3d-4c5e-9f60-7a8b9c0d1e2f")

// salvageFixture is origin with main pushed, a run tip published to the branch's
// checkpoint ref (pushed straight by git: the tip's objects are on origin, as after a
// real Publish), and this run's refs/uzi-recovery/<run-id> present at the base, so a
// test can prove salvage never deletes or moves it. It returns the fixture, the base
// (main) sha and the tip.
func salvageFixture(t *testing.T) (f *gitFixture, base, tip string) {
	t.Helper()
	f = newGitFixture(t)
	base = f.commit("a.txt", "base\n", "base")
	f.pushMain()
	f.git("checkout", "-b", salvageBranch, base)
	tip = f.commit("b.txt", "work\n", "run work")
	f.git("push", "origin", tip+":"+checkpointRef())
	f.git("push", "origin", base+":"+recoveryRef())
	if got := f.originRef(checkpointRef()); got != tip {
		t.Fatalf("setup: checkpoint = %q, want %q", got, tip)
	}
	return f, base, tip
}

func checkpointRef() string { return "refs/uzi-checkpoints/" + salvageBranch }

func recoveryRef() string { return "refs/uzi-recovery/" + salvageRunID.String() }

func salvageRef() string { return pushbroker.SalvageRef(salvageRunID) }

func createOpts(cloneURL, tip string) pushbroker.CreateSalvageRefOptions {
	return pushbroker.CreateSalvageRefOptions{CloneURL: cloneURL, Branch: salvageBranch, Tip: tip, RunID: salvageRunID}
}

// foreignRefs snapshots every ref salvage must never write: the branch checkpoint ref,
// this run's recovery ref and main.
func foreignRefs(f *gitFixture) map[string]string {
	out := map[string]string{}
	for _, r := range []string{checkpointRef(), recoveryRef(), "refs/heads/main"} {
		out[r] = f.originRef(r)
	}
	return out
}

// assertForeignRefsUntouched fails when any ref in before moved or vanished.
func assertForeignRefsUntouched(t *testing.T, f *gitFixture, before map[string]string) {
	t.Helper()
	for r, was := range before {
		if got := f.originRef(r); got != was {
			t.Errorf("%s changed: %q -> %q; salvage must never delete or move it", r, was, got)
		}
	}
}

// installPreReceive writes an executable pre-receive hook into origin, so real
// receive-pack refuses a chosen command class deterministically.
func installPreReceive(t *testing.T, f *gitFixture, script string) {
	t.Helper()
	path := filepath.Join(f.bare, "hooks", "pre-receive")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: a test-owned hook in a temp repo must be executable.
		t.Fatalf("write hook: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

// refuseAllWrites is a pre-receive hook that declines every command, so a test can prove
// an outcome was reached with no write at all.
const refuseAllWrites = "#!/bin/sh\necho 'no write expected' >&2\nexit 1\n"

func TestSalvageRefName(t *testing.T) {
	if got, want := salvageRef(), "refs/uzi-salvage/8f0c2a4e-1b3d-4c5e-9f60-7a8b9c0d1e2f"; got != want {
		t.Fatalf("SalvageRef = %q, want %q", got, want)
	}
}

// TestCreateSalvageRefFromBranchRef is the happy path: the branch checkpoint ref is at
// the tip, so the salvage ref is created there with a packless create, and the branch
// ref, the recovery ref and main are all left exactly as they were.
func TestCreateSalvageRefFromBranchRef(t *testing.T) {
	f, _, tip := salvageFixture(t)
	before := foreignRefs(f)

	res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.SalvageCreated {
		t.Fatalf("CreateSalvageRef = (%v, %v), want (created, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
	if got := f.originRef(checkpointRef()); got != tip {
		t.Fatalf("branch checkpoint ref = %q, want still %q", got, tip)
	}
	assertForeignRefsUntouched(t, f, before)
}

// TestCreateSalvageRefFromRecoveryRef: #1810 moved the run's tip to
// refs/uzi-recovery/<run-id> and a newer run owns the branch ref. The recovery ref at the
// tip is a verified source, so the salvage ref is created; neither source ref moves.
func TestCreateSalvageRefFromRecoveryRef(t *testing.T) {
	t.Run("branch ref moved", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", "+"+tip+":"+recoveryRef())
		sibling := f.commit("c.txt", "sibling\n", "sibling run")
		f.git("push", "origin", sibling+":"+checkpointRef())
		before := foreignRefs(f)

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageCreated {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (created, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != tip {
			t.Fatalf("salvage ref = %q, want tip %q", got, tip)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("branch ref absent", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", "+"+tip+":"+recoveryRef())
		f.git("push", "origin", ":"+checkpointRef())
		before := foreignRefs(f)

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageCreated {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (created, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != tip {
			t.Fatalf("salvage ref = %q, want tip %q", got, tip)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestSalvageCreateNeedsEmptyPack is the negative control for the explicit empty pack:
// the identical create command with NO pack is refused by real receive-pack ("unpack
// eof before pack header"), while the same command with the empty pack succeeds. So the
// empty pack is load-bearing, not decoration.
func TestSalvageCreateNeedsEmptyPack(t *testing.T) {
	f, _, tip := salvageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := pushbroker.ForwardPackForTest(ctx, f.cloneURL(), salvageRef(), "", tip, nil)
	if err == nil {
		t.Fatal("packless create with NO pack succeeded; want real receive-pack to refuse it")
	}
	t.Logf("no-pack create refused as expected: %v", err)
	if got := f.originRef(salvageRef()); got != "" {
		t.Fatalf("salvage ref = %q after refused create, want absent", got)
	}

	if err := pushbroker.ForwardPackForTest(ctx, f.cloneURL(), salvageRef(), "", tip, pushbroker.EmptyPackForTest()); err != nil {
		t.Fatalf("create with the empty pack: %v", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
}

// TestCreateSalvageRefIdempotent: a salvage ref already at the tip is created with NO
// write (a hook refusing every command proves it), even once both sources are gone.
func TestCreateSalvageRefIdempotent(t *testing.T) {
	f, _, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())
	before := foreignRefs(f)
	installPreReceive(t, f, refuseAllWrites)

	res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.SalvageCreated {
		t.Fatalf("CreateSalvageRef = (%v, %v), want (created, nil)", res, err)
	}
	assertForeignRefsUntouched(t, f, before)

	// Sources gone (e.g. #1810 settled and deleted them): still idempotent success.
	_ = os.Remove(filepath.Join(f.bare, "hooks", "pre-receive"))
	f.git("push", "origin", ":"+checkpointRef())
	f.git("push", "origin", ":"+recoveryRef())
	installPreReceive(t, f, refuseAllWrites)
	res, err = pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.SalvageCreated {
		t.Fatalf("re-run without sources = (%v, %v), want (created, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
}

// TestCreateSalvageRefRefusesOtherTip: an existing salvage ref at a different tip is
// never overwritten, and nothing else is touched.
func TestCreateSalvageRefRefusesOtherTip(t *testing.T) {
	f, base, tip := salvageFixture(t)
	f.git("push", "origin", base+":"+salvageRef())
	before := foreignRefs(f)

	res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.SalvageRefused {
		t.Fatalf("CreateSalvageRef = (%v, %v), want (refused, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != base {
		t.Fatalf("salvage ref = %q, want untouched %q", got, base)
	}
	assertForeignRefsUntouched(t, f, before)
}

// TestCreateSalvageRefUnavailable: when neither the branch ref nor the recovery ref is
// at the tip, nothing is created and the refs a sibling or #1810 own are left alone.
func TestCreateSalvageRefUnavailable(t *testing.T) {
	t.Run("branch ref missing", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", ":"+checkpointRef())
		before := foreignRefs(f)

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageUnavailable {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (unavailable, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("branch ref moved", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		sibling := f.commit("c.txt", "sibling\n", "sibling run")
		f.git("push", "origin", sibling+":"+checkpointRef())
		before := foreignRefs(f)

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageUnavailable {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (unavailable, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("empty remote", func(t *testing.T) {
		f := newGitFixture(t)
		tip := strings.Repeat("ab", 20)
		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageUnavailable {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (unavailable, nil)", res, err)
		}
	})
}

// TestCreateSalvageRefCreateFailure: when the salvage create is refused (a hook
// declining refs/uzi-salvage/*) or origin is unreachable, the result is failed and no
// other ref is touched.
func TestCreateSalvageRefCreateFailure(t *testing.T) {
	t.Run("create refused", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		before := foreignRefs(f)
		installPreReceive(t, f, "#!/bin/sh\n"+
			"while read old new ref; do\n"+
			"  case \"$ref\" in refs/uzi-salvage/*) echo 'salvage refused by test hook' >&2; exit 1;; esac\n"+
			"done\nexit 0\n")

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err == nil || res != pushbroker.SalvageFailed {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (failed, error)", res, err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("origin unreachable", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		before := foreignRefs(f)
		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(closedHTTPURL(t), tip))
		if err == nil || res != pushbroker.SalvageFailed {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (failed, error)", res, err)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestCreateSalvageRefCreateRaceRelists: a concurrent creator writes the salvage ref
// between CreateSalvageRef's list and its create (staged by a pre-receive hook that
// writes the ref, then declines ours). It re-lists once: the ref at OUR tip is created;
// at ANOTHER tip it is refused. No other ref moves either way.
func TestCreateSalvageRefCreateRaceRelists(t *testing.T) {
	// raceHook writes refs/uzi-salvage/* to sha before declining the create. The hook
	// runs with GIT_QUARANTINE_PATH set, under which git refuses ref updates, so it is
	// unset for the inner update-ref.
	raceHook := func(sha string) string {
		return "#!/bin/sh\n" +
			"while read old new ref; do\n" +
			"  case \"$ref\" in refs/uzi-salvage/*) env -u GIT_QUARANTINE_PATH git update-ref \"$ref\" " + sha + " || exit 2; echo 'raced by test hook' >&2; exit 1;; esac\n" +
			"done\nexit 0\n"
	}
	t.Run("same tip", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		before := foreignRefs(f)
		installPreReceive(t, f, raceHook(tip))

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageCreated {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (created, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != tip {
			t.Fatalf("salvage ref = %q, want tip %q", got, tip)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("other tip", func(t *testing.T) {
		f, base, tip := salvageFixture(t)
		before := foreignRefs(f)
		installPreReceive(t, f, raceHook(base))

		res, err := pushbroker.CreateSalvageRef(context.Background(), createOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.SalvageRefused {
			t.Fatalf("CreateSalvageRef = (%v, %v), want (refused, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != base {
			t.Fatalf("salvage ref = %q, want the racer's %q", got, base)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestDeleteRefSalvage: DeleteRef CAS-deletes a salvage ref at the matching tip; a
// mismatch or an absent ref is benign nil. The branch and recovery refs never move.
func TestDeleteRefSalvage(t *testing.T) {
	f, base, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())
	before := foreignRefs(f)
	del := func(expected string) error {
		return pushbroker.DeleteRef(context.Background(), pushbroker.DeleteRefOptions{
			CloneURL: f.cloneURL(), Ref: salvageRef(), ExpectedOldTip: expected,
		})
	}

	if err := del(base); err != nil {
		t.Fatalf("mismatched DeleteRef = %v, want nil", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("mismatched DeleteRef moved the ref: %q, want %q", got, tip)
	}
	if err := del(tip); err != nil {
		t.Fatalf("matching DeleteRef = %v, want nil", err)
	}
	if got := f.originRef(salvageRef()); got != "" {
		t.Fatalf("salvage ref = %q, want deleted", got)
	}
	if err := del(tip); err != nil {
		t.Fatalf("DeleteRef of absent ref = %v, want nil", err)
	}
	assertForeignRefsUntouched(t, f, before)
}

// TestDeleteRefBenignRefusalIsNotDone: casDelete reads a lock-failure refusal ("cannot
// lock ref" / "failed to update ref") as benign, and a forge can emit that text for
// transient lock contention, so its nil does not prove the ref is gone. Real
// git-receive-pack cannot be made to emit that reason on demand (a hook's stderr travels
// on the sideband, not in the "ng" reason), so the delete is stubbed to return nil
// without deleting. DeleteRef's re-list must find the ref still at the tip and error.
func TestDeleteRefBenignRefusalIsNotDone(t *testing.T) {
	f, _, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())

	err := pushbroker.DeleteRefWithBenignDeleteForTest(context.Background(), pushbroker.DeleteRefOptions{
		CloneURL: f.cloneURL(), Ref: salvageRef(), ExpectedOldTip: tip,
	})
	if err == nil || !strings.Contains(err.Error(), "still at tip after delete") {
		t.Fatalf("DeleteRef = %v; want the post-delete re-list to report the ref still at the tip", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want intact %q", got, tip)
	}
}

// countingServer counts every request it receives; the validation tests require zero.
func countingServer(t *testing.T) (url string, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "no request expected", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/origin.git", hits
}

func TestDeleteRefRejectsInvalidInputWithoutNetwork(t *testing.T) {
	url, hits := countingServer(t)
	good := strings.Repeat("ab", 20)
	cases := []struct{ name, ref, tip string }{
		{"heads ref", "refs/heads/main", good},
		{"branch checkpoint ref", checkpointRef(), good},
		{"recovery ref", recoveryRef(), good},
		{"tags ref", "refs/tags/v1", good},
		{"foreign prefix lookalike", "refs/uzi-salvaged/x", good},
		{"empty ref", "", good},
		{"bare salvage prefix", "refs/uzi-salvage/", good},
		{"trailing slash", "refs/uzi-salvage/agent/", good},
		{"dot-dot escape", "refs/uzi-salvage/../heads/main", good},
		{"empty tip", salvageRef(), ""},
		{"short tip", salvageRef(), "abc123"},
		{"zero tip", salvageRef(), strings.Repeat("0", 40)},
		{"non-hex tip", salvageRef(), strings.Repeat("zz", 20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pushbroker.DeleteRef(context.Background(), pushbroker.DeleteRefOptions{
				CloneURL: url, Ref: tc.ref, ExpectedOldTip: tc.tip,
			})
			if err == nil {
				t.Fatal("DeleteRef accepted invalid input")
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("invalid DeleteRef input reached the network %d time(s), want 0", n)
	}
}

func TestCreateSalvageRefRejectsInvalidInputWithoutNetwork(t *testing.T) {
	url, hits := countingServer(t)
	good := strings.Repeat("ab", 20)
	cases := []struct {
		name string
		o    pushbroker.CreateSalvageRefOptions
	}{
		{"nil run id", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: salvageBranch, Tip: good}},
		{"empty tip", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: salvageBranch, RunID: salvageRunID}},
		{"short tip", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: salvageBranch, Tip: "abc123", RunID: salvageRunID}},
		{"zero tip", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: salvageBranch, Tip: strings.Repeat("0", 40), RunID: salvageRunID}},
		{"non-hex tip", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: salvageBranch, Tip: strings.Repeat("zz", 20), RunID: salvageRunID}},
		{"empty branch", pushbroker.CreateSalvageRefOptions{CloneURL: url, Tip: good, RunID: salvageRunID}},
		{"dot-dot branch escape", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: "../heads/main", Tip: good, RunID: salvageRunID}},
		{"dot-dot inside branch", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: "x/../y", Tip: good, RunID: salvageRunID}},
		{"trailing slash branch", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: "agent/", Tip: good, RunID: salvageRunID}},
		{"bare slash branch", pushbroker.CreateSalvageRefOptions{CloneURL: url, Branch: "/", Tip: good, RunID: salvageRunID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := pushbroker.CreateSalvageRef(context.Background(), tc.o)
			if err == nil || res != pushbroker.SalvageFailed {
				t.Fatalf("CreateSalvageRef = (%v, %v), want (failed, error)", res, err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("invalid CreateSalvageRef input reached the network %d time(s), want 0", n)
	}
}

// closedHTTPURL returns an http URL on a loopback port nothing listens on.
func closedHTTPURL(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr + "/origin.git"
}

// salvageTestPAT is a GitHub-classic-shaped credential assembled at runtime so no
// token-shaped literal sits in source, yet secretscrub recognises the joined value.
func salvageTestPAT() string {
	return "gh" + "p_" + strings.Repeat("Sa1vage", 6)
}

// TestSalvageErrorsNeverCarryPAT: a transport failure from CreateSalvageRef and DeleteRef never
// carries the credential, whether it arrives as the BasicAuth password or embedded in
// the clone URL's userinfo, and the scrubbed form the caller persists never does either.
func TestSalvageErrorsNeverCarryPAT(t *testing.T) {
	pat := salvageTestPAT()
	if !strings.Contains(secretscrub.Scrub("x "+pat+" y"), "[redacted]") {
		t.Fatal("fixture PAT is not a shape secretscrub recognises")
	}
	tip := strings.Repeat("ab", 20)
	closed := closedHTTPURL(t)
	urls := map[string]string{
		"basic auth":   closed,
		"url userinfo": strings.Replace(closed, "http://", "http://uzi-bot:"+pat+"@", 1),
	}
	for name, u := range urls {
		t.Run(name, func(t *testing.T) {
			_, perr := pushbroker.CreateSalvageRef(context.Background(), pushbroker.CreateSalvageRefOptions{
				CloneURL: u, Branch: salvageBranch, Tip: tip, RunID: salvageRunID, Username: "uzi-bot", PAT: pat,
			})
			derr := pushbroker.DeleteRef(context.Background(), pushbroker.DeleteRefOptions{
				CloneURL: u, Ref: salvageRef(), ExpectedOldTip: tip, Username: "uzi-bot", PAT: pat,
			})
			for op, err := range map[string]error{"CreateSalvageRef": perr, "DeleteRef": derr} {
				if err == nil {
					t.Fatalf("%s against a closed port succeeded; want a transport error", op)
				}
				if strings.Contains(err.Error(), pat) {
					t.Errorf("%s error carries the PAT: %v", op, err)
				}
				if strings.Contains(secretscrub.Scrub(err.Error()), pat) {
					t.Errorf("%s scrubbed error carries the PAT", op)
				}
			}
		})
	}
}

// TestCreateSalvageRefOverSmartHTTP runs the packless salvage create and the salvage
// DeleteRef over REAL smart HTTP (git http-backend), the transport every forge uses. It
// skips locally when git-http-backend is absent and fails in CI (requireGitHTTPBackend).
func TestCreateSalvageRefOverSmartHTTP(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f, _, tip := salvageFixture(t)
	before := foreignRefs(f)
	remote := newGitHTTPRemote(t, f, backend)
	u := remote.cloneURL(f)

	res, err := pushbroker.CreateSalvageRef(context.Background(), pushbroker.CreateSalvageRefOptions{
		CloneURL: u, Branch: salvageBranch, Tip: tip, RunID: salvageRunID, Username: "uzi-bot", PAT: redirectTestPAT(),
	})
	if err != nil || res != pushbroker.SalvageCreated {
		t.Fatalf("CreateSalvageRef over smart HTTP = (%v, %v), want (created, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}

	if err := pushbroker.DeleteRef(context.Background(), pushbroker.DeleteRefOptions{
		CloneURL: u, Ref: salvageRef(), ExpectedOldTip: tip, Username: "uzi-bot", PAT: redirectTestPAT(),
	}); err != nil {
		t.Fatalf("DeleteRef over smart HTTP: %v", err)
	}
	if got := f.originRef(salvageRef()); got != "" {
		t.Fatalf("salvage ref = %q after DeleteRef, want deleted", got)
	}
	assertForeignRefsUntouched(t, f, before)
}
