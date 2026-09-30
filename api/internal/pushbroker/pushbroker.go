// Package pushbroker is the api's pure-Go git client for PRD #122 M8's brokered
// checkpoint publish. It is one of TWO places go-git is used (the other is
// internal/agentsource's clone-and-read of the agent-source repo, PRD #602 M3),
// deliberately isolated from the forge REST drivers (internal/forge): the api image is
// distroless-static and carries NO git binary, so every network git op here runs
// through go-git's smart-HTTP transport.
//
// The security contract lives one layer up in workersvc.Publish (which derives
// every field of Options from the run row, never from the worker). This package's
// single job is the mechanical one, and its single security-relevant invariant is
// NEVER FORCED: the push to refs/uzi-checkpoints/<branch> forwards the worker's pack
// through a MANUAL git-receive-pack session (not remote.PushContext — see forwardPack
// / issue #1009) whose single command carries the checkpoint tip AS FETCHED as its
// Old. That gives the invariant two agreeing guards: locally, the declared tip is
// verified to strictly descend origin's current tip before the wire; on the wire, the
// remote's compare-and-swap on that fetched Old refuses any update that would move the
// ref off it. A non-fast-forward / CAS-mismatch rejection is mapped to ErrNotDescendant
// — a legitimate "origin moved" outcome the caller turns into a 200 skip, never a 5xx.
//
// The other two ref writes keep the same invariant. Delete removes a checkpoint,
// recovery or salvage ref (a CAS on the expected tip when one is known; always a CAS for
// a recovery or salvage ref), and CreateRef (PRD #1810 D2, widened by PRD #1867) creates a
// refs/uzi-recovery/* or refs/uzi-salvage/* ref with a single command whose Old is the
// zero hash, so the remote's compare-and-swap refuses it whenever the ref already exists.
// No path in this package sends a forced update.
package pushbroker

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is the git packfile trailer format, not a security primitive.
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/packbudget"
	"github.com/vtmocanu/uzi/api/internal/redirectguard"
)

// Options carries everything a publish needs. Every field is derived by the caller
// from the run row (workersvc.Publish) — the worker names only the run id, the pack
// and the declared tip. CloneURL/BaseURL/Branch/DefaultBranch/Username/PAT are
// server-derived.
type Options struct {
	CloneURL string
	BaseURL  string
	Branch   string
	// DefaultBranch is the repo's default branch (from the run claim context). Its
	// objects are the exclude-boundary base a first-checkpoint delta pack was built
	// against, so we fetch them into the storer alongside the branch's own refs — but
	// we fetch ONLY these named refs, never every head (Rule: bounded fetch).
	DefaultBranch string
	Username      string
	PAT           string
	DeclaredTip   string
	// Pack is the raw (non-thin) delta packfile bytes the worker shipped. It is held
	// as []byte, not an io.Reader, because the publish reads it TWICE: once for the
	// budget pre-pass, once to apply it — each from a fresh reader.
	Pack []byte
}

// Result reports the ref that was (or would have been) advanced.
type Result struct {
	Ref string
	// AlreadyCurrent: origin's checkpoint ref already pointed at the declared tip, so nothing
	// was written. The success proves only that origin holds the tip, not who wrote it; the
	// caller decides whether the tip is the run's own (PRD #1810).
	AlreadyCurrent bool
}

var (
	// ErrNotDescendant means the declared tip does not strictly descend origin's
	// current checkpoint (or human-pushed branch) tip. The caller maps it to a 200
	// skip: the checkpoint ref only ever advances, so a non-descendant is a normal
	// "origin moved" outcome, not an error.
	ErrNotDescendant = errors.New("pushbroker: declared tip does not descend origin tip")
	// ErrTipMissing means the declared tip object is not present after applying the
	// pack — the worker's pack did not actually contain the commit it named.
	ErrTipMissing = errors.New("pushbroker: declared tip not present after applying pack")
	// ErrPackTooLarge means the worker's pack exceeds the budget along ANY of the axes
	// scanPackBudget bounds: too many objects, an over-cap per-object inflated/declared
	// size, too many cumulative RECONSTRUCTED bytes (the heap the apply allocates into
	// the storer), or too much cumulative INFLATION WORK (the bytes the scanner
	// zlib-inflates, summed over EVERY object — delta and non-delta alike). It is
	// enforced by a header + delta-header pre-pass, before any object is resolved into
	// the unbounded in-memory storer, so a decompression or delta bomb is refused
	// rather than OOMing the shared api (the reconstructed-size axis) OR pinning a core
	// inflating instruction streams whose declared targets are tiny (the inflation-work
	// axis — the variant a reconstructed-size-only budget waved through). The caller
	// maps it to a best-effort "unsupported" skip.
	ErrPackTooLarge = errors.New("pushbroker: pack exceeds inflation budget")
	// ErrPackInvalid means the pack header or an object header could not be parsed —
	// a genuinely malformed pack, distinct from an oversize one. The caller maps it to
	// a best-effort "unsupported" skip (200), NOT a 5xx: worker input must not
	// 500-storm the shared api.
	ErrPackInvalid = errors.New("pushbroker: pack is malformed")
	// ErrWorkflowScopeRejected means GitHub refused the checkpoint push because the
	// pushed tip's `.github/workflows/` tree differs from the current default branch and
	// the bot's `repo`-only PAT lacks the `workflow` scope (a deliberate supply-chain
	// guardrail — a worker that could rewrite CI is a risk). This is NOT an infra fault:
	// the branch is merely behind on workflow files, so the caller skips the checkpoint
	// cleanly (checkpoints are best-effort — PRD #456 M4) and never fails the run. The
	// finalize base-align (PRD #456 M1) is the real safety net for such a run's work.
	ErrWorkflowScopeRejected = errors.New("pushbroker: checkpoint push rejected for missing workflow scope")
	// ErrInvalidRef means a ref name or tip handed to CreateRef, Delete or ListRefTips is
	// outside the namespaces this package may touch (refs/uzi-checkpoints/*,
	// refs/uzi-recovery/*, refs/uzi-salvage/*; CreateRef narrows that per target, see
	// validateCreateRef), is not a well-formed git ref name, or (CreateRef) the tip is not
	// a 40-hex non-zero object id. It is a caller bug, refused before any network I/O.
	ErrInvalidRef = errors.New("pushbroker: invalid ref")
	// ErrRefExists means CreateRef found the target ref present on origin at a tip
	// other than the one requested: in the list before the create, or in the read-back
	// list after a refused or failed create. It is decided ONLY from an advertisement
	// that shows the ref at another tip, never from a receive-pack error's wording, so a
	// refusal for any other reason (a missing object, a pack error) is a wrapped error
	// and never this sentinel. The ref is left untouched.
	ErrRefExists = errors.New("pushbroker: ref already exists")
	// ErrRefExistsAtTip means CreateRef found the target ref already present on origin
	// at exactly the requested tip: a retry after an earlier create landed. Callers
	// treat it as success.
	ErrRefExistsAtTip = errors.New("pushbroker: ref already exists at the requested tip")
	// ErrSourceMissing means CreateRef's source ref is not advertised by origin at
	// exactly the requested tip (or origin is empty), so the tip object's presence on
	// origin is not proven and nothing was sent.
	ErrSourceMissing = errors.New("pushbroker: source ref not at the requested tip")
)

