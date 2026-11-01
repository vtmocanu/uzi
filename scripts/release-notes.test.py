#!/usr/bin/env python3
"""Offline Release presentation regressions, including the actual workflow steps."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class Formatting(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location('notes', ROOT / 'scripts/release-notes.py')
        cls.notes = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.notes)

    def render(self, text, hotfix=False, limit=600):
        result = self.notes.render(text.encode('utf-8'), 'fixture/repo', 'v1.2.3', hotfix, limit=limit)
        decoded = result.decode('utf-8')
        self.assertLessEqual(len(decoded), limit)
        return decoded

    def test_small_notes_are_byte_identical_including_crlf_and_unicode(self):
        raw = '### Fixed\r\n\r\n- Café 漢 😀.\r\n  Detail.\r\n'.encode('utf-8')
        self.assertEqual(self.notes.render(raw, 'fixture/repo', 'v1.2.3'), raw)

    def test_exact_default_budget_and_character_not_byte_count(self):
        for raw in [b'- ' + b'x' * 119997 + b'\n', ('- ' + '😀' * 119997 + '\n').encode('utf-8')]:
            self.assertEqual(len(raw.decode('utf-8')), 120000)
            self.assertEqual(self.notes.render(raw, 'fixture/repo', 'v1.2.3'), raw)

    def test_only_complete_multiline_bullets_and_their_headings_are_selected(self):
        first = '### Fixed\n\n- First.\n  First wrapped line.\n  Another line.\n'
        second = '\n- Second.\n  Second wrapped line.\n'
        huge = '\n### Added\n\n- Oversized.\n  ' + '漢' * 1000 + '\n'
        body = self.render(first + second + huge)
        self.assertIn(first + second, body)
        self.assertNotIn('Oversized', body)
        self.assertNotIn('### Added', body)
        self.assertIn('truncated', body)
        self.assertTrue(body.endswith('[Full release notes in CHANGELOG.md](https://github.com/fixture/repo/blob/v1.2.3/CHANGELOG.md)\n'))

    def test_oversized_first_bullet_uses_notice_and_link_without_partial_bullet(self):
        body = self.render('### Fixed\n\n- ' + 'x' * 1000 + '\n')
        self.assertIn('truncated', body)
        self.assertIn('CHANGELOG.md', body)
        self.assertNotIn('- ', body)
        self.assertNotIn('### Fixed', body)

    def test_blank_separated_continuation_is_not_cut_from_its_bullet(self):
        body = self.render('### Fixed\n\n- Short first line.\n\n  ' + 'x' * 1000 + '\n')
        self.assertNotIn('Short first line.', body)
        self.assertIn('truncated', body)

    def test_security_section_beyond_prefix_is_kept_whole_when_it_fits(self):
        security = '### Security\n\n- Security correction.\n  Complete wrapped detail.\n'
        body = self.render('### Added\n\n- ' + 'x' * 1000 + '\n\n' + security)
        self.assertIn(security.rstrip('\n'), body)
        self.assertNotIn('### Added', body)

    def test_oversized_security_section_keeps_marker_and_link(self):
        body = self.render('### Security\n\n- ' + 'x' * 1000 + '\n')
        self.assertIn('### Security\n', body)
        self.assertIn('See the complete notes linked below.', body)
        self.assertNotIn('- ', body)

    def test_security_prefix_semantics_and_no_invented_heading(self):
        for heading in ['### Security fixes', '  ### Security', '\u2003### Security']:
            with self.subTest(heading=heading):
                body = self.render('- ' + 'x' * 1000 + '\n\n' + heading + '\n- Correction.\n')
                self.assertTrue(any(line.strip().startswith('### Security') for line in body.split('\n')))
        body = self.render('### Fixed\n- ' + 'x' * 1000 + '\n')
        self.assertFalse(any(line.strip().startswith('### Security') for line in body.split('\n')))
        body = self.render('- No heading here.\u2028### Security\n  ' + 'x' * 1000 + '\n')
        self.assertNotIn('### Security', body)
        body = self.render('\x1c### Security\n- ' + 'x' * 1000 + '\n')
        self.assertNotIn('### Security', body)

    def test_warning_is_byte_identical_when_small_and_reserved_when_truncated(self):
        raw = b'### Fixed\n\n- Correction.\n'
        self.assertEqual(self.notes.render(raw, 'fixture/repo', 'v1.2.3', True),
                         raw + self.notes.HOTFIX_WARNING.encode('utf-8'))
        body = self.render('### Fixed\n\n- ' + 'x' * 1000 + '\n', hotfix=True)
        self.assertIn(self.notes.HOTFIX_WARNING, body)
        self.assertIn('truncated', body)
        self.assertIn('CHANGELOG.md', body)

    def test_warning_alone_can_push_notes_over_the_budget(self):
        raw = '- ' + 'x' * 550 + '\n'
        self.assertEqual(self.render(raw), raw)
        body = self.render(raw, hotfix=True)
        self.assertIn('truncated', body)
        self.assertIn(self.notes.HOTFIX_WARNING, body)

    def test_final_overhead_and_exact_small_budget_boundary(self):
        raw = '### Fixed\n\n- Whole bullet.\n'
        overhead = self.notes.NOTICE + '\n\n[Full release notes in CHANGELOG.md](https://github.com/fixture/repo/blob/v1.2.3/CHANGELOG.md)\n'
        # Force truncation with a later oversized bullet; the first fits exactly.
        limit = len(overhead + raw)
        body = self.render(raw + '- ' + 'x' * 1000 + '\n', limit=limit)
        self.assertEqual(len(body), limit)
        self.assertIn(raw, body)
        with self.assertRaises(ValueError):
            self.render('x' * 1000, limit=1)

    def test_all_metadata_is_reserved_before_a_regular_bullet(self):
        security = '\n\n### Security\n\n- Complete security note.\n'
        fallback = self.render('- ' + 'x' * 2000 + '\n' + security, hotfix=True, limit=900)
        capacity = 900 - len(fallback)
        first = '- ' + 'y' * (capacity - 2) + '\n'  # One character beyond the available space.
        body = self.render(first + '\n- ' + 'x' * 2000 + '\n' + security, hotfix=True, limit=900)
        self.assertNotIn('y' * (capacity - 2), body)
        self.assertIn('Complete security note.', body)
        self.assertIn(self.notes.HOTFIX_WARNING, body)

    def test_many_whole_bullets_consume_one_cumulative_budget(self):
        first = '### Fixed\n\n- ' + 'x' * 250 + '\n'
        second = '\n- ' + 'y' * 250 + '\n'
        body = self.render(first + second + '\n- ' + 'z' * 1000 + '\n')
        self.assertIn(first, body)
        self.assertNotIn('y' * 250, body)


class Workflow(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        program = 'puts YAML.safe_load(File.read(ARGV[0]), aliases: true).fetch("jobs").fetch("publish-release").fetch("steps").to_json'
        steps = json.loads(subprocess.check_output(['ruby', '-ryaml', '-rjson', '-e', program,
                           str(ROOT / '.github/workflows/release.yml')], text=True, timeout=30))
        cls.build = next(step['run'] for step in steps if step.get('name') == 'Build release notes from the CHANGELOG section')
        cls.publish = next(step['run'] for step in steps if step.get('name') == 'Create or update the GitHub Release')

    def fixture(self, directory, notes, tag='v1.2.3', hotfix=False):
        root = Path(directory)
        (root / 'scripts/lib').mkdir(parents=True)
        for name in ['changelog-section.sh', 'lib/changelog-bullets.awk', 'release-notes.py']:
            source = ROOT / 'scripts' / name
            if source.exists():
                shutil.copyfile(source, root / 'scripts' / name)
        (root / 'CHANGELOG.md').write_bytes(b'# Changelog\n\n## [1.2.3] - 2026-11-01\n' + notes)
        env = dict(os.environ, GIT_CONFIG_GLOBAL='/dev/null', GIT_CONFIG_SYSTEM='/dev/null',
                   GITHUB_REF_NAME=tag, GITHUB_REPOSITORY='fixture/repo',
                   UZI_CHANGELOG_FILE='CHANGELOG.md',
                   GITHUB_OUTPUT=str(root / 'output'), HOTFIX_UNVERIFIED='true' if hotfix else 'false')
        return root, env

    def build_notes(self, root, env):
        subprocess.run(['bash', '-e', '-c', self.build], cwd=root, env=env, check=True,
                       capture_output=True, timeout=30)
        return (root / 'release-notes.md').read_bytes()

    def test_oversized_publish_step_is_bounded(self):
        for tag in ['v1.2.3', 'v1.2.3-rc.1']:
            with self.subTest(tag=tag), tempfile.TemporaryDirectory() as directory:
                notes = b'### Fixed\n\n- **Huge.**\n  ' + b'x' * 130000 + b'\n'
                root, env = self.fixture(directory, notes, tag=tag)
                rendered = self.build_notes(root, env)
                self.assertLessEqual(len(rendered.decode('utf-8')), 120000)
                self.assertIn(b'truncated', rendered)
                self.assertIn(('https://github.com/fixture/repo/blob/' + tag + '/CHANGELOG.md').encode(), rendered)

    def test_actual_notes_step_reserves_security_and_hotfix_warning(self):
        with tempfile.TemporaryDirectory() as directory:
            security = b'### Security\n\n- Full security correction.\n  Its detail.\n'
            root, env = self.fixture(directory, b'### Added\n\n- ' + b'x' * 130000 + b'\n\n' + security, hotfix=True)
            rendered = self.build_notes(root, env)
            self.assertLessEqual(len(rendered.decode('utf-8')), 120000)
            self.assertIn(security.rstrip(), rendered)
            self.assertIn(b'**UNVERIFIED hotfix:', rendered)

    def test_small_hotfix_keeps_existing_warning_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            notes = b'### Fixed\n\n- Correction.\n  Detail.\n'
            root, env = self.fixture(directory, notes, hotfix=True)
            warning = (b'\n**UNVERIFIED hotfix: this off-main release has no full CI evidence. '
                       b'Tagged metadata, worker-image checks and real KinD smoke passed.**\n')
            self.assertEqual(self.build_notes(root, env), notes + warning)

    def test_actual_rc_delta_selection_and_small_output_are_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            first = b'### Fixed\n\n- Old correction.\n  Old detail.\n'
            root, env = self.fixture(directory, first, tag='v1.2.3-rc.2')
            def git(*args):
                return subprocess.check_output(['git', '-c', 'core.fsmonitor=false', *args], cwd=root, env=env,
                                               stderr=subprocess.STDOUT, timeout=30)
            git('init', '-q')
            git('config', 'user.name', 'Fixture')
            git('config', 'user.email', 'fixture@example.invalid')
            git('add', 'CHANGELOG.md')
            git('commit', '-qm', 'first candidate')
            git('tag', 'v1.2.3-rc.1')
            (root / 'CHANGELOG.md').write_bytes(b'# Changelog\n\n## [1.2.3] - 2026-11-01\n' + first + b'\n- New correction.\n  New detail.\n')
            rendered = self.build_notes(root, env)
            self.assertEqual(rendered, b'### Fixed\n\n- New correction.\n  New detail.\n')
            (root / 'CHANGELOG.md').write_bytes(b'# Changelog\n\n## [1.2.3] - 2026-11-01\n' + first + b'\n- Huge new correction.\n  ' + b'x' * 130000 + b'\n')
            rendered = self.build_notes(root, env)
            self.assertLessEqual(len(rendered.decode('utf-8')), 120000)
            self.assertIn(b'truncated', rendered)
            self.assertNotIn(b'Old correction', rendered)

    def test_create_and_edit_send_the_same_bounded_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root, env = self.fixture(directory, b'### Fixed\n\n- ' + b'x' * 130000 + b'\n')
            rendered = self.build_notes(root, env)
            (root / 'bin').mkdir()
            fake = root / 'bin/gh'
            fake.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
if sys.argv[1:3] == ['release', 'view']:
    sys.exit(int(os.environ['RELEASE_EXISTS']))
assert sys.argv[1:3] in (['release', 'create'], ['release', 'edit'])
args = sys.argv[1:]
pathlib.Path('sent-body').write_bytes(pathlib.Path(args[args.index('--notes-file') + 1]).read_bytes())
pathlib.Path('sent-args').write_text(json.dumps(args))
''')
            fake.chmod(0o755)
            for exists, verb in [('0', 'edit'), ('1', 'create')]:
                current = dict(env, PATH=str(root / 'bin') + os.pathsep + env['PATH'],
                               RELEASE_EXISTS=exists, TAG='v1.2.3', TITLE='v1.2.3')
                subprocess.run(['bash', '-e', '-c', self.publish], cwd=root, env=current, check=True,
                               capture_output=True, timeout=30)
                self.assertEqual(json.loads((root / 'sent-args').read_text())[:2], ['release', verb])
                self.assertEqual((root / 'sent-body').read_bytes(), rendered)


if __name__ == '__main__':
    unittest.main()
