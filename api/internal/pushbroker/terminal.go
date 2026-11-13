package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/google/uuid"
)

// Terminal preservation (issue #4007). A run's final work is published as one pack to
// refs/uzi-terminal/<run-id>, created once and then only fast-forwarded by compare-and-swap, and
// removed only compare-and-swap. Preparation is READ-ONLY and, beyond what the held path proves,
// verifies the worker's declared coverage manifest roots and walks the tip's whole object graph;
// the send functions then write exactly one receive-pack command and read the ref back.
//
// The ref is derived here from the run id and is never accepted from a caller. validManagedRef
// deliberately does NOT include this namespace: the generic Delete and ListRefTips paths refuse it,
// so a terminal ref can only be touched through the typed functions in this file.

// TerminalRefPrefix is the uzi-owned namespace a run's terminal preservation pointer lives in.
const TerminalRefPrefix = "refs/uzi-terminal/"

// TerminalRef names the terminal ref for runID: refs/uzi-terminal/<lowercase run uuid>.
func TerminalRef(runID uuid.UUID) string {
	return TerminalRefPrefix + runID.String()
}

// maxTerminalWalkObjects bounds the connectivity walk: the number of distinct commits, trees and
// blobs it may visit before giving up with ErrTerminalWalkTooLarge. The walk only touches objects
// already in the in-memory storer (the budgeted pack plus the shallow-fetched bases).
const maxTerminalWalkObjects = 500_000

// maxTerminalPrepareDuration bounds PrepareTerminalPack and maxTerminalSendDuration bounds each
// send, the read-back and the delete (including its confirming listing).
const (
	maxTerminalPrepareDuration = 60 * time.Second
	maxTerminalSendDuration    = 60 * time.Second
)

// MaxTerminalPrepareDuration and MaxTerminalSendDuration export the per-call ceilings for the
// caller's deadline arithmetic.
const (
	MaxTerminalPrepareDuration = maxTerminalPrepareDuration
	MaxTerminalSendDuration    = maxTerminalSendDuration
)

var (
	// ErrTerminalRootUnreachable means a declared manifest root commit is absent from the verified
	// objects or is not reachable from the tip.
	ErrTerminalRootUnreachable = errors.New("pushbroker: declared terminal root is not reachable from the tip")
	// ErrTerminalTreeMismatch means a declared root's tree is not the tree of its commit.
	ErrTerminalTreeMismatch = errors.New("pushbroker: declared terminal root tree does not match its commit")
	// ErrTerminalWalkTooLarge means the connectivity walk exceeded maxTerminalWalkObjects.
	ErrTerminalWalkTooLarge = errors.New("pushbroker: terminal connectivity walk exceeds the object budget")
	// ErrTerminalRefused wraps the remote's explicit refusal of a terminal create or update. The
	// text after the sentinel is the remote's own, untrusted, and must be scrubbed and bounded.
	ErrTerminalRefused = errors.New("pushbroker: terminal write refused by the remote")
	// ErrTerminalPriorMoved means an update's terminal ref is absent or no longer at PriorTip when
	// fetched at prepare time; nothing was written.
	ErrTerminalPriorMoved = errors.New("pushbroker: terminal ref is not at the prior tip")
	// ErrTerminalRefMoved means a CAS delete found the terminal ref at a tip other than the
	// expected one: something newer owns it and nothing was removed.
	ErrTerminalRefMoved = errors.New("pushbroker: terminal ref is not at the expected tip")
)

// TerminalRoot is one declared coverage-manifest root: a commit and the tree it must have.
type TerminalRoot struct {
	Commit string
	Tree   string
}

