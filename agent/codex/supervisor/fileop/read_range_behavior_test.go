package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func rangeRequest(offset, limit int64) request {
	return request{ID: 9, Op: opReadRange, Path: "text", Offset: json.RawMessage(fmt.Sprint(offset)), Limit: json.RawMessage(fmt.Sprint(limit))}
}
func rangeFixture(t *testing.T, body []byte) (*server, string, *int, *int) {
	t.Helper()
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "text"), body, 0600); err != nil {
		t.Fatal(err)
	}
	reads, closes := new(int), new(int)
	s.fileRead = func(f *os.File, p []byte) (int, error) {
		if f.Name() == "read_range" && len(p) > rangeReadChunk {
			t.Fatalf("read chunk %d", len(p))
		}
		n, err := f.Read(p)
		*reads += n
		return n, err
	}
	s.fileClose = func(f *os.File) error { *closes++; return f.Close() }
	return s, root, reads, closes
}
func checkRange(t *testing.T, s *server, req request, want string, lines int, partial, truncated, incomplete bool) map[string]any {
	t.Helper()
	got := s.handle(req)
	wantOK(t, got)
	scanned, ok := got["scannedBytes"].(int64)
	if !ok || scanned < 0 || scanned > rangeScanBytes {
		t.Fatalf("invalid scannedBytes: %v", scanned)
	}
	body := respData(t, got)
	if len(body) != len(want) || !bytes.Equal(body, []byte(want)) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(body), len(want))
	}
	if !utf8.Valid(body) || len(body) > rangeOutputBytes {
		t.Fatal("invalid output")
	}
	if got["linesReturned"] != lines || got["partialLastLine"] != partial || got["truncated"] != truncated || got["incomplete"] != incomplete || got["offset"] != int64Req(req.Offset) {
		t.Fatalf("metadata mismatch: %v", withoutData(got))
	}
	if incomplete {
		if got["reason"] != "scan_budget" {
			t.Fatal("missing scan reason")
		}
	} else if _, ok := got["reason"]; ok {
		t.Fatal("reason on completed result")
	}
	return got
}
func int64Req(raw json.RawMessage) int64 { v, _ := rangeInteger(raw, 9007199254740991); return v }
func withoutData(got map[string]any) map[string]any {
	copy := make(map[string]any)
	for k, v := range got {
		if k != "data" {
			copy[k] = v
		}
	}
	return copy
}

