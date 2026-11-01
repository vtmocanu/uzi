package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// TestPrerequisiteClosureReleaseE2E (issue #3727, C2 M4) composes the REAL worker client
// (agent/src/client.ts) with the real API handler, production wiring and Postgres. Only the forge is
// synthetic: the loopback smart-HTTP remote of newWiredFinal. It is outside the ordinary LiveDB
// sweep by name; Node is required only for this explicit proof and is never silently skipped once
// opted in.
func TestPrerequisiteClosureReleaseE2E(t *testing.T) {
	if os.Getenv("UZI_CODEX_REFRESH_LOSTREPLY_E2E") == "" {
		t.Skip("opt in with task test:codex-refresh-lostreply-e2e")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fixture root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("combined proof requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("combined proof requires installed agent tsx")
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Fatal("combined proof requires UZI_TEST_DATABASE_URL")
	}

	f := newWiredFinal(t, 100)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.mustExec("UPDATE workers SET token_hash=$2 WHERE id=$1", f.worker.ID, hash)

	inspect := func() map[string]any {
		var state string
		var evidence, disposition *string
		var closure, capture *uuid.UUID
		if err := f.pool.QueryRow(f.ctx, `SELECT state,release_evidence,final_disposition,final_closure_id,final_capture_id
 FROM recovery_custody_holds WHERE id=$1`, f.holdID).Scan(&state, &evidence, &disposition, &closure, &capture); err != nil {
			t.Errorf("inspect hold: %v", err)
		}
		str := func(u *uuid.UUID) any {
			if u == nil {
				return nil
			}
			return u.String()
		}
		_, present := f.gitTip()
		return map[string]any{
			"listings": f.git.hits.Load(), "pin_present": present,
			"hold": map[string]any{"state": state, "release_evidence": evidence, "final_disposition": disposition,
				"final_closure_id": str(closure), "final_capture_id": str(capture)},
		}
	}
	control := func(action string) (any, bool) {
		switch action {
		case "info":
			return map[string]any{
				"run_id": f.run.String(), "capture_id": f.capture.String(), "hold_id": f.holdID.String(),
				"repo_id": f.repo.String(), "generation": 1, "source_sha": "aaaa1111",
				"coverage_digest": strings.Repeat("a", 64),
			}, true
		case "inspect":
			return inspect(), true
		case "delete-pin":
			f.git.set(f.pinRef, "")
			return inspect(), true
		}
		return nil, false
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if action, isControl := strings.CutPrefix(r.URL.Path, "/fixture/"); isControl {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
			state, known := control(action)
			if !known {
				http.Error(w, "unknown fixture control", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
			return
		}
		f.router.ServeHTTP(w, r)
	}))
	defer server.Close()

	cfg, err := json.Marshal(map[string]string{"url": server.URL, "token": token})
	if err != nil {
		t.Fatal(err)
	}
	cmdCtx, cancel := context.WithTimeout(f.ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "node", "--import", "tsx", "test/fixtures/prerequisite-closure-release-e2e.ts")
	cmd.Dir = filepath.Join(root, "agent")
	cmd.Env = append(os.Environ(), "UZI_CLOSURE_RELEASE_FIXTURE="+string(cfg))
	out, err := cmd.CombinedOutput()
	// Never print the worker token, including on a failing assertion.
	t.Logf("worker fixture: %s", strings.ReplaceAll(string(out), token, "[redacted]"))
	if err != nil {
		t.Fatalf("worker fixture exit: %v", err)
	}
	f.assertReleased()
	if got := f.git.hits.Load(); got != 1 {
		t.Fatalf("expected exactly one forge listing across release and replay, got %d", got)
	}
}
