package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Issue #4007: terminal preservation against a real git receive-pack (the http-backend harness
// shared with the held tests).

var terminalRunID = uuid.MustParse("7b2f9c40-1d3e-4a85-b6c7-0e9f8a1d2c34")

func terminalRefName() string { return pushbroker.TerminalRef(terminalRunID) }

func terminalOpts(url, tip string, pack []byte) pushbroker.TerminalPackOptions {
	return pushbroker.TerminalPackOptions{
		CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(), RunID: terminalRunID,
		DefaultBranch: "main", Tip: tip, Pack: pack,
	}
}

// termFileFixture is heldHTTPFixture over a file:// origin: enough for every read-only
// preparation check, and it runs where git-http-backend is not installed.
func termFileFixture(t *testing.T) (f *gitFixture, url, base, tip string, pack []byte) {
	t.Helper()
	f = newGitFixture(t)
	base = f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip = f.commit("held.txt", "held work\n", "wip")
	return f, f.cloneURL(), base, tip, f.pack(tip, base)
}

func treeOf(f *gitFixture, rev string) string {
	f.t.Helper()
	return strings.TrimSpace(f.git("rev-parse", rev+"^{tree}"))
}

func listOpts(url string) pushbroker.ListRefsOptions {
	return pushbroker.ListRefsOptions{CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT()}
}

func mustCreateTerminal(t *testing.T, o pushbroker.TerminalPackOptions) {
	t.Helper()
	p, err := pushbroker.PrepareTerminalPack(context.Background(), o)
	if err != nil {
		t.Fatalf("PrepareTerminalPack: %v", err)
	}
	if out, err := pushbroker.SendTerminalCreate(context.Background(), p); err != nil || out != pushbroker.TerminalApplied {
		t.Fatalf("SendTerminalCreate = %v, %v", out, err)
	}
}

func TestTerminalRefNaming(t *testing.T) {
	if got, want := pushbroker.TerminalRef(terminalRunID), "refs/uzi-terminal/7b2f9c40-1d3e-4a85-b6c7-0e9f8a1d2c34"; got != want {
		t.Fatalf("TerminalRef = %q, want %q", got, want)
	}
	upper := uuid.MustParse(strings.ToUpper(terminalRunID.String()))
	if pushbroker.TerminalRef(upper) != pushbroker.TerminalRef(terminalRunID) {
		t.Fatal("TerminalRef must be the canonical lowercase form")
	}
}

// The generic paths must not learn the terminal namespace.
func TestGenericPathsRefuseTerminalRef(t *testing.T) {
	ctx := context.Background()
	for _, ref := range []string{terminalRefName(), "refs/uzi-terminal/x"} {
		err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{
			CloneURL: "https://127.0.0.1:1/never-dialled", Ref: ref, ExpectedOldTip: strings.Repeat("a", 40),
		})
		if !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("Delete(%s) = %v, want ErrInvalidRef", ref, err)
		}
		if _, err := pushbroker.ListRefTips(ctx, pushbroker.ListRefsOptions{CloneURL: "https://127.0.0.1:1/never-dialled"}, ref); !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("ListRefTips(%s) = %v, want ErrInvalidRef", ref, err)
		}
	}
	// A terminal ref is not an accepted pack base either.
	_, url, _, tip, pack := termFileFixture(t)
	o := terminalOpts(url, tip, pack)
	o.BaseRefs = []string{terminalRefName()}
	if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrInvalidRef) {
		t.Fatalf("terminal ref as base = %v, want ErrInvalidRef", err)
	}
}

