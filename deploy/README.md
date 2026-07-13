# Bitcoin 09 production deployment

These commands are for Ubuntu and must be run as root unless shown with
`runuser`. Keep order intake disabled through install, migration, restart
testing, and the separate controlled-pilot gate.

## Nine Inbox

Nine Inbox runs as a separate unprivileged service. It stores only encrypted
item envelopes and listens on numeric loopback; nginx is the only public
boundary. The installer builds a dedicated binary, installs the hardened unit
and bounded nginx routes, validates both configurations, then checks loopback
health and the public TLS page.

```bash
/opt/btc09/bitcoin09/deploy/scripts/install-nine-inbox.sh \
  /opt/btc09/bitcoin09
systemctl status btc09-nine-inbox --no-pager
curl -fsS http://127.0.0.1:8020/healthz
```

The canonical nginx host must already restore Cloudflare client addresses from
the reviewed official ranges. The Nine Inbox rate zones deliberately use that
restored `$binary_remote_addr`; the app does not trust arbitrary forwarding
headers. Port 8020 remains closed at the cloud firewall and binds only to
`127.0.0.1`.

For a production host without a Go compiler, build from a clean reviewed
checkout and pass the verified artifact explicitly:

```bash
BTC09_NINE_BINARY_SOURCE=/secure/staging/btc09-linux-amd64 \
BTC09_NINE_BINARY_SHA256=REVIEWED_SHA256 \
  /opt/btc09/bitcoin09/deploy/scripts/install-nine-inbox.sh \
  /opt/btc09/bitcoin09
```

The installer verifies the supplied SHA-256 before it changes the service,
binary, or nginx configuration.

## 1. Install dependencies and separate service accounts

```bash
apt-get update
apt-get install -y python3 python3-venv sqlite3 nginx curl ca-certificates shellcheck util-linux
getent group btc09-otc >/dev/null || groupadd --system btc09-otc
id -u btc09-otc >/dev/null 2>&1 || \
  useradd --system --gid btc09-otc --home-dir /var/lib/btc09-otc \
    --shell /usr/sbin/nologin btc09-otc
getent group btc09-otc-feed >/dev/null || groupadd --system btc09-otc-feed
id -u btc09-otc-feed >/dev/null 2>&1 || \
  useradd --system --gid btc09-otc-feed --home-dir /nonexistent \
    --shell /usr/sbin/nologin btc09-otc-feed
install -d -o root -g root -m 0755 /opt/btc09
install -d -o btc09-otc -g btc09-otc -m 0700 /var/lib/btc09-otc
test -d /var/lib -a ! -L /var/lib
if test -e /var/lib/btc09-otc-public; then
  test -d /var/lib/btc09-otc-public -a ! -L /var/lib/btc09-otc-public
  test "$(stat -c '%U:%G:%a' /var/lib/btc09-otc-public)" = root:btc09-otc:775
else
  install -d -o root -g btc09-otc -m 0775 /var/lib/btc09-otc-public
fi
install -d -o btc09-otc -g btc09-otc -m 0700 /var/lib/btc09-otc-staging
install -d -o root -g root -m 0755 /var/lib/btc09-otc-maintenance
install -d -o root -g root -m 0700 /var/lib/btc09-otc-maintenance/sources
install -d -o root -g root -m 0700 /var/backups/btc09 /etc/btc09
python3 -m venv /opt/btc09/venv
/opt/btc09/venv/bin/pip install --no-deps --only-binary=:all: \
  -r /opt/btc09/bitcoin09/bot/requirements.lock
/opt/btc09/venv/bin/pip check
/opt/btc09/venv/bin/python \
  /opt/btc09/bitcoin09/deploy/scripts/verify-otc-python-lock.py \
  /opt/btc09/bitcoin09/bot/requirements.lock
```

