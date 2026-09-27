package pushbroker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// CreateRef (PRD #1810 D2) and Delete's Ref option. The list-level outcomes are
// decided before any receive-pack session, so they run over file://. Everything that
// reaches the wire runs against a real git receive-pack (git http-backend, the
// redirect_origin_test.go harness): go-git's in-process file:// server neither reads
// a pack for a create nor enforces the Old compare-and-swap, so it cannot prove either.

const (
	recoveryRef   = pushbroker.RecoveryRefPrefix + "run-1"
	checkpointRef = "refs/uzi-checkpoints/main"
)

// seedCheckpoint pushes main (base) and a checkpoint ref one commit ahead (tip), and
// returns both shas. After it, origin holds base and tip; tip is reachable only from
// the checkpoint ref.
func seedCheckpoint(t *testing.T, f *gitFixture) (base, tip string) {
	t.Helper()
	base = f.commit("a.txt", "base\n", "base")
	f.pushMain()
	tip = f.commit("b.txt", "one\n", "c1")
	f.git("push", "origin", tip+":"+checkpointRef)
	return base, tip
}

func createOpts(cloneURL, tip string) pushbroker.CreateRefOptions {
	return pushbroker.CreateRefOptions{CloneURL: cloneURL, Ref: recoveryRef, Tip: tip, SourceRef: checkpointRef}
}

func TestCreateRefListOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("exists at tip", func(t *testing.T) {
		f := newGitFixture(t)
		_, tip := seedCheckpoint(t, f)
		f.git("push", "origin", tip+":"+recoveryRef)
		if err := pushbroker.CreateRef(ctx, createOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExistsAtTip) {
			t.Fatalf("CreateRef = %v, want ErrRefExistsAtTip", err)
		}
	})
	t.Run("exists elsewhere", func(t *testing.T) {
		f := newGitFixture(t)
		base, tip := seedCheckpoint(t, f)
		f.git("push", "origin", base+":"+recoveryRef)
		if err := pushbroker.CreateRef(ctx, createOpts(f.cloneURL(), tip)); !errors.Is(err, pushbroker.ErrRefExists) {
			t.Fatalf("CreateRef = %v, want ErrRefExists", err)
		}
		if got := f.originRef(recoveryRef); got != base {
			t.Fatalf("recovery ref moved to %q, want untouched at %s", got, base)
		}
	})
	t.Run("source absent", func(t *testing.T) {
		f := newGitFixture(t)
		_, tip := seedCheckpoint(t, f)
		o := createOpts(f.cloneURL(), tip)
		o.SourceRef = "refs/uzi-checkpoints/other"
		if err := pushbroker.CreateRef(ctx, o); !errors.Is(err, pushbroker.ErrSourceMissing) {
			t.Fatalf("CreateRef = %v, want ErrSourceMissing", err)
		}
		if got := f.originRef(recoveryRef); got != "" {
			t.Fatalf("recovery ref created at %s despite a missing source", got)
		}
	})
	t.Run("source at another tip", func(t *testing.T) {
		f := newGitFixture(t)
		base, _ := seedCheckpoint(t, f)
		// base IS on origin (it is main), but the checkpoint does not point at it.
		if err := pushbroker.CreateRef(ctx, createOpts(f.cloneURL(), base)); !errors.Is(err, pushbroker.ErrSourceMissing) {
			t.Fatalf("CreateRef = %v, want ErrSourceMissing", err)
		}
		if got := f.originRef(recoveryRef); got != "" {
			t.Fatalf("recovery ref created at %s despite a mismatched source", got)
		}
	})
	t.Run("empty origin", func(t *testing.T) {
		f := newGitFixture(t)
		o := createOpts(f.cloneURL(), "0123456789abcdef0123456789abcdef01234567")
		if err := pushbroker.CreateRef(ctx, o); !errors.Is(err, pushbroker.ErrSourceMissing) {
			t.Fatalf("CreateRef on an empty origin = %v, want ErrSourceMissing", err)
		}
	})
}

// httpFixture is a seeded fixture served by a real git receive-pack over TLS.
func httpFixture(t *testing.T) (f *gitFixture, url, base, tip string) {
	t.Helper()
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f = newGitFixture(t)
	base, tip = seedCheckpoint(t, f)
	url = newGitHTTPRemote(t, f, backend).cloneURL(f)
	return f, url, base, tip
}

