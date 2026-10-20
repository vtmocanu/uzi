package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type ciRepairBlobRT func(*http.Request) (*http.Response, error)

func (f ciRepairBlobRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type ciRepairReport struct {
	Generation         int64  `json:"generation"`
	RunID              string `json:"run_id"`
	ManifestID         string `json:"manifest_id"`
	SHA256             string `json:"sha256"`
	Size               int64  `json:"size"`
	Filemode           int    `json:"filemode"`
	Downloads          int    `json:"downloads"`
	OutsideClone       bool   `json:"outsideClone"`
	NoInputCloneWrites bool   `json:"noInputCloneWrites"`
}

// TestCIArtifactsRepairE2E connects real GitHub collection, PostgreSQL admission,
// worker HTTP claims/downloads and the shared production-reader harness fixture.
// It uses scripted provider transports; no model credentials or model calls are needed.
func TestCIArtifactsRepairE2E(t *testing.T) {
	if os.Getenv("UZI_CI_ARTIFACTS_E2E") != "1" {
		t.Skip("set UZI_CI_ARTIFACTS_E2E=1 for the PostgreSQL/Node CI artifact proof")
	}
	// Fail before helpers with optional-DB skips can conceal a missing prerequisite.
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Fatal("enabled E2E requires UZI_TEST_DATABASE_URL")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("enabled E2E requires Node: %v", err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, ".uzi", "scratch")
	dir, err := os.MkdirTemp(scratch, "ci-artifacts-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	agent := filepath.Join(root, "agent")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	probe := exec.CommandContext(ctx, node, "--import", "tsx", "-e", "") // #nosec G204 -- fixed Node probe; executable resolved by LookPath, no shell or model arguments.
	probe.Dir = agent
	ciRepairNodeLog(t, probe, filepath.Join(dir, "tsx-probe.log"))
	// Harnesses and modes run sequentially: resume consumes the fresh claim's
	// persisted state and cache. A failed fresh proof blocks only its own resume.
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			ciRepairHarnessE2E(ctx, t, node, agent, dir, harness)
		})
	}
}

func ciRepairNodeLog(t *testing.T, cmd *exec.Cmd, path string) []byte {
	t.Helper()
	log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) // #nosec G304 -- callers supply fixed log basenames inside this test’s private MkdirTemp directory.
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	runErr := cmd.Run()
	closeErr := log.Close()
	data, err := os.ReadFile(path) // #nosec G304 -- same private, caller-owned log created above; never supplied by evidence or model traffic.
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("Node fixture failed: %v; log %s\n%s", runErr, path, data)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return data
}