// Pack inflation budget (PRD #122 M8 hardening). The handler caps the COMPRESSED
// wire body at 64 MiB, but a small compressed pack can declare a vastly larger
// inflated size (a highly-compressible blob was measured inflating 5297x), and a
// tiny delta can declare a reconstructed target of many GiB. scanPackBudget bounds
// each object by the size it will RECONSTRUCT to in the storer (the delta target
// size for deltas), so the ceiling on heap the subsequent apply allocates is
// maxPackTotalBytes, not the wire size.
//
// The reconstructed-size caps defend the STORER (heap/OOM) path. They do NOT bound
// the scanner's own CPU: to READ each object the pre-pass zlib-inflates its declared
// Length (≤ maxPackObjectBytes per object — for a delta that is the INSTRUCTION
// stream, not the reconstructed target). A REF_DELTA can declare targetSz=0 behind a
// ~32 MiB instruction stream, so it contributes 0 to maxPackTotalBytes yet still
// forces ~32 MiB of inflation; many such deltas up to the 64 MiB wire cap forced
// ~900 MiB of uncancellable single-core inflation from an 897 KiB pack (measured,
// and ACCEPTED, before this cap). maxPackInflationWorkBytes bounds the CUMULATIVE
// bytes the scanner inflates across EVERY object, so total inflation CPU is bounded
// regardless of declared target sizes; it coexists with — does not replace — the
// reconstructed-size caps.
const (
	maxPackObjectBytes = 32 << 20 // 32 MiB per declared object

	maxPackTotalBytes = 128 << 20 // 128 MiB cumulative declared RECONSTRUCTED size (storer/OOM defense)

	// maxPackInflationWorkBytes caps the cumulative bytes scanPackBudget zlib-inflates
	// across all objects (the sum of every object's declared Length, delta and
	// non-delta), bounding total inflation CPU independent of declared target size (the
	// tiny-target / huge-instruction-stream delta bomb). Generous vs. any legitimate
	// checkpoint delta (a handful of commits) while capping worst-case work well under
	// the ~GiBs a 64 MiB wire body of packed deltas could otherwise force.
	maxPackInflationWorkBytes = 256 << 20 // 256 MiB cumulative inflated (CPU defense)

	maxPackObjects = 50000 // object-count ceiling
)

// maxDeleteDuration is a hard wall-clock ceiling on ONE best-effort checkpoint-ref
// delete (PRD #1030 M4). The delete is a single list + a tiny (packless) reference
// update, so it is far cheaper than a Publish; the ceiling exists only so a slow or
// down forge can never delay or wedge the caller (a worker's terminal-state report),
// which swallows the outcome regardless. Deliberately shorter than maxPublishDuration.
const maxDeleteDuration = 30 * time.Second

// maxCreateRefDuration is the wall-clock ceiling on ONE CreateRef: a list plus a
// receive-pack session carrying a single command and a 32-byte empty pack.
const maxCreateRefDuration = 30 * time.Second

// maxPublishDuration is a hard wall-clock ceiling on ONE brokered publish — the
// untrusted-pack budget scan (single-core zlib inflation), the origin fetch, and the
// non-forced push combined. /publish carries no request timeout of its own, and a
// worst-case pack can force tens of seconds of otherwise-uncancellable CPU; this
// makes any one publish cancellable and time-bounded regardless of pack contents or
// a slow forge. Generous for a legitimate checkpoint push, tight enough that a single
// request can never run unbounded.
const maxPublishDuration = 60 * time.Second

// MaxPublishDuration exports maxPublishDuration for the retention timing relations in workersvc
// (PRD #1810 D2: the supersession cooling period is sized above a live publish's pre-push budget
// plus this client-side ceiling).
const MaxPublishDuration = maxPublishDuration

// MaxDeleteDuration and MaxCreateRefDuration export the per-call ceilings of Delete, ListRefTips
// (which shares maxDeleteDuration) and CreateRef for the retention timing relations in workersvc
// (PRD #1810: one hung forge call ends at its own ceiling, inside the sweeper's per-record
// timeout, so the record's failure is recorded before the operation's deadline).
const (
	MaxDeleteDuration    = maxDeleteDuration
	MaxCreateRefDuration = maxCreateRefDuration
)

// checkpointRefPrefix is the uzi-owned ref namespace no CI watches (Rule 3). The
// end-of-run push targets refs/heads/<branch>; checkpoints never do.
const checkpointRefPrefix = "refs/uzi-checkpoints/"

// RecoveryRefPrefix is the uzi-owned namespace a superseded run's checkpoint tip is
// preserved under (PRD #1810 D2): refs/uzi-recovery/<run-id>. Like the checkpoint
// namespace it is outside refs/heads, so no CI watches it.
const RecoveryRefPrefix = "refs/uzi-recovery/"

// SalvageRefPrefix is the run-scoped namespace a FAILED run's last published checkpoint
// tip is copied into for bounded archival (PRD #1867): refs/uzi-salvage/<run-id>. Keyed
// by run id, so it never blocks a later run on the same branch, and outside refs/heads,
// so no CI watches it. Its refs are only ever created (CreateRef) and CAS-deleted.
const SalvageRefPrefix = "refs/uzi-salvage/"

// SalvageRef names the salvage ref for runID: refs/uzi-salvage/<run-id>.
func SalvageRef(runID uuid.UUID) string {
	return SalvageRefPrefix + runID.String()
}

// validManagedRef reports whether ref is a well-formed ref strictly under one of the
// namespaces Delete and ListRefTips accept: checkpoint, recovery or salvage.
func validManagedRef(ref string) bool {
	return validRefUnder(ref, checkpointRefPrefix) || validRefUnder(ref, RecoveryRefPrefix) ||
		validRefUnder(ref, SalvageRefPrefix)
}

