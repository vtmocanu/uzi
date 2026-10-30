#!/usr/bin/env python3
"""Atomic, bounded public evidence for one same-tag recovery operation."""
import copy
import datetime as dt
import json
import os
from pathlib import Path
import re
import tempfile

LIMIT = 32768
WORKFLOWS = {'release.yml', 'brew.yml', 'ci.yml', 'kind-smoke.yml'}
STATES = {'queued', 'in_progress', 'completed', 'waiting', 'pending', 'requested'}
CONCLUSIONS = {'success', 'failure', 'cancelled', 'timed_out', 'action_required',
               'startup_failure', 'neutral', 'skipped', 'stale', None, ''}
MAIN_STATES = {'not-applicable', 'unavailable', 'absent', 'pending', 'passed', 'failed', 'cancelled'}
FIELDS = {'schema', 'operation', 'repository', 'tag', 'sha', 'main_ci', 'attempts', 'decisions', 'truncated', 'state', 'reason', 'publication_read_errors'}


def integer(value, minimum=1):
    if type(value) is not int or not minimum <= value < 2**53:
        raise ValueError('invalid journal integer')
    return value


def label(value):
    if not isinstance(value, str):
        raise ValueError('invalid journal label')
    return ''.join(char for char in value if char.isprintable())[:160]


def stamp():
    return dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


def status(value, conclusion):
    if value not in STATES or conclusion not in CONCLUSIONS:
        raise ValueError('invalid journal state')


def validate(data):
    if (not isinstance(data, dict) or data.keys() != FIELDS or type(data['schema']) is not int
            or data['schema'] != 1 or not isinstance(data['operation'], str)
            or not re.fullmatch(r'[A-Za-z0-9_.:-]{1,128}', data['operation'])
            or not isinstance(data['repository'], str)
            or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', data['repository'])
            or not isinstance(data['tag'], str)
            or not re.fullmatch(r'v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-rc\.[1-9][0-9]*)?', data['tag'])
            or not isinstance(data['sha'], str) or not re.fullmatch(r'[0-9a-f]{40}', data['sha'])
            or type(data['truncated']) is not bool or not isinstance(data['attempts'], list)
            or not isinstance(data['decisions'], list)
            or not isinstance(data['main_ci'], dict)
            or data['main_ci'] != {'policy': 'report-only', 'state': data['main_ci'].get('state')}
            or data['main_ci']['state'] not in MAIN_STATES):
        raise ValueError('invalid journal envelope')
    for attempt in data['attempts']:
        if not isinstance(attempt, dict) or attempt.keys() != {'workflow', 'run', 'attempt', 'status', 'conclusion',
                'initial_status', 'initial_conclusion', 'first_observed_at', 'observed_at', 'url', 'jobs', 'jobs_omitted'}:
            raise ValueError('invalid journal attempt')
        if attempt['workflow'] not in WORKFLOWS:
            raise ValueError('invalid journal workflow')
        integer(attempt['run'])
        integer(attempt['attempt'])
        if attempt['url'] != f"https://github.com/{data['repository']}/actions/runs/{attempt['run']}/attempts/{attempt['attempt']}":
            raise ValueError('invalid canonical run URL')
        integer(attempt['jobs_omitted'], 0)
        status(attempt['status'], attempt['conclusion'])
        status(attempt['initial_status'], attempt['initial_conclusion'])
        for timestamp in (attempt['observed_at'], attempt['first_observed_at']):
            if not isinstance(timestamp, str) or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', timestamp):
                raise ValueError('invalid journal timestamp')
        if not isinstance(attempt['jobs'], list):
            raise ValueError('invalid journal job list')
        for job in attempt['jobs']:
            if not isinstance(job, dict) or job.keys() != {'id', 'name', 'status', 'conclusion', 'original_conclusion', 'url', 'steps', 'steps_omitted'}:
                raise ValueError('invalid journal job')
            integer(job['id'])
            if job['url'] != f"https://github.com/{data['repository']}/actions/runs/{attempt['run']}/job/{job['id']}":
                raise ValueError('invalid canonical job URL')
            integer(job['steps_omitted'], 0)
            if label(job['name']) != job['name']:
                raise ValueError('invalid bounded job label')
            status(job['status'], job['conclusion'])
            status(job['status'], job['original_conclusion'])
            if not isinstance(job['steps'], list):
                raise ValueError('invalid journal step list')
            for step in job['steps']:
                if not isinstance(step, dict) or step.keys() != {'number', 'name', 'status', 'conclusion'}:
                    raise ValueError('invalid journal step')
                integer(step['number'])
                if label(step['name']) != step['name']:
                    raise ValueError('invalid bounded step label')
                status(step['status'], step['conclusion'])
    for decision in data['decisions']:
        if not isinstance(decision, dict) or decision.keys() != {'workflow', 'run', 'attempt', 'reason', 'before', 'after', 'state', 'at'}:
            raise ValueError('invalid journal decision')
        if (decision['workflow'] not in WORKFLOWS or decision['reason'] not in
                {'publication-role', 'gate-refused', 'budget-exhausted', 'evidence-unavailable'}
                or decision['state'] not in {'reserved', 'requested', 'uncertain', 'blocked', 'passed-on-retry', 'failed-on-retry'}):
            raise ValueError('invalid journal decision state')
        integer(decision['run'])
        integer(decision['attempt'])
        integer(decision['before'], 0)
        integer(decision['after'], 0)
        if decision['after'] > decision['before']:
            raise ValueError('journal budget increased')
        if not isinstance(decision['at'], str) or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', decision['at']):
            raise ValueError('invalid decision timestamp')
    reasons = {'watching': {'publication-pending'}, 'workflows-green': {'publication-workflows-green'},
               'blocked': {'gate-refused', 'budget-exhausted', 'retry-request-unconfirmed', 'publication-refused'},
               'pending-timeout': {'publication-timeout'}, 'instrument-error': {'evidence-unavailable'}}
    if not isinstance(data['state'], str) or data['state'] not in reasons or data['reason'] not in reasons[data['state']]:
        raise ValueError('invalid journal operation result')
    errors = data['publication_read_errors']
    if not isinstance(errors, dict) or errors.keys() != {'release.yml', 'brew.yml'}:
        raise ValueError('invalid publication read counters')
    for count in errors.values():
        integer(count, 0)
    return data


