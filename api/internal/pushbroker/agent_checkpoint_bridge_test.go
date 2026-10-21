package pushbroker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// bridgePublish is one publish the loopback server saw: the tip the agent declared and how the
// real broker answered it.
type bridgePublish struct {
	tip     string
	skipped string
	err     string
	result  pushbroker.Result
}

// errBridgeSuperseded stands in for the api's slot refusal (workersvc "superseded"), a 200 skip the
// worker treats as not confirmed. Only the seeded-pause case answers it, to its first tick.
var errBridgeSuperseded = errors.New("bridge: superseded")

// bridgeSkipLabel is production's broker-refusal mapping (workersvc publishOutcome): each sentinel
// is a 200 skip carrying this label, never a 5xx. "" means the error is not a skip sentinel.
func bridgeSkipLabel(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, pushbroker.ErrNotDescendant):
		return "not_descendant"
	case errors.Is(err, pushbroker.ErrWorkflowScopeRejected):
		return "workflow_scope"
	case errors.Is(err, pushbroker.ErrCIConfigGuard):
		return "ci_config_guard"
	case errors.Is(err, pushbroker.ErrTipMissing):
		return "tip_missing"
	case errors.Is(err, pushbroker.ErrPackTooLarge):
		return "pack_too_large"
	case errors.Is(err, pushbroker.ErrPackInvalid):
		return "pack_invalid"
	case errors.Is(err, errBridgeSuperseded):
		return "superseded"
	}
	return ""
}

// bridgeServer is the loopback publish route: it hands each worker publish to publish (the real
// pushbroker.Publish) and answers as production does. Only the publish RPC reaches it; every other
// worker RPC goes to the agent fixture's in-process FakeApi, so this is not worker-route
// authorization coverage. With lostAck the first accepted publish lands but answers 500.
type bridgeServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []bridgePublish
}

func newBridgeServer(t *testing.T, ref string, lostAck bool, publish func(ctx context.Context, tip string, pack []byte) error) *bridgeServer {
	t.Helper()
	b := &bridgeServer{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/worker/runs/") || !strings.HasSuffix(r.URL.Path, "/publish") {
			http.NotFound(w, r)
			return
		}
		tip := r.Header.Get("X-Uzi-Checkpoint-Tip")
		pack, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read pack", http.StatusBadRequest)
			return
		}
		err = publish(r.Context(), tip, pack)
		rec := bridgePublish{tip: tip}
		resp := map[string]any{"published": true, "ref": ref}
		if label := bridgeSkipLabel(err); label != "" {
			rec.skipped = label
			resp = map[string]any{"published": false, "ref": ref, "skipped": label}
		} else if err != nil {
			rec.err = err.Error()
			b.record(rec)
			http.Error(w, "publish failed", http.StatusInternalServerError)
			return
		}
		if first := b.record(rec) == 1; lostAck && first {
			// The publish landed in origin; the agent never sees the acknowledgement.
			http.Error(w, "ack lost", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bridgeServer) record(p bridgePublish) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, p)
	return len(b.seen)
}

func (b *bridgeServer) publishes() []bridgePublish {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bridgePublish(nil), b.seen...)
}

// bridgeRoot locates the repository root and Node, failing (never skipping) once the gate is set.
func bridgeRoot(t *testing.T) (root, node string) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository root")
	}
	root = filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the bridge requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("the bridge requires installed agent dependencies (tsx)")
	}
	return root, node
}

// runBridgeFixture runs agent/test/fixtures/checkpoint-chain-bridge.ts with cfg and returns its
// BRIDGE_SUMMARY line decoded into summary, plus stdout/stderr for failure messages.
func runBridgeFixture(t *testing.T, root, node string, cfg map[string]any, summary any) (out string, runErr error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--import", "tsx", "test/fixtures/checkpoint-chain-bridge.ts")
	cmd.Dir = filepath.Join(root, "agent")
	cmd.WaitDelay = 10 * time.Second
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NODE_TEST_CONTEXT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "UZI_CHAIN_BRIDGE_FIXTURE="+string(raw))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr = cmd.Run()
	out = fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	found := false
	for _, line := range strings.Split(stdout.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "BRIDGE_SUMMARY "); ok {
			if err := json.Unmarshal([]byte(rest), summary); err != nil {
				t.Fatalf("bad summary %q: %v", rest, err)
			}
			found = true
		}
	}
	if !found && runErr == nil {
		t.Fatalf("fixture printed no BRIDGE_SUMMARY\n%s", out)
	}
	return out, runErr
}

