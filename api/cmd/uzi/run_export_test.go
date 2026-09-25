package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// bundleChecksum is the lowercase hex sha256 the server manifest declares, recomputed here
// so the tests verify against the SAME value the CLI checks.
func bundleChecksum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func i64(n int64) *int64 { return &n }

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

// availableCapture builds one downloadable capture DTO whose manifest matches data.
func availableCapture(id string, data []byte) apitypes.RecoveryArchiveDTO {
	return apitypes.RecoveryArchiveDTO{
		ID: id, State: "available", SourceSha: "deadbeefcafe0000",
		ByteSize: i64(int64(len(data))), Checksum: bundleChecksum(data),
	}
}

// TestRunExportHappyPath: a single available capture exports the verified bytes to --output.
func TestRunExportHappyPath(t *testing.T) {
	data := []byte("PACK\x00\x01a real-ish git bundle payload\x00\xff")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		RecoveryBytes: map[string][]byte{"cap-1": data},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := readBack(t, dest); got != string(data) {
		t.Errorf("exported bytes = %q, want %q", got, data)
	}
	// The 0600 permission on the published file (the temp is 0600 and hard-linked).
	if fi, _ := os.Lstat(dest); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("exported file perm = %o, want 600", fi.Mode().Perm())
	}
	if !strings.Contains(stdout, "cap-1") || !strings.Contains(stdout, dest) {
		t.Errorf("confirmation missing capture id / path:\n%s", stdout)
	}
	// The secret-review warning is on STDERR (D6/D7), on every download surface.
	if !strings.Contains(strings.ToLower(stderr), "secret") {
		t.Errorf("expected a secret-review warning on stderr:\n%s", stderr)
	}
}

