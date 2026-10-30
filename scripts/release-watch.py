#!/usr/bin/env python3
"""Watch publication with one per-workflow retry ledger and the legacy CLI.

Publication retries are signature-independent and bounded per workflow.
Gates stay manual except the opt-in, exact tagged-smoke recovery policy;
an infrastructure-looking failure never authorizes a generic gate retry.
"""
import argparse
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time


WORKFLOWS = ('release.yml', 'brew.yml')
FAILURES = {'failure', 'cancelled', 'timed_out', 'action_required', 'startup_failure'}
JOBS_QUERY = '.jobs[] | [.status, (.conclusion // ""), .name, .url] | @tsv'
CHECKOUT_STEP = r'Run actions/checkout@[0-9a-f]{40}'
SMOKE_INFRA_STEPS = {'Set up job', 'Create KinD cluster', 'Load images into kind', 'Setup Helm'}
ASSERT_SMOKE_STEPS = {'Set up job', 'Require the real tagged KinD smoke job'}
SMOKE_AFFECTED_JOBS = ['kind-smoke', 'kind-smoke-gate']
ASSERT_AFFECTED_JOBS = ['assert-tag-smoke', 'publish-chart', 'publish-release']
RELEASE_SUCCESS_ROLES = {'prep', 'assert-version', 'assert-changelog', 'assert-ci-coverage',
                         'assert-release-metadata', 'assert-worker-pin', 'assert-agent-version',
                         'publish-api', 'publish-web', 'publish-controller',
                         'publish-agent (base)', 'publish-agent (jvm)'}
RELEASE_SKIPPED_ROLES = {'publish-chart', 'publish-release'}


class RecoveryRefusal(ValueError):
    """New recovery proof is absent, ambiguous, cancelled or outside its policy."""


class RecoveryReadUnavailable(ValueError):
    """A read did not establish identity; never permission to send a write."""


def failed_steps(job, allowed, allow_checkout=True):
    steps = job.get('steps')
    if not isinstance(steps, list) or not steps:
        raise RecoveryRefusal('missing failing-step evidence')
    failed = []
    for step in steps:
        if step.get('status') != 'completed' or step.get('conclusion') not in ('success', 'skipped', 'failure', 'timed_out'):
            raise RecoveryRefusal('unknown or cancelled step evidence')
        if step['conclusion'] in ('failure', 'timed_out'):
            name = step.get('name', '')
            if name not in allowed and not (allow_checkout and re.fullmatch(CHECKOUT_STEP, name)):
                raise RecoveryRefusal('failure outside the exact step allowlist')
            failed.append(step)
    if not failed:
        raise RecoveryRefusal('failure without an attributable step')
    return failed


def smoke_roles(jobs):
    roles = {}
    for job in jobs:
        name = job.get('name')
        if name not in {'changes', 'kind-smoke', 'kind-smoke-gate'} or name in roles:
            raise RecoveryRefusal('unexpected smoke job role')
        if job.get('status') != 'completed' or job.get('conclusion') in (None, '', 'cancelled'):
            raise RecoveryRefusal('nonterminal or cancelled smoke job')
        roles[name] = job
    if 'kind-smoke' not in roles or 'kind-smoke-gate' not in roles:
        raise RecoveryRefusal('real smoke and aggregate are both required')
    if 'changes' in roles and roles['changes']['conclusion'] != 'skipped':
        raise RecoveryRefusal('unexpected tag path-filter result')
    return roles


def smoke_retry_job(run, jobs):
    """Return only the real job; its known aggregate is a dependent, not proof."""
    if run.get('status') != 'completed' or run.get('conclusion') not in ('failure', 'timed_out'):
        raise RecoveryRefusal('smoke run is not a classified terminal failure')
    roles = smoke_roles(jobs)
    real, aggregate = roles['kind-smoke'], roles['kind-smoke-gate']
    if real['conclusion'] not in ('failure', 'timed_out') or aggregate['conclusion'] != 'failure':
        raise RecoveryRefusal('unexpected real/aggregate failure combination')
    failed_steps(real, SMOKE_INFRA_STEPS)
    failed_steps(aggregate, {'Gate on the kind-smoke result'}, allow_checkout=False)
    return real