// Publish fetches origin's base objects, applies the worker's delta pack, verifies
// the declared tip strictly descends origin's current tip, and pushes it —
// NON-FORCED — to refs/uzi-checkpoints/<branch>.
//
// NOTE: the go-git round-trip against a REAL forge is a manual/e2e validation step
// (see the package tests, which prove the algorithm against a local bare fixture).
func Publish(ctx context.Context, o Options) (Result, error) {
	// Bound the WHOLE publish — the untrusted-pack budget scan, the origin fetch, and
	// the push — by a hard wall-clock ceiling, so a single request can never run
	// unbounded on a hostile pack or a slow forge. The derived ctx threads into
	// scanPackBudget (which checks it per object), fetchBaseRefs, and PushContext.
	ctx, cancel := context.WithTimeout(ctx, maxPublishDuration)
	defer cancel()

	ref := checkpointRefPrefix + o.Branch
	result := Result{Ref: ref}

	tipHash := plumbing.NewHash(o.DeclaredTip)
	if tipHash.IsZero() {
		// A malformed tip should have been rejected at the handler, but never trust
		// it here: a zero hash can never be a valid checkpoint tip.
		return Result{}, ErrTipMissing
	}

	// A publish always declares a tip, so it is never a ref delete, and even a zero-object
	// pack carries a 12-byte header plus a 20-byte checksum. A zero-byte body is therefore
	// never a valid pack: it is a worker whose pack producer failed. Forwarding it would
	// send origin a create/update with no pack, which origin answers "eof before pack
	// header" and surfaces as a 500; refuse it as malformed (a best-effort skip) instead.
	if len(o.Pack) == 0 {
		return Result{}, ErrPackInvalid
	}

	// Step 1: BEFORE resolving anything into the unbounded in-memory storer, walk the
	// pack and enforce the budget — per-object and cumulative RECONSTRUCTED size
	// (deltas bounded by their declared target), AND cumulative INFLATION WORK (the
	// bytes actually zlib-inflated, which the reconstructed-size counter misses for a
	// tiny-target delta). A worker-supplied pack is untrusted; this is what keeps a
	// decompression or delta bomb from OOM-killing OR pinning a core of the api. The
	// scan honours ctx, so the wall-clock ceiling above bounds it too.
	if err := scanPackBudget(ctx, o.Pack); err != nil {
		return Result{}, err
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("pushbroker: init: %w", err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{o.CloneURL},
	})
	if err != nil {
		return Result{}, fmt.Errorf("pushbroker: create remote: %w", err)
	}

	auth := authFor(o)
	checkpointRef := plumbing.ReferenceName(ref)
	branchRef := plumbing.ReferenceName("refs/remotes/origin/" + o.Branch)

	// Step 2: fetch ONLY the base refs this publish needs into the storer — the
	// branch itself, the repo default branch (the delta pack's exclude-boundary base
	// for the never-pushed-branch case), and the branch's checkpoint ref. Only these
	// three refs, and each SHALLOW (depth 1, set in fetchBaseRefs) — so this pulls only
	// the tip SNAPSHOT of each, never its history. That bound is load-bearing, not
	// cosmetic: a full (deep) fetch unpacks the ref's entire history into the in-memory
	// storer and OOM-killed the api on the first real repo (see fetchBaseRefs).
	//
	// A specific refspec for a ref origin does not advertise fails the WHOLE fetch
	// ("couldn't find remote ref"), and the PRIMARY M8 case is a branch never pushed
	// to heads — so we first LIST the advertised refs and fetch only the subset that
	// exists. Missing refs, an empty remote, and up-to-date are all tolerated.
	if err := fetchBaseRefs(ctx, remote, auth, o.Branch, o.DefaultBranch); err != nil {
		return Result{}, err
	}

	branchTip := resolveHash(repo, branchRef)
	checkpointTip := resolveHash(repo, checkpointRef)

	// Already up to date: origin's checkpoint ref ALREADY points at exactly the declared
	// tip. This is a genuine SUCCESS — "the checkpoint ref already reflects your tip" — so
	// it returns (result, nil), which the caller maps to Published=true, advances
	// lastPublishedTip on, and stops re-attempting. This is the resume scenario PRD #1030
	// M1 targets: a run resumed on a cold worker with lastPublishedTip reset and no new
	// commits re-declares the tip origin already holds.
	//
	// The check MUST sit BEFORE the strict-descendant check (step 5): there base ==
	// checkpointTip == the declared tip, and strictDescends returns (false, nil) for
	// base == declared.Hash, which would otherwise misreport an already-current tip as
	// ErrNotDescendant (a "skipped: not_descendant" feed line every interval). No pack
	// apply or tip-presence check is needed first: origin already holds the tip AT the
	// checkpoint ref, so its presence is proven and there is nothing to advance. tipHash
	// is non-zero (guarded above), so this fires only for a real, matching checkpoint ref.
	if checkpointTip == tipHash {
		result.AlreadyCurrent = true
		return result, nil
	}

	// Step 3: apply the received pack into the SAME storer, from a FRESH reader (the
	// budget pre-pass consumed its own reader). The base objects from step 2 provide
	// the parent chain for the worker's (non-thin) delta pack.
	if len(o.Pack) > 0 {
		if err := packfile.UpdateObjectStorage(repo.Storer, bytes.NewReader(o.Pack)); err != nil {
			return Result{}, fmt.Errorf("pushbroker: apply pack: %w", err)
		}
	}

	// Step 4: the declared tip must now be a commit present in the storer.
	declaredCommit, err := object.GetCommit(repo.Storer, tipHash)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return Result{}, ErrTipMissing
		}
		return Result{}, fmt.Errorf("pushbroker: read declared tip: %w", err)
	}

	// Step 5: strict-descendant / never-backward check (the "never forced"
	// invariant, enforced locally BEFORE the wire and again by the non-forced push).
	base := checkpointTip
	if base.IsZero() {
		base = branchTip
	}
	if !base.IsZero() {
		ok, err := strictDescends(repo, base, declaredCommit)
		if err != nil {
			return Result{}, fmt.Errorf("pushbroker: ancestry (base): %w", err)
		}
		if !ok {
			return Result{}, ErrNotDescendant
		}
	}
	// If a human pushed refs/heads/<branch> ahead of (or aside from) the checkpoint,
	// the checkpoint must not move to a tip that does not descend that branch.
	if !branchTip.IsZero() && branchTip != checkpointTip {
		ok, err := descendsOrEqual(repo, branchTip, declaredCommit)
		if err != nil {
			return Result{}, fmt.Errorf("pushbroker: ancestry (branch): %w", err)
		}
		if !ok {
			return Result{}, ErrNotDescendant
		}
	}

	// Step 6: set the local checkpoint ref (parity with the storer view) and forward the
	// worker's pack to origin via a MANUAL receive-pack session.
	//
	// We deliberately do NOT use remote.PushContext here. PushContext recomputes its own
	// send-set with revlist.Objects, walking from the declared tip and EXCLUDING origin's
	// advertised default. Our storer holds only a depth-1 SNAPSHOT of that default
	// (D_new), so when origin's default advanced since the worker cloned, the
	// branch-point's parent (D_old) is absent from the storer and the walk raises
	// plumbing.ErrObjectNotFound ("push: object not found") LOCALLY, before any bytes
	// leave (issue #1009). The worker's pack is already non-thin (built with ^D_old) and
	// the REMOTE still holds D_old (it is reachable from D_new), so forwarding the pack
	// verbatim and letting the remote resolve reachability is correct.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(checkpointRef, tipHash)); err != nil {
		return Result{}, fmt.Errorf("pushbroker: set local ref: %w", err)
	}

	// The receive-pack command's Old binds to the checkpoint tip AS FETCHED by
	// fetchBaseRefs (checkpointTip) — the exact SHA the strict-descendant checks above
	// ran against, and plumbing.ZeroHash when the ref did not exist at fetch time (a
	// create). It is DELIBERATELY not read from the receive-pack advertisement: binding
	// Old to the fetched value keeps the local fast-forward check and the server-side
	// compare-and-swap in agreement, so a refs/uzi-checkpoints/* ref — which is NOT under
	// refs/heads and so is NOT protected by the remote's denyNonFastForwards — can never
	// be force-moved by a mismatch between the two advertisements.
	// The already-up-to-date case (checkpointTip == tipHash) returned success far above,
	// before the pack apply and ancestry checks, so by here the declared tip strictly
	// descends the fetched checkpoint tip and there is a real update to forward.
	pushErr := forwardPack(ctx, remote, auth, checkpointRef, checkpointTip, tipHash, o.Pack)
	switch {
	case pushErr == nil:
		return result, nil
	case isWorkflowScopeRejection(pushErr):
		// The branch is behind on .github/workflows/** relative to the default branch
		// and the bot PAT lacks the `workflow` scope. Not an infra fault — the caller
		// skips the checkpoint cleanly (best-effort; PRD #456 M4). Checked BEFORE the
		// non-fast-forward arm so a rejection carrying both signals routes to the scope
		// sentinel, and so it never surfaces as a 5xx.
		return Result{}, ErrWorkflowScopeRejected
	case isNonFastForward(pushErr):
		// origin advanced its checkpoint (or a human moved the branch) between our fetch
		// and our push, so the receive-pack compare-and-swap on the fetched Old refused
		// the update — exactly the protocol-level guarantee the non-forced invariant wants.
		return Result{}, ErrNotDescendant
	default:
		return Result{}, fmt.Errorf("pushbroker: push: %w", pushErr)
	}
}

