#!/usr/bin/env bash
set -euo pipefail

SITE=/etc/nginx/sites-available/bitcoin09-domain-pending
ENABLED_SITE=/etc/nginx/sites-enabled/bitcoin09-domain-pending
HTTP_DEST=/etc/nginx/conf.d/bitcoin09-otc-http.conf
SERVER_DEST=/etc/nginx/snippets/bitcoin09-otc-server.conf
BACKUP_ROOT=/var/backups/btc09
INSTALL_LOCK=/run/lock/btc09-otc-nginx-install.lock
# Shared with Certbot: /etc/nginx/.certbot.lock
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTALLER=$SCRIPT_DIR/install-otc-nginx.sh
PATCH_HELPER=$SCRIPT_DIR/patch-otc-nginx-site.py
CERTBOT_LOCK_HELPER=$SCRIPT_DIR/with-certbot-nginx-lock.py
READBACK_HELPER=$SCRIPT_DIR/wait-otc-nginx-readback.py
HTTP_SOURCE=$SCRIPT_DIR/../nginx/bitcoin09-otc-http.conf
SERVER_SOURCE=$SCRIPT_DIR/../nginx/bitcoin09-otc-server.conf

fail() {
  printf '%s\n' "OTC nginx install failed: $1" >&2
  exit 1
}

[[ $# -eq 1 && $1 == --acknowledge-certbot-tls-edit ]] || \
  fail "pass --acknowledge-certbot-tls-edit"
[[ $(id -u) -eq 0 ]] || fail "must run as root"
for command in nginx systemctl python3 install cp mv rm mktemp sha256sum stat \
  realpath grep sync chmod chown dirname flock curl; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable"
done

for source in "$INSTALLER" "$CERTBOT_LOCK_HELPER"; do
  [[ -f $source && ! -L $source ]] || fail "reviewed lock source is missing or unsafe"
  [[ $(realpath -ms -- "$source") == "$(realpath -e -- "$source")" ]] || \
    fail "reviewed lock source contains a symbolic link"
  [[ $(stat -c '%u:%h' -- "$source") == 0:1 ]] || \
    fail "reviewed lock source must be root-owned and single-link"
done
[[ -x $INSTALLER && -x $CERTBOT_LOCK_HELPER ]] || \
  fail "reviewed lock source is not executable"
case ${OTC_NGINX_CERTBOT_LOCK_HELD:-0} in
  0) exec python3 "$CERTBOT_LOCK_HELPER" "$INSTALLER" "$@" ;;
  1) unset OTC_NGINX_CERTBOT_LOCK_HELD ;;
  *) fail "invalid Certbot lock state" ;;
esac

exec 9>"$INSTALL_LOCK"
flock -n 9 || fail "another OTC nginx install is running"
chmod 0600 "$INSTALL_LOCK"

for source in "$PATCH_HELPER" "$CERTBOT_LOCK_HELPER" "$READBACK_HELPER" \
  "$HTTP_SOURCE" "$SERVER_SOURCE"; do
  [[ -f $source && ! -L $source ]] || fail "reviewed source is missing or unsafe"
  [[ $(realpath -ms -- "$source") == "$(realpath -e -- "$source")" ]] || \
    fail "reviewed source contains a symbolic link"
  [[ $(stat -c '%u:%h' -- "$source") == 0:1 ]] || \
    fail "reviewed source must be root-owned and single-link"
done
[[ -x $PATCH_HELPER && -x $READBACK_HELPER ]] || \
  fail "nginx verification helper is not executable"
[[ -f $SITE && ! -L $SITE ]] || fail "Certbot TLS site is missing or unsafe"
[[ $(realpath -ms -- "$SITE") == "$SITE" ]] || fail "TLS site path is not exact"
[[ $(realpath -e -- "$SITE") == "$SITE" ]] || fail "TLS site contains a symbolic link"
[[ $(stat -c '%u:%h:%a' -- "$SITE") == 0:1:644 ]] || \
  fail "TLS site must be root-owned, single-link, mode 0644"
