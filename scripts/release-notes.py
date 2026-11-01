#!/usr/bin/env python3
"""Bound GitHub's Release presentation; never alter the source changelog or RC delta."""
import argparse
import re
import sys
from urllib.parse import quote

LIMIT = 120000  # Unicode characters, below GitHub's 125000-character body limit.
NOTICE = '_These release notes are truncated. Complete notes are linked below._\n\n'
HOTFIX_WARNING = ('\n**UNVERIFIED hotfix: this off-main release has no full CI evidence. '
                  'Tagged metadata, worker-image checks and real KinD smoke passed.**\n')
# Go unicode.IsSpace, used by releasecheck.Security's strings.TrimSpace.
# Python str.strip also removes U+001C..U+001F, which must not invent this signal.
SECURITY_SPACE = ('\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004'
                  '\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000')


def bullet_fragments(text):
    """Whole blocks using the shared parser's top-level bullet/heading markers.

    Top-level '- ' starts a block; a heading, next bullet or EOF ends it.
    Blank continuation paragraphs stay with their bullet rather than being cut.
    Context is emitted only with a following bullet, never as a dangling heading.
    Split only LF, as releasecheck.Security does, rather than Unicode line breaks.
    """
    pending, block = [], []
    lines = text.split('\n')
    for index, line in enumerate(lines):
        raw = line + ('\n' if index < len(lines) - 1 else '')
        if line.startswith('- '):
            if block:
                yield ''.join(block)
            block, pending = pending + [raw], []
        elif line.startswith(('### ', '## ')):
            if block:
                yield ''.join(block)
                block = []
            pending.append(raw)
        elif block:
            block.append(raw)
        else:
            pending.append(raw)
    if block:
        yield ''.join(block)


def render(raw, repository, tag, unverified=False, *, limit=LIMIT):
    notes = raw.decode('utf-8')
    warning = HOTFIX_WARNING if unverified else ''
    complete = notes + warning
    if len(complete) <= limit:
        return complete.encode('utf-8')

    url = f'https://github.com/{quote(repository, safe="/")}/blob/{quote(tag, safe="")}/CHANGELOG.md'
    footer = f'\n\n[Full release notes in CHANGELOG.md]({url})\n'
    sections = re.split(r'(?m)(?=^[^\S\n]*#{2,3} )', notes)
    security, ordinary = [], []
    for section in sections:
        (security if section.lstrip(SECURITY_SPACE).startswith('### Security') else ordinary).append(section)
    security_text = '\n\n'.join(section.strip('\r\n') for section in security)
    security_suffix = '\n\n' + security_text if security else ''
    if len(NOTICE + security_suffix + warning + footer) > limit and security:
        security_suffix = '\n\n### Security\n\nSee the complete notes linked below.'
    # Reserve every mandatory signal/link before taking any ordinary bullets.
    room = limit - len(NOTICE + security_suffix + warning + footer)
    if room < 0:
        raise ValueError('budget cannot contain the mandatory release metadata')
    kept = []
    for fragment in bullet_fragments(''.join(ordinary)):
        if len(fragment) > room:
            break
        kept.append(fragment)
        room -= len(fragment)
    return (NOTICE + ''.join(kept) + security_suffix + warning + footer).encode('utf-8')


def main():
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument('--repository', required=True)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--unverified-hotfix', action='store_true')
    args = parser.parse_args()
    try:
        result = render(sys.stdin.buffer.read(), args.repository, args.tag, args.unverified_hotfix)
    except (UnicodeError, ValueError) as error:
        parser.exit(2, f'release-notes: {error}\n')
    sys.stdout.buffer.write(result)


if __name__ == '__main__':
    main()
