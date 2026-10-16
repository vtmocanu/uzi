#!/usr/bin/env python3
"""Prepare invocation-owned, image-bound static tool metadata, offline."""
import json
from pathlib import Path
import re
import sys
import time

import bounded

PARENT_LABEL = "io.uzi.worker-uid.parent-image"
PROBE = ('const fs=require("node:fs"); try { fs.lstatSync("/app/uzi-worker-uid-proof"); '
         'throw new Error("proof directory already exists in image"); } '
         'catch(e) { if(e.code!=="ENOENT")throw e; } '
         'if(!require.resolve("tsx").startsWith("/app/node_modules/"))'
         'throw new Error("image-native tsx unavailable"); '
         'process.stdout.write(fs.readFileSync("/opt/uzi-worker-uid-tools.json","utf8"))')


def image_id(value):
    if not isinstance(value, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value):
        raise ValueError("invalid image identity")
    return value


def metadata(raw):
    items = json.loads(raw)
    if not isinstance(items, list) or len(items) != 1 or not isinstance(items[0], dict):
        raise ValueError("ambiguous image metadata")
    item = items[0]
    image_id(item.get("Id"))
    if item.get("Os") != "linux" or item.get("Architecture") not in ("amd64", "arm64"):
        raise ValueError("unsupported proof image platform")
    layers = item.get("RootFS", {}).get("Layers")
    if not isinstance(layers, list) or not layers:
        raise ValueError("image layers unavailable")
    for layer in layers:
        image_id(layer)
    return item


def tool_paths(raw, token, parent):
    item = json.loads(raw)
    if not isinstance(item, dict) or set(item) != {"schemaVersion", "goRoot", "taskBin", "invocation", "parentImageId"}:
        raise ValueError("invalid tool metadata fields")
    if type(item["schemaVersion"]) is not int or item["schemaVersion"] != 1 or item["invocation"] != token or item["parentImageId"] != parent:
        raise ValueError("foreign tool metadata binding")
    paths = [item["goRoot"], item["taskBin"]]
    for value in paths:
        if not isinstance(value, str) or len(value) > 1024 or not re.fullmatch(r"/nix/store/[a-z0-9]{32}-[A-Za-z0-9+_.-]+(?:/[A-Za-z0-9+_.-]+)*", value):
            raise ValueError("invalid canonical tool path")
        if any(part in (".", "..") for part in value.split("/")):
            raise ValueError("noncanonical tool path")
    if paths[0] == paths[1] or paths[0].startswith(paths[1] + "/") or paths[1].startswith(paths[0] + "/"):
        raise ValueError("overlapping tool mounts")
    return paths


def prepare(directory, token, provided):
    if not re.fullmatch(r"[0-9a-f]{32}", token):
        raise ValueError("invalid invocation token")
    deadline = time.monotonic() + 19  # Outer phase is 20s + five-second kill grace.

    def docker(args):
        left = deadline - time.monotonic()
        if left <= 5:
            raise ValueError("tool image preparation deadline exhausted")
        return bounded.docker(args, directory, min(10, left))

    def journal(kind, name, expected=None):
        with (directory / "attempted").open("a", encoding="utf8") as stream:
            stream.write(" ".join([kind, name, *([expected] if expected else [])]) + "\n")

    parent = metadata(docker(["image", "inspect", provided]))
    parent_id = parent["Id"]
    # Use an existing tag without modifying shared parent-image references.
    tags = parent.get("RepoTags")
    if not isinstance(tags, list) or not tags or not all(isinstance(tag, str) for tag in tags):
        raise ValueError("proof image requires an existing local tag")
    base = tags[0]
    if metadata(docker(["image", "inspect", base]))["Id"] != parent_id:
        raise ValueError("parent tag identity mismatch")
    child = f"wuid-2134-{token}-tools"
    for name in (child,):
        if docker(["image", "ls", "--filter", f"reference={name}:latest", "--format", "{{.ID}}"] ).strip():
            raise ValueError("proof image name already exists")
    context = docker(["context", "show"]).decode("utf8").strip()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", context):
        raise ValueError("invalid Docker context")
    journal("image", child)
    harness = Path(__file__).resolve().parent
    left = deadline - time.monotonic() - 2  # Reserve validation; never extend the outer phase.
    if left <= 1:
        raise ValueError("tool image build deadline exhausted")
    command = ["timeout", "--kill-after=5s", f"{left:g}s", "docker", "build", "--builder", context,
               "--platform", "linux/" + parent["Architecture"], "--network", "none", "--pull=false",
               "--provenance=false", "--build-arg", "BASE_IMAGE=" + base,
               "--build-arg", "INVOCATION=" + token, "--build-arg", "PARENT_IMAGE_ID=" + parent_id,
               "--label", bounded.LABEL + "=" + token, "--label", PARENT_LABEL + "=" + parent_id,
               "--file", str(harness / "tool-image.Dockerfile"), "--tag", child, str(harness)]
    status = bounded.capture(command, directory / "tool-image-build.stdout", left + 6)
    if status:
        raise ValueError(f"tool image build exit: {status}")
    derived = metadata(docker(["image", "inspect", child]))
    labels = derived.get("Config", {}).get("Labels")
    if not isinstance(labels, dict) or labels.get(bounded.LABEL) != token or labels.get(PARENT_LABEL) != parent_id:
        raise ValueError("foreign tool image binding")
    if derived["Architecture"] != parent["Architecture"] or derived["RootFS"]["Layers"][:len(parent["RootFS"]["Layers"])] != parent["RootFS"]["Layers"]:
        raise ValueError("tool image parent layers mismatch")
    child_id = derived["Id"]
    lines = (directory / "attempted").read_text().splitlines()
    (directory / "attempted").write_text("\n".join(line + " " + child_id if line == "image " + child else line for line in lines) + "\n")
    probe = f"wuid-2134-{token}-provision"
    journal("container", probe)
    raw = docker(["run", "--pull", "never", "--network", "none", "--tmpfs", "/data", "--tmpfs", "/nix",
                  "--label", bounded.LABEL + "=" + token, "--name", probe, "--user", "10003:10003",
                  "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--entrypoint",
                  "/usr/local/bin/node", child_id, "-e", PROBE])
    go_root, task_bin = tool_paths(raw, token, parent_id)
    volumes = {}
    for tool in ("go", "task"):
        name = f"wuid-2134-{token}-{tool}"
        if docker(["volume", "ls", "--filter", "name=" + name, "--format", "{{.Name}}"] ).strip():
            raise ValueError("proof tool volume name already exists")
        journal("volume", name)
        receipt = docker(["volume", "create", "--label", bounded.LABEL + "=" + token, name]).decode().strip()
        if receipt != name:
            raise ValueError("tool volume create identity mismatch")
        left = deadline - time.monotonic()
        if left <= 5:
            raise ValueError("tool volume preparation deadline exhausted")
        bounded.owned("volume", name, token, directory, min(10, left))
        volumes[tool] = name
    result = {"parentImageId": parent_id, "imageId": child_id, "goRoot": go_root, "taskBin": task_bin,
              "goVolume": volumes["go"], "taskVolume": volumes["task"]}
    (directory / "command-image.json").write_text(json.dumps(result) + "\n")
    print(parent_id, child_id, go_root, task_bin)


if __name__ == "__main__":
    try:
        prepare(Path(sys.argv[1]), sys.argv[2], sys.argv[3])
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"worker UID tool image FAIL: {error}", file=sys.stderr)
        sys.exit(1)
