# Private test image: record static tool paths without copying the Nix store.
ARG BASE_IMAGE=provided-worker-image-required
FROM ${BASE_IMAGE}
ARG INVOCATION
ARG PARENT_IMAGE_ID
RUN set -eu; \
    for tool in go task; do file -L "$(command -v "$tool")" | grep -F 'statically linked'; done; \
    node -e 'const fs=require("node:fs"),cp=require("node:child_process"),path=require("node:path"); for(const p of ["/app/uzi-worker-uid-proof","/opt/uzi-worker-uid-tools.json"]){try{fs.lstatSync(p);throw new Error("proof path already exists: "+p)}catch(e){if(e.code!=="ENOENT")throw e}} if(!require.resolve("tsx").startsWith("/app/node_modules/"))throw new Error("image-native tsx unavailable"); const goRoot=fs.realpathSync(cp.execFileSync("go",["env","GOROOT"],{encoding:"utf8"}).trim()); const taskBin=path.dirname(fs.realpathSync(cp.execFileSync("/bin/sh",["-c","command -v task"],{encoding:"utf8"}).trim())); for(const p of [goRoot,taskBin])if(!fs.statSync(p).isDirectory())throw new Error("tool directory unavailable"); fs.writeFileSync("/opt/uzi-worker-uid-tools.json",JSON.stringify({schemaVersion:1,goRoot,taskBin,invocation:process.argv[1],parentImageId:process.argv[2]}),{flag:"wx",mode:0o444});' "$INVOCATION" "$PARENT_IMAGE_ID"
