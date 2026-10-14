// Package outcomeloss defines the canonical encoding, digest and validation of the loss manifest
// a worker reports for a blocked terminal outcome (issue #2984). The agent builds the same bytes
// with an explicit key-order builder and JSON.stringify; both sides are pinned to the shared
// fixture directory fixtures/outcome-discard/.
//
// Canonical form: the manifest's Go struct field order is the key order; arrays are sorted by
// their escaped bytes; integers are decimal; the JSON is encoded with HTML escaping off and no
// trailing newline. loss_sha256 = lowercase hex sha256("uzi-outcome-loss-v1\n" || canonical JSON).
package outcomeloss

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Version is the manifest version this package encodes.
const Version = 1

// DigestPrefix is hashed before the canonical JSON so a manifest digest can never equal another
// object's sha256.
const DigestPrefix = "uzi-outcome-loss-v1\n"

// Caps. A manifest over any of them is reported incomplete by the worker and refused here.
const (
	MaxPathEntries    = 4000
	MaxCommits        = 512
	MaxHashedBytes    = int64(256) << 20
	MaxCanonicalBytes = 768 << 10
	MaxOutcomesPerPut = 8
	MaxPutBodyBytes   = 6 << 20
	// MaxReportBytes bounds one outcome's raw JSON inside a PUT body: the canonical manifest cap
	// plus the report envelope. It keeps the transient decode of one item small even when a
	// hostile body is shaped as millions of tiny array elements.
	MaxReportBytes = MaxCanonicalBytes + 64<<10
	MaxClones      = 16
	MaxSiblings    = 1024
	// MaxSafeInt is the largest integer both Go and the TS worker represent exactly.
	MaxSafeInt = int64(1)<<53 - 1

	maxEscapedField = 16384
	maxEscapedShort = 4096
)

// BlockedReasons is the closed set of reasons a terminal journal can be blocked with, mirroring
// the agent's TerminalBlockedReason.
var BlockedReasons = []string{"completion_permit_mismatch", "gap_unrecoverable", "reserve_exhausted", "run_not_owned"}

// ReasonRunNotOwned is the only reason an owner may discard.
const ReasonRunNotOwned = "run_not_owned"

// Commit bases a clone's unpublished commits are computed against.
const (
	BasisWorkerBare           = "worker_bare_tracking_ref"
	BasisCloneRemoteTracking  = "clone_remote_tracking"
	SiblingKindJournal        = "journal"
	SiblingKindFinalize       = "finalize"
	SiblingDispositionKept    = "kept"
	PathKindFile              = "file"
	PathKindSymlink           = "symlink"
	PathKindDeleted           = "deleted"
	statusCharset             = " MADRCUT?!"
	completeReasonMaxBytes    = 64
	incompleteReasonMaxEscape = 512
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHex64 reports whether s is exactly 64 lowercase hex characters.
func ValidHex64(s string) bool { return hex64.MatchString(s) }

func validCommit(s string) bool {
	if len(s) != 40 && len(s) != 64 {
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

// Escape renders raw bytes in the escaped-ASCII grammar: bytes 0x20-0x7E other than the
// backslash pass through, the backslash becomes `\\`, and every other byte becomes `\xNN`
// (lowercase hex).
func Escape(b []byte) string {
	const digits = "0123456789abcdef"
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c == '\\':
			sb.WriteString(`\\`)
		case c >= 0x20 && c <= 0x7e:
			sb.WriteByte(c)
		default:
			sb.WriteString(`\x`)
			sb.WriteByte(digits[c>>4])
			sb.WriteByte(digits[c&0xf])
		}
	}
	return sb.String()
}

// ValidEscaped reports whether s is a canonical escaped-ASCII string no longer than max bytes:
// only bytes 0x20-0x7E, a backslash only as `\\` or a lowercase `\xNN`, and no `\xNN` for a
// byte that Escape would pass through (so each byte string has exactly one spelling).
func ValidEscaped(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
		if c != '\\' {
			continue
		}
		if i+1 >= len(s) {
			return false
		}
		switch s[i+1] {
		case '\\':
			i++
		case 'x':
			if i+3 >= len(s) {
				return false
			}
			hi, ok1 := lowerHex(s[i+2])
			lo, ok2 := lowerHex(s[i+3])
			if !ok1 || !ok2 {
				return false
			}
			if v := hi<<4 | lo; v >= 0x20 && v <= 0x7e {
				return false
			}
			i += 3
		default:
			return false
		}
	}
	return true
}

func lowerHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid outcome loss manifest")

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks the manifest against the full grammar, ordering and cap rules. It does not
// check the encoded size; Canonical does.
func Validate(m *apitypes.OutcomeLossManifest) error {
	if m == nil {
		return bad("manifest missing")
	}
	if m.Version != Version {
		return bad("version %d", m.Version)
	}
	if !canonicalUUID(m.WorkerID) || !canonicalUUID(m.RunID) {
		return bad("worker_id or run_id is not a canonical uuid")
	}
	if !ValidEscaped(m.RunDir, maxEscapedShort) || m.RunDir == "" {
		return bad("run_dir")
	}
	if m.ClaimGeneration < 0 || m.ClaimGeneration > MaxSafeInt {
		return bad("claim_generation")
	}
	if !hex64.MatchString(m.JournalSHA256) {
		return bad("journal_sha256")
	}
	if !knownReason(m.BlockedReason) {
		return bad("blocked_reason")
	}
	o := m.Outcome
	if !ValidEscaped(o.Status, 64) || !ValidEscaped(o.Phase, 64) || !ValidEscaped(o.Branch, maxEscapedShort) ||
		!ValidEscaped(o.MRWebURL, 2048) || !ValidEscaped(o.FailOrigin, 128) {
		return bad("outcome string field")
	}
	if o.Head != "" && !validCommit(o.Head) {
		return bad("outcome head")
	}
	if o.MRIID != nil && (*o.MRIID < 0 || *o.MRIID > MaxSafeInt) {
		return bad("outcome mr_iid")
	}
	if m.Siblings == nil || m.Clones == nil {
		return bad("siblings and clones must be arrays")
	}
	if len(m.Siblings) > MaxSiblings || len(m.Clones) > MaxClones {
		return bad("too many siblings or clones")
	}
	prev := ""
	for i, s := range m.Siblings {
		if s.Kind != SiblingKindJournal && s.Kind != SiblingKindFinalize {
			return bad("sibling kind")
		}
		if s.Generation < 0 || s.Generation > MaxSafeInt || s.Disposition != SiblingDispositionKept {
			return bad("sibling generation or disposition")
		}
		if !ValidEscaped(s.Name, 1024) || s.Name == "" {
			return bad("sibling name")
		}
		if i > 0 && s.Name <= prev {
			return bad("siblings not sorted by name or duplicated")
		}
		prev = s.Name
	}
	var paths, commits int
	var hashed int64
	prevClone := ""
	for i, c := range m.Clones {
		if !ValidEscaped(c.Path, maxEscapedField) || c.Path == "" || !ValidEscaped(c.Branch, maxEscapedShort) {
			return bad("clone path or branch")
		}
		if i > 0 && c.Path <= prevClone {
			return bad("clones not sorted by path or duplicated")
		}
		prevClone = c.Path
		if c.Head != "" && !validCommit(c.Head) {
			return bad("clone head")
		}
		if c.CommitBasis != BasisWorkerBare && c.CommitBasis != BasisCloneRemoteTracking {
			return bad("commit_basis")
		}
		if c.UnpublishedCommits == nil || c.Paths == nil {
			return bad("clone arrays must not be null")
		}
		commits += len(c.UnpublishedCommits)
		paths += len(c.Paths)
		if commits > MaxCommits || paths > MaxPathEntries {
			return bad("commit or path cap exceeded")
		}
		for j, h := range c.UnpublishedCommits {
			if !validCommit(h) {
				return bad("unpublished commit id")
			}
			if j > 0 && h <= c.UnpublishedCommits[j-1] {
				return bad("unpublished commits not sorted or duplicated")
			}
		}
		for j, p := range c.Paths {
			if err := validPathItem(p); err != nil {
				return err
			}
			if j > 0 && p.Path <= c.Paths[j-1].Path {
				return bad("paths not sorted by path or duplicated")
			}
			hashed += p.Size
			if hashed > MaxHashedBytes {
				return bad("hashed byte cap exceeded")
			}
		}
	}
	return nil
}

