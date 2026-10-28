package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	rangeScanBytes   = 8 * 1024 * 1024
	rangeOutputBytes = 64 * 1024
	rangeReadChunk   = 32 * 1024
	codeNotText      = "E_NOT_TEXT"
	codeTimeout      = "E_TIMEOUT"
)

var errScanBudget = errors.New("scan budget")

type fileReader struct {
	s *server
	f *os.File
}

func (r fileReader) Read(p []byte) (int, error) { return r.s.fileRead(r.f, p) }

// rangeInteger uses the JSON number grammar, then checks the mathematical value.
// Raw fields keep irrelevant range arguments from changing other operations.
func rangeInteger(raw json.RawMessage, max int64) (int64, error) {
	text := strings.TrimSpace(string(raw))
	var number json.Number
	if len(text) == 0 || text[0] < '0' || text[0] > '9' || json.Unmarshal([]byte(text), &number) != nil {
		return 0, errBadArgs
	}
	// Normalize decimal/exponent notation exactly, avoiding float rounding near
	// the safe-integer bound. No exponent can cause a proportional allocation.
	mantissa := text
	exponent := 0
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		mantissa = text[:i]
		var err error
		exponent, err = strconv.Atoi(text[i+1:])
		if err != nil {
			return 0, errBadArgs
		}
	}
	fraction := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fraction = len(mantissa) - i - 1
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	mantissa = strings.TrimLeft(mantissa, "0")
	if len(mantissa) == 0 {
		return 0, errBadArgs
	}
	// Bound exponent arithmetic before subtracting the fractional digit count.
	if exponent > len(text)+16 || exponent < -len(text)-16 {
		return 0, errBadArgs
	}
	scale := exponent - fraction
	for scale < 0 && strings.HasSuffix(mantissa, "0") {
		mantissa = mantissa[:len(mantissa)-1]
		scale++
	}
	if scale < 0 || scale > 16 || len(mantissa)+scale > 16 {
		return 0, errBadArgs
	}
	v, err := strconv.ParseInt(mantissa, 10, 64)
	if err != nil {
		return 0, errBadArgs
	}
	for ; scale > 0; scale-- {
		v *= 10
	}
	if v < 1 || v > max {
		return 0, errBadArgs
	}
	return v, nil
}

func (s *server) doReadRange(req request) map[string]any {
	offset, err := rangeInteger(req.Offset, 9007199254740991)
	if err != nil {
		return errResp(req.ID, codeMalformed)
	}
	limit, err := rangeInteger(req.Limit, 2000)
	if err != nil {
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
	f := os.NewFile(uintptr(fd), "read_range")
	if f == nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	defer s.fileClose(f)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
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
	err = s.nonblock(fd, false)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	r := rangeReader{s: s, f: f, ctx: ctx, size: st.Size}
	data, lines, partial, truncated, incomplete, err := r.excerpt(offset, limit)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return errResp(req.ID, codeTimeout)
		}
		if errors.Is(err, errRangeText) {
			return errResp(req.ID, codeNotText)
		}
		return errResp(req.ID, classify(err))
	}
	fields := map[string]any{"data": base64.StdEncoding.EncodeToString(data), "size": st.Size, "offset": offset, "linesReturned": lines, "partialLastLine": partial, "truncated": truncated, "incomplete": incomplete, "scannedBytes": r.readBytes}
	if incomplete {
		fields["reason"] = "scan_budget"
	}
	return okResp(req.ID, fields)
}

var errRangeText = errors.New("not text")

// rangeReader owns one fixed prefetch buffer. readBytes counts actual bytes,
// including surplus. The buffer is drained before another read; inspection
// therefore cannot exceed the scan cap either.
// The opening size pins the readable extent, while early EOF accepts shrinkage.
type rangeReader struct {
	s         *server
	f         *os.File
	ctx       context.Context
	size      int64
	buf       [rangeReadChunk]byte
	lo, hi    int
	readBytes int64
	pending   error
}

func (r *rangeReader) next() (byte, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.lo < r.hi {
		b := r.buf[r.lo]
		r.lo++
		return b, nil
	}
	if r.pending != nil {
		return 0, r.pending
	}
	if r.readBytes >= r.size {
		return 0, io.EOF
	}
	if r.readBytes >= rangeScanBytes {
		return 0, errScanBudget
	}
	nmax := min(int64(rangeReadChunk), r.size-r.readBytes, int64(rangeScanBytes)-r.readBytes)
	if r.s.rangeChunk > 0 {
		nmax = min(nmax, int64(r.s.rangeChunk))
	}
	// EINTR retries are bounded by the same cooperative deadline. A stalled
	// filesystem syscall itself is not interruptible by this context.
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := r.s.fileRead(r.f, r.buf[:int(nmax)])
		if n < 0 || n > int(nmax) {
			return 0, unix.EIO
		}
		r.readBytes += int64(n)
		if cerr := r.ctx.Err(); cerr != nil {
			return 0, cerr
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, unix.EINTR) {
			return 0, err
		}
		if n > 0 {
			r.lo = 1
			r.hi = n
			if err != nil && !errors.Is(err, unix.EINTR) {
				r.pending = err
			}
			return r.buf[0], nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err == nil {
			return 0, unix.EIO
		}
		return 0, err
	}
}

// excerpt validates one codepoint at a time, including skipped text. It stops at
// the requested LF or the byte ceiling (completing only a crossing codepoint).
// A raw byte lookahead proves continuation without decoding the next character.
// Fixed output and UTF-8 carry avoid allocations proportional to source lines.
func (r *rangeReader) excerpt(offset, limit int64) (data []byte, lines int, partial, truncated, incomplete bool, err error) {
	var output [rangeOutputBytes]byte
	var runeBytes [utf8.UTFMax]byte
	used := 0
	line := int64(1)
	lastLF := true
	lineRepresented := false
	finish := func(more, budget bool) ([]byte, int, bool, bool, bool, error) {
		return output[:used], lines, used > 0 && !lastLF && more, more, budget, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return nil, 0, false, false, false, err
		}
		b, e := r.next()
		if e != nil {
			if errors.Is(e, io.EOF) {
				return finish(false, false)
			}
			if errors.Is(e, errScanBudget) {
				return finish(true, true)
			}
			return nil, 0, false, false, false, e
		}
		runeBytes[0] = b
		width := 1
		for !utf8.FullRune(runeBytes[:width]) {
			b, e = r.next()
			if e != nil {
				if errors.Is(e, errScanBudget) {
					return finish(true, true)
				}
				if errors.Is(e, io.EOF) {
					e = errRangeText
				}
				return nil, 0, false, false, false, e
			}
			runeBytes[width] = b
			width++
		}
		cp, n := utf8.DecodeRune(runeBytes[:width])
		if cp == 0 || (cp == utf8.RuneError && n == 1) {
			return nil, 0, false, false, false, errRangeText
		}
		selected := line >= offset
		if selected {
			if used+width > rangeOutputBytes {
				return finish(true, false)
			}
			copy(output[used:], runeBytes[:width])
			used += width
			if !lineRepresented {
				lines++
				lineRepresented = true
			}
			lastLF = cp == '\n'
		}
		if cp == '\n' {
			line++
			lineRepresented = false
		}
		if selected && (used == rangeOutputBytes || line-offset >= limit) {
			_, e := r.next()
			if errors.Is(e, io.EOF) {
				return finish(false, false)
			}
			if errors.Is(e, errScanBudget) {
				return finish(true, true)
			}
			if e != nil {
				return nil, 0, false, false, false, e
			}
			return finish(true, false)
		}
	}
}