`requirements.lock` records the exact complete dependency set read back from
the Ubuntu OTC venv after canonical package-name normalization and exclusion of
only the bootstrap tools `pip`, `setuptools`, and `wheel`. It is exact-version
locked, not hash-pinned. The verifier fails on every missing, unexpected,
duplicate, non-exact, or version-mismatched runtime distribution. The
`--no-deps` install prevents the resolver from silently choosing a different
transitive version. Verify release provenance separately and verify the
checkout, lock, and venv are root-owned and not writable by `btc09-otc`.

The feed account is a distinct UID and group with no membership in
`btc09-otc`. The state parent stays mode 0700. The sanitized public source is a
stable entry directly below root-owned `/var/lib`, owned `root:btc09-otc` mode
0775. The bot can replace files inside it but cannot replace or chmod the
directory entry. systemd bind-mounts that directory read-only into the feed
service's private runtime directory. A directory bind is required so atomic
feed-file replacements remain visible.

Install the allowlisted credential source without printing it. Its only valid
keys are `BOT_TOKEN` or `DISCORD_BOT_TOKEN`, `DISCORD_GUILD_ID`, `ADMIN_IDS`,
`TRANSLATION_API_URL`, `TRANSLATION_API_TOKEN`, and
`OTC_ADMIN_FEE_DESTINATION`. Safety configuration such as paths, network, fee,
or intake state must never appear here:

```bash
install -o root -g root -m 0600 /secure/staging/otc-secrets.env \
  /etc/btc09/otc-secrets.env
stat -c '%U:%G %a %n' /etc/btc09/otc-secrets.env
cd /opt/btc09/bitcoin09
/opt/btc09/venv/bin/python -c \
  'from bot.btc09_otc_bot import load_otc_secrets; load_otc_secrets("/etc/btc09/otc-secrets.env")'
```

Expected: `root:root 600` and no parser output. Migrate only the allowlisted
values from prior secret storage. Do not delete a shared legacy source until
every other service that uses it has been migrated separately; the OTC unit no
longer reads it. The service manager can read the credential source; the
service user cannot. `LoadCredential=` exposes only a private runtime copy.
The loader binds its narrow mode-0440 exception to systemd's root-owned,
per-unit credential mount below `/run/credentials`; a supplied environment
path alone cannot make another group-readable file acceptable.
Keep all safety-critical configuration pinned in the reviewed unit.

## 2. Install the fail-closed units before replacing any state

The new units and their durable maintenance condition must be installed and
daemon-reloaded before initial migration. Otherwise a reboot could start an old
unit while only half of a database/wallet generation is present.

