package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Owner steering for plan cross-check (issue #3841, ADR-2149). The checker child receives a
// frozen, bounded snapshot of the run owner's authoritative in-run steering: answers to questions
// (with the question they answered) and revise_plan feedback. The snapshot is built once, in the
// transaction that creates the round's cross_checks row, stored verbatim in
// cross_check_owner_steering, and delivered byte-identically on every claim of that child.
const (
	ownerSteeringVersion = 1
	// ownerSteeringSnapshotMaxBytes bounds the stored snapshot text (the table CHECK repeats it).
	ownerSteeringSnapshotMaxBytes = 65536
	// ownerSteeringMaxEntries is the SnapshotOwnerSteering max_entries argument: the newest
	// entries kept before the byte cap is applied.
	ownerSteeringMaxEntries = 32
	// Per-entry text caps, in bytes of scrubbed text.
	ownerSteeringAnswerBytes  = 8 * 1024
	ownerSteeringReviseBytes  = 8 * 1024
	ownerSteeringContextBytes = 4 * 1024

	// Per-field character caps: the authoritative caps SnapshotOwnerSteering's *_truncated flags
	// are defined against. The query delivers each field cut at the cap plus ownerSteeringCutMargin
	// characters, so the builder scrubs first and applies these caps second.
	ownerSteeringReviseChars   = 8192
	ownerSteeringAnswerChars   = 2048
	ownerSteeringHeaderChars   = 128
	ownerSteeringQuestionChars = 2048
	ownerSteeringOptionChars   = 256

	// ownerSteeringWireMaxBytes bounds the bytes the steering field adds to a checker claim.
	// Derivation: the snapshot text is at most 65536 bytes and the claim encoder (json.Marshal,
	// HTML escaping on) writes any one byte as at most 6 bytes (a control character or <, >, &
	// becomes \u00XX; a 3-byte U+2028 also becomes 6), giving 6*65536. The digest is 64 hex
	// characters. The remaining 57 bytes are the wrapper
	// ,"cross_check_owner_steering":{"snapshot":"","digest":""} around those two values.
	ownerSteeringWireMaxBytes = 6*ownerSteeringSnapshotMaxBytes + 64 + 57

	steeringOverflowEntryCap = "entry_cap"
	steeringOverflowByteCap  = "byte_cap"
)

// ownerSteeringInput is the builder's pure input: the cutoff and total from the one-statement
// read plus the kept rows. It carries its own JSON shape so fixtures can load it.
type ownerSteeringInput struct {
	Cutoff int64              `json:"cutoff"`
	Total  int64              `json:"total"`
	Rows   []ownerSteeringRow `json:"rows"`
}

type ownerSteeringRow struct {
	InputID      int64   `json:"input_id"`
	Kind         string  `json:"kind"` // "answer" or "revise_plan"
	QuestionID   string  `json:"question_id,omitempty"`
	GateBinding  *string `json:"gate_binding,omitempty"`
	GateRevision *int64  `json:"gate_revision,omitempty"`

	ReviseBody      *string `json:"revise_body,omitempty"`
	ReviseTruncated bool    `json:"revise_truncated,omitempty"`

	AnswersStatus    string   `json:"answers_status,omitempty"`
	Answers          []string `json:"answers,omitempty"`
	AnswersTruncated bool     `json:"answers_truncated,omitempty"`

	Occurrences        int64              `json:"occurrences,omitempty"`
	Variants           int64              `json:"variants,omitempty"`
	QuestionAgent      string             `json:"question_agent,omitempty"`
	QuestionsStatus    string             `json:"questions_status,omitempty"`
	Questions          []steeringQuestion `json:"questions,omitempty"`
	QuestionsTruncated bool               `json:"questions_truncated,omitempty"`
}

type steeringQuestion struct {
	Header      string   `json:"header"`
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	MultiSelect bool     `json:"multi_select"`
}

// Canonical v1 wire structs. Field order is the encoded order.
type steeringSnapshot struct {
	Version         int    `json:"version"`
	CutoffInputID   int64  `json:"cutoff_input_id"`
	OwnerEntries    []any  `json:"owner_entries"`
	QuestionContext []any  `json:"question_context"`
	OmittedEntries  int64  `json:"omitted_entries"`
	Overflow        bool   `json:"overflow"`
	OverflowReason  string `json:"overflow_reason"`
}

type steeringAnswerEntry struct {
	InputID       int64     `json:"input_id"`
	Kind          string    `json:"kind"`
	QuestionID    string    `json:"question_id"`
	AnswersStatus string    `json:"answers_status"`
	Answers       *[]string `json:"answers"`
	Truncated     bool      `json:"truncated"`
}

type steeringReviseEntry struct {
	InputID      int64   `json:"input_id"`
	Kind         string  `json:"kind"`
	GateBinding  *string `json:"gate_binding"`
	GateRevision *int64  `json:"gate_revision"`
	Feedback     *string `json:"feedback"`
	Truncated    bool    `json:"truncated"`
}

type steeringBoundContext struct {
	AnswerInputID int64              `json:"answer_input_id"`
	QuestionID    string             `json:"question_id"`
	Status        string             `json:"status"`
	Author        string             `json:"author"`
	Questions     []steeringQuestion `json:"questions"`
	Truncated     bool               `json:"truncated"`
}

type steeringUnboundContext struct {
	AnswerInputID int64  `json:"answer_input_id"`
	QuestionID    string `json:"question_id"`
	Status        string `json:"status"`
}

// steeringUnit is one kept entry with its question context, so the byte cap can drop an
// entry and its context together.
type steeringUnit struct {
	entry   any
	context any // nil for a revise entry
}

// cutUTF8 returns s cut to at most n bytes at a UTF-8 boundary.
func cutUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// cutRunes returns s cut to at most n characters (runes).
func cutRunes(s string, n int) (string, bool) {
	if n < 0 {
		n = 0
	}
	i := 0
	for count := 0; i < len(s); count++ {
		if count == n {
			return s[:i], true
		}
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return s, false
}

// scrubCut scrubs first, then applies the field's character cap and the remaining byte budget.
// The query delivers the field with a margin beyond the character cap, so a secret that straddles
// the cap is whole when scrubbed and no token prefix survives the cut.
func scrubCut(s string, chars int, budget *int) (string, bool) {
	scrubbed, cutChars := cutRunes(secretscrub.Scrub(s), chars)
	out, cutBytes := cutUTF8(scrubbed, *budget)
	*budget -= len(out)
	return out, cutChars || cutBytes
}

func steeringAuthor(agent string) string {
	if agent == "lead" {
		return "lead"
	}
	// Only the lead and the worker emit questions; anything else is labelled as the worker so the
	// field never claims an owner voice and never echoes arbitrary stored text.
	return "worker"
}

func buildSteeringUnit(r ownerSteeringRow) (steeringUnit, bool) {
	if r.Kind == "revise_plan" {
		e := steeringReviseEntry{InputID: r.InputID, Kind: "revise", GateBinding: r.GateBinding, GateRevision: r.GateRevision, Truncated: r.ReviseTruncated}
		if r.ReviseBody != nil {
			budget := ownerSteeringReviseBytes
			text, cut := scrubCut(*r.ReviseBody, ownerSteeringReviseChars, &budget)
			e.Feedback = &text
			e.Truncated = e.Truncated || cut
		}
		return steeringUnit{entry: e}, true
	}
	if r.Kind != "answer" {
		return steeringUnit{}, false
	}
	e := steeringAnswerEntry{InputID: r.InputID, Kind: "answer", QuestionID: secretscrub.Scrub(r.QuestionID), AnswersStatus: r.AnswersStatus}
	if r.AnswersStatus == "ok" {
		budget := ownerSteeringAnswerBytes
		answers := make([]string, 0, len(r.Answers))
		truncated := r.AnswersTruncated
		for _, a := range r.Answers {
			text, cut := scrubCut(a, ownerSteeringAnswerChars, &budget)
			truncated = truncated || cut
			answers = append(answers, text)
		}
		e.Answers = &answers
		e.Truncated = truncated
	}
	return steeringUnit{entry: e, context: buildSteeringContext(r, e.QuestionID)}, true
}

func buildSteeringContext(r ownerSteeringRow, questionID string) any {
	status := func(s string) any {
		return steeringUnboundContext{AnswerInputID: r.InputID, QuestionID: questionID, Status: s}
	}
	switch {
	case r.Occurrences == 0:
		return status("unavailable")
	case r.Variants > 1:
		return status("ambiguous")
	case r.QuestionsStatus == "malformed" || r.QuestionsStatus == "oversized":
		return status(r.QuestionsStatus)
	case r.QuestionsStatus != "ok":
		return status("malformed")
	}
	c := steeringBoundContext{AnswerInputID: r.InputID, QuestionID: questionID, Status: "bound",
		Author: steeringAuthor(r.QuestionAgent), Questions: []steeringQuestion{}, Truncated: r.QuestionsTruncated}
	budget := ownerSteeringContextBytes
	for _, q := range r.Questions {
		if budget <= 0 {
			c.Truncated = true
			break
		}
		var cut, anyCut bool
		out := steeringQuestion{Options: []string{}, MultiSelect: q.MultiSelect}
		out.Header, cut = scrubCut(q.Header, ownerSteeringHeaderChars, &budget)
		anyCut = anyCut || cut
		out.Question, cut = scrubCut(q.Question, ownerSteeringQuestionChars, &budget)
		anyCut = anyCut || cut
		for _, o := range q.Options {
			if budget <= 0 {
				anyCut = true
				break
			}
			var label string
			label, cut = scrubCut(o, ownerSteeringOptionChars, &budget)
			anyCut = anyCut || cut
			out.Options = append(out.Options, label)
		}
		c.Truncated = c.Truncated || anyCut
		c.Questions = append(c.Questions, out)
	}
	return c
}

func marshalSteeringSnapshot(cutoff int64, units []steeringUnit, omitted int64, reason string) ([]byte, error) {
	snap := steeringSnapshot{Version: ownerSteeringVersion, CutoffInputID: cutoff, OwnerEntries: []any{}, QuestionContext: []any{},
		OmittedEntries: omitted, Overflow: reason != "", OverflowReason: reason}
	for _, u := range units {
		snap.OwnerEntries = append(snap.OwnerEntries, u.entry)
		if u.context != nil {
			snap.QuestionContext = append(snap.QuestionContext, u.context)
		}
	}
	return json.Marshal(snap)
}

// buildOwnerSteeringSnapshot is a pure function from the read rows to the canonical v1 JSON
// bytes. Entries stay ascending by input id; the oldest are dropped, with their context, until
// the snapshot fits ownerSteeringSnapshotMaxBytes.
func buildOwnerSteeringSnapshot(in ownerSteeringInput) ([]byte, error) {
	rows := append([]ownerSteeringRow(nil), in.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].InputID < rows[j].InputID })
	units := make([]steeringUnit, 0, len(rows))
	for _, r := range rows {
		if u, ok := buildSteeringUnit(r); ok {
			units = append(units, u)
		}
	}
	omitted := in.Total - int64(len(in.Rows))
	if omitted < 0 {
		omitted = 0
	}
	reason := ""
	if omitted > 0 {
		reason = steeringOverflowEntryCap
	}
	for {
		raw, err := marshalSteeringSnapshot(in.Cutoff, units, omitted, reason)
		if err != nil {
			return nil, err
		}
		if len(raw) <= ownerSteeringSnapshotMaxBytes {
			return raw, nil
		}
		if len(units) == 0 {
			return nil, errors.New("owner steering snapshot exceeds its byte cap with no entries")
		}
		units = units[1:]
		omitted++
		reason = steeringOverflowByteCap
	}
}

