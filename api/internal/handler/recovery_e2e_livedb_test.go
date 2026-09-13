package handler

// PRD #1296 M7: the deterministic cross-component integration PROOF of durable run
// recovery. It exercises the FULL lifecycle end to end with the REAL recovery.Service +
// the REAL M1 store layer against live Postgres, driving an ISOLATED Git fixture built
// in-test with os/exec git (no forge, no network, no worker producer):
//
//	failure -> capture -> upload -> ACK -> terminal -> custody release ->
//	simulated worker/PVC teardown -> owner download (no worker) -> clean-clone import.
//
// It records the exact head/tree/history/binary equality (success criteria 2, 3) and the
// named authorization controls (success criterion 6), and proves an upload outage leaves
// the source preserved and retryable byte-identically without a PAT (success criterion 4).
//
// The in-test Git bundle stands in for the M3 TypeScript worker producer (M3 separately
// proves the producer builds a verifiable bundle); this milestone proves the archive
// PRIMITIVE round-trips a REAL binary Git bundle byte-for-byte through encrypted DB chunks
// and reproduces H exactly in a fresh forge clone after the worker is gone.
//
// Placement: this proof lives in the HANDLER package alongside the M2/M5 recovery live-DB
// tests so CI's test-api-store-it job (which runs `-run 'LiveDB$'` over ./internal/handler/...,
// mirrored by e2e/run-store-it.sh) executes it for durable coverage. It reaches os/exec git
// and drives recovery.Service directly, reusing that suite's owner/repo/run/worker/hold
// seeding (newRecoveryEnv) and its shared helpers (recoveryTestLimits, sha256hex, cappedBody,
// errReader). Run it with:
//
//	UZI_TEST_DATABASE_URL=... go test -run 'LiveDB$' ./internal/handler/...
//
// Self-skips unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that
// prints `ok` with PASS=0 is INVALID, not green.

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const m7ChunkSize = 1 << 20 // ~1 MiB plaintext per encrypted chunk (recovery.chunkPlaintextSize).

// ── isolated Git fixture (M7 "isolated Git fixtures") ─────────────────────────────────────

// gitFixture is an isolated source repository built with os/exec git in a throwaway temp
// tree: a base commit B, intermediate history, and head H carrying a large non-compressible
// BINARY blob (with NUL and high bytes). It yields a real prerequisite bundle (B..H, tip
// refs/heads/main) AND a self-contained bundle (main, no prerequisite), and records H's full
// identity so an import can be checked for exact equality.
type gitFixture struct {
	env         []string // isolated git env (no global/system config, fixed identity/dates)
	baseRepo    string   // a repo containing ONLY B's closure; a fresh clone of it has B, not H
	prereqBytes []byte   // real git bundle: prerequisite B, tip refs/heads/main = H
	selfBytes   []byte   // real git bundle: self-contained (no prerequisite), tip main = H

	commitB  string // base commit id (the bundle prerequisite)
	commitH  string // head commit id
	treeH    string // H's tree id
	blobOID  string // the binary blob's git object id at H
	history  string // `rev-list --parents main`: the full parent chain / history of H
	binName  string // the binary file's path in the tree
	binBytes []byte // the binary blob's exact content
}

// isolatedGitEnv builds an environment that pins git identity/dates and severs any global or
// system config, so the fixture is deterministic and independent of the host.
func isolatedGitEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=uzi-m7",
		"GIT_AUTHOR_EMAIL=m7@uzi.test",
		"GIT_COMMITTER_NAME=uzi-m7",
		"GIT_COMMITTER_EMAIL=m7@uzi.test",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05 +0000",
		"GIT_COMMITTER_DATE=2026-01-02T03:04:05 +0000",
		"GIT_TERMINAL_PROMPT=0",
		"LANG=C",
	}
}

// runGitRaw runs git and returns raw (un-trimmed) stdout/stderr so a binary blob read via
// `cat-file -p` is byte-exact. The command name is the constant "git"; args are entirely
// test-controlled fixture constants and temp paths.
func runGitRaw(env []string, dir string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: constant command "git"; args are test-controlled fixture constants/paths, never user input
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func mustGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	out, errb, err := runGitRaw(env, dir, args...)
	if err != nil {
		t.Fatalf("git %s (in %s): %v\nstderr: %s", strings.Join(args, " "), dir, err, string(errb))
	}
	return strings.TrimSpace(string(out))
}