// First open grows after Fstat: the unchanged ordinary bound consumes exactly
// 1 MiB+1. The independent range open sees its own size and consumes 8 MiB.
// This establishes the helper seam; broker sequencing is M2.
func TestReadThenRangeGrowthByteBound(t *testing.T) {
	s, root, reads, closes := rangeFixture(t, []byte("x"))
	path := filepath.Join(root, "text")
	calls := 0
	s.fstat = func(fd int, st *unix.Stat_t) error {
		calls++
		if err := unix.Fstat(fd, st); err != nil {
			return err
		}
		if calls == 1 {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.Write(bytes.Repeat([]byte("x"), rangeScanBytes+10))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	wantErr(t, s.handle(request{Op: opRead, Path: "text"}), codeOversize)
	if *reads != maxReadBytes+1 {
		t.Fatalf("ordinary read bytes %d", *reads)
	}
	// Ordinary ReadAll may request >32 KiB, but never did on this bound.
	before := *reads
	got := checkRange(t, s, rangeRequest(9007199254740991, 1), "", 0, false, true, true)
	if *reads-before != rangeScanBytes || *reads != 9*1024*1024+1 || *closes != 2 || calls != 2 {
		t.Fatalf("reads=%d closes=%d fstats=%d", *reads, *closes, calls)
	}
	if got["size"] != int64(rangeScanBytes+11) || got["scannedBytes"] != int64(rangeScanBytes) {
		t.Fatalf("metadata: %v", withoutData(got))
	}
}

func TestReadRangeEarlyOversizedPrefix(t *testing.T) {
	s, _, reads, closes := rangeFixture(t, append([]byte("first\n"), bytes.Repeat([]byte("x"), maxReadBytes+1)...))
	wantErr(t, s.handle(request{Op: opRead, Path: "text"}), codeOversize)
	if *reads != 0 {
		t.Fatal("ordinary oversized stat read bytes")
	}
	got := checkRange(t, s, rangeRequest(1, 1), "first\n", 1, false, true, false)
	if *reads > rangeReadChunk || *reads == 0 || *closes != 2 || got["scannedBytes"] != int64(*reads) {
		t.Fatalf("reads=%d closes=%d", *reads, *closes)
	}
}

func TestReadRangeTextAndLineSemantics(t *testing.T) {
	cases := []struct {
		name, body, want   string
		offset, limit      int64
		lines              int
		partial, truncated bool
	}{
		{"empty", "", "", 1, 1, 0, false, false},
		{"LF", "a\n", "a\n", 1, 1, 1, false, false},
		{"no phantom", "a\n", "", 2, 1, 0, false, false},
		{"unterminated", "a\nb", "b", 2, 1, 1, false, false},
		{"past EOF", "a\nb", "", 3, 1, 0, false, false},
		{"empty lines", "\n\n", "\n", 2, 1, 1, false, false},
		{"BOM CRLF", "\ufeffa\r\nb\r\nc", "\ufeffa\r\nb\r\n", 1, 2, 2, false, true},
		{"skip isn't truncation", "a\nb", "b", 2, 5, 1, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 2, 3, rangeReadChunk} {
				s, _, _, closes := rangeFixture(t, []byte(tc.body))
				s.rangeChunk = chunk
				checkRange(t, s, rangeRequest(tc.offset, tc.limit), tc.want, tc.lines, tc.partial, tc.truncated, false)
				if *closes != 1 {
					t.Fatal("not closed")
				}
			}
		})
	}
}

func TestReadRangeOutputFrontiers(t *testing.T) {
	for _, size := range []int{rangeOutputBytes - 1, rangeOutputBytes, rangeOutputBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := strings.Repeat("a", size)
			s, _, _, _ := rangeFixture(t, []byte(body))
			checkRange(t, s, rangeRequest(1, 1), body[:min(size, rangeOutputBytes)], 1, size > rangeOutputBytes, size > rangeOutputBytes, false)
		})
	}
	for _, cp := range []string{"¢", "€", "😀"} {
		for _, chunk := range []int{1, 2, 3, 32767, 32768} {
			for _, cross := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/%t", cp, chunk, cross), func(t *testing.T) {
					prefix := rangeOutputBytes - len(cp)
					if cross {
						prefix++
					}
					body := strings.Repeat("a", prefix) + cp
					s, _, _, _ := rangeFixture(t, []byte(body))
					s.rangeChunk = chunk
					want := body
					if cross {
						want = body[:prefix]
					}
					checkRange(t, s, rangeRequest(1, 1), want, 1, cross, cross, false)
				})
			}
		}
	}
	for _, prefix := range []int{rangeOutputBytes - 2, rangeOutputBytes - 1} {
		body := strings.Repeat("a", prefix) + "\r\n"
		s, _, _, _ := rangeFixture(t, []byte(body))
		checkRange(t, s, rangeRequest(1, 1), body[:min(len(body), rangeOutputBytes)], 1, len(body) > rangeOutputBytes, len(body) > rangeOutputBytes, false)
	}
	s, _, _, _ := rangeFixture(t, []byte(strings.Repeat("a", rangeOutputBytes-1)+"\nZ"))
	checkRange(t, s, rangeRequest(1, 1), strings.Repeat("a", rangeOutputBytes-1)+"\n", 1, false, true, false)
}

