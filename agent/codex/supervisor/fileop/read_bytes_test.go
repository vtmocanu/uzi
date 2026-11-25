package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func bytesReq(path string, offset, limit string) request {
	r := request{ID: 9, Op: opReadBytes, Path: path}
	if offset != "" {
		r.Offset = json.RawMessage(offset)
	}
	if limit != "" {
		r.Limit = json.RawMessage(limit)
	}
	return r
}

// bytesFixture builds a server with a file "f" holding body and a counting
// fileClose seam.
func bytesFixture(t *testing.T, body []byte) (*server, string, *int) {
	t.Helper()
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "f"), body, 0600); err != nil {
		t.Fatal(err)
	}
	closes := new(int)
	s.fileClose = func(f *os.File) error { *closes++; return f.Close() }
	return s, root, closes
}

// expiringCtx reports nil from Err for the first (at-1) calls and
// DeadlineExceeded from call number at onward.
type expiringCtx struct {
	calls *int
	at    int
}

func (c expiringCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c expiringCtx) Done() <-chan struct{}       { return nil }
func (c expiringCtx) Value(any) any               { return nil }
func (c expiringCtx) Err() error {
	*c.calls++
	if *c.calls >= c.at {
		return context.DeadlineExceeded
	}
	return nil
}

func expireAt(s *server, at int) {
	calls := new(int)
	s.rangeContext = func() (context.Context, context.CancelFunc) {
		return expiringCtx{calls: calls, at: at}, func() {}
	}
}

func TestReadBytesWindows(t *testing.T) {
	ident := regexp.MustCompile(`^\d+:\d+:\d+:\d+$`)
	for _, tc := range []struct {
		offset, limit string
		want          string
		wantOffset    int64
	}{
		{"0", "10", "0123456789", 0},
		{"0", "4", "0123", 0},
		{"3", "4", "3456", 3},
		{"8", "100", "89", 8},
		{"10", "5", "", 10},
		{"9007199254740991", "5", "", 9007199254740991},
		{"0.0", "3", "012", 0},
		{"0e5", "3", "012", 0},
		{"3e0", "2", "34", 3},
	} {
		s, _, closes := bytesFixture(t, []byte("0123456789"))
		got := s.handle(bytesReq("f", tc.offset, tc.limit))
		wantOK(t, got)
		if string(respData(t, got)) != tc.want || got["size"] != int64(10) || got["offset"] != tc.wantOffset || got["id"] != 9 {
			t.Fatalf("%s/%s: %v", tc.offset, tc.limit, withoutData(got))
		}
		if id, _ := got["ident"].(string); !ident.MatchString(id) {
			t.Fatalf("ident %q", id)
		}
		if *closes != 1 {
			t.Fatalf("closes=%d", *closes)
		}
	}
}

func TestReadBytesZeroSizeFile(t *testing.T) {
	s, _, _ := bytesFixture(t, nil)
	got := s.handle(bytesReq("f", "0", "5"))
	wantOK(t, got)
	if len(respData(t, got)) != 0 || got["size"] != int64(0) {
		t.Fatalf("%v", got)
	}
}

func TestReadBytesMaxLimit(t *testing.T) {
	body := bytes.Repeat([]byte("ab"), maxReadBytes/2+100)
	s, _, _ := bytesFixture(t, body)
	got := s.handle(bytesReq("f", "1", fmt.Sprint(maxReadBytes)))
	wantOK(t, got)
	data := respData(t, got)
	if len(data) != maxReadBytes || !bytes.Equal(data, body[1:1+maxReadBytes]) {
		t.Fatalf("len=%d", len(data))
	}
	if got["size"] != int64(len(body)) {
		t.Fatalf("size %v", got["size"])
	}
}

func TestReadBytesMalformedBeforeOpen(t *testing.T) {
	for _, tc := range []struct{ offset, limit string }{
		{"0", "0"}, {"0", fmt.Sprint(maxReadBytes + 1)}, {"0", ""}, {"0", "-1"}, {"0", "1.5"},
		{"0", "null"}, {"0", `"3"`},
		{"-1", "1"}, {"1.5", "1"}, {"0.5", "1"}, {`"x"`, "1"}, {"null", "1"}, {"", "1"}, {"9007199254740992", "1"},
		{"-0", "1"}, {"00", "1"}, {"0x0", "1"},
	} {
		s, _, _ := bytesFixture(t, []byte("abc"))
		opened := false
		// A missing path would yield E_NOT_FOUND if validation ran after the open.
		s.rangeContext = func() (context.Context, context.CancelFunc) {
			opened = true
			return context.WithTimeout(context.Background(), time.Second)
		}
		wantErr(t, s.handle(bytesReq("missing", tc.offset, tc.limit)), codeMalformed)
		if opened {
			t.Fatalf("%q/%q reached the open stage", tc.offset, tc.limit)
		}
	}
}

