"""Focused, hermetic fixture export tests; no Docker or dependency installation."""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
spec = importlib.util.spec_from_file_location("fixture_export", HERE / "export.py")
export = importlib.util.module_from_spec(spec)
spec.loader.exec_module(export)
SCRATCH = ROOT / ".uzi/scratch"
DATA = b"PNG fixture bytes"
NAMES = ("scene-dark.png", "scene-light.png")
PATH = b"/data/runner/uxlab-123abc/api/cmd/uzi/uxlab/proof"


def member(name, data=b"", kind=tarfile.REGTYPE, size=None):
    info = tarfile.TarInfo(name)
    info.type, info.size = kind, len(data) if size is None else size
    return info.tobuf(format=tarfile.USTAR_FORMAT) + data + bytes((-len(data)) % 512)


def archive(*members):
    return b"".join(members) + bytes(1024)


def pax_record(key, value):
    body = (key + "=" + value + "\n").encode("ascii")
    length = len(body) + 2
    while len(str(length)) + 1 + len(body) != length:
        length = len(str(length)) + 1 + len(body)
    return str(length).encode() + b" " + body


def expected(phases=("local",)):
    return {phase: {name: (len(DATA), hashlib.sha256(DATA).hexdigest()) for name in NAMES}
            for phase in phases}


def good_members(phases=("local",)):
    return [part for phase in phases for part in
            [member(phase + "/", kind=tarfile.DIRTYPE),
             *[member(phase + "/" + name, DATA) for name in NAMES]]]


