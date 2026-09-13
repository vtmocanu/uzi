package uzicli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// sha256Hex (package-level, skill.go) is the lowercase-hex sha256 the manifest declares;
// these tests verify against that same value.

// streamBytes returns an ArchiveStreamFunc that writes b verbatim and reports the count.
func streamBytes(b []byte) ArchiveStreamFunc {
	return func(w io.Writer) (int64, error) {
		n, err := w.Write(b)
		return int64(n), err
	}
}

// TestSafeWriteVerifiedFileHappyPath: the verified bytes land at dest, 0600, exact content.
func TestSafeWriteVerifiedFileHappyPath(t *testing.T) {
	data := []byte("a git bundle, \x00\x01\xfe binary and all")
	dest := filepath.Join(t.TempDir(), "out.bundle")
	if err := SafeWriteVerifiedFile(dest, int64(len(data)), sha256Hex(data), streamBytes(data)); err != nil {
		t.Fatalf("SafeWriteVerifiedFile: %v", err)
	}
	if got := readBack(t, dest); got != string(data) {
		t.Errorf("content = %q, want %q", got, data)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
	// No temp files are left behind in the directory.
	assertNoTempLeft(t, filepath.Dir(dest))
}

// TestSafeWriteVerifiedFileRefusesExistingFile: an existing regular file is never overwritten,
// and the stream is never invoked.
func TestSafeWriteVerifiedFileRefusesExistingFile(t *testing.T) {
	data := []byte("new")
	dest := filepath.Join(t.TempDir(), "out.bundle")
	if err := os.WriteFile(dest, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}
	streamed := false
	err := SafeWriteVerifiedFile(dest, int64(len(data)), sha256Hex(data), func(w io.Writer) (int64, error) {
		streamed = true
		return streamBytes(data)(w)
	})
	if err == nil {
		t.Fatal("expected a refusal on an existing file")
	}
	if streamed {
		t.Error("the stream must NOT run when the destination already exists")
	}
	if got := readBack(t, dest); got != "OLD" {
		t.Errorf("existing file clobbered: %q", got)
	}
	assertNoTempLeft(t, filepath.Dir(dest))
}

// TestSafeWriteVerifiedFileRefusesSymlink: a symlink at dest is refused and its target is not
// written through.
func TestSafeWriteVerifiedFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("KEEP"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "link")
	if err := os.Symlink(target, dest); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	data := []byte("new")
	err := SafeWriteVerifiedFile(dest, int64(len(data)), sha256Hex(data), streamBytes(data))
	if err == nil {
		t.Fatal("expected a refusal on a symlink destination")
	}
	if got := readBack(t, target); got != "KEEP" {
		t.Errorf("symlink target written through: %q", got)
	}
	if fi, _ := os.Lstat(dest); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dest no longer the original symlink")
	}
}

// TestSafeWriteVerifiedFileChecksumMismatch: a corrupt stream (right length, wrong bytes)
// verifies as a mismatch, exits nonzero, and leaves NO file at dest.
func TestSafeWriteVerifiedFileChecksumMismatch(t *testing.T) {
	want := []byte("expected-content")
	corrupt := []byte("CORRUPT!content!") // same length, different bytes
	if len(want) != len(corrupt) {
		t.Fatalf("test setup: lengths differ (%d vs %d)", len(want), len(corrupt))
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	err := SafeWriteVerifiedFile(dest, int64(len(want)), sha256Hex(want), streamBytes(corrupt))
	if err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a checksum mismatch must leave NO file; stat = %v", statErr)
	}
	assertNoTempLeft(t, filepath.Dir(dest))
}

// TestSafeWriteVerifiedFileSizeMismatch: a short/truncated stream is a size mismatch, nonzero,
// no file.
func TestSafeWriteVerifiedFileSizeMismatch(t *testing.T) {
	full := []byte("the-full-bundle-content")
	dest := filepath.Join(t.TempDir(), "out.bundle")
	// Manifest declares the full size, but the stream delivers only a prefix.
	err := SafeWriteVerifiedFile(dest, int64(len(full)), sha256Hex(full), streamBytes(full[:5]))
	if err == nil {
		t.Fatal("expected a size mismatch error")
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a truncated download must leave NO file; stat = %v", statErr)
	}
	assertNoTempLeft(t, filepath.Dir(dest))
}