// TerminalPackOptions carries a terminal publication. Like HeldPackOptions every field except the
// pack, tip, prior tip and roots is server-derived by the caller (workersvc).
type TerminalPackOptions struct {
	CloneURL string
	Username string
	PAT      string
	// RunID names the target ref, TerminalRef(RunID).
	RunID uuid.UUID
	// WorkBranch, CheckpointKey, DefaultBranch and BaseRefs select the server-fetched bases, as in
	// HeldPackOptions. BaseRefs must each be under refs/heads/ or a managed namespace.
	WorkBranch    string
	CheckpointKey string
	DefaultBranch string
	BaseRefs      []string
	// Tip is the lowercase 40-hex commit the terminal ref will point at.
	Tip string
	// PriorTip is the commit the terminal ref currently points at; empty means create. When set,
	// the ref is also fetched as a base and PriorTip must be a proper ancestor of Tip.
	PriorTip string
	// Pack is the raw (non-thin) packfile the worker shipped. It may be empty when Tip is already
	// present in the fetched bases (a tip on origin under another ref).
	Pack []byte
	// CIConfigGuard and CIConfigPaths apply the ci-config guard exactly as in HeldPackOptions.
	CIConfigGuard bool
	CIConfigPaths []string
	// Roots are the declared manifest roots, each of which must be a commit reachable from Tip
	// whose tree is the declared tree.
	Roots []TerminalRoot
}

// PreparedTerminalPack is a pack that passed PrepareTerminalPack. It holds the credential and the
// pack until the send, never the in-memory object storer.
type PreparedTerminalPack struct {
	ref   string
	tip   plumbing.Hash
	prior plumbing.Hash
	pack  []byte
	url   string
	o     TerminalPackOptions
}

// Ref is the terminal ref the prepared pack will be written to.
func (p *PreparedTerminalPack) Ref() string { return p.ref }

// Tip is the commit the terminal ref will point at.
func (p *PreparedTerminalPack) Tip() string { return p.tip.String() }

// PriorTip is the commit an update is compare-and-swapped from; empty for a create.
func (p *PreparedTerminalPack) PriorTip() string {
	if p.prior.IsZero() {
		return ""
	}
	return p.prior.String()
}

// TerminalOutcome is what SendTerminalCreate and SendTerminalUpdate can prove.
type TerminalOutcome uint8

const (
	// TerminalUnknown is the zero value and the default: the write may or may not have landed.
	// It is never terminal on an absent or unchanged ref.
	TerminalUnknown TerminalOutcome = iota
	// TerminalApplied means the ref was read back at the tip.
	TerminalApplied
	// TerminalRefused means a complete report carried an explicit "ng" for our command and the
	// read-back did not find the ref at our tip. Nothing was written.
	TerminalRefused
)

func (o TerminalOutcome) String() string {
	switch o {
	case TerminalApplied:
		return "applied"
	case TerminalRefused:
		return "refused"
	default:
		return "unknown"
	}
}