// DeleteOptions carries the connection inputs a checkpoint-ref delete needs. Like
// Options, every field is derived by the caller (workersvc) from the run row — the
// worker never names the repo, branch or credential. It is a focused subset of
// Options: a delete ships no pack and declares no tip, so it needs only enough to
// reach the remote and name the checkpoint ref.
type DeleteOptions struct {
	CloneURL string
	Branch   string
	Username string
	PAT      string
	// Ref, when set, is the FULL name of the ref to delete and replaces
	// refs/uzi-checkpoints/<Branch>. It must lie under refs/uzi-checkpoints/,
	// refs/uzi-recovery/ or refs/uzi-salvage/ (PRD #1867) and be a well-formed ref name,
	// else Delete returns ErrInvalidRef before any network I/O. Empty keeps the
	// branch-derived name. A recovery or salvage ref is only ever CAS-deleted: Ref under
	// either prefix with an empty ExpectedOldTip is ErrInvalidRef.
	Ref string
	// ExpectedOldTip is the tip this run last published to its checkpoint ref (the
	// persisted runs.checkpoint_tip). When set, Delete takes a compare-and-swap path:
	// it removes the ref ONLY if origin still points at exactly this tip, and treats a
	// CAS refusal (the ref advanced since our last publish, or is already absent) as
	// benign success — so a terminal cleanup can never clobber a SIBLING run's fresh
	// checkpoint on the same branch. When empty, Delete preserves its legacy
	// UNCONDITIONAL list-then-delete behaviour (backward-compatible; see Delete). When
	// set it must be a lowercase 40-hex non-zero object id, else ErrInvalidRef.
	ExpectedOldTip string
}

// Delete removes refs/uzi-checkpoints/<branch> (or o.Ref, a checkpoint, recovery or
// salvage ref named in full) from the remote, BEST-EFFORT (PRD #1030 M4). A run's checkpoint ref is uzi-owned scratch state; once the run reaches
// a terminal state it is stale, and a stale ref left behind later blocks a NEW run on
// the same branch with a not_descendant skip (the new run's tip does not descend the
// dead run's checkpoint). PRD #1810 routes terminal cleanup through checkpoint
// retention (workersvc), not a delete on every terminal transition: the branch ref is
// deleted once the run's last open custody hold settles, or immediately at the
// terminal transition when the run has no hold; a supersession (another run needing
// the slot) and the sweep reconciler also call this, each a CAS on the run's recorded
// tip. The salvage sweep (PRD #1867) CAS-deletes only its own refs/uzi-salvage/<run-id>
// at expiry. Every caller SWALLOWS or records the returned error — it must never block or
// fail the worker's terminal report — so this carries its OWN short wall-clock
// timeout rather than inheriting an unbounded ctx.
//
// The delete goes out as a go-git delete refspec (":<ref>" — empty source, the ref as
// destination), the pure-Go equivalent of `git push origin :refs/uzi-checkpoints/…`.
// It runs through the SAME endpoint/auth/remote setup as Publish (authFor, the origin
// remote config, the checkpointRefPrefix constant).
//
// When o.ExpectedOldTip is SET (the run's persisted checkpoint_tip), Delete takes a
// COMPARE-AND-SWAP path (casDelete): a manual receive-pack command whose Old binds to
// that tip and whose New is the zero hash. The remote's CAS on Old refuses the delete
// unless origin's ref still points at exactly the tip THIS run last published, so a
// terminal cleanup can never remove a SIBLING run's fresher checkpoint on the same
// branch (the delete-race PRD #1042 M3 closes). A CAS refusal — the ref advanced since
// our publish, or is already absent (server tip zero ≠ our Old) — is a BENIGN success:
// something else now owns the ref and we owned only our own tip, so it returns nil and
// never retries. Only a genuine transport/session/auth fault returns a wrapped error.
//
// When o.ExpectedOldTip is EMPTY (a never-published run has nothing to CAS against, and
// the legacy call shape), Delete preserves its original UNCONDITIONAL behaviour:
// deleting a ref that is already absent is SUCCESS, not an error, and to make that
// deterministic (and idempotent across go-git/forge version drift in how a
// delete-of-missing is reported) it first LISTS the remote's advertised refs and
// returns nil when the checkpoint ref is not present, exactly mirroring
// fetchBaseRefs's list-first defensiveness. An empty remote is likewise nil. On any
// other transport/remote fault it returns a wrapped error — which the caller swallows,
// but the wrap is scrubbed of the credential-bearing URL by the caller's
// secretscrub.Scrub path (the same invariant Publish's default arm keeps), so a
// go-git error embedding the PAT in its remote URL never reaches a log in the clear.
func Delete(ctx context.Context, o DeleteOptions) error {
	// Bound the whole delete by a hard wall-clock ceiling so a slow/down forge can
	// never delay the caller's terminal report (which swallows this outcome anyway).
	ctx, cancel := context.WithTimeout(ctx, maxDeleteDuration)
	defer cancel()

	ref := checkpointRefPrefix + o.Branch
	if o.Ref != "" {
		if !validManagedRef(o.Ref) {
			return ErrInvalidRef
		}
		// A recovery ref preserves a superseded run's only off-worker copy, and a salvage
		// ref a failed run's archived tip: each is only ever removed compare-and-swap on
		// its recorded tip, never unconditionally.
		if o.ExpectedOldTip == "" && (strings.HasPrefix(o.Ref, RecoveryRefPrefix) || strings.HasPrefix(o.Ref, SalvageRefPrefix)) {
			return ErrInvalidRef
		}
		ref = o.Ref
	}
	if o.ExpectedOldTip != "" && (!isObjectID(o.ExpectedOldTip) || plumbing.NewHash(o.ExpectedOldTip).IsZero()) {
		return ErrInvalidRef
	}
	refName := plumbing.ReferenceName(ref)

	remote, err := newOriginRemote(o.CloneURL)
	if err != nil {
		return err
	}
	auth := authFor(Options{Username: o.Username, PAT: o.PAT})

	// CAS delete: only remove the ref if origin still holds the tip WE published.
	if o.ExpectedOldTip != "" {
		return casDelete(ctx, remote, auth, refName, plumbing.NewHash(o.ExpectedOldTip))
	}

	// Legacy unconditional delete (empty ExpectedOldTip). List first: a delete of a ref
	// origin does not advertise is a no-op success, and listing makes that deterministic
	// rather than depending on how a given go-git/forge pair reports "remote ref does not
	// exist" on the push.
	advertised, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil // empty remote: the checkpoint ref cannot exist
		}
		return fmt.Errorf("pushbroker: list: %w", err)
	}
	present := false
	for _, r := range advertised {
		if r.Name() == refName {
			present = true
			break
		}
	}
	if !present {
		return nil // ref already absent — deleting a non-existent ref is success
	}

	// A delete refspec has an EMPTY source and the ref as destination.
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(":" + ref)},
		Auth:       auth,
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return fmt.Errorf("pushbroker: delete ref: %w", err)
}

