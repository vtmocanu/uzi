package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/uuid"
)

// Held-work publication (issue #2545). A failed run's commits after its last checkpoint (WIP
// included) are published as one pack to refs/uzi-held/<run-id>/<generation> by a create-only
// push. The work is split in two so the caller can commit a "may have invoked" marker between
// them:
//
//   - PrepareHeldPack is READ-ONLY: it budgets the worker's pack, fetches the bases, applies the
//     pack in memory and proves the declared tip is present. Nothing is sent.
//   - SendHeldCreate sends ONE receive-pack command, Old = zero hash (the remote's
//     compare-and-swap refuses an existing ref), then reads the ref back.
//
// No path here is forced, and a held ref is never moved: an existing ref is refused.

// maxHeldPrepareDuration bounds PrepareHeldPack: the pack budget scan, the base fetch and the
// in-memory apply. maxHeldSendDuration bounds SendHeldCreate: the push and the read-back. Each
// sits inside the step-A service budget its caller derives.
const (
	maxHeldPrepareDuration = 60 * time.Second
	maxHeldSendDuration    = 60 * time.Second
)

// MaxHeldPrepareDuration and MaxHeldSendDuration export the per-call ceilings for the caller's
// deadline arithmetic (the step-A service budget must cover both plus the database work).
const (
	MaxHeldPrepareDuration = maxHeldPrepareDuration
	MaxHeldSendDuration    = maxHeldSendDuration
)

// HeldPackOptions carries a held-work publication. Like Options, every field except the pack and
// tip is server-derived by the caller (workersvc): the worker names only the pack and the tip.
type HeldPackOptions struct {
	CloneURL string
	Username string
	PAT      string
	// RunID and Generation name the target ref, HeldRef(RunID, Generation).
	RunID      uuid.UUID
	Generation int64
	// Branch and DefaultBranch are fetched (shallow) as pack bases, with the branch's checkpoint
	// ref, exactly as Publish does: the pack was built against the last published checkpoint or
	// the merge base with the default branch.
	Branch        string
	DefaultBranch string
	// BaseRefs are further full refs to fetch as bases. Each must be a well-formed ref under
	// refs/heads/ or a managed namespace, else ErrInvalidRef.
	BaseRefs []string
	// Tip is the lowercase 40-hex commit the held ref will point at.
	Tip string
	// Pack is the raw (non-thin) packfile the worker shipped.
	Pack []byte
}

// PreparedHeldPack is a pack that passed PrepareHeldPack. It holds the credential and the pack
// until SendHeldCreate, never the in-memory object storer.
type PreparedHeldPack struct {
	ref    string
	tip    plumbing.Hash
	pack   []byte
	remote *git.Remote
	o      HeldPackOptions
}

// Ref is the held ref the prepared pack will be created at.
func (p *PreparedHeldPack) Ref() string { return p.ref }

// Tip is the commit the held ref will point at.
func (p *PreparedHeldPack) Tip() string { return p.tip.String() }

// HeldCreateOutcome is what SendHeldCreate can prove about a create.
type HeldCreateOutcome uint8

const (
	// HeldCreateUnknown is the zero value and the default: the create may or may not have landed.
	// It is returned for every result that is not proven, including a deadline that expires while
	// the forge goes on to apply the push. It is never terminal on an absent ref.
	HeldCreateUnknown HeldCreateOutcome = iota
	// HeldCreateCreated means the ref was read back at the tip.
	HeldCreateCreated
	// HeldCreateRefused means a received, complete report carried an explicit "ng" for our
	// command, and the read-back found no ref of ours. Nothing was created.
	HeldCreateRefused
)

func (o HeldCreateOutcome) String() string {
	switch o {
	case HeldCreateCreated:
		return "created"
	case HeldCreateRefused:
		return "refused"
	default:
		return "unknown"
	}
}

// ErrHeldCreateRefused wraps the remote's explicit refusal of a held create. The text after the
// sentinel is the remote's own, untrusted, and must be scrubbed and bounded by the caller.
var ErrHeldCreateRefused = errors.New("pushbroker: held create refused by the remote")

