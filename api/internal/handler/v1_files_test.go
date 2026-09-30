package handler

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSanitizeUploadName: the display name is the basename only (both separators), free of control
// and invisible characters, bounded, never empty, and valid under the workersvc name rules. Every
// invisible character is built from its code point so none sits in the source.
func TestSanitizeUploadName(t *testing.T) {
	rlo := string(rune(0x202e))
	zwsp := string(rune(0x200b))
	bom := string(rune(0xfeff))
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\x\y.pdf`, "y.pdf"},
		{`..\..\win.ini`, "win.ini"},
		{"a/b\\c.txt", "c.txt"},
		{"re" + rlo + "port.txt", "report.txt"},
		{bom + "z" + zwsp + "ero.md", "zero.md"},
		{"tab\there\nnl.txt", "tabherenl.txt"},
		{"  padded.csv  ", "padded.csv"},
		{"", "upload"},
		{"dir/", "upload"},
		{"..", "upload"},
		{".", "upload"},
		{"   ", "upload"},
		{rlo + zwsp, "upload"},
		{"résumé 報告.md", "résumé 報告.md"},
		{"bad\xffbytes.txt", "badbytes.txt"},
	}
	for _, c := range cases {
		if got := sanitizeUploadName(c.in); got != c.want {
			t.Errorf("sanitizeUploadName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("é", 400) + ".txt"
	got := sanitizeUploadName(long)
	if len(got) > v1FileDisplayNameMaxBytes || !utf8.ValidString(got) || got == "" {
		t.Errorf("long name not bounded on a rune boundary: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}

// TestV1UploadInspector drives the inspector the way Write does (Begin with the head, Chunk per
// chunk, End) over the type matrix.
func TestV1UploadInspector(t *testing.T) {
	pdf := []byte("%PDF-1.7\n%%EOF\n")
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 16)...)
	jpg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F'}
	zip := []byte{'P', 'K', 3, 4, 0, 0, 0, 0}
	const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	const xlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	cases := []struct {
		name         string
		ctype, fname string
		body         []byte
		want         string // "" means refused
	}{
		{"pdf by ext", "", "a.pdf", pdf, "application/pdf"},
		{"pdf by octet-stream", "application/octet-stream", "a.pdf", pdf, "application/pdf"},
		{"pdf undeclared", "", "upload", pdf, "application/pdf"},
		{"png", "image/png", "a.png", png, "image/png"},
		{"jpeg", "image/jpeg", "a.jpeg", jpg, "image/jpeg"},
		{"docx", "", "a.docx", zip, docx},
		{"xlsx by type", xlsx, "upload", zip, xlsx},
		{"zip undeclared", "", "a.zip", zip, ""},
		{"zip bare", "", "upload", zip, ""},
		{"png named pdf", "", "a.pdf", png, ""},
		{"pdf typed png", "image/png", "a.png", pdf, ""},
		{"pdf named txt", "", "a.txt", pdf, ""},
		{"pe named pdf", "", "a.pdf", []byte("MZ\x90\x00\x03"), ""},
		{"text plain", "text/plain; charset=utf-8", "a.txt", []byte("héllo\n"), "text/plain"},
		{"text undeclared", "", "upload", []byte("plain words"), "text/plain"},
		{"markdown", "", "a.md", []byte("# title\n"), "text/markdown"},
		{"csv", "text/csv", "a.csv", []byte("a,b\n1,2\n"), "text/csv"},
		{"csv generic type", "text/plain", "a.csv", []byte("a,b\n"), "text/csv"},
		{"csv vs md", "text/csv", "a.md", []byte("a,b\n"), ""},
		{"json", "application/json", "a.json", []byte(`{"a":[1,2]}`), "application/json"},
		{"bad json", "", "a.json", []byte(`{"a":`), ""},
		{"html declared", "text/html", "a.html", []byte("<p>x</p>"), ""},
		{"exe ext text", "", "a.exe", []byte("hello"), ""},
		{"nul in text", "", "a.txt", []byte("ab\x00cd"), ""},
		{"invalid utf8", "", "a.txt", []byte("ab\xff\xfecd"), ""},
		{"text typed pdf", "application/pdf", "a.pdf", []byte("just text"), ""},
		{"malformed ctype", "not a media type;;", "a.txt", []byte("x"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := &v1UploadInspector{declared: v1DeclaredTypes(c.ctype, c.fname)}
			head := c.body
			if len(head) > 512 {
				head = head[:512]
			}
			got, err := in.Begin(head)
			if err == nil {
				err = in.Chunk(c.body)
			}
			if err == nil {
				err = in.End()
			}
			if c.want == "" {
				if err == nil {
					t.Fatalf("accepted as %q, want a refusal", got)
				}
				var rr workersvc.RefusalReasoner
				if !errors.As(err, &rr) || rr.RefusalReason() != v1ReasonUnsupported {
					t.Fatalf("refusal %v does not carry reason %q", err, v1ReasonUnsupported)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// TestV1UploadInspectorSplitRunes: a multi-byte character split across a chunk boundary is valid;
// a truncated one at the end, and JSON split across chunks, are judged over the whole stream.
func TestV1UploadInspectorSplitRunes(t *testing.T) {
	text := []byte("a€b") // € is 3 bytes: split it 1 + 2 and 2 + 1.
	for cut := 2; cut <= 3; cut++ {
		in := &v1UploadInspector{}
		if _, err := in.Begin(text[:cut]); err != nil {
			t.Fatal(err)
		}
		if err := in.Chunk(text[:cut]); err != nil {
			t.Fatalf("cut %d first chunk: %v", cut, err)
		}
		if err := in.Chunk(text[cut:]); err != nil {
			t.Fatalf("cut %d second chunk: %v", cut, err)
		}
		if err := in.End(); err != nil {
			t.Fatalf("cut %d end: %v", cut, err)
		}
	}
	trunc := &v1UploadInspector{}
	_, _ = trunc.Begin(text[:2])
	if err := trunc.Chunk(text[:2]); err != nil {
		t.Fatal(err)
	}
	if err := trunc.End(); err == nil {
		t.Error("a file ending mid-character was accepted")
	}
	js := &v1UploadInspector{declared: []string{v1TypeJSON}}
	_, _ = js.Begin([]byte(`{"a":`))
	_ = js.Chunk([]byte(`{"a":`))
	_ = js.Chunk([]byte(`[1]}`))
	if err := js.End(); err != nil {
		t.Errorf("valid JSON split across chunks refused: %v", err)
	}
}

// TestSanitizeUploadNameKeepsExtension (B2): a long name is shortened in its STEM and keeps its
// extension, and the declared type derives from the full untruncated name.
func TestSanitizeUploadNameKeepsExtension(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("r", 196) + ".v2.pdf",
		strings.Repeat("報", 67) + ".pdf",
		strings.Repeat("x", 500) + ".pdf",
		strings.Repeat("é", 300) + ".markdown",
	} {
		got := sanitizeUploadName(in)
		ext := in[strings.LastIndex(in, "."):]
		if !strings.HasSuffix(got, ext) || len(got) > v1FileDisplayNameMaxBytes || !utf8.ValidString(got) {
			t.Errorf("sanitizeUploadName(%d bytes ending %q) = %d bytes %q: want <= %d bytes, valid UTF-8, keeping the extension",
				len(in), ext, len(got), got, v1FileDisplayNameMaxBytes)
		}
		if d := v1DeclaredTypes("", cleanUploadName(in)); len(d) != 1 || strings.HasPrefix(d[0], "unsupported/") {
			t.Errorf("declared types of the untruncated %q = %v, want the extension's type", ext, d)
		}
	}
	// A "extension" too long to be one is cut like the rest of the name, never kept whole.
	long := "a." + strings.Repeat("z", 300)
	if got := sanitizeUploadName(long); len(got) > v1FileDisplayNameMaxBytes {
		t.Errorf("a name with a %d-byte extension was left at %d bytes", 300, len(got))
	}
	// A name with only an extension survives.
	if got := sanitizeUploadName(".pdf"); got != ".pdf" {
		t.Errorf("sanitizeUploadName(.pdf) = %q", got)
	}
}

// TestV1UploadInspectorAliasesAndDetail: the accepted aliases, the BOM, and the refusal messages
// (fixed text; an unknown extension lists the allowed ones and echoes nothing).
func TestV1UploadInspectorAliasesAndDetail(t *testing.T) {
	run := func(ctype, fname string, body []byte) (string, error) {
		in := &v1UploadInspector{declared: v1DeclaredTypes(ctype, fname)}
		got, err := in.Begin(body)
		if err == nil {
			err = in.Chunk(body)
		}
		if err == nil {
			err = in.End()
		}
		return got, err
	}
	ok := []struct {
		ctype, fname, want string
		body               []byte
	}{
		{"application/json", "a.json", "application/json", append([]byte("\xef\xbb\xbf"), `{"a":1}`...)},
		{"application/vnd.ms-excel", "a.csv", "text/csv", []byte("a,b\n1,2\n")},
		{"text/x-markdown", "a.md", "text/markdown", []byte("# t\n")},
		{"image/jpg", "a.jpg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0}},
	}
	for _, c := range ok {
		if got, err := run(c.ctype, c.fname, c.body); err != nil || got != c.want {
			t.Errorf("%s %s: got %q, %v; want %q", c.ctype, c.fname, got, err, c.want)
		}
	}
	if _, err := run("application/vnd.ms-excel", "a.txt", []byte("a,b\n")); err == nil {
		t.Error("the excel alias was accepted for a non-.csv name")
	}
	detail := []struct {
		ctype, fname, want string
		body               []byte
	}{
		{"", "a.txt", "text files must not contain NUL bytes", []byte("a\x00b")},
		{"", "a.json", "declared as JSON but is not valid JSON", []byte(`{"a":`)},
		{"text/csv", "a.md", "the declared type and the file extension disagree", []byte("a")},
		{"", "a.exe", ".csv, .docx, .jpeg, .jpg, .json, .markdown, .md, .pdf, .png, .txt, .xlsx", []byte("hi")},
	}
	for _, c := range detail {
		_, err := run(c.ctype, c.fname, c.body)
		var d workersvc.RefusalDetailer
		if err == nil || !errors.As(err, &d) || !strings.Contains(d.RefusalDetail(), c.want) {
			t.Errorf("%s %s: refusal %v, want a detail containing %q", c.ctype, c.fname, err, c.want)
		}
	}
	_, err := run("application/x-qzqz", "a.qzqz", []byte("hi"))
	if err == nil || strings.Contains(err.Error(), "qzqz") {
		t.Errorf("the refusal for an unknown type echoes client input: %v", err)
	}
}

func TestParseV1FileSize(t *testing.T) {
	for _, s := range []string{"0", "5", "26214400", "007"} {
		if _, err := parseV1FileSize(s); err != nil {
			t.Errorf("parseV1FileSize(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "+5", "-5", " 5", "5 ", "0x5", "5.0", "1_0", "99999999999999999999"} {
		if _, err := parseV1FileSize(s); err == nil {
			t.Errorf("parseV1FileSize(%q) accepted", s)
		}
	}
}
