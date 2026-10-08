package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// Read the real fixture header for the authenticated trusted-worker declaration.
// The API keeps the archive opaque; the worker derives these prerequisites from the
// bundle header and declares them.
func inventoryPrerequisites(t *testing.T, data []byte) []string {
	t.Helper()
	header, _, ok := bytes.Cut(data, []byte("\n\n"))
	if !ok {
		t.Fatal("bundle has no header terminator")
	}
	var prerequisites []string
	for _, line := range strings.Split(string(header), "\n") {
		if strings.HasPrefix(line, "-") {
			sha, _, _ := strings.Cut(strings.TrimPrefix(line, "-"), " ")
			prerequisites = append(prerequisites, sha)
		}
	}
	return prerequisites
}

func inventoryCleanForgeImport(t *testing.T, data []byte, forge, prerequisite, q, h, h2 string) {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "clone")
	inventoryGit(t, parent, "clone", "--no-local", forge, dir)
	for _, name := range []string{"objects/info/alternates", "shallow"} {
		if _, err := os.Stat(filepath.Join(dir, ".git", name)); !os.IsNotExist(err) {
			t.Fatalf("clone has %s or cannot inspect it: %v", name, err)
		}
	}
	if got := inventoryGit(t, dir, "rev-parse", "HEAD"); got != prerequisite {
		t.Fatalf("forge HEAD=%s, want prerequisite %s", got, prerequisite)
	}
	refs := strings.Fields(inventoryGit(t, dir, "for-each-ref", "--format=%(refname)"))
	for _, ref := range refs {
		if !slices.Contains([]string{"refs/heads/inventory-h", "refs/remotes/origin/HEAD", "refs/remotes/origin/inventory-h"}, ref) {
			t.Fatalf("private ref before import: %s", ref)
		}
	}
	// Enumerate the clone's complete object store, including unreachable objects.
	objects := strings.Fields(inventoryGit(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"))
	if !slices.Contains(objects, prerequisite) {
		t.Fatal("published prerequisite missing")
	}
	for _, private := range []string{q, h, h2} {
		if slices.Contains(objects, private) {
			t.Fatalf("private object %s present before import", private)
		}
	}
	inventoryImportInto(t, dir, data, h, h2)
	if got := inventoryGit(t, dir, "rev-parse", "refs/heads/recovered-source"); got != q {
		t.Fatalf("imported source=%s, want %s", got, q)
	}
	if got := inventoryGit(t, dir, "rev-list", "--parents", "-n", "1", q); got != q+" "+h+" "+h2 {
		t.Fatalf("imported roots changed: %s", got)
	}
}

func TestRecoveryInventoryThinAggregateBundle(t *testing.T) {
	// Prove inherited Git overrides cannot redirect the fixture's object store.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nonexistent"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(t.TempDir(), "nonexistent"))
	data, q, h, h2, forge, base := inventoryBundleFixture(t, true)
	if got := inventoryPrerequisites(t, data); !slices.Equal(got, []string{base}) {
		t.Fatalf("actual prerequisites=%v, want %s", got, base)
	}
	inventoryCleanForgeImport(t, data, forge, base, q, h, h2)
}

func TestRecoveryInventoryThinClosureLiveDB(t *testing.T) {
	e := newRecoveryEnvGuarded(t, true)
	h, router, _ := cliLiveDB(t)
	jwtSecret := h.cfg.JWTSecret
	h.cfg = recoveryTestCfg()
	h.cfg.JWTSecret = jwtSecret
	h.box = e.box
	ownerToken := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, e.pool, "UPDATE workers SET token_hash=$2,protocol_capabilities=$3 WHERE id=$1", e.worker.ID, hash, []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1})
	cliMustExec(t, e.pool, "UPDATE runs SET worker_id=$2,claim_generation=1,status='completed' WHERE id=$1", e.run, e.worker.ID)
	call := func(method, path, credential string, body []byte, manifest *apitypes.RecoveryUploadManifest, code int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		if manifest != nil {
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(recoveryManifestHeader, string(encoded))
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != code {
			t.Fatalf("HTTP %s %s=%d, want %d: %s", method, path, rec.Code, code, rec.Body.String())
		}
		return rec
	}
	encode := func(v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	data, q, root, otherRoot, forge, base := inventoryBundleFixture(t, true)
	prerequisites := inventoryPrerequisites(t, data)
	if !slices.Equal(prerequisites, []string{base}) {
		t.Fatalf("thin prerequisites=%v", prerequisites)
	}
	digest := sha256hex([]byte(root + "\n" + otherRoot + "\n"))
	gen := int64(1)
	apiBase := "/api/worker/runs/" + e.run.String() + "/archives"
	rec := call(http.MethodPost, apiBase+"/reserve", token, encode(apitypes.RecoveryReserveRequest{Generation: &gen, IdempotencyKey: "thin-final", SourceSha: q, CoverageDigest: digest}), nil, http.StatusOK)
	var reserved apitypes.RecoveryReserveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reserved); err != nil {
		t.Fatal(err)
	}
	m := manifestFor(data)
	m.PrerequisiteShas = prerequisites
	call(http.MethodPost, apiBase+"/"+reserved.CaptureID+"/upload", token, data, &m, http.StatusOK)
	final := apitypes.RecoveryFinalDisposition{Kind: "archive", CaptureID: reserved.CaptureID, SourceSha: q, CoverageDigest: digest}
	call(http.MethodPost, apiBase+"/release", token, encode(apitypes.RecoveryReleaseRequest{Generation: &gen, FinalDisposition: &final}), nil, http.StatusConflict)
	var intact bool
	err = e.pool.QueryRow(e.ctx, `SELECT h.state='open' AND h.live_worker_id=$2 AND h.live_run_id=$3
 AND c.state='available' AND c.byte_size=$4 AND c.checksum=$5 AND c.prerequisite_shas=$6::text[]
 AND c.local_replica_worker_id IS NULL AND c.ready_retention_seconds IS NULL
 FROM recovery_custody_holds h JOIN recovery_captures c ON c.hold_id=h.id WHERE c.id=$1`,
		reserved.CaptureID, e.worker.ID, e.run, m.ByteSize, m.Checksum, prerequisites).Scan(&intact)
	if err != nil || !intact {
		t.Fatalf("thin FINAL changed available archive or custody: %v %v", intact, err)
	}
	call(http.MethodDelete, "/api/workers/"+e.worker.ID.String(), ownerToken, nil, nil, http.StatusConflict)
	download := call(http.MethodGet, "/api/runs/"+e.run.String()+"/archives/"+reserved.CaptureID+"/download", ownerToken, nil, nil, http.StatusOK)
	if !bytes.Equal(download.Body.Bytes(), data) {
		t.Fatal("owner download changed thin aggregate bytes")
	}
	inventoryCleanForgeImport(t, download.Body.Bytes(), forge, base, q, root, otherRoot)
}
