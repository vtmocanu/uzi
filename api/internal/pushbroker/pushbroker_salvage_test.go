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

// PRD #1867 M1: Promote (branch-scoped checkpoint -> run-scoped salvage ref) and the
// ref-generalized CAS DeleteRef, against the same file:// bare fixture the publish tests
// use. go-git's file:// transport execs the REAL git-receive-pack, so the packless
// create, the report-status and every hook below are real git behaviour.

const salvageBranch = "agent/issue-5"

var salvageRunID = uuid.MustParse("8f0c2a4e-1b3d-4c5e-9f60-7a8b9c0d1e2f")

// salvageFixture is origin with main pushed and a run tip published to the branch's
// checkpoint ref (pushed straight by git: the tip's objects are on origin, as after a
// real Publish). It returns the fixture, the base (main) sha and the tip.
func salvageFixture(t *testing.T) (f *gitFixture, base, tip string) {
	t.Helper()
	f = newGitFixture(t)
	base = f.commit("a.txt", "base\n", "base")
	f.pushMain()
	f.git("checkout", "-b", salvageBranch, base)
	tip = f.commit("b.txt", "work\n", "run work")
	f.git("push", "origin", tip+":"+checkpointRef())
	if got := f.originRef(checkpointRef()); got != tip {
		t.Fatalf("setup: checkpoint = %q, want %q", got, tip)
	}
	return f, base, tip
}

func checkpointRef() string { return "refs/uzi-checkpoints/" + salvageBranch }

func salvageRef() string { return pushbroker.SalvageRef(salvageRunID) }

