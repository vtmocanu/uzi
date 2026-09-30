package agentsource

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/vtmocanu/uzi/api/internal/skilltmpl"
)

// Product skill sets (PRD #1909 D9): the api reads a product's skills repo through the SAME
// bounded, guarded clone FetchRoleFiles uses (fetchTip), and looks for `skills/<name>/SKILL.md`
// and `.claude/skills/<name>/SKILL.md` at the repo root.

// skillRoots are the repo-root directories skills are read from, in precedence order: when the
// same skill name appears under both, the first wins and the later one is a duplicate note.
var skillRoots = []string{"skills", ".claude/skills"}

// skillFileName is the file inside each skill directory that carries the skill.
const skillFileName = "SKILL.md"

// MaxSkillFiles bounds how many SKILL.md files one fetch reads out of a clone. It is a DoS guard
// on an untrusted source, deliberately above the default per-run cap (SKILLS_MAX_PER_RUN, 32) so
// the cap, not the reader, decides what a job receives. The rest are ignored with one aggregated
// over_limit note.
const MaxSkillFiles = 64

// SkillFile is one candidate skill read from a clone: the directory name it sat in (the slug,
// the fallback skill name) and the raw SKILL.md bytes.
type SkillFile struct {
	Dir  string
	Data []byte
}

// FetchSkillFiles shallow-clones opts.CloneURL at opts.Ref and reads `skills/*/SKILL.md` and
// `.claude/skills/*/SKILL.md`, returning the resolved commit SHA, the files and any notes
// (too_large, duplicate, over_limit). It shares fetchTip with FetchRoleFiles, so the redirect
// allowlist, same-origin credential guard, wire cap, timeout and token scrub are the same code.
// opts.Dir is ignored. maxFileBytes bounds one SKILL.md (an oversized file is skipped WITHOUT
// reading its blob, with a too_large note); the total read is bounded by MaxSkillFiles *
// maxFileBytes. Only regular files in real directories are read (a symlink, a submodule or a
// non-directory entry is skipped, never followed). A repo with neither root is a valid empty set.
func FetchSkillFiles(ctx context.Context, opts CloneOptions, maxFileBytes int) (sha string, files []SkillFile, notes []Note, err error) {
	if maxFileBytes <= 0 {
		return "", nil, nil, errors.New("agentsource: skill file size cap must be positive")
	}
	scrub := scrubber(opts.Token)
	commit, resolved, terr := fetchTip(ctx, opts)
	if terr != nil {
		return "", nil, nil, terr
	}
	files, notes, rerr := readSkillFiles(commit, maxFileBytes)
	if rerr != nil {
		return "", nil, nil, fmt.Errorf("agentsource: read skill files: %s", scrub(rerr.Error()))
	}
	return resolved, files, notes, nil
}

// readSkillFiles walks each skill root of the commit's tree. See FetchSkillFiles.
func readSkillFiles(commit *object.Commit, maxFileBytes int) ([]SkillFile, []Note, error) {
	tree, err := commit.Tree()
	if err != nil {
		return nil, nil, err
	}
	var (
		files []SkillFile
		notes []Note
		seen  = map[string]struct{}{}
		total int64
		over  int
	)
	budget := int64(MaxSkillFiles) * int64(maxFileBytes)
	for _, root := range skillRoots {
		rootTree, terr := tree.Tree(root)
		if terr != nil {
			if errors.Is(terr, object.ErrDirectoryNotFound) || errors.Is(terr, object.ErrEntryNotFound) {
				continue
			}
			return nil, nil, terr
		}
		entries := append([]object.TreeEntry(nil), rootTree.Entries...)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for i := range entries {
			e := entries[i]
			if e.Mode != filemode.Dir {
				continue // a symlink, a submodule or a stray file: never followed
			}
			slug := safeLabel(e.Name)
			if _, dup := seen[e.Name]; dup {
				notes = append(notes, Note{Name: slug, Reason: NoteDuplicate})
				continue
			}
			sub, serr := rootTree.Tree(e.Name)
			if serr != nil {
				return nil, nil, serr
			}
			entry, eerr := sub.FindEntry(skillFileName)
			if eerr != nil {
				if errors.Is(eerr, object.ErrEntryNotFound) {
					continue // a directory with no SKILL.md is not a skill
				}
				return nil, nil, eerr
			}
			if entry.Mode != filemode.Regular && entry.Mode != filemode.Executable {
				continue
			}
			if len(files) >= MaxSkillFiles {
				over++
				continue
			}
			f, ferr := sub.TreeEntryFile(entry)
			if ferr != nil {
				return nil, nil, ferr
			}
			seen[e.Name] = struct{}{}
			// Skip an oversized file WITHOUT reading its blob: the OOM guard on a hostile single file.
			if f.Size > int64(maxFileBytes) {
				notes = append(notes, Note{Name: slug, Reason: NoteTooLarge})
				continue
			}
			if total+f.Size > budget {
				over++
				continue
			}
			content, cerr := f.Contents()
			if cerr != nil {
				return nil, nil, cerr
			}
			total += int64(len(content))
			files = append(files, SkillFile{Dir: e.Name, Data: []byte(content)})
		}
	}
	if over > 0 {
		notes = append(notes, Note{Reason: NoteOverLimit, Count: over})
	}
	return files, notes, nil
}

// SkillResult is the outcome of parsing ONE SKILL.md. On OK, Name, Description and Body are the
// only three things kept. On !OK the file is skipped and Name is the label to blame.
type SkillResult struct {
	OK          bool
	Name        string
	Description string
	Body        string
}

// ParseSkillFile parses one SKILL.md under dir (the directory name, the fallback identity). It
// keeps ONLY name, description and body: every other frontmatter key (allowed-tools, hooks, model
// and the rest of the capability-granting keys) is dropped, exactly as repo-borne skills are
// (agent/src/repo-skills.ts parseNameDescription), because the skill's runtime capabilities are
// the job runner's closed tool set and never a repo's say-so. The frontmatter name, when present
// and non-empty, is the identity (else the directory name) and must be kebab-case
// (skilltmpl.NameRe, which also makes it path-safe). The description is held to the strict rule
// ParseFile applies: non-empty, at most MaxDescriptionLen UTF-8 bytes, no control or format
// characters; the body must be non-empty. Input that is not valid UTF-8 is rejected outright.
func ParseSkillFile(raw []byte, dir string) SkillResult {
	label := safeLabel(dir)
	invalid := func(name string) SkillResult { return SkillResult{Name: name} }
	if !utf8.Valid(raw) {
		return invalid(label)
	}
	fm, ok := parseFrontmatter(raw)
	if !ok {
		return invalid(label)
	}
	name := dir
	if fm.hasName {
		if t := jsTrim(fm.name); t != "" {
			name = t
		}
	}
	if !skilltmpl.NameRe.MatchString(name) {
		return invalid(label)
	}
	desc := ""
	if fm.hasDesc {
		desc = jsTrim(fm.description)
	}
	if desc == "" || len(desc) > MaxDescriptionLen || hasUnsafeText(desc) || strings.ContainsAny(desc, "\r\n") {
		return invalid(name)
	}
	if jsTrim(fm.body) == "" {
		return invalid(name)
	}
	return SkillResult{OK: true, Name: name, Description: desc, Body: fm.body}
}