func TestCreateRefOverRealReceivePack(t *testing.T) {
	ctx := context.Background()

	t.Run("creates when absent, then reports exists at tip", func(t *testing.T) {
		f, url, _, tip := httpFixture(t)
		if err := pushbroker.CreateRef(ctx, createOpts(url, tip)); err != nil {
			t.Fatalf("CreateRef: %v", err)
		}
		if got := f.originRef(recoveryRef); got != tip {
			t.Fatalf("origin %s = %q, want %s", recoveryRef, got, tip)
		}
		if got := f.originRef(checkpointRef); got != tip {
			t.Fatalf("source ref moved to %q", got)
		}
		if err := pushbroker.CreateRef(ctx, createOpts(url, tip)); !errors.Is(err, pushbroker.ErrRefExistsAtTip) {
			t.Fatalf("second CreateRef = %v, want ErrRefExistsAtTip", err)
		}
	})

	t.Run("refuses when the ref exists at another tip", func(t *testing.T) {
		f, url, base, tip := httpFixture(t)
		f.git("push", "origin", base+":"+recoveryRef)
		if err := pushbroker.CreateRef(ctx, createOpts(url, tip)); !errors.Is(err, pushbroker.ErrRefExists) {
			t.Fatalf("CreateRef = %v, want ErrRefExists", err)
		}
		if got := f.originRef(recoveryRef); got != base {
			t.Fatalf("recovery ref moved to %q, want %s", got, base)
		}
	})

	t.Run("refuses a missing source", func(t *testing.T) {
		f, url, base, _ := httpFixture(t)
		if err := pushbroker.CreateRef(ctx, createOpts(url, base)); !errors.Is(err, pushbroker.ErrSourceMissing) {
			t.Fatalf("CreateRef = %v, want ErrSourceMissing", err)
		}
		if got := f.originRef(recoveryRef); got != "" {
			t.Fatalf("recovery ref created at %s", got)
		}
	})

	// The empty pack is required: the same create with no pack is refused by a real
	// receive-pack, which reads a pack for every non-delete command.
	t.Run("a create with no pack is refused", func(t *testing.T) {
		f, url, _, tip := httpFixture(t)
		err := pushbroker.CreateRefWithPack(ctx, createOpts(url, tip), nil)
		if err == nil {
			t.Fatal("a pack-less create was accepted; the empty-pack requirement is unproven")
		}
		if errors.Is(err, pushbroker.ErrRefExists) {
			t.Fatalf("pack-less refusal misclassified as ErrRefExists: %v", err)
		}
		t.Logf("pack-less create refused as expected: %v", err)
		if got := f.originRef(recoveryRef); got != "" {
			t.Fatalf("recovery ref created at %s by a pack-less create", got)
		}
		// Control: the identical call with the empty pack lands.
		if err := pushbroker.CreateRefWithPack(ctx, createOpts(url, tip), pushbroker.EmptyPack()); err != nil {
			t.Fatalf("control with the empty pack: %v", err)
		}
		if got := f.originRef(recoveryRef); got != tip {
			t.Fatalf("control: origin %s = %q, want %s", recoveryRef, got, tip)
		}
	})

	// The wire half of the CAS: with the list checks skipped, the remote itself
	// refuses Old = zero for a ref that exists, and the refusal maps to ErrRefExists.
	t.Run("wire CAS refuses an existing ref", func(t *testing.T) {
		f, url, base, tip := httpFixture(t)
		f.git("push", "origin", base+":"+recoveryRef)
		err := pushbroker.PushCreateSkippingList(ctx, url, recoveryRef, tip, pushbroker.EmptyPack())
		if !errors.Is(err, pushbroker.ErrRefExists) {
			t.Fatalf("wire create over an existing ref = %v, want ErrRefExists", err)
		}
		if got := f.originRef(recoveryRef); got != base {
			t.Fatalf("recovery ref moved to %q, want %s", got, base)
		}
	})

	// The remote's connectivity check is the second guard on the tip's presence: a
	// create naming an object origin does not hold is refused, list checks skipped.
	t.Run("wire refuses a tip origin does not hold", func(t *testing.T) {
		f, url, _, _ := httpFixture(t)
		local := f.commit("c.txt", "never pushed\n", "local only")
		err := pushbroker.PushCreateSkippingList(ctx, url, recoveryRef, local, pushbroker.EmptyPack())
		if err == nil {
			t.Fatal("a create naming an absent object was accepted")
		}
		t.Logf("absent-object create refused as expected: %v", err)
		if got := f.originRef(recoveryRef); got != "" {
			t.Fatalf("recovery ref created at %s", got)
		}
	})
}

func TestDeleteRecoveryRefOverRealReceivePack(t *testing.T) {
	ctx := context.Background()

	t.Run("CAS delete at the expected tip", func(t *testing.T) {
		f, url, _, tip := httpFixture(t)
		f.git("push", "origin", tip+":"+recoveryRef)
		if err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{CloneURL: url, Ref: recoveryRef, ExpectedOldTip: tip}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got := f.originRef(recoveryRef); got != "" {
			t.Fatalf("recovery ref still at %s after a CAS delete", got)
		}
		if got := f.originRef(checkpointRef); got != tip {
			t.Fatalf("Ref must replace the branch-derived name; checkpoint ref now %q", got)
		}
	})

	t.Run("CAS delete leaves a ref at another tip", func(t *testing.T) {
		f, url, base, tip := httpFixture(t)
		f.git("push", "origin", tip+":"+recoveryRef)
		if err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{CloneURL: url, Ref: recoveryRef, ExpectedOldTip: base}); err != nil {
			t.Fatalf("Delete = %v, want nil (benign)", err)
		}
		if got := f.originRef(recoveryRef); got != tip {
			t.Fatalf("recovery ref = %q, want untouched at %s", got, tip)
		}
	})

	t.Run("CAS delete of an absent ref", func(t *testing.T) {
		f, url, _, tip := httpFixture(t)
		if err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{CloneURL: url, Ref: recoveryRef, ExpectedOldTip: tip}); err != nil {
			t.Fatalf("Delete = %v, want nil", err)
		}
		if got := f.originRef(checkpointRef); got != tip {
			t.Fatalf("checkpoint ref = %q, want untouched", got)
		}
	})
}
