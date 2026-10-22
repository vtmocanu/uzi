#!/usr/bin/env python3
"""A colliding backup must never reuse or overwrite an existing snapshot."""
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest


class BackupDirectoryTests(unittest.TestCase):
    def test_false_success_mkdir_cannot_reuse_a_snapshot(self):
        script = pathlib.Path(__file__).with_name('backup-runs.sh').resolve()
        with tempfile.TemporaryDirectory(prefix='backup-collision-') as directory:
            root = pathlib.Path(directory).resolve()
            backups = root / 'backups'
            stamp = '20990101T000000Z'
            existing = backups / stamp
            existing.mkdir(parents=True, mode=0o700)
            original = existing / 'issue-1.run.json'
            original.write_text('preserved original snapshot\n')
            original.chmod(0o600)
            binary = root / 'bin'
            binary.mkdir()
            mkdir, date = shutil.which('mkdir'), shutil.which('date')
            self.assertIsNotNone(mkdir)
            self.assertIsNotNone(date)
            programs = {
                'mkdir': '''#!/bin/sh
if [ "$#" = 1 ] && [ "$1" = "$COLLIDING_DIR" ]; then exit 0; fi
exec "$REAL_MKDIR" "$@"
''',
                'date': '''#!/bin/sh
if [ "$*" = '-u +%Y%m%dT%H%M%SZ' ]; then printf '%s\n' "$FIXED_STAMP"; exit 0; fi
exec "$REAL_DATE" "$@"
''',
                'uzi': '''#!/bin/sh
case "$1 $2" in
  'run get') printf '%s\n' '{"id":"queued-1","kind":"issue","status":"queued","issue_iid":1,"worker_id":null}' ;;
  'run logs') exit 0 ;;
  *) exit 99 ;;
esac
''',
                'kubectl': '#!/bin/sh\nexit 99\n',
            }
            for name, content in programs.items():
                path = binary / name
                path.write_text(content)
                path.chmod(0o755)
            env = {'PATH': str(binary) + os.pathsep + os.environ['PATH'], 'HOME': str(root),
                   'TMPDIR': str(root), 'LC_ALL': 'C', 'UZI_BACKUP_DIR': str(backups),
                   'UZI_BACKUP_RETENTION_DAYS': '0', 'UZI_CTX': 'test-context',
                   'UZI_WORKER_NS': 'test-namespace', 'UZI_REPO_SLUG': 'testrepo',
                   'UZI_BIN': str(binary / 'uzi'), 'UZI_KUBECTL': str(binary / 'kubectl'),
                   'REAL_MKDIR': mkdir, 'REAL_DATE': date, 'FIXED_STAMP': stamp,
                   'COLLIDING_DIR': str(existing)}
            result = subprocess.run(['bash', str(script), 'queued-1'], cwd=root, env=env,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(original.read_text(), 'preserved original snapshot\n',
                             'colliding backup overwrote the earlier snapshot')
            selected = (backups / 'latest-attempt').resolve(strict=True)
            self.assertNotEqual(selected, existing.resolve())
            self.assertRegex(selected.name, '^' + stamp + r'\.[A-Za-z0-9]+$')
            self.assertEqual(selected.stat().st_mode & 0o777, 0o700)
            self.assertEqual(json.loads((selected / 'issue-1.run.json').read_text())['id'], 'queued-1')


if __name__ == '__main__':
    unittest.main()