```bash
systemctl stop btc09-otc-feed btc09-otc-bot 2>/dev/null || true
systemctl disable btc09-otc-feed btc09-otc-bot 2>/dev/null || true
test ! -e /var/lib/btc09-otc-maintenance/active
install -o root -g root -m 0600 /dev/null \
  /var/lib/btc09-otc-maintenance/active
sync /var/lib/btc09-otc-maintenance/active \
  /var/lib/btc09-otc-maintenance
: "${REVIEWED_BTC09_SHA256:?set this from the reviewed binary manifest}"
[[ $REVIEWED_BTC09_SHA256 =~ ^[0-9a-f]{64}$ ]]
printf '%s  %s\n' "$REVIEWED_BTC09_SHA256" /opt/btc09/btc09 | \
  sha256sum --check --strict -
tip_before=$(curl -fsS http://127.0.0.1:8009/api/v1/tip | \
  python3 -c 'import json,sys; p=json.load(sys.stdin); assert p["schema_version"]==1 and p["network"]=="btc09-mainnet"; print(p["tip"]["height"])')
[[ $tip_before =~ ^[0-9]+$ ]]
systemctl stop btc09-seed
! systemctl is-active --quiet btc09-seed
[[ $(systemctl show -p MainPID --value btc09-seed) == 0 ]]
chain_data=/opt/btc09/data
chain_blocks=/opt/btc09/data/blocks-mainnet.dat
chain_lock=/opt/btc09/data/blocks-mainnet.dat.lock
general_wallet=/opt/btc09/data/wallet-mainnet.json
python3 - "$chain_data" "$chain_blocks" "$chain_lock" "$general_wallet" <<'PY'
import os
import sys

for path in sys.argv[1:]:
    names = os.listxattr(path, follow_symlinks=False)
    if any(name.startswith("system.posix_acl_") for name in names):
        raise SystemExit(f"extended POSIX ACL is forbidden: {path}")
PY
test -d "$chain_data" -a ! -L "$chain_data"
test "$(realpath -ms -- "$chain_data")" = "$(realpath -e -- "$chain_data")"
test "$(stat -c %U -- "$chain_data")" = root
chain_data_mode=$(stat -c %a -- "$chain_data")
test "$((8#$chain_data_mode & 0022))" -eq 0
test -f "$chain_lock" -a ! -L "$chain_lock"
test "$(realpath -ms -- "$chain_lock")" = "$chain_lock"
test "$(realpath -e -- "$chain_lock")" = "$chain_lock"
test "$(stat -c %h -- "$chain_lock")" = 1
test -f "$chain_blocks" -a ! -L "$chain_blocks"
test "$(realpath -ms -- "$chain_blocks")" = "$chain_blocks"
test "$(realpath -e -- "$chain_blocks")" = "$chain_blocks"
test "$(stat -c %h -- "$chain_blocks")" = 1
case "$(stat -c '%U:%G:%a' -- "$chain_blocks")" in
  root:root:600|root:btc09-otc:640) ;;
  *) false ;;
esac
test -f "$general_wallet" -a ! -L "$general_wallet"
test "$(realpath -ms -- "$general_wallet")" = "$general_wallet"
test "$(realpath -e -- "$general_wallet")" = "$general_wallet"
test "$(stat -c %h -- "$general_wallet")" = 1
test "$(stat -c '%U:%G:%a' -- "$general_wallet")" = 'root:root:600'
blocks_identity_before=$(stat -Lc '%d:%i' -- "$chain_blocks")
chown root:btc09-otc "$chain_blocks"
chmod 0640 "$chain_blocks"
chown root:btc09-otc "$chain_lock"
chmod 0660 "$chain_lock"
test "$(stat -c '%U:%G %a' -- "$chain_blocks")" = 'root:btc09-otc 640'
test "$(stat -c '%U:%G %a' -- "$chain_lock")" = 'root:btc09-otc 660'
systemctl start btc09-seed
systemctl is-active --quiet btc09-seed
replacement_verified=0
for ((attempt=0; attempt<300; attempt++)); do
  tip_after=$({ curl -fsS http://127.0.0.1:8009/api/v1/tip | \
    python3 -c 'import json,sys; p=json.load(sys.stdin); assert p["schema_version"]==1 and p["network"]=="btc09-mainnet"; print(p["tip"]["height"])'; } 2>/dev/null || true)
  blocks_identity_after=$(stat -Lc '%d:%i' -- "$chain_blocks")
  if [[ $tip_after =~ ^[0-9]+$ ]] && (( tip_after > tip_before )) && \
      [[ $blocks_identity_after != "$blocks_identity_before" ]]; then
    replacement_verified=1
    break
  fi
  sleep 2
done
[[ $replacement_verified == 1 ]]
test "$(stat -c '%U:%G:%a:%h' -- "$chain_blocks")" = 'root:btc09-otc:640:1'
test "$(stat -c '%U:%G:%a:%h' -- "$general_wallet")" = 'root:root:600:1'
install -o root -g root -m 0644 \
  /opt/btc09/bitcoin09/bot/btc09-otc-bot.service \
  /etc/systemd/system/btc09-otc-bot.service
install -o root -g root -m 0644 \
  /opt/btc09/bitcoin09/bot/btc09-otc-feed.service \
  /etc/systemd/system/btc09-otc-feed.service
systemd-analyze verify /etc/systemd/system/btc09-otc-bot.service \
  /etc/systemd/system/btc09-otc-feed.service
systemctl daemon-reload
systemctl enable btc09-otc-bot btc09-otc-feed
systemctl cat btc09-otc-bot | \
  grep -Fx 'ConditionPathExists=!/var/lib/btc09-otc-maintenance/active'
systemctl cat btc09-otc-feed | \
  grep -Fx 'ConditionPathExists=!/var/lib/btc09-otc-maintenance/active'
```

