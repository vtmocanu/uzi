package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PublicationResumeFeature negotiates the write-ahead publication intent (issue #1324).
const PublicationResumeFeature = capability.PublicationResumeV1

// MaxPublicationIntentBytes bounds the opaque intent body. The intent column is json, which stores
// the posted bytes verbatim, so the table CHECK (octet_length(intent::text) <= 65536) is the same
// bound.
const MaxPublicationIntentBytes = 64 * 1024

var (
	// ErrPublicationResumeDisabled refuses NEW enrollment while RUN_PUBLICATION_RESUME_ENABLED is off.
	ErrPublicationResumeDisabled = errors.New("publication resume is disabled")
	// ErrPublicationIntentMismatch is an identity or candidate_seq disagreement (409).
	ErrPublicationIntentMismatch = errors.New("publication_intent_mismatch")
	// ErrPublicationEnrollmentRetired refuses a write against a non-active enrollment (409).
	ErrPublicationEnrollmentRetired = errors.New("publication_enrollment_retired")
	// ErrPublicationEnrollmentChanged is a state write that found the run enrolled only after it
	// took the run lock, without the worker lock the enrolled lock order needs. Retryable.
	ErrPublicationEnrollmentChanged = errors.New("publication_enrollment_changed")
)

var gitOIDHexRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

var (
	// ErrPublicationResumeUnsupported is a worker row that does not hold publication_resume_v1 (400).
	ErrPublicationResumeUnsupported = errors.New("publication_resume_unsupported")
	// ErrPublicationKindUnsupported is a run kind that never publishes code (400).
	ErrPublicationKindUnsupported = errors.New("publication_kind_unsupported")
)

// publicationKinds are the run kinds that reach code publication. It mirrors CODE_PUBLISHING_KINDS
// in agent/src/recovery.ts.
var publicationKinds = map[string]struct{}{
	runkind.Issue: {}, runkind.CIFix: {}, runkind.SelfImprove: {}, runkind.Prompt: {},
	runkind.Task: {}, runkind.MRRework: {},
}

// publicationIntentKeys is the allowlist of top-level intent members. Nested objects (source,
// mr, guard, staged_description) stay opaque but count toward the size bound.
var publicationIntentKeys = map[string]struct{}{
	"v": {}, "run_id": {}, "repo_id": {}, "branch": {}, "claim_generation": {}, "enrollment_seq": {},
	"original_head": {}, "floor": {}, "candidate": {}, "candidate_seq": {}, "source": {}, "mr": {},
	"guard": {}, "staged_description": {}, "mac": {},
}

// strictObjectMembers splits a JSON object into its members. It refuses a duplicate key, a key
// outside allowed (so a case variant of a known field is refused: Go's struct decoding matches
// member names case-insensitively, while a consumer reading the stored json text matches exactly),
// and anything but one object.
func strictObjectMembers(raw []byte, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, ErrInvalidState
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, ErrInvalidState
		}
		if _, known := allowed[key]; !known {
			return nil, ErrInvalidState
		}
		if _, dup := out[key]; dup {
			return nil, ErrInvalidState
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, ErrInvalidState
		}
		out[key] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, ErrInvalidState
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidState
	}
	return out, nil
}

const (
	// maxIntentNumberLiteral bounds one number literal in the intent. The store keeps the posted
	// text verbatim (json column), but no legitimate member needs more than this many characters.
	maxIntentNumberLiteral = 64
	// maxIntentDepth bounds nesting so a deeply nested body cannot exhaust a parser's stack.
	maxIntentDepth = 32
)

// checkIntentDocument walks the whole intent once: it refuses an over-long number literal (a
// 1e131071 literal is tiny on the wire and large once a numeric type expands it), nesting past
// maxIntentDepth, invalid UTF-8, and a \u escape that is NUL or a lone surrogate (PostgreSQL's
// json functions refuse the first and the Go decoder silently rewrites the second).
func checkIntentDocument(raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrInvalidState
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ErrInvalidState
		}
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' || v == '[' {
				if depth++; depth > maxIntentDepth {
					return ErrInvalidState
				}
			} else {
				depth--
			}
		case json.Number:
			if len(v) > maxIntentNumberLiteral {
				return ErrInvalidState
			}
		}
	}
	return checkIntentEscapes(raw)
}

