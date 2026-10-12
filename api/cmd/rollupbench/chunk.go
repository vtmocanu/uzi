package main

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Record is one run_messages row exactly as the rollup must preserve it. Payload holds the
// exact payload::text bytes read from Postgres, so a decoded record compares byte-for-byte
// with what the live table returned. Nullable columns are pointers so NULL stays distinct
// from the empty string / zero.
type Record struct {
	ID              int64
	Seq             int
	Kind            string
	Agent           *string
	AgentInstance   *string
	AgentLabel      *string
	Payload         []byte
	CreatedAtMicro  int64
	ClaimGeneration *int64
}

// CodecID names the compression algorithm stored in a chunk header.
type CodecID uint8

const (
	codecFlate CodecID = 1
	codecZstd  CodecID = 2
)

// CodecSpec is an algorithm plus the requested level (flate 1..9, zstd 1..22 as the zstd CLI numbers them).
type CodecSpec struct {
	ID    CodecID
	Level int
}

// Name is the stable label used in results, e.g. "flate-6" or "zstd-3".
func (s CodecSpec) Name() string {
	switch s.ID {
	case codecFlate:
		return fmt.Sprintf("flate-%d", s.Level)
	case codecZstd:
		return fmt.Sprintf("zstd-%d", s.Level)
	default:
		return "unknown"
	}
}

const (
	chunkMagic     = "RMC1"
	chunkVersion   = 1
	chunkHeaderLen = 28
	// zstdWindow is the encoder window and the decoder's maximum accepted window. A chunk body
	// is bounded by the caller's maxRaw, so a window beyond this only ever costs memory.
	zstdWindow = 1 << 20
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Header is the fixed-size prefix of a chunk, readable without decompressing.
type Header struct {
	Codec    CodecSpec
	FirstSeq int
	LastSeq  int
	Count    int
	RawLen   int
	CRC      uint32
}

// Errors the decoder reports. Tests match them with errors.Is.
var (
	errChunkHeader    = errors.New("chunk: bad header")
	errChunkTooLarge  = errors.New("chunk: declared uncompressed length exceeds the caller bound")
	errChunkLength    = errors.New("chunk: decoded length differs from the declared length")
	errChunkCRC       = errors.New("chunk: crc mismatch")
	errChunkCorrupt   = errors.New("chunk: corrupt body")
	errChunkSeqBounds = errors.New("chunk: records disagree with the header seq bounds or count")
)

// toU32 narrows a non-negative length to the 32-bit header field; a longer value is a programming
// error because chunk bodies are bounded far below 4 GiB.
func toU32(n int) uint32 {
	if n < 0 || n > 1<<31-1 {
		panic("chunk: header field out of range")
	}
	return uint32(n)
}

func toU8(n int) uint8 {
	if n < 0 || n > 255 {
		panic("chunk: level out of range")
	}
	return uint8(n)
}

// Engine encodes and decodes chunks. It owns the pooled compressors: one shared zstd encoder
// per requested level (EncodeAll is safe for concurrent use), a sync.Pool of streaming
// single-threaded zstd decoders, and sync.Pools of flate writers and readers. Concurrency is
// recorded in PoolingNote so the results state exactly what ran.
type Engine struct {
	maxRaw      int
	concurrency int

	mu   sync.Mutex
	zenc map[int]*zstd.Encoder
	zdec sync.Pool // *zstd.Decoder, streaming, concurrency 1

	fwriters sync.Map // level -> *sync.Pool of *flate.Writer
	freaders sync.Pool
}

// NewEngine builds an engine whose decoder refuses any chunk that declares, or decodes to,
// more than maxRaw uncompressed bytes. concurrency sets the zstd encoder/decoder concurrency.
func NewEngine(maxRaw, concurrency int) (*Engine, error) {
	if maxRaw <= 0 || concurrency <= 0 {
		return nil, errors.New("engine: maxRaw and concurrency must be positive")
	}
	return &Engine{maxRaw: maxRaw, concurrency: concurrency, zenc: map[int]*zstd.Encoder{}}, nil
}

// Close releases the zstd encoder goroutines.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, enc := range e.zenc {
		_ = enc.Close()
	}
}

// PoolingNote documents what is pooled, for settings.json.
func (e *Engine) PoolingNote() map[string]any {
	return map[string]any{
		"zstd_encoder":             "one shared encoder per requested level, EncodeAll (concurrency-safe)",
		"zstd_decoder":             "sync.Pool of streaming decoders read through io.LimitReader(declared length + 1)",
		"zstd_encoder_concurrency": e.concurrency,
		"zstd_decoder_concurrency": 1,
		"zstd_window_bytes":        zstdWindow,
		"zstd_decoder_max_memory":  e.maxRaw,
		"flate":                    "sync.Pool of writers per level and of readers (Resettable)",
	}
}

