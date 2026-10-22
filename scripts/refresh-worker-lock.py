#!/usr/bin/env python3
"""Refresh raw Devbox attributes in isolation, publishing only a coherent lock."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time

SPEC = importlib.util.spec_from_file_location('worker_toolchain', Path(__file__).with_name('check-worker-toolchain.py'))
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


def require_public_cache(config, packages):
    """A local realization alone cannot authorize a source-building worker image."""
    system = subprocess.check_output(['nix', 'eval', '--impure', '--raw', '--expr',
                                      'builtins.currentSystem'], text=True, timeout=60).strip()
    lock = json.loads((config / 'devbox.lock').read_text())
    paths = set()
    for key in packages:
        outputs = lock['packages'][key].get('systems', {}).get(system, {}).get('outputs', [])
        selected = [output.get('path', '') for output in outputs if output.get('default') is True]
        if not selected or any(not re.fullmatch(r'/nix/store/[a-z0-9]{32}-[^/\s]+', path) for path in selected):
            raise ValueError(f'missing resolved worker outputs for {key} on {system}; no lock published')
        paths.update(selected)
    started = time.monotonic()
    try:
        # Query the REMOTE store, even if install already built a path locally.
        # Recursive verification also refuses a missing dependency in its closure.
        subprocess.run(['nix', 'path-info', '--recursive', '--store', 'https://cache.nixos.org',
                        *sorted(paths)], check=True, stdout=subprocess.DEVNULL, timeout=300)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise ValueError('worker outputs not yet cached or public cache unavailable; '
                         'retry next week; no lock published') from error
    print(f'worker public-cache coverage: {len(paths)} output roots verified in {time.monotonic() - started:.1f}s')


def refresh(root, devbox):
    directory = root / 'agent/devbox-global'
    manifest_path, lock_path = directory / 'devbox.json', directory / 'devbox.lock'
    original_manifest, original_lock = manifest_path.read_bytes(), lock_path.read_bytes()
    modules = {root / module / 'go.mod': (root / module / 'go.mod').read_bytes()
               for module in ('api', 'controller')}
    CHECK.check(root)
    packages = CHECK.manifest_json(original_manifest.decode())['packages']
    # Raw attributes use Devbox's one stdenv input. Explicit package/flake pins stay
    # intact, including Go's evaluated Linux outputs and version metadata.
    raw = [key for key in packages if re.fullmatch(r'[A-Za-z_][A-Za-z0-9_.+-]*', key)]
    if not raw:
        raise ValueError('worker manifest has no raw attributes to refresh')
    with tempfile.TemporaryDirectory(prefix='worker-lock-refresh-') as temporary:
        stage = Path(temporary)
        config = stage / 'agent/devbox-global'
        config.mkdir(parents=True)
        (config / 'devbox.json').write_bytes(original_manifest)
        (config / 'devbox.lock').write_bytes(original_lock)
        for module in ('api', 'controller'):
            (stage / module).mkdir()
            (stage / module / 'go.mod').write_bytes(modules[root / module / 'go.mod'])

        def run(*arguments):
            subprocess.run([devbox, *arguments, '--config', str(config)], check=True)
            if (config / 'devbox.json').read_bytes() != original_manifest:
                raise ValueError('Devbox rewrote the worker manifest; no lock published')

        # Pinned Devbox supports this metadata-only operation without migrating raw
        # selectors to devbox-search @latest packages. A normal full update does not.
        run('update', 'nixpkgs', '--no-install')
        lock = json.loads((config / 'devbox.lock').read_text())
        for key in raw:
            del lock['packages'][key]
        (config / 'devbox.lock').write_text(json.dumps(lock, indent=2) + '\n')
        # Resolve missing raw attributes against the refreshed stdenv through Devbox's
        # supported install path, then populate its store-path metadata.
        run('install', '--tidy-lockfile')
        CHECK.check(stage)
        require_public_cache(config, packages)
        if (manifest_path.read_bytes() != original_manifest or lock_path.read_bytes() != original_lock
                or any(path.read_bytes() != contents for path, contents in modules.items())):
            raise ValueError('worker inputs changed during refresh; no lock published')
        with tempfile.NamedTemporaryFile(dir=directory, prefix='.devbox-lock-', delete=False) as output:
            pending = Path(output.name)
            try:
                output.write((config / 'devbox.lock').read_bytes())
                output.flush()
                os.fsync(output.fileno())
            except BaseException:
                pending.unlink()
                raise
        try:
            os.chmod(pending, lock_path.stat().st_mode & 0o777)
            os.replace(pending, lock_path)
        finally:
            pending.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument('--devbox', default='devbox')
    args = parser.parse_args()
    refresh(args.root.resolve(), args.devbox)


if __name__ == '__main__':
    main()
