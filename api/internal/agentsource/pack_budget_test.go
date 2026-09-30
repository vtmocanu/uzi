package agentsource

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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
	if !strings.Contains(err.Error(), "per-object size") {
		t.Fatalf("err = %q does not name the bound that tripped", err)
	}
}

// buildTipRepo builds a bare repo whose tip carries the given path -> content files.
func buildTipRepo(t *testing.T, files map[string]string) string {
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
	for p, c := range files {
		mustWrite(t, filepath.Join(work, p), c)
	}
	gitRun(t, work, "git", "add", "-A")
	gitRun(t, work, "git", "commit", "-m", "tip")
	gitRun(t, work, "git", "push", "origin", "main:main")
	return bare
}

const roleFileBody = "---\nname: coder\ndescription: d\n---\nbody\n"

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// TestFetchRoleFilesAdmitsLargeNonRoleFile is the regression for the agent-source regression: the
// source is any allowlisted repo, so a tip with an incompressible 17 MiB non-role file (over the
// skills per-object cap, under the 48 MiB wire cap) must still sync its role files.
func TestFetchRoleFilesAdmitsLargeNonRoleFile(t *testing.T) {
	requireGit(t)
	blob := make([]byte, 17<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	bare := buildTipRepo(t, map[string]string{
		".claude/agents/coder.md": roleFileBody,
		"assets/blob.bin":         string(blob),
	})
	_, files, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare})
	if err != nil {
		t.Fatalf("FetchRoleFiles: %v", err)
	}
	if len(files) != 1 || files[0].Name != "coder.md" {
		t.Fatalf("files = %+v, want the single coder.md", files)
	}
	// The same tip is still refused for a skills source, which keeps the tight per-object cap.
	if _, _, _, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare}, 1024); !errors.Is(err, ErrPackBudget) {
		t.Fatalf("FetchSkillFiles err = %v, want ErrPackBudget", err)
	}
}

// TestFetchRoleFilesAdmitsLargeReconstructedTip: five distinct 14 MiB files (each under the skills
// per-object cap, together 70 MiB, over the skills 64 MiB total) sync for a role source.
func TestFetchRoleFilesAdmitsLargeReconstructedTip(t *testing.T) {
	requireGit(t)
	files := map[string]string{".claude/agents/coder.md": roleFileBody}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("data/zero-%d.bin", i)] = strings.Repeat("\x00", 14<<20+i)
	}
	bare := buildTipRepo(t, files)
	_, got, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare})
	if err != nil {
		t.Fatalf("FetchRoleFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("files = %+v, want the single role file", got)
	}
	_, _, _, err = FetchSkillFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare}, 1024)
	if !errors.Is(err, ErrPackBudget) || !strings.Contains(err.Error(), "total reconstructed size") {
		t.Fatalf("FetchSkillFiles err = %v, want ErrPackBudget naming the total bound", err)
	}
}

// TestFetchRoleFilesRefusesBombAboveRoleLimits: a 70 MiB blob of zeros (a few tens of KiB on the
// wire) is over the agent-source per-object cap and is still refused before it is decoded.
func TestFetchRoleFilesRefusesBombAboveRoleLimits(t *testing.T) {
	requireGit(t)
	bare := buildBombRepo(t, 70<<20)
	_, _, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare})
	if !errors.Is(err, ErrPackBudget) {
		t.Fatalf("FetchRoleFiles err = %v, want ErrPackBudget", err)
	}
	if !strings.Contains(err.Error(), "per-object size") {
		t.Fatalf("err = %q does not name the bound that tripped", err)
	}
}
