package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// safeReportError retains error identity without rendering untrusted protocol,
// transport, decoder or close strings, including under verbose formatting.
type safeReportError struct {
	text  string
	cause error
}

func (e *safeReportError) Error() string              { return e.text }
func (e *safeReportError) Unwrap() error              { return e.cause }
func (e *safeReportError) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, e.text) }

func (r *rawReport) unknownError(cause error) error {
	kind := "incomplete"
	switch {
	case errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled):
		kind = "deadline"
	case r.http && !r.attached:
		kind = "unattached"
	case r.exhausted:
		kind = "budget"
	case r.readErr != nil:
		kind = "read_failure"
	case r.closeErr != nil:
		kind = "close_failure"
	case r.http && !r.eligible:
		kind = "http_status"
	case r.bytes == 0:
		kind = "empty_body"
	case r.invalid || r.outer.invalid || r.inner.invalid:
		kind = "malformed"
	case r.complete() && r.marker == "":
		kind = "no_command_marker"
	case r.complete() && r.unpack == "ok" && r.marker == "ok" && cause != nil:
		kind = "session_failure"
	}
	header := func(p packetObserver) int {
		if p.have < 4 {
			return p.have
		}
		return 0
	}
	text := fmt.Sprintf("missing or incomplete receive-pack acknowledgement: case=%s http=%t attached=%t eligible=%t bytes=%d sideband=%t flush=%t terminal=%t marker=%t invalid=%t outer_invalid=%t inner_invalid=%t outer_header=%d inner_header=%d outer_payload=%t inner_payload=%t budget=%t eof=%t finalized=%t read_failure=%t close_failure=%t",
		kind, r.http, r.attached, r.eligible, r.bytes, r.sideband, r.flushed, r.terminal, r.marker != "", r.invalid, r.outer.invalid, r.inner.invalid, header(r.outer), header(r.inner), r.outer.have == 4, r.inner.have == 4, r.exhausted, r.eof, r.finalized, r.readErr != nil, r.closeErr != nil)
	if r.exhausted {
		text += " response byte limit exhausted (1 MiB)"
	}
	return &safeReportError{text: text, cause: cause}
}
