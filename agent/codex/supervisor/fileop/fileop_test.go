package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newTestServer anchors a server at a fresh temp-dir worktree root, exercising the
// REAL openat2 kernel path. It fails loudly if openat2 is unavailable rather than
// silently degrading (the whole point of this helper is the kernel mechanism).
func newTestServer(t *testing.T) (*server, string) {
	t.Helper()
	root := t.TempDir()
	fd, err := unix.Open(root, unix.O_DIRECTORY|unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open root dirfd: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	s := newServer(fd)
	// Probe openat2 directly so a missing kernel feature is an explicit failure.
	if _, perr := s.openBeneath(".", unix.O_PATH, 0); perr != nil {
		if errors.Is(perr, unix.ENOSYS) {
			t.Fatalf("openat2 unavailable (ENOSYS): this helper REQUIRES openat2; not falling back to a realpath check")
		}
		t.Fatalf("openat2 probe on root failed: %v", perr)
	}
	return s, root
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func wantOK(t *testing.T, resp map[string]any) {
	t.Helper()
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("want ok, got %v", resp)
	}
}

func wantErr(t *testing.T, resp map[string]any, code string) {
	t.Helper()
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("want error %s, got ok response %v", code, resp)
	}
	if got, _ := resp["code"].(string); got != code {
		t.Fatalf("want code %s, got %v", code, resp)
	}
}

func wantErrOneOf(t *testing.T, resp map[string]any, codes ...string) {
	t.Helper()
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("want an error in %v, got ok %v", codes, resp)
	}
	got, _ := resp["code"].(string)
	for _, c := range codes {
		if got == c {
			return
		}
	}
	t.Fatalf("code %q not in %v (%v)", got, codes, resp)
}

// respData decodes the base64 body of a read response, respecting int/float64.
func respData(t *testing.T, resp map[string]any) []byte {
	t.Helper()
	enc, _ := resp["data"].(string)
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode data: %v", err)
	}
	return data
}

func TestWriteReadRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	wantOK(t, s.handle(request{ID: 1, Op: opWrite, Path: "hello.txt", Data: b64("hello world")}))
	r := s.handle(request{ID: 2, Op: opRead, Path: "hello.txt"})
	wantOK(t, r)
	if got := string(respData(t, r)); got != "hello world" {
		t.Fatalf("read body = %q, want %q", got, "hello world")
	}
	if sz, _ := r["size"].(int64); sz != 11 {
		t.Fatalf("read size = %v, want 11", r["size"])
	}
	// Truncating rewrite replaces the body.
	wantOK(t, s.handle(request{ID: 3, Op: opWrite, Path: "hello.txt", Data: b64("hi")}))
	r2 := s.handle(request{ID: 4, Op: opRead, Path: "hello.txt"})
	if got := string(respData(t, r2)); got != "hi" {
		t.Fatalf("after rewrite = %q, want %q", got, "hi")
	}
}

func TestStat(t *testing.T) {
	s, _ := newTestServer(t)
	// Missing target is ok+exists:false, NOT an error.
	miss := s.handle(request{ID: 1, Op: opStat, Path: "nope"})
	wantOK(t, miss)
	if ex, _ := miss["exists"].(bool); ex {
		t.Fatalf("missing stat exists = true, want false: %v", miss)
	}
	wantOK(t, s.handle(request{ID: 2, Op: opWrite, Path: "f", Data: b64("abcde")}))
	st := s.handle(request{ID: 3, Op: opStat, Path: "f"})
	wantOK(t, st)
	if ex, _ := st["exists"].(bool); !ex {
		t.Fatalf("stat exists = false, want true")
	}
	if ty, _ := st["type"].(string); ty != "file" {
		t.Fatalf("stat type = %q, want file", ty)
	}
	if sz, _ := st["size"].(int64); sz != 5 {
		t.Fatalf("stat size = %v, want 5", st["size"])
	}
	wantOK(t, s.handle(request{ID: 4, Op: opMkdir, Path: "d"}))
	sd := s.handle(request{ID: 5, Op: opStat, Path: "d"})
	if ty, _ := sd["type"].(string); ty != "dir" {
		t.Fatalf("dir stat type = %q, want dir", ty)
	}
}

