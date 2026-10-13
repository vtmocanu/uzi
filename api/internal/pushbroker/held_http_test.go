package pushbroker_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Issue #2545: held-work publication against a real git receive-pack (the git http-backend
// harness of redirect_origin_test.go). go-git's in-process file:// server neither reads a pack
// for a create nor reports a per-command ng, so only the HTTP backend can prove the create,
// the refusal and the CAS.

var heldRunID = uuid.MustParse("3c1d7e52-9a4b-4f68-8d21-5e0b6a7c9d13")

func heldRefName() string { return pushbroker.HeldRef(heldRunID, 2) }

// heldHTTPFixture is a served origin whose main holds base, and a tip one commit ahead whose
// pack (tip excluding base) is what the worker would ship. The tip is NOT on origin.
func heldHTTPFixture(t *testing.T) (f *gitFixture, remote *gitHTTPRemote, url, base, tip string, pack []byte) {
	t.Helper()
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f = newGitFixture(t)
	base = f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip = f.commit("held.txt", "held work\n", "wip")
	pack = f.pack(tip, base)
	remote = newGitHTTPRemote(t, f, backend)
	return f, remote, remote.cloneURL(f), base, tip, pack
}

func heldOpts(url, tip string, pack []byte) pushbroker.HeldPackOptions {
	return pushbroker.HeldPackOptions{
		CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(), RunID: heldRunID, Generation: 2,
		Branch: "main", DefaultBranch: "main", Tip: tip, Pack: pack,
	}
}

func TestHeldRefNaming(t *testing.T) {
	if got, want := pushbroker.HeldRef(heldRunID, 2), "refs/uzi-held/3c1d7e52-9a4b-4f68-8d21-5e0b6a7c9d13/2"; got != want {
		t.Fatalf("HeldRef = %q, want %q", got, want)
	}
	if !strings.HasPrefix(pushbroker.HeldRef(heldRunID, 1), pushbroker.HeldRefPrefix) {
		t.Fatal("HeldRef must sit under HeldRefPrefix")
	}
}

func TestHeldCreateOverRealReceivePack(t *testing.T) {
	ctx := context.Background()

	t.Run("prepare is read-only and send creates the ref at the tip", func(t *testing.T) {
		f, _, url, _, tip, pack := heldHTTPFixture(t)
		p, err := pushbroker.PrepareHeldPack(ctx, heldOpts(url, tip, pack))
		if err != nil {
			t.Fatalf("PrepareHeldPack: %v", err)
		}
		if p.Ref() != heldRefName() || p.Tip() != tip {
			t.Fatalf("prepared ref/tip = %s/%s, want %s/%s", p.Ref(), p.Tip(), heldRefName(), tip)
		}
		if got := f.originRef(heldRefName()); got != "" {
			t.Fatalf("PrepareHeldPack created %s at %s; it must be read-only", heldRefName(), got)
		}
		out, err := pushbroker.SendHeldCreate(ctx, p)
		if err != nil || out != pushbroker.HeldCreateCreated {
			t.Fatalf("SendHeldCreate = %v, %v; want created, nil", out, err)
		}
		if got := f.originRef(heldRefName()); got != tip {
			t.Fatalf("origin %s = %q, want %s", heldRefName(), got, tip)
		}
		// The pack carried the work: origin can now resolve the commit.
		if out := run(t, "", "git", "-C", f.bare, "cat-file", "-t", tip); strings.TrimSpace(out) != "commit" {
			t.Fatalf("origin cannot resolve the held commit: %q", out)
		}
		if got := f.originRef("refs/heads/main"); got == tip {
			t.Fatal("main must not move")
		}
		tips, err := pushbroker.ListRefTips(ctx, pushbroker.ListRefsOptions{CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT()}, heldRefName())
		if err != nil || tips[heldRefName()] != tip {
			t.Fatalf("ListRefTips = %v, %v; want the held ref at the tip", tips, err)
		}
	})

	// An existing ref is refused by the remote's own compare-and-swap, and only an explicit ng
	// for our command may be reported as refused.
	t.Run("an existing ref is refused with a definitive ng and left untouched", func(t *testing.T) {
		f, _, url, base, tip, pack := heldHTTPFixture(t)
		f.git("push", "origin", base+":"+heldRefName())
		p, err := pushbroker.PrepareHeldPack(ctx, heldOpts(url, tip, pack))
		if err != nil {
			t.Fatalf("PrepareHeldPack: %v", err)
		}
		out, err := pushbroker.SendHeldCreate(ctx, p)
		if out != pushbroker.HeldCreateRefused || !errors.Is(err, pushbroker.ErrHeldCreateRefused) {
			t.Fatalf("SendHeldCreate = %v, %v; want refused wrapping ErrHeldCreateRefused", out, err)
		}
		if strings.Contains(err.Error(), redirectTestPAT()) {
			t.Fatalf("refusal leaks the credential: %v", err)
		}
		if got := f.originRef(heldRefName()); got != base {
			t.Fatalf("held ref moved to %q, want untouched at %s", got, base)
		}
	})

	t.Run("a ref already at the tip reads back as created", func(t *testing.T) {
		f, _, url, _, tip, pack := heldHTTPFixture(t)
		p, err := pushbroker.PrepareHeldPack(ctx, heldOpts(url, tip, pack))
		if err != nil {
			t.Fatalf("PrepareHeldPack: %v", err)
		}
		f.git("push", "origin", tip+":"+heldRefName())
		out, err := pushbroker.SendHeldCreate(ctx, p)
		if out != pushbroker.HeldCreateCreated || err != nil {
			t.Fatalf("SendHeldCreate over our own tip = %v, %v; want created (no orphan left behind a refusal)", out, err)
		}
	})
}

