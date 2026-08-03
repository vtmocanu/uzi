package agenttmpl

import "slices"

// SameContent reports whether two definitions carry the same content: the four
// mutable columns of an agent template — description, model, tools and prompt
// body.
//
// It lives here, and not in handler, because it is the ONE answer to "has this
// row drifted from the definition this binary ships?" (issue #201 M4a). agenttmpl
// has no database dependency and both handler and store already import it, so the
// badge, the shipped-vs-stored diff and any later reconciler share a single
// implementation. The moment there are two, the UI can say a row is customized
// while a reconciler quietly overwrites it.
//
// NAME IS NOT COMPARED, and that is deliberate rather than an omission. It is the
// lookup key: the shipped side is always obtained by BuiltinByName(row.Name),
// whose loop condition is d.Name == name, so equality holds by construction — and
// the column is immutable after create (UpdateAgentTemplate does not carry it).
// A fifth term here could never be false and would send a reader hunting a case
// that cannot exist.
//
// Nothing is normalized, in either direction:
//
//   - Tools uses slices.Equal, NOT reflect.DeepEqual. slices.Equal(nil, []string{})
//     is true, which is the correct answer — both mean inherit-all — where
//     DeepEqual would report drift on a semantically identical row. It is also
//     order-sensitive, which is correct too: Render joins the list in order
//     (render.go), so a reordering really does change the rendered subagent file.
//     Do not sort either side; sorting hides a real edit.
//   - Description and PromptBody are compared exactly, never trimmed. Trimming
//     would hide a whitespace-only edit permanently. The builtin corpus is held to
//     the matching invariant by TestBuiltinsFrontmatterIsUnpadded, so the write
//     path's TrimSpace cannot make a pristine row look drifted. See
//     TestBuiltinsParseAndValid, which now asserts that invariant on the corpus.
//
// IT DOES NOT RENDER EITHER SIDE AND COMPARE THE BYTES, which is the tempting
// shortcut, and no case in this suite discriminates the two today — which is
// exactly why it is written the column way now and would be expensive to change
// later. Two reasons it must stay this way:
//
//   - Render serializes the FRONTMATTER, so anything a future release adds there
//     enters the comparison for free. PRD #85 M2 stamps a `version:` line into the
//     builtin files; it would appear on the shipped side and never on the stored
//     side (there is no version column), so every stamped builtin would report
//     drift forever, silently.
//   - Render omits the tools and model lines when they are empty, so a
//     serialization comparison answers a question about the rendered file rather
//     than about the columns. This function is asked about the columns.
func SameContent(a, b Definition) bool {
	return a.Description == b.Description &&
		a.Model == b.Model &&
		a.PromptBody == b.PromptBody &&
		slices.Equal(a.Tools, b.Tools)
}
