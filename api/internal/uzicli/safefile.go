package uzicli

// safefile.go is the atomic, no-clobber, no-partial-file writer for `uzi run export`
// (PRD #1296 D7, revision item 3). It is a NEW helper, deliberately not config.go's
// writeFileAtomic: that one clobbers (os.Rename over an existing path) and lacks O_EXCL
// and symlink refusal, both of which are load-bearing here — a recovery bundle is
// secret-bearing quarantined data, so it must never overwrite an existing file or a
// symlink, and an interrupted or corrupt download must never leave a misleading complete
// file at the destination.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ArchiveStreamFunc streams a capture's bundle bytes into w and returns the number of
// bytes written. SafeWriteVerifiedFile calls it EXACTLY ONCE, having wrapped w so every
// byte lands in a private temp file and is hashed at the same time.
type ArchiveStreamFunc func(w io.Writer) (int64, error)

// SafeWriteVerifiedFile streams a download into dest atomically, refusing to clobber
// anything already there and never leaving a partial file behind (PRD #1296 D7).
//
// The exact sequence, and why each step is the way it is:
//
//  1. FAIL FAST on a pre-existing destination. os.Lstat (NOT Stat — it must not follow a
//     symlink) refuses when anything already occupies dest: a real file, or a symlink
//     (valid or dangling). This is a courtesy that avoids a wasted download; the atomic
//     guard is step 4.
//  2. Stream into a 0600 temp file created in dest's OWN directory with
//     O_CREATE|O_WRONLY|O_EXCL. Same directory means the same filesystem, so the final
//     hard-link in step 4 cannot fail cross-device; O_EXCL means the temp name is freshly
//     ours; 0600 means the secret-bearing bytes are never group/other-readable even for
//     the brief window they exist under the temp name.
//  3. VERIFY the streamed byte count AND the sha256 checksum against the server manifest
//     BEFORE the destination name is ever created. A mismatch (a truncated/interrupted or
//     corrupt download) aborts here, and dest never comes into existence.
//  4. PUBLISH by hard-linking the verified temp to dest with os.Link, then removing the
//     temp. os.Link FAILS with EEXIST if anything — file OR symlink — already exists at
//     dest, and it is ATOMIC: a file that appears at dest in the window between step 1 and
//     here is still refused, with no TOCTOU race. Crucially this is NOT os.Rename (which
//     clobbers) and NOT an O_EXCL-create of the final path followed by a copy (which would
//     expose a partial file on interruption). The destination name only ever appears
//     fully-formed and verified.
//
// On ANY error the temp file is removed and NO file is left at dest.
func SafeWriteVerifiedFile(dest string, expectedSize int64, expectedChecksum string, stream ArchiveStreamFunc) error {
	if strings.TrimSpace(expectedChecksum) == "" {
		// Refuse to publish bytes we cannot verify: an empty manifest checksum means the
		// capture is not really a ready, downloadable artifact. Better a clean refusal than
		// a silently unverified file.
		return Exitf(ExitGeneric, "cannot export: the capture manifest carries no checksum to verify against")
	}
	if expectedSize < 0 {
		return Exitf(ExitGeneric, "cannot export: the capture manifest carries no byte size to verify against")
	}

	// (1) Fail fast if the destination is already occupied by a file or a symlink.
	if _, err := os.Lstat(dest); err == nil {
		return Exitf(ExitGeneric, "refusing to overwrite existing path %q (a recovery bundle never clobbers a file or symlink); choose a different --output", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Exitf(ExitGeneric, "cannot stat destination %q: %v", dest, err)
	}

	dir := filepath.Dir(dest)

	// (2) Create the private temp file in dest's own directory.
	tmp, tmpName, err := createExclTemp(dir, ".uzi-export-")
	if err != nil {
		return err
	}
	// Always remove the temp name. On failure this leaves NO file at dest; on success it
	// removes the redundant SECOND hard link created in step 4, leaving only dest.
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	written, streamErr := stream(io.MultiWriter(tmp, hasher))
	// Close before verifying/linking so all bytes are flushed and the fd is released.
	if cerr := tmp.Close(); cerr != nil && streamErr == nil {
		return Exitf(ExitGeneric, "cannot finish writing the download: %v", cerr)
	}
	if streamErr != nil {
		// An interrupted/aborted download. The temp is removed by the defer; dest is absent.
		return streamErr
	}

	// (3) Verify size and checksum against the manifest BEFORE the destination exists.
	if written != expectedSize {
		return Exitf(ExitGeneric, "download size mismatch: got %d bytes, manifest declares %d — the download was truncated or corrupt, nothing was written to %q",
			written, expectedSize, dest)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, expectedChecksum) {
		return Exitf(ExitGeneric, "download checksum mismatch: got %s, manifest declares %s — the download was corrupt, nothing was written to %q",
			got, strings.ToLower(expectedChecksum), dest)
	}

	// (4) Publish atomically: link the verified temp onto the ABSENT destination.
	if err := os.Link(tmpName, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			// A file or symlink appeared at dest between step 1 and here. os.Link's EEXIST is
			// the atomic no-clobber guarantee — we refuse rather than overwrite.
			return Exitf(ExitGeneric, "refusing to overwrite path %q that appeared during the download; nothing was written there", dest)
		}
		return Exitf(ExitGeneric, "cannot publish the verified download to %q: %v", dest, err)
	}
	return nil
}

