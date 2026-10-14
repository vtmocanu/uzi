#!/usr/bin/env python3
"""Collapse only added changelog subsections; preserve the base's ordered lines."""

import difflib
import sys
from pathlib import Path


def preserves(base, lines):
    """Require every nonblank base line, including repeated headings, in order."""
    remaining = iter(line for line in lines if line.strip())
    return all(any(candidate == line for candidate in remaining)
               for line in base if line.strip())


def collapse(base, lines):
    # A real removal/reword belongs to land-prep's removal guard, not normalization.
    if not preserves(base, lines):
        return lines
    protected = set()
    for match in difflib.SequenceMatcher(None, base, lines, autojunk=False).get_matching_blocks():
        protected.update(range(match.b, match.b + match.size))
    start = next((i for i, line in enumerate(lines) if line.startswith('## [Unreleased]')), None)
    if start is None:
        return lines
    end = next((i for i in range(start + 1, len(lines)) if lines[i].startswith('## ')), len(lines))
    heads = [i for i in range(start + 1, end) if lines[i].startswith('### ')]
    sections = [(i, heads[n + 1] if n + 1 < len(heads) else end)
                for n, i in enumerate(heads)]
    targets = {}
    for i, _ in sections:
        key = lines[i].rstrip()
        if key not in targets or (i in protected and targets[key] not in protected):
            targets[key] = i
    additions, removed = {}, set()
    for i, stop in sections:
        target = targets[lines[i].rstrip()]
        if target == i or i in protected:
            continue
        body = range(i + 1, stop)
        if any(j in protected and lines[j].strip() for j in body):
            continue
        content = list(lines[i + 1:stop])
        while content and not content[0].strip():
            content.pop(0)
        while content and not content[-1].strip():
            content.pop()
        additions.setdefault(target, []).extend(content)
        removed.update(range(i, stop))
    insertions = {}
    for i, stop in sections:
        if i not in additions:
            continue
        at = stop
        while at > i + 1 and not lines[at - 1].strip():
            at -= 1
        # Add only new lines; leave every base line, gap and section in place.
        insertions[at] = ['\n'] + additions[i] + ['\n']
    result = []
    for i in range(len(lines) + 1):
        result.extend(insertions.get(i, []))
        if i < len(lines) and i not in removed:
            result.append(lines[i])
    return result if preserves(base, result) else lines


if __name__ == '__main__':
    base = Path(sys.argv[1]).read_text().splitlines(keepends=True)
    source = Path(sys.argv[2])
    lines = source.read_text().splitlines(keepends=True)
    sys.stdout.write(''.join(collapse(base, lines)))
