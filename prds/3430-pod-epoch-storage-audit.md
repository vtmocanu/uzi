# Pod-epoch storage ownership source audit

Read on 2026-10-25 for PRD #3430 M1. This is source/design evidence; actual driver, node and mount acceptance remains the operator's M3 obligation.

## Pinned sources

| Runtime declaration | Kubelet | Sources |
|---|---|---|
| `containerd-runc` | 1.37.1 | [volume_linux.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/volume/volume_linux.go), [csi_mounter.go](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/volume/csi/csi_mounter.go) |
| `crio-crun` | 1.35.3 | [volume_linux.go](https://github.com/kubernetes/kubernetes/blob/v1.35.3/pkg/volume/volume_linux.go), [csi_mounter.go](https://github.com/kubernetes/kubernetes/blob/v1.35.3/pkg/volume/csi/csi_mounter.go) |

SHA-256 of the fetched raw source bytes:

| File/version | SHA-256 |
|---|---|
| volume_linux.go 1.37.1 | `d1cf226c04ad6ef848c8345e1832cc13663a0696dcd7a7918242786b60c9ba38` |
| volume_linux.go 1.35.3 | `2289454e559edbb6489d6ef36a61b676cea999bcc26b10a0e849ccb076174b63` |
| csi_mounter.go 1.37.1 | `1b68fdf89d2cbe0fe05eee98f894747dc8c366608769b94a8ace5a20bae2c89d` |
| csi_mounter.go 1.35.3 | `707e22ee36987feb3f0cb034e3b519137759bcd465899f48b958900e4e969448` |

## Exact matching-root predicate

In both versions, `skipPermissionChange` skips the walk only for `FSGroupChangeOnRootMismatch` when `requiresPermissionChange` returns false. The latter requires a directory with the requested gid, sufficient owner **and** group permissions, and setgid. For the writable data volume, `rwMask = 0660` plus `execMask = 0110` requires the complete `0770` permission subset. The check is:

```go
if (unixPerms&filePerm != unixPerms) || (fsInfo.Mode()&os.ModeSetgid == 0) {
    return true
}
```

See 1.37.1 lines 183–230 and 1.35.3 lines 184–231. A group-only check such as mode `02070` is insufficient even though the worker group has access.

The worker and seed guards therefore require gid 10001 and `(mode & 02770) == 02770`. They additionally reject world write and require trusted root/worker ownership. They preserve these fields throughout initialization; they do not repair a mismatched mounted root. The descendant migration excludes the mount-root inode, and participating startup skips data-root alignment.

Host execution tests isolate the actual pre-initialization validators and cover `02770`/`02775`, each missing owner permission, missing setgid and world write. The image fixture checks data-root metadata before and after each initializer mutation. These tests establish application invariance, not actual kubelet/CSI behavior.

## CSI delegation boundary

The CSI mounter asks `NodeSupportsVolumeMountGroup` (1.37.1 lines 251–261; 1.35.3 lines 250–260). When true it passes the gid through NodePublishVolume. The kubelet-owned `VolumeOwnership.ChangePermissions` path runs only when that capability is false and `supportsFSGroup` permits it (1.37.1 lines 338–348; 1.35.3 lines 333–343).

Therefore rendering `OnRootMismatch` alone does not prove that a delegated driver skips recursive ownership work. Application flock does not fence kubelet or CSI. Before declaring a runtime, the operator must verify the actual driver/node ownership behavior, matching-root preparation, attach enforcement or node-local storage, and absence of overlapping recursive ownership walks. A driver that walks despite a matching root is unsupported and must remain default-off. No live acceptance is claimed here.