func TestPrepareHeldPackRefusals(t *testing.T) {
	ctx := context.Background()
	_, _, url, base, tip, pack := heldHTTPFixture(t)
	cases := []struct {
		name string
		edit func(*pushbroker.HeldPackOptions)
		want error
	}{
		{"nil run id", func(o *pushbroker.HeldPackOptions) { o.RunID = uuid.Nil }, pushbroker.ErrInvalidRef},
		{"zero generation", func(o *pushbroker.HeldPackOptions) { o.Generation = 0 }, pushbroker.ErrInvalidRef},
		{"zero tip", func(o *pushbroker.HeldPackOptions) { o.Tip = strings.Repeat("0", 40) }, pushbroker.ErrInvalidRef},
		{"malformed tip", func(o *pushbroker.HeldPackOptions) { o.Tip = "nothex" }, pushbroker.ErrInvalidRef},
		{"unmanaged base ref", func(o *pushbroker.HeldPackOptions) { o.BaseRefs = []string{"refs/tags/v1"} }, pushbroker.ErrInvalidRef},
		{"empty pack", func(o *pushbroker.HeldPackOptions) { o.Pack = nil }, pushbroker.ErrPackInvalid},
		{"garbage pack", func(o *pushbroker.HeldPackOptions) { o.Pack = []byte("not a pack at all, definitely") }, pushbroker.ErrPackInvalid},
		// The pack carries the work, but the declared tip is another commit: origin holds only
		// base, and the pack's commit is not the tip.
		{"tip absent from the pack", func(o *pushbroker.HeldPackOptions) { o.Tip = base[:39] + flip(base[39]) }, pushbroker.ErrTipMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := heldOpts(url, tip, pack)
			tc.edit(&o)
			p, err := pushbroker.PrepareHeldPack(ctx, o)
			if !errors.Is(err, tc.want) || p != nil {
				t.Fatalf("PrepareHeldPack = %v, %v; want %v", p, err, tc.want)
			}
		})
	}
}

func flip(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}

// A held ref is removed only compare-and-swap; the unconditional form is a caller bug.
func TestDeleteHeldRef(t *testing.T) {
	ctx := context.Background()
	t.Run("an unconditional delete is refused before any I/O", func(t *testing.T) {
		err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{CloneURL: "https://127.0.0.1:1/never-dialled", Ref: heldRefName()})
		if !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("Delete without ExpectedOldTip = %v, want ErrInvalidRef", err)
		}
	})
	t.Run("a malformed held ref is refused", func(t *testing.T) {
		err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{
			CloneURL: "https://127.0.0.1:1/never-dialled", Ref: pushbroker.HeldRefPrefix,
			ExpectedOldTip: strings.Repeat("a", 40),
		})
		if !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("Delete of the bare prefix = %v, want ErrInvalidRef", err)
		}
	})
	t.Run("CAS delete at the recorded tip removes it; another tip leaves it", func(t *testing.T) {
		f, _, url, base, tip, _ := heldHTTPFixture(t)
		f.git("push", "origin", tip+":"+heldRefName())
		opts := pushbroker.DeleteOptions{CloneURL: url, Ref: heldRefName(), Username: "uzi-bot", PAT: redirectTestPAT()}
		opts.ExpectedOldTip = base
		if err := pushbroker.Delete(ctx, opts); err != nil {
			t.Fatalf("CAS delete at another tip = %v, want nil (benign)", err)
		}
		if got := f.originRef(heldRefName()); got != tip {
			t.Fatalf("held ref = %q after a mismatched CAS, want untouched at %s", got, tip)
		}
		opts.ExpectedOldTip = tip
		if err := pushbroker.Delete(ctx, opts); err != nil {
			t.Fatalf("CAS delete: %v", err)
		}
		if got := f.originRef(heldRefName()); got != "" {
			t.Fatalf("held ref still at %s after a CAS delete", got)
		}
	})
	t.Run("CreateRef still refuses a held target", func(t *testing.T) {
		err := pushbroker.CreateRef(ctx, pushbroker.CreateRefOptions{
			CloneURL: "https://127.0.0.1:1/never-dialled", Ref: heldRefName(), Tip: strings.Repeat("a", 40), SourceRef: checkpointRef,
		})
		if !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("CreateRef into the held namespace = %v, want ErrInvalidRef", err)
		}
	})
}

