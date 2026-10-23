#!/usr/bin/env python3
"""Exercise the real offline checker with independent malformed/drift fixtures."""
import copy
import importlib.util
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

os.environ.update(GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")

SPEC = importlib.util.spec_from_file_location('worker_toolchain', pathlib.Path(__file__).with_name('check-worker-toolchain.py'))
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class ToolchainTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        (self.root / 'agent/devbox-global').mkdir(parents=True)
        for module in ('api', 'controller'):
            (self.root / module).mkdir()
            (self.root / module / 'go.mod').write_text('module example.test/' + module + '\ngo 1.26.4\ntoolchain go1.27.3\n')
        self.key = 'go@1.27.3'
        self.packages = [self.key, 'jq.bin']
        self.lock = {'packages': {self.key: {'version': '1.27.3', 'systems': {'x86_64-linux': {
            'outputs': [{'path': '/nix/store/' + 'a' * 32 + '-go-1.27.3', 'default': True}]}}}, 'jq.bin': {}}}

    def write(self):
        directory = self.root / 'agent/devbox-global'
        (directory / 'devbox.json').write_text('// manifest\n' + json.dumps({'packages': self.packages}))
        (directory / 'devbox.lock').write_text(json.dumps(self.lock))

    def run_check(self, **options):
        self.write()
        return CHECK.check(self.root, **options)

    def test_aligned(self):
        self.assertEqual(self.run_check(installed_go='go version go1.27.3 linux/amd64'), '1.27.3')

    def test_both_module_requirements(self):
        for module in ('api', 'controller'):
            with self.subTest(module=module):
                mod = self.root / module / 'go.mod'
                original = mod.read_text()
                mod.write_text(original.replace('go1.27.3', 'go1.27.4'))
                with self.assertRaisesRegex(ValueError, 'older than ' + module):
                    self.run_check()
                mod.write_text(original)

    def test_go_only_modules(self):
        for module in ('api', 'controller'):
            for required in ('1.27.3', '1.27'):
                with self.subTest(module=module, required=required):
                    mod = self.root / module / 'go.mod'
                    original = mod.read_text()
                    mod.write_text(f'module example.test/{module}\ngo {required}\n')
                    self.assertEqual(self.run_check(), '1.27.3')
                    mod.write_text(original)

    def test_go_directive_cannot_exceed_worker(self):
        for module in ('api', 'controller'):
            for toolchain in ('', 'toolchain go1.27.3\n'):
                with self.subTest(module=module, toolchain=toolchain):
                    mod = self.root / module / 'go.mod'
                    original = mod.read_text()
                    mod.write_text(f'module example.test/{module}\ngo 1.27.4\n{toolchain}')
                    with self.assertRaisesRegex(ValueError, f'older than {module}/go.mod requirement go1.27.4'):
                        self.run_check()
                    mod.write_text(original)

    def test_module_directive_cardinality(self):
        mod = self.root / 'api/go.mod'
        for directives in ('toolchain go1.27.3\n', 'go 1.27.3\ngo 1.27.3\n',
                           'go 1.27.3\ntoolchain go1.27.3\ntoolchain go1.27.3\n'):
            with self.subTest(directives=directives):
                mod.write_text('module example.test/api\n' + directives)
                with self.assertRaisesRegex(ValueError, 'exactly one go and at most one toolchain'):
                    self.run_check()

    def test_orphaned_and_missing_keys(self):
        for packages in (['go', 'jq.bin'], [self.key], [self.key, 'jq.bin', 'new-package']):
            with self.subTest(packages=packages):
                self.packages = packages
                with self.assertRaisesRegex(ValueError, 'key mismatch'):
                    self.run_check()

    def test_baked_version_drift(self):
        for output in ('go version go1.26.8 linux/amd64', 'go version go1.27.4 linux/amd64', 'go version devel linux/amd64'):
            with self.subTest(output=output), self.assertRaisesRegex(ValueError, 'does not match lock'):
                self.run_check(installed_go=output)

    def test_arm64_metadata_and_binary_architecture(self):
        systems = self.lock['packages'][self.key]['systems']
        systems['aarch64-linux'] = {'outputs': [{'path': '/nix/store/' + 'b' * 32 + '-go-1.27.3', 'default': True}]}
        self.assertEqual(self.run_check(installed_go='go version go1.27.3 linux/arm64', arch='arm64'), '1.27.3')
        with self.assertRaisesRegex(ValueError, 'TARGETARCH'):
            self.run_check(installed_go='go version go1.27.3 linux/amd64', arch='arm64')
        systems['aarch64-linux']['outputs'][0]['path'] = '/nix/store/' + 'b' * 32 + '-go-1.27.2'
        with self.assertRaisesRegex(ValueError, 'architecture versions disagree'):
            self.run_check()

    def test_explicit_nix_attribute(self):
        old = self.key
        self.key = 'github:NixOS/nixpkgs/' + 'b' * 40 + '#go_1_27'
        self.packages[0] = self.key
        self.lock['packages'][self.key] = self.lock['packages'].pop(old)
        del self.lock['packages'][self.key]['version']
        self.assertEqual(self.run_check(), '1.27.3')

    def test_relock_metadata_requires_the_identical_immutable_pin(self):
        old_key = self.key
        self.key = 'github:NixOS/nixpkgs/' + 'b' * 40 + '#go_1_27'
        self.packages[0] = self.key
        entry = self.lock['packages'].pop(old_key)
        entry['resolved'] = self.key
        self.lock['packages'][self.key] = entry
        self.write()
        previous = self.root / 'before.lock'
        previous.write_text(json.dumps(self.lock))
        self.lock['packages'][self.key] = {'resolved': self.key}
        self.write()
        CHECK.restore_go_metadata(self.root, previous)
        self.assertEqual(CHECK.check(self.root), '1.27.3')
        self.lock['packages'][self.key]['resolved'] = 'different pin'
        self.write()
        with self.assertRaisesRegex(ValueError, 'resolution changed'):
            CHECK.restore_go_metadata(self.root, previous)

    def test_runtime_without_modules_release_requires_them(self):
        (self.root / 'api/go.mod').unlink()
        self.assertEqual(self.run_check(skip_modules=True), '1.27.3')
        with self.assertRaises(FileNotFoundError):
            self.run_check()

    def test_inconsistent_lock(self):
        self.lock['packages'][self.key]['version'] = '1.27.2'
        with self.assertRaisesRegex(ValueError, 'metadata and default output'):
            self.run_check()

    def test_stdenv_input_is_metadata_not_a_package(self):
        key = 'github:NixOS/nixpkgs/nixpkgs-unstable'
        self.lock['packages'][key] = {'resolved': 'github:NixOS/nixpkgs/' + 'c' * 40 + '?lastModified=123'}
        self.assertEqual(self.run_check(), '1.27.3')
        self.lock['packages'][key]['source'] = 'nixpkg'
        with self.assertRaisesRegex(ValueError, 'invalid Devbox stdenv'):
            self.run_check()

    def test_missing_or_ambiguous_output(self):
        outputs = self.lock['packages'][self.key]['systems']['x86_64-linux']['outputs']
        for replacement in ([], outputs + copy.deepcopy(outputs)):
            self.lock['packages'][self.key]['systems']['x86_64-linux']['outputs'] = replacement
            with self.assertRaisesRegex(ValueError, 'one default'):
                self.run_check()

    def test_json_comments_do_not_strip_urls(self):
        self.assertEqual(CHECK.manifest_json('{"url":"https://example.test/*x*/", /* comment */ "packages": ["go",],}'),
                         {'url': 'https://example.test/*x*/', 'packages': ['go']})

    def test_installed_flake_name_coverage(self):
        project = pathlib.Path(__file__).resolve().parent.parent
        source = (project / 'agent/devbox-global/assert-toolchain.sh').read_text()
        self.assertEqual(source.count('# --- 2.'), 1)
        script = self.root / 'coverage.sh'
        script.write_text(source.split('# --- 2.')[0])
        binary = self.root / 'bin'
        binary.mkdir()
        fake = binary / 'devbox'
        fake.write_text('#!/bin/sh\nprintf "%s\\n" "* github:NixOS/nixpkgs/' + 'b' * 40 + '#go_1_27" "* jq.bin@1.8.1"\n')
        fake.chmod(0o755)
        guard = self.root / 'guard.tsv'
        guard.write_text('github:NixOS/nixpkgs/' + 'b' * 40 + '#go_1_27\t-\t-\njq.bin\t-\t-\n')
        result = subprocess.run(['bash', str(script), str(guard)], env={**os.environ, 'PATH': str(binary) + os.pathsep + os.environ['PATH']}, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)

    def test_refresh_workflow_rejects_manifest_rewrites(self):
        project = pathlib.Path(__file__).resolve().parent.parent
        workflow = json.loads(subprocess.check_output(['yq', '-o=json', str(project / '.github/workflows/devbox-update.yml')]))
        steps = [s for s in workflow['jobs']['validate']['steps'] if s.get('name') == 'devbox update (full re-resolve to the latest nixpkgs)']
        self.assertEqual(len(steps), 1)
        old_key = self.key
        self.key = 'github:NixOS/nixpkgs/' + 'b' * 40 + '#go_1_27'
        self.packages[0] = self.key
        self.lock['packages'][self.key] = self.lock['packages'].pop(old_key)
        self.lock['packages'][self.key]['resolved'] = self.key
        self.write()
        directory = self.root / 'agent/devbox-global'
        script = self.root / 'relock.sh'
        script.write_text(steps[0]['run'])
        (self.root / 'scripts').mkdir()
        shutil.copy(project / 'scripts/check-worker-toolchain.py', self.root / 'scripts/check-worker-toolchain.py')
        after = copy.deepcopy(self.lock)
        after['packages'][self.key] = {'resolved': self.key}
        snapshot = self.root / 'after.lock'
        snapshot.write_text(json.dumps(after))
        binary = self.root / 'bin'
        binary.mkdir()
        fake = binary / 'devbox'
        fake.write_text('#!/bin/sh\ncp "$POST_LOCK" devbox.lock\nif [ "$REWRITE" = 1 ]; then printf "\\n// rewritten\\n" >> devbox.json; fi\n')
        fake.chmod(0o755)
        env = {**os.environ, 'GIT_CONFIG_GLOBAL': '/dev/null', 'GIT_CONFIG_SYSTEM': '/dev/null',
               'PATH': str(binary) + os.pathsep + os.environ['PATH'], 'RUNNER_TEMP': str(self.root), 'POST_LOCK': str(snapshot)}
        subprocess.run(['git', 'init', '-q', str(self.root)], check=True, env=env)
        subprocess.run(['git', '-C', str(self.root), 'add', 'agent'], check=True, env=env)
        subprocess.run(['git', '-C', str(self.root), '-c', 'user.name=test', '-c', 'user.email=test@example.test', '-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture'], check=True, env=env)
        for rewrite in ('0', '1'):
            with self.subTest(rewrite=rewrite):
                self.write()
                result = subprocess.run(['bash', str(script)], cwd=directory, env={**env, 'REWRITE': rewrite}, capture_output=True, text=True)
                if rewrite == '0':
                    self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
                    self.assertEqual(CHECK.check(self.root), '1.27.3')
                else:
                    self.assertNotEqual(result.returncode, 0, 'rewritten manifest must never publish only its new lock')

    def test_gate_and_offline_image_proof_are_wired(self):
        project = pathlib.Path(__file__).resolve().parent.parent
        taskfile = json.loads(subprocess.check_output(['yq', '-o=json', str(project / 'Taskfile.yml')]))
        self.assertIn({'task': 'check:worker-toolchain'}, taskfile['tasks']['gate:repo']['cmds'])
        workflow = json.loads(subprocess.check_output(['yq', '-o=json', str(project / '.github/workflows/ci.yml')]))
        job = workflow['jobs']['build-agent']
        filters = next(s['with']['filters'] for s in workflow['jobs']['changes']['steps'] if s.get('id') == 'filter')
        self.assertIn("'scripts/sqlc.sh'", filters)
        self.assertIn("'scripts/check-worker-toolchain.py'", filters)
        self.assertEqual(set(job['strategy']['matrix']['template']), {'base', 'jvm'})
        self.assertFalse(any(s.get('with', {}).get('load') is True for s in job['steps']))
        for template in ('base', 'jvm'):
            dockerfile = (project / 'agent/templates' / template / 'Dockerfile').read_text()
            runtime, release = dockerfile.split('FROM runtime AS release')
            self.assertIn('--skip-modules', runtime)
            self.assertIn('/opt/uzi-src/scripts/check-worker-toolchain.py', release)
            self.assertNotIn('--skip-modules', release)
            self.assertIn('ENV UZI_SQLC_DIR=/usr/local/share/uzi-sqlc', runtime)
            self.assertIn('USER runner\nRUN --network=none', release)
            proof = release.split('USER runner\nRUN --network=none', 1)[1]
            self.assertIn('scripts/sqlc.sh "$SQLC_PIN" version', proof)
            self.assertIn('cd api; go version; cd ../controller; go version\nUSER root', proof)


if __name__ == '__main__':
    unittest.main()
