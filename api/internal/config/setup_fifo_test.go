//go:build unix

package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A <NAME>_FILE that names a FIFO with no writer must not hang /server setup
// (PRD #3760): LoadSetup runs before the command's deadline applies, and a
// plain open of such a FIFO blocks in the kernel. Setup refuses anything that
// is not a regular file, naming the variable; the timer turns a regression's
// hang into a failure.
func TestLoadSetupRefusesAFIFOSecretFileWithoutBlocking(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "UZI_SECRET_KEY"} {
		t.Run(name, func(t *testing.T) {
			setupEnv(t)
			fifo := filepath.Join(t.TempDir(), "secret.fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			t.Setenv(name, "")
			t.Setenv(name+"_FILE", fifo)
			done := make(chan error, 1)
			go func() { _, err := LoadSetup(); done <- err }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), name+"_FILE") || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("err = %v, want a refusal naming %s_FILE and a regular file", err, name)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("LoadSetup blocked opening a FIFO %s_FILE with no writer", name)
			}
		})
	}
}
