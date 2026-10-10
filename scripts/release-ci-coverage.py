#!/usr/bin/env python3
"""Fail-closed release coverage and tagged smoke, using only git and gh reads."""
import argparse
import json
import os
import re
import subprocess
import sys
import time


class Refusal(Exception):
    """Coverage is absent, failed, or cannot be established."""


def command(*args):
    result = subprocess.run(args, capture_output=True, check=False)
    if result.returncode:
        raise Refusal(f"{args[0]} failed: {result.stderr.decode(errors='replace').strip()}")
    return result.stdout.decode()


def git(*args):
    return command("git", "-c", "core.fsmonitor=false", *args)


VERSION = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-rc\.[1-9][0-9]*)?"


def normalize_scalar(text, key, indent=0):
    """Replace one version token, preserving quotes, comments and all other bytes."""
    prefix = " " * indent + re.escape(key)
    pattern = re.compile(rf'(?m)^({prefix}[ \t]*:[ \t]*)([\"\']?)({VERSION})(\2)([ \t]*(?:#[^\n]*)?)$')
    matches = list(pattern.finditer(text))
    # Count declarations too: an extra malformed/quoted key must not disappear.
    declarations = re.findall(
        rf'(?m)^{" " * indent}(?:{re.escape(key)}|"{re.escape(key)}"|\'{re.escape(key)}\')[ \t]*:', text)
    if len(matches) != 1 or len(declarations) != 1:
        raise Refusal(f"expected one valid {key} scalar")
    return pattern.sub(lambda m: m[1] + m[2] + "VERSION" + m[4] + m[5], text)


def block(text, key, indent=0):
    # Only the current block-mapping format qualifies for inheritance. Unsupported
    # YAML forms require green CI on the exact tagged SHA instead of a new parser.
    lines = text.splitlines(keepends=True)
    declarations = [i for i, line in enumerate(lines) if re.match(
        rf'^{" " * indent}(?:{key}|"{key}"|\'{key}\')[ \t]*:', line)]
    if len(declarations) != 1:
        raise Refusal(f"expected unique mapping {key}")
    start = declarations[0]
    if not re.fullmatch(rf'{" " * indent}{key}:[ \t]*(?:#[^\n]*)?\n?', lines[start]):
        raise Refusal(f"unsupported mapping {key}")
    end = start + 1
    while end < len(lines):
        line = lines[end]
        if line.strip() and not line.lstrip().startswith('#'):
            spaces = len(line) - len(line.lstrip(' '))
            if spaces <= indent:
                break
        end += 1
    return ''.join(lines[start:end])


def normalized(path, text):
    if path == "deploy/chart/Chart.yaml":
        return normalize_scalar(normalize_scalar(text, "version"), "appVersion")
    if path == "deploy/chart/values.yaml":
        workers = block(text, "workers")
        image = block(workers, "image", 2)
        normalized_image = normalize_scalar(image, "tag", 4)
        return text.replace(workers, workers.replace(image, normalized_image, 1), 1)
    if path == "scripts/assert-worker-tag-decoupled.sh":
        pattern = re.compile(rf'(?m)^(PINNED_TAG=")({VERSION})("[ \t]*(?:#[^\n]*)?)$')
        if len(pattern.findall(text)) != 1 or len(re.findall(r'(?m)^PINNED_TAG=', text)) != 1:
            raise Refusal("expected one literal PINNED_TAG version")
        return pattern.sub(lambda m: m[1] + "VERSION" + m[3], text)
    raise Refusal(f"non-metadata path: {path}")


def metadata(parent, sha):
    parents = git("rev-list", "--parents", "-n", "1", sha).split()
    if parents != [sha, parent]:
        return False
    # Raw diff includes modes and object types; --no-renames makes a rename A/D.
    changes = git("diff", "--raw", "--no-renames", parent, sha, "--").splitlines()
    try:
        for change in changes:
            header, path = change.split('\t')
            oldmode, newmode, _, _, status = header.split()
            if status != 'M' or oldmode[1:] != newmode or newmode not in ('100644', '100755'):
                return False
            if path == 'CHANGELOG.md':
                continue
            old = git("show", f"{parent}:{path}")
            new = git("show", f"{sha}:{path}")
            if normalized(path, old) != normalized(path, new):
                return False
        return True
    except (Refusal, ValueError):
        return False


