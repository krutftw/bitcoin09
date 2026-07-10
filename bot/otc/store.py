from __future__ import annotations

import json
import hashlib
import os
import re
import sqlite3
import sys
import unicodedata
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from bot.otc.domain import (
    MAX_09C_UNITS,
    FeeQuote,
    OrderSide,
    OrderState,
    parse_asset,
    parse_method,
    parse_total_price,
)

SCHEMA_VERSION = 4


class MigrationBlocked(RuntimeError):
    """Raised when an automatic migration cannot preserve fund provenance."""


class UnsupportedSchemaVersion(RuntimeError):
    """Raised when a database was created by a newer application version."""


class AccountingInvariantError(RuntimeError):
    """Raised when durable fund evidence cannot be interpreted safely."""


@dataclass(frozen=True)
class ReconciliationResult:
    healthy: bool
    health_issues: tuple[str, ...]
    recovery_order_ids: tuple[int, ...]
    restricted_outpoints: tuple[tuple[str, int, int], ...]
    scan_ids: tuple[tuple[str, int], ...]
    order_changes: tuple["DepositOrderChange", ...]


@dataclass(frozen=True)
class DepositOrderChange:
    order_id: int
    old_state: str
    new_state: str
    credited_delta_units: int
    main_delta_units: int
    recovery_delta_units: int
    recovery_total_units: int


@dataclass(frozen=True)
class ConfirmationMutation:
    order: Mapping[str, Any]
    mutated: bool
    role: str
    release_queued: bool


@dataclass(frozen=True)
class WalletSolvencySnapshot:
    expected_tip_hash: str
    expected_tip_height: int
    wallet_spendable_units: int
    provisional_restricted_units: int
    customer_liability_units: int
    pending_platform_outflow_units: int
    usable_wallet_units: int
    required_wallet_units: int
    restricted_outpoints: tuple[tuple[str, int, int], ...]
    wallet_snapshot_hash: str


@dataclass(frozen=True)
class ClaimedTransfer:
    transfer_id: int
    operation_key: str
    order_id: int | None
    kind: str
    amount_units: int
    network_fee_units: int
    earned_fee_units: int
    destination: str
    attempt_count: int
    reserved_at: int
    expected_tip_hash: str
    expected_tip_height: int
    provisional_restricted_units: int
    restricted_outpoints: tuple[tuple[str, int, int], ...]


@dataclass(frozen=True)
class AttachRecoveryResult:
    classification: str
    transfer: Mapping[str, Any]


@dataclass(frozen=True)
class BroadcastAuthorizationResult:
    authorized: bool
    transfer: Mapping[str, Any]


def _split_sql_script(script: str) -> tuple[str, ...]:
    statements: list[str] = []
    pending = ""
    for line in script.splitlines(keepends=True):
        pending += line
        if sqlite3.complete_statement(pending):
            statement = pending.strip()
            if statement:
                statements.append(statement)
            pending = ""
    if pending.strip():
        raise RuntimeError("the v4 schema source contains incomplete SQL")
    return tuple(statements)


_SCHEMA_SOURCE = Path(__file__).with_name("schema_v4.sql").read_text(encoding="utf-8")
_ALL_V4_STATEMENTS = _split_sql_script(_SCHEMA_SOURCE)
_V4_SCHEMA_STATEMENTS = tuple(
    statement
    for statement in _ALL_V4_STATEMENTS
    if not statement.lstrip().upper().startswith("PRAGMA ")
    and not statement.lstrip().upper().startswith("INSERT INTO SCHEMA_META")
)

_V3_SCHEMA_SOURCE = """
CREATE TABLE schema_meta (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  version INTEGER NOT NULL
);
CREATE TABLE users (
  user_id INTEGER PRIMARY KEY,
  username TEXT NOT NULL,
  wallet_addr TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE orders (
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
  deposit_confirmed_units INTEGER NOT NULL DEFAULT 0
    CHECK(deposit_confirmed_units >= 0),
  total_price TEXT NOT NULL,
  settlement_asset TEXT NOT NULL,
  settlement_network TEXT,
  payment_method TEXT NOT NULL,
  state TEXT NOT NULL,
  deposit_addr TEXT,
  buyer_confirmed INTEGER NOT NULL DEFAULT 0
    CHECK(buyer_confirmed IN (0,1)),
  seller_confirmed INTEGER NOT NULL DEFAULT 0
    CHECK(seller_confirmed IN (0,1)),
  deposit_deadline INTEGER,
  matched_at INTEGER,
  trade_deadline INTEGER,
  disputed_at INTEGER,
  completed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE transfers (
  transfer_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER REFERENCES orders(order_id),
  kind TEXT NOT NULL CHECK(kind IN (
    'release','refund','resolve_buyer','resolve_seller','fee_withdrawal',
    'excess_refund'
  )),
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
CREATE INDEX orders_by_state ON orders(state, updated_at);
CREATE INDEX orders_by_deposit ON orders(deposit_addr);
CREATE UNIQUE INDEX one_active_order_transfer
ON transfers(order_id)
WHERE state IN ('reserved','broadcast','uncertain');
CREATE TABLE audit_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER,
  actor_id INTEGER,
  event_type TEXT NOT NULL,
  old_state TEXT,
  new_state TEXT,
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL
);
INSERT INTO schema_meta(id, version) VALUES(1, 3);
"""

_LIVE_PROTOTYPE_SOURCE = """
CREATE TABLE users (
  user_id INTEGER PRIMARY KEY,
  username TEXT,
  wallet_addr TEXT,
  created_at INTEGER,
  updated_at INTEGER
);
CREATE TABLE orders (
  order_id INTEGER PRIMARY KEY AUTOINCREMENT,
  seller_id INTEGER NOT NULL,
  seller_name TEXT,
  amount TEXT NOT NULL,
  price TEXT NOT NULL,
  currency TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending_deposit',
  escrow_bal_before TEXT,
  deposit_addr TEXT,
  deposit_expected TEXT,
  deposit_confirmed_balance TEXT,
  buyer_id INTEGER,
  buyer_name TEXT,
  seller_confirmed INTEGER DEFAULT 0,
  buyer_confirmed INTEGER DEFAULT 0,
  release_txid TEXT,
  refund_txid TEXT,
  fee TEXT,
  cancel_reason TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  matched_at INTEGER,
  disputed_at INTEGER,
  completed_at INTEGER
);
CREATE TABLE withdrawals (
  withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id INTEGER NOT NULL,
  amount TEXT NOT NULL,
  address TEXT NOT NULL,
  txid TEXT,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_deposit_addr ON orders(deposit_addr);
CREATE INDEX idx_orders_seller ON orders(seller_id);
CREATE INDEX idx_orders_buyer ON orders(buyer_id);
"""

_LIVE_PROTOTYPE_INDEXES = {
    "idx_orders_status": "CREATE INDEX idx_orders_status ON orders(status)",
    "idx_orders_deposit_addr": (
        "CREATE INDEX idx_orders_deposit_addr ON orders(deposit_addr)"
    ),
    "idx_orders_seller": "CREATE INDEX idx_orders_seller ON orders(seller_id)",
    "idx_orders_buyer": "CREATE INDEX idx_orders_buyer ON orders(buyer_id)",
}


def _canonical_schema_sql(sql: str) -> str:
    source = sql.strip()
    if source.endswith(";"):
        source = source[:-1].rstrip()
    output: list[str] = []
    pending_space = False
    quote: str | None = None
    index = 0
    while index < len(source):
        char = source[index]
        if quote is not None:
            output.append(char)
            if quote == "[":
                if char == "]":
                    quote = None
            elif char == quote:
                if index + 1 < len(source) and source[index + 1] == quote:
                    index += 1
                    output.append(source[index])
                else:
                    quote = None
            index += 1
            continue

        if char.isspace():
            pending_space = True
            index += 1
            continue
        if char in ("'", '"', "`", "["):
            if pending_space and output and output[-1] not in "(),":
                output.append(" ")
            output.append(char)
            quote = char
            pending_space = False
            index += 1
            continue
        if char in "(),":
            while output and output[-1] == " ":
                output.pop()
            output.append(char)
            pending_space = False
            index += 1
            continue
        if pending_space and output and output[-1] not in "(),":
            output.append(" ")
        output.append(char)
        pending_space = False
        index += 1
    if quote is not None:
        raise RuntimeError("schema SQL contains an unterminated quoted region")
    return "".join(output)


def _is_verified_sqlite_internal(
    object_type: str, name: str, table_name: str, sql: str | None
) -> bool:
    if (
        object_type == "table"
        and name == "sqlite_sequence"
        and table_name == "sqlite_sequence"
        and type(sql) is str
        and _canonical_schema_sql(sql)
        == _canonical_schema_sql("CREATE TABLE sqlite_sequence(name,seq)")
    ):
        return True
    return (
        object_type == "index"
        and sql is None
        and re.fullmatch(r"sqlite_autoindex_[A-Za-z0-9_]+_[1-9][0-9]*", name)
        is not None
        and bool(table_name)
    )


def _catalog_from_script(script: str) -> dict[str, tuple[str, str]]:
    conn = sqlite3.connect(":memory:")
    try:
        conn.execute("PRAGMA foreign_keys=ON")
        conn.execute("PRAGMA recursive_triggers=ON")
        conn.executescript(script)
        return _catalog_from_connection(conn)
    finally:
        conn.close()


def _catalog_from_connection(
    conn: sqlite3.Connection,
) -> dict[str, tuple[str, str]]:
    catalog: dict[str, tuple[str, str]] = {}
    for object_type, name, table_name, sql in conn.execute(
        "SELECT type, name, tbl_name, sql FROM sqlite_master"
    ):
        if _is_verified_sqlite_internal(object_type, name, table_name, sql):
            continue
        if name in catalog:
            raise RuntimeError(f"duplicate schema-source catalog name {name}")
        catalog[name] = (object_type, sql)
    return catalog


def _prototype_evidence_catalog() -> dict[str, tuple[str, str]]:
    conn = sqlite3.connect(":memory:")
    try:
        conn.executescript(_LIVE_PROTOTYPE_SOURCE)
        conn.execute("ALTER TABLE orders RENAME TO orders_v2_archive")
        evidence_names = {
            "orders_v2_archive",
            "withdrawals",
            *_LIVE_PROTOTYPE_INDEXES,
        }
        return {
            name: definition
            for name, definition in _catalog_from_connection(conn).items()
            if name in evidence_names
        }
    finally:
        conn.close()


def _v3_archive_catalog() -> dict[str, tuple[str, str]]:
    conn = sqlite3.connect(":memory:")
    try:
        conn.execute("PRAGMA foreign_keys=ON")
        conn.executescript(_V3_SCHEMA_SOURCE)
        for index_name in (
            "orders_by_state",
            "orders_by_deposit",
            "one_active_order_transfer",
        ):
            conn.execute(f'DROP INDEX "{index_name}"')
        conn.execute("ALTER TABLE orders RENAME TO orders_v3_archive")
        conn.execute("ALTER TABLE transfers RENAME TO transfers_v3_archive")
        conn.execute("ALTER TABLE audit_events RENAME TO audit_events_v3_archive")
        conn.execute("ALTER TABLE schema_meta RENAME TO schema_meta_v3_archive")
        archive_names = {
            "orders_v3_archive",
            "transfers_v3_archive",
            "audit_events_v3_archive",
            "schema_meta_v3_archive",
        }
        return {
            name: definition
            for name, definition in _catalog_from_connection(conn).items()
            if name in archive_names
        }
    finally:
        conn.close()


def _merged_catalog(
    *catalogs: Mapping[str, tuple[str, str]],
) -> dict[str, tuple[str, str]]:
    merged: dict[str, tuple[str, str]] = {}
    for catalog in catalogs:
        overlap = set(merged) & set(catalog)
        if overlap:
            raise RuntimeError(
                f"duplicate expected catalog object: {sorted(overlap)[0]}"
            )
        merged.update(catalog)
    return merged


_EXPECTED_V4_OBJECTS = _catalog_from_script(_SCHEMA_SOURCE)
_EXPECTED_V3_OBJECTS = _catalog_from_script(_V3_SCHEMA_SOURCE)
_EXPECTED_LIVE_PROTOTYPE_OBJECTS = _catalog_from_script(_LIVE_PROTOTYPE_SOURCE)
_EXPECTED_PROTOTYPE_EVIDENCE = _prototype_evidence_catalog()
_EXPECTED_V3_ARCHIVES = _v3_archive_catalog()
_EXPECTED_WITHDRAWALS = {"withdrawals": _EXPECTED_LIVE_PROTOTYPE_OBJECTS["withdrawals"]}
_EXPECTED_V3_CATALOG_VARIANTS = (
    ("fresh-v3", _EXPECTED_V3_OBJECTS),
    (
        "v3-with-withdrawals",
        _merged_catalog(_EXPECTED_V3_OBJECTS, _EXPECTED_WITHDRAWALS),
    ),
    (
        "v3-from-live-prototype",
        _merged_catalog(_EXPECTED_V3_OBJECTS, _EXPECTED_PROTOTYPE_EVIDENCE),
    ),
)
_V3_VARIANT_ORIGINS = {
    "fresh-v3": "v3_fresh",
    "v3-with-withdrawals": "v3_with_withdrawals",
    "v3-from-live-prototype": "v3_live_prototype",
}
_EXPECTED_V4_CATALOG_BY_ORIGIN = {
    "fresh": _EXPECTED_V4_OBJECTS,
    "live_prototype": _merged_catalog(
        _EXPECTED_V4_OBJECTS, _EXPECTED_PROTOTYPE_EVIDENCE
    ),
    "v3_fresh": _merged_catalog(_EXPECTED_V4_OBJECTS, _EXPECTED_V3_ARCHIVES),
    "v3_with_withdrawals": _merged_catalog(
        _EXPECTED_V4_OBJECTS,
        _EXPECTED_V3_ARCHIVES,
        _EXPECTED_WITHDRAWALS,
    ),
    "v3_live_prototype": _merged_catalog(
        _EXPECTED_V4_OBJECTS,
        _EXPECTED_V3_ARCHIVES,
        _EXPECTED_PROTOTYPE_EVIDENCE,
    ),
}
_V3_INDEX_NAMES = (
    "orders_by_state",
    "orders_by_deposit",
    "one_active_order_transfer",
)


@dataclass(frozen=True)
class _MigrationPlan:
    kind: str
    origin: str
    migrate_users: bool = False
    migrate_orders: bool = False


