//go:build unix

package setupcmd

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/config"
)

// A --forge-token-file that names a FIFO with no writer must not hang setup
// (PR #3956 review, item 2). Opening such a FIFO for reading blocks in the
// kernel before any context-aware read can start, so the guard is the open
// itself: setup refuses anything that is not a regular file, without
// blocking. The context here is never cancelled, so only a non-blocking
// refusal makes Run return; the timer turns the old hang into a failure.
func TestRunRefusesAFIFOTokenFileWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	h := newHarness()
	args := []string{"--forge-type", "github", "--forge-base-url", testBase, "--forge-token-file", path, "--repos", "acme/app"}
	done := make(chan int, 1)
	go func() { done <- h.run(context.Background(), args...) }()
	select {
	case code := <-done:
		if code != ExitUsage {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, h.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("setup blocked opening a FIFO token file with no writer")
	}
	if !strings.Contains(h.stderr.String(), "regular file") {
		t.Errorf("stderr does not say the token file must be a regular file:\n%s", h.stderr.String())
	}
	if h.opened || h.st.writes != 0 {
		t.Errorf("opened=%v writes=%d", h.opened, h.st.writes)
	}
}

// The same hang class for the boot secrets: the production LoadConfig
// (config.LoadSetup) must refuse a FIFO DATABASE_URL_FILE without blocking,
// and Run must stop before opening the database or writing anything.
func TestRunRefusesAFIFOSecretFileWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "db.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", fifo)
	h := newHarness()
	env := h.env()
	env.LoadConfig = config.LoadSetup
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), baseArgs("--repos", "acme/app"), h.stdin, &h.stdout, &h.stderr, env)
	}()
	select {
	case code := <-done:
		if code != ExitUsage {
			t.Fatalf("exit = %d, want 2; stdout:\n%s", code, h.stdout.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("setup blocked opening a FIFO DATABASE_URL_FILE with no writer")
	}
	if out := h.stdout.String() + h.stderr.String(); !strings.Contains(out, "DATABASE_URL_FILE") {
		t.Errorf("the refusal does not name DATABASE_URL_FILE:\n%s", out)
	}
	if h.opened || h.st.writes != 0 {
		t.Errorf("opened=%v writes=%d, want neither", h.opened, h.st.writes)
	}
}