func TestReadRangeValidationFrontier(t *testing.T) {
	for _, bad := range []string{"\x00", "\xff", "\xc2", "\xe2\x82", "\xf0\x9f\x98"} {
		for _, chunk := range []int{1, 2, 3, rangeReadChunk} {
			t.Run(fmt.Sprintf("%x/%d", bad, chunk), func(t *testing.T) {
				s, _, _, closes := rangeFixture(t, []byte("a\n"+bad))
				s.rangeChunk = chunk
				checkRange(t, s, rangeRequest(1, 1), "a\n", 1, false, true, false)
				wantErr(t, s.handle(rangeRequest(2, 1)), codeNotText)
				if *closes != 2 {
					t.Fatal("binary response leaked fd")
				}
				s, _, _, _ = rangeFixture(t, []byte(bad+"\na"))
				s.rangeChunk = chunk
				wantErr(t, s.handle(rangeRequest(2, 1)), codeNotText)
				// Exactly at the byte ceiling the next raw byte is outside validation.
				s, _, _, _ = rangeFixture(t, []byte(strings.Repeat("a", rangeOutputBytes)+bad))
				s.rangeChunk = chunk
				checkRange(t, s, rangeRequest(1, 1), strings.Repeat("a", rangeOutputBytes), 1, true, true, false)
			})
		}
	}
	// Invalid continuation of a codepoint crossing the ceiling is examined.
	s, _, _, _ := rangeFixture(t, []byte(strings.Repeat("a", rangeOutputBytes-1)+"\xe2\x00"))
	wantErr(t, s.handle(rangeRequest(1, 1)), codeNotText)
}

func TestReadRangeScanFrontiers(t *testing.T) {
	for _, size := range []int{rangeScanBytes - 1, rangeScanBytes, rangeScanBytes + 1} {
		s, _, reads, _ := rangeFixture(t, bytes.Repeat([]byte("a"), size))
		incomplete := size > rangeScanBytes
		checkRange(t, s, rangeRequest(2, 1), "", 0, false, incomplete, incomplete)
		if *reads != min(size, rangeScanBytes) {
			t.Fatalf("read bytes %d", *reads)
		}
	}
	for _, cp := range []string{"¢", "€", "😀"} {
		for split := 1; split < len(cp); split++ {
			prefix := strings.Repeat("a", rangeScanBytes-split)
			for _, eof := range []bool{false, true} {
				body := prefix + cp
				if eof {
					body = body[:rangeScanBytes]
				}
				s, _, reads, closes := rangeFixture(t, []byte(body))
				if eof {
					wantErr(t, s.handle(rangeRequest(2, 1)), codeNotText)
				} else {
					checkRange(t, s, rangeRequest(2, 1), "", 0, false, true, true)
				}
				if *reads != rangeScanBytes || *closes != 1 {
					t.Fatalf("reads=%d closes=%d", *reads, *closes)
				}
			}
		}
	}
	// Reached range has a valid prefix when the next UTF-8 rune hits scan cap.
	prefix := strings.Repeat("a", rangeScanBytes-5) + "\n"
	s, _, _, _ := rangeFixture(t, []byte(prefix+"ok\xf0\x9f\x98\x80"))
	checkRange(t, s, rangeRequest(2, 1), "ok", 1, true, true, true)
}