func TestMkdirAndExists(t *testing.T) {
	s, _ := newTestServer(t)
	wantOK(t, s.handle(request{ID: 1, Op: opMkdir, Path: "a"}))
	wantOK(t, s.handle(request{ID: 2, Op: opMkdir, Path: "a/b"}))
	wantErr(t, s.handle(request{ID: 3, Op: opMkdir, Path: "a"}), codeExists)
	// mkdir under a missing parent fails ENOENT, not a silent create.
	wantErr(t, s.handle(request{ID: 4, Op: opMkdir, Path: "missing/child"}), codeNotFound)
}

func TestRename(t *testing.T) {
	s, _ := newTestServer(t)
	wantOK(t, s.handle(request{ID: 1, Op: opWrite, Path: "a", Data: b64("payload")}))
	wantOK(t, s.handle(request{ID: 2, Op: opMkdir, Path: "sub"}))
	wantOK(t, s.handle(request{ID: 3, Op: opRename, Path: "a", NewPath: "sub/b"}))
	// Old name gone, new name carries the content.
	old := s.handle(request{ID: 4, Op: opStat, Path: "a"})
	if ex, _ := old["exists"].(bool); ex {
		t.Fatalf("old name still exists after rename")
	}
	r := s.handle(request{ID: 5, Op: opRead, Path: "sub/b"})
	wantOK(t, r)
	if got := string(respData(t, r)); got != "payload" {
		t.Fatalf("renamed body = %q", got)
	}
	// Rename of a missing source fails typed.
	wantErr(t, s.handle(request{ID: 6, Op: opRename, Path: "ghost", NewPath: "x"}), codeNotFound)
}

func TestUnlinkAndRmdir(t *testing.T) {
	s, _ := newTestServer(t)
	wantOK(t, s.handle(request{ID: 1, Op: opWrite, Path: "f", Data: b64("x")}))
	wantOK(t, s.handle(request{ID: 2, Op: opUnlink, Path: "f"}))
	gone := s.handle(request{ID: 3, Op: opStat, Path: "f"})
	if ex, _ := gone["exists"].(bool); ex {
		t.Fatalf("file still exists after unlink")
	}
	// unlink of a directory is refused; rmdir removes an empty one.
	wantOK(t, s.handle(request{ID: 4, Op: opMkdir, Path: "d"}))
	wantErr(t, s.handle(request{ID: 5, Op: opUnlink, Path: "d"}), codeIsDir)
	wantOK(t, s.handle(request{ID: 6, Op: opRmdir, Path: "d"}))
	// rmdir of a non-empty directory is refused.
	wantOK(t, s.handle(request{ID: 7, Op: opMkdir, Path: "e"}))
	wantOK(t, s.handle(request{ID: 8, Op: opWrite, Path: "e/f", Data: b64("y")}))
	wantErr(t, s.handle(request{ID: 9, Op: opRmdir, Path: "e"}), codeNotEmpty)
}

func TestList(t *testing.T) {
	s, _ := newTestServer(t)
	wantOK(t, s.handle(request{ID: 1, Op: opMkdir, Path: "d"}))
	wantOK(t, s.handle(request{ID: 2, Op: opWrite, Path: "d/f1", Data: b64("1")}))
	wantOK(t, s.handle(request{ID: 3, Op: opWrite, Path: "d/f2", Data: b64("2")}))
	wantOK(t, s.handle(request{ID: 4, Op: opMkdir, Path: "d/sub"}))
	l := s.handle(request{ID: 5, Op: opList, Path: "d"})
	wantOK(t, l)
	types := map[string]string{}
	for _, e := range l["entries"].([]map[string]any) {
		types[e["name"].(string)] = e["type"].(string)
	}
	if types["f1"] != "file" || types["f2"] != "file" || types["sub"] != "dir" {
		t.Fatalf("list types = %v", types)
	}
	// Listing the root via "." works (RESOLVE_BENEATH permits the anchor itself).
	lr := s.handle(request{ID: 6, Op: opList, Path: "."})
	wantOK(t, lr)
	if len(lr["entries"].([]map[string]any)) != 1 {
		t.Fatalf("root list = %v, want 1 entry (d)", lr["entries"])
	}
}