// buildGitFixture constructs the isolated source repo and both bundle shapes. B is created in
// a `base` repo; a `work` clone of it advances to H and produces the bundles. Because the H
// commits are never sent back to `base`, a later fresh clone of `base` has ONLY B's closure —
// modelling a forge whose public default history reaches the prerequisite but not H, with no
// git push anywhere.
func buildGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	env := isolatedGitEnv(t)
	root := t.TempDir()

	base := filepath.Join(root, "base")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	mustGit(t, env, base, "init", "-q", "-b", "main")
	writeFixtureFile(t, filepath.Join(base, "base.txt"), []byte("base content for prerequisite B\n"))
	mustGit(t, env, base, "add", "base.txt")
	mustGit(t, env, base, "commit", "-q", "-m", "B: base commit")
	commitB := mustGit(t, env, base, "rev-parse", "HEAD")

	work := filepath.Join(root, "work")
	mustGit(t, env, root, "clone", "-q", "--no-hardlinks", base, work)

	// An intermediate commit carrying a small binary, so H is not a direct child of B and the
	// parent chain has real depth to reproduce.
	writeFixtureFile(t, filepath.Join(work, "mid.bin"), []byte{0x00, 0x01, 0xff, 0xfe, 'm', 'i', 'd', 0x00, 0x80})
	mustGit(t, env, work, "add", "mid.bin")
	mustGit(t, env, work, "commit", "-q", "-m", "mid: intermediate binary")

	// H carries a large, non-compressible binary blob so the bundle spans MULTIPLE encrypted
	// chunks; explicit NUL and high bytes are planted so a text-diff pipeline would corrupt it.
	binName := "payload.bin"
	binBytes := make([]byte, m7ChunkSize+m7ChunkSize/2) // ~1.5 MiB -> 2 chunks
	if _, err := rand.Read(binBytes); err != nil {
		t.Fatalf("fill binary blob: %v", err)
	}
	binBytes[0] = 0x00
	binBytes[1] = 0xff
	binBytes[len(binBytes)-1] = 0x00
	if bytes.IndexByte(binBytes, 0x00) < 0 {
		t.Fatal("fixture binary must contain a NUL byte")
	}
	hasHigh := false
	for _, b := range binBytes {
		if b > 0x7f {
			hasHigh = true
			break
		}
	}
	if !hasHigh {
		t.Fatal("fixture binary must contain a high byte")
	}
	writeFixtureFile(t, filepath.Join(work, binName), binBytes)
	mustGit(t, env, work, "add", binName)
	mustGit(t, env, work, "commit", "-q", "-m", "H: head with binary payload")

	commitH := mustGit(t, env, work, "rev-parse", "HEAD")
	treeH := mustGit(t, env, work, "rev-parse", "HEAD^{tree}")
	blobOID := mustGit(t, env, work, "rev-parse", "HEAD:"+binName)
	history := mustGit(t, env, work, "rev-list", "--parents", "main")

	prereqPath := filepath.Join(root, "prereq.bundle")
	mustGit(t, env, work, "bundle", "create", prereqPath, commitB+"..main")
	selfPath := filepath.Join(root, "self.bundle")
	mustGit(t, env, work, "bundle", "create", selfPath, "main")

	return &gitFixture{
		env:         env,
		baseRepo:    base,
		prereqBytes: readFixtureFile(t, prereqPath),
		selfBytes:   readFixtureFile(t, selfPath),
		commitB:     commitB,
		commitH:     commitH,
		treeH:       treeH,
		blobOID:     blobOID,
		history:     history,
		binName:     binName,
		binBytes:    binBytes,
	}
}

