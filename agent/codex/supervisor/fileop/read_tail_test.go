package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func tailRequest(n int) request {
	return request{ID: 17, Op: opReadTail, Path: "text", Tail: json.RawMessage(fmt.Sprint(n))}
}
func tailFixture(t *testing.T, body []byte) (*server, string, *int, *int) {
	t.Helper()
	s, root, _, closes := rangeFixture(t, body)
	reads := new(int)
	s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) {
		if len(p) > rangeReadChunk {
			t.Fatal("oversized positioned read")
		}
		n, e := f.ReadAt(p, off)
		*reads += n
		return n, e
	}
	return s, root, reads, closes
}
func checkTail(t *testing.T, s *server, n int, want string, lines int, partial, truncated, incomplete bool) map[string]any {
	t.Helper()
	got := s.handle(tailRequest(n))
	wantOK(t, got)
	if data := respData(t, got); !bytes.Equal(data, []byte(want)) {
		t.Fatalf("body mismatch: got length %d, want %d", len(data), len(want))
	}
	if got["tail"] != int64(n) || got["fromEnd"] != true || got["linesReturned"] != lines || got["partialFirstLine"] != partial || got["truncated"] != truncated || got["incomplete"] != incomplete {
		t.Fatalf("metadata: %v", withoutData(got))
	}
	if incomplete {
		if got["reason"] != "scan_budget" {
			t.Fatal("missing reason")
		}
	} else if _, ok := got["reason"]; ok {
		t.Fatal("unexpected reason")
	}
	return got
}
func tailControl(t *testing.T) {
	t.Helper()
	s, _, _, _ := tailFixture(t, []byte("valid\n"))
	checkTail(t, s, 1, "valid\n", 1, false, false, false)
}

func TestReadTailLinesAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		n, lines   int
		trunc      bool
	}{
		{"", "", 2, 0, false}, {"a", "a", 2000, 1, false}, {"a\n", "a\n", 2000, 1, false},
		{"\n\n", "\n", 1, 1, true}, {"a\nb", "b", 1, 1, true}, {"a\nb\n", "b\n", 1, 1, true},
		{"\ufeffa\r\nb\r\n", "\ufeffa\r\nb\r\n", 3, 2, false},
		{strings.Repeat("old\n", 300000) + "end", "end", 1, 1, true},
	} {
		s, root, reads, closes := tailFixture(t, []byte(tc.body))
		got := checkTail(t, s, tc.n, tc.want, tc.lines, false, tc.trunc, false)
		if got["scannedBytes"] != int64(*reads) || got["size"] != int64(len(tc.body)) || *closes != 1 {
			t.Fatal("accounting or close mismatch")
		}
		var out, stderr bytes.Buffer
		input := fmt.Sprintf("{\"id\":17,\"op\":\"read_tail\",\"path\":\"text\",\"tail\":%d}\n", tc.n)
		if rc := run([]string{"--root", root}, strings.NewReader(input), &out, &stderr); rc != 0 {
			t.Fatalf("run=%d", rc)
		}
		var wire map[string]any
		if err := json.Unmarshal(out.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		wantOK(t, wire)
		if !bytes.Equal(respData(t, wire), []byte(tc.want)) || wire["fromEnd"] != true || wire["tail"] != float64(tc.n) {
			t.Fatal("wire mismatch")
		}
	}
}

func TestReadTailCapPrecedesBudget(t *testing.T) {
	s, _, reads, closes := tailFixture(t, []byte(strings.Repeat("x", rangeOutputBytes+10)))
	s.rangeChunk = 1
	s.tailBudget = rangeOutputBytes + 1
	got := checkTail(t, s, 1, strings.Repeat("x", rangeOutputBytes), 1, true, true, false)
	if *reads != rangeOutputBytes+1 || got["scannedBytes"] != int64(rangeOutputBytes+1) || *closes != 1 {
		t.Fatalf("reads=%d closes=%d metadata=%v", *reads, *closes, withoutData(got))
	}
}