func TestRangeAndTailStillRejectZero(t *testing.T) {
	s, _, _ := bytesFixture(t, []byte("a\n"))
	wantErr(t, s.handle(request{ID: 1, Op: opReadRange, Path: "f", Offset: json.RawMessage("0"), Limit: json.RawMessage("1")}), codeMalformed)
	wantErr(t, s.handle(request{ID: 1, Op: opReadTail, Path: "f", Tail: json.RawMessage("0")}), codeMalformed)
}

func TestReadBytesPreadNeverExceedsWindow(t *testing.T) {
	s, _, _ := bytesFixture(t, bytes.Repeat([]byte("x"), 100))
	total := 0
	s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) {
		if off < 10 || off+int64(len(p)) > 30 {
			t.Fatalf("read outside window: off=%d len=%d", off, len(p))
		}
		if off != 10+int64(total) { // retries continue where the short read stopped: 10, 17, 24
			t.Fatalf("retry offset=%d, want %d", off, 10+int64(total))
		}
		n := min(len(p), 7) // force short reads
		total += n
		return n, nil
	}
	got := s.handle(bytesReq("f", "10", "20"))
	wantOK(t, got)
	if total != 20 || len(respData(t, got)) != 20 {
		t.Fatalf("total=%d", total)
	}
}

func TestReadBytesShrinkAfterOpenReturnsShort(t *testing.T) {
	s, _, _ := bytesFixture(t, []byte("0123456789"))
	s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) {
		if len(p) < 6 {
			t.Fatalf("window %d", len(p))
		}
		copy(p, "234")
		return 3, io.EOF
	}
	got := s.handle(bytesReq("f", "2", "6"))
	wantOK(t, got)
	if string(respData(t, got)) != "234" || got["size"] != int64(10) {
		t.Fatalf("%v", got)
	}
}