// createExclTemp creates a fresh 0600 file in dir with O_CREATE|O_WRONLY|O_EXCL and a
// random name, returning the open file and its path. A random name plus O_EXCL means two
// concurrent exports into the same directory cannot collide onto one temp; on the rare
// EEXIST it retries with fresh randomness.
func createExclTemp(dir, prefix string) (*os.File, string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		var b [9]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", Exitf(ExitGeneric, "cannot generate a temp file name: %v", err)
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(b[:])+".tmp")
		//nolint:gosec // G304: name is a random temp file this helper builds inside the operator's own --output directory; creating it is the command's purpose, not untrusted inclusion.
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue // extraordinarily unlikely with 72 random bits; retry with a new name.
		}
		return nil, "", Exitf(ExitGeneric, "cannot create a temp file in %q: %v", dir, err)
	}
	return nil, "", Exitf(ExitGeneric, "cannot create a unique temp file in %q after several attempts", dir)
}

// SafeSaveDownload is the `uzi job file get` counterpart of SafeWriteVerifiedFile: the same
// temp-file-then-os.Link sequence, so nothing ever appears at dest until the bytes are complete
// and verified, and an existing path (file or symlink, including one that appears mid-download)
// is refused, never replaced. Unlike the export helper the manifest values are optional:
// expectedSize < 0 skips the size check and an empty expectedChecksum skips the digest check
// (a download whose name carries no sha256 and whose response had no Content-Length). It
// returns the byte count and the sha256 of what was streamed. The temp file is named
// `.uzi-download-<random>.tmp` in dest's directory; an interrupt that kills the process before
// the deferred removal leaves only that clearly named file, never anything at dest, and a later
// run is unaffected by it.
//
// Errors: an occupied dest is ExitUsage; a stream failure or size shortfall is
// ExitUnreachable; a digest mismatch is ExitGeneric.
func SafeSaveDownload(dest string, expectedSize int64, expectedChecksum string, stream ArchiveStreamFunc) (int64, string, error) {
	if _, err := os.Lstat(dest); err == nil {
		return 0, "", Exitf(ExitUsage, "%q already exists: refusing to overwrite it (choose another -o path or remove it)", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, "", Exitf(ExitGeneric, "cannot stat %q: %v", dest, err)
	}
	tmp, tmpName, err := createExclTemp(filepath.Dir(dest), ".uzi-download-")
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	written, streamErr := stream(io.MultiWriter(tmp, hasher))
	if cerr := tmp.Close(); cerr != nil && streamErr == nil {
		streamErr = cerr
	}
	if streamErr != nil {
		return 0, "", Exitf(ExitUnreachable, "download failed: %v", streamErr)
	}
	if expectedSize >= 0 && written != expectedSize {
		return 0, "", Exitf(ExitUnreachable, "download incomplete: got %d of %d bytes", written, expectedSize)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if expectedChecksum != "" && !strings.EqualFold(sum, expectedChecksum) {
		return 0, "", Exitf(ExitGeneric, "downloaded bytes do not match the file's sha256 (%s, expected %s): not saved", sum, strings.ToLower(expectedChecksum))
	}
	if err := os.Link(tmpName, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, "", Exitf(ExitUsage, "%q appeared during the download: refusing to overwrite it; nothing was written there", dest)
		}
		return 0, "", Exitf(ExitGeneric, "cannot move the download into place: %v", err)
	}
	return written, sum, nil
}
