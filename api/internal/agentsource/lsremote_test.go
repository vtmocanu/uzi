package agentsource

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPickLatestSemverTag is the PURE unit test of the semver selection rule (Decision
// 4), no git remote. It is the discriminating check: a lexical/string compare of
// "v1.10.0" vs "v1.2.0"/"v1.9.0" would pick v1.9.0 (or v1.2.0), while semver precedence
// picks v1.10.0. Both operands of every Compare are re-prefixed + IsValid-guarded, so a
// non-semver tag ("latest") is skipped, never compared.
func TestPickLatestSemverTag(t *testing.T) {
	// Case 1 — no prerelease: v1.10.0 must win over v1.9.0 and v1.2.0 (a lexical compare
	// gets this wrong), and the non-semver "latest" is ignored.
	got := pickLatestSemverTag(tagSet("v1.10.0", "v1.2.0", "v1.9.0", "latest"))
	if got != "v1.10.0" {
		t.Errorf("latest of {v1.10.0,v1.2.0,v1.9.0,latest} = %q, want v1.10.0 (semver, not lexical)", got)
	}

	// Case 2 — with a prerelease of a HIGHER major. Per semver precedence, v2.0.0-rc.1 is
	// greater than any 1.x release (a prerelease sits below its OWN 2.0.0 release, but a
	// prerelease of 2.0.0 is still above 1.10.0), so our rule returns v2.0.0-rc.1 as the
	// overall latest. This documents what the rule does with a prerelease: it is a valid
	// semver, so it is NOT skipped, and it wins here on major precedence.
	got = pickLatestSemverTag(tagSet("v1.10.0", "v1.2.0", "v1.9.0", "latest", "v2.0.0-rc.1"))
	if got != "v2.0.0-rc.1" {
		t.Errorf("latest incl. prerelease = %q, want v2.0.0-rc.1 (2.0.0-rc.1 > 1.10.0 by major)", got)
	}

	// A tag without the "v" prefix is still valid semver after re-prefixing; the ORIGINAL
	// advertised string is returned, not the normalized candidate.
	got = pickLatestSemverTag(tagSet("1.10.0", "1.2.0"))
	if got != "1.10.0" {
		t.Errorf("bare (no-v) tags: latest = %q, want the original string 1.10.0", got)
	}

	// No valid semver → "" (nil err at the ResolveLatestTag layer).
	if got := pickLatestSemverTag(tagSet("latest", "nightly", "release")); got != "" {
		t.Errorf("no valid semver tag: latest = %q, want empty", got)
	}
	if got := pickLatestSemverTag(nil); got != "" {
		t.Errorf("nil tag map: latest = %q, want empty", got)
	}
}

// tagSet builds a tag-name -> sha map from names; the sha value is irrelevant to the
// semver selection, so a constant placeholder suffices.
func tagSet(names ...string) map[string]string {
	m := make(map[string]string, len(names))
	for _, n := range names {
		m[n] = "0000000000000000000000000000000000000000"
	}
	return m
}

// TestResolveLatestTagFromFixtureRepo drives ListRemoteRefs/ResolveLatestTag against a
// real bare repo over file:// (the #602 FetchRoleFiles test pattern), bypassing the https
// allowlist exactly as #602 tests the fetch path. It asserts the DISCRIMINATING result:
// ResolveLatestTag returns v1.10.0, not the lexically-last v1.2.0.
func TestResolveLatestTagFromFixtureRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	bare := buildTagFixtureRepo(t)
	url := "file://" + bare

	// ListRemoteRefs: the advertised set carries every tag + a non-empty HEAD, from a
	// single ref advertisement (no pack fetch — it returns fast).
	refs, err := ListRemoteRefs(context.Background(), CloneOptions{CloneURL: url})
	if err != nil {
		t.Fatalf("ListRemoteRefs: %v", err)
	}
	if len(refs.HeadSHA) != 40 {
		t.Errorf("HeadSHA should be a 40-hex tip; got %q", refs.HeadSHA)
	}
	for _, want := range []string{"v1.0.0", "v1.2.0", "v1.10.0", "latest"} {
		if _, ok := refs.Tags[want]; !ok {
			t.Errorf("advertised tags missing %q; got %v", want, keys(refs.Tags))
		}
	}
	if _, ok := refs.Branches["main"]; !ok {
		t.Errorf("advertised branches missing main; got %v", keys(refs.Branches))
	}

	// ResolveLatestTag: v1.10.0 wins (semver), not v1.2.0 (lexical). The point of the
	// fixture.
	tag, err := ResolveLatestTag(context.Background(), CloneOptions{CloneURL: url})
	if err != nil {
		t.Fatalf("ResolveLatestTag: %v", err)
	}
	if tag != "v1.10.0" {
		t.Errorf("ResolveLatestTag = %q, want v1.10.0 (a lexical compare returns v1.2.0)", tag)
	}
}

// TestListRemoteRefsInvalidURL confirms the advertise path PAT-scrubs and errors cleanly
// on an unparseable URL, never panicking (no git binary needed).
func TestListRemoteRefsInvalidURL(t *testing.T) {
	if _, err := ListRemoteRefs(context.Background(), CloneOptions{CloneURL: "::::not a url"}); err == nil {
		t.Errorf("an invalid clone url must error")
	}
	if _, err := ResolveLatestTag(context.Background(), CloneOptions{CloneURL: "::::not a url"}); err == nil {
		t.Errorf("ResolveLatestTag on an invalid url must error")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// buildTagFixtureRepo creates a bare repo carrying tags v1.0.0/v1.2.0/v1.10.0 plus a
// non-semver tag "latest", and returns the bare repo path for a file:// clone. Distinct
// from reconcile_test.go's buildFixtureRepo (which carries a single tag v1) — this one
// exists to exercise the semver selection over a discriminating tag set.
func buildTagFixtureRepo(t *testing.T) string {
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

	mustWrite(t, filepath.Join(work, ".claude/agents/coder.md"), "---\nname: coder\ndescription: builds\n---\nbody\n")
	gitRun(t, work, "git", "add", "-A")
	gitRun(t, work, "git", "commit", "-m", "seed")
	gitRun(t, work, "git", "push", "origin", "main:main")

	// An annotated tag (needs a message) for the winner and lightweight tags for the
	// rest, so both the Peeled (annotated) and plain (lightweight) advertisement paths
	// are exercised.
	gitRun(t, work, "git", "tag", "-a", "v1.10.0", "-m", "release 1.10.0")
	gitRun(t, work, "git", "tag", "v1.0.0")
	gitRun(t, work, "git", "tag", "v1.2.0")
	gitRun(t, work, "git", "tag", "latest")
	gitRun(t, work, "git", "push", "origin", "--tags")
	return bare
}
