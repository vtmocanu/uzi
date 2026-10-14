package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const refreshPinned = "25d59524d33d2b74156cad143956d04d9ca509e8"

func refreshRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate worktree")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
}

type refreshQueryCounter struct{ queries atomic.Int64 }

func (c *refreshQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.queries.Add(1)
	return ctx
}
func (c *refreshQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func refreshPool(ctx context.Context, t *testing.T) (*pgxpool.Pool, *refreshQueryCounter) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	counter := &refreshQueryCounter{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, counter
}

func refreshRouter(t *testing.T, pool *pgxpool.Pool, counter *refreshQueryCounter, label string) http.Handler {
	t.Helper()
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{})
	svc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, now: time.Now, startedAt: time.Now(), version: "m3"}
	h.cfg.ActiveSnapshotDisabled = true
	router := h.WorkerRoutes(mw.NewLimiter(10000, time.Minute, nil))
	var mu sync.Mutex
	var heartbeatCycle int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serialize configuration changes and requests, including the real authenticated routes.
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/__enable" {
			h.cfg.ActiveSnapshotDisabled = false
			w.WriteHeader(http.StatusNoContent)
			return
		}
		before := counter.queries.Load()
		start := time.Now()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		handlerUS := time.Since(start).Microseconds()
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(rec.Code)
		if _, err := w.Write(rec.Body.Bytes()); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			heartbeatCycle++
			t.Logf("M3_DB control=%s cycle=%d path=heartbeat queries=%d handler_us=%d request_bytes=%d response_bytes=%d", label, heartbeatCycle, counter.queries.Load()-before, handlerUS, r.ContentLength, rec.Body.Len())
		}
	})
}

// refreshWriteReady confines the environment-provided name to the parent's scratch.
// Exported children derive the checkout from their known refreshExport layout,
// rather than trusting TMPDIR (their runtime.Caller source is the export).
func refreshWriteReady(sourceRoot, ready, address string) error {
	checkout := sourceRoot
	if strings.HasPrefix(filepath.Base(sourceRoot), "refresh-export-") &&
		filepath.Base(filepath.Dir(sourceRoot)) == "scratch" &&
		filepath.Base(filepath.Dir(filepath.Dir(sourceRoot))) == ".uzi" {
		checkout = filepath.Dir(filepath.Dir(filepath.Dir(sourceRoot)))
	}
	scratch := filepath.Join(checkout, ".uzi/scratch")
	realScratch, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return err
	}
	ready = filepath.Clean(ready)
	rel, err := filepath.Rel(realScratch, ready)
	if err != nil {
		return err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if !filepath.IsAbs(ready) || len(parts) != 2 || !strings.HasPrefix(parts[0], "refresh-e2e-") ||
		!strings.HasPrefix(parts[1], "ready-") {
		return errors.New("invalid fixture ready path")
	}
	id := strings.TrimPrefix(parts[1], "ready-")
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return errors.New("invalid fixture ready basename")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(ready))
	if err != nil || parent != filepath.Dir(ready) {
		return errors.New("invalid fixture ready parent")
	}
	// OpenRoot enforces containment during open, including a parent symlink swap.
	// Exclusive creation rejects an existing ready file or leaf symlink.
	root, err := os.OpenRoot(realScratch)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(address)
	return errors.Join(writeErr, file.Close())
}

func TestHeartbeatProtocolRefreshReadyGuard(t *testing.T) {
	checkout := refreshRoot(t)
	if err := os.MkdirAll(filepath.Join(checkout, ".uzi/scratch"), 0700); err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp(filepath.Join(checkout, ".uzi/scratch"), "refresh-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	valid := filepath.Join(scratch, "ready-"+uuid.NewString())
	if err := refreshWriteReady(checkout, valid, "http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	if err := refreshWriteReady(checkout, valid, "overwrite"); err == nil {
		t.Fatal("existing ready file accepted")
	}
	export := filepath.Join(checkout, ".uzi/scratch/refresh-export-fixture")
	if err := refreshWriteReady(export, filepath.Join(scratch, "ready-"+uuid.NewString()), "export child"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(scratch, "ready-invalid"),
		filepath.Join(checkout, "ready-"+uuid.NewString()),
		filepath.Join(checkout, ".uzi/scratch", "ready-"+uuid.NewString()),
	} {
		if err := refreshWriteReady(checkout, path, "invalid"); err == nil {
			t.Fatalf("invalid ready path accepted: %s", path)
		}
	}
	link := filepath.Join(scratch, "ready-"+uuid.NewString())
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if err := refreshWriteReady(checkout, link, "symlink"); err == nil {
		t.Fatal("leaf symlink accepted")
	}
	parentLink := scratch + "-link"
	if err := os.Symlink(checkout, parentLink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parentLink) })
	if err := refreshWriteReady(checkout, filepath.Join(parentLink, "ready-"+uuid.NewString()), "escape"); err == nil {
		t.Fatal("parent symlink escape accepted")
	}
}

