#!/usr/bin/env python3
"""Optional release result files: bounded public identities, never command prose."""
import argparse
import json
import os
from pathlib import Path
import re
import tempfile

MAX_BYTES = 65536
MAX_UNCITED = 50
STAGES = {'preconditions', 'notes-assembly', 'metadata-preparation', 'commit',
          'changelog-coverage', 'changelog-links', 'promotion', 'complete'}
REASONS = {'ready', 'release-precondition-refused', 'release-cut-failed',
           'changelog-coverage-ok', 'changelog-coverage-refused', 'changelog-coverage-error',
           'changelog-links-refused', 'promotion-failed'}
BASE = r'(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)'
TAG = rf'v{BASE}(?:-rc\.[1-9][0-9]*)?'
FIELDS = {'schema', 'stage', 'reason', 'planned_tag', 'uncited', 'uncited_count', 'uncited_truncated'}


def identities(rows):
    if not isinstance(rows, list) or len(rows) > MAX_UNCITED:
        raise ValueError('invalid identity list')
    for row in rows:
        if (not isinstance(row, dict) or row.keys() != {'sha', 'pr'}
                or not isinstance(row['sha'], str) or not re.fullmatch(r'[0-9a-f]{40}', row['sha'])
                or (row['pr'] is not None and (type(row['pr']) is not int or not 0 < row['pr'] < 2**53))):
            raise ValueError('invalid public identity')
    return rows


def validate(data):
    if (not isinstance(data, dict) or data.keys() != FIELDS or type(data['schema']) is not int
            or data['schema'] != 1 or not isinstance(data['stage'], str) or data['stage'] not in STAGES
            or not isinstance(data['reason'], str) or data['reason'] not in REASONS
            or (data['planned_tag'] is not None and
                (not isinstance(data['planned_tag'], str) or len(data['planned_tag']) > 128
                 or not re.fullmatch(TAG, data['planned_tag'])))):
        raise ValueError('invalid release result')
    rows, count, truncated = data['uncited'], data['uncited_count'], data['uncited_truncated']
    if rows is None:
        if count is not None or truncated is not False:
            raise ValueError('invalid uncollected identities')
    else:
        identities(rows)
        if (type(count) is not int or not len(rows) <= count < 2**53
                or type(truncated) is not bool or truncated != (count > len(rows))):
            raise ValueError('invalid identity count')
    return data


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate result key')
        result[key] = value
    return result


def read(path):
    with Path(path).open('rb') as stream:
        raw = stream.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise ValueError('release result too large')
    return validate(json.loads(raw, object_pairs_hook=unique_object))


def write(path, stage, reason, tag, *, missing=None, child=None, status=1, kind='cutter'):
    rows, count, truncated = None, None, False
    if missing:
        with Path(missing).open('rb') as stream:
            raw = stream.read(1048577)
        if len(raw) > 1048576:
            raise ValueError('identity source too large')
        lines = raw.decode('ascii').splitlines()
        count = len(lines)
        rows = []
        for index, line in enumerate(lines):
            sha, pr = line.split('\t')
            number = None
            if pr:
                if not pr.isascii() or not pr.isdecimal():
                    raise ValueError('invalid PR source')
                digits = pr.lstrip('0') or '0'
                if len(digits) <= 16:
                    candidate = int(digits)
                    if 0 < candidate < 2**53:
                        number = candidate
            row = {'sha': sha, 'pr': number}
            identities([row])
            if index < MAX_UNCITED:
                rows.append(row)
        truncated = count > len(rows)
    if child:
        prior = read(child)
        if prior['stage'] != 'changelog-coverage' or prior['reason'] not in {
                'changelog-coverage-ok', 'changelog-coverage-refused', 'changelog-coverage-error'}:
            raise ValueError('unexpected child result')
        rows, count, truncated = prior['uncited'], prior['uncited_count'], prior['uncited_truncated']
        if stage == 'changelog-coverage':
            reason = prior['reason']
        if status == 0 and prior['reason'] != 'changelog-coverage-ok':
            raise ValueError('successful cut contradicts child failure')
    if status == 0:
        reason = 'changelog-coverage-ok' if kind == 'oracle' else 'ready'
        if kind == 'cutter':
            stage = 'complete'
        if kind == 'oracle' and rows is None:
            rows, count = [], 0
    elif status == 3 and reason in ('', 'release-cut-failed'):
        reason = 'release-precondition-refused'
    elif not reason:
        reason = 'release-cut-failed'
    data = validate(dict(schema=1, stage=stage, reason=reason, planned_tag=tag or None,
                         uncited=rows, uncited_count=count, uncited_truncated=truncated))
    raw = (json.dumps(data, sort_keys=True) + '\n').encode()
    target = Path(path)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=target.parent, delete=False) as stream:
            temporary = Path(stream.name)
            stream.write(raw)
        os.replace(temporary, target)
    finally:
        if temporary is not None and temporary.exists():
            temporary.unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('path')
    parser.add_argument('--kind', choices=('oracle', 'cutter'), default='cutter')
    parser.add_argument('--stage', required=True)
    parser.add_argument('--reason', default='')
    parser.add_argument('--tag', default='')
    parser.add_argument('--status', type=int, required=True)
    parser.add_argument('--missing')
    parser.add_argument('--child')
    args = parser.parse_args()
    try:
        write(args.path, args.stage, args.reason, args.tag, missing=args.missing,
              child=args.child, status=args.status, kind=args.kind)
    except (ValueError, OSError, TypeError):
        parser.exit(1, 'release diagnostics: invalid or unavailable result\n')


if __name__ == '__main__':
    main()
