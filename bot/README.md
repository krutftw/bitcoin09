# Bitcoin 09 OTC trade bot

The Discord bot supports WTS and WTB 09C orders. It escrows only 09C. Fiat,
stablecoins, BTC, and other settlement assets move directly between buyer and
seller; the bot never takes custody of those funds.

## Discord interface

Use `/trade help` or `/help` for a short guide. `/trade` is a Discord command
group, so choose a subcommand rather than sending `/trade` by itself. Use the
group for order creation, browsing, matching, deposit checks,
confirmations, cancellation, disputes, account status, fee administration, and
optional English translation. Legacy top-level commands remain compatibility
wrappers. Public replies contain only the sanitized order projection. Deposit
addresses, user IDs, payment instructions, admin reasons, and wallet data stay
ephemeral or admin-only.

New orders are closed unless both `OTC_ACCEPTING_ORDERS=1` and every runtime
health check passes. Production installs and migrations must start with
`OTC_ACCEPTING_ORDERS=0`.

## State and process boundaries

The production layout is:

```text
/opt/btc09/bitcoin09                    read-only application checkout
/opt/btc09/venv                         Python environment
/opt/btc09/btc09                        09C binary
/opt/btc09/data                         root-owned non-writable chain parent
/opt/btc09/data/blocks-mainnet.dat      root:btc09-otc 0640 canonical blocks
/opt/btc09/data/blocks-mainnet.dat.lock root:btc09-otc 0660 coordination lock
/var/lib/btc09-otc/otc_bot.db           custody ledger
/var/lib/btc09-otc/wallet-mainnet.json  dedicated escrow wallet
/var/lib/btc09-otc-public/otc-bot-feed.json  public projection root:btc09-otc 0775 parent
/etc/btc09/otc-secrets.env              root:root 0600 allowlisted secrets
```

The custody bot runs as `btc09-otc:btc09-otc`. The feed server runs under the
distinct `btc09-otc-feed:btc09-otc-feed` identity with no custody-group
membership. The public source is a stable root-owned directory entry directly
under `/var/lib`; the bot can write its contents but cannot replace or chmod
the directory. The feed identity sees only a read-only directory bind of the
mode-0644 sanitized feed inside its private runtime directory; the state parent,
both wallets, database, bot credentials, and bot `/proc` tree remain
inaccessible. Never copy
`/opt/btc09/data/wallet-mainnet.json` into the escrow state directory. Create a
new, dedicated escrow wallet instead.

The chain store requires an `O_RDWR` coordination descriptor for
`blocks-mainnet.dat.lock`. That existing single-link regular file is the only
writable carve-out below the otherwise read-only chain-data tree. It remains
root-owned so the root seed interoperates with it; group access lets the OTC
process lock it. On Unix, the canonical writer uses a no-follow descriptor to
validate the existing single-link block snapshot and preserves its safe uid,
gid, and `0600` or `0640` mode across every same-directory atomic replacement;
first creation remains `0600`. The production block starts as
`root:btc09-otc 0640`, so the OTC process can read block history persistently
but cannot write it. The carve-out never grants write access to
`blocks-mainnet.dat`, and the general node wallet remains `root:root 0600` and
inaccessible.

The root-owned credential source stays mode 0600. systemd `LoadCredential=`
copies it into a private read-only credential mount. The loader accepts
systemd's root-owned mode-0440 copy only when it is a direct child of the
validated per-unit mount below `/run/credentials`; every ordinary credential
file must remain owner-only. The bot's bounded parser
accepts only Discord identity, translation, and admin-destination secret keys;
configuration and custody paths are rejected. Secret values never enter
`os.environ`. Do not loosen the source file permissions.
The unit explicitly uses `KillMode=control-group`, so a restart kills the old
main process and every prepare/broadcast child before recovery starts.
Both units also refuse to start while the durable root-owned `MAINTENANCE`
marker exists. Database/wallet generation installs keep that marker fsynced
across both replacements and clear it only after complete readback validation.

## Public feed

The bot atomically publishes a privacy-whitelisted mode-0644 feed to
`/var/lib/btc09-otc-public/otc-bot-feed.json`. The localhost feed service reads
it through `/run/btc09-otc-feed/public`, but cannot write the source or access
the database and wallets directly or through the bot's `/proc` root/file
descriptors. A modular nginx TLS include exposes only the public projection
without replacing Certbot-managed server blocks. Detailed health remains
on `http://127.0.0.1:8019/healthz`.

## Local development checks

From the repository root:

```bash
python -m unittest discover -s bot/tests -v
go test ./...
go vet ./...
shellcheck deploy/scripts/*.sh
```

The systemd cgroup test is intentionally not a local unit test. On an isolated
staging host, with intake disabled, run:

```bash
sudo /opt/btc09/bitcoin09/deploy/scripts/test-otc-systemd-restart.sh \
  --acknowledge-service-restart
```

It temporarily replaces only the service command, uses a root-anchored,
unpredictable fixture below `/run/btc09-otc-restart-test` plus an isolated v4
test database and regtest wallet adapter, injects one real production `Store` /
`TradeService` prepared transfer, restarts the actual systemd service, checks
old parent and child PIDs are gone, and proves recovery queried only the stored
txid. A trap restores the original disabled service. It never opens the
production database or wallet.

See `deploy/README.md` for installation, migration test, backup, restore, nginx,
Cloudflare, and systemd readback procedures.
