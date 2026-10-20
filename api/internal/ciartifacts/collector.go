package ciartifacts

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Collector is a single-owner, in-memory collection. It never materializes ZIP
// paths. The caller supplies a deadline and a scrubber covering its credentials.
type Collector struct {
	limits     Limits
	scrub      func(string) string
	manifest   Manifest
	inputs     []Input
	digests    map[string]Entry
	artifacts  map[string]bool
	retained   int64
	inflated   int64
	compressed int64
	reads      int
	stopped    bool
	// compressedProgress observes compressed consumption synchronously in tests.
	compressedProgress func()
}

func NewCollector(limits Limits, scrub func(string) string) *Collector {
	if scrub == nil {
		scrub = func(s string) string { return s }
	}
	zero := int64(0)
	return &Collector{limits: limits.Bounded(), scrub: scrub, manifest: Manifest{Omitted: &zero}, digests: map[string]Entry{}, artifacts: map[string]bool{}}
}

// SetExecution records scrubbed, bounded provenance before any rows are selected.
func (c *Collector) SetExecution(e Execution) {
	e.WorkflowKey, _ = boundedMetadata(c.scrub(e.WorkflowKey), 200)
	e.Workflow, _ = boundedMetadata(c.scrub(e.Workflow), 200)
	e.SHA, _ = boundedMetadata(c.scrub(e.SHA), 200)
	e.Branch, _ = boundedMetadata(c.scrub(e.Branch), 200)
	e.UpdatedAt, _ = boundedMetadata(c.scrub(e.UpdatedAt), 100)
	if e.Attempt != nil {
		n := *e.Attempt
		e.Attempt = &n
	}
	c.manifest.Execution = &e
}

// Result returns detached metadata and bytes. Inputs contain one representative
// source identity per scrubbed digest; kept aliases reference that same digest.
func (c *Collector) Result() (Manifest, []Input) {
	m := c.manifest
	m.Entries = append([]Entry{}, m.Entries...)
	m.Reasons = append([]string{}, m.Reasons...)
	if m.Execution != nil {
		e := *m.Execution
		if e.Attempt != nil {
			a := *e.Attempt
			e.Attempt = &a
		}
		m.Execution = &e
	}
	n := *m.Omitted
	m.Omitted = &n
	out := make([]Input, len(c.inputs))
	for i, in := range c.inputs {
		out[i] = in
		out[i].Content = bytes.Clone(in.Content)
	}
	return m, out
}

// Add inspects one complete downloaded ZIP. Refusals are fixed manifest reasons,
// never parser errors or download URLs. Reusing an artifact ID is ignored.
func (c *Collector) Add(ctx context.Context, artifactID, artifactName string, zipBytes []byte) {
	if c.artifacts[artifactID] {
		return
	}
	// Bound even a caller supplying an unlimited sequence of empty archives.
	if len(c.artifacts) >= 1000 {
		c.manifest.Uninspected = true
		return
	}
	c.artifacts[artifactID] = true
	// IDs are observed identities, not display labels; never silently rewrite one.
	if artifactID == "" || len(artifactID) > 200 || !utf8.ValidString(artifactID) || strings.ContainsFunc(artifactID, termsafe.Unsafe) {
		c.manifest.Uninspected = true
		return
	}
	name, shortened := boundedMetadata(c.scrub(artifactName), 200)
	base := Entry{ArtifactID: artifactID, ArtifactName: name, Shortened: shortened}
	refuse := func(reason string) {
		c.Unavailable(artifactID, artifactName, reason)
		e := base
		e.EntryOrdinal = 1000
		e.Status = "skipped"
		e.Reason = reason
		e.Summary = true
		c.record(e)
		c.manifest.Uninspected = true
	}
	if ctx.Err() != nil {
		refuse("deadline")
		return
	}
	if c.stopped {
		refuse("inflated_budget")
		return
	}
	if !preflightZIP(ctx, zipBytes) {
		if ctx.Err() != nil {
			refuse("deadline")
		} else {
			refuse("archive_safety")
		}
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		refuse("archive_safety")
		return
	}
	// ErrInsecurePath returns a usable bounded reader. Apply our per-entry
	// rejection so strict Go ZIP mode cannot discard unrelated safe evidence.
	// Per-reader overrides retain archive/zip\'s size, CRC and descriptor checks,
	// while exposing actual compressed consumption (not just header claims).
	zr.RegisterDecompressor(zip.Store, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(&compressedReader{Reader: r, ctx: ctx, count: &c.compressed, progress: c.compressedProgress})
	})
	zr.RegisterDecompressor(zip.Deflate, func(r io.Reader) io.ReadCloser {
		return flate.NewReader(&compressedReader{Reader: r, ctx: ctx, count: &c.compressed, progress: c.compressedProgress})
	})
	paths := make(map[string]bool)
	for i, f := range zr.File {
		if ctx.Err() != nil {
			refuse("deadline")
			return
		}
		if c.stopped {
			c.manifest.Uninspected = true
			return
		}
		e := base
		e.EntryOrdinal = i
		e.Status = "skipped"
		e.Path, shortened = boundedMetadata(c.scrub(f.Name), 512)
		e.Shortened = e.Shortened || shortened
		e.CompressedSize = f.CompressedSize64
		e.UncompressedSize = f.UncompressedSize64
		data, reason := c.inflate(ctx, f)
		if reason == "deadline" || reason == "inflated_budget" {
			c.Unavailable(artifactID, artifactName, reason)
			e.Reason = reason
			c.record(e)
			c.manifest.Uninspected = true
			return
		}
		if reason == "" {
			reason = classifyPath(f)
		}
		normalized := path.Clean(f.Name)
		if reason == "" && paths[normalized] {
			reason = "duplicate_path"
		}
		// A rejected first occurrence cannot reserve a normalized path.
		if reason == "" {
			paths[normalized] = true
		}
		if reason == "" && (!utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || bytes.ContainsFunc(data, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' })) {
			reason = "binary"
		}
		if reason == "" && len(data) == 0 {
			reason = "empty"
		}
		if reason == "" {
			data = []byte(c.scrub(string(data)))
			if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
				reason = "binary"
			}
			if int64(len(data)) > c.limits.MaxFileBytes {
				reason = "file_budget"
			}
			if len(data) == 0 {
				reason = "empty"
			}
		}
		if reason == "" {
			digest, safeName := Identity(data)
			previous, duplicate := c.digests[digest]
			if !duplicate && (len(c.inputs) >= c.limits.MaxFiles || c.retained+int64(len(data)) > c.limits.MaxBytes) {
				reason = "retention_budget"
			} else {
				e.Status = "kept"
				e.SHA256 = digest
				e.Name = safeName
				e.Size = int64(len(data))
				if !duplicate {
					c.digests[digest] = e
					c.retained += int64(len(data))
					c.inputs = append(c.inputs, Input{ArtifactID: artifactID, EntryOrdinal: i, Content: data})
				} else {
					e.Name = previous.Name
				}
			}
		}
		e.Reason = reason
		c.record(e)
	}
}