func TestReadTailCapsAndBudget(t *testing.T) {
	for _, chunk := range []int{1, 7, rangeReadChunk} {
		s, _, reads, _ := tailFixture(t, []byte(strings.Repeat("x", rangeOutputBytes+100)))
		s.rangeChunk = chunk
		got := checkTail(t, s, 2, strings.Repeat("x", rangeOutputBytes), 1, true, true, false)
		if got["scannedBytes"] != int64(*reads) || *reads > rangeScanBytes || (chunk == 1 && *reads != rangeOutputBytes+1) {
			t.Fatal("scan accounting")
		}
		s, _, _, _ = tailFixture(t, []byte(strings.Repeat("x", rangeOutputBytes)+"\nok\n"))
		s.rangeChunk = chunk
		s.tailBudget = rangeOutputBytes + 1
		checkTail(t, s, 2, "ok\n", 1, false, true, false)
	}
	s, _, reads, _ := tailFixture(t, []byte("older line\na\nb\n"))
	s.tailBudget = 7
	s.rangeChunk = 3
	got := checkTail(t, s, 3, "a\nb\n", 2, false, true, true)
	if *reads != 7 || got["scannedBytes"] != int64(7) {
		t.Fatal("budget exceeded")
	}
	s, _, _, _ = tailFixture(t, []byte(strings.Repeat("x", 100)))
	s.tailBudget = 10
	checkTail(t, s, 1, "", 0, false, true, true)
}

func TestReadTailTextFrontier(t *testing.T) {
	for _, chunk := range []int{1, 3, rangeReadChunk} {
		for _, bad := range []string{"\x00", "\xff"} {
			t.Run(fmt.Sprintf("%d-%x", chunk, bad), func(t *testing.T) {
				tailControl(t)
				s, _, _, _ := tailFixture(t, []byte(bad+"\ngood\n"))
				s.rangeChunk = chunk
				checkTail(t, s, 1, "good\n", 1, false, true, false)
				s, _, _, _ = tailFixture(t, []byte("old\ng"+bad+"ood\n"))
				s.rangeChunk = chunk
				wantErr(t, s.handle(tailRequest(1)), codeNotText)
				s, _, _, _ = tailFixture(t, []byte(bad+strings.Repeat("x", rangeOutputBytes+10)))
				s.rangeChunk = chunk
				checkTail(t, s, 1, strings.Repeat("x", rangeOutputBytes), 1, true, true, false)
			})
		}
		for _, tc := range []struct {
			prefix string
			bad    bool
		}{
			{"😀", false}, {"\xf0\x80\x80\x80", true}, {"\xf4\x90\x80\x80", true}, {"\xff\x80\x80\x80", true},
		} {
			tailControl(t)
			s, _, reads, _ := tailFixture(t, []byte(tc.prefix+strings.Repeat("x", 65535)))
			s.rangeChunk = chunk
			if tc.bad {
				wantErr(t, s.handle(tailRequest(1)), codeNotText)
			} else {
				got := checkTail(t, s, 1, strings.Repeat("x", 65535), 1, true, true, false)
				if got["scannedBytes"] != int64(*reads) || (chunk == 1 && *reads != rangeOutputBytes+3) {
					t.Fatal("lookbehind uncounted")
				}
			}
		}
		s, _, _, _ := tailFixture(t, []byte("old\n😀aé\n"))
		s.rangeChunk = chunk
		checkTail(t, s, 1, "😀aé\n", 1, false, true, false)
	}
}

// Pair each outside prefix with a clean prefix of the same length. In particular,
// the truncated four-byte scalar followed by x used to be rejected despite the
// entire malformed prefix preceding the raw output boundary.
func TestReadTailRuneStartFrontier(t *testing.T) {
	for _, chunk := range []int{1, 3, rangeReadChunk} {
		for _, boundary := range []string{"x", "é", "😀"} {
			for _, prefix := range []string{"\xff\x80\x80", "\xf0\x90\x80", "\x00\x80\x80"} {
				t.Run(fmt.Sprintf("%d-%x-%x", chunk, boundary, prefix), func(t *testing.T) {
					want := boundary + strings.Repeat("x", rangeOutputBytes-len(boundary))
					for _, older := range []string{"old", prefix} {
						s, _, reads, closes := tailFixture(t, []byte(older+want))
						s.rangeChunk = chunk
						got := checkTail(t, s, 1, want, 1, true, true, false)
						if got["scannedBytes"] != int64(*reads) || *reads < rangeOutputBytes+1 ||
							*reads > min(len(older+want), rangeOutputBytes+chunk) || *closes != 1 {
							t.Fatalf("reads=%d closes=%d metadata=%v", *reads, *closes, withoutData(got))
						}
					}
				})
			}
		}
	}
}