def encoded(data):
    return json.dumps(data, ensure_ascii=True, separators=(',', ':'), sort_keys=True).replace('`', '\\u0060').encode('ascii')


def audit(raw, total_raw=None, truncated_raw=None):
    try:
        rows = json.loads(raw or 'null')
        total = json.loads(total_raw or 'null')
        truncated = json.loads(truncated_raw or 'null')
    except (ValueError, TypeError):
        return None, None, None
    if not isinstance(rows, list):
        return None, None, None
    for row in rows:
        if (not isinstance(row, dict) or row.keys() != {'sha', 'pr', 'shipping'}
                or not isinstance(row['sha'], str) or not re.fullmatch('[0-9a-f]{40}', row['sha'])
                or type(row['shipping']) is not bool
                or (row['pr'] is not None and (type(row['pr']) is not int or not 0 < row['pr'] < 2**53))):
            return None, None, None
    if total is None and truncated is None:
        total = len(rows)
    elif (type(total) is not int or not len(rows) <= total < 2**53 or type(truncated) is not bool
          or truncated != (total > len(rows))):
        return None, None, None
    kept = []
    for row in rows[:64]:
        if len(encoded(kept + [row])) > 8192:
            break
        kept.append(row)
    return kept, total, len(kept) < total


def decode(raw):
    if isinstance(raw, str):
        raw = raw.encode('utf-8')
    if len(raw) > LIMIT:
        raise ValueError('journal exceeds source limit')
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('duplicate journal key')
            result[key] = value
        return result
    return validate(json.loads(raw, object_pairs_hook=unique))


def read(path):
    with Path(path).open('rb') as stream:
        return decode(stream.read(LIMIT + 1))


