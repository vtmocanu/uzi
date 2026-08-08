// Package pushbroker is the api's pure-Go git client for PRD #122 M8's brokered
// checkpoint publish. It is the ONE place go-git is used, deliberately isolated
// from the forge REST drivers (internal/forge): the api image is
// distroless-static and carries NO git binary, so every network git op here runs
// through go-git's smart-HTTP transport.
//
// The security contract lives one layer up in workersvc.Publish (which derives
// every field of Options from the run row, never from the worker). This package's
// single job is the mechanical one, and its single security-relevant invariant is
// NEVER FORCED: the push to refs/uzi-checkpoints/<branch> uses a non-forced
// refspec, and before pushing it verifies the declared tip strictly descends
// origin's current tip. A remote non-fast-forward rejection is mapped to
// ErrNotDescendant — a legitimate "origin moved" outcome the caller turns into a
// 200 skip, never a 5xx.
package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
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
	// ErrPackTooLarge means the worker's pack declares more RECONSTRUCTED bytes (per
	// object or cumulatively) or more objects than the budget allows. It is enforced
	// by a header + delta-header pre-pass, before any object is resolved into the
	// unbounded in-memory storer, so a decompression- or delta-bomb pack is refused
	// rather than OOMing the shared api. The caller maps it to a best-effort
	// "unsupported" skip.
	ErrPackTooLarge = errors.New("pushbroker: pack exceeds inflation budget")
	// ErrPackInvalid means the pack header or an object header could not be parsed —
	// a genuinely malformed pack, distinct from an oversize one. The caller maps it to
	// a best-effort "unsupported" skip (200), NOT a 5xx: worker input must not
	// 500-storm the shared api.
	ErrPackInvalid = errors.New("pushbroker: pack is malformed")
)

// Pack inflation budget (PRD #122 M8 hardening). The handler caps the COMPRESSED
// wire body at 64 MiB, but a small compressed pack can declare a vastly larger
// inflated size (a highly-compressible blob was measured inflating 5297x), and a
// tiny delta can declare a reconstructed target of many GiB. scanPackBudget bounds
// each object by the size it will RECONSTRUCT to in the storer (the delta target
// size for deltas), so the ceiling on heap the subsequent apply allocates is
// maxPackTotalBytes, not the wire size.
const (
	maxPackObjectBytes = 32 << 20  // 32 MiB per declared object
	maxPackTotalBytes  = 128 << 20 // 128 MiB cumulative declared inflated size
	maxPackObjects     = 50000     // object-count ceiling
)

// checkpointRefPrefix is the uzi-owned ref namespace no CI watches (Rule 3). The
// end-of-run push targets refs/heads/<branch>; checkpoints never do.
const checkpointRefPrefix = "refs/uzi-checkpoints/"

// Publish fetches origin's base objects, applies the worker's delta pack, verifies
// the declared tip strictly descends origin's current tip, and pushes it —
// NON-FORCED — to refs/uzi-checkpoints/<branch>.
//
// NOTE: the go-git round-trip against a REAL forge is a manual/e2e validation step
// (see the package tests, which prove the algorithm against a local bare fixture).
func Publish(ctx context.Context, o Options) (Result, error) {
	ref := checkpointRefPrefix + o.Branch
	result := Result{Ref: ref}

	tipHash := plumbing.NewHash(o.DeclaredTip)
	if tipHash.IsZero() {
		// A malformed tip should have been rejected at the handler, but never trust
		// it here: a zero hash can never be a valid checkpoint tip.
		return Result{}, ErrTipMissing
	}

	// Step 1: BEFORE resolving anything into the unbounded in-memory storer, walk the
	// pack and enforce the reconstruction budget (per-object and cumulative, deltas
	// bounded by their declared target size). A worker-supplied pack is untrusted;
	// this is what keeps a decompression or delta bomb from OOM-killing the api.
	if err := scanPackBudget(o.Pack); err != nil {
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
	// for the never-pushed-branch case), and the branch's checkpoint ref. We do NOT
	// fetch every head / the full history (Rule: bounded fetch): only these three.
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

	// Step 6: set the local checkpoint ref and push it NON-FORCED. No leading '+',
	// no push-options.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(checkpointRef, tipHash)); err != nil {
		return Result{}, fmt.Errorf("pushbroker: set local ref: %w", err)
	}
	pushErr := remote.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{pushRefSpec(ref)},
		Auth:       auth,
	})
	switch {
	case pushErr == nil, errors.Is(pushErr, git.NoErrAlreadyUpToDate):
		return result, nil
	case isNonFastForward(pushErr):
		// origin advanced its checkpoint between our fetch and our push. The
		// non-forced push refused it — exactly the protocol-level guarantee we want.
		return Result{}, ErrNotDescendant
	default:
		return Result{}, fmt.Errorf("pushbroker: push: %w", pushErr)
	}
}