// casDelete removes refName from origin ONLY if origin still points it at expectedOld
// (the run's persisted checkpoint tip) — a compare-and-swap delete, in TWO agreeing
// halves exactly as Publish guards its push:
//
//   - LOCAL guard (primary): it first LISTS origin's advertised refs and compares the
//     checkpoint ref's current tip against expectedOld. If the ref is absent, or points
//     anywhere OTHER than the tip we published (a newer sibling run or a human advanced
//     it), we own nothing to delete → benign nil, the ref is left untouched. This is the
//     reliable half and the one the file:// test harness exercises; it mirrors Publish's
//     local strict-descendant check binding to the FETCHED tip.
//   - WIRE guard (second): when the local check passes, the delete goes out as a MANUAL
//     receive-pack command whose Old binds to expectedOld (New = zero, NO packfile — a
//     delete ships no objects). A real forge's receive-pack compare-and-swap on Old then
//     refuses the delete if the ref moved in the list→delete window; that refusal is
//     classified benign (isCASDeleteRefusal, reusing isNonFastForward) → nil, never a
//     retry. (The go-git internal server behind file:// does not enforce this wire CAS,
//     which is why the local guard above is the primary one — see Publish's note that the
//     pure wire race is not reproducible in the harness.)
//
// Only a genuine transport/session/auth fault returns a wrapped error, which the caller
// swallows best-effort (scrubbed of the credential URL).
func casDelete(ctx context.Context, remote *git.Remote, auth transport.AuthMethod, refName plumbing.ReferenceName, expectedOld plumbing.Hash) error {
	advertised, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil // empty remote: the checkpoint ref cannot exist — nothing to delete
		}
		return fmt.Errorf("pushbroker: list: %w", err)
	}
	var current plumbing.Hash
	for _, r := range advertised {
		if r.Name() == refName {
			current = r.Hash()
			break
		}
	}
	if current.IsZero() || current != expectedOld {
		// Ref already absent, or advanced past the tip WE published: a never-published
		// run owns nothing, and an advanced ref now belongs to a newer run. Benign — leave
		// it alone rather than clobber a sibling's fresh checkpoint.
		return nil
	}

	// Origin still holds exactly our tip: delete it, binding the wire Old to that tip.
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return fmt.Errorf("pushbroker: remote has no URL")
	}
	ep, err := transport.NewEndpoint(urls[0])
	if err != nil {
		return fmt.Errorf("pushbroker: endpoint: %w", err)
	}
	c, err := transportFor(ep)
	if err != nil {
		return fmt.Errorf("pushbroker: transport client: %w", err)
	}
	sess, err := c.NewReceivePackSession(ep, auth)
	if err != nil {
		return fmt.Errorf("pushbroker: receive-pack session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return fmt.Errorf("pushbroker: advertise: %w", err)
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{
		Name: refName,
		Old:  expectedOld,
		New:  plumbing.ZeroHash,
	}}
	// No packfile: a delete carries no objects (req.Packfile stays nil).

	// ReceivePack returns report-status AND report.Error() (nil on success, else the
	// first failing command's "ng <ref> <reason>" text). A wire CAS/old-mismatch refusal
	// (the ref moved in the list→delete window) is classified benign; only a real fault
	// is surfaced.
	_, err = sess.ReceivePack(ctx, req)
	switch {
	case err == nil:
		return nil
	case isCASDeleteRefusal(err):
		return nil // ref moved out from under our Old in the list→delete window — benign
	default:
		return fmt.Errorf("pushbroker: cas delete ref: %w", err)
	}
}

// CreateRefOptions carries a recovery-ref create (PRD #1810 D2) or a salvage-ref create
// (PRD #1867). Like DeleteOptions every field is server-derived by the caller: the forge
// connection, the ref to create (Ref), the tip it must point at, and the ref (SourceRef)
// origin must currently advertise at exactly that tip. A recovery Ref (under
// RecoveryRefPrefix) takes a SourceRef under refs/uzi-checkpoints/ only; a salvage Ref
// (under SalvageRefPrefix) takes a SourceRef under refs/uzi-checkpoints/ or
// refs/uzi-recovery/ (validateCreateRef).
type CreateRefOptions struct {
	CloneURL  string
	Username  string
	PAT       string
	Ref       string
	Tip       string
	SourceRef string
}

// CreateRef creates o.Ref on origin pointing at o.Tip, an object origin already holds
// (it is the tip of o.SourceRef), so no objects are shipped. It never moves an
// existing ref:
//
//   - It LISTS origin's refs first. o.Ref already at o.Tip returns ErrRefExistsAtTip
//     (a retry after an earlier create; callers treat it as success); at any other tip
//     it returns ErrRefExists. o.SourceRef not advertised at exactly o.Tip (or an empty
//     origin) returns ErrSourceMissing: without it the tip's presence on origin is
//     unproven.
//   - It then sends ONE receive-pack command, Old = zero hash, New = o.Tip, with a
//     zero-object pack. Old = zero is a compare-and-swap: the remote refuses the
//     create if the ref appeared after the list. The remote's own connectivity check
//     is the second guard on the tip's presence.
//   - Whatever the receive-pack outcome, it LISTS origin again and decides from what
//     origin now advertises (createOutcome): o.Ref at o.Tip is success (nil after an
//     accepted create, ErrRefExistsAtTip after a refused or failed one, e.g. a racer
//     created the same tip or the response was lost); at another tip ErrRefExists;
//     absent a wrapped error. An accepted create is therefore nil only when the
//     read-back proves it, and a receive-pack refusal is never mapped to
//     ErrRefExists from its message text alone.
//
// The pack is required, not decorative: git receive-pack reads a pack for every
// non-delete command, and a create with none is refused ("eof before pack header",
// as Publish records). Invalid inputs return ErrInvalidRef before any network I/O.
// Any other failure is a wrapped error the caller must scrub like Delete's.
func CreateRef(ctx context.Context, o CreateRefOptions) error {
	return createRefWithPack(ctx, o, emptyPack())
}

// createRefWithPack is CreateRef with the pack as a parameter, so a test can prove
// against a real git receive-pack that the empty pack is what makes the create land.
func createRefWithPack(ctx context.Context, o CreateRefOptions, pack []byte) error {
	ctx, cancel := context.WithTimeout(ctx, maxCreateRefDuration)
	defer cancel()

	if err := validateCreateRef(o); err != nil {
		return err
	}
	refName := plumbing.ReferenceName(o.Ref)
	sourceName := plumbing.ReferenceName(o.SourceRef)
	tip := plumbing.NewHash(o.Tip)

	remote, err := newOriginRemote(o.CloneURL)
	if err != nil {
		return err
	}
	auth := authFor(Options{Username: o.Username, PAT: o.PAT})

	advertised, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return ErrSourceMissing // an empty origin holds no source ref
		}
		return fmt.Errorf("pushbroker: list: %w", err)
	}
	var target, source plumbing.Hash
	var targetPresent bool
	for _, r := range advertised {
		switch r.Name() {
		case refName:
			target, targetPresent = r.Hash(), true
		case sourceName:
			source = r.Hash()
		}
	}
	if targetPresent {
		if target == tip {
			return ErrRefExistsAtTip
		}
		return ErrRefExists
	}
	if source != tip {
		return ErrSourceMissing
	}
	return pushCreateVerified(ctx, remote, auth, refName, tip, pack)
}

