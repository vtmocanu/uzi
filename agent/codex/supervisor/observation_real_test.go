//go:build supervisor_observation_real

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This fixture is required by test:agent:worker-uid, not the unprivileged repo
// gate. The command-UID lane verifies its profile before invoking this target.
// This helper is re-executed by the real fixture with a restricted profile.
// Each payload waits on its inherited input, so only its saved owner controls it.
func TestObservationProcessHelper(t *testing.T) {
	mode := os.Getenv("UZI_OBSERVATION_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "supervisor":
		os.Exit(realMain([]string{"--expect-uid", strconv.Itoa(os.Getuid()), "--", "/usr/bin/env", "UZI_OBSERVATION_HELPER=payload", os.Args[0], "-test.run=^TestObservationProcessHelper$"}))
	case "leaf":
		time.Sleep(20 * time.Second)
		os.Exit(0)
	case "fork":
		cmd := exec.Command(os.Args[0], "-test.run=^TestObservationProcessHelper$")
		cmd.Env = append(os.Environ(), "UZI_OBSERVATION_HELPER=leaf")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "payload":
		threads, _ := strconv.Atoi(os.Getenv("UZI_OBSERVATION_THREADS"))
		done := make(chan struct{})
		ready := make(chan struct{}, threads)
		for i := 0; i < threads; i++ {
			go func() { runtime.LockOSThread(); ready <- struct{}{}; <-done; runtime.UnlockOSThread() }()
		}
		for i := 0; i < threads; i++ {
			<-ready
		}
		if os.Getenv("UZI_OBSERVATION_DETACHED") == "1" {
			cmd := exec.Command(os.Args[0], "-test.run=^TestObservationProcessHelper$")
			cmd.Env = append(os.Environ(), "UZI_OBSERVATION_HELPER=fork")
			if err := cmd.Run(); err != nil {
				os.Exit(4)
			}
		}
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
		os.Exit(0)
	}
	os.Exit(5)
}

type liveObservationRoot struct {
	cmd     *exec.Cmd
	control io.WriteCloser
	input   io.WriteCloser
	decoder *json.Decoder
	primary int
}

