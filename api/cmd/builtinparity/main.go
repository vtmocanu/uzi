// Command builtinparity is the builtin/upstream body-parity NUDGE (PRD #1849 M4).
// Every builtin in api/internal/agenttmpl/builtins/ except lead.md is meant to be
// a byte-for-byte copy of the upstream role library's published product-agents/
// file at the commit pinned in api/internal/agenttmpl/library/manifest.json.
// uzi-only rules live in the worker's prompt append and uzi-only tools in
// agenttmpl's productToolDelta, so nothing uzi-specific belongs in the file and a
// byte compare is the whole check. It reports each builtin that differs from, or
// is missing, its upstream counterpart.
//
// It is a NUDGE, never a gate: it exits 0 whatever it finds, and `task
// nudge:builtins` is absent from `gate`/`gate:*`. It exits 2 only when the
// instrument itself is broken (a directory missing or unreadable, or an upstream
// directory with no role files), so a broken invocation never reads as parity.
// scripts/builtin-parity.sh fetches the pinned upstream and runs it.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

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

// productOnly are builtins with no upstream counterpart by design.
var productOnly = map[string]bool{"lead": true}

func run(args []string) result {
	fs := flag.NewFlagSet("builtinparity", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "directory holding the upstream product-agents/*.md files (required)")
	builtins := fs.String("builtins", "internal/agenttmpl/builtins", "directory holding the builtin *.md files")
	if err := fs.Parse(args); err != nil {
		return result{code: 2}
	}
	if *upstream == "" {
		return result{code: 2, stderr: "builtinparity: -upstream is required\n"}
	}
	shipped, err := readRoles(*builtins)
	if err != nil {
		return result{code: 2, stderr: fmt.Sprintf("builtinparity: builtins: %v\n", err)}
	}
	up, err := readRoles(*upstream)
	if err != nil {
		return result{code: 2, stderr: fmt.Sprintf("builtinparity: upstream: %v\n", err)}
	}
	if len(shipped) == 0 {
		return result{code: 2, stderr: fmt.Sprintf("builtinparity: builtins: no role files in %s\n", *builtins)}
	}
	if len(up) == 0 {
		return result{code: 2, stderr: fmt.Sprintf("builtinparity: upstream: no role files in %s\n", *upstream)}
	}

	names := make([]string, 0, len(shipped))
	for n := range shipped {
		if !productOnly[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var out strings.Builder
	drift := 0
	for _, n := range names {
		u, ok := up[n]
		switch {
		case !ok:
			drift++
			fmt.Fprintf(&out, "MISSING  %s: no upstream product-agents/%s.md\n", n, n)
		case !bytes.Equal(shipped[n], u):
			drift++
			fmt.Fprintf(&out, "DIFFERS  %s: %s\n", n, firstDiff(shipped[n], u))
		}
	}
	if drift == 0 {
		fmt.Fprintf(&out, "builtins match upstream (%d roles; lead is product-only)\n", len(names))
	} else {
		fmt.Fprintf(&out, "\n%d of %d builtins drift from upstream. Copy product-agents/<role>.md from the pinned commit; put uzi-only rules in agent/src/prompt.ts and uzi-only tools in agenttmpl/tool_delta.go.\n", drift, len(names))
	}
	return result{code: 0, stdout: out.String()}
}

// readRoles maps each *.md file stem in dir to its bytes. Reads go through an
// os.Root, so a symlinked entry cannot reach outside dir.
func readRoles(dir string) (map[string][]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		b, err := root.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		out[strings.TrimSuffix(e.Name(), ".md")] = b
	}
	return out, nil
}

// firstDiff names the first differing line, so the report points at the drift.
func firstDiff(a, b []byte) string {
	al := strings.Split(string(a), "\n")
	bl := strings.Split(string(b), "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("first difference at line %d", i+1)
		}
	}
	return "differs only in trailing bytes"
}
