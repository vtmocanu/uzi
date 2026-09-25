package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type observingWriter struct {
	buf    bytes.Buffer
	events chan map[string]any
}

func (w *observingWriter) Write(p []byte) (int, error) {
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(p), &event); err == nil {
		w.events <- event
	}
	return w.buf.Write(p)
}

func awaitEvent(t *testing.T, events <-chan map[string]any, kind string) map[string]any {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event["event"] == kind {
				return event
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func awaitPID(t *testing.T, calls <-chan int, want int) {
	t.Helper()
	select {
	case pid := <-calls:
		if pid != want {
			t.Fatalf("reaped pid %d, want %d", pid, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for reap of %d", want)
	}
}

func sendTick(t *testing.T, ticks chan<- time.Time, done <-chan int) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case code := <-done:
		t.Fatalf("supervisor exited before tick: %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out delivering tick")
	}
}

func awaitRun(t *testing.T, done <-chan int, want int) {
	t.Helper()
	select {
	case code := <-done:
		if code != want {
			t.Fatalf("exit = %d, want %d", code, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for supervisor exit")
	}
}

func tickSupervisor(t *testing.T, children []int, reap func(int) (int, error)) (*supervisor, chan time.Time, chan error, chan int, *io.PipeWriter, *observingWriter, *atomic.Bool) {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	ticks := make(chan time.Time)
	ready := make(chan error, 1)
	calls := make(chan int, 16)
	obs := &observingWriter{events: make(chan map[string]any, 16)}
	sup, _ := newTestSupervisor("", scriptedReaper(reapEmpty))
	sup.ev = &evidence{w: obs}
	sup.control = &controlReader{r: reader}
	sup.seams.childReady = ready
	var cleared atomic.Bool
	sup.seams.directChildren = func() []int {
		if cleared.Load() {
			return nil
		}
		return children
	}
	sup.seams.adoptedTick = func() (<-chan time.Time, func()) { return ticks, func() {} }
	sup.seams.reapAdopted = func(pid int) (int, error) {
		got, err := reap(pid)
		calls <- pid
		return got, err
	}
	return sup, ticks, ready, calls, writer, obs, &cleared
}

func TestAdoptedTickReservesPrimaryUntilReapAndAllowsReusedPID(t *testing.T) {
	sup, ticks, ready, calls, control, obs, cleared := tickSupervisor(t, []int{9, 10}, func(pid int) (int, error) { return pid, nil })
	done := make(chan int, 1)
	go func() { done <- sup.run(9, procStatus{}) }()
	awaitEvent(t, obs.events, "started")
	sendTick(t, ticks, done)
	awaitPID(t, calls, 10)
	ready <- nil
	if event := awaitEvent(t, obs.events, "child_exit"); event["code"] != float64(0) {
		t.Fatalf("primary status: %v", event)
	}
	sendTick(t, ticks, done)
	awaitPID(t, calls, 9)
	awaitPID(t, calls, 10)
	cleared.Store(true)
	if _, err := io.WriteString(control, `{"op":"dispose","id":1}`+"\n"); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, done, 0)
}

func TestAdoptedTickRetriesBenignWaitOutcomes(t *testing.T) {
	steps := []struct {
		got int
		err error
	}{
		{0, nil}, {0, syscall.ECHILD}, {0, syscall.EINTR}, {10, nil},
	}
	index := 0
	sup, ticks, _, calls, control, obs, cleared := tickSupervisor(t, []int{9, 10}, func(pid int) (int, error) {
		step := steps[index]
		index++
		return step.got, step.err
	})
	done := make(chan int, 1)
	go func() { done <- sup.run(9, procStatus{}) }()
	awaitEvent(t, obs.events, "started")
	for range steps {
		sendTick(t, ticks, done)
		awaitPID(t, calls, 10)
	}
	if index != len(steps) {
		t.Fatalf("wait calls = %d", index)
	}
	cleared.Store(true)
	if _, err := io.WriteString(control, `{"op":"dispose","id":1}`+"\n"); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, done, 0)
}

func TestLiveAdoptedChildReapedBeforePrimaryExit(t *testing.T) {
	if os.Getenv("UZI_SUPERVISOR_LIVE_HELPER") == "1" {
		if !establishSubreaper() {
			os.Exit(2)
		}
		pidFile := os.Getenv("UZI_SUPERVISOR_PID_FILE")
		releaseFile := os.Getenv("UZI_SUPERVISOR_RELEASE_FILE")
		script := `sh -c 'sleep 0.1 & echo $! > "$1"' sh "$1"; while [ ! -e "$2" ]; do sleep 0.05; done; exit 7`
		pid, err := launchChild([]string{"/bin/sh", "-c", script, "sh", pidFile, releaseFile})
		if err != nil {
			os.Exit(2)
		}
		sup := &supervisor{
			ev:      &evidence{w: os.Stdout},
			control: &controlReader{r: os.Stdin},
			seams: seams{
				supervisorPid:  os.Getpid(),
				directChildren: selfDirectChildren,
				kill:           realKill,
				reap:           realReap,
				reapChild:      realReapChild,
				adoptedTick: func() (<-chan time.Time, func()) {
					ticker := time.NewTicker(500 * time.Millisecond)
					return ticker.C, ticker.Stop
				},
				reapAdopted: realReapAdopted,
				now:         time.Now,
				sleep:       func() { time.Sleep(2 * time.Millisecond) },
			},
		}
		os.Exit(sup.runWatched(pid, procStatus{}, watchChild))
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild-pid")
	releaseFile := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLiveAdoptedChildReapedBeforePrimaryExit$")
	cmd.Env = append(os.Environ(), "UZI_SUPERVISOR_LIVE_HELPER=1", "UZI_SUPERVISOR_PID_FILE="+pidFile, "UZI_SUPERVISOR_RELEASE_FILE="+releaseFile)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	primary, adopted := 0, 0
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			// Never signal the saved primary/grandchild PIDs: once the helper reaps
			// either, the number can be reused by an unrelated process. The release
			// file makes the primary exit on its own, the grandchild exits after its
			// short sleep, and the helper is our own unreaped child, so its PID is
			// still ours to kill.
			_ = os.WriteFile(releaseFile, nil, 0o600)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	events := make(chan map[string]any, 16)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var event map[string]any
			if json.Unmarshal(scanner.Bytes(), &event) == nil {
				events <- event
			}
		}
	}()
	started := awaitEvent(t, events, "started")
	primary = int(started["childPid"].(float64))
	deadline := time.After(10 * time.Second)
	for adopted == 0 {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			_, _ = fmt.Sscanf(string(data), "%d", &adopted)
		}
		select {
		case <-deadline:
			t.Fatal("grandchild pid not published")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	disappeared := false
	deadline = time.After(10 * time.Second)
	for !disappeared {
		disappeared = errors.Is(unix.Kill(adopted, 0), syscall.ESRCH)
		if disappeared {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("adopted child %d was not reaped before dispose", adopted)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := unix.Kill(primary, 0); err != nil {
		t.Fatalf("primary died before adopted reap: %v", err)
	}
	if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exit := awaitEvent(t, events, "child_exit")
	if exit["code"] != float64(7) {
		t.Fatalf("primary exit = %v", exit)
	}
	if _, err := io.WriteString(stdin, "{\"op\":\"dispose\",\"id\":1}\n"); err != nil {
		t.Fatal(err)
	}
	dispose := awaitEvent(t, events, "dispose")
	if dispose["state"] != stateDrained || dispose["authority"] != authorityECHILD {
		t.Fatalf("dispose = %v", dispose)
	}
	reaped := dispose["reaped"].([]any)
	for _, pid := range reaped {
		if pid == float64(adopted) {
			t.Fatalf("already reaped adopted pid in drain set: %v", reaped)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v; stderr: %s", err, stderr.String())
	}
}

func TestAdoptedTickUnexpectedWaitErrorIsAbnormal(t *testing.T) {
	sup, ticks, _, calls, _, obs, _ := tickSupervisor(t, []int{9, 10}, func(int) (int, error) { return 0, errors.New("wait failed") })
	done := make(chan int, 1)
	go func() { done <- sup.run(9, procStatus{}) }()
	awaitEvent(t, obs.events, "started")
	sendTick(t, ticks, done)
	awaitPID(t, calls, 10)
	awaitRun(t, done, 2)
	event := awaitEvent(t, obs.events, "abnormal")
	if !strings.Contains(event["reason"].(string), "adopted child wait failed") {
		t.Fatalf("abnormal: %v", event)
	}
}

func TestAdoptedTickUnexpectedPIDIsAbnormal(t *testing.T) {
	sup, ticks, _, calls, _, obs, _ := tickSupervisor(t, []int{9, 10}, func(int) (int, error) { return 11, nil })
	done := make(chan int, 1)
	go func() { done <- sup.run(9, procStatus{}) }()
	awaitEvent(t, obs.events, "started")
	sendTick(t, ticks, done)
	awaitPID(t, calls, 10)
	awaitRun(t, done, 2)
	event := awaitEvent(t, obs.events, "abnormal")
	if event["reason"] != "adopted child wait returned unexpected pid" {
		t.Fatalf("abnormal: %v", event)
	}
}