func TestReadTailBudgetDuringLookbehind(t *testing.T) {
	for _, chunk := range []int{1, 3, rangeReadChunk} {
		for _, scalar := range []string{"😀", "\xf4\x90\x80\x80"} {
			for extra := 1; extra <= 3; extra++ {
				t.Run(fmt.Sprintf("%d-%x-%d", chunk, scalar, extra), func(t *testing.T) {
					// The raw boundary is the scalar's last continuation byte.
					// Its lead requires all three lookbehind bytes to be read.
					want := strings.Repeat("x", rangeOutputBytes-1)
					s, _, reads, closes := tailFixture(t, []byte("older"+scalar+want))
					s.rangeChunk = chunk
					budget := int64(rangeOutputBytes + extra)
					s.tailBudget = budget
					if extra < 3 {
						got := checkTail(t, s, 1, "", 0, false, true, true)
						if got["scannedBytes"] != int64(*reads) {
							t.Fatal("lookbehind uncounted")
						}
					} else if scalar == "😀" {
						got := checkTail(t, s, 1, want, 1, true, true, false)
						if got["scannedBytes"] != int64(*reads) {
							t.Fatal("lookbehind uncounted")
						}
					} else {
						wantErr(t, s.handle(tailRequest(1)), codeNotText)
					}
					if int64(*reads) != budget || *closes != 1 {
						t.Fatalf("reads=%d budget=%d closes=%d", *reads, budget, *closes)
					}
				})
			}
		}
	}
}

func TestReadTailMalformed(t *testing.T) {
	tailControl(t)
	for _, raw := range []string{"", "null", "true", "0", "-1", "2001", "1.5", "\"1\""} {
		s, _, _, closes := tailFixture(t, []byte("valid"))
		q := tailRequest(1)
		q.Tail = json.RawMessage(raw)
		wantErr(t, s.handle(q), codeMalformed)
		if *closes != 0 {
			t.Fatal("opened malformed request")
		}
	}
	for _, field := range []string{"offset", "limit"} {
		s, _, _, _ := tailFixture(t, []byte("valid"))
		q := tailRequest(1)
		if field == "offset" {
			q.Offset = json.RawMessage("null")
		} else {
			q.Limit = json.RawMessage("0")
		}
		wantErr(t, s.handle(q), codeMalformed)
	}
}

func TestReadTailIOAndClose(t *testing.T) {
	for _, kind := range []string{"fstat", "nonblock", "short", "shrink", "read", "deadline", "deadline-stat", "deadline-open", "deadline-nonblock", "processing", "text", "device", "directory"} {
		t.Run(kind, func(t *testing.T) {
			tailControl(t)
			s, root, _, closes := tailFixture(t, []byte("valid\n"))
			code := codeIO
			switch kind {
			case "fstat":
				s.fstat = func(int, *unix.Stat_t) error { return unix.EIO }
			case "nonblock":
				s.nonblock = func(int, bool) error { return unix.EIO }
			case "short":
				s.fileReadAt = func(*os.File, []byte, int64) (int, error) { return 0, nil }
			case "read":
				s.fileReadAt = func(*os.File, []byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }
			case "shrink":
				s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) {
					if e := os.Truncate(filepath.Join(root, "text"), 0); e != nil {
						t.Fatal(e)
					}
					return f.ReadAt(p, off)
				}
			case "text":
				if err := os.WriteFile(filepath.Join(root, "text"), []byte("bad\x00\n"), 0600); err != nil {
					t.Fatal(err)
				}
				code = codeNotText
			case "processing":
				s.rangeContext = func() (context.Context, context.CancelFunc) {
					return &processingContext{Context: context.Background(), remaining: 10}, func() {}
				}
				code = codeTimeout
			case "deadline", "deadline-stat", "deadline-open", "deadline-nonblock":
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				s.rangeContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
				if kind == "deadline" {
					s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) { n, e := f.ReadAt(p, off); cancel(); return n, e }
				}
				if kind == "deadline-stat" {
					s.fstat = func(fd int, st *unix.Stat_t) error { cancel(); return unix.Fstat(fd, st) }
				}
				if kind == "deadline-nonblock" {
					s.nonblock = func(int, bool) error { cancel(); return unix.EIO }
				}
				if kind == "deadline-open" {
					cancel()
				}
				code = codeTimeout
			case "device", "directory":
				mode := uint32(unix.S_IFCHR)
				code = codeNotFile
				if kind == "directory" {
					mode = unix.S_IFDIR
					code = codeIsDir
				}
				s.fstat = func(fd int, st *unix.Stat_t) error { e := unix.Fstat(fd, st); st.Mode = mode; return e }
			}
			wantErr(t, s.handle(tailRequest(1)), code)
			wantClose := 1
			if kind == "deadline-open" {
				wantClose = 0
			}
			if *closes != wantClose {
				t.Fatalf("closes=%d want=%d", *closes, wantClose)
			}
		})
	}
	s, root, _, closes := tailFixture(t, []byte("old\nend"))
	s.fstat = func(fd int, st *unix.Stat_t) error {
		if e := unix.Fstat(fd, st); e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(root, "text"), []byte("old\nend\ngrowth"), 0600)
	}
	got := checkTail(t, s, 1, "end", 1, false, true, false)
	if got["size"] != int64(7) || *closes != 1 {
		t.Fatal("growth extent/close")
	}
}