def smoke_success(run, jobs):
    if run.get('status') != 'completed' or run.get('conclusion') != 'success':
        return False
    try:
        roles = smoke_roles(jobs)
    except RecoveryRefusal:
        return False
    return all(roles[name]['conclusion'] == 'success' for name in ('kind-smoke', 'kind-smoke-gate'))


def assertion_retry_job(run, jobs):
    """Step/failed-set check; caller also binds workflow, smoke and prerequisites."""
    if run.get('status') != 'completed' or run.get('conclusion') not in ('failure', 'timed_out') or not terminal_jobs(jobs):
        raise RecoveryRefusal('release attempt is not terminal')
    failures = [job for job in jobs if job['conclusion'] not in ('success', 'skipped', 'neutral')]
    if len(failures) != 1 or failures[0]['name'] != 'assert-tag-smoke' or failures[0]['conclusion'] not in ('failure', 'timed_out'):
        raise RecoveryRefusal('tag-smoke assertion is not the sole failed job')
    failed_steps(failures[0], ASSERT_SMOKE_STEPS)
    return failures[0]


def sibling(name, path):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).parent / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


evidence = sibling('release_coverage', 'release-ci-coverage.py')
journals = sibling('release_journal', 'lib/release-journal.py')


def classify(rows):
    pending, failures = False, []
    for row in rows.splitlines():
        fields = row.split('\t')
        fields += [''] * (4 - len(fields))
        status, conclusion, name, url = fields[:4]
        if status != 'completed':
            pending = True
        elif conclusion in FAILURES:
            failures.append((name, url))
    if pending:
        return 'PENDING', failures
    return ('FAIL' if failures else 'GREEN'), failures


def gate(failures):
    return any(name.startswith('assert') or name == 'prep' for name, _ in failures)


def terminal_jobs(jobs):
    # Journal/cache readiness only, independent of legacy publication policy.
    return bool(jobs) and all(job['status'] == 'completed' and job['conclusion'] not in (None, '') for job in jobs)