// TestAgentCheckpointChainBridge is the issue #2733 binding bridge. It runs the REAL agent
// (agent/test/fixtures/checkpoint-selective-chain-bridge.ts: a real RunRunner + GitCache driving a GitHub
// forge claim through a workflow-overlay checkpoint O and then a raw tick or iteration checkpoint
// N) against the REAL pushbroker.Publish, over the real HTTP route and WorkerClient.publishCheckpoint.
// The broker keeps its depth-1 fetch and pack boundaries; no commit shape is built on the Go side,
// so the agent and the broker cannot drift apart unnoticed (the unit tests on each side use a
// stand-in for the other). It asserts the checkpoint ref chains [O, N] over the overlay and that
// no publish is refused not_descendant, with the overlay acknowledged cleanly and with its
// acknowledgement lost after it landed.
//
// The stale-base family (issue #3242) runs the same real agent through a turn whose tip never moves
// while origin's default branch advances, then publishes via checkpoint-chain-bridge.ts.
// Its broker fetches over real smart HTTP (git http-backend), so the depth-1 base fetch is the one
// every forge serves.
//
// Gated by UZI_CHECKPOINT_CHAIN_BRIDGE=1 (task test:checkpoint-chain-bridge): it needs Node and the
// installed agent dependencies, so the default `go test ./...` skips it. Once the gate is set a
// missing prerequisite is a failure, never a skip.
func TestAgentCheckpointChainBridge(t *testing.T) {
	if os.Getenv("UZI_CHECKPOINT_CHAIN_BRIDGE") != "1" {
		t.Skip("opt in with task test:checkpoint-chain-bridge")
	}
	root, node := bridgeRoot(t)
	iids := map[string]int{"tick": 2733101, "iteration": 2733102}
	for _, boundary := range []string{"tick", "iteration"} {
		for _, lostAck := range []bool{false, true} {
			name := boundary + "/clean"
			if lostAck {
				name = boundary + "/lostack"
			}
			iid := iids[boundary]
			t.Run(name, func(t *testing.T) {
				runAgentCheckpointBridge(t, root, node, boundary, iid, lostAck)
			})
		}
	}
	for i, c := range staleBaseCases {
		t.Run("stale-base/"+c.name, func(t *testing.T) {
			runAgentStaleBaseBridge(t, root, node, c, 3242101+i)
		})
	}
	for i, c := range originSeededCases {
		t.Run("origin-seeded/"+c.name, func(t *testing.T) {
			runAgentOriginSeededBridge(t, root, node, c, 3284101+i)
		})
	}
}

// staleBaseCase is one issue #3242 publication shape. The run starts on main's tip (a linear,
// merge or root commit), origin's main then advances by advance (on the agent side) or, with race,
// in the gap between the worker resolving its range and the broker fetching its base. want is the
// first publish's outcome: "published", or the skip label the real broker must answer.
type staleBaseCase struct {
	name                     string
	shape                    string // "linear" | "merge" | "root"
	advance                  string // "none" | "plain" | "workflow" | "delete"
	commit, descendant, race bool
	lostAck, guard           bool
	seededPause              bool // a seeded claim: unconfirmed first tick, main moves, then a real pause
	want                     string
}

var staleBaseCases = []staleBaseCase{
	{name: "plain", shape: "linear", advance: "plain", want: "published"},
	{name: "workflow", shape: "linear", advance: "workflow", want: "published"},
	{name: "delete", shape: "linear", advance: "delete", want: "published"},
	{name: "unmoved", shape: "linear", advance: "none", want: "published"},
	{name: "race", shape: "linear", advance: "none", race: true, want: "published"},
	{name: "merge-base", shape: "merge", advance: "plain", want: "published"},
	{name: "root-base", shape: "root", advance: "plain", want: "published"},
	{name: "plain-commit", shape: "linear", advance: "plain", commit: true, want: "published"},
	{name: "workflow-commit", shape: "linear", advance: "workflow", commit: true, want: "published"},
	{name: "lostack-retry-descendant", shape: "linear", advance: "plain", lostAck: true, descendant: true, want: "published"},
	// Guarded runs: the guard compares the tip with the fetched default tip at protected paths, so a
	// base behind a workflow change is refused exactly as a committed run behind it is; a plain
	// advance leaves the protected paths equal and publishes (the readable positive control).
	{name: "guarded-plain", shape: "linear", advance: "plain", guard: true, want: "published"},
	{name: "guarded-workflow", shape: "linear", advance: "workflow", guard: true, want: "ci_config_guard"},
	{name: "guarded-workflow-commit", shape: "linear", advance: "workflow", commit: true, guard: true, want: "ci_config_guard"},
	// Issue #3242 / 81c950c7: a seeded run with no commit, its tracking ref set by an unconfirmed tick,
	// pauses after main moved; the pause publish takes the non-pinned zero range and must park.
	{name: "seeded-pause", shape: "linear", advance: "plain", seededPause: true, want: "published"},
}

