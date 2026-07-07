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
- `/withdraw <amount> <addr>` lets admin withdraw recorded fees only.
- `/admin resolve|stats|orders` gives admin controls.

## Safety model

- Every order gets a fresh 09C deposit address.
- A seller cannot cancel after a buyer accepts.
- A buyer can cancel before confirming payment; the order returns to open.
- 09C releases only after both sides confirm, or after admin dispute resolution.
- Matched trades that sit for 24 hours are moved to dispute.
- Fee withdrawal is limited by recorded completed-order fees.

This is still a hot-wallet custody bot. Keep early OTC trades small.