class Watcher:
    def __init__(self, version, max_reruns=2, interval=60, max_ticks=60, sleep=time.sleep, *, clock=time.monotonic):
        self.version = version.removeprefix('v')
        self.tag = 'v' + self.version
        self.max_reruns = max_reruns
        self.interval = interval
        self.max_ticks = max_ticks
        self.sleep = sleep
        self.remaining = dict.fromkeys(WORKFLOWS, max_reruns)
        self.gh_errors = 0
        self.journal = None
        self.repository = self.sha = self.kind = None
        self.main_api = None
        self.requested = {}
        self.completed_jobs = {}
        self.stop_reason = 'publication-refused'
        self.clock, self.started = clock, clock()
        self.recovering = False
        self.smoke_remaining, self.assert_remaining = 2, 1
        self.recovery_requests = {}
        self.recovery_seen = {}
        # CLI exits on an uncertain POST; this also fences in-process reuse.
        self.recovery_uncertain = False
        self.tag_oid = None
        self.refs_policy = None

    def enable_recovery(self, tag_oid):
        if (self.journal is None or not isinstance(tag_oid, str) or not re.fullmatch(r'[0-9a-f]{40}', tag_oid)
                or self.kind != ('rc' if '-rc.' in self.version else 'stable')):
            raise RecoveryRefusal('recovery requires a journal and pinned identity')
        self.recovering, self.tag_oid = True, tag_oid

    def git(self, *args):
        result = subprocess.run(['git', '-c', 'core.fsmonitor=false', *args], capture_output=True, text=True, check=False)
        if result.returncode:
            raise RecoveryReadUnavailable('cannot read recovery refs or workflow')
        return result.stdout.strip()

    def validate_identity(self):
        if self.refs_policy is None:
            self.refs_policy = sibling('release_refs', 'release-auto.py')
        try:
            refs = self.refs_policy.inventory(self.git('ls-remote', '--heads', '--tags', 'origin'))
        except (self.refs_policy.Stop, ValueError, KeyError, TypeError) as error:
            raise RecoveryReadUnavailable('recovery refs unavailable') from error
        if refs['tags'].get(self.tag) != self.sha or refs['tag_oids'].get(self.tag) != self.tag_oid:
            raise RecoveryRefusal('recovery tag identity changed')
        if self.kind == 'stable' and refs['previous_stable'] != self.tag:
            raise RecoveryRefusal('stable recovery superseded')

    def require_contract(self, workflow):
        path = '.github/workflows/' + workflow
        if self.git('rev-parse', 'HEAD:' + path) != self.git('rev-parse', self.sha + ':' + path):
            raise RecoveryRefusal('unsupported tagged workflow definition; manual recovery required')

    def can_write(self):
        if self.recovery_uncertain:
            raise RecoveryRefusal('previous recovery request is unconfirmed')
        if self.clock() - self.started >= 7200:
            return False
        self.validate_identity()
        return self.clock() - self.started < 7200

    def recovery_snapshot(self, workflow):
        try:
            selected = self.main_api.run(workflow, self.sha, self.tag)
        except evidence.Refusal as error:
            raise ValueError('recovery run evidence unavailable') from error
        if selected is None:
            return None
        run, jobs = self.snapshot(workflow, selected['id'], fresh=True)
        if (run['id'], run['run_attempt']) != (selected['id'], selected['run_attempt']):
            raise RecoveryRefusal('recovery selection changed during read')
        current = run['id'], run['run_attempt']
        previous = self.recovery_seen.get(workflow)
        if previous is not None and current != previous:
            requests = [item for item in self.recovery_requests.values() if item['workflow'] == workflow]
            own_publication = self.requested.get(workflow)
            expected = any(current == (item['run'], item['attempt'] + 1) for item in requests)
            expected = expected or bool(own_publication and current == (own_publication[0], own_publication[1] + 1))
            if not expected:
                raise RecoveryRefusal('external run or attempt change')
        self.recovery_seen[workflow] = current
        self.journal.observe(workflow, run, jobs)
        return run, jobs

    def track_recovery(self, name, run, jobs):
        request = self.recovery_requests.get(name)
        if request is None:
            return False
        if run['id'] != request['run'] or run['run_attempt'] not in (request['attempt'], request['attempt'] + 1):
            raise RecoveryRefusal('external recovery attempt intervention')
        if run['conclusion'] == 'cancelled' or any(job['conclusion'] == 'cancelled' for job in jobs):
            self.journal.decision(request['workflow'], run['id'], run['run_attempt'], name,
                                  request['remaining'], request['remaining'], 'failed-on-retry',
                                  target_job=request['job'], affected_jobs=request['affected'])
            raise RecoveryRefusal('cancelled recovery attempt; no automatic retry')
        if run['run_attempt'] == request['attempt']:
            return True
        target = [job for job in jobs if job['name'] == request['role']]
        if not jobs or (not target and (run['status'] != 'completed' or run['conclusion'] in (None, ''))):
            return True
        if len(target) != 1:
            raise RecoveryRefusal('recovery target missing or ambiguous')
        if name == 'kind-infra':
            if run['status'] != 'completed' or run['conclusion'] in (None, '') or not terminal_jobs(jobs):
                return True
            passed = smoke_success(run, jobs)
        else:
            if not terminal_jobs(target):
                return True
            passed = target[0]['conclusion'] == 'success'
        self.journal.decision(request['workflow'], run['id'], run['run_attempt'], name,
                              request['remaining'], request['remaining'],
                              'passed-on-retry' if passed else 'failed-on-retry',
                              target_job=target[0]['id'], affected_jobs=request['affected'])
        self.recovery_seen[request['workflow']] = run['id'], run['run_attempt']
        del self.recovery_requests[name]
        return False

    def release_safety(self, observed):
        if observed is None:
            return
        _, jobs = observed
        failures = [job for job in jobs if job['status'] == 'completed' and job['conclusion'] in FAILURES]
        if any(gate([(job['name'], '')]) and job['name'] != 'assert-tag-smoke' for job in failures):
            raise RecoveryRefusal('unrecoverable publication gate')
        if any(job['name'] == 'assert-tag-smoke' for job in failures) and len(failures) != 1:
            raise RecoveryRefusal('mixed publication failure cannot become a sole assertion')

    def recovery_post(self, name, workflow, run, target, affected):
        remaining = self.smoke_remaining if name == 'kind-infra' else self.assert_remaining
        if remaining <= 0:
            raise RecoveryRefusal('recovery budget exhausted')
        if not self.can_write():
            return False
        if name == 'kind-infra':
            self.smoke_remaining -= 1
        else:
            self.assert_remaining -= 1
        self.journal.decision(workflow, run['id'], run['run_attempt'], name, remaining, remaining - 1,
                              'reserved', target_job=target['id'], affected_jobs=affected)
        if self.clock() - self.started >= 7200:
            self.journal.decision(workflow, run['id'], run['run_attempt'], name, remaining - 1, remaining - 1,
                                  'blocked', target_job=target['id'], affected_jobs=affected)
            return False
        status, _, _ = self.gh('api', '--method', 'POST',
                               f"repos/{self.repository}/actions/jobs/{target['id']}/rerun", '--silent')
        self.journal.decision(workflow, run['id'], run['run_attempt'], name, remaining - 1, remaining - 1,
                              'uncertain' if status else 'requested', target_job=target['id'], affected_jobs=affected)
        if status:
            self.recovery_uncertain = True
            raise RecoveryRefusal('recovery POST unconfirmed')
        self.recovery_requests[name] = dict(workflow=workflow, run=run['id'], attempt=run['run_attempt'],
                                             role=target['name'], job=target['id'], affected=affected,
                                             remaining=remaining - 1)
        return True

    def recovery_tick(self):
        if self.recovery_uncertain:
            raise RecoveryRefusal('previous recovery POST unconfirmed')
        try:
            observed = self.recovery_snapshot('kind-smoke.yml')
        except RecoveryRefusal:
            raise
        except (ValueError, KeyError, TypeError, AttributeError, OSError):
            try:
                self.journal.read_unavailable('kind-smoke.yml')
            except (ValueError, KeyError, TypeError, OSError):
                pass
            print('release-watch: tagged smoke observation unavailable; waiting', file=sys.stderr)
            return 'pending'
        if observed is None:
            return 'pending'
        run, jobs = observed
        if self.track_recovery('kind-infra', run, jobs):
            return 'pending'
        if run['status'] != 'completed' or run['conclusion'] in (None, '') or not terminal_jobs(jobs):
            return 'pending'
        if smoke_success(run, jobs):
            return 'green'
        target = smoke_retry_job(run, jobs)
        self.release_safety(self.recovery_snapshot('release.yml'))
        self.require_contract('kind-smoke.yml')
        # Re-select and read complete current evidence immediately before writing.
        latest = self.recovery_snapshot('kind-smoke.yml')
        if latest is None or (latest[0]['id'], latest[0]['run_attempt']) != (run['id'], run['run_attempt']):
            raise RecoveryRefusal('smoke attempt changed before retry')
        fresh_target = smoke_retry_job(*latest)
        if fresh_target['id'] != target['id']:
            raise RecoveryRefusal('smoke job changed before retry')
        self.recovery_post('kind-infra', 'kind-smoke.yml', latest[0], fresh_target,
                           SMOKE_AFFECTED_JOBS)
        return 'pending'

    def assertion_candidate(self, run, jobs):
        target = assertion_retry_job(run, jobs)
        roles = {job['name']: job for job in jobs}
        expected = RELEASE_SUCCESS_ROLES | RELEASE_SKIPPED_ROLES | {'assert-tag-smoke'}
        if len(roles) != len(jobs) or roles.keys() != expected:
            raise RecoveryRefusal('unsupported or incomplete release role set')
        for name in RELEASE_SUCCESS_ROLES:
            if roles[name]['conclusion'] != 'success':
                raise RecoveryRefusal('independent publication prerequisite not successful')
        if any(roles[name]['conclusion'] != 'skipped' for name in RELEASE_SKIPPED_ROLES):
            raise RecoveryRefusal('unexpected downstream release result')
        return target

    def recover_assertion(self, run_id):
        observed = self.recovery_snapshot('release.yml')
        if observed is None or str(observed[0]['id']) != str(run_id):
            raise RecoveryRefusal('release recovery selection changed')
        run, jobs = observed
        if self.track_recovery('tag-smoke-assertion', run, jobs):
            return False
        target = self.assertion_candidate(run, jobs)
        self.require_contract('release.yml')
        self.require_contract('kind-smoke.yml')
        smoke = self.recovery_snapshot('kind-smoke.yml')
        if smoke is None or not smoke_success(*smoke):
            return False
        fresh = self.recovery_snapshot('release.yml')
        if fresh is None or (fresh[0]['id'], fresh[0]['run_attempt']) != (run['id'], run['run_attempt']):
            raise RecoveryRefusal('assertion attempt changed before retry')
        fresh_target = self.assertion_candidate(*fresh)
        if fresh_target['id'] != target['id']:
            raise RecoveryRefusal('assertion job changed before retry')
        latest_smoke = self.recovery_snapshot('kind-smoke.yml')
        if latest_smoke is None or not smoke_success(*latest_smoke):
            return False
        return self.recovery_post('tag-smoke-assertion', 'release.yml', fresh[0], fresh_target,
                                  ASSERT_AFFECTED_JOBS)

    def snapshot(self, workflow, run_id, branch=None, fresh=False):
        code, raw, _ = self.gh('api', f'repos/{self.repository}/actions/runs/{run_id}')
        if code:
            raise ValueError('journal run unavailable')
        run = json.loads(raw)
        if (not isinstance(run, dict) or not isinstance(run.get('path'), str)
                or not isinstance(run.get('repository'), dict) or not isinstance(run.get('head_repository'), dict)):
            raise ValueError('malformed journal run metadata')
        allowed_events = {'push', 'workflow_dispatch'} if workflow == 'brew.yml' else {'push'}
        if (str(journals.integer(run['id'])) != str(run_id) or run['head_sha'] != self.sha
                or run['head_branch'] != (self.tag if branch is None else branch) or run['event'] not in allowed_events
                or run['path'].split('@')[0] != f'.github/workflows/{workflow}'
                or run.get('repository', {}).get('full_name') != self.repository
                or run.get('head_repository', {}).get('full_name') != self.repository):
            raise ValueError('journal run identity mismatch')
        journals.integer(run['run_attempt'])
        journals.status(run['status'], run['conclusion'])
        key = workflow, run['id'], run['run_attempt']
        if not fresh and run['status'] == 'completed' and key in self.completed_jobs:
            return run, self.completed_jobs[key]
        endpoint = f"repos/{self.repository}/actions/runs/{run_id}/attempts/{run['run_attempt']}/jobs?per_page=100"
        code, raw, _ = self.gh('api', '--paginate', '--slurp', endpoint)
        if code:
            raise ValueError('journal jobs unavailable')
        pages = json.loads(raw)
        if not isinstance(pages, list) or not pages:
            raise ValueError('journal job pages absent')
        jobs = [job for page in pages for job in page['jobs']]
        if any(type(page['total_count']) is not int or page['total_count'] != len(jobs) for page in pages):
            raise ValueError('journal job pagination incomplete')
        identities = [journals.integer(job['id']) for job in jobs]
        if len(set(identities)) != len(identities):
            raise ValueError('duplicate journal jobs')
        # Validate every row before any detail truncation. No truncated evidence
        # may authorize a publication retry.
        for job in jobs:
            if journals.integer(job['run_id']) != run['id']:
                raise ValueError('journal job identity mismatch')
            if job['html_url'] != f"https://github.com/{self.repository}/actions/runs/{run_id}/job/{job['id']}":
                raise ValueError('journal job URL mismatch')
            journals.label(job['name'])
            journals.status(job['status'], job['conclusion'])
            step_numbers = [journals.integer(step['number']) for step in job['steps']]
            if len(set(step_numbers)) != len(step_numbers):
                raise ValueError('duplicate journal steps')
            for step in job['steps']:
                journals.label(step['name'])
                journals.status(step['status'], step['conclusion'])
        if run['status'] == 'completed' and terminal_jobs(jobs):
            self.completed_jobs[key] = jobs
        return run, jobs

    def classify_main(self):
        before = None
        try:
            before = copy.deepcopy(self.journal.data)
            self.observe_main()
        except Exception:
            # This entire lane is observational, including its journal writes.
            # Publication was finalized first; retain that result and last file.
            if before is not None:
                self.journal.data = before
            print('release-watch: main CI observation unavailable', file=sys.stderr)

    def observe_main(self):
        if self.kind != 'rc':
            self.journal.main_ci('not-applicable')
            return
        try:
            run = self.main_api.run('ci.yml', self.sha, 'main')
            if run is not None:
                run, jobs = self.snapshot('ci.yml', run['id'], branch='main')
                self.journal.observe('ci.yml', run, jobs)
            if run is None:
                state = 'absent'
            elif run['status'] != 'completed':
                state = 'pending'
            elif run['conclusion'] == 'success':
                state = 'passed'
            elif run['conclusion'] == 'cancelled':
                state = 'cancelled'
            else:
                state = 'failed'
        except (evidence.Refusal, ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.TimeoutExpired):
            state = 'unavailable'
        self.journal.main_ci(state)

    def record(self, workflow, run_id, fresh=False):
        if self.journal is None:
            return None
        run, jobs = self.snapshot(workflow, run_id, fresh=fresh)
        self.journal.observe(workflow, run, jobs)
        requested = self.requested.get(workflow)
        if (self.recovering and requested and run['status'] == 'completed'
                and (run['id'] != requested[0] or run['run_attempt'] not in (requested[1], requested[1] + 1))):
            raise RecoveryRefusal('external publication retry attempt intervention')
        if self.recovering:
            self.recovery_seen.setdefault(workflow, (run['id'], run['run_attempt']))
        if (requested and run['id'] == requested[0] and run['run_attempt'] > requested[1]
                and run['status'] == 'completed' and run['conclusion'] not in (None, '') and terminal_jobs(jobs)):
            passed = (run['conclusion'] == 'success' and bool(jobs)
                      and all(job['status'] == 'completed' and job['conclusion'] in ('success', 'skipped', 'neutral') for job in jobs))
            state = 'passed-on-retry' if passed else 'failed-on-retry'
            self.journal.decision(workflow, run['id'], run['run_attempt'], 'publication-role',
                                  self.remaining[workflow], self.remaining[workflow], state)
            del self.requested[workflow]
            if self.recovering:
                self.recovery_seen[workflow] = run['id'], run['run_attempt']
        return run, jobs

    def observe(self, workflow, run_id, required=False):
        try:
            return self.record(workflow, run_id, fresh=required)
        except RecoveryRefusal:
            raise
        except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.TimeoutExpired):
            if self.journal is not None:
                try:
                    self.journal.read_unavailable(workflow)
                except (ValueError, KeyError, TypeError, OSError):
                    pass
            if required:
                raise
            print(f'release-watch: {workflow} journal evidence unavailable; continuing publication observation', file=sys.stderr)
            return None

    def gh(self, *args, combine_errors=False):
        result = subprocess.run(['gh', *map(str, args)], stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT if combine_errors else subprocess.PIPE,
                                text=True, check=False)
        return result.returncode, result.stdout, result.stderr or ''

    def resolve(self, workflow):
        _, output, _ = self.gh('run', 'list', '--workflow', workflow, '--branch', self.tag,
                               '--limit', 1, '--json', 'databaseId', '--jq', '.[0].databaseId // empty')
        return output.rstrip('\n')

    def dump(self, run):
        status, output, error = self.gh('run', 'view', run, '--json', 'jobs', '--jq', JOBS_QUERY, combine_errors=True)
        return status, (output + error).rstrip('\n')

    def wait_requeued(self, run):
        for _ in range(6):
            _, status, _ = self.gh('run', 'view', run, '--json', 'status', '--jq', '.status')
            if status.rstrip('\n') != 'completed':
                return
            self.sleep(10)

    def run(self):
        try:
            result = self.poll()
        except RecoveryRefusal as error:
            self.stop_reason = 'tag-smoke-recovery-refused'
            print(f'release-watch: {error}', file=sys.stderr)
            result = 1
        except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.TimeoutExpired):
            if self.journal is None:
                raise
            print('release-watch: journal evidence unavailable; no further automatic recovery', file=sys.stderr)
            result = 3
        if self.journal is not None:
            state, reason = {0: ('workflows-green', 'publication-workflows-green'),
                             1: ('blocked', self.stop_reason),
                             2: ('pending-timeout', 'publication-timeout'),
                             3: ('instrument-error', 'evidence-unavailable')}[result]
            try:
                self.journal.finish(state, reason)
            except Exception:
                print('release-watch: terminal journal unavailable', file=sys.stderr)
                return result
            self.classify_main()
        return result

    def poll(self):
        mode = 'rc' if '-' in self.version else 'stable'
        print(f'release-watch: {self.tag} is a {mode} release; watching: {" ".join(WORKFLOWS)}')
        for tick in range(self.max_ticks):
            all_green = True
            smoke_state = self.recovery_tick() if self.recovering else 'green'
            if smoke_state != 'green':
                all_green = False
            for workflow in WORKFLOWS:
                run = self.resolve(workflow)
                if not run:
                    all_green = False
                    print(f'[tick {tick}] {workflow}: no run yet for {self.tag}; waiting')
                    continue
                status, rows = self.dump(run)
                if status:
                    self.gh_errors += 1
                    all_green = False
                    print(f'[tick {tick}] gh run view failed for {workflow} run {run} (consecutive {self.gh_errors}): {rows}', file=sys.stderr)
                    if self.gh_errors >= 3:
                        print('gh keeps failing; giving up (exit 3)', file=sys.stderr)
                        return 3
                    continue
                self.gh_errors = 0
                observed = self.observe(workflow, run)
                if self.recovering and workflow == 'release.yml' and observed is not None:
                    self.track_recovery('tag-smoke-assertion', *observed)
                if not rows:
                    all_green = False
                    print(f'[tick {tick}] {workflow} run {run}: no jobs yet; waiting')
                    continue
                verdict, failures = classify(rows)
                if verdict == 'GREEN':
                    continue
                all_green = False
                if verdict == 'PENDING':
                    continue
                # Preserve the legacy stale-first-tick requery before deciding.
                _, rows = self.dump(run)
                verdict, failures = classify(rows)
                if verdict != 'FAIL':
                    print(f'[tick {tick}] {workflow} run {run}: a fail cleared on re-query; continuing')
                    continue
                gate_failure = gate(failures)
                remaining = self.remaining[workflow]
                if (self.recovering and workflow == 'release.yml' and len(failures) == 1
                        and failures[0][0] == 'assert-tag-smoke'):
                    if smoke_state == 'green':
                        self.recover_assertion(run)
                    # A pending same-tag smoke/requeue is observed in this loop;
                    # it does not authorize blanket gate recovery or a new budget.
                    continue
                if gate_failure or remaining <= 0:
                    self.stop_reason = 'gate-refused' if gate_failure else 'budget-exhausted'
                    reason = 'publication gate failure; manual recovery required'
                    if workflow == 'brew.yml' and gate_failure:
                        print(f"After the same tag's release.yml succeeds: gh run rerun {run} --failed", file=sys.stderr)
                    if not gate_failure:
                        reason = f'reruns exhausted ({self.max_reruns} used for {workflow})'
                    print(f'=== {workflow} run {run}: FAILED ({reason}) — react now (gh run view --job <id> --log-failed) ===')
                    for name, url in failures:
                        print(f'  FAIL  {name:<28} {url}')
                    observed = self.observe(workflow, run)
                    if observed is not None:
                        snapshot, _ = observed
                        try:
                            self.journal.decision(workflow, int(run), snapshot['run_attempt'],
                                                  self.stop_reason, remaining, remaining, 'blocked')
                        except (ValueError, KeyError, TypeError, OSError):
                            pass
                    return 1
                snapshot = None
                if self.journal is not None:
                    snapshot, current_jobs = self.observe(workflow, run, required=True)
                    current = [(job['name'], job['html_url']) for job in current_jobs
                               if job['status'] == 'completed' and job['conclusion'] in FAILURES]
                    if (snapshot['status'] != 'completed' or not terminal_jobs(current_jobs)
                            or sorted(current) != sorted(failures)):
                        raise ValueError('publication failure changed before retry')
                if self.recovering:
                    try:
                        allowed = self.can_write()
                    except RecoveryReadUnavailable:
                        print('release-watch: publication identity unavailable; observing without retry', file=sys.stderr)
                        continue
                    if not allowed:
                        continue
                print(f'=== {workflow} run {run}: transient failure, auto-rerunning ({remaining} rerun(s) left for {workflow}) ===')
                for name, url in failures:
                    print(f'  rerun <- {name:<28} {url}')
                # Reserve the sole ledger before a possible remote side effect.
                # A failed/uncertain request never refunds this operation's budget.
                self.remaining[workflow] -= 1
                if snapshot:
                    self.journal.decision(workflow, int(run), snapshot['run_attempt'], 'publication-role',
                                          remaining, self.remaining[workflow], 'reserved')
                if self.recovering and self.clock() - self.started >= 7200:
                    self.journal.decision(workflow, int(run), snapshot['run_attempt'], 'publication-role',
                                          self.remaining[workflow], self.remaining[workflow], 'blocked')
                    continue
                status, _, _ = self.gh('run', 'rerun', run, '--failed')
                if snapshot:
                    self.journal.decision(workflow, int(run), snapshot['run_attempt'], 'publication-role',
                                          self.remaining[workflow], self.remaining[workflow],
                                          'uncertain' if status else 'requested')
                if status:
                    self.stop_reason = 'retry-request-unconfirmed'
                    print(f"=== {workflow} run {run}: 'gh run rerun --failed' itself failed; surfacing ===")
                    return 1
                if snapshot:
                    self.requested[workflow] = int(run), snapshot['run_attempt']
                self.wait_requeued(run)
            if all_green:
                used = sum(self.max_reruns - value for value in self.remaining.values())
                print(f'=== {self.tag}: {" ".join(WORKFLOWS)} all-green after {tick * self.interval}s (reruns used: {used}) ===')
                return 0
            self.sleep(self.interval)
        print(f'=== {self.tag}: still pending after {self.max_ticks * self.interval}s (max-ticks reached) ===')
        return 2


