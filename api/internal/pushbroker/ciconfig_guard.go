package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// ErrCIConfigGuard means a guarded run's candidate tip (a checkpoint or held-work publication)
// could not be proven free of changes to protected CI-config paths: a protected path differs from
// the default branch, or the proof could not be completed (missing default ref or tree, a walk
// error, the entry cap, a cancelled context). It fails closed; the caller skips the publication.
var ErrCIConfigGuard = errors.New("pushbroker: ci-config guard refused the publication")

const (
	// ciConfigWalkCap bounds the tree walk: every tree entry visited across BOTH trees, subtree
	// entries included, counts one. Reaching the cap is a refusal (fail closed), never a verdict.
	// An identical subtree (same mode and hash on both sides) is skipped without descending, so
	// it costs the one entry that names it; identical hash implies no differing path beneath.
	ciConfigWalkCap = 100_000
	// ciConfigMaxDepth bounds directory nesting so a hostile chain of one-entry trees cannot make
	// the path building quadratic. Real git paths are bounded by PATH_MAX, far below this.
	ciConfigMaxDepth = 4096
	// ciConfigMaxPathBytes bounds one tree entry name and one joined path. A walk bounded only by
	// entry count still runs for minutes on a single entry with a huge name (path concatenation
	// and glob matching are linear in its length), so an oversized name or path is a refusal.
	ciConfigMaxPathBytes = 4096
	// ciConfigMaxPathBytesTotal bounds the path bytes processed across the whole walk, the byte
	// analogue of ciConfigWalkCap.
	ciConfigMaxPathBytesTotal = 64 << 20
)

// ciConfigGlobRegex ports ciConfigGuardRegex of agent/src/ci-config-guard.ts: `**/` is an
// optional run of any characters ending in `/`, `**` is any characters, `*` is any character
// except `/`, and "any" includes newlines. Everything else is literal. Anchored on the whole
// input. The caller trims the glob.
func ciConfigGlobRegex(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	for i := 0; i < len(glob); {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString(`(?:.*/)?`)
			i += 3
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(`.*`)
			i += 2
		case glob[i] == '*':
			b.WriteString(`[^/]*`)
			i++
		default:
			// Advance one byte at a time; QuoteMeta only escapes ASCII metacharacters, so
			// multi-byte sequences pass through unchanged.
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
			i++
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

// newCIConfigMatcher returns a predicate that is true when a RAW path matches any glob. Globs
// are server-trusted configuration: trimmed, empty ones dropped. Paths are never trimmed or
// normalised; only a fully empty path is ignored. ok is false when no effective glob remains.
func newCIConfigMatcher(globs []string) (match func(string) bool, ok bool, err error) {
	var res []*regexp.Regexp
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		re, cerr := ciConfigGlobRegex(g)
		if cerr != nil {
			return nil, false, cerr
		}
		res = append(res, re)
	}
	if len(res) == 0 {
		return nil, false, nil
	}
	return func(p string) bool {
		if p == "" {
			return false
		}
		for _, re := range res {
			if re.MatchString(p) {
				return true
			}
		}
		return false
	}, true, nil
}

// ciConfigGuard refuses (ErrCIConfigGuard) when the candidate commit's tree differs from the tip
// of the fetched default branch (refs/remotes/origin/<defaultBranch>) at any protected path.
//
// It is depth-1 safe: the broker fetches the default branch at depth 1, so there is no history
// to walk and no merge base. It compares the two root trees directly.
//
// It is a backstop beside the worker's own merge-base guard (agent/src/runner.ts), and it fails
// closed on anything it cannot prove. The worker's pack is `tip ^floor`, so a subtree the
// candidate shares with the floor is usually absent from the broker's object store. When both
// sides hold a directory with differing hashes and the candidate-side tree object is absent,
// the comparison is unknown and the publication is refused: absence does not prove the run
// inherited the subtree unchanged (an agent-authored merge whose extra parent is outside the
// pack also leaves trees out). Once a checkpoint or branch ref exists on origin, fetchBaseRefs
// fetches it at depth 1, so its inherited subtrees are present and compared. The accepted cost
// is that a guarded publication is refused whenever the default branch moved under a directory
// since the run's floor: a non-fatal checkpoint skip or a held refusal (archive fallback); the
// worker's merge-base check decides the final branch push. Also fail closed: a missing
// candidate root tree, any missing default-side object, a candidate-only directory whose tree is
// missing, and any other error.
func ciConfigGuard(ctx context.Context, repo *git.Repository, candidate *object.Commit, defaultBranch string, globs []string) error {
	match, ok, err := newCIConfigMatcher(globs)
	if err != nil {
		return fmt.Errorf("%w: bad glob: %v", ErrCIConfigGuard, err)
	}
	if !ok {
		return nil // nothing is protected
	}
	if defaultBranch == "" {
		return fmt.Errorf("%w: default branch unknown", ErrCIConfigGuard)
	}
	ref, err := repo.Storer.Reference(plumbing.ReferenceName("refs/remotes/origin/" + defaultBranch))
	if err != nil || ref.Hash().IsZero() {
		return fmt.Errorf("%w: default branch ref not fetched", ErrCIConfigGuard)
	}
	def, err := object.GetCommit(repo.Storer, ref.Hash())
	if err != nil {
		return fmt.Errorf("%w: default tip unreadable: %v", ErrCIConfigGuard, err)
	}
	return ciConfigTreeGuard(ctx, repo.Storer, candidate.TreeHash, def.TreeHash, match)
}

// ciConfigTreeGuard compares two root trees and refuses on a differing protected path. A path
// differs when it is on one side only, its mode differs, or its blob hash differs.
func ciConfigTreeGuard(ctx context.Context, s storer.EncodedObjectStorer, cand, def plumbing.Hash, match func(string) bool) error {
	w := &ciConfigWalk{ctx: ctx, s: s, match: match, limit: ciConfigWalkCap, maxBytes: ciConfigMaxPathBytesTotal}
	if err := w.diff(cand, def, "", 0); err != nil {
		if errors.Is(err, ErrCIConfigGuard) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrCIConfigGuard, err)
	}
	return nil
}