func TestReadRangeOpeningExtent(t *testing.T) {
	for _, mode := range []string{"grow", "shrink", "replace"} {
		t.Run(mode, func(t *testing.T) {
			s, root, reads, closes := rangeFixture(t, []byte("old\n"))
			s.fstat = func(fd int, st *unix.Stat_t) error {
				if err := unix.Fstat(fd, st); err != nil {
					return err
				}
				p := filepath.Join(root, "text")
				switch mode {
				case "grow":
					f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.WriteString("new\n"); err != nil {
						t.Fatal(err)
					}
					if err = f.Close(); err != nil {
						t.Fatal(err)
					}
				case "shrink":
					if err := os.Truncate(p, 2); err != nil {
						t.Fatal(err)
					}
				case "replace":
					if err := os.Rename(p, p+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("new\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			want := "old\n"
			if mode == "shrink" {
				want = "ol"
			}
			got := checkRange(t, s, rangeRequest(1, 2), want, 1, false, false, false)
			if got["size"] != int64(4) || *reads != len(want) || *closes != 1 {
				t.Fatalf("opening extent: %v reads %d closes %d", withoutData(got), *reads, *closes)
			}
		})
	}
}

func TestReadRangeFailureClosure(t *testing.T) {
	for _, mode := range []string{"fstat", "nonblock", "read", "EINTR", "cancel-before", "cancel-after", "deadline", "processing", "cancel-stat", "cancel-nonblock"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, closes := rangeFixture(t, []byte("hello\nworld"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.rangeContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
			code := codeIO
			switch mode {
			case "cancel-stat":
				s.fstat = func(int, *unix.Stat_t) error { cancel(); return unix.EIO }
				code = codeTimeout
			case "cancel-nonblock":
				s.nonblock = func(int, bool) error { cancel(); return unix.EIO }
				code = codeTimeout
			case "fstat":
				s.fstat = func(int, *unix.Stat_t) error { return unix.EIO }
			case "nonblock":
				s.nonblock = func(int, bool) error { return unix.EIO }
			case "read":
				s.fileRead = func(*os.File, []byte) (int, error) { return 0, unix.EIO }
			case "EINTR":
				s.fileRead = func(*os.File, []byte) (int, error) { cancel(); return 0, unix.EINTR }
				code = codeTimeout
			case "cancel-before":
				cancel()
				code = codeTimeout
			case "cancel-after":
				s.fileRead = func(f *os.File, p []byte) (int, error) { n, e := f.Read(p); cancel(); return n, e }
				code = codeTimeout
			case "deadline":
				expired, c := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer c()
				s.rangeContext = func() (context.Context, context.CancelFunc) { return expired, func() {} }
				code = codeTimeout
			case "processing":
				s.rangeContext = func() (context.Context, context.CancelFunc) {
					return &processingContext{Context: ctx, remaining: 12}, func() {}
				}
				code = codeTimeout
			}
			wantErr(t, s.handle(rangeRequest(1, 2)), code)
			want := 1
			if mode == "cancel-before" || mode == "deadline" {
				want = 0
			}
			if *closes != want {
				t.Fatalf("closes %d want %d", *closes, want)
			}
		})
	}
	t.Run("retry EINTR and n+EOF", func(t *testing.T) {
		s, _, _, closes := rangeFixture(t, []byte("a"))
		calls := 0
		s.fileRead = func(f *os.File, p []byte) (int, error) {
			calls++
			if calls == 1 {
				return 0, unix.EINTR
			}
			n, e := f.Read(p)
			if e == nil {
				e = io.EOF
			}
			return n, e
		}
		checkRange(t, s, rangeRequest(1, 1), "a", 1, false, false, false)
		if calls != 2 || *closes != 1 {
			t.Fatal("EINTR retry/close")
		}
	})
}

// A deterministic cooperative cancellation while processing prefetched bytes.
type processingContext struct {
	context.Context
	remaining int
}

func (c *processingContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestReadRangeArguments(t *testing.T) {
	invalid := []string{"null", `"1"`, "0", "-1", "1.1", "1e1000", "9007199254740992", "9007199254740991.1", "1e-1000", `"x"`, "true", "{}", "[]"}
	for _, raw := range invalid {
		s, _, _, closes := rangeFixture(t, []byte("a"))
		req := rangeRequest(1, 1)
		req.Offset = json.RawMessage(raw)
		wantErr(t, s.handle(req), codeMalformed)
		if *closes != 0 {
			t.Fatal("invalid offset opened file")
		}
	}
	for _, raw := range []string{"null", `"1"`, "0", "-1", "1.1", "2001", "2000.00000000000001", "1e1000"} {
		s, _, _, closes := rangeFixture(t, []byte("a"))
		req := rangeRequest(1, 1)
		req.Limit = json.RawMessage(raw)
		wantErr(t, s.handle(req), codeMalformed)
		if *closes != 0 {
			t.Fatal("invalid limit opened file")
		}
	}
	s, _, _, closes := rangeFixture(t, []byte("a"))
	for _, field := range []string{"offset", "limit"} {
		req := rangeRequest(1, 1)
		if field == "offset" {
			req.Offset = nil
		} else {
			req.Limit = nil
		}
		wantErr(t, s.handle(req), codeMalformed)
	}
	if *closes != 0 {
		t.Fatal("missing argument opened")
	}
	for _, raw := range []string{"1", "1.0", "10e-1", "0.1e1", " 1 ", "1e+0"} {
		req := rangeRequest(1, 1)
		req.Offset = json.RawMessage(raw)
		checkRange(t, s, req, "a", 1, false, false, false)
	}
	// Irrelevant fields remain ignored, including malformed range types.
	req := request{Op: opRead, Path: "text", Offset: json.RawMessage(`"ignored"`), Limit: json.RawMessage("null")}
	wantOK(t, s.handle(req))
}

func TestReadOrdinaryProductionCaps(t *testing.T) {
	for _, size := range []int{maxReadBytes - 1, maxReadBytes, maxReadBytes + 1} {
		s, _, reads, closes := rangeFixture(t, bytes.Repeat([]byte("a"), size))
		got := s.handle(request{Op: opRead, Path: "text"})
		if size > maxReadBytes {
			wantErr(t, got, codeOversize)
			if *reads != 0 {
				t.Fatal("oversized ordinary read consumed bytes")
			}
		} else {
			wantOK(t, got)
			if got["size"] != int64(size) || len(respData(t, got)) != size || *reads != size {
				t.Fatal("ordinary cap drift")
			}
		}
		if *closes != 1 {
			t.Fatal("ordinary read leaked")
		}
	}
}

func TestReadRangeCompleteScanBoundary(t *testing.T) {
	for _, cp := range []string{"¢", "€", "😀"} {
		body := strings.Repeat("a", rangeScanBytes-len(cp)) + cp
		s, _, reads, _ := rangeFixture(t, []byte(body))
		checkRange(t, s, rangeRequest(2, 1), "", 0, false, false, false)
		if *reads != rangeScanBytes {
			t.Fatal("scan boundary read count")
		}
	}
	for _, extra := range []string{"", "x"} {
		body := strings.Repeat("a", rangeScanBytes-2) + "\n\n" + extra
		s, _, _, _ := rangeFixture(t, []byte(body))
		incomplete := extra != ""
		checkRange(t, s, rangeRequest(2, 1), "\n", 1, false, incomplete, incomplete)
	}
}

func TestReadRangeReadErrorWithBytes(t *testing.T) {
	s, _, _, closes := rangeFixture(t, []byte("a\nb"))
	s.fileRead = func(f *os.File, p []byte) (int, error) { n, _ := f.Read(p); return n, unix.EIO }
	wantErr(t, s.handle(rangeRequest(1, 1)), codeIO)
	if *closes != 1 {
		t.Fatal("bytes plus error leaked")
	}
}

func TestReadRangeNeutralIO(t *testing.T) {
	for _, e := range []error{unix.EIO, unix.EPERM, errors.New("private failure")} {
		s, _, _, closes := rangeFixture(t, []byte("a"))
		s.fileRead = func(*os.File, []byte) (int, error) { return 0, e }
		wantErr(t, s.handle(rangeRequest(1, 1)), classify(e))
		if *closes != 1 {
			t.Fatal("leak")
		}
	}
}