Do not start either service yet.

The chain preflight fails if the existing block or coordination file is
missing, linked, or below a symlinked or group/world-writable data parent. It
also rejects extended POSIX ACL access/default metadata on the parent, block,
lock, or general wallet, so named-user ACLs cannot widen the mode contract. It
changes only the existing block to `root:btc09-otc 0640` and the lock to
`root:btc09-otc 0660`; root ownership preserves seed interoperability. On Unix,
the canonical block writer safely opens and validates the existing target,
requires a single-link regular file owned by the writer with mode `0600` or
`0640`, and preserves its uid, gid, and mode across every atomic replacement.
First creation remains mode 0600. Metadata failure occurs before rename and
leaves the prior snapshot intact. The general node wallet remains `root:root
0600` and is never modified by this preflight. The exact lock-only exception
never grants write access to `blocks-mainnet.dat` or read access to the
root-only general wallet:
`ReadWritePaths=/opt/btc09/data/blocks-mainnet.dat.lock`.

`REVIEWED_BTC09_SHA256` must come from the separately reviewed artifact
manifest, not from hashing the untrusted host binary in place. The active
maintenance marker blocks OTC startup while the runbook stops and drains
`btc09-seed`, promotes the existing metadata, and restarts the already verified
binary. Acceptance requires a strictly higher tip and a different block-file
inode before rechecking `root:btc09-otc 0640`; a chmod-only result is not
sufficient evidence that atomic replacement preserves access. If no higher tip
arrives within the bounded wait, the gate fails closed with maintenance still
active. Never mine or fabricate a production block solely to satisfy this gate.

## 3. Create the staged dedicated wallet

Create the escrow wallet on the fresh machine as the service user:

```bash
test ! -e /var/lib/btc09-otc-staging/wallet-mainnet.json
runuser -u btc09-otc -- /opt/btc09/btc09 wallet new \
  -wallet-file /var/lib/btc09-otc-staging/wallet-mainnet.json \
  -network btc09-mainnet -json
chown btc09-otc:btc09-otc /var/lib/btc09-otc-staging/wallet-mainnet.json
chmod 0600 /var/lib/btc09-otc-staging/wallet-mainnet.json
install -o root -g root -m 0600 \
  /var/lib/btc09-otc-staging/wallet-mainnet.json \
  /var/lib/btc09-otc-maintenance/sources/wallet-mainnet.json
rm -f /var/lib/btc09-otc-staging/wallet-mainnet.json
```

The command is crash-durable and refuses to replace an existing wallet. Record
the one public address in the private deployment log, never the wallet file.
Never copy `/opt/btc09/data/wallet-mainnet.json`; that is the general node
wallet and must remain root-only and separate from OTC custody.

## 4. Back up and run a disabled migration test

Stop both OTC services and back up the legacy database together with the new
dedicated wallet. Do not pass the general node wallet to the backup script.

```bash
systemctl stop btc09-otc-feed btc09-otc-bot
test -f /opt/btc09/otc_bot.db -a ! -L /opt/btc09/otc_bot.db
test "$(realpath -ms /opt/btc09/otc_bot.db)" = \
  "$(realpath -e /opt/btc09/otc_bot.db)"
test "$(stat -c %h /opt/btc09/otc_bot.db)" = 1
chown root:root /opt/btc09/otc_bot.db
chmod 0600 /opt/btc09/otc_bot.db
test "$(stat -c '%U:%a' /opt/btc09/otc_bot.db)" = root:600
test "$(stat -c '%U:%a' \
  /var/lib/btc09-otc-maintenance/sources/wallet-mainnet.json)" = root:600
/opt/btc09/bitcoin09/deploy/scripts/backup-otc.sh \
  /opt/btc09/otc_bot.db \
  /var/lib/btc09-otc-maintenance/sources/wallet-mainnet.json \
  /var/backups/btc09
```