// PrepareTerminalPack validates the worker's pack for a terminal create or update and proves,
// read-only: the pack is within the inflation budget and applies on the fetched bases, the tip is
// present, the ci-config guard passes (when set), every object reachable from the tip is present
// (shallow-fetched base snapshots are the only permitted gaps in history), every declared root is a
// reachable commit with its declared tree and, for an update, PriorTip is a proper ancestor of the
// tip. It sends nothing. Errors: ErrInvalidRef (malformed inputs), ErrPackInvalid (including a
// missing object), ErrPackTooLarge, ErrTipMissing, ErrCIConfigGuard, ErrTerminalRootUnreachable,
// ErrTerminalTreeMismatch, ErrTerminalWalkTooLarge, ErrTerminalPriorMoved (update: the fetched
// terminal ref is absent or not at PriorTip) and ErrNotDescendant (prior tip).
//
// Producer rule: bases are fetched at depth 1, so only each fetched base tip (default branch, work
// branch, checkpoint ref, BaseRefs and, on update, the terminal ref) is present without its
// history. The declared tip must therefore descend one of those fetched base tips, or the pack must
// carry the history down to one; a commit whose parent is in neither is ErrPackInvalid. Declared
// roots must be reachable from the tip within the pack plus those depth-1 bases.
func PrepareTerminalPack(ctx context.Context, o TerminalPackOptions) (*PreparedTerminalPack, error) {
	ctx, cancel := context.WithTimeout(ctx, maxTerminalPrepareDuration)
	defer cancel()

	if o.RunID == uuid.Nil || !isObjectID(o.Tip) || plumbing.NewHash(o.Tip).IsZero() {
		return nil, ErrInvalidRef
	}
	ref := TerminalRef(o.RunID)
	if !validRefUnder(ref, TerminalRefPrefix) {
		return nil, ErrInvalidRef
	}
	var prior plumbing.Hash
	if o.PriorTip != "" {
		if !isObjectID(o.PriorTip) || plumbing.NewHash(o.PriorTip).IsZero() {
			return nil, ErrInvalidRef
		}
		prior = plumbing.NewHash(o.PriorTip)
	}
	for _, r := range o.Roots {
		if !isObjectID(r.Commit) || plumbing.NewHash(r.Commit).IsZero() ||
			!isObjectID(r.Tree) || plumbing.NewHash(r.Tree).IsZero() {
			return nil, ErrInvalidRef
		}
	}
	in := packPrep{
		CloneURL: o.CloneURL, Username: o.Username, PAT: o.PAT,
		WorkBranch: o.WorkBranch, CheckpointKey: o.CheckpointKey, DefaultBranch: o.DefaultBranch,
		BaseRefs: o.BaseRefs, Tip: o.Tip, Pack: o.Pack, AllowEmptyPack: true,
		CIConfigGuard: o.CIConfigGuard, CIConfigPaths: o.CIConfigPaths,
	}
	if !prior.IsZero() {
		in.ExtraBaseRefs = []string{ref}
	}
	repo, tipCommit, err := prepareFetchedPack(ctx, in)
	if err != nil {
		return nil, err
	}

	if !prior.IsZero() {
		// The terminal ref was fetched as a base. A ref that is gone or no longer at PriorTip is a
		// moved or deleted ref, not a bad pack: say so before the walk reports a missing object.
		cur, rerr := repo.Storer.Reference(plumbing.ReferenceName(ref))
		if rerr != nil || cur.Hash() != prior {
			return nil, fmt.Errorf("%w: %s", ErrTerminalPriorMoved, ref)
		}
	}

	shallow, err := repo.Storer.Shallow()
	if err != nil {
		return nil, fmt.Errorf("pushbroker: read shallow set: %w", err)
	}
	commits, err := walkTerminalConnectivity(ctx, repo.Storer, tipCommit, shallow)
	if err != nil {
		return nil, err
	}
	for _, r := range o.Roots {
		rc, ok := commits[plumbing.NewHash(r.Commit)]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrTerminalRootUnreachable, r.Commit)
		}
		if rc.TreeHash != plumbing.NewHash(r.Tree) {
			return nil, fmt.Errorf("%w: %s", ErrTerminalTreeMismatch, r.Commit)
		}
	}
	if !prior.IsZero() {
		if prior == tipCommit.Hash {
			return nil, fmt.Errorf("%w: prior tip equals the tip", ErrNotDescendant)
		}
		if _, ok := commits[prior]; !ok {
			return nil, ErrNotDescendant
		}
	}
	pack := o.Pack
	if len(pack) == 0 {
		// receive-pack reads a pack for every non-delete command; the objects are already on origin.
		pack = emptyPack()
	}
	return &PreparedTerminalPack{ref: ref, tip: tipCommit.Hash, prior: prior, pack: pack, url: o.CloneURL, o: o}, nil
}

