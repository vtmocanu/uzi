// Package agentsource parses external agent-role markdown files fetched from an
// admin-configured source repo (PRD #602 M3). It is the API-side twin of the
// worker's agent/src/repoagents.ts and reproduces that parsing contract EXACTLY,
// so a role file parses to the same {name, description, tools, model, prompt_body}
// — or the same skip/clamp reason — on both sides. The pairing is pinned by a
// hand-authored differential golden (parser_test.go + testdata/parity/) that a Go
// test and a TS test each assert against independently.
//
// It is deliberately NOT the strict api/internal/agenttmpl parser: that one rejects
// unknown frontmatter keys and carries no tools/description handling. The sync
// source is TS-ecosystem `.md` files, so repoagents.ts — tolerant, denylist-strip,
// description-reject — is the contract, and this package mirrors it rather than the
// strict builtin parser. It has no go-git and no database dependency: it operates on
// bytes the caller supplies (M3 part B does the clone + persistence).
//
// Where the two ecosystems' shared validators agree, they are reused so the rule
// cannot drift:
//   - NAME: reuses agenttmpl.IsValidName (== repoagents.ts AGENT_NAME_RE /
//     AGENT_NAME_MAX_LEN — verified equivalent; the regex admits only ASCII, so the
//     Go byte-length and the TS UTF-16 length agree on every input the regex passes).
//   - DESCRIPTION unsafe-char test: reuses termsafe.Unsafe (== the TS /[\p{Cc}\p{Cf}]/u).
//
// The MODEL validator is reproduced here rather than reused, because
// agenttmpl.ValidateModel and repoagents.ts's isValidModel DIVERGE — see validModel.
package agentsource

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Caps, mirroring repoagents.ts (PRD #37 Decision 7 / PRD #602 M3).
const (
	// MaxFiles bounds how many role files a set-level parse considers; the rest are
	// ignored with one aggregated over_limit note. It is a DoS guard on an untrusted
	// source, so it stays BOUNDED rather than unlimited. Raised 16 -> 32 (issue #703):
	// the published vtmocanu/skills product-agents/ roster is ~14-15 files, and 16 left
	// almost no headroom — a 16th/17th role would be silently dropped (over_limit),
	// which reads as "that role isn't published" rather than "the cap truncated it". 32
	// gives room for the roster to grow while keeping the bound. Note maxTotalBytes in
	// git.go is MaxFiles * MaxBytes, so this also raises the total read budget to 2 MiB,
	// still bounded. This value now intentionally DIVERGES from repoagents.ts's
	// REPO_AGENTS_MAX_FILES (16, worker-side): the set-level cap is not part of the shared
	// parity corpus (only per-file ParseFile parity is pinned), so the two count caps are
	// free to differ. The worker-side cap can follow later if its roster grows too.
	MaxFiles = 32
	// MaxBytes bounds a single file; a larger file is skipped (too_large), never parsed.
	MaxBytes = 64 * 1024
	// MaxDescriptionLen bounds a description in UTF-8 BYTES (not UTF-16 units), to match
	// the Go len() basis the API stores/validates against.
	MaxDescriptionLen = 1024
	// MaxTools bounds a declared allowlist; only the first MaxTools entries are considered.
	MaxTools = 64
	// MaxToolLen bounds a single tool token.
	MaxToolLen = 64
)

// NoteReason is the stable skip/clamp vocabulary, byte-for-byte the strings
// repoagents.ts's RepoAgentNoteReason uses.
type NoteReason string

const (
	// NoteTooLarge — a file exceeded MaxBytes and was skipped without being parsed.
	NoteTooLarge NoteReason = "too_large"
	// NoteOverLimit — one aggregated note for the files past MaxFiles (carries Count).
	NoteOverLimit NoteReason = "over_limit"
	// NoteInvalid — no frontmatter, bad name, missing/oversized/unsafe description, or empty body.
	NoteInvalid NoteReason = "invalid"
	// NoteDuplicate — an earlier file already claimed this name (first wins).
	NoteDuplicate NoteReason = "duplicate"
	// NoteToolsAllDenied — every declared tool is denied, so the role is skipped (fail closed).
	NoteToolsAllDenied NoteReason = "tools_all_denied"
	// NoteToolsFiltered — kept, with denied tools removed from its allowlist (carries Tools).
	NoteToolsFiltered NoteReason = "tools_filtered"
	// NoteModelIgnored — kept, with an unusable model string dropped (inherits the run default).
	NoteModelIgnored NoteReason = "model_ignored"
)

// Note is one skip or clamp, for the caller to surface (M3 part B emits these as
// run/status messages). Name is the role or filename it is about (empty for the
// aggregated over_limit note).
type Note struct {
	Name   string
	Reason NoteReason
	// Tools is set only for NoteToolsFiltered: which declared tokens were removed.
	Tools []string
	// Count is set only for NoteOverLimit: how many files past the cap were ignored.
	Count int
}

// FileResult is the outcome of parsing ONE role file's bytes (the twin of
// repoagents.ts's ParsedAgentFile). On OK the Role is usable and Notes carries any
// non-fatal clamps (tools_filtered / model_ignored). On !OK the file is skipped:
// Skip is NoteInvalid or NoteToolsAllDenied and Name is the role/slug to blame.
type FileResult struct {
	OK    bool
	Role  agenttmpl.Definition
	Notes []Note
	Skip  NoteReason
	Name  string
}

// SourceFile is one candidate file for a set-level parse: its `.md` filename and
// raw bytes as fetched. ParseSet slices, caps, and dedupes over these.
type SourceFile struct {
	Name string
	Data []byte
}

// SetResult is the outcome of parsing a set of files (the twin of
// repoagents.ts's DetectedRepoAgents): the kept roles sorted by name, plus every
// skip/clamp note.
type SetResult struct {
	Roles []agenttmpl.Definition
	Notes []Note
}

// The denylist repo/source agents can never receive, compared on CANONICAL names
// (Task canonicalizes to Agent). Mirrors REPO_AGENT_DENIED_TOOLS + TOOL_CANONICAL.
var deniedTools = map[string]struct{}{
	"Agent":          {},
	"ScheduleWakeup": {},
	"CronCreate":     {},
}

var (
	// A YAML block-scalar indicator standing alone as a value (`>`, `|`, `|-`, `|2`, …).
	blockScalarOpenerRe = regexp.MustCompile(`^[|>][0-9]*[+-]?$`)
	// A `-` block-sequence item; `\s*` (not `\s+`) so a column-0 `- Bash` (prettier/yamlfmt
	// output) is not falsely dropped.
	listItemRe = regexp.MustCompile(`^\s*-\s+(.+)$`)
	// The shape a real tool name can take (incl. `mcp__server__tool`).
	toolNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	// Characters safeLabel strips from a filename before it becomes a note name.
	unsafeLabelRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// ParseFile parses one role file's bytes under slug (the filename-derived fallback
// name). It reproduces repoagents.ts's parseAgentFile, with ONE deliberate api-side
// hardening divergence: it rejects input that is not valid UTF-8.
//
// repoagents.ts reads files with fs.readFile(..., "utf8"), which is LOSSY — an
// invalid byte decodes to U+FFFD and parsing proceeds. This twin instead rejects the
// whole file (invalid) before any rune-ranging. Reason: hasUnsafeText ranges runes,
// so a lone invalid byte (e.g. C1 0x9B) would decode to U+FFFD and pass the Cc/Cf
// check while the RAW byte survives verbatim in the stored description — a control-
// byte bypass of the write boundary, a parity break, and a Postgres UTF-8 insert
// failure on a role that already reported "parsed OK". Rejecting per-role (graceful
// failure, not a run failure) keeps the api write boundary clean, matching uzi's own
// termsafe.Validate doctrine that invalid UTF-8 is not storable.
func ParseFile(raw []byte, slug string) FileResult {
	invalid := func(name string) FileResult { return FileResult{OK: false, Skip: NoteInvalid, Name: name} }

	// Deliberate divergence from repoagents.ts (see the doc comment above): reject
	// non-UTF-8 input outright rather than lossily decoding invalid bytes to U+FFFD.
	if !utf8.Valid(raw) {
		return invalid(slug)
	}

	fm, ok := parseFrontmatter(raw)
	if !ok {
		return invalid(slug)
	}

	// The frontmatter name is the identity when present and non-empty after trim;
	// else the filename slug. Either way it must pass the shared kebab-case rule.
	name := slug
	if fm.hasName {
		if t := jsTrim(fm.name); t != "" {
			name = t
		}
	}
	if !agenttmpl.IsValidName(name) {
		return invalid(slug)
	}

	// The description is UNTRUSTED text bound for a status message, the DB, and the
	// admin approval panel. Held to the strict rule: non-empty, <= MaxDescriptionLen
	// UTF-8 bytes, and no control OR bidi/format character (termsafe.Unsafe). Rejected
	// (whole file skipped), not scrubbed.
	desc := ""
	if fm.hasDesc {
		desc = jsTrim(fm.description)
	}
	if desc == "" || len(desc) > MaxDescriptionLen || hasUnsafeText(desc) {
		return invalid(name)
	}

	// The body must be non-empty after trim, but is NOT sanitized — only the
	// description is (a Cf char in the body is KEPT; the same char in the description
	// is rejected). This asymmetry is reproduced exactly from repoagents.ts. The
	// emptiness check uses jsTrim (not strings.TrimSpace) to match repoagents.ts's
	// `body.trim() === ""` — the two disagree on U+FEFF and NEL, so a whitespace-only
	// body must resolve identically on both parsers.
	body := fm.body
	if jsTrim(body) == "" {
		return invalid(name)
	}

	var notes []Note
	role := agenttmpl.Definition{Name: name, Description: desc, PromptBody: body}

	if fm.hasTools {
		kept, denied := filterTools(fm.tools)
		// An allowlist that survives as empty would read as inherit-all (a privilege
		// escalation for a role that declared only denied tools). Fail closed: skip.
		if len(kept) == 0 {
			return FileResult{OK: false, Skip: NoteToolsAllDenied, Name: name}
		}
		if len(denied) > 0 {
			notes = append(notes, Note{Name: name, Reason: NoteToolsFiltered, Tools: denied})
		}
		role.Tools = kept
	}

	// The model is honored for any well-formed token (a full id, not only an alias);
	// only a string that could never be a model id is ignored.
	if fm.hasModel {
		if m := jsTrim(fm.model); m != "" {
			if validModel(m) {
				role.Model = m
			} else {
				notes = append(notes, Note{Name: name, Reason: NoteModelIgnored})
			}
		}
	}

	return FileResult{OK: true, Role: role, Notes: notes}
}

// reservedDocNames are conventional non-role documentation basenames that commonly
// sit beside role files in a source folder — a README describing the roster, a
// LICENSE, and so on. When a file with one of these names FAILS to parse as a role
// (e.g. a README has no frontmatter, so ParseFile returns invalid), ParseSet treats it
// as documentation and drops it SILENTLY rather than surfacing an `invalid` note that
// counts toward the "failed" total a status panel shows (issue #715: the live
// product-agents/ sync read "14 changed, 1 failed" where the "1 failed" was just
// product-agents/README.md). The name alone is NOT disqualifying: a reserved-named
// file that DOES parse as a valid role (a real `security.md` with frontmatter, say) is
// kept as a role — the drop applies only on the invalid path. This is a SILENT skip,
// not a new NoteReason: adding a reason would change the byte-for-byte contract this
// package shares with repoagents.ts (see the NoteReason block above). Matched
// case-insensitively on the whole filename.
var reservedDocNames = map[string]struct{}{
	"readme.md":          {},
	"license.md":         {},
	"contributing.md":    {},
	"changelog.md":       {},
	"code_of_conduct.md": {},
	"security.md":        {},
}

// ParseSet parses a set of candidate files over in-memory bytes (no filesystem, no
// symlinks) — the set-level twin of repoagents.ts's detectRepoAgents. The per-file
// ParseFile contract is pinned identical across the two; the set-level enumeration is
// free to diverge (e.g. MaxFiles differs from the worker's cap). `.md` files are taken
// in filename order, capped at MaxFiles (over_limit), each capped at MaxBytes
// (too_large), parsed, and deduped by name (first wins → duplicate). A conventional
// non-role doc (reservedDocNames, e.g. README.md) that fails to parse is dropped
// silently, so it never becomes a note or counts as a failure. The kept roles are
// returned sorted by name. It never returns an error — a bad file is a note, never a
// failure.
func ParseSet(files []SourceFile) SetResult {
	md := make([]SourceFile, 0, len(files))
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".md") {
			md = append(md, f)
		}
	}
	sort.SliceStable(md, func(i, j int) bool { return md[i].Name < md[j].Name })

	notes := []Note{}
	if over := len(md) - MaxFiles; over > 0 {
		notes = append(notes, Note{Reason: NoteOverLimit, Count: over})
		md = md[:MaxFiles]
	}

	var roles []agenttmpl.Definition
	seen := make(map[string]struct{})
	for _, f := range md {
		slug := safeLabel(strings.TrimSuffix(f.Name, ".md"))
		if len(f.Data) > MaxBytes {
			notes = append(notes, Note{Name: slug, Reason: NoteTooLarge})
			continue
		}
		res := ParseFile(f.Data, slug)
		if !res.OK {
			// A reserved documentation basename (README.md, LICENSE.md, …) that fails to
			// parse as a role is documentation, not a failed role: drop it silently so it
			// never counts toward the "failed" total (issue #715). Scoped to the `invalid`
			// path — a reserved name that parses as a valid role reaches the keep path
			// below, and a parsed-but-rejected role (tools_all_denied) still surfaces its
			// note.
			if res.Skip == NoteInvalid {
				if _, reserved := reservedDocNames[strings.ToLower(f.Name)]; reserved {
					continue
				}
			}
			notes = append(notes, Note{Name: res.Name, Reason: res.Skip})
			continue
		}
		if _, dup := seen[res.Role.Name]; dup {
			notes = append(notes, Note{Name: res.Role.Name, Reason: NoteDuplicate})
			continue
		}
		seen[res.Role.Name] = struct{}{}
		roles = append(roles, res.Role)
		notes = append(notes, res.Notes...)
	}

	sort.SliceStable(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return SetResult{Roles: roles, Notes: notes}
}

// frontmatter is the four scalar keys (plus a tools list) this parser keeps; every
// other key is silently ignored.
type frontmatter struct {
	name        string
	hasName     bool
	description string
	hasDesc     bool
	model       string
	hasModel    bool
	tools       []string
	hasTools    bool
	body        string
}

// parseFrontmatter is the line parser (NOT a YAML engine) reproducing
// repoagents.ts's parseFrontmatter: require `---` fences, normalize a leading BOM
// and CRLF/CR to LF, keep only name/description/model/tools (first occurrence wins,
// unknown keys ignored), drop a block-scalar opener value, and split the body with
// one leading blank line removed.
func parseFrontmatter(raw []byte) (frontmatter, bool) {
	s := strings.TrimPrefix(string(raw), "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return frontmatter{}, false
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return frontmatter{}, false
	}

	var fm frontmatter
	// inToolsBlock is set only right after a bare `tools:` and collects `- item`
	// continuation lines; a colon-bearing line ends it (a blank/colon-less line does
	// NOT — mirroring repoagents.ts, where the reset sits after the no-colon continue).
	inToolsBlock := false
	var toolsBlock []string

	for _, line := range lines[1:closeIdx] {
		if inToolsBlock {
			if m := listItemRe.FindStringSubmatch(line); m != nil {
				toolsBlock = append(toolsBlock, stripQuotes(jsTrim(m[1])))
				fm.tools = toolsBlock
				continue
			}
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		inToolsBlock = false
		value := stripQuotes(jsTrim(line[idx+1:]))
		if blockScalarOpenerRe.MatchString(value) {
			continue
		}
		switch {
		case key == "name" && !fm.hasName:
			fm.name, fm.hasName = value, true
		case key == "description" && !fm.hasDesc:
			fm.description, fm.hasDesc = value, true
		case key == "model" && !fm.hasModel:
			fm.model, fm.hasModel = value, true
		case key == "tools" && !fm.hasTools:
			fm.hasTools = true
			if value == "" {
				toolsBlock = []string{}
				fm.tools = toolsBlock
				inToolsBlock = true
			} else {
				fm.tools = parseInlineToolList(value)
			}
		}
	}

	bodyLines := lines[closeIdx+1:]
	if len(bodyLines) > 0 && bodyLines[0] == "" {
		bodyLines = bodyLines[1:]
	}
	fm.body = strings.Join(bodyLines, "\n")
	return fm, true
}

// filterTools splits a declared allowlist into kept and denied (both order-preserving,
// deduped). Malformed tokens are dropped silently; only the first MaxTools are
// considered; the denial test is on the CANONICAL name (Task→Agent) but the DECLARED
// name is what appears in the lists. Reproduces repoagents.ts filterTools.
func filterTools(declared []string) (kept, denied []string) {
	limit := declared
	if len(limit) > MaxTools {
		limit = limit[:MaxTools]
	}
	for _, tool := range limit {
		if len(tool) > MaxToolLen || !toolNameRe.MatchString(tool) {
			continue
		}
		if _, isDenied := deniedTools[canonicalTool(tool)]; isDenied {
			if !contains(denied, tool) {
				denied = append(denied, tool)
			}
			continue
		}
		if !contains(kept, tool) {
			kept = append(kept, tool)
		}
	}
	return kept, denied
}

// canonicalTool maps the SDK aliases that resolve onto a denied tool. Only aliases
// mapping onto a denied tool need an entry (mirrors TOOL_CANONICAL).
func canonicalTool(name string) string {
	if name == "Task" {
		return "Agent"
	}
	return name
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// parseInlineToolList turns `a, b` or `[a, b]` into ["a","b"] (mirrors parseInlineToolList).
func parseInlineToolList(value string) []string {
	s := jsTrim(value)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if t := stripQuotes(jsTrim(part)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// stripQuotes removes one layer of matching surrounding ASCII quotes (mirrors stripQuotes).
func stripQuotes(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// safeLabel reduces a filename to a note-safe label (mirrors safeLabel): drop
// everything outside [A-Za-z0-9._-], then bound the length. The result is ASCII, so
// the byte slice equals the UTF-16 slice repoagents.ts takes.
func safeLabel(value string) string {
	v := unsafeLabelRe.ReplaceAllString(value, "")
	if len(v) > agenttmpl.MaxNameLen {
		v = v[:agenttmpl.MaxNameLen]
	}
	return v
}

// hasUnsafeText reports whether s carries any control (Cc) or bidi/format (Cf)
// character, via the shared termsafe predicate (== the TS /[\p{Cc}\p{Cf}]/u).
func hasUnsafeText(s string) bool {
	for _, r := range s {
		if termsafe.Unsafe(r) {
			return true
		}
	}
	return false
}

// validModel is repoagents.ts's isValidModel (models.ts) with ONE deliberate api-side
// hardening divergence: it ALSO rejects any Unicode FORMAT char (Cf).
//
// repoagents.ts's char class is /[\p{Cc}<U+FFFD>\s]/u, which OMITS Cf — so a model like
// "opus<U+200B>" (trailing zero-width space, a Cf char that is not JS \s) is honored
// there. This twin rejects it (model_ignored, a fail-safe clamp: the role inherits the
// run default rather than being skipped). Reason: the model token is echoed onto the
// admin cross-owner Agents-status surface (RunDTO.Model) — the SAME surface the
// description Cf-rejection protects — where a bidi-reordered or zero-width-padded token
// is a spoofing vector. uzi's own agenttmpl.ValidateModel rejects Cf with that exact
// anti-spoofing rationale; honoring Cf here (as isValidModel does) would reintroduce
// the spoof at the write boundary. This is a Go-only divergence, so it is NOT pinned by
// the shared parity corpus — a Go-only unit test asserts the model_ignored clamp.
//
// It still deliberately does not reuse agenttmpl.ValidateModel, which diverges on the
// length basis: isValidModel bounds by UTF-16 length (JS .length) and this mirrors that;
// ValidateModel bounds by Go bytes. They agree on ASCII ids but not on astral-plane
// tokens, so the length rule stays TS-faithful even though the Cf rule now matches
// ValidateModel. The validator remains a non-empty token, <= MAX_MODEL_LEN UTF-16 units,
// free of Cc control chars, the replacement char (U+FFFD), Cf, and JS-regex whitespace.
const maxModelLen = 100 // MAX_MODEL_LEN

func validModel(value string) bool {
	n := utf16Len(value)
	if n == 0 || n > maxModelLen {
		return false
	}
	for _, r := range value {
		if r == '\uFFFD' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || jsWhitespace(r) {
			return false
		}
	}
	return true
}

// utf16Len counts UTF-16 code units, matching JS String.length (astral runes = 2).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// jsWhitespace reproduces the exact set JavaScript's regex `\s` matches (under /u),
// which differs from Go's unicode.IsSpace on two code points (JS includes U+FEFF; Go
// includes U+0085, which the Cc check already covers here).
func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// jsTrim trims leading and trailing whitespace using the exact JS-`\s` set
// (jsWhitespace), matching JavaScript String.prototype.trim(). It is used
// everywhere the parser trims a frontmatter value / name / description / model /
// inline-tool token, in place of strings.TrimSpace. The two disagree on U+FEFF
// (ZWNBSP): JS `.trim()` strips it, Go's strings.TrimSpace does NOT, so a field with
// a leading/trailing U+FEFF would diverge Go stricter than repoagents.ts. Using the
// JS set here removes that gap (they also agree on the other direction: JS omits
// U+0085/NEL, which strings.TrimSpace strips — but that is Cc and rejected downstream
// anyway). The body's leading-blank-line handling is intentionally NOT routed
// through this.
func jsTrim(s string) string {
	return strings.TrimFunc(s, jsWhitespace)
}
