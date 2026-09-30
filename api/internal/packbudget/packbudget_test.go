package packbudget

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

// testLimits are deliberately small so every axis can be tripped by a tiny hand-assembled pack.
var testLimits = Limits{ObjectBytes: 1 << 20, TotalBytes: 3 << 20, InflationWorkBytes: 4 << 20, Objects: 10}

// TestReadDeltaVarint pins git's delta-header LEB128 decode: low 7 bits per byte,
// continue while 0x80 is set, little-endian, with truncation and overflow rejected.
func TestReadDeltaVarint(t *testing.T) {
	t.Run("roundtrip", func(t *testing.T) {
		for _, v := range []uint64{0, 1, 127, 128, 300, 16384, 1 << 20, 40 << 20} {
			enc := writeDeltaVarint(v)
			got, rest, err := ReadDeltaVarint(enc)
			if err != nil {
				t.Fatalf("v=%d: err = %v", v, err)
			}
			if uint64(got) != v { //nolint:gosec // got is a small non-negative test value.
				t.Fatalf("v=%d: decoded %d", v, got)
			}
			if len(rest) != 0 {
				t.Fatalf("v=%d: rest = %v, want empty", v, rest)
			}
		}
	})

	t.Run("rest_preserved", func(t *testing.T) {
		enc := append(writeDeltaVarint(300), 0xaa, 0xbb)
		got, rest, err := ReadDeltaVarint(enc)
		if err != nil || got != 300 {
			t.Fatalf("val = %d, err = %v", got, err)
		}
		if !bytes.Equal(rest, []byte{0xaa, 0xbb}) {
			t.Fatalf("rest = %v, want [aa bb]", rest)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		// A single byte with the continuation bit set and nothing after it.
		if _, _, err := ReadDeltaVarint([]byte{0x80}); err == nil {
			t.Fatal("truncated varint accepted")
		}
		if _, _, err := ReadDeltaVarint(nil); err == nil {
			t.Fatal("empty input accepted")
		}
	})

	t.Run("overflow", func(t *testing.T) {
		// Ten continuation bytes overshoot int64; must be rejected, never wrap negative.
		big := bytes.Repeat([]byte{0xff}, 10)
		if _, _, err := ReadDeltaVarint(big); err == nil {
			t.Fatal("overflowing varint accepted")
		}
	})
}

// boundOf asserts err is a *BudgetError matching ErrTooLarge and returns its bound.
func boundOf(t *testing.T, err error) Bound {
	t.Helper()
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	var be *BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want *BudgetError", err)
	}
	return be.Bound
}

func TestScanAcceptsEmptyAndNormal(t *testing.T) {
	if err := Scan(context.Background(), nil, testLimits); err != nil {
		t.Fatalf("empty pack: %v", err)
	}
	base := []byte("base blob\n")
	pack := assemblePack(t, blobObject(t, base), refDeltaObject(t, blobOID(base), deltaBody(uint64(len(base)), 4096)))
	if err := Scan(context.Background(), pack, testLimits); err != nil {
		t.Fatalf("legit delta rejected: %v", err)
	}
}