class ExportTests(unittest.TestCase):
    def setUp(self):
        SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="uxlab-export-test-", dir=SCRATCH)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dest = self.root / "png"
        self.dest.mkdir()

    def decode(self, data, want=None):
        return export.decode(io.BytesIO(data), expected() if want is None else want, self.dest)

    def rejects(self, data, pattern, want=None):
        with self.assertRaisesRegex((ValueError, tarfile.TarError, UnicodeError), pattern):
            self.decode(data, want)

    def log(self, cell="local"):
        path = self.root / "log"
        phases = ("local",) if cell == "local" else ("full-build", "offline-render")
        path.write_text("\n".join(json.dumps({"kind": "artifacts", "phase": phase,
                         "pngs": [{"name": name, "bytes": size, "sha256": digest}
                                  for name, (size, digest) in files.items()]})
                                  for phase, files in expected(phases).items()))
        return path

    def test_receipt_normal_docker_compatible_ustar_and_pax(self):
        self.assertEqual(export.decode(io.BytesIO(archive(member("uxlab-artifacts", PATH)))), PATH.decode())
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w", format=tarfile.PAX_FORMAT) as tar:
            info = tarfile.TarInfo("uxlab-artifacts")
            info.size, info.mtime = len(PATH), 1.25
            tar.addfile(info, io.BytesIO(PATH))
        self.assertEqual(export.decode(io.BytesIO(buffer.getvalue())), PATH.decode())

    def test_receipt_oversize_rejected_before_payload_read_or_file_write(self):
        class HeaderOnly(io.BytesIO):
            def read(self, count):
                if self.tell() >= 512:
                    raise AssertionError("payload read before size validation")
                return super().read(count)
        with patch.object(export.os, "open", side_effect=AssertionError("file created")):
            with self.assertRaisesRegex(ValueError, "receipt name/type/size"):
                export.decode(HeaderOnly(member("uxlab-artifacts", size=256)[:512]))

    def test_receipt_rejects_extra_type_invalid_content_and_empty(self):
        for data in [archive(), archive(member("uxlab-artifacts", PATH), member("extra", b"x")),
                     archive(member("uxlab-artifacts", b"/etc/passwd")),
                     archive(member("uxlab-artifacts", kind=tarfile.DIRTYPE))]:
            with self.subTest(length=len(data)), self.assertRaises(ValueError):
                export.decode(io.BytesIO(data))

    def test_normal_docker_compatible_tar_and_candidate_phases(self):
        phases = ("full-build", "offline-render")
        self.decode(archive(member("./", kind=tarfile.DIRTYPE), *good_members(phases)), expected(phases))
        for phase in phases:
            self.assertEqual((self.dest / phase / NAMES[0]).read_bytes(), DATA)
            self.assertEqual((self.dest / phase).stat().st_mode & 0o777, 0o755)

    def test_bounded_reads_and_short_chunks(self):
        class ShortReader(io.BytesIO):
            def read(self, count):
                self.assert_count(count)
                return super().read(min(count, 17))
            def assert_count(self, count):
                if not 0 < count <= 65536:
                    raise AssertionError("unbounded read")
        export.decode(ShortReader(archive(*good_members())), expected(), self.dest)

    def test_small_pax_path_and_timestamps(self):
        fields = b"".join(pax_record(key, value) for key, value in
                          [("path", "local/scene-dark.png"), ("mtime", "1.25"),
                           ("atime", "0"), ("ctime", "-1.5")])
        data = archive(member("local", kind=tarfile.DIRTYPE),
                       member("PaxHeaders/file", fields, tarfile.XHDTYPE),
                       member("placeholder", DATA), member("local/scene-light.png", DATA))
        self.decode(data)
        self.assertEqual((self.dest / "local/scene-dark.png").read_bytes(), DATA)

    def test_pax_rejects_forbidden_duplicate_malformed_and_dangling(self):
        fields = [pax_record("size", "1"), pax_record("GNU.sparse.size", "1"),
                  pax_record("linkpath", "file"), pax_record("path", "/escape"),
                  pax_record("path", "../escape"),
                  pax_record("path", "local/a") * 2, b"99 path=a\n", b"0 path=a\n",
                  b"12 path=a\n", b"10 path=aX", b"", b"x" * 4097,
                  b"".join(pax_record("mtime", "1") for _ in range(17))]
        for field in fields:
            with self.subTest(field=field[:60]):
                self.rejects(archive(member("PaxHeaders/file", field, tarfile.XHDTYPE),
                                     member("placeholder", DATA)), "PAX|unsafe|unexpected")
        ext = member("PaxHeaders/file", pax_record("mtime", "1"), tarfile.XHDTYPE)
        self.rejects(archive(ext, ext), "repeated")
        self.rejects(archive(ext), "dangling")

    def test_links_special_sparse_global_gnu_headers_rejected(self):
        for kind in [tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.CHRTYPE, tarfile.BLKTYPE,
                     tarfile.FIFOTYPE, tarfile.GNUTYPE_SPARSE, tarfile.XGLTYPE,
                     tarfile.GNUTYPE_LONGNAME, tarfile.GNUTYPE_LONGLINK]:
            with self.subTest(kind=kind):
                self.rejects(archive(member("local/scene-dark.png", kind=kind)), "unsupported")
        info = tarfile.TarInfo("local/scene-dark.png")
        info.linkname = "target"
        self.rejects(archive(info.tobuf()), "link")

    def test_absolute_traversal_nested_extra_unicode_omitted_names(self):
        for name in ["/absolute", "../escape", "local/../escape", "local/sub/file.png",
                     "other/scene-dark.png", "local/unexpected.png", ".git", ".uzi",
                     "node_modules", "local//scene-dark.png", "local/scene-dark.png/"]:
            with self.subTest(name=name):
                self.rejects(archive(member(name, DATA)), "unsafe|unexpected|trailing slash")
        info = tarfile.TarInfo("local/café-dark.png")
        self.rejects(archive(info.tobuf(format=tarfile.USTAR_FORMAT, encoding="utf-8")), "ascii")

    def test_duplicate_file_and_directory_collisions(self):
        directory = member("local/", kind=tarfile.DIRTYPE)
        self.rejects(archive(directory, directory), "duplicate")
        # New destination for each case because decoder intentionally never reuses dirs.
        os.rmdir(self.dest / "local")
        self.rejects(archive(directory, member("local", DATA)), "duplicate")
        os.rmdir(self.dest / "local")
        self.rejects(archive(*good_members(), member("local/scene-dark.png", DATA)), "duplicate")

    def test_directory_size_and_file_size_before_payload(self):
        self.rejects(archive(member("local/", kind=tarfile.DIRTYPE, size=1)), "directory/size")
        want = expected()
        want["local"]["scene-dark.png"] = (export.FILE_MAX + 1, "0" * 64)
        with patch.object(export.os, "open", side_effect=AssertionError("file opened before size rejection")):
            self.rejects(archive(member("local", kind=tarfile.DIRTYPE),
                                 member("local/scene-dark.png", size=export.FILE_MAX + 1)),
                         "file byte limit", want)

    def test_missing_file_phase_and_hash_mismatch(self):
        self.rejects(archive(*good_members()[:2]), "incomplete")
        self.assertFalse((self.dest / "local/scene-light.png").exists())
        other = self.root / "other"
        other.mkdir()
        with self.assertRaisesRegex(ValueError, "incomplete"):
            export.decode(io.BytesIO(archive(*good_members())), expected(("local", "other")), other)
        bad = self.root / "bad"
        bad.mkdir()
        data = archive(member("local", kind=tarfile.DIRTYPE),
                       member("local/scene-dark.png", b"x" * len(DATA)))
        with self.assertRaisesRegex(ValueError, "hash mismatch"):
            export.decode(io.BytesIO(data), expected(), bad)

    def test_header_payload_metadata_and_stream_caps(self):
        with patch.object(export, "HEADER_MAX", 1):
            self.rejects(archive(*good_members()), "header count")
        os.rmdir(self.dest / "local")
        with patch.object(export, "PAYLOAD_MAX", len(DATA) - 1):
            self.rejects(archive(*good_members()), "payload byte")
        os.rmdir(self.dest / "local")
        with patch.object(export, "META_MAX", 511):
            self.rejects(archive(*good_members()), "metadata byte")
        with patch.object(export, "STREAM_MAX", 511):
            self.rejects(archive(*good_members()), "stream byte")

    def test_truncation_checksums_padding_and_concatenation(self):
        full = archive(*good_members())
        for index, data in enumerate([full[:100], full[:512 + 510], full[:-1], full[:-512],
                                      full + archive(member("extra")),
                                      full[:512] + b"\xff" * 512]):
            dest = self.root / str(index)
            dest.mkdir()
            with self.subTest(index=index), self.assertRaises((ValueError, tarfile.TarError, UnicodeError)):
                export.decode(io.BytesIO(data), expected(), dest)
        broken = bytearray(member("uxlab-artifacts", PATH))
        broken[0] ^= 1
        with self.assertRaises(tarfile.TarError):
            export.decode(io.BytesIO(archive(broken)))
        with self.assertRaisesRegex(ValueError, "two zero"):
            export.decode(io.BytesIO(member("uxlab-artifacts", PATH) + bytes(512) + b"x" * 512))
        bad_pad = bytearray(member("uxlab-artifacts", PATH))
        bad_pad[512 + len(PATH)] = 1
        with self.assertRaisesRegex(ValueError, "padding"):
            export.decode(io.BytesIO(archive(bad_pad)))

    def test_zero_padding_flood_and_nonzero_tail(self):
        with self.assertRaisesRegex(ValueError, "stream byte"):
            export.decode(io.BytesIO(archive(member("uxlab-artifacts", PATH)) + bytes(16384)))
        with patch.object(export, "META_MAX", 2048):
            self.rejects(archive(*good_members()) + bytes(4096), "metadata byte")
        dest = self.root / "tail"
        dest.mkdir()
        with self.assertRaisesRegex(ValueError, "nonzero trailing"):
            export.decode(io.BytesIO(archive(*good_members()) + b"x"), expected(), dest)

    def test_log_requires_complete_phases_themes_and_unique_names(self):
        log = self.log("candidate")
        self.assertEqual(export.expected_from_log(log, "candidate"), expected(("full-build", "offline-render")))
        records = [json.loads(line) for line in log.read_text().splitlines()]
        variations = [records[:1], records + records[:1],
                      [{**records[0], "pngs": records[0]["pngs"][:1]}, records[1]],
                      [{**records[0], "pngs": records[0]["pngs"] * 2}, records[1]],
                      [records[0], {**records[1], "pngs": [
                          {**png, "name": png["name"].replace("scene-", "other-")}
                          for png in records[1]["pngs"]]}]]
        for records in variations:
            log.write_text("\n".join(map(json.dumps, records)))
            with self.subTest(records=len(records)), self.assertRaises(ValueError):
                export.expected_from_log(log, "candidate")

    def test_cli_decoder_failure_cleans_partial_directory(self):
        result = subprocess.run([sys.executable, str(HERE / "export.py"), "local",
                                 str(self.log()), str(self.dest), "fresh-owned-dir"],
                                input=archive(*good_members()[:2]), capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.dest.exists())
        self.assertNotIn(b"PNG_EXPORT", result.stdout)

    def test_cli_success_chmod_and_pipeline_producer_failure_cleanup(self):
        tar = self.root / "input.tar"
        tar.write_bytes(archive(*good_members()))
        log = self.log()
        # Literal inert producer emits a valid archive then fails. Same pipefail/
        # conditional cleanup contract as run.sh; no candidate payload is executed.
        script = """
set -euo pipefail
if ! { cat -- "$1"; exit 7; } | "$2" "$3" local "$4" "$5" fresh-owned-dir; then
  rm -rf -- "$5"
  exit 1
fi
echo PNG_EXPORT
"""
        result = subprocess.run(["bash", "-c", script, "export-test", str(tar),
                                 sys.executable, str(HERE / "export.py"), str(log), str(self.dest)],
                                capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.dest.exists())
        self.assertNotIn(b"PNG_EXPORT", result.stdout)
        self.dest.mkdir(mode=0o700)
        result = subprocess.run([sys.executable, str(HERE / "export.py"), "local",
                                 str(log), str(self.dest), "fresh-owned-dir"],
                                input=tar.read_bytes(), capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.dest.stat().st_mode & 0o777, 0o755)


if __name__ == "__main__":
    unittest.main()
