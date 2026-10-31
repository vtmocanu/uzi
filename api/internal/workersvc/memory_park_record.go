package workersvc

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The memory-park settlement record (issue #3639) is the cooperative record the worker persists and
// posts as its settlement receipt. This file is the server's strict v1 decoder, mirroring the worker's
// parseMemoryPark (agent/src/git.ts) key for key. Deliberate server-side limits, each a 400:
//   - lone UTF-16 surrogates in any string are rejected explicitly by scanning the raw JSON escapes
//     (Go's decoder would silently substitute U+FFFD, and JS accepts them); the worker's own
//     serializer emits well-formed text, so this only narrows eligibility;
//   - numbers must be plain base-10 integers inside the safe-integer range (no fraction, exponent or -0);
//   - the raw bytes must be valid UTF-8, one JSON value, no trailing data, nested at most memoryParkMaxDepth.

const (
	memoryParkMaxDepth         = 16
	memoryParkMaxSources       = 16
	memoryParkMaxInterventions = 64
	maxSafeInteger             = 1<<53 - 1
	// MaxMemoryParkSettlementBytes caps a receipt body and the stored bytea (migration 00388).
	MaxMemoryParkSettlementBytes = 524288
)

var (
	errMemoryParkRecord = fmt.Errorf("%w: invalid memory park record", ErrInvalidState)

	memoryParkUUIDRe        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	memoryParkHex64Re       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	memoryParkIncarnationRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	memoryParkAttemptIDRe   = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-(g[0-9]+|gx)-[0-9a-f]{16}$`)
	memoryParkIntRe         = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
)

type memoryParkBinding struct {
	RunID           string
	ClaimGeneration int64
	WorkerID        string
	RegisterNonce   string
	MemoryEpisode   int64
}

type memoryParkOrigin struct {
	FlightID             string
	AttemptID            *string
	ExecutorIncarnation  *string
	AuthorityIncarnation string
	SourceEpoch          string
}

type memoryParkSource struct {
	Path         string
	AuthorityKey string
	AttemptID    *string
	Epoch        string
	Seq          int64
}

type memoryParkEvidence struct {
	ParkAck         memoryParkBinding
	InterventionIDs []string
	ProcessJoin     string
	Commands        string
	Steering        string
	Auxiliaries     string
	Feedback        string
	SinkReceipts    string
	ParkReap        string
}

type memoryParkSettlement struct {
	Confirmed bool
	Sources   []memoryParkSource
	Evidence  memoryParkEvidence
}

type memoryParkRecord struct {
	Binding    memoryParkBinding
	Ack        string
	Origin     memoryParkOrigin
	Settlement *memoryParkSettlement
}

// ---- raw JSON walk -------------------------------------------------------------------------------

// pjNum is a number token kept as its raw text until a field reads it as an integer.
type pjNum string

type pjObject struct {
	keys []string
	vals map[string]any
}

type pjParser struct {
	b   []byte
	pos int
}

func (p *pjParser) ws() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *pjParser) value(depth int) (any, error) {
	if depth > memoryParkMaxDepth {
		return nil, errMemoryParkRecord
	}
	p.ws()
	if p.pos >= len(p.b) {
		return nil, errMemoryParkRecord
	}
	switch c := p.b[p.pos]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case bytes.HasPrefix(p.b[p.pos:], []byte("true")):
		p.pos += 4
		return true, nil
	case bytes.HasPrefix(p.b[p.pos:], []byte("false")):
		p.pos += 5
		return false, nil
	case bytes.HasPrefix(p.b[p.pos:], []byte("null")):
		p.pos += 4
		return nil, nil
	}
	return nil, errMemoryParkRecord
}

func (p *pjParser) number() (any, error) {
	start := p.pos
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			p.pos++
			continue
		}
		break
	}
	return pjNum(p.b[start:p.pos]), nil
}

func (p *pjParser) object(depth int) (any, error) {
	p.pos++ // {
	obj := &pjObject{vals: map[string]any{}}
	p.ws()
	if p.pos < len(p.b) && p.b[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.ws()
		if p.pos >= len(p.b) || p.b[p.pos] != '"' {
			return nil, errMemoryParkRecord
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		key := k.(string)
		if _, dup := obj.vals[key]; dup {
			return nil, errMemoryParkRecord // a duplicate key is never accepted, whatever its value
		}
		p.ws()
		if p.pos >= len(p.b) || p.b[p.pos] != ':' {
			return nil, errMemoryParkRecord
		}
		p.pos++
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		obj.keys = append(obj.keys, key)
		obj.vals[key] = v
		p.ws()
		if p.pos >= len(p.b) {
			return nil, errMemoryParkRecord
		}
		switch p.b[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, errMemoryParkRecord
		}
	}
}

func (p *pjParser) array(depth int) (any, error) {
	p.pos++ // [
	out := []any{}
	p.ws()
	if p.pos < len(p.b) && p.b[p.pos] == ']' {
		p.pos++
		return out, nil
	}
	for {
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.pos >= len(p.b) {
			return nil, errMemoryParkRecord
		}
		switch p.b[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, errMemoryParkRecord
		}
	}
}

func mpHex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

// str decodes one JSON string from its raw bytes. A \uXXXX escape in the surrogate range must be a
// high surrogate immediately followed by a \u low surrogate; a lone high or low surrogate is rejected
// here, explicitly, instead of being replaced by U+FFFD as encoding/json would.
func (p *pjParser) str() (any, error) {
	p.pos++ // opening quote
	var sb strings.Builder
	for {
		if p.pos >= len(p.b) {
			return nil, errMemoryParkRecord
		}
		c := p.b[p.pos]
		switch {
		case c == '"':
			p.pos++
			return sb.String(), nil
		case c < 0x20:
			return nil, errMemoryParkRecord
		case c == '\\':
			if p.pos+1 >= len(p.b) {
				return nil, errMemoryParkRecord
			}
			e := p.b[p.pos+1]
			p.pos += 2
			switch e {
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				r, ok := mpHex4(p.b[p.pos:])
				if !ok {
					return nil, errMemoryParkRecord
				}
				p.pos += 4
				switch {
				case r >= 0xDC00 && r <= 0xDFFF:
					return nil, errMemoryParkRecord // lone low surrogate
				case r >= 0xD800 && r <= 0xDBFF:
					if p.pos+1 >= len(p.b) || p.b[p.pos] != '\\' || p.b[p.pos+1] != 'u' {
						return nil, errMemoryParkRecord // lone high surrogate
					}
					lo, ok := mpHex4(p.b[p.pos+2:])
					if !ok || lo < 0xDC00 || lo > 0xDFFF {
						return nil, errMemoryParkRecord // high surrogate without its low half
					}
					p.pos += 6
					sb.WriteRune(utf16.DecodeRune(r, lo))
				default:
					sb.WriteRune(r)
				}
			default:
				return nil, errMemoryParkRecord
			}
		default:
			sb.WriteByte(c) // raw UTF-8 was validated up front
			p.pos++
		}
	}
}

// ---- schema --------------------------------------------------------------------------------------

func mpKeys(v any, required []string, optional ...string) (*pjObject, error) {
	o, ok := v.(*pjObject)
	if !ok {
		return nil, errMemoryParkRecord
	}
	for _, k := range required {
		if _, ok := o.vals[k]; !ok {
			return nil, errMemoryParkRecord
		}
	}
	for _, k := range o.keys {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			return nil, errMemoryParkRecord
		}
	}
	return o, nil
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func mpString(v any, max int) (string, error) {
	s, ok := v.(string)
	if !ok || strings.ContainsRune(s, 0) {
		return "", errMemoryParkRecord
	}
	if n := utf16Len(s); n < 1 || n > max {
		return "", errMemoryParkRecord
	}
	return s, nil
}

func mpMatch(v any, re *regexp.Regexp) (string, error) {
	s, ok := v.(string)
	if !ok || !re.MatchString(s) {
		return "", errMemoryParkRecord
	}
	return s, nil
}

func mpOptMatch(v any, re *regexp.Regexp) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s, err := mpMatch(v, re)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func mpInt(v any, min int64) (int64, error) {
	n, ok := v.(pjNum)
	if !ok || !memoryParkIntRe.MatchString(string(n)) || string(n) == "-0" {
		return 0, errMemoryParkRecord
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || i < min || i > maxSafeInteger || i < -maxSafeInteger {
		return 0, errMemoryParkRecord
	}
	return i, nil
}

func mpOneOf(v any, allowed ...string) (string, error) {
	s, ok := v.(string)
	if !ok || !slices.Contains(allowed, s) {
		return "", errMemoryParkRecord
	}
	return s, nil
}

func mpBinding(v any) (memoryParkBinding, error) {
	o, err := mpKeys(v, []string{"runId", "claimGeneration", "workerId", "registerNonce", "memoryEpisode"})
	if err != nil {
		return memoryParkBinding{}, err
	}
	var b memoryParkBinding
	if b.RunID, err = mpString(o.vals["runId"], 128); err != nil {
		return b, err
	}
	if b.ClaimGeneration, err = mpInt(o.vals["claimGeneration"], 1); err != nil {
		return b, err
	}
	if b.WorkerID, err = mpString(o.vals["workerId"], 128); err != nil {
		return b, err
	}
	if b.RegisterNonce, err = mpString(o.vals["registerNonce"], 256); err != nil {
		return b, err
	}
	b.MemoryEpisode, err = mpInt(o.vals["memoryEpisode"], 0)
	return b, err
}

// compareUTF16 orders two strings by UTF-16 code unit, the order JavaScript's string `<` uses and the
// worker's sort of source paths. It differs from Go's byte order for U+E000..U+FFFF against non-BMP text.
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// decodeMemoryParkRecord is the strict v1 decode of one record: exactly one JSON value spelled in
// valid UTF-8, the exact case-sensitive key set at every level, no duplicate key, and every bound of
// the worker's parseMemoryPark. The caller applies the receipt rules on top.
func decodeMemoryParkRecord(raw []byte) (memoryParkRecord, error) {
	var rec memoryParkRecord
	if len(raw) == 0 || len(raw) > MaxMemoryParkSettlementBytes || !utf8.Valid(raw) {
		return rec, errMemoryParkRecord
	}
	p := &pjParser{b: raw}
	v, err := p.value(0)
	if err != nil {
		return rec, err
	}
	p.ws()
	if p.pos != len(p.b) {
		return rec, errMemoryParkRecord // trailing data
	}
	o, err := mpKeys(v, []string{"version", "binding", "ack", "origin"}, "settlement")
	if err != nil {
		return rec, err
	}
	if n, err := mpInt(o.vals["version"], 1); err != nil || n != 1 {
		return rec, errMemoryParkRecord
	}
	if rec.Binding, err = mpBinding(o.vals["binding"]); err != nil {
		return rec, err
	}
	if rec.Ack, err = mpOneOf(o.vals["ack"], "confirmed", "sent_unconfirmed", "not_sent"); err != nil {
		return rec, err
	}
	org, err := mpKeys(o.vals["origin"], []string{"flightId", "attemptId", "executorIncarnation", "authorityIncarnation", "sourceEpoch"})
	if err != nil {
		return rec, err
	}
	if rec.Origin.FlightID, err = mpMatch(org.vals["flightId"], memoryParkUUIDRe); err != nil {
		return rec, err
	}
	if rec.Origin.AttemptID, err = mpOptMatch(org.vals["attemptId"], memoryParkAttemptIDRe); err != nil {
		return rec, err
	}
	if rec.Origin.ExecutorIncarnation, err = mpOptMatch(org.vals["executorIncarnation"], memoryParkIncarnationRe); err != nil {
		return rec, err
	}
	if rec.Origin.AuthorityIncarnation, err = mpMatch(org.vals["authorityIncarnation"], memoryParkUUIDRe); err != nil {
		return rec, err
	}
	if rec.Origin.SourceEpoch, err = mpMatch(org.vals["sourceEpoch"], memoryParkUUIDRe); err != nil {
		return rec, err
	}
	sv, has := o.vals["settlement"]
	if !has {
		return rec, nil
	}
	if rec.Ack != "confirmed" {
		return rec, errMemoryParkRecord
	}
	st, err := decodeMemoryParkSettlement(sv, rec.Binding)
	if err != nil {
		return rec, err
	}
	rec.Settlement = st
	return rec, nil
}

func decodeMemoryParkSettlement(v any, binding memoryParkBinding) (*memoryParkSettlement, error) {
	s, err := mpKeys(v, []string{"version", "confirmed", "sources", "evidence"})
	if err != nil {
		return nil, err
	}
	if n, err := mpInt(s.vals["version"], 1); err != nil || n != 1 {
		return nil, errMemoryParkRecord
	}
	out := &memoryParkSettlement{}
	confirmed, ok := s.vals["confirmed"].(bool)
	if !ok {
		return nil, errMemoryParkRecord
	}
	out.Confirmed = confirmed
	srcs, ok := s.vals["sources"].([]any)
	if !ok || len(srcs) < 1 || len(srcs) > memoryParkMaxSources {
		return nil, errMemoryParkRecord
	}
	for _, raw := range srcs {
		e, err := mpKeys(raw, []string{"path", "authorityKey", "attemptId", "epoch", "seq"})
		if err != nil {
			return nil, err
		}
		var src memoryParkSource
		if src.Path, err = mpString(e.vals["path"], 4096); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(src.Path, "/") || path.Clean(src.Path) != src.Path {
			return nil, errMemoryParkRecord
		}
		if src.AuthorityKey, err = mpMatch(e.vals["authorityKey"], memoryParkHex64Re); err != nil {
			return nil, err
		}
		if src.AttemptID, err = mpOptMatch(e.vals["attemptId"], memoryParkAttemptIDRe); err != nil {
			return nil, err
		}
		if src.Epoch, err = mpMatch(e.vals["epoch"], memoryParkUUIDRe); err != nil {
			return nil, err
		}
		if src.Seq, err = mpInt(e.vals["seq"], 1); err != nil {
			return nil, err
		}
		out.Sources = append(out.Sources, src)
	}
	keys := map[string]struct{}{}
	for i, src := range out.Sources {
		if i > 0 && compareUTF16(out.Sources[i-1].Path, src.Path) >= 0 {
			return nil, errMemoryParkRecord // strictly ascending in UTF-16 code-unit order
		}
		keys[src.AuthorityKey] = struct{}{}
	}
	if len(keys) != len(out.Sources) {
		return nil, errMemoryParkRecord
	}
	ev, err := mpKeys(s.vals["evidence"], []string{"parkAck", "interventionIds", "processJoin", "commands", "steering",
		"auxiliaries", "feedback", "sinkReceipts", "parkReap"})
	if err != nil {
		return nil, err
	}
	if out.Evidence.ParkAck, err = mpBinding(ev.vals["parkAck"]); err != nil {
		return nil, err
	}
	if out.Evidence.ParkAck != binding {
		return nil, errMemoryParkRecord
	}
	ids, ok := ev.vals["interventionIds"].([]any)
	if !ok || len(ids) > memoryParkMaxInterventions {
		return nil, errMemoryParkRecord
	}
	seen := map[string]struct{}{}
	out.Evidence.InterventionIDs = []string{}
	for _, raw := range ids {
		id, err := mpString(raw, 128)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[id]; dup {
			return nil, errMemoryParkRecord
		}
		seen[id] = struct{}{}
		out.Evidence.InterventionIDs = append(out.Evidence.InterventionIDs, id)
	}
	if out.Evidence.ProcessJoin, err = mpOneOf(ev.vals["processJoin"], "executor", "codex_disposal"); err != nil {
		return nil, err
	}
	if out.Evidence.Commands, err = mpOneOf(ev.vals["commands"], "confirmed", "not_required"); err != nil {
		return nil, err
	}
	if out.Evidence.Steering, err = mpOneOf(ev.vals["steering"], "joined"); err != nil {
		return nil, err
	}
	if out.Evidence.Auxiliaries, err = mpOneOf(ev.vals["auxiliaries"], "joined", "none"); err != nil {
		return nil, err
	}
	if out.Evidence.Feedback, err = mpOneOf(ev.vals["feedback"], "drained"); err != nil {
		return nil, err
	}
	if out.Evidence.SinkReceipts, err = mpOneOf(ev.vals["sinkReceipts"], "joined"); err != nil {
		return nil, err
	}
	if out.Evidence.ParkReap, err = mpOneOf(ev.vals["parkReap"], "complete", "not_applicable"); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeMemoryParkReceipt applies the receipt rules on top of the strict decode and returns the
// intervention ids as UUIDs for the history lookup. Any failure is errMemoryParkRecord (400).
func decodeMemoryParkReceipt(raw []byte, runID, workerID uuid.UUID) (memoryParkRecord, []uuid.UUID, error) {
	rec, err := decodeMemoryParkRecord(raw)
	if err != nil {
		return rec, nil, err
	}
	if rec.Ack != "confirmed" || rec.Settlement == nil || !rec.Settlement.Confirmed ||
		rec.Binding.RunID != runID.String() || rec.Binding.WorkerID != workerID.String() ||
		len(rec.Binding.RegisterNonce) > 128 {
		return rec, nil, errMemoryParkRecord
	}
	matches := 0
	for _, src := range rec.Settlement.Sources {
		if (src.AttemptID == nil) != (rec.Origin.AttemptID == nil) ||
			(src.AttemptID != nil && *src.AttemptID != *rec.Origin.AttemptID) {
			continue
		}
		matches++
		if src.Epoch != rec.Origin.SourceEpoch {
			return rec, nil, errMemoryParkRecord
		}
	}
	if matches != 1 {
		return rec, nil, errMemoryParkRecord
	}
	ids := make([]uuid.UUID, 0, len(rec.Settlement.Evidence.InterventionIDs))
	for _, s := range rec.Settlement.Evidence.InterventionIDs {
		if !memoryParkUUIDRe.MatchString(s) {
			return rec, nil, errMemoryParkRecord
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return rec, nil, errMemoryParkRecord
		}
		ids = append(ids, id)
	}
	return rec, ids, nil
}