// walkTerminalConnectivity visits every commit, tree and blob reachable from tip in the storer.
// A missing object is ErrPackInvalid, except a missing PARENT of a shallow-boundary commit (a
// fetched base snapshot, whose history is deliberately absent). It returns the visited commits.
func walkTerminalConnectivity(ctx context.Context, s storer.EncodedObjectStorer, tip *object.Commit, shallow []plumbing.Hash) (map[plumbing.Hash]*object.Commit, error) {
	boundary := make(map[plumbing.Hash]bool, len(shallow))
	for _, h := range shallow {
		boundary[h] = true
	}
	commits := map[plumbing.Hash]*object.Commit{tip.Hash: tip}
	seen := map[plumbing.Hash]struct{}{}
	visited := 0
	tick := func() error {
		visited++
		if visited > maxTerminalWalkObjects {
			return ErrTerminalWalkTooLarge
		}
		if visited%1024 == 0 {
			return ctx.Err()
		}
		return nil
	}
	missing := func(what string, h plumbing.Hash, err error) error {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return fmt.Errorf("%w: %s %s is not present", ErrPackInvalid, what, h)
		}
		return fmt.Errorf("%w: read %s %s: %v", ErrPackInvalid, what, h, err)
	}

	queue := []*object.Commit{tip}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if err := tick(); err != nil {
			return nil, err
		}
		trees := []plumbing.Hash{c.TreeHash}
		for len(trees) > 0 {
			th := trees[len(trees)-1]
			trees = trees[:len(trees)-1]
			if _, ok := seen[th]; ok {
				continue
			}
			seen[th] = struct{}{}
			if err := tick(); err != nil {
				return nil, err
			}
			t, err := object.GetTree(s, th)
			if err != nil {
				return nil, missing("tree", th, err)
			}
			for _, e := range t.Entries {
				switch e.Mode {
				case filemode.Submodule:
					continue
				case filemode.Dir:
					trees = append(trees, e.Hash)
					continue
				}
				if _, ok := seen[e.Hash]; ok {
					continue
				}
				seen[e.Hash] = struct{}{}
				if err := tick(); err != nil {
					return nil, err
				}
				if err := s.HasEncodedObject(e.Hash); err != nil {
					return nil, missing("blob", e.Hash, err)
				}
			}
		}
		for _, ph := range c.ParentHashes {
			if _, ok := commits[ph]; ok {
				continue
			}
			pc, err := object.GetCommit(s, ph)
			if err != nil {
				if errors.Is(err, plumbing.ErrObjectNotFound) && boundary[c.Hash] {
					continue
				}
				return nil, missing("commit", ph, err)
			}
			commits[ph] = pc
			queue = append(queue, pc)
		}
	}
	return commits, nil
}

// SendTerminalCreate sends the single create command for a prepared pack with no prior tip:
// Old = zero hash, so the remote's compare-and-swap refuses an existing ref. It then reads the ref
// back through the bounded listing and decides ONLY from what it can prove, exactly as
// SendHeldCreate does: the ref at the tip is TerminalApplied; a complete report with an explicit
// ng and the ref not at the tip is TerminalRefused (wrapping ErrTerminalRefused); everything else
// is TerminalUnknown with the cause. The error is nil only for TerminalApplied.
func SendTerminalCreate(ctx context.Context, p *PreparedTerminalPack) (TerminalOutcome, error) {
	if p == nil || p.url == "" || !p.prior.IsZero() {
		return TerminalUnknown, ErrInvalidRef
	}
	return sendTerminal(ctx, p, plumbing.ZeroHash)
}

// SendTerminalUpdate sends the single update command for a prepared pack with a prior tip:
// Old = PriorTip, New = the tip, never forced; the remote's compare-and-swap refuses the update
// when the ref moved. The outcome model is SendTerminalCreate's.
func SendTerminalUpdate(ctx context.Context, p *PreparedTerminalPack) (TerminalOutcome, error) {
	if p == nil || p.url == "" || p.prior.IsZero() {
		return TerminalUnknown, ErrInvalidRef
	}
	return sendTerminal(ctx, p, p.prior)
}

func sendTerminal(ctx context.Context, p *PreparedTerminalPack, old plumbing.Hash) (TerminalOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, maxTerminalSendDuration)
	defer cancel()

	remote, err := newOriginRemote(p.url)
	if err != nil {
		return TerminalUnknown, err
	}
	auth := authFor(Options{Username: p.o.Username, PAT: p.o.PAT})
	refName := plumbing.ReferenceName(p.ref)
	res, pushErr := forwardPack(ctx, remote, auth, refName, old, p.tip, p.pack)
	tips, listErr := boundedRefTips(ctx, p.url, auth, map[plumbing.ReferenceName]bool{refName: true})
	current, present := tips[p.ref]

	switch {
	case listErr == nil && present && current == p.tip.String():
		return TerminalApplied, nil
	case listErr != nil:
		return TerminalUnknown, joinTerminalCause(pushErr, fmt.Errorf("read-back failed: %w", listErr))
	case res.ng:
		return TerminalRefused, fmt.Errorf("%w: %s", ErrTerminalRefused, res.reason)
	case pushErr != nil:
		return TerminalUnknown, joinTerminalCause(pushErr, nil)
	case present:
		return TerminalUnknown, errors.New("pushbroker: terminal write reported success but the ref is at another tip")
	default:
		return TerminalUnknown, errors.New("pushbroker: terminal write reported success but origin does not advertise the ref")
	}
}

