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
server_target=/etc/nginx/snippets/bitcoin09-open-miner-server.conf
site_target=/etc/nginx/sites-available/bitcoin09-domain-pending
include_line='    include /etc/nginx/snippets/bitcoin09-open-miner-server.conf;'
backup_dir=$(mktemp -d /tmp/btc09-open-miner.XXXXXX)
installed=0

for source in "$http_source" "$server_source"; do
    [[ -f "$source" ]] || { echo "missing deployment file: $source" >&2; exit 1; }
done
[[ -f "$site_target" && ! -L "$site_target" ]] || { echo "canonical nginx site is missing or unsafe" >&2; exit 1; }

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
        restore_file "$site_target" site
        nginx -t && systemctl reload nginx || echo "CRITICAL: open miner nginx rollback failed" >&2
    fi
    rm -rf -- "$backup_dir"
    exit "$status"
}
trap restore_nginx ERR INT TERM

health_address=${BTC09_MINER_HEALTH_ADDRESS:-}
[[ -n "$health_address" ]] || { echo "set BTC09_MINER_HEALTH_ADDRESS to a valid mainnet address" >&2; exit 1; }
health_json="{\"address\":\"$health_address\",\"worker\":\"deploy-check\"}"
curl --fail --silent --show-error -H 'Content-Type: application/json' \
    --data "$health_json" http://127.0.0.1:9010/api/v1/work >/dev/null

backup_file "$http_target" http
backup_file "$server_target" server
backup_file "$site_target" site
installed=1

install -m 0644 "$http_source" "$http_target"
install -m 0644 "$server_source" "$server_target"
if ! grep -Fqx "$include_line" "$site_target"; then
    python3 - "$site_target" "$include_line" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
include = sys.argv[2]
anchor = "    include /etc/nginx/snippets/bitcoin09-wallet-gateway-server.conf;"
text = path.read_text(encoding="ascii")
if text.count(anchor) != 1 or include in text:
    raise SystemExit("canonical wallet include anchor is missing or ambiguous")
path.write_text(text.replace(anchor, anchor + "\n" + include), encoding="ascii")
PY
fi
[[ $(grep -Fxc "$include_line" "$site_target") -eq 1 ]]

nginx -t
systemctl reload nginx
curl --fail --silent --show-error --resolve btc09.org:443:127.0.0.1 \
    -H 'Content-Type: application/json' --data "$health_json" \
    https://btc09.org/api/v1/work >/dev/null

trap - ERR INT TERM
rm -rf -- "$backup_dir"
echo "open miner nginx route installed"
