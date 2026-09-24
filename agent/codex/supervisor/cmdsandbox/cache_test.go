package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Run cache tokens (lowercase uuids).
const (
	runTokenA = "0a1b2c3d-0000-4000-8000-00000000000a"
	runTokenB = "0a1b2c3d-0000-4000-8000-00000000000b"
)

func TestParseArgsCache(t *testing.T) {
	cacheA := "/var/cache/uzi-codex-cmd/" + runTokenA
	base := []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }

	// (present) --cache before -- is parsed.
	_, _, _, cache, mode, child, err := parseArgs(with("--cache", cacheA, "--", "/bin/true"))
	if err != nil || cache != cacheA || mode != modeRequired || !reflect.DeepEqual(child, []string{"/bin/true"}) {
		t.Fatalf("--cache: cache=%q mode=%q child=%v err=%v", cache, mode, child, err)
	}

	// (absent) cache is "".
	if _, _, _, cache, _, _, err := parseArgs(with("--", "/bin/true")); err != nil || cache != "" {
		t.Fatalf("absent --cache: cache=%q err=%v", cache, err)
	}

	// (combined) --cache then --mode, both honoured.
	_, _, _, cache, mode, child, err = parseArgs(with("--cache", "/c/"+runTokenB, "--mode", "best-effort", "--", "/bin/true", "x"))
	if err != nil || cache != "/c/"+runTokenB || mode != modeBestEffort || !reflect.DeepEqual(child, []string{"/bin/true", "x"}) {
		t.Fatalf("--cache --mode: cache=%q mode=%q child=%v err=%v", cache, mode, child, err)
	}

	// (after --) a --cache token belongs to the child and is never the cache.
	_, _, _, cache, _, child, err = parseArgs(with("--", "/bin/sh", "--cache", "/etc"))
	if err != nil || cache != "" || !reflect.DeepEqual(child, []string{"/bin/sh", "--cache", "/etc"}) {
		t.Fatalf("--cache after --: cache=%q child=%v err=%v", cache, child, err)
	}
	_, _, _, cache, mode, child, err = parseArgs(with("--mode", "required", "--", "/bin/sh", "--cache", "/etc", "--mode", "best-effort"))
	if err != nil || cache != "" || mode != modeRequired || len(child) != 5 {
		t.Fatalf("flags after --: cache=%q mode=%q child=%v err=%v", cache, mode, child, err)
	}

	for name, args := range map[string][]string{
		"mode before cache": with("--mode", "required", "--cache", cacheA, "--", "/bin/true"),
		"relative cache":    with("--cache", "c/"+runTokenA, "--", "/bin/true"),
		"root cache":        with("--cache", "/", "--", "/bin/true"),
		"missing value":     with("--cache", "--", "/bin/true"),
		"repeated cache":    with("--cache", cacheA, "--cache", "/c/"+runTokenB, "--", "/bin/true"),
		// Must be clean and must be a run directory, never the cache root.
		"the cache root":      with("--cache", "/var/cache/uzi-codex-cmd", "--", "/bin/true"),
		"not a uuid":          with("--cache", "/c/run", "--", "/bin/true"),
		"upper-case uuid":     with("--cache", "/c/"+strings.ToUpper(runTokenA), "--", "/bin/true"),
		"uuid without dashes": with("--cache", "/c/"+strings.ReplaceAll(runTokenA, "-", "")+"xxxx", "--", "/bin/true"),
		"uuid plus suffix":    with("--cache", cacheA+"x", "--", "/bin/true"),
		"trailing slash":      with("--cache", cacheA+"/", "--", "/bin/true"),
		"dot-dot":             with("--cache", cacheA+"/..", "--", "/bin/true"),
		"dot-dot then uuid":   with("--cache", "/var/cache/../etc/"+runTokenA, "--", "/bin/true"),
		"dot element":         with("--cache", "/var/cache/./"+runTokenA, "--", "/bin/true"),
		"double slash":        with("--cache", "/var/cache//"+runTokenA, "--", "/bin/true"),
		"bare uuid":           with("--cache", runTokenA, "--", "/bin/true"),
	} {
		if _, _, _, _, _, _, err := parseArgs(args); err == nil {
			t.Errorf("%s: %v accepted", name, args)
		}
	}
}

// privateCache makes a real, non-empty 0700 cache directory.
func privateCache(t *testing.T) string {
	t.Helper()
	dir := privateTmp(t)
	if err := os.WriteFile(filepath.Join(dir, "gomod-entry"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "npm"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAdoptCacheAcceptsNonEmptyOwned0700Dir(t *testing.T) {
	dir := privateCache(t)
	fd, err := adoptCache(dir, os.Getuid(), unix.Open, unix.Fstat)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	flags, ferr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if ferr != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("adopted cache fd is not close-on-exec (flags=%d err=%v)", flags, ferr)
	}
	// The same check refuses it as a tmp: the difference is emptiness only.
	if _, err := adoptPrivateTmp(dir, os.Getuid(), unix.Open, unix.Fstat); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("adoptPrivateTmp of a non-empty dir = %v", err)
	}
}

func TestAdoptCacheRefusals(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(privateCache(t), link); err != nil {
			t.Fatal(err)
		}
		if _, err := adoptCache(link, os.Getuid(), unix.Open, unix.Fstat); err == nil {
			t.Fatal("adopted a symlinked cache")
		}
	})
	t.Run("owner", func(t *testing.T) {
		dir := privateCache(t)
		foreign := func(fd int, st *unix.Stat_t) error {
			if err := unix.Fstat(fd, st); err != nil {
				return err
			}
			st.Uid++
			return nil
		}
		if _, err := adoptCache(dir, os.Getuid(), unix.Open, foreign); err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("foreign owner = %v", err)
		}
		if _, err := adoptCache(dir, os.Getuid()+1, unix.Open, unix.Fstat); err == nil {
			t.Fatal("adopted a cache owned by another uid")
		}
	})
	t.Run("mode", func(t *testing.T) {
		dir := privateCache(t)
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := adoptCache(dir, os.Getuid(), unix.Open, unix.Fstat); err == nil || !strings.Contains(err.Error(), "mode 0750") {
			t.Fatalf("0750 cache = %v", err)
		}
	})
	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(file, nil, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := adoptCache(file, os.Getuid(), unix.Open, unix.Fstat); err == nil {
			t.Fatal("adopted a regular file")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := adoptCache(filepath.Join(t.TempDir(), "absent"), os.Getuid(), unix.Open, unix.Fstat); err == nil {
			t.Fatal("adopted a missing cache: the sandbox must never create it")
		}
	})
}

