# Bitcoin 09 OTC Trade System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the prototype sell-only hot-wallet bot with a tested two-sided WTB/WTS 09C escrow service that supports broad external settlement assets without taking custody of those outside funds.

**Architecture:** Keep Discord as the authenticated interaction surface and SQLite as the single-process durable store, but split domain rules, atomic persistence, wallet operations, orchestration, UI, feed projection, and optional translation into focused modules. Every wallet send is preceded by an atomic transfer reservation and followed by structured broadcast/reconciliation states; public output is a privacy-safe projection.

**Tech Stack:** Python 3.12+ standard library, `discord.py==2.7.1`, `requests==2.34.2`, `cryptography==49.0.0`, SQLite WAL, Go 1.24+, existing Bitcoin 09 node/P2P/explorer, systemd, nginx.

## Global Constraints

- Escrow only 09C; never receive or hold AUD, USD, CNY, USDT, USDC, BTC, ETH, bank funds, or other settlement assets.
- Support both WTS and WTB fixed-total-price orders.
- Common assets use autocomplete; custom asset codes must match `[A-Z0-9._-]{2,12}`.
- Public output must exclude Discord IDs, usernames, wallet/deposit addresses, payment coordinates, dispute text, and private evidence.
- Bot UI and structured records are English.
- Pilot service fee is exactly `0%`; network fees are quoted and reserved separately.
- New orders remain disabled whenever reconciliation, wallet solvency, database integrity, or explorer health is not green.
- Every fund-moving state claim uses one conditional update inside `BEGIN IMMEDIATE`.
- A timeout or any send result that may have signed/broadcast becomes `transfer_uncertain` and is never automatically retried.
- Use integer 09C base units at module boundaries; one 09C is `100_000_000` units.
- Use failing tests before production changes and review every task before the next task starts.
- Production enablement requires a backup, empty-v0.2.1 migration proof, controlled funded trade, refund, dispute, restart recovery, feed readback, and balance reconciliation.

---

### Task 1: Python Package, Domain Values, and Fee Quotes

**Files:**
- Create: `bot/otc/__init__.py`
- Create: `bot/otc/domain.py`
- Create: `bot/tests/__init__.py`
- Create: `bot/tests/test_domain.py`
- Create: `bot/requirements.txt`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `OrderSide`, `OrderState`, `TransferState`, `Money`, `SettlementTerms`, `FeeQuote`, `parse_09c`, `parse_asset`, `parse_method`, `quote_deposit`.
- `Money.units` is an `int`; no `float` or database `REAL` values are allowed.

- [ ] **Step 1: Add pinned runtime dependencies and ignore test caches**

```text
# bot/requirements.txt
cryptography==49.0.0
discord.py==2.7.1
requests==2.34.2
```

Add these lines to `.gitignore`:

```gitignore
bot/**/__pycache__/
bot/.coverage
```

- [ ] **Step 2: Write failing domain tests**

```python
# bot/tests/test_domain.py
import unittest

from bot.otc.domain import (
    FeeQuote,
    OrderSide,
    OrderState,
    parse_09c,
    parse_asset,
    parse_method,
    quote_deposit,
)


class DomainTests(unittest.TestCase):
    def test_amount_is_exact_integer_units(self):
        self.assertEqual(parse_09c("1.00000001"), 100_000_001)
        for bad in ("0", "-1", "1.000000001", "nan", "inf"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_09c(bad)

    def test_asset_and_method_validation(self):
        self.assertEqual(parse_asset(" usdt "), "USDT")
        self.assertEqual(parse_asset("x-custom"), "X-CUSTOM")
        self.assertEqual(parse_method("PayID"), "PayID")
        for bad in ("$", "A", "USDT ERC20 PLEASE DM ME", "US DT"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_asset(bad)

    def test_zero_percent_quote_reserves_network_fee(self):
        quote = quote_deposit(net_amount=5_000_000_000, network_fee=10_000, fee_bps=0)
        self.assertEqual(quote, FeeQuote(net_amount=5_000_000_000, network_fee=10_000, service_fee=0, deposit_required=5_000_010_000))

    def test_order_states_are_explicit(self):
        self.assertEqual(OrderSide.BUY.value, "buy")
        self.assertEqual(OrderState.TRANSFER_UNCERTAIN.value, "transfer_uncertain")


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 3: Run the test and verify RED**

Run:

```powershell
python -m unittest bot.tests.test_domain -v
```

Expected: import failure because `bot.otc.domain` does not exist.

- [ ] **Step 4: Implement the domain module**

```python
# bot/otc/domain.py
from __future__ import annotations

import re
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from enum import StrEnum

UNITS_PER_09C = 100_000_000
ASSET_RE = re.compile(r"[A-Z0-9._-]{2,12}\Z")
METHOD_RE = re.compile(r"[A-Za-z0-9 ._+/-]{2,32}\Z")


class OrderSide(StrEnum):
    BUY = "buy"
    SELL = "sell"