func TestScanBoundsNamed(t *testing.T) {
	base := []byte("base blob\n")
	t.Run("per_object_delta_target", func(t *testing.T) {
		pack := assemblePack(t, blobObject(t, base), refDeltaObject(t, blobOID(base), deltaBody(uint64(len(base)), 2<<20)))
		if got := boundOf(t, Scan(context.Background(), pack, testLimits)); got != BoundObjectBytes {
			t.Fatalf("bound = %q, want %q", got, BoundObjectBytes)
		}
	})
	t.Run("per_object_blob", func(t *testing.T) {
		pack := assemblePack(t, blobObject(t, make([]byte, 2<<20)))
		if got := boundOf(t, Scan(context.Background(), pack, testLimits)); got != BoundObjectBytes {
			t.Fatalf("bound = %q, want %q", got, BoundObjectBytes)
		}
	})
	t.Run("total_reconstructed", func(t *testing.T) {
		objs := [][]byte{blobObject(t, base)}
		for i := 0; i < 4; i++ { // 4 * 900 KiB > 3 MiB, each under the per-object cap
			objs = append(objs, refDeltaObject(t, blobOID(base), deltaBody(uint64(len(base)), 900<<10)))
		}
		if got := boundOf(t, Scan(context.Background(), assemblePack(t, objs...), testLimits)); got != BoundTotalBytes {
			t.Fatalf("bound = %q, want %q", got, BoundTotalBytes)
		}
	})
	t.Run("inflation_work", func(t *testing.T) {
		// Target size 0 keeps the reconstructed counter inert; only the work cap can trip.
		delta := refDeltaObject(t, blobOID(base), bigInstructionDelta(uint64(len(base)), 900<<10))
		objs := [][]byte{blobObject(t, base)}
		for i := 0; i < 6; i++ { // 6 * 900 KiB > 4 MiB
			objs = append(objs, delta)
		}
		if got := boundOf(t, Scan(context.Background(), assemblePack(t, objs...), testLimits)); got != BoundInflationWork {
			t.Fatalf("bound = %q, want %q", got, BoundInflationWork)
		}
	})
	t.Run("object_count", func(t *testing.T) {
		var buf bytes.Buffer
		buf.WriteString("PACK")
		_ = binary.Write(&buf, binary.BigEndian, uint32(2))
		_ = binary.Write(&buf, binary.BigEndian, uint32(testLimits.Objects+1))
		sum := sha1.Sum(buf.Bytes()) //nolint:gosec // pack trailer checksum is SHA-1 by format.
		buf.Write(sum[:])
		if got := boundOf(t, Scan(context.Background(), buf.Bytes(), testLimits)); got != BoundObjects {
			t.Fatalf("bound = %q, want %q", got, BoundObjects)
		}
	})
}

func TestBudgetErrorTextNamesBound(t *testing.T) {
	msg := (&BudgetError{BoundTotalBytes, 64 << 20}).Error()
	for _, want := range []string{"total reconstructed size", "67108864"} {
		if !bytes.Contains([]byte(msg), []byte(want)) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
}

func TestScanCancellable(t *testing.T) {
	base := []byte("base blob\n")
	pack := assemblePack(t, blobObject(t, base))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Scan(ctx, pack, testLimits); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestScanMalformed(t *testing.T) {
	if err := Scan(context.Background(), []byte("NOPExxxxxxxxxxxxxxxx"), testLimits); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad signature: err = %v, want ErrInvalid", err)
	}
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(1))
	sum := sha1.Sum(buf.Bytes()) //nolint:gosec // pack trailer checksum is SHA-1 by format.
	buf.Write(sum[:])
	if err := Scan(context.Background(), buf.Bytes(), testLimits); !errors.Is(err, ErrInvalid) {
		t.Fatalf("truncated body: err = %v, want ErrInvalid", err)
	}
}

// hand-assembled packfile builders (test-only)

func assemblePack(t *testing.T, objs ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(objs))) //nolint:gosec // test packs hold a handful of objects.
	for _, o := range objs {
		buf.Write(o)
	}
	sum := sha1.Sum(buf.Bytes()) //nolint:gosec // pack trailer checksum is SHA-1 by format.
	buf.Write(sum[:])
	return buf.Bytes()
}

func blobObject(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(packObjHeader(3, uint64(len(content)))) // OBJ_BLOB
	buf.Write(zlibBytes(t, content))
	return buf.Bytes()
}

func refDeltaObject(t *testing.T, base [20]byte, delta []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(packObjHeader(7, uint64(len(delta)))) // OBJ_REF_DELTA
	buf.Write(base[:])
	buf.Write(zlibBytes(t, delta))
	return buf.Bytes()
}

func deltaBody(baseSz, targetSz uint64) []byte {
	var b []byte
	b = append(b, writeDeltaVarint(baseSz)...)
	b = append(b, writeDeltaVarint(targetSz)...)
	b = append(b, 0x03, 'a', 'b', 'c')
	return b
}

// bigInstructionDelta declares target size 0 behind an instrBytes-long instruction stream.
func bigInstructionDelta(baseSz uint64, instrBytes int) []byte {
	b := writeDeltaVarint(baseSz)
	b = append(b, writeDeltaVarint(0)...)
	if pad := instrBytes - len(b); pad > 0 {
		b = append(b, make([]byte, pad)...)
	}
	return b
}

// packObjHeader encodes the packfile per-object type+length header (not the delta varint).
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
			return append(out, c)
		}
		out = append(out, c|0x80)
	}
}

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

func blobOID(content []byte) [20]byte {
	h := sha1.New() //nolint:gosec // git object id is SHA-1 by definition.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	var out [20]byte
	copy(out[:], h.Sum(nil))
	return out
}