func validPathItem(p apitypes.OutcomeLossPathItem) error {
	if !ValidEscaped(p.Path, maxEscapedField) || p.Path == "" || !ValidEscaped(p.OrigPath, maxEscapedField) {
		return bad("path or orig_path")
	}
	if len(p.Status) != 2 || !strings.ContainsRune(statusCharset, rune(p.Status[0])) || !strings.ContainsRune(statusCharset, rune(p.Status[1])) {
		return bad("path status")
	}
	if p.Size < 0 || p.Size > MaxSafeInt {
		return bad("path size")
	}
	if !ValidEscaped(p.LinkTarget, maxEscapedField) {
		return bad("link_target")
	}
	switch p.Kind {
	case PathKindFile:
		if !hex64.MatchString(p.SHA256) || p.LinkTarget != "" {
			return bad("file entry shape")
		}
	case PathKindSymlink:
		if !hex64.MatchString(p.SHA256) || p.LinkTarget == "" {
			return bad("symlink entry shape")
		}
	case PathKindDeleted:
		if p.SHA256 != "" || p.Size != 0 || p.LinkTarget != "" {
			return bad("deleted entry shape")
		}
	default:
		return bad("path kind")
	}
	return nil
}

func knownReason(r string) bool {
	for _, k := range BlockedReasons {
		if r == k {
			return true
		}
	}
	return false
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// Canonical validates m and returns its canonical JSON bytes (no trailing newline), refusing a
// manifest over MaxCanonicalBytes.
func Canonical(m *apitypes.OutcomeLossManifest) ([]byte, error) {
	if err := Validate(m); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(out) > MaxCanonicalBytes {
		return nil, bad("canonical manifest is %d bytes, over the %d cap", len(out), MaxCanonicalBytes)
	}
	return out, nil
}

// Digest returns the lowercase hex loss_sha256 of canonical JSON bytes.
func Digest(canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(DigestPrefix))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// Sum validates m and returns its canonical bytes and digest.
func Sum(m *apitypes.OutcomeLossManifest) ([]byte, string, error) {
	c, err := Canonical(m)
	if err != nil {
		return nil, "", err
	}
	return c, Digest(c), nil
}

// ValidateReport checks one reported outcome against the authenticated worker. A complete
// report must carry a valid manifest bound to the report's identity (worker, run, generation,
// journal sha, reason) whose recomputed digest equals loss_sha256; an incomplete report carries
// no manifest and no digest. It returns the recomputed digest ("" when incomplete).
func ValidateReport(workerID uuid.UUID, r *apitypes.BlockedOutcomeReport) (string, error) {
	if !canonicalUUID(r.RunID) {
		return "", bad("run_id")
	}
	if r.ClaimGeneration < 0 || r.ClaimGeneration > MaxSafeInt {
		return "", bad("claim_generation")
	}
	if !hex64.MatchString(r.JournalSHA256) {
		return "", bad("journal_sha256")
	}
	if !knownReason(r.BlockedReason) {
		return "", bad("blocked_reason")
	}
	if !ValidEscaped(r.IncompleteReason, incompleteReasonMaxEscape) {
		return "", bad("incomplete_reason")
	}
	if !r.LossComplete {
		if r.Loss != nil || r.LossSHA256 != "" || r.IncompleteReason == "" {
			return "", bad("an incomplete report carries a reason and no manifest or digest")
		}
		return "", nil
	}
	if r.IncompleteReason != "" || r.Loss == nil || !hex64.MatchString(r.LossSHA256) {
		return "", bad("a complete report carries a manifest and digest and no incomplete_reason")
	}
	m := r.Loss
	if m.WorkerID != workerID.String() || m.RunID != r.RunID || m.ClaimGeneration != r.ClaimGeneration ||
		m.JournalSHA256 != r.JournalSHA256 || m.BlockedReason != r.BlockedReason {
		return "", bad("manifest scope does not match the report")
	}
	_, sum, err := Sum(m)
	if err != nil {
		return "", err
	}
	if sum != r.LossSHA256 {
		return "", bad("loss_sha256 does not match the manifest")
	}
	return sum, nil
}

// ValidReasonCode reports whether s is a closed machine reason: 1..64 bytes of [a-z0-9_].
func ValidReasonCode(s string) bool {
	if len(s) == 0 || len(s) > completeReasonMaxBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}
