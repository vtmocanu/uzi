package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const restoreFrozen = "a07f813708ced206cd7b8c3f9d09e259b2fff64b"

// TestRecoveryRestoreWorkflowLiveDB is serial: both packages share one owned DB.
// Substitutions: fixture identities/config, loopback forge discovery, and native
// ancestry in place of a hosted forge API. Persistence, broker, storage, owner
// bearer authentication, CLI selection/download/write and Git import are real.
func TestRecoveryRestoreWorkflowLiveDB(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, ".uzi", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(scratch, "restore-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained_artifacts=%s", tmp)
	t.Setenv("TMPDIR", tmp)
	full := os.Getenv("UZI_TEST_RECOVERY_RESTORE_FULL_HISTORY_SOURCE")
	if full != "" || restoreFullSelected() {
		setup := restoreSetupFor(root, tmp, full, true)
		setup.Node = restoreClientSelected()
		if err := restorePreflight(setup); err != nil {
			t.Fatal(err)
		}
	}
	// Validate explicitly requested full input before any live-DB skip.
	if full != "" {
		if !filepath.IsAbs(full) {
			t.Fatal("full-history source must be absolute")
		}
		if restoreGit(t, tmp, full, "rev-parse", "HEAD") != restoreFrozen ||
			restoreGit(t, tmp, full, "rev-parse", "--is-shallow-repository") != "false" ||
			restoreGit(t, tmp, full, "rev-list", "--count", "HEAD") != "2405" {
			t.Fatal("full-history source differs from frozen nonshallow 2405-commit input")
		}
		restoreGit(t, tmp, full, "fsck", "--full", "--no-reflogs")
	}
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		if full != "" {
			t.Fatal("full history requested without owned UZI_TEST_DATABASE_URL")
		}
		t.Skip("owned UZI_TEST_DATABASE_URL required")
	}
	binary := filepath.Join(tmp, "restore-driver")
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the pinned Go tool must be available on PATH")
	}
	buildStarted := time.Now()
	out, err := restoreCommand(tmp, filepath.Join(root, "api"), goTool, "test", "-buildvcs=false", "-c", "-vet=off", "-o", binary, "./cmd/uzi")
	if err != nil {
		t.Fatalf("compile test driver: %v (output bytes %d)", err, len(out))
	}
	t.Logf("CLI_build=%s (outside owner restore clock)", time.Since(buildStarted))
	t.Run("SmallNative", func(t *testing.T) { restoreWorkflow(t, tmp, binary, "") })
	if full != "" {
		t.Run("E5FullHistory", func(t *testing.T) { restoreWorkflow(t, tmp, binary, full) })
		t.Run("E5ReleasedFullHistory", func(t *testing.T) { restoreWorkflow(t, tmp, binary, full, "released") })
		t.Run("E5BeforePinRefusalFullHistory", func(t *testing.T) { restoreWorkflow(t, tmp, binary, full, "refusal") })
	}
}