// TestAddRulesGrantsCacheThroughTheAdoptedFd: the cache rule goes through the
// adopted fd with the tmp's full rights, after the tmp rule, and no path rule
// names the cache, its parent (the cache root) or a path swapped in after
// adoption.
func TestAddRulesGrantsCacheThroughTheAdoptedFd(t *testing.T) {
	tmpFd, err := adoptPrivateTmp(privateTmp(t), os.Getuid(), unix.Open, unix.Fstat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(tmpFd) }()
	cache := privateCache(t)
	cacheFd, err := adoptCache(cache, os.Getuid(), unix.Open, unix.Fstat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(cacheFd) }()
	var adopted unix.Stat_t
	if err := unix.Fstat(cacheFd, &adopted); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cache, cache+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}

	const ruleset, handled = 7, uint64(0xabc)
	root := t.TempDir()
	var paths []string
	type fdRule struct {
		fd     int
		access uint64
		ino    uint64
	}
	var rules []fdRule
	err = addRules(ruleset, root, grantFds{tmp: tmpFd, cache: cacheFd}, handled, ruleAdders{
		path: func(_ int, p string, _ uint64) error { paths = append(paths, p); return nil },
		fd: func(_ int, fd int, access uint64) error {
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				t.Errorf("fstat rule fd %d: %v", fd, err)
			}
			rules = append(rules, fdRule{fd: fd, access: access, ino: st.Ino})
			return nil
		},
	})
	if err != nil {
		t.Fatalf("addRules: %v", err)
	}
	if len(rules) != 2 || rules[0].fd != tmpFd || rules[1].fd != cacheFd || rules[1].access != handled || rules[1].ino != adopted.Ino {
		t.Fatalf("fd rules = %+v, want the tmp then the adopted cache fd %d (ino %d) with %#x", rules, cacheFd, adopted.Ino, handled)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, filepath.Dir(cache)) {
			t.Fatalf("a path rule named the cache or its root (%q)", p)
		}
	}

	// Without --cache, only the tmp rule is added.
	rules = nil
	if err := addRules(ruleset, root, grantFds{tmp: tmpFd, cache: -1}, handled, ruleAdders{
		path: func(int, string, uint64) error { return nil },
		fd: func(_ int, fd int, access uint64) error {
			rules = append(rules, fdRule{fd: fd, access: access})
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].fd != tmpFd {
		t.Fatalf("fd rules without a cache = %+v", rules)
	}

	// A failing cache rule fails addRules (confine then fails closed).
	boom := errors.New("boom")
	if err := addRules(ruleset, root, grantFds{tmp: tmpFd, cache: cacheFd}, handled, ruleAdders{
		path: func(int, string, uint64) error { return nil },
		fd: func(_ int, fd int, _ uint64) error {
			if fd == cacheFd {
				return boom
			}
			return nil
		},
	}); !errors.Is(err, boom) {
		t.Fatalf("cache rule failure = %v", err)
	}
}

// landlockHelperEnv carries the JSON argv for the re-exec'd sandbox helper.
const landlockHelperEnv = "CMDSANDBOX_TEST_LANDLOCK_ARGS"

// TestLandlockCacheIsolation runs the real sandbox (required mode) in a
// re-exec'd copy of this test binary with --cache A: the child can write in
// A, cannot open a sibling B under the same parent, and cannot list the
// parent. It skips where Landlock (or the baked deny probe) is unavailable.
func TestLandlockCacheIsolation(t *testing.T) {
	if raw := os.Getenv(landlockHelperEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(90)
		}
		os.Exit(runOnLockedThread(args, runtime.LockOSThread, realMain))
	}
	if avail, _, _ := classifyLandlock(realVersionProbe); avail != landlockAvailable {
		t.Skip("Landlock is not available on this kernel")
	}
	if f, err := os.Open(landlockDenyProbe); err != nil {
		t.Skipf("the deny probe %s is not readable here: %v", landlockDenyProbe, err)
	} else {
		_ = f.Close()
	}

	parent := t.TempDir()
	a, b := filepath.Join(parent, runTokenA), filepath.Join(parent, runTokenB)
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tmp := privateTmp(t)
	script := `echo ok > "$A/written" || exit 10
if ls "$B" >/dev/null 2>&1; then exit 11; fi
if cat "$B/secret" >/dev/null 2>&1; then exit 12; fi
if ls "$P" >/dev/null 2>&1; then exit 13; fi
exit 0`
	args, err := json.Marshal([]string{"--root", root, "--tmp", tmp, "--cwd", root, "--cache", a, "--mode", "required", "--", "/bin/sh", "-c", script})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLandlockCacheIsolation$")
	cmd.Env = append(os.Environ(), landlockHelperEnv+"="+string(args), "A="+a, "B="+b, "P="+parent)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed child failed: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(a, "written")); err != nil || string(got) != "ok\n" {
		t.Fatalf("write in the cache: %q %v", got, err)
	}
}