func TestEscapeAbsolutePathBlocked(t *testing.T) {
	s, _ := newTestServer(t)
	// An absolute path outside root is refused by the kernel (RESOLVE_BENEATH).
	wantErr(t, s.handle(request{ID: 1, Op: opRead, Path: "/etc/passwd"}), codeEscape)
	wantErr(t, s.handle(request{ID: 2, Op: opStat, Path: "/etc/passwd"}), codeEscape)
	wantErr(t, s.handle(request{ID: 3, Op: opWrite, Path: "/tmp/uzi-escape", Data: b64("x")}), codeEscape)
	if _, err := os.Stat("/tmp/uzi-escape"); err == nil {
		t.Fatalf("escape write created a file outside root")
	}
	wantErr(t, s.handle(request{ID: 4, Op: opUnlink, Path: "/etc/hostname"}), codeEscape)
}

func TestEscapeDotDotBlocked(t *testing.T) {
	s, _ := newTestServer(t)
	wantErr(t, s.handle(request{ID: 1, Op: opRead, Path: "../../etc/passwd"}), codeEscape)
	wantErr(t, s.handle(request{ID: 2, Op: opStat, Path: "../secret"}), codeEscape)
	wantErr(t, s.handle(request{ID: 3, Op: opWrite, Path: "../evil", Data: b64("x")}), codeEscape)
	// A bare ".." final component is refused before any syscall.
	wantErr(t, s.handle(request{ID: 4, Op: opUnlink, Path: ".."}), codeEscape)
	wantErr(t, s.handle(request{ID: 5, Op: opMkdir, Path: "a/.."}), codeEscape)
}

