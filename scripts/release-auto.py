#!/usr/bin/env python3
"""Scheduled/manual release orchestration; the cutter and coverage tool own policy."""
import argparse
import datetime as dt
import hashlib
import html
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request
import uuid
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('release_coverage', ROOT / 'scripts/release-ci-coverage.py')
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
diag_spec = importlib.util.spec_from_file_location('release_diagnostics', ROOT / 'scripts/lib/release-diagnostics.py')
diagnostics = importlib.util.module_from_spec(diag_spec)
diag_spec.loader.exec_module(diagnostics)
journal_spec = importlib.util.spec_from_file_location('release_journal', ROOT / 'scripts/lib/release-journal.py')
journals = importlib.util.module_from_spec(journal_spec)
journal_spec.loader.exec_module(journals)
BASE = r'(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)'
RC = rf'v{BASE}-rc\.[1-9][0-9]*'
SHA = r'[0-9a-f]{40}'
PLAN_KEYS = {'OP', 'BASE', 'TAG', 'REASON', 'SHIPPING_COUNT', 'PREVIOUS_TAG', 'PUBLICATION_STATE', 'HEAD_SHA'}
CUTTER = '.agents/skills/uzi-release/scripts/release-cut.sh'
# Exact infrastructure step names only. Missing diagnostics and test failures refuse.
INFRA_STEPS = {
    'Set up job', 'go mod download',
    'Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1',
    'Run actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e',
    'Run actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1',
    'Run ./.github/actions/go-cache', 'Run ./.github/actions/setup-task',
}


class Stop(Exception):
    def __init__(self, reason, outcome='blocked'):
        super().__init__(reason)
        self.reason, self.outcome = reason, outcome


def require(condition, reason):
    if not condition:
        raise Stop(reason)


def inputs(env):
    require(env.get('GITHUB_REPOSITORY') == 'vtmocanu/uzi', 'wrong-repository')
    require(env.get('RELEASE_REF') == 'refs/heads/main', 'main-required')
    event = env.get('GITHUB_EVENT_NAME', '')
    require(event in ('workflow_dispatch', 'schedule'), 'invalid-event')
    scheduled = event == 'schedule'
    if not scheduled:
        dispatchers = re.split(r'[,\s]+', env.get('RELEASE_DISPATCHERS', '').strip())
        require(bool(env.get('RELEASE_ACTOR')) and env['RELEASE_ACTOR'] in dispatchers, 'dispatcher-not-allowed')
    kind = env.get('RELEASE_KIND', '')
    require(kind in ('rc', 'stable'), 'invalid-kind')
    version, supersede = env.get('RELEASE_VERSION', ''), env.get('RELEASE_SUPERSEDE', '')
    abandon = env.get('RELEASE_ABANDON', 'false')
    tag, sha = env.get('RELEASE_RC_TAG', ''), env.get('RELEASE_RC_SHA', '')
    if scheduled:
        require(kind == 'rc' and not version and not supersede and not tag and not sha and abandon == 'false',
                'scheduled-input-mismatch')
    require(abandon in ('true', 'false'), 'invalid-abandon')
    require(not version or (len(version) <= 100 and re.fullmatch(BASE, version)), 'invalid-version')
    require(not supersede or (len(supersede) <= 128 and re.fullmatch(RC, supersede)), 'invalid-supersede')
    if kind == 'stable':
        require(not version and abandon == 'false' and not supersede, 'stable-input-mismatch')
        require(len(tag) <= 128 and re.fullmatch(RC, tag) and re.fullmatch(SHA, sha), 'invalid-candidate')
    else:
        require(not tag and not sha, 'rc-input-mismatch')
        require(abandon == 'false' or bool(version), 'abandon-needs-version')
    return dict(kind=kind, version=version, abandon=abandon == 'true', supersede=supersede, rc_tag=tag, rc_sha=sha,
                scheduled=scheduled, nightly_enabled=env.get('RELEASE_NIGHTLY_ENABLED') == 'true',
                created_at=env.get('RUN_CREATED_AT', ''))


