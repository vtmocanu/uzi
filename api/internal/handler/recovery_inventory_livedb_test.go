package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// inventoryGit runs only literal Git fixture operations, without a remote or provider credential.
// Each command has a deadline; a failure stops this fixture, rather than importing partial bytes.
func inventoryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed Git binary; args are test-only literal operations and owned fixture paths/SHAs, no shell.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// inventoryBundle anchors both unrelated roots under a synthetic aggregate Q with the H2 tree.
func inventoryBundle(t *testing.T) ([]byte, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	inventoryGit(t, dir, "init")
	inventoryGit(t, dir, "config", "user.name", "Recovery Fixture")
	inventoryGit(t, dir, "config", "user.email", "recovery@example.com")
	inventoryGit(t, dir, "checkout", "-b", "inventory-h")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		inventoryGit(t, dir, "add", "work.txt")
		inventoryGit(t, dir, "commit", "-m", "inventory fixture")
	}
	write("original H content\n")
	h := inventoryGit(t, dir, "rev-parse", "HEAD")
	inventoryGit(t, dir, "checkout", "--orphan", "inventory-h2")
	inventoryGit(t, dir, "rm", "-rf", ".")
	write("unrelated H2 content\n")
	h2 := inventoryGit(t, dir, "rev-parse", "HEAD")
	tree := inventoryGit(t, dir, "rev-parse", h2+"^{tree}")
	q := inventoryGit(t, dir, "commit-tree", tree, "-p", h, "-p", h2, "-m", "recovery aggregate")
	if got := inventoryGit(t, dir, "rev-list", "--parents", "-n", "1", q); got != q+" "+h+" "+h2 {
		t.Fatalf("aggregate parents = %s, want both original roots", got)
	}
	if got := inventoryGit(t, dir, "rev-parse", q+"^{tree}"); got != tree {
		t.Fatalf("aggregate tree = %s, want current H2 tree %s", got, tree)
	}
	inventoryGit(t, dir, "update-ref", "refs/heads/recovered-source", q)
	path := filepath.Join(dir, "aggregate.bundle")
	inventoryGit(t, dir, "bundle", "create", path, "refs/heads/recovered-source")
	if got := inventoryGit(t, dir, "bundle", "list-heads", path); got != q+" refs/heads/recovered-source" {
		t.Fatalf("bundle refs = %q, want only recovered-source at Q", got)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: bundle path is created by this test in its own temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	return data, q, h, h2
}

func inventoryImport(t *testing.T, data []byte, h, h2 string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "download.bundle")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	inventoryGit(t, dir, "init")
	inventoryGit(t, dir, "bundle", "verify", path)
	inventoryGit(t, dir, "fetch", path, "refs/heads/recovered-source:refs/heads/recovered-source")
	for _, tc := range []struct{ sha, content string }{
		{h, "original H content"}, {h2, "unrelated H2 content"},
	} {
		inventoryGit(t, dir, "checkout", "--detach", tc.sha)
		if got := inventoryGit(t, dir, "rev-parse", "HEAD"); got != tc.sha {
			t.Fatalf("checkout = %s, want %s", got, tc.sha)
		}
		got, err := os.ReadFile(filepath.Join(dir, "work.txt")) //nolint:gosec // G304: fixed fixture filename in this test-created temporary repository.
		if err != nil || string(got) != tc.content+"\n" {
			t.Fatalf("restored content = %q, %v", got, err)
		}
	}
}

// This unit check executes the Git fixture even when the LiveDB proof cannot run.
func TestRecoveryInventoryAggregateBundle(t *testing.T) {
	data, _, h, h2 := inventoryBundle(t)
	inventoryImport(t, data, h, h2)
}

// inventoryBuildCLI builds once for both deletion cases, with a bounded foreground
// process and temporary build artifacts under the parent test's private directory.
func inventoryBuildCLI(t *testing.T, dir string) string {
	t.Helper()
	binary := filepath.Join(dir, "uzi")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "../../cmd/uzi") //nolint:gosec // G204: fixed Go build command/args; output is a test-owned temporary binary, no shell.
	cmd.WaitDelay = time.Second
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMPDIR=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "TMPDIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture CLI: %v\n%s", err, out)
	}
	return binary
}

