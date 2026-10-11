#!/usr/bin/env python3
"""Offline regression fixtures for release metadata, API evidence and polling."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SOURCE = Path(os.environ.get('UZI_COVERAGE_MODULE', Path(__file__).with_name('release-ci-coverage.py')))
spec = importlib.util.spec_from_file_location('coverage_policy', SOURCE)
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


def run(workflow='ci.yml', sha='head', branch='main', conclusion='success', status='completed', number=2):
    return dict(id=number, run_attempt=2, path=f'.github/workflows/{workflow}',
                head_sha=sha, event='push', head_branch=branch,
                repository={'full_name': 'example/uzi'}, head_repository={'full_name': 'example/uzi'},
                status=status, conclusion=conclusion)


class RepoFixture:
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.oldcwd = os.getcwd()
        os.chdir(self.temp.name)
        self.addCleanup(os.chdir, self.oldcwd)
        self.addCleanup(self.temp.cleanup)
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.email', 'test@example.com')
        self.git('config', 'user.name', 'Fixture')
        self.git('config', 'core.autocrlf', 'false')
        self.files = {
            'CHANGELOG.md': '## [Unreleased]\n\n## [0.1.0]\n- initial\n',
            'deploy/chart/Chart.yaml': 'apiVersion: v2\nversion: 0.1.0\nappVersion: "0.1.0"\n',
            'deploy/chart/values.yaml': 'api:\n  image:\n    tag: "0.1.0"\nworkers:\n  image:\n    repository: example/worker\n    tag: "0.1.0"\n',
            'scripts/assert-worker-tag-decoupled.sh': '#!/bin/sh\nPINNED_TAG="0.1.0" # pin\necho safe\n',
        }
        for name, text in self.files.items():
            self.write(name, text)
        self.parent = self.commit()

    def git(self, *args):
        return subprocess.check_output(['git', '-c', 'core.fsmonitor=false', *args], text=True, stderr=subprocess.DEVNULL).strip()

    def write(self, name, text):
        path = Path(name)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def commit(self):
        self.git('add', '-A')
        self.git('commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    def changed(self, path, old, new):
        self.write(path, self.files[path].replace(old, new))
        return policy.metadata(self.parent, self.commit())



class Metadata(RepoFixture, unittest.TestCase):
    def test_release_metadata(self):
        for path, text in self.files.items():
            if path == 'deploy/chart/values.yaml':
                prefix, suffix = text.rsplit('0.1.0', 1)
                self.write(path, prefix + '0.2.0-rc.1' + suffix)
            else:
                self.write(path, text.replace('0.1.0', '0.2.0-rc.1'))
        self.assertTrue(policy.metadata(self.parent, self.commit()))

    def test_changelog_only(self):
        self.assertTrue(self.changed('CHANGELOG.md', '- initial', '- new release'))

    def test_other_image(self):
        path = 'deploy/chart/values.yaml'
        self.write(path, self.files[path].replace('tag: "0.1.0"', 'tag: "0.2.0"', 1))
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_duplicate_workers(self):
        path = 'deploy/chart/values.yaml'
        self.write(path, self.files[path] + 'workers:\n  image:\n    tag: "0.2.0"\n')
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_duplicate_image(self):
        self.assertFalse(self.changed('deploy/chart/values.yaml', '    repository:', '  image:\n    repository:'))

    def test_duplicate_tag(self):
        self.assertFalse(self.changed('deploy/chart/values.yaml', '    repository:', '    tag: "0.2.0"\n    repository:'))

    def test_payload(self):
        self.assertFalse(self.changed('scripts/assert-worker-tag-decoupled.sh', '0.1.0', '$(echo injected)'))

    def test_shell_tail(self):
        self.assertFalse(self.changed('scripts/assert-worker-tag-decoupled.sh', '# pin', '; echo injected'))

    def test_invalid_version(self):
        self.assertFalse(self.changed('deploy/chart/Chart.yaml', '0.1.0', '01.2.0'))

    def test_line_endings(self):
        path = 'deploy/chart/Chart.yaml'
        Path(path).write_bytes(self.files[path].replace('0.1.0', '0.2.0').replace('\n', '\r\n').encode())
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_quoted_chart_duplicate(self):
        path = 'deploy/chart/Chart.yaml'
        duplicate = '"version": x\n' + self.files[path]
        self.write(path, duplicate)
        self.parent = self.commit()
        self.write(path, duplicate.replace('version: 0.1.0', 'version: 0.2.0'))
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_chart_duplicate(self):
        self.assertFalse(self.changed('deploy/chart/Chart.yaml', 'apiVersion:', 'version: 0.2.0\napiVersion:'))

    def test_extra_file(self):
        self.write('api/main.go', 'package main\n')
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_mode(self):
        Path('CHANGELOG.md').chmod(0o755)
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_rename(self):
        self.git('mv', 'CHANGELOG.md', 'RENAMED.md')
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_delete(self):
        Path('CHANGELOG.md').unlink()
        self.assertFalse(policy.metadata(self.parent, self.commit()))

    def test_merge(self):
        self.git('checkout', '-qb', 'other')
        self.write('CHANGELOG.md', 'other\n')
        other = self.commit()
        self.git('checkout', '-q', 'main')
        self.write('deploy/chart/Chart.yaml', self.files['deploy/chart/Chart.yaml'].replace('0.1.0', '0.2.0'))
        first = self.commit()
        self.git('merge', '--no-ff', '-qm', 'merge', other)
        self.assertFalse(policy.metadata(first, self.git('rev-parse', 'HEAD')))

    def test_root(self):
        self.assertFalse(policy.metadata(self.parent, self.parent))


class Chain(RepoFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.anchor = self.parent
        self.nodes = [self.anchor]
        for n in range(1, 4):
            self.write('deploy/chart/Chart.yaml', self.files['deploy/chart/Chart.yaml'].replace('0.1.0', f'0.1.{n}'))
            self.nodes.append(self.commit())
        self.api = unittest.mock.Mock()
        self.api.run.side_effect = lambda w, sha, b: run(w, sha) if sha == self.anchor else run(w, sha, conclusion='cancelled')

    def prove(self, target):
        chain = policy.metadata_chain(target)
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            policy.coverage(self.api, target, None, False, 0, 1, chain=chain)
        return output.getvalue()

    def test_two_hops(self):
        output = self.prove(self.nodes[2])
        self.assertIn(' -> '.join(reversed(self.nodes[:3])), output)
        self.assertIn('coverage SHA ' + self.anchor, output)

    def test_three_hops_refused(self):
        with self.assertRaises(policy.Refusal):
            self.prove(self.nodes[3])

    def test_middle_non_metadata_refused(self):
        self.git('checkout', '-q', self.anchor)
        self.write('api/change.go', 'package main\n')
        middle = self.commit()
        self.write('deploy/chart/Chart.yaml', self.files['deploy/chart/Chart.yaml'].replace('0.1.0', '0.2.0'))
        target = self.commit()
        self.assertEqual(policy.metadata_chain(target), [target, middle])
        with self.assertRaises(policy.Refusal):
            self.prove(target)

    def test_middle_failure_veto(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, conclusion='failure') if sha == self.nodes[1] else (run(w, sha) if sha == self.anchor else None)
        with self.assertRaisesRegex(policy.Refusal, 'failed CI'):
            self.prove(self.nodes[2])

    def test_stop_at_first_covered_commit(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha) if sha == self.nodes[2] else run(w, sha, conclusion='failure')
        output = self.prove(self.nodes[2])
        self.assertIn('chain ' + self.nodes[2], output)
        self.assertNotIn(' -> ', output)
        self.assertEqual(self.api.run.call_count, 2)


class Cutter(RepoFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        root = Path(__file__).resolve().parent.parent
        self.cutter = Path(os.environ.get('UZI_CUTTER_SCRIPT', root / '.agents/skills/uzi-release/scripts/release-cut.sh'))
        for name in ('shipping-paths.sh', 'dependency-bump.sh'):
            self.write('scripts/lib/' + name, (root / 'scripts/lib' / name).read_text())
        self.write('scripts/release-ci-coverage.py', "import sys\nfrom pathlib import Path\nwith Path('coverage-calls').open('a') as f: f.write(' '.join(sys.argv[1:]) + '\\n')\nraise SystemExit(1)\n")
        self.commit()
        self.git('tag', 'v0.1.0')
        self.git('tag', 'v0.2.0-rc.1')
        origin = tempfile.TemporaryDirectory()
        self.addCleanup(origin.cleanup)
        subprocess.check_call(['git', 'init', '-q', '--bare', origin.name])
        self.git('remote', 'add', 'origin', origin.name)
        self.git('push', '-q', 'origin', 'main', '--tags')

    def cut(self, *args):
        env = {**os.environ}
        env.pop('UZI_RELEASE_OFFLINE_FIXTURE', None)
        result = subprocess.run(['bash', str(self.cutter), *args], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 3, result.stderr + result.stdout)
        return Path('coverage-calls').read_text()

    def test_promote_only_checks_candidate_without_main(self):
        calls = self.cut('0.2.0', '--promote-only')
        self.assertEqual(calls, 'coverage v0.2.0-rc.1 --wait 0\n')

    def test_cut_still_checks_exact_main(self):
        calls = self.cut('0.2.0')
        self.assertEqual(calls, 'tip HEAD --wait 0\n')


class Hotfix(RepoFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.git('tag', 'v0.1.0', self.parent)
        self.git('update-ref', 'refs/remotes/origin/main', self.parent)
        self.git('checkout', '-qb', 'hotfix')
        self.write('api/fix.go', 'package main // stable backport\n')
        self.sha = self.commit()

    def annotate(self, tag='v0.1.1', message=policy.HOTFIX_MARKER, ref=None):
        self.git('tag', '-a', tag, '-m', tag, '-m', message, ref or self.sha)
        return tag

    def test_happy_path(self):
        tag = self.annotate()
        api = unittest.mock.Mock()
        api.run.return_value = None
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            policy.coverage(api, self.sha, self.parent, False, 0, 1, tag)
        self.assertIn('UNVERIFIED hotfix: no CI evidence', output.getvalue())
        self.assertIn('base v0.1.0', output.getvalue())

    def test_hotfix_job_output(self):
        tag = self.annotate()
        api = unittest.mock.Mock()
        api.run.return_value = None
        output = Path(self.temp.name) / 'job-output'
        with patch.dict(policy.os.environ, {'GITHUB_OUTPUT': str(output)}), \
             patch.object(policy.sys, 'argv', ['policy', 'coverage', self.sha, '--tag', tag, '--repo', 'example/uzi', '--wait', '0']), \
             patch.object(policy, 'GitHub', return_value=api), contextlib.redirect_stdout(io.StringIO()):
            policy.main()
        self.assertEqual(output.read_text(), 'hotfix_unverified=true\n')

    def test_verified_job_output(self):
        api = unittest.mock.Mock()
        api.run.side_effect = lambda w, sha, b: run(w, sha)
        output = Path(self.temp.name) / 'job-output'
        with patch.dict(policy.os.environ, {'GITHUB_OUTPUT': str(output)}), \
             patch.object(policy.sys, 'argv', ['policy', 'coverage', self.sha, '--repo', 'example/uzi', '--wait', '0']), \
             patch.object(policy, 'GitHub', return_value=api), contextlib.redirect_stdout(io.StringIO()):
            policy.main()
        self.assertEqual(output.read_text(), 'hotfix_unverified=false\n')

    def test_reachable_main_rejected(self):
        tag = self.annotate()
        self.git('update-ref', 'refs/remotes/origin/main', self.sha)
        with self.assertRaisesRegex(policy.Refusal, 'outside origin/main'):
            policy.hotfix_exception(self.sha, tag)

    def test_non_patch_rejected(self):
        tag = self.annotate('v0.2.0')
        with self.assertRaisesRegex(policy.Refusal, 'stable patch'):
            policy.hotfix_exception(self.sha, tag)

    def test_rc_rejected(self):
        tag = self.annotate('v0.1.1-rc.1')
        with self.assertRaisesRegex(policy.Refusal, 'stable patch'):
            policy.hotfix_exception(self.sha, tag)

    def test_lightweight_rejected(self):
        self.git('tag', 'v0.1.1', self.sha)
        self.assertIsNone(policy.hotfix_exception(self.sha, 'v0.1.1'))

    def test_missing_marker_rejected(self):
        tag = self.annotate(message='normal annotation')
        self.assertIsNone(policy.hotfix_exception(self.sha, tag))

    def test_inline_marker_rejected(self):
        tag = self.annotate(message='prefix ' + policy.HOTFIX_MARKER)
        self.assertIsNone(policy.hotfix_exception(self.sha, tag))

    def test_duplicate_marker_rejected(self):
        tag = self.annotate(message=policy.HOTFIX_MARKER + '\n' + policy.HOTFIX_MARKER)
        with self.assertRaisesRegex(policy.Refusal, 'exactly one'):
            policy.hotfix_exception(self.sha, tag)

    def test_target_mismatch_rejected(self):
        tag = self.annotate(ref=self.parent)
        with self.assertRaisesRegex(policy.Refusal, 'different commit'):
            policy.hotfix_exception(self.sha, tag)

    def test_missing_previous_rejected(self):
        tag = self.annotate('v0.1.2')
        with self.assertRaises(policy.Refusal):
            policy.hotfix_exception(self.sha, tag)

    def test_no_new_commit_rejected(self):
        tag = self.annotate(ref=self.parent)
        self.git('checkout', '--orphan', 'separate-main')
        self.git('rm', '-rf', '.')
        self.write('separate.txt', 'independent main\n')
        main_sha = self.commit()
        self.git('update-ref', 'refs/remotes/origin/main', main_sha)
        with self.assertRaises(policy.Refusal):
            policy.hotfix_exception(self.parent, tag)

    def test_second_parent_rejected(self):
        self.git('checkout', '--orphan', 'isolated')
        self.git('rm', '-rf', '.')
        self.write('isolated.txt', 'independent history\n')
        self.commit()
        self.git('merge', '--allow-unrelated-histories', '--no-ff', '-qm', 'merge', 'v0.1.0')
        sha = self.git('rev-parse', 'HEAD')
        tag = self.annotate(ref=sha)
        with self.assertRaisesRegex(policy.Refusal, 'first-parent'):
            policy.hotfix_exception(sha, tag)

    def test_known_failure_not_bypassed(self):
        tag = self.annotate()
        api = unittest.mock.Mock()
        api.run.return_value = run(conclusion='failure')
        with self.assertRaisesRegex(policy.Refusal, 'failed CI'):
            policy.coverage(api, self.sha, self.parent, False, 0, 1, tag)

    def test_audit_after_merge(self):
        tag = self.annotate()
        self.git('update-ref', 'refs/remotes/origin/main', self.sha)
        self.assertTrue(policy.annotated_hotfix(self.sha, tag))
        with self.assertRaises(policy.Refusal):
            policy.hotfix_exception(self.sha, tag)

    def test_unknown_main_rejected(self):
        tag = self.annotate()
        self.git('update-ref', '-d', 'refs/remotes/origin/main')
        with self.assertRaisesRegex(policy.Refusal, 'outside origin/main'):
            policy.hotfix_exception(self.sha, tag)


class Evidence(unittest.TestCase):
    def api(self, runs):
        api = policy.GitHub('example/uzi')
        self.command = patch.object(policy, 'command', return_value=json.dumps([{'total_count': len(runs), 'workflow_runs': runs}]))
        self.command.start()
        self.addCleanup(self.command.stop)
        return api

    def test_qualified(self):
        self.assertEqual(self.api([run()]).run('ci.yml', 'head', 'main')['id'], 2)

    def test_provenance(self):
        for field, value in [('head_sha', 'wrong'), ('event', 'pull_request'), ('head_branch', 'topic'),
                             ('repository', {'full_name': 'other/uzi'}), ('head_repository', {'full_name': 'other/uzi'}),
                             ('path', '.github/workflows/other.yml')]:
            with self.subTest(field=field), patch.object(policy, 'command', return_value=json.dumps([
                    {'total_count': 1, 'workflow_runs': [{**run(), field: value}]}])):
                self.assertIsNone(policy.GitHub('example/uzi').run('ci.yml', 'head', 'main'))

    def test_newest_failure(self):
        latest = self.api([run(number=1), run(number=3, conclusion='failure')]).run('ci.yml', 'head', 'main')
        self.assertEqual(policy.state(latest), 'failed')

    def test_current_attempt(self):
        latest = self.api([{**run(), 'run_attempt': 3, 'status': 'queued', 'conclusion': None}]).run('ci.yml', 'head', 'main')
        self.assertEqual(policy.state(latest), 'pending')

    def test_pagination(self):
        pages = [{'total_count': 2, 'workflow_runs': [run(number=1)]},
                 {'total_count': 2, 'workflow_runs': [run(number=4)]}]
        with patch.object(policy, 'command', return_value=json.dumps(pages)) as cmd:
            self.assertEqual(policy.GitHub('example/uzi').run('ci.yml', 'head', 'main')['id'], 4)
            self.assertIn('--paginate', cmd.call_args.args)
            self.assertIn('head_sha=head', cmd.call_args.args[-1])
            self.assertIn('workflows/ci.yml/runs', cmd.call_args.args[-1])

    def test_truncated(self):
        with patch.object(policy, 'command', return_value=json.dumps([{'total_count': 2, 'workflow_runs': [run()]}])):
            with self.assertRaises(policy.Refusal):
                policy.GitHub('example/uzi').run('ci.yml', 'head', 'main')

    def test_api_error(self):
        with patch.object(policy.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, b'', b'403 denied')):
            with self.assertRaisesRegex(policy.Refusal, '403 denied'):
                policy.GitHub('example/uzi').run('ci.yml', 'head', 'main')

    def test_invalid_json(self):
        with patch.object(policy, 'command', return_value='not json'):
            with self.assertRaises(policy.Refusal):
                policy.GitHub('example/uzi').run('ci.yml', 'head', 'main')

    def test_attempt_jobs(self):
        api = policy.GitHub('example/uzi')
        with patch.object(api, 'pages', return_value=[{'name': 'kind-smoke', 'id': 8}]) as pages:
            self.assertEqual(api.smoke_job(run())['id'], 8)
            self.assertIn('/attempts/2/jobs', pages.call_args.args[0])


class Decision(unittest.TestCase):
    def setUp(self):
        self.output = io.StringIO()
        redirect = contextlib.redirect_stdout(self.output)
        redirect.__enter__()
        self.addCleanup(redirect.__exit__, None, None, None)
        self.api = unittest.mock.Mock()
        self.proc = patch.object(policy.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0))
        self.proc.start()
        self.addCleanup(self.proc.stop)

    def test_inherit_without_waiting(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, status='queued', conclusion=None) if sha == 'head' else run(w, sha)
        with patch.object(policy.time, 'sleep', side_effect=AssertionError('must not wait')):
            policy.coverage(self.api, 'head', 'parent', True, 100, 1)
        self.assertIn('coverage SHA parent', self.output.getvalue())

    def test_exact_success(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha)
        policy.coverage(self.api, 'head', None, False, 0, 1)
        self.assertIn('coverage SHA head', self.output.getvalue())

    def test_failure_blocks_inheritance(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, conclusion='failure') if sha == 'head' else run(w, sha)
        with self.assertRaises(policy.Refusal):
            policy.coverage(self.api, 'head', 'parent', True, 0, 1)

    def test_cancelled_head_inherits(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, conclusion='cancelled') if sha == 'head' else run(w, sha)
        policy.coverage(self.api, 'head', 'parent', True, 0, 1)
        self.assertIn('coverage SHA parent', self.output.getvalue())

    def test_skipped_head_inherits(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, conclusion='skipped') if sha == 'head' else run(w, sha)
        policy.coverage(self.api, 'head', 'parent', True, 0, 1)
        self.assertIn('coverage SHA parent', self.output.getvalue())

    def test_cancelled_parent_pending_head_deadline(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, conclusion='cancelled') if sha == 'parent' else run(w, sha, status='in_progress', conclusion=None)
        with self.assertRaisesRegex(policy.Refusal, 'no green main-push coverage'):
            policy.coverage(self.api, 'head', 'parent', True, 0, 1)

    def test_cancelled_parent_waits_for_head(self):
        self.api.run.side_effect = [run(status='queued', conclusion=None), run('kind-smoke.yml', status='queued', conclusion=None),
                                   run(conclusion='cancelled'), run('kind-smoke.yml', conclusion='cancelled'),
                                   run(), run('kind-smoke.yml'), run(conclusion='cancelled'), run('kind-smoke.yml', conclusion='cancelled')]
        with patch.object(policy.time, 'sleep') as sleep:
            policy.coverage(self.api, 'head', 'parent', True, 10, 1)
            sleep.assert_called_once()
        self.assertIn('coverage SHA head', self.output.getvalue())

    def test_real_failure_conclusions_veto(self):
        for conclusion in ('failure', 'timed_out', 'action_required', 'startup_failure', 'neutral', 'stale'):
            with self.subTest(conclusion=conclusion):
                self.api.run.side_effect = lambda w, sha, b: run(w, sha, conclusion=conclusion) if sha == 'head' else run(w, sha)
                with self.assertRaisesRegex(policy.Refusal, 'failed CI'):
                    policy.coverage(self.api, 'head', 'parent', True, 0, 1)

    def test_unknown_conclusion_refuses(self):
        self.api.run.return_value = run(conclusion='unknown-future-conclusion')
        with self.assertRaisesRegex(policy.Refusal, 'unrecognized completed workflow conclusion'):
            policy.coverage(self.api, 'head', 'parent', True, 0, 1)

    def test_parent_failure(self):
        self.api.run.side_effect = lambda w, sha, b: None if sha == 'head' else run(w, sha, conclusion='failure')
        with self.assertRaises(policy.Refusal):
            policy.coverage(self.api, 'head', 'parent', True, 0, 1)

    def test_cancelled(self):
        self.api.run.return_value = run(conclusion='cancelled')
        with self.assertRaises(policy.Refusal):
            policy.coverage(self.api, 'head', None, False, 0, 1)

    def test_absent(self):
        self.api.run.return_value = None
        with self.assertRaises(policy.Refusal):
            policy.coverage(self.api, 'head', None, False, 0, 1)

    def test_no_inheritance(self):
        self.api.run.side_effect = lambda w, sha, b: None if sha == 'head' else run(w, sha)
        with self.assertRaises(policy.Refusal):
            policy.coverage(self.api, 'head', 'parent', False, 0, 1)

    def test_tip_pending_refuses(self):
        self.api.run.side_effect = lambda w, sha, b: run(w, sha, status='queued', conclusion=None) if sha == 'head' else run(w, sha)
        with patch.object(policy.sys, 'argv', ['policy', 'tip', 'head', '--repo', 'example/uzi', '--wait', '0']), \
             patch.object(policy, 'git', side_effect=lambda *args: 'head' if args[0] == 'rev-parse' else 'head parent'), \
             patch.object(policy, 'metadata', return_value=True), \
             patch.object(policy, 'GitHub', return_value=self.api), \
             patch.object(policy.time, 'sleep', side_effect=AssertionError('tip must refuse before push')):
            with self.assertRaisesRegex(policy.Refusal, 'queued'):
                policy.main()

    def test_poll(self):
        self.api.run.side_effect = [run(status='queued', conclusion=None), run('kind-smoke.yml', status='queued', conclusion=None), run(), run('kind-smoke.yml')]
        with patch.object(policy.time, 'sleep') as sleep:
            policy.coverage(self.api, 'head', None, False, 10, 1)
            sleep.assert_called_once()

    def test_promotion_without_main_run(self):
        self.api.run.side_effect = lambda w, sha, b: None if sha == 'head' else run(w, sha)
        policy.coverage(self.api, 'head', 'parent', True, 0, 1)
        self.assertIn('coverage SHA parent', self.output.getvalue())

    def test_promotion_fail_fast(self):
        self.api.run.return_value = None
        with patch.object(policy.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1)), patch.object(policy.time, 'sleep', side_effect=AssertionError('off-main must fail fast')):
            with self.assertRaises(policy.Refusal):
                policy.coverage(self.api, 'head', 'parent', True, 100, 1)

    def test_real_smoke(self):
        self.api.run.return_value = run('kind-smoke.yml', branch='v0.2.0')
        self.api.smoke_job.return_value = {'id': 8, 'status': 'completed', 'conclusion': 'success'}
        policy.smoke(self.api, 'head', 'v0.2.0', 0, 1)
        self.api.run.assert_called_with('kind-smoke.yml', 'head', 'v0.2.0')
        self.assertIn('job 8', self.output.getvalue())

    def test_cancelled_tag_smoke_refuses(self):
        self.api.run.return_value = run('kind-smoke.yml', conclusion='cancelled')
        with self.assertRaisesRegex(policy.Refusal, 'concluded cancelled'):
            policy.smoke(self.api, 'head', 'v0.2.0', 0, 1)
        self.api.smoke_job.assert_not_called()

    def test_skipped_tag_smoke_refuses(self):
        self.api.run.return_value = run('kind-smoke.yml', conclusion='skipped')
        with self.assertRaisesRegex(policy.Refusal, 'concluded skipped'):
            policy.smoke(self.api, 'head', 'v0.2.0', 0, 1)
        self.api.smoke_job.assert_not_called()

    def test_skipped_smoke(self):
        self.api.run.return_value = run('kind-smoke.yml')
        self.api.smoke_job.return_value = {'id': 8, 'status': 'completed', 'conclusion': 'skipped'}
        with self.assertRaises(policy.Refusal):
            policy.smoke(self.api, 'head', 'v0.2.0', 0, 1)

    def test_pending_smoke_deadline(self):
        self.api.run.return_value = None
        with self.assertRaises(policy.Refusal):
            policy.smoke(self.api, 'head', 'v0.2.0', 0, 1)

    def test_missing_real_job(self):
        api = policy.GitHub('example/uzi')
        with patch.object(api, 'pages', return_value=[{'name': 'kind-smoke-gate'}]):
            with self.assertRaises(policy.Refusal):
                api.smoke_job(run('kind-smoke.yml'))


if __name__ == '__main__':
    unittest.main(verbosity=2)