func TestTerminalCreate(t *testing.T) {
	ctx := context.Background()
	f, _, url, base, tip, pack := heldHTTPFixture(t)
	o := terminalOpts(url, tip, pack)
	o.Roots = []pushbroker.TerminalRoot{
		{Commit: tip, Tree: treeOf(f, tip)},
		{Commit: base, Tree: treeOf(f, base)},
	}
	p, err := pushbroker.PrepareTerminalPack(ctx, o)
	if err != nil {
		t.Fatalf("PrepareTerminalPack: %v", err)
	}
	if p.Ref() != terminalRefName() || p.Tip() != tip || p.PriorTip() != "" {
		t.Fatalf("prepared = %s/%s/%q", p.Ref(), p.Tip(), p.PriorTip())
	}
	if got := f.originRef(terminalRefName()); got != "" {
		t.Fatalf("prepare created the ref at %s; it must be read-only", got)
	}
	if got, present, err := pushbroker.ListTerminalTip(ctx, listOpts(url), terminalRunID); err != nil || present || got != "" {
		t.Fatalf("ListTerminalTip before = %q, %v, %v", got, present, err)
	}
	out, err := pushbroker.SendTerminalCreate(ctx, p)
	if err != nil || out != pushbroker.TerminalApplied {
		t.Fatalf("SendTerminalCreate = %v, %v", out, err)
	}
	if got := f.originRef(terminalRefName()); got != tip {
		t.Fatalf("origin ref = %q, want %s", got, tip)
	}
	if got := f.originRef("refs/heads/main"); got == tip {
		t.Fatal("main must not move")
	}
	if got, present, err := pushbroker.ListTerminalTip(ctx, listOpts(url), terminalRunID); err != nil || !present || got != tip {
		t.Fatalf("ListTerminalTip after = %q, %v, %v", got, present, err)
	}
	// A second create is refused by the remote's own CAS and leaves the ref alone.
	p2, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, pack))
	if err != nil {
		t.Fatal(err)
	}
	f.git("push", "--force", "origin", base+":"+terminalRefName())
	out, err = pushbroker.SendTerminalCreate(ctx, p2)
	if out != pushbroker.TerminalRefused || !errors.Is(err, pushbroker.ErrTerminalRefused) {
		t.Fatalf("create over an existing ref = %v, %v; want refused", out, err)
	}
	if strings.Contains(err.Error(), redirectTestPAT()) {
		t.Fatalf("refusal leaks the credential: %v", err)
	}
	if got := f.originRef(terminalRefName()); got != base {
		t.Fatalf("existing ref moved to %q", got)
	}
}

func TestTerminalFastForwardUpdate(t *testing.T) {
	ctx := context.Background()
	f, _, url, _, tip, pack := heldHTTPFixture(t)
	mustCreateTerminal(t, terminalOpts(url, tip, pack))

	tip2 := f.commit("more.txt", "more\n", "more work")
	o := terminalOpts(url, tip2, f.pack(tip2, tip))
	o.PriorTip = tip
	o.Roots = []pushbroker.TerminalRoot{{Commit: tip, Tree: treeOf(f, tip)}}
	p, err := pushbroker.PrepareTerminalPack(ctx, o)
	if err != nil {
		t.Fatalf("PrepareTerminalPack: %v", err)
	}
	if p.PriorTip() != tip {
		t.Fatalf("PriorTip = %q", p.PriorTip())
	}
	if _, err := pushbroker.SendTerminalCreate(ctx, p); !errors.Is(err, pushbroker.ErrInvalidRef) {
		t.Fatalf("create of a prior-bearing preparation = %v, want ErrInvalidRef", err)
	}
	out, err := pushbroker.SendTerminalUpdate(ctx, p)
	if err != nil || out != pushbroker.TerminalApplied {
		t.Fatalf("SendTerminalUpdate = %v, %v", out, err)
	}
	if got := f.originRef(terminalRefName()); got != tip2 {
		t.Fatalf("origin ref = %q, want %s", got, tip2)
	}
}

func TestTerminalUpdateRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("a tip that does not descend the prior is refused at prepare", func(t *testing.T) {
		f, url, base, tip, _ := termFileFixture(t)
		f.git("push", "origin", tip+":"+terminalRefName())
		f.git("checkout", "-q", "-b", "side", base)
		side := f.commit("side.txt", "side\n", "side")
		o := terminalOpts(url, side, f.pack(side, base))
		o.PriorTip = tip
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrNotDescendant) {
			t.Fatalf("PrepareTerminalPack = %v, want ErrNotDescendant", err)
		}
		if got := f.originRef(terminalRefName()); got != tip {
			t.Fatalf("ref = %q, want untouched at %s", got, tip)
		}
	})

	t.Run("an update to the prior tip itself is refused", func(t *testing.T) {
		f, url, _, tip, _ := termFileFixture(t)
		f.git("push", "origin", tip+":"+terminalRefName())
		o := terminalOpts(url, tip, nil)
		o.PriorTip = tip
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrNotDescendant) {
			t.Fatalf("PrepareTerminalPack = %v, want ErrNotDescendant", err)
		}
	})

	t.Run("a moved ref is never overwritten", func(t *testing.T) {
		f, _, url, base, tip, pack := heldHTTPFixture(t)
		mustCreateTerminal(t, terminalOpts(url, tip, pack))
		tip2 := f.commit("more.txt", "more\n", "more work")
		o := terminalOpts(url, tip2, f.pack(tip2, tip))
		o.PriorTip = tip
		p, err := pushbroker.PrepareTerminalPack(ctx, o)
		if err != nil {
			t.Fatalf("PrepareTerminalPack: %v", err)
		}
		f.git("push", "--force", "origin", base+":"+terminalRefName()) // a racer moves the ref
		out, err := pushbroker.SendTerminalUpdate(ctx, p)
		if out != pushbroker.TerminalRefused || !errors.Is(err, pushbroker.ErrTerminalRefused) {
			t.Fatalf("SendTerminalUpdate = %v, %v; want refused", out, err)
		}
		if got := f.originRef(terminalRefName()); got != base {
			t.Fatalf("ref = %q, want the racer's %s untouched", got, base)
		}
	})

	t.Run("an update with no prior is refused", func(t *testing.T) {
		_, _, url, _, tip, pack := heldHTTPFixture(t)
		p, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, pack))
		if err != nil {
			t.Fatal(err)
		}
		if out, err := pushbroker.SendTerminalUpdate(ctx, p); !errors.Is(err, pushbroker.ErrInvalidRef) || out != pushbroker.TerminalUnknown {
			t.Fatalf("SendTerminalUpdate without prior = %v, %v", out, err)
		}
	})
}

// A tip that already exists on origin under another ref needs no objects.
func TestTerminalZeroObjectCreate(t *testing.T) {
	ctx := context.Background()
	f, _, url, _, tip, _ := heldHTTPFixture(t)
	f.git("push", "origin", tip+":refs/heads/other")

	o := terminalOpts(url, tip, nil)
	o.BaseRefs = []string{"refs/heads/other"}
	p, err := pushbroker.PrepareTerminalPack(ctx, o)
	if err != nil {
		t.Fatalf("PrepareTerminalPack: %v", err)
	}
	out, err := pushbroker.SendTerminalCreate(ctx, p)
	if err != nil || out != pushbroker.TerminalApplied {
		t.Fatalf("SendTerminalCreate = %v, %v", out, err)
	}
	if got := f.originRef(terminalRefName()); got != tip {
		t.Fatalf("origin ref = %q, want %s", got, tip)
	}

	// Without the base that holds it, the same empty pack has no tip.
	o.BaseRefs = nil
	if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrTipMissing) {
		t.Fatalf("empty pack without the tip = %v, want ErrTipMissing", err)
	}
}

func TestTerminalRootVerification(t *testing.T) {
	ctx := context.Background()
	f, url, base, tip, pack := termFileFixture(t)

	t.Run("a root commit that is not in the verified objects is unreachable", func(t *testing.T) {
		o := terminalOpts(url, tip, pack)
		o.Roots = []pushbroker.TerminalRoot{{Commit: strings.Repeat("b", 40), Tree: treeOf(f, tip)}}
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrTerminalRootUnreachable) {
			t.Fatalf("err = %v, want ErrTerminalRootUnreachable", err)
		}
	})

	t.Run("a root present but not reachable from the tip is unreachable", func(t *testing.T) {
		f, url, base, tip, pack := termFileFixture(t)
		f.git("checkout", "-q", "-b", "side", base)
		side := f.commit("side.txt", "side\n", "side")
		f.git("push", "origin", "side:refs/heads/side")
		o := terminalOpts(url, tip, pack)
		o.BaseRefs = []string{"refs/heads/side"}
		o.Roots = []pushbroker.TerminalRoot{{Commit: side, Tree: treeOf(f, side)}}
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrTerminalRootUnreachable) {
			t.Fatalf("err = %v, want ErrTerminalRootUnreachable", err)
		}
	})

	t.Run("a mismatched tree is refused", func(t *testing.T) {
		o := terminalOpts(url, tip, pack)
		o.Roots = []pushbroker.TerminalRoot{{Commit: base, Tree: treeOf(f, tip)}}
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrTerminalTreeMismatch) {
			t.Fatalf("err = %v, want ErrTerminalTreeMismatch", err)
		}
	})

	t.Run("a malformed root is refused", func(t *testing.T) {
		o := terminalOpts(url, tip, pack)
		o.Roots = []pushbroker.TerminalRoot{{Commit: "nothex", Tree: treeOf(f, tip)}}
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("err = %v, want ErrInvalidRef", err)
		}
	})
}