class Journal:
    def __init__(self, path, operation, repository, tag, sha):
        self.path = Path(path)
        self.data = dict(schema=1, operation=operation, repository=repository, tag=tag, sha=sha,
                         main_ci={'policy': 'report-only', 'state': 'unavailable'},
                         attempts=[], decisions=[], truncated=False, state='watching', reason='publication-pending',
                         publication_read_errors={'release.yml': 0, 'brew.yml': 0})
        self.save()

    def finish(self, state, reason):
        self.data.update(state=state, reason=reason)
        self.save()

    def read_unavailable(self, workflow):
        self.data['publication_read_errors'][workflow] += 1
        self.save()

    def save(self):
        validate(self.data)
        data = copy.deepcopy(self.data)
        # Retain an envelope for every observed attempt and every retry decision.
        # Trim only detail rows, with explicit per-attempt/job omission counts.
        while len(encoded(data)) > LIMIT:
            detailed = [attempt for attempt in data['attempts'] if attempt['jobs']]
            if not detailed:
                raise ValueError('journal attempt/decision capacity exhausted')
            largest = max(detailed, key=lambda attempt: len(encoded(attempt)))
            jobs = [job for job in largest['jobs'] if job['steps']]
            if jobs:
                job = max(jobs, key=lambda item: len(encoded(item)))
                job['steps_omitted'] += len(job['steps'])
                job['steps'] = []
            else:
                largest['jobs'].pop()
                largest['jobs_omitted'] += 1
            data['truncated'] = True
        raw = encoded(data)
        descriptor, name = tempfile.mkstemp(prefix='.release-journal-', dir=self.path.parent)
        try:
            with os.fdopen(descriptor, 'wb') as stream:
                stream.write(raw)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(name, self.path)
        finally:
            if os.path.exists(name):
                os.unlink(name)

    def main_ci(self, state):
        if state not in MAIN_STATES:
            raise ValueError('invalid main CI classification')
        self.data['main_ci'] = {'policy': 'report-only', 'state': state}
        self.save()

    def observe(self, workflow, run, jobs):
        observed = stamp()
        attempt = dict(workflow=workflow, run=integer(run['id']), attempt=integer(run['run_attempt']),
                       status=run['status'], conclusion=run['conclusion'], observed_at=observed,
                       initial_status=run['status'], initial_conclusion=run['conclusion'], first_observed_at=observed,
                       jobs=[], jobs_omitted=0)
        attempt['url'] = f"https://github.com/{self.data['repository']}/actions/runs/{run['id']}/attempts/{run['run_attempt']}"
        for job in jobs:
            attempt['jobs'].append(dict(id=integer(job['id']), name=label(job['name']),
                                        url=f"https://github.com/{self.data['repository']}/actions/runs/{run['id']}/job/{job['id']}",
                                        status=job['status'], conclusion=job['conclusion'], original_conclusion=job['conclusion'], steps_omitted=0,
                                        steps=[dict(number=integer(step['number']), name=label(step['name']),
                                                    status=step['status'], conclusion=step['conclusion'])
                                               for step in job['steps'] if step['conclusion'] not in (None, '', 'success', 'skipped', 'neutral')]))
        validate(dict(self.data, attempts=[attempt]))
        key = (workflow, attempt['run'], attempt['attempt'])
        existing = next((item for item in self.data['attempts'] if (item['workflow'], item['run'], item['attempt']) == key), None)
        if existing is None:
            self.data['attempts'].append(attempt)
        else:
            for key in ('initial_status', 'initial_conclusion', 'first_observed_at'):
                attempt[key] = existing[key]
            # Preserve prior failing jobs/steps even if a later API read clears them.
            previous = {job['id']: job for job in existing['jobs']}
            for job in attempt['jobs']:
                old = previous.get(job['id'])
                if old:
                    job['original_conclusion'] = old['original_conclusion']
                    steps = {step['number']: step for step in old['steps']}
                    steps.update({step['number']: step for step in job['steps']})
                    job['steps'] = list(steps.values())
                previous[job['id']] = job
            attempt['jobs'] = list(previous.values())
            existing.update(attempt)
        self.save()

    def decision(self, workflow, run, attempt, reason, before, after, state):
        self.data['decisions'].append(dict(workflow=workflow, run=run, attempt=attempt, reason=reason,
                                           before=before, after=after, state=state, at=stamp()))
        self.save()