func boundedMetadata(s string, limit int) (string, bool) {
	q, short := renderBoundedQuote(s, limit)
	safe := unquoteMetadata(q)
	return safe, short || safe != s
}

// record reserves metadata for each unique attached file. Ordinary rows yield
// to kept references; each displaced enumerated row enters the exact skip count.
func (c *Collector) record(e Entry) {
	candidate := append(append([]Entry{}, c.manifest.Entries...), e)
	for {
		m := c.manifest
		m.Entries = candidate
		_, err := m.Normalize()
		if err == nil {
			c.manifest.Entries = candidate
			return
		}
		drop := -1
		for i, row := range candidate {
			if row.Status != "kept" {
				drop = i
				break
			}
		}
		if drop < 0 {
			seen := map[string]bool{}
			for i, row := range candidate {
				if seen[row.SHA256] {
					drop = i
					break
				}
				seen[row.SHA256] = true
			}
		}
		// Canonical references never yield to aliases or summaries. If display
		// metadata fills the byte budget, retain the verified identity alone.
		if drop < 0 {
			for i := range candidate {
				candidate[i].ArtifactName = ""
				candidate[i].Path = ""
				candidate[i].Label = ""
				candidate[i].Shortened = true
			}
			c.manifest.Uninspected = true
			m.Entries = candidate
			if _, err := m.Normalize(); err == nil {
				c.manifest.Entries = candidate
			}
			return
		}
		if candidate[drop].Status == "kept" {
			c.manifest.OmittedKeptAliases++
		} else if !candidate[drop].Summary {
			*c.manifest.Omitted++
		}
		candidate = append(candidate[:drop], candidate[drop+1:]...)
	}
}

// inflate charges every returned decompressor byte before classification. Each
// read is bounded by remaining work plus one shared overflow byte. A canceled
// caller or exhausted collection stops all later inflation, including archives.
func (c *Collector) inflate(ctx context.Context, f *zip.File) ([]byte, string) {
	c.compressed = 0
	if f.CompressedSize64 > 64<<20 || f.UncompressedSize64 > math.MaxInt64 {
		return nil, "invalid_entry"
	}
	expectedCompressed, expectedPlain := int64(f.CompressedSize64), int64(f.UncompressedSize64)
	r, err := f.Open()
	if err != nil {
		return nil, "invalid_entry"
	}
	defer func() { _ = r.Close() }() // size and CRC failures are reported by Read
	var data bytes.Buffer
	var total int64
	buf := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		remaining := c.limits.MaxInflatedBytes - c.inflated
		ratioRemaining := c.compressed*100 - total
		want := min(int64(len(buf)), remaining+1, ratioRemaining+1)
		c.reads++
		n, readErr := r.Read(buf[:int(want)])
		c.inflated += int64(n)
		total += int64(n)
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		if c.inflated > c.limits.MaxInflatedBytes {
			c.stopped = true
			return nil, "inflated_budget"
		}
		if total > c.compressed*100 || total > expectedCompressed*100 {
			return nil, "compression_ratio"
		}
		if total <= c.limits.MaxFileBytes {
			data.Write(buf[:n])
		}
		if readErr != nil {
			if readErr != io.EOF {
				return nil, "invalid_entry"
			}
			if total != expectedPlain || c.compressed != expectedCompressed {
				return nil, "invalid_entry"
			}
			if total > c.limits.MaxFileBytes {
				return nil, "file_budget"
			}
			return data.Bytes(), ""
		}
		// Readers supplied by archive/zip make progress or return an error.
	}
}

// ReadByte prevents flate from hiding compressed read-ahead in its own buffer.
type compressedReader struct {
	io.Reader
	ctx      context.Context
	count    *int64
	progress func()
}

func (r *compressedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(p)
	*r.count += int64(n)
	if n > 0 && r.progress != nil {
		r.progress()
	}
	return n, err
}
func (r *compressedReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}

func classifyPath(f *zip.File) string {
	s := f.Name
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) || s == "" || strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\\:") {
		return "unsafe_path"
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return "unsafe_path"
		}
	}
	if !f.Mode().IsRegular() {
		return "nonregular"
	}
	if path.Clean(s) == "." {
		return "unsafe_path"
	}
	return ""
}

func unquoteMetadata(q string) string {
	var s string
	// renderQuote emits Go escapes, including escapes JSON does not recognize.
	// strconv.Unquote is used in artifacts.go's display contract too.
	s, _ = strconv.Unquote(q)
	return s
}
