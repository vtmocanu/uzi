package workersvc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only persistence and the forge ancestry API are substituted. Sweep, broker
// discovery/create/readback and receive-pack use the production lane. Manifest
// facts come from this trusted fixture; this is not server bundle validation.
type prerequisiteNativeStore struct{ *prerequisiteMemory }

func (q *prerequisiteNativeStore) ListPrerequisitePendingDue(ctx context.Context, a store.ListPrerequisitePendingDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.row.State != "pending" || q.row.NextAttemptAt.Time.After(a.DueAt.Time) || a.AfterID == q.row.ID {
		return nil, nil
	}
	return []store.RecoveryPrerequisiteClosure{q.row}, nil
}

func (*prerequisiteNativeStore) ListPrerequisiteStaleInvokedDue(ctx context.Context, _ store.ListPrerequisiteStaleInvokedDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, ctx.Err()
}

func (*prerequisiteNativeStore) ListPrerequisiteReconcileDue(ctx context.Context, _ store.ListPrerequisiteReconcileDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, ctx.Err()
}

func (*prerequisiteNativeStore) ListPrerequisiteAuditDue(ctx context.Context, _ store.ListPrerequisiteAuditDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, ctx.Err()
}

func (*prerequisiteNativeStore) ListPrerequisiteCleanupDue(ctx context.Context, _ store.ListPrerequisiteCleanupDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, ctx.Err()
}

func prerequisiteNativeEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	if path := os.Getenv("GIT_EXEC_PATH"); path != "" {
		env = append(env, "GIT_EXEC_PATH="+path)
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

func prerequisiteNativeCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	argv := []string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com"}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "git", argv...) // #nosec G702 -- Literal Git subcommands and runtime-owned fixture arguments; no shell.
	cmd.Dir = dir
	cmd.Env = prerequisiteNativeEnv()
	return cmd.CombinedOutput()
}

func prerequisiteNativeOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := prerequisiteNativeCommand(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func prerequisiteNativeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(prerequisiteNativeOutput(t, dir, args...)))
}

func prerequisiteNativeFailure(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := prerequisiteNativeCommand(ctx, dir, args...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		t.Fatalf("expected native Git rejection for %v, got %v: %s", args, err, out)
	}
	t.Logf("native rejection %v:\n%s", args, out)
	return string(out)
}

