package handler

import (
	"strings"
	"testing"
)

// runInspector drives an inspector over one body the way Write does (Begin on the head, Chunk, End)
// and returns its detected type and the first refusal.
func runInspector(in *v1UploadInspector, body []byte) (string, error) {
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	ct, err := in.Begin(head)
	if err != nil {
		return "", err
	}
	if err := in.Chunk(body); err != nil {
		return "", err
	}
	return ct, in.End()
}

// TestOutputInspectorAllowsHTMLOnlyForOutputs: the output allowlist is the input allowlist plus
// HTML (UTF-8 text with no NUL); the input inspector still refuses HTML exactly as before, whether
// the type is declared by the extension or by the part's Content-Type.
func TestOutputInspectorAllowsHTMLOnlyForOutputs(t *testing.T) {
	page := []byte("<!doctype html><title>t</title><p>caf\xc3\xa9</p>")
	for _, name := range []string{"a.html", "A.HTM", "noext"} {
		in := &v1UploadInspector{declared: v1OutputDeclaredTypes(name), allowHTML: true}
		ct, err := runInspector(in, page)
		if err != nil {
			t.Fatalf("output %s: %v", name, err)
		}
		want := "text/html"
		if name == "noext" {
			want = "text/plain"
		}
		if ct != want {
			t.Errorf("output %s: type = %q, want %q", name, ct, want)
		}
	}
	for _, c := range []struct{ ctype, fname string }{{"", "a.html"}, {"text/html", "a.txt"}, {"text/html", ""}} {
		in := &v1UploadInspector{declared: v1DeclaredTypes(c.ctype, c.fname)}
		if _, err := runInspector(in, page); err == nil {
			t.Errorf("input (%q, %q) accepted an HTML upload", c.ctype, c.fname)
		}
	}
	for name, body := range map[string][]byte{
		"nul":         []byte("<p>\x00</p>"),
		"bad utf8":    {'<', 'p', '>', 0xff},
		"pdf as html": []byte("%PDF-1.7 body"),
	} {
		in := &v1UploadInspector{declared: v1OutputDeclaredTypes("x.html"), allowHTML: true}
		if _, err := runInspector(in, body); err == nil {
			t.Errorf("output html %s was accepted", name)
		}
	}
}

// TestOutputRefusalNamesHTMLAsAllowed: a refusal on an output that names the HTML type or lists the
// allowed extensions includes HTML, since the output allowlist takes it; an INPUT refusal still
// lists the input extensions only.
func TestOutputRefusalNamesHTMLAsAllowed(t *testing.T) {
	// A PDF declared as .html: the message names text/html as the declared type, not "unsupported".
	in := &v1UploadInspector{declared: v1OutputDeclaredTypes("x.html"), allowHTML: true}
	_, err := runInspector(in, []byte("%PDF-1.7 body"))
	if err == nil || !strings.Contains(err.Error(), "declared as text/html") {
		t.Fatalf("pdf declared as html: err = %v, want it to name text/html", err)
	}
	// An unknown extension on an output lists .html and .htm among the allowed extensions.
	in = &v1UploadInspector{declared: v1OutputDeclaredTypes("x.exe"), allowHTML: true}
	_, err = runInspector(in, []byte("plain text"))
	if err == nil || !strings.Contains(err.Error(), ".html") || !strings.Contains(err.Error(), ".htm,") {
		t.Fatalf("output .exe: err = %v, want the allowed extensions to include .html and .htm", err)
	}
	// The input inspector's list is unchanged.
	in = &v1UploadInspector{declared: v1DeclaredTypes("", "x.exe")}
	_, err = runInspector(in, []byte("plain text"))
	if err == nil || strings.Contains(err.Error(), "html") {
		t.Fatalf("input .exe: err = %v, want the input list without html", err)
	}
}

// TestOutputDeclaredTypesFromFullName: the output route derives the declared types from the full
// cleaned name (cleanUploadName), so a long name keeps its extension's meaning: a 300-byte .html
// name still declares HTML although its stored display name is bounded.
func TestOutputDeclaredTypesFromFullName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".html"
	clean := cleanUploadName(long)
	if got := v1OutputDeclaredTypes(clean); len(got) != 1 || got[0] != "text/html" {
		t.Fatalf("declared types of the full name = %v, want [text/html]", got)
	}
	if bounded := sanitizeUploadName(long); len(bounded) > v1FileDisplayNameMaxBytes || !strings.HasSuffix(bounded, ".html") {
		t.Fatalf("bounded name = %q", bounded)
	}
}
