package pushbroker

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // G505: verifying the git packfile trailer, which the format defines as SHA-1.
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
)

const testTip = "0123456789abcdef0123456789abcdef01234567"

func TestEmptyPackIsAValidZeroObjectPack(t *testing.T) {
	p := emptyPack()
	if len(p) != 32 {
		t.Fatalf("empty pack is %d bytes, want 32", len(p))
	}
	want := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	if !bytes.Equal(p[:12], want) {
		t.Fatalf("header = %x, want %x", p[:12], want)
	}
	sum := sha1.Sum(p[:12]) //nolint:gosec // G401: git pack trailer.
	if !bytes.Equal(p[12:], sum[:]) {
		t.Fatalf("trailer = %x, want sha1(header) %x", p[12:], sum)
	}
	s := packfile.NewScanner(bytes.NewReader(p))
	v, n, err := s.Header()
	if err != nil || v != 2 || n != 0 {
		t.Fatalf("go-git scanner reads (version %d, objects %d, err %v), want (2, 0, nil)", v, n, err)
	}
	if _, err := s.Checksum(); err != nil {
		t.Fatalf("go-git scanner rejects the trailer: %v", err)
	}
}

func TestCreateRefRequestIsOldZeroCreate(t *testing.T) {
	ref := plumbing.ReferenceName(RecoveryRefPrefix + "run-1")
	tip := plumbing.NewHash(testTip)
	req := createRefRequest(capability.NewList(), ref, tip, emptyPack())
	if len(req.Commands) != 1 {
		t.Fatalf("commands = %d, want exactly 1", len(req.Commands))
	}
	c := req.Commands[0]
	if c.Name != ref || c.New != tip {
		t.Fatalf("command = %+v, want %s -> %s", c, ref, tip)
	}
	if !c.Old.IsZero() {
		t.Fatalf("command Old = %s; a create must bind Old to the zero hash (the wire CAS)", c.Old)
	}
	if req.Packfile == nil {
		t.Fatal("create request carries no pack; receive-pack reads one for every non-delete command")
	}
	got, err := io.ReadAll(req.Packfile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, emptyPack()) {
		t.Fatalf("pack = %x, want the empty pack", got)
	}

	if req := createRefRequest(capability.NewList(), ref, tip, nil); req.Packfile != nil {
		t.Fatal("a nil pack must leave Packfile nil (the variant the wire test proves is refused)")
	}
}

func TestCreateRefRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	good := CreateRefOptions{
		// An unreachable URL: any network attempt would fail with a different error.
		CloneURL:  "file:///nonexistent/uzi-createref-invalid.git",
		Ref:       RecoveryRefPrefix + "run-1",
		Tip:       testTip,
		SourceRef: checkpointRefPrefix + "agent/issue-7",
	}
	cases := map[string]func(*CreateRefOptions){
		"ref outside recovery namespace": func(o *CreateRefOptions) { o.Ref = "refs/heads/main" },
		"ref is the checkpoint ns":       func(o *CreateRefOptions) { o.Ref = checkpointRefPrefix + "x" },
		"ref is the bare prefix":         func(o *CreateRefOptions) { o.Ref = RecoveryRefPrefix },
		"ref with dotdot":                func(o *CreateRefOptions) { o.Ref = RecoveryRefPrefix + "a..b" },
		"ref with space":                 func(o *CreateRefOptions) { o.Ref = RecoveryRefPrefix + "a b" },
		"ref with lock suffix":           func(o *CreateRefOptions) { o.Ref = RecoveryRefPrefix + "a.lock" },
		"ref with double slash":          func(o *CreateRefOptions) { o.Ref = RecoveryRefPrefix + "a//b" },
		"ref with colon":                 func(o *CreateRefOptions) { o.Ref = RecoveryRefPrefix + "a:b" },
		"source outside checkpoints":     func(o *CreateRefOptions) { o.SourceRef = "refs/heads/agent/issue-7" },
		"source empty":                   func(o *CreateRefOptions) { o.SourceRef = "" },
		"tip empty":                      func(o *CreateRefOptions) { o.Tip = "" },
		"tip zero":                       func(o *CreateRefOptions) { o.Tip = strings.Repeat("0", 40) },
		"tip short":                      func(o *CreateRefOptions) { o.Tip = testTip[:39] },
		"tip uppercase":                  func(o *CreateRefOptions) { o.Tip = strings.ToUpper(testTip) },
		"tip not hex":                    func(o *CreateRefOptions) { o.Tip = "z" + testTip[1:] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := good
			mutate(&o)
			if err := CreateRef(context.Background(), o); !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("CreateRef(%+v) = %v, want ErrInvalidRef", o, err)
			}
		})
	}
	if err := validateCreateRef(good); err != nil {
		t.Fatalf("control: valid options rejected: %v", err)
	}
}

