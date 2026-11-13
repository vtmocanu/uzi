# Plan-time evidence for #4291 (builder-cache prune leg)

These files are the plan-time verification that the frozen production argv

```
docker builder prune -f --filter type=regular --filter until=24h --max-used-space 4GiB
```

behaves as #4291 requires. The owner-side reviewer accepted them as discharging the
issue's "verify before implementing" gate, bound to the checksums below. They are an
archive: no task target executes them, and they are kept byte-identical to what ran
(do not edit them; re-check with `sha256sum` in this directory).

| File (committed name) | Original name | sha256 |
| --- | --- | --- |
| `full.sh.txt` | `full.sh` | `d7a434b0a8167ebea845f405375807629c51214b2884c796b3b1ef8946d514ff` |
| `lib.sh.txt` | `lib.sh` | `bc0949b1673f004ce548e15ecc49e5880d0016a382b2666a42c5a87d7b197802` |
| `full-rootful.log` | `full-rootful.log` | `97bb16f47dc38e4eaa4d2c04e056cf7a0118d5daf5c57b7bbb94f16603da6e7f` |
| `full-rootless.log` | `full-rootless.log` | `3234841665187e2747cbd83dd6dbece10698abbdaf18eb6f7ebbf6b196ffbcf5` |
| `containers-created.txt` | `containers-created.txt` | `b95efefd703f25b0ce6c357c1eadbda3252e6d5ae5542ec86e7bfb982c8ac8ad` |

The two scripts carry a `.txt` suffix because the archived `lib.sh` has a ShellCheck
warning (SC2034) and must not be edited. The suffix keeps the bytes exact without
exempting or weakening `task lint:shell`, which still checks `full.sh.txt` through its
shebang.

## What ran

- Owned, disposable nested daemons only (`bpspike-*` containers on the worker's daemon,
  each removed by exact name; see `containers-created.txt`). No prune, builder or system
  command ran against a shared daemon, and no clock was altered.
- Both postures from the chart-pinned images: `docker:29-dind` (rootful) and
  `docker:29-dind-rootless`.
- The deployed tuple, printed in each log: server 29.8.0 (API 1.56, minimum 1.40,
  BuildKit v0.33.0, containerd snapshotter), worker-image client 29.9.2 and buildx v0.38.1
  copied into the nested container.
- The exact production argv ran three times per posture (fresh cache, cache present,
  final). Each exited 0 and deleted nothing young.
- Checks 1-6 used a SCALED, test-only floor (`until=30s`) and scaled caps, each with a
  positive control deleted in the same prune. The real 24 h floor was not observed.

## Record IDs

Rootful (`full-rootful.log`):

- P1, run while a concurrent build held a record. Deleted `q9k64qg7kuglwpt5o0g9vo4k3`
  (unheld old control) and `pu8hp5zv9m9ni704uwfm3njlu`. Kept `tzfchqiwpvrz9x74silbhi5ea`
  (held, in use), `wf1dqr32dt42rigxhbc9o17we` (exec.cachemount), `flf3ayo5ai647iamdp6dpsg5a`
  (recently used), `azlibcwbppze1tss7imtptcdw` (young) and `bcawahr4wf9i30r5srh511af6`
  (base, in use). The held build then finished, `HOLD_BUILD_RC=0`.
- P4, the image check. Deleted `k7j56mz0jqgs5i261rh5du5so`, `zhaymyfbcbmr17cx9lgs2fbx8`,
  `d668lmpugiovaoh2yibgf3m9e` and `tzfchqiwpvrz9x74silbhi5ea` (formerly held, now old and
  unheld). Kept `l0ftsqv9dwztjjh6fpidpkpce` (the tagged image's record, shared) and
  `bcawahr4wf9i30r5srh511af6`. Afterwards `IMAGE_LS_RC=0`, `RUN_KEEP_RC=0`, `RUN_TRUE_RC=0`.

Rootless (`full-rootless.log`):

- P1. Deleted `v8bmsdejg601troia71z4g0q8` and `n8ujl5ynszb4hrpcm8vw8glec`. Kept
  `egm2ezjdyd0r7m535vqfiereo` (held), `p7q9gldu7t9n02vtnxt1c3o2p` (exec.cachemount),
  `x0zrtndbedq7r8bnkgf08ifpn` (recent), `stvsns9mweta7hprs3zc18vo3` (young) and
  `s3smd6ahue0dll6y88q7popek` (base). `HOLD_BUILD_RC=0`.
- P4. Deleted `vz8sajod1tkayre7bgr3mei52`, `zq6p4jtzy8m9b8uft0cfr2zv1`,
  `6sg2ehbitrgebww8czd4vk9e1` and `egm2ezjdyd0r7m535vqfiereo`. Kept
  `ni08y0rlpb0rvx07rpzf11gtx` and `s3smd6ahue0dll6y88q7popek`. `IMAGE_LS_RC=0`,
  `RUN_KEEP_RC=0`, `RUN_TRUE_RC=0`.

Both postures: with the cap above usage (P2, `--max-used-space 4GiB`) nothing was
deleted. With the cap just below usage (P3), least-recently-used eligible records were
deleted until usage fell under it. Records sharing snapshots with a local image
(`shared=true`) are never deleted by this argv.

The tracked, repeatable integration proof is `e2e/dind-builder-prune/run.sh`.
