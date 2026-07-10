# Bitcoin 09 OTC Trade System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the prototype sell-only hot-wallet bot with a tested two-sided WTB/WTS 09C escrow service that supports broad external settlement assets without taking custody of those outside funds.

**Architecture:** Keep Discord as the authenticated interaction surface and SQLite as the single-process durable store, but split domain rules, atomic persistence, wallet operations, orchestration, UI, feed projection, and optional translation into focused modules. Every wallet send is queued and globally claimed, prepared without network side effects, durably stored as one exact signed transaction, then broadcast/reconciled idempotently; public output is a privacy-safe projection.

**Tech Stack:** Python 3.12+ standard library, `discord.py==2.7.1`, `requests==2.34.2`, `cryptography==49.0.0`, SQLite WAL, Go 1.24+, existing Bitcoin 09 node/P2P/explorer, systemd, nginx.

## Global Constraints

- Escrow only 09C; never receive or hold AUD, USD, CNY, USDT, USDC, BTC, ETH, bank funds, or other settlement assets.
- Support both WTS and WTB fixed-total-price orders.
- Common assets use autocomplete; custom asset codes must match `[A-Z0-9._-]{2,12}`.
- Public output must exclude Discord IDs, usernames, wallet/deposit addresses, payment coordinates, dispute text, and private evidence.
- Bot UI and structured records are English.
- Pilot service fee is exactly `0%`; network fees are quoted and reserved separately.
- New orders remain disabled whenever reconciliation, wallet solvency, database integrity, or explorer health is not green.
- No wallet claim/prepare may run until every watched deposit address has one
  complete latest scan at the same live expected tip; provisional spendable
  deposits are restricted and excluded from coin selection.