func restoreWorkflow(t *testing.T, tmp, binary, full string, modes ...string) {
	node := (full != "" && len(modes) > 0) || (len(modes) > 1 && modes[1] == "workerclient")
	mode := ""
	if len(modes) != 0 {
		mode = modes[0]
	}
	started := time.Now()
	e := newPrerequisiteClosureEnv(t)
	dir, err := os.MkdirTemp(tmp, "case-")
	if err != nil {
		t.Fatal(err)
	}
	work, bare, control := filepath.Join(dir, "work"), filepath.Join(dir, "forge.git"), filepath.Join(dir, "control.git")
	if full == "" {
		restoreGit(t, tmp, dir, "init", "--initial-branch=main", work)
	} else {
		restoreGit(t, tmp, dir, "clone", "--no-local", "--no-hardlinks", full, work)
		restoreGit(t, tmp, work, "checkout", "-B", "main", restoreFrozen)
	}
	commit := func(name, content string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		restoreGit(t, tmp, work, "add", "--", name)
		restoreGit(t, tmp, work, "commit", "-m", name)
		return restoreGit(t, tmp, work, "rev-parse", "HEAD")
	}
	prerequisite := ""
	if full == "" {
		prerequisite = commit("base.txt", "prerequisite\n")
	} else {
		prerequisite = restoreFrozen
	}
	restoreGit(t, tmp, work, "branch", "captured", prerequisite)
	tip := commit("default.txt", "default descendant\n")
	restoreGit(t, tmp, dir, "init", "--bare", "--initial-branch=main", bare)
	restoreGit(t, tmp, work, "push", bare, "main")
	restoreGit(t, tmp, dir, "clone", "--bare", "--no-local", "--no-hardlinks", bare, control)
	restoreGit(t, tmp, work, "checkout", "captured")
	generated := "recovery generated source\n"
	source := commit("recovery-generated.txt", generated)
	tree := restoreGit(t, tmp, work, "rev-parse", "HEAD^{tree}")
	bundle := filepath.Join(dir, "source.bundle")
	restoreGit(t, tmp, work, "branch", "recovered-source", source)
	restoreGit(t, tmp, work, "bundle", "create", bundle, "refs/heads/recovered-source", "^"+prerequisite)
	data, err := os.ReadFile(bundle) // #nosec G304 -- Bundle was generated by this fixture inside its owned scratch directory.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("\n-"+prerequisite+" ")) || !bytes.Contains(data, []byte(source+" refs/heads/recovered-source\n")) {
		t.Fatal("bundle header does not name exact prerequisite and advertised source")
	}
	restoreGit(t, tmp, bare, "config", "http.receivepack", "true")
	traffic := new(restoreTraffic)
	remote, transferred := restoreRemote(t, tmp, dir, traffic)
	sealed, err := e.box.Seal([]byte("fixture-credential"))
	if err != nil {
		t.Fatal(err)
	}
	caps := []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1, capability.RecoveryPrerequisiteClosureV1, capability.RecoveryHeldPublicationV2}
	e.mustExec("UPDATE forge_connections SET base_url=$2,token_ciphertext=$3 WHERE user_id=$1", e.user, "https://github.com", sealed)
	e.mustExec("UPDATE repos SET path_with_namespace='forge',web_url=$2 WHERE id=$1", e.repo, "https://github.com/fixture/forge")
	e.mustExec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.worker.ID, caps)
	e.worker.ProtocolCapabilities = caps
	e.mustExec("UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", e.run, e.worker.ID)
	h := e.handler()
	h.cfg.PrerequisiteClosureForges = []string{"github"}
	h.wsvc.SetTxBeginner(e.pool)
	h.wsvc.SetForgeBaseURLAllowed(func(s string) bool { return s == remote.URL || (mode != "" && s == "https://github.com") })
	h.wsvc.SetForges(pinForgeBuilder{f: &restoreForge{tmp: tmp, work: work, project: forge.Project{ForgeProjectID: 1, PathWithNamespace: "forge", WebURL: remote.URL + "/forge", DefaultBranch: "main"}}})
	svc := h.RecoveryService()
	// Drive the scheduler synchronously so upload cannot race the measured sweep.
	svc.SetPrerequisiteClosure([]string{"github"}, nil)
	gen := int64(1)
	res, err := svc.Reserve(e.ctx, e.worker, e.run, apitypes.RecoveryReserveRequest{RunID: e.run.String(), IdempotencyKey: "restore", SourceSha: source, CoverageDigest: strings.Repeat("a", 64), Generation: &gen})
	if err != nil {
		t.Fatal(err)
	}
	capture := uuid.MustParse(res.CaptureID)
	m := manifestFor(data)
	m.PrerequisiteShas = []string{prerequisite}
	if _, err := svc.Upload(e.ctx, e.worker, e.run, capture, m, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if mode == "" {
		e.mustExec("UPDATE forge_connections SET base_url=$2 WHERE user_id=$1", e.user, remote.URL)
	}
	e.mustExec("UPDATE repos SET web_url=$2 WHERE id=$1", e.repo, remote.URL+"/forge")
	rewrite := func() string {
		restoreGit(t, tmp, work, "checkout", "--orphan", "replacement")
		restoreGit(t, tmp, work, "rm", "-rf", ".")
		replacement := commit("replacement.txt", "unrelated\n")
		restoreGit(t, tmp, work, "update-ref", "refs/heads/main", replacement)
		// Independent repos created here; neither failure is ignored. All commands have restoreCommand's two-minute bound.
		for _, repo := range []string{bare, control} {
			restoreGit(t, tmp, work, "push", repo, "HEAD:refs/heads/main", "--force")
			refs := strings.Fields(restoreGit(t, tmp, repo, "for-each-ref", "--format=%(refname)"))
			expected := []string{"refs/heads/main"}
			if repo == bare && mode != "refusal" {
				expected = append(expected, "refs/uzi-prereq/"+capture.String())
			}
			if strings.Join(refs, "\n") != strings.Join(expected, "\n") {
				t.Fatal("unexpected fixture-owned reachability before prune")
			}
			restoreGit(t, tmp, repo, "reflog", "expire", "--expire=now", "--all")
			if restoreGit(t, tmp, repo, "reflog", "show", "--all", "--format=%H") != "" {
				t.Fatal("fixture reflogs still retain history before prune")
			}
			restoreGit(t, tmp, repo, "gc", "--prune=now")
		}
		if restoreGit(t, tmp, control, "rev-parse", "main") != replacement {
			t.Fatal("control rewrite failed")
		}
		if _, err := restoreCommand(tmp, control, "git", "cat-file", "-e", prerequisite); err == nil {
			t.Fatal("unpinned control retained prerequisite")
		}
		return replacement
	}
	var refusalPrune time.Duration
	if mode == "refusal" {
		pruneStart := time.Now()
		rewrite()
		refusalPrune = time.Since(pruneStart)
	}
	if n, err := h.wsvc.SweepPrerequisiteClosures(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep changed=%d error=%v", n, err)
	}
	row, err := store.New(e.pool).GetPrerequisiteClosureForCapture(e.ctx, capture)
	pin := "refs/uzi-prereq/" + capture.String()
	if mode == "refusal" {
		if err != nil || row.State != "refused" || row.Reason != "prerequisite_not_on_default" || row.NormalizedBaseUrl.Valid {
			t.Fatalf("before-pin refusal: state=%s reason=%s binding_valid=%t error=%v", row.State, row.Reason, row.NormalizedBaseUrl.Valid, err)
		}
		if traffic.creates.Load() != 0 || traffic.receivePosts.Load() != 0 {
			t.Fatal("refused closure sent native pin commands")
		}
		finalStart := time.Now()
		restoreFinal(t, e, h, capture, row.ID, source, pin, tip, bare, tmp, traffic, node, true)
		finalTime := time.Since(finalStart)
		consumer := filepath.Join(dir, "refused-consumer")
		restoreGit(t, tmp, dir, "clone", "--no-local", "--no-hardlinks", remote.URL+"/forge.git", consumer)
		restoreMissing(t, tmp, consumer, bundle, prerequisite)
		if _, err := restoreCommand(tmp, consumer, "git", "cat-file", "-e", prerequisite); err == nil {
			t.Fatal("refusal consumer has prerequisite")
		}
		if _, err := os.Stat(filepath.Join(consumer, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
			t.Fatal("refusal consumer alternates")
		}
		if restoreGit(t, tmp, work, "rev-parse", "recovered-source") != source {
			t.Fatal("refusal lost source")
		}
		if restoreGit(t, tmp, work, "rev-parse", "recovered-source^{tree}") != tree || restoreGit(t, tmp, work, "show", "recovered-source:recovery-generated.txt") != strings.TrimSpace(generated) {
			t.Fatal("refusal did not retain original source tree/content")
		}
		if _, err := restoreCommand(tmp, bare, "git", "show-ref", "--verify", pin); err == nil {
			t.Fatal("refusal unexpectedly created a native pin")
		}
		t.Logf("before-pin refusal retained capture and source; native listings=%d receive_posts=%d creates=%d", traffic.listings.Load(), traffic.receivePosts.Load(), traffic.creates.Load())
		t.Logf("source_frozen=%s source_count=%s archive_bytes=%d generated_bytes=%d setup_including_prune=%s prune=%s FINAL=%s wall=%s full=%t owner_restore=not_applicable", restoreFrozen, restoreGit(t, tmp, work, "rev-list", "--count", source), len(data), len(generated), time.Since(started)-finalTime, refusalPrune, finalTime, time.Since(started), full != "")
		return
	}

	if err != nil || row.State != "pinned" || row.PinRef != pin || row.PinTip.String != tip {
		t.Fatalf("persisted closure not pinned: state=%s error=%v", row.State, err)
	}
	if restoreGit(t, tmp, bare, "rev-parse", pin) != tip {
		t.Fatal("native fixed ref differs from persisted tip")
	}
	setupTime := time.Since(started)
	if mode != "" {
		if !row.NormalizedBaseUrl.Valid || row.NormalizedBaseUrl.String != "https://github.com" {
			t.Fatal("positive frozen GitHub binding missing")
		}
		restoreFinal(t, e, h, capture, row.ID, source, pin, tip, bare, tmp, traffic, node, false)
		after, err := store.New(e.pool).GetPrerequisiteClosureForCapture(e.ctx, capture)
		if err != nil || after.NormalizedBaseUrl != row.NormalizedBaseUrl {
			t.Fatal("release changed frozen binding")
		}
	}
	pruneStart := time.Now()
	replacement := rewrite()
	pruneTime := time.Since(pruneStart)
	cloneStart := time.Now()
	consumer := filepath.Join(dir, "consumer")
	restoreGit(t, tmp, dir, "clone", "--no-local", "--no-hardlinks", remote.URL+"/forge.git", consumer)
	if _, err := os.Stat(filepath.Join(consumer, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("consumer has alternates or inspection failed")
	}
	cloneTime := time.Since(cloneStart)
	restoreMissing(t, tmp, consumer, bundle, prerequisite)
	if _, err := restoreCommand(tmp, consumer, "git", "cat-file", "-e", prerequisite); err == nil {
		t.Fatal("fresh consumer retained old prerequisite")
	}
	snapshot := func() string {
		t.Helper()
		var facts string
		if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_array(to_jsonb(c),to_jsonb(p),to_jsonb(h))::text
 FROM recovery_captures c JOIN recovery_prerequisite_closures p ON p.capture_id=c.id
 JOIN recovery_custody_holds h ON h.id=c.hold_id WHERE c.id=$1`, capture).Scan(&facts); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	before := snapshot()
	ownerHandler, router, _ := cliLiveDB(t)
	jwtSecret := ownerHandler.cfg.JWTSecret
	ownerHandler.cfg = recoveryTestCfg()
	ownerHandler.cfg.JWTSecret = jwtSecret
	ownerHandler.box = e.box
	token := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	sentinel := "inert-" + "inherited-restore-sentinel"
	for _, key := range []string{"UZI_TOKEN", "UZI_CONTEXT", "UZI_URL", "UZI_RESTORE_SENTINEL"} {
		t.Setenv(key, sentinel)
	}
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(fmt.Sprint(r.Header), sentinel) || strings.Contains(r.URL.String(), sentinel) {
			t.Error("inherited sentinel reached API")
			http.Error(w, "sentinel", 400)
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(apiServer.Close)
	api := apiServer.URL
	if mode != "" {
		restoreOwnerIsolation(t, e, tmp, dir, binary, api, capture)
	}
	config := uzicli.NewStore(filepath.Join(dir, "config"))
	if err := config.SaveConfig(&uzicli.Config{Current: "default", Contexts: map[string]uzicli.Context{"default": {URL: api}}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCredentials(&uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: token}}}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, api+"/api/runs/"+e.run.String()+"/archives", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var summary apitypes.RecoveryArchiveSummaryDTO
	err = json.NewDecoder(resp.Body).Decode(&summary)
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("authenticated owner summary: status=%d error=%v", resp.StatusCode, err)
	}
	var projection apitypes.RecoveryArchiveDTO
	for _, archive := range summary.Archives {
		if archive.ID == capture.String() {
			projection = archive
		}
	}
	cliEnv := append(restoreEnv(tmp), "RECOVERY_RESTORE_DRIVER=1", "RECOVERY_RESTORE_CONFIG="+config.Dir(), "RECOVERY_RESTORE_SKILL_HOME="+filepath.Join(dir, "skills"))
	versionCmd := exec.CommandContext(context.Background(), binary, "-test.run=^TestRecoveryRestoreDriver$", "--", "version", "--json")
	versionCmd.Dir, versionCmd.Env = dir, cliEnv
	versionOutput, err := versionCmd.CombinedOutput()
	var cliVersion struct {
		Version string `json:"version"`
	}
	if err != nil || json.Unmarshal(versionOutput, &cliVersion) != nil || cliVersion.Version == "" {
		t.Fatal("test driver did not emit its CLI version")
	}
	output := filepath.Join(dir, "export.bundle")
	exportStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestRecoveryRestoreDriver$", "--", "run", "export", e.run.String(), "--capture", capture.String(), "--output", output) // #nosec G204 -- Compiled fixture driver, fixture UUIDs and owned output path; no shell or production configuration.
	cmd.Dir = dir
	cmd.Env = cliEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI export: %v (sanitized output bytes=%d)", err, len(out))
	}
	exportTime := time.Since(exportStart)
	got, err := os.ReadFile(output) // #nosec G304 -- The explicit export destination is owned by this fixture.
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("CLI export bytes differ")
	}
	beforeTransfer := transferred.Load()
	fetchTime, importTime, err := restoreOwnerImport(tmp, consumer, output, capture.String(), projection)
	if err != nil {
		t.Fatal(err)
	}
	ownerTime := time.Since(exportStart)
	ownerTransferred := transferred.Load() - beforeTransfer
	if ownerTransferred <= 0 || len(data) == 0 {
		t.Fatal("archive and upload-pack response bytes must be positive")
	}
	if restoreGit(t, tmp, consumer, "rev-parse", "recovered-source") != source || restoreGit(t, tmp, consumer, "rev-parse", "recovered-source^{tree}") != tree || restoreGit(t, tmp, consumer, "show", "recovered-source:recovery-generated.txt") != strings.TrimSpace(generated) {
		t.Fatal("recovered source differs")
	}
	for _, mode := range []string{"missing_metadata", "missing_ref", "missing_tip", "foreign_capture", "foreign_ref", "foreign_tip"} {
		t.Run(mode, func(t *testing.T) {
			bad := projection
			if projection.PrerequisiteClosure == nil {
				t.Fatal("missing owner closure")
			}
			closure := *projection.PrerequisiteClosure
			bad.PrerequisiteClosure = &closure
			switch mode {
			case "missing_metadata":
				bad.PrerequisiteClosure = nil
			case "missing_ref":
				closure.PinRef = ""
			case "missing_tip":
				closure.PinTip = nil
			case "foreign_capture":
				bad.ID = uuid.NewString()
			case "foreign_ref":
				closure.PinRef = "refs/uzi-prereq/" + uuid.NewString()
			case "foreign_tip":
				closure.PinTip = &replacement
			}
			if _, _, err := restoreOwnerImport(tmp, consumer, output, capture.String(), bad); err == nil {
				t.Fatal("control unexpectedly imported")
			} else {
				t.Logf("rejected: %v; archive retained=%s", err, output)
			}
		})
	}
	for _, tc := range []struct{ name, origin, ref string }{{"missing_ref", "origin", "refs/uzi-prereq/" + uuid.NewString()}, {"unavailable_history", remote.URL + "/control.git", pin}} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := restoreCommand(tmp, consumer, "git", "fetch", tc.origin, tc.ref+":"+tc.ref)
			if err == nil {
				t.Fatal("missing native ref unexpectedly fetched")
			}
			t.Logf("git fetch %s %s: %v: %s", tc.origin, tc.ref, err, out)
			restoreMissing(t, tmp, control, output, prerequisite)
		})
	}
	if snapshot() != before || restoreGit(t, tmp, bare, "rev-parse", pin) != tip || restoreGit(t, tmp, work, "rev-parse", "recovered-source") != source {
		t.Fatal("display/export/import changed durable facts or fixture pin/source")
	}
	if mode == "" {
		t.Run("expired_download", func(t *testing.T) {
			e.mustExec("UPDATE recovery_captures SET state='expired', expires_at=now()-interval '1 second' WHERE id=$1", capture)
			req, err := http.NewRequest(http.MethodGet, api+"/api/runs/"+e.run.String()+"/archives/"+capture.String()+"/download", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var failure struct {
				Reason string `json:"reason"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&failure)
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusConflict || decodeErr != nil || failure.Reason != "not_available" {
				t.Fatalf("expired download status=%d", resp.StatusCode)
			}
			t.Logf("expired download rejected status=%d", resp.StatusCode)
		})
	}
	retained, err := os.ReadFile(output) // #nosec G304 -- Re-read only the fixture-owned export destination to prove retention.
	if err != nil || !bytes.Equal(retained, data) {
		t.Fatal("controls did not retain exported archive")
	}
	t.Logf("source_frozen=%s source_count=%s source_SHA=%s tree=%s CLI_version=%s go=%s GOOS=%s GOARCH=%s git=%s", restoreFrozen, restoreGit(t, tmp, work, "rev-list", "--count", source), source, tree, cliVersion.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, restoreGit(t, tmp, dir, "--version"))
	t.Logf("archive_bytes=%d HTTP_upload_pack_response_body_bytes=%d generated_bytes=%d generated_sha256=%x archive_sha256=%x export=%s fetch=%s verify_import=%s owner_total=%s wall=%s setup=%s clone=%s prune=%s full=%t transport=owned-loopback-smartHTTP cache=warm-build-local-Git-no-alternates queue=none limits=single-worktree-not-hosted-latency", len(data), ownerTransferred, len(generated), sha256.Sum256([]byte(generated)), sha256.Sum256(data), exportTime, fetchTime, importTime, ownerTime, time.Since(started), setupTime, cloneTime, pruneTime, full != "")
}

