#!/usr/bin/env bash
set -Eeuo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "run as root" >&2
    exit 1
fi

repo_root=${1:-/opt/btc09/bitcoin09}
http_source="$repo_root/deploy/nginx/bitcoin09-support-funding-http.conf"
server_source="$repo_root/deploy/nginx/bitcoin09-support-funding-server.conf"
service_source="$repo_root/deploy/systemd/btc09-support-funding.service"
app_source="$repo_root/tools/support/funding-service.mjs"
env_target=/etc/btc09/support-funding.env
claim_env_target=/etc/btc09/support-claims.env
http_target=/etc/nginx/conf.d/bitcoin09-support-funding-http.conf
server_target=/etc/nginx/snippets/bitcoin09-support-funding-server.conf
service_target=/etc/systemd/system/btc09-support-funding.service
site_target=/etc/nginx/sites-available/bitcoin09-domain-pending
include_line='    include /etc/nginx/snippets/bitcoin09-support-funding-server.conf;'
backup_dir=$(mktemp -d /tmp/btc09-support-funding.XXXXXX)
installed=0
was_active=0
was_enabled=0

for source in "$http_source" "$server_source" "$service_source" "$app_source"; do
    [[ -f "$source" && ! -L "$source" ]] || { echo "missing or unsafe deployment file: $source" >&2; exit 1; }
done
[[ -f "$env_target" && ! -L "$env_target" ]] || { echo "missing or unsafe credential file: $env_target" >&2; exit 1; }
[[ -f "$claim_env_target" && ! -L "$claim_env_target" ]] || { echo "missing or unsafe credential file: $claim_env_target" >&2; exit 1; }
[[ -f "$site_target" && ! -L "$site_target" ]] || { echo "canonical nginx site is missing or unsafe" >&2; exit 1; }
[[ $(stat -c '%a' "$env_target") == 600 ]] || { echo "$env_target must have mode 0600" >&2; exit 1; }
[[ $(stat -c '%a' "$claim_env_target") == 600 ]] || { echo "$claim_env_target must have mode 0600" >&2; exit 1; }
grep -Eq '^NOWPAYMENTS_API_KEY=.+$' "$env_target" || { echo "NOWPAYMENTS_API_KEY is missing" >&2; exit 1; }
grep -Eq '^BTC09_SUPPORT_CLAIM_SECRET=.{32,}$' "$claim_env_target" || { echo "BTC09_SUPPORT_CLAIM_SECRET is missing or too short" >&2; exit 1; }

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

restore_install() {
    local status=$?
    trap - ERR INT TERM
    if [[ $installed -eq 1 ]]; then
        systemctl stop btc09-support-funding 2>/dev/null || true
        restore_file "$http_target" http
        restore_file "$server_target" server
        restore_file "$service_target" service
        restore_file "$site_target" site
        systemctl daemon-reload || true
        if [[ $was_enabled -eq 0 ]]; then
            systemctl disable btc09-support-funding 2>/dev/null || true
        fi
        if [[ $was_active -eq 1 ]]; then
            systemctl start btc09-support-funding || true
        fi
        nginx -t && systemctl reload nginx || echo "CRITICAL: funding tracker rollback failed" >&2
    fi
    rm -rf -- "$backup_dir"
    exit "$status"
}
trap restore_install ERR INT TERM

if systemctl is-active --quiet btc09-support-funding 2>/dev/null; then
    was_active=1
fi
if systemctl is-enabled --quiet btc09-support-funding 2>/dev/null; then
    was_enabled=1
fi

node --check "$app_source"
getent group btc09-support >/dev/null || groupadd --system btc09-support
id -u btc09-support >/dev/null 2>&1 || \
    useradd --system --gid btc09-support --home-dir /nonexistent --shell /usr/sbin/nologin btc09-support
install -d -o btc09-support -g btc09-support -m 0700 /var/lib/btc09-support

backup_file "$http_target" http
backup_file "$server_target" server
backup_file "$service_target" service
backup_file "$site_target" site
installed=1

install -o root -g root -m 0644 "$http_source" "$http_target"
install -o root -g root -m 0644 "$server_source" "$server_target"
install -o root -g root -m 0644 "$service_source" "$service_target"

if ! grep -Fqx "$include_line" "$site_target"; then
    python3 - "$site_target" "$include_line" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
include = sys.argv[2]
text = path.read_text(encoding="ascii")
anchors = (
    "    include /etc/nginx/snippets/bitcoin09-nine-inbox-server.conf;",
    "    include /etc/nginx/snippets/bitcoin09-open-miner-server.conf;",
)
matches = [anchor for anchor in anchors if text.count(anchor) == 1]
if not matches or include in text:
    raise SystemExit("canonical nginx include anchor is missing or ambiguous")
anchor = matches[0]
path.write_text(text.replace(anchor, anchor + "\n" + include), encoding="ascii")
PY
fi
[[ $(grep -Fxc "$include_line" "$site_target") -eq 1 ]]

systemd-analyze verify "$service_target"
nginx -t
systemctl daemon-reload
systemctl enable --now btc09-support-funding
systemctl restart btc09-support-funding
systemctl reload nginx

ready=0
for _attempt in {1..20}; do
    if curl --fail --silent --show-error http://127.0.0.1:8032/healthz >/dev/null && \
       curl --fail --silent --show-error --resolve btc09.org:443:127.0.0.1 \
           https://btc09.org/api/support/v1/status | grep -Fq '"provider":"NOWPayments"'; then
        ready=1
        break
    fi
    sleep 1
done
[[ $ready -eq 1 ]]
[[ $(curl --silent --output /dev/null --write-out '%{http_code}' \
    --request POST http://127.0.0.1:8032/internal/support/v1/claims) == 403 ]]
systemctl is-active --quiet btc09-support-funding
ss -ltn | grep -Fq '127.0.0.1:8032'
if ss -ltn | grep -Eq '0\.0\.0\.0:8032|\[::\]:8032'; then
    echo "funding tracker unexpectedly has a public listener" >&2
    exit 1
fi

trap - ERR INT TERM
rm -rf -- "$backup_dir"
echo "BTC09 funding tracker installed"
