#!/usr/bin/env python3
"""Watch publication with one per-workflow retry ledger and the legacy CLI.

Publication retries are signature-independent and bounded per workflow.
assert-* and prep gates require manual recovery; their failures never gain a
publication retry merely because an infrastructure step failed.
"""
import argparse
import copy
import importlib.util
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time


WORKFLOWS = ('release.yml', 'brew.yml')
FAILURES = {'failure', 'cancelled', 'timed_out', 'action_required', 'startup_failure'}
JOBS_QUERY = '.jobs[] | [.status, (.conclusion // ""), .name, .url] | @tsv'


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
    def __init__(self, version, max_reruns=2, interval=60, max_ticks=60, sleep=time.sleep):
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
        if (requested and run['id'] == requested[0] and run['run_attempt'] > requested[1]
                and run['status'] == 'completed' and run['conclusion'] not in (None, '') and terminal_jobs(jobs)):
            passed = (run['conclusion'] == 'success' and bool(jobs)
                      and all(job['status'] == 'completed' and job['conclusion'] in ('success', 'skipped', 'neutral') for job in jobs))
            state = 'passed-on-retry' if passed else 'failed-on-retry'
            self.journal.decision(workflow, run['id'], run['run_attempt'], 'publication-role',
                                  self.remaining[workflow], self.remaining[workflow], state)
            del self.requested[workflow]
        return run, jobs

    def observe(self, workflow, run_id, required=False):
        try:
            return self.record(workflow, run_id, fresh=required)
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
                self.observe(workflow, run)
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
                print(f'=== {workflow} run {run}: transient failure, auto-rerunning ({remaining} rerun(s) left for {workflow}) ===')
                for name, url in failures:
                    print(f'  rerun <- {name:<28} {url}')
                snapshot = None
                if self.journal is not None:
                    snapshot, current_jobs = self.observe(workflow, run, required=True)
                    current = [(job['name'], job['html_url']) for job in current_jobs
                               if job['status'] == 'completed' and job['conclusion'] in FAILURES]
                    if (snapshot['status'] != 'completed' or not terminal_jobs(current_jobs)
                            or sorted(current) != sorted(failures)):
                        raise ValueError('publication failure changed before retry')
                # Reserve the sole ledger before a possible remote side effect.
                # A failed/uncertain request never refunds this operation's budget.
                self.remaining[workflow] -= 1
                if snapshot:
                    self.journal.decision(workflow, int(run), snapshot['run_attempt'], 'publication-role',
                                          remaining, self.remaining[workflow], 'reserved')
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