// checkIntentEscapes refuses \u0000 and an unpaired surrogate escape. raw is already known to be
// well-formed JSON, so a backslash only occurs inside a string and always starts an escape.
func checkIntentEscapes(raw []byte) error {
	hex4 := func(i int) (rune, bool) {
		if i+4 > len(raw) {
			return 0, false
		}
		var r rune
		for _, c := range raw[i : i+4] {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if i+1 >= len(raw) {
			return ErrInvalidState
		}
		if raw[i+1] != 'u' {
			i++ // skip the escaped character, so an escaped backslash is not an escape start
			continue
		}
		r, ok := hex4(i + 2)
		if !ok || r == 0 {
			return ErrInvalidState
		}
		i += 5
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return ErrInvalidState
		case r >= 0xD800 && r <= 0xDBFF:
			if i+2 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return ErrInvalidState
			}
			lo, ok := hex4(i + 3)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return ErrInvalidState
			}
			i += 6
		}
	}
	return nil
}

// canonicalUUID parses a lowercase, hyphenated, 36-character UUID string and nothing else, so the
// Go identity is byte-equal to the text the database compares (run_id::text).
func canonicalUUID(raw json.RawMessage) (uuid.UUID, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || len(s) != 36 || s != strings.ToLower(s) {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil || id.String() != s {
		return uuid.Nil, false
	}
	return id, true
}

// validBranchName refuses empty, over-long, invalid UTF-8 and control or format (ANSI, bidi,
// zero-width) characters.
func validBranchName(s string) bool {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

// PublicationIntentRequest is the body of POST /api/worker/runs/{id}/publication-intent.
// EnrollmentSeq is absent for a new enrollment. CandidateSeq is the NEW candidate sequence on a
// candidate update: current+1 stores it, an exact repeat of the stored value with the same
// candidate is an idempotent success (a lost ACK), anything else is a mismatch. Intent is opaque
// to the server beyond the validated fields; the server never reads a URL or path from it.
type PublicationIntentRequest struct {
	ClaimGeneration int64           `json:"claim_generation"`
	EnrollmentSeq   *int32          `json:"enrollment_seq,omitempty"`
	CandidateSeq    *int32          `json:"candidate_seq,omitempty"`
	Intent          json.RawMessage `json:"intent"`
}

// PublicationIntentResponse acknowledges a stored (or already stored) intent.
type PublicationIntentResponse struct {
	EnrollmentSeq int32  `json:"enrollment_seq"`
	CandidateSeq  int32  `json:"candidate_seq"`
	State         string `json:"state"`
}

// PublicationResumePayload is the claim's publication-only resume input. Sent only to a
// publication_resume_v1 worker while an active enrollment exists.
type PublicationResumePayload struct {
	EnrollmentSeq int32           `json:"enrollment_seq"`
	CandidateSeq  int32           `json:"candidate_seq"`
	Intent        json.RawMessage `json:"intent"`
	Handoff       json.RawMessage `json:"handoff"`
}

type publicationIntentFloor struct {
	Kind string  `json:"kind"`
	Oid  *string `json:"oid,omitempty"`
}

// publicationIntentBody is the validated subset of the opaque intent.
type publicationIntentBody struct {
	RunID           uuid.UUID              `json:"run_id"`
	RepoID          uuid.UUID              `json:"repo_id"`
	Branch          string                 `json:"branch"`
	ClaimGeneration int64                  `json:"claim_generation"`
	OriginalHead    string                 `json:"original_head"`
	Floor           publicationIntentFloor `json:"floor"`
	Candidate       *string                `json:"candidate"`
}

func parsePublicationIntent(raw json.RawMessage) (publicationIntentBody, error) {
	var b publicationIntentBody
	if len(raw) == 0 || len(raw) > MaxPublicationIntentBytes {
		return b, ErrInvalidState
	}
	// Only exact, unique, allowlisted top-level keys reach the typed decode below, so the typed
	// view and the stored json read the same members. The raw bytes are stored unchanged.
	probe, err := strictObjectMembers(raw, publicationIntentKeys)
	if err != nil {
		return b, err
	}
	if err := checkIntentDocument(raw); err != nil {
		return b, err
	}
	// Required members must be present, not merely zero.
	for _, k := range []string{"run_id", "repo_id", "branch", "claim_generation", "original_head", "floor", "candidate"} {
		if _, ok := probe[k]; !ok {
			return b, ErrInvalidState
		}
	}
	floor, err := strictObjectMembers(probe["floor"], map[string]struct{}{"kind": {}, "oid": {}})
	if err != nil {
		return b, err
	}
	for field, dst := range map[string]any{
		"branch": &b.Branch, "claim_generation": &b.ClaimGeneration,
		"original_head": &b.OriginalHead, "candidate": &b.Candidate,
	} {
		if err := json.Unmarshal(probe[field], dst); err != nil {
			return b, ErrInvalidState
		}
	}
	var okRun, okRepo bool
	b.RunID, okRun = canonicalUUID(probe["run_id"])
	b.RepoID, okRepo = canonicalUUID(probe["repo_id"])
	if !okRun || !okRepo {
		return b, ErrInvalidState
	}
	if err := json.Unmarshal(probe["floor"], &b.Floor); err != nil {
		return b, ErrInvalidState
	}
	if !validBranchName(b.Branch) || b.ClaimGeneration <= 0 || !gitOIDHexRe.MatchString(b.OriginalHead) {
		return b, ErrInvalidState
	}
	// The floor is exactly {"kind":"absent"} or {"kind":"pinned","oid":"<40 hex>"}: an oid member
	// on an absent floor (even null) is refused, so the Go identity equals the database's.
	switch b.Floor.Kind {
	case "pinned":
		if len(floor) != 2 || b.Floor.Oid == nil || !gitOIDHexRe.MatchString(*b.Floor.Oid) {
			return b, ErrInvalidState
		}
	case "absent":
		if len(floor) != 1 || b.Floor.Oid != nil {
			return b, ErrInvalidState
		}
	default:
		return b, ErrInvalidState
	}
	if b.Candidate != nil && !gitOIDHexRe.MatchString(*b.Candidate) {
		return b, ErrInvalidState
	}
	return b, nil
}

// sameIdentity compares every identity field of two validated intents. The candidate is not
// identity: it advances under candidate_seq.
func (b publicationIntentBody) sameIdentity(o publicationIntentBody) bool {
	if b.RunID != o.RunID || b.RepoID != o.RepoID || b.Branch != o.Branch ||
		b.ClaimGeneration != o.ClaimGeneration || b.OriginalHead != o.OriginalHead || b.Floor.Kind != o.Floor.Kind {
		return false
	}
	if (b.Floor.Oid == nil) != (o.Floor.Oid == nil) {
		return false
	}
	return b.Floor.Oid == nil || *b.Floor.Oid == *o.Floor.Oid
}

func (b publicationIntentBody) sameCandidate(o publicationIntentBody) bool {
	if (b.Candidate == nil) != (o.Candidate == nil) {
		return false
	}
	return b.Candidate == nil || *b.Candidate == *o.Candidate
}

// runFenceQueries is the statement surface every writer of a possibly-enrolled run needs.
type runFenceQueries interface {
	GetWorkerForUpdate(context.Context, uuid.UUID) (store.Worker, error)
	OptInRunFences(context.Context) error
}

// optInRunFences is the single opt-in for the run-row fences that authorize owner, generation
// and claim changes (checkpoint coordinates and publication resume). It locks the worker row
// FOR UPDATE unless the caller already holds it, then sets BOTH transaction-local opt-ins. The
// caller must already be inside the transaction the run write runs in, and must lock the worker
// before the run row. Setting the opt-ins is harmless for a run that is enrolled in neither.
func optInRunFences(ctx context.Context, q runFenceQueries, workerID uuid.UUID, workerLocked bool) error {
	if !workerLocked {
		if _, err := q.GetWorkerForUpdate(ctx, workerID); err != nil {
			return err
		}
	}
	return q.OptInRunFences(ctx)
}

// publicationEnrollmentReader is the statement surface that reads a run's active enrollment.
type publicationEnrollmentReader interface {
	GetActivePublicationResume(context.Context, uuid.UUID) (store.RunPublicationResume, error)
}

// publicationEnrolled reports whether the run has an active enrollment. Writers of authority
// columns call it BEFORE locking the run row, to decide whether the bound worker is locked first
// (lock order: worker, then run).
func publicationEnrolled(ctx context.Context, q publicationEnrollmentReader, runID uuid.UUID) (bool, error) {
	_, err := q.GetActivePublicationResume(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// recheckPublicationEnrollment runs after a writer took the run row lock. When the pre-lock read
// found no enrollment the worker row was not locked; an enrollment committed in between would
// make this write an authority write taken in the wrong lock order (run before worker), so it
// is refused with a retryable error and nothing is written. Enrollment holds the run lock until
// commit, so the read here sees every enrollment that committed before this transaction's lock.
func recheckPublicationEnrollment(ctx context.Context, q publicationEnrollmentReader, runID uuid.UUID, workerLocked bool) error {
	if workerLocked {
		return nil
	}
	now, err := publicationEnrolled(ctx, q, runID)
	if err != nil {
		return err
	}
	if now {
		return ErrPublicationEnrollmentChanged
	}
	return nil
}

// publicationResumePayload builds the claim member for a capable worker with an active
// enrollment; nil otherwise. It reads only the side table: claim assembly is otherwise unchanged.
func (s *Service) publicationResumePayload(ctx context.Context, wkr store.Worker, run store.Run) (*PublicationResumePayload, error) {
	if !slices.Contains(wkr.ProtocolCapabilities, PublicationResumeFeature) {
		return nil, nil
	}
	q, ok := s.q.(publicationEnrollmentReader)
	if !ok {
		// A capable worker must not silently lose its resume input.
		return nil, errors.New("publication resume store unavailable")
	}
	row, err := q.GetActivePublicationResume(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &PublicationResumePayload{EnrollmentSeq: row.EnrollmentSeq, CandidateSeq: row.CandidateSeq, Intent: json.RawMessage(row.Intent)}
	if len(row.Handoff) > 0 {
		out.Handoff = json.RawMessage(row.Handoff)
	}
	return out, nil
}

// RegisterPublicationIntent enrolls a run's publication intent or advances its candidate, in
// one transaction: worker lock, run lock, fences opt-in, then the side table. It validates the
// run is this worker's at the stated claim generation and running, and that the worker holds the
// capability. NEW enrollment needs the feature switch; an existing enrollment stays writable.
func (s *Service) RegisterPublicationIntent(ctx context.Context, w store.Worker, runID uuid.UUID, req PublicationIntentRequest) (PublicationIntentResponse, error) {
	var zero PublicationIntentResponse
	if req.ClaimGeneration <= 0 {
		return zero, ErrInvalidState
	}
	body, err := parsePublicationIntent(req.Intent)
	if err != nil {
		return zero, err
	}
	// The intent's claim_generation is the ENROLLMENT generation, an immutable identity field.
	// A new enrollment is made at the live generation; a candidate update may come from a later
	// claim of the same run, whose live fence is req.ClaimGeneration against runs.claim_generation.
	if body.RunID != runID || body.ClaimGeneration > req.ClaimGeneration ||
		(req.EnrollmentSeq == nil && body.ClaimGeneration != req.ClaimGeneration) {
		return zero, ErrInvalidState
	}
	if (req.EnrollmentSeq == nil) != (req.CandidateSeq == nil) {
		return zero, ErrInvalidState
	}
	if req.EnrollmentSeq != nil && (*req.EnrollmentSeq <= 0 || *req.CandidateSeq <= 0) {
		return zero, ErrInvalidState
	}
	if s.txBeginner == nil {
		return zero, errors.New("publication intent transaction unavailable")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err = q.LockForgeOriginWorker(ctx, store.LockForgeOriginWorkerParams{RunID: runID, WorkerID: w.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrRunNotOwned
		}
		return zero, err
	}
	current, err := q.GetWorkerByID(ctx, w.ID)
	if err != nil {
		return zero, err
	}
	if current.UserID != w.UserID {
		return zero, ErrRunNotOwned
	}
	if !slices.Contains(current.ProtocolCapabilities, PublicationResumeFeature) {
		return zero, ErrPublicationResumeUnsupported
	}
	if err = optInRunFences(ctx, q, w.ID, true); err != nil {
		return zero, err
	}
	run, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(w.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrRunNotOwned
		}
		return zero, err
	}
	if run.UserID != w.UserID {
		return zero, ErrRunNotOwned
	}
	// The live fence: this worker's run, at the generation the request states, claim held, running.
	if run.ClaimGeneration != req.ClaimGeneration || run.ClaimReleasedAt.Valid || run.Status != "running" {
		return zero, ErrStaleClaim
	}
	if _, ok := publicationKinds[run.Kind]; !ok {
		return zero, ErrPublicationKindUnsupported
	}
	if !run.RepoID.Valid || uuid.UUID(run.RepoID.Bytes) != body.RepoID ||
		!run.Branch.Valid || run.Branch.String == "" || run.Branch.String != body.Branch {
		return zero, ErrInvalidState
	}
	resp, err := s.applyPublicationIntent(ctx, q, run, req, body)
	if err != nil {
		// Client errors: 23514 a CHECK or guard refused the body; 22P05 an untranslatable
		// character; 22P02 malformed json text; 22003 numeric out of range; 22021 invalid byte
		// sequence for the encoding.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23514", "22P05", "22P02", "22003", "22021":
				return zero, ErrInvalidState
			}
		}
		return zero, err
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, err
	}
	return resp, nil
}

func (s *Service) applyPublicationIntent(ctx context.Context, q *store.Queries, run store.Run, req PublicationIntentRequest, body publicationIntentBody) (PublicationIntentResponse, error) {
	var zero PublicationIntentResponse
	reply := func(r store.RunPublicationResume) PublicationIntentResponse {
		return PublicationIntentResponse{EnrollmentSeq: r.EnrollmentSeq, CandidateSeq: r.CandidateSeq, State: r.State}
	}
	if req.EnrollmentSeq == nil {
		active, err := q.GetActivePublicationResume(ctx, run.ID)
		if err == nil {
			stored, perr := parsePublicationIntent(active.Intent)
			if perr != nil || !stored.sameIdentity(body) {
				return zero, ErrPublicationIntentMismatch
			}
			return reply(active), nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return zero, err
		}
		closed, err := q.HasInactivePublicationResumeAtGeneration(ctx, store.HasInactivePublicationResumeAtGenerationParams{
			RunID: run.ID, ClaimGeneration: req.ClaimGeneration,
		})
		if err != nil {
			return zero, err
		}
		if closed {
			return zero, ErrPublicationEnrollmentRetired
		}
		if !s.p.PublicationResume {
			return zero, ErrPublicationResumeDisabled
		}
		row, err := q.EnrollPublicationResume(ctx, store.EnrollPublicationResumeParams{
			RunID: run.ID, ClaimGeneration: req.ClaimGeneration, WorkerID: run.WorkerID.Bytes, Intent: req.Intent,
		})
		if err != nil {
			return zero, err
		}
		return reply(row), nil
	}
	row, err := q.GetPublicationResume(ctx, store.GetPublicationResumeParams{RunID: run.ID, EnrollmentSeq: *req.EnrollmentSeq})
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrPublicationIntentMismatch
	}
	if err != nil {
		return zero, err
	}
	if row.State != "active" {
		return zero, ErrPublicationEnrollmentRetired
	}
	stored, perr := parsePublicationIntent(row.Intent)
	if perr != nil || !stored.sameIdentity(body) {
		return zero, ErrPublicationIntentMismatch
	}
	want := *req.CandidateSeq
	switch want {
	case row.CandidateSeq:
		// A repeat of the stored sequence is a lost ACK only when it carries the same candidate.
		if !stored.sameCandidate(body) {
			return zero, ErrPublicationIntentMismatch
		}
		return reply(row), nil
	case row.CandidateSeq + 1:
		updated, err := q.UpdatePublicationCandidate(ctx, store.UpdatePublicationCandidateParams{
			RunID: run.ID, EnrollmentSeq: row.EnrollmentSeq, Intent: bytes.Clone(req.Intent),
			NewCandidateSeq: want, ExpectedCandidateSeq: row.CandidateSeq,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrPublicationIntentMismatch
		}
		if err != nil {
			return zero, err
		}
		return reply(updated), nil
	default:
		return zero, ErrPublicationIntentMismatch
	}
}