// inventoryCLIExport invokes the actual command after worker deletion, then
// attempts one overwrite. Each invocation has a deadline; failure stops this case.
func inventoryCLIExport(t *testing.T, binary, url, ownerToken, runID, captureID string, want []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "recovered.bundle")
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "UZI_TOKEN", "UZI_URL", "UZI_CONTEXT", "UZI_SKILL_AUTO_UPGRADE", "HOME", "XDG_CONFIG_HOME", "TMPDIR":
			continue
		}
		env = append(env, entry)
	}
	// DefaultStore ignores XDG_CONFIG_HOME; a private HOME also isolates config reads.
	env = append(env, "UZI_TOKEN="+ownerToken, "UZI_SKILL_AUTO_UPGRADE=0", "HOME="+dir, "TMPDIR="+dir)
	run := func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--url", url, "run", "export", runID, "--capture", captureID, "--output", output) //nolint:gosec // G204: test-built binary, fixed export command, local test server and fixture IDs/paths, no shell.
		cmd.Env = env
		cmd.Dir = dir
		cmd.WaitDelay = time.Second
		return cmd.CombinedOutput()
	}
	if out, err := run(); err != nil {
		t.Fatalf("CLI run export: %v\n%s", err, out)
	}
	read := func() []byte {
		t.Helper()
		got, err := os.ReadFile(output) //nolint:gosec // G304: output is a test-created destination in this test-owned temporary directory.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) || sha256hex(got) != sha256hex(want) {
			t.Fatal("CLI export changed aggregate bytes or checksum")
		}
		info, err := os.Stat(output)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("CLI output mode = %o, want 0600", info.Mode().Perm())
		}
		return got
	}
	downloaded := read()
	// Use different existing bytes so a successful clobber cannot masquerade as refusal.
	sentinel := []byte("existing private destination\n")
	if err := os.WriteFile(output, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err == nil || !strings.Contains(string(out), "refusing to overwrite existing path") {
		t.Fatalf("CLI overwrite = %v, want existing-destination refusal: %s", err, out)
	}
	if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, sentinel) { //nolint:gosec // G304: output is the test-owned temporary destination populated with the sentinel above.
		t.Fatalf("CLI overwrite changed existing destination: %q, %v", got, err)
	}
	return downloaded
}

