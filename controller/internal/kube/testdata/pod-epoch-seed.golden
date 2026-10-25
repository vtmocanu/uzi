set -eu
/usr/local/bin/node -e '
const fs = require("node:fs");
const s = fs.lstatSync("/data-seed");
if (!s.isDirectory() || s.isSymbolicLink() || s.gid !== 10001 ||
    (s.mode & 0o2770) !== 0o2770 || (s.mode & 0o002) ||
    ![0, 10001].includes(s.uid)) throw new Error("unsafe data root posture");
'
/usr/local/bin/node /usr/local/lib/uzi/pod-epoch-init.cjs seed /data-seed "$UZI_POD_UID"
if [ "${UZI_EPOCH_LOCK:-}" = 1 ]; then
  identity="$(/usr/local/bin/node /usr/local/lib/uzi/pod-epoch-init.cjs prepare /data-seed)"
  if [ "$identity" != unsupported ]; then
    exec 9</data-seed/.uzi-epoch.lock
    /usr/local/bin/node /usr/local/lib/uzi/pod-epoch-init.cjs hold /data-seed "$identity" 9
    trap 'exec 9>&-' EXIT
  fi
fi
set -eu
set -o pipefail
WANT="$(cat /etc/uzi-toolchain-profile 2>/dev/null || true)"
HAVE="$(cat /nix-seed/.uzi-nix-toolchain 2>/dev/null || true)"
if [ -f /nix-seed/.uzi-nix-seeded ] && [ -f /nix-seed/.uzi-nix-toolchain ] && [ -n "$WANT" ] && [ "$WANT" = "$HAVE" ]; then
  echo "uzi-seed-nix: store already seeded (toolchain $WANT matches); nothing to do"
  exit 0
fi
echo "uzi-seed-nix: seeding the nix store from the image (want=$WANT have=$HAVE)"
find /nix-seed -mindepth 1 -type d -exec chmod u+w {} + 2>/dev/null || true
find /nix-seed -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
echo "uzi-seed-nix: clearing dangling shared devbox/nix provisioning state under /data-seed (reseed)"
rm -rf /data-seed/agent-home/.local/share/devbox /data-seed/agent-home/.local/state/nix /data-seed/agent-home/.local/share/nix /data-seed/provision 2>/dev/null || true
cd /nix
tar -cf - -- $(ls -A) | tar -xpf - -C /nix-seed
printf '%s' "$WANT" > /nix-seed/.uzi-nix-toolchain
: > /nix-seed/.uzi-nix-seeded
echo "uzi-seed-nix: seeded"