// ownerSteeringInputFromRows maps the SnapshotOwnerSteering rows to the builder input. The
// statement always returns the cutoff and total, even with zero kept rows (then InputID is null).
func ownerSteeringInputFromRows(rows []store.SnapshotOwnerSteeringRow) (ownerSteeringInput, error) {
	var in ownerSteeringInput
	if len(rows) == 0 {
		return in, errors.New("owner steering read returned no cut row")
	}
	in.Cutoff, in.Total = rows[0].Cutoff, rows[0].Total
	for _, r := range rows {
		if !r.InputID.Valid {
			continue
		}
		row := ownerSteeringRow{InputID: r.InputID.Int64, Kind: r.InputKind.String, QuestionID: r.QuestionID.String,
			AnswersStatus: r.AnswersStatus.String, AnswersTruncated: r.AnswersTruncated.Bool,
			Occurrences: r.Occurrences.Int64, Variants: r.Variants.Int64,
			QuestionAgent: r.QuestionAgent.String, QuestionsStatus: r.QuestionsStatus.String, QuestionsTruncated: r.QuestionsTruncated.Bool,
			ReviseTruncated: r.ReviseTruncated.Bool}
		if r.GateBinding.Valid {
			b := r.GateBinding.String
			row.GateBinding = &b
		}
		if r.GateRevision.Valid {
			v := r.GateRevision.Int64
			row.GateRevision = &v
		}
		if r.InputKind.String == "revise_plan" && r.ReviseBody.Valid {
			b := r.ReviseBody.String
			row.ReviseBody = &b
		}
		if len(r.Answers) > 0 {
			if err := json.Unmarshal(r.Answers, &row.Answers); err != nil {
				return in, errors.New("owner steering answers projection is not a string array")
			}
		}
		if len(r.Questions) > 0 {
			if err := json.Unmarshal(r.Questions, &row.Questions); err != nil {
				return in, errors.New("owner steering questions projection is malformed")
			}
		}
		in.Rows = append(in.Rows, row)
	}
	return in, nil
}

