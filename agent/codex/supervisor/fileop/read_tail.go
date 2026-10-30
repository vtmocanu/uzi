package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func (s *server) doReadTail(req request) map[string]any {
	tail, err := rangeInteger(req.Tail, 2000)
	if err != nil || req.Offset != nil || req.Limit != nil {
		return errResp(req.ID, codeMalformed)
	}
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	ctx, cancel := s.rangeContext()
	defer cancel()
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	fd, err := s.openBeneath(req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if ctx.Err() != nil {
			return errResp(req.ID, codeTimeout)
		}
		return errResp(req.ID, classify(err))
	}
	f := os.NewFile(uintptr(fd), "read_tail")
	if f == nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	defer s.fileClose(f)
	var st unix.Stat_t
	err = s.fstat(fd, &st)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return errResp(req.ID, codeIsDir)
		}
		return errResp(req.ID, codeNotFile)
	}
	if st.Size < 0 {
		return errResp(req.ID, codeIO)
	}
	err = s.nonblock(fd, false)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	budget := int64(rangeScanBytes)
	if s.tailBudget > 0 {
		budget = min(budget, s.tailBudget)
	}
	r := tailReader{s: s, f: f, ctx: ctx, pos: st.Size, budget: budget}
	data, lines, partial, incomplete, err := r.excerpt(tail)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		if errors.Is(err, errRangeText) {
			return errResp(req.ID, codeNotText)
		}
		return errResp(req.ID, codeIO)
	}
	fields := map[string]any{"data": base64.StdEncoding.EncodeToString(data), "size": st.Size, "tail": tail, "fromEnd": true, "linesReturned": lines, "partialFirstLine": partial, "truncated": int64(len(data)) < st.Size, "incomplete": incomplete, "scannedBytes": r.scanned}
	if incomplete {
		fields["reason"] = "scan_budget"
	}
	return okResp(req.ID, fields)
}

// tailReader traverses the opening extent backwards. Each refill consumes budget,
// including unused prefetch. Short positioned reads are errors, never evidence of
// an earlier beginning of file. Only zero-byte EINTR is retried, without charging
// bytes; the same cooperative context bounds all retry attempts for this request.
// Every other I/O failure terminates this request.
type tailReader struct {
	s                    *server
	f                    *os.File
	ctx                  context.Context
	pos, budget, scanned int64
	buf                  [rangeReadChunk]byte
	remaining            int
}

func (r *tailReader) previous() (byte, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		if r.pos == 0 {
			return 0, nil
		}
		n := min(int64(rangeReadChunk), r.pos, r.budget-r.scanned)
		if r.s.rangeChunk > 0 {
			n = min(n, int64(r.s.rangeChunk))
		}
		if n == 0 {
			return 0, errScanBudget
		}
		for {
			if err := r.ctx.Err(); err != nil {
				return 0, err
			}
			got, err := r.s.fileReadAt(r.f, r.buf[:n], r.pos-n)
			if got < 0 || int64(got) > n {
				return 0, unix.EIO
			}
			r.scanned += int64(got)
			if r.ctx.Err() != nil {
				return 0, r.ctx.Err()
			}
			if got == 0 && errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil || int64(got) != n {
				return 0, unix.EIO
			}
			r.remaining = got
			break
		}
	}
	r.remaining--
	r.pos--
	return r.buf[r.remaining], nil
}

// excerpt stores at most one output window and a chunk. completed marks the
// suffix proven to consist of whole lines; an unfinished older line is discarded
// on either cap. Only the selected suffix is text-validated, never surplus.
func (r *tailReader) excerpt(tail int64) ([]byte, int, bool, bool, error) {
	var out [rangeOutputBytes + 3]byte
	start, completed, lines := len(out), len(out), 0
	finish := func(partial, incomplete bool) ([]byte, int, bool, bool, error) {
		data := out[start:]
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil, 0, false, false, errRangeText
		}
		return data, lines, partial, incomplete, nil
	}
	first := true
	for r.pos > 0 {
		b, err := r.previous()
		if errors.Is(err, errScanBudget) {
			start = completed
			return finish(false, true)
		}
		if err != nil {
			return nil, 0, false, false, err
		}
		if b == '\n' && !first {
			lines++
			completed = start
			if int64(lines) == tail {
				return finish(false, false)
			}
		}
		first = false
		if start == 3 {
			if lines > 0 {
				start = completed
				return finish(false, false)
			}
			// A rune start at the raw boundary needs no lookbehind; older bytes
			// are outside the text frontier even if they resemble a crossing lead.
			if utf8.RuneStart(out[3]) {
				lines = 1
				return finish(true, false)
			}
			// The final line alone exceeds the cap. Inspect at most three bytes
			// before the RAW boundary, validating any scalar that crosses it before
			// discarding its continuation bytes from the returned suffix.
			out[2] = b
			count := 1
			for count < 3 && r.pos > 0 && !utf8.RuneStart(out[3-count]) {
				c, e := r.previous()
				if errors.Is(e, errScanBudget) {
					start = completed
					return finish(false, true)
				}
				if e != nil {
					return nil, 0, false, false, e
				}
				out[2-count] = c
				count++
			}
			trim, e := tailBoundary(out[3-count:], count)
			if e != nil {
				return nil, 0, false, false, e
			}
			start = 3 + trim
			lines = 1
			return finish(true, false)
		}
		start--
		out[start] = b
	}
	if start < len(out) {
		lines++
	}
	return finish(false, false)
}

// tailBoundary validates just a scalar crossing the raw suffix boundary. Bytes
// wholly before that frontier, including prefetched bytes, are irrelevant.
func tailBoundary(frontier []byte, boundary int) (int, error) {
	for i := boundary - 1; i >= 0; i-- {
		if !utf8.RuneStart(frontier[i]) {
			continue
		}
		n := boundary - i
		_, width := utf8.DecodeRune(frontier[i:])
		// A leading byte whose nominal width crosses the boundary is part of
		// the validation frontier even when its continuation is malformed.
		expected := 1
		switch {
		case frontier[i] >= 0xf0:
			expected = 4
		case frontier[i] >= 0xe0:
			expected = 3
		case frontier[i] >= 0xc0:
			expected = 2
		}
		if expected > n {
			if width == 1 {
				return 0, errRangeText
			}
			return width - n, nil
		}
		break
	}
	if boundary < len(frontier) && !utf8.RuneStart(frontier[boundary]) {
		return 0, errRangeText
	}
	return 0, nil
}