def nightly_window(stamp):
    require(isinstance(stamp, str) and re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', stamp), 'invalid-run-age')
    try:
        zone = ZoneInfo('Europe/Bucharest')
        day = dt.datetime.fromisoformat(stamp.replace('Z', '+00:00')).astimezone(zone).date()
        return tuple(dt.datetime.combine(day, dt.time(hour), zone).timestamp() for hour in (13, 14))
    except (ValueError, OverflowError, ZoneInfoNotFoundError):
        raise Stop('invalid-nightly-window') from None


def nightly_cutoff(stamp, now, reserve=0):
    cutoff, _ = nightly_window(stamp)
    require(dt.datetime.fromisoformat(stamp.replace('Z', '+00:00')).timestamp() <= now + 60, 'invalid-run-age')
    if now >= cutoff - reserve:
        raise Stop('missed-target', 'defer')
    return cutoff


def parse_plan(text):
    require(len(text) < 8192, 'invalid-plan')
    result = {}
    for line in text.splitlines():
        require('=' in line, 'invalid-plan')
        key, value = line.split('=', 1)
        require(key in PLAN_KEYS and key not in result, 'invalid-plan')
        result[key] = value
    require(result.keys() == PLAN_KEYS, 'invalid-plan')
    require(result['OP'] in ('nextrc', 'new-base', 'noop', 'blocked'), 'invalid-plan')
    require(re.fullmatch(SHA, result['HEAD_SHA']) and result['SHIPPING_COUNT'].isdigit(), 'invalid-plan')
    require(re.fullmatch(r'[a-z][a-z0-9-]*', result['REASON']), 'invalid-plan')
    if result['OP'] != 'blocked':
        require(re.fullmatch(BASE, result['BASE']) and re.fullmatch(RC, result['TAG']), 'invalid-plan')
    return result


def version(tag):
    return tuple(int(part) for part in re.split(r'[.-]', tag[1:].replace('rc.', '')))


def inventory(text):
    refs = {}
    for line in text.splitlines():
        parts = line.split('\t')
        require(len(parts) == 2 and re.fullmatch(SHA, parts[0]) and parts[1] not in refs, 'invalid-remote-refs')
        refs[parts[1]] = parts[0]
    tags = {}
    for ref, oid in refs.items():
        if ref.startswith('refs/tags/') and not ref.endswith('^{}'):
            tag = ref.removeprefix('refs/tags/')
            if re.fullmatch(rf'v{BASE}(?:-rc\.[1-9][0-9]*)?', tag):
                tags[tag] = refs.get(ref + '^{}', oid)
    rc = max((tag for tag in tags if '-rc.' in tag), key=version, default='')
    stable = max((tag for tag in tags if '-rc.' not in tag), key=version, default='')
    tag_lines = sorted(f'{oid}\t{ref}' for ref, oid in refs.items() if ref.startswith('refs/tags/v'))
    tags_digest = hashlib.sha256(('\n'.join(tag_lines) + ('\n' if tag_lines else '')).encode()).hexdigest()
    return dict(main=refs.get('refs/heads/main', ''), tags=tags, tags_digest=tags_digest, previous_rc=rc,
                previous_rc_sha=tags.get(rc, ''), previous_stable=stable, previous_stable_sha=tags.get(stable, ''))


def eligible_jobs(jobs):
    bad = [job for job in jobs if job.get('conclusion') in ('failure', 'timed_out', 'cancelled', 'action_required')]
    if not bad:
        return False
    for job in bad:
        steps = [step for step in job.get('steps', []) if step.get('conclusion') in ('failure', 'timed_out', 'cancelled')]
        if not steps or any(step.get('name') not in INFRA_STEPS for step in steps):
            return False
    return True


def command(*args, timeout=60, raw=False, live=False):
    result = subprocess.run(args, capture_output=not live, text=True, timeout=timeout, check=False)
    if result.returncode:
        # Diagnostics stay in job logs; incident reports contain typed reasons.
        if not live:
            print((result.stdout + result.stderr)[-8192:], file=sys.stderr)
        raise Stop('command-failed')
    if live:
        return ''
    return result.stdout if raw else result.stdout.strip()


def outputs(values):
    wire = dict(values)
    if 'changes' in wire:
        rows, count, truncated = journals.audit(wire['changes'])
        wire.update(changes=journals.encoded(rows).decode('ascii'), changes_count=json.dumps(count),
                    changes_truncated=json.dumps(truncated))
    target = os.environ.get('GITHUB_OUTPUT')
    if target:
        with open(target, 'a') as stream:
            for key, value in wire.items():
                value = str(value)
                require('\n' not in value and '\r' not in value, 'invalid-output')
                stream.write(f'{key}={value}\n')


def summary(values):
    target = os.environ.get('GITHUB_STEP_SUMMARY')
    if target:
        with open(target, 'a') as stream:
            public = dict(values)
            for key in ('changes', 'uncited', 'uncited_count', 'uncited_truncated'):
                public[key] = json.loads(values.get(key, 'null'))
            stream.write('```json\n' + json.dumps(public, sort_keys=True, indent=2) + '\n```\n')
            for change in json.loads(values.get('changes', '[]')):
                sha = change['sha']
                pr = f'PR #{change["pr"]}' if change['pr'] else 'no PR number in subject'
                stream.write(f'- [{sha[:10]}](https://github.com/vtmocanu/uzi/commit/{sha}): {pr}, shipping={change["shipping"]}\n')


def append_watch_log(source, target, limit=32768):
    with source.open('rb') as stream:
        data = stream.read(limit + 1)
    truncated = len(data) > limit
    text = data[:limit].decode('utf-8', errors='replace')
    with target.open('a') as stream:
        stream.write(f'### Publication watcher log (source limit: {limit} bytes)\n\n<pre>{html.escape(text)}</pre>\n')
        if truncated:
            stream.write(f'\n**Truncated at {limit} source bytes; full output remains in the step log.**\n')


def append_watch_journal(source, target):
    try:
        data = journals.read(source)
    except (ValueError, KeyError, TypeError, AttributeError, OSError):
        data = None
    with target.open('a') as stream:
        stream.write('### Recovery attempt journal\n\n')
        if data is None:
            stream.write('Journal unavailable. This is not evidence of successful recovery.\n')
        else:
            if data['state'] == 'watching':
                stream.write('**Incomplete journal:** terminal status was not saved; attempt records are partial.\n\n')
            stream.write('<pre>' + html.escape(json.dumps(data, indent=2, sort_keys=True)) + '</pre>\n')
    return data


def created_at(env, fetch=None):
    run_id = env.get('GITHUB_RUN_ID', '')
    require(run_id.isdigit(), 'invalid-run-id')
    url = f'https://api.github.com/repos/vtmocanu/uzi/actions/runs/{run_id}'
    if fetch is None:
        def fetch(location):
            require(bool(env.get('GH_TOKEN')), 'run-metadata-token-missing')
            request = urllib.request.Request(location, headers={'Accept': 'application/vnd.github+json',
                                                               'Authorization': 'Bearer ' + env['GH_TOKEN']})
            with urllib.request.urlopen(request, timeout=20) as response:
                return json.load(response)
    run = fetch(url)
    require(run.get('id') == int(run_id) and env.get('GITHUB_EVENT_NAME') in ('workflow_dispatch', 'schedule')
            and run.get('event') == env.get('GITHUB_EVENT_NAME')
            and run.get('head_branch') == 'main' and run.get('head_sha') == env.get('GITHUB_SHA')
            and run.get('repository', {}).get('full_name') == 'vtmocanu/uzi'
            and run.get('path', '').split('@')[0] == '.github/workflows/release-auto.yml', 'invalid-run-metadata')
    stamp = run.get('created_at', '')
    require(re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', stamp), 'invalid-run-age')
    dt.datetime.fromisoformat(stamp.replace('Z', '+00:00'))
    return stamp


class Release:
    def __init__(self, config, run=command, api=None, clock=time.monotonic, sleep=time.sleep):
        self.config, self.run = config, run
        self.api = api or policy.GitHub('vtmocanu/uzi')
        self.clock, self.sleep = clock, sleep
        self.wall_time = time.time
        self.retries = []
        self.evidence = {}

    def git(self, *args):
        return self.run('git', '-c', 'core.fsmonitor=false', *args)

    def plan(self, requested=True):
        args = ['bash', CUTTER, '--plan']
        if requested and self.config['version']:
            args.extend(['--version', self.config['version']])
        if requested and self.config['abandon']:
            args.append('--abandon-train')
        return parse_plan(self.run(*args))

    def publication(self, tag):
        sha = self.git('rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}')
        return policy.state(self.api.run('release.yml', sha, tag))

    def remote(self):
        return inventory(self.git('ls-remote', 'origin', 'refs/heads/main', 'refs/tags/v*'))

    def cut(self, args, directory, tag):
        self.evidence.update(phase='prepare', stage='preconditions', planned_tag=tag)
        directory.mkdir(parents=True, exist_ok=True)
        result_file = directory / 'cut-result.json'
        result_file.unlink(missing_ok=True)
        failed = None
        command_token = uuid.uuid4().hex
        print('::stop-commands::' + command_token, flush=True)
        try:
            self.run(*args, '--result-file', str(result_file), timeout=600, live=True)
        except Stop as error:
            failed = error
        except subprocess.TimeoutExpired:
            raise Stop('release-cut-timed-out') from None
        finally:
            print('::' + command_token + '::', flush=True)
        try:
            data = diagnostics.read(result_file)
        except (OSError, ValueError, TypeError):
            raise Stop('release-diagnostics-unavailable') from None
        require(data['planned_tag'] == tag or (failed is not None and data['planned_tag'] is None),
                'release-diagnostics-tag-mismatch')
        require((data['reason'] == 'ready') if failed is None else
                data['reason'] not in ('ready', 'changelog-coverage-ok'), 'release-diagnostics-status-mismatch')
        self.evidence.update(stage=data['stage'], uncited=json.dumps(data['uncited']),
                             uncited_count=json.dumps(data['uncited_count']),
                             uncited_truncated=json.dumps(data['uncited_truncated']))
        if failed is not None:
            raise Stop(data['reason']) from failed

    def watch_admission(self, tag, sha):
        require(re.fullmatch(SHA, sha) and re.fullmatch(rf'v{BASE}(?:-rc\.[1-9][0-9]*)?', tag), 'invalid-recovery-tag')
        current = self.remote()
        require(current['tags'].get(tag) == sha, 'recovery-tag-mismatch')
        if self.config['kind'] == 'stable':
            require(tag == self.config['rc_tag'].split('-rc.')[0]
                    and current['previous_stable'] == tag, 'stable-recovery-superseded')
        else:
            require(re.fullmatch(RC, tag), 'invalid-recovery-tag')
        return dict(outcome='watch-admitted')

    def accept_rc(self, plan):
        cfg = self.config
        if cfg['supersede']:
            candidate = self.plan(requested=False)['PREVIOUS_TAG']
            require(re.fullmatch(RC, candidate) and candidate == cfg['supersede'], 'supersede-not-latest')
            require(self.publication(candidate) in ('failed', 'absent'), 'supersede-not-failed')
        if plan['OP'] == 'blocked':
            require(cfg['supersede'] and plan['PREVIOUS_TAG'] == cfg['supersede']
                    and plan['REASON'] == 'previous-publication-not-successful'
                    and plan['PUBLICATION_STATE'] in ('failed', 'absent'), plan['REASON'])
        if cfg['scheduled'] and plan['OP'] == 'noop':
            raise Stop(plan['REASON'], 'no-op')
        # Dispatch explicitly preserves manual empty-delta respins. The cutter
        # still enforces notes/releasability and every mutation/publication guard.
        require(re.fullmatch(BASE, plan['BASE']) and re.fullmatch(RC, plan['TAG']), 'invalid-plan')

    def tip(self, seconds=7200, interval=15):
        if self.config['scheduled']:
            if not self.config['nightly_enabled']:
                raise Stop('nightly-disabled', 'no-op')
            now = self.wall_time()
            cutoff = nightly_cutoff(self.config['created_at'], now, reserve=600)
            seconds = min(cutoff - 600 - now, 10800)
        deadline_reason = 'tip-not-green-by-cutoff' if self.config['scheduled'] else 'tip-not-green-by-deadline'
        started = self.clock()
        deadline, retried, pending_retry = started + seconds, False, None
        deadline_utc = dt.datetime.fromtimestamp(self.wall_time() + seconds, dt.timezone.utc).isoformat(timespec='seconds')
        last_progress = None
        selected, plan = None, None
        skip_tip = False
        while True:
            if self.clock() > deadline:
                raise Stop('skip-ci-tip' if skip_tip else deadline_reason, 'defer')
            self.git('fetch', '--prune', '--tags', 'origin')
            sha = self.git('rev-parse', 'refs/remotes/origin/main')
            self.evidence['parent'] = sha
            self.evidence.update(phase='prepare', stage='plan')
            tags = self.git('show-ref', '--tags', '-d')
            if selected != (sha, tags):
                self.git('checkout', '-B', 'main', sha)
                plan = self.plan()
                require(plan['HEAD_SHA'] == sha, 'plan-head-mismatch')
                if re.fullmatch(RC, plan['TAG']):
                    self.evidence['planned_tag'] = plan['TAG']
                self.accept_rc(plan)
                selected = sha, tags
            ci = self.api.run('ci.yml', sha, 'main')
            smoke = self.api.run('kind-smoke.yml', sha, 'main')
            ci_state, smoke_state = policy.state(ci), policy.state(smoke)
            if pending_retry and ci and ci['id'] == pending_retry[0] and ci['run_attempt'] <= pending_retry[1]:
                require(self.clock() < pending_retry[2], 'tip-ci-retry-not-started')
                ci_state = 'pending'
            skip_tip = (ci is None or smoke is None) and policy.skips_ci(self.git('show', '-s', '--format=%B', sha))
            self.evidence['stage'] = 'tip-wait'
            progress = (sha, plan['TAG'], ci_state, (ci or {}).get('id'), (ci or {}).get('run_attempt'),
                        (ci or {}).get('status'), (ci or {}).get('conclusion'),
                        smoke_state, skip_tip, (smoke or {}).get('id'), (smoke or {}).get('run_attempt'),
                        (smoke or {}).get('status'), (smoke or {}).get('conclusion'))
            if progress != last_progress:
                print('release-auto: ' + json.dumps(dict(phase='prepare', stage='tip-wait', parent=sha,
                      planned_tag=plan['TAG'], ci_state=ci_state, ci_run=(ci or {}).get('id'),
                      ci_attempt=(ci or {}).get('run_attempt'), smoke_state=smoke_state,
                      ci_status=(ci or {}).get('status'), ci_conclusion=(ci or {}).get('conclusion'),
                      smoke_run=(smoke or {}).get('id'), smoke_attempt=(smoke or {}).get('run_attempt'),
                      smoke_status=(smoke or {}).get('status'), smoke_conclusion=(smoke or {}).get('conclusion'),
                      elapsed_seconds=round(self.clock() - started, 1), deadline_utc=deadline_utc,
                      skip_ci_tip=skip_tip,
                      remaining_seconds=round(max(0, deadline - self.clock()), 1)), sort_keys=True), flush=True)
                last_progress = progress
            if smoke_state == 'failed':
                raise Stop('tip-smoke-failed')
            if skip_tip:
                require(ci_state != 'failed', 'tip-ci-not-retryable')
                if not self.config['scheduled']:
                    raise Stop('skip-ci-tip')
            if ci_state == 'failed':
                marker = ci['id'], ci['run_attempt']
                if not retried:
                    jobs = self.api.pages(f'repos/vtmocanu/uzi/actions/runs/{ci["id"]}/attempts/{ci["run_attempt"]}/jobs?per_page=100', 'jobs')
                    require(eligible_jobs(jobs), 'tip-ci-not-retryable')
                    entry = dict(sha=sha, run=ci['id'], attempt=ci['run_attempt'],
                                             result='requesting', jobs=[{'id': j.get('id'), 'name': j.get('name'),
                                                'steps': [{'name': s.get('name'), 'conclusion': s.get('conclusion')}
                                                          for s in j.get('steps', []) if s.get('conclusion') in ('failure', 'timed_out', 'cancelled')]}
                                                for j in jobs if j.get('conclusion') != 'success'])
                    self.retries.append(entry)
                    try:
                        self.run('gh', 'run', 'rerun', str(ci['id']), '--failed', '--repo', 'vtmocanu/uzi')
                    except (Stop, policy.Refusal, OSError, subprocess.TimeoutExpired):
                        entry['result'] = 'request unconfirmed'
                        raise
                    entry['result'] = 'requested'
                    retried = True
                    pending_retry = (*marker, self.clock() + 60)
                else:
                    for retry in self.retries:
                        if retry['sha'] == sha and retry['run'] == ci['id'] and ci['run_attempt'] > retry['attempt']:
                            retry['result'] = 'failed after retry'
                            retry['final_attempt'] = ci['run_attempt']
                    raise Stop('tip-ci-retry-failed')
            elif ci_state == 'success' and smoke_state == 'success':
                if self.clock() > deadline:
                    raise Stop(deadline_reason, 'defer')
                for retry in self.retries:
                    if retry['sha'] == sha and retry['run'] == ci['id'] and ci['run_attempt'] > retry['attempt']:
                        retry['result'] = 'passed on retry'
                # Publication state may have changed during a long tip wait.
                plan = self.plan()
                require(plan['HEAD_SHA'] == sha, 'plan-head-mismatch')
                self.accept_rc(plan)
                if self.config['scheduled'] and self.clock() > deadline:
                    raise Stop(deadline_reason, 'defer')
                return sha, plan
            if self.clock() >= deadline:
                raise Stop('skip-ci-tip' if skip_tip else deadline_reason, 'defer')
            self.sleep(min(interval, max(0, deadline - self.clock())))

    def chart(self, ref, tag):
        chart = self.git('show', f'{ref}:deploy/chart/Chart.yaml')
        for field in ('version', 'appVersion'):
            matches = re.findall(rf'(?m)^{field}:[ \t]*["\']?({policy.VERSION})["\']?[ \t]*(?:#.*)?$', chart)
            require(matches == [tag[1:]], 'prepared-version-mismatch')

    def changes(self, base, head):
        records = []
        history = self.git('log', '--first-parent', '--format=%H%x09%s', f'{base}..{head}')
        for line in history.splitlines():
            sha, subject = line.split('\t', 1)
            paths = self.run('git', '-c', 'core.fsmonitor=false', 'diff', '--name-only', '-z', sha + '^1', sha, raw=True).split('\0')
            shipping = self.run('bash', '-c', '. scripts/lib/shipping-paths.sh; for path; do if is_shipping "$path"; then printf shipping; exit 0; fi; done; printf nonshipping',
                                'release-auto-paths', *[path for path in paths if path]) == 'shipping'
            parent = self.git('rev-parse', '--verify', sha + '^1')
            shipping = shipping and not policy.metadata(parent, sha)
            match = re.search(r'\(#([0-9]+)\)$|^Merge pull request #([0-9]+) ', subject)
            # Links/identities only, never arbitrary commit prose in incident text.
            records.append(dict(sha=sha, pr=int(next(part for part in match.groups() if part)) if match else None, shipping=shipping))
        return records

    def prepare(self, directory):
        cfg = self.config
        if cfg['kind'] == 'rc':
            self.evidence.update(phase='prepare', stage='preconditions')
            parent, plan = self.tip()
            if cfg['scheduled']:
                nightly_cutoff(cfg['created_at'], self.wall_time())
            before = self.remote()
            require(before['main'] == parent, 'main-race')
            tag = plan['TAG']
            args = ['bash', CUTTER, plan['BASE'], '--ci']
            if cfg['abandon']:
                args.append('--skip-promote')
            try:
                self.cut(args, directory, tag)
            except Stop:
                require(self.remote()['main'] == parent, 'main-race')
                raise
            self.evidence['stage'] = 'metadata-verification'
            release = self.git('rev-parse', 'HEAD')
            require(self.git('rev-list', '--parents', '-n', '1', release).split() == [release, parent], 'prepared-parent-mismatch')
            self.run('python3', '-I', 'scripts/release-ci-coverage.py', 'metadata', parent, release)
            self.chart(release, tag)
            require(tag not in before['tags'], 'target-tag-exists')
            self.git('tag', '-a', tag, '-m', tag, release)
        else:
            self.evidence.update(phase='prepare', stage='candidate-validation',
                                 planned_tag=cfg['rc_tag'].split('-rc.')[0])
            self.git('fetch', '--prune', '--tags', 'origin')
            # Read discovery fields only: RC no-op/main-note/runtime guards do not
            # govern promotion of an already published, pinned candidate.
            require(self.plan(requested=False)['PREVIOUS_TAG'] == cfg['rc_tag'], 'candidate-not-latest')
            parent = self.git('rev-parse', '--verify', f'refs/tags/{cfg["rc_tag"]}^{{commit}}')
            require(parent == cfg['rc_sha'], 'candidate-sha-mismatch')
            self.evidence['parent'] = parent
            require(self.publication(cfg['rc_tag']) == 'success', 'candidate-not-published')
            self.run('python3', '-I', 'scripts/release-ci-coverage.py', 'coverage', parent, '--hops', '1', '--wait', '0')
            before = self.remote()
            require(before['previous_rc'] == cfg['rc_tag'] and before['tags'].get(cfg['rc_tag']) == parent, 'candidate-not-latest')
            base = cfg['rc_tag'][1:].split('-rc.')[0]
            tag = 'v' + base
            require(tag not in before['tags'] and (not before['previous_stable'] or version(before['previous_stable']) < version(tag)), 'stable-superseded')
            self.cut(['bash', CUTTER, base, '--promote-only', '--ci'], directory, tag)
            self.evidence['stage'] = 'metadata-verification'
            release = self.git('rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}')
            require(self.git('rev-list', '--parents', '-n', '1', release).split() == [release, parent], 'promotion-parent-mismatch')
            self.run('python3', '-I', 'scripts/release-ci-coverage.py', 'metadata', parent, release)
            self.chart(release, tag)
        after = self.remote()
        # Stable publication is pinned to the candidate and never depends on main.
        require((after == before) if cfg['kind'] == 'rc' else (after['tags_digest'] == before['tags_digest']), 'release-ref-race')
        self.evidence['stage'] = 'bundle'
        self.git('update-ref', 'refs/heads/release-auto-prepared', release)
        directory.mkdir(parents=True, exist_ok=True)
        bundle = directory / 'release.bundle'
        self.git('bundle', 'create', str(bundle), 'refs/heads/release-auto-prepared', f'refs/tags/{tag}', '^' + parent)
        result = dict(outcome='prepared', reason='ready', parent=parent, release=release, tag=tag,
                      phase='prepare', stage='complete', planned_tag=tag,
                      digest=hashlib.sha256(bundle.read_bytes()).hexdigest(),
                      tag_oid=self.git('rev-parse', f'refs/tags/{tag}'))
        result.update({key: before[key] for key in ('previous_rc', 'previous_rc_sha', 'previous_stable', 'previous_stable_sha')})
        result['refs_digest'] = before['tags_digest']
        audit_head = parent
        if cfg['kind'] == 'stable':
            self.git('fetch', '--no-tags', 'origin', 'refs/heads/main')
            audit_head = self.git('rev-parse', 'FETCH_HEAD')
        boundary = plan['PREVIOUS_TAG'] if cfg['kind'] == 'rc' else before['previous_stable']
        result['audit_head'] = audit_head
        result['changes'] = json.dumps(self.changes(boundary, audit_head)) if boundary else '[]'
        result['retry_log'] = json.dumps(self.retries)
        for key in ('uncited', 'uncited_count', 'uncited_truncated'):
            if key in self.evidence:
                result[key] = self.evidence[key]
        (directory / 'retries.json').write_text(json.dumps(self.retries, indent=2))
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('authorize', 'prepare', 'watch-check', 'watch-log'))
    args = parser.parse_args()
    release = None
    try:
        require(os.environ.get('GITHUB_ACTIONS') == 'true', 'ci-context-required')
        cfg = inputs(os.environ)
        if args.mode == 'watch-log':
            append_watch_log(Path(os.environ['RUNNER_TEMP']) / 'release-watch.log', Path(os.environ['GITHUB_STEP_SUMMARY']))
            data = append_watch_journal(Path(os.environ['RUNNER_TEMP']) / 'release-watch.json',
                                        Path(os.environ['GITHUB_STEP_SUMMARY']))
            outputs({'journal': json.dumps(data, separators=(',', ':'))})
            return 0
        if args.mode == 'authorize':
            if cfg['scheduled'] and not cfg['nightly_enabled']:
                result = dict(outcome='no-op', reason='nightly-disabled', kind='rc', phase='authorize')
            else:
                result = dict(outcome='authorized', kind=cfg['kind'], created_at=created_at(os.environ))
                if cfg['scheduled']:
                    nightly_cutoff(result['created_at'], time.time())
        elif args.mode == 'prepare':
            require(os.environ.get('GITHUB_RUN_ATTEMPT') == '1', 're-dispatch-required')
            directory = Path(os.environ['RUNNER_TEMP']) / 'release-auto'
            release = Release(cfg)
            result = release.prepare(directory)
        else:
            result = Release(cfg).watch_admission(os.environ.get('RELEASE_TAG', ''), os.environ.get('RELEASE_SHA', ''))
        outputs(result)
        summary(result)
        return 0
    except (Stop, policy.Refusal, ValueError, OSError, subprocess.TimeoutExpired) as error:
        result = dict(outcome=getattr(error, 'outcome', 'blocked'), reason=getattr(error, 'reason', 'instrument-error'))
        if release is not None:
            result.update(release.evidence)
            result['retry_log'] = json.dumps(release.retries)
        outputs(result)
        summary(result)
        print(f'release-auto: {result["reason"]}', file=sys.stderr)
        if result['reason'] == 're-dispatch-required':
            print('Re-dispatch instead of rerunning prepare or push.', file=sys.stderr)
        print(str(error)[-8192:], file=sys.stderr)
        return 0 if result['outcome'] == 'no-op' else 1


if __name__ == '__main__':
    sys.exit(main())
