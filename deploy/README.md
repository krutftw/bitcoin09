# Bitcoin 09 VPS deploy notes

Current public host:

```text
site:     https://btc09.178.128.105.41.sslip.io
explorer: https://explorer.btc09.178.128.105.41.sslip.io
seed:     178.128.105.41:9009
```

Runtime layout:

```text
/opt/btc09/btc09
/opt/btc09/data
/opt/btc09/btc09_otc_bot.py
/opt/btc09/serve_otc_feed.py
/opt/btc09/otc_bot.db
/opt/btc09/public/otc-bot-feed.json
/var/www/bitcoin09
/etc/btc09/discord.env
```

Services:

```text
btc09-seed
btc09-otc-bot
btc09-otc-feed
btc09-discord-stats
nginx
```

After uploading static site files to `/var/www/bitcoin09`, make the files
readable by nginx:

```bash
find /var/www/bitcoin09 -type d -exec chmod 755 {} +
find /var/www/bitcoin09 -type f -exec chmod 644 {} +
```

Discord secrets belong in `/etc/btc09/discord.env` with mode `600`, not inside
systemd unit files.