func TestDeleteRejectsRefOutsideUziNamespaces(t *testing.T) {
	for _, ref := range []string{
		"refs/heads/main",
		"refs/tags/v1",
		RecoveryRefPrefix,
		checkpointRefPrefix,
		RecoveryRefPrefix + "a..b",
		"refs/uzi-recoveryx/a",
	} {
		err := Delete(context.Background(), DeleteOptions{
			CloneURL: "file:///nonexistent/uzi-delete-invalid.git", Ref: ref, ExpectedOldTip: testTip,
		})
		if !errors.Is(err, ErrInvalidRef) {
			t.Errorf("Delete(Ref=%q) = %v, want ErrInvalidRef", ref, err)
		}
	}
}

// TestCreateOutcomeDecidesFromReadBack pins CreateRef's classification: the sentinel is
// decided from origin's read-back advertisement, never from a receive-pack error's wording
// (the http-backend wire tests run the same paths against a real receive-pack).
func TestCreateOutcomeDecidesFromReadBack(t *testing.T) {
	tip := plumbing.NewHash(testTip)
	other := plumbing.NewHash("89abcdef0123456789abcdef0123456789abcdef")
	refused := errors.New("pushbroker: create ref: ng refs/uzi-recovery/x failed to update ref")
	listDown := errors.New("pushbroker: list: connection refused")
	cases := []struct {
		name      string
		pushErr   error
		current   plumbing.Hash
		present   bool
		listErr   error
		want      error // a sentinel, or nil for success
		wantOther bool  // a non-sentinel error
	}{
		{name: "accepted and read back at tip", current: tip, present: true, want: nil},
		{name: "accepted but absent on read-back", wantOther: true},
		{name: "accepted but read back at another tip", current: other, present: true, want: ErrRefExists},
		{name: "accepted but read-back failed", listErr: listDown, wantOther: true},
		{name: "refused, read back at tip (racer or lost response)", pushErr: refused, current: tip, present: true, want: ErrRefExistsAtTip},
		{name: "refused, read back at another tip", pushErr: refused, current: other, present: true, want: ErrRefExists},
		{name: "refused, absent on read-back", pushErr: refused, wantOther: true},
		{name: "refused, read-back failed", pushErr: refused, listErr: listDown, wantOther: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := createOutcome(c.pushErr, c.current, c.present, c.listErr, tip)
			if c.wantOther {
				if err == nil || errors.Is(err, ErrRefExists) || errors.Is(err, ErrRefExistsAtTip) || errors.Is(err, ErrSourceMissing) {
					t.Fatalf("createOutcome = %v, want a wrapped non-sentinel error", err)
				}
				return
			}
			if c.want == nil {
				if err != nil {
					t.Fatalf("createOutcome = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("createOutcome = %v, want %v", err, c.want)
			}
		})
	}
}

// TestDeleteRecoveryRefRequiresCAS: a recovery ref is only ever CAS-deleted, so an empty
// ExpectedOldTip is refused before any network I/O; a malformed ExpectedOldTip is refused
// for any ref.
func TestDeleteRecoveryRefRequiresCAS(t *testing.T) {
	url := "file:///nonexistent/uzi-delete-invalid.git"
	if err := Delete(context.Background(), DeleteOptions{CloneURL: url, Ref: RecoveryRefPrefix + "run-1"}); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("unconditional delete of a recovery ref = %v, want ErrInvalidRef", err)
	}
	for _, tip := range []string{
		testTip[:39], strings.ToUpper(testTip), "z" + testTip[1:], strings.Repeat("0", 40), testTip + "0",
	} {
		for _, o := range []DeleteOptions{
			{CloneURL: url, Branch: "agent/issue-7", ExpectedOldTip: tip},
			{CloneURL: url, Ref: RecoveryRefPrefix + "run-1", ExpectedOldTip: tip},
		} {
			if err := Delete(context.Background(), o); !errors.Is(err, ErrInvalidRef) {
				t.Errorf("Delete(%+v) = %v, want ErrInvalidRef", o, err)
			}
		}
	}
}

func TestListRefTipsRejectsForeignRefs(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", RecoveryRefPrefix, "refs/uzi-recoveryx/a"} {
		_, err := ListRefTips(context.Background(), ListRefsOptions{CloneURL: "file:///nonexistent/uzi-list-invalid.git"},
			checkpointRefPrefix+"agent/issue-7", ref)
		if !errors.Is(err, ErrInvalidRef) {
			t.Errorf("ListRefTips(%q) = %v, want ErrInvalidRef", ref, err)
		}
	}
}