// newDelayingFront serves the same origin through a front that forwards a receive-pack POST to
// the real http-backend IMMEDIATELY (the push is applied) but holds the response for delay.
// A client whose deadline is shorter than delay sees only a timeout.
func newDelayingFront(t *testing.T, remote *gitHTTPRemote, f *gitFixture, delay time.Duration) (cloneURL string) {
	t.Helper()
	inner := remote.srv.Config.Handler
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r) // the push is applied here
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = io.Copy(w, rec.Body)
	}))
	t.Cleanup(front.Close)
	return front.URL + "/" + filepath.Base(f.bare)
}

// A forge that applies the push but answers after the client deadline leaves the outcome
// unknown, never refused, and a later listing finds the ref at the tip.
func TestHeldCreateAppliedAfterClientDeadlineIsUnknown(t *testing.T) {
	f, remote, _, _, tip, pack := heldHTTPFixture(t)
	front := newDelayingFront(t, remote, f, 4*time.Second)

	prep, err := pushbroker.PrepareHeldPack(context.Background(), heldOpts(front, tip, pack))
	if err != nil {
		t.Fatalf("PrepareHeldPack: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := pushbroker.SendHeldCreate(ctx, prep)
	if out != pushbroker.HeldCreateUnknown || err == nil {
		t.Fatalf("SendHeldCreate = %v, %v; want unknown with a cause", out, err)
	}
	if errors.Is(err, pushbroker.ErrHeldCreateRefused) {
		t.Fatalf("a timeout must never read as a refusal: %v", err)
	}
	// The forge applied the push regardless; a later listing proves it.
	if got := f.originRef(heldRefName()); got != tip {
		t.Fatalf("origin %s = %q, want the push applied at %s", heldRefName(), got, tip)
	}
	tips, err := pushbroker.ListRefTips(context.Background(), pushbroker.ListRefsOptions{CloneURL: front}, heldRefName())
	if err != nil || tips[heldRefName()] != tip {
		t.Fatalf("later ListRefTips = %v, %v; want the ref at the tip", tips, err)
	}
}

// A forge that fails the push with a gateway error (no report at all) is NOT a refusal, even
// though the read-back works and shows no ref: only an explicit ng may refuse, and an absent ref
// never makes an unknown outcome terminal.
func TestHeldCreateGatewayErrorIsUnknownNotRefused(t *testing.T) {
	f, remote, _, _, tip, pack := heldHTTPFixture(t)
	inner := remote.srv.Config.Handler
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	url := front.URL + "/" + filepath.Base(f.bare)

	prep, err := pushbroker.PrepareHeldPack(context.Background(), heldOpts(url, tip, pack))
	if err != nil {
		t.Fatalf("PrepareHeldPack: %v", err)
	}
	out, err := pushbroker.SendHeldCreate(context.Background(), prep)
	if out != pushbroker.HeldCreateUnknown || err == nil || errors.Is(err, pushbroker.ErrHeldCreateRefused) {
		t.Fatalf("SendHeldCreate = %v, %v; want unknown, not refused", out, err)
	}
	if got := f.originRef(heldRefName()); got != "" {
		t.Fatalf("held ref created at %s through a failed push", got)
	}
}

// A complete report whose unpack failed and that carries no ng line for our command is a
// rejection of the push but not an explicit refusal of the ref: SendHeldCreate may only call a
// create Refused on an ng, so this stays Unknown (and a later listing decides).
func TestHeldCreateUnpackFailureWithoutNgIsUnknownNotRefused(t *testing.T) {
	f, remote, _, _, tip, pack := heldHTTPFixture(t)
	inner := remote.srv.Config.Handler
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			enc := pktline.NewEncoder(w)
			_ = enc.EncodeString("unpack index-pack abnormal exit\n")
			_ = enc.Flush()
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	url := front.URL + "/" + filepath.Base(f.bare)

	prep, err := pushbroker.PrepareHeldPack(context.Background(), heldOpts(url, tip, pack))
	if err != nil {
		t.Fatalf("PrepareHeldPack: %v", err)
	}
	out, err := pushbroker.SendHeldCreate(context.Background(), prep)
	if out != pushbroker.HeldCreateUnknown || err == nil || errors.Is(err, pushbroker.ErrHeldCreateRefused) {
		t.Fatalf("SendHeldCreate = %v, %v; want unknown, not refused", out, err)
	}
	if got := f.originRef(heldRefName()); got != "" {
		t.Fatalf("held ref created at %s through a failed unpack", got)
	}
}
