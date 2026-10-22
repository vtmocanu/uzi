#!/usr/bin/env python3
"""Exercise the workflow's real refresh step with controlled Devbox outcomes."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

os.environ.update(GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
PROJECT = Path(__file__).resolve().parent.parent
SPEC = importlib.util.spec_from_file_location('toolchain_fixtures', PROJECT / 'scripts/check-worker-toolchain.test.py')
FIXTURES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURES)


class RefreshTests(unittest.TestCase):
    write = FIXTURES.ToolchainTests.write

    def setUp(self):
        FIXTURES.ToolchainTests.setUp(self)
        original_key = self.key
        self.key = 'github:NixOS/nixpkgs/' + 'a' * 40 + '#go_1_27'
        self.packages[0] = self.key
        self.lock['packages'][self.key] = self.lock['packages'].pop(original_key)
        self.lock['packages'][self.key]['resolved'] = self.key
        self.packages.append('gcc')
        for key in ('jq.bin', 'gcc'):
            self.lock['packages'][key] = {'source': 'nixpkg', 'resolved': 'github:NixOS/nixpkgs/' + 'a' * 40 + '#' + key}
        self.write()
        scripts = self.root / 'scripts'
        scripts.mkdir()
        for name in ('check-worker-toolchain.py', 'refresh-worker-lock.py'):
            shutil.copyfile(PROJECT / 'scripts' / name, scripts / name)
        workflow = json.loads(subprocess.check_output(['yq', '-o=json', str(PROJECT / '.github/workflows/devbox-update.yml')]))
        step = next(step for step in workflow['jobs']['validate']['steps'] if step['name'].startswith('devbox update ('))
        self.command = step['run']
        self.cwd = self.root / step.get('working-directory', '.')
        self.before_manifest = (self.root / 'agent/devbox-global/devbox.json').read_bytes()
        self.before_lock = (self.root / 'agent/devbox-global/devbox.lock').read_bytes()
        (self.root / 'fake-lock.json').write_bytes(self.before_lock)
        binary = self.root / 'bin'
        binary.mkdir()
        fake = binary / 'devbox'
        fake.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
with open(os.environ['DEVBOX_TRACE'], 'a') as trace:
    trace.write(json.dumps(args) + '\\n')
config = pathlib.Path(args[args.index('--config') + 1]) if '--config' in args else pathlib.Path.cwd()
manifest, lock = config / 'devbox.json', config / 'devbox.lock'
mode = os.environ['REFRESH_CASE']
if mode == 'rewrite':
    manifest.write_text(manifest.read_text() + '\\n// migrated to search\\n')
    data = json.loads(pathlib.Path(os.environ['FAKE_LOCK']).read_text())
    data['packages']['jq.bin'] = {'resolved': 'github:NixOS/nixpkgs/' + 'b' * 40 + '#jq.bin',
        'systems': {'x86_64-linux': {'outputs': [{'path': '/nix/store/' + 'c' * 32 + '-jq-1.8.2-bin', 'default': True}]}}}
    lock.write_text(json.dumps(data))
    sys.exit(0)
if args[0] == 'update' and 'nixpkgs' in args:
    data = json.loads(lock.read_text())
    data['packages']['github:NixOS/nixpkgs/nixpkgs-unstable'] = {'resolved': 'github:NixOS/nixpkgs/' + 'b' * 40}
    lock.write_text(json.dumps(data))
elif args[0] in ('install', 'update'):
    data = json.loads(lock.read_text())
    if args[0] == 'update':
        assert '--no-install' in args
        assert data['packages'][args[1]]['source'] == 'nixpkg', 'raw anchor must survive'
        assert any(key not in data['packages'] for key in ('jq.bin', 'gcc')), 'other raw entries must be invalidated'
    assert os.environ['PIN_KEY'] in data['packages'], 'explicit pin must survive'
    for key, suffix in (('jq.bin', 'c' * 32 + '-jq-1.8.2-bin'), ('gcc', 'f' * 32 + '-gcc-16.2.0')):
        if key not in data['packages']:
            data['packages'][key] = {'source': 'nixpkg', 'resolved': 'github:NixOS/nixpkgs/' + 'b' * 40 + '#' + key,
                'systems': {'x86_64-linux': {'outputs': [{'path': '/nix/store/' + suffix, 'default': True}]}}}
    if mode == 'go-change':
        data['packages'][os.environ['PIN_KEY']]['resolved'] += '?changed=true'
    if mode == 'orphan' and args[0] == 'install':
        data['packages']['jq.bin@latest'] = {}
    if mode == 'output-change' and args[0] == 'install':
        data['packages']['jq.bin']['systems']['x86_64-linux']['outputs'][0]['path'] = '/nix/store/' + 'd' * 32 + '-jq-1.8.2-bin'
    if mode == 'input-change' and args[0] == 'install':
        (pathlib.Path(os.environ['SOURCE_ROOT']) / 'api/go.mod').write_text('module changed\\ngo 1.99.0\\n')
    if mode == 'partial' and args[0] == 'install':
        del data['packages']['jq.bin']
    lock.write_text(json.dumps(data))
    if mode == 'partial' and args[0] == 'install':
        sys.exit(7)
else:
    lock.write_text('{"packages": {}}')
    sys.exit(7)
''')
        fake.chmod(0o755)
        nix = binary / 'nix'
        nix.write_text('''#!/usr/bin/env python3
import os, sys
args = sys.argv[1:]
if args[0] == 'eval':
    print('x86_64-linux')
elif args[0] == 'path-info':
    assert '--recursive' in args
    assert args[args.index('--store') + 1] == 'https://cache.nixos.org'
    assert '/nix/store/' + 'a' * 32 + '-go-1.27.3' in args
    assert '/nix/store/' + 'c' * 32 + '-jq-1.8.2-bin' in args
    assert '/nix/store/' + 'f' * 32 + '-gcc-16.2.0' in args
    if os.environ['REFRESH_CASE'] == 'cache-miss':
        sys.exit(1)
else:
    sys.exit(2)
''')
        nix.chmod(0o755)
        sleep = binary / 'sleep'
        sleep.write_text('#!/bin/sh\nexit 0\n')
        sleep.chmod(0o755)
        self.env = {**os.environ, 'PATH': str(binary) + os.pathsep + os.environ['PATH'],
                    'RUNNER_TEMP': str(self.root), 'PIN_KEY': self.key,
                    'FAKE_LOCK': str(self.root / 'fake-lock.json'), 'SOURCE_ROOT': str(self.root),
                    'DEVBOX_TRACE': str(self.root / 'devbox-trace.jsonl')}
        subprocess.run(['git', 'init', '-q', str(self.root)], check=True)
        subprocess.run(['git', '-C', str(self.root), 'add', 'agent'], check=True)

    def invoke(self, mode):
        return subprocess.run(['bash', '-euo', 'pipefail', '-c', self.command], cwd=self.cwd,
                              env={**self.env, 'REFRESH_CASE': mode}, capture_output=True, text=True)

    def assert_originals(self):
        self.assertEqual((self.root / 'agent/devbox-global/devbox.json').read_bytes(), self.before_manifest)
        self.assertEqual((self.root / 'agent/devbox-global/devbox.lock').read_bytes(), self.before_lock)

    def test_refresh_success(self):
        result = self.invoke('success')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.root / 'agent/devbox-global/devbox.json').read_bytes(), self.before_manifest)
        lock = json.loads((self.root / 'agent/devbox-global/devbox.lock').read_text())
        self.assertEqual(lock['packages'][self.key], self.lock['packages'][self.key])
        self.assertIn('b' * 40, lock['packages']['jq.bin']['resolved'])

    def test_refresh_manifest_rewrite_cannot_publish_either_input(self):
        result = self.invoke('rewrite')
        self.assertNotEqual(result.returncode, 0)
        self.assert_originals()

    def test_refresh_partial_install_cannot_publish_lock(self):
        result = self.invoke('partial')
        self.assertNotEqual(result.returncode, 0)
        self.assert_originals()

    def test_refresh_orphan_lock_cannot_publish(self):
        result = self.invoke('orphan')
        self.assertNotEqual(result.returncode, 0)
        self.assert_originals()

    def test_refresh_cannot_publish_against_changed_module_inputs(self):
        result = self.invoke('input-change')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / 'agent/devbox-global/devbox.lock').read_bytes(), self.before_lock)
        self.assertIn('go 1.99.0', (self.root / 'api/go.mod').read_text())

    def test_locally_realized_but_uncached_output_cannot_publish(self):
        result = self.invoke('cache-miss')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not yet cached', result.stderr)
        self.assert_originals()

    def test_uncached_output_is_refused_before_install(self):
        result = self.invoke('cache-miss')
        self.assertNotEqual(result.returncode, 0)
        calls = [json.loads(line) for line in (self.root / 'devbox-trace.jsonl').read_text().splitlines()]
        self.assertFalse(any(call[0] == 'install' for call in calls), calls)
        self.assert_originals()

    def test_install_cannot_change_cache_verified_outputs(self):
        result = self.invoke('output-change')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('outputs changed', result.stderr)
        self.assert_originals()

    def test_metadata_refresh_cannot_change_the_immutable_go_pin(self):
        result = self.invoke('go-change')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('pin metadata changed', result.stderr)
        self.assert_originals()


if __name__ == '__main__':
    unittest.main()