func prerequisiteNativeBackend(t *testing.T) string {
	t.Helper()
	var candidates []string
	if path := os.Getenv("GIT_EXEC_PATH"); path != "" {
		candidates = append(candidates, filepath.Join(path, "git-http-backend"))
	}
	path := prerequisiteNativeGit(t, "", "--exec-path")
	candidates = append(candidates, filepath.Join(path, "git-http-backend"))
	if path, err := exec.LookPath("git-http-backend"); err == nil {
		candidates = append(candidates, path)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 { // #nosec G703 -- Git helper paths come from the trusted test environment or installed Git, never HTTP input.
			return candidate
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("git-http-backend missing; checked %v", candidates)
	}
	t.Skipf("git-http-backend missing; checked %v", candidates)
	return ""
}

type prerequisiteNativeRemote struct {
	requests atomic.Int32
	inTX     atomic.Bool
	mu       sync.Mutex
	posts    int
	commands [][3]string
}

// Every request checks the transaction flag. CGI runs synchronously with the
// request context and a five-second timeout.
func (remote *prerequisiteNativeRemote) serve(t *testing.T, backend, root string, m *prerequisiteMemory, w http.ResponseWriter, r *http.Request) {
	remote.requests.Add(1)
	if m.inTX.Load() {
		remote.inTX.Store(true)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "fixture request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		remote.mu.Lock()
		remote.posts++
		scan := pktline.NewScanner(bytes.NewReader(body))
		for scan.Scan() {
			packet := scan.Bytes()
			if len(packet) == 0 {
				break
			}
			fields := strings.Fields(strings.SplitN(string(packet), "\x00", 2)[0])
			if len(fields) == 3 {
				remote.commands = append(remote.commands, [3]string{fields[0], fields[1], fields[2]})
			}
		}
		remote.mu.Unlock()
	}
	cmd := exec.CommandContext(ctx, backend)
	cmd.Env = append(prerequisiteNativeEnv(),
		"GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "REQUEST_METHOD="+r.Method,
		"PATH_INFO="+r.URL.Path, "QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"), "CONTENT_LENGTH="+strconv.Itoa(len(body)),
		"GATEWAY_INTERFACE=CGI/1.1", "SERVER_PROTOCOL=HTTP/1.1",
		"SERVER_NAME=localhost", "SERVER_PORT=80", "REMOTE_ADDR=127.0.0.1")
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Errorf("git-http-backend: %v: %s", err, stderr.String())
		http.Error(w, "fixture backend failed", http.StatusInternalServerError)
		return
	}
	reader := bufio.NewReader(bytes.NewReader(out))
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		t.Errorf("CGI headers: %v: %s", err, out)
		http.Error(w, "fixture CGI headers failed", http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	for key, values := range header {
		if key == "Status" {
			if len(values) != 0 {
				fields := strings.Fields(values[0])
				if len(fields) != 0 {
					status, err = strconv.Atoi(fields[0])
				}
			}
			if err != nil || status < 100 || status > 599 {
				t.Errorf("invalid CGI status: %v", values)
				http.Error(w, "fixture CGI status failed", http.StatusInternalServerError)
				return
			}
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, reader); err != nil && ctx.Err() == nil {
		t.Errorf("CGI response: %v", err)
	}
}

type prerequisiteNativeFixture struct {
	s                          *Service
	m                          *prerequisiteMemory
	remote                     *prerequisiteNativeRemote
	work, bare, bundle         string
	p1, p2, source, defaultTip string
	compares                   int
}

func newPrerequisiteNativeFixture(t *testing.T, two bool) *prerequisiteNativeFixture {
	t.Helper()
	backend := prerequisiteNativeBackend(t)
	root := t.TempDir()
	x := &prerequisiteNativeFixture{work: filepath.Join(root, "work"), bare: filepath.Join(root, "forge.git"), bundle: filepath.Join(root, "source.bundle")}
	prerequisiteNativeGit(t, "", "init", "--bare", "--initial-branch=main", x.bare)
	prerequisiteNativeGit(t, "", "init", "--initial-branch=main", x.work)
	x.p1 = x.commit(t, "base.txt", "prerequisite one\n")
	prerequisiteNativeGit(t, x.work, "branch", "source")
	x.defaultTip = x.commit(t, "default.txt", "default only\n")
	if x.defaultTip == x.p1 {
		t.Fatal("fixture default must be a strict descendant of P1")
	}
	prerequisiteNativeGit(t, x.work, "remote", "add", "origin", x.bare)
	// Only default D reaches the forge. Neither S nor the unrelated P2 is pushed.
	prerequisiteNativeGit(t, x.work, "push", "origin", "main")
	prerequisiteNativeGit(t, x.work, "checkout", "source")
	if two {
		prerequisiteNativeGit(t, x.work, "checkout", "--orphan", "second")
		prerequisiteNativeGit(t, x.work, "rm", "-rf", ".")
		x.p2 = x.commit(t, "second.txt", "prerequisite two\n")
		prerequisiteNativeGit(t, x.work, "checkout", "source")
		prerequisiteNativeGit(t, x.work, "merge", "--allow-unrelated-histories", "--no-ff", "second", "-m", "merge P2")
	}
	x.source = x.commit(t, "source.txt", "captured source\n")
	args := []string{"bundle", "create", x.bundle, "HEAD", "^" + x.p1}
	if two {
		args = append(args, "^"+x.p2)
	}
	prerequisiteNativeGit(t, x.work, args...)
	prerequisiteNativeGit(t, x.work, "bundle", "verify", x.bundle)
	prerequisiteNativeGit(t, x.bare, "config", "http.receivepack", "true")
	// Backend child Git commands must also leave no detached maintenance behind.
	prerequisiteNativeGit(t, x.bare, "config", "gc.auto", "0")
	prerequisiteNativeGit(t, x.bare, "config", "maintenance.auto", "false")

	s, m, _, builder := newPrerequisiteMemory(t, 1)
	x.s, x.m = s, m
	data, err := os.ReadFile(x.bundle)
	if err != nil {
		t.Fatal(err)
	}
	m.row.PrerequisiteShas = []string{x.p1}
	m.row.Checksum = fmt.Sprintf("%x", sha256.Sum256(data))
	m.capture.PrerequisiteShas = []string{x.p1}
	m.capture.Checksum = prerequisiteText(m.row.Checksum)
	m.capture.SourceSha = x.source
	m.capture.ByteSize = pgtype.Int8{Int64: int64(len(data)), Valid: true}
	m.capture.ChunkCount = pgtype.Int4{Int32: 1, Valid: true}
	m.capture.ExpiresAt = prerequisiteTime(m.now.Add(24 * time.Hour))
	m.run.RepoID = prerequisiteID(m.row.RepoID)
	worker := uuid.New()
	m.hold.RepoID = prerequisiteID(m.row.RepoID)
	m.run.WorkerID = prerequisiteID(worker)
	m.hold.OriginalWorkerID, m.hold.OriginalWorkerIdentity = worker, "native-fixture"
	m.capture.OriginalWorkerID, m.capture.OriginalWorkerIdentity = prerequisiteID(worker), "native-fixture"
	remote := &prerequisiteNativeRemote{}
	x.remote = remote
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote.serve(t, backend, root, m, w, r)
	}))
	t.Cleanup(server.Close)
	m.repo.WebUrl, m.conn.BaseUrl = server.URL+"/forge", server.URL
	s.SetForgeBaseURLAllowed(func(base string) bool { return base == server.URL })
	s.SetPrerequisiteStore(&prerequisiteNativeStore{m}, m.transaction)
	builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(ctx context.Context, _ int64, head, sha string) (forge.Ancestry, error) {
		x.compares++
		if m.inTX.Load() {
			t.Error("native ancestry proof inside transaction")
		}
		out, err := prerequisiteNativeCommand(ctx, x.work, "merge-base", "--is-ancestor", sha, head)
		if err == nil {
			return forge.AncestryAncestor, nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
			return forge.AncestryNotAncestor, nil
		}
		return forge.AncestryUnknown, fmt.Errorf("native ancestry: %w: %s", err, out)
	}}
	return x
}