// TestRecoveryInventoryClosureLiveDB exercises the real worker and owner router mounts.
// Earlier capture + terminal reconciliation cannot close guarded custody. Only an exact
// FINAL receipt authorizes deletion, and the owner can recover both unrelated roots afterward.
func TestRecoveryInventoryClosureLiveDB(t *testing.T) {
	privateDir := t.TempDir()
	var cliBinary string
	for _, deletion := range []string{"manual", "ephemeral-reaper"} {
		t.Run(deletion, func(t *testing.T) {
			e := newRecoveryEnvGuarded(t, true)
			var guarded bool
			if err := e.pool.QueryRow(e.ctx, "SELECT inventory_guarded FROM recovery_custody_holds WHERE id=$1", e.holdID).Scan(&guarded); err != nil || !guarded {
				t.Fatalf("initial hold inventory_guarded = %t, want true: %v", guarded, err)
			}
			if cliBinary == "" {
				cliBinary = inventoryBuildCLI(t, privateDir)
			}
			h, router, _ := cliLiveDB(t)
			jwtSecret := h.cfg.JWTSecret
			h.cfg = recoveryTestCfg()
			h.cfg.JWTSecret = jwtSecret
			h.cfg.RecoveryMaxCapturesPerClaim = 2
			h.box = e.box
			ownerToken := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
			token, hash, err := jointoken.Generate()
			if err != nil {
				t.Fatal(err)
			}
			cliMustExec(t, e.pool, "UPDATE workers SET token_hash=$2, protocol_capabilities=$3 WHERE id=$1",
				e.worker.ID, hash, []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1})
			cliMustExec(t, e.pool, "UPDATE runs SET worker_id=$2, claim_generation=1 WHERE id=$1", e.run, e.worker.ID)
			if deletion == "ephemeral-reaper" {
				cliMustExec(t, e.pool, "UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', ephemeral=true, ephemeral_run_id=$2 WHERE id=$1", e.worker.ID, e.run)
			}
			call := func(method, path, credential string, body []byte, manifest *apitypes.RecoveryUploadManifest) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, path, bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+credential)
				req.Header.Set("Content-Type", "application/json")
				if manifest != nil {
					b, marshalErr := json.Marshal(manifest)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					req.Header.Set(recoveryManifestHeader, string(b))
					req.Header.Set("Content-Type", "application/octet-stream")
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			check := func(rec *httptest.ResponseRecorder, code int) {
				t.Helper()
				if rec.Code != code {
					t.Fatalf("HTTP = %d, want %d: %s", rec.Code, code, rec.Body.String())
				}
			}
			encode := func(v any) []byte {
				t.Helper()
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			base := "/api/worker/runs/" + e.run.String() + "/archives"
			gen := int64(1)
			reserve := func(key, source, digest string) string {
				t.Helper()
				rec := call(http.MethodPost, base+"/reserve", token, encode(apitypes.RecoveryReserveRequest{
					Generation: &gen, IdempotencyKey: key, SourceSha: source, CoverageDigest: digest,
				}), nil)
				check(rec, http.StatusOK)
				var res apitypes.RecoveryReserveResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.CaptureID == "" {
					t.Fatalf("reserve response: %+v %v", res, err)
				}
				return res.CaptureID
			}
			upload := func(id string, data []byte) {
				t.Helper()
				m := manifestFor(data)
				check(call(http.MethodPost, base+"/"+id+"/upload", token, data, &m), http.StatusOK)
				if e.captureState(id) != "available" {
					t.Fatal("upload did not become available")
				}
				var sealed []byte
				if err := e.pool.QueryRow(e.ctx, "SELECT sealed FROM recovery_capture_chunks WHERE capture_id=$1 ORDER BY chunk_index LIMIT 1", id).Scan(&sealed); err != nil {
					t.Fatal(err)
				}
				if len(sealed) == 0 || bytes.Contains(sealed, data) {
					t.Fatal("stored payload is empty or plaintext")
				}
			}
			data, sourceQ, head, otherHead := inventoryBundle(t)
			early := []byte("earlier capture without H2")
			earlyID := reserve("earlier", head, sha256hex([]byte(head)))
			upload(earlyID, early)
			// The terminal transition wins before the later aggregate reserve.
			cliMustExec(t, e.pool, "UPDATE runs SET status='completed' WHERE id=$1", e.run)
			if _, err := h.wsvc.ReconcileCustodyReleases(e.ctx); err != nil {
				t.Fatal(err)
			}
			legacy := call(http.MethodPost, base+"/release", token, encode(apitypes.RecoveryReleaseRequest{Generation: &gen}), nil)
			check(legacy, http.StatusOK)
			var legacyAck apitypes.RecoveryReleaseResponse
			if err := json.Unmarshal(legacy.Body.Bytes(), &legacyAck); err != nil || legacyAck.Released || legacyAck.HoldsReleased != 0 || e.holdState() != "open" {
				t.Fatalf("early capture settled custody: %+v %v", legacyAck, err)
			}
			deletePath := "/api/workers/" + e.worker.ID.String()
			check(call(http.MethodDelete, deletePath, ownerToken, nil, nil), http.StatusConflict)
			reap := func() {
				t.Helper()
				if _, err := store.ReapEphemeralWorkers(e.ctx, e.pool, pgtype.Timestamptz{Time: time.Now(), Valid: true}, pgtype.Interval{}); err != nil {
					t.Fatal(err)
				}
			}
			exists := func(want bool) {
				t.Helper()
				var got bool
				if err := e.pool.QueryRow(e.ctx, "SELECT EXISTS(SELECT 1 FROM workers WHERE id=$1)", e.worker.ID).Scan(&got); err != nil || got != want {
					t.Fatalf("worker exists = %v, want %v: %v", got, want, err)
				}
			}
			reap()
			exists(true)
			digest := sha256hex([]byte(head + "\n" + otherHead + "\n"))
			finalID := reserve("aggregate-final", sourceQ, digest)
			upload(finalID, data)
			check(call(http.MethodPost, base+"/reserve", token, encode(apitypes.RecoveryReserveRequest{
				Generation: &gen, IdempotencyKey: "over-quota", SourceSha: sourceQ, CoverageDigest: digest,
			}), nil), http.StatusInsufficientStorage)
			final := apitypes.RecoveryFinalDisposition{Kind: "archive", CaptureID: finalID, SourceSha: sourceQ, CoverageDigest: digest}
			finalReq := apitypes.RecoveryReleaseRequest{Generation: &gen, FinalDisposition: &final}
			// A real available capture belonging to another owner/hold is no authority,
			// even when presented with this generation's otherwise valid FINAL identity.
			foreignEnv := newRecoveryEnv(t)
			foreignCapture := custodySeedCapture(t, e.pool, foreignEnv.user, foreignEnv.run,
				foreignEnv.worker.ID, foreignEnv.holdID, "foreign-final", "available", 1)
			foreignFinal := final
			foreignFinal.CaptureID = foreignCapture.String()
			check(call(http.MethodPost, base+"/release", token, encode(apitypes.RecoveryReleaseRequest{
				Generation: &gen, FinalDisposition: &foreignFinal,
			}), nil), http.StatusForbidden)
			wrongGen := int64(9)
			bad := finalReq
			bad.Generation = &wrongGen
			check(call(http.MethodPost, base+"/release", token, encode(bad), nil), http.StatusForbidden)
			badFinal := final
			badFinal.CoverageDigest = sha256hex([]byte("wrong coverage"))
			bad.FinalDisposition, bad.Generation = &badFinal, &gen
			check(call(http.MethodPost, base+"/release", token, encode(bad), nil), http.StatusConflict)
			if e.holdState() != "open" {
				t.Fatal("negative FINAL closed custody")
			}
			// Simulate a lost FINAL ACK: dispatch the actual HTTP request, ignore its response,
			// then let the authorized deletion path destroy the credential's database identity.
			call(http.MethodPost, base+"/release", token, encode(finalReq), nil)
			if e.holdState() != "released" {
				t.Fatal("FINAL did not commit")
			}
			check(call(http.MethodPost, base+"/reserve", token, encode(apitypes.RecoveryReserveRequest{
				Generation: &gen, IdempotencyKey: "closed-hold", SourceSha: sourceQ, CoverageDigest: digest,
			}), nil), http.StatusForbidden)
			if deletion == "manual" {
				check(call(http.MethodDelete, deletePath, ownerToken, nil, nil), http.StatusNoContent)
			} else {
				reap()
			}
			exists(false)
			check(call(http.MethodPost, base+"/release", token, encode(finalReq), nil), http.StatusUnauthorized)
			rec := call(http.MethodGet, "/api/recovery/holds", ownerToken, nil, nil)
			check(rec, http.StatusOK)
			var holds apitypes.RecoveryCustodyHoldsDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &holds); err != nil {
				t.Fatal(err)
			}
			if len(holds.Holds) != 1 || holds.Holds[0].ID != e.holdID.String() || holds.Holds[0].State != "released" || holds.Holds[0].FinalReceipt == nil || *holds.Holds[0].FinalReceipt != final {
				t.Fatalf("owner receipt after deletion: %+v", holds)
			}
			rec = call(http.MethodGet, "/api/runs/"+e.run.String()+"/archives/"+finalID+"/download", ownerToken, nil, nil)
			check(rec, http.StatusOK)
			if !bytes.Equal(rec.Body.Bytes(), data) {
				t.Fatal("owner download changed aggregate bytes")
			}
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			cliData := inventoryCLIExport(t, cliBinary, server.URL, ownerToken, e.run.String(), finalID, data)
			inventoryImport(t, cliData, head, otherHead)
		})
	}
}