// writeFixtureFile writes with 0600 perms; the path is a test-owned temp path.
func writeFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is a test-owned temp file created in this test, never user input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// assertImportsToH imports bundleFile into importRepo and asserts H reproduces with IDENTICAL
// commit id, parent chain/history, tree id, and binary blob (oid AND bytes). importRepo must
// already contain the bundle's prerequisite closure and must NOT contain H beforehand.
func (fx *gitFixture) assertImportsToH(t *testing.T, importRepo, bundleFile string) {
	t.Helper()
	// H must be ABSENT before import (the artifact, not the clone, carries H).
	if _, _, err := runGitRaw(fx.env, importRepo, "cat-file", "-e", fx.commitH); err == nil {
		t.Fatalf("H %s already present in the import repo before import; the closure was polluted", fx.commitH)
	}
	// `git bundle verify` confirms the file is a well-formed bundle whose prerequisites are
	// satisfied by the import repo (or none are required for the self-contained case).
	if _, errb, err := runGitRaw(fx.env, importRepo, "bundle", "verify", bundleFile); err != nil {
		t.Fatalf("git bundle verify %s: %v\nstderr: %s", bundleFile, err, string(errb))
	}
	mustGit(t, fx.env, importRepo, "fetch", "-q", bundleFile, "refs/heads/main:refs/heads/recovered")

	if got := mustGit(t, fx.env, importRepo, "rev-parse", "recovered"); got != fx.commitH {
		t.Fatalf("recovered commit id = %s, want H %s", got, fx.commitH)
	}
	if got := mustGit(t, fx.env, importRepo, "rev-parse", "recovered^{tree}"); got != fx.treeH {
		t.Fatalf("recovered tree id = %s, want %s", got, fx.treeH)
	}
	if got := mustGit(t, fx.env, importRepo, "rev-list", "--parents", "recovered"); got != fx.history {
		t.Fatalf("recovered parent chain/history mismatch\n got: %s\nwant: %s", got, fx.history)
	}
	if got := mustGit(t, fx.env, importRepo, "rev-parse", "recovered:"+fx.binName); got != fx.blobOID {
		t.Fatalf("recovered binary blob oid = %s, want %s", got, fx.blobOID)
	}
	gotBytes, errb, err := runGitRaw(fx.env, importRepo, "cat-file", "-p", "recovered:"+fx.binName)
	if err != nil {
		t.Fatalf("cat-file recovered binary: %v\nstderr: %s", err, string(errb))
	}
	if !bytes.Equal(gotBytes, fx.binBytes) {
		t.Fatalf("recovered binary is not byte-identical: got %d bytes, want %d", len(gotBytes), len(fx.binBytes))
	}
}

// ── M7 live-DB helpers (reusing the M2/M5 recoveryEnv seeding) ─────────────────────────────
//
// The owner/connection/repo/worker/run + OPEN custody hold seeding, recoveryTestLimits,
// sha256hex, cappedBody and errReader all come from recovery_livedb_test.go in this package;
// only the M7-specific extras (attempt-carrying reserve, owner download, worker teardown, and
// the prerequisite-aware manifest) are defined here.

// m7ManifestFor builds the byte manifest the worker binds (D2): the complete-bundle byte size,
// sha256 checksum, expected ~1 MiB chunk count, and the resolved public prerequisite closure.
func m7ManifestFor(data []byte, prereqs []string) apitypes.RecoveryUploadManifest {
	n := int64(len(data))
	count := 0
	if n > 0 {
		count = int((n + m7ChunkSize - 1) / m7ChunkSize)
	}
	return apitypes.RecoveryUploadManifest{ByteSize: n, Checksum: sha256hex(data), ChunkCount: count, PrerequisiteShas: prereqs}
}

// reserveWithAttempt reserves a capture like recoveryEnv.reserve but also binds the attempted
// head-sha provenance (H') the M7 proof carries alongside the source sha.
func (e *recoveryEnv) reserveWithAttempt(svc *recovery.Service, wkr store.Worker, key, sourceSha, attempted string) apitypes.RecoveryReserveResponse {
	e.t.Helper()
	res, err := svc.Reserve(e.ctx, wkr, e.run, apitypes.RecoveryReserveRequest{
		RunID: e.run.String(), IdempotencyKey: key, SourceSha: sourceSha, AttemptedHeadSha: attempted,
	})
	if err != nil {
		e.t.Fatalf("reserve %q: %v", key, err)
	}
	return res
}

