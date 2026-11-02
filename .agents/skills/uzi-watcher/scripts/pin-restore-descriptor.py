#!/usr/bin/env python3
"""Read-only V2 retained-descriptor consumer for backup-runs.sh.

No clone probes, Git commands, restore production, custody release or lease claims.
V2 validates each already decoded journal against the worker journal/progress
contract. The historical V1 jq consumer remains separate and unchanged.
Limits: 32 JSON containers, 64 KiB envelope, eight 16 KiB raw journals,
32 KiB actual nested bytes (including a distinct active journal), and 1 MiB
actual decoded journal bytes across this backup operation, including failed reads.
The budget file contains only a byte counter, never journal metadata.
"""
import base64
import binascii
import hashlib
import json
import math
import posixpath
import re
import fcntl
import os
import stat
import sys

MAX_SAFE = 9007199254740991
ID = r"[A-Za-z0-9_-]{1,160}"
UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
ATTEMPT = r"[0-9]{8}T[0-9]{6}Z-(g[0-9]+|gx)-[0-9a-f]{16}"


def require(ok):
    if not ok:
        raise ValueError("invalid pin restore descriptor")


def pairs(items):
    result = {}
    for key, value in items:
        require(key not in result)
        result[key] = value
    return result


def size(raw):
    return len(raw.encode("utf-8", errors="strict"))


def parse(raw, limit):
    require(size(raw) <= limit)
    # Bound nesting before json.loads allocates containers; quoted braces do not
    # count. JSON syntax and decoded duplicate keys are checked by json.loads.
    depth = 0
    quoted = escaped = False
    for ch in raw:
        if quoted:
            if escaped:
                escaped = False
            elif ch == "\\":
                escaped = True
            elif ch == '"':
                quoted = False
        elif ch == '"':
            quoted = True
        elif ch in "[{":
            depth += 1
            require(depth <= 32)
        elif ch in "]}":
            depth -= 1
    def constant(_):
        raise ValueError("non-JSON number")
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=constant)


def keys(value, names):
    require(type(value) is dict and set(value) == set(names.split()))
    return value


def text(value, bound=4096):
    require(type(value) is str and 0 < size(value) <= bound)
    for ch in value:
        c = ord(ch)
        require(not (c < 32 or 127 <= c <= 159 or 0xd800 <= c <= 0xdfff
                     or c in (0x061c, 0x200e, 0x200f)
                     or 0x2028 <= c <= 0x202e or 0x2066 <= c <= 0x2069))
    return value


def match(value, pattern):
    require(re.fullmatch(pattern, text(value)) is not None)
    return value


def positive(value):
    require(type(value) in (int, float) and math.isfinite(value)
            and value == int(value) and 1 <= value <= MAX_SAFE)
    return value


def absolute(value):
    p = text(value)
    require(p.startswith("/") and not p.startswith("//")
            and posixpath.normpath(p) == p)
    return p


def generation(aid):
    match(aid, ATTEMPT)
    token = aid.split("-")[1][1:]
    require(token != "x")
    return positive(int(token))


def link(value):
    o = keys(value, "version intentId predecessorJournalDigest predecessorGeneration predecessorEpoch successor restore")
    require(type(o["version"]) in (int, float) and o["version"] == 1)
    match(o["intentId"], ID)
    match(o["predecessorJournalDigest"], r"[0-9a-f]{64}")
    g = positive(o["predecessorGeneration"])
    match(o["predecessorEpoch"], UUID)
    s = keys(o["successor"], "runId claimGeneration workerId registerNonce authorityIncarnation executorIncarnation sourceEpoch attemptId clonePath")
    match(s["runId"], ID)
    require(positive(s["claimGeneration"]) > g)
    text(s["workerId"], 128)
    text(s["registerNonce"], 256)
    match(s["authorityIncarnation"], UUID)
    if s["executorIncarnation"] is not None:
        match(s["executorIncarnation"], r"[A-Za-z0-9._:-]{1,128}")
    match(s["sourceEpoch"], UUID)
    require(generation(s["attemptId"]) == s["claimGeneration"])
    require(absolute(s["clonePath"]).endswith(".attempt-" + s["attemptId"]))
    r = keys(o["restore"], "captureId authenticatedRecordDigest predecessorGeneration bareDir pinRef tip contentSource expectedHead expectedTree")
    match(r["captureId"], ID)
    match(r["authenticatedRecordDigest"], r"[0-9a-f]{64}")
    require(positive(r["predecessorGeneration"]) == g)
    match(r["bareDir"], r"[A-Za-z0-9._+-]{1,255}")
    require(r["bareDir"] not in (".", ".."))
    require(text(r["pinRef"], 512) == f'refs/uzi-recovery-pin/{s["runId"]}/{int(g)}')
    for name in ("tip", "contentSource", "expectedHead", "expectedTree"):
        match(r[name], r"[0-9a-f]{40}")
    return o


