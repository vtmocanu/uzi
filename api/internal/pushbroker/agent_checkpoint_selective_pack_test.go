package pushbroker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Each pipe retains at most one MiB of actual bytes. Overflow cancels the owned
// command; Run joins it (including the output pumps) before the caller fails.
type checkpointBridgeOutput struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *checkpointBridgeOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - b.buffer.Len()
	if n > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.overflow = true
		b.cancel()
	} else {
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}

func (b *checkpointBridgeOutput) Bytes() []byte  { return b.buffer.Bytes() }
func (b *checkpointBridgeOutput) String() string { return b.buffer.String() }

func checkpointBridgeRefusal(err error) string {
	for _, c := range []struct {
		err    error
		reason string
	}{
		{pushbroker.ErrTipMissing, "tip_missing"}, {pushbroker.ErrPackTooLarge, "pack_too_large"},
		{pushbroker.ErrPackInvalid, "pack_invalid"}, {pushbroker.ErrNotDescendant, "not_descendant"},
		{pushbroker.ErrWorkflowScopeRejected, "workflow_scope"}, {pushbroker.ErrCIConfigGuard, "ci_config_guard"},
	} {
		if errors.Is(err, c.err) {
			return c.reason
		}
	}
	return ""
}

// The wire limit is independent of the broker's reconstructed-object budget.
func checkpointBridgePack(r io.Reader) ([]byte, error) {
	pack, err := io.ReadAll(io.LimitReader(r, (64<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(pack) > 64<<20 {
		return nil, fmt.Errorf("checkpoint bridge request exceeds 64 MiB")
	}
	return pack, nil
}

type selectiveArtifact struct {
	Name          string   `json:"name"`
	File          string   `json:"file"`
	Tip           string   `json:"tip"`
	Inventory     []string `json:"inventory"`
	Reconstructed int64    `json:"reconstructed"`
}
type selectiveSummary struct {
	Local                        string              `json:"local"`
	Branch                       string              `json:"branch"`
	DefaultName                  string              `json:"defaultName"`
	Tip                          string              `json:"tip"`
	Floor                        string              `json:"floor"`
	X                            string              `json:"x"`
	D                            string              `json:"d"`
	DefaultTip                   string              `json:"defaultTip"`
	PublicBlobs                  []string            `json:"publicBlobs"`
	PrivateBlob                  string              `json:"privateBlob"`
	CandidateTree                string              `json:"candidateTree"`
	ProtectedChanged             string              `json:"protectedChanged"`
	ProtectedBase                string              `json:"protectedBase"`
	Stopped                      bool                `json:"stopped"`
	HistoricalBlob               string              `json:"historicalBlob"`
	HistoricalBlobAbsentDepthOne bool                `json:"historicalBlobAbsentDepthOne"`
	CapturedDefault              string              `json:"capturedDefault"`
	CapturedFloor                string              `json:"capturedFloor"`
	Calls                        []string            `json:"calls"`
	Settled                      bool                `json:"settled"`
	Artifacts                    []selectiveArtifact `json:"artifacts"`
}

func selectiveBridgeFixture(t *testing.T, parent context.Context, mode string) (*gitFixture, selectiveSummary) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	scratch, err := os.MkdirTemp(filepath.Join(root, ".uzi/scratch"), "m2-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	// Kept for main's evidence review; the worker owns scratch retirement.
	f := newGitFixture(t)
	cfg, err := json.Marshal(map[string]string{"origin": f.bare, "scratch": scratch, "mode": mode, "defaultBranch": "integration/custom"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(parent, 110*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "test/fixtures/checkpoint-selective-pack-bridge.ts")
	cmd.Dir = filepath.Join(root, "agent")
	cmd.WaitDelay = 3 * time.Second
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NODE_TEST_CONTEXT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "UZI_SELECTIVE_BRIDGE_FIXTURE="+string(cfg))
	out := &checkpointBridgeOutput{cancel: cancel}
	errout := &checkpointBridgeOutput{cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, errout
	runErr := cmd.Run()
	if out.overflow || errout.overflow {
		t.Fatal("fixture output exceeded one MiB; child cancelled and joined")
	}
	if runErr != nil {
		t.Fatalf("fixture %s: %v\n%s\n%s", mode, runErr, out.String(), errout.String())
	}
	var summary selectiveSummary
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		if raw, ok := strings.CutPrefix(line, "SELECTIVE_SUMMARY "); ok {
			if found {
				t.Fatal("duplicate summary")
			}
			if err := json.Unmarshal([]byte(raw), &summary); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found || summary.Tip == "" || !summary.Settled || summary.DefaultName != "integration/custom" {
		t.Fatalf("incomplete summary: %+v", summary)
	}
	// Persist the already-bounded metadata alongside pack files for inventory review.
	if err := os.WriteFile(filepath.Join(scratch, "summary.json"), out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("mode=%s evidence=%s", mode, scratch)
	return f, summary
}

func selectiveArtifactPack(t *testing.T, s selectiveSummary, a selectiveArtifact) []byte {
	t.Helper()
	rel, err := filepath.Rel(s.Local, a.File)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatal("artifact escaped fixture directory")
	}
	file, err := os.Open(a.File)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	pack, err := checkpointBridgePack(file)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func TestAgentCheckpointSelectivePackBridge(t *testing.T) {
	if os.Getenv("UZI_CHECKPOINT_SELECTIVE_PACK_BRIDGE") != "1" {
		t.Skip("opt in with UZI_CHECKPOINT_SELECTIVE_PACK_BRIDGE=1")
	}
	modes := []string{"PrimaryLargeHistory", "LargeRunOnly", "ReachableDefault", "ExactLinearHistory", "AbsentOriginBranch",
		"NoDefaultMerge", "EqualDefault", "RevertDefault", "AbsentServerName", "UnresolvedDefault",
		"MirrorHeadsOnly", "ConflictingOriginHEAD", "ReplayAdvance", "ReplayRewrite", "ReplayDelete",
		"FrozenHeld", "ProtectedDifference", "UnprotectedDifference", "ExpiredParent", "CancelledParent",
		"PreparationAbort", "PreparationDeadline", "CapExpiredParent", "LiveCapFallback"}
	t.Run("OutputBounds", testCheckpointBridgeOutputBounds)
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			caseCtx, caseCancel := context.WithTimeout(t.Context(), 119*time.Second)
			defer caseCancel()
			f, s := selectiveBridgeFixture(t, caseCtx, mode)
			if s.Stopped {
				if len(s.Artifacts) != 1 {
					t.Fatal("stopped preparation produced a publishable pack")
				}
				if got := f.originRef("refs/uzi-checkpoints/" + s.Branch); got != "" {
					t.Fatalf("stopped operation published %s", got)
				}
				return // No broker call is made for cancelled/expired preparation.
			}
			if len(s.Artifacts) < 2 {
				t.Fatal("missing production pack")
			}
			legacy, selected := s.Artifacts[0], s.Artifacts[1]
			if !s.HistoricalBlobAbsentDepthOne {
				t.Fatal("historical omitted blob must be absent from depth-one bases and present at origin")
			}
			legacyMode := mode == "AbsentServerName" || mode == "UnresolvedDefault" || mode == "MirrorHeadsOnly" ||
				mode == "ConflictingOriginHEAD" || mode == "LiveCapFallback"
			if legacyMode && !equalCheckpointInventory(legacy.Inventory, selected.Inventory) {
				t.Fatal("legacy fallback inventory changed")
			}
			has := func(a selectiveArtifact, oid, kind string) bool {
				for _, row := range a.Inventory {
					if row == oid+" "+kind {
						return true
					}
				}
				return false
			}
			selectedSet, legacySet := make(map[string]bool), make(map[string]bool)
			for _, row := range selected.Inventory {
				selectedSet[row] = true
			}
			for _, row := range legacy.Inventory {
				legacySet[row] = true
				if (strings.HasSuffix(row, " commit") || strings.HasSuffix(row, " tree")) && !selectedSet[row] {
					t.Fatalf("original range commit/tree omitted: %s", row)
				}
			}
			for _, row := range selected.Inventory {
				if !legacySet[row] {
					t.Fatalf("selective pack added an object outside original walk: %s", row)
				}
			}
			if mode == "ExactLinearHistory" && s.DefaultTip != s.D {
				t.Fatal("exact linear default must be D")
			}
			if mode == "AbsentOriginBranch" {
				base := strings.TrimSpace(run(t, "", "git", "-C", filepath.Join(s.Local, "worker.git"), "merge-base", s.DefaultTip, s.Tip))
				if s.CapturedFloor != base {
					t.Fatalf("captured floor=%s want merge-base=%s", s.CapturedFloor, base)
				}
			}
			for _, oid := range []string{s.X, s.D, s.Tip} {
				if !has(selected, oid, "commit") {
					t.Fatalf("candidate ancestry commit omitted: %s", oid)
				}
			}
			if !has(selected, s.CandidateTree, "tree") && mode != "RevertDefault" {
				t.Fatal("original candidate tree omitted")
			}
			if !legacyMode && mode != "LargeRunOnly" && mode != "NoDefaultMerge" && mode != "AbsentOriginBranch" && mode != "PrimaryLargeHistory" {
				if !has(legacy, s.HistoricalBlob, "blob") || has(selected, s.HistoricalBlob, "blob") {
					t.Fatalf("historical default blob must be omitted: %s", s.HistoricalBlob)
				}
			}
			opts := pushbroker.Options{CloneURL: f.cloneURL(), Branch: s.Branch, DefaultBranch: s.DefaultName, DeclaredTip: s.Tip}
			publish := func(a selectiveArtifact) error {
				opts.Pack = selectiveArtifactPack(t, s, a)
				_, err := pushbroker.Publish(caseCtx, opts)
				t.Logf("artifact=%s tip=%s wire=%d reconstructed=%d outcome=%s err=%v",
					a.Name, a.Tip, len(opts.Pack), a.Reconstructed, checkpointBridgeRefusal(err), err)
				return err
			}
			if mode == "PrimaryLargeHistory" {
				if legacy.Reconstructed <= 128<<20 {
					t.Fatal("legacy control did not exceed cumulative budget")
				}
				if err := publish(legacy); !errors.Is(err, pushbroker.ErrPackTooLarge) {
					t.Fatalf("legacy Publish=%v; want ErrPackTooLarge", err)
				}
				if f.originRef("refs/uzi-checkpoints/"+s.Branch) != "" {
					t.Fatal("legacy refusal created ref")
				}
			}
			if mode == "LargeRunOnly" {
				if err := publish(selected); !errors.Is(err, pushbroker.ErrPackTooLarge) {
					t.Fatalf("run-only Publish=%v; want ErrPackTooLarge", err)
				}
				return
			}
			if mode == "ProtectedDifference" || mode == "UnprotectedDifference" {
				opts.CIConfigGuard = true
				opts.CIConfigPaths = []string{"ci/**"}
				if mode == "ProtectedDifference" && !has(selected, s.ProtectedChanged, "tree") {
					t.Fatal("differing protected tree missing")
				}
				if mode == "UnprotectedDifference" && !has(selected, s.ProtectedBase, "tree") {
					// Equal floor subtree is fetched with the current default, not required in the pack.
					if got := f.originRef("refs/heads/" + s.DefaultName); got != s.DefaultTip {
						t.Fatal("required fetched floor unavailable")
					}
				}
			}
			if legacyMode {
				if err := publish(legacy); err != nil {
					t.Fatalf("legacy actual Publish: %v", err)
				}
			}
			if mode == "LiveCapFallback" {
				producers := 0
				for _, call := range s.Calls {
					if strings.Contains(call, "pack-objects") {
						producers++
						if !strings.Contains(call, "--revs") {
							t.Fatal("cap fallback did not use legacy producer")
						}
					}
				}
				if producers != 1 {
					t.Fatalf("fallback producers=%d want=1", producers)
				}
			}
			err := publish(selected)
			if mode == "ProtectedDifference" {
				if !errors.Is(err, pushbroker.ErrCIConfigGuard) || !strings.Contains(err.Error(), "protected path differs") {
					t.Fatalf("want protected difference with all original trees, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("selective actual Publish must succeed: %v", err)
			}
			if legacyMode && mode != "LiveCapFallback" && s.CapturedDefault != "" {
				t.Fatal("untrusted or unavailable default captured blob-exclusion authority")
			}
			if !legacyMode && mode != "FrozenHeld" && s.CapturedDefault != s.DefaultTip {
				t.Fatalf("captured default=%s want=%s", s.CapturedDefault, s.DefaultTip)
			}
			if mode == "PrimaryLargeHistory" && has(selected, s.HistoricalBlob, "blob") {
				t.Fatalf("historical default blob retained: %s", s.HistoricalBlob)
			}
			if got := f.originRef("refs/uzi-checkpoints/" + s.Branch); got != s.Tip {
				t.Fatalf("remote=%s want=%s", got, s.Tip)
			}
			if mode == "FrozenHeld" {
				o := pushbroker.HeldPackOptions{CloneURL: f.cloneURL(), Branch: s.Branch, DefaultBranch: s.DefaultName,
					RunID: uuid.New(), Generation: 1, Tip: s.Tip, Pack: selectiveArtifactPack(t, s, selected), CIConfigGuard: true, CIConfigPaths: []string{"ci/**"}}
				prepared, err := pushbroker.PrepareHeldPack(caseCtx, o)
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := pushbroker.SendHeldCreate(caseCtx, prepared)
				if err != nil || outcome != pushbroker.HeldCreateCreated {
					t.Fatalf("held outcome=%v err=%v", outcome, err)
				}
				if got := f.originRef(pushbroker.HeldRef(o.RunID, 1)); got != s.Tip {
					t.Fatalf("held ref=%s want=%s", got, s.Tip)
				}
			}
			if strings.HasPrefix(mode, "Replay") {
				if !equalCheckpointInventory(selected.Inventory, s.Artifacts[2].Inventory) {
					t.Fatal("replay inventory drift")
				}
				if err := publish(s.Artifacts[2]); err != nil {
					t.Fatalf("replay Publish: %v", err)
				}
				if f.originRef("refs/uzi-checkpoints/"+s.Branch) != s.Tip {
					t.Fatal("replay changed remote tip")
				}
			}
		})
	}
}

// Exercise byte caps through os/exec's real pipe-copy path, including exactly-at-cap controls.
type checkpointBridgeCounter struct{ read int64 }

func (r *checkpointBridgeCounter) Read(p []byte) (int, error) {
	clear(p)
	r.read += int64(len(p))
	return len(p), nil
}

func testCheckpointBridgeOutputBounds(t *testing.T) {
	t.Run("WireOverflow", func(t *testing.T) {
		r := &checkpointBridgeCounter{}
		pack, err := checkpointBridgePack(r)
		if err == nil || pack != nil || r.read != (64<<20)+1 {
			t.Fatalf("wire overflow: bytes=%d retained=%d err=%v", r.read, len(pack), err)
		}
	})
	if os.Getenv("UZI_CHECKPOINT_SELECTIVE_PACK_BRIDGE") != "1" {
		t.Skip("selective bridge opt-in required")
	}
	for _, pipe := range []string{"stdout", "stderr"} {
		for _, overflow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overflow=%t", pipe, overflow), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				size := 1 << 20
				if overflow {
					size++
				}
				// Fixed inert program: no repository-provided command payload is evaluated.
				program := fmt.Sprintf("process.%s.write(Buffer.alloc(%d));", pipe, size)
				if overflow {
					program += "setTimeout(() => {}, 20000);"
				}
				cmd := exec.CommandContext(ctx, "node", "-e", program) //nolint:gosec // G204: fixed program assembled only from local pipe enum and byte cap, never repository input.
				cmd.WaitDelay = time.Second
				out, errout := &checkpointBridgeOutput{cancel: cancel}, &checkpointBridgeOutput{cancel: cancel}
				cmd.Stdout, cmd.Stderr = out, errout
				err := cmd.Run() // Includes cancellation and the output-copy joins.
				chosen := out
				if pipe == "stderr" {
					chosen = errout
				}
				if chosen.overflow != overflow || len(chosen.Bytes()) != 1<<20 {
					t.Fatalf("overflow=%t bytes=%d; want overflow=%t bytes=1048576", chosen.overflow, len(chosen.Bytes()), overflow)
				}
				if !overflow && err != nil {
					t.Fatal(err)
				}
				if overflow && ctx.Err() == context.DeadlineExceeded {
					t.Fatal("overflow failed to cancel before deadline")
				}
			})
		}
	}
}

func equalCheckpointInventory(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