class GitHub:
    def __init__(self, repo):
        if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repo):
            raise Refusal('invalid repository')
        self.repo = repo

    def pages(self, endpoint, field):
        raw = command('gh', 'api', '--paginate', '--slurp', endpoint)
        try:
            pages = json.loads(raw)
            if not isinstance(pages, list) or not pages:
                raise ValueError('missing pages')
            items = [item for page in pages for item in page[field]]
            if any(page['total_count'] != len(items) for page in pages):
                raise ValueError('incomplete or changing pagination')
            return items
        except (ValueError, KeyError, TypeError) as exc:
            raise Refusal(f'invalid {field} API response: {exc}') from exc

    def run(self, workflow, sha, branch):
        endpoint = f'repos/{self.repo}/actions/workflows/{workflow}/runs?event=push&head_sha={sha}&per_page=100'
        runs = self.pages(endpoint, 'workflow_runs')
        eligible = []
        for run in runs:
            # Do not accept PR aggregates, other branches, forks or another workflow.
            if (run.get('head_sha') == sha and run.get('event') == 'push'
                    and run.get('head_branch') == branch
                    and run.get('repository', {}).get('full_name') == self.repo
                    and run.get('head_repository', {}).get('full_name') == self.repo
                    and run.get('path', '').split('@')[0] == f'.github/workflows/{workflow}'):
                if not isinstance(run.get('run_attempt'), int) or run['run_attempt'] < 1:
                    raise Refusal('missing current run attempt')
                eligible.append(run)
        # A previous successful run cannot mask a newer failed/requeued run.
        newest = max(eligible, key=lambda r: r['id']) if eligible else None
        # Keep proof/error logs bounded and omit actor/profile/repository payloads.
        return {key: newest[key] for key in ('id', 'run_attempt', 'status', 'conclusion')} if newest else None

    def smoke_job(self, run):
        endpoint = f"repos/{self.repo}/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs?per_page=100"
        jobs = [j for j in self.pages(endpoint, 'jobs') if j.get('name') == 'kind-smoke']
        if not jobs and state(run) == 'pending':
            return None
        if len(jobs) != 1:
            raise Refusal('expected exactly one real kind-smoke job')
        return jobs[0]


def state(run):
    if run is None:
        return 'absent'
    if run.get('status') == 'completed':
        return 'success' if run.get('conclusion') == 'success' else 'failed'
    if run.get('status') in ('queued', 'in_progress', 'waiting', 'pending', 'requested'):
        return 'pending'
    raise Refusal('unrecognized workflow status')


HOTFIX_MARKER = 'CI-Coverage: hotfix-uncovered'


def annotated_hotfix(sha, tag):
    """Read immutable tag intent for audit; this alone never permits publication."""
    if not tag or not re.fullmatch('v' + VERSION, tag):
        return False
    ref = 'refs/tags/' + tag
    if git('cat-file', '-t', ref).strip() != 'tag':
        return False
    message = git('cat-file', '-p', ref).partition('\n\n')[2]
    if HOTFIX_MARKER not in message.splitlines():
        return False
    if message.splitlines().count(HOTFIX_MARKER) != 1:
        raise Refusal('hotfix annotation must contain exactly one standalone marker')
    if git('rev-parse', '--verify', ref + '^{commit}').strip() != sha:
        raise Refusal('hotfix annotation targets a different commit')
    return True


def hotfix_exception(sha, tag):
    """Recognize an explicit admin-tag exception, never evidence of successful CI."""
    if not annotated_hotfix(sha, tag):
        return None
    version = re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.([1-9][0-9]*)', tag)
    if not version:
        raise Refusal('hotfix exception requires a stable patch tag with patch >= 1')
    result = subprocess.run(['git', 'merge-base', '--is-ancestor', sha, 'refs/remotes/origin/main'], check=False)
    if result.returncode != 1:
        raise Refusal('hotfix exception requires a known commit outside origin/main')
    previous = f'v{version[1]}.{version[2]}.{int(version[3]) - 1}'
    base = git('rev-parse', '--verify', f'refs/tags/{previous}^{{commit}}').strip()
    if base == sha or base not in git('rev-list', '--first-parent', sha).splitlines():
        raise Refusal(f'hotfix must descend on its first-parent chain from {previous}')
    return f'UNVERIFIED hotfix: no CI evidence; same as pre-gate behaviour; tag {tag}, SHA {sha}, base {previous} ({base})'