// pushCreateVerified sends the create command, then re-lists origin and classifies the
// outcome from the read-back (createOutcome).
func pushCreateVerified(ctx context.Context, remote *git.Remote, auth transport.AuthMethod, ref plumbing.ReferenceName, tip plumbing.Hash, pack []byte) error {
	pushErr := pushCreate(ctx, remote, auth, ref, tip, pack)
	current, present, listErr := listRefTip(ctx, remote, auth, ref)
	return createOutcome(pushErr, current, present, listErr, tip)
}

// pushCreate sends the create command through a manual receive-pack session and returns
// the raw outcome, wrapped: nil when the remote reported success. It does not classify;
// createOutcome does, from the read-back.
func pushCreate(ctx context.Context, remote *git.Remote, auth transport.AuthMethod, ref plumbing.ReferenceName, tip plumbing.Hash, pack []byte) error {
	sess, ar, err := openReceivePack(ctx, remote, auth)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	if _, err := sess.ReceivePack(ctx, createRefRequest(ar.Capabilities, ref, tip, pack)); err != nil {
		return fmt.Errorf("pushbroker: create ref: %w", err)
	}
	return nil
}

// listRefTip lists origin and returns where it advertises ref (present false when it does
// not, including an empty origin).
func listRefTip(ctx context.Context, remote *git.Remote, auth transport.AuthMethod, ref plumbing.ReferenceName) (plumbing.Hash, bool, error) {
	advertised, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return plumbing.ZeroHash, false, nil
		}
		return plumbing.ZeroHash, false, fmt.Errorf("pushbroker: list: %w", err)
	}
	for _, r := range advertised {
		if r.Name() == ref {
			return r.Hash(), true, nil
		}
	}
	return plumbing.ZeroHash, false, nil
}

// createOutcome classifies a create from its receive-pack result (pushErr) and the
// read-back list that followed it (current/present, or listErr):
//
//   - read-back failed: the outcome is unknown, so a wrapped error (never a sentinel);
//   - ref at tip: nil when the create was accepted, ErrRefExistsAtTip when it was
//     refused or failed (someone, or a lost response of ours, put exactly our tip
//     there). Both are success to callers;
//   - ref at another tip: ErrRefExists;
//   - ref absent: a wrapped error. An accepted create the read-back cannot see is an
//     error too, never a silent success.
func createOutcome(pushErr error, current plumbing.Hash, present bool, listErr error, tip plumbing.Hash) error {
	switch {
	case listErr != nil:
		if pushErr != nil {
			return fmt.Errorf("%w (read-back failed: %v)", pushErr, listErr)
		}
		return fmt.Errorf("pushbroker: create ref read-back: %w", listErr)
	case present && current == tip:
		if pushErr != nil {
			return ErrRefExistsAtTip
		}
		return nil
	case present:
		return ErrRefExists
	case pushErr != nil:
		return pushErr
	default:
		return errors.New("pushbroker: create ref reported success but origin does not advertise the ref")
	}
}

// ListRefsOptions names the forge connection ListRefTips lists. Server-derived by the
// caller, like DeleteOptions.
type ListRefsOptions struct {
	CloneURL string
	Username string
	PAT      string
}

// ListRefTips lists origin once and returns the tips of those of refs it advertises
// (an absent ref has no key; an empty origin yields an empty map). Every ref must lie
// under refs/uzi-checkpoints/, refs/uzi-recovery/ or refs/uzi-salvage/, else
// ErrInvalidRef before any network I/O. A read, never a write: a caller uses it to tell
// the reasons CreateRef reported ErrSourceMissing apart, to pick a salvage source, or to
// confirm a salvage delete (PRD #1867).
func ListRefTips(ctx context.Context, o ListRefsOptions, refs ...string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, maxDeleteDuration)
	defer cancel()
	want := make(map[plumbing.ReferenceName]bool, len(refs))
	for _, r := range refs {
		if !validManagedRef(r) {
			return nil, ErrInvalidRef
		}
		want[plumbing.ReferenceName(r)] = true
	}
	remote, err := newOriginRemote(o.CloneURL)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(refs))
	advertised, err := remote.ListContext(ctx, &git.ListOptions{Auth: authFor(Options{Username: o.Username, PAT: o.PAT})})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return out, nil
		}
		return nil, fmt.Errorf("pushbroker: list: %w", err)
	}
	for _, r := range advertised {
		if want[r.Name()] {
			out[r.Name().String()] = r.Hash().String()
		}
	}
	return out, nil
}

// createRefRequest builds the single-command create: Old is ALWAYS the zero hash (the
// wire compare-and-swap that refuses an existing ref), New is the tip, and the pack
// is attached when non-nil. There is no force form.
func createRefRequest(caps *capability.List, ref plumbing.ReferenceName, tip plumbing.Hash, pack []byte) *packp.ReferenceUpdateRequest {
	req := packp.NewReferenceUpdateRequestFromCapabilities(caps)
	req.Commands = []*packp.Command{{
		Name: ref,
		Old:  plumbing.ZeroHash,
		New:  tip,
	}}
	if pack != nil {
		req.Packfile = io.NopCloser(bytes.NewReader(pack))
	}
	return req
}

// emptyPack returns a valid zero-object packfile: "PACK", version 2, object count 0
// (both big-endian uint32), then the SHA-1 of those 12 bytes. 32 bytes in all.
func emptyPack() []byte {
	hdr := make([]byte, 12, 32)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], 0)
	sum := sha1.Sum(hdr) //nolint:gosec // G401: the packfile trailer is defined as SHA-1 by the git pack format, not used for security.
	return append(hdr, sum[:]...)
}

// validateCreateRef checks CreateRef's inputs without touching the network. The target
// is a recovery ref sourced from a checkpoint ref (PRD #1810), or a salvage ref sourced
// from a checkpoint or recovery ref (PRD #1867); every other pairing is ErrInvalidRef.
func validateCreateRef(o CreateRefOptions) error {
	switch {
	case validRefUnder(o.Ref, RecoveryRefPrefix):
		if !validRefUnder(o.SourceRef, checkpointRefPrefix) {
			return ErrInvalidRef
		}
	case validRefUnder(o.Ref, SalvageRefPrefix):
		if !validRefUnder(o.SourceRef, checkpointRefPrefix) && !validRefUnder(o.SourceRef, RecoveryRefPrefix) {
			return ErrInvalidRef
		}
	default:
		return ErrInvalidRef
	}
	if !isObjectID(o.Tip) || plumbing.NewHash(o.Tip).IsZero() {
		return ErrInvalidRef
	}
	return nil
}

// validRefUnder reports whether ref is a well-formed git ref name (git
// check-ref-format rules, via go-git) strictly under prefix.
func validRefUnder(ref, prefix string) bool {
	if !strings.HasPrefix(ref, prefix) || len(ref) == len(prefix) {
		return false
	}
	return plumbing.ReferenceName(ref).Validate() == nil
}

// isObjectID reports whether s is a lowercase 40-hex SHA-1 object id.
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// newOriginRemote builds the throwaway in-memory repo and its "origin" remote every
// list/receive-pack operation here dials.
func newOriginRemote(cloneURL string) (*git.Remote, error) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return nil, fmt.Errorf("pushbroker: init: %w", err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{cloneURL},
	})
	if err != nil {
		return nil, fmt.Errorf("pushbroker: create remote: %w", err)
	}
	return remote, nil
}

