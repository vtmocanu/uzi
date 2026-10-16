package ciartifacts

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
)

type zipItem struct {
	name    string
	text    string
	mode    os.FileMode
	deflate bool
}

func zipFixture(t *testing.T, items ...zipItem) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, item := range items {
		h := &zip.FileHeader{Name: item.name, Method: zip.Store}
		if item.deflate {
			h.Method = zip.Deflate
		}
		if item.mode != 0 {
			h.SetMode(item.mode)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(item.text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestCollectorSafetyAndScrubbing(t *testing.T) {
	secret := "glpat-" + strings.Repeat("a", 20)
	c := NewCollector(Limits{}, func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") })
	items := []zipItem{
		{name: "../escape", text: "a"}, {name: "/absolute", text: "a"},
		{name: "C:/drive", text: "a"}, {name: "a\\b", text: "a"},
		{name: "link", text: "a", mode: os.ModeSymlink | 0777},
		{name: "device", text: "a", mode: os.ModeDevice | 0600},
		{name: "nul", text: "a\x00b"}, {name: "utf8", text: "\xff"},
		{name: "good", text: secret}, {name: "./good", text: "different"},
	}
	c.Add(context.Background(), "1", "logs\n`"+secret, zipFixture(t, items...))
	c.Add(context.Background(), "2", "second", zipFixture(t, zipItem{name: "another", text: secret}))
	m, in := c.Result()
	if len(in) != 1 || string(in[0].Content) != "[redacted]" {
		t.Fatalf("inputs: %+v", in)
	}
	for i, want := range []string{"unsafe_path", "unsafe_path", "unsafe_path", "unsafe_path", "nonregular", "nonregular", "binary", "binary", "", "duplicate_path", ""} {
		if m.Entries[i].Reason != want {
			t.Fatalf("entry %d: %+v", i, m.Entries[i])
		}
	}
	if c.inflated != int64(10+len(secret)*2+len("different")) {
		t.Fatalf("charged %d", c.inflated)
	}
	rendered, err := m.Render()
	if err != nil || strings.Contains(rendered, secret) || strings.Contains(rendered, "`") || !strings.Contains(rendered, "1 additional kept") {
		t.Fatalf("%s %v", rendered, err)
	}
	if m.Entries[8].SHA256 != m.Entries[10].SHA256 {
		t.Fatal("scrubbed digest mismatch")
	}
}

func TestCollectorAggregateWorkAndStoppedReads(t *testing.T) {
	for _, size := range []int{10, 11} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			c := NewCollector(Limits{MaxInflatedBytes: 10}, nil)
			c.Add(context.Background(), "a", "a", zipFixture(t, zipItem{name: "../bad", text: "12345"}))
			c.Add(context.Background(), "b", "b", zipFixture(t, zipItem{name: "x", text: strings.Repeat("x", size-5)}))
			if c.inflated != int64(size) || c.stopped != (size == 11) {
				t.Fatalf("work=%d stopped=%v", c.inflated, c.stopped)
			}
			if size == 10 {
				c.Add(context.Background(), "probe", "probe", zipFixture(t, zipItem{name: "x2", text: "xx"}))
			}
			reads := c.reads
			c.Add(context.Background(), "later", "later", zipFixture(t, zipItem{name: "y", text: "yyyy"}))
			if c.inflated != 11 || c.reads != reads {
				t.Fatalf("continued: work=%d reads=%d/%d", c.inflated, c.reads, reads)
			}
		})
	}
	c := NewCollector(Limits{MaxInflatedBytes: 9}, nil)
	c.Add(context.Background(), "a", "a", zipFixture(t, zipItem{name: "x", text: "1234"}))
	c.Add(context.Background(), "b", "b", zipFixture(t, zipItem{name: "y", text: "1234"}, zipItem{name: "z", text: "1234"}))
	_, in := c.Result()
	if c.inflated != 10 || len(in) != 1 {
		t.Fatalf("duplicates not charged: %d %d", c.inflated, len(in))
	}
}

func TestCollectorCRCFileBudgetRatioAndDeadline(t *testing.T) {
	corrupt := zipFixture(t, zipItem{name: "crc", text: "hello"})
	at := bytes.Index(corrupt, []byte("hello"))
	corrupt[at] = 'j'
	for _, tc := range []struct {
		name   string
		data   []byte
		limits Limits
		reason string
	}{
		{"crc", corrupt, Limits{}, "invalid_entry"},
		{"oversized", zipFixture(t, zipItem{name: "large", text: "12345"}), Limits{MaxFileBytes: 4}, "file_budget"},
		{"ratio", zipFixture(t, zipItem{name: "bomb", text: strings.Repeat("x", 1<<20), deflate: true}), Limits{}, "compression_ratio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCollector(tc.limits, nil)
			c.Add(context.Background(), "a", "a", tc.data)
			m, in := c.Result()
			if tc.name == "ratio" {
				compressed := m.Entries[0].CompressedSize
				if compressed > 64<<20 {
					t.Fatal("fixture exceeds archive bound")
					return
				}
				if c.inflated > int64(compressed)*100+1 {
					t.Fatalf("unbounded ratio probe: %d", c.inflated)
				}
			}
			if len(in) != 0 || len(m.Entries) != 1 || m.Entries[0].Reason != tc.reason {
				t.Fatalf("%+v %+v", m, in)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewCollector(Limits{}, nil)
	c.Add(ctx, "a", "a", corrupt)
	m, _ := c.Result()
	if c.reads != 0 || !m.Uninspected || m.Entries[0].Reason != "deadline" {
		t.Fatal(m)
	}
	// Cancel after the first entry has been scrubbed, without a real-time wait.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	c = NewCollector(Limits{}, func(s string) string {
		if s == "first content" {
			cancel()
		}
		return s
	})
	c.Add(ctx, "a", "a", zipFixture(t, zipItem{name: "first", text: "first content"}, zipItem{name: "second", text: "second content"}))
	m, in := c.Result()
	if len(in) != 1 || !m.Uninspected || c.inflated != 13 {
		t.Fatalf("%+v %d", m, c.inflated)
	}
}

func TestCollectorPreflight(t *testing.T) {
	good := zipFixture(t, zipItem{name: "x", text: "text"})
	central := bytes.Index(good, []byte{'P', 'K', 1, 2})
	end := len(good) - 22
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"entries", func(b []byte) { binary.LittleEndian.PutUint16(b[end+10:], 1001) }},
		{"offset", func(b []byte) { binary.LittleEndian.PutUint32(b[end+16:], 0xfffffff0) }},
		{"multidisk", func(b []byte) { b[end+4] = 1 }},
		{"encrypted", func(b []byte) { b[central+8] |= 1 }},
		{"zip64", func(b []byte) { binary.LittleEndian.PutUint32(b[central+24:], 0xffffffff) }},
		{"length", func(b []byte) { binary.LittleEndian.PutUint16(b[central+28:], 4097) }},
		{"local", func(b []byte) { binary.LittleEndian.PutUint32(b[central+42:], fixtureZIPOffset(t, len(b))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bytes.Clone(good)
			tc.mutate(b)
			if preflightZIP(context.Background(), b) {
				t.Fatal("unsafe preflight accepted")
			}
			c := NewCollector(Limits{}, nil)
			c.Add(context.Background(), "good", "good", good)
			c.Add(context.Background(), "bad", "bad", b)
			m, in := c.Result()
			if len(in) != 1 || c.inflated != 4 || !m.Uninspected || m.Entries[1].Reason != "archive_safety" {
				t.Fatalf("%+v", m)
			}
		})
	}
	if !preflightZIP(context.Background(), good) {
		t.Fatal("valid ZIP refused")
	}
	// Counts are checked before the standard parser can allocate its entry array.
	huge := bytes.Clone(good)
	binary.LittleEndian.PutUint16(huge[end+8:], 65535)
	binary.LittleEndian.PutUint16(huge[end+10:], 65535)
	if allocs := testing.AllocsPerRun(10, func() {
		if preflightZIP(context.Background(), huge) {
			panic("accepted")
		}
	}); allocs != 0 {
		t.Fatalf("preflight allocated %f", allocs)
	}
}

func TestCollectorRowsKeepLateEvidenceAndExactCounts(t *testing.T) {
	c := NewCollector(Limits{MaxFiles: 1, MaxBytes: 4}, nil)
	items := make([]zipItem, 0, 1000)
	for i := 0; i < 999; i++ {
		items = append(items, zipItem{name: fmt.Sprintf("../%d", i), text: "bad"})
	}
	items = append(items, zipItem{name: "late", text: "kept"})
	c.Add(context.Background(), "a", strings.Repeat("`", 200), zipFixture(t, items...))
	m, in := c.Result()
	if _, err := m.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 || len(m.Entries) != 201 || *m.Omitted != 799 || m.Uninspected {
		t.Fatalf("rows=%d omitted=%d inputs=%d unknown=%v", len(m.Entries), *m.Omitted, len(in), m.Uninspected)
	}
	got, err := m.RenderWithin(1800)
	if err != nil || !strings.Contains(got, "kept: file=") || len(got) > 1800 {
		t.Fatalf("%s %v", got, err)
	}
	shown := strings.Count(got, "skipped: unsafe_path")
	if !strings.Contains(got, fmt.Sprintf("%d more skipped", 999-shown)) {
		t.Fatal(got)
	}
}

// Err becomes canceled immediately after the first plaintext byte is charged.
type cancelOnRead struct {
	context.Context
	c *Collector
}

func (x cancelOnRead) Err() error {
	if x.c.inflated > 0 {
		return context.Canceled
	}
	return nil
}
func TestCollectorPaddedCompressedSize(t *testing.T) {
	b := zipFixture(t, zipItem{name: "x", text: "some evidence", deflate: true})
	central := bytes.Index(b, []byte{'P', 'K', 1, 2})
	end := len(b) - 22
	compressed := binary.LittleEndian.Uint32(b[central+20:])
	// Insert padding after the deflate stream but before its data descriptor.
	dataEnd := 30 + len("x") + int(compressed)
	padded := append(bytes.Clone(b[:dataEnd]), make([]byte, 100)...)
	padded = append(padded, b[dataEnd:]...)
	central += 100
	end += 100
	binary.LittleEndian.PutUint32(padded[central+20:], compressed+100)
	binary.LittleEndian.PutUint32(padded[end+16:], fixtureZIPOffset(t, central))
	// Descriptor size must agree too; actual deflate consumption still differs.
	binary.LittleEndian.PutUint32(padded[dataEnd+100+8:], compressed+100)
	c := NewCollector(Limits{}, nil)
	c.Add(context.Background(), "a", "a", padded)
	m, in := c.Result()
	if len(in) != 0 || len(m.Entries) != 1 || m.Entries[0].Reason != "invalid_entry" {
		t.Fatalf("%+v", m)
	}
}
func TestCollectorIncrementalCancellation(t *testing.T) {
	c := NewCollector(Limits{}, nil)
	ctx := cancelOnRead{Context: context.Background(), c: c}
	c.Add(ctx, "a", "a", zipFixture(t, zipItem{name: "large", text: strings.Repeat("x", 100000)}))
	m, in := c.Result()
	if c.reads != 1 || c.inflated != 1 || len(in) != 0 || !m.Uninspected {
		t.Fatalf("reads=%d work=%d manifest=%+v", c.reads, c.inflated, m)
	}
}

// Two empty stored DEFLATE blocks consume input without emitting plaintext.
// CreateRaw keeps this tiny deterministic fixture independent of flate's writer.
func TestCollectorCancellationWithoutOutput(t *testing.T) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	raw := []byte{0, 0, 0, 255, 255, 1, 0, 0, 255, 255}
	f, err := w.CreateRaw(&zip.FileHeader{Name: "empty", Method: zip.Deflate, CompressedSize64: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(raw); err != nil {
		t.Fatal(err)
	}
	later, err := w.CreateHeader(&zip.FileHeader{Name: "later", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := later.Write([]byte("must not read")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewCollector(Limits{}, nil)
	c.compressedProgress = cancel
	c.Add(ctx, "a", "a", b.Bytes())
	m, inputs := c.Result()
	if ctx.Err() == nil || c.compressed != 1 || c.inflated != 0 || len(inputs) != 0 ||
		!m.Uninspected || len(m.Entries) != 1 || m.Entries[0].Reason != "deadline" ||
		len(m.Reasons) != 1 || m.Reasons[0] != "deadline" || *m.Omitted != 0 {
		t.Fatalf("compressed=%d output=%d inputs=%d manifest=%+v", c.compressed, c.inflated, len(inputs), m)
	}
	reads := c.reads
	c.Add(ctx, "b", "b", zipFixture(t, zipItem{name: "later archive", text: "must not read"}))
	if c.reads != reads || c.inflated != 0 {
		t.Fatal("continued work after cancellation")
	}
}

func TestCollectorByteBoundAndAliases(t *testing.T) {
	c := NewCollector(Limits{}, nil)
	items := make([]zipItem, 0, 300)
	for i := 0; i < 250; i++ {
		items = append(items, zipItem{name: fmt.Sprintf("%04d/", i) + strings.Repeat("`", 4000), text: "same"})
	}
	for i := 0; i < 50; i++ {
		items = append(items, zipItem{name: fmt.Sprintf("kept-%d", i) + strings.Repeat("`", 4000), text: fmt.Sprint(i)})
	}
	// Split archives so aggregate central metadata stays within preflight bounds.
	for i := 0; i < len(items); i += 100 {
		c.Add(context.Background(), fmt.Sprint(i), strings.Repeat("`", 200), zipFixture(t, items[i:min(i+100, len(items))]...))
	}
	m, in := c.Result()
	if len(in) != 50 {
		t.Fatalf("inputs=%d", len(in))
	}
	normalized, err := m.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalized.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range in {
		_, name := Identity(in.Content)
		if !strings.Contains(got, name) {
			t.Fatalf("lost attached file %s", name)
		}
	}
	if len(got) > RenderMaxBytes || (*m.Omitted == 0 && m.OmittedKeptAliases == 0) {
		t.Fatal("bounds not exercised")
	}
	// Every omitted enumerated source plus retained rows accounts for all 300.
	if int64(len(m.Entries))+*m.Omitted+m.OmittedKeptAliases != 300 {
		t.Fatalf("rows=%d omitted=%d", len(m.Entries), *m.Omitted)
	}
}

func TestPreflightOverlappingLocalRecords(t *testing.T) {
	b := zipFixture(t, zipItem{name: "x", text: "x"}, zipItem{name: "x", text: "x"})
	central := bytes.Index(b, []byte{'P', 'K', 1, 2})
	second := central + 46 + 1
	binary.LittleEndian.PutUint32(b[second+42:], 0)
	if preflightZIP(context.Background(), b) {
		t.Fatal("overlapping local records accepted")
	}
}
func TestPreflightAggregateMetadataAndEOCD(t *testing.T) {
	items := make([]zipItem, 300)
	for i := range items {
		items[i] = zipItem{name: fmt.Sprint(i) + strings.Repeat("x", 4000), text: "x"}
	}
	b := zipFixture(t, items...)
	if preflightZIP(context.Background(), b) {
		t.Fatal("aggregate metadata accepted")
	}
	b = zipFixture(t, zipItem{name: "x", text: "x"})
	// A plausible later EOCD inside the real EOCD's comment must never let the
	// standard parser choose a different directory from preflight.
	binary.LittleEndian.PutUint16(b[len(b)-2:], 24)
	b = append(b, make([]byte, 24)...)
	copy(b[len(b)-24:], []byte{'P', 'K', 5, 6})
	if preflightZIP(context.Background(), b) {
		t.Fatal("ambiguous EOCD accepted")
	}
}

func TestCollectorArchiveSummariesNeverInventFileCounts(t *testing.T) {
	c := NewCollector(Limits{}, nil)
	for i := 0; i < MaxSkipped+2; i++ {
		c.Add(context.Background(), fmt.Sprint(i), "malformed", nil)
	}
	m, in := c.Result()
	if len(in) != 0 || len(m.Entries) != MaxSkipped || *m.Omitted != 0 || !m.Uninspected {
		t.Fatalf("%+v", m)
	}
	got, err := m.RenderWithin(200)
	if err != nil || !strings.Contains(got, "0 more skipped") || !strings.Contains(got, "count is unknown") {
		t.Fatalf("%s %v", got, err)
	}
}

func TestExpiredArtifactRenderEscapes(t *testing.T) {
	m := Manifest{Entries: []Entry{{ArtifactID: "a", ArtifactName: "logs\n```", Status: "skipped", Reason: "expired"}}}
	got, err := m.Render()
	if err != nil || !strings.Contains(got, "unavailable: expired") || strings.Contains(got, "```") || !strings.Contains(got, "artifact `logs�\\u0060\\u0060\\u0060` unavailable: expired\n") {
		t.Fatalf("%q %v", got, err)
	}
}