// originSeededCase is the issue #3284 shape: origin already carries refs/heads/<branch> at a seed
// commit and NO checkpoint ref; the claim is seeded with planned_base_commit = seed and
// require_base_match = true; the run does zero new work. Origin's main advances (advance) after the
// worker resolved its floor. mode "tick" publishes through one tick; mode "pause" first lets a tick
// establish the tracking ref (answered superseded before the broker, as in the seeded-pause idiom),
// so the pause is the first publish the broker sees. want is the broker's outcome for that publish.
type originSeededCase struct {
	name    string
	advance string // "none" | "plain" | "workflow"
	mode    string // "tick" | "pause"
	guard   bool
	want    string // "published" or the skip label
}

var originSeededCases = []originSeededCase{
	{name: "tick-unmoved", advance: "none", mode: "tick", want: "published"},
	{name: "tick-plain", advance: "plain", mode: "tick", want: "published"},
	{name: "tick-workflow", advance: "workflow", mode: "tick", want: "published"},
	{name: "pause-unmoved", advance: "none", mode: "pause", want: "published"},
	{name: "pause-plain", advance: "plain", mode: "pause", want: "published"},
	{name: "pause-workflow", advance: "workflow", mode: "pause", want: "published"},
	{name: "guarded-tick-plain", advance: "plain", mode: "tick", guard: true, want: "published"},
	{name: "guarded-pause-plain", advance: "plain", mode: "pause", guard: true, want: "published"},
	{name: "guarded-tick-workflow", advance: "workflow", mode: "tick", guard: true, want: "ci_config_guard"},
	{name: "guarded-pause-workflow", advance: "workflow", mode: "pause", guard: true, want: "ci_config_guard"},
}

