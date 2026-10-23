#!/usr/bin/env python3
"""Check the committed worker package keys and Go version without Nix or network."""
import argparse
import copy
import json
import pathlib
import re
import sys


def manifest_json(text):
    # HuJSON comments/trailing commas, preserving quoted URLs and escaped quotes.
    tokens = r'"(?:[^"\\]|\\.)*"|//[^\n]*|/\*.*?\*/'
    text = re.sub(tokens, lambda m: ' ' if m[0].startswith('/') else m[0], text, flags=re.S)
    return json.loads(re.sub(r'"(?:[^"\\]|\\.)*"|,\s*(?=[}\]])',
                             lambda m: '' if m[0].startswith(',') else m[0], text))


def version(text):
    if not isinstance(text, str) or not re.fullmatch(r'\d+\.\d+\.\d+', text):
        raise ValueError(f'expected a released Go X.Y.Z version, got {text!r}')
    return tuple(map(int, text.split('.')))


def restore_go_metadata(root, previous):
    """Devbox's full relock omits flake output metadata; reuse only the identical pin."""
    path = root / 'agent/devbox-global/devbox.lock'
    old = json.loads(previous.read_text())['packages']
    lock = json.loads(path.read_text())
    pins = [key for key in old if re.fullmatch(r'github:NixOS/nixpkgs/[0-9a-f]{40}#go_\d+_\d+', key)]
    if len(pins) != 1:
        raise ValueError('expected one immutable prior Go flake pin')
    key = pins[0]
    entry = lock['packages'].get(key, {})
    if entry.get('resolved') != old[key].get('resolved'):
        raise ValueError('Go flake resolution changed; re-evaluate its version and outputs with Nix')
    for field in ('version', 'systems'):
        entry[field] = copy.deepcopy(old[key][field])
    path.write_text(json.dumps(lock, indent=2) + '\n')


def check(root, skip_modules=False, installed_go=None, arch=None):
    config = manifest_json((root / 'agent/devbox-global/devbox.json').read_text())
    lock = json.loads((root / 'agent/devbox-global/devbox.lock').read_text())
    packages = config['packages']
    if not isinstance(packages, list) or not all(isinstance(p, str) for p in packages):
        raise ValueError('worker packages must be a list of package keys')
    if len(set(packages)) != len(packages):
        raise ValueError('duplicate worker package keys')
    locked = lock['packages']
    # Devbox stores its stdenv flake-input pin in this same map. It is metadata,
    # not an installable package; no other extra lock key receives this exception.
    input_key = 'github:NixOS/nixpkgs/nixpkgs-unstable'
    package_keys = set(locked)
    if input_key in locked:
        entry = locked[input_key]
        if set(entry) - {'last_modified', 'resolved'} or not re.fullmatch(
                r'github:NixOS/nixpkgs/[0-9a-f]{40}(?:\?[^#\s]+)?', entry.get('resolved', '')):
            raise ValueError('invalid Devbox stdenv input pin')
        package_keys.remove(input_key)
    missing, orphaned = set(packages) - package_keys, package_keys - set(packages)
    if missing or orphaned:
        raise ValueError(f'package/lock key mismatch: missing={sorted(missing)}, orphaned={sorted(orphaned)}')
    go_keys = [p for p in packages if re.fullmatch(r'go(?:@[^ ]+)?|(?:github:[^ ]+#)?go_\d+_\d+', p)]
    if len(go_keys) != 1:
        raise ValueError(f'expected one explicit Go package, got {go_keys}')
    entry = locked[go_keys[0]]
    def output_version(system):
        outputs = entry['systems'][system]['outputs']
        defaults = [o['path'] for o in outputs if o.get('default') is True and o.get('name', 'out') == 'out']
        if len(defaults) != 1:
            raise ValueError('Go lock must have one default Linux out output')
        match = re.fullmatch(r'/nix/store/[a-z0-9]{32}-go-(\d+\.\d+\.\d+)', defaults[0])
        if not match:
            raise ValueError('cannot read the resolved Go version from the lock output')
        return match[1]
    resolved = output_version('x86_64-linux')
    if 'aarch64-linux' in entry['systems'] and output_version('aarch64-linux') != resolved:
        raise ValueError('Go lock Linux architecture versions disagree')
    if 'version' in entry and version(entry['version']) != version(resolved):
        raise ValueError('Go lock metadata and default output versions disagree')
    if not skip_modules:
        for module in ('api', 'controller'):
            text = (root / module / 'go.mod').read_text()
            go_lines = re.findall(r'^[ \t]*go[ \t]+([^\s]+)[ \t]*(?://[^\n]*)?$', text, re.M)
            toolchain_lines = re.findall(r'^[ \t]*toolchain[ \t]+([^\s]+)[ \t]*(?://[^\n]*)?$', text, re.M)
            if len(go_lines) != 1 or len(toolchain_lines) > 1:
                raise ValueError(f'{module}/go.mod must declare exactly one go and at most one toolchain directive')
            requirements = go_lines + [t.removeprefix('go') for t in toolchain_lines]
            # A language-only go directive such as 1.27 requires that release family.
            requirements = [r + '.0' if re.fullmatch(r'\d+\.\d+', r) else r for r in requirements]
            required = max(requirements, key=version)
            if version(resolved) < version(required):
                raise ValueError(f'worker Go {resolved} is older than {module}/go.mod requirement go{required}')
    if installed_go is not None:
        actual = re.fullmatch(r'go version go(\d+\.\d+\.\d+) linux/(amd64|arm64)', installed_go)
        if not actual or actual[1] != resolved:
            raise ValueError(f'baked Go does not match lock {resolved}: {installed_go!r}')
        if arch is not None and actual[2] != arch:
            raise ValueError('baked Go architecture does not match TARGETARCH')
        system = {'amd64': 'x86_64-linux', 'arm64': 'aarch64-linux'}[actual[2]]
        if output_version(system) != actual[1]:
            raise ValueError('baked Go does not match its architecture output')
    elif arch is not None:
        raise ValueError('--arch requires --installed-go')
    return resolved


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=pathlib.Path, default=pathlib.Path(__file__).resolve().parent.parent)
    parser.add_argument('--skip-modules', action='store_true', help='runtime image stage only; release stage checks modules')
    parser.add_argument('--installed-go', help='exact output of the baked go version')
    parser.add_argument('--arch', choices=('amd64', 'arm64'), help='image TARGETARCH to check against the binary')
    parser.add_argument('--restore-go-metadata', type=pathlib.Path, help='refresh only: previous committed lock for the identical immutable Go pin')
    args = parser.parse_args()
    try:
        if args.restore_go_metadata:
            restore_go_metadata(args.root, args.restore_go_metadata)
        resolved = check(args.root, args.skip_modules, args.installed_go, args.arch)
    except (ValueError, KeyError, TypeError, OSError) as error:
        print(f'worker-toolchain: {error}', file=sys.stderr)
        return 1
    print(f'worker-toolchain: package keys synchronized; Go {resolved}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