func joinTerminalCause(pushErr, other error) error {
	if pushErr == nil && other == nil {
		return errors.New("pushbroker: terminal write outcome unknown")
	}
	return errors.Join(pushErr, other)
}

// ListTerminalTip lists origin once (bounded by MaxRefAdvertisementBytes) and returns the tip of
// TerminalRef(runID); present is false when the ref is absent or the origin is empty.
func ListTerminalTip(ctx context.Context, o ListRefsOptions, runID uuid.UUID) (tip string, present bool, err error) {
	if runID == uuid.Nil {
		return "", false, ErrInvalidRef
	}
	ctx, cancel := context.WithTimeout(ctx, maxTerminalSendDuration)
	defer cancel()
	ref := TerminalRef(runID)
	tips, err := boundedRefTips(ctx, o.CloneURL, authFor(Options{Username: o.Username, PAT: o.PAT}),
		map[plumbing.ReferenceName]bool{plumbing.ReferenceName(ref): true})
	if err != nil {
		return "", false, err
	}
	tip, present = tips[ref]
	return tip, present, nil
}

// DeleteTerminalOptions carries a terminal-ref delete. ExpectedTip is required.
type DeleteTerminalOptions struct {
	CloneURL string
	Username string
	PAT      string
	RunID    uuid.UUID
	// ExpectedTip is the lowercase 40-hex non-zero tip the ref must still hold.
	ExpectedTip string
}

// TerminalDeleteOutcome is what DeleteTerminal can prove from its confirming listing.
type TerminalDeleteOutcome uint8

const (
	// TerminalDeleteUnknown is the zero value: the listing failed or the ref is still at the
	// expected tip after the delete, so nothing is proven.
	TerminalDeleteUnknown TerminalDeleteOutcome = iota
	// TerminalDeleteGone means the confirming listing found the ref absent.
	TerminalDeleteGone
	// TerminalDeleteMoved means the ref is present at another tip: it is not ours to remove.
	TerminalDeleteMoved
)

func (o TerminalDeleteOutcome) String() string {
	switch o {
	case TerminalDeleteGone:
		return "gone"
	case TerminalDeleteMoved:
		return "moved"
	default:
		return "unknown"
	}
}

// DeleteTerminal removes TerminalRef(RunID) compare-and-swap at ExpectedTip (an empty or zero
// expected tip is ErrInvalidRef before any I/O), then lists to confirm. The error is nil only for
// TerminalDeleteGone; a ref at another tip is TerminalDeleteMoved with ErrTerminalRefMoved.
func DeleteTerminal(ctx context.Context, o DeleteTerminalOptions) (TerminalDeleteOutcome, error) {
	if o.RunID == uuid.Nil || !isObjectID(o.ExpectedTip) || plumbing.NewHash(o.ExpectedTip).IsZero() {
		return TerminalDeleteUnknown, ErrInvalidRef
	}
	ctx, cancel := context.WithTimeout(ctx, maxTerminalSendDuration)
	defer cancel()

	ref := TerminalRef(o.RunID)
	refName := plumbing.ReferenceName(ref)
	remote, err := newOriginRemote(o.CloneURL)
	if err != nil {
		return TerminalDeleteUnknown, err
	}
	auth := authFor(Options{Username: o.Username, PAT: o.PAT})
	delErr := casDelete(ctx, remote, auth, refName, plumbing.NewHash(o.ExpectedTip))
	tips, listErr := boundedRefTips(ctx, o.CloneURL, auth, map[plumbing.ReferenceName]bool{refName: true})
	if listErr != nil {
		return TerminalDeleteUnknown, errors.Join(delErr, fmt.Errorf("confirming list failed: %w", listErr))
	}
	current, present := tips[ref]
	switch {
	case !present:
		return TerminalDeleteGone, nil
	case current != o.ExpectedTip:
		return TerminalDeleteMoved, ErrTerminalRefMoved
	default:
		if delErr == nil {
			delErr = errors.New("pushbroker: terminal delete reported success but the ref is still present")
		}
		return TerminalDeleteUnknown, delErr
	}
}