`backup-otc.sh` requires root and refuses symlinks, hard links, unsafe modes,
writable ancestry, traversal, and destinations outside `/var/backups/btc09`.
Root-owned legacy/staged sources must be mode 0600 below root-owned non-writable
directory ancestry. The snapshot helper holds no-follow source and directory
descriptors, revalidates source-name inode identity, uses SQLite backup through
the held directory, and creates outputs relative to a held destination
descriptor. It writes a mode-0600 wallet copy and SHA256 manifest in an atomic
timestamped directory, verifies the copied database and manifest, and removes
incomplete staging on failure.

Run the migration test on a copy. The original legacy database must remain
unchanged:

```bash
rm -f /tmp/otc-migration-test.db
sqlite3 /opt/btc09/otc_bot.db '.backup /tmp/otc-migration-test.db'
chown btc09-otc:btc09-otc /tmp/otc-migration-test.db
chmod 0600 /tmp/otc-migration-test.db
cd /opt/btc09/bitcoin09
runuser -u btc09-otc -- env \
  OTC_ACCEPTING_ORDERS=0 \
  DB_PATH=/tmp/otc-migration-test.db \
  /opt/btc09/venv/bin/python -c \
  'from bot.otc.store import Store; s=Store("/tmp/otc-migration-test.db"); s.initialize(); assert s.integrity_check()=="ok"'
test "$(sqlite3 /tmp/otc-migration-test.db 'PRAGMA integrity_check')" = ok
test -z "$(sqlite3 /tmp/otc-migration-test.db 'PRAGMA foreign_key_check')"
sqlite3 /tmp/otc-migration-test.db \
  'SELECT version,origin FROM schema_meta WHERE id=1;'
sqlite3 /tmp/otc-migration-test.db \
  '.backup /var/lib/btc09-otc-maintenance/sources/otc_bot.db'
chown root:root /var/lib/btc09-otc-maintenance/sources/otc_bot.db
chmod 0600 /var/lib/btc09-otc-maintenance/sources/otc_bot.db
rm -f /tmp/otc-migration-test.db
```

Expected schema version is `4`; no migration or foreign-key error is allowed.
Before copying the real database, run the version-aware read-only preflight. It
reuses the Store migration validator for the two proven exact legacy prototype
catalogs (fresh-column and historical incremental-ALTER construction), v3, and
v4, so catalog rules cannot drift. It requires zero orders and zero legacy
withdrawals, plus zero v4 fee withdrawals or unresolved transfers. It prints no
rows or private values:

```bash
cd /opt/btc09/bitcoin09
/opt/btc09/venv/bin/python \
  deploy/scripts/preflight-otc-migration.py /opt/btc09/otc_bot.db
```

Only after the preflight reports zero obligations, install the complete staged
generation through the durable maintenance gate:

```bash
/opt/btc09/bitcoin09/deploy/scripts/install-otc-generation.sh \
  /var/lib/btc09-otc-maintenance/sources/otc_bot.db \
  /var/lib/btc09-otc-maintenance/sources/wallet-mainnet.json && \
rm -f /var/lib/btc09-otc-maintenance/sources/otc_bot.db \
  /var/lib/btc09-otc-maintenance/sources/wallet-mainnet.json
```