// A pack that carries the tip commit and its tree but not a blob the tree names is not connected.
func TestTerminalMissingObjectRefused(t *testing.T) {
	f, url, base, tip, _ := termFileFixture(t)
	revs := f.git("rev-list", tip, "^"+base, "--objects")
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(revs), "\n") {
		if !strings.HasSuffix(l, " held.txt") {
			keep = append(keep, l)
		}
	}
	if len(keep) == len(strings.Split(strings.TrimSpace(revs), "\n")) {
		t.Fatal("fixture drift: held.txt blob not found to drop")
	}
	cmd := exec.Command("git", "-C", f.work, "pack-objects", "--stdout") //nolint:gosec // G204: test helper against a test-owned temp repo.
	cmd.Env = cleanGitEnv()
	cmd.Stdin = strings.NewReader(strings.Join(keep, "\n") + "\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	p, err := pushbroker.PrepareTerminalPack(context.Background(), terminalOpts(url, tip, out.Bytes()))
	if !errors.Is(err, pushbroker.ErrPackInvalid) || p != nil {
		t.Fatalf("PrepareTerminalPack = %v, %v; want ErrPackInvalid", p, err)
	}
}

// termDeepFixture is a file:// origin whose main holds three commits, so the depth-1 fetched base
// (the third) has an unfetched parent. It returns the third commit as base.
func termDeepFixture(t *testing.T) (f *gitFixture, url, base string) {
	t.Helper()
	f = newGitFixture(t)
	f.commit("a.txt", "one\n", "c1")
	f.commit("a.txt", "two\n", "c2")
	base = f.commit("a.txt", "three\n", "c3")
	f.pushMain()
	return f, f.cloneURL(), base
}

// objectsPack packs the `rev-list --objects` lines of tip ^base minus any line drop() selects.
func objectsPack(t *testing.T, f *gitFixture, tip, base string, drop func(line string) bool) []byte {
	t.Helper()
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(f.git("rev-list", tip, "^"+base, "--objects")), "\n") {
		if !drop(l) {
			keep = append(keep, l)
		}
	}
	cmd := exec.Command("git", "-C", f.work, "pack-objects", "--stdout") //nolint:gosec // G204: test helper against a test-owned temp repo.
	cmd.Env = cleanGitEnv()
	cmd.Stdin = strings.NewReader(strings.Join(keep, "\n") + "\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	return out.Bytes()
}

// The shallow-boundary rule: history may run out only at a depth-1 fetched base tip.
func TestTerminalShallowBoundary(t *testing.T) {
	ctx := context.Background()

	t.Run("history that meets the shallow base is accepted", func(t *testing.T) {
		f, url, base := termDeepFixture(t)
		tip := f.commit("held.txt", "held work\n", "wip")
		p, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, f.pack(tip, base)))
		if err != nil || p == nil {
			t.Fatalf("PrepareTerminalPack = %v, %v; want accepted", p, err)
		}
	})

	t.Run("a non-boundary commit with an absent parent is refused", func(t *testing.T) {
		f, url, base := termDeepFixture(t)
		c4 := f.commit("b.txt", "four\n", "c4") // never on origin
		f.commit("c.txt", "five\n", "c5")
		tip := f.commit("d.txt", "six\n", "c6")
		// The pack carries every tree and blob (including c4's b.txt) and commits c5 and c6, but
		// not the c4 commit itself, so c5 names an absent parent that is not a shallow base.
		pack := objectsPack(t, f, tip, base, func(l string) bool { return strings.HasPrefix(l, c4) })
		p, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, pack))
		if !errors.Is(err, pushbroker.ErrPackInvalid) || p != nil {
			t.Fatalf("PrepareTerminalPack = %v, %v; want ErrPackInvalid", p, err)
		}
	})

	t.Run("a missing tree is refused", func(t *testing.T) {
		f, url, base := termDeepFixture(t)
		if err := os.MkdirAll(filepath.Join(f.work, "dir"), 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(f.work, "dir", "x.txt"), "nested\n")
		f.git("add", "dir/x.txt")
		f.git("commit", "-m", "nested")
		tip := strings.TrimSpace(f.git("rev-parse", "HEAD"))
		pack := objectsPack(t, f, tip, base, func(l string) bool { return strings.HasSuffix(l, " dir") })
		p, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, pack))
		if !errors.Is(err, pushbroker.ErrPackInvalid) || p != nil {
			t.Fatalf("PrepareTerminalPack = %v, %v; want ErrPackInvalid", p, err)
		}
	})
}