// This child is compiled from unmodified pinned or explicitly calibrated scratch source.
// It shares only the parent's throwaway DSN; no capabilities are seeded in workers.
func TestHeartbeatProtocolRefreshServer(t *testing.T) {
	ready := os.Getenv("UZI_REFRESH_SERVER_READY")
	if ready == "" {
		t.Skip("internal child of TestHeartbeatProtocolRefreshE2E")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool, counter := refreshPool(ctx, t)
	done := make(chan struct{})
	var stop sync.Once
	router := refreshRouter(t, pool, counter, os.Getenv("UZI_REFRESH_LABEL"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__stop" {
			stop.Do(func() { close(done) })
			w.WriteHeader(http.StatusNoContent)
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	if err := refreshWriteReady(refreshRoot(t), ready, server.URL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("fixture server was not stopped")
	}
}

func refreshExport(t *testing.T, ctx context.Context, root, ref string) string {
	t.Helper()
	if ref != refreshPinned && ref != "HEAD" {
		t.Fatal("invalid export ref")
	}
	resolved, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", ref+"^{commit}").Output() // #nosec G204 -- fixed git command, runtime.Caller checkout root, only pinned hash or HEAD above; no shell
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(resolved))
	if len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		t.Fatal("invalid export commit")
	}
	dir, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "refresh-export-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	archive := exec.CommandContext(ctx, "git", "-C", root, "archive", sha) // #nosec G204 -- fixed git command, runtime.Caller checkout root, validated 40-hex commit above; no shell
	tar := exec.CommandContext(ctx, "tar", "-x", "-C", dir)                // #nosec G204 -- fixed tar command extracts the validated repository commit into this function’s owned MkdirTemp export; no shell
	reader, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	tar.Stdin = reader
	if err := archive.Start(); err != nil {
		t.Fatal(err)
	}
	tarErr := tar.Run()
	archiveErr := archive.Wait()
	if tarErr != nil || archiveErr != nil {
		t.Fatalf("export: tar=%v archive=%v", tarErr, archiveErr)
	}
	lock, err := os.ReadFile(filepath.Join(dir, "agent/package-lock.json")) // #nosec G304 -- constant lockfile path in this function’s owned MkdirTemp export
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(root, "agent/package-lock.json")) // #nosec G304 -- constant lockfile path under runtime.Caller checkout root
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lock, current) {
		t.Fatal("historical agent lockfile differs: cannot borrow provisioned dependencies")
	}
	if err := os.Symlink(filepath.Join(root, "agent/node_modules"), filepath.Join(dir, "agent/node_modules")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"api/internal/handler/heartbeat_protocol_refresh_e2e_test.go", "agent/test/fixtures/heartbeat-protocol-refresh-e2e.ts"} {
		data, err := os.ReadFile(filepath.Join(root, path)) // #nosec G304 -- only the two constant fixture paths above under runtime.Caller checkout root
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), data, 0600); err != nil { // #nosec G703 -- only the two constant fixture paths above in this function’s owned MkdirTemp export
			t.Fatal(err)
		}
	}
	t.Logf("export ref=%s lockfile=identical harness=portable-test-driver production=unmodified", sha)
	return dir
}

func refreshBinary(t *testing.T, ctx context.Context, dir string, calibrate bool) string {
	t.Helper()
	if calibrate {
		path := filepath.Join(dir, "api/internal/capability/capability.go")
		data, err := os.ReadFile(path) // #nosec G304 -- constant capability.go path in refreshExport-owned scratch export
		if err != nil {
			t.Fatal(err)
		}
		needle := "CompletionInterlockV1:          {},"
		if bytes.Count(data, []byte(needle)) != 1 {
			t.Fatal("vocabulary calibration anchor drifted")
		}
		data = bytes.Replace(data, []byte(needle), []byte(""), 1)
		if err := os.WriteFile(path, data, 0600); err != nil { // #nosec G703 -- constant capability.go path in refreshExport’s owned scratch export, never the checkout
			t.Fatal(err)
		}
		t.Log("variant=vocabulary-calibration omits completion_interlock_v1 from actual protocolVocabulary; protocolOrder unchanged")
	}
	binary := filepath.Join(dir, "refresh-server.test")
	cmd := exec.CommandContext(ctx, "go", "test", "-buildvcs=false", "-c", "-o", binary, "./internal/handler") // #nosec G204 -- fixed go test command; binary is a constant filename in refreshExport-owned scratch export; no shell
	cmd.Dir = filepath.Join(dir, "api")
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("server build: %v\n%s", err, out)
	}
	return binary
}

