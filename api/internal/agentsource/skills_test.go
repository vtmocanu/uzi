package agentsource

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSkillFileKeepsOnlyNameDescriptionBody (PRD #1909 D9): every other frontmatter key is
// stripped, the capability-granting ones included, and the body is carried verbatim below it.
func TestParseSkillFileKeepsOnlyNameDescriptionBody(t *testing.T) {
	raw := "---\n" +
		"name: brand-voice\n" +
		"description: \"How this product writes\"\n" +
		"allowed-tools: Bash, WebFetch\n" +
		"tools: [Bash]\n" +
		"model: opus\n" +
		"hooks: {PreToolUse: rm -rf}\n" +
		"disable-model-invocation: true\n" +
		"---\n" +
		"\n" +
		"# Brand voice\nBe brief.\n"
	got := ParseSkillFile([]byte(raw), "brand-voice")
	if !got.OK {
		t.Fatalf("ParseSkillFile rejected a valid skill: %+v", got)
	}
	if got.Name != "brand-voice" || got.Description != "How this product writes" {
		t.Errorf("name/description = %q / %q", got.Name, got.Description)
	}
	if got.Body != "# Brand voice\nBe brief.\n" {
		t.Errorf("body = %q, want the text below the frontmatter", got.Body)
	}
	// The result type has no field for any other key: prove the values did not leak into the
	// three it has.
	for _, field := range []string{got.Name, got.Description, got.Body} {
		for _, stripped := range []string{"allowed-tools", "WebFetch", "PreToolUse", "rm -rf", "disable-model-invocation"} {
			if strings.Contains(field, stripped) {
				t.Errorf("a stripped frontmatter value %q survived in %q", stripped, field)
			}
		}
	}
}

func TestParseSkillFileIdentityAndRejections(t *testing.T) {
	parse := func(raw, dir string) SkillResult {
		t.Helper()
		return ParseSkillFile([]byte(raw), dir)
	}
	t.Run("the directory name is the identity when the frontmatter has no name", func(t *testing.T) {
		got := parse("---\ndescription: d\n---\nbody\n", "from-dir")
		if !got.OK || got.Name != "from-dir" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the frontmatter name wins over the directory", func(t *testing.T) {
		got := parse("---\nname: declared\ndescription: d\n---\nbody\n", "dir")
		if !got.OK || got.Name != "declared" {
			t.Fatalf("got %+v", got)
		}
	})
	for name, raw := range map[string]string{
		"no frontmatter":           "just a body\n",
		"unterminated":             "---\nname: a\ndescription: d\nbody\n",
		"empty description":        "---\nname: a\ndescription:\n---\nbody\n",
		"missing description":      "---\nname: a\n---\nbody\n",
		"empty body":               "---\nname: a\ndescription: d\n---\n   \n",
		"bidi in description":      "---\nname: a\ndescription: evil\u202etext\n---\nbody\n",
		"oversized description":    "---\nname: a\ndescription: " + strings.Repeat("d", MaxDescriptionLen+1) + "\n---\nbody\n",
		"name not kebab-case":      "---\nname: Not Kebab\ndescription: d\n---\nbody\n",
		"path-shaped name":         "---\nname: ../escape\ndescription: d\n---\nbody\n",
		"name with a dot":          "---\nname: a.b\ndescription: d\n---\nbody\n",
		"name too long":            "---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\nbody\n",
		"invalid utf-8":            "---\nname: a\ndescription: d\xff\n---\nbody\n",
		"block scalar description": "---\nname: a\ndescription: |\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := parse(raw, "a"); got.OK {
				t.Fatalf("ParseSkillFile accepted %q: %+v", name, got)
			}
		})
	}
}