// openReceivePack opens a manual receive-pack session on remote's URL (through
// transportFor, so http(s) keeps the same-origin redirect guard) and returns it with
// its reference advertisement. The caller closes the session.
func openReceivePack(ctx context.Context, remote *git.Remote, auth transport.AuthMethod) (transport.ReceivePackSession, *packp.AdvRefs, error) {
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return nil, nil, fmt.Errorf("pushbroker: remote has no URL")
	}
	ep, err := transport.NewEndpoint(urls[0])
	if err != nil {
		return nil, nil, fmt.Errorf("pushbroker: endpoint: %w", err)
	}
	c, err := transportFor(ep)
	if err != nil {
		return nil, nil, fmt.Errorf("pushbroker: transport client: %w", err)
	}
	sess, err := c.NewReceivePackSession(ep, auth)
	if err != nil {
		return nil, nil, fmt.Errorf("pushbroker: receive-pack session: %w", err)
	}
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		_ = sess.Close()
		return nil, nil, fmt.Errorf("pushbroker: advertise: %w", err)
	}
	return sess, ar, nil
}

// forwardPack ships the worker's (non-thin) packfile to origin through a MANUAL
// git-receive-pack session and returns the outcome. Unlike remote.PushContext it does
// NOT recompute a send-set — it forwards `pack` verbatim and lets the remote resolve
// reachability against ITS objects, which is what fixes the "object not found" the
// send-set walk raised locally once origin's default advanced past the pack's
// exclude boundary (issue #1009). The single command carries the caller-supplied Old
// (the checkpoint tip as fetched) and New (the declared tip); the remote's
// compare-and-swap on Old is the wire-level half of the never-forced invariant.
//
// The endpoint is derived from the SAME URL the remote uses (its config URL — the
// value fetchBaseRefs listed and PushContext would have dialed), so the receive-pack
// session talks to the identical host over the identical transport. The receive-pack
// session does its OWN reference advertisement (AdvertisedReferencesContext); the
// earlier upload-pack List from fetchBaseRefs is a different service and is not reusable.
func forwardPack(ctx context.Context, remote *git.Remote, auth transport.AuthMethod, ref plumbing.ReferenceName, oldHash, newHash plumbing.Hash, pack []byte) error {
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return fmt.Errorf("pushbroker: remote has no URL")
	}
	ep, err := transport.NewEndpoint(urls[0])
	if err != nil {
		return fmt.Errorf("pushbroker: endpoint: %w", err)
	}
	c, err := transportFor(ep)
	if err != nil {
		return fmt.Errorf("pushbroker: transport client: %w", err)
	}
	sess, err := c.NewReceivePackSession(ep, auth)
	if err != nil {
		return fmt.Errorf("pushbroker: receive-pack session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return fmt.Errorf("pushbroker: advertise: %w", err)
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{
		Name: ref,
		Old:  oldHash,
		New:  newHash,
	}}
	if len(pack) > 0 {
		req.Packfile = io.NopCloser(bytes.NewReader(pack))
	}
	// ReceivePack returns the report-status AND report.Error() (nil on success, else the
	// first failing command's "ng <ref> <reason>" text or the unpack error). The caller
	// classifies on that error's text via isWorkflowScopeRejection / isNonFastForward.
	_, err = sess.ReceivePack(ctx, req)
	return err
}

// scanPackBudget rejects a worker pack that would inflate past the budget constants above,
// before any object is resolved into the unbounded in-memory storer. The walk itself lives in
// packbudget.Scan (shared with agentsource's untrusted clone); this wrapper supplies the
// checkpoint-publish limits and maps the shared errors onto ErrPackTooLarge / ErrPackInvalid.
// A context error is returned as is. Because it validates the SAME immutable pack []byte that
// UpdateObjectStorage later parses, there is no TOCTOU.
func scanPackBudget(ctx context.Context, pack []byte) error {
	err := packbudget.Scan(ctx, pack, packbudget.Limits{
		ObjectBytes:        maxPackObjectBytes,
		TotalBytes:         maxPackTotalBytes,
		InflationWorkBytes: maxPackInflationWorkBytes,
		Objects:            maxPackObjects,
	})
	switch {
	case errors.Is(err, packbudget.ErrTooLarge):
		return ErrPackTooLarge
	case errors.Is(err, packbudget.ErrInvalid):
		return ErrPackInvalid
	}
	return err
}

// readDeltaVarint is packbudget.ReadDeltaVarint (git's delta-header LEB128), kept so the
// pack-assembly tests in this package keep pinning the decode.
func readDeltaVarint(b []byte) (int64, []byte, error) { return packbudget.ReadDeltaVarint(b) }

// fetchBaseRefs fetches ONLY the branch, the default branch, and the branch's
// checkpoint ref — never every head. Because a specific refspec for an
// unadvertised ref fails the whole fetch, it first lists the remote's advertised
// refs and fetches only the subset that exists. Missing refs, an empty remote and
// up-to-date are all tolerated (the primary M8 case is a branch never pushed).
func fetchBaseRefs(ctx context.Context, remote *git.Remote, auth transport.AuthMethod, branch, defaultBranch string) error {
	advertised, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil // nothing to fetch; the storer stays empty and tips resolve zero
		}
		return fmt.Errorf("pushbroker: list: %w", err)
	}
	exists := make(map[plumbing.ReferenceName]bool, len(advertised))
	for _, r := range advertised {
		exists[r.Name()] = true
	}

	// Deduplicate: branch and defaultBranch can coincide, and defaultBranch may be
	// empty (a repo-less/unknown default). Each head lands under refs/remotes/origin/*;
	// the checkpoint namespace is mirrored 1:1.
	want := map[string]string{}
	addHead := func(b string) {
		if b == "" {
			return
		}
		want["refs/heads/"+b] = "refs/remotes/origin/" + b
	}
	addHead(branch)
	addHead(defaultBranch)
	if branch != "" {
		want[checkpointRefPrefix+branch] = checkpointRefPrefix + branch
	}

	var specs []config.RefSpec
	for src, dst := range want {
		if exists[plumbing.ReferenceName(src)] {
			specs = append(specs, config.RefSpec("+"+src+":"+dst))
		}
	}
	if len(specs) == 0 {
		return nil // none of the wanted refs exist on origin yet
	}
	fetchErr := remote.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   specs,
		Auth:       auth,
		Tags:       git.NoTags,
		// SHALLOW (depth 1) — bounds memory to ONE snapshot, not the branch's whole
		// history. A full fetch of a real repo unpacks its entire pack into the
		// in-memory storer: measured 787 MiB heap / 742 MB RSS for uzi's 131 MiB pack,
		// which OOM-killed the 512Mi api pod at every checkpoint (PRD #122 M8, found in
		// the first real-forge deploy — the unit tests use a 1-commit local fixture,
		// where depth is a no-op, so they never caught it). Depth 1 fetches only each
		// wanted ref's TIP snapshot — exactly the base objects the worker's thin delta
		// pack references and the ancestry checks need (they only ever walk DOWN to the
		// fetched tip, never below it). Measured with depth 1: 47 MiB heap / 194 MB RSS
		// including the push. Validated against gitlab.example.com before shipping.
		Depth: 1,
	})
	if fetchErr != nil &&
		!errors.Is(fetchErr, git.NoErrAlreadyUpToDate) &&
		!errors.Is(fetchErr, transport.ErrEmptyRemoteRepository) {
		return fmt.Errorf("pushbroker: fetch: %w", fetchErr)
	}
	return nil
}