func (e *Engine) zstdEncoder(level int) (*zstd.Encoder, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if enc, ok := e.zenc[level]; ok {
		return enc, nil
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(e.concurrency),
		zstd.WithWindowSize(zstdWindow),
		zstd.WithEncoderCRC(false), // the chunk CRC-32C covers the body; no second checksum
	)
	if err != nil {
		return nil, err
	}
	e.zenc[level] = enc
	return enc, nil
}

// appendRecord appends one length-prefixed record to dst and returns it.
func appendRecord(dst []byte, r *Record) []byte {
	var body []byte
	body = binary.AppendVarint(body, r.ID)
	body = binary.AppendVarint(body, int64(r.Seq))
	body = binary.AppendUvarint(body, uint64(len(r.Kind)))
	body = append(body, r.Kind...)
	var flags byte
	if r.Agent != nil {
		flags |= 1
	}
	if r.AgentInstance != nil {
		flags |= 2
	}
	if r.AgentLabel != nil {
		flags |= 4
	}
	if r.ClaimGeneration != nil {
		flags |= 8
	}
	body = append(body, flags)
	for _, s := range []*string{r.Agent, r.AgentInstance, r.AgentLabel} {
		if s != nil {
			body = binary.AppendUvarint(body, uint64(len(*s)))
			body = append(body, *s...)
		}
	}
	body = binary.AppendUvarint(body, uint64(len(r.Payload)))
	body = append(body, r.Payload...)
	body = binary.AppendVarint(body, r.CreatedAtMicro)
	if r.ClaimGeneration != nil {
		body = binary.AppendVarint(body, *r.ClaimGeneration)
	}
	dst = binary.AppendUvarint(dst, uint64(len(body)))
	return append(dst, body...)
}

// encodedSize is the exact number of body bytes record r occupies.
func encodedSize(r *Record) int {
	return len(appendRecord(nil, r))
}

// reader is a bounds-checked cursor over a record body.
type reader struct {
	b   []byte
	err error
}

func (r *reader) fail() { r.err = errChunkCorrupt }

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.fail()
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.fail()
		return 0
	}
	r.b = r.b[n:]
	return v
}

