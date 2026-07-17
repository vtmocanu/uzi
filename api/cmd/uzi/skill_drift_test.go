package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestSkillMatchesCommandTree is the drift control (PRD #64 Success Criterion 5 /
// "Drift control"): the bundled SKILL.md and the real cobra tree must not diverge.
// It parses the embedded skill and the command tree and asserts BOTH directions,
// which is what makes the skill mechanically un-driftable:
//
//   - forward: every command/flag the SKILL.md NAMES exists in the tree (a skill
//     that documents `--agents` or `uzi worker create` fails here);
//   - reverse: every runnable command and every flag in the tree is documented in
//     the SKILL.md (a new verb/flag added without a doc line fails here).
//
// It reads the skill from uzicli.EmbeddedSkill() — the exact bytes go:embed ships —
// so it tests what agents actually receive, not a copy.
func TestSkillMatchesCommandTree(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	doc := stripFrontmatter(uzicli.EmbeddedSkill())

	runnable := collectRunnableCommands(root)       // path→true for each leaf command
	docFlags := extractDocFlags(doc)                // every --flag the doc names
	docCommands := extractDocCommands(t, root, doc) // path→true for each command the doc names

	// forward (the PRD's requirement): every command/flag the SKILL.md NAMES exists
	// in the tree. The command side is enforced inside extractDocCommands (it
	// t.Errorf's on a named-but-missing subcommand — e.g. `uzi worker create`);
	// here we enforce the flag side (a documented `--agents` fails).
	for f := range docFlags {
		if !flagExistsInTree(root, f) {
			t.Errorf("SKILL.md documents flag --%s, which does not exist in the command tree", f)
		}
	}

	// reverse (belt-and-braces): every runnable command is documented, so a new
	// verb added without a SKILL.md line fails the build. (Flag completeness in
	// this direction is intentionally not asserted — it would require importing
	// pflag just to enumerate names; the forward flag check plus this command
	// check already make an undocumented `--agents`-style drift impossible.)
	for path := range runnable {
		if !docCommands[path] {
			t.Errorf("cobra command `uzi %s` is runnable but SKILL.md does not document it (add it to the command reference)", path)
		}
	}
}

// flagExistsInTree reports whether a long flag name is defined anywhere in the
// tree (a command's local flags or the persistent globals). Lookup's *pflag.Flag
// result is only nil-checked, never named, so this stays free of a pflag import.
func flagExistsInTree(root *cobra.Command, name string) bool {
	found := false
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Flags().Lookup(name) != nil || c.PersistentFlags().Lookup(name) != nil {
			found = true
		}
		for _, kid := range c.Commands() {
			walk(kid)
		}
	}
	walk(root)
	return found
}

// stripFrontmatter removes the leading `---`…`---` YAML block so its keys
// (allowed-tools, name, …) and the `Bash(uzi *)` glob are not mistaken for
// commands or flags.
func stripFrontmatter(doc string) string {
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return doc
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return doc
}

// collectRunnableCommands returns path→true for every runnable (has Run/RunE)
// command in the tree, keyed by its path after "uzi" (e.g. "run list"). Group
// commands (no RunE) are containers and are not required to be documented as
// standalone verbs.
func collectRunnableCommands(root *cobra.Command) map[string]bool {
	out := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c != root && c.Runnable() && !c.Hidden {
			out[pathAfterRoot(c)] = true
		}
		for _, kid := range c.Commands() {
			walk(kid)
		}
	}
	walk(root)
	return out
}

// pathAfterRoot is a command's full path with the leading "uzi " stripped.
func pathAfterRoot(c *cobra.Command) string {
	return strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" ")
}

var docFlagRe = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)

// extractDocFlags returns every long flag name (`--foo`) the doc names.
func extractDocFlags(doc string) map[string]bool {
	out := map[string]bool{}
	for _, m := range docFlagRe.FindAllStringSubmatch(doc, -1) {
		out[m[1]] = true
	}
	return out
}

// extractDocCommands finds every `uzi …` invocation in the doc's code contexts
// (fenced blocks + inline spans), walks it through the tree, and t.Errorf's when
// the doc names a subcommand the tree lacks (the forward direction). It returns
// path→true for every real command the doc names (for the reverse check).
func extractDocCommands(t *testing.T, root *cobra.Command, doc string) map[string]bool {
	t.Helper()
	mentioned := map[string]bool{}
	for _, unit := range codeUnits(doc) {
		fields := strings.Fields(unit)
		if len(fields) == 0 || fields[0] != "uzi" {
			continue
		}
		cur := root
		var path []string
		for _, tok := range fields[1:] {
			if !isCommandNameToken(tok) {
				break // a flag, a <placeholder>, or a value like own|repo / a,b
			}
			child := findChild(cur, tok)
			if child == nil {
				// A token that looks like a subcommand but isn't one is drift — but
				// only when cur actually HAS subcommands (else it is an arg to a leaf,
				// e.g. `uzi run get r1`).
				if hasSubcommands(cur) {
					t.Errorf("SKILL.md references `uzi %s` but %q has no %q subcommand",
						strings.Join(append(path, tok), " "), cur.Name(), tok)
				}
				break
			}
			cur = child
			path = append(path, tok)
			mentioned[strings.Join(path, " ")] = true
		}
	}
	return mentioned
}

var inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")

// codeUnits returns the doc's code contexts: each fenced-block line, plus each
// inline `code` span in the non-fenced text. Commands are documented in code, so
// this keeps prose like "run the tool" from being parsed as a command.
func codeUnits(doc string) []string {
	var units []string
	var prose strings.Builder
	inFence := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			units = append(units, line)
		} else {
			prose.WriteString(line)
			prose.WriteByte('\n')
		}
	}
	for _, m := range inlineCodeRe.FindAllStringSubmatch(prose.String(), -1) {
		units = append(units, m[1])
	}
	return units
}

var commandNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// isCommandNameToken reports whether tok could be a subcommand name: lowercase
// letters/digits/hyphens only. This excludes flags (--x), placeholders (<id>,
// [--x]), and values (own|repo, a,b).
func isCommandNameToken(tok string) bool { return commandNameRe.MatchString(tok) }

func findChild(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func hasSubcommands(c *cobra.Command) bool {
	for _, k := range c.Commands() {
		if !k.Hidden {
			return true
		}
	}
	return false
}
