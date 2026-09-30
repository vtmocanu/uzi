package agentsource

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBombRepo builds a bare repo whose tip carries one blob of `size` zero bytes at
// skills/bomb/SKILL.md: git packs it to a few tens of KiB on the wire, but it inflates to `size`.
func buildBombRepo(t *testing.T, size int) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	gitRun(t, "", "git", "init", "--bare", "--initial-branch=main", bare)
	gitRun(t, "", "git", "init", "--initial-branch=main", work)
	gitRun(t, work, "git", "config", "user.email", "t@example.com")
	gitRun(t, work, "git", "config", "user.name", "t")
	gitRun(t, work, "git", "config", "commit.gpgsign", "false")
	gitRun(t, work, "git", "remote", "add", "origin", bare)
	mustWrite(t, filepath.Join(work, "skills/bomb/SKILL.md"), strings.Repeat("\x00", size))
	gitRun(t, work, "git", "add", "-A")
	gitRun(t, work, "git", "commit", "-m", "bomb")
	gitRun(t, work, "git", "push", "origin", "main:main")
	return bare
}

// TestFetchSkillFilesRefusesInflationBombBeforeDecode is the regression for the decompression
// bomb: a 20 MiB blob of zeros (well under the 48 MiB wire cap once compressed, over the 16 MiB
// per-object reconstructed cap) must be refused by the pack pre-scan with ErrPackBudget, not
// inflated into the in-memory storer and then skipped as a too_large note.
func TestFetchSkillFilesRefusesInflationBombBeforeDecode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	bare := buildBombRepo(t, 20<<20)
	_, files, notes, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare}, 1024)
	if err == nil {
		t.Fatalf("a bomb pack was accepted (files=%d notes=%v)", len(files), notes)
	}
	if !errors.Is(err, ErrPackBudget) {
		t.Fatalf("err = %v, want ErrPackBudget", err)
	}
	// The same shared fetchTip guards the role-file reader.
	if _, _, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare}); !errors.Is(err, ErrPackBudget) {
		t.Fatalf("FetchRoleFiles err = %v, want ErrPackBudget", err)
	}
}
