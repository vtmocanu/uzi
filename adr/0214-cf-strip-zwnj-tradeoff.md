# ADR-214: Strip the whole Unicode `Cf` category, accepting the ZWNJ cost

**Status**: Accepted (records an existing decision; changes no behavior)
**Date**: 2026-08-21
**Deciders**: Vlad (maintainer), agent team
**Source**: [GitHub issue vtmocanu/uzi#214](https://github.com/vtmocanu/uzi/issues/214), reframed 2026-08-18 from a GitLab-imported analysis into a doc-only ADR task. The issue body carries the original analysis and the verified Unicode-category evidence; this ADR is the durable home for the decision.
**Numbering**: `0214` is the **issue** number, like `0065` and `0238`; it is not an ADR sequence number. A reader who assumes "ADR number == ADR count" will miscount.

## Decision (summary)

uzi strips **every** Unicode `Cf` (format) character from untrusted text, at two
coupled seams:

- **The CLI render boundary** ([#180](https://github.com/vtmocanu/uzi/pull/180))
  removes `Cf` (and `Cc`, sparing `\n`/`\t` where line structure matters) on the
  way out, so a bidi override or a zero-width rune cannot reorder or hide part of
  a value an operator reads in `uzi worker list`, `uzi run get`, and the other
  human tables.
- **The write-side validator** ([#169](https://github.com/vtmocanu/uzi/pull/169))
  adopts the invariant **reject exactly what the renderer strips**: a value that
  would not survive the render boundary unchanged is refused at the door
  (`POST /api/workers` and its siblings) with a 400.

`api/internal/termsafe` is the **single predicate** both seams use
(`Unsafe(r) == unicode.IsControl(r) || unicode.In(r, unicode.Cf)`), so the render
strip and the write-side rejection cannot drift apart. Any future narrowing
happens in that one package and reaches both by construction.

This ADR does **not** narrow the strip. It records the trade so the argument for
ever revisiting it is stated in its strong form rather than reconstructed from the
weak one.

## Context

`Cf` bundles two very different kinds of character behind one Unicode category:

- **Attack primitives.** U+202E RIGHT-TO-LEFT OVERRIDE (and the rest of the bidi
  family) is `Cf`; it visually reorders text, so a stored coordinate or a label
  can be made to *read* as a different one. This is Trojan-Source / CVE-2021-42574
  (issue #124), and it is the reason the strip exists.
- **Legitimate typography.** U+200C ZERO WIDTH NON-JOINER is also `Cf`, and it is
  **required for correct Persian and Arabic typography** — it controls whether
  adjacent letters join, so its absence changes how a word reads. Stripping it
  mangles legitimate human-language text.

The category also contains U+200D ZERO WIDTH JOINER (`Cf`), which joins multi-part
emoji such as `👨‍👩‍👧`. That is the **visible but weak** cost: a family emoji in a
worker name is decorative, and the render already degrades gracefully into the
component glyphs. ZWNJ is the case with real weight, and it is the one an
allowlist argument will reach for last.

## Decision detail

### Strip the whole category, do not allowlist

Rejecting only the "dangerous" `Cf` characters (the bidi overrides) and permitting
the "safe" ones is an argument surface that never closes: after ZWJ comes ZWNJ,
then the BOM, then the soft hyphen, each with a plausible case and each demanding
its own security review by whoever is on call. A wholesale category strip is one
rule with no per-character exceptions to keep correct, which is why the CLI
CHANGELOG for #180 records the emoji decomposition as *"the accepted cost of
rejecting the whole `Cf` category rather than an allowlist."*

### Why it is still correct today

Not because ZWNJ and ZWJ are dangerous the way a bidi override is, but because of
what all three share: **they are invisible, and invisible in an identifier is a
spoofing primitive.** `A<ZWJ>B` and `AB` are different stored values that display
identically. These names are read back in **cross-tenant admin listings whose
entire job is telling rows apart**, so "two different entries that look the same"
is the same class of harm as the bidi override, just weaker. That is exactly what
`termsafe`'s rejection message says: the characters *"let two different entries
look identical, or make one read as another."*

The decision also changed character when #169 landed. #180 was renderer-only, so
the cost was **cosmetic** — a mangled display. #169 adopted reject-exactly-what-the-
renderer-strips, which means narrowing the renderer now also makes the character
**storable** in a cross-tenant identifier. Cosmetic became **security-relevant**,
and that is why the trade cannot be settled on the emoji case alone.

## Consequences

### The cost, stated honestly

- **ZWNJ (U+200C) is dropped**, so a name that is correct in a script requiring it
  (Persian, Arabic) can be corrupted or refused. This is the real cost.
- **ZWJ (U+200D) is dropped**, so multi-part emoji decompose into their component
  glyphs on render and are refused on write. This is the visible but decorative
  cost.
- Single-codepoint emoji are unaffected (a variation selector is `Mn`, not `Cf`),
  and combining marks survive — so "Zalgo" remains a grapheme-width problem, not a
  `Cf` one. The verified category evidence is in the issue body.

### What would justify revisiting

A **real** user, not a hypothetical one:

- a user whose name is in a script that needs ZWNJ hitting a **400 from
  `POST /api/workers`**, or
- a user reporting that their name renders **wrong in `uzi worker list`**.

Until one of those happens, this is a recorded trade, not a defect.

### If it is ever narrowed, do NOT hand-roll an allowlist

Unicode already solved this. IDNA's **CONTEXTJ** rules permit ZWJ and ZWNJ **only
in valid joining contexts** (after a virama, or between characters with the right
joining types) rather than unconditionally. That admits the legitimate Persian and
Arabic usage while still refusing a bare joiner dropped into a name to make it
collide with another. Copy that shape into `termsafe` — the one predicate both
seams share — or leave the wholesale strip alone. An ad-hoc "allow ZWJ, keep
banning the rest" is the surface this decision exists to refuse.

### The related path that must move with it

[#213](https://github.com/vtmocanu/uzi/pull/213) closed a **different** path by
which `Cf` reached a stored, operator-read value: `deriveChatTitle` let format
characters into `runs.title`. If the `termsafe` rule is ever narrowed, that path
must follow the same rule rather than diverge — the invariant is "every seam that
writes an operator-read identifier applies the one predicate," and a second
predicate is exactly the drift #169 was written to prevent.

## Related

- **[#180](https://github.com/vtmocanu/uzi/pull/180)** — the CLI render boundary
  that strips `Cf`, and where the emoji decomposition cost is recorded in the
  CHANGELOG.
- **[#169](https://github.com/vtmocanu/uzi/pull/169)** — the write-side validator
  and the reject-exactly-what-the-renderer-strips invariant, which is what turned
  this from cosmetic into a storability question.
- **[#213](https://github.com/vtmocanu/uzi/pull/213)** — `deriveChatTitle`'s
  separate `Cf`-into-`runs.title` path; if the rule is ever narrowed, its fix must
  follow rather than diverge.
- `api/internal/termsafe` — the single `Unsafe` predicate both the render boundary
  and the validator call, so any narrowing happens in one place and reaches both.