func (x *prerequisiteNativeFixture) commit(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(x.work, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	prerequisiteNativeGit(t, x.work, "add", "--", name)
	prerequisiteNativeGit(t, x.work, "commit", "-m", name)
	return prerequisiteNativeGit(t, x.work, "rev-parse", "HEAD")
}

func (x *prerequisiteNativeFixture) rewriteDefault(t *testing.T) {
	t.Helper()
	prerequisiteNativeGit(t, x.work, "checkout", "--orphan", "replacement")
	prerequisiteNativeGit(t, x.work, "rm", "-rf", ".")
	replacement := x.commit(t, "replacement.txt", "unrelated default\n")
	prerequisiteNativeGit(t, x.work, "push", "origin", "HEAD:refs/heads/replacement")
	prerequisiteNativeGit(t, x.bare, "update-ref", "refs/heads/main", replacement)
	prerequisiteNativeGit(t, x.bare, "update-ref", "-d", "refs/heads/replacement")
}

func (x *prerequisiteNativeFixture) cleanClone(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "consumer")
	prerequisiteNativeGit(t, "", "clone", "--no-local", x.bare, dir)
	return dir
}

func (x *prerequisiteNativeFixture) sweep(t *testing.T, pinned bool) {
	t.Helper()
	n, err := x.s.SweepPrerequisiteClosures(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("public sweep changed=%d error=%v row=%+v", n, err, x.m.row)
	}
	if x.remote.requests.Load() == 0 || x.remote.inTX.Load() || x.compares != 1 {
		t.Fatalf("requests=%d I/O-in-TX=%v comparisons=%d", x.remote.requests.Load(), x.remote.inTX.Load(), x.compares)
	}
	x.remote.mu.Lock()
	defer x.remote.mu.Unlock()
	r := x.m.row
	if want := "refs/uzi-prereq/" + r.CaptureID.String(); r.PinRef != want {
		t.Fatalf("capture pin ref=%s want %s", r.PinRef, want)
	}
	if !pinned {
		if r.State != "refused" || r.Reason != "prerequisite_not_on_default" || x.m.marked != 0 || x.remote.posts != 0 || len(x.remote.commands) != 0 || r.PinTip.Valid || r.LiveRunID.Valid {
			t.Fatalf("unproved create: row=%+v markers=%d posts=%d commands=%v", r, x.m.marked, x.remote.posts, x.remote.commands)
		}
		prerequisiteNativeFailure(t, x.bare, "show-ref", "--verify", r.PinRef)
		return
	}
	if r.State != "pinned" || r.PinTip.String != x.defaultTip || !r.PinTip.Valid || r.LiveRunID != prerequisiteID(r.RunID) || x.m.marked != 1 {
		t.Fatalf("pin outcome: row=%+v markers=%d", r, x.m.marked)
	}
	want := [3]string{strings.Repeat("0", 40), x.defaultTip, r.PinRef}
	if x.remote.posts != 1 || len(x.remote.commands) != 1 || x.remote.commands[0] != want {
		t.Fatalf("receive-pack posts=%d commands=%v want one %v", x.remote.posts, x.remote.commands, want)
	}
	if got := prerequisiteNativeGit(t, x.bare, "rev-parse", r.PinRef); got != x.defaultTip {
		t.Fatalf("actual pin=%s want D=%s", got, x.defaultTip)
	}
}