def fields(value, required, optional=""):
    require(type(value) is dict and set(required.split()) <= set(value)
            and set(value) <= set((required + " " + optional).split()))
    return value


def integer(value, minimum=0):
    require(type(value) in (int, float) and math.isfinite(value)
            and value == int(value) and minimum <= value <= MAX_SAFE)
    return value


def version(value):
    require(type(value) in (int, float) and value == 1)


def mpstring(value, maximum):
    # The memory-park contract uses JS UTF-16 length, permits controls except NUL.
    require(type(value) is str and 1 <= len(value.encode("utf-16-le", errors="surrogatepass")) // 2 <= maximum
            and "\0" not in value)
    return value


def mpbinding(value):
    b = keys(value, "runId claimGeneration memoryEpisode workerId registerNonce")
    for name, maximum in (("runId", 128), ("workerId", 128), ("registerNonce", 256)):
        mpstring(b[name], maximum)
    integer(b["claimGeneration"], 1)
    integer(b["memoryEpisode"])
    return b


def memorypark(value, rid):
    m = fields(value, "version binding ack origin", "settlement")
    version(m["version"])
    binding = mpbinding(m["binding"])
    require(binding["runId"] == rid and m["ack"] in ("confirmed", "sent_unconfirmed", "not_sent"))
    o = keys(m["origin"], "flightId attemptId executorIncarnation authorityIncarnation sourceEpoch")
    for name in ("flightId", "authorityIncarnation", "sourceEpoch"):
        match(o[name], UUID)
    if o["attemptId"] is not None:
        match(o["attemptId"], ATTEMPT)
    if o["executorIncarnation"] is not None:
        match(o["executorIncarnation"], r"[A-Za-z0-9._:-]{1,128}")
    if "settlement" not in m:
        return
    require(m["ack"] == "confirmed")
    s = keys(m["settlement"], "version confirmed sources evidence")
    version(s["version"])
    require(type(s["confirmed"]) is bool and type(s["sources"]) is list and 1 <= len(s["sources"]) <= 16)
    paths, authorities = [], set()
    for item in s["sources"]:
        x = keys(item, "path authorityKey attemptId epoch seq")
        p = mpstring(x["path"], 4096)
        require(p.startswith("/") and not p.startswith("//") and posixpath.normpath(p) == p)
        paths.append(p)
        authorities.add(match(x["authorityKey"], r"[0-9a-f]{64}"))
        match(x["epoch"], UUID)
        integer(x["seq"], 1)
        if x["attemptId"] is not None:
            match(x["attemptId"], ATTEMPT)
    # JS compares UTF-16 code units, including for astral path characters.
    ordered = sorted(set(paths), key=lambda p: p.encode("utf-16-be", errors="surrogatepass"))
    require(paths == ordered and len(authorities) == len(paths))
    ev = keys(s["evidence"], "parkAck interventionIds processJoin commands steering auxiliaries feedback sinkReceipts parkReap")
    require(mpbinding(ev["parkAck"]) == binding)
    ids = ev["interventionIds"]
    require(type(ids) is list and len(ids) <= 64)
    for item in ids:
        mpstring(item, 128)
    require(len(set(ids)) == len(ids))
    for name, allowed in (("processJoin", ("executor", "codex_disposal")),
                          ("commands", ("confirmed", "not_required")), ("steering", ("joined",)),
                          ("auxiliaries", ("joined", "none")), ("feedback", ("drained",)),
                          ("sinkReceipts", ("joined",)), ("parkReap", ("complete", "not_applicable"))):
        require(ev[name] in allowed)


class Budget:
    def __init__(self, filename=None):
        self.filename = filename
        self.used = 0

    def consume(self, count):
        if not self.filename:
            self.used += count
            require(self.used <= 1048576)
            return
        # Serial shell substitutions share a private output directory. Never follow
        # a counter symlink or accept another owner's/hard-linked/special file.
        fd = os.open(self.filename, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            info = os.fstat(fd)
            require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1
                    and info.st_uid == os.geteuid() and info.st_mode & 0o077 == 0 and info.st_size <= 16)
            raw = os.read(fd, 17)
            require(not raw or re.fullmatch(rb"[0-9]{1,16}", raw) is not None)
            self.used = (int(raw) if raw else 0) + count
            # Persist over-budget failures too; later descriptors cannot reset it.
            os.lseek(fd, 0, os.SEEK_SET)
            data = str(self.used).encode("ascii")
            require(os.write(fd, data) == len(data))
            os.ftruncate(fd, len(data))
            os.fsync(fd)
            require(self.used <= 1048576)
        finally:
            os.close(fd)