[[ -L $ENABLED_SITE ]] || fail "enabled TLS site is not a symbolic link"
[[ $(realpath -e -- "$ENABLED_SITE") == "$SITE" ]] || \
  fail "enabled TLS site does not resolve to the reviewed sites-available target"
for parent in /etc/nginx/sites-available /etc/nginx/sites-enabled \
  /etc/nginx/conf.d /etc/nginx/snippets; do
  [[ -d $parent && ! -L $parent ]] || fail "nginx configuration parent is unsafe"
  [[ $(realpath -ms -- "$parent") == "$(realpath -e -- "$parent")" ]] || \
    fail "nginx configuration parent contains a symbolic link"
  [[ $(stat -c '%u:%a' -- "$parent") == 0:755 ]] || \
    fail "nginx configuration parent must be root-owned mode 0755"
done
[[ -d $BACKUP_ROOT && ! -L $BACKUP_ROOT ]] || fail "backup root is missing or unsafe"
[[ $(stat -c '%u:%a' -- "$BACKUP_ROOT") == 0:700 ]] || \
  fail "backup root must be root-owned mode 0700"

entry_path() {
  case $1 in
    site) printf '%s\n' "$SITE" ;;
    http) printf '%s\n' "$HTTP_DEST" ;;
    server) printf '%s\n' "$SERVER_DEST" ;;
    *) return 1 ;;
  esac
}

safe_optional_entry() {
  local path=$1
  if [[ -e $path || -L $path ]]; then
    [[ -f $path && ! -L $path ]] || return 1
    [[ $(realpath -ms -- "$path") == "$path" ]] || return 1
    [[ $(realpath -e -- "$path") == "$path" ]] || return 1
    [[ $(stat -c '%u:%g:%a:%h' -- "$path") == 0:0:644:1 ]] || return 1
  fi
}
safe_optional_entry "$HTTP_DEST" || fail "existing http fragment is unsafe"
safe_optional_entry "$SERVER_DEST" || fail "existing server fragment is unsafe"

backup=$(mktemp -d -- "$BACKUP_ROOT/nginx-otc.XXXXXXXXXX")
chmod 0700 "$backup"
baseline=$backup/BASELINE
: > "$baseline"

snapshot_entry() {
  local name=$1 path metadata digest backup_digest current
  path=$(entry_path "$name")
  if [[ -e $path || -L $path ]]; then
    [[ -f $path && ! -L $path ]] || return 1
    metadata=$(stat -Lc '%d:%i|%u:%g:%a:%h' -- "$path") || return 1
    digest=$(sha256sum -- "$path") || return 1
    digest=${digest%% *}
    cp -p -- "$path" "$backup/$name" || return 1
    backup_digest=$(sha256sum -- "$backup/$name") || return 1
    [[ ${backup_digest%% *} == "$digest" ]] || return 1
    current=$(stat -Lc '%d:%i|%u:%g:%a:%h' -- "$path") || return 1
    [[ $current == "$metadata" ]] || return 1
    printf 'present|%s|%s|%s\n' "$name" "$metadata" "$digest" >> "$baseline"
  else
    printf 'absent|%s|||\n' "$name" >> "$baseline"
  fi
}
snapshot_entry site || fail "cannot snapshot TLS site baseline"
snapshot_entry http || fail "cannot snapshot http fragment baseline"
snapshot_entry server || fail "cannot snapshot server fragment baseline"