// take returns the next n bytes, refusing a length that exceeds what is left.
func (r *reader) take(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)) {
		r.fail()
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) byte1() byte {
	if r.err != nil {
		return 0
	}
	if len(r.b) == 0 {
		r.fail()
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

// parseRecords decodes a body into records. Payload slices alias into one copy of the body
// owned by the caller, so a caller that keeps records must keep the body alive (Decode does).
func parseRecords(body []byte, count int) ([]Record, error) {
	out := make([]Record, 0, min(count, len(body)/7))
	rd := &reader{b: body}
	for len(rd.b) > 0 {
		n := rd.uvarint()
		rec := rd.take(n)
		if rd.err != nil {
			return nil, rd.err
		}
		var r Record
		in := &reader{b: rec}
		r.ID = in.varint()
		r.Seq = int(in.varint())
		r.Kind = string(in.take(in.uvarint()))
		flags := in.byte1()
		var strs [3]*string
		for i := range strs {
			if flags&(1<<i) != 0 {
				s := string(in.take(in.uvarint()))
				strs[i] = &s
			}
		}
		r.Agent, r.AgentInstance, r.AgentLabel = strs[0], strs[1], strs[2]
		r.Payload = in.take(in.uvarint())
		r.CreatedAtMicro = in.varint()
		if flags&8 != 0 {
			g := in.varint()
			r.ClaimGeneration = &g
		}
		if in.err != nil || len(in.b) != 0 {
			return nil, errChunkCorrupt
		}
		out = append(out, r)
		if len(out) > count {
			return nil, errChunkSeqBounds
		}
	}
	return out, nil
}

// Encode serialises recs (non-empty, ascending seq) into one chunk.
func (e *Engine) Encode(spec CodecSpec, recs []Record) ([]byte, error) {
	if len(recs) == 0 {
		return nil, errors.New("encode: empty chunk")
	}
	var body []byte
	for i := range recs {
		body = appendRecord(body, &recs[i])
	}
	hdr := make([]byte, chunkHeaderLen, chunkHeaderLen+len(body)/2+64)
	copy(hdr, chunkMagic)
	hdr[4] = chunkVersion
	hdr[5] = byte(spec.ID)
	hdr[6] = toU8(spec.Level)
	binary.LittleEndian.PutUint32(hdr[8:], toU32(recs[0].Seq))
	binary.LittleEndian.PutUint32(hdr[12:], toU32(recs[len(recs)-1].Seq))
	binary.LittleEndian.PutUint32(hdr[16:], toU32(len(recs)))
	binary.LittleEndian.PutUint32(hdr[20:], toU32(len(body)))
	binary.LittleEndian.PutUint32(hdr[24:], crc32.Checksum(body, crcTable))
	switch spec.ID {
	case codecFlate:
		w, err := e.flateWriter(spec.Level, bytes.NewBuffer(hdr))
		if err != nil {
			return nil, err
		}
		if _, err := w.w.Write(body); err != nil {
			return nil, err
		}
		if err := w.w.Close(); err != nil {
			return nil, err
		}
		out := w.buf.Bytes()
		e.putFlateWriter(spec.Level, w)
		return out, nil
	case codecZstd:
		enc, err := e.zstdEncoder(spec.Level)
		if err != nil {
			return nil, err
		}
		return enc.EncodeAll(body, hdr), nil
	default:
		return nil, fmt.Errorf("encode: unknown codec %d", spec.ID)
	}
}

type pooledFlateWriter struct {
	w   *flate.Writer
	buf *bytes.Buffer
}

func (e *Engine) flateWriter(level int, dst *bytes.Buffer) (*pooledFlateWriter, error) {
	p, _ := e.fwriters.LoadOrStore(level, &sync.Pool{})
	if v := p.(*sync.Pool).Get(); v != nil {
		pw := v.(*pooledFlateWriter)
		pw.buf = dst
		pw.w.Reset(dst)
		return pw, nil
	}
	w, err := flate.NewWriter(dst, level)
	if err != nil {
		return nil, err
	}
	return &pooledFlateWriter{w: w, buf: dst}, nil
}

func (e *Engine) putFlateWriter(level int, pw *pooledFlateWriter) {
	pw.buf = nil
	p, _ := e.fwriters.Load(level)
	p.(*sync.Pool).Put(pw)
}

// PeekHeader parses the header without touching the body.
func PeekHeader(data []byte) (Header, error) {
	if len(data) < chunkHeaderLen || string(data[:4]) != chunkMagic || data[4] != chunkVersion {
		return Header{}, errChunkHeader
	}
	h := Header{
		Codec:    CodecSpec{ID: CodecID(data[5]), Level: int(data[6])},
		FirstSeq: int(binary.LittleEndian.Uint32(data[8:])),
		LastSeq:  int(binary.LittleEndian.Uint32(data[12:])),
		Count:    int(binary.LittleEndian.Uint32(data[16:])),
		RawLen:   int(binary.LittleEndian.Uint32(data[20:])),
		CRC:      binary.LittleEndian.Uint32(data[24:]),
	}
	if h.Codec.ID != codecFlate && h.Codec.ID != codecZstd {
		return Header{}, errChunkHeader
	}
	if h.Count <= 0 || h.FirstSeq > h.LastSeq {
		return Header{}, errChunkHeader
	}
	return h, nil
}

// Decode verifies and decodes a chunk. The declared uncompressed length is checked against the
// engine bound BEFORE any decompression, and the decompressor is capped so a header that lies
// low cannot make it produce more than the bound.
func (e *Engine) Decode(data []byte) ([]Record, Header, error) {
	h, err := PeekHeader(data)
	if err != nil {
		return nil, h, err
	}
	if h.RawLen > e.maxRaw {
		return nil, h, errChunkTooLarge
	}
	// Every record encodes to at least 7 bytes (length prefix, id, seq, kind length, flags,
	// payload length, created_at), so a larger count is a corrupt header. The CRC covers the
	// body only, so this check keeps a bad count from driving parseRecords' allocation.
	if h.Count > h.RawLen/7 {
		return nil, h, errChunkHeader
	}
	comp := data[chunkHeaderLen:]
	var body []byte
	switch h.Codec.ID {
	case codecFlate:
		body, err = e.inflate(comp, h.RawLen)
	case codecZstd:
		body, err = e.unzstd(comp, h.RawLen)
	}
	if err != nil {
		return nil, h, err
	}
	if len(body) != h.RawLen {
		return nil, h, errChunkLength
	}
	if crc32.Checksum(body, crcTable) != h.CRC {
		return nil, h, errChunkCRC
	}
	recs, err := parseRecords(body, h.Count)
	if err != nil {
		return nil, h, err
	}
	if len(recs) != h.Count || recs[0].Seq != h.FirstSeq || recs[len(recs)-1].Seq != h.LastSeq {
		return nil, h, errChunkSeqBounds
	}
	return recs, h, nil
}

func (e *Engine) inflate(comp []byte, rawLen int) ([]byte, error) {
	var fr io.ReadCloser
	if v := e.freaders.Get(); v != nil {
		fr = v.(io.ReadCloser)
		if err := fr.(flate.Resetter).Reset(bytes.NewReader(comp), nil); err != nil {
			return nil, err
		}
	} else {
		fr = flate.NewReader(bytes.NewReader(comp))
	}
	defer e.freaders.Put(fr)
	// Read through a limit one byte past the declared length: a stream that produces more is
	// stopped there and rejected, and a truncated stream surfaces flate's own unexpected-EOF.
	var buf bytes.Buffer
	buf.Grow(rawLen + 1)
	if _, err := buf.ReadFrom(io.LimitReader(fr, int64(rawLen)+1)); err != nil {
		return nil, fmt.Errorf("%w: %v", errChunkCorrupt, err)
	}
	if buf.Len() > rawLen {
		return nil, errChunkLength // produced more than declared
	}
	return buf.Bytes(), nil
}

// unzstd streams the frame through a limit one byte past the declared length, like inflate:
// a frame that carries no content size (or lies about it) is stopped at the declared length
// and rejected, so the output never grows beyond rawLen+1. WithDecoderMaxMemory bounds the
// window of a streaming decode, not its output, which is why the limit reader is needed.
func (e *Engine) unzstd(comp []byte, rawLen int) ([]byte, error) {
	var dec *zstd.Decoder
	if v := e.zdec.Get(); v != nil {
		dec = v.(*zstd.Decoder)
	} else {
		var err error
		dec, err = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(e.maxRaw)), //nolint:gosec // G115: maxRaw is validated positive in NewEngine.
			zstd.WithDecoderMaxWindow(zstdWindow),
		)
		if err != nil {
			return nil, fmt.Errorf("zstd decoder: %w", err)
		}
	}
	defer func() {
		_ = dec.Reset(nil) // drop the reference to comp before the decoder waits in the pool
		e.zdec.Put(dec)
	}()
	// bytes.Reader is not a bytes.Buffer, so the decoder streams instead of taking its
	// in-memory shortcut.
	if err := dec.Reset(bytes.NewReader(comp)); err != nil {
		return nil, fmt.Errorf("%w: %v", errChunkCorrupt, err)
	}
	var buf bytes.Buffer
	buf.Grow(rawLen + 1)
	if _, err := buf.ReadFrom(io.LimitReader(dec, int64(rawLen)+1)); err != nil {
		return nil, fmt.Errorf("%w: %v", errChunkCorrupt, err)
	}
	if buf.Len() > rawLen {
		return nil, errChunkLength // produced more than declared
	}
	return buf.Bytes(), nil
}

