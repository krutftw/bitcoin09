# Bitcoin 09 OTC escrow bot

Discord bot for early 09C OTC trades.

The bot is not an exchange. It only escrows the seller's 09C. Buyer payment in
USDT, BTC, fiat, or anything else happens directly between buyer and seller.

## Commands

- `/setaddress <addr>` sets your 09C receive/refund address.
- `/sell <amount> <price> <currency>` creates a sell order and a fresh deposit address.
- `/deposit <order_id>` checks the order deposit address.
- `/orders` lists open sell orders.
- `/buy <order_id>` accepts an order.
- `/confirm <order_id>` confirms your side of the trade.
- `/cancel <order_id>` cancels when the order state allows it.
- `/dispute <order_id>` asks admin to resolve.
- `/balance` shows escrow accounting.
- `/stats` shows live chain, pool, miner, and payout stats.
- `/withdraw <amount> <addr>` lets admin withdraw recorded fees only.
- `/admin resolve|stats|orders` gives admin controls.

## Website feed

The bot writes a sanitized public feed to
`/opt/btc09/public/otc-bot-feed.json`. The feed includes order IDs, status,
amount, total price, currency, and timestamps. It does not include Discord IDs,
usernames, wallet addresses, deposit addresses, or off-chain payment details.

`serve_otc_feed.py` serves that JSON at `/otc-bot-feed.json` through nginx on
`btc09.org`, so the market page can load the live sanitized feed over HTTPS.

## Services

The live setup uses four systemd services and one timer:

- `btc09-otc-bot.service`: Discord escrow slash commands.
- `btc09-otc-feed.service`: localhost JSON feed for nginx.
- `btc09-discord-stats.service`: clickable Discord role buttons.
- `btc09-market-refresh.timer`: refreshes public GitHub OTC issue records for
  the hosted market board.

Keep Discord secrets out of unit files. Put them in `/etc/btc09/discord.env`
with `chmod 600`:

```text
DISCORD_CLIENT_ID=...
DISCORD_GUILD_ID=...
DISCORD_BOT_TOKEN=...
ADMIN_IDS=123456789012345678
```

`btc09-discord-stats.service` needs Node 22+ because it uses the built-in
WebSocket API.

## Safety model

- Every order gets a fresh 09C deposit address.
- A seller cannot cancel after a buyer accepts.
- A buyer can cancel before confirming payment; the order returns to open.
- 09C releases only after both sides confirm, or after admin dispute resolution.
- Matched trades that sit for 24 hours are moved to dispute.
- Fee withdrawal is limited by recorded completed-order fees.

This is still a hot-wallet custody bot. Keep early OTC trades small.
