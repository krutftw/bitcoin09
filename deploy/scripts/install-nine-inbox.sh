#!/usr/bin/env bash
set -Eeuo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "run as root" >&2
    exit 1
fi

repo_root=${1:-/opt/btc09/bitcoin09}
binary_source=${BTC09_NINE_BINARY_SOURCE:-}
http_source="$repo_root/deploy/nginx/bitcoin09-nine-inbox-http.conf"
server_source="$repo_root/deploy/nginx/bitcoin09-nine-inbox-server.conf"
service_source="$repo_root/deploy/systemd/btc09-nine-inbox.service"
http_target=/etc/nginx/conf.d/bitcoin09-nine-inbox-http.conf
server_target=/etc/nginx/snippets/bitcoin09-nine-inbox-server.conf
service_target=/etc/systemd/system/btc09-nine-inbox.service
binary_target=/opt/btc09/btc09-nine-inbox
site_target=/etc/nginx/sites-available/bitcoin09-domain-pending
include_line='    include /etc/nginx/snippets/bitcoin09-nine-inbox-server.conf;'
backup_dir=$(mktemp -d /tmp/btc09-nine-inbox.XXXXXX)
staged_binary=$(mktemp /opt/btc09/.btc09-nine-inbox.XXXXXX)
installed=0
was_active=0
was_enabled=0

for source in "$http_source" "$server_source" "$service_source"; do
    [[ -f "$source" && ! -L "$source" ]] || { echo "missing or unsafe deployment file: $source" >&2; exit 1; }
done
[[ -d "$repo_root" && ! -L "$repo_root" ]] || { echo "repository root is missing or unsafe" >&2; exit 1; }
[[ -f "$site_target" && ! -L "$site_target" ]] || { echo "canonical nginx site is missing or unsafe" >&2; exit 1; }
[[ -d /opt/btc09 && ! -L /opt/btc09 ]] || { echo "/opt/btc09 is missing or unsafe" >&2; exit 1; }
if [[ -n "$binary_source" ]]; then
    [[ -f "$binary_source" && ! -L "$binary_source" ]] || { echo "prebuilt binary is missing or unsafe" >&2; exit 1; }
    [[ ${BTC09_NINE_BINARY_SHA256:-} =~ ^[0-9a-f]{64}$ ]] || { echo "set BTC09_NINE_BINARY_SHA256 for the prebuilt binary" >&2; exit 1; }
    printf '%s  %s\n' "$BTC09_NINE_BINARY_SHA256" "$binary_source" | sha256sum --check --strict -
fi

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
    rm -f -- "$staged_binary"
    if [[ $installed -eq 1 ]]; then
        systemctl stop btc09-nine-inbox 2>/dev/null || true
        restore_file "$http_target" http
        restore_file "$server_target" server
        restore_file "$service_target" service
        restore_file "$binary_target" binary
        restore_file "$site_target" site
        systemctl daemon-reload || true
        if [[ $was_enabled -eq 0 ]]; then
            systemctl disable btc09-nine-inbox 2>/dev/null || true
        fi
        if [[ $was_active -eq 1 ]]; then
            systemctl start btc09-nine-inbox || true
        fi
        nginx -t && systemctl reload nginx || echo "CRITICAL: Nine Inbox rollback failed" >&2
    fi
    rm -rf -- "$backup_dir"
    exit "$status"
}
trap restore_install ERR INT TERM

if systemctl is-active --quiet btc09-nine-inbox 2>/dev/null; then
    was_active=1
fi
if systemctl is-enabled --quiet btc09-nine-inbox 2>/dev/null; then
    was_enabled=1
fi

if [[ -n "$binary_source" ]]; then
    install -o root -g root -m 0755 "$binary_source" "$staged_binary"
else
    cd "$repo_root"
    go test ./nineinbox ./cmd/btc09 -count=1
    go build -trimpath -o "$staged_binary" ./cmd/btc09
fi
chmod 0755 "$staged_binary"
chown root:root "$staged_binary"
"$staged_binary" version >/dev/null

getent group btc09-nine-inbox >/dev/null || groupadd --system btc09-nine-inbox
id -u btc09-nine-inbox >/dev/null 2>&1 || \
    useradd --system --gid btc09-nine-inbox --home-dir /nonexistent \
        --shell /usr/sbin/nologin btc09-nine-inbox
install -d -o btc09-nine-inbox -g btc09-nine-inbox -m 0700 /var/lib/btc09-nine-inbox

backup_file "$http_target" http
backup_file "$server_target" server
backup_file "$service_target" service
backup_file "$binary_target" binary
backup_file "$site_target" site
installed=1

install -o root -g root -m 0755 "$staged_binary" "$binary_target"
rm -f -- "$staged_binary"
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
    "    include /etc/nginx/snippets/bitcoin09-open-miner-server.conf;",
    "    include /etc/nginx/snippets/bitcoin09-wallet-gateway-server.conf;",
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
systemctl enable --now btc09-nine-inbox
systemctl restart btc09-nine-inbox
systemctl reload nginx

ready=0
for _attempt in {1..20}; do
    if curl --fail --silent --show-error http://127.0.0.1:8020/healthz >/dev/null; then
        ready=1
        break
    fi
    sleep 1
done
[[ $ready -eq 1 ]]

page=$(mktemp "$backup_dir/page.XXXXXX")
curl --fail --silent --show-error --resolve btc09.org:443:127.0.0.1 \
    https://btc09.org/inbox/ >"$page"
grep -Fq 'Send yourself anything.' "$page"
systemctl is-active --quiet btc09-nine-inbox
ss -ltn | grep -Fq '127.0.0.1:8020'
if ss -ltn | grep -Eq '0\.0\.0\.0:8020|\[::\]:8020'; then
    echo "Nine Inbox unexpectedly has a public listener" >&2
    exit 1
fi

trap - ERR INT TERM
rm -rf -- "$backup_dir"
echo "Nine Inbox service and public route installed"
