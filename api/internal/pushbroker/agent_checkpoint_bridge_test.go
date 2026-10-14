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
}

// TestAgentCheckpointChainBridge is the issue #2733 binding bridge. It runs the REAL agent
// (agent/test/fixtures/checkpoint-chain-bridge.ts: a real RunRunner + GitCache driving a GitHub
// forge claim through a workflow-overlay checkpoint O and then a raw tick or iteration checkpoint
// N) against the REAL pushbroker.Publish, over the real HTTP route and WorkerClient.publishCheckpoint.
// The broker keeps its depth-1 fetch and pack boundaries; no commit shape is built on the Go side,
// so the agent and the broker cannot drift apart unnoticed (the unit tests on each side use a
// stand-in for the other). It asserts the checkpoint ref chains [O, N] over the overlay and that
// no publish is refused not_descendant, with the overlay acknowledged cleanly and with its
// acknowledgement lost after it landed.
//
// Gated by UZI_CHECKPOINT_CHAIN_BRIDGE=1 (task test:checkpoint-chain-bridge): it needs Node and the
// installed agent dependencies, so the default `go test ./...` skips it. Once the gate is set a
// missing prerequisite is a failure, never a skip.
func TestAgentCheckpointChainBridge(t *testing.T) {
	if os.Getenv("UZI_CHECKPOINT_CHAIN_BRIDGE") != "1" {
		t.Skip("opt in with task test:checkpoint-chain-bridge")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the bridge requires Node")
	}
	if _, err := os.Stat(filepath.Join(root, "agent/node_modules/tsx/package.json")); err != nil {
		t.Fatal("the bridge requires installed agent dependencies (tsx)")
	}
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

	var mu sync.Mutex
	var seen []bridgePublish
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		// Simplified production mapping: ErrTipMissing, ErrPackTooLarge and ErrPackInvalid are 500 here
		// but 200 skips in production; AlreadyCurrent ownership and the slot retry are not modelled.
		_, err = pushbroker.Publish(r.Context(), pushbroker.Options{
			CloneURL: f.cloneURL(), Branch: branch, DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
		})
		rec := bridgePublish{tip: tip}
		resp := map[string]any{"published": true, "ref": ref}
		switch {
		case err == nil:
		case errors.Is(err, pushbroker.ErrNotDescendant):
			rec.skipped = "not_descendant"
		case errors.Is(err, pushbroker.ErrWorkflowScopeRejected):
			rec.skipped = "workflow_scope"
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

	cfg, err := json.Marshal(map[string]any{
		"url": srv.URL, "token": "tkn-bridge-123456", "origin": f.bare, "iid": iid,
		"boundary": boundary, "scratch": t.TempDir(),
	})
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
	cmd.Env = append(cmd.Env, "UZI_CHAIN_BRIDGE_FIXTURE="+string(cfg))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

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

	for _, p := range publishes {
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