func promoteOpts(cloneURL, tip string) pushbroker.PromoteOptions {
	return pushbroker.PromoteOptions{CloneURL: cloneURL, Branch: salvageBranch, Tip: tip, RunID: salvageRunID}
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

func TestSalvageRefName(t *testing.T) {
	if got, want := salvageRef(), "refs/uzi-salvage/8f0c2a4e-1b3d-4c5e-9f60-7a8b9c0d1e2f"; got != want {
		t.Fatalf("SalvageRef = %q, want %q", got, want)
	}
}

// TestPromoteCreatesSalvageAndDeletesBranchRef is the happy path: the salvage ref is
// created at the tip with a packless create, then the branch ref is CAS-deleted.
func TestPromoteCreatesSalvageAndDeletesBranchRef(t *testing.T) {
	f, _, tip := salvageFixture(t)
	mainBefore := f.originRef("refs/heads/main")

	res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.PromoteDone {
		t.Fatalf("Promote = (%v, %v), want (done, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
	if got := f.originRef(checkpointRef()); got != "" {
		t.Fatalf("branch checkpoint ref = %q, want deleted", got)
	}
	if got := f.originRef("refs/heads/main"); got != mainBefore {
		t.Fatalf("refs/heads/main moved: %q -> %q", mainBefore, got)
	}
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

// TestPromoteIdempotentRerun: a salvage ref already at the tip is success without a
// second create; the branch ref is still CAS-deleted, and a further re-run with the
// branch ref gone is also done.
func TestPromoteIdempotentRerun(t *testing.T) {
	f, _, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())

	for i := range 2 { // pass 0: branch ref present; pass 1: already gone
		res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.PromoteDone {
			t.Fatalf("pass %d: Promote = (%v, %v), want (done, nil)", i, res, err)
		}
		if got := f.originRef(salvageRef()); got != tip {
			t.Fatalf("pass %d: salvage ref = %q, want tip %q", i, got, tip)
		}
		if got := f.originRef(checkpointRef()); got != "" {
			t.Fatalf("pass %d: branch ref = %q, want deleted", i, got)
		}
	}
}

// TestPromoteRefusesSalvageAtOtherTip: an existing salvage ref at a different tip is
// never overwritten, and the branch ref is kept.
func TestPromoteRefusesSalvageAtOtherTip(t *testing.T) {
	f, base, tip := salvageFixture(t)
	f.git("push", "origin", base+":"+salvageRef())

	res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.PromoteRefused {
		t.Fatalf("Promote = (%v, %v), want (refused, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != base {
		t.Fatalf("salvage ref = %q, want untouched %q", got, base)
	}
	if got := f.originRef(checkpointRef()); got != tip {
		t.Fatalf("branch ref = %q, want untouched %q", got, tip)
	}
}

// TestPromoteUnavailable: a missing branch ref, or one a sibling moved, yields
// unavailable with no salvage ref created and the moved ref left alone.
func TestPromoteUnavailable(t *testing.T) {
	t.Run("branch ref missing", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", ":"+checkpointRef())

		res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.PromoteUnavailable {
			t.Fatalf("Promote = (%v, %v), want (unavailable, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
	})
	t.Run("branch ref moved", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		sibling := f.commit("c.txt", "sibling\n", "sibling run")
		f.git("push", "origin", sibling+":"+checkpointRef())

		res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.PromoteUnavailable {
			t.Fatalf("Promote = (%v, %v), want (unavailable, nil)", res, err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		if got := f.originRef(checkpointRef()); got != sibling {
			t.Fatalf("moved branch ref = %q, want untouched %q", got, sibling)
		}
	})
	t.Run("empty remote", func(t *testing.T) {
		f := newGitFixture(t)
		tip := strings.Repeat("ab", 20)
		res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
		if err != nil || res != pushbroker.PromoteUnavailable {
			t.Fatalf("Promote = (%v, %v), want (unavailable, nil)", res, err)
		}
	})
}

// TestPromoteCreateFailureKeepsBranchRef: when the salvage create is refused (a hook
// declining refs/uzi-salvage/*) or origin is unreachable, the result is failed and the
// branch ref is never deleted.
func TestPromoteCreateFailureKeepsBranchRef(t *testing.T) {
	t.Run("create refused", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		installPreReceive(t, f, "#!/bin/sh\n"+
			"while read old new ref; do\n"+
			"  case \"$ref\" in refs/uzi-salvage/*) echo 'salvage refused by test hook' >&2; exit 1;; esac\n"+
			"done\nexit 0\n")

		res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
		if err == nil || res != pushbroker.PromoteFailed {
			t.Fatalf("Promote = (%v, %v), want (failed, error)", res, err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		if got := f.originRef(checkpointRef()); got != tip {
			t.Fatalf("branch ref = %q after failed create, want intact %q", got, tip)
		}
	})
	t.Run("origin unreachable", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		res, err := pushbroker.Promote(context.Background(), promoteOpts(closedHTTPURL(t), tip))
		if err == nil || res != pushbroker.PromoteFailed {
			t.Fatalf("Promote = (%v, %v), want (failed, error)", res, err)
		}
		if got := f.originRef(checkpointRef()); got != tip {
			t.Fatalf("branch ref = %q, want intact %q", got, tip)
		}
	})
}

// TestPromotePartialSuccessBranchPending: the salvage ref is created but the branch-ref
// delete is refused (a hook declining checkpoint-ref deletions), so the result is
// salvaged_branch_pending with an error, both refs present. Once the refusal lifts, the
// re-run finds the same-tip salvage ref and completes only the delete.
func TestPromotePartialSuccessBranchPending(t *testing.T) {
	f, _, tip := salvageFixture(t)
	hook := filepath.Join(f.bare, "hooks", "pre-receive")
	installPreReceive(t, f, "#!/bin/sh\n"+
		"z=0000000000000000000000000000000000000000\n"+
		"while read old new ref; do\n"+
		"  case \"$ref\" in refs/uzi-checkpoints/*) if [ \"$new\" = \"$z\" ]; then echo 'delete refused by test hook' >&2; exit 1; fi;; esac\n"+
		"done\nexit 0\n")

	res, err := pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
	if err == nil || res != pushbroker.PromoteSalvagedBranchPending {
		t.Fatalf("Promote = (%v, %v), want (salvaged_branch_pending, error)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
	if got := f.originRef(checkpointRef()); got != tip {
		t.Fatalf("branch ref = %q, want still %q", got, tip)
	}

	if err := os.Remove(hook); err != nil {
		t.Fatalf("remove hook: %v", err)
	}
	res, err = pushbroker.Promote(context.Background(), promoteOpts(f.cloneURL(), tip))
	if err != nil || res != pushbroker.PromoteDone {
		t.Fatalf("re-run Promote = (%v, %v), want (done, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q after re-run, want tip %q", got, tip)
	}
	if got := f.originRef(checkpointRef()); got != "" {
		t.Fatalf("branch ref = %q after re-run, want deleted", got)
	}
}

// TestDeleteRefSalvage: DeleteRef CAS-deletes a salvage ref at the matching tip; a
// mismatch or an absent ref is benign nil and leaves origin untouched.
func TestDeleteRefSalvage(t *testing.T) {
	f, base, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())
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
	if got := f.originRef(checkpointRef()); got != tip {
		t.Fatalf("DeleteRef touched the branch ref: %q, want %q", got, tip)
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
		{"tags ref", "refs/tags/v1", good},
		{"foreign prefix lookalike", "refs/uzi-salvaged/x", good},
		{"empty ref", "", good},
		{"bare salvage prefix", "refs/uzi-salvage/", good},
		{"bare checkpoint prefix", "refs/uzi-checkpoints/", good},
		{"trailing slash", "refs/uzi-checkpoints/agent/", good},
		{"dot-dot escape", "refs/uzi-checkpoints/../heads/main", good},
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

func TestPromoteRejectsInvalidInputWithoutNetwork(t *testing.T) {
	url, hits := countingServer(t)
	good := strings.Repeat("ab", 20)
	cases := []struct {
		name string
		o    pushbroker.PromoteOptions
	}{
		{"nil run id", pushbroker.PromoteOptions{CloneURL: url, Branch: salvageBranch, Tip: good}},
		{"empty tip", pushbroker.PromoteOptions{CloneURL: url, Branch: salvageBranch, RunID: salvageRunID}},
		{"short tip", pushbroker.PromoteOptions{CloneURL: url, Branch: salvageBranch, Tip: "abc123", RunID: salvageRunID}},
		{"zero tip", pushbroker.PromoteOptions{CloneURL: url, Branch: salvageBranch, Tip: strings.Repeat("0", 40), RunID: salvageRunID}},
		{"non-hex tip", pushbroker.PromoteOptions{CloneURL: url, Branch: salvageBranch, Tip: strings.Repeat("zz", 20), RunID: salvageRunID}},
		{"empty branch", pushbroker.PromoteOptions{CloneURL: url, Tip: good, RunID: salvageRunID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := pushbroker.Promote(context.Background(), tc.o)
			if err == nil || res != pushbroker.PromoteFailed {
				t.Fatalf("Promote = (%v, %v), want (failed, error)", res, err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("invalid Promote input reached the network %d time(s), want 0", n)
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

// TestSalvageErrorsNeverCarryPAT: a transport failure from Promote and DeleteRef never
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
			_, perr := pushbroker.Promote(context.Background(), pushbroker.PromoteOptions{
				CloneURL: u, Branch: salvageBranch, Tip: tip, RunID: salvageRunID, Username: "uzi-bot", PAT: pat,
			})
			derr := pushbroker.DeleteRef(context.Background(), pushbroker.DeleteRefOptions{
				CloneURL: u, Ref: salvageRef(), ExpectedOldTip: tip, Username: "uzi-bot", PAT: pat,
			})
			for op, err := range map[string]error{"Promote": perr, "DeleteRef": derr} {
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

// TestPromoteOverSmartHTTP runs the packless salvage create and the branch-ref CAS
// delete over REAL smart HTTP (git http-backend), the transport every forge uses. It
// skips locally when git-http-backend is absent and fails in CI (requireGitHTTPBackend).
func TestPromoteOverSmartHTTP(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f, _, tip := salvageFixture(t)
	remote := newGitHTTPRemote(t, f, backend)
	u := remote.cloneURL(f)

	res, err := pushbroker.Promote(context.Background(), pushbroker.PromoteOptions{
		CloneURL: u, Branch: salvageBranch, Tip: tip, RunID: salvageRunID, Username: "uzi-bot", PAT: redirectTestPAT(),
	})
	if err != nil || res != pushbroker.PromoteDone {
		t.Fatalf("Promote over smart HTTP = (%v, %v), want (done, nil)", res, err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
	if got := f.originRef(checkpointRef()); got != "" {
		t.Fatalf("branch ref = %q, want deleted", got)
	}

	if err := pushbroker.DeleteRef(context.Background(), pushbroker.DeleteRefOptions{
		CloneURL: u, Ref: salvageRef(), ExpectedOldTip: tip, Username: "uzi-bot", PAT: redirectTestPAT(),
	}); err != nil {
		t.Fatalf("DeleteRef over smart HTTP: %v", err)
	}
	if got := f.originRef(salvageRef()); got != "" {
		t.Fatalf("salvage ref = %q after DeleteRef, want deleted", got)
	}
}
