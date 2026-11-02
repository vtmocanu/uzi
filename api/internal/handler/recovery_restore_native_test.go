package handler

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// Only these nonsecret process settings cross into native Git and the CLI driver.
// HOME is preserved; all temporary output belongs to the workflow's scratch dir.
func restoreEnv(tmp string) []string {
	var env []string
	for _, key := range []string{"HOME", "PATH", "LANG", "LC_ALL", "GIT_EXEC_PATH", "GOROOT", "GOPATH", "GOCACHE"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return append(env, "TMPDIR="+tmp, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

func restoreCommand(tmp, dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 G702 -- Test-owned Git/Go executables and literal subcommands; arguments are fixture paths, never shell input.
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, restoreEnv(tmp), time.Second
	return cmd.CombinedOutput()
}

func restoreGit(t *testing.T, tmp, dir string, args ...string) string {
	t.Helper()
	argv := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com"}, args...)
	out, err := restoreCommand(tmp, dir, "git", argv...)
	if err != nil {
		t.Fatalf("native Git %s: %v: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func restoreMissing(t *testing.T, tmp, dir, bundle, prerequisite string) {
	t.Helper()
	out, err := restoreCommand(tmp, dir, "git", "bundle", "verify", bundle)
	if err == nil || !strings.Contains(string(out), prerequisite) {
		t.Fatal("bundle verification must reject and name the missing prerequisite")
	}
}

type restoreForge struct {
	forgetest.BaseFake
	project   forge.Project
	tmp, work string
}

func (f *restoreForge) GetProject(context.Context, int64) (forge.Project, error) {
	return f.project, nil
}
func (f *restoreForge) CompareAncestry(ctx context.Context, _ int64, head, sha string) (forge.Ancestry, error) {
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", sha, head) // #nosec G204 -- Fixture-generated commit IDs passed as argv to native Git, without a shell.
	cmd.Dir, cmd.Env = f.work, restoreEnv(f.tmp)
	err := cmd.Run()
	if err == nil {
		return forge.AncestryAncestor, nil
	}
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 1 {
		return forge.AncestryNotAncestor, nil
	}
	return forge.AncestryUnknown, err
}

// Discovery/ancestry are substituted; receive-pack and upload-pack use Git's
// native smart HTTP CGI backend, exactly as the workersvc native fixture does.
type restoreTraffic struct {
	listings, receivePosts, creates atomic.Int64
	mu                              sync.Mutex
	createRef, createTip            string
}

func restoreRemote(t *testing.T, tmp, root string, observations ...*restoreTraffic) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	backend, err := exec.LookPath("git-http-backend")
	if err != nil {
		backend = filepath.Join(restoreGit(t, tmp, root, "--exec-path"), "git-http-backend")
	}
	if info, err := os.Stat(backend); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("native git-http-backend required")
	}
	transferred := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
		if err != nil {
			http.Error(w, "request too large", http.StatusBadRequest)
			return
		}
		for _, observation := range observations {
			if strings.HasSuffix(r.URL.Path, "/info/refs") {
				observation.listings.Add(1)
			}
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
				observation.receivePosts.Add(1)
				// Receive commands precede the pack. Count only native create commands.
				packets := body
				for len(packets) >= 4 {
					n, parseErr := strconv.ParseUint(string(packets[:4]), 16, 16)
					if parseErr != nil || n < 4 || int(n) > len(packets) {
						break
					}
					fields := strings.Fields(strings.SplitN(string(packets[4:n]), "\x00", 2)[0])
					if len(fields) == 3 && fields[0] == strings.Repeat("0", 40) && strings.HasPrefix(fields[2], "refs/uzi-prereq/") {
						observation.creates.Add(1)
						observation.mu.Lock()
						observation.createRef, observation.createTip = fields[2], fields[1]
						observation.mu.Unlock()
					}
					packets = packets[n:]
				}
			}
		}
		cmd := exec.CommandContext(ctx, backend)
		cmd.Env = append(restoreEnv(tmp), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD="+r.Method, "PATH_INFO="+r.URL.Path, "QUERY_STRING="+r.URL.RawQuery,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"), "CONTENT_LENGTH="+strconv.Itoa(len(body)),
			"GATEWAY_INTERFACE=CGI/1.1", "SERVER_PROTOCOL=HTTP/1.1", "SERVER_NAME=localhost", "SERVER_PORT=80", "REMOTE_ADDR=127.0.0.1")
		cmd.Stdin = bytes.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			t.Error("native backend failed")
			http.Error(w, "backend failed", http.StatusInternalServerError)
			return
		}
		reader := bufio.NewReader(bytes.NewReader(out))
		headers, err := textproto.NewReader(reader).ReadMIMEHeader()
		if err != nil {
			t.Error("invalid native CGI headers")
			http.Error(w, "backend headers", http.StatusInternalServerError)
			return
		}
		for k, values := range headers {
			for _, v := range values {
				if k != "Status" {
					w.Header().Add(k, v)
				}
			}
		}
		status := http.StatusOK
		if v := headers.Get("Status"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &status); err != nil {
				t.Error(err)
				return
			}
		}
		w.WriteHeader(status)
		n, err := io.Copy(w, reader)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			transferred.Add(n)
		}
		if err != nil && ctx.Err() == nil {
			t.Error("native response copy failed")
		}
	}))
	t.Cleanup(server.Close)
	return server, transferred
}
