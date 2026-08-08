package pushbroker

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" //nolint:gosec // git object ids ARE SHA-1; this builds test-fixture pack ids, not a security primitive.
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// These internal tests exercise the delta-aware inflation budget directly (the
// unexported scanPackBudget and readDeltaVarint). They hand-assemble packfiles so a
// DELTA object can declare a huge reconstructed target size behind a tiny
// instruction stream — the exact shape scanPackBudget exists to refuse and that a
// header-Length-only bound would wave through.

// TestReadDeltaVarint pins git's delta-header LEB128 decode: low 7 bits per byte,
// continue while 0x80 is set, little-endian, with truncation and overflow rejected.
func TestReadDeltaVarint(t *testing.T) {
	t.Run("roundtrip", func(t *testing.T) {
		for _, v := range []uint64{0, 1, 127, 128, 300, 16384, 1 << 20, 40 << 20} {
			enc := writeDeltaVarint(v)
			got, rest, err := readDeltaVarint(enc)
			if err != nil {
				t.Fatalf("v=%d: err = %v", v, err)
			}
			if uint64(got) != v {
				t.Fatalf("v=%d: decoded %d", v, got)
			}
			if len(rest) != 0 {
				t.Fatalf("v=%d: rest = %v, want empty", v, rest)
			}
		}
	})

	t.Run("rest_preserved", func(t *testing.T) {
		enc := append(writeDeltaVarint(300), 0xaa, 0xbb)
		got, rest, err := readDeltaVarint(enc)
		if err != nil || got != 300 {
			t.Fatalf("val = %d, err = %v", got, err)
		}
		if !bytes.Equal(rest, []byte{0xaa, 0xbb}) {
			t.Fatalf("rest = %v, want [aa bb]", rest)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		// A single byte with the continuation bit set and nothing after it.
		if _, _, err := readDeltaVarint([]byte{0x80}); err == nil {
			t.Fatal("truncated varint accepted")
		}
		if _, _, err := readDeltaVarint(nil); err == nil {
			t.Fatal("empty input accepted")
		}
	})

	t.Run("overflow", func(t *testing.T) {
		// Ten continuation bytes overshoot int64; must be rejected, never wrap negative.
		big := bytes.Repeat([]byte{0xff}, 10)
		if _, _, err := readDeltaVarint(big); err == nil {
			t.Fatal("overflowing varint accepted")
		}
	})
}

// TestScanPackBudgetRejectsDeltaBomb is the core regression: a REF_DELTA whose
// instruction stream is a few bytes (well under the 32 MiB per-object cap on the
// header Length) but whose declared TARGET size is 40 MiB (over the cap). The old
// Length-only pre-pass accepted it; scanPackBudget must reject it as ErrPackTooLarge
// on the target size, before any reconstruction.
func TestScanPackBudgetRejectsDeltaBomb(t *testing.T) {
	baseContent := []byte("base blob\n")
	pack := assemblePack(t,
		blobObject(t, baseContent),
		refDeltaObject(t, blobOID(baseContent), deltaBody(uint64(len(baseContent)), 40<<20)),
	)
	// The whole thing is tiny on the wire — a naive apply would still reconstruct 40 MiB.
	if len(pack) > 1<<10 {
		t.Fatalf("bomb pack is %d bytes; expected it to be tiny", len(pack))
	}
	if err := scanPackBudget(context.Background(), pack); !errors.Is(err, ErrPackTooLarge) {
		t.Fatalf("err = %v, want ErrPackTooLarge", err)
	}
}

// TestScanPackBudgetRejectsDeltaBombCumulative proves the CUMULATIVE bound also
// counts reconstructed (target) bytes: several deltas each under the per-object cap
// but summing past maxPackTotalBytes are refused.
func TestScanPackBudgetRejectsDeltaBombCumulative(t *testing.T) {
	baseContent := []byte("base blob\n")
	perDelta := uint64(30 << 20) // under the 32 MiB per-object cap
	objs := [][]byte{blobObject(t, baseContent)}
	// 5 * 30 MiB = 150 MiB reconstructed > 128 MiB cumulative cap.
	for i := 0; i < 5; i++ {
		objs = append(objs, refDeltaObject(t, blobOID(baseContent), deltaBody(uint64(len(baseContent)), perDelta)))
	}
	pack := assemblePack(t, objs...)
	if err := scanPackBudget(context.Background(), pack); !errors.Is(err, ErrPackTooLarge) {
		t.Fatalf("err = %v, want ErrPackTooLarge (cumulative)", err)
	}
}

// TestScanPackBudgetRejectsInstructionStreamBomb is the regression for the DoS this
// M8 hardening closes — the DUAL of the target-size bomb above. Here every REF_DELTA
// declares a TARGET size of 0 (so it contributes nothing to the reconstructed-size
// counter and maxPackTotalBytes NEVER fires) but carries a ~maxPackObjectBytes
// INSTRUCTION stream. The scanner must zlib-inflate each instruction stream in full to
// stay aligned; enough of them cross the cumulative inflation-work cap. The
// pre-hardening budget counted only reconstructed (target) bytes for deltas, so it
// waved this through — an 897 KiB pack was measured inflating ~900 MiB and being
// ACCEPTED (err=nil). scanPackBudget must now reject it as ErrPackTooLarge via
// maxPackInflationWorkBytes.
//
// That it is specifically the NEW cap firing (all budget axes share ErrPackTooLarge):
// every targetSz is 0 so `total` stays 0 (reconstructed cap inert), each h.Length is
// under maxPackObjectBytes (per-object cap inert), and the object count is a handful
// << maxPackObjects (count cap inert). Only the inflation-work counter can trip here.
func TestScanPackBudgetRejectsInstructionStreamBomb(t *testing.T) {
	baseContent := []byte("base blob\n")
	const perDeltaInstr = 30 << 20 // under the 32 MiB per-object cap, near it
	// Enough deltas to push cumulative inflation work past maxPackInflationWorkBytes
	// (+2 over the integer division for margin), while every other budget axis stays
	// comfortably inert.
	nDeltas := maxPackInflationWorkBytes/perDeltaInstr + 2
	// All deltas are byte-identical: compress the object ONCE and reuse it, so test
	// construction stays cheap while the scan still inflates each in turn.
	deltaObj := refDeltaObject(t, blobOID(baseContent), bigInstructionDelta(uint64(len(baseContent)), perDeltaInstr))
	objs := [][]byte{blobObject(t, baseContent)}
	for i := 0; i < nDeltas; i++ {
		objs = append(objs, deltaObj)
	}
	pack := assemblePack(t, objs...)
	// Tiny on the wire (the zero-filled instruction streams zlib-compress away), yet a
	// pre-hardening scanner would inflate hundreds of MiB and accept it.
	if len(pack) > 1<<20 {
		t.Fatalf("instruction-stream bomb pack is %d bytes; expected it to be small on the wire", len(pack))
	}
	if err := scanPackBudget(context.Background(), pack); !errors.Is(err, ErrPackTooLarge) {
		t.Fatalf("err = %v, want ErrPackTooLarge (inflation-work cap)", err)
	}
}

// TestScanPackBudgetCancellable proves scanPackBudget honours ctx: an already-cancelled
// context is returned before the first object is inflated, so the per-publish
// wall-clock timeout in Publish can actually cut a long inflation short.
func TestScanPackBudgetCancellable(t *testing.T) {
	baseContent := []byte("base blob\n")
	pack := assemblePack(t,
		blobObject(t, baseContent),
		refDeltaObject(t, blobOID(baseContent), deltaBody(uint64(len(baseContent)), 4096)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := scanPackBudget(ctx, pack); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestScanPackBudgetAcceptsNormalDelta proves a delta whose declared target size is
// small passes — the budget is not rejecting all deltas.
func TestScanPackBudgetAcceptsNormalDelta(t *testing.T) {
	baseContent := []byte("base blob\n")
	pack := assemblePack(t,
		blobObject(t, baseContent),
		refDeltaObject(t, blobOID(baseContent), deltaBody(uint64(len(baseContent)), 4096)),
	)
	if err := scanPackBudget(context.Background(), pack); err != nil {
		t.Fatalf("scanPackBudget rejected a legit delta: %v", err)
	}
}

// TestScanPackBudgetMalformed maps a genuinely unparseable pack to ErrPackInvalid (a
// best-effort skip), distinct from ErrPackTooLarge.
func TestScanPackBudgetMalformed(t *testing.T) {
	t.Run("bad_signature", func(t *testing.T) {
		if err := scanPackBudget(context.Background(), []byte("NOPExxxxxxxxxxxxxxxx")); !errors.Is(err, ErrPackInvalid) {
			t.Fatalf("err = %v, want ErrPackInvalid", err)
		}
	})
	t.Run("truncated_body", func(t *testing.T) {
		// Valid header declaring one object, then no object bytes at all.
		var buf bytes.Buffer
		buf.WriteString("PACK")
		_ = binary.Write(&buf, binary.BigEndian, uint32(2))
		_ = binary.Write(&buf, binary.BigEndian, uint32(1))
		sum := sha1.Sum(buf.Bytes())
		buf.Write(sum[:])
		if err := scanPackBudget(context.Background(), buf.Bytes()); !errors.Is(err, ErrPackInvalid) {
			t.Fatalf("err = %v, want ErrPackInvalid", err)
		}
	})
}

// TestScanPackBudgetTooManyObjects keeps the object-count ceiling covered.
func TestScanPackBudgetTooManyObjects(t *testing.T) {
	// Hand a header claiming > maxPackObjects; scanPackBudget rejects on the count
	// alone, before reading any object body.
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(maxPackObjects+1))
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	if err := scanPackBudget(context.Background(), buf.Bytes()); !errors.Is(err, ErrPackTooLarge) {
		t.Fatalf("err = %v, want ErrPackTooLarge", err)
	}
}

// TestPublishRejectsDeltaBomb drives the bomb through Publish. scanPackBudget is
// step 1, BEFORE any remote is created or dialed, so the bomb is refused with
// ErrPackTooLarge and NOTHING is reconstructed or pushed — the CloneURL below is
// never contacted. The test completing immediately (no 40 MiB allocation, no
// network) is the evidence the huge target is never materialized.
func TestPublishRejectsDeltaBomb(t *testing.T) {
	baseContent := []byte("base blob\n")
	pack := assemblePack(t,
		blobObject(t, baseContent),
		refDeltaObject(t, blobOID(baseContent), deltaBody(uint64(len(baseContent)), 40<<20)),
	)
	_, err := Publish(context.Background(), Options{
		CloneURL:    "file:///nonexistent/never-dialed.git",
		Branch:      "main",
		DeclaredTip: "1111111111111111111111111111111111111111",
		Pack:        pack,
	})
	if !errors.Is(err, ErrPackTooLarge) {
		t.Fatalf("Publish err = %v, want ErrPackTooLarge", err)
	}
}

// -------------------------------------------------------------------------
// hand-assembled packfile builders (test-only)
// -------------------------------------------------------------------------

// assemblePack frames objs (each already type-header + body) into a v2 packfile with
// the trailing SHA-1 checksum over all preceding bytes.
func assemblePack(t *testing.T, objs ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("PACK")
	if err := binary.Write(&buf, binary.BigEndian, uint32(2)); err != nil { // version
		t.Fatalf("write version: %v", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, uint32(len(objs))); err != nil { // count
		t.Fatalf("write count: %v", err)
	}
	for _, o := range objs {
		buf.Write(o)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// blobObject encodes a full (non-delta) OBJ_BLOB: packfile object header + zlib body.
func blobObject(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(packObjHeader(3, uint64(len(content)))) // OBJ_BLOB = 3
	buf.Write(zlibBytes(t, content))
	return buf.Bytes()
}

// refDeltaObject encodes an OBJ_REF_DELTA: packfile object header (Length = the
// UNCOMPRESSED delta body length), the 20-byte base object id, then the zlib delta.
func refDeltaObject(t *testing.T, base [20]byte, delta []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(packObjHeader(7, uint64(len(delta)))) // OBJ_REF_DELTA = 7
	buf.Write(base[:])
	buf.Write(zlibBytes(t, delta))
	return buf.Bytes()
}

// deltaBody builds a git delta stream header: base size varint, target size varint,
// then a trivial insert op. scanPackBudget reads only the two varints; the op just
// makes the stream non-empty.
func deltaBody(baseSz, targetSz uint64) []byte {
	var b []byte
	b = append(b, writeDeltaVarint(baseSz)...)
	b = append(b, writeDeltaVarint(targetSz)...)
	b = append(b, 0x03, 'a', 'b', 'c') // insert-op: literal 3 bytes
	return b
}

// bigInstructionDelta is the DUAL of deltaBody: a delta whose declared TARGET size is
// 0 (so it contributes nothing to scanPackBudget's reconstructed-size counter) but
// whose whole body is instrBytes long — the INSTRUCTION stream the scanner must
// zlib-inflate. The packfile object header (refDeltaObject) declares Length = len of
// this body, so the returned slice is exactly instrBytes. scanPackBudget reads only
// the two leading varints and discards the rest, so the zero filler is never executed
// as delta ops; zeros zlib-compress to almost nothing, which is what makes the bomb
// tiny on the wire yet huge to inflate.
func bigInstructionDelta(baseSz uint64, instrBytes int) []byte {
	b := writeDeltaVarint(baseSz)
	b = append(b, writeDeltaVarint(0)...) // target size 0
	if pad := instrBytes - len(b); pad > 0 {
		b = append(b, make([]byte, pad)...)
	}
	return b
}

// packObjHeader encodes the packfile per-object type+length header: the type in bits
// 4-6 of the first byte with the low 4 size bits, then 7 size bits per continuation
// byte. This is the OBJECT-header encoding — deliberately different from the delta
// size varint readDeltaVarint decodes.
func packObjHeader(typ byte, size uint64) []byte {
	first := (typ << 4) | byte(size&0x0f)
	size >>= 4
	if size == 0 {
		return []byte{first}
	}
	out := []byte{first | 0x80}
	for {
		c := byte(size & 0x7f)
		size >>= 7
		if size == 0 {
			out = append(out, c)
			return out
		}
		out = append(out, c|0x80)
	}
}

// writeDeltaVarint is the inverse of readDeltaVarint: git's delta size LEB128.
func writeDeltaVarint(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// zlibBytes zlib-compresses data (packfile object bodies are zlib streams).
func zlibBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// blobOID computes a git blob object id (sha1 of "blob <len>\0"+content). The
// ref-delta's declared base; scanPackBudget never resolves it, so it only needs to
// be well-formed.
func blobOID(content []byte) [20]byte {
	h := sha1.New() //nolint:gosec // git object id is SHA-1 by definition.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	var out [20]byte
	copy(out[:], h.Sum(nil))
	return out
}
