package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// controlOp is a parsed, validated control frame. TimeoutMs is meaningful only
// for a dispose and is already clamped to [0, maxDisposeTimeoutMs].
type controlOp struct {
	Op        string
	ID        int
	TimeoutMs int
}

// Control-frame rejection sentinels. Each maps to a short, sanitized abnormal
// reason (no argv, no paths, no secrets).
var (
	errControlOversized = errors.New("oversized control input")
	errMalformedControl = errors.New("malformed control input")
	errUnknownOp        = errors.New("unknown control op")

	// errControlEOF is returned by controlReader when the trusted control
	// channel closes. EOF is ALWAYS abnormal, never clean success.
	errControlEOF = errors.New("control EOF")
)

// parseControlLine validates and decodes one control frame. It is pure: it takes
// the raw line bytes and returns the op or a rejection sentinel, so oversized /
// malformed / unknown-op handling is unit-testable without a live channel.
func parseControlLine(line []byte) (controlOp, error) {
	if len(line) > maxControlLine {
		return controlOp{}, errControlOversized
	}
	var raw struct {
		Op        string `json:"op"`
		ID        int    `json:"id"`
		TimeoutMs *int   `json:"timeoutMs"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return controlOp{}, errMalformedControl
	}
	switch raw.Op {
	case opSnapshot:
		return controlOp{Op: opSnapshot, ID: raw.ID}, nil
	case opDispose:
		t := defaultDisposeTimeoutMs
		if raw.TimeoutMs != nil {
			t = clampTimeout(*raw.TimeoutMs)
		}
		return controlOp{Op: opDispose, ID: raw.ID, TimeoutMs: t}, nil
	default:
		return controlOp{}, errUnknownOp
	}
}

// clampTimeout bounds a requested dispose timeout to [0, maxDisposeTimeoutMs].
func clampTimeout(t int) int {
	if t < 0 {
		return 0
	}
	if t > maxDisposeTimeoutMs {
		return maxDisposeTimeoutMs
	}
	return t
}

// controlReader frames newline-delimited control input from fd 3, enforcing the
// maxControlLine ceiling on both a completed line and unbounded buffer growth.
type controlReader struct {
	r   io.Reader
	buf []byte
}

// readLine returns the next control frame (without its trailing newline), or a
// sentinel: errControlOversized if a frame exceeds maxControlLine, or
// errControlEOF when the channel closes (with or without a trailing partial
// line). A partial unterminated line at EOF is abnormal, not a frame.
func (c *controlReader) readLine() ([]byte, error) {
	for {
		if i := bytes.IndexByte(c.buf, '\n'); i >= 0 {
			if i > maxControlLine {
				return nil, errControlOversized
			}
			line := c.buf[:i]
			c.buf = c.buf[i+1:]
			return line, nil
		}
		if len(c.buf) > maxControlLine {
			return nil, errControlOversized
		}
		chunk := make([]byte, 4096)
		n, err := c.r.Read(chunk)
		if n > 0 {
			c.buf = append(c.buf, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Any remaining bytes are a partial frame with no terminator;
				// treat the whole close as abnormal controller loss.
				return nil, errControlEOF
			}
			return nil, err
		}
	}
}
