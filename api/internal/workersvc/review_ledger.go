package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The MR-rework ledger tracks comments by (namespace, id), never by a bare id (ADR-1199): a
// forge draws comment ids from distinct sequences (forge.CommentNS*), so ids are ordered only
// within one namespace. The ledger keeps one advance-only mark per namespace plus a set of
// source-qualified pending keys, and everything in this package that asks "is this comment
// new" or "which pending comment is this" goes through the types below.

// CommentKey is the identity of one MR comment.
type CommentKey struct {
	Namespace string
	ID        int64
}

// String is the ledger's stored form, "<namespace>:<id>".
func (k CommentKey) String() string { return k.Namespace + ":" + strconv.FormatInt(k.ID, 10) }

// KeyOf is the identity of a forge comment.
func KeyOf(c forge.MRComment) CommentKey {
	return CommentKey{Namespace: c.IDNamespace, ID: c.ID}
}

// ParseCommentKey parses the stored form. The key splits at its LAST colon; the id must be a
// canonical non-negative decimal within int64 (no sign, no leading zeros) and the namespace
// lowercase letters and underscores, which is exactly what the ledger's CHECK constraint
// accepts, so a key that parses here round-trips through the database.
func ParseCommentKey(s string) (CommentKey, error) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return CommentKey{}, fmt.Errorf("comment key %q: missing ':'", s)
	}
	ns, idStr := s[:i], s[i+1:]
	for _, r := range ns {
		if r != '_' && (r < 'a' || r > 'z') {
			return CommentKey{}, fmt.Errorf("comment key %q: invalid namespace", s)
		}
	}
	id, err := parseCanonicalID(idStr)
	if err != nil {
		return CommentKey{}, fmt.Errorf("comment key %q: %w", s, err)
	}
	return CommentKey{Namespace: ns, ID: id}, nil
}

// parseCanonicalID accepts only what strconv.FormatInt would print for a non-negative int64.
func parseCanonicalID(s string) (int64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("id %q is not a canonical decimal", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("id %q is not a canonical decimal", s)
		}
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id %q: %w", s, err)
	}
	return id, nil
}

// ReviewLedgerState is the part of a ledger row that decides whether a comment is new: the
// consumed mark of each namespace and the pending (permission-unknown) comment keys. A
// comment is NEW when its id is above its own namespace's mark or its key is pending.
type ReviewLedgerState struct {
	Marks   map[string]int64
	Pending []CommentKey
}

// IsNew reports whether the comment is new against this state.
func (s ReviewLedgerState) IsNew(c forge.MRComment) bool {
	return s.isNewKey(KeyOf(c), s.pendingSet())
}

// isNewKey is the one newness rule: above the key's own namespace's mark, or pending. pending
// is s.pendingSet(), passed in so a loop builds it once.
func (s ReviewLedgerState) isNewKey(k CommentKey, pending map[CommentKey]bool) bool {
	return k.ID > s.Marks[k.Namespace] || pending[k]
}

func (s ReviewLedgerState) pendingSet() map[CommentKey]bool {
	set := make(map[CommentKey]bool, len(s.Pending))
	for _, k := range s.Pending {
		set[k] = true
	}
	return set
}

// LedgerStateFromRow decodes a ledger row. A LEGACY row (comment_marks NULL) has no namespaced
// state yet and is an error: it must be bootstrapped first (Service.BootstrapReviewLedger). The
// zero row, which is what a ref with no ledger row reads as, is the empty non-legacy state.
func LedgerStateFromRow(row store.GetMRReworkLedgerRow) (ReviewLedgerState, error) {
	if row.Legacy {
		return ReviewLedgerState{}, errors.New("review ledger row is legacy: bootstrap it first")
	}
	marks, err := decodeMarks(row.CommentMarks)
	if err != nil {
		return ReviewLedgerState{}, err
	}
	pending := make([]CommentKey, 0, len(row.PendingUnknownKeys))
	for _, s := range row.PendingUnknownKeys {
		k, err := ParseCommentKey(s)
		if err != nil {
			return ReviewLedgerState{}, fmt.Errorf("decode pending_unknown_keys: %w", err)
		}
		pending = append(pending, k)
	}
	return ReviewLedgerState{Marks: marks, Pending: pending}, nil
}

// decodeMarks reads comment_marks: empty bytes, JSON null and {} are the empty map; otherwise a
// JSON object whose values are non-negative integers.
func decodeMarks(b []byte) (map[string]int64, error) {
	marks := map[string]int64{}
	if len(bytes.TrimSpace(b)) == 0 {
		return marks, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decode comment_marks: %w", err)
	}
	for ns, n := range raw {
		// The raw token, so a quoted "1", 1.5 or 1e3 is not mistaken for the integer 1.
		id, err := parseCanonicalID(string(n))
		if err != nil {
			return nil, fmt.Errorf("decode comment_marks[%q]: %w", ns, err)
		}
		marks[ns] = id
	}
	return marks, nil
}