// PrepareHeldPack validates the worker's pack for a held create and proves, read-only, that it
// is applicable: the pack is within the inflation budget, the bases are fetched, the pack
// applies on them in memory and the declared tip is a commit present afterwards. It sends
// nothing and never creates the ref. Bad inputs return ErrInvalidRef (or ErrTipMissing /
// ErrPackInvalid / ErrPackTooLarge) before any network I/O where possible.
func PrepareHeldPack(ctx context.Context, o HeldPackOptions) (*PreparedHeldPack, error) {
	ctx, cancel := context.WithTimeout(ctx, maxHeldPrepareDuration)
	defer cancel()

	if o.RunID == uuid.Nil || o.Generation < 1 || !isObjectID(o.Tip) || plumbing.NewHash(o.Tip).IsZero() {
		return nil, ErrInvalidRef
	}
	ref := HeldRef(o.RunID, o.Generation)
	if !validRefUnder(ref, HeldRefPrefix) {
		return nil, ErrInvalidRef
	}
	for _, b := range o.BaseRefs {
		if !validRefUnder(b, "refs/heads/") && !validManagedRef(b) {
			return nil, ErrInvalidRef
		}
	}
	if len(o.Pack) == 0 {
		return nil, ErrPackInvalid
	}
	if err := scanPackBudget(ctx, o.Pack); err != nil {
		return nil, err
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return nil, fmt.Errorf("pushbroker: init: %w", err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{o.CloneURL}})
	if err != nil {
		return nil, fmt.Errorf("pushbroker: create remote: %w", err)
	}
	auth := authFor(Options{Username: o.Username, PAT: o.PAT})

	want := map[string]string{}
	addHead := func(b string) {
		if b != "" {
			want["refs/heads/"+b] = "refs/remotes/origin/" + b
		}
	}
	addHead(o.Branch)
	addHead(o.DefaultBranch)
	if o.Branch != "" {
		want[checkpointRefPrefix+o.Branch] = checkpointRefPrefix + o.Branch
	}
	for _, b := range o.BaseRefs {
		if strings.HasPrefix(b, "refs/heads/") {
			want[b] = "refs/remotes/origin/" + strings.TrimPrefix(b, "refs/heads/")
		} else {
			want[b] = b
		}
	}
	if err := fetchWantedRefs(ctx, remote, auth, want); err != nil {
		return nil, err
	}
	if err := packfile.UpdateObjectStorage(repo.Storer, bytes.NewReader(o.Pack)); err != nil {
		return nil, fmt.Errorf("pushbroker: apply pack: %w", err)
	}
	tip := plumbing.NewHash(o.Tip)
	if _, err := object.GetCommit(repo.Storer, tip); err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return nil, ErrTipMissing
		}
		return nil, fmt.Errorf("pushbroker: read declared tip: %w", err)
	}
	// The send needs only the URL, so it gets a fresh remote rather than one that pins the
	// storer holding every fetched base object.
	sendRemote, err := newOriginRemote(o.CloneURL)
	if err != nil {
		return nil, err
	}
	return &PreparedHeldPack{ref: ref, tip: tip, pack: o.Pack, remote: sendRemote, o: o}, nil
}

// SendHeldCreate sends the single create command for a prepared pack: Old = zero hash, New = the
// tip, the pack attached, through the raw-report path so a definitive refusal can be told from
// transport loss. It then lists origin and decides ONLY from what it can prove:
//
//   - the ref read back at the tip: HeldCreateCreated, whatever the push reported (this caller is
//     the only sender of the create, so a ref at our exact tip is ours);
//   - a complete report with an explicit ng for our command and no ref of ours on read-back:
//     HeldCreateRefused, with the remote's reason wrapped in ErrHeldCreateRefused;
//   - everything else, including a push that outlives ctx, a lost response, a report without a
//     verdict, a failed read-back and a success report whose ref cannot be seen:
//     HeldCreateUnknown with the cause.
//
// The push and the read-back share one deadline, so a create the forge applies after it expires
// is Unknown here and is found later by listing. The error is nil only for HeldCreateCreated.
func SendHeldCreate(ctx context.Context, p *PreparedHeldPack) (HeldCreateOutcome, error) {
	if p == nil || p.remote == nil {
		return HeldCreateUnknown, ErrInvalidRef
	}
	ctx, cancel := context.WithTimeout(ctx, maxHeldSendDuration)
	defer cancel()

	auth := authFor(Options{Username: p.o.Username, PAT: p.o.PAT})
	refName := plumbing.ReferenceName(p.ref)
	res, pushErr := forwardPack(ctx, p.remote, auth, refName, plumbing.ZeroHash, p.tip, p.pack)
	current, present, listErr := listRefTip(ctx, p.remote, auth, refName)

	switch {
	case listErr == nil && present && current == p.tip:
		return HeldCreateCreated, nil
	case listErr != nil:
		return HeldCreateUnknown, joinHeldCause(pushErr, fmt.Errorf("read-back failed: %w", listErr))
	case res.ng:
		return HeldCreateRefused, fmt.Errorf("%w: %s", ErrHeldCreateRefused, res.reason)
	case pushErr != nil:
		return HeldCreateUnknown, joinHeldCause(pushErr, nil)
	case present:
		return HeldCreateUnknown, errors.New("pushbroker: held create reported success but the ref is at another tip")
	default:
		return HeldCreateUnknown, errors.New("pushbroker: held create reported success but origin does not advertise the ref")
	}
}

func joinHeldCause(pushErr, other error) error {
	if pushErr == nil && other == nil {
		return errors.New("pushbroker: held create outcome unknown")
	}
	return errors.Join(pushErr, other)
}