The installer creates or validates and fsyncs the root-owned
`/var/lib/btc09-otc-maintenance/active` marker, then stops both services. It
stages and verifies a complete v4 database and dedicated wallet, replaces both
files while the condition blocks every auto-start, fsyncs and revalidates the
installed generation, and only then removes and fsyncs the marker. The two file
moves are not an atomic pair; the durable marker is what makes a crash or reboot
fail closed. Services remain stopped.

## 5. Start and verify units while disabled

```bash
systemctl enable --now btc09-otc-bot btc09-otc-feed
```

Read back the unit contract without reading the credential source:

```bash
systemctl show btc09-otc-bot \
  -p User -p Group -p MainPID -p ControlGroup -p KillMode -p TimeoutStopUSec \
  -p NoNewPrivileges -p PrivateTmp -p PrivateDevices -p ProtectSystem \
  -p ReadWritePaths -p ReadOnlyPaths
systemctl show btc09-otc-feed \
  -p User -p Group -p MainPID -p ControlGroup -p BindReadOnlyPaths -p InaccessiblePaths
systemctl is-active btc09-otc-bot btc09-otc-feed
systemd-analyze security btc09-otc-bot.service
systemd-analyze security btc09-otc-feed.service
/opt/btc09/venv/bin/python \
  /opt/btc09/bitcoin09/deploy/scripts/check-otc-process-environment.py
/opt/btc09/bitcoin09/deploy/scripts/check-otc-service-isolation.sh
runuser -u btc09-otc -- test ! -r /opt/btc09/data/wallet-mainnet.json
```

Compare the security exposure with the pre-deploy baseline. A missing hardening
property or unexpected writable path blocks deployment. Confirm the feed binds
only numeric loopback:

```bash
ss -ltnp | grep '127.0.0.1:8019'
/opt/btc09/bitcoin09/deploy/scripts/check-otc-health.sh \
  http://127.0.0.1:8019/healthz \
  /var/lib/btc09-otc/otc_bot.db 0
```

The health checker parses bounded JSON without printing it, requires detailed
HTTP health, v4 schema, SQLite integrity and foreign keys, a real JSON boolean
for `accepting_orders`, and no unresolved address, transfer, reorg, or chain
state. Expected disabled health is HTTP 503 with otherwise healthy detail.
The process-environment verifier requires root to read `/proc`, compares only
the seven pinned safety keys, and prints a sanitized pass/fail result without
reading or displaying credential values.
The isolation verifier also proves the feed's effective read-only bind has the
same device/inode as `/var/lib/btc09-otc-public`, accepts exactly one regular
`otc-bot-feed.json`, and rejects private-state alias entries. Inside the bot's
effective mount it also opens only the validated chain lock `O_RDWR` without
writing content, proves the group-readable block file survives the writer's
atomic replacement contract but cannot be opened for writing, and proves the
general node wallet cannot be opened for reading.

Run the actual cgroup/recovery gate before any pilot:

```bash
/opt/btc09/bitcoin09/deploy/scripts/test-otc-systemd-restart.sh \
  --acknowledge-service-restart
```

## 6. Resume after reboot with a maintenance marker

If the host reboots or an install command fails while
`/var/lib/btc09-otc-maintenance/active` exists, both units must remain stopped.
Do not remove or blind-clear the marker and do not guess which half-generation
was installed. Revalidate the authoritative complete source database/wallet
pair, then rerun the generation installer with those same two sources:

```bash
test -f /var/lib/btc09-otc-maintenance/active
! systemctl is-active --quiet btc09-otc-bot
! systemctl is-active --quiet btc09-otc-feed
test "$(sqlite3 "$source_db" 'PRAGMA integrity_check')" = ok
test -z "$(sqlite3 "$source_db" 'PRAGMA foreign_key_check')"
test "$(stat -c %a "$source_wallet")" = 600
/opt/btc09/bitcoin09/deploy/scripts/install-otc-generation.sh \
  "$source_db" "$source_wallet"
```

The rerun replaces and validates both members from one known generation before
it clears the marker. If the original staged sources were lost, recreate the
v4 migration copy or select one verified backup directory; never combine files
from different backups. Only after the installer reports success may the
disabled services be started and all health/isolation gates rerun.