type ciConfigWalk struct {
	ctx   context.Context
	s     storer.EncodedObjectStorer
	match func(string) bool
	limit int
	seen  int
	bytes int
	// maxBytes is the total path-byte cap (ciConfigMaxPathBytesTotal; a field so tests can lower it).
	maxBytes int
}

// visit counts one tree entry (name under the directory path prefix) and enforces the entry cap,
// the per-name and per-path byte caps, the total byte cap and the context.
func (w *ciConfigWalk) visit(prefix, name string) error {
	w.seen++
	if w.seen >= w.limit {
		return fmt.Errorf("%w: tree walk reached %d entries", ErrCIConfigGuard, w.limit)
	}
	if err := w.ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrCIConfigGuard, err)
	}
	if len(name) > ciConfigMaxPathBytes || len(prefix)+len(name) > ciConfigMaxPathBytes {
		return fmt.Errorf("%w: tree entry path longer than %d bytes", ErrCIConfigGuard, ciConfigMaxPathBytes)
	}
	w.bytes += len(prefix) + len(name)
	if w.bytes > w.maxBytes {
		return fmt.Errorf("%w: tree walk processed more than %d path bytes", ErrCIConfigGuard, w.maxBytes)
	}
	return nil
}

func (w *ciConfigWalk) tree(h plumbing.Hash) (*object.Tree, error) {
	if h.IsZero() {
		return &object.Tree{}, nil
	}
	return object.GetTree(w.s, h)
}

func (w *ciConfigWalk) hit(path string) error {
	if w.match(path) {
		return fmt.Errorf("%w: protected path differs from the default branch", ErrCIConfigGuard)
	}
	return nil
}

func isDirEntry(e object.TreeEntry) bool { return e.Mode == filemode.Dir }

func (w *ciConfigWalk) diff(a, b plumbing.Hash, prefix string, depth int) error {
	if depth > ciConfigMaxDepth {
		return fmt.Errorf("%w: tree nesting too deep", ErrCIConfigGuard)
	}
	ta, err := w.tree(a)
	if err != nil {
		return err
	}
	tb, err := w.tree(b)
	if err != nil {
		return err
	}
	byName := make(map[string]object.TreeEntry, len(tb.Entries))
	for _, e := range tb.Entries {
		if _, dup := byName[e.Name]; dup {
			return fmt.Errorf("%w: duplicate tree entry name", ErrCIConfigGuard)
		}
		byName[e.Name] = e
	}
	inA := make(map[string]struct{}, len(ta.Entries))
	for _, ea := range ta.Entries {
		if _, dup := inA[ea.Name]; dup {
			return fmt.Errorf("%w: duplicate tree entry name", ErrCIConfigGuard)
		}
		inA[ea.Name] = struct{}{}
		if err := w.visit(prefix, ea.Name); err != nil {
			return err
		}
		eb, both := byName[ea.Name]
		if !both {
			if err := w.whole(ea, prefix, depth); err != nil {
				return err
			}
			continue
		}
		if err := w.visit(prefix, ea.Name); err != nil {
			return err
		}
		if err := w.pair(ea, eb, prefix, depth); err != nil {
			return err
		}
	}
	for _, eb := range tb.Entries {
		if _, dup := inA[eb.Name]; dup {
			continue
		}
		if err := w.visit(prefix, eb.Name); err != nil {
			return err
		}
		if err := w.whole(eb, prefix, depth); err != nil {
			return err
		}
	}
	return nil
}

// pair compares the same-named entry on both sides.
func (w *ciConfigWalk) pair(ea, eb object.TreeEntry, prefix string, depth int) error {
	if ea.Hash == eb.Hash && ea.Mode == eb.Mode {
		return nil // identical (sub)tree or blob: nothing beneath differs
	}
	path := prefix + ea.Name
	switch {
	case isDirEntry(ea) && isDirEntry(eb):
		// A candidate subtree the store lacks cannot be proven unchanged: refuse. See ciConfigGuard.
		if _, err := w.s.EncodedObject(plumbing.TreeObject, ea.Hash); errors.Is(err, plumbing.ErrObjectNotFound) {
			return fmt.Errorf("%w: a differing candidate subtree is not in the pack", ErrCIConfigGuard)
		}
		return w.diff(ea.Hash, eb.Hash, path+"/", depth+1)
	case isDirEntry(ea) != isDirEntry(eb):
		// A file replaced a directory or the reverse: the file path differs on its own and
		// every path under the directory is gone or new.
		if err := w.whole(ea, prefix, depth); err != nil {
			return err
		}
		return w.whole(eb, prefix, depth)
	default:
		return w.hit(path)
	}
}

// whole treats every path under entry e as differing (the entry exists on one side only).
func (w *ciConfigWalk) whole(e object.TreeEntry, prefix string, depth int) error {
	path := prefix + e.Name
	if !isDirEntry(e) {
		return w.hit(path)
	}
	if depth+1 > ciConfigMaxDepth {
		return fmt.Errorf("%w: tree nesting too deep", ErrCIConfigGuard)
	}
	t, err := w.tree(e.Hash)
	if err != nil {
		return err
	}
	for _, c := range t.Entries {
		if err := w.visit(path+"/", c.Name); err != nil {
			return err
		}
		if err := w.whole(c, path+"/", depth+1); err != nil {
			return err
		}
	}
	return nil
}
