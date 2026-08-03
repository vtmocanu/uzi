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
//
// DELIBERATELY NOT SANITIZED, and that is not an oversight (#180). encoding/json
// escapes every control byte on its own — ESC leaves here as the six characters
// backslash-u-0-0-1-b — so the machine channel is already terminal-safe, and it is
// terminal-safe LOSSLESSLY: a consumer decodes back to the server's exact bytes.
// Running a stripper over it would destroy the one faithful record of what a hostile
// server actually sent, which is the opposite of what an agent-facing format is for.
func (p *Printer) JSON(v any) error {
	enc := json.NewEncoder(p.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Table writes an aligned table. An empty headers slice omits the header row.
//
// Every header and every cell goes through CellText first: this is the shared
// human-table boundary all seventeen table call sites reach, and it is where a
// server-supplied string stops being able to drive the terminal (#180). Sanitizing
// here rather than per-call-site means a new command cannot forget it, and it holds
// even for cells whose value is "server-controlled today" — the assumption this repo
// has watched rot before.
//
// Unconditional, NOT gated on Format: a table is a human artefact by definition, every
// caller branches to JSON() before reaching here, and a dead `if` in a security
// boundary is worse than none. Idempotent, so the call sites that already sanitize
// (cellText on worker names, token labels, actor cells) are unaffected — stripping
// twice strips nothing the second time.
//
// The bold header codes are added AFTER sanitizing, so the Printer's own escapes are
// the only ones that survive this function.
func (p *Printer) Table(headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(p.out, 0, 2, 2, ' ', 0)
	if len(headers) > 0 {
		cols := make([]string, len(headers))
		for i, h := range headers {
			cols[i] = CellText(h)
			if p.Color {
				cols[i] = "\x1b[1m" + cols[i] + "\x1b[0m"
			}
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = CellText(c)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// Printf writes a formatted line to the output. It is the escape hatch for the
// commands whose human view is neither a flat table nor a JSON document (run
// detail, run logs, the review verdict + recommendation blocks): routing all
// output through the Printer keeps a single destination.
//
// The format string and every string operand go through SanitizeTTY — the flowing-text
// half, which SPARES \t and \n. That is required, not a weaker choice: renderReview
// prints multi-line judge markdown through here line by line, and folding newlines
// would collapse it into one line. The cost is that a newline in a server string can
// still inject a line onto stdout; the containment is that nothing sized or aligned is
// printed through Printf, so an injected line is a spoofed sentence rather than a
// forged table row.
//
// Non-string operands are ints, floats and bools whose fmt verbs cannot emit a control
// character. A %v over a struct could, which is why no call site does one.
//
// SKIPPED IN --json MODE, and this is the load-bearing half of the JSON promise:
// renderMessage streams NDJSON through Println (not JSON()) for `run logs --follow`,
// and json.Marshal does NOT escape the Cf block — U+202E and U+200D come out raw, so a
// sanitizer here would silently mutate an agent's parsed payload. Measured, not assumed.
//
// One consequence worth stating: a future caller wanting COLOUR through Printf will
// find its escapes stripped. Nothing does today (the header above is the only ANSI this
// package emits), and losing colour is the correct failure for this boundary.
func (p *Printer) Printf(format string, a ...any) {
	if p.Format != FormatJSON {
		format = SanitizeTTY(format)
		a = sanitizeArgs(a)
	}
	_, _ = fmt.Fprintf(p.out, format, a...)
}

// Println writes its arguments followed by a newline to the output. Sanitized on the
// same terms as Printf, including the --json passthrough NDJSON depends on.
func (p *Printer) Println(a ...any) {
	if p.Format != FormatJSON {
		a = sanitizeArgs(a)
	}
	_, _ = fmt.Fprintln(p.out, a...)
}

// sanitizeArgs returns a copy of a with every string operand sanitized, leaving the
// caller's slice untouched. Copying matters: `a ...any` aliases the caller's backing
// array for a slice-spread call, and rewriting it in place would mutate a value the
// caller may still use.
func sanitizeArgs(a []any) []any {
	out := make([]any, len(a))
	for i, v := range a {
		if s, ok := v.(string); ok {
			out[i] = SanitizeTTY(s)
			continue
		}
		out[i] = v
	}
	return out
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