// downloadBytes streams the owner download through the real recovery.Service owner path and
// returns the decrypted body, asserting a 200 and the private/no-store/nosniff headers.
func (e *recoveryEnv) downloadBytes(svc *recovery.Service, owner uuid.UUID, captureID string) []byte {
	e.t.Helper()
	rec := httptest.NewRecorder()
	if err := svc.Download(e.ctx, rec, owner, e.run, uuid.MustParse(captureID)); err != nil {
		e.t.Fatalf("owner download: %v", err)
	}
	if rec.Code != http.StatusOK {
		e.t.Fatalf("owner download status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		e.t.Errorf("Cache-Control = %q, want private, no-store", cc)
	}
	if nn := rec.Header().Get("X-Content-Type-Options"); nn != "nosniff" {
		e.t.Errorf("X-Content-Type-Options = %q, want nosniff", nn)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		e.t.Errorf("Content-Disposition = %q, want an attachment filename", cd)
	}
	return rec.Body.Bytes()
}

// tryDeleteWorker attempts to delete the seeded worker row, so the caller can assert the
// RESTRICT FK blocks it while custody is open and permits it once released.
func (e *recoveryEnv) tryDeleteWorker() error {
	_, err := e.pool.Exec(e.ctx, `DELETE FROM workers WHERE id=$1`, e.worker.ID)
	return err
}

// ── The deterministic M7 sequence ─────────────────────────────────────────────────────────

// TestRecoveryLifecycleE2ELiveDB is the M7 proof of steps 1-6: an isolated Git fixture is
// captured, uploaded and ACKed through the real recovery.Service; the run goes terminal; the
// custody is released; the worker (and thus its PVC/token) is torn down; and the OWNER then
// downloads the surviving archive with NO worker and imports it into a fresh forge clone,
// reproducing H byte-for-byte. It covers BOTH the prerequisite bundle (imports against a
// public closure with B) and the self-contained bundle (imports into an empty repo).
//
// Recorded exact-equality (success criteria 2 & 3): the imported ref reproduces H's commit
// id, its full parent chain/history, its tree id, and the binary payload's git oid AND raw
// bytes; and the downloaded bytes' sha256 equals the uploaded manifest checksum (a
// byte-identical round-trip through encrypted DB chunks). No H', checkpoint tip or reused
// branch can stand in for H because the imported commit id is asserted equal to H itself.
//
// Named authorization controls (success criterion 6): worker ops (reserve/upload/status) are
// gated by the ORIGINAL worker identity over its OPEN hold — a foreign worker is refused
// (ErrNotAuthorized), the service-level equivalent of the worker-Bearer control. Owner ops
// are gated by owner scope — a foreign owner is refused (ErrCaptureNotFound); at the router
// this is RequireUser + GetRun owner-or-404 (an admin viewing a foreign run is also refused,
// GetRun ignoring admin), proven at the HTTP layer in the M2/M5 handler live-DB tests
// (internal/handler/recovery_livedb_test.go: TestRecoveryOwnerDownloadAuthLiveDB).
func TestRecoveryLifecycleE2ELiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())

	// Step 1: isolated Git fixture + its real bundles.
	fx := buildGitFixture(t)

	// Step 2: the seeded run has an OPEN custody hold before any capture.
	if e.holdState() != "open" {
		t.Fatalf("precondition: hold state = %q, want open", e.holdState())
	}

	// Step 3: capture -> upload -> ACK through the real recovery.Service (prerequisite bundle).
	capP := e.reserveWithAttempt(svc, e.worker, "k-prereq", fx.commitH, fx.commitH /*attempted H' provenance*/)
	// Commit-before-ACK precondition: a freshly reserved capture is 'preparing' with zero
	// chunks. Readiness and the full chunk inventory then commit TOGETHER (asserted post-upload
	// below); the interrupted-upload test proves a partial stream commits nothing.
	if capP.State != "preparing" {
		t.Fatalf("reserved capture state = %q, want preparing", capP.State)
	}
	if e.chunkCount(capP.CaptureID) != 0 {
		t.Fatalf("fresh capture has %d chunks, want 0", e.chunkCount(capP.CaptureID))
	}
	mP := m7ManifestFor(fx.prereqBytes, []string{fx.commitB})
	upP, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(capP.CaptureID), mP, cappedBody(fx.prereqBytes, recoveryTestLimits().MaxBundleBytes))
	if err != nil {
		t.Fatalf("upload prerequisite bundle: %v", err)
	}
	if upP.State != "available" {
		t.Fatalf("prerequisite capture state = %q, want available", upP.State)
	}
	if got := e.chunkCount(capP.CaptureID); got != mP.ChunkCount || got < 2 {
		t.Fatalf("stored %d chunks, want %d (>=2, proving a real multi-chunk binary bundle)", got, mP.ChunkCount)
	}

	// Step 3 (self-contained variant): a second capture carrying the self-contained bundle.
	capS := e.reserveWithAttempt(svc, e.worker, "k-self", fx.commitH, "")
	mS := m7ManifestFor(fx.selfBytes, nil)
	if _, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(capS.CaptureID), mS, cappedBody(fx.selfBytes, recoveryTestLimits().MaxBundleBytes)); err != nil {
		t.Fatalf("upload self-contained bundle: %v", err)
	}
	if st := e.captureState(capS.CaptureID); st != "available" {
		t.Fatalf("self-contained capture state = %q, want available", st)
	}

	// Worker-identity control: a DIFFERENT worker (even of the same owner) cannot reserve,
	// upload to, or inspect a capture it does not hold.
	foreign := store.Worker{ID: uuid.New(), UserID: e.user, Name: "foreign-worker"}
	if _, err := svc.Reserve(e.ctx, foreign, e.run, apitypes.RecoveryReserveRequest{RunID: e.run.String(), IdempotencyKey: "k-foreign", SourceSha: fx.commitH}); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("foreign-worker reserve err = %v, want ErrNotAuthorized", err)
	}
	if _, err := svc.Upload(e.ctx, foreign, e.run, uuid.MustParse(capP.CaptureID), mP, cappedBody(fx.prereqBytes, recoveryTestLimits().MaxBundleBytes)); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("foreign-worker upload err = %v, want ErrNotAuthorized", err)
	}
	if _, err := svc.Status(e.ctx, foreign, e.run, uuid.MustParse(capP.CaptureID)); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("foreign-worker status err = %v, want ErrNotAuthorized", err)
	}

	// Step 4: run goes terminal (failed finalization). The execution stays honestly failed;
	// recoverability is a separate fact.
	e.mustExec(`UPDATE runs SET status='failed' WHERE id=$1`, e.run)

	// Custody blocks teardown at the DB backstop: while the hold is OPEN its live_worker_id is
	// a non-null ON DELETE RESTRICT FK, so deleting the worker is refused. (M4 adds the Go-level
	// DELETE guards; this proves the schema-level last line of defense.)
	if err := e.tryDeleteWorker(); err == nil {
		t.Fatal("deleting the worker while custody is OPEN succeeded; the RESTRICT FK must block it")
	}

	// The successful capture releases the covered source's custody (the worker release op).
	rel, err := svc.Release(e.ctx, e.worker, e.run)
	if err != nil || !rel.Released || rel.HoldsReleased != 1 {
		t.Fatalf("release = (%+v, %v), want 1 hold released", rel, err)
	}
	if e.holdState() != "released" {
		t.Fatalf("hold state = %q after release, want released", e.holdState())
	}

	// Simulated worker/PVC teardown: with the hold's live FKs cleared, the worker row (and, in
	// production, its token and PVC) can be deleted. The archive is decoupled from the worker.
	if err := e.tryDeleteWorker(); err != nil {
		t.Fatalf("deleting the worker after release failed: %v", err)
	}

	// The capture + its chunks SURVIVE the teardown (hold->capture and worker are never a
	// cascade onto the archive bytes).
	if st := e.captureState(capP.CaptureID); st != "available" {
		t.Fatalf("prerequisite capture state = %q after teardown, want still available", st)
	}
	if got := e.chunkCount(capP.CaptureID); got != mP.ChunkCount {
		t.Fatalf("prerequisite chunks = %d after teardown, want %d (survived)", got, mP.ChunkCount)
	}
	if st := e.captureState(capS.CaptureID); st != "available" {
		t.Fatalf("self-contained capture state = %q after teardown, want still available", st)
	}

	// Step 5: OWNER download with NO worker (owner-scoped). A foreign owner is refused.
	if err := svc.Download(e.ctx, httptest.NewRecorder(), uuid.New(), e.run, uuid.MustParse(capP.CaptureID)); !errors.Is(err, recovery.ErrCaptureNotFound) {
		t.Fatalf("foreign-owner download err = %v, want ErrCaptureNotFound (owner-scope seam)", err)
	}
	gotP := e.downloadBytes(svc, e.user, capP.CaptureID)
	if !bytes.Equal(gotP, fx.prereqBytes) {
		t.Fatalf("downloaded prerequisite bundle: %d bytes, want %d identical", len(gotP), len(fx.prereqBytes))
	}
	if sha256hex(gotP) != mP.Checksum {
		t.Fatalf("downloaded prerequisite sha256 = %s, want the uploaded manifest checksum %s", sha256hex(gotP), mP.Checksum)
	}
	gotS := e.downloadBytes(svc, e.user, capS.CaptureID)
	if !bytes.Equal(gotS, fx.selfBytes) {
		t.Fatalf("downloaded self-contained bundle: %d bytes, want %d identical", len(gotS), len(fx.selfBytes))
	}
	if sha256hex(gotS) != mS.Checksum {
		t.Fatalf("downloaded self-contained sha256 = %s, want %s", sha256hex(gotS), mS.Checksum)
	}

	// Step 6: clean-clone import + exact equality.
	work := t.TempDir()
	// Prerequisite bundle -> a FRESH clone that has ONLY B's public closure (not H).
	prereqFile := filepath.Join(work, "downloaded-prereq.bundle")
	writeFixtureFile(t, prereqFile, gotP)
	freshWithB := filepath.Join(work, "fresh-with-b")
	mustGit(t, fx.env, work, "clone", "-q", "--no-hardlinks", fx.baseRepo, freshWithB)
	fx.assertImportsToH(t, freshWithB, prereqFile)

	// Self-contained bundle -> a brand-new EMPTY repo with no prerequisite at all.
	selfFile := filepath.Join(work, "downloaded-self.bundle")
	writeFixtureFile(t, selfFile, gotS)
	emptyRepo := filepath.Join(work, "empty")
	if err := os.MkdirAll(emptyRepo, 0o700); err != nil {
		t.Fatalf("mkdir empty repo: %v", err)
	}
	mustGit(t, fx.env, emptyRepo, "init", "-q", "-b", "main")
	fx.assertImportsToH(t, emptyRepo, selfFile)
}

