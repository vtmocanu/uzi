package pushbroker_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
)

// PRD #1867: a failed run's checkpoint tip copied into the run-scoped
// refs/uzi-salvage/<run-id> with #1810's primitives, whose allowed namespaces PRD #1867
// widens: CreateRef (a salvage Ref sourced from the branch checkpoint ref or the run's
// refs/uzi-recovery/<run-id>), the CAS-only Delete of a salvage Ref, and ListRefTips of a
// salvage ref. They run against the same file:// bare fixture the publish tests use, whose
// hooks below are real git behaviour; the smart-HTTP test at the end runs a real
// git receive-pack. createref_http_test.go and createref_internal_test.go cover the
// primitives' own mechanics (the empty pack, the wire CAS, the read-back classification),
// which do not depend on the namespace.
//
// The branch checkpoint ref and refs/uzi-recovery/<run-id> belong to PR #1819's
// retention (PRD #1810): every test asserts a salvage write leaves both exactly as they
// were.

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
	f.git("push", "origin", tip+":"+salvageCheckpointRef())
	f.git("push", "origin", base+":"+salvageRecoveryRef())
	if got := f.originRef(salvageCheckpointRef()); got != tip {
		t.Fatalf("setup: checkpoint = %q, want %q", got, tip)
	}
	return f, base, tip
}

func salvageCheckpointRef() string { return "refs/uzi-checkpoints/" + salvageBranch }

func salvageRecoveryRef() string { return pushbroker.RecoveryRefPrefix + salvageRunID.String() }

func salvageRef() string { return pushbroker.SalvageRef(salvageRunID) }

// salvageCreateOpts is a salvage create sourced from the branch checkpoint ref.
func salvageCreateOpts(cloneURL, tip string) pushbroker.CreateRefOptions {
	return pushbroker.CreateRefOptions{CloneURL: cloneURL, Ref: salvageRef(), Tip: tip, SourceRef: salvageCheckpointRef()}
}

func salvageDeleteOpts(cloneURL, tip string) pushbroker.DeleteOptions {
	return pushbroker.DeleteOptions{CloneURL: cloneURL, Ref: salvageRef(), ExpectedOldTip: tip}
}

