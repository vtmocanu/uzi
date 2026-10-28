package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The same path-policy, real openat2 symlink and special-file cases exercised by
// fileop_test.go apply independently to the new operation.
func TestReadRangeContainment(t *testing.T) {
	s, root, _, closes := rangeFixture(t, []byte("inside"))
	for _, tc := range []struct{ path, code string }{
		{"/etc/passwd", codeEscape}, {"../text", codeEscape}, {"sub/../text", codeEscape},
		{".git/config", codeDenied}, {"sub/.git/config", codeDenied},
		{"a//b", codeMalformed}, {"./text", codeMalformed}, {"", codeMalformed},
		{"missing", codeNotFound},
	} {
		req := rangeRequest(1, 1)
		req.Path = tc.path
		wantErr(t, s.handle(req), tc.code)
	}
	if *closes != 0 {
		t.Fatal("policy/open failure acquired fd")
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target, path string }{
		{"outside", "/etc", "outside/passwd"},
		{"inside", "text", "inside"},
		{"directory", "dir", "directory/text"},
	} {
		if err := os.Symlink(tc.target, filepath.Join(root, tc.name)); err != nil {
			t.Fatal(err)
		}
		req := rangeRequest(1, 1)
		req.Path = tc.path
		wantErr(t, s.handle(req), codeSymlink)
	}
	if *closes != 0 {
		t.Fatal("symlink acquired fd")
	}
	req := rangeRequest(1, 1)
	req.Path = "dir"
	wantErr(t, s.handle(req), codeIsDir)
	if *closes != 1 {
		t.Fatal("directory fd not closed")
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	req.Path = "pipe"
	done := make(chan map[string]any, 1)
	go func() { done <- s.handle(req) }()
	select {
	case got := <-done:
		wantErr(t, got, codeNotFile)
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO open blocked")
	}
	if *closes != 2 {
		t.Fatal("FIFO fd not closed")
	}
}

func TestReadRangeSocket(t *testing.T) {
	s, root, reads, closes := rangeFixture(t, []byte("inside"))
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	req := rangeRequest(1, 1)
	req.Path = "socket"
	wantErr(t, s.handle(req), codeNotFile)
	if *reads != 0 || *closes != 0 {
		t.Fatalf("socket open should fail before a body read: reads=%d closes=%d", *reads, *closes)
	}
}

// Real devices need privileges to create. Exercise the same Fstat rejection
// branch on a real confined descriptor, without opening a host device.
func TestReadRangeDeviceTypes(t *testing.T) {
	for _, mode := range []uint32{unix.S_IFCHR, unix.S_IFBLK, unix.S_IFSOCK} {
		s, _, reads, closes := rangeFixture(t, []byte("inside"))
		s.fstat = func(fd int, st *unix.Stat_t) error {
			if err := unix.Fstat(fd, st); err != nil {
				return err
			}
			st.Mode = mode
			return nil
		}
		wantErr(t, s.handle(rangeRequest(1, 1)), codeNotFile)
		if *reads != 0 || *closes != 1 {
			t.Fatalf("nonregular mode %o: reads=%d closes=%d", mode, *reads, *closes)
		}
	}
}

func TestReadRangeLineLimit(t *testing.T) {
	s, _, _, _ := rangeFixture(t, []byte("a\n"+string(makeLines(2000))+"end"))
	checkRange(t, s, rangeRequest(2, 2000), string(makeLines(2000)), 2000, false, true, false)
}
func makeLines(n int) []byte {
	b := make([]byte, 2*n)
	for i := 0; i < len(b); i += 2 {
		b[i] = 'b'
		b[i+1] = '\n'
	}
	return b
}