- Every fund-moving state claim uses one conditional update inside `BEGIN IMMEDIATE`.
- A timeout after network submission becomes `transfer_uncertain`; recovery may
  query or rebroadcast only the already persisted signed transaction and never
  build a replacement payment.
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
- Produces: `OrderSide`, `OrderState`, `TransferState`, `Money`, `SettlementTerms`, `FeeQuote`, `parse_09c`, `parse_total_price`, `parse_asset`, `parse_method`, `quote_deposit`.
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
    parse_total_price,
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

    def test_total_price_is_canonical_positive_decimal(self):
        self.assertEqual(parse_total_price(" 0.00000001 "), "0.00000001")
        self.assertEqual(parse_total_price("2500"), "2500")
        for bad in (".", "0", "00.1", "1.", "1..2", "+1", "1e3", "1,000"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_total_price(bad)

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
TOTAL_PRICE_RE = re.compile(
    r"(?:0|[1-9][0-9]{0,17})(?:\.[0-9]{1,18})?\Z"
)


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


def parse_total_price(value: str) -> str:
    if type(value) is not str:
        raise ValueError("total price must be text")
    price = value.strip()
    if not TOTAL_PRICE_RE.fullmatch(price) or not any(c in "123456789" for c in price):
        raise ValueError("total price must be a positive plain decimal")
    return price


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
- Modify: `bot/otc/domain.py`
- Modify: `bot/tests/test_domain.py`
- Read: `bot/btc09_otc_bot.py:68-157`

**Interfaces:**
- Consumes: enums and integer-unit rules from `bot.otc.domain`.
- Produces: `Store(path)`, `Store.initialize()`, `Store.integrity_check()`, `Store.create_order(...)`, `Store.get_order(order_id)`, `Store.append_audit(...)`.
- `python -m bot.otc.store` requires an explicit `DB_PATH`, initializes that
  exact database, verifies integrity, and prints bounded JSON containing schema
  version/integrity. Import-only execution or a missing path exits nonzero.

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
            self.assertTrue({"users", "orders", "deposit_scans", "deposit_credits", "transfers", "transfer_credit_allocations", "audit_events", "schema_meta"} <= names)
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
conn.execute("PRAGMA recursive_triggers=ON")
conn.execute("PRAGMA journal_mode=WAL")
conn.execute("PRAGMA synchronous=FULL")
```

Use `SCHEMA_VERSION = 4`. Version/catalog compatibility is checked without
persistent mutation before WAL activation. After WAL succeeds, repeat the full
validation under `BEGIN IMMEDIATE`, perform all migration/schema work, validate
the resulting catalog, stamp v4, and commit last. A denied WAL activation,
newer schema, malformed required object, or blocked migration must not stamp or
partially rewrite the database. An already-v4 database is validation-only and
does not upsert its version row. Validate the complete catalog, not only required
object presence: allow exactly the 53 active v4 objects plus exact recognized
empty archive/withdrawal/index evidence for its migration origin, and reject
every other table, view, index, or trigger. Stamp the durable semantic origin
in `schema_meta.origin`: `fresh`, `live_prototype`, `v3_fresh`,
`v3_with_withdrawals`, or `v3_live_prototype`. Three schema-meta triggers make
the one version/origin row insert-once and reject update, delete, or replacement.
Every later v4 initialization
uses that marker to select exactly one permitted catalog; deleting a complete
coherent evidence set must not make a migrated database look fresh. Catalog
cardinality is keyed by object type and name, duplicate names across object
types are rejected, and only verified SQL-NULL autoindexes plus the exact
internal `sqlite_sequence` object are excluded. Executable `sqlite_*` objects
remain part of validation. Canonical SQL comparison preserves every byte inside
quoted literals and identifiers. If the exact live-prototype `orders`
table lacks `side`, require it to contain zero rows and no sequence history,
rename it to `orders_v2_archive`, and create
the v4 table. A non-empty prototype table must raise `MigrationBlocked` instead
of guessing how to transform live funds. Keep the existing `users` and
`withdrawals` rows. An exact empty v3 database may be archived and rebuilt; any
v3 database containing orders, transfers, deposit credits, or a nonzero legacy
aggregate must raise `MigrationBlocked` because no transaction outpoints may be
fabricated from a balance. Extend `OrderState` with `recovery_hold` and
`TransferState` with `queued`, `prepared`, and `cancelled`,
then create this exact v4 schema:

For exact empty v3, first verify `orders`, `transfers`, and `audit_events` all
contain zero rows and every order aggregate is zero. Inside the same migration
transaction rename them to `orders_v3_archive`, `transfers_v3_archive`,
`audit_events_v3_archive`, and `schema_meta_v3_archive`; drop only their verified v3 named indexes so the v4
index names can be created. Preserve `users`, `withdrawals`, any prior empty
`orders_v2_archive`, and all archive tables as evidence. Any nonempty or
non-exact v3 financial catalog is blocked without mutation.
The presence of any matching `sqlite_sequence` row is prior-use evidence even
when `seq` is `0` or negative; absence/zero-row table checks cannot override it.
The unversioned empty-catalog path is stricter: a truly fresh database has no
`sqlite_sequence` object, so even an empty internal sequence table blocks fresh
certification. Prototype and v3 origins may legitimately retain that object and
are instead subject to their exact catalog and named-sequence checks.

The ordered migration algorithm is: (1) raw read-only version/catalog/row-count
preflight; (2) WAL plus `synchronous=FULL`; (3) `BEGIN IMMEDIATE`; (4) repeat
the exact preflight under the writer lock, including empty withdrawals; (5) drop
only verified named v3 indexes; (6) rename orders first so SQLite rewrites the
old transfer FK to the archive, then rename transfers, audit, and the non-STRICT
v3 schema_meta; (7) rebuild any compatible nullable users table; (8) create/validate the complete v4
tables/indexes/triggers; (9) run `foreign_key_check`; (10) stamp v4 and commit as
the final fallible mutation. Every DDL statement is transactional. Inject a
failure after each numbered mutation and prove rollback restores the original
catalog, data, version, and journal policy.

```sql
PRAGMA foreign_keys = ON;
PRAGMA recursive_triggers = ON;
CREATE TABLE IF NOT EXISTS schema_meta (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  version INTEGER NOT NULL,
  origin TEXT NOT NULL CHECK(origin IN (
    'fresh','live_prototype','v3_fresh','v3_with_withdrawals',
    'v3_live_prototype'
  ))
) STRICT;
CREATE TRIGGER IF NOT EXISTS schema_meta_insert_once
BEFORE INSERT ON schema_meta
WHEN EXISTS (SELECT 1 FROM schema_meta)
BEGIN
  SELECT RAISE(ABORT, 'schema metadata row already exists');
END;
CREATE TRIGGER IF NOT EXISTS schema_meta_update_block
BEFORE UPDATE ON schema_meta
BEGIN
  SELECT RAISE(ABORT, 'schema metadata is immutable');
END;
CREATE TRIGGER IF NOT EXISTS schema_meta_delete_block
BEFORE DELETE ON schema_meta
BEGIN
  SELECT RAISE(ABORT, 'schema metadata is immutable');
END;
CREATE TABLE IF NOT EXISTS users (
  user_id INTEGER PRIMARY KEY,
  username TEXT NOT NULL CHECK(
    instr(username, char(0)) = 0 AND
    length(CAST(username AS BLOB)) BETWEEN 1 AND 128
  ),
  wallet_addr TEXT CHECK(wallet_addr IS NULL OR (
    instr(wallet_addr, char(0)) = 0 AND
    length(CAST(wallet_addr AS BLOB)) BETWEEN 1 AND 128
  )),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS orders (
  order_id INTEGER PRIMARY KEY AUTOINCREMENT,
  side TEXT NOT NULL CHECK(side IN ('buy','sell')),
  maker_id INTEGER NOT NULL,
  maker_name TEXT NOT NULL CHECK(
    instr(maker_name, char(0)) = 0 AND
    length(CAST(maker_name AS BLOB)) BETWEEN 1 AND 128
  ),
  buyer_id INTEGER,
  buyer_name TEXT CHECK(buyer_name IS NULL OR (
    instr(buyer_name, char(0)) = 0 AND
    length(CAST(buyer_name AS BLOB)) BETWEEN 1 AND 128
  )),
  seller_id INTEGER,
  seller_name TEXT CHECK(seller_name IS NULL OR (
    instr(seller_name, char(0)) = 0 AND
    length(CAST(seller_name AS BLOB)) BETWEEN 1 AND 128
  )),
  net_amount_units INTEGER NOT NULL
    CHECK(net_amount_units BETWEEN 1 AND 2100000000000000),
  network_fee_units INTEGER NOT NULL
    CHECK(network_fee_units BETWEEN 0 AND 2100000000000000),
  service_fee_units INTEGER NOT NULL
    CHECK(service_fee_units BETWEEN 0 AND 2100000000000000),
  deposit_required_units INTEGER NOT NULL
    CHECK(deposit_required_units BETWEEN 1 AND 2100000000000000),
  total_price TEXT NOT NULL CHECK(
    instr(total_price, char(0)) = 0 AND
    length(CAST(total_price AS BLOB)) BETWEEN 1 AND 37 AND
    total_price NOT GLOB '*[^0-9.]*' AND
    substr(total_price, 1, 1) != '.' AND
    substr(total_price, -1, 1) != '.' AND
    (instr(total_price, '.') = 0 OR
      instr(substr(total_price, instr(total_price, '.') + 1), '.') = 0) AND
    (substr(total_price, 1, 1) != '0' OR
      (length(total_price) > 1 AND substr(total_price, 2, 1) = '.')) AND
    ((instr(total_price, '.') = 0 AND length(total_price) <= 18) OR
      (instr(total_price, '.') BETWEEN 2 AND 19 AND
       length(total_price) - instr(total_price, '.') BETWEEN 1 AND 18)) AND
    total_price GLOB '*[1-9]*'
  ),
  settlement_asset TEXT NOT NULL CHECK(
    instr(settlement_asset, char(0)) = 0 AND
    length(CAST(settlement_asset AS BLOB)) BETWEEN 2 AND 12 AND
    settlement_asset NOT GLOB '*[^A-Z0-9._-]*'
  ),
  settlement_network TEXT CHECK(settlement_network IS NULL OR (
    instr(settlement_network, char(0)) = 0 AND
    length(CAST(settlement_network AS BLOB)) BETWEEN 1 AND 48 AND
    settlement_network NOT GLOB '*[^A-Za-z0-9._ -]*'
  )),
  payment_method TEXT NOT NULL CHECK(
    instr(payment_method, char(0)) = 0 AND
    length(CAST(payment_method AS BLOB)) BETWEEN 1 AND 80 AND
    payment_method NOT GLOB '*[^ -~]*'
  ),
  state TEXT NOT NULL CHECK(state IN (
    'awaiting_deposit','open','matched','disputed','release_reserved',
    'refund_reserved','broadcast','completed','refunded','cancelled',
    'deposit_expired','recovery_hold','transfer_failed_safe','transfer_uncertain'
  )),
  deposit_addr TEXT CHECK(deposit_addr IS NULL OR (
    instr(deposit_addr, char(0)) = 0 AND
    length(CAST(deposit_addr AS BLOB)) BETWEEN 1 AND 128
  )),
  buyer_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(buyer_confirmed IN (0,1)),
  seller_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(seller_confirmed IN (0,1)),
  deposit_deadline INTEGER,
  matched_at INTEGER,
  trade_deadline INTEGER,
  disputed_at INTEGER,
  completed_at INTEGER,
  funded_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(order_id, deposit_addr),
  CHECK(deposit_required_units =
    net_amount_units + network_fee_units + service_fee_units),
  CHECK((buyer_id IS NULL AND buyer_name IS NULL) OR
        (buyer_id IS NOT NULL AND buyer_name IS NOT NULL)),
  CHECK((seller_id IS NULL AND seller_name IS NULL) OR
        (seller_id IS NOT NULL AND seller_name IS NOT NULL)),
  CHECK(
    (side = 'sell' AND seller_id = maker_id AND seller_name = maker_name) OR
    (side = 'buy' AND buyer_id = maker_id AND buyer_name = maker_name)
  ),
  CHECK(buyer_id IS NULL OR seller_id IS NULL OR buyer_id != seller_id),
  FOREIGN KEY(maker_id) REFERENCES users(user_id),
  FOREIGN KEY(buyer_id) REFERENCES users(user_id),
  FOREIGN KEY(seller_id) REFERENCES users(user_id)
) STRICT;
CREATE TABLE IF NOT EXISTS deposit_scans (
  scan_id INTEGER PRIMARY KEY AUTOINCREMENT,
  network TEXT NOT NULL CHECK(network IN ('btc09-mainnet','btc09-regtest')),
  address TEXT NOT NULL CHECK(
    instr(address, char(0)) = 0 AND
    length(CAST(address AS BLOB)) BETWEEN 1 AND 128
  ),
  tip_hash TEXT NOT NULL CHECK(
    instr(tip_hash, char(0)) = 0 AND
    length(CAST(tip_hash AS BLOB)) = 64 AND
    tip_hash NOT GLOB '*[^0-9a-f]*'
  ),
  tip_height INTEGER NOT NULL CHECK(tip_height >= 0),
  observed_at INTEGER NOT NULL,
  UNIQUE(scan_id, network, address)
) STRICT;
CREATE TABLE IF NOT EXISTS deposit_credits (
  credit_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL REFERENCES orders(order_id),
  network TEXT NOT NULL CHECK(network IN ('btc09-mainnet','btc09-regtest')),
  txid TEXT NOT NULL CHECK(
    instr(txid, char(0)) = 0 AND
    length(CAST(txid AS BLOB)) = 64 AND
    txid NOT GLOB '*[^0-9a-f]*'
  ),
  vout INTEGER NOT NULL CHECK(vout >= 0),
  deposit_addr TEXT NOT NULL CHECK(
    instr(deposit_addr, char(0)) = 0 AND
    length(CAST(deposit_addr AS BLOB)) BETWEEN 1 AND 128
  ),
  amount_units INTEGER NOT NULL
    CHECK(amount_units BETWEEN 1 AND 2100000000000000),
  block_hash TEXT NOT NULL CHECK(
    instr(block_hash, char(0)) = 0 AND
    length(CAST(block_hash AS BLOB)) = 64 AND
    block_hash NOT GLOB '*[^0-9a-f]*'
  ),
  block_height INTEGER NOT NULL CHECK(block_height >= 0),
  confirmations INTEGER NOT NULL CHECK(confirmations >= 1),
  coinbase INTEGER NOT NULL CHECK(coinbase IN (0,1)),
  mature INTEGER NOT NULL CHECK(mature IN (0,1)),
  current_best_chain INTEGER NOT NULL CHECK(current_best_chain IN (0,1)),
  spent_by_txid TEXT,
  spent_by_vin INTEGER,
  spent_by_block_hash TEXT,
  spent_by_block_height INTEGER,
  credited_at INTEGER,
  main_units INTEGER NOT NULL DEFAULT 0 CHECK(main_units >= 0),
  recovery_units INTEGER NOT NULL DEFAULT 0 CHECK(recovery_units >= 0),
  recovery_reason TEXT CHECK(
    recovery_reason IS NULL OR
    recovery_reason IN ('excess','late','cancelled_partial')
  ),
  first_seen_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  last_seen_scan_id INTEGER NOT NULL REFERENCES deposit_scans(scan_id),
  last_checked_scan_id INTEGER NOT NULL REFERENCES deposit_scans(scan_id),
  UNIQUE(network, txid, vout),
  UNIQUE(credit_id, order_id),
  CHECK(main_units + recovery_units <= amount_units),
  CHECK(credited_at IS NOT NULL OR main_units + recovery_units = 0),
  CHECK(credited_at IS NULL OR main_units + recovery_units = amount_units),
  CHECK(
    (recovery_units = 0 AND recovery_reason IS NULL) OR
    (recovery_units > 0 AND recovery_reason IS NOT NULL)
  ),
  CHECK(last_seen_at >= first_seen_at),
  CHECK(credited_at IS NULL OR credited_at >= first_seen_at),
  CHECK(current_best_chain = 0 OR last_seen_scan_id = last_checked_scan_id),
  CHECK(
    (spent_by_txid IS NULL AND spent_by_vin IS NULL AND
     spent_by_block_hash IS NULL AND spent_by_block_height IS NULL) OR
    (spent_by_txid IS NOT NULL AND spent_by_vin IS NOT NULL AND
     spent_by_block_hash IS NOT NULL AND spent_by_block_height IS NOT NULL AND
     instr(spent_by_txid, char(0)) = 0 AND
     length(CAST(spent_by_txid AS BLOB)) = 64 AND
     spent_by_txid NOT GLOB '*[^0-9a-f]*' AND
     spent_by_vin >= 0 AND
     instr(spent_by_block_hash, char(0)) = 0 AND
     length(CAST(spent_by_block_hash AS BLOB)) = 64 AND
     spent_by_block_hash NOT GLOB '*[^0-9a-f]*' AND
     spent_by_block_height >= 0)
  ),
  FOREIGN KEY(order_id, deposit_addr)
    REFERENCES orders(order_id, deposit_addr),
  FOREIGN KEY(last_seen_scan_id, network, deposit_addr)
    REFERENCES deposit_scans(scan_id, network, address),
  FOREIGN KEY(last_checked_scan_id, network, deposit_addr)
    REFERENCES deposit_scans(scan_id, network, address)
) STRICT;
CREATE TABLE IF NOT EXISTS transfers (
  transfer_id INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_key TEXT NOT NULL UNIQUE CHECK(
    instr(operation_key, char(0)) = 0 AND
    length(CAST(operation_key AS BLOB)) BETWEEN 1 AND 160 AND
    operation_key NOT GLOB '*[^a-z0-9:_-]*'
  ),
  order_id INTEGER REFERENCES orders(order_id),
  wallet_scope TEXT NOT NULL DEFAULT 'escrow' CHECK(wallet_scope = 'escrow'),
  kind TEXT NOT NULL CHECK(kind IN (
    'release','refund','resolve_buyer','resolve_seller','recovery_refund',
    'fee_withdrawal'
  )),
  is_main_outcome INTEGER NOT NULL CHECK(is_main_outcome IN (0,1)),
  state TEXT NOT NULL CHECK(state IN (
    'queued','reserved','prepared','broadcast','confirmed','failed_safe',
    'uncertain','cancelled'
  )),
  amount_units INTEGER NOT NULL
    CHECK(amount_units BETWEEN 1 AND 2100000000000000),
  network_fee_units INTEGER NOT NULL
    CHECK(network_fee_units BETWEEN 0 AND 2100000000000000),
  earned_fee_units INTEGER NOT NULL DEFAULT 0
    CHECK(earned_fee_units BETWEEN 0 AND 2100000000000000),
  destination TEXT NOT NULL CHECK(
    instr(destination, char(0)) = 0 AND
    length(CAST(destination AS BLOB)) BETWEEN 1 AND 128
  ),
  txid TEXT CHECK(
    txid IS NULL OR (
      instr(txid, char(0)) = 0 AND
      length(CAST(txid AS BLOB)) = 64 AND
      txid NOT GLOB '*[^0-9a-f]*'
    )
  ),
  signed_tx_hex TEXT CHECK(
    signed_tx_hex IS NULL OR
    (instr(signed_tx_hex, char(0)) = 0 AND
     length(CAST(signed_tx_hex AS BLOB)) BETWEEN 2 AND 20000 AND
     length(CAST(signed_tx_hex AS BLOB)) % 2 = 0 AND
     signed_tx_hex NOT GLOB '*[^0-9a-f]*')
  ),
  prepared_tip_hash TEXT CHECK(
    prepared_tip_hash IS NULL OR (
      instr(prepared_tip_hash, char(0)) = 0 AND
      length(CAST(prepared_tip_hash AS BLOB)) = 64 AND
      prepared_tip_hash NOT GLOB '*[^0-9a-f]*'
    )
  ),
  prepared_tip_height INTEGER CHECK(
    prepared_tip_height IS NULL OR prepared_tip_height >= 0
  ),
  result_class TEXT CHECK(
    result_class IS NULL OR
    result_class IN ('broadcast','safe_to_retry','uncertain')
  ),
  error_text TEXT CHECK(error_text IS NULL OR (
    instr(error_text, char(0)) = 0 AND
    length(CAST(error_text AS BLOB)) <= 500
  )),
  attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
  reserved_at INTEGER,
  signed_at INTEGER,
  broadcast_at INTEGER,
  confirmed_at INTEGER,
  confirmed_block_hash TEXT CHECK(
    confirmed_block_hash IS NULL OR
    (instr(confirmed_block_hash, char(0)) = 0 AND
     length(CAST(confirmed_block_hash AS BLOB)) = 64 AND
     confirmed_block_hash NOT GLOB '*[^0-9a-f]*')
  ),
  confirmed_block_height INTEGER,
  confirmations INTEGER NOT NULL DEFAULT 0 CHECK(confirmations >= 0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK(
    (kind = 'fee_withdrawal' AND order_id IS NULL) OR
    (kind != 'fee_withdrawal' AND order_id IS NOT NULL)
  ),
  CHECK(
    (is_main_outcome = 1 AND kind IN (
      'release','refund','resolve_buyer','resolve_seller'
    )) OR
    (is_main_outcome = 0 AND kind IN ('recovery_refund','fee_withdrawal'))
  ),
  CHECK(earned_fee_units = 0 OR kind IN ('release','resolve_buyer')),
  CHECK(confirmed_block_height IS NULL OR confirmed_block_height >= 0),
  CHECK(
    state NOT IN ('queued','reserved','failed_safe','cancelled') OR
    (txid IS NULL AND signed_tx_hex IS NULL AND signed_at IS NULL AND
     prepared_tip_hash IS NULL AND prepared_tip_height IS NULL)
  ),
  CHECK(
    state NOT IN ('prepared','broadcast','confirmed','uncertain') OR
    (txid IS NOT NULL AND signed_tx_hex IS NOT NULL AND signed_at IS NOT NULL AND
     prepared_tip_hash IS NOT NULL AND prepared_tip_height IS NOT NULL)
  ),
  CHECK(state != 'broadcast' OR txid IS NOT NULL),
  CHECK(
    state != 'confirmed' OR
    (txid IS NOT NULL AND confirmed_at IS NOT NULL AND
     confirmed_block_hash IS NOT NULL AND confirmed_block_height IS NOT NULL AND
     confirmations >= 1)
  ),
  UNIQUE(transfer_id, order_id)
) STRICT;
CREATE TABLE IF NOT EXISTS transfer_credit_allocations (
  transfer_id INTEGER NOT NULL REFERENCES transfers(transfer_id),
  credit_id INTEGER NOT NULL REFERENCES deposit_credits(credit_id),
  order_id INTEGER NOT NULL REFERENCES orders(order_id),
  bucket TEXT NOT NULL CHECK(bucket IN ('main','recovery')),
  units INTEGER NOT NULL CHECK(units BETWEEN 1 AND 2100000000000000),
  PRIMARY KEY(transfer_id, credit_id, bucket),
  FOREIGN KEY(transfer_id, order_id)
    REFERENCES transfers(transfer_id, order_id),
  FOREIGN KEY(credit_id, order_id)
    REFERENCES deposit_credits(credit_id, order_id)
) STRICT;
CREATE INDEX IF NOT EXISTS orders_by_state ON orders(state, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS one_order_per_deposit_address
ON orders(deposit_addr) WHERE deposit_addr IS NOT NULL;
CREATE INDEX IF NOT EXISTS deposit_scans_by_address
ON deposit_scans(network, address, scan_id DESC);
CREATE INDEX IF NOT EXISTS deposit_credits_by_order
ON deposit_credits(order_id, credited_at);
CREATE INDEX IF NOT EXISTS deposit_credits_by_address
ON deposit_credits(network, deposit_addr, current_best_chain);
CREATE UNIQUE INDEX IF NOT EXISTS one_main_outcome_per_order
ON transfers(order_id) WHERE is_main_outcome = 1;
CREATE UNIQUE INDEX IF NOT EXISTS one_transfer_per_txid
ON transfers(txid) WHERE txid IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS one_unfinished_recovery_per_order
ON transfers(order_id)
WHERE kind = 'recovery_refund' AND state NOT IN ('confirmed','cancelled');
CREATE UNIQUE INDEX IF NOT EXISTS one_wallet_send_in_flight
ON transfers(wallet_scope) WHERE state IN ('reserved','prepared','broadcast');
CREATE INDEX IF NOT EXISTS transfers_queue
ON transfers(state, created_at, transfer_id);
CREATE INDEX IF NOT EXISTS transfer_allocations_by_credit
ON transfer_credit_allocations(credit_id);
CREATE TRIGGER IF NOT EXISTS order_insert_invariant
BEFORE INSERT ON orders
WHEN NOT (
  NEW.side = 'sell'
  AND NEW.state = 'awaiting_deposit'
  AND NEW.seller_id = NEW.maker_id
  AND NEW.seller_name = NEW.maker_name
  AND NEW.buyer_id IS NULL
  AND NEW.buyer_name IS NULL
  AND NEW.deposit_addr IS NOT NULL
  AND NEW.buyer_confirmed = 0
  AND NEW.seller_confirmed = 0
) AND NOT (
  NEW.side = 'buy'
  AND NEW.state = 'open'
  AND NEW.buyer_id = NEW.maker_id
  AND NEW.buyer_name = NEW.maker_name
  AND NEW.seller_id IS NULL
  AND NEW.seller_name IS NULL
  AND NEW.deposit_addr IS NULL
  AND NEW.buyer_confirmed = 0
  AND NEW.seller_confirmed = 0
)
BEGIN
  SELECT RAISE(ABORT, 'invalid initial order roles or state');
END;
CREATE TRIGGER IF NOT EXISTS order_quote_immutable
BEFORE UPDATE ON orders
WHEN NEW.side IS NOT OLD.side
  OR NEW.maker_id IS NOT OLD.maker_id
  OR NEW.maker_name IS NOT OLD.maker_name
  OR NEW.net_amount_units IS NOT OLD.net_amount_units
  OR NEW.network_fee_units IS NOT OLD.network_fee_units
  OR NEW.service_fee_units IS NOT OLD.service_fee_units
  OR NEW.deposit_required_units IS NOT OLD.deposit_required_units
  OR NEW.total_price IS NOT OLD.total_price
  OR NEW.settlement_asset IS NOT OLD.settlement_asset
  OR NEW.settlement_network IS NOT OLD.settlement_network
  OR NEW.payment_method IS NOT OLD.payment_method
  OR NEW.created_at IS NOT OLD.created_at
  OR (OLD.deposit_addr IS NOT NULL AND NEW.deposit_addr IS NOT OLD.deposit_addr)
BEGIN
  SELECT RAISE(ABORT, 'order quote and deposit identity are immutable');
END;
CREATE TRIGGER IF NOT EXISTS order_participant_transition_guard
BEFORE UPDATE ON orders
WHEN (
  OLD.side = 'sell' AND NOT (
    (NEW.buyer_id IS OLD.buyer_id AND NEW.buyer_name IS OLD.buyer_name) OR
    (OLD.buyer_id IS NULL AND OLD.buyer_name IS NULL
      AND NEW.buyer_id IS NOT NULL AND NEW.buyer_name IS NOT NULL
      AND NEW.buyer_id != OLD.maker_id
      AND OLD.state = 'open' AND NEW.state = 'matched'
      AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
      AND NEW.buyer_confirmed = 0 AND NEW.seller_confirmed = 0) OR
    (OLD.buyer_id IS NOT NULL AND OLD.buyer_name IS NOT NULL
      AND NEW.buyer_id IS NULL AND NEW.buyer_name IS NULL
      AND OLD.state = 'matched' AND NEW.state = 'open'
      AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
      AND NEW.buyer_confirmed = 0 AND NEW.seller_confirmed = 0)
  )
) OR (
  OLD.side = 'buy' AND NOT (
    (NEW.seller_id IS OLD.seller_id AND NEW.seller_name IS OLD.seller_name) OR
    (OLD.seller_id IS NULL AND OLD.seller_name IS NULL
      AND NEW.seller_id IS NOT NULL AND NEW.seller_name IS NOT NULL
      AND NEW.seller_id != OLD.maker_id
      AND OLD.state = 'open' AND NEW.state = 'awaiting_deposit'
      AND OLD.deposit_addr IS NULL AND NEW.deposit_addr IS NOT NULL
      AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
      AND NEW.buyer_confirmed = 0 AND NEW.seller_confirmed = 0)
  )
)
BEGIN
  SELECT RAISE(ABORT, 'invalid order participant transition');
END;
CREATE TRIGGER IF NOT EXISTS order_deposit_assignment_guard
BEFORE UPDATE OF deposit_addr ON orders
WHEN NEW.deposit_addr IS NOT OLD.deposit_addr AND NOT (
  OLD.side = 'buy'
  AND OLD.deposit_addr IS NULL
  AND NEW.deposit_addr IS NOT NULL
  AND OLD.state = 'open'
  AND NEW.state = 'awaiting_deposit'
  AND OLD.seller_id IS NULL
  AND OLD.seller_name IS NULL
  AND NEW.seller_id IS NOT NULL
  AND NEW.seller_name IS NOT NULL
)
BEGIN
  SELECT RAISE(ABORT, 'deposit address may only attach with first WTB seller');
END;
CREATE TRIGGER IF NOT EXISTS order_confirmation_guard
BEFORE UPDATE OF buyer_confirmed, seller_confirmed ON orders
WHEN NEW.buyer_confirmed != OLD.buyer_confirmed
  OR NEW.seller_confirmed != OLD.seller_confirmed
BEGIN
  SELECT CASE WHEN OLD.state != 'matched'
      OR NEW.buyer_confirmed < OLD.buyer_confirmed
      OR NEW.seller_confirmed < OLD.seller_confirmed
      OR NEW.buyer_confirmed + NEW.seller_confirmed
         != OLD.buyer_confirmed + OLD.seller_confirmed + 1
      OR (NEW.buyer_confirmed + NEW.seller_confirmed = 1
          AND NEW.state != 'matched')
      OR (NEW.buyer_confirmed + NEW.seller_confirmed = 2
          AND NEW.state != 'release_reserved')
    THEN RAISE(ABORT, 'invalid payment confirmation transition') END;
END;
CREATE TRIGGER IF NOT EXISTS order_state_machine
BEFORE UPDATE OF state ON orders
WHEN NEW.state IS NOT OLD.state AND NOT (
  (OLD.side = 'sell' AND OLD.state = 'awaiting_deposit'
    AND (
      (NEW.state = 'open' AND COALESCE((
        SELECT SUM(c.main_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) >= OLD.deposit_required_units) OR
      (NEW.state IN ('cancelled','deposit_expired') AND COALESCE((
        SELECT SUM(c.amount_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) = 0) OR
      (NEW.state = 'recovery_hold'
        AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
        AND COALESCE((SELECT SUM(c.amount_units) FROM deposit_credits c
          WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL), 0)
            BETWEEN 1 AND OLD.deposit_required_units - 1) OR
      (NEW.state = 'refund_reserved' AND EXISTS (
        SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
          AND t.kind IN ('refund','recovery_refund') AND t.state = 'queued'
          AND COALESCE((SELECT SUM(a.units)
            FROM transfer_credit_allocations a
            WHERE a.transfer_id = t.transfer_id), 0)
              = t.amount_units + t.network_fee_units + t.earned_fee_units
      ))
    )) OR
  (OLD.side = 'sell' AND OLD.state = 'open'
    AND ((NEW.state = 'matched' AND NEW.buyer_id IS NOT NULL)
      OR (NEW.state = 'refund_reserved' AND EXISTS (
        SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
          AND t.kind = 'refund' AND t.state = 'queued'
          AND COALESCE((SELECT SUM(a.units)
            FROM transfer_credit_allocations a
            WHERE a.transfer_id = t.transfer_id), 0)
              = t.amount_units + t.network_fee_units + t.earned_fee_units
      )))) OR
  (OLD.side = 'sell' AND OLD.state = 'matched'
    AND ((NEW.state = 'open' AND NEW.buyer_id IS NULL
          AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0)
      OR (NEW.state = 'release_reserved'
          AND NEW.buyer_confirmed = 1 AND NEW.seller_confirmed = 1
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'release' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units))
      OR NEW.state = 'disputed'
      OR (NEW.state = 'refund_reserved'
          AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'refund' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units)))) OR
  (OLD.side = 'buy' AND OLD.state = 'open'
    AND ((NEW.state = 'awaiting_deposit' AND NEW.seller_id IS NOT NULL
          AND NEW.deposit_addr IS NOT NULL)
      OR NEW.state = 'cancelled')) OR
  (OLD.side = 'buy' AND OLD.state = 'awaiting_deposit'
    AND (
      (NEW.state = 'matched' AND COALESCE((
        SELECT SUM(c.main_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) >= OLD.deposit_required_units) OR
      (NEW.state IN ('cancelled','deposit_expired') AND COALESCE((
        SELECT SUM(c.amount_units) FROM deposit_credits c
        WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL
      ), 0) = 0) OR
      (NEW.state = 'recovery_hold'
        AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
        AND COALESCE((SELECT SUM(c.amount_units) FROM deposit_credits c
          WHERE c.order_id = OLD.order_id AND c.credited_at IS NOT NULL), 0)
            BETWEEN 1 AND OLD.deposit_required_units - 1) OR
      (NEW.state = 'refund_reserved' AND EXISTS (
        SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
          AND t.kind IN ('refund','recovery_refund') AND t.state = 'queued'
          AND COALESCE((SELECT SUM(a.units)
            FROM transfer_credit_allocations a
            WHERE a.transfer_id = t.transfer_id), 0)
              = t.amount_units + t.network_fee_units + t.earned_fee_units
      ))
    )) OR
  (OLD.side = 'buy' AND OLD.state = 'matched'
    AND ((NEW.state = 'release_reserved'
          AND NEW.buyer_confirmed = 1 AND NEW.seller_confirmed = 1
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'release' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units))
      OR NEW.state = 'disputed'
      OR (NEW.state = 'refund_reserved'
          AND OLD.buyer_confirmed = 0 AND OLD.seller_confirmed = 0
          AND EXISTS (SELECT 1 FROM transfers t
            WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
              AND t.kind = 'refund' AND t.state = 'queued'
              AND COALESCE((SELECT SUM(a.units)
                FROM transfer_credit_allocations a
                WHERE a.transfer_id = t.transfer_id), 0)
                  = t.amount_units + t.network_fee_units + t.earned_fee_units)))) OR
  (OLD.state = 'disputed' AND (
    (NEW.state = 'release_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind = 'resolve_buyer' AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    )) OR
    (NEW.state = 'refund_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind = 'resolve_seller' AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    ))
  )) OR
  (OLD.state = 'release_reserved'
    AND NEW.state IN ('broadcast','completed','transfer_failed_safe','transfer_uncertain')) OR
  (OLD.state = 'refund_reserved'
    AND NEW.state IN ('broadcast','refunded','transfer_failed_safe','transfer_uncertain')) OR
  (OLD.state = 'broadcast'
    AND NEW.state IN ('completed','refunded','transfer_uncertain')) OR
  (OLD.state = 'transfer_failed_safe' AND (
    (NEW.state = 'release_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind IN ('release','resolve_buyer') AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    )) OR
    (NEW.state = 'refund_reserved' AND EXISTS (
      SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
        AND t.kind IN ('refund','resolve_seller','recovery_refund')
        AND t.state = 'queued'
        AND COALESCE((SELECT SUM(a.units)
          FROM transfer_credit_allocations a
          WHERE a.transfer_id = t.transfer_id), 0)
            = t.amount_units + t.network_fee_units + t.earned_fee_units
    ))
  )) OR
  (OLD.state = 'transfer_uncertain'
    AND NEW.state IN ('broadcast','completed','refunded')) OR
  (OLD.state IN ('completed','refunded')
    AND NEW.state = 'transfer_uncertain') OR
  (OLD.state = 'recovery_hold' AND NEW.state = 'refund_reserved'
    AND EXISTS (SELECT 1 FROM transfers t WHERE t.order_id = OLD.order_id
      AND t.kind = 'recovery_refund' AND t.state = 'queued'
      AND COALESCE((SELECT SUM(a.units)
        FROM transfer_credit_allocations a
        WHERE a.transfer_id = t.transfer_id), 0)
          = t.amount_units + t.network_fee_units + t.earned_fee_units))
)
BEGIN
  SELECT RAISE(ABORT, 'invalid order state transition');
END;
CREATE TRIGGER IF NOT EXISTS order_delete_block
BEFORE DELETE ON orders
BEGIN
  SELECT RAISE(ABORT, 'orders are append-only');
END;
CREATE TRIGGER IF NOT EXISTS deposit_scan_update_block
BEFORE UPDATE ON deposit_scans
BEGIN
  SELECT RAISE(ABORT, 'deposit scans are append-only');
END;
CREATE TRIGGER IF NOT EXISTS deposit_scan_delete_block
BEFORE DELETE ON deposit_scans
BEGIN
  SELECT RAISE(ABORT, 'deposit scans are append-only');
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_identity_immutable
BEFORE UPDATE ON deposit_credits
WHEN NEW.order_id IS NOT OLD.order_id
  OR NEW.network IS NOT OLD.network
  OR NEW.txid IS NOT OLD.txid
  OR NEW.vout IS NOT OLD.vout
  OR NEW.deposit_addr IS NOT OLD.deposit_addr
  OR NEW.amount_units IS NOT OLD.amount_units
  OR NEW.coinbase IS NOT OLD.coinbase
  OR NEW.first_seen_at IS NOT OLD.first_seen_at
  OR (OLD.credited_at IS NOT NULL AND NEW.credited_at IS NOT OLD.credited_at)
BEGIN
  SELECT RAISE(ABORT, 'deposit credit identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_classification_guard
BEFORE UPDATE OF credited_at, main_units, recovery_units, recovery_reason
ON deposit_credits
WHEN NOT (
  NEW.credited_at IS OLD.credited_at
  AND NEW.main_units = OLD.main_units
  AND NEW.recovery_units = OLD.recovery_units
  AND NEW.recovery_reason IS OLD.recovery_reason
) AND NOT (
  OLD.credited_at IS NULL
  AND NEW.credited_at IS NOT NULL
  AND OLD.main_units = 0
  AND OLD.recovery_units = 0
  AND NEW.main_units + NEW.recovery_units = OLD.amount_units
) AND NOT (
  OLD.credited_at IS NOT NULL
  AND NEW.credited_at = OLD.credited_at
  AND OLD.main_units > 0
  AND NEW.main_units = 0
  AND NEW.recovery_units = OLD.main_units + OLD.recovery_units
  AND NEW.recovery_reason = 'cancelled_partial'
  AND EXISTS (
    SELECT 1 FROM orders o
    WHERE o.order_id = OLD.order_id
      AND o.state IN ('refund_reserved','recovery_hold')
      AND o.buyer_confirmed = 0
      AND o.seller_confirmed = 0
      AND COALESCE((
        SELECT SUM(c.amount_units)
        FROM deposit_credits c
        WHERE c.order_id = o.order_id AND c.credited_at IS NOT NULL
      ), 0) < o.deposit_required_units
  )
  AND NOT EXISTS (
    SELECT 1 FROM transfer_credit_allocations a
    WHERE a.credit_id = OLD.credit_id AND a.bucket = 'main'
  )
  AND NOT EXISTS (
    SELECT 1 FROM transfers t
    WHERE t.order_id = OLD.order_id AND t.is_main_outcome = 1
  )
)
BEGIN
  SELECT RAISE(ABORT, 'invalid deposit credit classification change');
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_observation_guard
BEFORE UPDATE ON deposit_credits
WHEN NEW.block_hash IS NOT OLD.block_hash
  OR NEW.block_height IS NOT OLD.block_height
  OR NEW.confirmations IS NOT OLD.confirmations
  OR NEW.mature IS NOT OLD.mature
  OR NEW.current_best_chain IS NOT OLD.current_best_chain
  OR NEW.spent_by_txid IS NOT OLD.spent_by_txid
  OR NEW.spent_by_vin IS NOT OLD.spent_by_vin
  OR NEW.spent_by_block_hash IS NOT OLD.spent_by_block_hash
  OR NEW.spent_by_block_height IS NOT OLD.spent_by_block_height
  OR NEW.last_seen_at IS NOT OLD.last_seen_at
  OR NEW.last_seen_scan_id IS NOT OLD.last_seen_scan_id
  OR NEW.last_checked_scan_id IS NOT OLD.last_checked_scan_id
BEGIN
  SELECT CASE WHEN NEW.last_checked_scan_id <= OLD.last_checked_scan_id
    THEN RAISE(ABORT, 'credit observation requires a newer scan') END;
  SELECT CASE WHEN NEW.last_seen_at < OLD.last_seen_at
    THEN RAISE(ABORT, 'credit last-seen time cannot move backwards') END;
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_bucket_capacity_guard
BEFORE UPDATE OF main_units, recovery_units ON deposit_credits
BEGIN
  SELECT CASE WHEN NEW.main_units < COALESCE((
    SELECT SUM(a.units) FROM transfer_credit_allocations a
    WHERE a.credit_id = OLD.credit_id AND a.bucket = 'main'
  ), 0) THEN RAISE(ABORT, 'main credit capacity is allocated') END;
  SELECT CASE WHEN NEW.recovery_units < COALESCE((
    SELECT SUM(a.units) FROM transfer_credit_allocations a
    WHERE a.credit_id = OLD.credit_id AND a.bucket = 'recovery'
  ), 0) THEN RAISE(ABORT, 'recovery credit capacity is allocated') END;
END;
CREATE TRIGGER IF NOT EXISTS deposit_credit_delete_block
BEFORE DELETE ON deposit_credits
BEGIN
  SELECT RAISE(ABORT, 'deposit credits are append-only');
END;
CREATE TRIGGER IF NOT EXISTS transfer_operation_key_lifetime_guard
BEFORE INSERT ON transfers
WHEN EXISTS (
  SELECT 1 FROM transfers t WHERE t.operation_key = NEW.operation_key
)
BEGIN
  SELECT RAISE(ABORT, 'transfer operation key already exists');
END;
CREATE TRIGGER IF NOT EXISTS transfer_insert_must_queue
BEFORE INSERT ON transfers
WHEN NEW.state != 'queued'
  OR NEW.attempt_count != 0
  OR NEW.reserved_at IS NOT NULL
  OR NEW.signed_at IS NOT NULL
  OR NEW.broadcast_at IS NOT NULL
  OR NEW.confirmed_at IS NOT NULL
  OR NEW.confirmed_block_hash IS NOT NULL
  OR NEW.confirmed_block_height IS NOT NULL
  OR NEW.confirmations != 0
BEGIN
  SELECT RAISE(ABORT, 'new transfer must be a clean queued operation');
END;
CREATE TRIGGER IF NOT EXISTS transfer_economics_on_insert
BEFORE INSERT ON transfers
BEGIN
  SELECT CASE WHEN EXISTS (
    SELECT 1 FROM orders o
    WHERE o.deposit_addr IS NOT NULL AND o.deposit_addr = NEW.destination
  ) THEN RAISE(ABORT, 'destination is an escrow deposit address') END;
  SELECT CASE WHEN NEW.kind IN ('release','resolve_buyer') AND NOT EXISTS (
    SELECT 1 FROM orders o JOIN users u ON u.user_id = o.buyer_id
    WHERE o.order_id = NEW.order_id
      AND NEW.operation_key = 'order:' || o.order_id || ':main'
      AND NEW.amount_units = o.net_amount_units
      AND NEW.network_fee_units = o.network_fee_units
      AND NEW.earned_fee_units = o.service_fee_units
      AND NEW.destination = u.wallet_addr
  ) THEN RAISE(ABORT, 'invalid buyer outcome economics') END;
  SELECT CASE WHEN NEW.kind IN ('refund','resolve_seller') AND NOT EXISTS (
    SELECT 1 FROM orders o JOIN users u ON u.user_id = o.seller_id
    WHERE o.order_id = NEW.order_id
      AND NEW.operation_key = 'order:' || o.order_id || ':main'
      AND NEW.amount_units = o.net_amount_units + o.service_fee_units
      AND NEW.network_fee_units = o.network_fee_units
      AND NEW.earned_fee_units = 0
      AND NEW.destination = u.wallet_addr
  ) THEN RAISE(ABORT, 'invalid seller outcome economics') END;
  SELECT CASE WHEN NEW.kind = 'recovery_refund' AND NOT EXISTS (
    SELECT 1 FROM orders o JOIN users u ON u.user_id = o.seller_id
    WHERE o.order_id = NEW.order_id
      AND NEW.operation_key = 'order:' || o.order_id || ':recovery:' || (
        SELECT MAX(c.credit_id) FROM deposit_credits c
        WHERE c.order_id = o.order_id AND c.credited_at IS NOT NULL
          AND c.recovery_units > COALESCE((
            SELECT SUM(a.units) FROM transfer_credit_allocations a
            WHERE a.credit_id = c.credit_id AND a.bucket = 'recovery'
          ), 0)
      )
      AND NEW.network_fee_units = o.network_fee_units
      AND NEW.earned_fee_units = 0
      AND NEW.destination = u.wallet_addr
      AND NEW.amount_units + NEW.network_fee_units =
        COALESCE((SELECT SUM(c.recovery_units)
                  FROM deposit_credits c
                  WHERE c.order_id = NEW.order_id
                    AND c.credited_at IS NOT NULL), 0)
        - COALESCE((SELECT SUM(a.units)
                    FROM transfer_credit_allocations a
                    JOIN transfers t ON t.transfer_id = a.transfer_id
                    WHERE t.order_id = NEW.order_id
                      AND a.bucket = 'recovery'), 0)
  ) THEN RAISE(ABORT, 'invalid recovery economics') END;
  SELECT CASE WHEN NEW.kind = 'fee_withdrawal' AND (
    NEW.earned_fee_units != 0 OR
    NEW.amount_units + NEW.network_fee_units >
      COALESCE((SELECT SUM(earned_fee_units) FROM transfers
                WHERE state = 'confirmed'
                  AND kind IN ('release','resolve_buyer')), 0)
      - COALESCE((SELECT SUM(amount_units + network_fee_units) FROM transfers
                  WHERE kind = 'fee_withdrawal' AND state != 'cancelled'), 0)
  ) THEN RAISE(ABORT, 'fee withdrawal exceeds earned revenue') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_economics_immutable
BEFORE UPDATE ON transfers
WHEN NEW.operation_key IS NOT OLD.operation_key
  OR NEW.order_id IS NOT OLD.order_id
  OR NEW.wallet_scope IS NOT OLD.wallet_scope
  OR NEW.kind IS NOT OLD.kind
  OR NEW.is_main_outcome IS NOT OLD.is_main_outcome
  OR NEW.amount_units IS NOT OLD.amount_units
  OR NEW.network_fee_units IS NOT OLD.network_fee_units
  OR NEW.earned_fee_units IS NOT OLD.earned_fee_units
  OR NEW.destination IS NOT OLD.destination
BEGIN
  SELECT RAISE(ABORT, 'transfer economics are immutable');
END;
CREATE TRIGGER IF NOT EXISTS signed_transfer_immutable
BEFORE UPDATE ON transfers
WHEN OLD.signed_tx_hex IS NOT NULL AND (
  NEW.txid IS NOT OLD.txid OR
  NEW.signed_tx_hex IS NOT OLD.signed_tx_hex OR
  NEW.signed_at IS NOT OLD.signed_at OR
  NEW.prepared_tip_hash IS NOT OLD.prepared_tip_hash OR
  NEW.prepared_tip_height IS NOT OLD.prepared_tip_height
)
BEGIN
  SELECT RAISE(ABORT, 'prepared transaction identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS transfer_timeline_guard
BEFORE UPDATE ON transfers
BEGIN
  SELECT CASE WHEN NEW.created_at IS NOT OLD.created_at
    OR NEW.updated_at < OLD.updated_at
    THEN RAISE(ABORT, 'invalid transfer timestamps') END;
  SELECT CASE WHEN NEW.attempt_count != OLD.attempt_count +
    CASE WHEN OLD.state = 'queued' AND NEW.state = 'reserved' THEN 1 ELSE 0 END
    THEN RAISE(ABORT, 'invalid transfer attempt count') END;
  SELECT CASE WHEN OLD.state = 'queued' AND NEW.state = 'reserved' AND (
      NEW.reserved_at IS NULL OR NEW.reserved_at < OLD.updated_at
    ) THEN RAISE(ABORT, 'claim requires reservation time') END;
  SELECT CASE WHEN NEW.reserved_at IS NOT OLD.reserved_at AND NOT (
      OLD.state = 'queued' AND NEW.state = 'reserved' AND OLD.reserved_at IS NULL
    ) AND NOT (
      OLD.state = 'failed_safe' AND NEW.state = 'queued'
      AND NEW.reserved_at IS NULL
    ) THEN RAISE(ABORT, 'invalid reservation time change') END;
  SELECT CASE WHEN OLD.state = 'reserved' AND NEW.state = 'prepared' AND (
      NEW.signed_at IS NULL OR NEW.txid IS NULL OR NEW.signed_tx_hex IS NULL OR
      NEW.prepared_tip_hash IS NULL OR NEW.prepared_tip_height IS NULL
    ) THEN RAISE(ABORT, 'prepare requires complete durable identity') END;
  SELECT CASE WHEN OLD.signed_tx_hex IS NULL AND NEW.signed_tx_hex IS NOT NULL
      AND NOT (OLD.state = 'reserved' AND NEW.state = 'prepared')
    THEN RAISE(ABORT, 'signed identity may only attach on prepare') END;
  SELECT CASE WHEN OLD.broadcast_at IS NOT NULL
      AND NEW.broadcast_at IS NOT OLD.broadcast_at
    THEN RAISE(ABORT, 'broadcast time is immutable') END;
  SELECT CASE WHEN NEW.state = 'broadcast' AND OLD.state != 'broadcast'
      AND NEW.broadcast_at IS NULL
    THEN RAISE(ABORT, 'trusted-node observation time required') END;
  SELECT CASE WHEN OLD.broadcast_at IS NULL AND NEW.broadcast_at IS NOT NULL
      AND NEW.state != 'broadcast'
    THEN RAISE(ABORT, 'broadcast time requires broadcast state') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_state_machine
BEFORE UPDATE OF state ON transfers
WHEN NEW.state IS NOT OLD.state AND NOT (
  (OLD.state = 'queued' AND NEW.state = 'reserved') OR
  (OLD.state = 'reserved' AND NEW.state IN ('prepared','failed_safe')) OR
  (OLD.state = 'prepared' AND NEW.state IN ('broadcast','confirmed','uncertain')) OR
  (OLD.state = 'broadcast' AND NEW.state IN ('confirmed','uncertain')) OR
  (OLD.state = 'uncertain' AND NEW.state IN ('broadcast','confirmed')) OR
  (OLD.state = 'confirmed' AND NEW.state = 'uncertain') OR
  (OLD.state = 'failed_safe' AND NEW.state = 'queued') OR
  (OLD.state = 'failed_safe' AND NEW.state = 'cancelled'
    AND OLD.kind = 'fee_withdrawal')
)
BEGIN
  SELECT RAISE(ABORT, 'invalid transfer state transition');
END;
CREATE TRIGGER IF NOT EXISTS uncertain_blocks_prebroadcast_progress
BEFORE UPDATE OF state ON transfers
WHEN (
  (OLD.state = 'reserved' AND NEW.state = 'prepared') OR
  (OLD.state = 'prepared' AND NEW.state = 'broadcast')
) AND EXISTS (
  SELECT 1 FROM transfers t WHERE t.state = 'uncertain'
)
BEGIN
  SELECT RAISE(ABORT, 'uncertain transfer blocks pre-broadcast progress');
END;
CREATE TRIGGER IF NOT EXISTS allocation_insert_guard
BEFORE INSERT ON transfer_credit_allocations
BEGIN
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM transfers t
    WHERE t.transfer_id = NEW.transfer_id
      AND t.order_id = NEW.order_id
      AND t.kind != 'fee_withdrawal'
      AND t.state = 'queued'
      AND ((t.is_main_outcome = 1 AND NEW.bucket = 'main') OR
           (t.kind = 'recovery_refund' AND NEW.bucket = 'recovery'))
  ) THEN RAISE(ABORT, 'invalid transfer allocation target') END;
  SELECT CASE WHEN NEW.units + COALESCE((
    SELECT SUM(a.units) FROM transfer_credit_allocations a
    WHERE a.credit_id = NEW.credit_id AND a.bucket = NEW.bucket
  ), 0) > COALESCE((
    SELECT CASE NEW.bucket
      WHEN 'main' THEN c.main_units ELSE c.recovery_units END
    FROM deposit_credits c
    WHERE c.credit_id = NEW.credit_id
      AND c.order_id = NEW.order_id
      AND c.credited_at IS NOT NULL
  ), -1) THEN RAISE(ABORT, 'credit bucket over-allocation') END;
END;
CREATE TRIGGER IF NOT EXISTS allocation_immutable_update
BEFORE UPDATE ON transfer_credit_allocations
BEGIN
  SELECT RAISE(ABORT, 'transfer allocations are immutable');
END;
CREATE TRIGGER IF NOT EXISTS allocation_immutable_delete
BEFORE DELETE ON transfer_credit_allocations
BEGIN
  SELECT RAISE(ABORT, 'transfer allocations are immutable');
END;
CREATE TRIGGER IF NOT EXISTS transfer_delete_block
BEFORE DELETE ON transfers
BEGIN
  SELECT RAISE(ABORT, 'transfers are append-only');
END;
CREATE TRIGGER IF NOT EXISTS transfer_claim_guard
BEFORE UPDATE OF state ON transfers
WHEN OLD.state = 'queued' AND NEW.state = 'reserved'
BEGIN
  SELECT CASE WHEN EXISTS (
    SELECT 1 FROM transfers WHERE state = 'uncertain'
  ) THEN RAISE(ABORT, 'uncertain transfer blocks wallet claim') END;
  SELECT CASE WHEN OLD.kind != 'fee_withdrawal' AND NOT EXISTS (
    SELECT 1 FROM orders o
    WHERE o.order_id = OLD.order_id AND (
      (OLD.kind = 'release' AND o.state = 'release_reserved'
        AND o.buyer_confirmed = 1 AND o.seller_confirmed = 1) OR
      (OLD.kind = 'refund' AND o.state = 'refund_reserved'
        AND o.buyer_confirmed = 0 AND o.seller_confirmed = 0) OR
      (OLD.kind = 'resolve_buyer' AND o.state = 'release_reserved') OR
      (OLD.kind = 'resolve_seller' AND o.state = 'refund_reserved') OR
      (OLD.kind = 'recovery_refund' AND o.state IN (
        'refund_reserved','completed','refunded','cancelled','deposit_expired'
      ))
    )
  ) THEN RAISE(ABORT, 'transfer is not authorized by order state') END;
  SELECT CASE WHEN COALESCE((
    SELECT SUM(units) FROM transfer_credit_allocations
    WHERE transfer_id = OLD.transfer_id
  ), 0) != CASE WHEN OLD.kind = 'fee_withdrawal' THEN 0
                ELSE OLD.amount_units + OLD.network_fee_units
                     + OLD.earned_fee_units END
  THEN RAISE(ABORT, 'transfer allocations do not balance') END;
  SELECT CASE WHEN EXISTS (
    SELECT 1
    FROM (
      SELECT credit_id, bucket, SUM(units) AS allocated_units
      FROM transfer_credit_allocations
      GROUP BY credit_id, bucket
    ) a
    JOIN deposit_credits c ON c.credit_id = a.credit_id
    WHERE a.allocated_units > CASE a.bucket
      WHEN 'main' THEN c.main_units ELSE c.recovery_units END
  ) THEN RAISE(ABORT, 'allocated credit capacity changed') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_confirmation_guard
BEFORE UPDATE OF state ON transfers
WHEN NEW.state = 'confirmed' AND OLD.state != 'confirmed'
BEGIN
  SELECT CASE WHEN COALESCE((
    SELECT SUM(units) FROM transfer_credit_allocations
    WHERE transfer_id = OLD.transfer_id
  ), 0) != CASE WHEN OLD.kind = 'fee_withdrawal' THEN 0
                ELSE OLD.amount_units + OLD.network_fee_units
                     + OLD.earned_fee_units END
  THEN RAISE(ABORT, 'confirmed transfer allocations do not balance') END;
  SELECT CASE WHEN EXISTS (
    SELECT 1
    FROM (
      SELECT credit_id, bucket, SUM(units) AS allocated_units
      FROM transfer_credit_allocations
      GROUP BY credit_id, bucket
    ) a
    JOIN deposit_credits c ON c.credit_id = a.credit_id
    WHERE a.allocated_units > CASE a.bucket
      WHEN 'main' THEN c.main_units ELSE c.recovery_units END
  ) THEN RAISE(ABORT, 'allocated credit capacity changed') END;
END;
CREATE TRIGGER IF NOT EXISTS transfer_confirmation_evidence_guard
BEFORE UPDATE ON transfers
BEGIN
  SELECT CASE WHEN OLD.state != 'confirmed' AND NEW.state != 'confirmed' AND (
      NEW.confirmed_at IS NOT OLD.confirmed_at OR
      NEW.confirmed_block_hash IS NOT OLD.confirmed_block_hash OR
      NEW.confirmed_block_height IS NOT OLD.confirmed_block_height OR
      NEW.confirmations != OLD.confirmations
    ) THEN RAISE(ABORT, 'confirmation evidence requires reconciliation transition') END;
  SELECT CASE WHEN OLD.state = 'confirmed' AND NEW.state = 'confirmed' AND (
      NEW.confirmed_at IS NOT OLD.confirmed_at OR
      NEW.confirmed_block_hash IS NOT OLD.confirmed_block_hash OR
      NEW.confirmed_block_height IS NOT OLD.confirmed_block_height OR
      NEW.confirmations < OLD.confirmations
    ) THEN RAISE(ABORT, 'confirmed anchor is immutable') END;
  SELECT CASE WHEN OLD.state = 'confirmed' AND NEW.state = 'uncertain' AND (
      NEW.confirmed_at IS NOT OLD.confirmed_at OR
      NEW.confirmed_block_hash IS NOT OLD.confirmed_block_hash OR
      NEW.confirmed_block_height IS NOT OLD.confirmed_block_height OR
      NEW.confirmations != OLD.confirmations
    ) THEN RAISE(ABORT, 'reorg transition must preserve prior evidence') END;
END;
CREATE TABLE IF NOT EXISTS audit_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER,
  actor_id INTEGER,
  event_type TEXT NOT NULL CHECK(
    instr(event_type, char(0)) = 0 AND
    length(CAST(event_type AS BLOB)) BETWEEN 1 AND 80 AND
    event_type NOT GLOB '*[^a-z0-9:_-]*'
  ),
  old_state TEXT CHECK(old_state IS NULL OR (
    instr(old_state, char(0)) = 0 AND
    length(CAST(old_state AS BLOB)) BETWEEN 1 AND 48 AND
    old_state NOT GLOB '*[^a-z0-9_]*'
  )),
  new_state TEXT CHECK(new_state IS NULL OR (
    instr(new_state, char(0)) = 0 AND
    length(CAST(new_state AS BLOB)) BETWEEN 1 AND 48 AND
    new_state NOT GLOB '*[^a-z0-9_]*'
  )),
  detail_json TEXT NOT NULL DEFAULT '{}' CHECK(
    instr(detail_json, char(0)) = 0 AND
    length(CAST(detail_json AS BLOB)) BETWEEN 2 AND 4000 AND
    json_valid(detail_json)
  ),
  created_at INTEGER NOT NULL
) STRICT;
CREATE TRIGGER IF NOT EXISTS audit_event_update_block
BEFORE UPDATE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit events are append-only');
END;
CREATE TRIGGER IF NOT EXISTS audit_event_delete_block
BEFORE DELETE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit events are append-only');
END;
INSERT INTO schema_meta(id, version, origin) VALUES(1, 4, 'fresh');
```

The archive table is read-only migration evidence and is dropped only in a
future release after its zero-row condition has been verified from backup.

Schema tests compare normalized definitions for every required table/index and
cover invalid hashes/states, duplicate outpoints, duplicate lifetime operation
keys, a second main order outcome, and two in-flight wallet operations including
`order_id IS NULL` fee withdrawals. They prove a `queued` or `failed_safe` row
does not use the unique send lane, `reserved`/`prepared`/`broadcast` do, and application
claim logic separately refuses every claim while any `uncertain` row exists.
They reserve an operation, reorg a different confirmed earning transfer to
`uncertain`, and prove the reserved operation cannot attach signed bytes and a
prepared operation cannot enter broadcast until uncertainty clears; truthful
confirmation and exact uncertain-row recovery remain legal.
Every Store connection asserts `foreign_keys=ON`, `recursive_triggers=ON`, WAL,
and `synchronous=FULL`; tests use `INSERT OR REPLACE` to prove neither an
operation key nor an append-only row can be replaced through SQLite conflict
semantics.

Also test the database defenses directly, not only Store methods: STRICT rejection
of fractional REAL monetary values (and public Store rejection of Python bool,
float, and string), exact quote-sum CHECK and supply
bounds, every partial-null `spent_by` tuple, scan/order/address composite foreign
keys, cross-order and fee-withdrawal allocations, cumulative bucket
over-allocation, forged outcome/earned-fee economics, immutable transfer fields
and allocations, unbalanced claim/confirmation, invalid state transitions, and
an uncertain row racing concurrent claims. Inject `U+0000` plus hidden suffixes
into every hash, signed hex, operation key, address, and bounded machine string;
assert byte-length/canonical checks reject them and that hidden identities cannot
bypass unique indexes. After a confirmed payout, attempt direct SQL mutation or
deletion of order quote economics, scans, credit identity/amount/classification,
bucket capacity, transfers, confirmation anchors, and audit rows. Only a newer
matching scan observation, monotonic confirmations on the same confirmed anchor,
or an explicit confirmed-to-uncertain-to-confirmed reconciliation may change
the corresponding evidence.
Exercise every side-aware order transition directly: sell-maker remains seller,
buy-maker remains buyer, WTS buyer can set once on `open -> matched` and clear
only on zero-flag `matched -> open`, WTB seller can set only once on
`open -> awaiting_deposit`, and neither participant can be redirected afterward.
An unaccepted/cancelled WTB cannot be poisoned with a deposit address: NULL may
become non-NULL only in the same `open -> awaiting_deposit` update that assigns
its first seller, after which it is immutable.
Prove a fully funded or payment-flagged trade cannot enter partial-credit
reclassification, while a genuinely underfunded zero-flag order with no main
operation can move main capacity to `cancelled_partial` recovery exactly once.
`Store.create_order` always normalizes `total_price` through
`parse_total_price`; direct SQL is independently rejected for zero, leading
zeros, missing digits, repeated/trailing dots, exponent/comma/sign syntax, more
than 18 integer or fractional digits, non-ASCII, and embedded NUL.

Migration fixtures include (a) a sanitized byte-for-byte catalog equivalent of
the live prototype: nullable five-column users with one compatible row, expanded
empty legacy orders plus its four named indexes, empty withdrawals, WAL mode,
and no `schema_meta`; and (b) databases created by the actual committed v3 Store,
both with and without an existing empty `orders_v2_archive`. Require legacy
withdrawals to be empty; nonempty withdrawals have no trustworthy opening
revenue provenance and block migration. Require absent/zero `sqlite_sequence`
history for prototype orders/withdrawals and v3 orders/transfers/audits,
withdrawals, and any prior order archive; insert-then-delete fixtures must block
without mutation. Add populated shadow-liability tables and rogue triggers to
v3/v4 fixtures and prove complete-catalog validation rejects them. For every success assert exact v4
catalog, preserved user/archive evidence, `integrity_check`, and
`foreign_key_check`. For every blocked/fault-injected phase assert catalog,
rows, version, and journal mode are unchanged.

- [ ] **Step 4: Verify migration, integrity, and rollback behavior**

```powershell
python -m unittest bot.tests.test_store_schema -v
python -m unittest discover -s bot/tests -p "test_*.py" -v
$env:DB_PATH = Join-Path $env:TEMP "btc09-otc-migrate-proof.db"
python -m bot.otc.store
```

Expected: all tests pass and `PRAGMA integrity_check` returns `ok`.

- [ ] **Step 5: Commit**

```powershell
git add bot/otc/domain.py bot/otc/schema_v4.sql bot/otc/store.py bot/tests/test_domain.py bot/tests/test_store_schema.py
git commit -m "Upgrade OTC schema for durable accounting"
```

---

### Task 3: Atomic Credits, Claims, Transfer Queue, and Solvency

**Files:**
- Modify: `bot/otc/store.py`
- Create: `bot/tests/test_store_atomic.py`

**Interfaces:**
- Produces: `reconcile_all_deposit_outputs`, `reserve_accept`,
  `record_confirmation`, `queue_order_transfer`, `claim_next_transfer`,
  `attach_signed_transfer` (including the exact prepared tip),
  `recover_ambiguous_attach`, `mark_transfer_broadcast`, `mark_transfer_failed_safe`,
  `mark_transfer_uncertain`, `mark_transfer_confirmed`,
  `requeue_failed_safe`, `cancel_failed_safe_fee_withdrawal`,
  `customer_liability_units`,
  `order_liability_units`, `provisional_restricted_units`,
  `wallet_solvency_snapshot`, `earned_fee_units`, `available_fee_units`, and
  `queue_fee_withdrawal`.
- Every mutation takes an explicit exact-integer `now` value and uses
  `BEGIN IMMEDIATE`. Audit insertion reuses that same connection; it never
  opens a nested writer.
- Queue/claim functions return a row/result only to the winning caller and
  `None` to losers.

- [ ] **Step 1: Write credit and liability lifecycle tests**

Use quote `net=100`, `network_fee=10`, `service_fee=7`, `required=117` so
fee omissions are observable. Insert normalized address-output snapshots with
stable lowercase 64-hex transaction/block IDs and verify:

- two partial outpoints of 50 and 67 produce liability 117 and deterministically
  allocate exactly 117 `main_units` without an aggregate order-balance cache;
- replaying either outpoint is idempotent, while the same outpoint assigned to
  another order is rejected;
- a 137-unit credit produces liability 137; after a confirmed release it leaves
  exactly 20, and a confirmed 10-unit recovery refund plus its 10-unit fee
  leaves zero;
- a cancelled/expired/completed order receiving a new 20-unit late credit gains
  liability 20 and is flagged for recovery instead of reopening. Its terminal
  main-order state remains unchanged; recovery status is derived from residual
  recovery units/transfers and never overwrites `completed`, `refunded`,
  `cancelled`, or `deposit_expired`;
- a credited output missing from a later complete canonical snapshot is marked
  noncanonical, retains its liability, and reports recovery/unhealthy;
- an output that disappears before reaching configured depth never becomes a
  liability; coinbase is credited only when both depth and maturity pass;
- liability stays 117 through queued, reserved, broadcast, uncertain, and
  failed-safe transfer states. A transfer row is not added to the order credit
  again. Liability falls only on confirmed disposition.
- confirmed release changes liability `117 -> 0` and earned fees `0 -> 7`;
  outgoing reorg `confirmed -> uncertain` restores `117`/`0`, and canonical
  reconfirmation restores `0`/`7` without a second operation.
- all watched addresses must be fetched at one expected hash/height and applied
  in one transaction. A missing/extra watched address, mixed tip, new address
  racing the batch, or final live-tip change rejects the whole barrier without
  partially advancing any address;
- a best-chain mature output below credit depth is recorded with
`credited_at=NULL`, counted as provisional/restricted, subtracted from usable
wallet funds, and excluded from coin selection. It cannot make an insolvent
credited ledger appear funded.

Each accepted complete all-address batch appends a new `deposit_scans`
observation per watched address only when that address's latest scan has a
different tip; it may reuse only the current latest identical scan, never an
older historical row with a matching hash. Thus A -> B -> A creates three
increasing scan IDs without writing an unbounded duplicate row for repeated
polls at A. The batch upserts returned
outpoints, and marks previously observed-but-absent outpoints off the current
best chain in one transaction. Before commit, compare the caller's address set
to the current distinct non-null `orders.deposit_addr` set; any difference rolls
back the entire batch. `credited_at` is permanent
once depth/maturity policy passes. While the order is active, credited units
fill `main_units` deterministically up to `deposit_required_units`; the remainder
is recovery/excess. Credits first observed after cancellation/expiry/completion
are recovery/late units. A first-seen already-spent output is still recorded and
credited so the obligation cannot vanish; unless `spent_by` matches a known
transfer it also creates an unknown-spend health failure and stops wallet work.

The exact formula per order is:

```text
SUM(deposit_credits.amount_units WHERE credited_at IS NOT NULL)
- SUM(transfer_credit_allocations.units joined to confirmed transfers)
```

Every aggregate is wrapped with `COALESCE(SUM(...), 0)`; an empty ledger is
zero, while a negative result is never hidden.

The common-tip claim guard runs inside the same `BEGIN IMMEDIATE` that claims a
queued transfer. For every current watched address, its maximum `scan_id` must
name the caller's exact expected tip hash/height and every credit's
`last_checked_scan_id` must be that latest scan. `claim_next_transfer` returns
the barrier anchor and the sorted restricted outpoint set to the winner. Before
prepare, the service rechecks the live tip is still that anchor, obtains the
locked structured wallet snapshot at that anchor (including primary/change and
every key), verifies every restricted unspent outpoint appears in it with the
same units, and requires:

```text
wallet_spendable_units - provisional_restricted_units
  >= customer_liability_units + pending_platform_outflow_units
```

The wallet command also excludes each restricted outpoint, so aggregate math
cannot be defeated by deterministic coin selection. If the tip advances, the
worker safely releases/requeues the unsigned reservation through the documented
failed-safe path, rebuilds the all-address barrier, and claims again; it never
signs against mixed/stale ledger evidence.
The snapshot's exact `wallet_snapshot_hash` is passed through the prepare
comparison; a wallet address/UTXO-set change between solvency and signing is a
safe pre-attach retry, never permission to reuse the earlier balance.

`provisional_restricted_units` is the exact sum of current-best-chain, mature,
unspent (`spent_by IS NULL`) outputs with `credited_at IS NULL`. The restricted
outpoint set is derived from those same rows in the same read transaction. An
immature coinbase is not reported spendable and is not subtracted; any partial
spent tuple, current-chain mismatch, or negative usable balance fails health.

Reservation atomically allocates credit buckets without exceeding each credit's
`main_units`/`recovery_units`. Release/resolve-buyer allocates
`amount + network fee + earned fee = deposit_required`; refund/resolve-seller
allocates `amount + network fee = deposit_required`; recovery refund allocates
`amount + network fee` exclusively from `recovery` buckets. Main outcomes may
allocate only `main` buckets. A fee withdrawal has no customer-credit allocation.
Raise `AccountingInvariantError` rather than clamp or hide a negative result.
Composite foreign keys require transfer, credit, and allocation to share one
order. Capacity is reduced by every durable allocation, regardless of transfer
state, so two queued/concurrent operations cannot reserve the same units.
Allocations and transfer economics are immutable; DB triggers independently
reject forged quote values, earned fees, destinations, cross-order links,
wrong-bucket use, oversubscription, or an unbalanced claim/confirmation.
Every transfer destination is also rejected when it equals any currently
watched order deposit address, preventing same-order and cross-order escrow
loop payouts even for a direct SQL writer. Task 4 repeats this check against
the locked wallet snapshot's complete owned-address set before preparation.
Order transitions that authorize a transfer require its queued allocations to
already equal its immutable amount plus fees. Claim-time kind/state coupling is
independent: release requires `release_reserved` plus both party flags, ordinary
refund requires zero-flag `refund_reserved`, dispute outcomes require their
reserved resolution state, and recovery is limited to `refund_reserved` or an
unchanged terminal main-order state. A zero-allocation placeholder release can
neither unlock a release transition nor let a recovery transfer consume main
credit; test that exact adversarial sequence.
Append-only scan tests cover the same address at tips A, then B, then A again;
all three observations receive increasing scan IDs and the last A snapshot may
authoritatively reconcile current credit evidence.

- [ ] **Step 2: Write accept, confirmation, and global-claim concurrency tests**

Cover 20 concurrent attempts for each path:

- WTS `open -> matched` assigns exactly one non-maker buyer.
- WTB `open -> awaiting_deposit` assigns exactly one non-maker seller and the
  winner's preallocated fresh address/deadline. Losing fresh addresses are never
  stored or disclosed.
- Existing/wrong roles and self-accept do not mutate the order or audit log.
- Buyer and seller confirmations are role-authorized and idempotent. Whichever
  valid actor confirms second atomically sets its flag, inserts exactly one
  `queued` release with operation key `order:{id}:main`, snapshots the buyer's
  validated stored address, and moves the order to `release_reserved` in the
  same transaction.
- A crash immediately after that commit leaves one safe queued operation; there
  is no `matched + both flags + no transfer` gap.
- With several queued operations, 20 concurrent `claim_next_transfer` calls
  change exactly one row to `reserved`. Queued rows do not occupy the wallet
  slot; reserved/prepared/broadcast rows occupy the unique lane globally, including fee
  withdrawals whose `order_id` is NULL. Any uncertain row independently makes
  every claim return `None`.
- Claims with one stale/missing latest address scan, provisional funds masking a
  one-unit deficit, or an expected tip different from the barrier all return
  `None`; no wallet process starts.

Use this accept interface so both sides are representable:

```python
store.reserve_accept(
    order_id=order_id,
    actor_id=actor_id,
    actor_name=actor_name,
    preallocated_deposit_addr=addr_or_none,
    deposit_deadline=deadline_or_none,
    now=now,
)
```

- [ ] **Step 3: Implement immutable outcome and transfer state rules**

Main outcome amount/destination is derived inside the locked transaction:

| kind | destination | amount | network fee | earns service fee |
|---|---|---:|---:|---:|
| `release`, `resolve_buyer` | buyer stored address | order net | quoted | on confirmation |
| `refund`, `resolve_seller` | seller stored address | net + service fee | quoted | never |
| `recovery_refund` | seller stored address | unallocated recovery units - quoted fee | quoted | never |

When residual liability is not greater than its fee, do not create a zero or
negative payout. Atomically move the order to `recovery_hold`, retain every unit
of liability, keep watching the same address, and tell the depositor that a
top-up must make the total greater than the disclosed network fee. New credited
output re-evaluates the hold; `fee + 1` queues a one-unit recovery payout. The
0% pilot does not silently subsidize dust from another customer or unproven
revenue. Tests cover residual `1`, exactly `fee`, and `fee + 1`.
At insert, the DB trigger snapshots economics by requiring `amount + fee` to
equal every currently unallocated `recovery` unit for that order; recovery
transfers cannot allocate `main` capacity. Credits first qualified after the
queue commit remain liability for a later recovery; they do not mutate or block
the immutable in-flight operation.
The recovery operation key is deterministic:
`order:{order_id}:recovery:{max_unallocated_recovery_credit_id}`. A replay of
the same recovery set cannot create a new lifetime row; a later qualifying
credit produces a new key only after the prior recovery is terminal.
For late/excess recovery on an already terminal order, queue/confirm the
`recovery_refund` without changing that main order state. For a pre-terminal
partial cancellation, `recovery_hold -> refund_reserved -> refunded` remains
the explicit user-visible path. Tests distinguish both cases.
The four main outcomes share one lifetime operation/index and cannot be replaced
after a failed-safe result. An explicit failed-safe retry changes the same
immutable row back to `queued`; uncertain is never automatically retried.

Transfer CAS rules:

- `queued -> reserved` only through the global claim winner;
- `reserved -> prepared` atomically stores the full txid and signed transaction
  hex plus the exact persisted/live tip hash and height returned by the
  no-network prepare process. SQLite WAL with
  `synchronous=FULL` is the durable operation journal and commits before any
  submit or peer write;
- `prepared -> broadcast` occurs only after submitting that exact stored
  transaction and observing the exact txid as mempool/canonical on the trusted
  live node. A crash, timeout, or peer write without trusted-node observation
  changes it to `uncertain`, but the exact transaction remains available for
  idempotent status lookup or rebroadcast;
- `reserved -> failed_safe` is legal only when preparation failed before a
  signed transaction was durably attached;
- `prepared|broadcast -> uncertain` retains full liability and blocks every
  global claim;
- `prepared|broadcast|uncertain -> confirmed` is reconciliation-only and atomically
  completes/refunds the order according to immutable kind;
- `confirmed -> uncertain` handles an outgoing-transaction reorg, immediately
  restoring allocated liability and reversing earned fees until reconfirmed;
- a reserved row found after restart has no signed tx and cannot have reached
  the network by construction; after confirming the old prepare child is gone,
  it may become `failed_safe` and requeue the same operation. Prepared and later
  rows always resume/reconcile the exact stored tx and never build a replacement.

`Wallet.prepare` accepts only Go output that has decoded/validated the signed
transaction against the chain and exactly matches the requested destination,
amount, and fee. `attach_signed_transfer` independently validates bounded
lowercase hex, recomputes SHA256d txid from those bytes, and compares the
structured destination/amount/fee metadata with the immutable reserved row. It
also validates/stores the 64-hex prepared tip and exact integer height before
committing. Tests reject a txid/hex mismatch, different quoted metadata,
malformed/trailing transaction bytes at the Go boundary, a mismatched live tip,
and a second attachment; prepared identity is DB-immutable thereafter.

Treat an exception returned by SQLite COMMIT as ambiguous, not as rollback.
`recover_ambiguous_attach` closes that connection and reopens the database: an
exact matching txid/hex/tip in `prepared` resumes recovery; a completely
unsigned `reserved` row permits the same no-network prepare to retry; any
partial/mismatched/unreadable result raises `AccountingInvariantError`, fails
health, and halts all claims. Keep commit behind an injectable Store boundary so
stdlib `sqlite3` tests can deterministically raise immediately before calling
COMMIT and immediately after a successful COMMIT but before returning to the
caller. Also kill a subprocess at randomized offsets during COMMIT and always
classify by reopen/read; do not claim a Python-level WAL-sync hook that stdlib
SQLite does not expose. Test reopen/read failure separately.

- [ ] **Step 4: Implement earned-fee and fee-withdrawal idempotency**

Gross earned fees are the immutable `earned_fee_units` on confirmed
`release`/`resolve_buyer` outcomes. Available fee funds equal gross earned fees minus
`amount + network_fee` for every non-cancelled fee-withdrawal operation,
including failed-safe and uncertain rows. `queue_fee_withdrawal` requires a
caller-supplied stable interaction/operation key, validates exact integer amount
and configured admin destination, and atomically refuses oversubscription.

Tests prove two concurrent requests that oversubscribe earnings have one winner;
replaying the same key before or after confirmation never creates a second row;
different keys within earned revenue can both queue; a 0% fee pilot cannot
withdraw; and a failed-safe fee transfer remains encumbered until the same row
is retried or explicitly cancelled.

Cancellation is an audited admin CAS permitted only for a `fee_withdrawal` in
`failed_safe` with no txid/signed transaction. It is forbidden from queued,
reserved, prepared, broadcast, uncertain, confirmed, and every customer
transfer. A confirmed release reorg after fees were already withdrawn may make
available revenue negative; that is an accounting invariant/health failure that
halts intake and claims, never a value clamped to zero.

- [ ] **Step 5: Stress, verify, and commit**

```powershell
1..50 | ForEach-Object { python -m unittest bot.tests.test_store_atomic -q; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
python -m unittest discover -s bot/tests -p "test_*.py" -v
go test ./... -count=1
git add bot/otc/store.py bot/tests/test_store_atomic.py
git commit -m "Make OTC fund transitions atomic"
```

Expected: 50 clean concurrency iterations, the full Python/Go suites pass, and
no liability, transfer, audit, or fee reservation is duplicated.

---

### Task 4: Canonical Credit Snapshot and Structured Wallet Boundaries

**Files:**
- Modify: `core/chain.go`
- Modify: `core/core_test.go`
- Modify: `wallet/wallet.go`
- Create: `wallet/wallet_test.go`
- Create: `wallet/filelock_unix.go`
- Create: `wallet/filelock_windows.go`
- Create: `wallet/durable_replace_unix.go`
- Create: `wallet/durable_replace_windows.go`
- Modify: `explorer/explorer.go`
- Modify: `explorer/explorer_test.go`
- Modify: `p2p/p2p.go`
- Modify: `p2p/p2p_test.go`
- Modify: `cmd/btc09/main.go`
- Modify: `cmd/btc09/main_test.go`
- Create: `bot/otc/explorer.py`
- Create: `bot/otc/wallet.py`
- Create: `bot/tests/test_explorer.py`
- Create: `bot/tests/test_wallet.py`

**Interfaces:**
- Go produces `Chain.ConfirmedOutputsForPKH`, atomic transaction-accept results,
  canonical tip/block and transaction-status lookup,
  `GET /api/v1/address/{address}/outputs`, `GET /api/v1/tip`,
  `GET /api/v1/block/{hash}`, `Node.WaitForPeers`, `Node.BroadcastTx`,
  crash-durable `btc09 wallet new -wallet-file`, locked structured
  `btc09 wallet snapshot -json`, `btc09 prepare-send -json`,
  and `btc09 broadcast-tx -json -require-broadcast`.
- Python produces `Explorer.outputs(address, expected_tip)`, `Explorer.tip()`,
  `Explorer.block(hash)`, `Explorer.transaction(txid)`,
  `Wallet.new_address()`, `Wallet.snapshot(expected_tip) -> WalletSnapshot`,
  `Wallet.prepare(destination, amount_units, fee_units, expected_tip,
  restricted_outpoints) -> PreparedTransfer`, and
  `Wallet.broadcast(signed_tx_hex, expected_txid) -> BroadcastResult` with
  `SafeSendFailure` and `UncertainSendFailure`.

- [ ] **Step 1: Write failing canonical address-history tests**

Build regtest chains and assert:

- a normal confirmed output has exact lowercase `txid:vout`, integer units,
  block hash/height, confirmations, `coinbase=false`, and `mature=true`;
- the output remains in history with exact `spent_by` identity after a later
  confirmed transaction spends it;
- mempool-only outputs are absent and confirmations advance with the tip;
- coinbase outputs are returned but switch `mature` only at the consensus
  maturity boundary;
- a heavier-fork reorg removes or reanchors the outpoint and changes the one
  consistent tip anchor;
- concurrent reads/reorgs never mix blocks from different main-chain tips.

Implement:

```go
func (c *Chain) ConfirmedOutputsForPKH(pkh [20]byte) AddressOutputSnapshot
```

Scan the current canonical `mainIDs`/blocks under one consistent chain read
snapshot, not the current UTXO map. Return deterministic order by block height,
transaction index, and vout. The snapshot contains network, `complete=true`,
tip hash/height, and all best-chain outputs including spent outputs. Each output
contains full txid/vout, integer `amount_units`, block anchor, confirmations,
coinbase/maturity, and an optional confirmed spending tx/input/block identity.

- [ ] **Step 2: Expose and test the explorer JSON contract**

Add:

```http
GET /api/v1/address/{base58-address}/outputs?expected_tip_hash={hash}&expected_tip_height={height}
```

```json
{
  "schema_version": 1,
  "network": "btc09-mainnet",
  "address": "...",
  "complete": true,
  "tip": {"height": 7000, "hash": "<64 lowercase hex>"},
  "outputs": [{
    "txid": "<64 lowercase hex>",
    "vout": 0,
    "amount_units": 100000000,
    "block": {"height": 6995, "hash": "<64 lowercase hex>"},
    "confirmations": 6,
    "coinbase": false,
    "mature": true,
    "spent_by": null
  }]
}
```

Return schema version 1, exact JSON integers/full hashes, deterministic complete
output array, and `Cache-Control: no-store`. Return 400 for a bad address and 405
for non-GET. An empty array with `complete=true` is authoritative. The endpoint
reads the live in-memory chain; do not implement this by launching a CLI that
can lag the running node.

When expected-tip query fields are present they are both required and strictly
validated. Under the same chain read snapshot used for the address history,
return 409 without output data unless the live tip exactly matches them. The
service first reads `/tip`, fetches every watched address with that expectation,
then reads `/tip` again; only two equal tip reads and all equal response anchors
may enter the atomic all-address Store reconciliation.

Also expose `GET /api/v1/transaction/{64-hex-txid}` with exact status
`unknown`, `mempool`, or `confirmed`; confirmed results include block hash,
height, and confirmations. This is the reconciliation source for a prepared or
broadcast operation and uses the same consistent live-chain/mempool snapshot.

Expose `GET /api/v1/tip` as one exact live hash/height pair and
`GET /api/v1/block/{64-hex-hash}` as a canonical-membership lookup containing
`canonical`, the block height when known, and the current tip anchor. All three
responses use one in-memory chain snapshot, lowercase full hashes,
`Cache-Control: no-store`, strict 400/404/405 behavior, and no CLI/disk fallback.
The block lookup lets recovery prove that the exact tip used to sign is still a
canonical ancestor after newer blocks arrive.

- [ ] **Step 3: Write and implement the fail-closed Python explorer adapter**

Tests reject timeouts, non-200, malformed JSON, `complete=false`, wrong
schema/network/address, duplicate `(network,txid,vout)`, negative/non-integer
units, short/non-lowercase hashes, impossible heights/confirmations, duplicate
or malformed `spent_by`, and unsorted output. All failures are “unknown” and
must never be converted to a zero balance. Valid empty and multi-partial
snapshots remain distinguishable.
Transaction-status tests reject identity/status mismatches and treat transport
failure as unknown-to-the-service, not chain status `unknown`.
Tip/block tests reject stale or internally inconsistent anchors. A live tip
change between the pre-prepare and post-prepare reads is a safe preparation
failure because no network side effect occurred. Loss of a stored prepared
tip from the canonical chain is uncertain and globally halts wallet work; an
already persisted signed transaction is never replaced.
Batch tests fetch addresses A/B/C at expected tip T and reject all results if B
returns 409, C returns another tip, the final tip advances, the watched DB set
changes, or an output/outpoint repeats across address responses.

- [ ] **Step 4: Write failing prepare/persist/broadcast Go tests**

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

func TestPrepareSendDoesNotSubmitOrBroadcast(t *testing.T) {
    // returns full txid + signed bytes, chain mempool stays empty, zero peer writes
}

func TestBroadcastPreparedTxUsesExactBytes(t *testing.T) {
    // decode one prepared tx, submit it, and report the same full txid
}

func TestDuplicateTxIsNotRegossiped(t *testing.T) {
    // a cyclic peer graph receives one relay for a newly added tx and none for
    // an already-known replay
}

func TestPrepareSendRequiresExactExpectedLiveTip(t *testing.T) {
    // persisted chain hash/height must exactly match the caller-supplied tip
}

func TestPrepareSendNeverSelectsRestrictedOutpoint(t *testing.T) {
    // a larger restricted UTXO is skipped in favor of eligible inputs; fail
    // safely when eligible inputs cannot cover amount plus fee
}

func TestWalletSnapshotIncludesPrimaryChangeAndAllKeys(t *testing.T) {
    // exact integer total/outpoints cover internal primary/change plus every
    // other wallet key at the expected network/tip
}
```

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

Change transaction acceptance to return an atomic result that distinguishes
`added` from `already_known`. The P2P receive path relays only `added`; an exact
replay is successful and idempotent but is never re-gossiped. Add a cyclic
multi-peer test that would fail if duplicates bounce indefinitely.

Change `broadcast` to return successful peer writes. Refactor the wallet so
`BuildPayment` selects inputs and signs but does not call `Chain.AcceptTx`;
`SubmitPayment` validates/submits a previously signed transaction. Every wallet
load and mutation uses the same cross-process lock. Keep the human `send`
command compatible while removing its `flag.Float64` path too. The bot uses a
dedicated wallet and three machine boundaries:

```text
btc09 wallet snapshot -wallet-file WALLET -datadir DATA -network NETWORK -expected-tip-hash HASH -expected-tip-height HEIGHT -json
btc09 prepare-send -to ADDRESS -amount DECIMAL -fee DECIMAL -datadir DATA -network NETWORK -wallet-file WALLET -expected-tip-hash HASH -expected-tip-height HEIGHT -exclude-outpoints-json - -json
btc09 broadcast-tx -tx-hex SIGNED_HEX -expected-txid TXID -datadir DATA -network NETWORK -seeds HOSTS -json -require-broadcast
```

`wallet snapshot` takes the same interprocess wallet lock, requires its loaded
chain to equal the expected tip/network, and returns canonical sorted wallet
addresses plus every mature spendable wallet UTXO (`txid:vout`, integer units),
an overflow-checked `spendable_units` sum, and a deterministic SHA-256
`wallet_snapshot_hash` over network, tip, address set, and UTXO set. It includes
the internal primary/change address even when that address is not attached to an
order. It has no save, signing, mempool, or network side effect.

`prepare-send` must never start P2P, submit to the mempool, or write to a peer.
It loads the persisted chain and refuses to sign unless its exact hash/height
matches the caller's immediately preceding live `GET /api/v1/tip` result. It
reads a bounded canonical JSON array of lowercase `txid:vout` identities from
stdin and excludes all of them from coin selection. It returns the matched
snapshot anchor and the selected inputs so both languages can prove exclusion:

```json
{"ok":true,"stage":"prepared","txid":"<64 lowercase hex>","signed_tx_hex":"<lowercase hex>","destination":"<09C address>","amount_units":100,"fee_units":10,"snapshot_tip":{"height":7000,"hash":"<64 lowercase hex>"},"wallet_snapshot_hash":"<64 lowercase hex>","selected_outpoints":["<txid>:0"]}
```

The parent reads the live tip again after prepare. It attaches the prepared
transaction only when both live reads and the returned snapshot anchor are
exactly equal. A changed tip is a safe retry because prepare had no network
side effect. It commits the exact txid/hex and prepared tip to the reserved
SQLite transfer (`reserved -> prepared`, WAL `synchronous=FULL`) before invoking
`broadcast-tx`.

If the FULL commit call raises, the process closes the connection, reopens the
database, and reads the row before deciding: exact committed bytes/anchor enter
prepared recovery; a still-reserved row with every signed field NULL is safe to
retry; mismatch, unreadable state, or partial evidence sets the service health
to failed and halts the global lane. An injectable Store commit boundary tests
immediately before COMMIT and immediately after a successful durable COMMIT but
before return, plus subprocess kills at randomized COMMIT offsets. Classification
always comes from reopen/read because Python stdlib exposes no WAL-sync hook.

Before every first broadcast or rebroadcast, the parent asks the live block
endpoint whether the stored prepared tip is still canonical. If not, it moves
the row to `uncertain`, halts the global wallet lane, and never builds a
replacement. The broadcast command opens the explicitly selected network and
datadir, decodes and re-hashes the stored bytes, requires the expected
transaction to be valid, waits for a peer before initial
submission, and returns the same txid plus submission/peer-write count:

```json
{"ok":true,"stage":"broadcast","status":"submitted","txid":"<same 64 hex>","peer_writes":1}
```

The ephemeral broadcaster never reports `mempool` merely because its temporary
chain accepted the transaction or because a socket write succeeded. After a
`submitted` result, Python polls the trusted local live transaction endpoint for
the exact txid; only an observed `mempool` or `confirmed` status promotes the DB
row to `broadcast`/`confirmed`. If that endpoint already reports mempool or
canonical before submission, the exact replay is idempotent and may have zero
new peer writes. A disconnect, zero write for a previously unknown tx, malformed
result, local-observation timeout, or process crash during broadcast is
uncertain, but the stored signed transaction can be looked up and rebroadcast
without building a second payment.

Replace every current `flag.Float64` amount/fee path, including legacy `send`,
completely. Parse strict
plain ASCII decimal strings to int64 base units: no sign, exponent, whitespace
inside, more than eight decimals, zero amount, negative fee, or value above
21,000,000 09C. Tests include one base unit, eight decimals, maximum supply,
one unit over, exponent notation, overflow-length input, and decimals known to
truncate through binary float. No accounting boundary uses float.

Add `-wallet-file` to every key-reading/writing wallet command. Production uses
`BTC09_WALLET_PATH=/var/lib/btc09-otc/wallet-mainnet.json`, never the chain
datadir wallet. `wallet new` takes an interprocess lock file, re-reads the wallet
under that lock, writes a mode-0600 temporary file in the wallet directory,
fsyncs the file, atomically renames it, fsyncs the directory, and only then
prints/returns the address. Prepare holds the same lock while reading keys and
constructing the signed transaction. No command signs from a long-lived cached
key slice, and `Load` never performs an unlocked implicit first-key write. Add
subprocess concurrency tests and a
crash helper killed after temp creation, file sync, rename, and directory sync;
after each case the wallet is either the complete old file or complete new file,
never malformed, and no returned address lacks a durable private key.
Use build-tagged platform primitives: advisory `flock` plus rename/directory
fsync on Unix, and `LockFileEx` plus write-through replace/handle flush on
Windows. An unsupported durability primitive is a hard error, not a silent
best-effort save. Kill a subprocess while it owns the platform lock and prove
the OS releases the lock so the next process can open the unchanged wallet; do
not implement ownership with a bare persistent `O_EXCL` sentinel file.

- [ ] **Step 5: Implement and test the Python wallet adapter**

Use integer base-unit decimal formatting without float and invoke the wallet
creation, snapshot, and two send commands above. `Wallet.new_address` returns only a
durably stored address from the dedicated wallet. `Wallet.snapshot` rejects a
wrong network/tip, duplicate address/outpoint, bad integer sum, malformed hash,
or noncanonical ordering. `Wallet.prepare` first reads
the live tip and requires it equal the Store claim's common-ledger tip, then
passes that anchor and the configured Explorer network to the command,
passes the Store's sorted provisional/restricted outpoints through bounded JSON
stdin, and validates exact network/amount/fee, full txid,
bounded canonical signed hex and returned snapshot, then reads the live tip
again. It accepts only three equal anchors, requires the prepare response's
wallet snapshot hash equal the immediately preceding locked snapshot, and
confirms decoding/re-hashing through the CLI agrees. Any selected input present
in the restricted set is a hard invariant failure, not a retry. An intervening
address/key or UTXO-set change is a safe pre-attach retry.
Because prepare has no submission/network path, a killed/failed prepare is safe
to retry while the DB remains `reserved` and has no signed bytes.

`Wallet.broadcast` accepts only the DB-stored hex/expected txid and first proves
the stored prepared tip remains canonical. Reject malformed/multiple JSON
values, identity mismatch, zero peer writes for a transaction the trusted node
did not already know, stderr-only/unstructured failures, missing trusted-node
observation, or timeout as `UncertainSendFailure`. A prepared exact tx may be
retried/rebroadcast; it is never replaced. Never include a token, private key,
seed, signed transaction, or complete subprocess environment in an exception/log.

Crash tests cover before signing, after signing but before parent receipt, each
ambiguous FULL SQLite commit boundary, after local submission, after the first
peer write, and before the broadcast DB update. Race tests cover disk/live tip
mismatch, reorg during signing, a new block during signing, and reorg after the
prepared commit. A funded regtest path passes `btc09-regtest`; a mainnet tip or
output is rejected when any boundary is configured for regtest and vice versa.
Every post-commit case reconciles or rebroadcasts only the same
txid (or halts uncertain if its snapshot lost canonicality); every pre-commit
case proves no network write.
Snapshot tests include funds held only at the primary/change address, multiple
wallet keys, restricted outpoints, exact sum overflow, a new address between
snapshot and prepare, and wrong network/tip. No test parses the human `balance`
or `wallet list` output.

- [ ] **Step 6: Verify and commit**

```powershell
go test ./... -count=1
python -m unittest bot.tests.test_explorer bot.tests.test_wallet -v
python -m unittest discover -s bot/tests -p "test_*.py" -v
git add core/chain.go core/core_test.go wallet/wallet.go wallet/wallet_test.go wallet/filelock_unix.go wallet/filelock_windows.go wallet/durable_replace_unix.go wallet/durable_replace_windows.go explorer/explorer.go explorer/explorer_test.go p2p/p2p.go p2p/p2p_test.go cmd/btc09/main.go cmd/btc09/main_test.go bot/otc/explorer.py bot/otc/wallet.py bot/tests/test_explorer.py bot/tests/test_wallet.py
git commit -m "Add canonical OTC wallet boundaries"
```

---

### Task 5: WTS and WTB Application Service

**Files:**
- Create: `bot/otc/service.py`
- Create: `bot/tests/test_service_orders.py`
- Modify: `bot/otc/store.py`

**Interfaces:**
- Produces: `TradeService.create_sell`, `create_buy`, `accept`, `check_deposit`, `confirm_sent`, `confirm_received`, `list_open`, `mine`.
- Consumes: `Store`, `Wallet`, the strict `Explorer` snapshot adapter,
  fresh-address callable, confirmation-depth configuration, and a clock through
  constructor injection.

- [ ] **Step 1: Write failing WTS/WTB tests**

Create real temporary SQLite stores and small fake boundary adapters. Cover:

```python
def test_wts_requires_deposit_before_listing():
    order = service.create_sell(seller_id=1, net_amount=100_000_000, total_price="2", asset="AUD", method="PayID", network=None)
    assert order.state == "awaiting_deposit"
    explorer.set_outputs(order.deposit_addr, [confirmed_output(order.deposit_required_units)])
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
the buyer assigned; acceptance preallocates an address, then atomically assigns
the winning seller/address/deadline. `check_deposit` consumes one complete
canonical output snapshot and reconciles stable outpoints, never a balance.
Underpayment stays waiting. Fully credited WTS becomes `open`; fully credited
WTB becomes `matched` because both roles are already assigned. Overpayment stays
an exact residual liability; late payment creates a recovery event and does not
reopen, rematch, or silently disappear. All known deposit addresses, including
cancelled/expired/completed orders, remain in the watcher set.

- [ ] **Step 4: Add confirmation/release tests and implementation**

Test that buyer `confirm_sent` alone does not release, seller
`confirm_received` alone does not release, either order of both confirmations
creates exactly one queued transfer, and 20 concurrent second confirmations plus
workers produce one global claim, one prepare, one durable signed-tx attachment,
and one exact broadcast. Before invoking the wallet,
the service completes the all-watched-address common-tip barrier, verifies
aggregate spendable wallet units minus provisional/restricted units cover all
customer liabilities/platform commitments, excludes those provisional
outpoints from coin selection, and verifies the selected order covers its own
immutable payout/fee. It does not add that fee again. The service updates
`prepared` only after `Wallet.prepare` returns a valid result and the Store
commits exact txid/signed hex with FULL durability. It then calls
`Wallet.broadcast` only with those stored bytes. A safe prepare failure becomes
`transfer_failed_safe`; any broadcast ambiguity becomes `transfer_uncertain`.
On startup a signed prepared row is reconciled/rebroadcast using the same tx;
an unsigned reserved row is safe to requeue only after the old prepare child is
known dead. No recovery path builds a replacement transaction.

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
- Cancelling an underpaid order with credited partial units creates one recovery
  refund (net of its disclosed network fee) when positive; residual `<= fee`
  enters `recovery_hold` with liability/watch intact until a top-up.
- Two simultaneous admin resolutions invoke the wallet once.
- A matched timeout changes to `disputed` and does not call the wallet.
- A `queued` transfer on restart is safe for a worker to claim. A `reserved`
  transfer has no signed bytes/network side effect; after systemd confirms the
  old prepare child is dead, the same operation becomes failed-safe and requeues.
- A `prepared` transfer on restart looks up and, if needed, rebroadcasts only its
  DB-stored signed transaction. Crashes after local submit, first peer write, or
  before the DB broadcast update all retain one txid and never build another.
- A `broadcast` transfer with a transaction present in the explorer becomes
  `completed` or `refunded` according to kind only at required confirmation depth.
- An unknown `broadcast` transaction remains a liability and becomes
  `transfer_uncertain` after the configured reconciliation deadline.
- A previously confirmed transfer that disappears in a reorg becomes uncertain;
  its credit allocation stops discharging liability and earned fees reverse.
- A late deposit after cancellation creates `recovery_refund` liability while
  preserving the terminal main-order state; dust is a derived recovery hold.
- A credited deposit output that becomes noncanonical, or an output spent by an
  unknown transaction, retains liability and fails health closed.
- A failed-safe fee withdrawal can be cancelled by an admin once; cancellation
  from any other state/customer kind is rejected. If its earning release later
  reorgs after a confirmed fee withdrawal, negative fee availability closes
  health instead of being clamped.
- `system_health().accepting_orders` is false for DB failure, explorer failure,
  wallet balance below customer liability plus pending platform commitments,
  any uncertain transfer, any post-credit reorg, or unknown custodian spend.

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_service_recovery -v
```

- [ ] **Step 3: Implement recovery rules**

Admin resolution requires `winner in {'buyer','seller'}` and a 10-500 character
reason. The store writes the reason only to private audit JSON, never the public
feed. Cancellation and resolution queue immutable transfer rows before any
wallet call; only the global claim winner may invoke the wallet.
`expire_orders` uses conditional updates and only opens disputes.
`reconcile_transfers` is safe to run at startup and every 30 seconds.

Cancellation follows a side/state matrix:

- WTS awaiting deposit with zero credit cancels without transfer; partial credit
  is reclassified/allocated to one recovery refund or dust hold; funded open WTS queues the
  main seller refund.
- A WTS taker buyer may leave `matched -> open` only before either payment flag;
  the same seller escrow and watched deposit address remain attached.
- Unaccepted WTB cancels without transfer. Once a seller accepts, the order is
  never reopened: zero-credit cancellation closes it, while partial/full credit
  refunds that seller (or remains in dust hold) and closes it after canonical confirmation. A new WTB must
  be posted for another seller/address.
- Once either party records payment movement, both sides require dispute/admin
  resolution; no timeout or cancellation auto-pays or auto-refunds.

Stress every matrix row with concurrent cancel/accept/confirm attempts and prove
old WTB deposit addresses remain watched for late recovery credits.

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
- `OTC_ACCEPTING_ORDERS=0` blocks new create/accept actions with the same clear
  English maintenance notice used during the upgrade, while read-only status,
  dispute, reconciliation, and safe recovery paths remain available.

- [ ] **Step 2: Verify RED**

```powershell
python -m unittest bot.tests.test_discord_ui -v
```

- [ ] **Step 3: Implement grouped commands and components**

Use `app_commands.Group(name="trade", description="Buy and sell Bitcoin 09")`.
Use English autocomplete for AUD, USD, EUR, GBP, CNY, JPY, USDT, USDC, BTC,
ETH, SOL, LTC, DOGE, and BNB, and accept validated custom asset text.
Commands must defer before explorer/wallet work and use follow-ups after defer.
Persistent button `custom_id` values contain only action and numeric order ID,
never user IDs or addresses. Stable Discord interaction IDs become operation
keys where an admin fee/recovery action needs lifetime replay protection.

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
The default production feed path is
`/var/lib/btc09-otc/public/otc-bot-feed.json`; the bot writes it atomically and
the local feed service has read-only access. No mutable feed or database remains
under `/opt/btc09` once the hardened services are enabled.
The feed reparses the stored canonical total with `Decimal`, derives net 09C
from integer units, and emits price-per-09C as a canonical decimal string; it
never converts either value through binary float. A malformed direct fixture
fails health/feed generation closed instead of publishing a misleading price.

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

Add `/healthz` fields: database/foreign-key integrity, explorer snapshot and tx
status reachability, wallet spendable units, customer liability, pending
platform outflows, provisional/restricted units, common ledger tip and stale
watched-address count, gross/available fee units (including negative invariant),
queued/reserved/prepared/broadcast/uncertain counts, credited noncanonical and
unknown-spend counts, feed age seconds, and `accepting_orders`. Bind the feed
service to `127.0.0.1` only; nginx exposes
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
The production wallet argument is the dedicated
`/var/lib/btc09-otc/wallet-mainnet.json`, never a general node-datadir wallet.

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
Environment=OTC_ACCEPTING_ORDERS=0
Environment=DB_PATH=/var/lib/btc09-otc/otc_bot.db
Environment=BTC09_WALLET_PATH=/var/lib/btc09-otc/wallet-mainnet.json
Environment=PUBLIC_FEED_PATH=/var/lib/btc09-otc/public/otc-bot-feed.json
KillMode=control-group
TimeoutStopSec=15
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
ReadWritePaths=/var/lib/btc09-otc
ReadOnlyPaths=/opt/btc09/data
UMask=0077
```

The feed service sets
`OTC_FEED_PATH=/var/lib/btc09-otc/public/otc-bot-feed.json` and receives only
read access to that file. Change the Python defaults in Tasks 7/8 to these same
state paths so an omitted environment variable cannot fall back into the
read-only application tree.

`KillMode=control-group` is part of the wallet safety contract: after a service
restart, no old prepare/broadcast child may survive. Startup verifies there is
at most one reserved/prepared/broadcast lane row, reconciles it before enabling
the worker, and never logs signed transaction hex.

Add an actual systemd integration test, not only a unit assertion: a test
adapter starts a long-lived child in the service cgroup and records both parent
and child PIDs; `systemctl restart btc09-otc-bot` must make both old PIDs
disappear before the new worker reports recovery complete. Then inject one
prepared row and prove the restarted service reconciles only its exact txid. A
surviving old child, overlapping worker generation, or claim before recovery
keeps intake disabled and fails deployment.

Keep `/etc/btc09/discord.env` root-owned mode 600 and expose needed values with
a root-owned environment file readable through the service manager, not by
making the file world-readable.

- [ ] **Step 3: Test migration on a copy and deploy disabled**

```powershell
ssh root@178.128.105.41 "systemctl stop btc09-otc-bot; /opt/btc09/bitcoin09/deploy/scripts/backup-otc.sh /opt/btc09/otc_bot.db /opt/btc09/data/wallet-mainnet.json /var/backups/btc09; install -d -o btc09-otc -g btc09-otc -m 0700 /var/lib/btc09-otc/public; sqlite3 /opt/btc09/otc_bot.db '.backup /var/lib/btc09-otc/otc_bot.db'; chown btc09-otc:btc09-otc /var/lib/btc09-otc/otc_bot.db; chmod 0600 /var/lib/btc09-otc/otc_bot.db; sqlite3 /var/lib/btc09-otc/otc_bot.db '.backup /tmp/otc-migration-test.db'; OTC_ACCEPTING_ORDERS=0 DB_PATH=/tmp/otc-migration-test.db /opt/btc09/venv/bin/python -m bot.otc.store; sqlite3 /tmp/otc-migration-test.db 'PRAGMA integrity_check'"
```

Expected: backup hashes written, migration exit 0, integrity `ok`, original DB
untouched. Verify the legacy aggregate is still zero, then create a fresh
dedicated escrow wallet at `/var/lib/btc09-otc/wallet-mainnet.json` with the
crash-durable command via `runuser -u btc09-otc -- ...` (or chown the completed
mode-0600 file before startup); do not copy the general node wallet into escrow. Deploy
code and dependencies, set `OTC_ACCEPTING_ORDERS=0`, start bot, and read back
command sync plus detailed local health.

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
the bot after deposit and before confirmation to prove recovery. In a disabled,
operator-controlled pilot run, use the tested one-shot hold immediately after
the FULL `prepared` DB commit, stop/restart the service, then prove reconciliation
broadcasts the stored tx bytes and produces the same single txid. The hold must
refuse to activate while public intake is enabled and must be removed/unset
before launch. The pass
condition is buyer receives the exact net amount, liability returns to zero,
and the network-fee buffer reconciles exactly.

- [ ] **Step 2: Run WTB, refund, and dispute cases**

WTB: buyer posts, seller accepts/deposits, buyer pays externally only after
deposit verification, both confirm, release reconciles.

Refund: funded open WTS cancels once under two simultaneous attempts, seller
receives the quoted refundable amount, and one txid exists.

Dispute: matched order times out or is disputed, two simultaneous admin
resolutions produce one queued/prepared operation and one txid. Use a reason that
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
OTC_ACCEPTING_ORDERS=1
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
