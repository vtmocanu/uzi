package main

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

var testSpecs = []CodecSpec{{codecFlate, 6}, {codecZstd, 3}}

func sp(s string) *string { return &s }
func ip(i int64) *int64   { return &i }

// sampleRecords covers every field, including each NULL combination.
func sampleRecords(n int) []Record {
	recs := make([]Record, n)
	for i := range recs {
		r := Record{
			ID: int64(1000 + i*3), Seq: i + 1, Kind: []string{"text", "tool_use", "tool_result", "status"}[i%4],
			Payload: []byte(fmt.Sprintf(`{"text":"message %d %s"}`, i, strings.Repeat("x", i%50))), CreatedAtMicro: 1_760_000_000_000_000 + int64(i)*1234567,
		}
		if i%2 == 0 {
			r.Agent = sp("coder")
		}
		if i%3 == 0 {
			r.AgentInstance = sp("coder#2")
		}
		if i%5 == 0 {
			r.AgentLabel = sp("") // present but empty must stay distinct from NULL
		}
		if i%7 != 0 {
			r.ClaimGeneration = ip(int64(i))
		}
		recs[i] = r
	}
	return recs
}

func newTestEngine(t *testing.T, maxRaw int) *Engine {
	t.Helper()
	e, err := NewEngine(maxRaw, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestRoundTripEveryFieldBothCodecs(t *testing.T) {
	e := newTestEngine(t, 64<<20)
	recs := sampleRecords(200)
	// An oversized message (bigger than any chunk bound) travels alone and untruncated.
	big := Record{ID: 99999, Seq: 201, Kind: "tool_result", Payload: []byte(`{"content":"` + strings.Repeat("A", 3<<20) + `"}`), CreatedAtMicro: 5}
	recs = append(recs, big)
	for _, s := range testSpecs {
		t.Run(s.Name(), func(t *testing.T) {
			for _, part := range [][]Record{recs[:200], recs[200:]} {
				data, err := e.Encode(s, part)
				if err != nil {
					t.Fatal(err)
				}
				got, h, err := e.Decode(data)
				if err != nil {
					t.Fatal(err)
				}
				if h.Count != len(part) || h.FirstSeq != part[0].Seq || h.LastSeq != part[len(part)-1].Seq || h.Codec != s {
					t.Fatalf("header %+v", h)
				}
				for i := range part {
					if !recordsEqual(&part[i], &got[i]) {
						t.Fatalf("record %d differs: %+v vs %+v", i, part[i], got[i])
					}
				}
			}
		})
	}
	// NULL versus empty string must be preserved distinctly.
	data, err := e.Encode(testSpecs[0], recs[:1])
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := e.Decode(data)
	if got[0].AgentLabel == nil || *got[0].AgentLabel != "" {
		t.Fatal("empty label decoded as NULL")
	}
	if got[0].ClaimGeneration != nil {
		t.Fatal("NULL claim_generation decoded as a value")
	}
}

func TestCRCMismatchRejected(t *testing.T) {
	e := newTestEngine(t, 1<<20)
	for _, s := range testSpecs {
		data, err := e.Encode(s, sampleRecords(20))
		if err != nil {
			t.Fatal(err)
		}
		bad := bytes.Clone(data)
		bad[24] ^= 0xff // header CRC field
		if _, _, err := e.Decode(bad); !errors.Is(err, errChunkCRC) {
			t.Fatalf("%s: want CRC error, got %v", s.Name(), err)
		}
	}
}

// rebuild constructs a chunk with arbitrary header values around a real body, to test lies.
func rebuild(t *testing.T, e *Engine, s CodecSpec, recs []Record, declaredRaw uint32) []byte {
	t.Helper()
	data, err := e.Encode(s, recs)
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.Clone(data)
	binary.LittleEndian.PutUint32(out[20:], declaredRaw)
	return out
}

func TestDeclaredLengthLieRejectedBothDirections(t *testing.T) {
	e := newTestEngine(t, 1<<20)
	recs := sampleRecords(50)
	var body []byte
	for i := range recs {
		body = appendRecord(body, &recs[i])
	}
	real := toU32(len(body))
	for _, s := range testSpecs {
		if _, _, err := e.Decode(rebuild(t, e, s, recs, real+10)); !errors.Is(err, errChunkLength) {
			t.Fatalf("%s lie high: %v", s.Name(), err)
		}
		if _, _, err := e.Decode(rebuild(t, e, s, recs, real-10)); !errors.Is(err, errChunkLength) && !errors.Is(err, errChunkCorrupt) {
			t.Fatalf("%s lie low: %v", s.Name(), err)
		}
		// Declared above the caller bound: refused before any decompression.
		if _, _, err := e.Decode(rebuild(t, e, s, recs, 2<<20)); !errors.Is(err, errChunkTooLarge) {
			t.Fatalf("%s over bound: %v", s.Name(), err)
		}
	}
}

func hdrFor(spec CodecSpec, raw int, crc uint32) []byte {
	h := make([]byte, chunkHeaderLen)
	copy(h, chunkMagic)
	h[4] = chunkVersion
	h[5] = byte(spec.ID)
	h[6] = toU8(spec.Level)
	binary.LittleEndian.PutUint32(h[8:], 1)
	binary.LittleEndian.PutUint32(h[12:], 1)
	binary.LittleEndian.PutUint32(h[16:], 1)
	binary.LittleEndian.PutUint32(h[20:], uint32(raw)) //nolint:gosec // G115: test value is small and positive.
	binary.LittleEndian.PutUint32(h[24:], crc)
	return h
}

func TestDecompressionBombRejectedWithoutAllocatingBeyondBound(t *testing.T) {
	const maxRaw = 1 << 20
	e := newTestEngine(t, maxRaw)
	const huge = 256 << 20
	zeros := make([]byte, huge)
	// flate: a tiny stream that expands to 256 MiB, behind a header that lies LOW.
	var fb bytes.Buffer
	fw, _ := flate.NewWriter(&fb, 9)
	_, _ = fw.Write(zeros)
	_ = fw.Close()
	flateBomb := append(hdrFor(CodecSpec{codecFlate, 9}, 1000, 0), fb.Bytes()...)
	// zstd: the same.
	enc, err := e.zstdEncoder(3)
	if err != nil {
		t.Fatal(err)
	}
	zstdBomb := append(hdrFor(CodecSpec{codecZstd, 3}, 1000, 0), enc.EncodeAll(zeros, nil)...)
	for name, bomb := range map[string][]byte{"flate": flateBomb, "zstd": zstdBomb} {
		if len(bomb) > 1<<20 {
			t.Fatalf("%s bomb is not small (%d bytes)", name, len(bomb))
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, _, err := e.Decode(bomb)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Fatalf("%s bomb decoded", name)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*maxRaw {
			t.Fatalf("%s bomb allocated %d bytes for a %d byte bound", name, grew, maxRaw)
		}
	}
}

// A zstd frame written through the streaming encoder carries no frame content size, so the
// decoder cannot refuse it up front: only the declared header length can stop it.
func TestZstdFrameWithoutContentSizeBoundedByDeclaredLength(t *testing.T) {
	const maxRaw = 1 << 20
	e := newTestEngine(t, maxRaw)
	const huge = 64 << 20
	var zb bytes.Buffer
	zw, err := zstd.NewWriter(&zb, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(zstdWindow))
	if err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 1<<20)
	for i := 0; i < huge/len(zeros); i++ {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if zb.Len() > 1<<20 {
		t.Fatalf("frame is not small (%d bytes)", zb.Len())
	}
	frame := zb.Bytes()
	if hasContentSize(frame) {
		t.Fatal("test frame unexpectedly declares a content size")
	}
	for _, declared := range []int{1000, maxRaw} {
		data := append(hdrFor(CodecSpec{codecZstd, 3}, declared, 0), frame...)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, _, err := e.Decode(data)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, errChunkLength) {
			t.Fatalf("declared %d: got %v, want errChunkLength", declared, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*maxRaw {
			t.Fatalf("declared %d: allocated %d bytes for a %d byte bound", declared, grew, maxRaw)
		}
	}
}

// hasContentSize reports whether a frame header carries the content-size field.
func hasContentSize(frame []byte) bool {
	var h zstd.Header
	return h.Decode(frame) == nil && h.HasFCS
}

func TestTruncatedAndBitFlippedRejected(t *testing.T) {
	e := newTestEngine(t, 1<<20)
	for _, s := range testSpecs {
		data, err := e.Encode(s, sampleRecords(100))
		if err != nil {
			t.Fatal(err)
		}
		for _, cut := range []int{len(data) - 1, len(data) / 2, chunkHeaderLen + 1, chunkHeaderLen - 1, 3} {
			if _, _, err := e.Decode(data[:cut]); err == nil {
				t.Fatalf("%s: truncation to %d accepted", s.Name(), cut)
			}
		}
		for _, pos := range []int{chunkHeaderLen + 2, (chunkHeaderLen + len(data)) / 2, len(data) - 2} {
			bad := bytes.Clone(data)
			bad[pos] ^= 0x10
			if _, _, err := e.Decode(bad); err == nil {
				t.Fatalf("%s: bit flip at %d accepted", s.Name(), pos)
			}
		}
	}
}

func TestBodyLengthPrefixLieRejected(t *testing.T) {
	// A record whose length prefix runs past the body must fail, not panic or over-read.
	e := newTestEngine(t, 1<<20)
	// One pad byte keeps RawLen at the 7-byte minimum record size, so the header count check
	// passes and the length-prefix lie is caught by the record parser.
	body := append(binary.AppendUvarint(nil, 1<<40), 0)
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, 1)
	_, _ = fw.Write(body)
	_ = fw.Close()
	data := append(hdrFor(CodecSpec{codecFlate, 1}, len(body), crc32.Checksum(body, crcTable)), buf.Bytes()...)
	if _, _, err := e.Decode(data); !errors.Is(err, errChunkCorrupt) {
		t.Fatalf("got %v", err)
	}
}

func TestChunkBoundaries(t *testing.T) {
	mk := func(n, payload int) []Record {
		recs := make([]Record, n)
		for i := range recs {
			recs[i] = Record{ID: int64(i), Seq: i + 1, Kind: "text", Payload: bytes.Repeat([]byte("p"), payload), CreatedAtMicro: int64(i)}
		}
		return recs
	}
	// Count ceiling binds.
	recs := mk(2500, 10)
	chunks := SplitChunks(recs, ChunkBounds{ByteBound: 1 << 20, CountCeiling: 1000})
	if len(chunks) != 3 || len(chunks[0]) != 1000 || len(chunks[2]) != 500 {
		t.Fatalf("count ceiling: %d chunks", len(chunks))
	}
	// Byte bound binds.
	recs = mk(100, 1000)
	chunks = SplitChunks(recs, ChunkBounds{ByteBound: 10_000, CountCeiling: 1000})
	for i, c := range chunks {
		var sz int
		for k := range c {
			sz += encodedSize(&c[k])
		}
		if sz > 10_000 {
			t.Fatalf("chunk %d is %d bytes", i, sz)
		}
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != 100 {
		t.Fatalf("rows lost: %d", total)
	}
	// An oversized message is alone in its own chunk and never truncated; neighbours keep going.
	recs = mk(10, 100)
	recs[5].Payload = bytes.Repeat([]byte("B"), 50_000)
	chunks = SplitChunks(recs, ChunkBounds{ByteBound: 4096, CountCeiling: 1000})
	found := false
	for _, c := range chunks {
		for k := range c {
			if len(c[k].Payload) == 50_000 {
				found = true
				if len(c) != 1 {
					t.Fatalf("oversized message shares a chunk with %d others", len(c)-1)
				}
			}
		}
	}
	if !found {
		t.Fatal("oversized message dropped")
	}
	// Contiguity: every row appears exactly once, in order.
	next := 1
	for _, c := range chunks {
		for k := range c {
			if c[k].Seq != next {
				t.Fatalf("seq %d where %d expected", c[k].Seq, next)
			}
			next++
		}
	}
}

func TestChunkKinds(t *testing.T) {
	got := chunkKinds([]Record{{Kind: "b"}, {Kind: "a"}, {Kind: "b"}})
	if fmt.Sprint(got) != "[a b]" {
		t.Fatal(got)
	}
}

// The CRC covers the body only, so a valid body behind a header whose Count is forged huge must
// be refused before parseRecords sizes its slice from that count.
func TestForgedHeaderCountRejectedBeforeAllocation(t *testing.T) {
	const maxRaw = 1 << 20
	e := newTestEngine(t, maxRaw)
	for _, s := range testSpecs {
		data, err := e.Encode(s, sampleRecords(20))
		if err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint32(data[16:], 0xFFFFFFFF)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, _, err = e.Decode(data)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, errChunkHeader) {
			t.Fatalf("%v: forged count: err = %v, want errChunkHeader", s, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*maxRaw {
			t.Fatalf("%v: forged count allocated %d bytes", s, grew)
		}
	}
}