func refreshChild(t *testing.T, ctx context.Context, binary, scratch, label string) string {
	t.Helper()
	ready := filepath.Join(scratch, "ready-"+uuid.NewString())
	cmd := exec.CommandContext(ctx, binary, "-test.v", "-test.run=^TestHeartbeatProtocolRefreshServer$")
	cmd.Env = append(os.Environ(), "TMPDIR="+scratch, "UZI_REFRESH_SERVER_READY="+ready, "UZI_REFRESH_LABEL="+label)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var address string
	t.Cleanup(func() {
		if address != "" {
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Post(address+"/__stop", "application/json", nil)
			if err != nil {
				t.Errorf("stop child: %v", err)
				_ = cmd.Process.Kill()
			} else {
				_ = response.Body.Close()
			}
		} else {
			_ = cmd.Process.Kill()
		}
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		var err error
		select {
		case err = <-wait:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			err = <-wait
			t.Error("child shutdown deadline exceeded")
		}
		t.Logf("child output:\n%s", output.String())
		if err != nil {
			t.Errorf("child exit: %v", err)
		}
	})
	// Bounded readiness: 600 attempts at 100ms. A failed child cannot block sibling cases forever.
	for n := 0; n < 600; n++ {
		data, err := os.ReadFile(ready) // #nosec G304 -- parent-generated ready-UUID in its owned MkdirTemp scratch, not the child environment value
		if err == nil {
			address = string(data)
			break
		}
		select {
		case <-ctx.Done():
			n = 600
		case <-time.After(100 * time.Millisecond):
		}
	}
	if address == "" {
		t.Fatal("server readiness deadline exceeded")
	}
	return address
}