// foreignRefs snapshots every ref salvage must never write: the branch checkpoint ref,
// this run's recovery ref and main.
func foreignRefs(f *gitFixture) map[string]string {
	out := map[string]string{}
	for _, r := range []string{salvageCheckpointRef(), salvageRecoveryRef(), "refs/heads/main"} {
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
	if !strings.HasPrefix(salvageRef(), pushbroker.SalvageRefPrefix) {
		t.Fatalf("SalvageRef %q is not under SalvageRefPrefix", salvageRef())
	}
}

// TestSalvageCreateRefFromBranchRef is the happy path: the branch checkpoint ref is at
// the tip, so CreateRef creates the salvage ref there, and the branch ref, the recovery
// ref and main are all left exactly as they were.
func TestSalvageCreateRefFromBranchRef(t *testing.T) {
	f, _, tip := salvageFixture(t)
	before := foreignRefs(f)

	if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); err != nil {
		t.Fatalf("CreateRef(salvage) = %v, want nil", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
	assertForeignRefsUntouched(t, f, before)

	tips, err := pushbroker.ListRefTips(context.Background(), pushbroker.ListRefsOptions{CloneURL: f.cloneURL()},
		salvageRef(), salvageCheckpointRef(), salvageRecoveryRef())
	if err != nil {
		t.Fatalf("ListRefTips with a salvage ref: %v", err)
	}
	if tips[salvageRef()] != tip || tips[salvageCheckpointRef()] != tip || tips[salvageRecoveryRef()] != before[salvageRecoveryRef()] {
		t.Fatalf("ListRefTips = %v", tips)
	}
}

// TestSalvageCreateRefFromRecoveryRef: #1810 moved the run's tip to
// refs/uzi-recovery/<run-id> and a newer run owns (or freed) the branch ref. The
// recovery ref at the tip is a valid salvage source, so the salvage ref is created;
// neither source ref moves.
func TestSalvageCreateRefFromRecoveryRef(t *testing.T) {
	opts := func(url, tip string) pushbroker.CreateRefOptions {
		o := salvageCreateOpts(url, tip)
		o.SourceRef = salvageRecoveryRef()
		return o
	}
	t.Run("branch ref moved", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", "+"+tip+":"+salvageRecoveryRef())
		sibling := f.commit("c.txt", "sibling\n", "sibling run")
		f.git("push", "origin", sibling+":"+salvageCheckpointRef())
		before := foreignRefs(f)

		if err := pushbroker.CreateRef(context.Background(), opts(f.cloneURL(), tip)); err != nil {
			t.Fatalf("CreateRef(salvage from recovery) = %v, want nil", err)
		}
		if got := f.originRef(salvageRef()); got != tip {
			t.Fatalf("salvage ref = %q, want tip %q", got, tip)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("branch ref absent", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", "+"+tip+":"+salvageRecoveryRef())
		f.git("push", "origin", ":"+salvageCheckpointRef())
		before := foreignRefs(f)

		if err := pushbroker.CreateRef(context.Background(), opts(f.cloneURL(), tip)); err != nil {
			t.Fatalf("CreateRef(salvage from recovery) = %v, want nil", err)
		}
		if got := f.originRef(salvageRef()); got != tip {
			t.Fatalf("salvage ref = %q, want tip %q", got, tip)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestSalvageCreateRefIdempotent: a salvage ref already at the tip is ErrRefExistsAtTip
// with NO write (a hook refusing every command proves it), even once both sources are
// gone, since the target is checked before the source.
func TestSalvageCreateRefIdempotent(t *testing.T) {
	f, _, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())
	before := foreignRefs(f)
	installPreReceive(t, f, refuseAllWrites)

	if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExistsAtTip) {
		t.Fatalf("CreateRef = %v, want ErrRefExistsAtTip", err)
	}
	assertForeignRefsUntouched(t, f, before)

	// Sources gone (e.g. #1810 settled and deleted them): still the same-tip outcome.
	_ = os.Remove(filepath.Join(f.bare, "hooks", "pre-receive"))
	f.git("push", "origin", ":"+salvageCheckpointRef())
	f.git("push", "origin", ":"+salvageRecoveryRef())
	installPreReceive(t, f, refuseAllWrites)
	if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExistsAtTip) {
		t.Fatalf("re-run without sources = %v, want ErrRefExistsAtTip", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
}

// TestSalvageCreateRefRefusesOtherTip: an existing salvage ref at a different tip is
// never overwritten (ErrRefExists), and nothing else is touched.
func TestSalvageCreateRefRefusesOtherTip(t *testing.T) {
	f, base, tip := salvageFixture(t)
	f.git("push", "origin", base+":"+salvageRef())
	before := foreignRefs(f)

	if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExists) {
		t.Fatalf("CreateRef = %v, want ErrRefExists", err)
	}
	if got := f.originRef(salvageRef()); got != base {
		t.Fatalf("salvage ref = %q, want untouched %q", got, base)
	}
	assertForeignRefsUntouched(t, f, before)
}

// TestSalvageCreateRefSourceMissing: when the named source is not at the tip, nothing is
// created (ErrSourceMissing) and the refs a sibling or #1810 own are left alone.
func TestSalvageCreateRefSourceMissing(t *testing.T) {
	t.Run("branch ref missing", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		f.git("push", "origin", ":"+salvageCheckpointRef())
		before := foreignRefs(f)

		if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrSourceMissing) {
			t.Fatalf("CreateRef = %v, want ErrSourceMissing", err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("branch ref moved", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		sibling := f.commit("c.txt", "sibling\n", "sibling run")
		f.git("push", "origin", sibling+":"+salvageCheckpointRef())
		before := foreignRefs(f)

		if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrSourceMissing) {
			t.Fatalf("CreateRef = %v, want ErrSourceMissing", err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestSalvageCreateRefCreateFailure: when the salvage create is refused (a hook
// declining refs/uzi-salvage/*) or origin is unreachable, CreateRef returns a
// non-sentinel error and no other ref is touched.
func TestSalvageCreateRefCreateFailure(t *testing.T) {
	isSentinel := func(err error) bool {
		return errors.Is(err, pushbroker.ErrRefExists) || errors.Is(err, pushbroker.ErrRefExistsAtTip) ||
			errors.Is(err, pushbroker.ErrSourceMissing) || errors.Is(err, pushbroker.ErrInvalidRef)
	}
	t.Run("create refused", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		before := foreignRefs(f)
		installPreReceive(t, f, "#!/bin/sh\n"+
			"while read old new ref; do\n"+
			"  case \"$ref\" in refs/uzi-salvage/*) echo 'salvage refused by test hook' >&2; exit 1;; esac\n"+
			"done\nexit 0\n")

		err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip))
		if err == nil || isSentinel(err) {
			t.Fatalf("CreateRef = %v, want a non-sentinel error", err)
		}
		if got := f.originRef(salvageRef()); got != "" {
			t.Fatalf("salvage ref = %q, want absent", got)
		}
		assertForeignRefsUntouched(t, f, before)
	})
	t.Run("origin unreachable", func(t *testing.T) {
		f, _, tip := salvageFixture(t)
		before := foreignRefs(f)
		err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(closedHTTPURL(t), tip))
		if err == nil || isSentinel(err) {
			t.Fatalf("CreateRef = %v, want a non-sentinel error", err)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestSalvageCreateRefCreateRace: a concurrent creator writes the salvage ref between
// CreateRef's list and its create (staged by a pre-receive hook that writes the ref, then
// declines ours). CreateRef's read-back decides: the ref at OUR tip is ErrRefExistsAtTip;
// at ANOTHER tip it is ErrRefExists. No other ref moves either way.
func TestSalvageCreateRefCreateRace(t *testing.T) {
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

		if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExistsAtTip) {
			t.Fatalf("CreateRef = %v, want ErrRefExistsAtTip", err)
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

		if err := pushbroker.CreateRef(context.Background(), salvageCreateOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExists) {
			t.Fatalf("CreateRef = %v, want ErrRefExists", err)
		}
		if got := f.originRef(salvageRef()); got != base {
			t.Fatalf("salvage ref = %q, want the racer's %q", got, base)
		}
		assertForeignRefsUntouched(t, f, before)
	})
}

// TestDeleteSalvageRef: Delete{Ref: salvage ref} CAS-deletes it at the matching tip; a
// mismatch or an absent ref is benign nil. The branch and recovery refs never move.
func TestDeleteSalvageRef(t *testing.T) {
	f, base, tip := salvageFixture(t)
	f.git("push", "origin", tip+":"+salvageRef())
	before := foreignRefs(f)
	del := func(expected string) error {
		return pushbroker.Delete(context.Background(), salvageDeleteOpts(f.cloneURL(), expected))
	}

	if err := del(base); err != nil {
		t.Fatalf("mismatched Delete = %v, want nil", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("mismatched Delete moved the ref: %q, want %q", got, tip)
	}
	if err := del(tip); err != nil {
		t.Fatalf("matching Delete = %v, want nil", err)
	}
	if got := f.originRef(salvageRef()); got != "" {
		t.Fatalf("salvage ref = %q, want deleted", got)
	}
	if err := del(tip); err != nil {
		t.Fatalf("Delete of absent ref = %v, want nil", err)
	}
	assertForeignRefsUntouched(t, f, before)
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

// TestSalvagePrefixPositionsRejectedWithoutNetwork: refs/uzi-salvage/ is accepted only in
// the positions PRD #1867 widens (a CreateRef target with a checkpoint or recovery
// source, a CAS Delete, a ListRefTips entry). Every other placement, and every malformed
// salvage ref, is ErrInvalidRef before any request.
func TestSalvagePrefixPositionsRejectedWithoutNetwork(t *testing.T) {
	url, hits := countingServer(t)
	ctx := context.Background()
	tip := strings.Repeat("ab", 20)
	checkpoint, recovery, salvage := salvageCheckpointRef(), salvageRecoveryRef(), salvageRef()

	creates := []struct{ name, ref, source string }{
		{"salvage sourced from a salvage ref", salvage, pushbroker.SalvageRef(uuid.New())},
		{"salvage sourced from a head", salvage, "refs/heads/main"},
		{"salvage with an empty source", salvage, ""},
		{"recovery sourced from a salvage ref", recovery, salvage},
		{"recovery sourced from a recovery ref", recovery, pushbroker.RecoveryRefPrefix + uuid.NewString()},
		{"checkpoint target", checkpoint, salvage},
		{"bare salvage prefix", pushbroker.SalvageRefPrefix, checkpoint},
		{"salvage lookalike prefix", "refs/uzi-salvaged/x", checkpoint},
		{"salvage with dotdot", pushbroker.SalvageRefPrefix + "a..b", checkpoint},
		{"salvage with trailing slash", pushbroker.SalvageRefPrefix + "a/", checkpoint},
		{"salvage with lock suffix", pushbroker.SalvageRefPrefix + "a.lock", checkpoint},
	}
	for _, c := range creates {
		o := pushbroker.CreateRefOptions{CloneURL: url, Ref: c.ref, Tip: tip, SourceRef: c.source}
		if err := pushbroker.CreateRef(ctx, o); !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Errorf("CreateRef %s (Ref=%q SourceRef=%q) = %v, want ErrInvalidRef", c.name, c.ref, c.source, err)
		}
	}

	deletes := []struct{ name, ref, tip string }{
		{"salvage without a tip (CAS only)", salvage, ""},
		{"bare salvage prefix", pushbroker.SalvageRefPrefix, tip},
		{"salvage lookalike prefix", "refs/uzi-salvaged/x", tip},
		{"salvage with dotdot", pushbroker.SalvageRefPrefix + "../heads/main", tip},
		{"salvage with trailing slash", pushbroker.SalvageRefPrefix + "agent/", tip},
		{"salvage with a short tip", salvage, "abc123"},
		{"salvage with a zero tip", salvage, strings.Repeat("0", 40)},
		{"salvage with a non-hex tip", salvage, strings.Repeat("zz", 20)},
	}
	for _, d := range deletes {
		o := pushbroker.DeleteOptions{CloneURL: url, Ref: d.ref, ExpectedOldTip: d.tip}
		if err := pushbroker.Delete(ctx, o); !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Errorf("Delete %s (Ref=%q tip=%q) = %v, want ErrInvalidRef", d.name, d.ref, d.tip, err)
		}
	}

	for _, ref := range []string{pushbroker.SalvageRefPrefix, "refs/uzi-salvaged/x", pushbroker.SalvageRefPrefix + "a..b"} {
		if _, err := pushbroker.ListRefTips(ctx, pushbroker.ListRefsOptions{CloneURL: url}, salvage, ref); !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Errorf("ListRefTips(%q) = %v, want ErrInvalidRef", ref, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("invalid salvage input reached the network %d time(s), want 0", n)
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

// TestSalvageErrorsNeverCarryPAT: a transport failure from the three salvage calls
// (CreateRef, Delete and ListRefTips of a salvage ref) never carries the credential,
// whether it arrives as the BasicAuth password or embedded in the clone URL's userinfo,
// and the scrubbed form the caller persists never does either.
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
			ctx := context.Background()
			co := salvageCreateOpts(u, tip)
			co.Username, co.PAT = "uzi-bot", pat
			do := salvageDeleteOpts(u, tip)
			do.Username, do.PAT = "uzi-bot", pat
			_, lerr := pushbroker.ListRefTips(ctx, pushbroker.ListRefsOptions{CloneURL: u, Username: "uzi-bot", PAT: pat}, salvageRef())
			errs := map[string]error{
				"CreateRef":   pushbroker.CreateRef(ctx, co),
				"Delete":      pushbroker.Delete(ctx, do),
				"ListRefTips": lerr,
			}
			for op, err := range errs {
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

// TestSalvageRefOverSmartHTTP runs the salvage create (from the branch checkpoint ref,
// idempotently re-run) and the salvage CAS Delete over REAL smart HTTP (git
// http-backend), the transport every forge uses. It skips locally when git-http-backend
// is absent and fails in CI (requireGitHTTPBackend).
func TestSalvageRefOverSmartHTTP(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f, _, tip := salvageFixture(t)
	before := foreignRefs(f)
	u := newGitHTTPRemote(t, f, backend).cloneURL(f)
	ctx := context.Background()

	co := salvageCreateOpts(u, tip)
	co.Username, co.PAT = "uzi-bot", redirectTestPAT()
	if err := pushbroker.CreateRef(ctx, co); err != nil {
		t.Fatalf("CreateRef(salvage) over smart HTTP = %v, want nil", err)
	}
	if got := f.originRef(salvageRef()); got != tip {
		t.Fatalf("salvage ref = %q, want tip %q", got, tip)
	}
	if err := pushbroker.CreateRef(ctx, co); !errors.Is(err, pushbroker.ErrRefExistsAtTip) {
		t.Fatalf("second CreateRef(salvage) = %v, want ErrRefExistsAtTip", err)
	}

	do := salvageDeleteOpts(u, tip)
	do.Username, do.PAT = "uzi-bot", redirectTestPAT()
	if err := pushbroker.Delete(ctx, do); err != nil {
		t.Fatalf("Delete(salvage) over smart HTTP: %v", err)
	}
	if got := f.originRef(salvageRef()); got != "" {
		t.Fatalf("salvage ref = %q after Delete, want deleted", got)
	}
	assertForeignRefsUntouched(t, f, before)
}