// ChunkBounds is the greedy chunking rule: close a chunk before it would exceed ByteBound
// uncompressed body bytes or CountCeiling records. A record whose own encoding exceeds
// ByteBound is emitted alone, never truncated.
type ChunkBounds struct {
	ByteBound    int
	CountCeiling int
}

// SplitChunks partitions recs (ascending seq) under the bounds.
func SplitChunks(recs []Record, b ChunkBounds) [][]Record {
	var out [][]Record
	start, cur := 0, 0
	flush := func(end int) {
		if end > start {
			out = append(out, recs[start:end])
		}
		start, cur = end, 0
	}
	for i := range recs {
		sz := encodedSize(&recs[i])
		if sz > b.ByteBound {
			flush(i)
			out = append(out, recs[i:i+1])
			start = i + 1
			continue
		}
		if cur+sz > b.ByteBound || i-start >= b.CountCeiling {
			flush(i)
		}
		cur += sz
	}
	flush(len(recs))
	return out
}

// chunkKinds returns the sorted distinct kinds in a chunk, stored beside it so filtered reads
// can skip chunks that cannot match.
func chunkKinds(recs []Record) []string {
	seen := map[string]bool{}
	for i := range recs {
		seen[recs[i].Kind] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// recordsEqual compares every field, including NULL-ness.
func recordsEqual(a, b *Record) bool {
	if a.ID != b.ID || a.Seq != b.Seq || a.Kind != b.Kind || a.CreatedAtMicro != b.CreatedAtMicro {
		return false
	}
	if !bytes.Equal(a.Payload, b.Payload) {
		return false
	}
	return ptrEq(a.Agent, b.Agent) && ptrEq(a.AgentInstance, b.AgentInstance) &&
		ptrEq(a.AgentLabel, b.AgentLabel) && ptrEq(a.ClaimGeneration, b.ClaimGeneration)
}

func ptrEq[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