class Store:
    def __init__(
        self,
        path: str | Path,
        *,
        network: str = "btc09-mainnet",
        attach_commit_boundary: Callable[[sqlite3.Connection], None] | None = None,
    ) -> None:
        self.path = Path(path)
        self.network = self._network(network)
        self._attach_commit_boundary = attach_commit_boundary
        self._health_failure: str | None = None

    def connect(self) -> sqlite3.Connection:
        return self._connect(apply_wal=True)

    def _connect(self, *, apply_wal: bool) -> sqlite3.Connection:
        conn = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        try:
            conn.row_factory = sqlite3.Row
            conn.execute("PRAGMA busy_timeout=30000")
            conn.execute("PRAGMA foreign_keys=ON")
            conn.execute("PRAGMA recursive_triggers=ON")
            if apply_wal:
                mode = conn.execute("PRAGMA journal_mode=WAL").fetchone()[0]
                if str(mode).lower() != "wal":
                    raise RuntimeError("SQLite refused WAL journal mode")
            conn.execute("PRAGMA synchronous=FULL")
        except BaseException:
            conn.close()
            raise
        return conn

    def initialize(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        conn = self._connect(apply_wal=False)
        original_journal_mode = str(
            conn.execute("PRAGMA journal_mode").fetchone()[0]
        ).lower()
        wal_activated = False
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._preflight_initialization(conn)
            conn.execute("ROLLBACK")

            mode = str(conn.execute("PRAGMA journal_mode=WAL").fetchone()[0]).lower()
            if mode != "wal":
                raise RuntimeError("SQLite refused WAL journal mode")
            wal_activated = original_journal_mode != "wal"
            conn.execute("PRAGMA synchronous=FULL")

            conn.execute("BEGIN IMMEDIATE")
            plan = self._preflight_initialization(conn)
            self._migration_checkpoint("writer_preflight")
            if plan.kind == "v4":
                foreign_key_failures = conn.execute(
                    "PRAGMA foreign_key_check"
                ).fetchall()
                if foreign_key_failures:
                    raise MigrationBlocked(
                        "existing v4 database failed foreign-key validation"
                    )
                conn.execute("COMMIT")
                return

            self._apply_migration(conn, plan)
            self._migration_checkpoint("archives")

            for statement in _V4_SCHEMA_STATEMENTS:
                conn.execute(statement)
            self._migration_checkpoint("schema")
            self._validate_exact_catalog(
                self._read_catalog(conn),
                _EXPECTED_V4_CATALOG_BY_ORIGIN[plan.origin],
                f"v4 origin {plan.origin}",
            )
            self._validate_v4_evidence_rows(conn)

            foreign_key_failures = conn.execute("PRAGMA foreign_key_check").fetchall()
            if foreign_key_failures:
                raise MigrationBlocked("v4 migration failed foreign-key validation")
            self._migration_checkpoint("foreign_keys")

            conn.execute(
                """
                INSERT INTO schema_meta(id, version, origin) VALUES(1, ?, ?)
                """,
                (SCHEMA_VERSION, plan.origin),
            )
            self._migration_checkpoint("stamp")
            conn.execute("COMMIT")
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            if wal_activated:
                self._restore_journal_mode(conn, original_journal_mode)
            raise
        finally:
            conn.close()

    def integrity_check(self) -> str:
        conn = self.connect()
        try:
            row = conn.execute("PRAGMA integrity_check").fetchone()
            if row is None or type(row[0]) is not str:
                raise RuntimeError("database integrity check did not return a result")
            return row[0]
        finally:
            conn.close()

    def create_order(
        self,
        *,
        side: OrderSide,
        maker_id: int,
        maker_name: str,
        net_amount_units: int,
        network_fee_units: int,
        service_fee_units: int,
        deposit_required_units: int,
        total_price: str,
        settlement_asset: str,
        settlement_network: str | None,
        payment_method: str,
        state: OrderState,
        created_at: int,
        updated_at: int,
        buyer_id: int | None = None,
        buyer_name: str | None = None,
        seller_id: int | None = None,
        seller_name: str | None = None,
        deposit_addr: str | None = None,
        buyer_confirmed: int = 0,
        seller_confirmed: int = 0,
        deposit_deadline: int | None = None,
        matched_at: int | None = None,
        trade_deadline: int | None = None,
        disputed_at: int | None = None,
        completed_at: int | None = None,
        funded_at: int | None = None,
        maker_wallet_addr: str | None = None,
    ) -> int:
        if type(side) is not OrderSide:
            raise ValueError("side must be a valid order side")
        if type(state) is not OrderState:
            raise ValueError("state must be a valid order state")

        self._require_integer(maker_id, "maker ID")
        quote = FeeQuote(
            net_amount_units,
            network_fee_units,
            service_fee_units,
            deposit_required_units,
        )
        if any(
            value > MAX_09C_UNITS
            for value in (
                quote.net_amount,
                quote.network_fee,
                quote.service_fee,
                quote.deposit_required,
            )
        ):
            raise ValueError("order amounts must not exceed the protocol supply")
        for flag, label in (
            (buyer_confirmed, "buyer confirmation"),
            (seller_confirmed, "seller confirmation"),
        ):
            self._require_integer(flag, label)
            if flag not in (0, 1):
                raise ValueError(f"{label} must be 0 or 1")
        for value, label in (
            (created_at, "creation time"),
            (updated_at, "update time"),
        ):
            self._require_integer(value, label)
        for value, label in (
            (buyer_id, "buyer ID"),
            (seller_id, "seller ID"),
            (deposit_deadline, "deposit deadline"),
            (matched_at, "match time"),
            (trade_deadline, "trade deadline"),
            (disputed_at, "dispute time"),
            (completed_at, "completion time"),
            (funded_at, "funded time"),
        ):
            self._require_optional_integer(value, label)

        maker_name = self._bounded_text(maker_name, "maker name", 128)
        total_price = parse_total_price(total_price)
        if type(settlement_asset) is not str:
            raise ValueError("settlement asset must be text")
        settlement_asset = parse_asset(settlement_asset)
        settlement_network = self._optional_machine_text(
            settlement_network,
            "settlement network",
            48,
            re.compile(r"[A-Za-z0-9._ -]+\Z"),
        )
        if type(payment_method) is not str:
            raise ValueError("payment method must be text")
        payment_method = parse_method(payment_method)
        buyer_name = self._optional_bounded_text(buyer_name, "buyer name", 128)
        seller_name = self._optional_bounded_text(seller_name, "seller name", 128)
        deposit_addr = self._optional_bounded_text(deposit_addr, "deposit address", 128)
        maker_wallet_addr = self._optional_bounded_text(
            maker_wallet_addr, "maker wallet address", 128
        )

        if side is OrderSide.BUY:
            if buyer_id is None:
                buyer_id = maker_id
            if buyer_name is None:
                buyer_name = maker_name
        else:
            if seller_id is None:
                seller_id = maker_id
            if seller_name is None:
                seller_name = maker_name

        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            conn.execute(
                """
                INSERT INTO users(user_id, username, wallet_addr, created_at, updated_at)
                VALUES(?, ?, ?, ?, ?)
                ON CONFLICT(user_id) DO UPDATE SET
                  username=excluded.username,
                  wallet_addr=COALESCE(excluded.wallet_addr,users.wallet_addr),
                  updated_at=excluded.updated_at
                """,
                (
                    maker_id,
                    maker_name,
                    maker_wallet_addr,
                    created_at,
                    updated_at,
                ),
            )
            cursor = conn.execute(
                """
                INSERT INTO orders (
                  side, maker_id, maker_name, buyer_id, buyer_name,
                  seller_id, seller_name, net_amount_units, network_fee_units,
                  service_fee_units, deposit_required_units, total_price,
                  settlement_asset, settlement_network, payment_method, state,
                  deposit_addr, buyer_confirmed, seller_confirmed,
                  deposit_deadline, matched_at, trade_deadline, disputed_at,
                  completed_at, funded_at, created_at, updated_at
                ) VALUES (
                  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
                  ?, ?, ?, ?, ?, ?, ?
                )
                """,
                (
                    side.value,
                    maker_id,
                    maker_name,
                    buyer_id,
                    buyer_name,
                    seller_id,
                    seller_name,
                    quote.net_amount,
                    quote.network_fee,
                    quote.service_fee,
                    quote.deposit_required,
                    total_price,
                    settlement_asset,
                    settlement_network,
                    payment_method,
                    state.value,
                    deposit_addr,
                    buyer_confirmed,
                    seller_confirmed,
                    deposit_deadline,
                    matched_at,
                    trade_deadline,
                    disputed_at,
                    completed_at,
                    funded_at,
                    created_at,
                    updated_at,
                ),
            )
            if cursor.lastrowid is None:
                raise RuntimeError("database did not return the new order ID")
            order_id = int(cursor.lastrowid)
            conn.execute("COMMIT")
            return order_id
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def get_order(self, *, order_id: int) -> sqlite3.Row | None:
        self._require_integer(order_id, "order ID")
        conn = self.connect()
        try:
            return conn.execute(
                "SELECT * FROM orders WHERE order_id = ?", (order_id,)
            ).fetchone()
        finally:
            conn.close()

    def list_open_orders(self) -> tuple[Mapping[str, Any], ...]:
        conn = self.connect()
        try:
            return tuple(
                dict(row)
                for row in conn.execute(
                    """
                    SELECT * FROM orders WHERE state='open'
                    ORDER BY created_at,order_id
                    """
                ).fetchall()
            )
        finally:
            conn.close()

    def watched_deposit_addresses(self) -> tuple[str, ...]:
        conn = self.connect()
        try:
            return tuple(
                row[0]
                for row in conn.execute(
                    """
                    SELECT DISTINCT deposit_addr FROM orders
                    WHERE deposit_addr IS NOT NULL ORDER BY deposit_addr
                    """
                ).fetchall()
            )
        finally:
            conn.close()

    def deposit_accounting(self, *, order_id: int) -> Mapping[str, int]:
        self._require_integer(order_id, "order ID")
        conn = self.connect()
        try:
            if (
                conn.execute(
                    "SELECT 1 FROM orders WHERE order_id=?", (order_id,)
                ).fetchone()
                is None
            ):
                raise ValueError("order does not exist")
            return self._deposit_accounting_conn(
                conn, order_id=order_id, network=self.network
            )
        finally:
            conn.close()

    @classmethod
    def _deposit_accounting_conn(
        cls, conn: sqlite3.Connection, *, order_id: int, network: str
    ) -> Mapping[str, int]:
        row = conn.execute(
            """
            SELECT COALESCE(SUM(amount_units),0) AS credited_units,
              COALESCE(SUM(main_units),0) AS main_units,
              COALESCE(SUM(recovery_units),0) AS recovery_units
            FROM deposit_credits
            WHERE order_id=? AND network=? AND credited_at IS NOT NULL
            """,
            (order_id, network),
        ).fetchone()
        return {
            "credited_units": cls._nonnegative_aggregate(
                row["credited_units"], "credited deposit units"
            ),
            "main_units": cls._nonnegative_aggregate(
                row["main_units"], "main deposit units"
            ),
            "recovery_units": cls._nonnegative_aggregate(
                row["recovery_units"], "recovery deposit units"
            ),
        }

    def get_order_transfer(self, *, order_id: int) -> Mapping[str, Any] | None:
        self._require_integer(order_id, "order ID")
        conn = self.connect()
        try:
            row = conn.execute(
                """
                SELECT * FROM transfers WHERE order_id=? AND is_main_outcome=1
                ORDER BY transfer_id LIMIT 1
                """,
                (order_id,),
            ).fetchone()
            return None if row is None else dict(row)
        finally:
            conn.close()

    def get_order_recovery_transfer(self, *, order_id: int) -> Mapping[str, Any] | None:
        self._require_integer(order_id, "order ID")
        conn = self.connect()
        try:
            row = conn.execute(
                """
                SELECT * FROM transfers
                WHERE order_id=? AND kind='recovery_refund'
                ORDER BY transfer_id DESC LIMIT 1
                """,
                (order_id,),
            ).fetchone()
            return None if row is None else dict(row)
        finally:
            conn.close()

    def count_transfers(self, *, order_id: int | None = None) -> int:
        self._require_optional_integer(order_id, "order ID")
        conn = self.connect()
        try:
            if order_id is None:
                value = conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0]
            else:
                value = conn.execute(
                    "SELECT COUNT(*) FROM transfers WHERE order_id=?", (order_id,)
                ).fetchone()[0]
            if type(value) is not int or value < 0:
                raise AccountingInvariantError("transfer count is malformed")
            return value
        finally:
            conn.close()

    def get_transfer(self, *, transfer_id: int) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        if transfer_id <= 0:
            raise ValueError("transfer ID must be positive")
        conn = self.connect()
        try:
            return dict(self._transfer_row_conn(conn, transfer_id=transfer_id))
        finally:
            conn.close()

    def list_reconcilable_transfers(self) -> tuple[Mapping[str, Any], ...]:
        conn = self.connect()
        try:
            return tuple(
                dict(row)
                for row in conn.execute(
                    """
                    SELECT * FROM transfers
                    WHERE state IN ('prepared','broadcast','confirmed','uncertain')
                    ORDER BY transfer_id
                    """
                ).fetchall()
            )
        finally:
            conn.close()

    def health_issues(self) -> tuple[str, ...]:
        conn = self.connect()
        try:
            issues = set(self._health_issues_conn(conn, network=self.network))
            if self._health_failure is not None:
                issues.add("process_health_failure")
            return tuple(sorted(issues))
        finally:
            conn.close()

    def pending_platform_outflow_units(self) -> int:
        conn = self.connect()
        try:
            return self._pending_platform_outflow_conn(conn)
        finally:
            conn.close()

    def active_wallet_transfer(self) -> Mapping[str, Any] | None:
        conn = self.connect()
        try:
            rows = conn.execute(
                """
                SELECT * FROM transfers
                WHERE state IN ('reserved','prepared','broadcast')
                ORDER BY transfer_id
                """
            ).fetchall()
            if len(rows) > 1:
                raise AccountingInvariantError("multiple wallet transfers are active")
            return None if not rows else dict(rows[0])
        finally:
            conn.close()

    def transfer_allocation_units(self, *, transfer_id: int) -> int:
        self._require_integer(transfer_id, "transfer ID")
        conn = self.connect()
        try:
            if (
                conn.execute(
                    "SELECT 1 FROM transfers WHERE transfer_id=?", (transfer_id,)
                ).fetchone()
                is None
            ):
                raise ValueError("transfer does not exist")
            value = conn.execute(
                """
                SELECT COALESCE(SUM(units),0) FROM transfer_credit_allocations
                WHERE transfer_id=?
                """,
                (transfer_id,),
            ).fetchone()[0]
            return self._nonnegative_aggregate(value, "transfer allocation units")
        finally:
            conn.close()

    def validate_claimed_fee_withdrawal(
        self,
        *,
        transfer_id: int,
        expected_attempt_count: int,
        expected_reserved_at: int,
    ) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(expected_attempt_count, "expected attempt count")
        self._require_integer(expected_reserved_at, "expected reservation time")
        conn = self.connect()
        try:
            conn.execute("BEGIN")
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if (
                row["kind"] != "fee_withdrawal"
                or row["order_id"] is not None
                or row["earned_fee_units"] != 0
                or row["state"] != "reserved"
                or row["attempt_count"] != expected_attempt_count
                or row["reserved_at"] != expected_reserved_at
            ):
                raise AccountingInvariantError(
                    "fee withdrawal reservation identity is invalid"
                )
            allocated = conn.execute(
                """
                SELECT COALESCE(SUM(units),0) FROM transfer_credit_allocations
                WHERE transfer_id=?
                """,
                (transfer_id,),
            ).fetchone()[0]
            if allocated != 0:
                raise AccountingInvariantError(
                    "fee withdrawal has customer credit allocations"
                )
            available = self._available_fee_conn(conn)
            if available < 0:
                raise AccountingInvariantError(
                    "fee withdrawal exceeds immutable earned revenue"
                )
            gross = conn.execute(
                """
                SELECT COALESCE(SUM(earned_fee_units),0) FROM transfers
                WHERE state='confirmed' AND kind IN ('release','resolve_buyer')
                """
            ).fetchone()[0]
            encumbered = conn.execute(
                """
                SELECT COALESCE(SUM(amount_units + network_fee_units),0)
                FROM transfers
                WHERE kind='fee_withdrawal' AND state!='cancelled'
                """
            ).fetchone()[0]
            if (
                gross < encumbered
                or row["amount_units"] + row["network_fee_units"] <= 0
            ):
                raise AccountingInvariantError(
                    "fee withdrawal reservation is not covered by earned revenue"
                )
            conn.execute("COMMIT")
            return dict(row)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def append_audit(
        self,
        *,
        event_type: str,
        created_at: int,
        order_id: int | None = None,
        actor_id: int | None = None,
        old_state: OrderState | None = None,
        new_state: OrderState | None = None,
        detail: Mapping[str, Any] | None = None,
    ) -> int:
        self._require_optional_integer(order_id, "order ID")
        self._require_optional_integer(actor_id, "actor ID")
        self._require_integer(created_at, "creation time")
        event_type = self._machine_text(
            event_type,
            "audit event type",
            80,
            re.compile(r"[a-z0-9:_-]+\Z"),
        )
        old_state_value = self._optional_state_value(old_state)
        new_state_value = self._optional_state_value(new_state)
        if detail is None:
            detail = {}
        if not isinstance(detail, Mapping):
            raise ValueError("audit detail must be a JSON object")
        try:
            detail_json = json.dumps(
                dict(detail),
                sort_keys=True,
                separators=(",", ":"),
                ensure_ascii=True,
                allow_nan=False,
            )
        except (TypeError, ValueError):
            raise ValueError(
                "audit detail must contain JSON-compatible values"
            ) from None
        if not 2 <= len(detail_json.encode("utf-8")) <= 4_000:
            raise ValueError("audit detail must be at most 4000 bytes")

        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            cursor = conn.execute(
                """
                INSERT INTO audit_events (
                  order_id, actor_id, event_type, old_state, new_state,
                  detail_json, created_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    order_id,
                    actor_id,
                    event_type,
                    old_state_value,
                    new_state_value,
                    detail_json,
                    created_at,
                ),
            )
            if cursor.lastrowid is None:
                raise RuntimeError("database did not return the new audit event ID")
            event_id = int(cursor.lastrowid)
            conn.execute("COMMIT")
            return event_id
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def reserve_accept(
        self,
        *,
        order_id: int,
        actor_id: int,
        actor_name: str,
        preallocated_deposit_addr: str | None,
        deposit_deadline: int | None,
        now: int,
        actor_wallet: str | None = None,
        trade_deadline: int | None = None,
    ) -> Mapping[str, Any] | None:
        self._require_integer(order_id, "order ID")
        self._require_integer(actor_id, "actor ID")
        self._require_integer(now, "acceptance time")
        if order_id <= 0 or actor_id <= 0:
            raise ValueError("order and actor IDs must be positive")
        actor_name = self._bounded_text(actor_name, "actor name", 128)
        self._require_optional_integer(deposit_deadline, "deposit deadline")
        self._require_optional_integer(trade_deadline, "trade deadline")
        if preallocated_deposit_addr is not None:
            preallocated_deposit_addr = self._bounded_text(
                preallocated_deposit_addr, "preallocated deposit address", 128
            )
        actor_wallet = self._optional_bounded_text(
            actor_wallet, "actor wallet address", 128
        )

        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            if (
                order is None
                or order["state"] != "open"
                or actor_id == order["maker_id"]
            ):
                conn.execute("COMMIT")
                return None
            conn.execute(
                """
                INSERT INTO users(user_id,username,wallet_addr,created_at,updated_at)
                VALUES(?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET
                  username=excluded.username,
                  wallet_addr=COALESCE(excluded.wallet_addr,users.wallet_addr),
                  updated_at=excluded.updated_at
                """,
                (actor_id, actor_name, actor_wallet, now, now),
            )
            if order["side"] == "sell":
                if (
                    preallocated_deposit_addr is not None
                    or deposit_deadline is not None
                ):
                    raise ValueError("WTS acceptance does not assign a deposit address")
                if trade_deadline is not None and trade_deadline <= now:
                    raise ValueError("trade deadline must be in the future")
                cursor = conn.execute(
                    """
                    UPDATE orders SET buyer_id=?,buyer_name=?,state='matched',
                      matched_at=?,trade_deadline=?,updated_at=?
                    WHERE order_id=? AND side='sell' AND state='open'
                      AND buyer_id IS NULL AND buyer_name IS NULL AND maker_id!=?
                    """,
                    (
                        actor_id,
                        actor_name,
                        now,
                        trade_deadline,
                        now,
                        order_id,
                        actor_id,
                    ),
                )
            else:
                if preallocated_deposit_addr is None or deposit_deadline is None:
                    raise ValueError(
                        "WTB acceptance requires a fresh address and deposit deadline"
                    )
                if deposit_deadline <= now:
                    raise ValueError("deposit deadline must be in the future")
                if trade_deadline is not None:
                    raise ValueError("WTB trade deadline starts after deposit funding")
                cursor = conn.execute(
                    """
                    UPDATE orders SET seller_id=?,seller_name=?,deposit_addr=?,
                      deposit_deadline=?,state='awaiting_deposit',matched_at=?,updated_at=?
                    WHERE order_id=? AND side='buy' AND state='open'
                      AND seller_id IS NULL AND seller_name IS NULL
                      AND deposit_addr IS NULL AND maker_id!=?
                    """,
                    (
                        actor_id,
                        actor_name,
                        preallocated_deposit_addr,
                        deposit_deadline,
                        now,
                        now,
                        order_id,
                        actor_id,
                    ),
                )
            if cursor.rowcount != 1:
                conn.execute("ROLLBACK")
                return None
            accepted = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            self._append_audit_conn(
                conn,
                order_id=order_id,
                actor_id=actor_id,
                event_type="order_accepted",
                old_state="open",
                new_state=accepted["state"],
                detail={"side": order["side"]},
                created_at=now,
            )
            conn.execute("COMMIT")
            return dict(accepted)
        except sqlite3.IntegrityError as exc:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise AccountingInvariantError(
                "order acceptance violated durable identity"
            ) from exc
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def record_confirmation(
        self, *, order_id: int, actor_id: int, now: int
    ) -> Mapping[str, Any] | None:
        result = self.record_confirmation_result(
            order_id=order_id, actor_id=actor_id, now=now
        )
        return None if result is None else result.order

    def record_confirmation_result(
        self, *, order_id: int, actor_id: int, now: int
    ) -> ConfirmationMutation | None:
        self._require_integer(order_id, "order ID")
        self._require_integer(actor_id, "actor ID")
        self._require_integer(now, "confirmation time")
        if order_id <= 0 or actor_id <= 0:
            raise ValueError("order and actor IDs must be positive")
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            if order is None or order["state"] != "matched":
                # A replay after the second confirmation returns the already
                # committed release state to the same party without a new audit.
                if (
                    order is not None
                    and order["state"] == "release_reserved"
                    and actor_id
                    in (
                        order["buyer_id"],
                        order["seller_id"],
                    )
                ):
                    role = "buyer" if actor_id == order["buyer_id"] else "seller"
                    conn.execute("COMMIT")
                    return ConfirmationMutation(dict(order), False, role, False)
                conn.execute("COMMIT")
                return None
            if actor_id == order["buyer_id"]:
                field = "buyer_confirmed"
                role = "buyer"
            elif actor_id == order["seller_id"]:
                field = "seller_confirmed"
                role = "seller"
            else:
                conn.execute("COMMIT")
                return None
            if order[field] == 1:
                conn.execute("COMMIT")
                return ConfirmationMutation(dict(order), False, role, False)

            second = order["buyer_confirmed"] + order["seller_confirmed"] == 1
            if second:
                self._queue_order_transfer_conn(
                    conn,
                    order=order,
                    kind="release",
                    now=now,
                    actor_id=actor_id,
                    allow_release=True,
                    transition_order=False,
                )
                new_state = "release_reserved"
            else:
                new_state = "matched"
            conn.execute(
                f"""
                UPDATE orders SET {field}=1,state=?,updated_at=?
                WHERE order_id=? AND state='matched' AND {field}=0
                """,
                (new_state, now, order_id),
            )
            updated = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            self._append_audit_conn(
                conn,
                order_id=order_id,
                actor_id=actor_id,
                event_type="payment_confirmed",
                old_state="matched",
                new_state=new_state,
                detail={"role": "buyer" if field == "buyer_confirmed" else "seller"},
                created_at=now,
            )
            conn.execute("COMMIT")
            return ConfirmationMutation(dict(updated), True, role, second)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def cancel_order(
        self, *, order_id: int, actor_id: int, now: int
    ) -> Mapping[str, Any]:
        """Apply the side/state cancellation matrix in one writer transaction."""

        self._require_integer(order_id, "order ID")
        self._require_integer(actor_id, "actor ID")
        self._require_integer(now, "cancellation time")
        if order_id <= 0 or actor_id <= 0:
            raise ValueError("order and actor IDs must be positive")
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            if order is None:
                raise ValueError("order does not exist")
            if actor_id not in {order["buyer_id"], order["seller_id"]}:
                raise PermissionError("only an order participant may cancel")

            if order["state"] in {
                "cancelled",
                "deposit_expired",
                "recovery_hold",
                "refund_reserved",
                "refunded",
            }:
                conn.execute("COMMIT")
                return dict(order)
            if order["state"] in {
                "disputed",
                "release_reserved",
                "broadcast",
                "completed",
                "transfer_failed_safe",
                "transfer_uncertain",
            }:
                raise AccountingInvariantError(
                    "order state requires dispute or transfer reconciliation"
                )
            if order["buyer_confirmed"] or order["seller_confirmed"]:
                raise AccountingInvariantError(
                    "payment movement requires dispute resolution"
                )

            old_state = order["state"]
            if order["side"] == "sell" and old_state == "matched":
                if actor_id == order["buyer_id"]:
                    conn.execute(
                        """
                        UPDATE orders SET buyer_id=NULL,buyer_name=NULL,state='open',
                          matched_at=NULL,trade_deadline=NULL,updated_at=?
                        WHERE order_id=? AND side='sell' AND state='matched'
                          AND buyer_confirmed=0 AND seller_confirmed=0
                        """,
                        (now, order_id),
                    )
                    self._append_audit_conn(
                        conn,
                        order_id=order_id,
                        actor_id=actor_id,
                        event_type="buyer_left_order",
                        old_state="matched",
                        new_state="open",
                        detail={},
                        created_at=now,
                    )
                else:
                    self._queue_order_transfer_conn(
                        conn,
                        order=order,
                        kind="refund",
                        now=now,
                        actor_id=actor_id,
                        allow_release=False,
                        transition_order=True,
                    )
            elif order["side"] == "sell" and old_state == "open":
                if actor_id != order["seller_id"]:
                    raise PermissionError("only the seller may cancel an open sell")
                self._queue_order_transfer_conn(
                    conn,
                    order=order,
                    kind="refund",
                    now=now,
                    actor_id=actor_id,
                    allow_release=False,
                    transition_order=True,
                )
            elif order["side"] == "buy" and old_state == "open":
                if actor_id != order["buyer_id"]:
                    raise PermissionError("only the buyer may cancel an open buy")
                conn.execute(
                    "UPDATE orders SET state='cancelled',updated_at=? "
                    "WHERE order_id=? AND state='open'",
                    (now, order_id),
                )
                self._append_audit_conn(
                    conn,
                    order_id=order_id,
                    actor_id=actor_id,
                    event_type="order_cancelled",
                    old_state="open",
                    new_state="cancelled",
                    detail={},
                    created_at=now,
                )
            elif old_state == "awaiting_deposit":
                accounting = self._deposit_accounting_conn(
                    conn, order_id=order_id, network=self.network
                )
                credited = accounting["credited_units"]
                if credited == 0:
                    conn.execute(
                        "UPDATE orders SET state='cancelled',updated_at=? "
                        "WHERE order_id=? AND state='awaiting_deposit'",
                        (now, order_id),
                    )
                    self._append_audit_conn(
                        conn,
                        order_id=order_id,
                        actor_id=actor_id,
                        event_type="order_cancelled",
                        old_state="awaiting_deposit",
                        new_state="cancelled",
                        detail={},
                        created_at=now,
                    )
                elif credited < order["deposit_required_units"]:
                    self._queue_recovery_transfer_conn(
                        conn, order=order, now=now, actor_id=actor_id
                    )
                else:
                    self._advance_funded_order_conn(
                        conn,
                        order_id=order_id,
                        network=self.network,
                        now=now,
                    )
                    funded = conn.execute(
                        "SELECT * FROM orders WHERE order_id=?", (order_id,)
                    ).fetchone()
                    if funded["state"] not in {"open", "matched"}:
                        raise AccountingInvariantError(
                            "fully credited cancellation could not advance"
                        )
                    self._queue_order_transfer_conn(
                        conn,
                        order=funded,
                        kind="refund",
                        now=now,
                        actor_id=actor_id,
                        allow_release=False,
                        transition_order=True,
                    )
            elif order["side"] == "buy" and old_state == "matched":
                self._queue_order_transfer_conn(
                    conn,
                    order=order,
                    kind="refund",
                    now=now,
                    actor_id=actor_id,
                    allow_release=False,
                    transition_order=True,
                )
            else:
                raise AccountingInvariantError("order is not cancellable")

            updated = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            conn.execute("COMMIT")
            return dict(updated)
        except sqlite3.IntegrityError as exc:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise AccountingInvariantError(
                "cancellation violated durable order accounting"
            ) from exc
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def open_order_dispute(
        self, *, order_id: int, actor_id: int, reason: str, now: int
    ) -> Mapping[str, Any]:
        self._require_integer(order_id, "order ID")
        self._require_integer(actor_id, "actor ID")
        self._require_integer(now, "dispute time")
        if order_id <= 0 or actor_id <= 0:
            raise ValueError("order and actor IDs must be positive")
        reason = self._private_reason(reason)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            if order is None:
                raise ValueError("order does not exist")
            if actor_id not in {order["buyer_id"], order["seller_id"]}:
                raise PermissionError("only an order participant may dispute")
            if order["state"] == "disputed":
                conn.execute("COMMIT")
                return dict(order)
            if order["state"] != "matched":
                raise AccountingInvariantError("only a matched order may be disputed")
            cursor = conn.execute(
                """
                UPDATE orders SET state='disputed',disputed_at=COALESCE(disputed_at,?),
                  updated_at=? WHERE order_id=? AND state='matched'
                """,
                (now, now, order_id),
            )
            if cursor.rowcount != 1:
                raise AccountingInvariantError("dispute transition lost its state race")
            self._append_audit_conn(
                conn,
                order_id=order_id,
                actor_id=actor_id,
                event_type="dispute_opened",
                old_state="matched",
                new_state="disputed",
                detail={"reason": reason},
                created_at=now,
            )
            updated = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            conn.execute("COMMIT")
            return dict(updated)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def resolve_order_dispute(
        self,
        *,
        order_id: int,
        actor_id: int,
        winner: str,
        reason: str,
        now: int,
    ) -> Mapping[str, Any]:
        self._require_integer(order_id, "order ID")
        self._require_integer(actor_id, "actor ID")
        self._require_integer(now, "resolution time")
        if order_id <= 0 or actor_id <= 0:
            raise ValueError("order and actor IDs must be positive")
        if winner not in {"buyer", "seller"}:
            raise ValueError("winner must be buyer or seller")
        reason = self._private_reason(reason)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            if order is None:
                raise ValueError("order does not exist")
            existing = conn.execute(
                "SELECT * FROM transfers WHERE order_id=? AND is_main_outcome=1",
                (order_id,),
            ).fetchone()
            if existing is not None:
                conn.execute("COMMIT")
                return dict(order)
            if order["state"] != "disputed":
                raise AccountingInvariantError("admin resolution requires a dispute")
            kind = "resolve_buyer" if winner == "buyer" else "resolve_seller"
            transfer = self._queue_order_transfer_conn(
                conn,
                order=order,
                kind=kind,
                now=now,
                actor_id=actor_id,
                allow_release=False,
                transition_order=True,
            )
            if transfer is None:
                raise AccountingInvariantError("resolution was not durably queued")
            new_state = "release_reserved" if winner == "buyer" else "refund_reserved"
            self._append_audit_conn(
                conn,
                order_id=order_id,
                actor_id=actor_id,
                event_type="dispute_resolved",
                old_state="disputed",
                new_state=new_state,
                detail={"winner": winner, "reason": reason},
                created_at=now,
            )
            updated = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            conn.execute("COMMIT")
            return dict(updated)
        except sqlite3.IntegrityError as exc:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise AccountingInvariantError(
                "resolution violated durable order accounting"
            ) from exc
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def expire_matched_orders(self, *, now: int) -> tuple[int, ...]:
        self._require_integer(now, "expiry time")
        conn = self.connect()
        expired: list[int] = []
        try:
            conn.execute("BEGIN IMMEDIATE")
            rows = conn.execute(
                """
                SELECT order_id,trade_deadline FROM orders
                WHERE state='matched' AND trade_deadline IS NOT NULL
                  AND trade_deadline < ? ORDER BY order_id
                """,
                (now,),
            ).fetchall()
            for row in rows:
                cursor = conn.execute(
                    """
                    UPDATE orders SET state='disputed',
                      disputed_at=COALESCE(disputed_at,?),updated_at=?
                    WHERE order_id=? AND state='matched' AND trade_deadline < ?
                    """,
                    (now, now, row["order_id"], now),
                )
                if cursor.rowcount != 1:
                    continue
                expired.append(row["order_id"])
                self._append_audit_conn(
                    conn,
                    order_id=row["order_id"],
                    actor_id=None,
                    event_type="trade_timeout_disputed",
                    old_state="matched",
                    new_state="disputed",
                    detail={"trade_deadline": row["trade_deadline"]},
                    created_at=now,
                )
            conn.execute("COMMIT")
            return tuple(expired)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def queue_order_transfer(
        self,
        *,
        order_id: int,
        kind: str,
        now: int,
        actor_id: int | None = None,
    ) -> Mapping[str, Any] | None:
        self._require_integer(order_id, "order ID")
        self._require_integer(now, "queue time")
        self._require_optional_integer(actor_id, "actor ID")
        kind = self._machine_text(
            kind,
            "transfer kind",
            32,
            re.compile(r"(?:refund|resolve_buyer|resolve_seller|recovery_refund)\Z"),
        )
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            if order is None:
                raise ValueError("order does not exist")
            result = self._queue_order_transfer_conn(
                conn,
                order=order,
                kind=kind,
                now=now,
                actor_id=actor_id,
                allow_release=False,
                transition_order=True,
            )
            conn.execute("COMMIT")
            return result
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def claim_next_transfer(
        self,
        *,
        expected_tip_hash: str,
        expected_tip_height: int,
        wallet_spendable_units: int,
        now: int,
    ) -> ClaimedTransfer | None:
        expected_tip_hash = self._hash_text(expected_tip_hash, "expected tip hash")
        self._require_integer(expected_tip_height, "expected tip height")
        self._require_integer(wallet_spendable_units, "wallet spendable units")
        self._require_integer(now, "claim time")
        if (
            expected_tip_height < 0
            or wallet_spendable_units < 0
            or wallet_spendable_units > MAX_09C_UNITS
        ):
            raise ValueError("claim tip or spendable units are out of range")

        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            if self._health_failure is not None or self._health_issues_conn(
                conn, network=self.network
            ):
                conn.execute("COMMIT")
                return None
            try:
                self._require_common_tip_conn(
                    conn,
                    network=self.network,
                    expected_tip_hash=expected_tip_hash,
                    expected_tip_height=expected_tip_height,
                )
            except AccountingInvariantError:
                conn.execute("COMMIT")
                return None
            if (
                conn.execute(
                    "SELECT 1 FROM transfers WHERE state='uncertain' LIMIT 1"
                ).fetchone()
                is not None
            ):
                conn.execute("COMMIT")
                return None
            if (
                conn.execute(
                    """
                SELECT 1 FROM transfers
                WHERE state IN ('reserved','prepared','broadcast') LIMIT 1
                """
                ).fetchone()
                is not None
            ):
                conn.execute("COMMIT")
                return None
            restricted = self._restricted_outpoints_conn(conn, network=self.network)
            provisional = sum(item[2] for item in restricted)
            liability = self._customer_liability_conn(conn, network=self.network)
            pending = self._pending_platform_outflow_conn(conn)
            usable = wallet_spendable_units - provisional
            if usable < 0 or usable < liability + pending:
                conn.execute("COMMIT")
                return None
            queued = conn.execute(
                """
                SELECT * FROM transfers WHERE state='queued'
                ORDER BY created_at,transfer_id LIMIT 1
                """
            ).fetchone()
            if queued is None:
                conn.execute("COMMIT")
                return None
            try:
                cursor = conn.execute(
                    """
                    UPDATE transfers SET state='reserved',
                      attempt_count=attempt_count+1,reserved_at=?,updated_at=?
                    WHERE transfer_id=? AND state='queued'
                    """,
                    (now, now, queued["transfer_id"]),
                )
            except sqlite3.IntegrityError as exc:
                raise AccountingInvariantError(
                    "queued transfer is not claimable under its order evidence"
                ) from exc
            if cursor.rowcount != 1:
                conn.execute("ROLLBACK")
                return None
            claimed = conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?",
                (queued["transfer_id"],),
            ).fetchone()
            self._append_audit_conn(
                conn,
                order_id=claimed["order_id"],
                actor_id=None,
                event_type="transfer_claimed",
                old_state="queued",
                new_state="reserved",
                detail={"attempt_count": claimed["attempt_count"]},
                created_at=now,
            )
            conn.execute("COMMIT")
            return ClaimedTransfer(
                transfer_id=claimed["transfer_id"],
                operation_key=claimed["operation_key"],
                order_id=claimed["order_id"],
                kind=claimed["kind"],
                amount_units=claimed["amount_units"],
                network_fee_units=claimed["network_fee_units"],
                earned_fee_units=claimed["earned_fee_units"],
                destination=claimed["destination"],
                attempt_count=claimed["attempt_count"],
                reserved_at=claimed["reserved_at"],
                expected_tip_hash=expected_tip_hash,
                expected_tip_height=expected_tip_height,
                provisional_restricted_units=provisional,
                restricted_outpoints=restricted,
            )
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def attach_signed_transfer(
        self,
        *,
        transfer_id: int,
        expected_attempt_count: int,
        expected_reserved_at: int,
        txid: str,
        signed_tx_hex: str,
        destination: str,
        amount_units: int,
        network_fee_units: int,
        prepared_tip_hash: str,
        prepared_tip_height: int,
        live_tip_hash: str,
        live_tip_height: int,
        wallet_snapshot_hash: str,
        expected_wallet_snapshot_hash: str,
        now: int,
    ) -> AttachRecoveryResult:
        values = self._validate_attachment_values(
            transfer_id=transfer_id,
            expected_attempt_count=expected_attempt_count,
            expected_reserved_at=expected_reserved_at,
            txid=txid,
            signed_tx_hex=signed_tx_hex,
            destination=destination,
            amount_units=amount_units,
            network_fee_units=network_fee_units,
            prepared_tip_hash=prepared_tip_hash,
            prepared_tip_height=prepared_tip_height,
            live_tip_hash=live_tip_hash,
            live_tip_height=live_tip_height,
            wallet_snapshot_hash=wallet_snapshot_hash,
            expected_wallet_snapshot_hash=expected_wallet_snapshot_hash,
            now=now,
        )
        conn = self.connect()
        closed = False
        commit_attempted = False
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            if (
                conn.execute(
                    "SELECT 1 FROM transfers WHERE state='uncertain' LIMIT 1"
                ).fetchone()
                is not None
            ):
                raise AccountingInvariantError(
                    "uncertain transfer blocks signed attachment"
                )
            row = conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?", (transfer_id,)
            ).fetchone()
            if row is None:
                raise ValueError("transfer does not exist")
            if (
                row["state"] != "reserved"
                or row["attempt_count"] != expected_attempt_count
                or row["reserved_at"] != expected_reserved_at
            ):
                raise AccountingInvariantError(
                    "reservation attempt is stale or not reserved"
                )
            if any(
                row[field] is not None
                for field in (
                    "txid",
                    "signed_tx_hex",
                    "signed_at",
                    "prepared_tip_hash",
                    "prepared_tip_height",
                )
            ):
                raise AccountingInvariantError(
                    "reserved transfer has partial signed evidence"
                )
            if (
                row["destination"] != values["destination"]
                or row["amount_units"] != values["amount_units"]
                or row["network_fee_units"] != values["network_fee_units"]
            ):
                raise AccountingInvariantError(
                    "prepared transaction metadata differs from immutable transfer"
                )
            if self._common_tip_precondition_drift_conn(
                conn,
                network=self.network,
                expected_tip_hash=values["prepared_tip_hash"],
                expected_tip_height=values["prepared_tip_height"],
            ):
                conn.execute("COMMIT")
                return AttachRecoveryResult("safe_precondition_drift", dict(row))
            conn.execute(
                """
                UPDATE transfers SET state='prepared',txid=?,signed_tx_hex=?,
                  signed_at=?,prepared_tip_hash=?,prepared_tip_height=?,updated_at=?
                WHERE transfer_id=? AND state='reserved' AND attempt_count=?
                  AND reserved_at=?
                """,
                (
                    values["txid"],
                    values["signed_tx_hex"],
                    now,
                    values["prepared_tip_hash"],
                    values["prepared_tip_height"],
                    now,
                    transfer_id,
                    expected_attempt_count,
                    expected_reserved_at,
                ),
            )
            self._append_audit_conn(
                conn,
                order_id=row["order_id"],
                actor_id=None,
                event_type="transfer_prepared",
                old_state="reserved",
                new_state="prepared",
                detail={
                    "attempt_count": expected_attempt_count,
                    "txid": values["txid"],
                },
                created_at=now,
            )
            commit_attempted = True
            self._commit_attach(conn)
            prepared = conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?", (transfer_id,)
            ).fetchone()
            return AttachRecoveryResult("prepared", dict(prepared))
        except BaseException:
            if commit_attempted:
                conn.close()
                closed = True
                return self.recover_ambiguous_attach(
                    transfer_id=transfer_id,
                    expected_attempt_count=expected_attempt_count,
                    expected_reserved_at=expected_reserved_at,
                    txid=values["txid"],
                    signed_tx_hex=values["signed_tx_hex"],
                    prepared_tip_hash=values["prepared_tip_hash"],
                    prepared_tip_height=values["prepared_tip_height"],
                )
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            if not closed:
                conn.close()

    def recover_ambiguous_attach(
        self,
        *,
        transfer_id: int,
        expected_attempt_count: int,
        expected_reserved_at: int,
        txid: str,
        signed_tx_hex: str,
        prepared_tip_hash: str,
        prepared_tip_height: int,
    ) -> AttachRecoveryResult:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(expected_attempt_count, "expected attempt count")
        self._require_integer(expected_reserved_at, "expected reservation time")
        self._require_integer(prepared_tip_height, "prepared tip height")
        if (
            transfer_id <= 0
            or expected_attempt_count <= 0
            or expected_reserved_at <= 0
            or prepared_tip_height < 0
        ):
            raise ValueError("ambiguous attachment token is out of range")
        txid = self._hash_text(txid, "transaction ID")
        signed_tx_hex = self._signed_hex(signed_tx_hex)
        calculated = (
            hashlib.sha256(hashlib.sha256(bytes.fromhex(signed_tx_hex)).digest())
            .digest()
            .hex()
        )
        if calculated != txid:
            raise AccountingInvariantError(
                "signed bytes do not match the transaction ID"
            )
        prepared_tip_hash = self._hash_text(prepared_tip_hash, "prepared tip hash")
        try:
            conn = self.connect()
        except BaseException as exc:
            self._health_failure = "ambiguous attachment database reopen failed"
            raise AccountingInvariantError(self._health_failure) from exc
        try:
            conn.execute("BEGIN")
            row = conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?", (transfer_id,)
            ).fetchone()
            if row is None:
                self._health_failure = "ambiguous attachment transfer is missing"
                raise AccountingInvariantError(self._health_failure)
            if (
                row["attempt_count"] != expected_attempt_count
                or row["reserved_at"] != expected_reserved_at
            ):
                raise AccountingInvariantError("ambiguous recovery token is stale")
            if row["state"] in {
                "prepared",
                "broadcast",
                "confirmed",
                "uncertain",
            } and (
                row["txid"],
                row["signed_tx_hex"],
                row["prepared_tip_hash"],
                row["prepared_tip_height"],
            ) == (txid, signed_tx_hex, prepared_tip_hash, prepared_tip_height):
                conn.execute("COMMIT")
                return AttachRecoveryResult(row["state"], dict(row))
            signed_fields = (
                row["txid"],
                row["signed_tx_hex"],
                row["signed_at"],
                row["prepared_tip_hash"],
                row["prepared_tip_height"],
            )
            if row["state"] == "reserved" and all(
                value is None for value in signed_fields
            ):
                conn.execute("COMMIT")
                return AttachRecoveryResult("retry_reserved", dict(row))
            self._health_failure = (
                "ambiguous attachment evidence is partial or mismatched"
            )
            raise AccountingInvariantError(self._health_failure)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def mark_transfer_broadcast(
        self,
        *,
        transfer_id: int,
        observed_txid: str,
        observed_status: str,
        now: int,
        expected_tip_hash: str,
        expected_tip_height: int,
    ) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(now, "broadcast observation time")
        observed_txid = self._hash_text(observed_txid, "observed transaction ID")
        if observed_status not in ("mempool", "confirmed"):
            raise ValueError("trusted observation must be mempool or confirmed")
        expected_tip = self._expected_tip(expected_tip_hash, expected_tip_height)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._require_observation_tip_conn(conn, expected_tip)
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if row["txid"] != observed_txid:
                raise AccountingInvariantError(
                    "trusted node observed a different transaction"
                )
            if row["state"] == "broadcast":
                conn.execute("COMMIT")
                return dict(row)
            if row["state"] not in ("prepared", "uncertain"):
                raise AccountingInvariantError(
                    "transfer is not ready for broadcast observation"
                )
            if (
                row["state"] == "uncertain"
                and conn.execute(
                    """
                SELECT 1 FROM transfers
                WHERE state='uncertain' AND transfer_id!=? LIMIT 1
                """,
                    (transfer_id,),
                ).fetchone()
                is not None
            ):
                raise AccountingInvariantError(
                    "another uncertain transfer must be reconciled first"
                )
            conn.execute(
                """
                UPDATE transfers SET state='broadcast',
                  broadcast_at=COALESCE(broadcast_at,?),result_class='broadcast',
                  error_text=NULL,updated_at=? WHERE transfer_id=?
                """,
                (now, now, transfer_id),
            )
            self._set_order_transfer_state_conn(
                conn, transfer=row, new_transfer_state="broadcast", now=now
            )
            self._append_transfer_audit_conn(
                conn,
                transfer=row,
                event_type="transfer_broadcast",
                old_state=row["state"],
                new_state="broadcast",
                now=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return dict(updated)
        except sqlite3.IntegrityError as exc:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise AccountingInvariantError(
                "broadcast transition violated the wallet lane"
            ) from exc
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def broadcast_prepared_with_authorization(
        self,
        *,
        transfer_id: int,
        expected_txid: str,
        invoke: Callable[[Mapping[str, Any]], tuple[str, str]],
        now: int,
        expected_tip_hash: str,
        expected_tip_height: int,
    ) -> BroadcastAuthorizationResult:
        """Serialize uncertainty writes and the exact prepared wallet call.

        The callback runs while this connection owns the SQLite writer lock.
        Therefore an uncertainty transition either commits before this method
        and suppresses the callback, or waits until this exact prepared
        transaction has finished its authorized call and durable transition.
        """

        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(now, "broadcast authorization time")
        if transfer_id <= 0 or now <= 0 or not callable(invoke):
            raise ValueError("broadcast authorization input is invalid")
        expected_txid = self._hash_text(expected_txid, "expected transaction ID")
        expected_tip = self._expected_tip(expected_tip_hash, expected_tip_height)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._require_observation_tip_conn(conn, expected_tip)
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if row["txid"] != expected_txid:
                raise AccountingInvariantError(
                    "broadcast authorization identifies another transaction"
                )
            if row["state"] == "broadcast":
                conn.execute("COMMIT")
                return BroadcastAuthorizationResult(False, dict(row))
            if row["state"] != "prepared":
                raise AccountingInvariantError(
                    "transfer is not prepared for broadcast authorization"
                )
            if (
                conn.execute(
                    "SELECT 1 FROM transfers WHERE state='uncertain' LIMIT 1"
                ).fetchone()
                is not None
            ):
                conn.execute("COMMIT")
                return BroadcastAuthorizationResult(False, dict(row))

            observation = invoke(dict(row))
            if (
                not isinstance(observation, tuple)
                or len(observation) != 2
                or observation[1] not in ("mempool", "confirmed")
            ):
                raise AccountingInvariantError(
                    "wallet returned invalid broadcast observation"
                )
            observed_txid = self._hash_text(observation[0], "observed transaction ID")
            if observed_txid != expected_txid:
                raise AccountingInvariantError(
                    "wallet observed another broadcast transaction"
                )
            conn.execute(
                """
                UPDATE transfers SET state='broadcast',
                  broadcast_at=COALESCE(broadcast_at,?),result_class='broadcast',
                  error_text=NULL,updated_at=?
                WHERE transfer_id=? AND state='prepared' AND txid=?
                """,
                (now, now, transfer_id, expected_txid),
            )
            self._set_order_transfer_state_conn(
                conn, transfer=row, new_transfer_state="broadcast", now=now
            )
            self._append_transfer_audit_conn(
                conn,
                transfer=row,
                event_type="transfer_broadcast",
                old_state="prepared",
                new_state="broadcast",
                now=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return BroadcastAuthorizationResult(True, dict(updated))
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def mark_transfer_failed_safe(
        self,
        *,
        transfer_id: int,
        expected_attempt_count: int,
        expected_reserved_at: int,
        error_text: str,
        now: int,
    ) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(expected_attempt_count, "expected attempt count")
        self._require_integer(expected_reserved_at, "expected reservation time")
        self._require_integer(now, "failure time")
        if expected_attempt_count <= 0 or expected_reserved_at <= 0:
            raise ValueError("failure reservation token must be positive")
        error_text = self._bounded_error(error_text)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if row["state"] == "failed_safe" and (
                row["attempt_count"],
                row["reserved_at"],
            ) == (expected_attempt_count, expected_reserved_at):
                conn.execute("COMMIT")
                return dict(row)
            if (
                row["state"] != "reserved"
                or row["attempt_count"] != expected_attempt_count
                or row["reserved_at"] != expected_reserved_at
                or any(
                    row[field] is not None
                    for field in ("txid", "signed_tx_hex", "signed_at")
                )
            ):
                raise AccountingInvariantError(
                    "only an unsigned reserved transfer can fail safely"
                )
            conn.execute(
                """
                UPDATE transfers SET state='failed_safe',result_class='safe_to_retry',
                  error_text=?,updated_at=? WHERE transfer_id=? AND state='reserved'
                  AND attempt_count=? AND reserved_at=?
                """,
                (
                    error_text,
                    now,
                    transfer_id,
                    expected_attempt_count,
                    expected_reserved_at,
                ),
            )
            self._set_order_transfer_state_conn(
                conn, transfer=row, new_transfer_state="failed_safe", now=now
            )
            self._append_transfer_audit_conn(
                conn,
                transfer=row,
                event_type="transfer_failed_safe",
                old_state="reserved",
                new_state="failed_safe",
                now=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return dict(updated)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def requeue_failed_safe(self, *, transfer_id: int, now: int) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(now, "requeue time")
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if row["state"] == "queued":
                conn.execute("COMMIT")
                return dict(row)
            if row["state"] != "failed_safe" or any(
                row[field] is not None
                for field in ("txid", "signed_tx_hex", "signed_at")
            ):
                raise AccountingInvariantError(
                    "transfer is not a safe failed operation"
                )
            conn.execute(
                """
                UPDATE transfers SET state='queued',reserved_at=NULL,
                  result_class=NULL,error_text=NULL,updated_at=? WHERE transfer_id=?
                """,
                (now, transfer_id),
            )
            self._set_order_transfer_state_conn(
                conn, transfer=row, new_transfer_state="queued", now=now
            )
            self._append_transfer_audit_conn(
                conn,
                transfer=row,
                event_type="transfer_requeued",
                old_state="failed_safe",
                new_state="queued",
                now=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return dict(updated)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def mark_transfer_uncertain(
        self,
        *,
        transfer_id: int,
        expected_state: str,
        expected_txid: str,
        error_text: str,
        now: int,
        expected_tip_hash: str,
        expected_tip_height: int,
    ) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(now, "uncertainty time")
        if expected_state not in ("prepared", "broadcast", "confirmed"):
            raise ValueError("expected uncertainty source state is invalid")
        expected_txid = self._hash_text(expected_txid, "expected transaction ID")
        error_text = self._bounded_error(error_text)
        expected_tip = self._expected_tip(expected_tip_hash, expected_tip_height)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._require_observation_tip_conn(conn, expected_tip)
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if row["state"] == "uncertain":
                if (
                    row["txid"] == expected_txid
                    and row["error_text"] == error_text
                    and row["result_class"] == "uncertain"
                ):
                    conn.execute("COMMIT")
                    return dict(row)
                raise AccountingInvariantError(
                    "uncertainty callback differs from durable evidence"
                )
            if row["state"] != expected_state or row["txid"] != expected_txid:
                raise AccountingInvariantError(
                    "uncertainty callback is stale or identifies another transaction"
                )
            conn.execute(
                """
                UPDATE transfers SET state='uncertain',result_class='uncertain',
                  error_text=?,updated_at=? WHERE transfer_id=? AND state=? AND txid=?
                """,
                (error_text, now, transfer_id, expected_state, expected_txid),
            )
            self._set_order_transfer_state_conn(
                conn, transfer=row, new_transfer_state="uncertain", now=now
            )
            self._append_transfer_audit_conn(
                conn,
                transfer=row,
                event_type="transfer_uncertain",
                old_state=row["state"],
                new_state="uncertain",
                now=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            # Adverse evidence is committed even if it makes earned revenue
            # negative.  Subsequent intake/claims derive and enforce the halt.
            self._uncertainty_checkpoint("before_commit")
            conn.execute("COMMIT")
            return dict(updated)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def _uncertainty_checkpoint(self, phase: str) -> None:
        """Test seam for ordering uncertainty against broadcast authorization."""

    def mark_transfer_confirmed(
        self,
        *,
        transfer_id: int,
        observed_txid: str,
        confirmed_block_hash: str,
        confirmed_block_height: int,
        confirmations: int,
        now: int,
        expected_tip_hash: str,
        expected_tip_height: int,
    ) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(confirmed_block_height, "confirmed block height")
        self._require_integer(confirmations, "confirmations")
        self._require_integer(now, "confirmation time")
        if confirmed_block_height < 0 or confirmations < 1:
            raise ValueError("confirmation evidence is invalid")
        observed_txid = self._hash_text(observed_txid, "observed transaction ID")
        confirmed_block_hash = self._hash_text(
            confirmed_block_hash, "confirmed block hash"
        )
        expected_tip = self._expected_tip(expected_tip_hash, expected_tip_height)
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._require_observation_tip_conn(conn, expected_tip)
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if row["txid"] != observed_txid:
                raise AccountingInvariantError(
                    "confirmation identifies another transaction"
                )
            if row["state"] == "confirmed":
                if (
                    row["confirmed_block_hash"] != confirmed_block_hash
                    or row["confirmed_block_height"] != confirmed_block_height
                    or confirmations < row["confirmations"]
                ):
                    raise AccountingInvariantError("confirmed anchor is immutable")
                if confirmations > row["confirmations"]:
                    conn.execute(
                        "UPDATE transfers SET confirmations=?,updated_at=? WHERE transfer_id=?",
                        (confirmations, now, transfer_id),
                    )
                    row = self._transfer_row_conn(conn, transfer_id=transfer_id)
                conn.execute("COMMIT")
                return dict(row)
            if row["state"] not in ("prepared", "broadcast", "uncertain"):
                raise AccountingInvariantError(
                    "transfer is not reconcilable as confirmed"
                )
            conn.execute(
                """
                UPDATE transfers SET state='confirmed',confirmed_at=?,
                  confirmed_block_hash=?,confirmed_block_height=?,confirmations=?,
                  result_class='broadcast',error_text=NULL,updated_at=?
                WHERE transfer_id=?
                """,
                (
                    now,
                    confirmed_block_hash,
                    confirmed_block_height,
                    confirmations,
                    now,
                    transfer_id,
                ),
            )
            self._set_order_transfer_state_conn(
                conn, transfer=row, new_transfer_state="confirmed", now=now
            )
            self._append_transfer_audit_conn(
                conn,
                transfer=row,
                event_type="transfer_confirmed",
                old_state=row["state"],
                new_state="confirmed",
                now=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return dict(updated)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def earned_fee_units(self) -> int:
        conn = self.connect()
        try:
            value = conn.execute(
                """
                SELECT COALESCE(SUM(earned_fee_units),0) FROM transfers
                WHERE state='confirmed' AND kind IN ('release','resolve_buyer')
                """
            ).fetchone()[0]
            return self._nonnegative_aggregate(value, "earned fee units")
        finally:
            conn.close()

    def available_fee_units(self) -> int:
        conn = self.connect()
        try:
            value = self._available_fee_conn(conn)
            if value < 0:
                raise AccountingInvariantError("available fee revenue is negative")
            return value
        finally:
            conn.close()

    def queue_fee_withdrawal(
        self,
        *,
        operation_key: str,
        amount_units: int,
        network_fee_units: int,
        destination: str,
        configured_admin_destination: str,
        now: int,
        actor_id: int | None = None,
    ) -> Mapping[str, Any] | None:
        operation_key = self._machine_text(
            operation_key,
            "fee withdrawal operation key",
            160,
            re.compile(r"[a-z0-9:_-]+\Z"),
        )
        self._require_integer(amount_units, "fee withdrawal amount")
        self._require_integer(network_fee_units, "fee withdrawal network fee")
        self._require_integer(now, "fee withdrawal queue time")
        self._require_optional_integer(actor_id, "actor ID")
        if (
            amount_units <= 0
            or network_fee_units < 0
            or amount_units > MAX_09C_UNITS
            or network_fee_units > MAX_09C_UNITS
            or amount_units + network_fee_units > MAX_09C_UNITS
        ):
            raise ValueError("fee withdrawal amounts are out of range")
        destination = self._bounded_text(destination, "fee destination", 128)
        configured_admin_destination = self._bounded_text(
            configured_admin_destination, "configured admin destination", 128
        )
        if destination != configured_admin_destination:
            raise AccountingInvariantError(
                "fee withdrawal destination differs from configured admin destination"
            )
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._raise_if_unhealthy_conn(conn)
            existing = conn.execute(
                "SELECT * FROM transfers WHERE operation_key=?", (operation_key,)
            ).fetchone()
            if existing is not None:
                if (
                    existing["kind"] != "fee_withdrawal"
                    or existing["amount_units"] != amount_units
                    or existing["network_fee_units"] != network_fee_units
                    or existing["destination"] != destination
                ):
                    raise AccountingInvariantError(
                        "fee operation key is bound to different economics"
                    )
                conn.execute("COMMIT")
                return dict(existing)
            self._assert_safe_destination_conn(conn, destination=destination)
            available = self._available_fee_conn(conn)
            if available < 0:
                raise AccountingInvariantError("available fee revenue is negative")
            if amount_units + network_fee_units > available:
                conn.execute("COMMIT")
                return None
            cursor = conn.execute(
                """
                INSERT INTO transfers(
                  operation_key,kind,is_main_outcome,state,amount_units,
                  network_fee_units,earned_fee_units,destination,created_at,updated_at
                ) VALUES(?,'fee_withdrawal',0,'queued',?,?,0,?,?,?)
                """,
                (
                    operation_key,
                    amount_units,
                    network_fee_units,
                    destination,
                    now,
                    now,
                ),
            )
            if cursor.lastrowid is None:
                raise AccountingInvariantError("fee withdrawal ID was not persisted")
            transfer_id = int(cursor.lastrowid)
            self._append_audit_conn(
                conn,
                order_id=None,
                actor_id=actor_id,
                event_type="fee_withdrawal_queued",
                old_state=None,
                new_state="queued",
                detail={
                    "operation_key": operation_key,
                    "transfer_id": transfer_id,
                },
                created_at=now,
            )
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return dict(row)
        except sqlite3.IntegrityError as exc:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise AccountingInvariantError(
                "fee withdrawal violates durable earned-revenue accounting"
            ) from exc
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def cancel_failed_safe_fee_withdrawal(
        self, *, transfer_id: int, actor_id: int, now: int
    ) -> Mapping[str, Any]:
        self._require_integer(transfer_id, "transfer ID")
        self._require_integer(actor_id, "actor ID")
        self._require_integer(now, "fee cancellation time")
        if transfer_id <= 0 or actor_id <= 0:
            raise ValueError("transfer and actor IDs must be positive")
        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            row = self._transfer_row_conn(conn, transfer_id=transfer_id)
            if (
                row["kind"] != "fee_withdrawal"
                or row["state"] != "failed_safe"
                or row["txid"] is not None
                or row["signed_tx_hex"] is not None
            ):
                raise AccountingInvariantError(
                    "only an unsigned failed-safe fee withdrawal may be cancelled"
                )
            conn.execute(
                "UPDATE transfers SET state='cancelled',updated_at=? WHERE transfer_id=?",
                (now, transfer_id),
            )
            self._append_audit_conn(
                conn,
                order_id=None,
                actor_id=actor_id,
                event_type="fee_withdrawal_cancelled",
                old_state="failed_safe",
                new_state="cancelled",
                detail={
                    "operation_key": row["operation_key"],
                    "transfer_id": transfer_id,
                },
                created_at=now,
            )
            updated = self._transfer_row_conn(conn, transfer_id=transfer_id)
            conn.execute("COMMIT")
            return dict(updated)
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def _validate_attachment_values(
        self,
        *,
        transfer_id: int,
        expected_attempt_count: int,
        expected_reserved_at: int,
        txid: str,
        signed_tx_hex: str,
        destination: str,
        amount_units: int,
        network_fee_units: int,
        prepared_tip_hash: str,
        prepared_tip_height: int,
        live_tip_hash: str,
        live_tip_height: int,
        wallet_snapshot_hash: str,
        expected_wallet_snapshot_hash: str,
        now: int,
    ) -> dict[str, Any]:
        for value, label in (
            (transfer_id, "transfer ID"),
            (expected_attempt_count, "expected attempt count"),
            (expected_reserved_at, "expected reservation time"),
            (amount_units, "prepared amount units"),
            (network_fee_units, "prepared fee units"),
            (prepared_tip_height, "prepared tip height"),
            (live_tip_height, "live tip height"),
            (now, "attachment time"),
        ):
            self._require_integer(value, label)
        if (
            transfer_id <= 0
            or expected_attempt_count <= 0
            or amount_units <= 0
            or network_fee_units < 0
            or prepared_tip_height < 0
            or live_tip_height < 0
        ):
            raise ValueError("prepared transfer integers are out of range")
        txid = self._hash_text(txid, "transaction ID")
        signed_tx_hex = self._signed_hex(signed_tx_hex)
        calculated = (
            hashlib.sha256(hashlib.sha256(bytes.fromhex(signed_tx_hex)).digest())
            .digest()
            .hex()
        )
        if calculated != txid:
            raise AccountingInvariantError(
                "signed bytes do not match the transaction ID"
            )
        destination = self._bounded_text(destination, "prepared destination", 128)
        prepared_tip_hash = self._hash_text(prepared_tip_hash, "prepared tip hash")
        live_tip_hash = self._hash_text(live_tip_hash, "live tip hash")
        if (prepared_tip_hash, prepared_tip_height) != (
            live_tip_hash,
            live_tip_height,
        ):
            raise AccountingInvariantError("live tip changed before signed attachment")
        wallet_snapshot_hash = self._hash_text(
            wallet_snapshot_hash, "wallet snapshot hash"
        )
        expected_wallet_snapshot_hash = self._hash_text(
            expected_wallet_snapshot_hash, "expected wallet snapshot hash"
        )
        if wallet_snapshot_hash != expected_wallet_snapshot_hash:
            raise AccountingInvariantError("wallet snapshot changed before signing")
        return {
            "txid": txid,
            "signed_tx_hex": signed_tx_hex,
            "destination": destination,
            "amount_units": amount_units,
            "network_fee_units": network_fee_units,
            "prepared_tip_hash": prepared_tip_hash,
            "prepared_tip_height": prepared_tip_height,
        }

    def _commit_attach(self, conn: sqlite3.Connection) -> None:
        if self._attach_commit_boundary is None:
            conn.execute("COMMIT")
        else:
            self._attach_commit_boundary(conn)

    @classmethod
    def _signed_hex(cls, value: object) -> str:
        value = cls._machine_text(
            value,
            "signed transaction hex",
            20_000,
            re.compile(r"[0-9a-f]+\Z"),
        )
        if len(value) < 2 or len(value) % 2:
            raise ValueError("signed transaction hex must contain complete bytes")
        return value

    @classmethod
    def _bounded_error(cls, value: object) -> str:
        value = cls._bounded_text(value, "transfer error", 500)
        return value

    @staticmethod
    def _transfer_row_conn(
        conn: sqlite3.Connection, *, transfer_id: int
    ) -> sqlite3.Row:
        row = conn.execute(
            "SELECT * FROM transfers WHERE transfer_id=?", (transfer_id,)
        ).fetchone()
        if row is None:
            raise ValueError("transfer does not exist")
        return row

    def _append_transfer_audit_conn(
        self,
        conn: sqlite3.Connection,
        *,
        transfer: sqlite3.Row,
        event_type: str,
        old_state: str,
        new_state: str,
        now: int,
    ) -> None:
        self._append_audit_conn(
            conn,
            order_id=transfer["order_id"],
            actor_id=None,
            event_type=event_type,
            old_state=old_state,
            new_state=new_state,
            detail={
                "operation_key": transfer["operation_key"],
                "transfer_id": transfer["transfer_id"],
            },
            created_at=now,
        )

    @staticmethod
    def _set_order_transfer_state_conn(
        conn: sqlite3.Connection,
        *,
        transfer: sqlite3.Row,
        new_transfer_state: str,
        now: int,
    ) -> None:
        if transfer["order_id"] is None:
            return
        order = conn.execute(
            "SELECT * FROM orders WHERE order_id=?", (transfer["order_id"],)
        ).fetchone()
        if transfer["kind"] == "recovery_refund":
            cancelled_partial = conn.execute(
                """
                SELECT 1
                FROM transfer_credit_allocations a
                JOIN deposit_credits c ON c.credit_id=a.credit_id
                WHERE a.transfer_id=? AND c.recovery_reason='cancelled_partial'
                LIMIT 1
                """,
                (transfer["transfer_id"],),
            ).fetchone()
            if cancelled_partial is None:
                return
        if new_transfer_state == "broadcast":
            target = "broadcast"
        elif new_transfer_state == "failed_safe":
            target = "transfer_failed_safe"
        elif new_transfer_state == "uncertain":
            target = "transfer_uncertain"
        elif new_transfer_state == "queued":
            target = (
                "release_reserved"
                if transfer["kind"] in ("release", "resolve_buyer")
                else "refund_reserved"
            )
        elif new_transfer_state == "confirmed":
            target = (
                "completed"
                if transfer["kind"] in ("release", "resolve_buyer")
                else "refunded"
            )
        else:
            return
        if order["state"] != target:
            conn.execute(
                "UPDATE orders SET state=?,completed_at=CASE WHEN ? IN "
                "('completed','refunded') THEN COALESCE(completed_at,?) ELSE completed_at END,"
                "updated_at=? WHERE order_id=?",
                (target, target, now, now, order["order_id"]),
            )

    def _queue_order_transfer_conn(
        self,
        conn: sqlite3.Connection,
        *,
        order: sqlite3.Row,
        kind: str,
        now: int,
        actor_id: int | None,
        allow_release: bool,
        transition_order: bool,
    ) -> Mapping[str, Any] | None:
        if kind == "recovery_refund":
            return self._queue_recovery_transfer_conn(
                conn, order=order, now=now, actor_id=actor_id
            )
        operation_key = f"order:{order['order_id']}:main"
        existing = conn.execute(
            "SELECT * FROM transfers WHERE operation_key=?", (operation_key,)
        ).fetchone()
        if existing is not None:
            if existing["kind"] != kind:
                raise AccountingInvariantError(
                    "main outcome already has a different kind"
                )
            return dict(existing)
        if kind == "release" and not allow_release:
            raise ValueError("release is queued atomically by record_confirmation")
        if kind in ("release", "resolve_buyer"):
            user_id = order["buyer_id"]
            amount = order["net_amount_units"]
            earned = order["service_fee_units"]
            target_state = "release_reserved"
        else:
            user_id = order["seller_id"]
            amount = order["net_amount_units"] + order["service_fee_units"]
            earned = 0
            target_state = "refund_reserved"
        if user_id is None:
            raise AccountingInvariantError("transfer participant is missing")
        user = conn.execute(
            "SELECT wallet_addr FROM users WHERE user_id=?", (user_id,)
        ).fetchone()
        if user is None or user["wallet_addr"] is None:
            raise AccountingInvariantError(
                "transfer participant has no validated wallet"
            )
        destination = self._bounded_text(
            user["wallet_addr"], "transfer destination", 128
        )
        self._assert_safe_destination_conn(conn, destination=destination)
        if kind == "release":
            if order["state"] != "matched" or (
                order["buyer_confirmed"] + order["seller_confirmed"] != 1
            ):
                raise AccountingInvariantError(
                    "release is not authorized by confirmations"
                )
        elif kind == "refund":
            if order["state"] not in ("open", "matched") or (
                order["buyer_confirmed"] or order["seller_confirmed"]
            ):
                raise AccountingInvariantError(
                    "refund is not authorized by order state"
                )
        elif kind == "resolve_buyer":
            if order["state"] != "disputed":
                raise AccountingInvariantError("buyer resolution requires a dispute")
        elif kind == "resolve_seller" and order["state"] != "disputed":
            raise AccountingInvariantError("seller resolution requires a dispute")

        cursor = conn.execute(
            """
            INSERT INTO transfers(
              operation_key,order_id,kind,is_main_outcome,state,amount_units,
              network_fee_units,earned_fee_units,destination,created_at,updated_at
            ) VALUES(?,?,?,1,'queued',?,?,?,?,?,?)
            """,
            (
                operation_key,
                order["order_id"],
                kind,
                amount,
                order["network_fee_units"],
                earned,
                destination,
                now,
                now,
            ),
        )
        if cursor.lastrowid is None:
            raise AccountingInvariantError("transfer ID was not persisted")
        transfer_id = int(cursor.lastrowid)
        required = amount + order["network_fee_units"] + earned
        self._allocate_credit_bucket_conn(
            conn,
            transfer_id=transfer_id,
            order_id=order["order_id"],
            bucket="main",
            required_units=required,
        )
        if transition_order:
            conn.execute(
                "UPDATE orders SET state=?,updated_at=? WHERE order_id=?",
                (target_state, now, order["order_id"]),
            )
        self._append_audit_conn(
            conn,
            order_id=order["order_id"],
            actor_id=actor_id,
            event_type="transfer_queued",
            old_state=order["state"],
            new_state=target_state,
            detail={"kind": kind, "operation_key": operation_key},
            created_at=now,
        )
        return dict(
            conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?", (transfer_id,)
            ).fetchone()
        )

    def _allocate_credit_bucket_conn(
        self,
        conn: sqlite3.Connection,
        *,
        transfer_id: int,
        order_id: int,
        bucket: str,
        required_units: int,
    ) -> None:
        remaining = required_units
        column = "main_units" if bucket == "main" else "recovery_units"
        for credit in conn.execute(
            f"""
            SELECT c.credit_id,c.{column} AS capacity,
              COALESCE((SELECT SUM(a.units) FROM transfer_credit_allocations a
                        WHERE a.credit_id=c.credit_id AND a.bucket=?),0) AS allocated
            FROM deposit_credits c
            WHERE c.order_id=? AND c.network=? AND c.credited_at IS NOT NULL
              AND c.{column}>0 ORDER BY c.credit_id
            """,
            (bucket, order_id, self.network),
        ).fetchall():
            capacity = credit["capacity"] - credit["allocated"]
            if capacity < 0:
                raise AccountingInvariantError("credit bucket is overallocated")
            units = min(remaining, capacity)
            if units > 0:
                conn.execute(
                    """
                    INSERT INTO transfer_credit_allocations(
                      transfer_id,credit_id,order_id,bucket,units
                    ) VALUES(?,?,?,?,?)
                    """,
                    (transfer_id, credit["credit_id"], order_id, bucket, units),
                )
                remaining -= units
            if remaining == 0:
                break
        if remaining != 0:
            raise AccountingInvariantError(
                "credited bucket cannot fund immutable transfer"
            )

    def _queue_recovery_transfer_conn(
        self,
        conn: sqlite3.Connection,
        *,
        order: sqlite3.Row,
        now: int,
        actor_id: int | None,
    ) -> Mapping[str, Any] | None:
        transitioned_to_hold = False
        awaiting_recovery_only = False
        if order["state"] == "awaiting_deposit":
            accounting = conn.execute(
                """
                SELECT COALESCE(SUM(amount_units),0) AS credited_units,
                  COALESCE(SUM(main_units),0) AS main_units,
                  COALESCE(SUM(recovery_units),0) AS recovery_units
                FROM deposit_credits
                WHERE order_id=? AND network=? AND credited_at IS NOT NULL
                """,
                (order["order_id"], self.network),
            ).fetchone()
            credited = self._nonnegative_aggregate(
                accounting["credited_units"], "recovery credited units"
            )
            main = self._nonnegative_aggregate(
                accounting["main_units"], "recovery main units"
            )
            recovery = self._nonnegative_aggregate(
                accounting["recovery_units"], "recovery bucket units"
            )
            awaiting_recovery_only = bool(
                credited >= order["deposit_required_units"]
                and main == 0
                and recovery == credited
            )
            if not awaiting_recovery_only and not (
                1 <= credited < order["deposit_required_units"]
            ):
                raise AccountingInvariantError(
                    "partial cancellation requires a credited underpayment"
                )
            if not awaiting_recovery_only:
                conn.execute(
                    "UPDATE orders SET state='recovery_hold',updated_at=? "
                    "WHERE order_id=?",
                    (now, order["order_id"]),
                )
                for credit in conn.execute(
                    """
                    SELECT credit_id,main_units,recovery_units FROM deposit_credits
                    WHERE order_id=? AND network=? AND credited_at IS NOT NULL
                      AND main_units>0 ORDER BY credit_id
                    """,
                    (order["order_id"], self.network),
                ).fetchall():
                    conn.execute(
                        """
                        UPDATE deposit_credits SET main_units=0,
                          recovery_units=?,recovery_reason='cancelled_partial'
                        WHERE credit_id=?
                        """,
                        (
                            credit["main_units"] + credit["recovery_units"],
                            credit["credit_id"],
                        ),
                    )
                transitioned_to_hold = True
                order = conn.execute(
                    "SELECT * FROM orders WHERE order_id=?", (order["order_id"],)
                ).fetchone()

        terminal_states = {"completed", "refunded", "cancelled", "deposit_expired"}
        if (
            order["state"] not in terminal_states | {"recovery_hold"}
            and not awaiting_recovery_only
        ):
            raise AccountingInvariantError(
                "recovery refund requires a partial hold or terminal main order"
            )
        unfinished = conn.execute(
            """
            SELECT * FROM transfers WHERE order_id=? AND kind='recovery_refund'
              AND state NOT IN ('confirmed','cancelled')
            ORDER BY transfer_id LIMIT 1
            """,
            (order["order_id"],),
        ).fetchone()
        if unfinished is not None:
            return dict(unfinished)

        residual = conn.execute(
            """
            SELECT
              COALESCE((SELECT SUM(recovery_units) FROM deposit_credits
                        WHERE order_id=? AND network=?
                          AND credited_at IS NOT NULL),0)
              - COALESCE((SELECT SUM(a.units)
                          FROM transfer_credit_allocations a
                          JOIN deposit_credits c ON c.credit_id=a.credit_id
                          WHERE a.order_id=? AND c.network=?
                            AND a.bucket='recovery'),0)
            """,
            (order["order_id"], self.network, order["order_id"], self.network),
        ).fetchone()[0]
        if type(residual) is not int or residual < 0:
            raise AccountingInvariantError("recovery capacity is negative or malformed")
        if residual == 0:
            if transitioned_to_hold:
                raise AccountingInvariantError(
                    "recovery hold has no residual liability"
                )
            return None
        if residual <= order["network_fee_units"]:
            if transitioned_to_hold:
                self._append_audit_conn(
                    conn,
                    order_id=order["order_id"],
                    actor_id=actor_id,
                    event_type="recovery_hold",
                    old_state="awaiting_deposit",
                    new_state="recovery_hold",
                    detail={
                        "residual_units": residual,
                        "network_fee_units": order["network_fee_units"],
                    },
                    created_at=now,
                )
            return None
        max_credit_id = conn.execute(
            """
            SELECT MAX(c.credit_id)
            FROM deposit_credits c
            WHERE c.order_id=? AND c.network=? AND c.credited_at IS NOT NULL
              AND c.recovery_units > COALESCE((
                SELECT SUM(a.units) FROM transfer_credit_allocations a
                WHERE a.credit_id=c.credit_id AND a.bucket='recovery'
              ),0)
            """,
            (order["order_id"], self.network),
        ).fetchone()[0]
        if type(max_credit_id) is not int:
            raise AccountingInvariantError("recovery operation lacks a credit identity")
        seller = conn.execute(
            "SELECT wallet_addr FROM users WHERE user_id=?", (order["seller_id"],)
        ).fetchone()
        if seller is None or seller["wallet_addr"] is None:
            raise AccountingInvariantError("recovery depositor has no validated wallet")
        destination = self._bounded_text(
            seller["wallet_addr"], "recovery destination", 128
        )
        self._assert_safe_destination_conn(conn, destination=destination)
        operation_key = f"order:{order['order_id']}:recovery:{max_credit_id}"
        existing = conn.execute(
            "SELECT * FROM transfers WHERE operation_key=?", (operation_key,)
        ).fetchone()
        if existing is not None:
            return dict(existing)
        cursor = conn.execute(
            """
            INSERT INTO transfers(
              operation_key,order_id,kind,is_main_outcome,state,amount_units,
              network_fee_units,earned_fee_units,destination,created_at,updated_at
            ) VALUES(?,?,'recovery_refund',0,'queued',?,?,0,?,?,?)
            """,
            (
                operation_key,
                order["order_id"],
                residual - order["network_fee_units"],
                order["network_fee_units"],
                destination,
                now,
                now,
            ),
        )
        if cursor.lastrowid is None:
            raise AccountingInvariantError("recovery transfer ID was not persisted")
        transfer_id = int(cursor.lastrowid)
        self._allocate_credit_bucket_conn(
            conn,
            transfer_id=transfer_id,
            order_id=order["order_id"],
            bucket="recovery",
            required_units=residual,
        )
        new_state = order["state"]
        if order["state"] in {"awaiting_deposit", "recovery_hold"}:
            conn.execute(
                "UPDATE orders SET state='refund_reserved',updated_at=? WHERE order_id=?",
                (now, order["order_id"]),
            )
            new_state = "refund_reserved"
        self._append_audit_conn(
            conn,
            order_id=order["order_id"],
            actor_id=actor_id,
            event_type="transfer_queued",
            old_state=order["state"],
            new_state=new_state,
            detail={"kind": "recovery_refund", "operation_key": operation_key},
            created_at=now,
        )
        return dict(
            conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?", (transfer_id,)
            ).fetchone()
        )

    def _raise_if_unhealthy_conn(self, conn: sqlite3.Connection) -> None:
        if self._health_failure is not None:
            raise AccountingInvariantError(self._health_failure)
        if (
            conn.execute(
                "SELECT 1 FROM transfers WHERE state='uncertain' LIMIT 1"
            ).fetchone()
            is not None
        ):
            raise AccountingInvariantError("uncertain transfer blocks custody intake")
        issues = self._health_issues_conn(conn, network=self.network)
        if issues:
            raise AccountingInvariantError(f"ledger health failure: {','.join(issues)}")

    def _can_auto_queue_recovery_conn(
        self, conn: sqlite3.Connection, *, order: sqlite3.Row
    ) -> bool:
        if self._health_failure is not None:
            return False
        if self._health_issues_conn(conn, network=self.network):
            return False
        if (
            conn.execute(
                """
            SELECT 1 FROM (
              SELECT network,deposit_addr AS address FROM deposit_credits
              WHERE order_id=?
              UNION ALL
              SELECT network,address FROM deposit_scans WHERE address=?
            ) evidence WHERE network!=? LIMIT 1
            """,
                (order["order_id"], order["deposit_addr"], self.network),
            ).fetchone()
            is not None
        ):
            return False
        if order["seller_id"] is None:
            return False
        seller = conn.execute(
            "SELECT wallet_addr FROM users WHERE user_id=?", (order["seller_id"],)
        ).fetchone()
        if seller is None or seller["wallet_addr"] is None:
            return False
        return (
            conn.execute(
                """
            SELECT 1 FROM orders
            WHERE deposit_addr IS NOT NULL AND deposit_addr=? LIMIT 1
            """,
                (seller["wallet_addr"],),
            ).fetchone()
            is None
        )

    @staticmethod
    def _assert_safe_destination_conn(
        conn: sqlite3.Connection, *, destination: str
    ) -> None:
        if (
            conn.execute(
                """
            SELECT 1 FROM orders
            WHERE deposit_addr IS NOT NULL AND deposit_addr=? LIMIT 1
            """,
                (destination,),
            ).fetchone()
            is not None
        ):
            raise AccountingInvariantError("destination is an escrow deposit address")

    def reconcile_all_deposit_outputs(
        self,
        *,
        network: str,
        expected_tip_hash: str,
        expected_tip_height: int,
        snapshots: Sequence[Mapping[str, Any]],
        final_tip_hash: str,
        final_tip_height: int,
        credit_depth: int,
        now: int,
        trade_timeout_seconds: int | None = None,
    ) -> ReconciliationResult:
        """Apply one complete all-address observation at a single live tip.

        The explorer adapter validates its wire contract.  This boundary repeats
        the fund-sensitive identity and integer checks and refuses a partial or
        mixed batch before any durable observation is advanced.
        """

        self._require_integer(now, "reconciliation time")
        self._require_integer(expected_tip_height, "expected tip height")
        self._require_integer(final_tip_height, "final tip height")
        self._require_integer(credit_depth, "credit depth")
        if expected_tip_height < 0 or final_tip_height < 0:
            raise ValueError("tip heights must be non-negative")
        if credit_depth < 1:
            raise ValueError("credit depth must be positive")
        if trade_timeout_seconds is not None and (
            type(trade_timeout_seconds) is not int or trade_timeout_seconds < 1
        ):
            raise ValueError("trade timeout must be a positive integer")
        network = self._network(network)
        if network != self.network:
            raise ValueError(
                "reconciliation network does not match the configured store"
            )
        expected_tip_hash = self._hash_text(expected_tip_hash, "expected tip hash")
        final_tip_hash = self._hash_text(final_tip_hash, "final tip hash")
        if (final_tip_hash, final_tip_height) != (
            expected_tip_hash,
            expected_tip_height,
        ):
            raise AccountingInvariantError(
                "live tip changed during deposit reconciliation"
            )
        normalized = self._normalize_snapshot_batch(
            network=network,
            expected_tip_hash=expected_tip_hash,
            expected_tip_height=expected_tip_height,
            snapshots=snapshots,
        )

        conn = self.connect()
        try:
            conn.execute("BEGIN IMMEDIATE")
            watched_rows = conn.execute(
                """
                SELECT order_id, deposit_addr FROM orders
                WHERE deposit_addr IS NOT NULL ORDER BY deposit_addr
                """
            ).fetchall()
            watched = {row["deposit_addr"]: row["order_id"] for row in watched_rows}
            if set(normalized) != set(watched):
                raise AccountingInvariantError(
                    "deposit snapshot addresses do not equal the current watched set"
                )
            health_clear_before_evidence = (
                self._health_failure is None
                and not self._health_issues_conn(conn, network=self.network)
            )

            scan_ids: list[tuple[str, int]] = []
            changed_orders: set[int] = set()
            before_changes: dict[int, tuple[str, Mapping[str, int]]] = {}
            for address in sorted(watched):
                outputs = normalized[address]
                latest = conn.execute(
                    """
                    SELECT * FROM deposit_scans
                    WHERE network=? AND address=? ORDER BY scan_id DESC LIMIT 1
                    """,
                    (network, address),
                ).fetchone()
                reuse_latest = bool(
                    latest is not None
                    and latest["tip_hash"] == expected_tip_hash
                    and latest["tip_height"] == expected_tip_height
                )
                if reuse_latest:
                    order_id = watched[address]
                    before_order = conn.execute(
                        """
                        SELECT state,deposit_deadline,deposit_required_units
                        FROM orders WHERE order_id=?
                        """,
                        (order_id,),
                    ).fetchone()
                    if before_order is None:
                        raise AccountingInvariantError("watched order disappeared")
                    before_accounting = self._deposit_accounting_conn(
                        conn, order_id=order_id, network=network
                    )
                    deadline_due = bool(
                        before_order["state"] == "awaiting_deposit"
                        and before_order["deposit_deadline"] is not None
                        and now > before_order["deposit_deadline"]
                    )
                    funding_ready = bool(
                        health_clear_before_evidence
                        and before_order["state"] == "awaiting_deposit"
                        and before_accounting["main_units"]
                        >= before_order["deposit_required_units"]
                    )
                    if health_clear_before_evidence:
                        self._enforce_deposit_deadline_conn(
                            conn,
                            order_id=order_id,
                            network=network,
                            now=now,
                            allow_fund_movement=False,
                        )
                    after_order = conn.execute(
                        "SELECT state FROM orders WHERE order_id=?", (order_id,)
                    ).fetchone()
                    after_accounting = self._deposit_accounting_conn(
                        conn, order_id=order_id, network=network
                    )
                    deadline_ready = health_clear_before_evidence and deadline_due
                    if (
                        deadline_ready
                        or funding_ready
                        or (
                            after_order["state"] != before_order["state"]
                            or after_accounting != before_accounting
                        )
                    ):
                        before_changes[order_id] = (
                            before_order["state"],
                            before_accounting,
                        )
                        changed_orders.add(order_id)
                    self._require_same_tip_semantic_noop(
                        conn,
                        network=network,
                        address=address,
                        outputs=outputs,
                    )
                    scan_ids.append((address, int(latest["scan_id"])))
                    continue

                cursor = conn.execute(
                    """
                    INSERT INTO deposit_scans(
                      network,address,tip_hash,tip_height,observed_at
                    ) VALUES(?,?,?,?,?)
                    """,
                    (
                        network,
                        address,
                        expected_tip_hash,
                        expected_tip_height,
                        now,
                    ),
                )
                if cursor.lastrowid is None:
                    raise AccountingInvariantError("deposit scan ID was not persisted")
                scan_id = int(cursor.lastrowid)
                scan_ids.append((address, scan_id))
                order_id = watched[address]
                before_order = conn.execute(
                    "SELECT state FROM orders WHERE order_id=?", (order_id,)
                ).fetchone()
                if before_order is None:
                    raise AccountingInvariantError("watched order disappeared")
                before_changes[order_id] = (
                    before_order["state"],
                    self._deposit_accounting_conn(
                        conn, order_id=order_id, network=network
                    ),
                )
                if health_clear_before_evidence:
                    self._enforce_deposit_deadline_conn(
                        conn,
                        order_id=order_id,
                        network=network,
                        now=now,
                        allow_fund_movement=False,
                    )
                returned: set[tuple[str, int]] = set()
                for output in outputs:
                    returned.add((output["txid"], output["vout"]))
                    self._upsert_deposit_output_conn(
                        conn,
                        order_id=order_id,
                        network=network,
                        address=address,
                        output=output,
                        scan_id=scan_id,
                        credit_depth=credit_depth,
                        now=now,
                    )
                for credit in conn.execute(
                    """
                    SELECT credit_id,txid,vout,current_best_chain,last_checked_scan_id
                    FROM deposit_credits
                    WHERE network=? AND deposit_addr=?
                    ORDER BY credit_id
                    """,
                    (network, address),
                ).fetchall():
                    if (credit["txid"], credit["vout"]) in returned:
                        continue
                    conn.execute(
                        """
                        UPDATE deposit_credits
                        SET current_best_chain=0,last_checked_scan_id=?
                        WHERE credit_id=?
                        """,
                        (scan_id, credit["credit_id"]),
                    )
                changed_orders.add(order_id)

            # The watched set is deliberately checked again immediately before
            # commit.  Another process can only race before our IMMEDIATE lock;
            # this second comparison also detects test fault injection.
            current_watched = {
                row[0]
                for row in conn.execute(
                    "SELECT DISTINCT deposit_addr FROM orders WHERE deposit_addr IS NOT NULL"
                )
            }
            if current_watched != set(normalized):
                raise AccountingInvariantError(
                    "watched deposit set changed during reconciliation"
                )

            progress_allowed = (
                self._health_failure is None
                and not self._health_issues_conn(conn, network=self.network)
            )
            order_changes: list[DepositOrderChange] = []
            for order_id in sorted(changed_orders):
                if progress_allowed:
                    self._enforce_deposit_deadline_conn(
                        conn,
                        order_id=order_id,
                        network=self.network,
                        now=now,
                        allow_fund_movement=True,
                    )
                    refreshed = conn.execute(
                        "SELECT state FROM orders WHERE order_id=?", (order_id,)
                    ).fetchone()
                    if refreshed is None:
                        raise AccountingInvariantError("reconciled order disappeared")
                    if refreshed["state"] == "awaiting_deposit":
                        self._advance_funded_order_conn(
                            conn,
                            order_id=order_id,
                            network=self.network,
                            now=now,
                            trade_timeout_seconds=trade_timeout_seconds,
                        )
                refreshed = conn.execute(
                    "SELECT * FROM orders WHERE order_id=?", (order_id,)
                ).fetchone()
                if (
                    progress_allowed
                    and refreshed["state"]
                    in {
                        "recovery_hold",
                        "completed",
                        "refunded",
                        "cancelled",
                        "deposit_expired",
                    }
                    and self._can_auto_queue_recovery_conn(conn, order=refreshed)
                ):
                    self._queue_recovery_transfer_conn(
                        conn, order=refreshed, now=now, actor_id=None
                    )
                refreshed = conn.execute(
                    "SELECT * FROM orders WHERE order_id=?", (order_id,)
                ).fetchone()
                if refreshed is None:
                    raise AccountingInvariantError("reconciled order disappeared")
                old_state, before_accounting = before_changes[order_id]
                after_accounting = self._deposit_accounting_conn(
                    conn, order_id=order_id, network=network
                )
                order_changes.append(
                    DepositOrderChange(
                        order_id=order_id,
                        old_state=old_state,
                        new_state=refreshed["state"],
                        credited_delta_units=(
                            after_accounting["credited_units"]
                            - before_accounting["credited_units"]
                        ),
                        main_delta_units=(
                            after_accounting["main_units"]
                            - before_accounting["main_units"]
                        ),
                        recovery_delta_units=(
                            after_accounting["recovery_units"]
                            - before_accounting["recovery_units"]
                        ),
                        recovery_total_units=after_accounting["recovery_units"],
                    )
                )
                self._append_audit_conn(
                    conn,
                    order_id=order_id,
                    actor_id=None,
                    event_type="deposit_reconciled",
                    old_state=None,
                    new_state=None,
                    detail={
                        "tip_hash": expected_tip_hash,
                        "tip_height": expected_tip_height,
                    },
                    created_at=now,
                )

            issues_set = set(self._health_issues_conn(conn, network=self.network))
            if self._health_failure is not None:
                issues_set.add("process_health_failure")
            issues = tuple(sorted(issues_set))
            recovery_ids = self._recovery_order_ids_conn(conn, network=self.network)
            restricted = self._restricted_outpoints_conn(conn, network=self.network)
            conn.execute("COMMIT")
            return ReconciliationResult(
                healthy=not issues,
                health_issues=issues,
                recovery_order_ids=recovery_ids,
                restricted_outpoints=restricted,
                scan_ids=tuple(scan_ids),
                order_changes=tuple(order_changes),
            )
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def order_liability_units(self, *, order_id: int) -> int:
        self._require_integer(order_id, "order ID")
        conn = self.connect()
        try:
            if (
                conn.execute(
                    "SELECT 1 FROM orders WHERE order_id=?", (order_id,)
                ).fetchone()
                is None
            ):
                raise ValueError("order does not exist")
            return self._order_liability_conn(
                conn, order_id=order_id, network=self.network
            )
        finally:
            conn.close()

    def customer_liability_units(self) -> int:
        conn = self.connect()
        try:
            return self._customer_liability_conn(conn, network=self.network)
        finally:
            conn.close()

    def provisional_restricted_units(self) -> int:
        conn = self.connect()
        try:
            units = conn.execute(
                """
                SELECT COALESCE(SUM(amount_units),0) FROM deposit_credits
                WHERE network=? AND credited_at IS NULL
                  AND current_best_chain=1 AND mature=1
                  AND spent_by_txid IS NULL
                """,
                (self.network,),
            ).fetchone()[0]
            return self._nonnegative_aggregate(units, "provisional restricted units")
        finally:
            conn.close()

    def wallet_solvency_snapshot(
        self,
        *,
        expected_tip_hash: str,
        expected_tip_height: int,
        wallet_spendable_units: int,
        wallet_outpoints: Mapping[str, int],
        wallet_snapshot_hash: str,
    ) -> WalletSolvencySnapshot:
        self._require_integer(expected_tip_height, "expected tip height")
        self._require_integer(wallet_spendable_units, "wallet spendable units")
        if expected_tip_height < 0 or wallet_spendable_units < 0:
            raise ValueError("wallet amounts and tip height must be non-negative")
        expected_tip_hash = self._hash_text(expected_tip_hash, "expected tip hash")
        wallet_snapshot_hash = self._hash_text(
            wallet_snapshot_hash, "wallet snapshot hash"
        )
        if not isinstance(wallet_outpoints, Mapping):
            raise ValueError("wallet outpoints must be a mapping")
        normalized_outpoints: dict[str, int] = {}
        for key, units in wallet_outpoints.items():
            key = self._outpoint_text(key)
            self._require_integer(units, "wallet outpoint units")
            if units <= 0 or units > MAX_09C_UNITS or key in normalized_outpoints:
                raise ValueError("wallet outpoints must be unique positive units")
            normalized_outpoints[key] = units
        structured_spendable = sum(normalized_outpoints.values())
        if structured_spendable > MAX_09C_UNITS:
            raise ValueError("wallet outpoint sum exceeds the protocol supply")
        if structured_spendable != wallet_spendable_units:
            raise AccountingInvariantError(
                "wallet spendable total differs from structured outpoints"
            )

        conn = self.connect()
        try:
            conn.execute("BEGIN")
            self._require_common_tip_conn(
                conn,
                network=self.network,
                expected_tip_hash=expected_tip_hash,
                expected_tip_height=expected_tip_height,
            )
            issues_set = set(self._health_issues_conn(conn, network=self.network))
            if self._health_failure is not None:
                issues_set.add("process_health_failure")
            issues = tuple(sorted(issues_set))
            if issues:
                raise AccountingInvariantError(
                    f"ledger health failure: {','.join(issues)}"
                )
            self._solvency_checkpoint("after_health")
            restricted = self._restricted_outpoints_conn(conn, network=self.network)
            for txid, vout, units in restricted:
                if normalized_outpoints.get(f"{txid}:{vout}") != units:
                    raise AccountingInvariantError(
                        "wallet snapshot does not contain exact restricted outpoint"
                    )
            provisional = sum(item[2] for item in restricted)
            liability = self._customer_liability_conn(conn, network=self.network)
            pending = self._pending_platform_outflow_conn(conn)
            usable = wallet_spendable_units - provisional
            required = liability + pending
            if usable < 0:
                raise AccountingInvariantError(
                    "provisional funds exceed wallet spendable funds"
                )
            if usable < required:
                raise AccountingInvariantError("escrow wallet is insolvent")
            result = WalletSolvencySnapshot(
                expected_tip_hash=expected_tip_hash,
                expected_tip_height=expected_tip_height,
                wallet_spendable_units=wallet_spendable_units,
                provisional_restricted_units=provisional,
                customer_liability_units=liability,
                pending_platform_outflow_units=pending,
                usable_wallet_units=usable,
                required_wallet_units=required,
                restricted_outpoints=restricted,
                wallet_snapshot_hash=wallet_snapshot_hash,
            )
            conn.execute("COMMIT")
            return result
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
            raise
        finally:
            conn.close()

    def _solvency_checkpoint(self, phase: str) -> None:
        """Test seam proving all solvency reads share one SQLite snapshot."""

    @classmethod
    def _normalize_snapshot_batch(
        cls,
        *,
        network: str,
        expected_tip_hash: str,
        expected_tip_height: int,
        snapshots: Sequence[Mapping[str, Any]],
    ) -> dict[str, tuple[dict[str, Any], ...]]:
        if isinstance(snapshots, (str, bytes, bytearray)) or not isinstance(
            snapshots, Sequence
        ):
            raise ValueError("deposit snapshots must be a bounded sequence")
        if len(snapshots) > 10_000:
            raise ValueError("too many deposit snapshots")
        normalized: dict[str, tuple[dict[str, Any], ...]] = {}
        seen_outpoints: set[tuple[str, str, int]] = set()
        for snapshot in snapshots:
            if not isinstance(snapshot, Mapping):
                raise ValueError("each deposit snapshot must be an object")
            if (
                snapshot.get("network") != network
                or snapshot.get("complete") is not True
            ):
                raise AccountingInvariantError(
                    "deposit snapshot is incomplete or wrong network"
                )
            address = cls._bounded_text(
                snapshot.get("address"), "snapshot address", 128
            )
            if address in normalized:
                raise AccountingInvariantError("duplicate deposit snapshot address")
            tip_hash = cls._hash_text(snapshot.get("tip_hash"), "snapshot tip hash")
            cls._require_integer(snapshot.get("tip_height"), "snapshot tip height")
            if (tip_hash, snapshot["tip_height"]) != (
                expected_tip_hash,
                expected_tip_height,
            ):
                raise AccountingInvariantError("deposit snapshot has a mixed tip")
            outputs = snapshot.get("outputs")
            if isinstance(outputs, (str, bytes, bytearray)) or not isinstance(
                outputs, Sequence
            ):
                raise ValueError("snapshot outputs must be a bounded sequence")
            if len(outputs) > 100_000:
                raise ValueError("too many outputs in deposit snapshot")
            normalized_outputs: list[dict[str, Any]] = []
            for raw in outputs:
                if not isinstance(raw, Mapping):
                    raise ValueError("deposit output must be an object")
                txid = cls._hash_text(raw.get("txid"), "deposit transaction ID")
                cls._require_integer(raw.get("vout"), "deposit output index")
                cls._require_integer(raw.get("amount_units"), "deposit output units")
                cls._require_integer(raw.get("block_height"), "deposit block height")
                cls._require_integer(raw.get("confirmations"), "deposit confirmations")
                vout = raw["vout"]
                amount = raw["amount_units"]
                block_height = raw["block_height"]
                confirmations = raw["confirmations"]
                if vout < 0 or amount <= 0 or amount > MAX_09C_UNITS:
                    raise ValueError("deposit output has invalid index or units")
                if (
                    block_height < 0
                    or block_height > expected_tip_height
                    or confirmations < 1
                ):
                    raise ValueError("deposit output has invalid chain position")
                block_hash = cls._hash_text(raw.get("block_hash"), "deposit block hash")
                coinbase = cls._binary_flag(raw.get("coinbase"), "coinbase flag")
                mature = cls._binary_flag(raw.get("mature"), "maturity flag")
                expected_confirmations = expected_tip_height - block_height + 1
                if confirmations != expected_confirmations:
                    raise AccountingInvariantError(
                        "deposit confirmations do not match the common tip"
                    )
                coinbase_maturity = 100 if network == "btc09-mainnet" else 2
                expected_mature = (
                    1 if not coinbase or confirmations >= coinbase_maturity else 0
                )
                if mature != expected_mature:
                    raise AccountingInvariantError(
                        "deposit maturity does not match network consensus"
                    )
                spent_by = raw.get("spent_by")
                spent_tuple: tuple[str | None, int | None, str | None, int | None]
                if spent_by is None:
                    spent_tuple = (None, None, None, None)
                else:
                    if not isinstance(spent_by, Mapping):
                        raise ValueError("spent-by evidence must be an object or null")
                    spent_txid = cls._hash_text(
                        spent_by.get("txid"), "spending transaction ID"
                    )
                    cls._require_integer(spent_by.get("vin"), "spending input index")
                    spent_block_hash = cls._hash_text(
                        spent_by.get("block_hash"), "spending block hash"
                    )
                    cls._require_integer(
                        spent_by.get("block_height"), "spending block height"
                    )
                    if (
                        spent_by["vin"] < 0
                        or not block_height
                        <= spent_by["block_height"]
                        <= expected_tip_height
                    ):
                        raise ValueError("spent-by evidence has an invalid position")
                    spent_tuple = (
                        spent_txid,
                        spent_by["vin"],
                        spent_block_hash,
                        spent_by["block_height"],
                    )
                identity = (network, txid, vout)
                if identity in seen_outpoints:
                    raise AccountingInvariantError(
                        "duplicate deposit outpoint in batch"
                    )
                seen_outpoints.add(identity)
                normalized_outputs.append(
                    {
                        "txid": txid,
                        "vout": vout,
                        "amount_units": amount,
                        "block_hash": block_hash,
                        "block_height": block_height,
                        "confirmations": confirmations,
                        "coinbase": coinbase,
                        "mature": mature,
                        "spent_by_txid": spent_tuple[0],
                        "spent_by_vin": spent_tuple[1],
                        "spent_by_block_hash": spent_tuple[2],
                        "spent_by_block_height": spent_tuple[3],
                    }
                )
            normalized_outputs.sort(key=lambda item: (item["txid"], item["vout"]))
            normalized[address] = tuple(normalized_outputs)
        return normalized

    @classmethod
    def _require_same_tip_semantic_noop(
        cls,
        conn: sqlite3.Connection,
        *,
        network: str,
        address: str,
        outputs: Sequence[Mapping[str, Any]],
    ) -> None:
        current = {
            (row["txid"], row["vout"]): row
            for row in conn.execute(
                """
                SELECT * FROM deposit_credits
                WHERE network=? AND deposit_addr=? AND current_best_chain=1
                """,
                (network, address),
            )
        }
        returned = {(row["txid"], row["vout"]): row for row in outputs}
        if set(current) != set(returned):
            raise AccountingInvariantError(
                "same-tip deposit snapshot changed its output set"
            )
        fields = (
            "amount_units",
            "block_hash",
            "block_height",
            "confirmations",
            "coinbase",
            "mature",
            "spent_by_txid",
            "spent_by_vin",
            "spent_by_block_hash",
            "spent_by_block_height",
        )
        for identity, existing in current.items():
            if any(existing[field] != returned[identity][field] for field in fields):
                raise AccountingInvariantError("same-tip deposit evidence changed")

    @classmethod
    def _upsert_deposit_output_conn(
        cls,
        conn: sqlite3.Connection,
        *,
        order_id: int,
        network: str,
        address: str,
        output: Mapping[str, Any],
        scan_id: int,
        credit_depth: int,
        now: int,
    ) -> None:
        existing = conn.execute(
            "SELECT * FROM deposit_credits WHERE network=? AND txid=? AND vout=?",
            (network, output["txid"], output["vout"]),
        ).fetchone()
        qualifies = output["confirmations"] >= credit_depth and output["mature"] == 1
        if existing is None:
            main_units, recovery_units, reason = cls._classify_new_credit_conn(
                conn,
                order_id=order_id,
                network=network,
                amount_units=output["amount_units"],
                qualifies=qualifies,
                now=now,
            )
            conn.execute(
                """
                INSERT INTO deposit_credits(
                  order_id,network,txid,vout,deposit_addr,amount_units,
                  block_hash,block_height,confirmations,coinbase,mature,
                  current_best_chain,spent_by_txid,spent_by_vin,
                  spent_by_block_hash,spent_by_block_height,credited_at,
                  main_units,recovery_units,recovery_reason,first_seen_at,
                  last_seen_at,last_seen_scan_id,last_checked_scan_id
                ) VALUES(?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?)
                """,
                (
                    order_id,
                    network,
                    output["txid"],
                    output["vout"],
                    address,
                    output["amount_units"],
                    output["block_hash"],
                    output["block_height"],
                    output["confirmations"],
                    output["coinbase"],
                    output["mature"],
                    output["spent_by_txid"],
                    output["spent_by_vin"],
                    output["spent_by_block_hash"],
                    output["spent_by_block_height"],
                    now if qualifies else None,
                    main_units,
                    recovery_units,
                    reason,
                    now,
                    now,
                    scan_id,
                    scan_id,
                ),
            )
            return

        if (
            existing["order_id"] != order_id
            or existing["deposit_addr"] != address
            or existing["amount_units"] != output["amount_units"]
            or existing["coinbase"] != output["coinbase"]
        ):
            raise AccountingInvariantError(
                "deposit outpoint identity conflicts with its durable credit"
            )
        credited_at = existing["credited_at"]
        main_units = existing["main_units"]
        recovery_units = existing["recovery_units"]
        reason = existing["recovery_reason"]
        if credited_at is None and qualifies:
            main_units, recovery_units, reason = cls._classify_new_credit_conn(
                conn,
                order_id=order_id,
                network=network,
                amount_units=output["amount_units"],
                qualifies=True,
                now=now,
            )
            credited_at = now
        conn.execute(
            """
            UPDATE deposit_credits SET
              block_hash=?,block_height=?,confirmations=?,mature=?,
              current_best_chain=1,spent_by_txid=?,spent_by_vin=?,
              spent_by_block_hash=?,spent_by_block_height=?,credited_at=?,
              main_units=?,recovery_units=?,recovery_reason=?,last_seen_at=?,
              last_seen_scan_id=?,last_checked_scan_id=?
            WHERE credit_id=?
            """,
            (
                output["block_hash"],
                output["block_height"],
                output["confirmations"],
                output["mature"],
                output["spent_by_txid"],
                output["spent_by_vin"],
                output["spent_by_block_hash"],
                output["spent_by_block_height"],
                credited_at,
                main_units,
                recovery_units,
                reason,
                now,
                scan_id,
                scan_id,
                existing["credit_id"],
            ),
        )

    def _enforce_deposit_deadline_conn(
        self,
        conn: sqlite3.Connection,
        *,
        order_id: int,
        network: str,
        now: int,
        allow_fund_movement: bool,
    ) -> None:
        order = conn.execute(
            "SELECT * FROM orders WHERE order_id=?", (order_id,)
        ).fetchone()
        if (
            order is None
            or order["state"] != "awaiting_deposit"
            or order["deposit_deadline"] is None
            or now <= order["deposit_deadline"]
        ):
            return
        credited = conn.execute(
            """
            SELECT COALESCE(SUM(amount_units),0) AS credited_units,
              COALESCE(SUM(main_units),0) AS main_units
            FROM deposit_credits
            WHERE order_id=? AND network=? AND credited_at IS NOT NULL
            """,
            (order_id, network),
        ).fetchone()
        credited_units = self._nonnegative_aggregate(
            credited["credited_units"], "deadline credited units"
        )
        main_credited = self._nonnegative_aggregate(
            credited["main_units"], "deadline main credit units"
        )
        if credited_units == 0:
            new_state = "deposit_expired"
            conn.execute(
                """
                UPDATE orders SET state='deposit_expired',updated_at=?
                WHERE order_id=? AND state='awaiting_deposit'
                """,
                (now, order_id),
            )
        elif main_credited == 0 and credited_units >= order["deposit_required_units"]:
            if not allow_fund_movement:
                return
            queued = self._queue_recovery_transfer_conn(
                conn, order=order, now=now, actor_id=None
            )
            if queued is None:
                raise AccountingInvariantError(
                    "full late recovery did not queue a refund"
                )
            new_state = "refund_reserved"
        elif main_credited < order["deposit_required_units"]:
            new_state = "recovery_hold"
            conn.execute(
                """
                UPDATE orders SET state='recovery_hold',updated_at=?
                WHERE order_id=? AND state='awaiting_deposit'
                """,
                (now, order_id),
            )
            for credit in conn.execute(
                """
                SELECT credit_id,main_units,recovery_units FROM deposit_credits
                WHERE order_id=? AND network=? AND credited_at IS NOT NULL
                  AND main_units>0 ORDER BY credit_id
                """,
                (order_id, network),
            ).fetchall():
                conn.execute(
                    """
                    UPDATE deposit_credits SET main_units=0,
                      recovery_units=?,recovery_reason='cancelled_partial'
                    WHERE credit_id=?
                    """,
                    (
                        credit["main_units"] + credit["recovery_units"],
                        credit["credit_id"],
                    ),
                )
        else:
            if not allow_fund_movement:
                return
            self._advance_funded_order_conn(
                conn, order_id=order_id, network=network, now=now
            )
            funded = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            self._queue_order_transfer_conn(
                conn,
                order=funded,
                kind="refund",
                now=now,
                actor_id=None,
                allow_release=False,
                transition_order=True,
            )
            new_state = "refund_reserved"
        self._append_audit_conn(
            conn,
            order_id=order_id,
            actor_id=None,
            event_type="deposit_deadline_expired",
            old_state="awaiting_deposit",
            new_state=new_state,
            detail={"deposit_deadline": order["deposit_deadline"]},
            created_at=now,
        )

    @classmethod
    def _classify_new_credit_conn(
        cls,
        conn: sqlite3.Connection,
        *,
        order_id: int,
        network: str,
        amount_units: int,
        qualifies: bool,
        now: int,
    ) -> tuple[int, int, str | None]:
        if not qualifies:
            return 0, 0, None
        order = conn.execute(
            "SELECT * FROM orders WHERE order_id=?", (order_id,)
        ).fetchone()
        if order is None:
            raise AccountingInvariantError("deposit credit refers to a missing order")
        if (
            order["state"] == "awaiting_deposit"
            and order["deposit_deadline"] is not None
            and now > order["deposit_deadline"]
        ):
            return 0, amount_units, "late"
        main_allocated = conn.execute(
            """
            SELECT COALESCE(SUM(main_units),0) FROM deposit_credits
            WHERE order_id=? AND network=? AND credited_at IS NOT NULL
            """,
            (order_id, network),
        ).fetchone()[0]
        main_allocated = cls._nonnegative_aggregate(main_allocated, "main credit units")
        if main_allocated > order["deposit_required_units"]:
            raise AccountingInvariantError("main credit capacity exceeds order quote")
        has_main_transfer = (
            conn.execute(
                "SELECT 1 FROM transfers WHERE order_id=? AND is_main_outcome=1",
                (order_id,),
            ).fetchone()
            is not None
        )
        main_states = {"awaiting_deposit", "open", "matched", "disputed"}
        if order["state"] in main_states and not has_main_transfer:
            remaining = order["deposit_required_units"] - main_allocated
            main = min(amount_units, remaining)
            recovery = amount_units - main
            return main, recovery, "excess" if recovery else None
        reason = (
            "cancelled_partial"
            if order["state"] in {"recovery_hold", "refund_reserved"}
            and main_allocated < order["deposit_required_units"]
            else "late"
        )
        return 0, amount_units, reason

    @classmethod
    def _advance_funded_order_conn(
        cls,
        conn: sqlite3.Connection,
        *,
        order_id: int,
        network: str,
        now: int,
        trade_timeout_seconds: int | None = None,
    ) -> None:
        order = conn.execute(
            "SELECT * FROM orders WHERE order_id=?", (order_id,)
        ).fetchone()
        if order is None or order["state"] != "awaiting_deposit":
            return
        main_units = conn.execute(
            """
            SELECT COALESCE(SUM(main_units),0) FROM deposit_credits
            WHERE order_id=? AND network=? AND credited_at IS NOT NULL
            """,
            (order_id, network),
        ).fetchone()[0]
        if main_units < order["deposit_required_units"]:
            return
        new_state = "open" if order["side"] == "sell" else "matched"
        if new_state == "matched" and trade_timeout_seconds is not None:
            conn.execute(
                """
                UPDATE orders SET state='matched',funded_at=COALESCE(funded_at,?),
                  matched_at=?,trade_deadline=?,updated_at=?
                WHERE order_id=? AND state='awaiting_deposit'
                """,
                (now, now, now + trade_timeout_seconds, now, order_id),
            )
        else:
            conn.execute(
                """
                UPDATE orders SET state=?,funded_at=COALESCE(funded_at,?),updated_at=?
                WHERE order_id=? AND state='awaiting_deposit'
                """,
                (new_state, now, now, order_id),
            )

    @classmethod
    def _order_liability_conn(
        cls, conn: sqlite3.Connection, *, order_id: int, network: str
    ) -> int:
        credited = conn.execute(
            """
            SELECT COALESCE(SUM(amount_units),0) FROM deposit_credits
            WHERE order_id=? AND network=? AND credited_at IS NOT NULL
            """,
            (order_id, network),
        ).fetchone()[0]
        discharged = conn.execute(
            """
            SELECT COALESCE(SUM(a.units),0)
            FROM transfer_credit_allocations a
            JOIN transfers t ON t.transfer_id=a.transfer_id
            JOIN deposit_credits c ON c.credit_id=a.credit_id
            WHERE a.order_id=? AND c.network=? AND t.state='confirmed'
            """,
            (order_id, network),
        ).fetchone()[0]
        liability = credited - discharged
        if liability < 0:
            raise AccountingInvariantError("order liability is negative")
        return liability

    @classmethod
    def _customer_liability_conn(cls, conn: sqlite3.Connection, *, network: str) -> int:
        credited = conn.execute(
            """
            SELECT COALESCE(SUM(amount_units),0) FROM deposit_credits
            WHERE network=? AND credited_at IS NOT NULL
            """,
            (network,),
        ).fetchone()[0]
        discharged = conn.execute(
            """
            SELECT COALESCE(SUM(a.units),0)
            FROM transfer_credit_allocations a
            JOIN transfers t ON t.transfer_id=a.transfer_id
            JOIN deposit_credits c ON c.credit_id=a.credit_id
            WHERE c.network=? AND t.state='confirmed'
            """,
            (network,),
        ).fetchone()[0]
        liability = credited - discharged
        if liability < 0:
            raise AccountingInvariantError("customer liability is negative")
        return liability

    @classmethod
    def _restricted_outpoints_conn(
        cls, conn: sqlite3.Connection, *, network: str
    ) -> tuple[tuple[str, int, int], ...]:
        return tuple(
            (row["txid"], row["vout"], row["amount_units"])
            for row in conn.execute(
                """
                SELECT txid,vout,amount_units FROM deposit_credits
                WHERE network=? AND credited_at IS NULL
                  AND current_best_chain=1 AND mature=1
                  AND spent_by_txid IS NULL
                ORDER BY txid,vout
                """,
                (network,),
            )
        )

    @classmethod
    def _recovery_order_ids_conn(
        cls, conn: sqlite3.Connection, *, network: str
    ) -> tuple[int, ...]:
        rows = conn.execute(
            """
            SELECT o.order_id,
              COALESCE((SELECT SUM(c.recovery_units) FROM deposit_credits c
                        WHERE c.order_id=o.order_id AND c.network=?
                          AND c.credited_at IS NOT NULL),0)
              - COALESCE((SELECT SUM(a.units)
                          FROM transfer_credit_allocations a
                          JOIN transfers t ON t.transfer_id=a.transfer_id
                          JOIN deposit_credits c ON c.credit_id=a.credit_id
                          WHERE a.order_id=o.order_id AND c.network=?
                            AND a.bucket='recovery'
                            AND t.state='confirmed'),0) AS residual,
              EXISTS(SELECT 1 FROM deposit_credits c
                     WHERE c.order_id=o.order_id AND c.network=?
                       AND c.credited_at IS NOT NULL
                       AND c.current_best_chain=0) AS reorg
            FROM orders o ORDER BY o.order_id
            """,
            (network, network, network),
        ).fetchall()
        result: list[int] = []
        for row in rows:
            if row["residual"] < 0:
                raise AccountingInvariantError("recovery liability is negative")
            if row["residual"] > 0 or row["reorg"]:
                result.append(row["order_id"])
        return tuple(result)

    @classmethod
    def _health_issues_conn(
        cls, conn: sqlite3.Connection, *, network: str
    ) -> tuple[str, ...]:
        issues: set[str] = set()
        if (
            conn.execute(
                "SELECT 1 FROM transfers WHERE state='uncertain' LIMIT 1"
            ).fetchone()
            is not None
        ):
            issues.add("uncertain_transfer")
        if (
            conn.execute(
                """
            SELECT 1 FROM (
              SELECT network,address FROM deposit_scans
              UNION ALL
              SELECT network,deposit_addr AS address FROM deposit_credits
            ) evidence
            WHERE evidence.network!=?
              AND evidence.address IN (
                SELECT deposit_addr FROM orders WHERE deposit_addr IS NOT NULL
              )
            LIMIT 1
            """,
                (network,),
            ).fetchone()
            is not None
        ):
            issues.add("cross_network_evidence")
        if (
            conn.execute(
                """
            SELECT 1 FROM deposit_credits
            WHERE network=? AND credited_at IS NOT NULL
              AND current_best_chain=0 LIMIT 1
            """,
                (network,),
            ).fetchone()
            is not None
        ):
            issues.add("post_credit_reorg")
        if (
            conn.execute(
                """
            SELECT 1 FROM deposit_credits c
            WHERE c.network=? AND c.current_best_chain=1
              AND c.spent_by_txid IS NOT NULL
              AND NOT EXISTS(SELECT 1 FROM transfers t WHERE t.txid=c.spent_by_txid)
            LIMIT 1
            """,
                (network,),
            ).fetchone()
            is not None
        ):
            issues.add("unknown_spend")
        if (
            conn.execute(
                """
            SELECT 1 FROM orders o
            LEFT JOIN users u ON u.user_id=o.seller_id
            WHERE (
              COALESCE((SELECT SUM(c.recovery_units) FROM deposit_credits c
                        WHERE c.order_id=o.order_id AND c.network=?
                          AND c.credited_at IS NOT NULL),0)
              - COALESCE((SELECT SUM(a.units)
                          FROM transfer_credit_allocations a
                          JOIN deposit_credits c ON c.credit_id=a.credit_id
                          WHERE a.order_id=o.order_id AND c.network=?
                            AND a.bucket='recovery'),0)
            ) > 0
              AND (u.wallet_addr IS NULL OR EXISTS(
                SELECT 1 FROM orders watched
                WHERE watched.deposit_addr IS NOT NULL
                  AND watched.deposit_addr=u.wallet_addr
              ))
            LIMIT 1
            """,
                (network, network),
            ).fetchone()
            is not None
        ):
            issues.add("unsafe_recovery_destination")
        if (
            conn.execute(
                """
            SELECT 1 FROM transfers
            WHERE (
              state IN ('queued','reserved','failed_safe','cancelled')
              AND (txid IS NOT NULL OR signed_tx_hex IS NOT NULL
                   OR signed_at IS NOT NULL OR prepared_tip_hash IS NOT NULL
                   OR prepared_tip_height IS NOT NULL)
            ) OR (
              state IN ('prepared','broadcast','confirmed','uncertain')
              AND (txid IS NULL OR signed_tx_hex IS NULL
                   OR signed_at IS NULL OR prepared_tip_hash IS NULL
                   OR prepared_tip_height IS NULL)
            )
            LIMIT 1
            """
            ).fetchone()
            is not None
        ):
            issues.add("malformed_transfer_identity")
        cls._customer_liability_conn(conn, network=network)
        available = cls._available_fee_conn(conn)
        if available < 0:
            issues.add("negative_available_fees")
        return tuple(sorted(issues))

    @classmethod
    def _pending_platform_outflow_conn(cls, conn: sqlite3.Connection) -> int:
        value = conn.execute(
            """
            SELECT COALESCE(SUM(amount_units + network_fee_units),0)
            FROM transfers
            WHERE kind='fee_withdrawal'
              AND state NOT IN ('confirmed','cancelled')
            """
        ).fetchone()[0]
        return cls._nonnegative_aggregate(value, "pending platform outflow")

    @classmethod
    def _available_fee_conn(cls, conn: sqlite3.Connection) -> int:
        earned = conn.execute(
            """
            SELECT COALESCE(SUM(earned_fee_units),0) FROM transfers
            WHERE state='confirmed' AND kind IN ('release','resolve_buyer')
            """
        ).fetchone()[0]
        encumbered = conn.execute(
            """
            SELECT COALESCE(SUM(amount_units + network_fee_units),0)
            FROM transfers WHERE kind='fee_withdrawal' AND state!='cancelled'
            """
        ).fetchone()[0]
        return earned - encumbered

    @classmethod
    def _require_common_tip_conn(
        cls,
        conn: sqlite3.Connection,
        *,
        network: str,
        expected_tip_hash: str,
        expected_tip_height: int,
    ) -> None:
        if cls._common_tip_precondition_drift_conn(
            conn,
            network=network,
            expected_tip_hash=expected_tip_hash,
            expected_tip_height=expected_tip_height,
        ):
            raise AccountingInvariantError("watched addresses lack a common latest tip")

    @classmethod
    def _common_tip_precondition_drift_conn(
        cls,
        conn: sqlite3.Connection,
        *,
        network: str,
        expected_tip_hash: str,
        expected_tip_height: int,
    ) -> bool:
        watched = conn.execute(
            "SELECT DISTINCT deposit_addr FROM orders WHERE deposit_addr IS NOT NULL"
        ).fetchall()
        drifted = False
        for (address,) in watched:
            latest = conn.execute(
                """
                SELECT * FROM deposit_scans
                WHERE network=? AND address=? ORDER BY scan_id DESC LIMIT 1
                """,
                (network, address),
            ).fetchone()
            if latest is None or (latest["tip_hash"], latest["tip_height"]) != (
                expected_tip_hash,
                expected_tip_height,
            ):
                drifted = True
                if latest is None:
                    continue
            stale = conn.execute(
                """
                SELECT 1 FROM deposit_credits
                WHERE network=? AND deposit_addr=?
                  AND last_checked_scan_id!=? LIMIT 1
                """,
                (network, address, latest["scan_id"]),
            ).fetchone()
            if stale is not None:
                raise AccountingInvariantError("deposit credit evidence is stale")
        return drifted

    @classmethod
    def _append_audit_conn(
        cls,
        conn: sqlite3.Connection,
        *,
        order_id: int | None,
        actor_id: int | None,
        event_type: str,
        old_state: str | None,
        new_state: str | None,
        detail: Mapping[str, Any],
        created_at: int,
    ) -> int:
        detail_json = json.dumps(
            dict(detail),
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        )
        cursor = conn.execute(
            """
            INSERT INTO audit_events(
              order_id,actor_id,event_type,old_state,new_state,detail_json,created_at
            ) VALUES(?,?,?,?,?,?,?)
            """,
            (
                order_id,
                actor_id,
                event_type,
                old_state,
                new_state,
                detail_json,
                created_at,
            ),
        )
        if cursor.lastrowid is None:
            raise AccountingInvariantError("audit event ID was not persisted")
        return int(cursor.lastrowid)

    @staticmethod
    def _nonnegative_aggregate(value: object, label: str) -> int:
        if type(value) is not int or value < 0:
            raise AccountingInvariantError(f"{label} is not a non-negative integer")
        return value

    @classmethod
    def _network(cls, value: object) -> str:
        if value not in ("btc09-mainnet", "btc09-regtest"):
            raise ValueError("network must be btc09-mainnet or btc09-regtest")
        return str(value)

    @classmethod
    def _hash_text(cls, value: object, label: str) -> str:
        return cls._machine_text(value, label, 64, re.compile(r"[0-9a-f]{64}\Z"))

    def _expected_tip(
        self, expected_tip_hash: object, expected_tip_height: object
    ) -> tuple[str, int]:
        tip_hash = self._hash_text(expected_tip_hash, "expected observation tip hash")
        self._require_integer(expected_tip_height, "expected observation tip height")
        if expected_tip_height < 0:
            raise ValueError("expected observation tip height must be non-negative")
        return tip_hash, expected_tip_height

    def _require_observation_tip_conn(
        self,
        conn: sqlite3.Connection,
        expected_tip: tuple[str, int],
    ) -> None:
        self._require_common_tip_conn(
            conn,
            network=self.network,
            expected_tip_hash=expected_tip[0],
            expected_tip_height=expected_tip[1],
        )

    @classmethod
    def _outpoint_text(cls, value: object) -> str:
        return cls._machine_text(
            value,
            "wallet outpoint",
            84,
            re.compile(r"[0-9a-f]{64}:(?:0|[1-9][0-9]*)\Z"),
        )

    @classmethod
    def _binary_flag(cls, value: object, label: str) -> int:
        if type(value) is bool:
            return int(value)
        cls._require_integer(value, label)
        if value not in (0, 1):
            raise ValueError(f"{label} must be 0 or 1")
        return value

    def _apply_migration(self, conn: sqlite3.Connection, plan: _MigrationPlan) -> None:
        if plan.kind == "v3":
            for index_name in _V3_INDEX_NAMES:
                conn.execute(f'DROP INDEX "{index_name}"')
            self._migration_checkpoint("v3_indexes")
            conn.execute("ALTER TABLE orders RENAME TO orders_v3_archive")
            conn.execute("ALTER TABLE transfers RENAME TO transfers_v3_archive")
            conn.execute("ALTER TABLE audit_events RENAME TO audit_events_v3_archive")
            conn.execute("ALTER TABLE schema_meta RENAME TO schema_meta_v3_archive")
            self._migration_checkpoint("v3_archives")
        elif plan.migrate_orders:
            conn.execute("ALTER TABLE orders RENAME TO orders_v2_archive")
            self._migration_checkpoint("v2_archive")

        if plan.migrate_users:
            self._rebuild_users(conn)
            self._migration_checkpoint("users")

    @classmethod
    def _preflight_initialization(cls, conn: sqlite3.Connection) -> _MigrationPlan:
        schema_meta = cls._schema_object(conn, "schema_meta")
        if schema_meta is not None:
            if schema_meta["type"] != "table":
                raise MigrationBlocked("schema_meta is not a table")
            version = cls._read_schema_version(conn)
            if version > SCHEMA_VERSION:
                raise UnsupportedSchemaVersion(
                    f"database schema version {version} is newer than supported "
                    f"version {SCHEMA_VERSION}"
                )
            if version == SCHEMA_VERSION:
                origin = cls._read_schema_origin(conn)
                expected = _EXPECTED_V4_CATALOG_BY_ORIGIN.get(origin)
                if expected is None:
                    raise MigrationBlocked(
                        f"schema_meta contains unsupported migration origin {origin!r}"
                    )
                cls._validate_exact_catalog(
                    cls._read_catalog(conn),
                    expected,
                    f"v4 origin {origin}",
                )
                cls._validate_v4_evidence_rows(conn)
                cls._require_zero_sequences(
                    conn,
                    (
                        "orders_v2_archive",
                        "withdrawals",
                        "orders_v3_archive",
                        "transfers_v3_archive",
                        "audit_events_v3_archive",
                    ),
                )
                return _MigrationPlan("v4", origin)
            if version == 3:
                origin = cls._validate_v3_migration(conn)
                return _MigrationPlan("v3", origin, migrate_users=True)
            raise MigrationBlocked(
                f"database schema version {version} has no safe automatic migration"
            )
        return cls._preflight_unversioned(conn)

    @classmethod
    def _preflight_unversioned(cls, conn: sqlite3.Connection) -> _MigrationPlan:
        actual = cls._read_catalog(conn)
        if not actual:
            if cls._schema_object(conn, "sqlite_sequence") is not None:
                raise MigrationBlocked(
                    "unversioned database retains sqlite_sequence prior-use evidence"
                )
            cls._require_zero_sequences(conn, None)
            return _MigrationPlan("fresh", "fresh")

        cls._validate_exact_catalog(
            actual,
            _EXPECTED_LIVE_PROTOTYPE_OBJECTS,
            "live prototype",
        )
        cls._validate_compatible_users(conn)
        for table in ("orders", "withdrawals"):
            if conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] != 0:
                raise MigrationBlocked(
                    f"cannot migrate live prototype because {table} contains "
                    "financial records"
                )
        cls._require_zero_sequences(conn, ("orders", "withdrawals"))
        return _MigrationPlan(
            "prototype",
            "live_prototype",
            migrate_users=True,
            migrate_orders=True,
        )

    @classmethod
    def _validate_v3_migration(cls, conn: sqlite3.Connection) -> str:
        variant = cls._validate_catalog_variants(
            conn, _EXPECTED_V3_CATALOG_VARIANTS, "v3"
        )
        for table in ("orders", "transfers", "audit_events"):
            if conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] != 0:
                raise MigrationBlocked(
                    f"cannot migrate v3 because {table} contains financial records"
                )
        withdrawals = cls._schema_object(conn, "withdrawals")
        if withdrawals is not None:
            if conn.execute("SELECT COUNT(*) FROM withdrawals").fetchone()[0] != 0:
                raise MigrationBlocked(
                    "legacy withdrawals have no trustworthy opening revenue provenance"
                )
        archive = cls._schema_object(conn, "orders_v2_archive")
        if archive is not None and (
            archive["type"] != "table"
            or conn.execute("SELECT COUNT(*) FROM orders_v2_archive").fetchone()[0] != 0
        ):
            raise MigrationBlocked("v3 prototype archive must remain empty")
        cls._require_zero_sequences(
            conn,
            (
                "orders",
                "transfers",
                "audit_events",
                "withdrawals",
                "orders_v2_archive",
            ),
        )
        cls._validate_compatible_users(conn)
        return _V3_VARIANT_ORIGINS[variant]

    @staticmethod
    def _read_catalog(
        conn: sqlite3.Connection,
    ) -> dict[str, tuple[str, str]]:
        catalog: dict[str, tuple[str, str]] = {}
        for row in conn.execute("SELECT type, name, tbl_name, sql FROM sqlite_master"):
            if _is_verified_sqlite_internal(
                row["type"], row["name"], row["tbl_name"], row["sql"]
            ):
                continue
            if row["name"] in catalog:
                raise MigrationBlocked(
                    f"duplicate catalog name {row['name']} across object types"
                )
            catalog[row["name"]] = (row["type"], row["sql"])
        return catalog

    @classmethod
    def _validate_catalog_variants(
        cls,
        conn: sqlite3.Connection,
        variants: tuple[tuple[str, Mapping[str, tuple[str, str]]], ...],
        label: str,
    ) -> str:
        actual = cls._read_catalog(conn)
        for variant_name, expected in variants:
            if cls._catalogs_match(actual, expected):
                return variant_name

        allowed_names = set().union(*(set(expected) for _, expected in variants))
        unexpected = sorted(set(actual) - allowed_names)
        if unexpected:
            raise MigrationBlocked(f"unexpected {label} catalog object {unexpected[0]}")

        required_names = set.intersection(*(set(expected) for _, expected in variants))
        missing = sorted(required_names - set(actual))
        if missing:
            expected_type = variants[0][1][missing[0]][0]
            raise MigrationBlocked(
                f"required {label} {expected_type} {missing[0]} is missing"
            )

        for name, actual_definition in actual.items():
            allowed_definitions = {
                (object_type, _canonical_schema_sql(sql))
                for _, expected in variants
                if name in expected
                for object_type, sql in (expected[name],)
            }
            if type(actual_definition[1]) is not str:
                raise MigrationBlocked(
                    f"{name} does not match a recognized {label} definition"
                )
            normalized_actual = (
                actual_definition[0],
                _canonical_schema_sql(actual_definition[1]),
            )
            if allowed_definitions and normalized_actual not in allowed_definitions:
                raise MigrationBlocked(
                    f"{name} does not match a recognized {label} definition"
                )

        raise MigrationBlocked(
            f"{label} catalog does not match recognized migration evidence"
        )

    @classmethod
    def _validate_exact_catalog(
        cls,
        actual: Mapping[str, tuple[str, str]],
        expected: Mapping[str, tuple[str, str]],
        label: str,
    ) -> None:
        unexpected = sorted(set(actual) - set(expected))
        if unexpected:
            raise MigrationBlocked(f"unexpected {label} catalog object {unexpected[0]}")
        missing = sorted(set(expected) - set(actual))
        if missing:
            raise MigrationBlocked(
                f"required {label} {expected[missing[0]][0]} {missing[0]} is missing"
            )
        for name, expected_definition in expected.items():
            if not cls._definitions_match(actual[name], expected_definition):
                raise MigrationBlocked(
                    f"{name} does not match the trusted {label} definition"
                )

    @classmethod
    def _catalogs_match(
        cls,
        actual: Mapping[str, tuple[str, str]],
        expected: Mapping[str, tuple[str, str]],
    ) -> bool:
        return set(actual) == set(expected) and all(
            cls._definitions_match(actual[name], expected_definition)
            for name, expected_definition in expected.items()
        )

    @staticmethod
    def _definitions_match(actual: tuple[str, str], expected: tuple[str, str]) -> bool:
        return (
            actual[0] == expected[0]
            and type(actual[1]) is str
            and _canonical_schema_sql(actual[1]) == _canonical_schema_sql(expected[1])
        )

    @classmethod
    def _require_zero_sequences(
        cls, conn: sqlite3.Connection, table_names: tuple[str, ...] | None
    ) -> None:
        if cls._schema_object(conn, "sqlite_sequence") is None:
            return
        if table_names is None:
            row = conn.execute(
                "SELECT name, seq FROM sqlite_sequence LIMIT 1"
            ).fetchone()
        else:
            placeholders = ",".join("?" for _ in table_names)
            row = conn.execute(
                f"""
                SELECT name, seq FROM sqlite_sequence
                WHERE name IN ({placeholders})
                LIMIT 1
                """,
                table_names,
            ).fetchone()
        if row is not None:
            raise MigrationBlocked(
                f"{row['name']} was previously used according to sqlite_sequence"
            )

    @classmethod
    def _validate_v4_evidence_rows(cls, conn: sqlite3.Connection) -> None:
        for table in (
            "withdrawals",
            "orders_v2_archive",
            "orders_v3_archive",
            "transfers_v3_archive",
            "audit_events_v3_archive",
        ):
            if (
                cls._schema_object(conn, table) is not None
                and conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] != 0
            ):
                raise MigrationBlocked(
                    f"v4 migration evidence table {table} must remain empty"
                )

        if cls._schema_object(conn, "schema_meta_v3_archive") is not None:
            rows = conn.execute(
                "SELECT id, version FROM schema_meta_v3_archive"
            ).fetchall()
            if len(rows) != 1 or tuple(rows[0]) != (1, 3):
                raise MigrationBlocked(
                    "schema_meta_v3_archive must retain exactly the (1, 3) evidence row"
                )

    @staticmethod
    def _schema_object(conn: sqlite3.Connection, name: str) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT type, name, sql FROM sqlite_master WHERE name = ?", (name,)
        ).fetchone()

    @staticmethod
    def _read_schema_version(conn: sqlite3.Connection) -> int:
        try:
            rows = conn.execute("SELECT id, version FROM schema_meta").fetchall()
        except sqlite3.DatabaseError as exc:
            raise MigrationBlocked("schema_meta cannot be read safely") from exc
        if len(rows) != 1 or rows[0]["id"] != 1 or type(rows[0]["version"]) is not int:
            raise MigrationBlocked("schema_meta must contain one integer version row")
        return rows[0]["version"]

    @staticmethod
    def _read_schema_origin(conn: sqlite3.Connection) -> str:
        try:
            rows = conn.execute("SELECT id, origin FROM schema_meta").fetchall()
        except sqlite3.DatabaseError as exc:
            raise MigrationBlocked("schema_meta origin cannot be read safely") from exc
        if len(rows) != 1 or rows[0]["id"] != 1 or type(rows[0]["origin"]) is not str:
            raise MigrationBlocked(
                "schema_meta must contain one text migration-origin row"
            )
        return rows[0]["origin"]

    @classmethod
    def _validate_compatible_users(cls, conn: sqlite3.Connection) -> None:
        columns = tuple(
            (row["name"], row["type"].upper())
            for row in conn.execute("PRAGMA table_info(users)")
        )
        expected = (
            ("user_id", "INTEGER"),
            ("username", "TEXT"),
            ("wallet_addr", "TEXT"),
            ("created_at", "INTEGER"),
            ("updated_at", "INTEGER"),
        )
        if columns != expected:
            raise MigrationBlocked("prototype users table has an incompatible shape")
        for row in conn.execute(
            "SELECT user_id, username, wallet_addr, created_at, updated_at FROM users"
        ):
            try:
                cls._require_integer(row["user_id"], "user ID")
                cls._bounded_text(row["username"], "username", 128)
                cls._optional_bounded_text(row["wallet_addr"], "wallet address", 128)
                cls._require_integer(row["created_at"], "creation time")
                cls._require_integer(row["updated_at"], "update time")
            except ValueError as exc:
                raise MigrationBlocked(
                    "prototype user records contain incompatible identity or timestamps"
                ) from exc

    @classmethod
    def _rebuild_users(cls, conn: sqlite3.Connection) -> None:
        if cls._schema_object(conn, "users_v4_migration") is not None:
            raise MigrationBlocked("temporary v4 users table already exists")
        conn.execute("ALTER TABLE users RENAME TO users_v4_migration")
        conn.execute(_EXPECTED_V4_OBJECTS["users"][1])
        conn.execute(
            """
            INSERT INTO users(user_id, username, wallet_addr, created_at, updated_at)
            SELECT user_id, username, wallet_addr, created_at, updated_at
            FROM users_v4_migration
            """
        )
        conn.execute("DROP TABLE users_v4_migration")

    @staticmethod
    def _restore_journal_mode(conn: sqlite3.Connection, mode: str) -> None:
        allowed = {"delete", "truncate", "persist", "memory", "off", "wal"}
        if mode not in allowed:
            raise RuntimeError(f"cannot restore unknown SQLite journal mode {mode!r}")
        restored = str(
            conn.execute(f"PRAGMA journal_mode={mode}").fetchone()[0]
        ).lower()
        if restored != mode:
            raise RuntimeError(
                f"could not restore SQLite journal mode from WAL to {mode}"
            )

    def _migration_checkpoint(self, phase: str) -> None:
        """Fault-injection seam used to prove transactional migration rollback."""

    @staticmethod
    def _require_integer(value: object, label: str) -> None:
        if type(value) is not int:
            raise ValueError(f"{label} must be an integer")

    @classmethod
    def _require_optional_integer(cls, value: object, label: str) -> None:
        if value is not None:
            cls._require_integer(value, label)

    @staticmethod
    def _bounded_text(value: object, label: str, max_bytes: int) -> str:
        if type(value) is not str:
            raise ValueError(f"{label} must be non-empty text")
        value = value.strip()
        if not value or "\x00" in value or len(value.encode("utf-8")) > max_bytes:
            raise ValueError(f"{label} must be 1-{max_bytes} bytes without NUL")
        return value

    @staticmethod
    def _private_reason(value: object) -> str:
        if type(value) is not str:
            raise ValueError("reason must be 10-500 characters")
        value = value.strip()
        if not 10 <= len(value) <= 500 or any(
            unicodedata.category(character) == "Cc" for character in value
        ):
            raise ValueError(
                "reason must be 10-500 characters without control characters"
            )
        return value

    @classmethod
    def _optional_bounded_text(
        cls, value: object, label: str, max_bytes: int
    ) -> str | None:
        if value is None:
            return None
        return cls._bounded_text(value, label, max_bytes)

    @classmethod
    def _machine_text(
        cls, value: object, label: str, max_bytes: int, pattern: re.Pattern[str]
    ) -> str:
        value = cls._bounded_text(value, label, max_bytes)
        if not pattern.fullmatch(value):
            raise ValueError(f"{label} contains invalid characters")
        return value

    @classmethod
    def _optional_machine_text(
        cls,
        value: object,
        label: str,
        max_bytes: int,
        pattern: re.Pattern[str],
    ) -> str | None:
        if value is None:
            return None
        return cls._machine_text(value, label, max_bytes, pattern)

    @staticmethod
    def _optional_state_value(value: OrderState | None) -> str | None:
        if value is None:
            return None
        if type(value) is not OrderState:
            raise ValueError("audit state must be a valid order state")
        return value.value


def _run_module_cli() -> int:
    db_path = os.environ.get("DB_PATH")
    if db_path is None or not db_path.strip() or "\x00" in db_path:
        print(
            json.dumps({"error": "DB_PATH is required"}, separators=(",", ":")),
            file=sys.stderr,
        )
        return 2
    try:
        store = Store(db_path)
        store.initialize()
        integrity = store.integrity_check()
        if integrity != "ok":
            raise RuntimeError("database integrity check failed")
    except Exception as exc:
        print(
            json.dumps({"error": type(exc).__name__}, separators=(",", ":")),
            file=sys.stderr,
        )
        return 1
    print(
        json.dumps(
            {"integrity": integrity, "schema_version": SCHEMA_VERSION},
            sort_keys=True,
            separators=(",", ":"),
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(_run_module_cli())