class OrderState(StrEnum):
    AWAITING_DEPOSIT = "awaiting_deposit"
    OPEN = "open"
    MATCHED = "matched"
    DISPUTED = "disputed"
    RELEASE_RESERVED = "release_reserved"
    REFUND_RESERVED = "refund_reserved"
    BROADCAST = "broadcast"
    COMPLETED = "completed"
    REFUNDED = "refunded"
    CANCELLED = "cancelled"
    DEPOSIT_EXPIRED = "deposit_expired"
    TRANSFER_FAILED_SAFE = "transfer_failed_safe"
    TRANSFER_UNCERTAIN = "transfer_uncertain"


class TransferState(StrEnum):
    RESERVED = "reserved"
    BROADCAST = "broadcast"
    CONFIRMED = "confirmed"
    FAILED_SAFE = "failed_safe"
    UNCERTAIN = "uncertain"


@dataclass(frozen=True)
class FeeQuote:
    net_amount: int
    network_fee: int
    service_fee: int
    deposit_required: int


@dataclass(frozen=True)
class SettlementTerms:
    asset: str
    method: str
    network: str | None


def parse_09c(value: str) -> int:
    try:
        amount = Decimal(value.strip())
    except (InvalidOperation, AttributeError):
        raise ValueError("invalid 09C amount")
    if not amount.is_finite() or amount <= 0 or amount.as_tuple().exponent < -8:
        raise ValueError("09C amount must be positive with at most 8 decimals")
    return int(amount * UNITS_PER_09C)


def parse_asset(value: str) -> str:
    asset = value.strip().upper()
    if not ASSET_RE.fullmatch(asset):
        raise ValueError("asset must be 2-12 letters, numbers, dot, underscore, or hyphen")
    return asset


def parse_method(value: str) -> str:
    method = " ".join(value.strip().split())
    if not METHOD_RE.fullmatch(method):
        raise ValueError("payment method must be 2-32 plain characters")
    return method


def quote_deposit(*, net_amount: int, network_fee: int, fee_bps: int) -> FeeQuote:
    if net_amount <= 0 or network_fee < 0 or not 0 <= fee_bps <= 10_000:
        raise ValueError("invalid fee quote")
    service_fee = (net_amount * fee_bps + 9_999) // 10_000
    return FeeQuote(net_amount, network_fee, service_fee, net_amount + network_fee + service_fee)
```

- [ ] **Step 5: Verify GREEN and commit**

```powershell
python -m unittest bot.tests.test_domain -v
git add .gitignore bot/requirements.txt bot/otc/__init__.py bot/otc/domain.py bot/tests/__init__.py bot/tests/test_domain.py
git commit -m "Build OTC domain model"
```

Expected: four passing tests.

---

### Task 2: Versioned SQLite Schema and Empty-v0.2.1 Migration

**Files:**
- Create: `bot/otc/store.py`
- Create: `bot/tests/test_store_schema.py`
- Read: `bot/btc09_otc_bot.py:68-157`

**Interfaces:**
- Consumes: enums and integer-unit rules from `bot.otc.domain`.
- Produces: `Store(path)`, `Store.initialize()`, `Store.integrity_check()`, `Store.create_order(...)`, `Store.get_order(order_id)`, `Store.append_audit(...)`.

- [ ] **Step 1: Write migration and schema tests**

```python
# bot/tests/test_store_schema.py
import sqlite3
import tempfile
import unittest
from pathlib import Path

from bot.otc.store import SCHEMA_VERSION, Store


class StoreSchemaTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "otc.db"

    def tearDown(self):
        self.tmp.cleanup()

    def test_new_schema_has_required_tables_and_version(self):
        store = Store(self.path)
        store.initialize()
        with sqlite3.connect(self.path) as conn:
            names = {row[0] for row in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            self.assertTrue({"users", "orders", "transfers", "audit_events", "schema_meta"} <= names)
            self.assertEqual(conn.execute("SELECT version FROM schema_meta").fetchone()[0], SCHEMA_VERSION)

    def test_empty_prototype_database_migrates_without_loss(self):
        with sqlite3.connect(self.path) as conn:
            conn.executescript("""
                CREATE TABLE users (user_id INTEGER PRIMARY KEY, username TEXT, wallet_addr TEXT, created_at INTEGER, updated_at INTEGER);
                CREATE TABLE orders (order_id INTEGER PRIMARY KEY AUTOINCREMENT, seller_id INTEGER NOT NULL, amount TEXT NOT NULL, price TEXT NOT NULL, currency TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
                CREATE TABLE withdrawals (withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT, admin_id INTEGER NOT NULL, amount TEXT NOT NULL, address TEXT NOT NULL, txid TEXT, status TEXT NOT NULL, created_at INTEGER NOT NULL);
                INSERT INTO users VALUES (7, 'pilot', NULL, 1, 1);
            """)
        store = Store(self.path)
        store.initialize()
        self.assertEqual(store.integrity_check(), "ok")
        with sqlite3.connect(self.path) as conn:
            self.assertEqual(conn.execute("SELECT username FROM users WHERE user_id=7").fetchone()[0], "pilot")
```

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_store_schema -v
```

Expected: import failure for `bot.otc.store`.

- [ ] **Step 3: Implement connection policy and migration**

Create `Store.connect()` so every connection executes:

```python
conn = sqlite3.connect(self.path, timeout=30, isolation_level=None)
conn.row_factory = sqlite3.Row
conn.execute("PRAGMA busy_timeout=30000")
conn.execute("PRAGMA foreign_keys=ON")
conn.execute("PRAGMA journal_mode=WAL")
```

Use `SCHEMA_VERSION = 3`. `initialize()` must run `BEGIN IMMEDIATE`. If an
existing `orders` table lacks `side`, require it to contain zero rows, rename
it to `orders_v2_archive`, and create the v3 table. A non-empty prototype table
must raise `MigrationBlocked` instead of guessing how to transform live funds.
Keep the existing `users` and `withdrawals` rows. Create this exact v3 schema:

```sql
CREATE TABLE IF NOT EXISTS schema_meta (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
  user_id INTEGER PRIMARY KEY,
  username TEXT NOT NULL,
  wallet_addr TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS orders (
  order_id INTEGER PRIMARY KEY AUTOINCREMENT,
  side TEXT NOT NULL CHECK(side IN ('buy','sell')),
  maker_id INTEGER NOT NULL,
  maker_name TEXT NOT NULL,
  buyer_id INTEGER,
  buyer_name TEXT,
  seller_id INTEGER,
  seller_name TEXT,
  net_amount_units INTEGER NOT NULL CHECK(net_amount_units > 0),
  network_fee_units INTEGER NOT NULL CHECK(network_fee_units >= 0),
  service_fee_units INTEGER NOT NULL CHECK(service_fee_units >= 0),
  deposit_required_units INTEGER NOT NULL CHECK(deposit_required_units > 0),
  deposit_confirmed_units INTEGER NOT NULL DEFAULT 0 CHECK(deposit_confirmed_units >= 0),
  total_price TEXT NOT NULL,
  settlement_asset TEXT NOT NULL,
  settlement_network TEXT,
  payment_method TEXT NOT NULL,
  state TEXT NOT NULL,
  deposit_addr TEXT,
  buyer_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(buyer_confirmed IN (0,1)),
  seller_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(seller_confirmed IN (0,1)),
  deposit_deadline INTEGER,
  matched_at INTEGER,
  trade_deadline INTEGER,
  disputed_at INTEGER,
  completed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS transfers (
  transfer_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER REFERENCES orders(order_id),
  kind TEXT NOT NULL CHECK(kind IN ('release','refund','resolve_buyer','resolve_seller','fee_withdrawal','excess_refund')),
  state TEXT NOT NULL,
  amount_units INTEGER NOT NULL CHECK(amount_units > 0),
  network_fee_units INTEGER NOT NULL CHECK(network_fee_units >= 0),
  destination TEXT NOT NULL,
  txid TEXT,
  result_class TEXT,
  error_text TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_by_state ON orders(state, updated_at);
CREATE INDEX IF NOT EXISTS orders_by_deposit ON orders(deposit_addr);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_order_transfer
ON transfers(order_id)
WHERE state IN ('reserved','broadcast','uncertain');
CREATE TABLE IF NOT EXISTS audit_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER,
  actor_id INTEGER,
  event_type TEXT NOT NULL,
  old_state TEXT,
  new_state TEXT,
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL
);
INSERT INTO schema_meta(id, version) VALUES(1, 3)
ON CONFLICT(id) DO UPDATE SET version=excluded.version;
```

The archive table is read-only migration evidence and is dropped only in a
future release after its zero-row condition has been verified from backup.

- [ ] **Step 4: Verify migration, integrity, and rollback behavior**

```powershell
python -m unittest bot.tests.test_store_schema -v
python -m unittest discover -s bot/tests -p "test_*.py" -v
```

Expected: all tests pass and `PRAGMA integrity_check` returns `ok`.

- [ ] **Step 5: Commit**

```powershell
git add bot/otc/store.py bot/tests/test_store_schema.py
git commit -m "Add versioned OTC database schema"
```

---

### Task 3: Atomic Order Claims, Transfer Reservations, and Solvency

**Files:**
- Modify: `bot/otc/store.py`
- Create: `bot/tests/test_store_atomic.py`

**Interfaces:**
- Produces: `reserve_accept`, `record_confirmation`, `reserve_transfer`, `mark_transfer_broadcast`, `mark_transfer_failed_safe`, `mark_transfer_uncertain`, `liability_units`, `reserve_fee_withdrawal`.
- All reservation functions return a row/result only to the winning caller and `None` to losers.

- [ ] **Step 1: Write concurrent reservation tests**

```python
# bot/tests/test_store_atomic.py
import concurrent.futures
import tempfile
import unittest
from pathlib import Path

from bot.otc.domain import OrderSide, OrderState
from bot.otc.store import Store


def insert_order(store, *, state, side="sell", both_confirmed=False):
    now = 1_700_000_000
    buyer_id = 20 if state != OrderState.OPEN.value else None
    seller_id = 10
    with store.connect() as conn:
        conn.execute("BEGIN IMMEDIATE")
        cur = conn.execute(
            """INSERT INTO orders (
                side, maker_id, maker_name, buyer_id, buyer_name, seller_id, seller_name,
                net_amount_units, network_fee_units, service_fee_units,
                deposit_required_units, deposit_confirmed_units, total_price,
                settlement_asset, settlement_network, payment_method, state,
                deposit_addr, buyer_confirmed, seller_confirmed, created_at, updated_at
            ) VALUES (?, 10, 'seller', ?, ?, ?, 'seller', 100000000000, 10000, 0,
                      100000010000, 100000010000, '20', 'USDT', 'TRC20',
                      'external wallet', ?, 'deposit-address', ?, ?, ?, ?)""",
            (side, buyer_id, "buyer" if buyer_id else None, seller_id, state,
             int(both_confirmed), int(both_confirmed), now, now),
        )
        conn.commit()
        return cur.lastrowid


class AtomicStoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(Path(self.tmp.name) / "otc.db")
        self.store.initialize()

    def tearDown(self):
        self.tmp.cleanup()

    def test_only_one_buyer_can_accept(self):
        order_id = insert_order(self.store, side=OrderSide.SELL.value, state=OrderState.OPEN.value)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda buyer: self.store.reserve_accept(order_id, buyer), (1001, 1002)))
        self.assertEqual(sum(result is not None for result in results), 1)

    def test_only_one_release_can_be_reserved(self):
        order_id = insert_order(self.store, state=OrderState.MATCHED.value, both_confirmed=True)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda _: self.store.reserve_transfer(order_id, "release", "valid-address"), range(2)))
        self.assertEqual(sum(result is not None for result in results), 1)
        self.assertEqual(self.store.get_order(order_id)["state"], "release_reserved")

    def test_liability_includes_uncertain_transfer(self):
        order_id = insert_order(self.store, state=OrderState.MATCHED.value, both_confirmed=True)
        transfer = self.store.reserve_transfer(order_id, "release", "valid-address")
        self.store.mark_transfer_uncertain(transfer["transfer_id"], "timeout")
        self.assertEqual(self.store.liability_units(), 100_000_010_000)
```

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_store_atomic -v
```

Expected: missing reservation methods.

- [ ] **Step 3: Implement atomic helpers**

Every helper uses this transaction shape:

```python
with self.connect() as conn:
    conn.execute("BEGIN IMMEDIATE")
    row = conn.execute("SELECT * FROM orders WHERE order_id=?", (order_id,)).fetchone()
    if row is None or row["state"] not in allowed_states:
        conn.rollback()
        return None
    updated = conn.execute(
        "UPDATE orders SET state=?, updated_at=? WHERE order_id=? AND state=?",
        (new_state, now, order_id, row["state"]),
    ).rowcount
    if updated != 1:
        conn.rollback()
        return None
    conn.execute("INSERT INTO audit_events (...) VALUES (...)", (...,))
    conn.commit()
```

`reserve_transfer` must insert the transfer row before commit. Its amount and
network fee come from the immutable order quote, not Discord arguments.
`liability_units` sums `deposit_confirmed_units` for funded orders and active or
uncertain transfers until a confirmed release/refund removes the liability.

- [ ] **Step 4: Stress the atomic tests**

```powershell
1..50 | ForEach-Object { python -m unittest bot.tests.test_store_atomic -q; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
python -m unittest discover -s bot/tests -p "test_*.py" -v
```

Expected: 50 clean iterations and the full Python suite passes.

- [ ] **Step 5: Commit**

```powershell
git add bot/otc/store.py bot/tests/test_store_atomic.py
git commit -m "Make OTC fund transitions atomic"
```

---

### Task 4: Structured BTC09 Send and Wallet Adapter

**Files:**
- Modify: `p2p/p2p.go`
- Modify: `p2p/p2p_test.go`
- Modify: `cmd/btc09/main.go`
- Modify: `cmd/btc09/main_test.go`
- Create: `bot/otc/wallet.py`
- Create: `bot/tests/test_wallet.py`

**Interfaces:**
- Go produces: `Node.WaitForPeers(ctx, minimum) bool`, `Node.BroadcastTx(tx) int`, and `btc09 send -json -require-broadcast`.
- JSON result: `{"ok":true,"txid":"<64 hex>","amount_units":N,"fee_units":N,"broadcast_peers":N}`.
- Python produces: `Wallet.send(destination, amount_units, fee_units) -> BroadcastResult` and exceptions `SafeSendFailure`, `UncertainSendFailure`.

- [ ] **Step 1: Write failing Go tests**

Add tests that assert:

```go
func TestBroadcastTxReturnsSuccessfulPeerWrites(t *testing.T) {
    // one writable bufferConn and one failingConn are registered
    // want BroadcastTx(tx) == 1
}

func TestWaitForPeersHonorsContext(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
    defer cancel()
    if n.WaitForPeers(ctx, 1) { t.Fatal("reported a peer") }
}

func TestSendJSONContainsFullTxID(t *testing.T) {
    // exercise the JSON encoder helper with a known 32-byte ID
    // decode output and assert len(txid) == 64 and broadcast_peers == 1
}
```

- [ ] **Step 2: Verify Go RED**

```powershell
go test ./p2p ./cmd/btc09 -run "TestBroadcastTx|TestWaitForPeers|TestSendJSON" -count=1 -v
```

Expected: missing functions or failed expectations.

- [ ] **Step 3: Implement peer wait, broadcast count, and JSON output**

Use this P2P contract:

```go
func (n *Node) WaitForPeers(ctx context.Context, minimum int) bool {
    ticker := time.NewTicker(25 * time.Millisecond)
    defer ticker.Stop()
    for {
        if n.PeerCount() >= minimum { return true }
        select {
        case <-ctx.Done(): return false
        case <-ticker.C:
        }
    }
}

func (n *Node) BroadcastTx(tx *core.Tx) int {
    return n.broadcast(&Msg{Type: "tx", Raw: tx.Bytes()}, "")
}
```

Change `broadcast` to return the number of `p.send` calls that return `nil`.
In `cmdSend`, add `-json` and `-require-broadcast`. When seeds are present,
start the node and require at least one peer before calling `wallet.Send` when
`-require-broadcast` is set. Print the full transaction ID and successful peer
count only after broadcast attempts complete.

- [ ] **Step 4: Write failing Python adapter tests**

```python
# bot/tests/test_wallet.py
import subprocess
import unittest
from unittest.mock import patch

from bot.otc.wallet import SafeSendFailure, UncertainSendFailure, Wallet


class WalletTests(unittest.TestCase):
    def setUp(self):
        self.wallet = Wallet("btc09", "data", "127.0.0.1:9009")

    @patch("bot.otc.wallet.subprocess.run")
    def test_parses_structured_broadcast(self, run):
        run.return_value = subprocess.CompletedProcess([], 0, '{"ok":true,"txid":"' + 'ab'*32 + '","amount_units":100,"fee_units":10,"broadcast_peers":1}\n', '')
        result = self.wallet.send("address", 100, 10)
        self.assertEqual(result.broadcast_peers, 1)

    @patch("bot.otc.wallet.subprocess.run", side_effect=subprocess.TimeoutExpired("btc09", 30))
    def test_timeout_is_uncertain(self, run):
        with self.assertRaises(UncertainSendFailure):
            self.wallet.send("address", 100, 10)

    @patch("bot.otc.wallet.subprocess.run")
    def test_prebroadcast_nonzero_is_safe_failure(self, run):
        run.return_value = subprocess.CompletedProcess([], 2, '{"ok":false,"stage":"preflight","error":"insufficient_funds"}\n', '')
        with self.assertRaises(SafeSendFailure):
            self.wallet.send("address", 100, 10)

    @patch("bot.otc.wallet.subprocess.run")
    def test_unstructured_nonzero_is_uncertain(self, run):
        run.return_value = subprocess.CompletedProcess([], 1, '', 'process crashed')
        with self.assertRaises(UncertainSendFailure):
            self.wallet.send("address", 100, 10)
```

- [ ] **Step 5: Implement adapter, verify, and commit**

The adapter must use base-unit-to-decimal formatting without float, invoke:

```text
btc09 send -to ADDRESS -amount DECIMAL -fee DECIMAL -datadir DATA -seeds 127.0.0.1:9009 -json -require-broadcast
```

and reject malformed JSON, short txids, zero broadcast peers, amount mismatch,
fee mismatch, or any unstructured nonzero exit as uncertain. A failure is safe
to retry only when the CLI emits valid JSON with `ok=false` and
`stage=preflight`, before `wallet.Send` is called.

```powershell
go test ./... -count=1
python -m unittest bot.tests.test_wallet -v
git add p2p/p2p.go p2p/p2p_test.go cmd/btc09/main.go cmd/btc09/main_test.go bot/otc/wallet.py bot/tests/test_wallet.py
git commit -m "Add structured idempotent wallet boundary"
```

---

### Task 5: WTS and WTB Application Service

**Files:**
- Create: `bot/otc/service.py`
- Create: `bot/tests/test_service_orders.py`
- Modify: `bot/otc/store.py`

**Interfaces:**
- Produces: `TradeService.create_sell`, `create_buy`, `accept`, `check_deposit`, `confirm_sent`, `confirm_received`, `list_open`, `mine`.
- Consumes: `Store`, `Wallet`, explorer balance callable, fresh-address callable, and a clock through constructor injection.

- [ ] **Step 1: Write failing WTS/WTB tests**

Create real temporary SQLite stores and small fake boundary adapters. Cover:

```python
def test_wts_requires_deposit_before_listing():
    order = service.create_sell(seller_id=1, net_amount=100_000_000, total_price="2", asset="AUD", method="PayID", network=None)
    assert order.state == "awaiting_deposit"
    explorer.set_balance(order.deposit_addr, order.deposit_required_units)
    assert service.check_deposit(order.order_id, actor_id=1).state == "open"

def test_wtb_assigns_seller_then_requires_deposit():
    order = service.create_buy(buyer_id=2, net_amount=100_000_000, total_price="2", asset="USDT", method="external wallet", network="TRC20")
    assert order.state == "open"
    accepted = service.accept(order.order_id, actor_id=1)
    assert accepted.seller_id == 1
    assert accepted.state == "awaiting_deposit"

def test_buyer_is_not_told_to_pay_before_deposit():
    # check_deposit under quote leaves awaiting_deposit and emits no payment-ready event
```

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_service_orders -v
```

Expected: missing `TradeService`.

- [ ] **Step 3: Implement command-independent orchestration**

`TradeService` returns domain result objects/events and never imports Discord.
WTS creation creates its deposit address immediately. WTB creation is open with
the buyer assigned; acceptance atomically assigns the seller and creates the
deposit address. `check_deposit` compares integer spendable units to the quoted
requirement. Underpayment stays waiting; overpayment creates an excess refund
liability; late payment creates a recovery event and does not reopen the order.

- [ ] **Step 4: Add confirmation/release tests and implementation**

Test that buyer `confirm_sent` alone does not release, seller
`confirm_received` alone does not release, either order of both confirmations
creates exactly one reserved transfer, and 20 concurrent second confirmations
produce one wallet call. The service updates `broadcast` only from a valid
`BroadcastResult`, `transfer_failed_safe` from `SafeSendFailure`, and
`transfer_uncertain` from `UncertainSendFailure`.

- [ ] **Step 5: Verify and commit**

```powershell
1..20 | ForEach-Object { python -m unittest bot.tests.test_service_orders -q; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
python -m unittest discover -s bot/tests -p "test_*.py" -v
git add bot/otc/service.py bot/otc/store.py bot/tests/test_service_orders.py
git commit -m "Build two-sided OTC order service"
```

---

### Task 6: Refund, Dispute, Timeout, and Restart Reconciliation

**Files:**
- Modify: `bot/otc/service.py`
- Modify: `bot/otc/store.py`
- Create: `bot/tests/test_service_recovery.py`

**Interfaces:**
- Produces: `cancel`, `open_dispute`, `resolve_dispute`, `expire_orders`, `reconcile_transfers`, `system_health`.

- [ ] **Step 1: Write failing recovery tests**

Cover these exact cases with a temporary database:

- Two simultaneous funded cancellations invoke the wallet once.
- Two simultaneous admin resolutions invoke the wallet once.
- A matched timeout changes to `disputed` and does not call the wallet.
- A `reserved` transfer on restart blocks new order creation until reconciled.
- A `broadcast` transfer with a transaction present in the explorer becomes
  `completed` or `refunded` according to kind.
- An unknown `broadcast` transaction remains a liability and becomes
  `transfer_uncertain` after the configured reconciliation deadline.
- A late deposit after cancellation creates `excess_refund`/recovery liability.
- `system_health().accepting_orders` is false for DB failure, explorer failure,
  wallet balance below liability, or any uncertain transfer.

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_service_recovery -v
```

- [ ] **Step 3: Implement recovery rules**

Admin resolution requires `winner in {'buyer','seller'}` and a 10-500 character
reason. The store writes the reason only to private audit JSON, never the public
feed. Cancellation and resolution reserve transfer rows before wallet calls.
`expire_orders` uses conditional updates and only opens disputes.
`reconcile_transfers` is safe to run at startup and every 30 seconds.

- [ ] **Step 4: Stress and commit**

```powershell
1..50 | ForEach-Object { python -m unittest bot.tests.test_service_recovery -q; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
python -m unittest discover -s bot/tests -p "test_*.py" -v
git add bot/otc/service.py bot/otc/store.py bot/tests/test_service_recovery.py
git commit -m "Add OTC dispute and restart recovery"
```

---

### Task 7: Discord `/trade` UI and Compatibility Wrappers

**Files:**
- Create: `bot/otc/discord_ui.py`
- Create: `bot/tests/test_discord_ui.py`
- Modify: `bot/btc09_otc_bot.py`

**Interfaces:**
- Produces grouped commands listed in the design and button/modal handlers.
- Consumes only `TradeService`; no SQL, subprocess, or explorer calls occur in UI code.

- [ ] **Step 1: Write failing authorization and rendering tests**

Use small fake interaction/user objects. Assert:

- order summaries are English and contain side, net 09C, total price, asset,
  network/method, and status;
- public messages contain no Discord ID, wallet/deposit address, or payment
  coordinates;
- only seller can check a WTS deposit;
- only participants can confirm/dispute;
- only configured admins can resolve;
- a duplicate button interaction returns the current state and does not call
  the service twice;
- fund-moving actions require an ephemeral confirmation button.

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_discord_ui -v
```

- [ ] **Step 3: Implement grouped commands and components**

Use `app_commands.Group(name="trade", description="Buy and sell Bitcoin 09")`.
Use autocomplete for the common asset list and accept validated custom text.
Commands must defer before explorer/wallet work and use follow-ups after defer.
Persistent button `custom_id` values contain only action and numeric order ID,
never user IDs or addresses.

Keep `/sell`, `/orders`, `/buy`, `/deposit`, `/confirm`, `/cancel`, `/dispute`,
`/setaddress`, and `/balance` as one-release wrappers that call the same service.
Replace `/withdraw` with an admin-only transfer reservation path; default pilot
fee balance is zero.

- [ ] **Step 4: Reduce the composition root**

`bot/btc09_otc_bot.py` must only load validated configuration, initialize the
store/service/adapters, register UI, start reconciliation/deposit background
tasks, and call `bot.run`. Remove duplicate embedded SQL and wallet logic after
tests prove the new modules.

- [ ] **Step 5: Verify and commit**

```powershell
python -m unittest discover -s bot/tests -p "test_*.py" -v
python -m py_compile bot/btc09_otc_bot.py bot/otc/*.py
git add bot/btc09_otc_bot.py bot/otc/discord_ui.py bot/tests/test_discord_ui.py
git commit -m "Add structured Discord trade workflow"
```

---

### Task 8: Privacy-Safe Feed, Health, and Optional English Translation

**Files:**
- Create: `bot/otc/public_feed.py`
- Create: `bot/otc/translation.py`
- Create: `bot/tests/test_public_feed.py`
- Create: `bot/tests/test_translation.py`
- Modify: `bot/serve_otc_feed.py`
- Modify: `deploy/nginx/bitcoin09.conf`

**Interfaces:**
- Produces: `build_public_feed(store)`, `write_public_feed(path, payload)`, `TranslationProvider.translate_to_english(text)`, `/healthz` JSON.

- [ ] **Step 1: Write failing feed privacy tests**

Create an order populated with IDs, usernames, wallet addresses, deposit
addresses, and private dispute text. Serialize the feed and recursively assert
none of those values occur. Assert schema version, health timestamp, side,
status, net amount, total price, price per 09C, asset, method/network label, and
public timestamps are present.

Test atomic writing by patching `os.replace`, verifying the temporary file is
fsynced and replaced in the same directory, and confirming readers never see
partial JSON.

- [ ] **Step 2: Write translation adapter tests**

The default `DisabledTranslationProvider` raises `TranslationUnavailable`
without network access. The Discord message command reports this ephemerally.
An `HTTPTranslationProvider` is created only when `TRANSLATION_API_URL` and
`TRANSLATION_API_TOKEN` are configured; it uses a 10-second timeout, limits
input to 2,000 characters, requests English output, and never logs source text
or the token.

- [ ] **Step 3: Verify RED, implement, and verify GREEN**

```powershell
python -m unittest bot.tests.test_public_feed bot.tests.test_translation -v
python -m unittest discover -s bot/tests -p "test_*.py" -v
```

Add `/healthz` fields: database integrity, explorer reachable, wallet
spendable units, liability units, uncertain transfer count, feed age seconds,
and `accepting_orders`. Bind the feed service to `127.0.0.1` only; nginx exposes
the sanitized feed but keeps detailed health local.

- [ ] **Step 4: Commit**

```powershell
git add bot/otc/public_feed.py bot/otc/translation.py bot/tests/test_public_feed.py bot/tests/test_translation.py bot/serve_otc_feed.py deploy/nginx/bitcoin09.conf
git commit -m "Publish private OTC market health feed"
```

---

### Task 9: Service Hardening, Backup, and Staged Deployment

**Files:**
- Modify: `bot/btc09-otc-bot.service`
- Modify: `bot/btc09-otc-feed.service`
- Modify: `bot/README.md`
- Modify: `deploy/README.md`
- Create: `deploy/scripts/backup-otc.sh`
- Create: `deploy/scripts/check-otc-health.sh`

**Interfaces:**
- Produces reproducible dependency install, root-only backup, health check, and hardened systemd services.

- [ ] **Step 1: Add shell-script syntax checks before implementation**

The scripts must accept explicit paths and fail closed. `backup-otc.sh` runs
`sqlite3 DB '.backup DEST'`, copies the wallet with mode 600, writes SHA256
hashes, and refuses a destination outside `/var/backups/btc09`. `check-otc-health.sh`
requires `integrity=ok`, `accepting_orders` boolean, and zero unresolved schema
migrations.

Run expected RED while files are absent:

```powershell
ssh root@178.128.105.41 "test -x /opt/btc09/bitcoin09/deploy/scripts/backup-otc.sh"
```

Expected: nonzero.

- [ ] **Step 2: Add systemd hardening**

Run the bot under a dedicated `btc09-otc` user. Put mutable state in
`/var/lib/btc09-otc`, with wallet access through a narrowly permissioned group.
Add:

```ini
User=btc09-otc
Group=btc09-otc
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
RestrictSUIDSGID=true
ReadWritePaths=/var/lib/btc09-otc /opt/btc09/data
UMask=0077
```

Keep `/etc/btc09/discord.env` root-owned mode 600 and expose needed values with
a root-owned environment file readable through the service manager, not by
making the file world-readable.

- [ ] **Step 3: Test migration on a copy and deploy disabled**

```powershell
ssh root@178.128.105.41 "systemctl stop btc09-otc-bot; /opt/btc09/bitcoin09/deploy/scripts/backup-otc.sh /opt/btc09/otc_bot.db /opt/btc09/data/wallet-mainnet.json /var/backups/btc09; cp /opt/btc09/otc_bot.db /tmp/otc-migration-test.db; OTC_ORDERS_ENABLED=0 DB_PATH=/tmp/otc-migration-test.db /opt/btc09/venv/bin/python -m bot.otc.store; sqlite3 /tmp/otc-migration-test.db 'PRAGMA integrity_check'"
```

Expected: backup hashes written, migration exit 0, integrity `ok`, original DB
untouched. Deploy code and dependencies, set `OTC_ORDERS_ENABLED=0`, start bot,
and read back command sync plus detailed local health.

- [ ] **Step 4: Verify services and commit deployment files**

```powershell
ssh root@178.128.105.41 "systemctl is-active btc09-otc-bot btc09-otc-feed; curl -fsS http://127.0.0.1:8019/healthz; journalctl -u btc09-otc-bot -n 100 --no-pager"
git add bot/btc09-otc-bot.service bot/btc09-otc-feed.service bot/README.md deploy/README.md deploy/scripts/backup-otc.sh deploy/scripts/check-otc-health.sh
git commit -m "Harden OTC production services"
```

Expected: both active, health JSON valid, no traceback/token/private data in logs.

---

### Task 10: Controlled Funded Pilot, Website, Discord, and GitHub Readback

**Files:**
- Modify: `docs/markets.html`
- Modify: `docs/index.html`
- Modify: `README.md`
- Modify: `bot/README.md`
- Modify: `tools/discord/setup-server.mjs`
- Create: `docs/OTC-PILOT.md`

**Interfaces:**
- Final deliverable: capped 0% fee pilot with proven WTS, WTB, refund, dispute, restart, feed, website, Discord, and GitHub paths.

- [ ] **Step 1: Run the controlled WTS case**

Use a separate pilot seller and buyer address and the smallest practical net
amount. Record before-balances, deposit-required units, deposit txid, release
txid, after-balances, transfer rows, order state, and liability total. Restart
the bot after deposit and before confirmation to prove recovery. The pass
condition is buyer receives the exact net amount, liability returns to zero,
and the network-fee buffer reconciles exactly.

- [ ] **Step 2: Run WTB, refund, and dispute cases**

WTB: buyer posts, seller accepts/deposits, buyer pays externally only after
deposit verification, both confirm, release reconciles.

Refund: funded open WTS cancels once under two simultaneous attempts, seller
receives the quoted refundable amount, and one txid exists.

Dispute: matched order times out or is disputed, two simultaneous admin
resolutions produce one reserved transfer and one txid. Use a reason that
contains no payment credentials.

- [ ] **Step 3: Update website and Discord copy**

The website must show separate bot WTB/WTS rows, net 09C, total settlement,
asset/network/method, pilot status, 0% service fee, 09C-only custody, and the
warning that external payment is direct and not verified by the bot. Do not
claim guaranteed safety, exchange status, official price, or external-fund
escrow.

Update Discord help/resources/OTC guide using the managed setup script. Read
back the registered commands and managed messages. Post one announcement only
after all live gates pass; use the existing duplicate marker.

- [ ] **Step 4: Rotate exposed credentials before enabling orders**

Rotate the Discord bot token and Cloudflare/API credentials exposed in the
prior session, update `/etc/btc09/discord.env`, restart affected services, and
verify command/feed/site readback. Revoke unused R2 credentials. Never print
new values in logs, commits, or task output.

- [ ] **Step 5: Enable capped pilot and verify every surface**

Set explicit production limits:

```text
OTC_ORDERS_ENABLED=1
FEE_BPS=0
MAX_ORDER_09C=1000
MAX_TOTAL_LIABILITY_09C=5000
ORDER_TIMEOUT_SECONDS=86400
```

Verify:

```powershell
python -m unittest discover -s bot/tests -p "test_*.py" -v
go test ./... -count=1
go vet ./...
curl.exe -fsS https://btc09.org/otc-bot-feed.json
curl.exe -fsS https://btc09.org/markets.html
gh api repos/krutftw/bitcoin09/commits/master/status
gh release view --repo krutftw/bitcoin09
ssh root@178.128.105.41 "systemctl is-active btc09-seed btc09-otc-bot btc09-otc-feed btc09-discord-stats nginx; /opt/btc09/bitcoin09/deploy/scripts/check-otc-health.sh"
```

- [ ] **Step 6: Commit, push, and perform readback**

```powershell
git add docs/markets.html docs/index.html README.md bot/README.md tools/discord/setup-server.mjs docs/OTC-PILOT.md
git commit -m "Launch capped two-sided OTC pilot"
git push origin master
git status --short --branch
```

Pass conditions: remote master equals local HEAD, worktree is clean, live files
contain the pushed commit content, Discord commands/messages match, public feed
contains no private fields, services are active, health accepts orders, and no
unreconciled or uncertain transfer remains.