class Decoder:
    def __init__(self, rid, branch, stem, root, bare_dir, budget=None):
        self.rid = match(rid, ID)
        self.branch = text(branch, 256)
        self.stem = match(stem, r"[A-Za-z0-9_-]{1,255}")
        self.root = absolute(root)
        self.bare_dir = match(bare_dir, r"[A-Za-z0-9._+-]{1,255}")
        self.budget = budget if budget is not None else Budget()

    def source(self, s):
        require(type(s) is dict and s.get("runId") == self.rid)
        match(s["runId"], ID)
        p = absolute(s["clonePath"])
        expected = self.root.rstrip("/") + "/" + self.stem
        if "attemptId" in s:
            match(s["attemptId"], ATTEMPT)
            expected += ".attempt-" + s["attemptId"]
        require(p == expected)
        if "restoreTip" in s:
            match(s["restoreTip"], r"[0-9a-f]{40}")
        return s

    def journal(self, raw):
        require(size(raw) <= 16384)
        self.budget.consume(size(raw))
        j = parse(raw, 16384)
        require(type(j) is dict and "predecessors" not in j and "intent" not in j)
        positive(j["sourceGeneration"])
        fields(j, "runId clonePath sourceGeneration", "attemptId restoreTip recovery retainedSources peeledMarkers memoryPark completionSourceFence pinRestore")
        if "pinRestore" in j:
            l = link(j["pinRestore"])
            require("recovery" not in j and "retainedSources" not in j)
            self.tuple(j, l)
        if "retainedSources" in j:
            require(type(j["retainedSources"]) is list)
        if "peeledMarkers" in j:
            markers = j["peeledMarkers"]
            require(type(markers) is list and len(markers) <= 64)
            for marker in markers:
                match(marker, r"[0-9a-f]{40}")
            require(len(set(markers)) == len(markers))
        if "memoryPark" in j:
            memorypark(j["memoryPark"], self.rid)
        if "recovery" in j:
            self.progress(j)
        self.source(j)
        if "completionSourceFence" in j:
            fence = keys(j["completionSourceFence"], "version sourceUseEpoch sourceCoreDigest")
            version(fence["version"])
            match(fence["sourceUseEpoch"], UUID)
            match(fence["sourceCoreDigest"], r"[0-9a-f]{64}")
        if "attemptId" in j:
            require(generation(j["attemptId"]) == j["sourceGeneration"])
        for s in self.paths(j)[1:]:
            fields(s, "runId clonePath", "attemptId restoreTip")
            self.source(s)
        return j

    def progress(self, j):
        r = fields(j["recovery"], "version source attempts startedAt deadline backoffMs stage",
                   "claimGeneration episode restoreTip restoreSource successor blocker")
        version(r["version"])
        require(1 <= integer(r["attempts"], 1) <= 3)
        integer(r["startedAt"])
        require(integer(r["deadline"]) == r["startedAt"] + 300000)
        require(integer(r["backoffMs"]) <= 480000)
        if "claimGeneration" in r:
            integer(r["claimGeneration"], 1)
        if "episode" in r:
            integer(r["episode"])
        stage = r["stage"]
        require(stage in ("capturing", "captured", "adopting", "ready-for-model", "blocked"))
        for name in ("restoreTip", "restoreSource"):
            if name in r:
                match(r[name], r"[0-9a-f]{40}")
        if "restoreSource" in r:
            require("restoreTip" in r)
        if "blocker" in r:
            require(r["blocker"] in ("capture_failed", "source_missing", "adoption_failed", "budget_exhausted",
                                    "clock_invalid", "oversize", "prerequisites_unavailable", "quiescence_failed",
                                    "preservation_failed", "decoded_history_limit", "readiness_cleanup_unconfirmed"))
        if stage == "blocked":
            require("blocker" in r)
        if stage in ("captured", "adopting", "ready-for-model"):
            require("restoreTip" in r and j.get("restoreTip") == r["restoreTip"])
        if stage in ("adopting", "ready-for-model"):
            require(type(r.get("successor")) is dict and r["successor"].get("restoreTip") == r["restoreTip"])
        def same(s):
            require(type(s) is dict)
            return all(j.get(k) == s.get(k) for k in ("runId", "clonePath", "attemptId"))
        require(type(r["source"]) is dict)
        require(same(r["source"]) or any(same(s) for s in j.get("retainedSources", []))
                or (type(r.get("successor")) is dict and same(r["successor"])))
        if stage == "ready-for-model":
            require(same(r["successor"]))

    @staticmethod
    def paths(j):
        return [j] + j.get("retainedSources", []) + [
            j["recovery"][name] for name in ("source", "successor")
            if name in j.get("recovery", {})]

    @staticmethod
    def tuple(j, l):
        s = l["successor"]
        require(j["runId"] == s["runId"] and j["clonePath"] == s["clonePath"]
                and j.get("attemptId") == s["attemptId"]
                and j["sourceGeneration"] == s["claimGeneration"])

    def ledger(self, raw, sources, live=None):
        rows = []
        # CLI transport passes exact NUL-framed config rows. The string form is
        # retained for the small in-process fixtures.
        records = raw if type(raw) is list else [line for line in raw.split("\n") if line]
        for record in records:
            name, value = record if type(record) is tuple else record.split(" ", 1)
            m = re.fullmatch(r"uzi-attempts\.(.*)\.entry", name)
            require(m is not None)
            row = parse(value, 64 * 1024 * 1024)
            keys(row, "attemptId runId clonePath state")
            match(row["attemptId"], ATTEMPT)
            match(row["runId"], ID)
            absolute(row["clonePath"])
            require(row["state"] in ("live", "abandoned", "retired", "reclaimed"))
            rows.append((m[1], row))
        latest, by_path = {}, {}
        for branch, row in rows:
            identity = (branch, row["runId"], row["clonePath"], row["attemptId"])
            for index, key in ((latest, row["attemptId"]), (by_path, row["clonePath"])):
                if key in index:
                    old_branch, old = index[key]
                    require(identity == (old_branch, old["runId"], old["clonePath"], old["attemptId"]))
                index[key] = (branch, row)
        def same(branch, row, s):
            return (branch == self.branch and row["runId"] == s["runId"]
                    and row["clonePath"] == s["clonePath"]
                    and row.get("attemptId") == s.get("attemptId"))
        for s in sources:
            for branch, row in rows:
                if row["attemptId"] == s.get("attemptId") or row["clonePath"] == s["clonePath"]:
                    require(same(branch, row, s))
            if "attemptId" in s:
                require(s["attemptId"] in latest)
                require(same(*latest[s["attemptId"]], s))
        if live is not None:
            require(latest[live["attemptId"]][1]["state"] == "live")

    def decode(self, raw, active, ledger):
        e = keys(parse(raw, 65536), "version runId branch key predecessors intent")
        require(type(e["version"]) in (int, float) and e["version"] == 2
                and e["runId"] == self.rid and e["branch"] == self.branch and e["key"] == self.stem)
        intent = keys(e["intent"], "version phase link")
        require(type(intent["version"]) in (int, float) and intent["version"] == 1
                and intent["phase"] in ("prepared", "ready"))
        l = link(intent["link"])
        require(l["successor"]["runId"] == self.rid and l["restore"]["bareDir"] == self.bare_dir)
        self.source(l["successor"])
        history = e["predecessors"]
        require(type(history) is list and 1 <= len(history) <= 8)
        seen = {}
        paths = set()
        attempts = set()
        raw_sum = 0
        sources = []
        last = None
        for entry in history:
            keys(entry, "journalDigest rawJournal generation sourceEpoch")
            require(type(entry["rawJournal"]) is str)
            raw_sum += size(entry["rawJournal"])
            require(raw_sum <= 32768)
            j = self.journal(entry["rawJournal"])
            digest = match(entry["journalDigest"], r"[0-9a-f]{64}")
            positive(entry["generation"])
            match(entry["sourceEpoch"], UUID)
            require(hashlib.sha256(entry["rawJournal"].encode("utf-8")).hexdigest() == digest
                    and j["sourceGeneration"] == entry["generation"]
                    and digest not in seen and j["clonePath"] not in paths
                    and ("attemptId" not in j or j["attemptId"] not in attempts))
            if last is not None:
                require(entry["generation"] > last["generation"] and "pinRestore" in j)
            if "pinRestore" in j:
                previous = j["pinRestore"]
                referenced = seen.get(previous["predecessorJournalDigest"])
                require(previous["restore"]["bareDir"] == self.bare_dir
                        and referenced is not None
                        and referenced["generation"] == previous["predecessorGeneration"]
                        and referenced["sourceEpoch"] == previous["predecessorEpoch"]
                        and previous["successor"]["sourceEpoch"] == entry["sourceEpoch"])
                self.tuple(j, previous)
            seen[digest] = entry
            paths.add(j["clonePath"])
            if "attemptId" in j:
                attempts.add(j["attemptId"])
            sources.extend(self.paths(j))
            last = entry
        require(l["predecessorJournalDigest"] == last["journalDigest"]
                and l["predecessorGeneration"] == last["generation"]
                and l["predecessorEpoch"] == last["sourceEpoch"]
                and l["successor"]["clonePath"] not in paths
                and l["successor"]["attemptId"] not in attempts)
        live = None
        if active is not None:
            if active == last["rawJournal"]:
                require(intent["phase"] == "prepared")
            else:
                require(raw_sum + size(active) <= 32768)
                j = self.journal(active)
                self.tuple(j, l)
                require(j.get("pinRestore") == l)
                # Prefer the actual active source when present; its tuple is
                # metadata attribution only, never execution/completion proof.
                sources = self.paths(j) + sources
                live = j
        self.ledger(ledger, sources, live)
        return list(dict.fromkeys((s.get("attemptId", ""), s["clonePath"]) for s in sources))