func TestHeartbeatProtocolRefreshE2E(t *testing.T) {
	if os.Getenv("UZI_HEARTBEAT_PROTOCOL_REFRESH_E2E") != "1" {
		t.Skip("opt in with UZI_HEARTBEAT_PROTOCOL_REFRESH_E2E=1 ./e2e/run-store-it.sh")
	}
	root := refreshRoot(t)
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("combined proof requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("combined proof requires provisioned agent tsx")
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Fatal("combined proof requires throwaway UZI_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	scratch, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "refresh-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Error(err)
		}
	})
	// Each export has no Git metadata. Only the real checkout resolves refs.
	pinned := refreshExport(t, ctx, root, refreshPinned)
	pinnedPlain := refreshBinary(t, ctx, pinned, false)
	pinnedCalibration := refreshExport(t, ctx, root, refreshPinned)
	pinnedCalibrated := refreshBinary(t, ctx, pinnedCalibration, true)
	for _, tc := range []struct {
		name, client, old string
		fail              bool
	}{
		{"client_feature_adoption", pinned, pinnedPlain, true},
		{"client_feature_adoption", root, pinnedPlain, false},
		{"server_capability_claim_restoration", pinned, pinnedCalibrated, true},
		{"server_capability_claim_restoration", root, pinnedCalibrated, false},
		{"old_worker_new_api", pinned, pinnedPlain, false},
		{"new_worker_old_api", root, pinnedPlain, false},
	} {
		label := tc.name + "/candidate"
		if tc.client == pinned {
			label = tc.name + "/pinned"
		}
		t.Run(label, func(t *testing.T) {
			f := newSkewFixture(ctx, t)
			mustExecT(ctx, t, f.pool, "UPDATE runs SET completion_contract_version=1 WHERE id=$1", f.runID)
			pool, counter := refreshPool(ctx, t)
			server := httptest.NewServer(refreshRouter(t, pool, counter, label+"/current-recognizer"))
			defer server.Close()
			oldLabel := label + "/pinned-old"
			if tc.name == "server_capability_claim_restoration" {
				oldLabel = label + "/pinned-vocabulary-calibration-old"
			}
			old := refreshChild(t, ctx, tc.old, scratch, oldLabel)
			endpoint := server.URL
			if tc.fail && tc.name == "server_capability_claim_restoration" {
				endpoint = refreshChild(t, ctx, pinnedPlain, scratch, label+"/pinned-recognizer")
			}
			statePath := filepath.Join(scratch, "state-"+uuid.NewString()+".json")
			cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "--test", "--test-reporter=tap", "--test-timeout=120000", "test/fixtures/heartbeat-protocol-refresh-e2e.ts")
			cmd.Dir = filepath.Join(tc.client, "agent")
			cmd.Env = append(os.Environ(), "TMPDIR="+scratch, "UZI_REFRESH_CASE="+tc.name, "UZI_REFRESH_STATE="+statePath,
				"UZI_REFRESH_CLIENT="+filepath.Join(tc.client, "agent/src/client.ts"),
				"UZI_REFRESH_URL="+endpoint, "UZI_REFRESH_OLD_URL="+old, "UZI_REFRESH_TOKEN="+f.token, "UZI_REFRESH_RUN="+f.runID.String())
			out, err := cmd.CombinedOutput()
			t.Logf("named Node control:\n%s", out)
			output := string(out)
			if strings.Contains(output, "# skipped 1") || strings.Contains(output, "# cancelled 1") {
				t.Fatal("control skipped or cancelled")
			}
			if tc.fail {
				expected := "feature X must be used by tick 2"
				if tc.name == "server_capability_claim_restoration" {
					expected = "same queued run must become claimable by tick 2"
				}
				if err == nil || !strings.Contains(output, expected) || !strings.Contains(output, "ERR_ASSERTION") ||
					!strings.Contains(output, "\n# pass 0\n") || !strings.Contains(output, "\n# fail 1\n") {
					t.Fatalf("pinned control must fail at named public-wire assertion: %v", err)
				}
			} else if err != nil || !strings.Contains(output, "\n# pass 1\n") || !strings.Contains(output, "\n# fail 0\n") {
				t.Fatalf("candidate/control did not pass: %v", err)
			}
			if !strings.Contains(output, "\n# tests 1\n") || !strings.Contains(output, "\n# skipped 0\n") || !strings.Contains(output, "\n# cancelled 0\n") || !strings.Contains(output, tc.name) {
				t.Fatal("missing named zero-skip/cancel control")
			}
			stateBytes, err := os.ReadFile(statePath) // #nosec G304 -- parent-generated state-UUID.json in its owned MkdirTemp scratch
			if err != nil {
				t.Fatalf("read Node registration state: %v", err)
			}
			var state struct {
				WorkerID string `json:"worker_id"`
				Nonce    string `json:"register_nonce"`
			}
			if err := json.Unmarshal(stateBytes, &state); err != nil {
				t.Fatal(err)
			}
			if state.WorkerID == "" || state.Nonce == "" {
				t.Fatal("incomplete Node registration state")
			}
			var caps []string
			var runs, slots int32
			var workerID uuid.UUID
			var nonce string
			var epoch int64
			if err := f.pool.QueryRow(ctx, "SELECT id,snapshot_register_nonce,snapshot_epoch,protocol_capabilities,max_concurrent_runs,max_cross_check_slots FROM workers WHERE user_id=$1", f.owner).Scan(&workerID, &nonce, &epoch, &caps, &runs, &slots); err != nil {
				t.Fatal(err)
			}
			if runs != 4 || slots != 2 {
				t.Fatalf("registration limits changed: %d/%d", runs, slots)
			}
			if workerID.String() != state.WorkerID || nonce != state.Nonce {
				t.Fatal("stored worker identity or incarnation changed after registration")
			}
			wantEpoch := int64(0)
			if tc.name == "client_feature_adoption" && !tc.fail {
				wantEpoch = 2
			}
			if epoch != wantEpoch {
				t.Fatalf("snapshot_epoch=%d want %d", epoch, wantEpoch)
			}
			hasCompletion := false
			for _, cap := range caps {
				if cap == "completion_interlock_v1" {
					hasCompletion = true
				}
			}
			wantCompletion := tc.name != "server_capability_claim_restoration" || !tc.fail
			if hasCompletion != wantCompletion {
				t.Fatalf("stored completion_interlock_v1=%t want %t", hasCompletion, wantCompletion)
			}
			var status string
			var claimedWorker uuid.UUID
			if tc.name == "server_capability_claim_restoration" {
				if err := f.pool.QueryRow(ctx, "SELECT status,COALESCE(worker_id,'00000000-0000-0000-0000-000000000000'::uuid) FROM runs WHERE id=$1", f.runID).Scan(&status, &claimedWorker); err != nil {
					t.Fatal(err)
				}
				if tc.fail && (status != "queued" || claimedWorker != uuid.Nil) {
					t.Fatalf("same queued run must remain unclaimable: status=%s", status)
				}
				if !tc.fail && (status != "claimed" || claimedWorker != workerID) {
					t.Fatalf("same queued run must become claimable by tick 2: status=%s", status)
				}
			}
			t.Logf("M3_DB identity=unchanged final_caps=%v limits=%d/%d snapshot_epoch=%d", caps, runs, slots, epoch)
		})
	}
}