## 7. nginx and Cloudflare gate

Do not replace the Certbot-owned TLS virtual host with the old HTTP-only
template. The modular installer preserves the exact regular
`/etc/nginx/sites-available/bitcoin09-domain-pending` file and its enabled
symlink. It removes only legacy inline OTC feed and `/otc-feed-healthz`
locations, inserts one reviewed server-context include into the canonical
`btc09.org` TLS block, and leaves explorer, redirect, certificate, and other
Certbot directives byte-for-byte intact. Intake remains disabled.

```bash
/opt/btc09/bitcoin09/deploy/scripts/install-otc-nginx.sh \
  --acknowledge-certbot-tls-edit
nginx -T > /root/nginx-effective.txt
grep -nE 'bitcoin09-otc|otc-bot-feed|limit_req|limit_conn|CF-Connecting-IP' \
  /root/nginx-effective.txt
test "$(grep -Fxc \
  '    include /etc/nginx/snippets/bitcoin09-otc-server.conf;' \
  /etc/nginx/sites-available/bitcoin09-domain-pending)" = 1
! grep -q 'otc-feed-healthz' \
  /etc/nginx/sites-available/bitcoin09-domain-pending
rm -f /root/nginx-effective.txt
```

The installer requires the normal root-owned enabled symlink to resolve
exactly to the regular sites-available target. It backs up the target and any
prior fragments below root-only `/var/backups/btc09`, installs the reviewed
http-context Cloudflare/rate-zone fragment and server-context feed snippet,
and patches idempotently. It holds Certbot's compatible Nginx configuration
lock for the full transaction as well as a private install mutex. Device/inode,
hash, metadata, and presence-state baselines prevent a concurrent operator edit
from being overwritten. The backup manifest hashes both that complete presence
record and every saved file and is verified strictly before rollback.

Before reload, bounded `nginx -T` output must contain exactly one canonical
feed route and one `127.0.0.1:8019/otc-bot-feed.json` proxy, with no alternate
8019 upstream, public health route, or duplicate response-header owner. The
feed location defines no `add_header` or `proxy_hide_header`: the backend owns
the singleton `Access-Control-Allow-Origin: *` and `Cache-Control: no-store`,
while the canonical TLS server's existing
`X-Content-Type-Options: nosniff` inherits into the location. After reload, a
bounded ten-second readiness gate opens fresh local TLS/SNI connections and
requires the exact singleton header signature plus public health HTTP 404 on
three consecutive attempts. A transient response from a draining old Nginx
worker resets that streak. The installer then performs one final visible feed
header audit and health-404 readback; detailed health remains loopback-only at
`http://127.0.0.1:8019/healthz`.

Any patch, effective-config, reload, or readback failure triggers rollback only
if all installed entries still match this invocation. Rollback first verifies
the strict manifest, atomically restores or removes all three entries, and
reads back their exact hashes, metadata, and absence state. It reloads only
after the restored configuration passes `nginx -t`; tamper, partial restore,
baseline drift, readiness timeout, or reload failure emits a distinct CRITICAL
error and exits nonzero without overwriting a concurrent edit.

Verify every configured Cloudflare `set_real_ip_from` network against the
official current lists before trusting forwarded client IPs:

```bash
curl -fsS https://www.cloudflare.com/ips-v4 -o /tmp/cloudflare-ips-v4
curl -fsS https://www.cloudflare.com/ips-v6 -o /tmp/cloudflare-ips-v6
nginx -T 2>/dev/null | awk '/set_real_ip_from/{gsub(";",""); print $2}' \
  | sort -u > /tmp/nginx-trusted-proxies
cat /tmp/cloudflare-ips-v4 /tmp/cloudflare-ips-v6 | sort -u \
  > /tmp/cloudflare-official
diff -u /tmp/cloudflare-official /tmp/nginx-trusted-proxies
rm -f /tmp/cloudflare-ips-v4 /tmp/cloudflare-ips-v6 \
  /tmp/nginx-trusted-proxies /tmp/cloudflare-official
```

