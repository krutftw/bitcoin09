#!/usr/bin/env bash
set -euo pipefail

STATE=/var/lib/btc09-otc
MAINTENANCE_DIR=/var/lib/btc09-otc-maintenance
MARKER=$MAINTENANCE_DIR/active
CONDITION='ConditionPathExists=!/var/lib/btc09-otc-maintenance/active'

fail() {
  printf '%s\n' "OTC generation install failed: $1" >&2
  exit 1
}

[[ $# -eq 2 ]] || fail "usage: install-otc-generation.sh V4_DB DEDICATED_WALLET"
[[ $(id -u) -eq 0 ]] || fail "must run as root"
for command in systemctl sqlite3 sha256sum install stat realpath sync cmp flock id; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable"
done

source_db=$1
source_wallet=$2
for source in "$source_db" "$source_wallet"; do
  [[ $source == /* && -f $source && ! -L $source ]] || fail "source must be an absolute regular non-symlink file"
  [[ $(realpath -ms -- "$source") == "$(realpath -e -- "$source")" ]] || fail "source contains a symbolic link"
  [[ $(stat -c %h -- "$source") -eq 1 ]] || fail "hard-linked sources are refused"
  [[ $(stat -c %u -- "$source") -eq 0 && $(stat -c %a -- "$source") == 600 ]] || fail "generation sources must be root-owned mode 0600"
done
[[ $(realpath -e -- "$source_db") != "$STATE/otc_bot.db" ]] || fail "database source must be staged outside active state"
[[ $(realpath -e -- "$source_wallet") != "$STATE/wallet-mainnet.json" ]] || fail "wallet source must be staged outside active state"

systemctl daemon-reload
for service in btc09-otc-bot.service btc09-otc-feed.service; do
  systemctl cat "$service" 2>/dev/null | grep -Fxq "$CONDITION" || fail "installed unit lacks the durable maintenance condition"
done

[[ $(realpath -ms -- "$MAINTENANCE_DIR") == "$(realpath -m -- "$MAINTENANCE_DIR")" ]] || fail "maintenance directory contains a symbolic link"
if [[ -e $MAINTENANCE_DIR ]]; then
  [[ -d $MAINTENANCE_DIR && ! -L $MAINTENANCE_DIR ]] || fail "maintenance path is unsafe"
  [[ $(stat -c %u -- "$MAINTENANCE_DIR") -eq 0 && $(stat -c %a -- "$MAINTENANCE_DIR") == 755 ]] || fail "maintenance directory ownership or mode is unsafe"
else
  install -d -o root -g root -m 0755 -- "$MAINTENANCE_DIR"
fi
exec 9>"$MAINTENANCE_DIR/.generation.lock"
flock -n 9 || fail "another generation install is active"
chmod 0600 "$MAINTENANCE_DIR/.generation.lock"

if [[ -e $MARKER ]]; then
  [[ -f $MARKER && ! -L $MARKER ]] || fail "maintenance marker is unsafe"
  [[ $(stat -c %u -- "$MARKER") -eq 0 && $(stat -c %a -- "$MARKER") == 600 ]] || fail "maintenance marker ownership or mode is unsafe"
else
  marker_staging="$MAINTENANCE_DIR/.active.$$"
  [[ ! -e $marker_staging ]] || fail "maintenance marker staging already exists"
  install -o root -g root -m 0600 /dev/null "$marker_staging"
  sync "$marker_staging" || sync
  mv -T -- "$marker_staging" "$MARKER"
  sync "$MAINTENANCE_DIR" || sync
fi

systemctl stop btc09-otc-feed.service btc09-otc-bot.service
systemctl is-active --quiet btc09-otc-bot.service && fail "bot service remained active"
systemctl is-active --quiet btc09-otc-feed.service && fail "feed service remained active"

[[ $(realpath -ms -- "$STATE") == "$(realpath -m -- "$STATE")" ]] || fail "state directory contains a symbolic link"
if [[ -e $STATE ]]; then
  [[ -d $STATE && ! -L $STATE ]] || fail "state path is unsafe"
else
  install -d -o btc09-otc -g btc09-otc -m 0700 -- "$STATE"
fi
state_uid=$(stat -c %u -- "$STATE")
state_gid=$(stat -c %g -- "$STATE")
bot_uid=$(id -u btc09-otc)
bot_gid=$(id -g btc09-otc)
if [[ $state_uid -eq 0 && $state_gid -eq 0 ]]; then
  [[ -f $MARKER ]] || fail "root-owned state requires an active maintenance marker"
elif [[ $state_uid -ne $bot_uid || $state_gid -ne $bot_gid ]]; then
  fail "state ownership is unsafe"
fi
[[ $(stat -c %a -- "$STATE") == 700 ]] || fail "state mode is unsafe"
chown root:root "$STATE"
chmod 0700 "$STATE"
sync "$STATE" || sync
[[ $(stat -c %d -- "$STATE") == "$(stat -c %d -- "$MAINTENANCE_DIR")" ]] || fail "maintenance and state directories must share a filesystem"
staging="$MAINTENANCE_DIR/.generation.$$"
[[ ! -e $staging ]] || fail "generation staging already exists"
mkdir -m 0700 -- "$staging"
cleanup() {
  status=$?
  if [[ -n ${staging:-} && -d ${staging:-} ]]; then
    rm -rf -- "$staging"
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

staged_db="$staging/otc_bot.db"
staged_wallet="$staging/wallet-mainnet.json"
sqlite3 "$source_db" ".backup '$staged_db'"
install -o btc09-otc -g btc09-otc -m 0600 -- "$source_wallet" "$staged_wallet"
chown btc09-otc:btc09-otc "$staged_db"
chmod 0600 "$staged_db"
[[ $(sqlite3 "$staged_db" 'PRAGMA integrity_check;') == ok ]] || fail "staged database failed integrity"
[[ -z $(sqlite3 "$staged_db" 'PRAGMA foreign_key_check;') ]] || fail "staged database failed foreign keys"
[[ $(sqlite3 "$staged_db" 'SELECT COUNT(*) FROM schema_meta WHERE id=1 AND version=4;') == 1 ]] || fail "staged database is not v4"
cmp --silent -- "$source_wallet" "$staged_wallet" || fail "staged wallet readback differs"
(
  cd "$staging"
  sha256sum otc_bot.db wallet-mainnet.json > SHA256SUMS
  chmod 0600 SHA256SUMS
  sha256sum --check --status SHA256SUMS
)
sync "$staged_db" "$staged_wallet" "$staging/SHA256SUMS" || sync
sync "$staging" || sync

rm -f -- "$STATE/otc_bot.db-wal" "$STATE/otc_bot.db-shm" \
  "$STATE/otc_bot.db-journal"
sync "$STATE" || sync
mv -T -- "$staged_db" "$STATE/otc_bot.db"
sync "$STATE/otc_bot.db" "$STATE" || sync
mv -T -- "$staged_wallet" "$STATE/wallet-mainnet.json"
sync "$STATE/wallet-mainnet.json" "$STATE" || sync
chown btc09-otc:btc09-otc "$STATE/otc_bot.db" "$STATE/wallet-mainnet.json"
chmod 0600 "$STATE/otc_bot.db" "$STATE/wallet-mainnet.json"
[[ $(sqlite3 "$STATE/otc_bot.db" 'PRAGMA integrity_check;') == ok ]] || fail "installed database failed integrity"
[[ -z $(sqlite3 "$STATE/otc_bot.db" 'PRAGMA foreign_key_check;') ]] || fail "installed database failed foreign keys"
cmp --silent -- "$source_wallet" "$STATE/wallet-mainnet.json" || fail "installed wallet differs from source generation"
rm -f -- "$STATE/otc_bot.db-wal" "$STATE/otc_bot.db-shm" \
  "$STATE/otc_bot.db-journal"
chown btc09-otc:btc09-otc "$STATE"
chmod 0700 "$STATE"
sync "$STATE/otc_bot.db" "$STATE/wallet-mainnet.json" "$STATE" || sync

rm -f -- "$MARKER"
sync "$MAINTENANCE_DIR" || sync
rm -rf -- "$staging"
staging=
trap - EXIT HUP INT TERM
printf '%s\n' "OTC generation installed and verified; maintenance gate cleared; services remain stopped"