def main():
    if hasattr(sys.stdout, 'reconfigure'):
        sys.stdout.reconfigure(line_buffering=True)
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument('version')
    parser.add_argument('--max-reruns', type=int, default=2)
    parser.add_argument('--interval', type=int, default=60)
    parser.add_argument('--max-ticks', type=int, default=60)
    parser.add_argument('--journal')
    parser.add_argument('--operation')
    parser.add_argument('--repository')
    parser.add_argument('--sha')
    parser.add_argument('--kind', choices=('rc', 'stable'))
    parser.add_argument('--recover-tag-smoke', action='store_true')
    parser.add_argument('--tag-oid')
    try:
        args = parser.parse_args()
    except SystemExit:
        return 1 if sys.argv and sys.argv[-1] in ('--max-reruns', '--interval', '--max-ticks') else 3
    root = subprocess.run(['git', '-c', 'core.fsmonitor=false', 'rev-parse', '--show-toplevel'], capture_output=True, check=False)
    if root.returncode:
        print('release-watch: not in a git checkout', file=sys.stderr)
        return 3
    watch = Watcher(args.version, args.max_reruns, args.interval, args.max_ticks)
    if args.journal:
        try:
            watch.repository, watch.sha, watch.kind = args.repository, args.sha, args.kind
            if args.kind not in ('rc', 'stable'):
                raise ValueError('journal requires release kind')
            watch.journal = journals.Journal(args.journal, args.operation, args.repository, watch.tag, args.sha)
            watch.main_api = evidence.GitHub(args.repository)
        except (ValueError, TypeError, OSError):
            print('release-watch: cannot initialize recovery journal', file=sys.stderr)
            return 3
    if args.recover_tag_smoke:
        try:
            watch.enable_recovery(args.tag_oid)
        except RecoveryRefusal:
            print('release-watch: recovery requires complete pinned journal arguments', file=sys.stderr)
            return 3
    token = secrets.token_hex(16) if watch.journal is not None and os.environ.get('GITHUB_ACTIONS') == 'true' else None
    if token:
        print(f'::stop-commands::{token}', flush=True)
    try:
        return watch.run()
    except (ValueError, TypeError, AttributeError, OSError):
        print('release-watch: journal unavailable', file=sys.stderr)
        return 3
    finally:
        if token:
            print(f'::{token}::', flush=True)


if __name__ == '__main__':
    sys.exit(main())
