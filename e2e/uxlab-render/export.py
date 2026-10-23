#!/usr/bin/env python3
"""Fixture-only Docker tar decoder. Never delegates framing/extraction to tarfile."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tarfile

MIB = 1024 * 1024
PNG = re.compile(r"[a-zA-Z0-9_.-]+-(light|dark)\.png", re.ASCII)
RECEIPT = re.compile(rb"/data/runner/uxlab-[a-f0-9-]+/api/cmd/uzi/uxlab/proof")
FILE_MAX = 16 * MIB
PAYLOAD_MAX = 512 * MIB
META_MAX = 16 * MIB
STREAM_MAX = 528 * MIB
HEADER_MAX = 8192


def require(ok, message):
    if not ok:
        raise ValueError(message)


class Reader:
    def __init__(self, stream, cap):
        self.stream, self.cap, self.used = stream, cap, 0

    def read(self, count):
        require(0 <= count <= 65536, "read chunk limit")
        # One overflow byte distinguishes exact EOF from a padding flood.
        count = min(count, self.cap - self.used + 1)
        require(count > 0, "stream byte limit")
        data = self.stream.read(count)
        self.used += len(data)
        require(self.used <= self.cap, "stream byte limit")
        return data

    def exact(self, count):
        require(0 <= count <= 65536, "read chunk limit")
        out = bytearray()
        while len(out) < count:
            data = self.read(count - len(out))
            require(data, "truncated tar")
            out.extend(data)
        return bytes(out)


def pax_fields(data):
    fields, offset, records = {}, 0, 0
    while offset < len(data):
        records += 1
        require(records <= 16, "PAX record count")
        space = data.find(b" ", offset, min(len(data), offset + 7))
        require(space > offset, "PAX length framing")
        digits = data[offset:space]
        require(digits.isdigit() and digits[:1] != b"0", "PAX length framing")
        length = int(digits)
        end = offset + length
        require(space + 1 < end <= len(data) and data[end - 1:end] == b"\n", "PAX length framing")
        record = data[space + 1:end - 1]
        key, sep, value = record.partition(b"=")
        require(sep and key in (b"path", b"mtime", b"atime", b"ctime"), "PAX key")
        require(key not in fields and len(value) <= 1024 and b"\0" not in value, "PAX value/duplicate")
        fields[key] = value.decode("ascii")
        if key != b"path":
            require(re.fullmatch(r"-?[0-9]+(?:\.[0-9]+)?", fields[key]), "PAX timestamp")
        offset = end
    require(fields, "empty PAX")
    return fields


def safe_path(name):
    require(name.isascii() and len(name) <= 255, "non-ASCII/long path")
    if name == "./":
        return "."
    if name.startswith("./"):
        name = name[2:]
    if name.endswith("/"):
        name = name[:-1]
    require(name == "." or (name and all(
        re.fullmatch(r"[a-zA-Z0-9_.-]+", part) and part not in (".", "..")
        for part in name.split("/"))), "unsafe path")
    return name


def expected_from_log(log, cell):
    with open(log, "rb") as source:
        raw = source.read(8 * MIB + 1)
    require(len(raw) <= 8 * MIB, "log byte limit")
    expected = {}
    phases = {"local"} if cell == "local" else {"full-build", "offline-render"}
    for line in raw.splitlines():
        if not line.startswith(b"{"):
            continue
        record = json.loads(line)
        require(isinstance(record, dict), "log record object")
        if record.get("kind") != "artifacts":
            continue
        phase = record.get("phase")
        require(phase in phases and phase not in expected, "unexpected/duplicate phase receipt")
        pngs = record.get("pngs")
        require(isinstance(pngs, list) and 0 < len(pngs) <= 2048, "PNG receipt count")
        files = {}
        for png in pngs:
            require(isinstance(png, dict), "PNG receipt object")
            name, size, digest = png.get("name"), png.get("bytes"), png.get("sha256")
            require(isinstance(name, str) and PNG.fullmatch(name) and name not in files, "PNG receipt name")
            require(type(size) is int and 0 < size <= FILE_MAX, "PNG receipt size")
            require(isinstance(digest, str) and re.fullmatch(r"[0-9a-f]{64}", digest), "PNG receipt hash")
            files[name] = (size, digest)
        for name in files:
            peer = re.sub(r"-(light|dark)\.png$", lambda m:
                          "-dark.png" if m[1] == "light" else "-light.png", name)
            require(peer in files, "missing theme peer")
        expected[phase] = files
    require(set(expected) == phases, "missing phase receipt")
    sets = [set(files) for files in expected.values()]
    require(all(names == sets[0] for names in sets), "phase sets differ")
    return expected


def decode(stream, expected=None, destination=None):
    receipt = expected is None
    reader = Reader(stream, 16384 if receipt else STREAM_MAX)
    headers = payload = metadata = 0
    pending = None
    seen, directories, files = set(), set(), set()
    receipt_bytes = None
    while True:
        block = reader.exact(512)
        metadata += 512
        require(metadata <= (16384 if receipt else META_MAX), "metadata byte limit")
        if block == bytes(512):
            require(pending is None, "dangling PAX")
            require(reader.exact(512) == bytes(512), "two zero terminators required")
            metadata += 512
            # Consume bounded zero padding to EOF; never accept a second archive.
            while True:
                tail = reader.read(65536)
                if not tail:
                    break
                metadata += len(tail)
                require(metadata <= (16384 if receipt else META_MAX), "metadata byte limit")
                require(not any(tail), "nonzero trailing data")
            break
        headers += 1
        require(headers <= (8 if receipt else HEADER_MAX), "header count limit")
        info = tarfile.TarInfo.frombuf(block, "ascii", "strict")
        require(info.size >= 0 and not info.linkname, "negative size/link")
        require(info.type in (tarfile.REGTYPE, tarfile.AREGTYPE, tarfile.DIRTYPE, tarfile.XHDTYPE),
                "unsupported tar type")
        if info.type == tarfile.XHDTYPE:
            require(pending is None and 0 < info.size <= 4096, "PAX size/repeated extension")
            # PAX header labels are metadata (Python uses ././@PaxHeader).
            # Only the effective next entry path is eligible for extraction.
            require(len(info.name) <= 255 and all(32 <= ord(c) < 127 for c in info.name),
                    "PAX header label")
            pending = pax_fields(reader.exact(info.size))
            padding = (-info.size) % 512
            require(not any(reader.exact(padding)), "nonzero padding")
            metadata += info.size + padding
            require(metadata <= (16384 if receipt else META_MAX), "metadata byte limit")
            continue
        effective = pending.get(b"path", info.name) if pending else info.name
        require(info.type == tarfile.DIRTYPE or not effective.endswith("/"), "file path trailing slash")
        name = safe_path(effective)
        pending = None
        require(name not in seen, "duplicate path")
        seen.add(name)
        is_dir = info.type == tarfile.DIRTYPE
        if receipt:
            require(name == "uxlab-artifacts" and not is_dir and info.size <= 255,
                    "receipt name/type/size")
        elif is_dir:
            require(info.size == 0 and (name == "." or name in expected), "unexpected directory/size")
            directories.add(name)
            if name != ".":
                os.mkdir(Path(destination) / name, 0o755)
            continue
        else:
            parts = name.split("/")
            require(len(parts) == 2 and parts[0] in expected and PNG.fullmatch(parts[1]),
                    "unexpected PNG path")
            phase, base = parts
            require(phase in directories and base in expected[phase], "unexpected file/missing directory")
            require(info.size == expected[phase][base][0], "receipt size mismatch")
        require(info.size <= (255 if receipt else FILE_MAX), "file byte limit")
        payload += info.size
        require(payload <= (255 if receipt else PAYLOAD_MAX), "payload byte limit")
        digest = hashlib.sha256()
        fd = None
        data = bytearray()
        try:
            if not receipt:
                fd = os.open(Path(destination) / name,
                             os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644)
            remaining = info.size
            while remaining:
                chunk = reader.exact(min(65536, remaining))
                digest.update(chunk)
                if receipt:
                    data.extend(chunk)
                else:
                    # File writes can be short; each iteration makes bounded progress.
                    view = memoryview(chunk)
                    while view:
                        written = os.write(fd, view)
                        require(written > 0, "short file write")
                        view = view[written:]
                remaining -= len(chunk)
        finally:
            if fd is not None:
                os.close(fd)
        padding = (-info.size) % 512
        require(not any(reader.exact(padding)), "nonzero padding")
        metadata += padding
        require(metadata <= (16384 if receipt else META_MAX), "metadata byte limit")
        if receipt:
            require(RECEIPT.fullmatch(data), "invalid receipt path")
            receipt_bytes = bytes(data)
        else:
            require(digest.hexdigest() == expected[phase][base][1], "PNG hash mismatch")
            files.add(name)
    if receipt:
        require(receipt_bytes is not None and len(seen) == 1, "missing receipt")
        return receipt_bytes.decode("ascii")
    require(files == {phase + "/" + name for phase, names in expected.items() for name in names},
            "incomplete export")
    require(directories - {"."} == set(expected), "incomplete phase directories")


def main():
    if len(sys.argv) == 2 and sys.argv[1] == "receipt":
        print(decode(sys.stdin.buffer))
        return
    require(len(sys.argv) == 5 and sys.argv[1] in ("candidate", "local"), "fixture export arguments")
    _, cell, log, dest, marker = sys.argv
    require(marker == "fresh-owned-dir", "fixture export arguments")
    target = Path(dest)
    stat = target.lstat()
    require(target.is_dir() and not target.is_symlink() and stat.st_uid == os.getuid()
            and not any(target.iterdir()), "fresh owned directory required")
    try:
        decode(sys.stdin.buffer, expected_from_log(log, cell), dest)
        target.chmod(0o755)
    except BaseException:
        shutil.rmtree(target)
        raise


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, tarfile.TarError, UnicodeError, KeyError, TypeError) as error:
        print("export rejected: " + str(error), file=sys.stderr)
        sys.exit(1)
