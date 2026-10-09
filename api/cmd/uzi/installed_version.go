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

func realInstalledVersion(formula string) (string, error) {
	return probeInstalledVersion(runProcess, formula)
}

func probeInstalledVersion(run processRunner, formula string) (string, error) {
	if !allowedFormula(formula) {
		return "", errors.New("invalid installed formula")
	}
	out, err := quietProcess(run, "brew", []string{"--prefix", formula}, nil)
	if err != nil {
		return "", fmt.Errorf("formula prefix: %w", err)
	}
	prefix := strings.TrimSpace(out)
	if !filepath.IsAbs(prefix) {
		return "", errors.New("formula prefix is not absolute")
	}
	prefix, err = filepath.EvalSymlinks(prefix)
	if err != nil {
		return "", fmt.Errorf("formula prefix: %w", err)
	}
	if !validFormulaPrefix(prefix, formula) {
		return "", errors.New("foreign formula prefix")
	}
	path := filepath.Join(prefix, "bin", "uzi")
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("installed executable: %w", err)
	}
	rel, err := filepath.Rel(prefix, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("installed executable escapes formula prefix")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("installed executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("installed executable is not executable")
	}
	out, err = quietProcess(run, path, []string{"version"}, installedProbeEnv())
	if err != nil {
		return "", fmt.Errorf("installed version: %w", err)
	}
	line, _, _ := strings.Cut(out, "\n")
	if _, ok := validatedVersion(line); !ok {
		return "", errors.New("invalid installed version")
	}
	return line, nil
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