// Owner workflow validates capture-bound metadata before importing any archive.
func restoreOwnerImport(tmp, consumer, archive, capture string, a apitypes.RecoveryArchiveDTO) (time.Duration, time.Duration, error) {
	c := a.PrerequisiteClosure
	if a.ID != capture || c == nil || c.State != "pinned" || c.PinRef != "refs/uzi-prereq/"+capture || c.PinTip == nil || *c.PinTip == "" {
		return 0, 0, fmt.Errorf("capture-bound pin metadata missing or foreign")
	}
	start := time.Now()
	out, err := restoreCommand(tmp, consumer, "git", "fetch", "origin", c.PinRef+":"+c.PinRef)
	fetch := time.Since(start)
	if err != nil {
		return fetch, 0, fmt.Errorf("fetch pin: %w: %s", err, out)
	}
	start = time.Now()
	out, err = restoreCommand(tmp, consumer, "git", "rev-parse", c.PinRef)
	if err != nil || strings.TrimSpace(string(out)) != *c.PinTip {
		return fetch, time.Since(start), fmt.Errorf("owner pin_tip mismatch")
	}
	for _, args := range [][]string{{"bundle", "verify", archive}, {"fetch", archive, "refs/heads/recovered-source:refs/heads/recovered-source"}} {
		out, err = restoreCommand(tmp, consumer, "git", args...)
		if err != nil {
			return fetch, time.Since(start), fmt.Errorf("verify/import: %w: %s", err, out)
		}
	}
	return fetch, time.Since(start), nil
}
