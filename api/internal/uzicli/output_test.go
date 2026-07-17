package uzicli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"exit-error", Exitf(ExitNotFound, "gone"), ExitNotFound},
		{"wrapped-exit-error", errWrap(Exitf(ExitConflict, "busy")), ExitConflict},
		// A plain error that is NOT a cobra usage error defaults to generic (1),
		// not usage (2): a leaked network/decode error must not read as "you
		// invoked me wrong".
		{"plain-error", errors.New("boom"), ExitGeneric},
		// Cobra's own argument/command parse errors (returned unwrapped) are usage.
		{"cobra-unknown-command", errors.New(`unknown command "bogus" for "uzi"`), ExitUsage},
		{"cobra-accepts", errors.New("accepts 1 arg(s), received 0"), ExitUsage},
		{"cobra-required-flag", errors.New(`required flag(s) "id" not set`), ExitUsage},
	}
	for _, tc := range cases {
		if got := ExitCodeFor(tc.err); got != tc.want {
			t.Errorf("%s: ExitCodeFor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func errWrap(err error) error { return &wrapper{err} }

type wrapper struct{ err error }

func (w *wrapper) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapper) Unwrap() error { return w.err }

func TestPrinterJSON(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, false, true, false, false)
	if p.Format != FormatJSON {
		t.Fatalf("Format = %v, want JSON", p.Format)
	}
	if err := p.JSON(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"a": 1`) {
		t.Errorf("JSON output = %q", buf.String())
	}
}

func TestPrinterTablePlain(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, false, false, false, false) // non-TTY ⇒ no colour
	if p.Color {
		t.Fatal("colour must be off on a non-TTY")
	}
	if err := p.Table([]string{"ID", "NAME"}, [][]string{{"1", "alpha"}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ID") || !strings.Contains(out, "alpha") {
		t.Errorf("table = %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("table contains ANSI escapes on a non-TTY: %q", out)
	}
}

func TestColourGating(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	// TTY + table + colour allowed ⇒ on.
	if !NewPrinter(nil, true, false, false, false).Color {
		t.Error("want colour on for TTY table")
	}
	// --no-color ⇒ off.
	if NewPrinter(nil, true, false, true, false).Color {
		t.Error("--no-color must disable colour")
	}
	// JSON ⇒ off regardless of TTY.
	if NewPrinter(nil, true, true, false, false).Color {
		t.Error("JSON output must never be coloured")
	}
	// NO_COLOR set ⇒ off.
	t.Setenv("NO_COLOR", "1")
	if NewPrinter(nil, true, false, false, false).Color {
		t.Error("NO_COLOR must disable colour")
	}
}
