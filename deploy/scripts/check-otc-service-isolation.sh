#!/usr/bin/env bash
set -euo pipefail

BOT_SERVICE=btc09-otc-bot.service
FEED_SERVICE=btc09-otc-feed.service
BOT_USER=btc09-otc
FEED_USER=btc09-otc-feed
PYTHON=/opt/btc09/venv/bin/python
PUBLIC_SOURCE=/var/lib/btc09-otc-public
PUBLIC_MOUNT=/run/btc09-otc-feed/public
CHAIN_DATA=/opt/btc09/data
CHAIN_BLOCKS=/opt/btc09/data/blocks-mainnet.dat
CHAIN_LOCK=/opt/btc09/data/blocks-mainnet.dat.lock

fail() {
  printf '%s\n' "OTC service isolation check failed: $1" >&2
  exit 1
}

[[ $# -eq 0 ]] || fail "this command takes no arguments"
[[ $(id -u) -eq 0 ]] || fail "must run as root"
for command in systemctl getent id nsenter runuser grep cut tr stat findmnt realpath; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable"
done
[[ -x $PYTHON ]] || fail "deployed Python is unavailable"
getent passwd "$BOT_USER" >/dev/null || fail "bot service user is missing"
getent passwd "$FEED_USER" >/dev/null || fail "feed service user is missing"
bot_group_id=$(getent group "$BOT_USER" | cut -d: -f3)
[[ $bot_group_id =~ ^[0-9]+$ ]] || fail "bot custody group is invalid"
if id -G "$FEED_USER" | tr ' ' '\n' | grep -Fxq "$bot_group_id"; then
  fail "feed user belongs to the custody group"
fi

"$PYTHON" - "$CHAIN_DATA" "$CHAIN_BLOCKS" "$CHAIN_LOCK" /opt/btc09/data/wallet-mainnet.json <<'PY'
import os
import sys

for path in sys.argv[1:]:
    names = os.listxattr(path, follow_symlinks=False)
    if any(name.startswith("system.posix_acl_") for name in names):
        raise SystemExit("extended POSIX ACL is forbidden on chain state")
PY

[[ -d $CHAIN_DATA && ! -L $CHAIN_DATA ]] || fail "chain data parent is unsafe"
[[ $(realpath -ms -- "$CHAIN_DATA") == "$(realpath -e -- "$CHAIN_DATA")" ]] || fail "chain data parent contains a symbolic link"
chain_parent_owner=$(stat -c '%u' -- "$CHAIN_DATA")
chain_parent_mode=$(stat -c '%a' -- "$CHAIN_DATA")
[[ $chain_parent_owner == 0 ]] || fail "chain data parent is not root-owned"
(( (8#$chain_parent_mode & 0022) == 0 )) || fail "chain data parent is group/world writable"
[[ -f $CHAIN_LOCK && ! -L $CHAIN_LOCK ]] || fail "chain lock is missing or unsafe"
[[ $(realpath -ms -- "$CHAIN_LOCK") == "$CHAIN_LOCK" ]] || fail "chain lock path is not exact"
[[ $(realpath -e -- "$CHAIN_LOCK") == "$CHAIN_LOCK" ]] || fail "chain lock contains a symbolic link"
[[ $(stat -c '%h' -- "$CHAIN_LOCK") == 1 ]] || fail "chain lock has multiple links"
[[ $(stat -c '%u:%g:%a' -- "$CHAIN_LOCK") == "0:$bot_group_id:660" ]] || fail "chain lock ownership or mode is unsafe"
[[ -f $CHAIN_BLOCKS && ! -L $CHAIN_BLOCKS ]] || fail "chain block file is missing or unsafe"
[[ $(realpath -ms -- "$CHAIN_BLOCKS") == "$CHAIN_BLOCKS" ]] || fail "chain block path is not exact"
[[ $(realpath -e -- "$CHAIN_BLOCKS") == "$CHAIN_BLOCKS" ]] || fail "chain block path contains a symbolic link"
[[ $(stat -c '%h' -- "$CHAIN_BLOCKS") == 1 ]] || fail "chain block file has multiple links"
[[ $(stat -c '%u:%g:%a' -- "$CHAIN_BLOCKS") == "0:$bot_group_id:640" ]] || fail "chain block ownership or mode is unsafe"
lock_identity=$(stat -Lc '%d:%i' -- "$CHAIN_LOCK")
blocks_identity=$(stat -Lc '%d:%i' -- "$CHAIN_BLOCKS")

bot_pid=$(systemctl show -p MainPID --value "$BOT_SERVICE")
feed_pid=$(systemctl show -p MainPID --value "$FEED_SERVICE")
[[ $bot_pid =~ ^[1-9][0-9]*$ && $feed_pid =~ ^[1-9][0-9]*$ ]] || fail "service PID evidence is invalid"
[[ -d $PUBLIC_SOURCE && ! -L $PUBLIC_SOURCE ]] || fail "public source entry is unsafe"
[[ $(realpath -ms -- "$PUBLIC_SOURCE") == "$(realpath -e -- "$PUBLIC_SOURCE")" ]] || fail "public source contains a symbolic link"
[[ $(stat -c '%u:%g:%a' -- "$PUBLIC_SOURCE") == "0:$bot_group_id:775" ]] || fail "public source ownership or mode is unsafe"
source_identity=$(stat -Lc '%d:%i' -- "$PUBLIC_SOURCE")
mount_identity=$(nsenter --target "$feed_pid" --mount -- stat -Lc '%d:%i' -- "$PUBLIC_MOUNT")
[[ $source_identity == "$mount_identity" ]] || fail "effective public bind does not match the approved source"
[[ $(nsenter --target "$feed_pid" --mount -- findmnt -n -o TARGET --target "$PUBLIC_MOUNT") == "$PUBLIC_MOUNT" ]] || fail "public path is not an effective mount point"
nsenter --target "$feed_pid" --mount -- findmnt -n -o OPTIONS --target "$PUBLIC_MOUNT" \
  | tr ',' '\n' | grep -Fxq ro || fail "effective public bind is not read-only"
if nsenter --target "$bot_pid" --mount -- \
  runuser -u "$BOT_USER" -- test -r /opt/btc09/data/wallet-mainnet.json; then
  fail "custody bot can read the general node wallet"
fi

nsenter --target "$bot_pid" --mount -- \
  runuser -u "$BOT_USER" -- "$PYTHON" - "$bot_group_id" "$lock_identity" "$blocks_identity" <<'PY'
import os
import stat
import sys

bot_group_id = int(sys.argv[1])
expected_identity = sys.argv[2]
expected_blocks_identity = sys.argv[3]
data_path = "/opt/btc09/data"
blocks_path = "/opt/btc09/data/blocks-mainnet.dat"
lock_path = "/opt/btc09/data/blocks-mainnet.dat.lock"
wallet_path = "/opt/btc09/data/wallet-mainnet.json"

for path in (data_path, blocks_path, lock_path):
    names = os.listxattr(path, follow_symlinks=False)
    if any(name.startswith("system.posix_acl_") for name in names):
        raise SystemExit("extended POSIX ACL is visible inside the bot mount")

parent = os.lstat(data_path)
if (
    not stat.S_ISDIR(parent.st_mode)
    or parent.st_uid != 0
    or stat.S_IMODE(parent.st_mode) & 0o022
):
    raise SystemExit("chain data parent metadata is unsafe inside the bot mount")

lock = os.lstat(lock_path)
if (
    not stat.S_ISREG(lock.st_mode)
    or lock.st_nlink != 1
    or lock.st_uid != 0
    or lock.st_gid != bot_group_id
    or stat.S_IMODE(lock.st_mode) != 0o660
    or f"{lock.st_dev}:{lock.st_ino}" != expected_identity
):
    raise SystemExit("chain lock metadata is unsafe inside the bot mount")
try:
    descriptor = os.open(
        lock_path,
        os.O_RDWR | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
    )
except OSError:
    raise SystemExit("chain lock is not writable inside the bot mount") from None
else:
    opened = os.fstat(descriptor)
    os.close(descriptor)
if (opened.st_dev, opened.st_ino) != (lock.st_dev, lock.st_ino):
    raise SystemExit("chain lock identity changed during open")

blocks = os.lstat(blocks_path)
if (
    not stat.S_ISREG(blocks.st_mode)
    or blocks.st_nlink != 1
    or blocks.st_uid != 0
    or blocks.st_gid != bot_group_id
    or stat.S_IMODE(blocks.st_mode) != 0o640
    or f"{blocks.st_dev}:{blocks.st_ino}" != expected_blocks_identity
):
    raise SystemExit("chain block file metadata is unsafe inside the bot mount")
descriptor = os.open(
    blocks_path,
    os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
)
os.close(descriptor)
try:
    descriptor = os.open(
        blocks_path,
        os.O_WRONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
    )
except OSError:
    pass
else:
    os.close(descriptor)
    raise SystemExit("chain block file is writable inside the bot mount")

try:
    descriptor = os.open(
        wallet_path,
        os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
    )
except OSError:
    pass
else:
    os.close(descriptor)
    raise SystemExit("general node wallet is readable inside the bot mount")
PY

nsenter --target "$feed_pid" --mount -- \
  runuser -u "$FEED_USER" -- "$PYTHON" - "$bot_pid" <<'PY'
import json
import os
import stat
import sys
import time
from pathlib import Path

bot_pid = sys.argv[1]
denied = (
    "/var/lib/btc09-otc/otc_bot.db",
    "/var/lib/btc09-otc/wallet-mainnet.json",
    "/opt/btc09/data/wallet-mainnet.json",
    "/run/credentials/btc09-otc-bot.service/otc-secrets",
    "/var/lib/btc09-otc-public",
    "/run/btc09-otc-feed/public/otc_bot.db",
    "/run/btc09-otc-feed/public/wallet-mainnet.json",
    "/run/btc09-otc-feed/public/otc-secrets",
    "/run/btc09-otc-feed/public/credentials",
    "/run/btc09-otc-feed/public/private",
    f"/proc/{bot_pid}/root/var/lib/btc09-otc/otc_bot.db",
    f"/proc/{bot_pid}/root/var/lib/btc09-otc/wallet-mainnet.json",
    f"/proc/{bot_pid}/root/opt/btc09/data/wallet-mainnet.json",
    f"/proc/{bot_pid}/root/run/credentials/btc09-otc-bot.service/otc-secrets",
)
for path in denied:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_CLOEXEC", 0))
    except OSError:
        continue
    else:
        os.close(descriptor)
        raise SystemExit(1)

bot_proc = Path("/proc") / bot_pid
if bot_proc.exists():
    raise SystemExit(1)
try:
    tuple((bot_proc / "fd").iterdir())
except OSError:
    pass
else:
    raise SystemExit(1)

public_path = Path("/run/btc09-otc-feed/public/otc-bot-feed.json")
for _attempt in range(100):
    entries = {entry.name for entry in public_path.parent.iterdir()}
    if entries == {public_path.name}:
        break
    transient = entries - {public_path.name}
    if not transient or not all(
        name.startswith(f".{public_path.name}.") and name.endswith(".tmp")
        for name in transient
    ):
        raise SystemExit(1)
    time.sleep(0.01)
else:
    raise SystemExit(1)
public_entry = public_path.lstat()
if not stat.S_ISREG(public_entry.st_mode) or public_entry.st_nlink != 1:
    raise SystemExit(1)
if stat.S_IMODE(public_path.parent.stat().st_mode) != 0o775:
    raise SystemExit(1)
if stat.S_IMODE(public_path.stat().st_mode) != 0o644:
    raise SystemExit(1)
with public_path.open("rb") as handle:
    encoded = handle.read((4 << 20) + 1)
if not encoded or len(encoded) > 4 << 20:
    raise SystemExit(1)
payload = json.loads(encoded.decode("utf-8", "strict"))
if type(payload) is not dict or payload.get("schema_version") != 1:
    raise SystemExit(1)
forbidden = {
    "user_id", "buyer_id", "seller_id", "maker_id", "actor_id",
    "wallet_addr", "deposit_addr", "destination", "signed_tx_hex",
    "detail_json", "username", "buyer_name", "seller_name",
}
pending = [payload]
while pending:
    value = pending.pop()
    if isinstance(value, dict):
        if forbidden.intersection(value):
            raise SystemExit(1)
        pending.extend(value.values())
    elif isinstance(value, list):
        pending.extend(value)
PY

printf '%s\n' "OTC service isolation check passed (distinct UID, custody denied, public feed readable)"
