#!/usr/bin/env python3
"""Focused metadata-consumer fixtures mirroring pin-restore-journal.test.ts."""
import base64
import copy
import io
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest import mock
import tempfile

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("descriptor", HERE / "pin-restore-descriptor.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def dump(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def epoch(g):
    return f"00000000-0000-4000-8000-{g:012}"


def attempt(g):
    return f"20261101T120000Z-g{g}-0123456789abcdef"


def identity(g):
    return dict(runId="run-1", clonePath="/runner/repo/issue-1.attempt-" + attempt(g),
                attemptId=attempt(g), sourceGeneration=g)


def entry(raw, g):
    return dict(journalDigest=hashlib.sha256(raw.encode()).hexdigest(),
                rawJournal=raw, generation=g, sourceEpoch=epoch(g))


def link(raw, g):
    return dict(version=1, intentId=f"intent-{g}",
                predecessorJournalDigest=entry(raw, g)["journalDigest"],
                predecessorGeneration=g, predecessorEpoch=epoch(g),
                successor=dict(runId="run-1", claimGeneration=g + 1, workerId="worker.example",
                               registerNonce="register:nonce", authorityIncarnation=epoch(10),
                               executorIncarnation="exec-123", sourceEpoch=epoch(g + 1),
                               attemptId=attempt(g + 1), clonePath=identity(g + 1)["clonePath"]),
                restore=dict(captureId="capture-1", authenticatedRecordDigest="a" * 64,
                             predecessorGeneration=g, bareDir="host+repo.git",
                             pinRef=f"refs/uzi-recovery-pin/run-1/{g}", tip="b" * 40,
                             contentSource="e" * 40, expectedHead="c" * 40, expectedTree="d" * 40))


def envelope():
    raw = dump(identity(1))
    return dict(version=2, runId="run-1", branch="uzi/issue-1", key="issue-1",
                predecessors=[entry(raw, 1)],
                intent=dict(version=1, phase="prepared", link=link(raw, 1)))


def next_raw(e):
    return dump(dict(identity(e["intent"]["link"]["successor"]["claimGeneration"]),
                     pinRestore=e["intent"]["link"]))


def append(e, raw=None):
    g = e["intent"]["link"]["successor"]["claimGeneration"]
    raw = next_raw(e) if raw is None else raw
    e["predecessors"].append(entry(raw, g))
    e["intent"]["link"] = link(raw, g)


def replace(e, raw):
    e["predecessors"][0] = entry(raw, 1)
    e["intent"]["link"] = link(raw, 1)


def ledger_row(j, branch="uzi/issue-1", state="reclaimed"):
    row = {k: j[k] for k in ("runId", "clonePath", "attemptId")}
    row["state"] = state
    return f"uzi-attempts.{branch}.entry {dump(row)}"


def ledger(e, active=None):
    journals = [json.loads(x["rawJournal"]) for x in e["predecessors"]]
    if active is not None:
        journals.append(json.loads(active))
    rows = {}
    for j in journals:
        sources = [j] + j.get("retainedSources", []) + list(
            v for k, v in j.get("recovery", {}).items() if k in ("source", "successor"))
        for s in sources:
            if "attemptId" in s:
                is_new_active = active is not None and active != e["predecessors"][-1]["rawJournal"] and j is journals[-1]
                rows[s["attemptId"]] = ledger_row(s, state="live" if is_new_active else "reclaimed")
    return "\n".join(rows.values())


def decoder(budget=None):
    return module.Decoder("run-1", "uzi/issue-1", "issue-1", "/runner/repo", "host+repo.git", budget=budget)


class DescriptorTests(unittest.TestCase):
    def decode(self, e, active=None, rows=None, budget=None):
        return decoder(budget).decode(dump(e), active, ledger(e, active) if rows is None else rows)

    def refused(self, e, active=None, rows=None):
        with self.assertRaises((ValueError, KeyError, TypeError, UnicodeError)):
            self.decode(e, active, rows)

    def test_prepared_ready_absent_and_exact_old_bytes(self):
        e = envelope()
        old = e["predecessors"][0]["rawJournal"]
        self.assertEqual(self.decode(e, old), [(attempt(1), identity(1)["clonePath"])])
        self.assertEqual(self.decode(e, next_raw(e))[0][0], attempt(2))
        self.assertEqual(len(self.decode(e)), 1)  # absent never loses original
        self.refused(e, " " + old)
        self.refused(e, "")
        e["intent"]["phase"] = "ready"
        self.decode(e, next_raw(e))
        self.decode(e)
        self.refused(e, old)
        self.refused(e, dump(identity(9)))
        active = json.loads(next_raw(e))
        active["pinRestore"]["restore"]["expectedTree"] = "f" * 40
        self.refused(e, dump(active))
        active = json.loads(next_raw(e))
        active["pinRestore"] = dict(reversed(list(active["pinRestore"].items())))
        self.decode(e, dump(active))

    def test_exact_raw_fence_and_memorypark_controls(self):
        e = envelope()
        j = identity(1)
        binding = dict(runId="run-1", claimGeneration=1, memoryEpisode=0,
                       workerId="worker\tµ", registerNonce="nonce")
        j["memoryPark"] = dict(version=1, binding=binding, ack="sent_unconfirmed",
                               origin=dict(flightId=epoch(10), attemptId=attempt(1),
                                           executorIncarnation=None, authorityIncarnation=epoch(11),
                                           sourceEpoch=epoch(1)))
        j["completionSourceFence"] = dict(version=1, sourceUseEpoch=epoch(1), sourceCoreDigest="a" * 64)
        raw = "  " + dump(j) + " \n\n"
        replace(e, raw)
        self.decode(e, raw)
        self.refused(e, raw.rstrip())
        e["predecessors"][0]["journalDigest"] = hashlib.sha256(raw.rstrip().encode()).hexdigest()
        self.refused(e, raw)
        for field in ("completionSourceFence", "memoryPark"):
            bad = copy.deepcopy(j)
            bad[field]["extra"] = True
            replace(e, dump(bad))
            self.refused(e)
        bad = copy.deepcopy(j)
        bad["completionSourceFence"]["sourceUseEpoch"] += "\n"
        replace(e, dump(bad))
        self.refused(e)

    def test_worker_memorypark_settlement_exact_shapes(self):
        e = envelope()
        j = identity(1)
        binding = dict(runId="run-1", claimGeneration=1, memoryEpisode=0,
                       workerId="worker\tµ", registerNonce="nonce")
        source = dict(path="/source/a", authorityKey="a" * 64, attemptId=None, epoch=epoch(1), seq=1)
        evidence = dict(parkAck=binding, interventionIds=["intervention"], processJoin="executor",
                        commands="confirmed", steering="joined", auxiliaries="none", feedback="drained",
                        sinkReceipts="joined", parkReap="complete")
        j["memoryPark"] = dict(version=1, binding=binding, ack="confirmed",
                               origin=dict(flightId=epoch(10), attemptId=attempt(1),
                                           executorIncarnation=None, authorityIncarnation=epoch(11),
                                           sourceEpoch=epoch(1)),
                               settlement=dict(version=1, confirmed=True, sources=[source], evidence=evidence))
        replace(e, dump(j))
        self.decode(e)
        for path, value in ((["ack"], "not_sent"), (["binding", "claimGeneration"], True),
                            (["binding", "workerId"], "😀" * 65),
                            (["settlement", "confirmed"], 1),
                            (["settlement", "sources"], [source, source]),
                            (["settlement", "sources", 0, "path"], "/source/../other"),
                            (["settlement", "evidence", "interventionIds"], ["same", "same"]),
                            (["settlement", "evidence", "parkReap"], "unknown")):
            bad = copy.deepcopy(j)
            obj = bad["memoryPark"]
            for k in path[:-1]:
                obj = obj[k]
            obj[path[-1]] = value
            replace(e, dump(bad))
            self.refused(e)
        for path in ([], ["binding"], ["origin"], ["settlement"], ["settlement", "evidence"],
                     ["settlement", "sources", 0]):
            bad = copy.deepcopy(j)
            obj = bad["memoryPark"]
            for k in path:
                obj = obj[k]
            obj["unknown"] = True
            replace(e, dump(bad))
            self.refused(e)

    def test_duplicate_aliases_all_layers_and_depth_utf8(self):
        e = envelope()
        raw = dump(e)
        for key in ("version", "phase", "workerId", "captureId", "generation"):
            # Decoded escaped aliases must fail even when values agree.
            needle = '"' + key + '":'
            index = raw.index(needle)
            tail = raw[index + len(needle):]
            val, _ = json.JSONDecoder().raw_decode(tail)
            duplicate = '"' + "\\u%04x" % ord(key[0]) + key[1:] + '":' + dump(val) + ','
            bad = raw[:index] + duplicate + raw[index:]
            with self.subTest(key=key), self.assertRaises(ValueError):
                decoder().decode(bad, None, ledger(e))
        nested = e["predecessors"][0]["rawJournal"].replace('"runId":', '"\\u0072unId":"run-1","runId":')
        replace(e, nested)
        self.refused(e)
        for raw in ('{"x":NaN}', '{"x":Infinity}', "[" * 33 + "0" + "]" * 33,
                    '"\ud800"', '{"x":1,}', '{} {}'):
            with self.assertRaises((ValueError, UnicodeError)):
                module.parse(raw, 65536)
        self.assertEqual(module.parse("[" * 32 + "0" + "]" * 32, 100),
                         json.loads("[" * 32 + "0" + "]" * 32))
        with self.assertRaises(UnicodeError):
            b'"\xff"'.decode("utf-8")

    def test_exact_shapes_and_safe_link_fields(self):
        locations = [(), ("intent",), ("intent", "link"), ("intent", "link", "successor"),
                     ("intent", "link", "restore"), ("predecessors", 0)]
        for location in locations:
            for mode in ("extra", "missing"):
                e = envelope()
                obj = e
                for k in location:
                    obj = obj[k]
                if mode == "extra":
                    obj["extra"] = 1
                else:
                    del obj[next(iter(obj))]
                with self.subTest(location=location, mode=mode):
                    self.refused(e)
        cases = [
            ("successor", "runId", "other"), ("successor", "claimGeneration", 1),
            ("successor", "claimGeneration", True), ("successor", "workerId", "x" * 129),
            ("successor", "workerId", "µ" * 65), ("successor", "registerNonce", "x" * 257),
            ("successor", "registerNonce", "bad\u202e"), ("successor", "workerId", "bad\n"),
            ("successor", "authorityIncarnation", "bad"), ("successor", "sourceEpoch", "bad"),
            ("successor", "executorIncarnation", "bad/exec"), ("successor", "attemptId", attempt(3)),
            ("successor", "clonePath", "/runner/repo/../other"),
            ("restore", "pinRef", "refs/uzi-recovery-pin/run-1/2"),
            ("restore", "bareDir", ".."), ("restore", "tip", "b" * 40 + "\n"),
            ("restore", "authenticatedRecordDigest", "A" * 64),
            ("restore", "predecessorGeneration", 2),
        ]
        for section, key, val in cases:
            e = envelope()
            e["intent"]["link"][section][key] = val
            with self.subTest(section=section, key=key, val=val):
                self.refused(e)
        for key, val in (("predecessorEpoch", epoch(9)), ("predecessorGeneration", 2),
                         ("predecessorJournalDigest", "f" * 64), ("intentId", "bad\n")):
            e = envelope()
            e["intent"]["link"][key] = val
            self.refused(e)

    def test_raw_identity_generation_legacy_rules_and_auxiliary_paths(self):
        for key, val in (("sourceGeneration", 0), ("sourceGeneration", 2),
                         ("sourceGeneration", 9007199254740992),
                         ("runId", "other"), ("attemptId", attempt(2)),
                         ("clonePath", "/runner/repo/issue-1"),
                         ("predecessors", []), ("unknown", 1)):
            e = envelope()
            j = identity(1)
            j[key] = val
            replace(e, dump(j))
            self.refused(e)
        e = envelope()
        j = identity(1)
        del j["sourceGeneration"]
        replace(e, dump(j))
        self.refused(e)
        j = dict(runId="run-1", clonePath="/runner/repo/issue-1", sourceGeneration=1)
        sibling = {k: identity(4)[k] for k in ("runId", "clonePath", "attemptId")}
        j["retainedSources"] = [sibling]
        replace(e, dump(j))
        self.assertIn((attempt(4), sibling["clonePath"]), self.decode(e))
        j["retainedSources"][0]["clonePath"] = "/runner/repo/other.attempt-" + attempt(4)
        replace(e, dump(j))
        self.refused(e)
        j = identity(1)
        source = {k: j[k] for k in ("runId", "clonePath", "attemptId")}
        j["recovery"] = dict(version=1, source=source, attempts=1, startedAt=1000,
                             deadline=301000, backoffMs=0, stage="capturing")
        replace(e, dump(j))
        self.decode(e)  # no fabricated captured stage or restoreTip required
        for patch in (dict(attempts=4), dict(deadline=0), dict(stage="captured"),
                      dict(stage="blocked"), dict(backoffMs=480001), dict(unknown=1)):
            bad = copy.deepcopy(j)
            bad["recovery"].update(patch)
            replace(e, dump(bad))
            self.refused(e)

    def test_chain_crosslinks_earlier_refs_and_monotonicity(self):
        e = envelope()
        append(e)
        self.decode(e, next_raw(e))
        old = e["predecessors"][1]["rawJournal"]
        self.decode(e, old)
        for mutate in ("reverse", "missing-link", "epoch", "forward", "duplicate"):
            bad = copy.deepcopy(e)
            if mutate == "reverse":
                bad["predecessors"].reverse()
            elif mutate == "duplicate":
                bad["predecessors"].append(bad["predecessors"][0])
            elif mutate == "epoch":
                bad["predecessors"][1]["sourceEpoch"] = epoch(99)
            else:
                j = json.loads(old)
                if mutate == "missing-link":
                    del j["pinRestore"]
                else:
                    j["pinRestore"]["predecessorJournalDigest"] = "f" * 64
                bad["predecessors"][1] = entry(dump(j), 2)
                bad["intent"]["link"] = link(dump(j), 2)
            self.refused(bad)
        earlier = link(e["predecessors"][0]["rawJournal"], 1)
        earlier["successor"] = e["intent"]["link"]["successor"]
        append(e, dump(dict(identity(3), pinRestore=earlier)))
        self.decode(e)

    def test_ledger_all_raw_rows_latest_identity_branch_and_states(self):
        e = envelope()
        rows = ledger(e)
        self.decode(e, rows=rows)
        for suffix in ("garbage", "uzi-attempts.uzi/issue-1.entry ",
                       "uzi-attempts.other.entry []"):
            self.refused(e, rows=rows + "\n" + suffix)
        j = identity(1)
        for state in ("live", "reclaimed", "retired", "abandoned"):
            self.decode(e, rows=ledger_row(j, state=state))
        for change in (dict(runId="other"), dict(clonePath="/runner/repo/else"),
                       dict(attemptId=attempt(7))):
            bad = dict(j, **change)
            conflict = ledger_row(bad)
            # A conflicting earlier row cannot hide behind a matching latest row.
            self.refused(e, rows=conflict + "\n" + rows)
            self.refused(e, rows=rows + "\n" + conflict)
        self.refused(e, rows=rows.replace("uzi/issue-1.entry", "other.entry"))
        self.refused(e, rows="")
        self.refused(e, rows=rows + "\n" + rows.replace('"runId":', '"runId":"other","runId":'))
        self.refused(e, rows=rows.replace('"state":', '"futureSchema":true,"state":'))
        unrelated = ledger_row(dict(identity(9), runId="other"))
        self.decode(e, rows=rows + "\n" + unrelated)

    def test_byte_count_depth_and_actual_operation_budget(self):
        e = envelope()
        raw = e["predecessors"][0]["rawJournal"]
        replace(e, raw + " " * (16384 - len(raw.encode())))
        self.decode(e)
        replace(e, e["predecessors"][0]["rawJournal"] + " ")
        self.refused(e)
        e = envelope()
        j = identity(1)
        # Valid legacy MP field uses Unicode; byte bounds still apply before jq.
        raw = dump(j) + " " * (16385 - len(dump(j).encode()))
        replace(e, raw)
        self.refused(e)
        for padding in (" ", "\t"):
            e = envelope()
            raw = e["predecessors"][0]["rawJournal"]
            replace(e, raw + padding * (16384 - len(raw.encode())))
            raw = next_raw(e)
            append(e, raw + padding * (16384 - len(raw.encode())))
            if padding == " ":
                self.decode(e, e["predecessors"][-1]["rawJournal"])
                self.refused(e, next_raw(e))  # active pushes actual nested bytes over
            else:
                self.assertGreater(len(dump(e).encode()), 65536)
                self.refused(e)
        e = envelope()
        for _ in range(7):
            append(e)
        self.decode(e)
        append(e)
        self.refused(e)
        e = envelope()
        budget = module.Budget()
        raw_size = len(e["predecessors"][0]["rawJournal"].encode())
        self.decode(e, budget=budget)
        self.decode(e, budget=budget)
        self.assertEqual(budget.used, raw_size * 2)
        budget.used = 1048576 - raw_size
        self.decode(e, budget=budget)
        with self.assertRaises(ValueError):
            self.decode(e, budget=budget)

    def test_worker_restore_source_progress_and_sha_relations(self):
        for stage in ("capturing", "captured", "adopting", "ready-for-model", "blocked"):
            e = envelope()
            j = identity(1)
            source = {k: j[k] for k in ("runId", "clonePath", "attemptId")}
            j["restoreTip"] = "a" * 40
            j["recovery"] = dict(version=1, source=source, attempts=1, startedAt=1000,
                                 deadline=301000, backoffMs=0, stage=stage, restoreTip="a" * 40,
                                 restoreSource="b" * 40, successor=dict(source, restoreTip="a" * 40),
                                 blocker="source_missing")
            replace(e, dump(j))
            self.decode(e)
            for change in (dict(restoreSource="bad"), dict(restoreTip="c" * 40),
                           dict(restoreSource=None), dict(version=True)):
                bad = copy.deepcopy(j)
                bad["recovery"].update(change)
                replace(e, dump(bad))
                if stage in ("capturing", "blocked") and change == dict(restoreTip="c" * 40):
                    self.decode(e)  # Worker only binds captured/adopting/ready tips.
                else:
                    self.refused(e)
            bad = copy.deepcopy(j)
            del bad["recovery"]["restoreTip"]
            replace(e, dump(bad))
            self.refused(e)

    def test_linked_journal_forbids_episode_and_binds_bare_identity(self):
        e = envelope()
        source = {k: identity(2)[k] for k in ("runId", "clonePath", "attemptId")}
        recovery = dict(version=1, source=source, attempts=1, startedAt=1000,
                        deadline=301000, backoffMs=0, stage="capturing")
        for field, value in (("retainedSources", []), ("recovery", recovery)):
            active = json.loads(next_raw(e))
            active[field] = value
            self.refused(e, dump(active))
            bad = copy.deepcopy(e)
            append(bad, dump(active))
            self.refused(bad)
        e["intent"]["link"]["restore"]["bareDir"] = "other.git"
        self.refused(e)

    def test_predecessor_listing_never_probes_source_files_or_runs_commands(self):
        e = envelope()
        with mock.patch.object(module.os, "open", side_effect=AssertionError("source file opened")), \
                mock.patch.object(subprocess, "run", side_effect=AssertionError("command executed")):
            self.decode(e)
            self.decode(e, next_raw(e))

    def test_linked_active_requires_live_latest_ledger(self):
        e = envelope()
        active = next_raw(e)
        rows = ledger(e, active)
        self.decode(e, active, rows=rows)
        for state in ("reclaimed", "retired", "abandoned"):
            self.refused(e, active, rows=rows.replace('"state":"live"', '"state":"' + state + '"'))

    def test_full_budget_single_decode_and_failed_occurrences(self):
        e = envelope()
        raw = e["predecessors"][0]["rawJournal"]
        replace(e, raw + " " * (16384 - len(raw.encode())))
        active = next_raw(e)
        active += " " * (16384 - len(active.encode()))
        budget = module.Budget()
        for _ in range(32):
            self.decode(e, active, budget=budget)
        self.assertEqual(budget.used, 1048576)
        with self.assertRaises(ValueError):
            self.decode(e, active, budget=budget)
        # Failed semantic decoding still consumes its raw occurrence.
        budget = module.Budget()
        bad = dump(dict(identity(1), unknown=True))
        replace(e, bad)
        with self.assertRaises(ValueError):
            self.decode(e, budget=budget)
        self.assertEqual(budget.used, len(bad.encode()))

    def test_counter_durable_shared_and_no_symlink_or_hardlink_io(self):
        with tempfile.TemporaryDirectory(dir=HERE.parents[3] / ".uzi" / "scratch") as tmp:
            path = Path(tmp) / "counter"
            module.Budget(path).consume(100)
            module.Budget(path).consume(200)
            self.assertEqual(path.read_text(), "300")
            module.Budget(path).consume(1048576 - 300)
            with self.assertRaises(ValueError):
                module.Budget(path).consume(1)
            with self.assertRaises(ValueError):
                module.Budget(path).consume(1)
            target = Path(tmp) / "target"
            target.write_text("123")
            path.unlink()
            path.symlink_to(target)
            with self.assertRaises(OSError):
                module.Budget(path).consume(1)
            self.assertEqual(target.read_text(), "123")
            path.unlink()
            path.symlink_to(Path(tmp) / "absent")
            with self.assertRaises(OSError):
                module.Budget(path).consume(1)
            self.assertFalse((Path(tmp) / "absent").exists())
            path.unlink()
            os.link(target, path)
            with self.assertRaises(ValueError):
                module.Budget(path).consume(1)
            self.assertEqual(target.read_text(), "123")

    def test_ledger_conflicts_even_unrelated_and_multiline_value(self):
        e = envelope()
        unrelated = identity(9)
        rows = ledger(e) + "\n" + ledger_row(unrelated)
        for change in (dict(runId="other"), dict(attemptId=attempt(10)), dict(clonePath="/runner/repo/other")):
            self.refused(e, rows=rows + "\n" + ledger_row(dict(unrelated, **change)))
        name, raw = ledger(e).split(" ", 1)
        self.decode(e, rows=[(name, "\n" + json.dumps(json.loads(raw), indent=2) + "\n\n")])

    def test_stdin_read_is_bounded(self):
        stream = mock.Mock()
        stream.buffer.read.return_value = b""
        args = ["decoder", "run-1", "uzi/issue-1", "issue-1", "/runner/repo", "host+repo.git", "unused"]
        with mock.patch.object(sys, "argv", args), mock.patch.object(sys, "stdin", stream), mock.patch.object(sys, "stderr", io.StringIO()):
            self.assertEqual(module.main(), 1)
        stream.buffer.read.assert_called_once_with(92 * 1024 * 1024 + 1)

    def test_cli_invalid_utf8_duplicates_and_preserved_newline_transport(self):
        e = envelope()
        replace(e, e["predecessors"][0]["rawJournal"] + "\n\n")
        raw = ("uzi-retained.run-1.journal\n" + dump(e) + "\0"
               + "uzi-recovery.uzi/issue-1.clone\n" + e["predecessors"][0]["rawJournal"] + "\0"
               + ledger(e).replace(".entry ", ".entry\n", 1) + "\0").encode()
        with tempfile.TemporaryDirectory(dir=HERE.parents[3] / ".uzi" / "scratch") as tmp:
            args = [sys.executable, str(HERE / "pin-restore-descriptor.py"),
                    "run-1", "uzi/issue-1", "issue-1", "/runner/repo", "host+repo.git", tmp + "/budget"]
            def run(config, status=b"0"):
                return subprocess.run(args, input=base64.encodebytes(config + b"\0__UZI_CONFIG_RC=" + status),
                                      capture_output=True, check=False)
            good = run(raw)
            self.assertEqual(good.returncode, 0, good.stderr)
            active_record = ("uzi-recovery.uzi/issue-1.clone\n" + e["predecessors"][0]["rawJournal"] + "\0").encode()
            empty_active = raw.replace(active_record, b"uzi-recovery.uzi/issue-1.clone\n\0")
            self.assertEqual(run(empty_active).returncode, 0)
            for bad in (raw.replace(b"run-1", b"run-\xff", 1),
                        raw + b"uzi-retained.run-1.journal\n\0",
                        raw + b"uzi-recovery.uzi/issue-1.clone\n\0",
                        raw.replace(b"\n\n\0uzi-attempts", b"\n\0uzi-attempts")):
                self.assertNotEqual(run(bad).returncode, 0)
            self.assertNotEqual(run(raw, b"128").returncode, 0)


if __name__ == "__main__":
    unittest.main()
