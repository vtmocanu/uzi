package handler

import "testing"

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