// A moved or deleted terminal ref is reported as such at prepare, not as a bad pack.
func TestTerminalUpdatePriorMoved(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		move func(f *gitFixture, base string)
	}{
		{"moved", func(f *gitFixture, base string) { f.git("push", "--force", "origin", base+":"+terminalRefName()) }},
		{"deleted", func(f *gitFixture, _ string) { f.git("push", "origin", ":"+terminalRefName()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, url, base, tip, pack := termFileFixture(t)
			mustCreateTerminal(t, terminalOpts(url, tip, pack))
			tip2 := f.commit("more.txt", "more\n", "more work")
			tc.move(f, base)
			o := terminalOpts(url, tip2, f.pack(tip2, tip))
			o.PriorTip = tip
			p, err := pushbroker.PrepareTerminalPack(ctx, o)
			if !errors.Is(err, pushbroker.ErrTerminalPriorMoved) || p != nil {
				t.Fatalf("PrepareTerminalPack = %v, %v; want ErrTerminalPriorMoved", p, err)
			}
		})
	}
}

func TestTerminalPrepareInputRefusals(t *testing.T) {
	_, url, _, tip, pack := termFileFixture(t)
	cases := []struct {
		name string
		edit func(*pushbroker.TerminalPackOptions)
		want error
	}{
		{"nil run id", func(o *pushbroker.TerminalPackOptions) { o.RunID = uuid.Nil }, pushbroker.ErrInvalidRef},
		{"zero tip", func(o *pushbroker.TerminalPackOptions) { o.Tip = strings.Repeat("0", 40) }, pushbroker.ErrInvalidRef},
		{"malformed prior", func(o *pushbroker.TerminalPackOptions) { o.PriorTip = "nothex" }, pushbroker.ErrInvalidRef},
		{"garbage pack", func(o *pushbroker.TerminalPackOptions) { o.Pack = []byte("not a pack at all, definitely") }, pushbroker.ErrPackInvalid},
		{"tip absent", func(o *pushbroker.TerminalPackOptions) { o.Tip = strings.Repeat("c", 40) }, pushbroker.ErrTipMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := terminalOpts(url, tip, pack)
			tc.edit(&o)
			if p, err := pushbroker.PrepareTerminalPack(context.Background(), o); !errors.Is(err, tc.want) || p != nil {
				t.Fatalf("PrepareTerminalPack = %v, %v; want %v", p, err, tc.want)
			}
		})
	}
}

func TestTerminalCIConfigGuardRefusal(t *testing.T) {
	f, url, tip, pack := ciMovedDefault(t, ".github/workflows/x.yml")
	o := terminalOpts(url, tip, pack)
	o.CIConfigGuard, o.CIConfigPaths = true, ciFloor
	if _, err := pushbroker.PrepareTerminalPack(context.Background(), o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
		t.Fatalf("PrepareTerminalPack = %v, want ErrCIConfigGuard", err)
	}
	if got := f.originRef(terminalRefName()); got != "" {
		t.Fatalf("terminal ref created at %s", got)
	}
}