// scanPackBudget walks the pack and rejects it (ErrPackTooLarge) the instant a
// bound on the RECONSTRUCTED object size is exceeded, before any object is resolved
// into the unbounded in-memory storer. It reads the object count from the pack
// header, then for each object reads its header (NextObjectHeader) and fully
// consumes its inflated body (NextObject) to keep the scanner aligned.
//
// The subtlety this pre-pass exists for: an object header's declared Length bounds
// the INFLATED, UNRESOLVED content — for a non-delta object that is the object
// itself, but for a DELTA object (OFS/REF) it is only the delta INSTRUCTION stream,
// NOT the reconstructed object. go-git's UpdateObjectStorage later reconstructs each
// delta to its full target size (patch_delta.go grows a buffer to targetSz, with no
// hard cap — maxObjectPreallocBytes is only a prealloc hint). So a delta with a tiny
// instruction stream (well under the per-object cap) can declare a target size of
// many GiB and OOM the shared api. Bounding by Length alone would miss it entirely.
//
// Therefore, for a delta object we inflate its (Length-bounded, ≤ maxPackObjectBytes)
// body and read the target size — the SECOND unsigned LEB128 varint of the delta
// header (`[base-size][target-size][ops...]`) — and cap THAT. The cumulative counter
// tracks reconstructed bytes (targetSz for deltas, Length otherwise), which is
// exactly what lands in the storer, so it genuinely bounds the apply. Because this
// validates the SAME immutable pack []byte that UpdateObjectStorage later parses,
// there is no TOCTOU.
//
// A pack whose header or an object header cannot be parsed is a genuinely malformed
// pack: it returns ErrPackInvalid (a best-effort "unsupported" skip), never a 5xx.
// A declared-size overrun (a lying delta/object whose zlib stream inflates past its
// header Length) surfaces from the scanner's bounded writer as
// ErrInflatedSizeMismatch and is treated as ErrPackTooLarge.
func scanPackBudget(pack []byte) error {
	if len(pack) == 0 {
		return nil
	}
	scanner := packfile.NewScanner(bytes.NewReader(pack))
	_, objects, err := scanner.Header()
	if err != nil {
		return ErrPackInvalid // malformed header
	}
	if objects > maxPackObjects {
		return ErrPackTooLarge
	}
	var total int64
	for i := uint32(0); i < objects; i++ {
		h, err := scanner.NextObjectHeader()
		if err != nil {
			// A declared-size overrun on the PREVIOUS object surfaces here as a size
			// mismatch; anything else is a genuinely malformed pack.
			if errors.Is(err, packfile.ErrInflatedSizeMismatch) {
				return ErrPackTooLarge
			}
			return ErrPackInvalid
		}
		// The header Length bounds the inflated/unresolved content (the delta
		// instruction stream for a delta). Cap it first — this both rejects an
		// oversize non-delta object and keeps the delta body we inflate below to
		// ≤ maxPackObjectBytes.
		if h.Length < 0 || h.Length > maxPackObjectBytes {
			return ErrPackTooLarge
		}

		var contributed int64
		switch h.Type {
		case plumbing.OFSDeltaObject, plumbing.REFDeltaObject:
			// Inflate the delta body into a capped buffer (the scanner bounds the
			// write to h.Length ≤ 32 MiB) and read the reconstructed target size.
			var buf bytes.Buffer
			if _, _, err := scanner.NextObject(&buf); err != nil {
				if errors.Is(err, packfile.ErrInflatedSizeMismatch) {
					return ErrPackTooLarge
				}
				return ErrPackInvalid
			}
			b := buf.Bytes()
			if _, b, err = readDeltaVarint(b); err != nil { // base size (discard)
				return ErrPackInvalid
			}
			var targetSz int64
			if targetSz, _, err = readDeltaVarint(b); err != nil { // reconstructed size
				return ErrPackInvalid
			}
			if targetSz > maxPackObjectBytes {
				return ErrPackTooLarge
			}
			contributed = targetSz
		default:
			// Consume and advance past the object body; the scanner's bounded writer
			// still surfaces an overrun as a size mismatch.
			if _, _, err := scanner.NextObject(io.Discard); err != nil {
				if errors.Is(err, packfile.ErrInflatedSizeMismatch) {
					return ErrPackTooLarge
				}
				return ErrPackInvalid
			}
			contributed = h.Length
		}

		total += contributed
		if total > maxPackTotalBytes {
			return ErrPackTooLarge
		}
	}
	return nil
}

// readDeltaVarint decodes one unsigned little-endian base-128 varint from the head
// of b using git's DELTA-header size encoding (accumulate the low 7 bits of each
// byte, continue while the high bit 0x80 is set) and returns the value, the bytes
// after it, and an error on a truncated stream or a shift that would overflow int64.
// This is git's delta size encoding — the same decodeLEB128 patch_delta.go applies to
// the delta header's base/target sizes — and is DELIBERATELY not the packfile
// object-header length encoding, which packs the type into the first byte and shifts
// differently.
func readDeltaVarint(b []byte) (val int64, rest []byte, err error) {
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		// Cap the shift before applying it: a valid target size for our caps needs at
		// most a few bytes, so a varint this long is a malformed/hostile header. This
		// also keeps val non-negative (bit 63 is never written).
		if shift >= 63 {
			return 0, nil, errors.New("pushbroker: delta varint overflow")
		}
		val |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			return val, b[i+1:], nil
		}
	}
	return 0, nil, errors.New("pushbroker: delta varint truncated")
}

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
	})
	if fetchErr != nil &&
		!errors.Is(fetchErr, git.NoErrAlreadyUpToDate) &&
		!errors.Is(fetchErr, transport.ErrEmptyRemoteRepository) {
		return fmt.Errorf("pushbroker: fetch: %w", fetchErr)
	}
	return nil
}

// pushRefSpec builds the checkpoint push refspec. It is DELIBERATELY NON-FORCED —
// no leading '+' — which is the wire-level half of the never-forced invariant (the
// upstream strict-descendant check is only a local guard). Factored out so
// TestPushRefSpecNotForced can assert the literal refspec carries no '+', catching a
// forced-push regression that would otherwise stay green behind the local check.
func pushRefSpec(ref string) config.RefSpec {
	return config.RefSpec(ref + ":" + ref)
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
	return baseCommit.IsAncestor(declared)
}

// isNonFastForward matches go-git's push-time non-fast-forward rejection. go-git
// returns a formatted error ("non-fast-forward update: <ref>") rather than a
// sentinel at the push layer, so the message text is the only discriminator.
func isNonFastForward(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "non-fast-forward")
}