func prerequisiteNativeMissing(t *testing.T, consumer, bundle, sha string) {
	t.Helper()
	prerequisiteNativeFailure(t, consumer, "cat-file", "-e", sha)
	for _, args := range [][]string{
		{"bundle", "verify", bundle},
		{"fetch", bundle, "HEAD:refs/heads/restored"},
	} {
		if out := prerequisiteNativeFailure(t, consumer, args...); !strings.Contains(out, sha) {
			t.Fatalf("native rejection does not name missing %s: %s", sha, out)
		}
	}
	prerequisiteNativeFailure(t, consumer, "show-ref", "--verify", "refs/heads/restored")
}

// This control models the baseline without any anchor, rather than merely
// leaving an existing pin unfetched by the consumer.
func TestPrerequisiteNativeWithoutPinCannotRestoreAfterDefaultRewrite(t *testing.T) {
	x := newPrerequisiteNativeFixture(t, false)
	x.rewriteDefault(t)
	prerequisiteNativeFailure(t, x.bare, "show-ref", "--verify", x.m.row.PinRef)
	prerequisiteNativeMissing(t, x.cleanClone(t), x.bundle, x.p1)
	if x.remote.requests.Load() != 0 || x.m.marked != 0 || x.m.row.LiveRunID.Valid {
		t.Fatal("no-pin control unexpectedly invoked the lane")
	}
}

func TestPrerequisiteNativeSweepRestoresAfterDefaultRewrite(t *testing.T) {
	x := newPrerequisiteNativeFixture(t, false)
	x.sweep(t, true)
	x.rewriteDefault(t)
	consumer := x.cleanClone(t)
	// Controlled no-pin variant: the same consumer must fail before pin fetch.
	prerequisiteNativeMissing(t, consumer, x.bundle, x.p1)
	pin := x.m.row.PinRef
	prerequisiteNativeGit(t, consumer, "fetch", "origin", pin+":"+pin)
	prerequisiteNativeGit(t, consumer, "bundle", "verify", x.bundle)
	prerequisiteNativeGit(t, consumer, "fetch", x.bundle, "HEAD:refs/heads/restored")
	for _, revision := range []string{"refs/heads/restored", "refs/heads/restored^{tree}"} {
		want := x.source
		if strings.HasSuffix(revision, "^{tree}") {
			want = prerequisiteNativeGit(t, x.work, "rev-parse", x.source+"^{tree}")
		}
		if got := prerequisiteNativeGit(t, consumer, "rev-parse", revision); got != want {
			t.Fatalf("restored %s=%s want %s", revision, got, want)
		}
	}
	if got := string(prerequisiteNativeOutput(t, consumer, "show", "refs/heads/restored:source.txt")); got != "captured source\n" {
		t.Fatalf("restored source file=%q", got)
	}
}

func TestPrerequisiteNativeSweepRefusesRewriteBeforePreparation(t *testing.T) {
	x := newPrerequisiteNativeFixture(t, false)
	x.rewriteDefault(t)
	x.sweep(t, false)
	prerequisiteNativeMissing(t, x.cleanClone(t), x.bundle, x.p1)
}

func TestPrerequisiteNativeSweepCannotCoverUndeclaredPrerequisite(t *testing.T) {
	x := newPrerequisiteNativeFixture(t, true)
	// The trusted declaration deliberately omits P2. T3/T4 prove only the
	// declared P1; native restore exposes the T1/T2 trust boundary.
	x.sweep(t, true)
	x.rewriteDefault(t)
	for _, sha := range strings.Fields(prerequisiteNativeGit(t, x.bare, "rev-list", "--all")) {
		if sha == x.p2 {
			t.Fatal("P2 leaked into forge ref reachability")
		}
	}
	prerequisiteNativeFailure(t, x.bare, "cat-file", "-e", x.p2)
	consumer := x.cleanClone(t)
	pin := x.m.row.PinRef
	prerequisiteNativeGit(t, consumer, "fetch", "origin", pin+":"+pin)
	prerequisiteNativeGit(t, consumer, "cat-file", "-e", x.p1)
	prerequisiteNativeMissing(t, consumer, x.bundle, x.p2)
	prerequisiteNativeFailure(t, consumer, "cat-file", "-e", x.source)
}