backup_files=(BASELINE site)
[[ -e $backup/http ]] && backup_files+=(http)
[[ -e $backup/server ]] && backup_files+=(server)
(cd -- "$backup" && sha256sum "${backup_files[@]}" > SHA256SUMS)
chmod 0600 "$backup"/*
for name in site http server; do
  [[ ! -e $backup/$name ]] || sync "$backup/$name"
done
sync "$backup/BASELINE"
sync "$backup/SHA256SUMS"
sync "$backup"
sync "$BACKUP_ROOT"

baseline_entry_matches() {
  local name=$1 line state recorded_name identity metadata digest path current current_digest
  line=$(grep -E "^(present|absent)\|$name\|" "$baseline") || return 1
  IFS='|' read -r state recorded_name identity metadata digest <<< "$line"
  [[ $recorded_name == "$name" ]] || return 1
  path=$(entry_path "$name") || return 1
  if [[ $state == present ]]; then
    [[ -f $path && ! -L $path ]] || return 1
    current=$(stat -Lc '%d:%i|%u:%g:%a:%h' -- "$path") || return 1
    [[ $current == "$identity|$metadata" ]] || return 1
    current_digest=$(sha256sum -- "$path") || return 1
    [[ ${current_digest%% *} == "$digest" ]]
  else
    [[ $state == absent && ! -e $path && ! -L $path ]]
  fi
}

baseline_matches() {
  baseline_entry_matches site && baseline_entry_matches http && \
    baseline_entry_matches server
}

backup_valid() {
  (cd -- "$backup" && sha256sum --check --strict SHA256SUMS >/dev/null) || return 1
  local name
  for name in site http server; do
    [[ $(grep -Ec "^(present|absent)\|$name\|" "$baseline") -eq 1 ]] || return 1
  done
  [[ $(grep -Ec '^(present|absent)\|(site|http|server)\|' "$baseline") -eq 3 ]] || return 1
}

restored_matches_baseline() {
  local state name _identity metadata digest path current current_digest count=0
  while IFS='|' read -r state name _identity metadata digest; do
    path=$(entry_path "$name") || return 1
    count=$((count + 1))
    if [[ $state == present ]]; then
      [[ -f $path && ! -L $path ]] || return 1
      current=$(stat -Lc '%u:%g:%a:%h' -- "$path") || return 1
      [[ $current == "$metadata" ]] || return 1
      current_digest=$(sha256sum -- "$path") || return 1
      [[ ${current_digest%% *} == "$digest" ]] || return 1
    else
      [[ $state == absent && ! -e $path && ! -L $path ]] || return 1
    fi
  done < "$baseline"
  [[ $count -eq 3 ]]
}

http_tmp=$HTTP_DEST.otc-new.$$
server_tmp=$SERVER_DEST.otc-new.$$
site_tmp=$SITE.otc-new.$$
effective_tmp=$backup/EFFECTIVE.tmp
headers_tmp=$backup/HEADERS.tmp
install -o root -g root -m 0644 -- "$HTTP_SOURCE" "$http_tmp"
install -o root -g root -m 0644 -- "$SERVER_SOURCE" "$server_tmp"
python3 "$PATCH_HELPER" "$SITE" "$site_tmp"
chown root:root "$site_tmp"
chmod 0644 "$site_tmp"

desired_http=$(sha256sum -- "$http_tmp"); desired_http=${desired_http%% *}
desired_server=$(sha256sum -- "$server_tmp"); desired_server=${desired_server%% *}
desired_site=$(sha256sum -- "$site_tmp"); desired_site=${desired_site%% *}
desired_http_identity=$(stat -Lc '%d:%i' -- "$http_tmp")
desired_server_identity=$(stat -Lc '%d:%i' -- "$server_tmp")
desired_site_identity=$(stat -Lc '%d:%i' -- "$site_tmp")
modified=0

desired_entry_matches() {
  local name=$1 desired=$2 desired_identity=$3 path current_digest current
  path=$(entry_path "$name") || return 1
  [[ -f $path && ! -L $path ]] || return 1
  current=$(stat -Lc '%d:%i|%u:%g:%a:%h' -- "$path") || return 1
  [[ $current == "$desired_identity|0:0:644:1" ]] || return 1
  current_digest=$(sha256sum -- "$path") || return 1
  [[ ${current_digest%% *} == "$desired" ]]
}

baseline_or_desired_entry_matches() {
  local name=$1 desired=$2 desired_identity=$3
  baseline_entry_matches "$name" && return 0
  desired_entry_matches "$name" "$desired" "$desired_identity"
}

installed_matches() {
  baseline_or_desired_entry_matches http "$desired_http" "$desired_http_identity" || return 1
  baseline_or_desired_entry_matches server "$desired_server" "$desired_server_identity" || return 1
  baseline_or_desired_entry_matches site "$desired_site" "$desired_site_identity" || return 1
}

restore_entry() {
  local name=$1 path temporary state
  path=$(entry_path "$name") || return 1
  state=$(grep -E "^(present|absent)\|$name\|" "$baseline") || return 1
  case $state in
    present*)
      temporary=$path.otc-rollback.$$
      install -o root -g root -m 0644 -- "$backup/$name" "$temporary" || return 1
      mv -Tf -- "$temporary" "$path" || return 1
      ;;
    absent*) rm -f -- "$path" || return 1 ;;
    *) return 1 ;;
  esac
}

sync_restored_present_files() {
  local state name _identity _metadata _digest path
  while IFS='|' read -r state name _identity _metadata _digest; do
    if [[ $state == present ]]; then
      path=$(entry_path "$name") || return 1
      sync "$path" || return 1
    elif [[ $state != absent ]]; then
      return 1
    fi
  done < "$baseline"
}

rollback() {
  backup_valid || return 1
  installed_matches || return 1
  restore_entry site || return 1
  restore_entry http || return 1
  restore_entry server || return 1
  sync_restored_present_files || return 1
  sync /etc/nginx/sites-available || return 1
  sync /etc/nginx/conf.d || return 1
  sync /etc/nginx/snippets || return 1
  restored_matches_baseline || return 1
  nginx -t || return 1
  systemctl reload nginx || return 1
}

on_exit() {
  local status=$?
  rm -f -- "$http_tmp" "$server_tmp" "$site_tmp" "$effective_tmp" "$headers_tmp"
  if ((status != 0 && modified)); then
    if ! rollback; then
      printf '%s\n' "CRITICAL: OTC nginx rollback failed; nginx was not reloaded" >&2
      exit 97
    fi
  fi
  exit "$status"
}
trap on_exit EXIT

# Revalidate the complete baseline immediately before the first replacement.
baseline_matches || fail "nginx baseline drifted before install"
modified=1
mv -Tf -- "$http_tmp" "$HTTP_DEST"
mv -Tf -- "$server_tmp" "$SERVER_DEST"
mv -Tf -- "$site_tmp" "$SITE"
installed_matches || fail "installed nginx entries failed immediate readback"
sync "$SITE" "$HTTP_DEST" "$SERVER_DEST" /etc/nginx/sites-available \
  /etc/nginx/conf.d /etc/nginx/snippets || sync

nginx -t
nginx -T > "$effective_tmp" 2>&1
[[ $(stat -c '%s' -- "$effective_tmp") -le 1048576 ]] || \
  fail "effective nginx configuration is oversized"
python3 "$PATCH_HELPER" --audit-effective "$effective_tmp"
systemctl reload nginx
python3 "$READBACK_HELPER" "$PATCH_HELPER" || \
  fail "new nginx workers did not become ready"

curl --silent --show-error --fail --max-time 10 \
  --noproxy '*' \
  --http1.1 --no-keepalive --header 'Connection: close' \
  --resolve btc09.org:443:127.0.0.1 \
  --dump-header "$headers_tmp" --output /dev/null \
  https://btc09.org/otc-bot-feed.json
python3 "$PATCH_HELPER" --audit-headers "$headers_tmp"
health_status=$(curl --silent --show-error --max-time 10 \
  --noproxy '*' \
  --http1.1 --no-keepalive --header 'Connection: close' \
  --resolve btc09.org:443:127.0.0.1 --output /dev/null \
  --write-out '%{http_code}' https://btc09.org/otc-feed-healthz)
[[ $health_status == 404 ]] || fail "public operational health did not return 404"

trap - EXIT
rm -f -- "$effective_tmp" "$headers_tmp"
printf '%s\n' "OTC nginx install passed; rollback backup retained at $backup"