// TestRunExportJSONMetadataOnly: --json prints the metadata result and NEVER the raw bytes.
func TestRunExportJSONMetadataOnly(t *testing.T) {
	data := []byte("SECRETLY-BINARY-BUNDLE-BYTES-\x00\x01\x02")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		RecoveryBytes: map[string][]byte{"cap-1": data},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest, "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// The file is still written with the real bytes.
	if got := readBack(t, dest); got != string(data) {
		t.Errorf("exported bytes = %q, want %q", got, data)
	}
	// STDOUT is metadata only — it must carry the result fields and NONE of the payload.
	for _, want := range []string{`"capture_id": "cap-1"`, `"verified": true`, `"output"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--json stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "SECRETLY-BINARY-BUNDLE-BYTES") {
		t.Errorf("--json stdout leaked raw bundle bytes:\n%s", stdout)
	}
}

// TestRunExportMultipleAvailableRequiresCapture: >1 available capture without --capture is a
// USAGE error that lists the ids and attempts NO download and writes NO file.
func TestRunExportMultipleAvailableRequiresCapture(t *testing.T) {
	a := []byte("bundle-A")
	b := []byte("bundle-BB")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				availableCapture("cap-A", a), availableCapture("cap-B", b),
			}},
		},
		RecoveryBytes: map[string][]byte{"cap-A": a, "cap-B": b},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	for _, want := range []string{"cap-A", "cap-B", "--capture"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("ambiguity error missing %q:\n%s", want, stderr)
		}
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("no download must be attempted on an ambiguous selection; got calls %v", fc.RecoveryDownloadCalls)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no file must be written on an ambiguous selection; Stat err = %v", err)
	}
}

// longCaptureID builds a full 36-char UUID-shaped capture id at runtime (issue #1417): real
// capture ids are server UUIDs, and it is their full length that overflowed the one-line
// error. Built from numbers, never a UUID literal in source.
func longCaptureID(n uint64) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", 0x1a2b0000+n, 0x5e6f, 0x4a1b, 0x8c2d, 0x3e4f5a6b0000+n)
}

// longRunID is the run id for the #1417 fixtures: a real run id is a server UUID too, and
// its length is what pushes the second capture id past the error line's 200-char fold.
var longRunID = longCaptureID(0xabc)

// assertFullIDsListed checks both full ids reach stderr, and that the pre-#1417 one-line
// error (oldPreamble + one "\n  <captureListLine>" per capture) exceeded the 200-char fold
// before the end of the second id, so the check is meaningful.
func assertFullIDsListed(t *testing.T, stderr, oldPreamble string, archives []apitypes.RecoveryArchiveDTO) {
	t.Helper()
	old := oldPreamble
	for _, a := range archives {
		old += "\n  " + captureListLine(a)
	}
	if len(archives) != 2 || len(archives[1].ID) != 36 || strings.Index(old, archives[1].ID)+36 <= 200 {
		t.Fatalf("fixture does not exercise the 200-char error fold (old message %d chars)", len(old))
	}
	for _, a := range archives {
		if !strings.Contains(stderr, a.ID) {
			t.Errorf("stderr missing full capture id %q:\n%s", a.ID, stderr)
		}
	}
}

// TestRunExportMultipleAvailableListsFullIDs (issue #1417): with two available captures
// whose ids are full UUIDs, the listing goes to stderr as a table, not inside the one-line
// error the root folds and caps at 200 chars, so the SECOND id is printed whole.
func TestRunExportMultipleAvailableListsFullIDs(t *testing.T) {
	idA, idB := longCaptureID(1), longCaptureID(2)
	a, b := []byte("bundle-A"), []byte("bundle-BB")
	archives := []apitypes.RecoveryArchiveDTO{availableCapture(idA, a), availableCapture(idB, b)}
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			longRunID: {Supported: true, Archives: archives},
		},
		RecoveryBytes: map[string][]byte{idA: a, idB: b},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", longRunID, "--output", dest)
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage) (stderr: %s)", code, uzicli.ExitUsage, stderr)
	}
	assertFullIDsListed(t, stderr,
		"run "+longRunID+" has 2 recovery archives available; choose one with --capture <id>:", archives)
	if !strings.Contains(stderr, "(captures listed above)") {
		t.Errorf("short error should point at the listing:\n%s", stderr)
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("no download on an ambiguous selection; got %v", fc.RecoveryDownloadCalls)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no file on an ambiguous selection; Stat err = %v", err)
	}
}

// TestRunExportMultipleAvailableJSON: under --json the capture listing is emitted to stdout
// as a JSON array of the capture DTOs, and the exit is still the usage error.
func TestRunExportMultipleAvailableJSON(t *testing.T) {
	idA, idB := longCaptureID(1), longCaptureID(2)
	a, b := []byte("bundle-A"), []byte("bundle-BB")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			longRunID: {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				availableCapture(idA, a), availableCapture(idB, b),
			}},
		},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", longRunID, "--output", dest, "--json")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage) (stderr: %s)", code, uzicli.ExitUsage, stderr)
	}
	var got []apitypes.RecoveryArchiveDTO
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json stdout is not a capture array: %v\n%s", err, stdout)
	}
	if len(got) != 2 || got[0].ID != idA || got[1].ID != idB {
		t.Errorf("--json captures = %+v, want ids %s, %s", got, idA, idB)
	}
	if !strings.Contains(stderr, "--capture") || !strings.Contains(stderr, "(captures emitted as JSON)") {
		t.Errorf("stderr error should name --capture and point at the JSON listing:\n%s", stderr)
	}
}

// TestRunExportNoneAvailableListsFullIDs: captures exist but none is downloadable — exit 5
// (conflict), and every capture's full id reaches stderr with its honest state.
func TestRunExportNoneAvailableListsFullIDs(t *testing.T) {
	idA, idB := longCaptureID(1), longCaptureID(2)
	archives := []apitypes.RecoveryArchiveDTO{
		{ID: idA, State: "needs_action", SourceSha: "deadbeefcafe0000", ByteSize: i64(8)},
		{ID: idB, State: "expired", SourceSha: "deadbeefcafe0000", ByteSize: i64(8)},
	}
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			longRunID: {Supported: true, Archives: archives},
		},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", longRunID, "--output", dest)
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict) (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	assertFullIDsListed(t, stderr,
		"run "+longRunID+" has no downloadable recovery archive; its captures are:", archives)
	for _, st := range []string{"needs_action", "expired"} {
		if !strings.Contains(stderr, st) {
			t.Errorf("stderr missing state %q:\n%s", st, stderr)
		}
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("no download when nothing is available; got %v", fc.RecoveryDownloadCalls)
	}
}

// TestRunExportCaptureLongID: the full id copied from the listing selects that capture and
// exports its verified bytes.
func TestRunExportCaptureLongID(t *testing.T) {
	idA, idB := longCaptureID(1), longCaptureID(2)
	a, b := []byte("bundle-A"), []byte("bundle-BB")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			longRunID: {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				availableCapture(idA, a), availableCapture(idB, b),
			}},
		},
		RecoveryBytes: map[string][]byte{idA: a, idB: b},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", longRunID, "--output", dest, "--capture", idB)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := readBack(t, dest); got != string(b) {
		t.Errorf("exported bytes = %q, want %q", got, b)
	}
	if len(fc.RecoveryDownloadCalls) != 1 || fc.RecoveryDownloadCalls[0] != idB {
		t.Errorf("expected exactly one download of %s; got %v", idB, fc.RecoveryDownloadCalls)
	}
}

// TestRunExportPicksSoleAvailable: with more than one capture but only ONE available, export
// selects the available one (the others discarded/needs_action) — not a silent pick of a
// different attempt, the only downloadable one.
func TestRunExportPicksSoleAvailable(t *testing.T) {
	data := []byte("the-one-available")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				{ID: "cap-old", State: "discarded"},
				availableCapture("cap-live", data),
			}},
		},
		RecoveryBytes: map[string][]byte{"cap-live": data},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := readBack(t, dest); got != string(data) {
		t.Errorf("exported bytes = %q, want %q", got, data)
	}
	if len(fc.RecoveryDownloadCalls) != 1 || fc.RecoveryDownloadCalls[0] != "cap-live" {
		t.Errorf("expected exactly one download of cap-live; got %v", fc.RecoveryDownloadCalls)
	}
}

// TestRunExportRefusesExistingFile: a pre-existing regular file at --output is NOT clobbered,
// and no download is attempted (the refusal is before the stream).
func TestRunExportRefusesExistingFile(t *testing.T) {
	data := []byte("fresh-bundle")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		RecoveryBytes: map[string][]byte{"cap-1": data},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	if err := os.WriteFile(dest, []byte("PRE-EXISTING"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code == uzicli.ExitOK {
		t.Fatalf("exit = 0, want nonzero (stderr: %s)", stderr)
	}
	if got := readBack(t, dest); got != "PRE-EXISTING" {
		t.Errorf("existing file was clobbered: now %q", got)
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("no download must be attempted when the destination exists; got %v", fc.RecoveryDownloadCalls)
	}
}

// TestRunExportRefusesSymlink: a symlink at --output is refused and its target is NOT written
// through — the classic clobber-via-symlink attack.
func TestRunExportRefusesSymlink(t *testing.T) {
	data := []byte("fresh-bundle")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		RecoveryBytes: map[string][]byte{"cap-1": data},
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(target, []byte("DO-NOT-TOUCH"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "link.bundle")
	if err := os.Symlink(target, dest); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code == uzicli.ExitOK {
		t.Fatalf("exit = 0, want nonzero (stderr: %s)", stderr)
	}
	// The symlink target must be untouched...
	if got := readBack(t, target); got != "DO-NOT-TOUCH" {
		t.Errorf("symlink target was written through: now %q", got)
	}
	// ...and dest must still be the symlink, not replaced by a regular file.
	if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dest is no longer the original symlink (mode=%v err=%v)", fi.Mode(), err)
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("no download must be attempted when the destination is a symlink; got %v", fc.RecoveryDownloadCalls)
	}
}

// TestRunExportInterruptedLeavesNoFile: a mid-stream download failure exits nonzero and leaves
// NO file at the destination (no partial, misleading output).
func TestRunExportInterruptedLeavesNoFile(t *testing.T) {
	data := []byte("this-would-be-64MiB-in-production")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		// Write SOME bytes, then fail — the interrupted-download shape.
		RecoveryDownloadHook: func(_, _ string, w io.Writer) (int64, error) {
			n, _ := w.Write(data[:8])
			return int64(n), uzicli.Exitf(uzicli.ExitUnreachable, "download interrupted after %d bytes: connection reset", n)
		},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code != uzicli.ExitUnreachable {
		t.Fatalf("exit = %d, want %d (unreachable) (stderr: %s)", code, uzicli.ExitUnreachable, stderr)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an interrupted download must leave NO file at dest; Stat err = %v", err)
	}
}

// TestRunExportCorruptLeavesNoFile: a full-length stream whose bytes do not match the manifest
// checksum is a corrupt download — nonzero exit, no file.
func TestRunExportCorruptLeavesNoFile(t *testing.T) {
	data := []byte("expected-bundle-contents")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			// The manifest declares the size/checksum of `data`...
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		// ...but the server streams DIFFERENT bytes of the same length (corruption).
		RecoveryBytes: map[string][]byte{"cap-1": []byte("CORRUPTED-bundle-content")},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code == uzicli.ExitOK {
		t.Fatalf("exit = 0, want nonzero on a checksum mismatch (stderr: %s)", stderr)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a corrupt download must leave NO file at dest; Stat err = %v", err)
	}
	if !strings.Contains(strings.ToLower(stderr), "checksum") {
		t.Errorf("expected a checksum-mismatch message:\n%s", stderr)
	}
}

// TestRunExportCaptureNotAvailable: --capture naming a non-available capture is refused with
// its honest state and no download.
func TestRunExportCaptureNotAvailable(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				{ID: "cap-pending", State: "needs_action", Reason: "bundle exceeds the maximum size"},
			}},
		},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest, "--capture", "cap-pending")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict) (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	if !strings.Contains(stderr, "needs_action") {
		t.Errorf("refusal should name the honest state:\n%s", stderr)
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("no download for a non-available capture; got %v", fc.RecoveryDownloadCalls)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no file for a non-available capture; Stat err = %v", err)
	}
}

// TestRunExportUnknownCapture: --capture with an id absent from the run is a 404-shaped error.
func TestRunExportUnknownCapture(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", []byte("x"))}},
		},
		RecoveryBytes: map[string][]byte{"cap-1": []byte("x")},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest, "--capture", "cap-nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found) (stderr: %s)", code, uzicli.ExitNotFound, stderr)
	}
}

// TestRunExportNoArchive: a run with no captures at all is a clean not-found.
func TestRunExportNoArchive(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: false},
		},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, _, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

// TestRunExportRequiresOutput: --output is required.
func TestRunExportRequiresOutput(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "export", "r1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

// TestRunGetRecoverySummary: `uzi run get` on a terminal run with captures shows the
// metadata-only RECOVERY block; a non-terminal run shows none.
func TestRunGetRecoverySummary(t *testing.T) {
	data := []byte("bundle")
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{
			"failed1":  {ID: "failed1", Kind: "issue", Status: "failed"},
			"running1": {ID: "running1", Kind: "issue", Status: "running"},
		},
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"failed1":  {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
			"running1": {Supported: true, HasOpenHold: true},
		},
	}

	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "failed1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "RECOVERY") || !strings.Contains(stdout, "cap-1") {
		t.Errorf("terminal run get missing RECOVERY block / capture id:\n%s", stdout)
	}

	// A non-terminal run must NOT fetch/print the recovery block (no extra round-trip, no
	// false pending claim in the ordinary run view).
	rstdout, _, rcode := runCLI(t, fakeEnv(fc), "run", "get", "running1")
	if rcode != uzicli.ExitOK {
		t.Fatalf("running run get exit = %d", rcode)
	}
	if strings.Contains(rstdout, "RECOVERY") {
		t.Errorf("non-terminal run get must not show a RECOVERY block:\n%s", rstdout)
	}
}

// TestRunGetRecoverySummaryBestEffort: a recovery-summary fetch failure must NOT fail
// `run get` — the run detail still renders and exits 0.
func TestRunGetRecoverySummaryBestEffort(t *testing.T) {
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{"failed1": {ID: "failed1", Kind: "issue", Status: "failed"}},
		// The summary read fails (e.g. an older server without the endpoint).
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitUnreachable, "boom"),
	}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "failed1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get must survive a recovery-summary failure; exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "failed1") {
		t.Errorf("run detail should still render:\n%s", stdout)
	}
	if strings.Contains(stdout, "RECOVERY") {
		t.Errorf("a failed recovery fetch must render no RECOVERY block:\n%s", stdout)
	}
}