def config_records():
    # One bounded transport parser for legacy queries and the strict V2 consumer.
    limit = 92 * 1024 * 1024
    payload = sys.stdin.buffer.read(limit + 1)
    require(len(payload) <= limit)
    require(re.fullmatch(rb"[A-Za-z0-9+/=\r\n]*", payload) is not None)
    config = base64.b64decode(payload.replace(b"\r", b"").replace(b"\n", b""), validate=True)
    suffix = b"\0__UZI_CONFIG_RC=0"
    require(len(config) <= 64 * 1024 * 1024 and config.endswith(suffix))
    body = config[:-len(suffix)]
    require(not body or body.endswith(b"\0"))
    records = []
    for record in body.split(b"\0")[:-1]:
        require(bool(record))
        name, separator, value = record.decode("utf-8", errors="strict").partition("\n")
        require(bool(name))
        records.append((name, separator, value))
    return records


def config_query(records, mode, key):
    # Match Git's last-value --get and ordered --get-all/--get-regexp output.
    if mode == "--get-regexp":
        rows = [(n, sep, v) for n, sep, v in records
                if n.startswith("uzi-attempts.") and n.endswith(".entry")]
    else:
        require(mode in ("--get", "--get-all"))
        rows = [(n, sep, v) for n, sep, v in records if n == key]
        if mode == "--get":
            rows = rows[-1:]
    for name, separator, value in rows:
        if mode == "--get-regexp":
            sys.stdout.write(name + (" " if separator else ""))
        sys.stdout.write(value + "\n")
    # Equivalent to kexec_probe's additional framing newline, even for absence.
    sys.stdout.write("\n")
    return 0 if rows else 1


