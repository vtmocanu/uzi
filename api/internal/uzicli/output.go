// Package uzicli holds the reusable pieces of the uzi CLI: the API Client
// interface (with a fake for tests), config/credentials handling, and output
// rendering. It is a leaf package — stdlib plus a TOML decoder only — so the
// CLI binary never links the server's pgx/chi stack (Success Criterion 8).
package uzicli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// Exit codes, documented in the SKILL.md so agents branch without parsing prose
// (PRD #64). Every RunE in cmd/uzi returns nil or an *ExitError carrying one of
// these; cobra's own flag/usage parse errors are the only plain errors that
// reach ExitCodeFor, and they map to ExitUsage.
const (
	ExitOK          = 0 // success
	ExitGeneric     = 1 // generic error
	ExitUsage       = 2 // usage error
	ExitAuth        = 3 // auth required / invalid / wrong scope
	ExitNotFound    = 4 // not found
	ExitConflict    = 5 // conflict (e.g. run finished)
	ExitUnreachable = 6 // server unreachable / 5xx
)

// ExitError carries the process exit code a command should produce. main() maps
// it via ExitCodeFor.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Exitf builds an *ExitError with a formatted message.
func Exitf(code int, format string, a ...any) *ExitError {
	return &ExitError{Code: code, Err: fmt.Errorf(format, a...)}
}

// ExitCodeFor resolves the process exit code for an error returned by Execute.
// A nil error is success; an *ExitError contributes its code; a cobra
// argument/command parse error is a usage error (2); anything else defaults to
// the generic error (1).
//
// The default is 1, NOT 2 (usage): every command RunE and the live HTTPClient
// wrap real failures in *ExitError, so a raw error reaching here should be a
// cobra usage error — but if some future path leaks an unwrapped network/decode
// error, "generic" is the honest fallback. Telling an agent "you invoked me
// wrong" (2) when the server was unreachable would be a lie. Flag parse errors
// are wrapped into *ExitError{ExitUsage} by the root's FlagErrorFunc and take the
// errors.As branch; the isCobraUsageError check catches the argument/command
// errors cobra returns unwrapped (it exposes no typed error for them).
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if isCobraUsageError(err) {
		return ExitUsage
	}
	return ExitGeneric
}

// isCobraUsageError reports whether err is one of cobra's own argument/command
// parse errors — unknown command, wrong arg count, invalid argument. Cobra
// exposes no typed error for these, so we match its stable message prefixes.
// Flag parse errors do not reach here: the root's FlagErrorFunc wraps them into
// *ExitError first.
func isCobraUsageError(err error) bool {
	msg := err.Error()
	for _, prefix := range []string{
		"unknown command",
		"unknown flag",
		"unknown shorthand flag",
		"invalid argument",
		"required flag",
		"accepts ",
		"requires ",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// Format selects the rendering of a command's result.
type Format int

const (
	FormatTable Format = iota // human tables on a TTY
	FormatJSON                // machine-readable, for agents (--json)
)

// Printer renders command output either as an indented JSON document (--json)
// or as an aligned text table. Colour and spinners are disabled on a non-TTY or
// when NO_COLOR/--no-color is set, but a pipe never auto-switches to JSON — a
// silent format change is a footgun, so agents pass --json explicitly (PRD #64).
type Printer struct {
	out    io.Writer
	Format Format
	Color  bool
	Quiet  bool
}

// NewPrinter builds a Printer. tty reports whether the destination is a
// terminal; colour is enabled only for a coloured table on a TTY with colour
// not otherwise suppressed.
func NewPrinter(out io.Writer, tty, jsonFlag, noColor, quiet bool) *Printer {
	f := FormatTable
	if jsonFlag {
		f = FormatJSON
	}
	color := tty && !noColor && os.Getenv("NO_COLOR") == "" && f == FormatTable
	return &Printer{out: out, Format: f, Color: color, Quiet: quiet}
}

// JSON writes v as an indented JSON document.
func (p *Printer) JSON(v any) error {
	enc := json.NewEncoder(p.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Table writes an aligned table. An empty headers slice omits the header row.
func (p *Printer) Table(headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(p.out, 0, 2, 2, ' ', 0)
	if len(headers) > 0 {
		cols := headers
		if p.Color {
			cols = make([]string, len(headers))
			for i, h := range headers {
				cols[i] = "\x1b[1m" + h + "\x1b[0m"
			}
		}
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// Printf writes a formatted line to the output. It is the escape hatch for the
// commands whose human view is neither a flat table nor a JSON document (run
// detail, run logs, the review verdict + recommendation blocks): routing all
// output through the Printer keeps a single destination.
func (p *Printer) Printf(format string, a ...any) {
	fmt.Fprintf(p.out, format, a...)
}

// Println writes its arguments followed by a newline to the output.
func (p *Printer) Println(a ...any) {
	fmt.Fprintln(p.out, a...)
}

// IsTerminal reports whether f is attached to a character device (a terminal).
// Stdlib-only, so uzicli stays a leaf; good enough to gate colour and spinners.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