func ciRepairHarnessE2E(ctx context.Context, t *testing.T, node, agent, scratch, harness string) {
	t.Helper()
	f := newSkewFixture(ctx, t)
	q := store.New(f.pool)
	box := newHandlerTestBox(t)
	var repoID uuid.UUID
	var projectID int64
	if err := f.pool.QueryRow(ctx, `SELECT r.repo_id,p.forge_project_id FROM runs r JOIN repos p ON p.id=r.repo_id WHERE r.id=$1`, f.runID).Scan(&repoID, &projectID); err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, f.pool, "DELETE FROM runs WHERE id=$1", f.runID)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, query := range []string{
			"DELETE FROM recovery_custody_holds WHERE user_id=$1",
			"DELETE FROM runs WHERE user_id=$1",
			"DELETE FROM run_schedules WHERE user_id=$1",
			"DELETE FROM workers WHERE user_id=$1",
			"DELETE FROM users WHERE id=$1",
		} {
			if _, err := f.pool.Exec(cleanup, query, f.owner); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	const content = "CI artifact canary: café\n"
	archive := ciRepairZIP(t, content)
	upstream := ciRepairGitHub(t, projectID, archive)
	defer upstream.Close()
	mustExecT(ctx, t, f.pool, `UPDATE forge_connections SET forge_type='github',base_url=$1 WHERE user_id=$2`, upstream.URL, f.owner)
	mustExecT(ctx, t, f.pool, `UPDATE repos SET path_with_namespace='g/repo' WHERE id=$1`, repoID)
	caps := []string{capability.CIConfigGuardV1, capability.CIArtifactsV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCompletionInterlockV1}
	mustExecT(ctx, t, f.pool, `UPDATE workers SET protocol_capabilities=$1,max_concurrent_runs=1 WHERE user_id=$2`, caps, f.owner)
	if harness == "codex" {
		sealed, err := box.Seal([]byte("openai-fixture-" + uuid.NewString()))
		if err != nil {
			t.Fatal(err)
		}
		secretID := uuid.New()
		mustExecT(ctx, t, f.pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'openai_api_key','e2e',true,$3,'master')`, secretID, f.owner, sealed)
		if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secretID, UserID: f.owner, Status: "static"}); err != nil {
			t.Fatal(err)
		}
	}
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	wsvc.SetTxBeginner(f.pool)
	wsvc.SetJobFiles(workersvc.NewJobFiles(f.pool, box, workersvc.JobFileLimits{}, nil))
	fs := forgesvc.NewWithForgeBuilder(q, box, 10*time.Second, nil, func(kind forge.Type, base, token string, timeout time.Duration) (forge.Forge, error) {
		driver, err := forge.New(kind, base, token, timeout)
		if err != nil {
			return nil, err
		}
		forge.SetGitHubArtifactFixtureForTesting(t, driver, func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != "ci-evidence.example" {
				return nil, fmt.Errorf("unexpected artifact host %q", host)
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}, ciRepairBlobRT(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://ci-evidence.example/archive?signature=fixture" || len(r.Header) != 1 || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				return nil, fmt.Errorf("unexpected blob request or headers")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(archive)), ContentLength: int64(len(archive))}, nil
		}))
		return driver, nil
	})
	schedule, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID: f.owner, RepoID: repoID, Target: "ci_watch", Prompt: pgtype.Text{String: "Repair using CI evidence", Valid: true},
		Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", Enabled: true,
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, f.pool, `UPDATE run_schedules SET origin='user',harness=$1,ci_artifacts='["logs*"]' WHERE id=$2`, harness, schedule.ID)
	schedule, err = q.GetRunSchedule(ctx, schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := schedsvc.New(q, wsvc, fs, nil, nil, nil, time.Minute, nil)
	outcome, err := scheduler.RunNow(ctx, schedule)
	if err != nil || len(outcome.Started) != 1 {
		t.Fatalf("RunNow: %+v err=%v", outcome, err)
	}
	runID := outcome.Started[0].RunID
	var raw []byte
	if err := f.pool.QueryRow(ctx, "SELECT ci_artifact_manifest FROM runs WHERE id=$1", runID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var manifest ciartifacts.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].ManifestID == "" {
		t.Fatalf("manifest: %s", raw)
	}
	entry := manifest.Entries[0]
	digest, storageName := ciartifacts.Identity([]byte(content))
	if entry.SHA256 != digest || entry.Size != int64(len(content)) {
		t.Fatalf("manifest content identity: %+v", entry)
	}
	rows, err := f.pool.Query(ctx, `SELECT f.id,f.state,f.storage_name,f.sha256,f.byte_size,c.sealed FROM job_files f JOIN job_file_chunks c ON c.file_id=f.id WHERE f.run_id=$1 ORDER BY c.chunk_index`, runID)
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	for rows.Next() {
		var id uuid.UUID
		var state, name, hash string
		var size int64
		var sealed []byte
		if err := rows.Scan(&id, &state, &name, &hash, &size, &sealed); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if id.String() != entry.ManifestID || state != "attached" || name != storageName || hash != digest || size != int64(len(content)) || len(sealed) == 0 || bytes.Contains(sealed, []byte(content)) {
			rows.Close()
			t.Fatal("invalid sealed artifact storage")
		}
		chunks++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if chunks == 0 {
		t.Fatal("no encrypted chunks")
	}
	h := &Handler{pool: f.pool, q: q, box: box, wsvc: wsvc, version: "dev", now: time.Now, startedAt: time.Now()}
	server := httptest.NewServer(h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil)))
	defer server.Close()
	dir := filepath.Join(scratch, harness)
	if err := os.MkdirAll(filepath.Join(dir, "clone"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	home, clone := filepath.Join(dir, "home"), filepath.Join(dir, "clone")
	invoke := func(t *testing.T, mode string) ciRepairReport {
		t.Helper()
		config, err := json.Marshal(map[string]any{"url": server.URL, "token": f.token, "harness": harness, "mode": mode, "home": home, "clone": clone, "state": state,
			"expected": map[string]any{"manifest_id": entry.ManifestID, "text": strings.TrimSuffix(content, "\n"), "sha256": digest, "size": len(content)}})
		if err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(dir, mode+".json")
		if err := os.WriteFile(configPath, config, 0600); err != nil {
			t.Fatal(err)
		}
		child, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(child, node, "--import", "tsx", "test/fixtures/ci-artifacts-repair-e2e.ts", configPath) // #nosec G204 -- fixed committed fixture and private generated config, resolved Node executable, no shell.
		cmd.Dir = agent
		ciRepairNodeLog(t, cmd, filepath.Join(dir, mode+".log"))
		data, err := os.ReadFile(state) // #nosec G304 -- fixed state basename in the test-owned private directory, not an artifact-selected path.
		if err != nil {
			t.Fatal(err)
		}
		var report ciRepairReport
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if report.RunID != runID.String() || report.ManifestID != entry.ManifestID || report.SHA256 != digest || report.Size != int64(len(content)) || report.Filemode != 0400 || !report.OutsideClone || !report.NoInputCloneWrites {
			t.Fatalf("reader report: %+v", report)
		}
		return report
	}
	var fresh ciRepairReport
	if !t.Run("fresh", func(t *testing.T) {
		fresh = invoke(t, "fresh")
		if fresh.Generation <= 0 || fresh.Downloads != 1 {
			t.Fatalf("fresh report: %+v", fresh)
		}
	}) {
		return
	}
	t.Run("resume", func(t *testing.T) {
		mustExecT(ctx, t, f.pool, `INSERT INTO run_user_inputs(run_id,kind,body,consumed_at,applied_at,disposition) VALUES($1,'approve_plan','',now(),now(),'applied')`, runID)
		mustExecT(ctx, t, f.pool, `UPDATE runs SET status='queued',worker_id=NULL,claim_released_at=NULL,plan_md='fixture approved plan',session_id='fixture-session',auto_approve=false WHERE id=$1`, runID)
		resumed := invoke(t, "resume")
		if resumed.Generation <= fresh.Generation || resumed.Downloads != 0 || resumed.SHA256 != fresh.SHA256 || resumed.Size != fresh.Size {
			t.Fatalf("resume report: %+v fresh=%+v", resumed, fresh)
		}
		path := fmt.Sprintf("%s/api/worker/runs/%s/files/%s?claim_generation=%d", server.URL, runID, entry.ManifestID, fresh.Generation)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("old generation download: %d, want 409", resp.StatusCode)
		}
	})
}

func ciRepairZIP(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, err := z.Create("evidence.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, text); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func ciRepairGitHub(t *testing.T, projectID int64, archive []byte) *httptest.Server {
	t.Helper()
	sha := strings.Repeat("a", 40)
	repo := map[string]any{"id": projectID, "name": "repo", "full_name": "g/repo", "owner": map[string]any{"login": "g"}}
	run := map[string]any{"id": 100, "workflow_id": 7, "run_attempt": 1, "name": "CI", "status": "completed", "conclusion": "failure", "head_sha": sha, "head_branch": "main", "event": "push",
		"repository": repo, "head_repository": repo, "updated_at": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), "html_url": "https://github.example/g/repo/actions/runs/100"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized fixture request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case fmt.Sprintf("/api/v3/repositories/%d", projectID):
			body = repo
		case "/api/v3/repos/g/repo/actions/runs":
			body = map[string]any{"total_count": 1, "workflow_runs": []any{run}}
		case "/api/v3/repos/g/repo/actions/runs/100":
			body = run
		case "/api/v3/repos/g/repo/actions/runs/100/jobs":
			body = map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 1001, "run_id": 100, "name": "failing test", "status": "completed", "conclusion": "failure", "head_sha": sha}}}
		case "/api/v3/repos/g/repo/actions/jobs/1001/logs":
			http.NotFound(w, r)
			return
		case "/api/v3/repos/g/repo/actions/runs/100/artifacts":
			body = map[string]any{"total_count": 1, "artifacts": []any{map[string]any{"id": 9, "name": "logs-evidence", "size_in_bytes": len(archive), "archive_download_url": "https://metadata.invalid/never",
				"workflow_run": map[string]any{"id": 100, "repository_id": projectID, "head_sha": sha}}}}
		case "/api/v3/repos/g/repo/actions/artifacts/9/zip":
			w.Header().Set("Location", "https://ci-evidence.example/archive?signature=fixture")
			w.WriteHeader(http.StatusFound)
			return
		default:
			t.Errorf("unexpected GitHub request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("GitHub response: %v", err)
		}
	}))
}