func TestSymlinkComponentOutsideBlocked(t *testing.T) {
	s, root := newTestServer(t)
	// A symlink whose target is OUTSIDE root: NO_SYMLINKS refuses to follow it at
	// all, so the failure is E_SYMLINK (ELOOP), never a followed escape.
	if err := os.Symlink("/etc", filepath.Join(root, "linkdir")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Intermediate symlink component rejected.
	wantErr(t, s.handle(request{ID: 1, Op: opRead, Path: "linkdir/passwd"}), codeSymlink)
	// Final symlink component rejected (stat does not become exists:false).
	wantErr(t, s.handle(request{ID: 2, Op: opStat, Path: "linkdir"}), codeSymlink)
	// A symlink to a single outside file, read directly, is rejected too.
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "pwlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wantErr(t, s.handle(request{ID: 3, Op: opRead, Path: "pwlink"}), codeSymlink)
}

func TestSymlinkInsideAlsoBlocked(t *testing.T) {
	s, root := newTestServer(t)
	// NO_SYMLINKS is intentional: even a symlink whose target is INSIDE the root is
	// rejected. The helper never follows a symlink; the broker composes explicit ops.
	wantOK(t, s.handle(request{ID: 1, Op: opWrite, Path: "real", Data: b64("inside")}))
	if err := os.Symlink("real", filepath.Join(root, "inlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wantErr(t, s.handle(request{ID: 2, Op: opRead, Path: "inlink"}), codeSymlink)
	wantErr(t, s.handle(request{ID: 3, Op: opStat, Path: "inlink"}), codeSymlink)
	// An intermediate inside-symlink component is likewise rejected.
	wantOK(t, s.handle(request{ID: 4, Op: opMkdir, Path: "realdir"}))
	wantOK(t, s.handle(request{ID: 5, Op: opWrite, Path: "realdir/f", Data: b64("y")}))
	if err := os.Symlink("realdir", filepath.Join(root, "dlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wantErr(t, s.handle(request{ID: 6, Op: opRead, Path: "dlink/f"}), codeSymlink)
}

func TestSymlinkSwapTOCTOU(t *testing.T) {
	s, root := newTestServer(t)
	// A legit regular file that reads fine.
	wantOK(t, s.handle(request{ID: 1, Op: opWrite, Path: "swap", Data: b64("legit")}))
	r := s.handle(request{ID: 2, Op: opRead, Path: "swap"})
	wantOK(t, r)
	if string(respData(t, r)) != "legit" {
		t.Fatalf("pre-swap read wrong")
	}
	// Between "checks" the entry is swapped for a symlink pointing outside root.
	// With openat2 there is no separate check: the very next open fails atomically
	// with E_SYMLINK rather than following the swapped-in link. This proves the
	// mechanism is NOT check-then-use.
	if err := os.Remove(filepath.Join(root, "swap")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "swap")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wantErr(t, s.handle(request{ID: 3, Op: opRead, Path: "swap"}), codeSymlink)
	wantErr(t, s.handle(request{ID: 4, Op: opWrite, Path: "swap", Data: b64("x")}), codeSymlink)
}

func TestGitDenied(t *testing.T) {
	s, _ := newTestServer(t)
	// A .git component is denied as defense-in-depth (openat2 is primary), at ANY depth.
	wantErr(t, s.handle(request{ID: 1, Op: opWrite, Path: ".git/config", Data: b64("x")}), codeDenied)
	wantErr(t, s.handle(request{ID: 2, Op: opStat, Path: ".git"}), codeDenied)
	wantErr(t, s.handle(request{ID: 3, Op: opMkdir, Path: ".git"}), codeDenied)
	wantErr(t, s.handle(request{ID: 4, Op: opRename, Path: "x", NewPath: ".git/y"}), codeDenied)
	// A nested/submodule .git component is denied too (defect 1 defense-in-depth), not
	// just the first component as the old first-component-only screen allowed.
	wantOK(t, s.handle(request{ID: 5, Op: opMkdir, Path: "sub"}))
	wantErr(t, s.handle(request{ID: 6, Op: opMkdir, Path: "sub/.git"}), codeDenied)
	wantErr(t, s.handle(request{ID: 7, Op: opWrite, Path: "sub/.git/x", Data: b64("x")}), codeDenied)
	// A look-alike name is NOT a .git component and is allowed.
	wantOK(t, s.handle(request{ID: 8, Op: opWrite, Path: ".gitignore", Data: b64("*")}))
	wantOK(t, s.handle(request{ID: 9, Op: opWrite, Path: "sub/.gitkeep", Data: b64("")}))
}

// TestGitBypassViaDotDotDenied is the regression for defect 1: openat2 uses
// RESOLVE_BENEATH, which PERMITS "..", so "sub/../.git/config" resolves to the real
// .git/config beneath root even though its FIRST component is the innocent "sub". The
// old first-component-only screen passed it; the per-component policy denies it before
// any syscall. Fails against the pre-fix code (the write would overwrite the planted
// .git/config and the read would return its bytes).
func TestGitBypassViaDotDotDenied(t *testing.T) {
	s, root := newTestServer(t)
	// Plant a real git dir with sentinel content, plus a real intermediate dir so the
	// ".."-bypass paths actually have a target the kernel would resolve to.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("SECRET-GIT"), 0o600); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	for _, p := range []string{"sub/../.git/config", "a/b/../../.git/config"} {
		wantErrOneOf(t, s.handle(request{ID: 1, Op: opWrite, Path: p, Data: b64("PWNED")}), codeDenied, codeEscape)
		wantErrOneOf(t, s.handle(request{ID: 2, Op: opRead, Path: p}), codeDenied, codeEscape)
		wantErrOneOf(t, s.handle(request{ID: 3, Op: opStat, Path: p}), codeDenied, codeEscape)
	}
	// A nested .git component reached without ".." is denied outright.
	wantErr(t, s.handle(request{ID: 4, Op: opStat, Path: "sub/.git/x"}), codeDenied)

	// Nothing above created or modified the real .git/config.
	got, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil {
		t.Fatalf("read back .git/config: %v", err)
	}
	if string(got) != "SECRET-GIT" {
		t.Fatalf(".git/config was altered through a bypass: %q", got)
	}
}

// TestPathComponentPolicy pins the per-component pathname policy: ".." anywhere and an
// absolute path are E_ESCAPE (screened before any syscall), empty/"." interior
// components are E_MALFORMED, and a bare "." (the anchor) is permitted. Fails against
// the pre-fix code, which left these to openat2 (yielding E_NOT_FOUND/E_ESCAPE from
// the kernel rather than the deterministic policy code).
func TestPathComponentPolicy(t *testing.T) {
	s, _ := newTestServer(t)
	for _, p := range []string{"..", "../x", "a/../../x", "sub/../other"} {
		wantErr(t, s.handle(request{ID: 1, Op: opStat, Path: p}), codeEscape)
	}
	wantErr(t, s.handle(request{ID: 2, Op: opStat, Path: "/etc/passwd"}), codeEscape)
	wantErr(t, s.handle(request{ID: 3, Op: opRead, Path: "/etc/passwd"}), codeEscape)
	for _, p := range []string{"a//b", "a/./b", "./a", "a/.", "a/b/"} {
		wantErr(t, s.handle(request{ID: 4, Op: opStat, Path: p}), codeMalformed)
	}
	// A bare "." is the anchor and stays permitted (root listing works).
	wantOK(t, s.handle(request{ID: 5, Op: opList, Path: "."}))
}

// TestSpecialFilesNoHang is the regression for defect 2: opening a FIFO/socket with a
// blocking open (the pre-fix code) hangs this single-threaded helper forever. Each op
// is run under a 2s timeout that FAILS if it hangs; all must return E_NOT_FILE
// promptly, and a regular file must still round-trip.
func TestSpecialFilesNoHang(t *testing.T) {
	s, root := newTestServer(t)
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	runWithTimeout := func(name string, fn func() map[string]any) map[string]any {
		t.Helper()
		ch := make(chan map[string]any, 1)
		go func() { ch <- fn() }()
		select {
		case resp := <-ch:
			return resp
		case <-time.After(2 * time.Second):
			t.Fatalf("%s on a special file hung (blocking open not fixed)", name)
			return nil
		}
	}

	rd := runWithTimeout("read", func() map[string]any {
		return s.handle(request{ID: 1, Op: opRead, Path: "pipe"})
	})
	wantErr(t, rd, codeNotFile)

	wr := runWithTimeout("write", func() map[string]any {
		return s.handle(request{ID: 2, Op: opWrite, Path: "pipe", Data: b64("data")})
	})
	wantErr(t, wr, codeNotFile)

	// list on a FIFO is ENOTDIR: O_DIRECTORY is rejected before any fifo blocking.
	ls := runWithTimeout("list", func() map[string]any {
		return s.handle(request{ID: 3, Op: opList, Path: "pipe"})
	})
	wantErr(t, ls, codeNotDir)

	// A regular file still reads and writes fine.
	wantOK(t, s.handle(request{ID: 4, Op: opWrite, Path: "reg", Data: b64("regular")}))
	rr := s.handle(request{ID: 5, Op: opRead, Path: "reg"})
	wantOK(t, rr)
	if got := string(respData(t, rr)); got != "regular" {
		t.Fatalf("regular read = %q, want %q", got, "regular")
	}

	// A unix socket file is also E_NOT_FILE (open returns ENXIO). Guarded because the
	// socket path can exceed the sun_path limit on a long temp-dir path.
	sockPath := filepath.Join(root, "sock")
	if ln, lerr := net.Listen("unix", sockPath); lerr == nil {
		defer ln.Close()
		sr := runWithTimeout("read-socket", func() map[string]any {
			return s.handle(request{ID: 6, Op: opRead, Path: "sock"})
		})
		wantErr(t, sr, codeNotFile)
		sw := runWithTimeout("write-socket", func() map[string]any {
			return s.handle(request{ID: 7, Op: opWrite, Path: "sock", Data: b64("x")})
		})
		wantErr(t, sw, codeNotFile)
	}
}

func TestOversizeReadRejected(t *testing.T) {
	s, _ := newTestServer(t)
	s.maxRead = 8
	wantOK(t, s.handle(request{ID: 1, Op: opWrite, Path: "big", Data: b64("0123456789")}))
	wantErr(t, s.handle(request{ID: 2, Op: opRead, Path: "big"}), codeOversize)
	// A body at the cap still reads.
	wantOK(t, s.handle(request{ID: 3, Op: opWrite, Path: "ok", Data: b64("01234567")}))
	wantOK(t, s.handle(request{ID: 4, Op: opRead, Path: "ok"}))
}

func TestOversizeWriteRejected(t *testing.T) {
	s, _ := newTestServer(t)
	s.maxWrite = 8
	wantErr(t, s.handle(request{ID: 1, Op: opWrite, Path: "big", Data: b64("0123456789")}), codeOversize)
	// The oversize write neither created nor truncated anything.
	miss := s.handle(request{ID: 2, Op: opStat, Path: "big"})
	if ex, _ := miss["exists"].(bool); ex {
		t.Fatalf("oversize write created a file")
	}
}

func TestOversizePathRejected(t *testing.T) {
	s, _ := newTestServer(t)
	long := strings.Repeat("a", maxPathLen+1)
	wantErr(t, s.handle(request{ID: 1, Op: opStat, Path: long}), codeOversize)
}

func TestUnknownOpAndBadBase64(t *testing.T) {
	s, _ := newTestServer(t)
	u := s.handle(request{ID: 9, Op: "frobnicate", Path: "x"})
	wantErr(t, u, codeUnknownOp)
	if id, _ := u["id"].(int); id != 9 {
		t.Fatalf("unknown-op id not echoed: %v", u)
	}
	wantErr(t, s.handle(request{ID: 1, Op: opWrite, Path: "f", Data: "!!!not base64!!!"}), codeMalformed)
	wantErr(t, s.handle(request{ID: 2, Op: opStat, Path: ""}), codeMalformed)
}

// decodeResponses reads every NDJSON response line from a transport buffer.
func decodeResponses(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var resps []map[string]any
	dec := json.NewDecoder(out)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode response: %v", err)
		}
		resps = append(resps, m)
	}
	return resps
}

func TestServeMalformedLineStaysHealthy(t *testing.T) {
	s, _ := newTestServer(t)
	input := strings.Join([]string{
		`{bad json`,
		`{"id":2,"op":"write","path":"f","data":"` + b64("live") + `"}`,
		`{"id":3,"op":"read","path":"f"}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := serve(s, strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, &out)
	if len(resps) != 3 {
		t.Fatalf("got %d responses, want 3: %v", len(resps), resps)
	}
	wantErr(t, resps[0], codeMalformed)
	wantOK(t, resps[1]) // server survived the malformed line and served the write
	wantOK(t, resps[2]) // and the following read
	if got := string(respData(t, resps[2])); got != "live" {
		t.Fatalf("post-malformed read = %q, want %q", got, "live")
	}
}

func TestServeOversizeLineStaysHealthy(t *testing.T) {
	s, _ := newTestServer(t)
	s.lineMax = 64
	input := strings.Repeat("a", 300) + "\n" +
		`{"id":7,"op":"stat","path":"nope"}` + "\n"
	var out bytes.Buffer
	if err := serve(s, strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, &out)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2: %v", len(resps), resps)
	}
	wantErr(t, resps[0], codeOversize) // the over-length line is rejected
	wantOK(t, resps[1])                // and the reader resynchronized to the next frame
}

// TestConcurrentMutatorNeverEscapes hammers one relpath while a background mutator
// atomically flips it between a regular file (content "SAFE") and a symlink to an
// OUTSIDE secret. Because openat2 resolves atomically, every read either sees the
// safe body, or is refused (E_SYMLINK / E_NOT_FOUND) - it NEVER returns the secret.
func TestConcurrentMutatorNeverEscapes(t *testing.T) {
	s, root := newTestServer(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	race := filepath.Join(root, "race")
	tmpReg := filepath.Join(root, ".stage-reg")
	tmpLink := filepath.Join(root, ".stage-link")
	// Seed "race" as a safe regular file so the first reads have a target.
	if err := os.WriteFile(race, []byte("SAFE"), 0o600); err != nil {
		t.Fatalf("seed race: %v", err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Atomically make "race" a symlink to the outside secret.
			os.Remove(tmpLink)
			if os.Symlink(secret, tmpLink) == nil {
				os.Rename(tmpLink, race)
			}
			// Atomically make "race" a safe regular file again.
			os.Remove(tmpReg)
			if os.WriteFile(tmpReg, []byte("SAFE"), 0o600) == nil {
				os.Rename(tmpReg, race)
			}
		}
	}()

	for i := 0; i < 4000; i++ {
		r := s.handle(request{ID: i, Op: opRead, Path: "race"})
		if ok, _ := r["ok"].(bool); ok {
			if got := string(respData(t, r)); got != "SAFE" {
				t.Fatalf("read leaked non-safe content %q (secret escape!)", got)
			}
		} else {
			wantErrOneOf(t, r, codeSymlink, codeNotFound)
		}
	}
	close(stop)
	<-done
}
