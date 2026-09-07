package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseControlLineSnapshot(t *testing.T) {
	op, err := parseControlLine([]byte(`{"op":"snapshot","id":7}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op.Op != opSnapshot || op.ID != 7 {
		t.Errorf("got %+v, want snapshot id 7", op)
	}
}

func TestParseControlLineDisposeClamp(t *testing.T) {
	tests := []struct {
		name string
		line string
		want int
	}{
		{"in-range", `{"op":"dispose","id":1,"timeoutMs":3000}`, 3000},
		{"over-max", `{"op":"dispose","id":1,"timeoutMs":99999}`, maxDisposeTimeoutMs},
		{"negative", `{"op":"dispose","id":1,"timeoutMs":-5}`, 0},
		{"absent", `{"op":"dispose","id":1}`, defaultDisposeTimeoutMs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, err := parseControlLine([]byte(tc.line))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if op.Op != opDispose {
				t.Fatalf("op = %q, want dispose", op.Op)
			}
			if op.TimeoutMs != tc.want {
				t.Errorf("TimeoutMs = %d, want %d", op.TimeoutMs, tc.want)
			}
		})
	}
}

func TestParseControlLineRejections(t *testing.T) {
	tests := []struct {
		name    string
		line    []byte
		wantErr error
	}{
		{"malformed", []byte(`{not json`), errMalformedControl},
		{"unknown-op", []byte(`{"op":"nuke","id":1}`), errUnknownOp},
		{"empty-op", []byte(`{"id":1}`), errUnknownOp},
		{"oversized", append([]byte(`{"op":"snapshot","id":1,"x":"`), bytes.Repeat([]byte("a"), maxControlLine+1)...), errControlOversized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseControlLine(tc.line)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestControlReaderFrames(t *testing.T) {
	r := &controlReader{r: strings.NewReader("{\"op\":\"snapshot\",\"id\":1}\n{\"op\":\"dispose\",\"id\":2}\n")}
	first, err := r.readLine()
	if err != nil {
		t.Fatalf("first readLine: %v", err)
	}
	if string(first) != `{"op":"snapshot","id":1}` {
		t.Errorf("first frame = %q", first)
	}
	second, err := r.readLine()
	if err != nil {
		t.Fatalf("second readLine: %v", err)
	}
	if string(second) != `{"op":"dispose","id":2}` {
		t.Errorf("second frame = %q", second)
	}
	if _, err := r.readLine(); !errors.Is(err, errControlEOF) {
		t.Errorf("third readLine err = %v, want errControlEOF", err)
	}
}

func TestControlReaderEOFOnPartialLine(t *testing.T) {
	// A trailing frame with no newline at EOF is abnormal, not a frame.
	r := &controlReader{r: strings.NewReader(`{"op":"snapshot","id":1}`)}
	if _, err := r.readLine(); !errors.Is(err, errControlEOF) {
		t.Errorf("err = %v, want errControlEOF", err)
	}
}

func TestControlReaderOversizedNoNewline(t *testing.T) {
	// Unbounded buffer growth without a terminator is rejected as oversized.
	huge := strings.Repeat("a", maxControlLine+50)
	r := &controlReader{r: strings.NewReader(huge)}
	if _, err := r.readLine(); !errors.Is(err, errControlOversized) {
		t.Errorf("err = %v, want errControlOversized", err)
	}
}

func TestControlReaderOversizedCompletedLine(t *testing.T) {
	// A completed line longer than the ceiling is oversized even with a newline.
	huge := strings.Repeat("a", maxControlLine+5) + "\n"
	r := &controlReader{r: strings.NewReader(huge)}
	if _, err := r.readLine(); !errors.Is(err, errControlOversized) {
		t.Errorf("err = %v, want errControlOversized", err)
	}
}
