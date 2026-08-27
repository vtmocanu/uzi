// Command roleparity is the dev-team/product role-parity NUDGE (issue #63). It
// compares this repo's dev-team roster (.claude/agents/*.md) against the product
// builtin roster (api/internal/agenttmpl/builtins/*.md) and reports roles present
// on one side and absent from the other, minus an accepted-divergence allowlist
// (scripts/role-parity-accepted.tsv). Role identity is the .md filename stem.
//
// It is a NUDGE, never a gate. By default it ALWAYS exits 0, so it is
// structurally incapable of failing a build — `task nudge:roles` runs it and is
// deliberately absent from `gate`/`gate:*`. The optional -strict flag exits 1 on
// any un-accepted divergence, for a human who opts in locally; the task target
// never passes it. See CLAUDE.md "Builtin agent templates" for the
// decoupled-by-design / nudge-not-a-gate resolution this implements.
//
// A genuine operational error (a roster directory or the accepted list missing)
// exits 2. That is not a gate failure — the target is reached by no gate — but a
// broken invocation should be loud rather than silently report parity.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// result is run's outcome: the process exit code plus the text to print. run
// builds the output as strings and RETURNS it rather than writing to os.Stdout,
// so a test can assert both the exit code and the message without capturing a
// file descriptor. main does the actual printing.
type result struct {
	code   int
	stdout string
	stderr string
}

func main() {
	res := run(os.Args[1:])
	if res.stdout != "" {
		fmt.Print(res.stdout)
	}
	if res.stderr != "" {
		fmt.Fprint(os.Stderr, res.stderr)
	}
	os.Exit(res.code)
}

// run is main's body as a testable unit: it parses args, resolves the rosters,
// and RETURNS the exit code and output rather than printing or calling os.Exit.
// Keeping the exit code a return value is what lets a test pin the load-bearing
// invariant — the default path (any divergence, no -strict) returns 0 — so a
// future regression that exits nonzero on a nudge is caught. Codes: 0 = success
// (nudge printed or rosters agree); 1 = -strict with an un-accepted divergence;
// 2 = operational error (a roster dir or the accepted list missing).
func run(args []string) result {
	fs := flag.NewFlagSet("roleparity", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	strict := fs.Bool("strict", false, "exit 1 on any un-accepted divergence (opt-in; the nudge default never does)")
	repoRoot := fs.String("repo-root", "", "repo root (default: auto-detected by walking up to the .git marker)")
	if err := fs.Parse(args); err != nil {
		return result{code: 2}
	}

	root := *repoRoot
	if root == "" {
		r, err := findRepoRoot()
		if err != nil {
			return result{code: 2, stderr: fmt.Sprintf("roleparity: %v\n", err)}
		}
		root = r
	}

	product, err := roleStems(filepath.Join(root, "api", "internal", "agenttmpl", "builtins"))
	if err != nil {
		return result{code: 2, stderr: fmt.Sprintf("roleparity: read product roster: %v\n", err)}
	}
	devteam, err := roleStems(filepath.Join(root, ".claude", "agents"))
	if err != nil {
		return result{code: 2, stderr: fmt.Sprintf("roleparity: read dev-team roster: %v\n", err)}
	}
	acc, err := readAccepted(filepath.Join(root, "scripts", "role-parity-accepted.tsv"))
	if err != nil {
		return result{code: 2, stderr: fmt.Sprintf("roleparity: read accepted list: %v\n", err)}
	}

	divs := roleParity(product, devteam, acc)
	if len(divs) == 0 {
		return result{code: 0, stdout: "role parity: dev-team and product rosters agree (modulo accepted divergences). Nothing to nudge.\n"}
	}
	out := fmt.Sprintf("role parity: %d un-accepted divergence(s) — a nudge, not a failure:\n", len(divs))
	for _, d := range divs {
		out += fmt.Sprintf("  [%s] %s — %s\n", d.side, d.role, d.msg)
	}
	code := 0
	if *strict {
		code = 1
	}
	return result{code: code, stdout: out}
}

// roleStems returns the sorted role identities in dir: the filename stem of each
// top-level *.md file. Role identity is the filename stem (issue #63).
func roleStems(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var stems []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		stems = append(stems, strings.TrimSuffix(name, ".md"))
	}
	sort.Strings(stems)
	return stems, nil
}

// readAccepted parses the tab-separated accepted-divergence allowlist. Each
// non-blank, non-comment line is `side<TAB>role<TAB>reason`, where side is
// "product-only" or "devteam-only". Blank lines and comment lines (first
// non-blank character '#', leading whitespace allowed) are ignored. The reason
// column is documentation for humans; only side+role affect the nudge.
func readAccepted(path string) (accepted, error) {
	acc := accepted{
		productOnly: map[string]bool{},
		devteamOnly: map[string]bool{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return accepted{}, err
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			return accepted{}, fmt.Errorf("%s:%d: expected 3 tab-separated columns (side, role, reason), got %d", path, i+1, len(fields))
		}
		roleSide, role := fields[0], fields[1]
		switch side(roleSide) {
		case sideProductOnly:
			acc.productOnly[role] = true
		case sideDevteamOnly:
			acc.devteamOnly[role] = true
		default:
			return accepted{}, fmt.Errorf("%s:%d: unknown side %q (want %q or %q)", path, i+1, roleSide, sideProductOnly, sideDevteamOnly)
		}
	}
	return acc, nil
}

// findRepoRoot walks up from the working directory to the first directory
// containing a .git entry (a directory in a normal clone, a file in a linked
// worktree) and returns it. It lets `task nudge:roles` run the tool from the api
// module dir while still resolving repo-root paths.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .git found walking up from working directory")
		}
		dir = parent
	}
}