def main():
    try:
        records = config_records()
        if sys.argv[1:2] == ["--config-query"]:
            return config_query(records, *sys.argv[2:])
        rid, branch, stem, root, bare_dir, budget = sys.argv[1:]
        values, ledger = {}, []
        for name, separator, value in records:
            if name.startswith(("uzi-recovery.", "uzi-retained.")):
                require(separator and name not in values)
                values[name] = value
            elif name.startswith("uzi-attempts.") and name.endswith(".entry"):
                require(separator)
                ledger.append((name, value))
        raw = values[f"uzi-retained.{rid}.journal"]
        # Worker metadata treats an empty active slot as absent, never as proof.
        # Certified retirement clears the active VALUE to empty. This is the
        # worker reader\'s absent-metadata form, never retirement/execution proof.
        active = values.get(f"uzi-recovery.{branch}.clone") or None or None
        decoder = Decoder(rid, branch, stem, root, bare_dir, Budget(budget))
        rows = decoder.decode(raw, active, ledger)
        for aid, p in rows:
            print(aid + "\t" + p)
    except (ValueError, KeyError, TypeError, OverflowError, OSError, RecursionError,
            UnicodeError, binascii.Error):
        print("invalid or unreadable V2 protected descriptor", file=sys.stderr)
        return 2 if sys.argv[1:2] == ["--config-query"] else 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