func TestDeleteTerminal(t *testing.T) {
	ctx := context.Background()

	t.Run("an empty or zero expected tip is refused before any I/O", func(t *testing.T) {
		for _, tipv := range []string{"", strings.Repeat("0", 40), "nothex"} {
			out, err := pushbroker.DeleteTerminal(ctx, pushbroker.DeleteTerminalOptions{
				CloneURL: "https://127.0.0.1:1/never-dialled", RunID: terminalRunID, ExpectedTip: tipv,
			})
			if !errors.Is(err, pushbroker.ErrInvalidRef) || out != pushbroker.TerminalDeleteUnknown {
				t.Fatalf("DeleteTerminal(%q) = %v, %v", tipv, out, err)
			}
		}
		if _, err := pushbroker.DeleteTerminal(ctx, pushbroker.DeleteTerminalOptions{
			CloneURL: "https://127.0.0.1:1/never-dialled", ExpectedTip: strings.Repeat("a", 40),
		}); !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("DeleteTerminal with a nil run id = %v", err)
		}
	})

	t.Run("a wrong tip leaves the ref and the right tip removes it", func(t *testing.T) {
		f, _, url, base, tip, pack := heldHTTPFixture(t)
		mustCreateTerminal(t, terminalOpts(url, tip, pack))
		d := pushbroker.DeleteTerminalOptions{CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(), RunID: terminalRunID, ExpectedTip: base}
		out, err := pushbroker.DeleteTerminal(ctx, d)
		if out != pushbroker.TerminalDeleteMoved || !errors.Is(err, pushbroker.ErrTerminalRefMoved) {
			t.Fatalf("DeleteTerminal at a wrong tip = %v, %v", out, err)
		}
		if got := f.originRef(terminalRefName()); got != tip {
			t.Fatalf("ref = %q, want untouched at %s", got, tip)
		}
		d.ExpectedTip = tip
		out, err = pushbroker.DeleteTerminal(ctx, d)
		if out != pushbroker.TerminalDeleteGone || err != nil {
			t.Fatalf("DeleteTerminal at the tip = %v, %v", out, err)
		}
		if got := f.originRef(terminalRefName()); got != "" {
			t.Fatalf("ref still at %s", got)
		}
		// Already absent: confirmed gone.
		if out, err := pushbroker.DeleteTerminal(ctx, d); out != pushbroker.TerminalDeleteGone || err != nil {
			t.Fatalf("DeleteTerminal of an absent ref = %v, %v", out, err)
		}
	})
}