func TestReadBytesIdentChanges(t *testing.T) {
	s, root, _ := bytesFixture(t, []byte("0123456789"))
	ident := func() string {
		got := s.handle(bytesReq("f", "0", "1"))
		wantOK(t, got)
		return got["ident"].(string)
	}
	first := ident()
	if ident() != first {
		t.Fatal("ident unstable without mutation")
	}
	// In-place metadata change with an explicit, distinct mtime.
	if err := os.Chtimes(filepath.Join(root, "f"), time.Unix(1_000_000_000, 0), time.Unix(1_000_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	second := ident()
	if second == first {
		t.Fatal("ident unchanged after in-place mtime change")
	}
	// Same-path replacement: write a new file and rename it over.
	tmp := filepath.Join(root, "g")
	if err := os.WriteFile(tmp, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, time.Unix(1_000_000_000, 0), time.Unix(1_000_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "f")); err != nil {
		t.Fatal(err)
	}
	if third := ident(); third == second {
		t.Fatal("ident unchanged after replacement")
	}
}

// A same-size, same-inode in-place rewrite changes only the time parts of ident.
func TestReadBytesIdentChangesOnSameSizeInPlaceRewrite(t *testing.T) {
	s, root, _ := bytesFixture(t, []byte("0123456789"))
	identOf := func() []string {
		got := s.handle(bytesReq("f", "0", "1"))
		wantOK(t, got)
		if got["size"] != int64(10) {
			t.Fatalf("size %v", got["size"])
		}
		parts := strings.Split(got["ident"].(string), ":")
		if len(parts) != 4 {
			t.Fatalf("ident %q", got["ident"])
		}
		return parts
	}
	before := identOf()
	time.Sleep(20 * time.Millisecond) // let mtime/ctime advance
	f, err := os.OpenFile(filepath.Join(root, "f"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("abcdefghij"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	after := identOf()
	if before[0] != after[0] || before[1] != after[1] {
		t.Fatalf("dev:ino changed: %v -> %v", before, after)
	}
	if before[2] == after[2] || before[3] == after[3] {
		t.Fatalf("mtime/ctime parts must both differ: %v -> %v", before, after)
	}
}

func TestReadBytesMutationBetweenFstatAndPreadUndetected(t *testing.T) {
	s, root, _ := bytesFixture(t, []byte("0123456789"))
	before := s.handle(bytesReq("f", "0", "10"))
	wantOK(t, before)
	s.fstat = func(fd int, st *unix.Stat_t) error {
		if err := unix.Fstat(fd, st); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "f"), []byte("abcdefghij"), 0600)
	}
	got := s.handle(bytesReq("f", "0", "10"))
	wantOK(t, got)
	if string(respData(t, got)) != "abcdefghij" {
		t.Fatalf("data %q", respData(t, got))
	}
	if got["size"] != int64(10) || got["ident"] != before["ident"] {
		t.Fatalf("response must reflect the fstat sample: %v", withoutData(got))
	}
}

func TestReadBytesGrowthAfterFstatClampedToOpeningSize(t *testing.T) {
	s, root, _ := bytesFixture(t, []byte("0123456789"))
	s.fstat = func(fd int, st *unix.Stat_t) error {
		if err := unix.Fstat(fd, st); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(root, "f"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write([]byte("abcdefghij"))
		return err
	}
	got := s.handle(bytesReq("f", "0", "100"))
	wantOK(t, got)
	if string(respData(t, got)) != "0123456789" || got["size"] != int64(10) {
		t.Fatalf("%q %v", respData(t, got), withoutData(got))
	}
}

func TestReadBytesOffsetAtOrPastSizeIssuesNoPread(t *testing.T) {
	for _, offset := range []string{"10", "11", "1000"} {
		s, _, _ := bytesFixture(t, []byte("0123456789"))
		reads := 0
		s.fileReadAt = func(*os.File, []byte, int64) (int, error) { reads++; return 0, nil }
		got := s.handle(bytesReq("f", offset, "5"))
		wantOK(t, got)
		if reads != 0 || len(respData(t, got)) != 0 || got["size"] != int64(10) {
			t.Fatalf("offset %s: reads=%d %v", offset, reads, withoutData(got))
		}
	}
}

func TestReadBytesZeroProgressWithoutErrorIsIO(t *testing.T) {
	s, _, closes := bytesFixture(t, []byte("abc"))
	s.fileReadAt = func(*os.File, []byte, int64) (int, error) { return 0, nil }
	wantErr(t, s.handle(bytesReq("f", "0", "2")), codeIO)
	if *closes != 1 {
		t.Fatalf("closes=%d", *closes)
	}
}

// Expiry landing at the post-fstat check must win over the file-type rejection.
func TestReadBytesExpiryBeatsFileTypeRejection(t *testing.T) {
	s, root, closes := bytesFixture(t, []byte("abc"))
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "p"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"d", "p"} {
		*closes = 0
		expireAt(s, 3)
		wantErr(t, s.handle(bytesReq(path, "0", "1")), codeTimeout)
		if *closes != 1 {
			t.Fatalf("%s closes=%d", path, *closes)
		}
	}
}

func TestReadBytesFileTypesAndPaths(t *testing.T) {
	s, root, closes := bytesFixture(t, []byte("abc"))
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "p"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, code string }{
		{"d", codeIsDir}, {"p", codeNotFile}, {"l", codeSymlink},
		{"../x", codeEscape}, {"nope", codeNotFound},
	} {
		*closes = 0
		wantErr(t, s.handle(bytesReq(tc.path, "0", "1")), tc.code)
		wantClosed := 1
		if tc.path == "../x" || tc.path == "l" || tc.path == "nope" {
			wantClosed = 0
		}
		if tc.path == "p" {
			// A readerless FIFO may be rejected at open (ENXIO) or by fstat.
			continue
		}
		if *closes != wantClosed {
			t.Fatalf("%s closes=%d want %d", tc.path, *closes, wantClosed)
		}
	}
}

func TestReadBytesInjectedFailures(t *testing.T) {
	t.Run("fstat", func(t *testing.T) {
		s, _, closes := bytesFixture(t, []byte("abc"))
		s.fstat = func(int, *unix.Stat_t) error { return unix.EACCES }
		wantErr(t, s.handle(bytesReq("f", "0", "1")), classify(unix.EACCES))
		if *closes != 1 {
			t.Fatalf("closes=%d", *closes)
		}
	})
	t.Run("nonblock", func(t *testing.T) {
		s, _, closes := bytesFixture(t, []byte("abc"))
		s.nonblock = func(int, bool) error { return unix.EACCES }
		wantErr(t, s.handle(bytesReq("f", "0", "1")), codeIO)
		if *closes != 1 {
			t.Fatalf("closes=%d", *closes)
		}
	})
	t.Run("pread", func(t *testing.T) {
		s, _, closes := bytesFixture(t, []byte("abc"))
		s.fileReadAt = func(*os.File, []byte, int64) (int, error) { return 0, unix.EIO }
		wantErr(t, s.handle(bytesReq("f", "0", "1")), codeIO)
		if *closes != 1 {
			t.Fatalf("closes=%d", *closes)
		}
	})
	t.Run("pread-bad-count", func(t *testing.T) {
		s, _, _ := bytesFixture(t, []byte("abc"))
		s.fileReadAt = func(_ *os.File, p []byte, _ int64) (int, error) { return len(p) + 1, nil }
		wantErr(t, s.handle(bytesReq("f", "0", "1")), codeIO)
		s.fileReadAt = func(*os.File, []byte, int64) (int, error) { return -1, nil }
		wantErr(t, s.handle(bytesReq("f", "0", "1")), codeIO)
	})
	t.Run("pread-eintr-retried", func(t *testing.T) {
		s, _, _ := bytesFixture(t, []byte("abc"))
		first := true
		s.fileReadAt = func(f *os.File, p []byte, off int64) (int, error) {
			if first {
				first = false
				return 0, syscall.EINTR
			}
			return f.ReadAt(p, off)
		}
		got := s.handle(bytesReq("f", "0", "2"))
		wantOK(t, got)
		if string(respData(t, got)) != "ab" {
			t.Fatal("data")
		}
	})
}

// Context check order for an existing file: 1 pre-open, 2 after open, 3 after
// fstat, 4 after nonblock, 5 after each pread, 6 before success.
func TestReadBytesExpiryPrecedence(t *testing.T) {
	failers := map[string]func(*server){
		"none":     func(*server) {},
		"fstat":    func(s *server) { s.fstat = func(int, *unix.Stat_t) error { return unix.EIO } },
		"nonblock": func(s *server) { s.nonblock = func(int, bool) error { return unix.EIO } },
		"pread": func(s *server) {
			s.fileReadAt = func(*os.File, []byte, int64) (int, error) { return 0, unix.EIO }
		},
	}
	// expiry call -> which injected failure it must mask (3=fstat, 4=nonblock, 5=pread)
	cases := []struct {
		at     int
		failer string
		closes int
	}{
		{1, "none", 0}, {2, "none", 1}, {3, "none", 1}, {4, "none", 1}, {5, "none", 1}, {6, "none", 1},
		{3, "fstat", 1}, {4, "nonblock", 1}, {5, "pread", 1},
	}
	for _, tc := range cases {
		s, _, closes := bytesFixture(t, []byte("abc"))
		failers[tc.failer](s)
		expireAt(s, tc.at)
		wantErr(t, s.handle(bytesReq("f", "0", "2")), codeTimeout)
		if *closes != tc.closes {
			t.Fatalf("at=%d failer=%s closes=%d want %d", tc.at, tc.failer, *closes, tc.closes)
		}
	}
	// Control: no expiry, failers surface their own code.
	s, _, _ := bytesFixture(t, []byte("abc"))
	failers["nonblock"](s)
	wantErr(t, s.handle(bytesReq("f", "0", "2")), codeIO)
}

func TestReadBytesExpiryMasksOpenFailure(t *testing.T) {
	s, _, closes := bytesFixture(t, []byte("abc"))
	expireAt(s, 2) // pre-open check passes; the check after the failed open expires
	wantErr(t, s.handle(bytesReq("missing", "0", "1")), codeTimeout)
	if *closes != 0 {
		t.Fatalf("closes=%d", *closes)
	}
	s, _, _ = bytesFixture(t, []byte("abc"))
	wantErr(t, s.handle(bytesReq("missing", "0", "1")), codeNotFound)
}