// pushRefSpec builds the checkpoint refspec in its DELIBERATELY NON-FORCED form —
// no leading '+'. Since issue #1009 the actual push no longer goes through a refspec
// at all (forwardPack ships the pack via a receive-pack Command whose Old is the
// wire-level guard); this remains as the canonical non-forced form and is exercised
// only by TestPushRefSpecNotForced, which asserts the literal refspec carries no '+'
// so the never-forced intent stays pinned even though the push mechanism moved.
func pushRefSpec(ref string) config.RefSpec {
	return config.RefSpec(ref + ":" + ref)
}

// httpTransport is the go-git smart-HTTP transport every broker operation uses. Its
// client follows a redirect only while it stays on the original request's scheme,
// hostname and effective port (redirectguard.SameOrigin), composed after go-git's
// own policy (redirects only on the initial discovery request). go-git alone admits
// a same-host redirect that changes scheme or port, and net/http re-sends the
// BasicAuth header to the same hostname, so without this the PAT could leave in
// cleartext or reach another port before go-git's later endpoint check runs.
var httpTransport = githttp.NewClient(&http.Client{
	// Explicit, not nil: go-git type-asserts *http.Transport when an endpoint carries
	// TLS or proxy options.
	Transport:     http.DefaultTransport,
	CheckRedirect: redirectguard.SameOrigin,
})

// init installs httpTransport for http and https in go-git's process-global
// protocol registry, ONCE, before any goroutine can read it (the registry is an
// unsynchronised map, so it is never written per request). remote.ListContext,
// FetchContext and PushContext have no per-call transport option and resolve
// through the registry; the manual receive-pack sessions select httpTransport
// directly (transportFor). internal/agentsource never consults the registry for
// http(s): its scoped client is unaffected.
func init() {
	client.InstallProtocol("http", httpTransport)
	client.InstallProtocol("https", httpTransport)
}

// transportFor returns the transport for a manual session on ep: httpTransport for
// http(s), go-git's stock client otherwise (the file:// test fixtures).
func transportFor(ep *transport.Endpoint) (transport.Transport, error) {
	switch ep.Protocol {
	case "http", "https":
		return httpTransport, nil
	default:
		return client.NewClient(ep)
	}
}

// authFor returns BasicAuth only when a credential is present. A file:// fixture
// (the tests) passes empty creds and gets nil auth — go-git's file transport does
// not take an AuthMethod, and passing an empty BasicAuth over HTTP would be a
// pointless anonymous attempt. Real-forge publishes always carry a PAT.
func authFor(o Options) transport.AuthMethod {
	if o.PAT == "" && o.Username == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: o.Username, Password: o.PAT}
}

// resolveHash returns the hash a ref points at, or the zero hash if it is absent.
func resolveHash(repo *git.Repository, name plumbing.ReferenceName) plumbing.Hash {
	r, err := repo.Reference(name, true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return r.Hash()
}

// strictDescends reports whether declared is a STRICT descendant of base: not equal
// to base, and base reachable from declared.
func strictDescends(repo *git.Repository, base plumbing.Hash, declared *object.Commit) (bool, error) {
	if base == declared.Hash {
		return false, nil
	}
	return descendsOrEqual(repo, base, declared)
}

// descendsOrEqual reports whether base is an ancestor of declared (equal counts, as
// `git merge-base --is-ancestor X X` does).
func descendsOrEqual(repo *git.Repository, base plumbing.Hash, declared *object.Commit) (bool, error) {
	baseCommit, err := object.GetCommit(repo.Storer, base)
	if err != nil {
		// The base tip's commit object is missing from the storer, which for a
		// fetched ref should never happen; treat it as a non-descendant rather than
		// a 5xx so a corrupt/partial origin degrades to a skip.
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return false, nil
		}
		return false, err
	}
	ok, err := baseCommit.IsAncestor(declared)
	if err != nil {
		// IsAncestor walks preorder from `declared` toward its parents, stopping the
		// instant it reaches base. A genuine descendant therefore never walks BELOW base,
		// so it never needs an object the depth-1 base fetch left out. But a genuine
		// NON-descendant walk finds no stop point and runs off the end of the pack into
		// the branch-point's excluded parent (D_old, absent from the storer), surfacing
		// plumbing.ErrObjectNotFound. That is a non-descendant, not a fault: map it to
		// (false, nil) so the caller returns ErrNotDescendant (a 200 skip) rather than a
		// 5xx "ancestry" error (issue #1009). Mirrors the GetCommit(base) handling above.
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// isNonFastForward matches a receive-pack report-status that rejected the checkpoint
// update because origin's ref moved out from under our fetched Old — the wire-level
// half of the never-forced invariant. There is no sentinel: ReceivePack surfaces the
// server's per-command "ng <ref> <reason>" text (and PushContext, still exercised by
// TestPushRefSpecNotForced's sibling, formatted "non-fast-forward update: <ref>"), so
// the message text is the only discriminator. It matches the several reasons a
// compare-and-swap / fast-forward refusal takes across git versions and services:
// "non-fast-forward", plus the CAS-mismatch forms "failed to update ref", "cannot lock
// ref", and "fetch first". All map to ErrNotDescendant (a 200 skip). None of these
// substrings appear in GitHub's workflow-scope rejection, and the switch checks
// isWorkflowScopeRejection first regardless, so the two predicates never collide.
func isNonFastForward(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "failed to update ref") ||
		strings.Contains(msg, "cannot lock ref") ||
		strings.Contains(msg, "fetch first")
}

// isCASDeleteRefusal matches a receive-pack report-status that refused a CAS DELETE
// (casDelete) because origin's checkpoint ref no longer equals the Old we bound to the
// run's persisted tip — either it advanced past that tip (a newer sibling run or a
// human moved it) or it is already absent (server tip zero ≠ our non-zero Old). Both
// are benign: the caller returns nil and never retries.
//
// As with isNonFastForward there is no sentinel — ReceivePack surfaces the server's
// per-command "ng <ref> <reason>" text, so the message is the only discriminator. It
// reuses isNonFastForward's compare-and-swap / lock-failure forms ("non-fast-forward",
// "failed to update ref", "cannot lock ref", "fetch first" — a mismatched Old on a
// delete surfaces as a "cannot lock ref … but expected …" lock failure), and ADDS the
// delete-of-missing forms a ref-already-absent refusal can take across git versions and
// services ("does not exist", "no such ref").
func isCASDeleteRefusal(err error) bool {
	if err == nil {
		return false
	}
	if isNonFastForward(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such ref")
}

// isWorkflowScopeRejection matches GitHub's remote rejection of a push whose
// `.github/workflows/` tree differs from the current default branch when the PAT lacks
// the `workflow` scope. The observed message is a `remote rejected` line reading
// "refusing to allow a Personal Access Token to create or update workflow
// `.github/workflows/<file>` without workflow scope" (PRD #456). go-git surfaces the
// remote's rejection text formatted into the push error, so — as with isNonFastForward
// — the message text is the only discriminator. Match tolerantly and case-insensitively
// on the stable substrings so a wording drift on GitHub's side does not miss it: any of
// "workflow scope" (the short form), "without workflow scope", or the fuller "refusing
// to allow a personal access token to create or update workflow" clause counts.
func isWorkflowScopeRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "workflow scope") ||
		strings.Contains(msg, "without workflow scope") ||
		strings.Contains(msg, "refusing to allow a personal access token to create or update workflow")
}