// A forge that never answers the push must end as unknown, promptly, never refused. The gated
// handler aborts on the request context so the server can shut down.
func TestSendTerminalCreateHungRemoteIsUnknown(t *testing.T) {
	f, remote, _, _, tip, pack := heldHTTPFixture(t)
	inner := remote.srv.Config.Handler
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			<-r.Context().Done()
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	url := front.URL + "/" + filepath.Base(f.bare)

	p, err := pushbroker.PrepareTerminalPack(context.Background(), terminalOpts(url, tip, pack))
	if err != nil {
		t.Fatalf("PrepareTerminalPack: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := pushbroker.SendTerminalCreate(ctx, p)
	if out != pushbroker.TerminalUnknown || err == nil || errors.Is(err, pushbroker.ErrTerminalRefused) {
		t.Fatalf("SendTerminalCreate = %v, %v; want unknown, not refused", out, err)
	}
	if got := f.originRef(terminalRefName()); got != "" {
		t.Fatalf("terminal ref created at %s through a hung push", got)
	}
}

// Preparation-only checks over file://, so they run without git-http-backend.
func TestTerminalPrepareLocalOrigin(t *testing.T) {
	ctx := context.Background()

	t.Run("zero-object pack with the tip on origin under another ref", func(t *testing.T) {
		f, url, _, tip, _ := termFileFixture(t)
		f.git("push", "origin", tip+":refs/heads/other")
		o := terminalOpts(url, tip, nil)
		o.BaseRefs = []string{"refs/heads/other"}
		p, err := pushbroker.PrepareTerminalPack(ctx, o)
		if err != nil || p.Tip() != tip || p.Ref() != terminalRefName() {
			t.Fatalf("PrepareTerminalPack = %v, %v", p, err)
		}
		o.BaseRefs = nil
		if _, err := pushbroker.PrepareTerminalPack(ctx, o); !errors.Is(err, pushbroker.ErrTipMissing) {
			t.Fatalf("empty pack without the tip = %v, want ErrTipMissing", err)
		}
	})

	t.Run("fast-forward update with declared roots", func(t *testing.T) {
		f, url, _, tip, _ := termFileFixture(t)
		f.git("push", "origin", tip+":"+terminalRefName())
		tip2 := f.commit("more.txt", "more\n", "more work")
		o := terminalOpts(url, tip2, f.pack(tip2, tip))
		o.PriorTip = tip
		o.Roots = []pushbroker.TerminalRoot{{Commit: tip, Tree: treeOf(f, tip)}, {Commit: tip2, Tree: treeOf(f, tip2)}}
		p, err := pushbroker.PrepareTerminalPack(ctx, o)
		if err != nil || p.PriorTip() != tip || p.Tip() != tip2 {
			t.Fatalf("PrepareTerminalPack = %v, %v", p, err)
		}
	})
}

// Send and delete over file:// so they run without git-http-backend. go-git's file transport execs
// the host's git-receive-pack binary (not an in-process server), and that binary enforces the
// old-value compare-and-swap with an explicit "ng", so the moved-ref and second-create outcomes
// are deterministically TerminalRefused here. This depends on git being installed on the test host.
func TestTerminalSendAndDeleteLocalOrigin(t *testing.T) {
	ctx := context.Background()

	t.Run("create applied then a second create refused", func(t *testing.T) {
		f, url, base, tip, pack := termFileFixture(t)
		mustCreateTerminal(t, terminalOpts(url, tip, pack))
		if got := f.originRef(terminalRefName()); got != tip {
			t.Fatalf("origin ref = %q, want %s", got, tip)
		}
		p2, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, pack))
		if err != nil {
			t.Fatal(err)
		}
		f.git("push", "--force", "origin", base+":"+terminalRefName())
		out, err := pushbroker.SendTerminalCreate(ctx, p2)
		if out != pushbroker.TerminalRefused || !errors.Is(err, pushbroker.ErrTerminalRefused) {
			t.Fatalf("second create = %v, %v; want refused", out, err)
		}
		if got := f.originRef(terminalRefName()); got != base {
			t.Fatalf("existing ref moved to %q", got)
		}
	})

	t.Run("fast-forward update applied", func(t *testing.T) {
		f, url, _, tip, pack := termFileFixture(t)
		mustCreateTerminal(t, terminalOpts(url, tip, pack))
		tip2 := f.commit("more.txt", "more\n", "more work")
		o := terminalOpts(url, tip2, f.pack(tip2, tip))
		o.PriorTip = tip
		p, err := pushbroker.PrepareTerminalPack(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := pushbroker.SendTerminalUpdate(ctx, p); err != nil || out != pushbroker.TerminalApplied {
			t.Fatalf("SendTerminalUpdate = %v, %v", out, err)
		}
		if got := f.originRef(terminalRefName()); got != tip2 {
			t.Fatalf("origin ref = %q, want %s", got, tip2)
		}
	})

	t.Run("moved-ref update is never applied", func(t *testing.T) {
		f, url, base, tip, pack := termFileFixture(t)
		mustCreateTerminal(t, terminalOpts(url, tip, pack))
		tip2 := f.commit("more.txt", "more\n", "more work")
		o := terminalOpts(url, tip2, f.pack(tip2, tip))
		o.PriorTip = tip
		p, err := pushbroker.PrepareTerminalPack(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		f.git("push", "--force", "origin", base+":"+terminalRefName())
		out, err := pushbroker.SendTerminalUpdate(ctx, p)
		if out != pushbroker.TerminalRefused || !errors.Is(err, pushbroker.ErrTerminalRefused) {
			t.Fatalf("SendTerminalUpdate = %v, %v; want refused", out, err)
		}
		if got := f.originRef(terminalRefName()); got != base {
			t.Fatalf("ref = %q, want the racer's %s untouched", got, base)
		}
	})

	t.Run("update with no prior is ErrInvalidRef", func(t *testing.T) {
		_, url, _, tip, pack := termFileFixture(t)
		p, err := pushbroker.PrepareTerminalPack(ctx, terminalOpts(url, tip, pack))
		if err != nil {
			t.Fatal(err)
		}
		if out, err := pushbroker.SendTerminalUpdate(ctx, p); !errors.Is(err, pushbroker.ErrInvalidRef) || out != pushbroker.TerminalUnknown {
			t.Fatalf("SendTerminalUpdate = %v, %v", out, err)
		}
	})

	t.Run("delete at a wrong tip is moved, at the right tip is gone", func(t *testing.T) {
		f, url, base, tip, pack := termFileFixture(t)
		mustCreateTerminal(t, terminalOpts(url, tip, pack))
		d := pushbroker.DeleteTerminalOptions{CloneURL: url, RunID: terminalRunID, ExpectedTip: base}
		out, err := pushbroker.DeleteTerminal(ctx, d)
		if out != pushbroker.TerminalDeleteMoved || !errors.Is(err, pushbroker.ErrTerminalRefMoved) {
			t.Fatalf("wrong-tip delete = %v, %v", out, err)
		}
		if got := f.originRef(terminalRefName()); got != tip {
			t.Fatalf("ref = %q, want kept at %s", got, tip)
		}
		d.ExpectedTip = tip
		if out, err := pushbroker.DeleteTerminal(ctx, d); out != pushbroker.TerminalDeleteGone || err != nil {
			t.Fatalf("right-tip delete = %v, %v", out, err)
		}
		if got := f.originRef(terminalRefName()); got != "" {
			t.Fatalf("ref still at %s", got)
		}
	})
}
