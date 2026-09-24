package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// holderRun is an in-process --hold-cache run over pipes.
type holderRun struct {
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan int
}

// startHolder runs --hold-cache for token under root in a goroutine.
func startHolder(t *testing.T, root, token string) *holderRun {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &holderRun{in: inW, out: bufio.NewReader(outR), done: make(chan int, 1)}
	env := testModeEnv(outW, t.TempDir(), nil)
	env.stdin = inR
	args := []string{modeHoldCache, "--expect-uid", fmt.Sprint(os.Geteuid()), "--cache-root", root, "--cache-token", token}
	go func() {
		code := runMode(args, env)
		_ = outW.Close()
		h.done <- code
	}()
	t.Cleanup(func() { _ = inW.Close(); _, _ = io.Copy(io.Discard, h.out) })
	return h
}

func (h *holderRun) line(t *testing.T) string {
	t.Helper()
	s, err := h.out.ReadString('\n')
	if err != nil {
		t.Fatalf("holder line: %q %v", s, err)
	}
	return strings.TrimSuffix(s, "\n")
}

func (h *holderRun) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(h.in, s); err != nil {
		t.Fatal(err)
	}
}

// finish closes stdin (EOF), and returns the last line and the exit code.
func (h *holderRun) finish(t *testing.T) (string, int) {
	t.Helper()
	_ = h.in.Close()
	last := h.line(t)
	return last, <-h.done
}

// newCacheRoot is a 0700 cache root owned by the caller.
func newCacheRoot(t *testing.T) string {
	t.Helper()
	return newRoots(t).cache
}

func readyLine(root, token string) string {
	return `{"event":"cache_ready","path":"` + root + "/" + token + `"}`
}

func TestHoldCacheReleaseDrainedRemoves(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	root := newCacheRoot(t)
	h := startHolder(t, root, testToken)
	if got := h.line(t); got != readyLine(root, testToken) {
		t.Fatalf("ready = %q", got)
	}
	dir := filepath.Join(root, testToken)
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil || st.Mode&0o7777 != 0o700 || int(st.Uid) != os.Geteuid() {
		t.Fatalf("cache dir: mode %o uid %d err %v", st.Mode, st.Uid, err)
	}
	for _, sub := range cacheSubdirs {
		if fi, err := os.Lstat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("subdir %s: %v", sub, err)
		}
	}
	// A module-cache-shaped tree inside.
	mkTree(t, filepath.Join(dir, "gomod", "example.com"))

	// While held, a second open file description cannot take the lock.
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("second LOCK_NB while held = %v, want EWOULDBLOCK", err)
	}

	h.send(t, `{"op":"release","drained":true}`+"\n")
	last, code := h.finish(t)
	if last != `{"event":"cache_cleanup","state":"removed","reason":""}` || code != 0 {
		t.Fatalf("cleanup = %q exit %d", last, code)
	}
	requireGone(t, dir)
}

func TestHoldCacheWithoutAttestationRetains(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	oversize := `{"op":"release","drained":false}` + strings.Repeat(" ", maxReleaseLine) + "\n"
	for name, input := range map[string]string{
		"bare EOF":               "",
		"drained false":          `{"op":"release","drained":false}` + "\n",
		"true then false (last)": `{"op":"release","drained":true}` + "\n" + `{"op":"release","drained":false}` + "\n",
		"false then true":        `{"op":"release","drained":false}` + "\n" + `{"op":"release","drained":true}` + "\n",
		"false, junk, true":      `{"op":"release","drained":false}` + "\nnot json\n" + `{"op":"release","drained":true}`,
		"duplicate drained":      `{"op":"release","drained":false,"drained":true}` + "\n",
		"duplicate op":           `{"op":"release","op":"release","drained":true}` + "\n",
		"drained null":           `{"op":"release","drained":null}` + "\n",
		"garbage only":           "release\n" + `{"op":"release","drained":"true"}` + "\n" + `{"op":"release","drained":1}` + "\n",
		"extra key":              `{"op":"release","drained":true,"force":true}` + "\n",
		"wrong op":               `{"op":"Release","drained":true}` + "\n",
		"key case":               `{"Op":"release","drained":true}` + "\n",
		"trailing data":          `{"op":"release","drained":true} {}` + "\n",
		"oversize true":          `{"op":"release","drained":true}` + strings.Repeat(" ", maxReleaseLine) + "\n",
		"oversize then nothing":  oversize,
	} {
		t.Run(name, func(t *testing.T) {
			root := newCacheRoot(t)
			h := startHolder(t, root, testToken)
			if got := h.line(t); got != readyLine(root, testToken) {
				t.Fatalf("ready = %q", got)
			}
			h.send(t, input)
			last, code := h.finish(t)
			if last != `{"event":"cache_cleanup","state":"retained","reason":"unattested"}` || code != 3 {
				t.Fatalf("cleanup = %q exit %d", last, code)
			}
			if _, err := os.Lstat(filepath.Join(root, testToken, "gomod")); err != nil {
				t.Fatalf("retained cache is gone: %v", err)
			}
		})
	}
}