func startObservationRoot(t *testing.T, bin string, threads int, detached bool) *liveObservationRoot {
	t.Helper()
	uid := os.Getuid()
	args := []string{"--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs"}
	if uid == 0 {
		args = append([]string{"--bounding-set=-all"}, args...)
		uid = commandUID
		args = append(args, "--reuid="+strconv.Itoa(uid), "--regid="+strconv.Itoa(uid), "--clear-groups")
	} else {
		// An unprivileged process cannot drop its bounding set, even when it
		// is already empty. Require the lane's real cap-less profile instead.
		raw, err := os.ReadFile("/proc/self/status")
		if err != nil {
			t.Fatal(err)
		}
		st, err := parseProcStatus(string(raw))
		if err != nil || uid != commandUID || st.CapEff != 0 || st.CapPrm != 0 ||
			st.CapInh != 0 || st.CapAmb != 0 || st.CapBnd != 0 || st.NoNewPrivs != 1 {
			t.Fatalf("real observation prerequisite: cap-less command UID %d with no-new-privileges: status=%+v err=%v", commandUID, st, err)
		}
	}
	args = append(args, bin, "-test.run=^TestObservationProcessHelper$")
	cmd := exec.Command("/bin/setpriv", args...)
	cmd.Env = append(os.Environ(), "UZI_OBSERVATION_HELPER=supervisor", "UZI_OBSERVATION_THREADS="+strconv.Itoa(threads), "GOMAXPROCS=2")
	if detached {
		cmd.Env = append(cmd.Env, "UZI_OBSERVATION_DETACHED=1")
	}
	cr, cw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{cr, ew}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = cr.Close()
	_ = ew.Close()
	_ = er.SetReadDeadline(time.Now().Add(30 * time.Second))
	root := &liveObservationRoot{cmd: cmd, control: cw, input: input, decoder: json.NewDecoder(er)}
	t.Cleanup(func() {
		_ = input.Close()
		_ = cw.Close()
		_ = er.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	var started startedEvidence
	if err := root.decoder.Decode(&started); err != nil {
		t.Fatalf("start=%v stderr=%s", err, stderr.String())
	}
	if started.Event != "started" {
		t.Fatalf("profile prevented real fixture: %+v stderr=%s", started, stderr.String())
	}
	root.primary = started.ChildPid
	ready := make([]byte, 6)
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("payload readiness %q %v", ready, err)
	}
	return root
}
func (root *liveObservationRoot) observation(t *testing.T, id, timeout int) observationEvidence {
	t.Helper()
	_, err := fmt.Fprintf(root.control, "{\"op\":\"observe\",\"id\":%d,\"timeoutMs\":%d}\n", id, timeout)
	if err != nil {
		t.Fatal(err)
	}
	var result observationEvidence
	if err := root.decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Event != "observe" || result.ID != id {
		t.Fatalf("%+v", result)
	}
	return result
}
func (root *liveObservationRoot) dispose(t *testing.T) {
	t.Helper()
	// Release payload threads for ordinary observation-fixture teardown. The
	// disposal regression below keeps input open to exercise control-first exit.
	_ = root.input.Close()
	time.Sleep(100 * time.Millisecond)
	_, err := io.WriteString(root.control, "{\"op\":\"dispose\",\"id\":999,\"timeoutMs\":2000}\n")
	if err != nil {
		t.Fatal(err)
	}
	for {
		var ev map[string]any
		if err := root.decoder.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		if ev["event"] == "child_exit" {
			continue
		}
		if ev["event"] != "dispose" || ev["state"] != "drained" || ev["authority"] != authorityECHILD {
			t.Fatalf("dispose=%v", ev)
		}
		break
	}
	if err := root.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
func TestRealObservationSupervisedTrees(t *testing.T) {
	// Keep real fixture artifacts inside the worktree. The command uid needs
	// traverse/read permission to execute the copied test binary.
	if err := os.MkdirAll("../../../.uzi/scratch", 0755); err != nil {
		t.Fatal(err)
	}
	stage, err := os.MkdirTemp("../../../.uzi/scratch", "observation-real-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0755); err != nil {
		t.Fatal(err)
	}
	bin, err := filepath.Abs(filepath.Join(stage, "supervisor.test"))
	if err != nil {
		t.Fatal(err)
	}
	copyExecutable(t, os.Args[0], bin)
	t.Run("dispose-primary-exit", func(t *testing.T) {
		root := startObservationRoot(t, bin, 0, false)
		// The primary blocks on its owned input. Disposal must observe its actual
		// SIGKILL wait status; it cannot rely on the ordinary readiness arm winning.
		if _, err := io.WriteString(root.control, "{\"op\":\"dispose\",\"id\":991,\"timeoutMs\":2000}\n"); err != nil {
			t.Fatal(err)
		}
		exits, disposed := 0, false
		for {
			var ev map[string]any
			err := root.decoder.Decode(&ev)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			switch ev["event"] {
			case "child_exit":
				exits++
				if disposed || ev["code"] != float64(137) {
					t.Fatalf("primary exit must precede disposal with observed SIGKILL status: %v", ev)
				}
			case "dispose":
				if ev["state"] != "drained" || ev["authority"] != authorityECHILD {
					t.Fatalf("owned disposal not confirmed: %v", ev)
				}
				disposed = true
			default:
				t.Fatalf("unexpected lifecycle event: %v", ev)
			}
		}
		if err := root.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if !disposed || exits != 1 {
			t.Fatalf("real helper lost primary status during disposal: disposed=%v child_exit_count=%d", disposed, exits)
		}
	})
	t.Run("repeat-and-detached-doublefork", func(t *testing.T) {
		root := startObservationRoot(t, bin, 0, true)
		time.Sleep(100 * time.Millisecond)
		for i := 1; i <= 300; i++ {
			got := root.observation(t, i, 1000)
			if got.State != "complete" || len(got.Processes) < 2 {
				t.Fatalf("observation %d: %+v", i, got)
			}
			if err := unix.Kill(root.primary, 0); err != nil {
				t.Fatalf("primary died: %v", err)
			}
		}
		if got := root.observation(t, 301, 0); got.State != "unavailable" || got.Reason != "timeout" {
			t.Fatalf("%+v", got)
		}
		if got := root.observation(t, 302, 1000); got.State != "complete" {
			t.Fatalf("timeout poisoned root: %+v", got)
		}
		root.dispose(t)
	})
	t.Run("two-oversized-siblings-stay-alive", func(t *testing.T) {
		// 132 pinned threads per payload exceeds the aggregate 256 thread visits
		// across the two consistency passes, while keeping both roots under 512 tasks.
		a := startObservationRoot(t, bin, 132, false)
		b := startObservationRoot(t, bin, 132, false)
		for i := 1; i <= 3; i++ {
			for _, root := range []*liveObservationRoot{a, b} {
				got := root.observation(t, i, 1000)
				if got.State != "unavailable" || got.Reason != "oversize" {
					t.Fatalf("%+v", got)
				}
				if _, err := readIdentity(root.primary); err != nil {
					t.Fatalf("oversize killed sibling: %v", err)
				}
			}
		}
		a.dispose(t)
		b.dispose(t)
	})
}
