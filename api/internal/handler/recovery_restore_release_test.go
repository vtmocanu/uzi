package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// restoreFinal substitutes only test control routes. Ordinary requests use the
// production router, authentication, FINAL observer, limiter and database.
func restoreFinal(t *testing.T, e *recoveryEnv, h *Handler, capture, closure uuid.UUID, source, pin, tip, bare, tmp string, traffic *restoreTraffic, node, refusal bool) {
	t.Helper()
	for _, key := range []string{"UZI_TOKEN", "UZI_URL", "UZI_CONTEXT", "UZI_RESTORE_SENTINEL"} {
		t.Setenv(key, "inert-inherited-final-sentinel")
	}
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e.mustExec("UPDATE workers SET token_hash=$2 WHERE id=$1", e.worker.ID, hash)
	e.mustExec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	router := h.WorkerRoutes(mw.NewLimiter(100, time.Hour, nil))
	if !h.RecoveryService().PrerequisiteFinalEnabled() {
		t.Fatal("production FINAL port not wired")
	}
	inspect := func() map[string]any {
		t.Helper()
		var state string
		var evidence, disposition *string
		var finalClosure, finalCapture *uuid.UUID
		if err := e.pool.QueryRow(e.ctx, `SELECT state,release_evidence,final_disposition,final_closure_id,final_capture_id
   FROM recovery_custody_holds WHERE id=$1`, e.holdID).Scan(&state, &evidence, &disposition, &finalClosure, &finalCapture); err != nil {
			t.Error("fixture hold inspection failed")
			return nil
		}
		_, pinErr := restoreCommand(tmp, bare, "git", "show-ref", "--verify", pin)
		return map[string]any{"listings": traffic.listings.Load(), "pin_present": pinErr == nil,
			"hold": map[string]any{"state": state, "release_evidence": evidence, "final_disposition": disposition, "final_closure_id": finalClosure, "final_capture_id": finalCapture}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if action, control := strings.CutPrefix(r.URL.Path, "/fixture/"); control {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch action {
			case "info":
				_ = json.NewEncoder(w).Encode(map[string]any{"run_id": e.run, "capture_id": capture, "closure_id": closure, "hold_id": e.holdID, "repo_id": e.repo, "worker_id": e.worker.ID, "generation": 1, "source_sha": source, "coverage_digest": strings.Repeat("a", 64)})
			case "inspect":
				_ = json.NewEncoder(w).Encode(inspect())
			default:
				http.Error(w, "unknown fixture control", http.StatusNotFound)
			}
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	before := traffic.listings.Load()
	beforePosts, beforeCreates := traffic.receivePosts.Load(), traffic.creates.Load()
	var available bool
	if err := e.pool.QueryRow(e.ctx, `SELECT c.state='available' AND c.manifest_bound AND c.expires_at>now()
  AND c.source_sha=$2 AND c.hold_id=$3 AND EXISTS(SELECT 1 FROM recovery_capture_chunks ch WHERE ch.capture_id=c.id)
  AND h.state='open' AND h.live_worker_id=$4 AND h.live_run_id=c.run_id
  AND h.final_capture_id IS NULL AND h.final_closure_id IS NULL AND h.final_disposition IS NULL AND h.release_evidence IS NULL
  AND r.status='completed' AND r.worker_id=$4 AND r.claim_generation=1
  FROM recovery_captures c JOIN recovery_custody_holds h ON h.id=c.hold_id JOIN runs r ON r.id=c.run_id WHERE c.id=$1`,
		capture, source, e.holdID, e.worker.ID).Scan(&available); err != nil || !available {
		t.Fatalf("FINAL prerequisites/owner/generation invalid: %v", err)
	}
	if node {
		root, err := filepath.Abs("../../..")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := json.Marshal(map[string]any{"url": server.URL, "token": token, "retain_pin": true, "expected_refusal": refusal})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(e.ctx, 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "test/fixtures/prerequisite-closure-release-e2e.ts")
		cmd.Dir, cmd.Env = filepath.Join(root, "agent"), append(restoreEnv(tmp), "UZI_CLOSURE_RELEASE_FIXTURE="+string(cfg))
		out, err := cmd.CombinedOutput()
		if err != nil {
			if strings.Contains(string(out), "committed closure release state actual=open expected=released") {
				t.Fatal("WorkerClient committed closure release state actual=open expected=released")
			}
			t.Fatalf("WorkerClient fixture failed: %v; private output bytes=%d", err, len(out))
		}
		t.Logf("WorkerClient fixture passed; private output bytes=%d", len(out))
	} else {
		gen := int64(1)
		body, err := json.Marshal(apitypes.RecoveryReleaseRequest{Generation: &gen, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "archive", CaptureID: capture.String(), SourceSha: source, CoverageDigest: strings.Repeat("a", 64)}})
		if err != nil {
			t.Fatal(err)
		}
		var ack []byte
		// One original request plus one replay bounds attempts; a failure stops this proof.
		for attempt := 0; attempt < 2; attempt++ {
			req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, server.URL+"/api/worker/runs/"+e.run.String()+"/archives/release", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 15 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
			closeErr := resp.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal("FINAL response read failed")
			}
			if refusal {
				var failure struct {
					Reason string `json:"reason"`
				}
				if resp.StatusCode != http.StatusConflict || json.Unmarshal(out, &failure) != nil || failure.Reason != "manifest_conflict" {
					t.Fatalf("FINAL refusal status=%d", resp.StatusCode)
				}
			} else {
				var response apitypes.RecoveryReleaseResponse
				if resp.StatusCode != http.StatusOK || json.Unmarshal(out, &response) != nil || !response.Released || response.HoldsReleased != 1 || response.RunID != e.run.String() || response.Generation == nil || *response.Generation != 1 {
					t.Fatalf("FINAL ACK status=%d", resp.StatusCode)
				}
			}
			if attempt == 0 {
				ack = out
			} else if !bytes.Equal(ack, out) {
				t.Fatal("FINAL replay changed response")
			}
		}
	}
	var valid bool
	if refusal {
		if err := e.pool.QueryRow(e.ctx, `SELECT state='open' AND release_evidence IS NULL AND final_disposition IS NULL
   AND final_capture_id IS NULL AND final_closure_id IS NULL AND live_worker_id=$2 FROM recovery_custody_holds WHERE id=$1`, e.holdID, e.worker.ID).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if traffic.listings.Load() != before {
			t.Fatal("refused FINAL listed remote")
		}
	} else {
		if err := e.pool.QueryRow(e.ctx, `SELECT h.state='released' AND h.release_evidence='prerequisite_closure' AND h.final_disposition='archive'
   AND h.final_capture_id=$2 AND h.final_closure_id=$3 AND h.live_worker_id IS NULL AND h.live_run_id IS NULL
   AND p.normalized_base_url='https://github.com' AND p.state='pinned'
   FROM recovery_custody_holds h JOIN recovery_prerequisite_closures p ON p.id=h.final_closure_id WHERE h.id=$1`, e.holdID, capture, closure).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if traffic.listings.Load()-before != 1 {
			t.Fatal("FINAL must perform exactly one native listing; replay none")
		}
		if restoreGit(t, tmp, bare, "rev-parse", pin) != tip {
			t.Fatal("FINAL removed native pin")
		}
		traffic.mu.Lock()
		correctCommand := traffic.createRef == pin && traffic.createTip == tip
		traffic.mu.Unlock()
		if !correctCommand {
			t.Fatal("native create command does not bind exact UUID pin and full tip")
		}
		if beforePosts != 1 || beforeCreates != 1 {
			t.Fatalf("production sweep must create exactly one native pin: posts=%d creates=%d", beforePosts, beforeCreates)
		}
	}
	if !valid || traffic.receivePosts.Load() != beforePosts || traffic.creates.Load() != beforeCreates {
		t.Fatal("FINAL custody receipt or native traffic changed unexpectedly")
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT state='available' AND manifest_bound AND expires_at>now()
  AND EXISTS(SELECT 1 FROM recovery_capture_chunks WHERE capture_id=$1) FROM recovery_captures WHERE id=$1`, capture).Scan(&valid); err != nil || !valid {
		t.Fatal("FINAL lost archive bytes/protection")
	}
	t.Logf("FINAL native listings before=%d after=%d receive_posts=%d creates=%d refusal=%t", before, traffic.listings.Load(), traffic.receivePosts.Load(), traffic.creates.Load(), refusal)
}

func TestRecoveryReleasedRestoreSmallLiveDB(t *testing.T)  { restoreSmall(t, "released") }
func TestRecoveryBeforePinRefusalSmallLiveDB(t *testing.T) { restoreSmall(t, "refusal") }

func restoreSmall(t *testing.T, mode string) {
	t.Helper()
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("owned UZI_TEST_DATABASE_URL required")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, ".uzi", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(scratch, "restore-small-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	if err := restorePreflight(restoreSetupFor(root, tmp, "", false)); err != nil {
		t.Fatal(err)
	}
	binary := ""
	if mode == "released" {
		binary = filepath.Join(tmp, "restore-driver")
		start := time.Now()
		if out, err := restoreCommand(tmp, filepath.Join(root, "api"), "go", "test", "-buildvcs=false", "-c", "-vet=off", "-o", binary, "./cmd/uzi"); err != nil {
			t.Fatalf("compile CLI driver: %v; output bytes=%d", err, len(out))
		}
		t.Logf("CLI_build=%s (outside owner restore clock)", time.Since(start))
	}
	restoreWorkflow(t, tmp, binary, "", mode)
}

// The same URL with unrelated credentials must not expose this owner's capture.
// Both stores are explicit fixture Stores; the CLI never consults DefaultStore.
func restoreOwnerIsolation(t *testing.T, e *recoveryEnv, tmp, dir, binary, api string, capture uuid.UUID) {
	t.Helper()
	other := cliSeedUser(t, e.pool, false)
	unrelated := cliMintToken(t, e.pool, other, clitoken.ScopeUser)
	for _, suffix := range []string{"", "/" + capture.String() + "/download"} {
		req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, api+"/api/runs/"+e.run.String()+"/archives"+suffix, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+unrelated)
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unrelated owner capture access status=%d", resp.StatusCode)
		}
	}
	for _, mode := range []string{"empty", "unrelated"} {
		config := uzicli.NewStore(filepath.Join(dir, "isolation-"+mode))
		if err := config.SaveConfig(&uzicli.Config{Current: "fixture", Contexts: map[string]uzicli.Context{"fixture": {URL: api}}}); err != nil {
			t.Fatal(err)
		}
		if mode == "unrelated" {
			if err := config.SaveCredentials(&uzicli.Credentials{Contexts: map[string]uzicli.Credential{"fixture": {Token: unrelated}}}); err != nil {
				t.Fatal(err)
			}
		}
		output := filepath.Join(dir, "forbidden-"+mode+".bundle")
		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestRecoveryRestoreDriver$", "--", "run", "export", e.run.String(), "--capture", capture.String(), "--output", output) // #nosec G204 -- Compiled fixture driver, owned output path and generated UUIDs passed as argv; no shell or production configuration.
		cmd.Dir, cmd.Env = dir, append(restoreEnv(tmp), "RECOVERY_RESTORE_DRIVER=1", "RECOVERY_RESTORE_CONFIG="+config.Dir(), "RECOVERY_RESTORE_SKILL_HOME="+filepath.Join(dir, "skills"))
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			t.Fatalf("%s explicit store exported another owner's capture; output bytes=%d", mode, len(out))
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("unauthorized CLI wrote archive")
		}
	}
}

func TestRecoveryRestoreWorkerClientSmallE2E(t *testing.T) {
	if os.Getenv("UZI_CODEX_REFRESH_LOSTREPLY_E2E") == "" {
		t.Skip("explicit WorkerClient proof only")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, ".uzi", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(scratch, "restore-workerclient-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	c := restoreSetupFor(root, tmp, "", false)
	c.Node = true
	if err := restorePreflight(c); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(tmp, "restore-driver")
	start := time.Now()
	if out, err := restoreCommand(tmp, filepath.Join(root, "api"), "go", "test", "-buildvcs=false", "-c", "-vet=off", "-o", binary, "./cmd/uzi"); err != nil {
		t.Fatalf("compile CLI driver: %v; output bytes=%d", err, len(out))
	}
	t.Logf("CLI_build=%s (outside owner restore clock)", time.Since(start))
	for _, mode := range []string{"released", "refusal"} {
		t.Run(mode, func(t *testing.T) { restoreWorkflow(t, tmp, binary, "", mode, "workerclient") })
	}
}