// TestSafeWriteVerifiedFileStreamError: a mid-stream transport error (some bytes written, then
// an error) exits nonzero, propagates the stream's error, and leaves NO file.
func TestSafeWriteVerifiedFileStreamError(t *testing.T) {
	data := []byte("partial-then-boom")
	dest := filepath.Join(t.TempDir(), "out.bundle")
	sentinel := Exitf(ExitUnreachable, "connection reset")
	err := SafeWriteVerifiedFile(dest, int64(len(data)), sha256Hex(data), func(w io.Writer) (int64, error) {
		n, _ := w.Write(data[:6]) // partial write into the temp file
		return int64(n), sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the stream's own error to propagate, got %v", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("an interrupted download must leave NO file; stat = %v", statErr)
	}
	assertNoTempLeft(t, filepath.Dir(dest))
}

// TestSafeWriteVerifiedFileEmptyChecksumRefused: an empty manifest checksum is refused rather
// than silently publishing unverified bytes.
func TestSafeWriteVerifiedFileEmptyChecksumRefused(t *testing.T) {
	data := []byte("bytes")
	dest := filepath.Join(t.TempDir(), "out.bundle")
	streamed := false
	err := SafeWriteVerifiedFile(dest, int64(len(data)), "  ", func(w io.Writer) (int64, error) {
		streamed = true
		return streamBytes(data)(w)
	})
	if err == nil {
		t.Fatal("expected a refusal when the manifest carries no checksum")
	}
	if streamed {
		t.Error("must refuse before streaming when there is nothing to verify against")
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("no file must be written; stat = %v", statErr)
	}
}

// TestSafeWriteVerifiedFileRaceAtPublish proves the atomic-publish guard, not just the
// fail-fast preflight: the destination is ABSENT when the call starts (preflight passes),
// but a racing writer creates it WHILE the download streams. os.Link's EEXIST is then the
// atomic no-clobber guarantee — the racing file is preserved, not overwritten, and no temp
// is left behind.
func TestSafeWriteVerifiedFileRaceAtPublish(t *testing.T) {
	data := []byte("verified-download-content")
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bundle")
	err := SafeWriteVerifiedFile(dest, int64(len(data)), sha256Hex(data), func(w io.Writer) (int64, error) {
		// A concurrent writer wins the destination name mid-download (the post-preflight
		// window). The bytes we stream still verify; publish must then refuse.
		if werr := os.WriteFile(dest, []byte("RACER-WON"), 0o600); werr != nil {
			t.Fatalf("seeding the race: %v", werr)
		}
		return streamBytes(data)(w)
	})
	if err == nil {
		t.Fatal("expected a publish refusal when the destination appears mid-download")
	}
	if got := readBack(t, dest); got != "RACER-WON" {
		t.Errorf("the racing file was clobbered: now %q", got)
	}
	assertNoTempLeft(t, dir)
}

// assertNoTempLeft fails if any of our .uzi-export-*.tmp scratch files remain in dir.
func assertNoTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if name := e.Name(); len(name) >= len(".uzi-export-") && name[:len(".uzi-export-")] == ".uzi-export-" {
			t.Errorf("temp file left behind: %s", filepath.Join(dir, name))
		}
	}
}

// TestSafeWriteVerifiedFileUppercaseChecksum: the checksum comparison is case-insensitive, so
// a manifest that declares an UPPERCASE digest still verifies (matching the server's EqualFold).
func TestSafeWriteVerifiedFileUppercaseChecksum(t *testing.T) {
	data := []byte("case-insensitive-digest")
	dest := filepath.Join(t.TempDir(), "out.bundle")
	upper := fmt.Sprintf("%X", sha256.Sum256(data))
	if err := SafeWriteVerifiedFile(dest, int64(len(data)), upper, streamBytes(data)); err != nil {
		t.Fatalf("uppercase checksum should verify: %v", err)
	}
	if got := readBack(t, dest); got != string(data) {
		t.Errorf("content = %q", got)
	}
}

// readBack reads a file the test itself wrote under t.TempDir(); the path is test-controlled,
// not untrusted input.
func readBack(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: test reads a temp file it created under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