// TestRecoveryUploadOutagePreservesSourceLiveDB is the M7 proof of step 7 (success criterion
// 4): an interrupted/failed upload commits NOTHING (all chunks roll back), leaves the capture
// non-ready in needs_action, and RETAINS the open custody hold so the pinned source is
// preserved. A subsequent retry under the SAME capture identity — reproduced from the same
// verified bytes, with no forge PAT — succeeds and downloads byte-identically.
func TestRecoveryUploadOutagePreservesSourceLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	fx := buildGitFixture(t)
	data := fx.prereqBytes

	res := e.reserveWithAttempt(svc, e.worker, "k-retry", fx.commitH, fx.commitH)
	cid := uuid.MustParse(res.CaptureID)

	// Upload outage: stream a real prefix, then a transport error. At least one full ~1 MiB
	// chunk is inserted inside the transaction before the failure, so a clean rollback must
	// discard it.
	prefix := bytes.Repeat([]byte("p"), m7ChunkSize+m7ChunkSize/2)
	badManifest := apitypes.RecoveryUploadManifest{ByteSize: int64(len(prefix)) + 10, Checksum: sha256hex(prefix), ChunkCount: 2}
	interrupted := http.MaxBytesReader(
		httptest.NewRecorder(),
		io.NopCloser(io.MultiReader(bytes.NewReader(prefix), errReader{err: errors.New("connection reset")})),
		recoveryTestLimits().MaxBundleBytes,
	)
	if _, err := svc.Upload(e.ctx, e.worker, e.run, cid, badManifest, interrupted); err == nil {
		t.Fatal("interrupted upload succeeded; want a transport/integrity error")
	}
	if e.chunkCount(res.CaptureID) != 0 {
		t.Fatalf("interrupted upload left %d chunks, want 0 (rolled back)", e.chunkCount(res.CaptureID))
	}
	if st := e.captureState(res.CaptureID); st != "needs_action" {
		t.Fatalf("capture state = %q after outage, want needs_action", st)
	}
	if e.holdState() != "open" {
		t.Fatalf("hold state = %q after outage, want open (source retained)", e.holdState())
	}

	// Retry under the SAME capture identity with the real verified bytes and manifest.
	m := m7ManifestFor(data, []string{fx.commitB})
	up, err := svc.Upload(e.ctx, e.worker, e.run, cid, m, cappedBody(data, recoveryTestLimits().MaxBundleBytes))
	if err != nil {
		t.Fatalf("retry upload: %v", err)
	}
	if up.State != "available" {
		t.Fatalf("retry capture state = %q, want available", up.State)
	}

	// The retried artifact is byte-identical to the source bytes.
	got := e.downloadBytes(svc, e.user, res.CaptureID)
	if !bytes.Equal(got, data) {
		t.Fatalf("retried download: %d bytes, want %d identical", len(got), len(data))
	}
	if sha256hex(got) != m.Checksum {
		t.Fatalf("retried download sha256 = %s, want %s", sha256hex(got), m.Checksum)
	}
}
