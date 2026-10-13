package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func graph() *apitypes.PrDescriptionDiagram {
	return &apitypes.PrDescriptionDiagram{
		Kind: "flow", Title: "Request <b>path</b>",
		Nodes: []apitypes.PrDescriptionDiagramNode{
			{Key: "request", Label: "Incoming <b>request</b>"},
			{Key: "service", Label: "Service"},
			{Key: "store", Label: "Store"},
		},
		Edges: []apitypes.PrDescriptionDiagramEdge{
			{From: "request", To: "service", Label: "Call"},
			{From: "service", To: "store", Label: "Write"},
		},
	}
}

func invoke(t *testing.T, fields apitypes.PrDescriptionFields) response {
	t.Helper()
	raw, err := json.Marshal(request{Fields: &fields})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if rc := run(bytes.NewReader(raw), &output); rc != 0 {
		t.Fatalf("exit %d: %s", rc, output.String())
	}
	if output.Len() > maxOutputBytes {
		t.Fatal("output exceeded byte cap")
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestValidGraphAndRealProseSanitization(t *testing.T) {
	got := invoke(t, apitypes.PrDescriptionFields{
		Summary: "Fixes #7 and @reviewer <b>notes</b>",
		Changes: []string{"[Details](https://example.com)", "<!-- removed -->"},
		Verification: []apitypes.PrDescriptionVerification{
			{Command: "go test ./...", Result: "pass", VerifiedAtSha: "ABCDEF1"},
		},
		Diagram: graph(),
	})
	want := &apitypes.PrDescriptionDiagram{
		Kind: "flow", Title: "Request path",
		Nodes: []apitypes.PrDescriptionDiagramNode{
			{Key: "request", Label: "Incoming request"},
			{Key: "service", Label: "Service"},
			{Key: "store", Label: "Store"},
		},
		Edges: []apitypes.PrDescriptionDiagramEdge{
			{From: "request", To: "service", Label: "Call"},
			{From: "service", To: "store", Label: "Write"},
		},
	}
	if got.DiagramRejected || !reflect.DeepEqual(got.Fields.Diagram, want) {
		t.Fatalf("valid graph was not sanitized and preserved: %+v", got)
	}
	if got.Fields.Summary != "F\u200Bixes #7 and @\u200Breviewer notes" ||
		!reflect.DeepEqual(got.Fields.Changes, []string{"Details"}) ||
		got.Fields.Verification[0].VerifiedAtSha != "abcdef1" {
		t.Fatalf("public sanitizer behavior absent: %+v", got.Fields)
	}
	if got.Fields.ScopeNotes == nil || got.Fields.ReviewPointers == nil {
		t.Fatal("sanitizer's non-nil empty lists were lost")
	}
}

func TestWholeGraphRejection(t *testing.T) {
	for name, mutate := range map[string]func(*apitypes.PrDescriptionDiagram){
		"normalized closing directive": func(d *apitypes.PrDescriptionDiagram) { d.Nodes[1].Label = "Fix<b></b>es #7" },
		"mention in edge":              func(d *apitypes.PrDescriptionDiagram) { d.Edges[1].Label = "@reviewer" },
		"unsafe title":                 func(d *apitypes.PrDescriptionDiagram) { d.Title = "Closes GH-7" },
		"duplicate key":                func(d *apitypes.PrDescriptionDiagram) { d.Nodes[2].Key = "service" },
		"unknown endpoint":             func(d *apitypes.PrDescriptionDiagram) { d.Edges[1].To = "missing" },
		"flow self edge":               func(d *apitypes.PrDescriptionDiagram) { d.Edges[1].To = "service" },
	} {
		t.Run(name, func(t *testing.T) {
			d := graph()
			mutate(d)
			got := invoke(t, apitypes.PrDescriptionFields{Summary: "Keep prose", Diagram: d})
			if !got.DiagramRejected || got.Fields.Diagram != nil || got.Fields.Summary != "Keep prose" {
				t.Fatalf("whole graph must drop while retaining prose: %+v", got)
			}
		})
	}
}

func TestAbsentDiagram(t *testing.T) {
	got := invoke(t, apitypes.PrDescriptionFields{Summary: "Prose only"})
	if got.DiagramRejected || got.Fields.Diagram != nil {
		t.Fatalf("absent diagram is not a rejection: %+v", got)
	}
}

func TestFixedJSONFailures(t *testing.T) {
	for name, input := range map[string]string{
		"empty":                    "",
		"broken":                   `{"fields":`,
		"missing fields":           `{}`,
		"null fields":              `{"fields":null}`,
		"null request":             `null`,
		"array":                    `[]`,
		"wrong field type":         `{"fields":{"summary":3}}`,
		"unknown field with label": `{"fields":{"private label":"do not echo"}}`,
		"multiple":                 `{"fields":{}} {"fields":{}}`,
		"trailing null":            `{"fields":{}} null`,
		"trailing junk":            `{"fields":{}} private-label`,
		"invalid utf8":             "{\"fields\":{\"summary\":\"" + string([]byte{0xff}) + "\"}}",
	} {
		t.Run(name, func(t *testing.T) {
			assertFailure(t, strings.NewReader(input), "invalid_json")
		})
	}
}

func assertFailure(t *testing.T, input io.Reader, class string) {
	t.Helper()
	var output bytes.Buffer
	want := "{\"error\":\"" + class + "\"}\n"
	if rc := run(input, &output); rc != 1 || output.String() != want {
		t.Fatalf("failure must contain only fixed class: exit %d, %q", rc, output.String())
	}
}

func TestAPIRejectionDoesNotLeakPartialFields(t *testing.T) {
	for name, fields := range map[string]apitypes.PrDescriptionFields{
		"oversize summary": {Summary: strings.Repeat("x", 4001)},
		"invalid enum":     {Summary: "private prose", ScopeNotes: []apitypes.PrDescriptionScopeNote{{Kind: "private-label", Text: "private text"}}},
		"invalid sha":      {Verification: []apitypes.PrDescriptionVerification{{Command: "private command", Result: "pass", VerifiedAtSha: "private-sha"}}},
		"raw diagram cap":  {Diagram: &apitypes.PrDescriptionDiagram{Title: strings.Repeat("x", 1001)}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(request{Fields: &fields})
			if err != nil {
				t.Fatal(err)
			}
			assertFailure(t, bytes.NewReader(raw), "api_rejected")
		})
	}
}

func TestInputByteBoundary(t *testing.T) {
	const valid = `{"fields":{}}`
	var output bytes.Buffer
	exact := valid + strings.Repeat(" ", maxInputBytes-len(valid))
	if rc := run(strings.NewReader(exact), &output); rc != 0 {
		t.Fatalf("exact byte cap failed: exit %d", rc)
	}
	assertFailure(t, strings.NewReader(exact+" "), "input_too_large")
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) {
	return 0, errors.New("private reader diagnostic")
}

type brokenWriter struct {
	calls int
	short bool
}

func (w *brokenWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("private writer diagnostic")
}

func TestIOFailures(t *testing.T) {
	assertFailure(t, brokenReader{}, "input_error")
	for _, short := range []bool{false, true} {
		w := &brokenWriter{short: short}
		if rc := run(strings.NewReader(`{"fields":{}}`), w); rc != 1 || w.calls != 1 {
			t.Fatalf("failed output should terminate without retry: exit %d, calls %d", rc, w.calls)
		}
	}
}

// endlessReader proves oversized input stops reading at the sentinel byte.
type endlessReader struct {
	read int
}

func (r *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.read += len(p)
	return len(p), nil
}

func TestReadIsBounded(t *testing.T) {
	r := &endlessReader{}
	assertFailure(t, r, "input_too_large")
	if r.read != maxInputBytes+1 {
		t.Fatalf("read must stop at cap plus sentinel: read %d", r.read)
	}
}

func TestAllRawCapsFitEscapedTransport(t *testing.T) {
	// '<' is escaped as six JSON bytes by encoding/json. Populate every raw
	// slot to its documented byte cap rather than testing only small graphs.
	item := strings.Repeat("<", 1000)
	fields := apitypes.PrDescriptionFields{
		Summary: strings.Repeat("<", 4000),
		Diagram: &apitypes.PrDescriptionDiagram{Kind: "flow", Title: item},
	}
	for i := 0; i < 50; i++ {
		fields.Changes = append(fields.Changes, item)
		fields.ReviewPointers = append(fields.ReviewPointers, item)
		fields.ScopeNotes = append(fields.ScopeNotes, apitypes.PrDescriptionScopeNote{Kind: "changed", Text: item})
		fields.Verification = append(fields.Verification, apitypes.PrDescriptionVerification{
			Command: item, Result: "pass", VerifiedAtSha: strings.Repeat("a", 64),
		})
		fields.Diagram.Nodes = append(fields.Diagram.Nodes, apitypes.PrDescriptionDiagramNode{Key: item, Label: item})
		fields.Diagram.Edges = append(fields.Diagram.Edges, apitypes.PrDescriptionDiagramEdge{From: item, To: item, Label: item})
	}
	got := invoke(t, fields)
	if !got.DiagramRejected || got.Fields.Diagram != nil {
		t.Fatal("raw caps permit transport; layout-invalid diagram must still drop")
	}
	if got.Fields.Summary != strings.Repeat("&lt;", 149)+"…" {
		t.Fatalf("real summary layout bound not applied: %q", got.Fields.Summary)
	}
}
