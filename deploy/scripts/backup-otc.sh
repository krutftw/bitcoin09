#!/usr/bin/env bash
set -euo pipefail

BACKUP_ROOT=/var/backups/btc09
STATE=/var/lib/btc09-otc
MAINTENANCE_DIR=/var/lib/btc09-otc-maintenance
MARKER=$MAINTENANCE_DIR/active
CONDITION='ConditionPathExists=!/var/lib/btc09-otc-maintenance/active'
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
SNAPSHOT_HELPER=$SCRIPT_DIR/snapshot-otc-backup.py

fail() {
  printf '%s\n' "OTC backup failed: $1" >&2
  exit 1
}

[[ $# -eq 3 ]] || fail "usage: backup-otc.sh DB WALLET DESTINATION"
[[ $(id -u) -eq 0 ]] || fail "must run as root"

db=$1
wallet=$2
destination=$3

for command in realpath stat sqlite3 sha256sum install flock sync python3 dirname chown chmod; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable"
done
[[ -f $SNAPSHOT_HELPER && ! -L $SNAPSHOT_HELPER ]] || fail "backup snapshot helper is unsafe"

[[ $db == /* && $wallet == /* && $destination == /* ]] || fail "all paths must be absolute"
[[ $db != *$'\n'* && $wallet != *$'\n'* && $destination != *$'\n'* ]] || fail "paths contain invalid characters"
[[ $db =~ ^[A-Za-z0-9_./-]+$ && $wallet =~ ^[A-Za-z0-9_./-]+$ && $destination =~ ^[A-Za-z0-9_./-]+$ ]] || fail "paths contain invalid characters"

active_sources=0
if [[ $db == "$STATE/otc_bot.db" || $wallet == "$STATE/wallet-mainnet.json" ]]; then
  [[ $db == "$STATE/otc_bot.db" && $wallet == "$STATE/wallet-mainnet.json" ]] || fail "active database and wallet must be backed up as one pair"
  active_sources=1
fi

if [[ $active_sources -eq 1 ]]; then
  [[ -d $MAINTENANCE_DIR && ! -L $MAINTENANCE_DIR ]] || fail "maintenance directory is unsafe"
  [[ $(realpath -ms -- "$MAINTENANCE_DIR") == "$(realpath -e -- "$MAINTENANCE_DIR")" ]] || fail "maintenance directory contains a symbolic link"
  [[ $(stat -c '%u:%g:%a' -- "$MAINTENANCE_DIR") == 0:0:755 ]] || fail "maintenance directory ownership or mode is unsafe"
  exec 8>"$MAINTENANCE_DIR/.generation.lock"
  flock -n 8 || fail "another generation install or active backup is running"
  chmod 0600 "$MAINTENANCE_DIR/.generation.lock"
fi

logical_destination=$(realpath -ms -- "$destination")
physical_destination=$(realpath -m -- "$destination")
[[ $logical_destination == "$physical_destination" ]] || fail "destination contains a symbolic link"
case "$physical_destination" in
  "$BACKUP_ROOT" | "$BACKUP_ROOT"/*) ;;
  *) fail "destination must remain under $BACKUP_ROOT" ;;
esac

logical_root=$(realpath -ms -- "$BACKUP_ROOT")
physical_root=$(realpath -m -- "$BACKUP_ROOT")
[[ $logical_root == "$physical_root" && ! -L $BACKUP_ROOT ]] || fail "backup root contains a symbolic link"
if [[ -e $BACKUP_ROOT ]]; then
  [[ -d $BACKUP_ROOT ]] || fail "backup root must be a directory"
else
  install -d -o root -g root -m 0700 -- "$BACKUP_ROOT"
fi
[[ $(stat -c %u -- "$BACKUP_ROOT") -eq 0 ]] || fail "backup root must be owned by root"
[[ $(stat -c %a -- "$BACKUP_ROOT") == 700 ]] || fail "backup root must have mode 0700"

if [[ -e $physical_destination ]]; then
  [[ -d $physical_destination && ! -L $physical_destination ]] || fail "destination must be a non-symlink directory"
else
  install -d -o root -g root -m 0700 -- "$physical_destination"
fi
[[ $(stat -c %u -- "$physical_destination") -eq 0 ]] || fail "destination must be owned by root"
destination_mode=$(stat -c %a -- "$physical_destination")
(( (8#$destination_mode & 8#077) == 0 )) || fail "destination permissions are unsafe"

exec 9>"$physical_destination/.backup.lock"
flock -x 9
chmod 0600 "$physical_destination/.backup.lock"

marker_created=0
if [[ $active_sources -eq 1 ]]; then
  for command in systemctl pgrep grep mv; do
    command -v "$command" >/dev/null 2>&1 || fail "required active-state command is unavailable"
  done
  systemctl daemon-reload
  for service in btc09-otc-bot.service btc09-otc-feed.service; do
    systemctl cat "$service" 2>/dev/null | grep -Fxq "$CONDITION" || fail "installed unit lacks the durable maintenance condition"
  done
  if [[ -e $MARKER ]]; then
    [[ -f $MARKER && ! -L $MARKER && $(stat -c '%u:%a' -- "$MARKER") == 0:600 ]] || fail "maintenance marker is unsafe"
  else
    marker_staging="$MAINTENANCE_DIR/.active.backup.$$"
    [[ ! -e $marker_staging ]] || fail "maintenance marker staging already exists"
    install -o root -g root -m 0600 /dev/null "$marker_staging"
    sync "$marker_staging" || sync
    mv -T -- "$marker_staging" "$MARKER"
    sync "$MAINTENANCE_DIR" || sync
    marker_created=1
  fi
  systemctl stop btc09-otc-feed.service btc09-otc-bot.service
  systemctl is-active --quiet btc09-otc-bot.service && fail "bot service remained active"
  systemctl is-active --quiet btc09-otc-feed.service && fail "feed service remained active"
  for service in btc09-otc-bot.service btc09-otc-feed.service; do
    control_group=$(systemctl show -p ControlGroup --value "$service")
    [[ -z $control_group || $control_group == /* ]] || fail "service cgroup evidence is invalid"
    cgroup_file=/sys/fs/cgroup$control_group/cgroup.procs
    if [[ -n $control_group && -r $cgroup_file ]] && \
      grep -Eq '^[0-9]+$' "$cgroup_file"; then
      fail "service cgroup still contains processes"
    fi
  done
  bot_uid=$(id -u btc09-otc)
  if pgrep -u "$bot_uid" >/dev/null 2>&1; then
    fail "custody service user still has running processes"
  fi
  [[ -d $STATE && ! -L $STATE ]] || fail "active state directory is unsafe"
  [[ $(realpath -ms -- "$STATE") == "$(realpath -e -- "$STATE")" ]] || fail "active state path contains a symbolic link"
  [[ $(stat -c %a -- "$STATE") == 700 ]] || fail "active state mode is unsafe"
  chown root:root "$STATE"
  chmod 0700 "$STATE"
  for source in "$db" "$wallet"; do
    [[ -f $source && ! -L $source ]] || fail "active source must be a regular non-symlink file"
    [[ $(stat -c %h -- "$source") -eq 1 ]] || fail "hard-linked active sources are refused"
    [[ $(stat -c %a -- "$source") == 600 ]] || fail "active source mode is unsafe"
    chown root:root "$source"
    chmod 0600 "$source"
  done
  sync "$db" "$wallet" "$STATE" || sync
fi

validate_root_controlled_source() {
  source=$1
  [[ -f $source && ! -L $source ]] || fail "source must be a regular non-symlink file"
  [[ $(realpath -ms -- "$source") == "$(realpath -e -- "$source")" ]] || fail "source contains a symbolic link"
  [[ $(stat -c %h -- "$source") -eq 1 ]] || fail "hard-linked sources are refused"
  [[ $(stat -c '%u:%a' -- "$source") == 0:600 ]] || fail "source must be root-owned mode 0600"
  parent=$(dirname -- "$source")
  [[ -d $parent && ! -L $parent ]] || fail "source parent is unsafe"
  [[ $(realpath -ms -- "$parent") == "$(realpath -e -- "$parent")" ]] || fail "source parent contains a symbolic link"
  current=$parent
  while :; do
    [[ $(stat -c %u -- "$current") -eq 0 ]] || fail "source ancestry must be root-owned"
    current_mode=$(stat -c %a -- "$current")
    (( (8#$current_mode & 8#022) == 0 )) || fail "source ancestry is writable by a non-root identity"
    [[ $current == / ]] && break
    current=$(dirname -- "$current")
  done
}

validate_root_controlled_source "$db"
validate_root_controlled_source "$wallet"

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
name="otc-$timestamp-$$"
staging="$physical_destination/.$name.incomplete"
final="$physical_destination/$name"
[[ ! -e $staging && ! -e $final ]] || fail "backup destination already exists"
mkdir -m 0700 -- "$staging"

cleanup() {
  status=$?
  if [[ -n ${staging:-} && -d ${staging:-} && ! -L ${staging:-} ]]; then
    rm -rf --one-file-system -- "$staging"
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

python3 "$SNAPSHOT_HELPER" "$db" "$wallet" "$staging"
backup_db="$staging/otc_bot.db"
backup_wallet="$staging/wallet-mainnet.json"
[[ $(sqlite3 "$backup_db" 'PRAGMA integrity_check;') == ok ]] || fail "database backup failed integrity check"
(
  cd "$staging"
  sha256sum otc_bot.db wallet-mainnet.json > manifest.sha256.tmp
  chmod 0600 manifest.sha256.tmp
  mv -- manifest.sha256.tmp manifest.sha256
  sha256sum --check --status manifest.sha256
)

sync "$backup_db" "$backup_wallet" "$staging/manifest.sha256" || sync
mv -T -n -- "$staging" "$final"
[[ ! -e $staging && -d $final ]] || fail "atomic backup publish failed"
staging=
sync "$physical_destination" || sync

if [[ $active_sources -eq 1 ]]; then
  chown btc09-otc:btc09-otc "$db" "$wallet"
  chmod 0600 "$db" "$wallet"
  chown btc09-otc:btc09-otc "$STATE"
  chmod 0700 "$STATE"
  sync "$db" "$wallet" "$STATE" || sync
  if [[ $marker_created -eq 1 ]]; then
    rm -f -- "$MARKER"
    sync "$MAINTENANCE_DIR" || sync
  fi
fi

trap - EXIT HUP INT TERM
printf 'OTC backup complete: %s\n' "$final"