// buildSkillsRepo creates a bare repo whose tip carries skills under both roots, a same-named
// duplicate, an oversized skill, a directory with no SKILL.md, a stray file, and a tag.
func buildSkillsRepo(t *testing.T) string {
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

	skill := func(name string) string {
		return "---\nname: " + name + "\ndescription: " + name + " playbook\nallowed-tools: Bash\n---\n\nbody of " + name + "\n"
	}
	mustWrite(t, filepath.Join(work, "skills/alpha/SKILL.md"), skill("alpha"))
	mustWrite(t, filepath.Join(work, "skills/beta/SKILL.md"), skill("beta"))
	mustWrite(t, filepath.Join(work, ".claude/skills/gamma/SKILL.md"), skill("gamma"))
	// The same directory name under both roots: skills/ wins, the .claude copy is a duplicate.
	mustWrite(t, filepath.Join(work, ".claude/skills/alpha/SKILL.md"), skill("alpha"))
	mustWrite(t, filepath.Join(work, "skills/big/SKILL.md"), "---\nname: big\ndescription: d\n---\n"+strings.Repeat("x", 5000))
	mustWrite(t, filepath.Join(work, "skills/no-skill-file/README.md"), "not a skill")
	mustWrite(t, filepath.Join(work, "skills/stray.md"), "a file, not a directory")
	mustWrite(t, filepath.Join(work, "skills/alpha/extra/SKILL.md"), skill("nested")) // never read
	gitRun(t, work, "git", "add", "-A")
	gitRun(t, work, "git", "commit", "-m", "seed")
	gitRun(t, work, "git", "push", "origin", "main:main")
	gitRun(t, work, "git", "tag", "v1")
	gitRun(t, work, "git", "push", "origin", "v1")
	return bare
}

func TestFetchSkillFilesFromFixtureRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	bare := buildSkillsRepo(t)
	url := "file://" + bare

	sha, files, notes, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: url}, 1024)
	if err != nil {
		t.Fatalf("FetchSkillFiles: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("sha = %q, want a 40-hex commit id", sha)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Dir] = true
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !got[want] {
			t.Errorf("skill %q was not read; got %v", want, got)
		}
	}
	for _, not := range []string{"no-skill-file", "stray.md", "extra", "big"} {
		if got[not] {
			t.Errorf("%q must not be returned as a skill file; got %v", not, got)
		}
	}
	if len(files) != 3 {
		t.Errorf("files = %d, want 3 (alpha once, beta, gamma)", len(files))
	}
	var sawDup, sawBig bool
	for _, n := range notes {
		switch {
		case n.Reason == NoteDuplicate && n.Name == "alpha":
			sawDup = true
		case n.Reason == NoteTooLarge && n.Name == "big":
			sawBig = true
		}
	}
	if !sawDup || !sawBig {
		t.Errorf("notes = %+v, want a duplicate note for alpha and a too_large note for big", notes)
	}

	// A tag resolves and a missing ref errors, as for the role reader.
	if shaTag, _, _, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: url, Ref: "v1"}, 1024); err != nil || shaTag != sha {
		t.Errorf("tag fetch = %q, %v; want %q", shaTag, err, sha)
	}
	if _, _, _, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: url, Ref: "nope"}, 1024); err == nil {
		t.Error("a missing ref must error")
	}
	if _, _, _, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: url}, 0); err == nil {
		t.Error("a non-positive size cap must error")
	}
}

// TestFetchSkillFilesEmptyRepoIsAValidEmptySet: a repo with neither root is an empty set, not an
// error (an admin then sees zero skills, and can still approve that).
func TestFetchSkillFilesEmptyRepoIsAValidEmptySet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	bare := buildFixtureRepo(t) // .claude/agents only
	sha, files, _, err := FetchSkillFiles(context.Background(), CloneOptions{CloneURL: "file://" + bare}, 1024)
	if err != nil || len(sha) != 40 || len(files) != 0 {
		t.Fatalf("FetchSkillFiles = %q, %d files, %v; want a sha and no files", sha, len(files), err)
	}
}

// TestFetchSkillFilesErrorNeverCarriesTheToken: the clone token is scrubbed from every error text,
// on a transport failure, a bad ref and a bad URL. The token is assembled at run time so no
// token-shaped literal sits in source.
func TestFetchSkillFilesErrorNeverCarriesTheToken(t *testing.T) {
	token := "pat" + "-" + strings.Repeat("Zq9", 8)
	absent := "file://" + filepath.Join(t.TempDir(), "absent.git")
	for name, opts := range map[string]CloneOptions{
		"unreachable source": {CloneURL: absent, Token: token},
		"invalid ref":        {CloneURL: absent, Ref: "bad ref~" + token, Token: token},
		"invalid url":        {CloneURL: "https://" + token + " bad", Token: token},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := FetchSkillFiles(context.Background(), opts, 1024)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("the error text carries the clone token: %v", err)
			}
		})
	}
}