func TestReadTailInterruptedReads(t *testing.T) {
	t.Run("single zero EINTR", func(t *testing.T) {
		s, _, reads, closes := tailFixture(t, []byte("a\nb\n"))
		s.tailBudget = 4
		s.rangeChunk = 2
		readAt := s.fileReadAt
		calls := 0
		s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) {
			calls++
			if calls == 1 {
				return 0, unix.EINTR
			}
			return readAt(f, p, off)
		}
		got := checkTail(t, s, 2, "a\nb\n", 2, false, false, false)
		if calls != 3 || *reads != 4 || got["scannedBytes"] != int64(4) || *closes != 1 {
			t.Fatalf("calls=%d reads=%d closes=%d metadata=%v", calls, *reads, *closes, withoutData(got))
		}
	})
	t.Run("repeated zero EINTR cancellation", func(t *testing.T) {
		tailControl(t)
		s, _, _, closes := tailFixture(t, []byte("valid\n"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		contexts, calls := 0, 0
		s.rangeContext = func() (context.Context, context.CancelFunc) {
			contexts++
			return ctx, func() {}
		}
		s.fileReadAt = func(*os.File, []byte, int64) (int, error) {
			calls++
			if calls == 3 {
				cancel()
			}
			return 0, unix.EINTR
		}
		wantErr(t, s.handle(tailRequest(1)), codeTimeout)
		if contexts != 1 || calls != 3 || *closes != 1 {
			t.Fatalf("contexts=%d calls=%d closes=%d", contexts, calls, *closes)
		}
	})
}

func TestReadTailPositionedReadFailures(t *testing.T) {
	tailControl(t)
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{
		{"short without error", 1, nil},
		{"short EOF", 1, io.EOF},
		{"nonzero EINTR", 1, unix.EINTR},
		{"negative count", -1, nil},
		{"excess count", 7, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, closes := tailFixture(t, []byte("valid\n"))
			calls := 0
			s.fileReadAt = func(*os.File, []byte, int64) (int, error) {
				calls++
				return tc.n, tc.err
			}
			wantErr(t, s.handle(tailRequest(1)), codeIO)
			if calls != 1 || *closes != 1 {
				t.Fatalf("calls=%d closes=%d", calls, *closes)
			}
		})
	}
}

func TestReadTailContainment(t *testing.T) {
	tailControl(t)
	s, root, _, closes := tailFixture(t, []byte("valid"))
	for _, tc := range []struct{ path, code string }{
		{"../text", codeEscape}, {"/etc/passwd", codeEscape}, {".git/config", codeDenied}, {"missing", codeNotFound},
	} {
		q := tailRequest(1)
		q.Path = tc.path
		wantErr(t, s.handle(q), tc.code)
	}
	if err := os.Symlink("text", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	q := tailRequest(1)
	q.Path = "link"
	wantErr(t, s.handle(q), codeSymlink)
	if *closes != 0 {
		t.Fatal("failed open closed fd")
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	q.Path = "dir"
	wantErr(t, s.handle(q), codeIsDir)
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	q.Path = "fifo"
	wantErr(t, s.handle(q), codeNotFile)
	if *closes != 2 {
		t.Fatal("special file close")
	}
	// Use a relative socket address to stay within AF_UNIX's pathname limit.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	address, err := filepath.Rel(cwd, filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	if absolute := filepath.Join(root, "socket"); len(absolute) < len(address) {
		address = absolute
	}
	listener, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	q.Path = "socket"
	wantErr(t, s.handle(q), codeNotFile)
	if *closes != 2 {
		t.Fatal("socket open acquired fd")
	}
}
