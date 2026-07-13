#!/usr/bin/env bash
set -Eeuo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "run as root" >&2
    exit 1
fi

repo_root=${1:-/opt/btc09/bitcoin09}
http_source="$repo_root/deploy/nginx/bitcoin09-open-miner-http.conf"
server_source="$repo_root/deploy/nginx/bitcoin09-open-miner-server.conf"
http_target=/etc/nginx/conf.d/bitcoin09-open-miner-http.conf
server_target=/etc/nginx/sites-available/bitcoin09-open-miner
enabled_target=/etc/nginx/sites-enabled/bitcoin09-open-miner
backup_dir=$(mktemp -d /tmp/btc09-open-miner.XXXXXX)
installed=0

for source in "$http_source" "$server_source"; do
    [[ -f "$source" ]] || { echo "missing deployment file: $source" >&2; exit 1; }
done

backup_file() {
    local target=$1 name=$2
    if [[ -e "$target" || -L "$target" ]]; then
        cp -a -- "$target" "$backup_dir/$name"
    else
        : > "$backup_dir/$name.absent"
    fi
}

restore_file() {
    local target=$1 name=$2
    rm -f -- "$target"
    if [[ -e "$backup_dir/$name" || -L "$backup_dir/$name" ]]; then
        cp -a -- "$backup_dir/$name" "$target"
    fi
}

restore_nginx() {
    local status=$?
    if [[ $installed -eq 1 ]]; then
        restore_file "$http_target" http
        restore_file "$server_target" server
        restore_file "$enabled_target" enabled
        nginx -t && systemctl reload nginx || echo "CRITICAL: open miner nginx rollback failed" >&2
    fi
    rm -rf -- "$backup_dir"
    exit "$status"
}
trap restore_nginx ERR INT TERM

# A valid work request is the strongest loopback health check. The installer
# never stores this public address and the issued template has no custody role.
health_address=${BTC09_MINER_HEALTH_ADDRESS:-}
[[ -n "$health_address" ]] || { echo "set BTC09_MINER_HEALTH_ADDRESS to a valid mainnet address" >&2; exit 1; }
curl --fail --silent --show-error \
    -H 'Content-Type: application/json' \
    --data "{\"address\":\"$health_address\",\"worker\":\"deploy-check\"}" \
    http://127.0.0.1:9010/api/v1/work >/dev/null

backup_file "$http_target" http
backup_file "$server_target" server
backup_file "$enabled_target" enabled
installed=1

install -m 0644 "$http_source" "$http_target"
install -m 0644 "$server_source" "$server_target"
ln -sfn "$server_target" "$enabled_target"
nginx -t
systemctl reload nginx
curl --fail --silent --show-error -o /dev/null -H 'Host: mine.btc09.org' http://127.0.0.1/

trap - ERR INT TERM
rm -rf -- "$backup_dir"
echo "open miner nginx route installed"
