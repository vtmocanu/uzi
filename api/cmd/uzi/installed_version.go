package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// processRunner lets tests inspect the production commands and drive their output
// writers without starting Brew or a second CLI.
type processRunner func(context.Context, *exec.Cmd) error

func runProcess(_ context.Context, cmd *exec.Cmd) error { return cmd.Run() }

type probeCapture struct {
	mu       sync.Mutex
	stdout   bytes.Buffer
	used     int
	overflow bool
	cancel   context.CancelFunc
}
type probeStream struct {
	capture *probeCapture
	stdout  bool
}

func (s probeStream) Write(p []byte) (int, error) {
	c := s.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overflow {
		return 0, errBrewProbeOverflow
	}
	remaining := 64*1024 - c.used
	n := len(p)
	if n > remaining {
		n = remaining
		c.overflow = true
		c.cancel()
	}
	c.used += n
	if s.stdout {
		_, _ = c.stdout.Write(p[:n])
	}
	if c.overflow {
		return n, errBrewProbeOverflow
	}
	return n, nil
}

// Each subprocess gets its own ten-second deadline and shared 64 KiB output
// budget. An overflow fails the entire observation, including otherwise valid stdout.
func quietProcess(run processRunner, path string, args []string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // Internal Brew arguments or a validated absolute formula executable, never PATH uzi.
	// The caller may still be inside a deleted worktree; probes need no local files.
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	cmd.Env = env
	c := &probeCapture{cancel: cancel}
	cmd.Stdout = probeStream{c, true}
	cmd.Stderr = probeStream{c, false}
	err := run(ctx, cmd)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overflow {
		return "", errBrewProbeOverflow
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return c.stdout.String(), nil
}

func allowedFormula(formula string) bool { return formula == "uzi-cli" || formula == "uzi-cli-rc" }

func validFormulaPrefix(prefix, formula string) bool {
	return filepath.IsAbs(prefix) && ((filepath.Base(filepath.Dir(filepath.Dir(prefix))) == "Cellar" && filepath.Base(filepath.Dir(prefix)) == formula) ||
		(filepath.Base(prefix) == formula && filepath.Base(filepath.Dir(prefix)) == "opt"))
}

func installedProbeEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+3)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "UZI_SKILL_AUTO_UPGRADE" && key != "UZI_VERSION_CHECK" && key != "UZI_URL" {
			out = append(out, entry)
		}
	}
	return append(out, "UZI_SKILL_AUTO_UPGRADE=0", "UZI_VERSION_CHECK=0", "UZI_URL=://")
}

// installedCLI is the formula-owned executable a probe verified and the version it
// reported. Path is the symlink-resolved file inside the formula prefix: the version
// probe ran it, and the post-upgrade relaunch execs the same file.
type installedCLI struct {
	Version string
	Path    string
}

func realInstalledCLI(formula string) (installedCLI, error) {
	return probeInstalledCLI(runProcess, formula)
}

func probeInstalledCLI(run processRunner, formula string) (installedCLI, error) {
	if !allowedFormula(formula) {
		return installedCLI{}, errors.New("invalid installed formula")
	}
	out, err := quietProcess(run, "brew", []string{"--prefix", formula}, nil)
	if err != nil {
		return installedCLI{}, fmt.Errorf("formula prefix: %w", err)
	}
	prefix := strings.TrimSpace(out)
	if !filepath.IsAbs(prefix) {
		return installedCLI{}, errors.New("formula prefix is not absolute")
	}
	prefix, err = filepath.EvalSymlinks(prefix)
	if err != nil {
		return installedCLI{}, fmt.Errorf("formula prefix: %w", err)
	}
	if !validFormulaPrefix(prefix, formula) {
		return installedCLI{}, errors.New("foreign formula prefix")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(prefix, "bin", "uzi"))
	if err != nil {
		return installedCLI{}, fmt.Errorf("installed executable: %w", err)
	}
	rel, err := filepath.Rel(prefix, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return installedCLI{}, errors.New("installed executable escapes formula prefix")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return installedCLI{}, fmt.Errorf("installed executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return installedCLI{}, errors.New("installed executable is not executable")
	}
	out, err = quietProcess(run, resolved, []string{"version"}, installedProbeEnv())
	if err != nil {
		return installedCLI{}, fmt.Errorf("installed version: %w", err)
	}
	line, _, _ := strings.Cut(out, "\n")
	if _, ok := validatedVersion(line); !ok {
		return installedCLI{}, errors.New("invalid installed version")
	}
	return installedCLI{Version: line, Path: resolved}, nil
}

// Reject size before sanitizing; truncation must never turn invalid output into a
// valid version. Models keep only the sanitized, single-v canonical display value.
func validatedVersion(raw string) (string, bool) {
	if len(raw) > 1024 || len([]rune(raw)) > 256 {
		return "", false
	}
	clean := cellText(raw)
	clean = "v" + strings.TrimPrefix(clean, "v")
	if _, ok := uzicli.CompareServerVersion(clean, clean); !ok {
		return "", false
	}
	return clean, true
}

var _ io.Writer = probeStream{}