// freezeOwnerSteering snapshots the lead's owner steering and stores it for the just-created
// plan cross-check round. It must run in the round-creating transaction, under the lead lock;
// any failure aborts the round.
func freezeOwnerSteering(ctx context.Context, q *store.Queries, leadID, crossCheckID uuid.UUID) error {
	rows, err := q.SnapshotOwnerSteering(ctx, store.SnapshotOwnerSteeringParams{RunID: leadID, MaxEntries: ownerSteeringMaxEntries})
	if err != nil {
		return err
	}
	in, err := ownerSteeringInputFromRows(rows)
	if err != nil {
		return err
	}
	snapshot, err := buildOwnerSteeringSnapshot(in)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(snapshot)
	n, err := q.InsertOwnerSteering(ctx, store.InsertOwnerSteeringParams{CrossCheckID: crossCheckID, Snapshot: string(snapshot), Digest: digest[:]})
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCrossCheckRefused
	}
	return nil
}

// ClaimOwnerSteering is the frozen owner-steering snapshot a plan-checker child receives beside
// its candidate. Snapshot is the stored text, delivered unchanged; Digest is the hex sha256 of
// its bytes.
type ClaimOwnerSteering struct {
	Snapshot string `json:"snapshot"`
	Digest   string `json:"digest"`
}

// verifyOwnerSteering checks the size, UTF-8 validity and digest of a steering payload.
func verifyOwnerSteering(s *ClaimOwnerSteering) error {
	if s == nil || len(s.Snapshot) > ownerSteeringSnapshotMaxBytes || !utf8.ValidString(s.Snapshot) {
		return ErrCrossCheckRefused
	}
	want, err := hex.DecodeString(s.Digest)
	sum := sha256.Sum256([]byte(s.Snapshot))
	if err != nil || len(want) != sha256.Size || hex.EncodeToString(sum[:]) != s.Digest {
		return ErrCrossCheckRefused
	}
	return nil
}