func runAgentOriginSeededBridge(t *testing.T, root, node string, c originSeededCase, iid int) {
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f := newGitFixture(t)
	f.commit("README.md", "bridge\n", "initial")
	if err := os.MkdirAll(filepath.Join(f.work, ".github", "workflows"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.commit(".github/workflows/ci.yml", "name: ci\non: push\njobs: {}\n# base\n", "workflow v1")
	f.pushMain()
	seed := f.commit("seed.txt", "seed\n", "seed")
	branch := fmt.Sprintf("agent/issue-%d", iid)
	ref := "refs/uzi-checkpoints/" + branch
	f.git("push", "origin", seed+":refs/heads/"+branch)
	if f.originRef(ref) != "" {
		t.Fatal("precondition: origin must carry no checkpoint ref")
	}
	url := newGitHTTPRemote(t, f, backend).cloneURL(f)

	var (
		mu    sync.Mutex
		first = true
	)
	srv := newBridgeServer(t, ref, false, func(ctx context.Context, tip string, pack []byte) error {
		mu.Lock()
		superseded := c.mode == "pause" && first
		first = false
		mu.Unlock()
		if superseded {
			return errBridgeSuperseded
		}
		opts := pushbroker.Options{
			CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(),
			Branch: branch, DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
		}
		if c.guard {
			opts.CIConfigGuard, opts.CIConfigPaths = true, []string{".github/workflows/**"}
		}
		_, err := pushbroker.Publish(ctx, opts)
		return err
	})

	var summary struct {
		BaseTip             string `json:"baseTip"`
		MainTip             string `json:"mainTip"`
		ClaimPlannedBase    string `json:"claimPlannedBase"`
		ClaimRequireBase    bool   `json:"claimRequireBase"`
		ClaimCheckpointTip  string `json:"claimCheckpointTip"`
		ClaimPlanSource     string `json:"claimPlanSource"`
		CheckpointRefBefore string `json:"checkpointRefBefore"`
		FirstTickOutcome    string `json:"firstTickOutcome"`
		TickOutcome         string `json:"tickOutcome"`
		Parked              bool   `json:"parked"`
		CheckpointRefAfter  string `json:"checkpointRefAfter"`
		BranchAfter         string `json:"branchAfter"`
	}
	out, runErr := runBridgeFixture(t, root, node, map[string]any{
		"url": srv.srv.URL, "token": "tkn-bridge-123456", "origin": f.bare, "iid": iid, "boundary": "tick",
		"originSeeded": map[string]any{"seed": seed, "advance": c.advance, "mode": c.mode},
		"scratch":      t.TempDir(),
	}, &summary)
	publishes := srv.publishes()
	describe := func() string {
		return fmt.Sprintf("case=%+v seed=%s publishes=%+v summary=%+v\n%s", c, seed, publishes, summary, out)
	}
	if runErr != nil {
		t.Fatalf("agent fixture failed: %v\n%s", runErr, describe())
	}
	// Preconditions: a seeded require-base claim on the seed, no inherited checkpoint, no checkpoint
	// ref, zero new work (the worktree is still the seed).
	if summary.ClaimPlanSource != "seeded" || summary.ClaimPlannedBase != seed || !summary.ClaimRequireBase ||
		summary.ClaimCheckpointTip != "" || summary.CheckpointRefBefore != "" || summary.BaseTip != seed {
		t.Fatalf("claim/origin preconditions not as intended\n%s", describe())
	}
	if c.advance != "none" && (summary.MainTip == "" || f.originRef("refs/heads/main") != summary.MainTip) {
		t.Fatalf("origin main did not advance after the floor resolved\n%s", describe())
	}
	// The publish under test is the only one the broker sees (a tick), or the second (a pause, after
	// the superseded first tick). Every request declares the seed.
	wantPublishes := 1
	if c.mode == "pause" {
		wantPublishes = 2
		if summary.FirstTickOutcome == "published" || len(publishes) == 0 || publishes[0].skipped != "superseded" {
			t.Fatalf("the first tick was not answered superseded before the broker\n%s", describe())
		}
	}
	if len(publishes) != wantPublishes {
		t.Fatalf("publishes = %+v, want %d\n%s", publishes, wantPublishes, describe())
	}
	got := publishes[len(publishes)-1]
	if got.tip != seed {
		t.Fatalf("declared tip %s, want the seed %s\n%s", got.tip, seed, describe())
	}
	if c.want != "published" {
		if got.skipped != c.want || summary.Parked || summary.TickOutcome == "published" || summary.CheckpointRefAfter != "" {
			t.Fatalf("want the broker's %q refusal with no checkpoint ref and no park\n%s", c.want, describe())
		}
		return
	}
	if got.skipped != "" || got.err != "" {
		t.Fatalf("the first publication was refused: skipped=%q err=%q\n%s", got.skipped, got.err, describe())
	}
	if c.mode == "tick" && summary.TickOutcome != "published" {
		t.Fatalf("tick outcome = %q, want published\n%s", summary.TickOutcome, describe())
	}
	if c.mode == "pause" && !summary.Parked {
		t.Fatalf("the pause did not park\n%s", describe())
	}
	if ck := summary.CheckpointRefAfter; ck != seed {
		t.Fatalf("checkpoint ref = %s, want the seed %s\n%s", ck, seed, describe())
	}
	// Read inside the turn: the run's own finish may push the branch afterwards.
	if b := summary.BranchAfter; b != seed {
		t.Fatalf("origin branch was %s at the checkpoint, want the seed %s\n%s", b, seed, describe())
	}
}

func runAgentCheckpointBridge(t *testing.T, root, node, boundary string, iid int, lostAck bool) {
	f := newGitFixture(t)
	f.commit("README.md", "bridge\n", "initial")
	if err := os.MkdirAll(filepath.Join(f.work, ".github", "workflows"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.commit(".github/workflows/ci.yml", "name: ci\non: push\njobs: {}\n# base\n", "workflow v1")
	f.pushMain()
	branch := fmt.Sprintf("agent/issue-%d", iid)
	ref := "refs/uzi-checkpoints/" + branch
	// The origin branch pins the old floor; default gains then deletes a bounded historical blob.
	run(t, "", "git", "-C", f.work, "push", "origin", "HEAD:refs/heads/"+branch)
	f.commit("historical.txt", "historical default content\n", "historical default blob")
	historicalBlob := strings.TrimSpace(run(t, "", "git", "-C", f.work, "rev-parse", "HEAD:historical.txt"))
	run(t, "", "git", "-C", f.work, "rm", "historical.txt")
	run(t, "", "git", "-C", f.work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "remove historical default blob")
	f.pushMain()

	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	var mu sync.Mutex
	var seen []bridgePublish
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/worker/runs/") || !strings.HasSuffix(r.URL.Path, "/publish") {
			http.NotFound(w, r)
			return
		}
		tip := r.Header.Get("X-Uzi-Checkpoint-Tip")
		pack, err := checkpointBridgePack(r.Body)
		if err != nil {
			cancel() // Join the child through cmd.Run before reporting the bounded-read failure.
			http.Error(w, "read pack", http.StatusBadRequest)
			return
		}
		// Match production's typed refusal tokens; service ownership/slot retry stays outside this bridge.
		result, err := pushbroker.Publish(r.Context(), pushbroker.Options{
			CloneURL: f.cloneURL(), Branch: branch, DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
			CIConfigGuard: true, CIConfigPaths: []string{".github/workflows/**"},
		})
		rec := bridgePublish{tip: tip, result: result}
		resp := map[string]any{"published": true, "ref": ref}
		switch {
		case err == nil:
		case checkpointBridgeRefusal(err) != "":
			rec.skipped = checkpointBridgeRefusal(err)
		default:
			rec.err = err.Error()
			http.Error(w, "publish failed", http.StatusInternalServerError)
			mu.Lock()
			seen = append(seen, rec)
			mu.Unlock()
			return
		}
		if rec.skipped != "" {
			resp = map[string]any{"published": false, "ref": ref, "skipped": rec.skipped}
		}
		mu.Lock()
		seen = append(seen, rec)
		first := len(seen) == 1
		mu.Unlock()
		if lostAck && first {
			// The overlay landed in origin; the agent never sees the acknowledgement.
			http.Error(w, "ack lost", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	scratch, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "chain-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("chain evidence=%s", scratch)
	cfg, err := json.Marshal(map[string]any{
		"url": srv.URL, "token": "tkn-bridge-123456", "origin": f.bare, "iid": iid,
		"boundary": boundary, "scratch": scratch, "defaultBranch": "main", "historicalBlob": historicalBlob,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, node, "--import", "tsx", "test/fixtures/checkpoint-selective-chain-bridge.ts")
	cmd.Dir = filepath.Join(root, "agent")
	cmd.WaitDelay = 10 * time.Second
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NODE_TEST_CONTEXT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "UZI_CHAIN_BRIDGE_FIXTURE="+string(cfg))
	stdout := &checkpointBridgeOutput{cancel: cancel}
	stderr := &checkpointBridgeOutput{cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	if stdout.overflow || stderr.overflow {
		t.Fatal("fixture output exceeded one MiB; child cancelled and joined")
	}

	mu.Lock()
	publishes := append([]bridgePublish(nil), seen...)
	mu.Unlock()
	describe := func() string {
		return fmt.Sprintf("publishes=%+v\nstdout:\n%s\nstderr:\n%s", publishes, stdout.String(), stderr.String())
	}
	if runErr != nil {
		t.Fatalf("agent fixture failed: %v\n%s", runErr, describe())
	}
	var summary struct {
		Overlay     string   `json:"overlay"`
		OverlayRef  string   `json:"overlayRef"`
		RawTip      string   `json:"rawTip"`
		TickOutcome string   `json:"tickOutcome"`
		Tips        []string `json:"tips"`
		Results     []struct {
			OK         bool `json:"ok"`
			HTTPStatus int  `json:"httpStatus"`
		} `json:"results"`
	}
	found := false
	for _, line := range strings.Split(stdout.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "BRIDGE_SUMMARY "); ok {
			if err := json.Unmarshal([]byte(rest), &summary); err != nil {
				t.Fatalf("bad summary %q: %v", rest, err)
			}
			found = true
		}
	}
	if !found || summary.Overlay == "" || summary.RawTip == "" {
		t.Fatalf("fixture summary incomplete: %+v\n%s", summary, describe())
	}

	// The overlay publish: origin's checkpoint ref was the overlay commit O the agent built.
	if summary.OverlayRef != summary.Overlay {
		t.Fatalf("after the overlay publish the ref = %s, want overlay %s\n%s", summary.OverlayRef, summary.Overlay, describe())
	}
	subject := strings.TrimSpace(run(t, "", "git", "-C", f.bare, "log", "-1", "--format=%s", summary.Overlay))
	if !strings.HasPrefix(subject, "ckpt(overlay):") {
		t.Fatalf("overlay subject = %q, want a ckpt(overlay): commit", subject)
	}
	// The fixture advances default with the same run file before changing workflows:
	// the real overlay wrapper must therefore have exactly default's complete tree.
	overlayTree := strings.TrimSpace(run(t, "", "git", "-C", f.bare, "rev-parse", summary.Overlay+"^{tree}"))
	defaultTree := strings.TrimSpace(run(t, "", "git", "-C", f.bare, "rev-parse", "refs/heads/main^{tree}"))
	if overlayTree != defaultTree {
		t.Fatalf("overlay tree %s differs from default tree %s", overlayTree, defaultTree)
	}

	// A lost acknowledgement is an HTTP 500 the agent retries once with the SAME overlay tip, so
	// that case makes three requests (overlay, overlay retry, chained checkpoint); a clean run two.
	wantPublishes := 2
	if lostAck {
		wantPublishes = 3
	}
	if len(publishes) != wantPublishes {
		t.Fatalf("publishes = %d, want %d\n%s", len(publishes), wantPublishes, describe())
	}
	last := len(publishes) - 1
	recorded := make([]string, len(publishes))
	for i, p := range publishes {
		recorded[i] = p.tip
	}
	if !slices.Equal(recorded, summary.Tips) {
		t.Fatalf("Go-recorded tips %v != fixture-reported tips %v\n%s", recorded, summary.Tips, describe())
	}
	if publishes[0].tip != summary.Overlay {
		t.Fatalf("first publish tip %s, want overlay %s\n%s", publishes[0].tip, summary.Overlay, describe())
	}
	if lostAck && publishes[1].tip != summary.Overlay {
		t.Fatalf("retry tip %s, want the same overlay %s\n%s", publishes[1].tip, summary.Overlay, describe())
	}
	if boundary == "tick" && summary.TickOutcome != "published" {
		t.Fatalf("tick outcome = %q, want published\n%s", summary.TickOutcome, describe())
	}
	if lostAck {
		// The agent must actually have seen the overlay publish fail after it landed, then the
		// retry of the same tip acknowledged.
		if len(summary.Results) < 2 || summary.Results[0].OK || summary.Results[0].HTTPStatus != http.StatusInternalServerError || !summary.Results[1].OK {
			t.Fatalf("lost-ack: results = %+v, want [{ok:false httpStatus:500} {ok:true} ...]\n%s", summary.Results, describe())
		}
	}

	for i, p := range publishes {
		want := pushbroker.PublishAdvanced
		already := lostAck && i == 1
		if already {
			want = pushbroker.PublishUnclassified
		}
		if p.result.Disposition != want || p.result.AlreadyCurrent != already {
			t.Fatalf("publish %d result=%+v want disposition=%v alreadyCurrent=%t", i, p.result, want, already)
		}
		if p.skipped != "" || p.err != "" {
			t.Fatalf("a publish was not accepted (tip %s skipped=%q err=%q)\n%s", p.tip, p.skipped, p.err, describe())
		}
	}
	final := f.originRef(ref)
	if publishes[last].tip != final {
		t.Fatalf("last publish tip %s != final ref %s\n%s", publishes[last].tip, final, describe())
	}
	if final == summary.Overlay {
		t.Fatalf("the checkpoint ref never advanced past the overlay %s\n%s", summary.Overlay, describe())
	}
	parents := strings.Fields(run(t, "", "git", "-C", f.bare, "rev-list", "--parents", "-n", "1", final))
	if len(parents) != 3 || parents[0] != final || parents[1] != summary.Overlay || parents[2] != summary.RawTip {
		t.Fatalf("final ref %s parents = %v, want exactly [overlay %s, raw tip %s]\n%s",
			final, parents[1:], summary.Overlay, summary.RawTip, describe())
	}
}

// runAgentStaleBaseBridge is issue #3242's reproduction: a run whose tip may still be the default
// branch commit it cloned, while origin's default branch moves, publishes a checkpoint. Origin has no
// branch and no checkpoint ref yet. Before each publish reaches the broker, a depth-1 fetch over the
// same smart HTTP URL records whether the declared tip is absent from every base ref tip the broker
// fetches, so a refusal can be told apart from a transport artifact.
func runAgentStaleBaseBridge(t *testing.T, root, node string, c staleBaseCase, iid int) {
	backend := requireGitHTTPBackend(t)
	trustLoopbackTLS(t)
	f := newGitFixture(t)
	switch c.shape {
	case "root":
		for _, dir := range []string{"docs", ".github/workflows"} {
			if err := os.MkdirAll(filepath.Join(f.work, dir), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		writeFile(t, filepath.Join(f.work, "README.md"), "bridge\n")
		writeFile(t, filepath.Join(f.work, "docs", "a.md"), "doc\n")
		writeFile(t, filepath.Join(f.work, ".github", "workflows", "ci.yml"), "name: ci\non: push\njobs: {}\n# base\n")
		f.git("add", "-A")
		f.git("commit", "-m", "root")
	default:
		f.commit("README.md", "bridge\n", "initial")
		if err := os.MkdirAll(filepath.Join(f.work, "docs"), 0o750); err != nil {
			t.Fatal(err)
		}
		f.commit("docs/a.md", "doc\n", "docs")
		if err := os.MkdirAll(filepath.Join(f.work, ".github", "workflows"), 0o750); err != nil {
			t.Fatal(err)
		}
		f.commit(".github/workflows/ci.yml", "name: ci\non: push\njobs: {}\n# base\n", "workflow v1")
		if c.shape == "merge" {
			f.git("checkout", "-q", "-b", "side")
			f.commit("side.txt", "side\n", "side")
			f.git("checkout", "-q", "main")
			f.commit("main.txt", "main\n", "main")
			f.git("merge", "-q", "--no-ff", "-m", "merge side", "side")
		}
	}
	f.pushMain()
	branch := fmt.Sprintf("agent/issue-%d", iid)
	ref := "refs/uzi-checkpoints/" + branch
	url := newGitHTTPRemote(t, f, backend).cloneURL(f)

	var (
		absentMu sync.Mutex
		absent   []bool
		raced    bool
	)
	srv := newBridgeServer(t, ref, c.lostAck, func(ctx context.Context, tip string, pack []byte) error {
		absentMu.Lock()
		if c.race && !raced {
			// The worker resolved its range while main was its tip; main moves before the broker fetches.
			raced = true
			f.commit("raced.txt", "raced\n", "raced")
			f.pushMain()
		}
		absentMu.Unlock()
		missing := !shallowBaseHas(t, url, tip, branch)
		absentMu.Lock()
		absent = append(absent, missing)
		first := len(absent) == 1
		absentMu.Unlock()
		if c.seededPause && first {
			return errBridgeSuperseded
		}
		opts := pushbroker.Options{
			CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(),
			Branch: branch, DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
		}
		if c.guard {
			opts.CIConfigGuard, opts.CIConfigPaths = true, []string{".github/workflows/**"}
		}
		_, err := pushbroker.Publish(ctx, opts)
		return err
	})

	var summary struct {
		BaseTip           string `json:"baseTip"`
		MainTip           string `json:"mainTip"`
		WorkTip           string `json:"workTip"`
		DescendantTip     string `json:"descendantTip"`
		TickOutcome       string `json:"tickOutcome"`
		DescendantOutcome string `json:"descendantOutcome"`
		Parked            bool   `json:"parked"`
	}
	out, runErr := runBridgeFixture(t, root, node, map[string]any{
		"url": srv.srv.URL, "token": "tkn-bridge-123456", "origin": f.bare, "iid": iid, "boundary": "tick",
		"stale":   map[string]any{"advance": c.advance, "commit": c.commit, "descendant": c.descendant, "pause": c.seededPause},
		"seeded":  c.seededPause,
		"scratch": t.TempDir(),
	}, &summary)
	publishes := srv.publishes()
	absentMu.Lock()
	absent = append([]bool(nil), absent...)
	absentMu.Unlock()
	describe := func() string {
		return fmt.Sprintf("case=%+v publishes=%+v absent=%v summary=%+v\n%s", c, publishes, absent, summary, out)
	}
	if runErr != nil {
		t.Fatalf("agent fixture failed: %v\n%s", runErr, describe())
	}
	if len(publishes) == 0 {
		t.Fatalf("no checkpoint publish reached the broker\n%s", describe())
	}
	if c.seededPause {
		// [unconfirmed tick of B, pause publish of B after main moved]; the pause must park on B.
		if len(publishes) != 2 || publishes[0].skipped != "superseded" || publishes[1].tip != summary.BaseTip {
			t.Fatalf("publishes = %+v, want [tick B skipped superseded, pause B]\n%s", publishes, describe())
		}
		if !absent[1] {
			t.Fatalf("precondition: B is still a shallow base tip at the pause, so this case proves nothing\n%s", describe())
		}
		if publishes[1].skipped != "" || publishes[1].err != "" || !summary.Parked {
			t.Fatalf("the seeded no-commit pause did not publish and park: %+v parked=%v\n%s", publishes[1], summary.Parked, describe())
		}
		if got := f.originRef(ref); got != summary.BaseTip {
			t.Fatalf("checkpoint ref = %s, want the base %s\n%s", got, summary.BaseTip, describe())
		}
		return
	}
	moved := c.advance != "none" || c.race
	if moved && !c.commit && !absent[0] {
		t.Fatalf("precondition: the unmoved tip %s is still a shallow base tip, so this case proves nothing\n%s",
			publishes[0].tip, describe())
	}
	if c.want != "published" {
		if publishes[0].skipped != c.want {
			t.Fatalf("first publish = skipped %q err %q, want the broker's %q refusal\n%s", publishes[0].skipped, publishes[0].err, c.want, describe())
		}
		return
	}
	for i, p := range publishes {
		if p.skipped != "" || p.err != "" {
			t.Fatalf("publish %d (tip %s, absent from shallow base=%v) was refused: skipped=%q err=%q\n%s",
				i, p.tip, absent[i], p.skipped, p.err, describe())
		}
	}
	first := summary.BaseTip
	if c.commit {
		first = summary.WorkTip
	}
	if publishes[0].tip != first {
		t.Fatalf("first publish declared %s, want the run's tip %s\n%s", publishes[0].tip, first, describe())
	}
	if summary.TickOutcome != "published" {
		t.Fatalf("tick outcome = %q, want published\n%s", summary.TickOutcome, describe())
	}
	wantTips := []string{first}
	if c.lostAck {
		// The first publish landed but its 500 made the agent retry the SAME literal tip.
		wantTips = append(wantTips, first)
	}
	if c.descendant {
		if summary.DescendantOutcome != "published" {
			t.Fatalf("descendant outcome = %q, want published\n%s", summary.DescendantOutcome, describe())
		}
		wantTips = append(wantTips, summary.DescendantTip)
	}
	recorded := make([]string, len(publishes))
	for i, p := range publishes {
		recorded[i] = p.tip
	}
	if !slices.Equal(recorded, wantTips) {
		t.Fatalf("published tips %v, want %v\n%s", recorded, wantTips, describe())
	}
	if got := f.originRef(ref); got != wantTips[len(wantTips)-1] {
		t.Fatalf("checkpoint ref = %s, want the last published tip %s\n%s", got, wantTips[len(wantTips)-1], describe())
	}
}

// shallowBaseHas reports whether a depth-1 fetch of the broker's base refs (the branch, main and
// the checkpoint ref, whichever origin advertises) over url contains commit sha.
func shallowBaseHas(t *testing.T, url, sha, branch string) bool {
	t.Helper()
	dir := t.TempDir()
	run(t, "", "git", "init", "-q", "--bare", dir)
	// The loopback TLS certificate is self-signed; this probe never leaves 127.0.0.1.
	refs := run(t, "", "git", "-c", "http.sslVerify=false", "ls-remote", url)
	var specs []string
	for _, want := range []string{"refs/heads/" + branch, "refs/heads/main", "refs/uzi-checkpoints/" + branch} {
		if strings.Contains(refs, "\t"+want+"\n") {
			specs = append(specs, "+"+want+":"+want)
		}
	}
	args := append([]string{"-C", dir, "-c", "http.sslVerify=false", "fetch", "-q", "--depth", "1", url}, specs...)
	run(t, "", "git", args...)
	probe := exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}") //nolint:gosec // G204: test helper against a test-owned temp repo; sha is the declared tip under test, not attacker input.
	probe.Env = cleanGitEnv()
	return probe.Run() == nil
}
