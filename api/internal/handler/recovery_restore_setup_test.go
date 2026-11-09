package handler

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type restoreSetup struct {
	Root, Tmp, Source, Database, Backend, ToolPath, ExpectedSHA, ExpectedCount string
	Full, Node                                                                 bool
}

// Read the parsed Go flag so both -test.run=value and -test.run value work.
func restoreSubtestFilter() *regexp.Regexp {
	run := flag.Lookup("test.run")
	if run == nil {
		return nil
	}
	parts := strings.SplitN(run.Value.String(), "/", 2)
	if len(parts) != 2 {
		return nil
	}
	filter, _ := regexp.Compile(strings.SplitN(parts[1], "/", 2)[0])
	return filter
}

func restoreFullSelected() bool {
	filter := restoreSubtestFilter()
	if filter == nil {
		return false
	}
	for _, name := range []string{"E5FullHistory", "E5ReleasedFullHistory", "E5BeforePinRefusalFullHistory"} {
		if filter.MatchString(name) {
			return true
		}
	}
	return false
}

// Respect an explicitly selected restore-only E5 case, which does not need Node.
func restoreClientSelected() bool {
	filter := restoreSubtestFilter()
	return filter == nil || filter.MatchString("E5ReleasedFullHistory") || filter.MatchString("E5BeforePinRefusalFullHistory")
}

func restoreSetupFor(root, tmp, source string, full bool) restoreSetup {
	backend, _ := exec.LookPath("git-http-backend")
	if backend == "" {
		if out, err := restoreCommand(tmp, root, "git", "--exec-path"); err == nil {
			backend = filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
		}
	}
	return restoreSetup{Root: root, Tmp: tmp, Source: source, Database: os.Getenv("UZI_TEST_DATABASE_URL"), Backend: backend, ToolPath: os.Getenv("PATH"), ExpectedSHA: restoreFrozen, ExpectedCount: "2405", Full: full, Node: full}
}