// Ignored lines (oversize, garbage) do not undo a valid drained:true, and a
// final unterminated valid line counts.
func TestHoldCacheIgnoresBadLines(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	for name, input := range map[string]string{
		"garbage after true":   `{"op":"release","drained":true}` + "\nnot json\n",
		"oversize false after": `{"op":"release","drained":true}` + "\n" + `{"op":"release","drained":false}` + strings.Repeat(" ", 3*maxReleaseLine) + "\n",
		"unterminated true":    "junk\n" + `{"op":"release","drained":true}`,
		"line at the limit":    `{"op":"release","drained":true}` + strings.Repeat(" ", maxReleaseLine-len(`{"op":"release","drained":true}`)) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := newCacheRoot(t)
			h := startHolder(t, root, testToken)
			h.line(t)
			h.send(t, input)
			last, code := h.finish(t)
			if last != `{"event":"cache_cleanup","state":"removed","reason":""}` || code != 0 {
				t.Fatalf("cleanup = %q exit %d", last, code)
			}
			requireGone(t, filepath.Join(root, testToken))
		})
	}
}

func TestHoldCacheSetupErrors(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	uid := fmt.Sprint(os.Geteuid())
	run := func(root string, env func(modeEnv) modeEnv) (string, int) {
		var out bytes.Buffer
		e := testModeEnv(&out, t.TempDir(), nil)
		if env != nil {
			e = env(e)
		}
		code := runMode([]string{modeHoldCache, "--expect-uid", uid, "--cache-root", root, "--cache-token", testToken}, e)
		return out.String(), code
	}
	wantErr := func(name, reason string, got string, code int) {
		t.Helper()
		if want := `{"event":"cache_error","reason":"` + reason + `"}` + "\n"; got != want || code != 2 {
			t.Errorf("%s: %q exit %d, want %q exit 2", name, got, code, want)
		}
	}

	wrongMode := newCacheRoot(t)
	if err := os.Chmod(wrongMode, 0o755); err != nil {
		t.Fatal(err)
	}
	got, code := run(wrongMode, nil)
	wantErr("wrong mode", "root", got, code)

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(newCacheRoot(t), link); err != nil {
		t.Fatal(err)
	}
	got, code = run(link, nil)
	wantErr("symlinked root", "root", got, code)

	got, code = run(filepath.Join(t.TempDir(), "absent"), nil)
	wantErr("missing root", "root", got, code)

	// A root owned by another uid: openCacheRoot's owner check.
	if _, err := openCacheRoot(newCacheRoot(t), os.Geteuid()+1); !errors.Is(err, errCacheRoot) {
		t.Errorf("foreign-owned root = %v", err)
	}

	got, code = run(newCacheRoot(t), func(e modeEnv) modeEnv { e.uids = func() (int, int) { return 1, 1 }; return e })
	wantErr("uid", "uid", got, code)

	got, code = run(newCacheRoot(t), func(e modeEnv) modeEnv { e.hygiene = func() error { return errors.New("x") }; return e })
	wantErr("fd hygiene", "fd_hygiene", got, code)

	// The nondumpable step runs first: its failure creates nothing.
	dumpable := newCacheRoot(t)
	got, code = run(dumpable, func(e modeEnv) modeEnv {
		e.nondumpable = func() bool { return false }
		e.hygiene = func() error { t.Error("hygiene ran after a nondumpable failure"); return nil }
		return e
	})
	wantErr("dumpable", "dumpable", got, code)
	if _, err := os.Lstat(filepath.Join(dumpable, testToken)); !os.IsNotExist(err) {
		t.Errorf("nondumpable failure created the cache: %v", err)
	}

	existing := newCacheRoot(t)
	if err := os.Mkdir(filepath.Join(existing, testToken), 0o700); err != nil {
		t.Fatal(err)
	}
	got, code = run(existing, nil)
	wantErr("existing token", "create", got, code)

	lockFail := newCacheRoot(t)
	setFlock(t, func(int, int) error { return unix.ENOLCK })
	got, code = run(lockFail, nil)
	wantErr("lock", "lock", got, code)
	setFlock(t, unix.Flock)

	swapped := newCacheRoot(t)
	setAfterLockHook(t, func(parentFd int, name string) {
		p := filepath.Join(swapped, name)
		if err := os.Rename(p, p+".pinned"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	got, code = run(swapped, nil)
	wantErr("recheck", "recheck", got, code)
}

func TestRemoveCache(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	uid := fmt.Sprint(os.Geteuid())
	run := func(root string) (string, int) {
		var out bytes.Buffer
		code := runMode([]string{modeRemoveCache, "--expect-uid", uid, "--cache-root", root, "--cache-token", testToken}, testModeEnv(&out, t.TempDir(), nil))
		return strings.TrimSuffix(out.String(), "\n"), code
	}

	// Locked (live): retained.
	root := newCacheRoot(t)
	dir := filepath.Join(root, testToken)
	mkTree(t, dir)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	got, code := run(root)
	if got != `{"event":"cache_cleanup","state":"retained","reason":"live"}` || code != 3 {
		t.Fatalf("locked: %q exit %d", got, code)
	}
	requireExists(t, dir)

	// Unlocked: removed, with no proof needed.
	_ = unix.Close(fd)
	got, code = run(root)
	if got != `{"event":"cache_cleanup","state":"removed","reason":""}` || code != 0 {
		t.Fatalf("unlocked: %q exit %d", got, code)
	}
	requireGone(t, dir)

	// Missing: absent.
	got, code = run(root)
	if got != `{"event":"cache_cleanup","state":"absent","reason":""}` || code != 0 {
		t.Fatalf("missing: %q exit %d", got, code)
	}

	// A symlink at the token: retained mismatch, the target untouched.
	target := filepath.Join(t.TempDir(), "target")
	mkTree(t, target)
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	got, code = run(root)
	if got != `{"event":"cache_cleanup","state":"retained","reason":"mismatch"}` || code != 3 {
		t.Fatalf("symlink: %q exit %d", got, code)
	}
	requireExists(t, target)

	// A root that is not 0700: cache_error.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	got, code = run(root)
	if got != `{"event":"cache_error","reason":"root"}` || code != 2 {
		t.Fatalf("bad root: %q exit %d", got, code)
	}
}

// The holder, run as a real process and then released: the end-to-end CLI.
func TestHoldCacheProcess(t *testing.T) {
	if !requireNonRootCommandUID(t) {
		return
	}
	root := newCacheRoot(t)
	args := fmt.Sprintf(`[%q,"--expect-uid","%d","--cache-root",%q,"--cache-token",%q]`, modeHoldCache, os.Geteuid(), root, testToken)
	h, line := startHelper(t, testHelperRunMode, args)
	if line != readyLine(root, testToken) {
		t.Fatalf("ready = %q", line)
	}
	// The holder is nondumpable: this same-uid process cannot reopen its
	// stdin through the proc fd directory, while it can reopen a dumpable
	// same-uid process's (the control).
	control := spawnUser(t, t.TempDir())
	if f, err := os.Open(procFdPath(control.Process.Pid)); err != nil {
		t.Fatalf("control: a dumpable same-uid stdin: %v", err)
	} else {
		_ = f.Close()
	}
	if f, err := os.Open(procFdPath(h.cmd.Process.Pid)); err == nil {
		_ = f.Close()
		t.Fatal("the holder's stdin was reopened through the proc fd directory")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("reopen the holder's stdin: %v, want EACCES", err)
	}
	if _, err := io.WriteString(h.in, `{"op":"release","drained":true}`+"\n"); err != nil {
		t.Fatal(err)
	}
	_ = h.in.Close()
	last, err := h.out.ReadString('\n')
	if err != nil || last != `{"event":"cache_cleanup","state":"removed","reason":""}`+"\n" {
		t.Fatalf("cleanup = %q %v", last, err)
	}
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("holder exit: %v", err)
	}
	requireGone(t, filepath.Join(root, testToken))
}

// errReader returns its data, then err.
type errReader struct {
	data []byte
	err  error
}

func (e *errReader) Read(p []byte) (int, error) {
	if len(e.data) == 0 {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

// A stdin read error other than EOF retains, even after a drained:true; a
// clean EOF after the same line removes.
func TestReadReleaseRetainsOnReadError(t *testing.T) {
	line := `{"op":"release","drained":true}` + "\n"
	if !readRelease(&errReader{data: []byte(line), err: io.EOF}) {
		t.Fatal("drained:true then EOF was not attested")
	}
	if readRelease(&errReader{data: []byte(line), err: unix.EIO}) {
		t.Fatal("drained:true then a read error was attested")
	}
	if readRelease(&errReader{data: []byte(line + "junk"), err: unix.EIO}) {
		t.Fatal("a read error mid-line was attested")
	}

	// Through the holder: the stdin pipe fails.
	if !requireNonRootCommandUID(t) {
		return
	}
	root := newCacheRoot(t)
	h := startHolder(t, root, testToken)
	h.line(t)
	h.send(t, line)
	_ = h.in.CloseWithError(unix.EIO)
	last := h.line(t)
	if code := <-h.done; last != `{"event":"cache_cleanup","state":"retained","reason":"unattested"}` || code != 3 {
		t.Fatalf("cleanup = %q exit %d", last, code)
	}
	if _, err := os.Lstat(filepath.Join(root, testToken, "gomod")); err != nil {
		t.Fatalf("retained cache is gone: %v", err)
	}
}

func TestParseRelease(t *testing.T) {
	for line, want := range map[string]bool{
		`{"op":"release","drained":true}`:            true,
		`{"drained":true,"op":"release"}`:            true,
		` { "op" : "release" , "drained" : false } `: false,
		`{"op":"release","drained":false}` + "\n":    false,
	} {
		if got, ok := parseRelease([]byte(line)); !ok || got != want {
			t.Errorf("%q: %v %v, want %v true", line, got, ok, want)
		}
	}
	for _, line := range []string{
		`{"op":"release","drained":true,"drained":true}`,
		`{"op":"release","drained":false,"drained":true}`,
		`{"op":"release","op":"release","drained":true}`,
		`{"op":"release","drained":true,"op":"release"}`,
		`{"op":"release","drained":null}`,
		`{"op":null,"drained":true}`,
		`{"op":"release","drained":"true"}`,
		`{"op":"release","drained":1}`,
		`{"op":"release"}`,
		`{"drained":true}`,
		`{"op":"release","drained":true,"x":1}`,
		`{"op":"release","drained":true}{}`,
		`{"op":"release","drained":true} x`,
		`["op","release"]`,
		`{"op":"release","drained":true`,
		``,
	} {
		if _, ok := parseRelease([]byte(line)); ok {
			t.Errorf("%q accepted", line)
		}
	}
}

// procFdPath is pid's fd 0 in the proc fd directory.
func procFdPath(pid int) string {
	return filepath.Join(procRootPath, strconv.Itoa(pid), "fd", "0")
}