// encodeMarks is the JSON object the ledger statements take as @marks: nil and empty are "{}",
// never "null".
func encodeMarks(m map[string]int64) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil { // a map[string]int64 always marshals
		return []byte("{}")
	}
	return b
}

// encodeKeys is the stored form of keys, never nil so a pgx array parameter is never NULL.
func encodeKeys(keys []CommentKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.String())
	}
	return out
}

// ReviewLedgerBootstrapOverflowError reports that bootstrapping a legacy ledger row would
// expand its pending ids past ReviewPendingCap: a legacy id present in several namespaces
// becomes one key per namespace. Nothing was written. It is an ErrReviewLedgerBootstrapOverflow.
type ReviewLedgerBootstrapOverflowError struct {
	LegacyIDs    int
	ExpandedKeys int
}

func (e *ReviewLedgerBootstrapOverflowError) Error() string {
	return ErrReviewLedgerBootstrapOverflow.Error()
}

func (e *ReviewLedgerBootstrapOverflowError) Is(target error) bool {
	return target == ErrReviewLedgerBootstrapOverflow
}

// bootstrapLedger derives the namespaced state of a legacy row from its scalar high-water mark
// and pending ids and the MR's complete comment listing. A namespace's mark is the largest id
// observed in it at or below the legacy mark (omitted when none): the scalar was a max over
// mixed id spaces, so this is the most it can safely say was consumed per namespace. A legacy
// pending id becomes a key in EVERY namespace whose comments include that id (ambiguity keeps
// them all, in legacy order then namespace order); an id no comment carries is gone and is not
// carried over.
func bootstrapLedger(legacyHighWater int64, legacyPending []int64, comments []forge.MRComment) (map[string]int64, []CommentKey) {
	marks := map[string]int64{}
	byID := map[int64][]string{}
	for _, c := range comments {
		if c.ID <= legacyHighWater {
			marks[c.IDNamespace] = max(marks[c.IDNamespace], c.ID)
		}
		if !slices.Contains(byID[c.ID], c.IDNamespace) {
			byID[c.ID] = append(byID[c.ID], c.IDNamespace)
		}
	}
	pending := []CommentKey{}
	seen := map[CommentKey]bool{}
	for _, id := range legacyPending {
		nss := slices.Clone(byID[id])
		slices.Sort(nss)
		for _, ns := range nss {
			k := CommentKey{Namespace: ns, ID: id}
			if !seen[k] {
				seen[k] = true
				pending = append(pending, k)
			}
		}
	}
	return marks, pending
}

// BootstrapReviewLedger returns the namespaced ledger state for the row pre, which the caller
// read BEFORE listing comments. A non-legacy row decodes as is. A legacy row (comment_marks
// NULL, written before ADR-1199) is upgraded once from its scalar state and comments, the
// complete listing taken after pre was read. The write is a compare-and-set on the legacy
// scalars pre carried, under the run-branch lock: if anything moved them, or another replica
// bootstrapped first, nothing is written and ErrReworkLedgerChanged says to re-read. A legacy
// row whose pending ids would expand past ReviewPendingCap is refused before any write with
// *ReviewLedgerBootstrapOverflowError and left untouched. The forge is never called inside the
// transaction.
func (s *Service) BootstrapReviewLedger(ctx context.Context, repoID uuid.UUID, ref string, pre store.GetMRReworkLedgerRow, comments []forge.MRComment) (ReviewLedgerState, error) {
	if !pre.Legacy {
		return LedgerStateFromRow(pre)
	}
	marks, pending := bootstrapLedger(pre.HighWater, pre.PendingUnknownIds, comments)
	if len(pending) > ReviewPendingCap {
		return ReviewLedgerState{}, &ReviewLedgerBootstrapOverflowError{LegacyIDs: len(pre.PendingUnknownIds), ExpandedKeys: len(pending)}
	}
	if s.txBeginner == nil {
		return ReviewLedgerState{}, errors.New("bootstrap review ledger: no transaction beginner wired")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return ReviewLedgerState{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	q := store.New(tx)
	// The same (repo, ref) lock every ledger-writing create takes first, so a bootstrap and an
	// advance serialize and the loser sees the winner's row.
	if err := q.LockRunBranch(ctx, repoID, ref); err != nil {
		return ReviewLedgerState{}, err
	}
	legacyPending := pre.PendingUnknownIds
	if legacyPending == nil {
		legacyPending = []int64{}
	}
	n, err := q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{
		Marks:           encodeMarks(marks),
		PendingKeys:     encodeKeys(pending),
		RepoID:          repoID,
		Ref:             ref,
		LegacyHighWater: pre.HighWater,
		LegacyPending:   legacyPending,
	})
	if err != nil {
		return ReviewLedgerState{}, fmt.Errorf("bootstrap mr rework ledger: %w", err)
	}
	if n == 0 {
		return ReviewLedgerState{}, ErrReworkLedgerChanged
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewLedgerState{}, err
	}
	return ReviewLedgerState{Marks: marks, Pending: pending}, nil
}