// Test-only preflight runs before helpers that may skip. Errors contain only
// fixed diagnostics: neither connection strings nor child output can escape.
func restorePreflight(c restoreSetup) error {
	if c.Full {
		if c.Source == "" || !filepath.IsAbs(c.Source) {
			return errors.New("absent_source: absolute full-history source required")
		}
		if info, err := os.Stat(c.Source); err != nil || !info.IsDir() {
			return errors.New("absent_source: full-history source unavailable")
		}
		out, err := restoreCommand(c.Tmp, c.Source, "git", "rev-parse", "--is-shallow-repository")
		if err != nil {
			return errors.New("wrong_source: not a Git repository")
		}
		if strings.TrimSpace(string(out)) != "false" {
			return errors.New("shallow_source: nonshallow source required")
		}
		for _, check := range []struct {
			args []string
			want string
		}{{[]string{"rev-parse", "HEAD"}, c.ExpectedSHA}, {[]string{"rev-list", "--count", "HEAD"}, c.ExpectedCount}} {
			out, err := restoreCommand(c.Tmp, c.Source, "git", check.args...)
			if err != nil || strings.TrimSpace(string(out)) != check.want {
				return errors.New("wrong_source: frozen source identity/count mismatch")
			}
		}
	}
	if c.Node {
		found := false
		for _, dir := range filepath.SplitList(c.ToolPath) {
			if dir == "" {
				continue
			}
			if info, err := os.Stat(filepath.Join(dir, "node")); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				found = true
				break
			}
		}
		if !found {
			return errors.New("missing_node: combined proof requires Node")
		}
		if info, err := os.Stat(filepath.Join(c.Root, "agent", "node_modules", "tsx", "package.json")); err != nil || !info.Mode().IsRegular() {
			return errors.New("missing_tsx: combined proof requires installed agent tsx")
		}
	}
	if info, err := os.Stat(c.Backend); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("missing_native_backend: native git-http-backend required")
	}
	if c.Database == "" {
		return errors.New("absent_database: owned UZI_TEST_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, c.Database)
	if err != nil {
		return errors.New("inaccessible_database: owned database unavailable")
	}
	pingErr := conn.Ping(ctx)
	closeErr := conn.Close(ctx)
	if pingErr != nil || closeErr != nil {
		return errors.New("inaccessible_database: owned database unavailable")
	}
	return nil
}

func TestRecoveryRestoreSetupValidation(t *testing.T) {
	if raw := os.Getenv("RECOVERY_RESTORE_SETUP_CHILD"); raw != "" {
		var c restoreSetup
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal("setup child config")
		}
		if err := restorePreflight(c); err != nil {
			t.Fatal(err)
		}
		t.Fatal("invalid setup unexpectedly accepted")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, ".uzi", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(scratch, "restore-preflight-")
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(tmp, "good")
	restoreGit(t, tmp, tmp, "init", "--initial-branch=main", good)
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(good, "source.txt"), []byte(fmt.Sprint(i)), 0600); err != nil {
			t.Fatal(err)
		}
		restoreGit(t, tmp, good, "add", "source.txt")
		restoreGit(t, tmp, good, "commit", "-m", "fixture")
	}
	shallow := filepath.Join(tmp, "shallow")
	restoreGit(t, tmp, tmp, "clone", "--no-local", "--no-hardlinks", "--depth=1", "file://"+good, shallow)
	c := restoreSetupFor(root, tmp, good, true)
	c.ExpectedSHA = restoreGit(t, tmp, good, "rev-parse", "HEAD")
	c.ExpectedCount = "2"
	wrong := filepath.Join(tmp, "wrong")
	restoreGit(t, tmp, tmp, "clone", "--no-local", "--no-hardlinks", good, wrong)
	restoreGit(t, tmp, wrong, "checkout", "-b", "wrong-input", "HEAD~1")
	// A loopback port with no listener gives a bounded, credential-free refusal.
	inaccessible := "postgres://fixture:fixture@127.0.0.1:1/fixture?sslmode=disable&connect_timeout=1" // #nosec G101 -- Inert public fixture credentials targeting an unreachable loopback port, never a retained database.
	// Isolate invalid-input checks from real DB/Node/backend availability.
	// This executable is only stat'ed, never run as a backend or Node.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c.Node, c.Backend, c.Database = false, executable, inaccessible
	tools := filepath.Join(tmp, "tool-shapes")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "node"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil { // #nosec G306 -- Isolated test-only executable shape required for the executable-bit check; never launched.
		t.Fatal(err)
	}
	c.ToolPath = tools
	tests := []struct {
		name   string
		change func(*restoreSetup)
	}{
		{"absent_source", func(c *restoreSetup) { c.Source = "" }},
		{"wrong_source", func(c *restoreSetup) { c.Source = wrong }},
		{"shallow_source", func(c *restoreSetup) { c.Source = shallow }},
		{"absent_database", func(c *restoreSetup) { c.Database = "" }},
		{"inaccessible_database", func(c *restoreSetup) { c.Database = inaccessible }},
		{"missing_node", func(c *restoreSetup) { c.Node = true; c.ToolPath = filepath.Join(tmp, "missing-tools") }},
		{"missing_tsx", func(c *restoreSetup) { c.Node = true; c.Root = filepath.Join(tmp, "missing-artifacts") }},
		{"missing_native_backend", func(c *restoreSetup) { c.Backend = filepath.Join(tmp, "missing-backend") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := c
			tc.change(&candidate)
			err := restorePreflight(candidate)
			if err == nil || !strings.HasPrefix(err.Error(), tc.name+":") {
				t.Fatalf("expected %s diagnostic, got %v", tc.name, err)
			}
			cfg, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecoveryRestoreSetupValidation$", "-test.count=1") // #nosec G204 -- Own compiled test binary with fixed argv; fixture config remains in an allowlisted child environment.
			cmd.Dir, cmd.Env = filepath.Join(root, "api", "internal", "handler"), append(restoreEnv(tmp), "RECOVERY_RESTORE_SETUP_CHILD="+string(cfg))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.name+":") {
				t.Fatalf("requested-mode subprocess did not fail with %s; output bytes=%d", tc.name, len(out))
			}
		})
	}
}

func TestRecoveryRestoreRequestedModeValidation(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, ".uzi", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(scratch, "restore-requested-")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Selecting a named full-history proof must fail before the ordinary DB skip.
	for _, selection := range []struct{ name, pattern string }{
		{"E5ReleasedFullHistory", "^E5ReleasedFullHistory$"},
		{"E5BeforePinRefusalFullHistory", "^E5BeforePinRefusalFullHistory$"},
		{"combined", "^E5(Released|BeforePinRefusal)FullHistory$"},
		{"released_prefix", "^E5Released"},
		{"refusal_prefix", "^E5BeforePinRefusal"},
		{"original_prefix", "^E5Full"},
	} {
		t.Run(selection.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecoveryRestoreWorkflowLiveDB$/"+selection.pattern, "-test.count=1") // #nosec G204 -- Own test executable and enumerated literal subtest selectors, never shell input.
			cmd.Dir, cmd.Env = filepath.Join(root, "api", "internal", "handler"), restoreEnv(tmp)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "absent_source:") || strings.Contains(string(out), "--- SKIP") {
				t.Fatalf("selected full proof did not fail hard; output bytes=%d", len(out))
			}
		})
	}
}