def coverage(api, sha, parent, inherit, wait, interval, tag=None):
    deadline = time.monotonic() + wait
    workflows = ('ci.yml', 'kind-smoke.yml')
    while True:
        heads = {w: api.run(w, sha, 'main') for w in workflows}
        if any(state(r) == 'failed' for r in heads.values()):
            raise Refusal(f'tagged/main SHA {sha} has failed CI: {heads}')
        exception = hotfix_exception(sha, tag) if tag else None
        if exception:
            print(exception, flush=True)
            return True
        selected = {}
        for workflow in workflows:
            ancestor = api.run(workflow, parent, 'main') if inherit else None
            if state(ancestor) == 'success':
                selected[workflow] = (parent, ancestor)
            elif state(heads[workflow]) == 'success':
                selected[workflow] = (sha, heads[workflow])
        if len(selected) == len(workflows):
            for workflow, (covered, run) in selected.items():
                print(f'PASS {workflow}: coverage SHA {covered}, run {run["id"]}, attempt {run["run_attempt"]}', flush=True)
            return False
        # A promotion commit off main has no prospective main runs to wait for.
        on_main = subprocess.run(['git', 'merge-base', '--is-ancestor', sha, 'refs/remotes/origin/main'], check=False).returncode == 0
        if not on_main or time.monotonic() >= deadline:
            raise Refusal(f'no green main-push coverage for {sha}; selected={list(selected)}; heads={heads}; off-main hotfixes require the documented annotated-tag exception')
        print(f'Waiting for main-push coverage of {sha}', flush=True)
        time.sleep(min(interval, max(0, deadline - time.monotonic())))


def smoke(api, sha, tag, wait, interval):
    deadline = time.monotonic() + wait
    while True:
        run = api.run('kind-smoke.yml', sha, tag)
        if state(run) == 'failed':
            raise Refusal(f'tag smoke run {run["id"]} failed')
        if run:
            job = api.smoke_job(run)
            if job and job.get('status') == 'completed':
                if job.get('conclusion') != 'success':
                    raise Refusal(f'real kind-smoke job {job["id"]} did not succeed')
                if state(run) == 'success':
                    print(f'PASS tag smoke: SHA {sha}, run {run["id"]}, attempt {run["run_attempt"]}, job {job["id"]}')
                    return
        if time.monotonic() >= deadline:
            raise Refusal(f'no successful real tag smoke for {tag} at {sha}')
        print(f'Waiting for real tag smoke {tag} at {sha}', flush=True)
        time.sleep(min(interval, max(0, deadline - time.monotonic())))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('metadata', 'coverage', 'tip', 'smoke', 'annotation'))
    parser.add_argument('ref')
    parser.add_argument('other', nargs='?')
    parser.add_argument('--repo')
    parser.add_argument('--tag', help='actual annotated release tag; enables the explicit hotfix exception')
    parser.add_argument('--wait', type=float, default=2700)
    parser.add_argument('--interval', type=float, default=15)
    args = parser.parse_args()
    if args.wait < 0 or args.interval <= 0:
        raise Refusal('invalid poll bounds')
    sha = git('rev-parse', '--verify', f'{args.ref}^{{commit}}').strip()
    if args.mode == 'metadata':
        if not args.other:
            raise Refusal('metadata needs parent and commit')
        target = git('rev-parse', '--verify', f'{args.other}^{{commit}}').strip()
        if not metadata(sha, target):
            raise Refusal('diff is not strictly release metadata')
        print('PASS strictly release metadata')
        return
    if args.mode == 'annotation':
        if not annotated_hotfix(sha, args.tag):
            raise Refusal('no explicit annotated hotfix intent')
        print(f'UNVERIFIED hotfix annotation: no full CI evidence; tag {args.tag}, SHA {sha}')
        return
    repo = args.repo or os.environ.get('GITHUB_REPOSITORY') or command('gh', 'repo', 'view', '--json', 'nameWithOwner', '--jq', '.nameWithOwner').strip()
    api = GitHub(repo)
    if args.mode == 'smoke':
        if not args.other or not re.fullmatch('v' + VERSION, args.other):
            raise Refusal('smoke needs a valid release tag')
        smoke(api, sha, args.other, args.wait, args.interval)
    else:
        parents = git('rev-list', '--parents', '-n', '1', sha).split()
        parent = parents[1] if len(parents) == 2 else None
        inherit = args.mode == 'coverage' and parent is not None and metadata(parent, sha)
        unverified = coverage(api, sha, parent, inherit, args.wait, args.interval, args.tag if args.mode == 'coverage' else None)
        if args.mode == 'coverage' and os.environ.get('GITHUB_OUTPUT'):
            with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
                output.write(f'hotfix_unverified={str(unverified).lower()}\n')


if __name__ == '__main__':
    try:
        main()
    except (Refusal, ValueError, KeyError, TypeError) as error:
        print(f'release-ci-coverage: FAIL: {error}', file=sys.stderr)
        sys.exit(1)
