package workersvc

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Native Git supplies all object and wire semantics; no Node or git-http-backend.
type taskHeldGitFixture struct {
	origin, work, url            string
	base, checkpoint, decoy, tip string
	pack                         []byte
}

func newTaskHeldGitFixture(t *testing.T, x *taskLifecycleFixture) *taskHeldGitFixture {
	t.Helper()
	dir := t.TempDir()
	g := &taskHeldGitFixture{origin: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "work")}
	taskHeldGit(t, dir, "init", "--bare", g.origin)
	taskHeldGit(t, dir, "init", "-b", "main", g.work)
	taskHeldGit(t, g.work, "config", "user.name", "fixture")
	taskHeldGit(t, g.work, "config", "user.email", "fixture@example.test")
	taskHeldGit(t, g.work, "remote", "add", "origin", g.origin)
	commit := func(name, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(g.work, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		taskHeldGit(t, g.work, "add", "--", name)
		taskHeldGit(t, g.work, "commit", "-m", message)
		return taskHeldGit(t, g.work, "rev-parse", "HEAD")
	}
	commit("main.txt", "main\n", "main")
	taskHeldGit(t, g.work, "push", "origin", "HEAD:refs/heads/main")
	g.base = commit("work.txt", "root work\n", "work branch B")
	taskHeldGit(t, g.work, "push", "origin", "HEAD:refs/heads/"+x.rootKey)
	var payload strings.Builder
	for i := 0; i < 8192; i++ {
		fmt.Fprintf(&payload, "%04d checkpoint payload available only through child P\n", i)
	}
	body := payload.String()
	g.checkpoint = commit("child.txt", body, "child checkpoint P")
	taskHeldGit(t, g.work, "push", "origin", "HEAD:"+x.childRef)
	g.tip = commit("child.txt", body+"held continuation\n", "held work H")
	// A sibling of P is a discriminating decoy: fetching the root key cannot
	// supply P's blob, which the thin pack uses as an external delta base.
	taskHeldGit(t, g.work, "checkout", "--detach", g.base)
	g.decoy = commit("decoy.txt", "root checkpoint only\n", "root decoy D")
	taskHeldGit(t, g.work, "push", "origin", "HEAD:"+x.rootRef)

	g.pack = taskHeldGitOutput(t, g.work, g.tip+"\n^"+g.checkpoint+"\n", "pack-objects", "--stdout", "--revs", "--thin")
	if len(g.pack) == 0 {
		t.Fatal("Git produced no pack")
	}
	remote := httptest.NewServer(taskCheckpointSmartHTTP(g.origin))
	t.Cleanup(remote.Close)
	g.url = remote.URL + "/origin.git"
	return g
}

func (g *taskHeldGitFixture) ref(t *testing.T, ref, want string) {
	t.Helper()
	if got := taskHeldGit(t, g.origin, "rev-parse", ref); got != want {
		t.Fatalf("remote %s=%s want=%s", ref, got, want)
	}
}

// The shared text helper has no process/output budget. Keep the same clean Git
// environment here, bounding every new native-Git command as well as HTTP I/O.
func taskHeldGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(taskHeldGitOutput(t, dir, "", args...)))
}

func taskHeldGitOutput(t *testing.T, dir, input string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // Fixed Git executable, literal operations supplied only by this fixture, and owned paths; no shell.
	cmd.Env = taskCheckpointEnv()
	cmd.Stdin = strings.NewReader(input)
	stdout, stderr := taskCheckpointHTTPBuffer{limit: 1 << 20}, taskCheckpointHTTPBuffer{limit: 1 << 20}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture git %v: %v: %s", args, err, stderr.String())
	}
	return append([]byte(nil), stdout.Bytes()...)
}