An unexpected range diff blocks installation. Re-run the installer only after
updating and reviewing the exact range fragment; repeat `nginx -t` and inspect
`nginx -T` after every nginx or Certbot change.

### Official Open solo miner

The official coordinator shares the synced node but binds only to loopback.
Add `-solo-api 127.0.0.1:9010` to the node service, restart it, and confirm the
listener is not public:

```bash
ss -lntp | grep '127.0.0.1:9010'
! ss -lntp | grep -E '0\.0\.0\.0:9010|\[::\]:9010'
```

Install the exact two-route nginx boundary in the existing Cloudflare-protected
`btc09.org` TLS host. Pass any valid public mainnet payout address only for the
loopback work health check; the installer does not store it.

```bash
BTC09_MINER_HEALTH_ADDRESS=YOUR_09C_ADDRESS \
  /opt/btc09/bitcoin09/deploy/scripts/install-open-miner.sh \
  /opt/btc09/bitcoin09
nginx -T 2>/dev/null | grep -nE 'btc09_miner|127.0.0.1:9010'
```

No Cloud Firewall rule is added for 9010. Cloudflare and nginx receive HTTPS;
nginx restores the client IP, applies separate work and submit limits, and
proxies only `POST /api/v1/work` and `POST /api/v1/submit` to loopback.

## 8. Restore and rollback

Choose a completed backup directory and verify it before stopping services:

```bash
backup=/var/backups/btc09/otc-YYYYMMDDTHHMMSSZ-PID
cd "$backup"
sha256sum --check manifest.sha256
test "$(sqlite3 otc_bot.db 'PRAGMA integrity_check')" = ok
```

First back up the current complete generation while intake remains disabled,
then use the same maintenance-gated installer for rollback. This is not an
atomic pair move; the durable marker prevents either unit from starting across
a crash or reboot until both restored files verify:

```bash
test ! -e /var/lib/btc09-otc-maintenance/active
install -o root -g root -m 0600 /dev/null \
  /var/lib/btc09-otc-maintenance/active
sync /var/lib/btc09-otc-maintenance/active \
  /var/lib/btc09-otc-maintenance
systemctl stop btc09-otc-feed btc09-otc-bot
/opt/btc09/bitcoin09/deploy/scripts/backup-otc.sh \
  /var/lib/btc09-otc/otc_bot.db \
  /var/lib/btc09-otc/wallet-mainnet.json \
  /var/backups/btc09
/opt/btc09/bitcoin09/deploy/scripts/install-otc-generation.sh \
  "$backup/otc_bot.db" "$backup/wallet-mainnet.json"
systemctl start btc09-otc-bot btc09-otc-feed
/opt/btc09/bitcoin09/deploy/scripts/check-otc-health.sh \
  http://127.0.0.1:8019/healthz /var/lib/btc09-otc/otc_bot.db 0
/opt/btc09/bitcoin09/deploy/scripts/check-otc-service-isolation.sh
```

For active-state sources the backup tool independently verifies the effective
maintenance conditions, fsyncs or validates the durable marker, stops both
units, proves the dedicated custody UID has no remaining processes, and takes
root control of the state parent and source pair before revalidation. A failed
backup intentionally leaves the marker and root-controlled state in place so
the services remain fail closed. A successful backup restores custody
ownership; it clears only a marker that the backup invocation created.
It always acquires the shared generation lock before the destination lock and
holds both through ownership restoration and marker handling. The generation
installer takes only the generation lock, while non-active source backups take
only the destination lock, so no reverse acquisition path can deadlock.

Do not restore only one half of a database/wallet backup. Do not enable intake
as part of rollback. Enabling the controlled pilot is a separate reviewed step.
