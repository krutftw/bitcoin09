# Bitcoin 09 VPS deploy notes

Current public host:

```text
site:     https://btc09.org
explorer: https://explorer.btc09.org
seed:     seed.btc09.org:9009
```

Runtime layout:

```text
/opt/btc09/btc09
/opt/btc09/data
/opt/btc09/btc09_otc_bot.py
/opt/btc09/serve_otc_feed.py
/opt/btc09/otc_bot.db
/opt/btc09/public/otc-bot-feed.json
/opt/btc09/bitcoin09/tools/market/build-market-data.mjs
/var/www/bitcoin09
/etc/btc09/discord.env
```

Services:

```text
btc09-seed
btc09-otc-bot
btc09-otc-feed
btc09-market-refresh.timer
btc09-discord-stats
nginx
```

Temporary `sslip.io` launch hostnames should use
`deploy/nginx/bitcoin09-legacy-redirects.conf` and redirect to the canonical
`btc09.org` hosts. They should not serve duplicate site or explorer content.

`btc09-market-refresh.timer` refreshes `/var/www/bitcoin09/market-data.json`
from the public GitHub OTC issue records. This keeps the VPS-hosted market
board current even when GitHub Actions are unavailable.

After uploading static site files to `/var/www/bitcoin09`, make the files
readable by nginx:

```bash
find /var/www/bitcoin09 -type d -exec chmod 755 {} +
find /var/www/bitcoin09 -type f -exec chmod 644 {} +
```

Discord secrets belong in `/etc/btc09/discord.env` with mode `600`, not inside
systemd unit files.
