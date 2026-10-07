package pushbroker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
)

const testReportByteLimit = 1 << 20

func TestRawResponseBudgetDecoder(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	lines := make([]string, 100001)
	lines[0] = "unpack ok"
	for i := 1; i < len(lines); i++ {
		lines[i] = "ok " + ref
	}
	wire := rawFixtureWire(t, lines...)
	original := &chunkErrorReader{data: wire, chunk: len(wire), terminal: io.EOF}
	raw := &rawReport{ref: ref}
	reader := &rawReportReader{reader: original, report: raw}
	decoded := packp.NewReportStatus()
	err := decoded.Decode(reader)
	read := len(wire) - len(original.data)
	t.Logf("decoder bytes=%d retained statuses=%d error=%v", read, len(decoded.CommandStatuses), err)
	if err == nil || read != testReportByteLimit || len(decoded.CommandStatuses) >= 100000 {
		t.Fatalf("unbounded decoder: bytes=%d statuses=%d error=%v", read, len(decoded.CommandStatuses), err)
	}
	if raw.complete() || !raw.invalid {
		t.Fatal("exhaustion retained evidence")
	}
	reads := original.reads
	for i := 0; i < 3; i++ {
		n, next := reader.Read(make([]byte, 8))
		if n != 0 || next == nil || len(next.Error()) > 200 {
			t.Fatalf("exhausted read=%d, %v", n, next)
		}
	}
	if original.reads != reads {
		t.Fatal("reader drained beyond budget")
	}
}

func TestRawResponseBudgetProgress(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	// Each progress packet is only six bytes including outer framing, but the
	// cumulative wire is over budget before the small valid report arrives.
	wire := []byte(strings.Repeat("0006\x02x", testReportByteLimit/6+1))
	inner := rawFixtureWire(t, "unpack ok", "ok "+ref)
	// The inner report is small enough for either sideband mode.
	var tail bytes.Buffer
	enc := pktline.NewEncoder(&tail)
	if err := enc.Encode(append([]byte{1}, inner...)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	wire = append(wire, tail.Bytes()...)
	for _, mode := range []sideband.Type{sideband.Sideband, sideband.Sideband64k} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			original := &chunkErrorReader{data: wire, chunk: 65520, terminal: io.EOF}
			raw := &rawReport{ref: ref, sideband: true}
			reader := &rawReportReader{reader: original, report: raw}
			decoded := packp.NewReportStatus()
			err := decoded.Decode(sideband.NewDemuxer(mode, reader))
			read := len(wire) - len(original.data)
			t.Logf("progress decoder bytes=%d statuses=%d error=%v", read, len(decoded.CommandStatuses), err)
			if err == nil || read != testReportByteLimit || len(decoded.CommandStatuses) != 0 || raw.complete() || !raw.invalid {
				t.Fatalf("progress escaped budget: bytes=%d statuses=%d raw=%+v error=%v", read, len(decoded.CommandStatuses), raw, err)
			}
		})
	}
}

func TestRawResponseBudgetInvalidatesCompleteEvidence(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	for _, marker := range []string{"ok " + ref, "ng " + ref + " non-fast-forward"} {
		t.Run(marker, func(t *testing.T) {
			wire := rawFixtureWire(t, "unpack ok", marker)
			original := &chunkErrorReader{data: wire, chunk: len(wire), terminal: nil}
			raw := &rawReport{ref: ref}
			reader := &rawReportReader{reader: original, report: raw}
			if n, err := reader.Read(make([]byte, len(wire))); n != len(wire) || err != nil || !raw.complete() {
				t.Fatalf("within-limit evidence lost: n=%d error=%v raw=%+v", n, err, raw)
			}
			// Valid channel-2 progress after a complete inner report preserves
			// evidence until the cumulative outer response budget is exhausted.
			raw.sideband = true
			remaining := testReportByteLimit - len(wire)
			original.data = []byte(strings.Repeat("0006\x02x", remaining/6+2))
			original.chunk = len(original.data)
			n, err := reader.Read(make([]byte, len(original.data)))
			if n != remaining || err == nil || raw.complete() || !raw.invalid {
				t.Fatalf("complete evidence survived exhaustion: n=%d error=%v raw=%+v", n, err, raw)
			}
			reads := original.reads
			if n, err := reader.Read(nil); n != 0 || err != nil || original.reads != reads {
				t.Fatalf("zero-length exhausted read: n=%d error=%v reads=%d", n, err, original.reads)
			}
		})
	}
}

func TestRawResponseBudgetZeroReadAndOriginalError(t *testing.T) {
	cause := errors.New("original reader failure")
	original := &chunkErrorReader{data: []byte("000dunpack ok"), chunk: 32, terminal: cause}
	reader := &rawReportReader{reader: original, report: &rawReport{}}
	if n, err := reader.Read(nil); n != 0 || err != nil || original.reads != 0 {
		t.Fatalf("zero read touched source: n=%d error=%v reads=%d", n, err, original.reads)
	}
	got := make([]byte, 32)
	n, err := reader.Read(got)
	if err != cause || !bytes.Equal(got[:n], []byte("000dunpack ok")) {
		t.Fatalf("changed original read: n=%d bytes=%q error=%v", n, got[:n], err)
	}
	original = &chunkErrorReader{data: bytes.Repeat([]byte("x"), testReportByteLimit), chunk: testReportByteLimit, terminal: cause}
	reader = &rawReportReader{reader: original, report: &rawReport{}}
	n, err = reader.Read(make([]byte, testReportByteLimit))
	if n != testReportByteLimit || !errors.Is(err, cause) || !errors.Is(err, errReportResponseLimit) {
		t.Fatalf("boundary dropped cause: n=%d error=%v", n, err)
	}
}
