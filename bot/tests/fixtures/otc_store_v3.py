from __future__ import annotations

import json
import re
import sqlite3
from collections.abc import Mapping
from pathlib import Path
from typing import Any

from bot.otc.domain import (
    FeeQuote,
    OrderSide,
    OrderState,
    parse_asset,
    parse_method,
)

SCHEMA_VERSION = 3


class MigrationBlocked(RuntimeError):
    """Raised when automatic migration could change live order funds."""


class UnsupportedSchemaVersion(RuntimeError):
    """Raised when the database was created by a newer application version."""


_CREATE_USERS_STATEMENT = """
CREATE TABLE IF NOT EXISTS users (
  user_id INTEGER PRIMARY KEY,
  username TEXT NOT NULL,
  wallet_addr TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)
"""


_SCHEMA_STATEMENTS = (
    """
    CREATE TABLE IF NOT EXISTS schema_meta (
      id INTEGER PRIMARY KEY CHECK(id = 1),
      version INTEGER NOT NULL
    )
    """,
    _CREATE_USERS_STATEMENT,
    """
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
    )
    """,
    """
    CREATE TABLE IF NOT EXISTS transfers (
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
    )
    """,
    "CREATE INDEX IF NOT EXISTS orders_by_state ON orders(state, updated_at)",
    "CREATE INDEX IF NOT EXISTS orders_by_deposit ON orders(deposit_addr)",
    """
    CREATE UNIQUE INDEX IF NOT EXISTS one_active_order_transfer
    ON transfers(order_id)
    WHERE state IN ('reserved','broadcast','uncertain')
    """,
    """
    CREATE TABLE IF NOT EXISTS audit_events (
      event_id INTEGER PRIMARY KEY AUTOINCREMENT,
      order_id INTEGER,
      actor_id INTEGER,
      event_type TEXT NOT NULL,
      old_state TEXT,
      new_state TEXT,
      detail_json TEXT NOT NULL DEFAULT '{}',
      created_at INTEGER NOT NULL
    )
    """,
)

_REQUIRED_SCHEMA_OBJECTS = {
    "schema_meta": ("table", _SCHEMA_STATEMENTS[0]),
    "users": ("table", _SCHEMA_STATEMENTS[1]),
    "orders": ("table", _SCHEMA_STATEMENTS[2]),
    "transfers": ("table", _SCHEMA_STATEMENTS[3]),
    "orders_by_state": ("index", _SCHEMA_STATEMENTS[4]),
    "orders_by_deposit": ("index", _SCHEMA_STATEMENTS[5]),
    "one_active_order_transfer": ("index", _SCHEMA_STATEMENTS[6]),
    "audit_events": ("table", _SCHEMA_STATEMENTS[7]),
}

_PROTOTYPE_USERS_INFO = (
    ("user_id", "INTEGER", 0, None, 1),
    ("username", "TEXT", 0, None, 0),
    ("wallet_addr", "TEXT", 0, None, 0),
    ("created_at", "INTEGER", 0, None, 0),
    ("updated_at", "INTEGER", 0, None, 0),
)


class Store:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)

    def connect(self) -> sqlite3.Connection:
        return self._connect(apply_wal=True)

    def _connect(self, *, apply_wal: bool) -> sqlite3.Connection:
        conn = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        try:
            conn.row_factory = sqlite3.Row
            conn.execute("PRAGMA busy_timeout=30000")
            conn.execute("PRAGMA foreign_keys=ON")
            if apply_wal:
                conn.execute("PRAGMA journal_mode=WAL")
        except BaseException:
            conn.close()
            raise
        return conn

    def initialize(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        conn = self._connect(apply_wal=False)
        try:
            conn.execute("BEGIN IMMEDIATE")
            self._preflight_initialization(conn)
            conn.execute("ROLLBACK")

            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("BEGIN IMMEDIATE")
            migrate_orders, migrate_users = self._preflight_initialization(conn)

            if migrate_orders:
                conn.execute("ALTER TABLE orders RENAME TO orders_v2_archive")
            if migrate_users:
                self._migrate_prototype_users(conn)

            for statement in _SCHEMA_STATEMENTS:
                conn.execute(statement)
            self._validate_complete_schema(conn)
            conn.execute(
                """
                INSERT INTO schema_meta(id, version) VALUES(1, ?)
                ON CONFLICT(id) DO UPDATE SET version=excluded.version
                """,
                (SCHEMA_VERSION,),
            )
            conn.execute("COMMIT")
        except BaseException:
            if conn.in_transaction:
                conn.execute("ROLLBACK")
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
        deposit_confirmed_units: int = 0,
        deposit_addr: str | None = None,
        buyer_confirmed: int = 0,
        seller_confirmed: int = 0,
        deposit_deadline: int | None = None,
        matched_at: int | None = None,
        trade_deadline: int | None = None,
        disputed_at: int | None = None,
        completed_at: int | None = None,
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
        self._require_integer(deposit_confirmed_units, "confirmed deposit units")
        if deposit_confirmed_units < 0:
            raise ValueError("confirmed deposit units must be non-negative")
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
        ):
            self._require_optional_integer(value, label)

        maker_name = self._require_text(maker_name, "maker name")
        total_price = self._require_text(total_price, "total price")
        if type(settlement_asset) is not str:
            raise ValueError("settlement asset must be text")
        settlement_asset = parse_asset(settlement_asset)
        if settlement_network is not None:
            settlement_network = self._require_text(
                settlement_network, "settlement network"
            )
        if type(payment_method) is not str:
            raise ValueError("payment method must be text")
        payment_method = parse_method(payment_method)
        buyer_name = self._optional_text(buyer_name, "buyer name")
        seller_name = self._optional_text(seller_name, "seller name")
        deposit_addr = self._optional_text(deposit_addr, "deposit address")

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
            cursor = conn.execute(
                """
                INSERT INTO orders (
                  side, maker_id, maker_name, buyer_id, buyer_name,
                  seller_id, seller_name, net_amount_units, network_fee_units,
                  service_fee_units, deposit_required_units,
                  deposit_confirmed_units, total_price, settlement_asset,
                  settlement_network, payment_method, state, deposit_addr,
                  buyer_confirmed, seller_confirmed, deposit_deadline, matched_at,
                  trade_deadline, disputed_at, completed_at, created_at, updated_at
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
                    deposit_confirmed_units,
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
                    created_at,
                    updated_at,
                ),
            )
            if cursor.lastrowid is None:
                raise RuntimeError("database did not return the new order ID")
            return cursor.lastrowid
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
        event_type = self._require_text(event_type, "audit event type")
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

        conn = self.connect()
        try:
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
            return cursor.lastrowid
        finally:
            conn.close()

    @staticmethod
    def _table_exists(conn: sqlite3.Connection, table: str) -> bool:
        return (
            conn.execute(
                """
                SELECT 1 FROM sqlite_master
                WHERE type = 'table' AND name = ?
                """,
                (table,),
            ).fetchone()
            is not None
        )

    @staticmethod
    def _schema_object(
        conn: sqlite3.Connection, name: str
    ) -> sqlite3.Row | None:
        return conn.execute(
            "SELECT type, name, sql FROM sqlite_master WHERE name = ?", (name,)
        ).fetchone()

    @staticmethod
    def _canonical_schema_sql(sql: str) -> str:
        normalized = re.sub(r"\s+", " ", sql.strip().removesuffix(";").strip())
        normalized = re.sub(r"\s*([(),])\s*", r"\1", normalized)
        normalized = normalized.replace(
            "CREATE TABLE IF NOT EXISTS ", "CREATE TABLE ", 1
        )
        return normalized.replace(
            "CREATE INDEX IF NOT EXISTS ", "CREATE INDEX ", 1
        ).replace(
            "CREATE UNIQUE INDEX IF NOT EXISTS ", "CREATE UNIQUE INDEX ", 1
        )

    @classmethod
    def _required_object_matches(
        cls, row: sqlite3.Row, name: str
    ) -> bool:
        expected_type, expected_sql = _REQUIRED_SCHEMA_OBJECTS[name]
        actual_sql = row["sql"]
        return (
            row["type"] == expected_type
            and type(actual_sql) is str
            and cls._canonical_schema_sql(actual_sql)
            == cls._canonical_schema_sql(expected_sql)
        )

    @classmethod
    def _validate_existing_required_object(
        cls, conn: sqlite3.Connection, name: str
    ) -> None:
        row = cls._schema_object(conn, name)
        if row is None:
            return
        if not cls._required_object_matches(row, name):
            expected_type = _REQUIRED_SCHEMA_OBJECTS[name][0]
            raise MigrationBlocked(
                f"cannot initialize database because {name} {expected_type} does "
                "not match the required v3 definition"
            )

    @classmethod
    def _preflight_initialization(
        cls, conn: sqlite3.Connection
    ) -> tuple[bool, bool]:
        cls._validate_existing_required_object(conn, "schema_meta")
        version = cls._existing_schema_version(conn)
        if version is not None and version > SCHEMA_VERSION:
            raise UnsupportedSchemaVersion(
                f"database schema version {version} is newer than supported "
                f"version {SCHEMA_VERSION}"
            )
        return cls._preflight_schema(conn)

    @classmethod
    def _preflight_schema(cls, conn: sqlite3.Connection) -> tuple[bool, bool]:
        migrate_orders = cls._preflight_orders(conn)
        migrate_users = cls._preflight_users(conn)
        for name in (
            "transfers",
            "audit_events",
            "orders_by_state",
            "orders_by_deposit",
            "one_active_order_transfer",
        ):
            cls._validate_existing_required_object(conn, name)
        return migrate_orders, migrate_users

    @classmethod
    def _preflight_orders(cls, conn: sqlite3.Connection) -> bool:
        row = cls._schema_object(conn, "orders")
        if row is None:
            return False
        if cls._required_object_matches(row, "orders"):
            return False
        if row["type"] != "table":
            cls._validate_existing_required_object(conn, "orders")
        columns = {item["name"] for item in conn.execute("PRAGMA table_info(orders)")}
        if "side" in columns:
            cls._validate_existing_required_object(conn, "orders")

        order_count = conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0]
        if order_count:
            raise MigrationBlocked(
                "cannot migrate the prototype database because it contains existing "
                "orders; migrate those orders explicitly"
            )
        if cls._schema_object(conn, "orders_v2_archive") is not None:
            raise MigrationBlocked(
                "cannot migrate the prototype database because the migration archive "
                "already exists"
            )
        return True

    @classmethod
    def _preflight_users(cls, conn: sqlite3.Connection) -> bool:
        row = cls._schema_object(conn, "users")
        if row is None:
            return False
        if cls._required_object_matches(row, "users"):
            return False
        if row["type"] != "table" or not cls._has_prototype_users_shape(conn):
            cls._validate_existing_required_object(conn, "users")
        if cls._schema_object(conn, "users_v2_migration") is not None:
            raise MigrationBlocked(
                "cannot migrate the prototype database because a temporary users "
                "table already exists"
            )
        invalid_user = conn.execute(
            """
            SELECT user_id FROM users
            WHERE username IS NULL OR created_at IS NULL OR updated_at IS NULL
            LIMIT 1
            """
        ).fetchone()
        if invalid_user is not None:
            raise MigrationBlocked(
                "cannot migrate the prototype database because user records are "
                "missing a username or timestamp"
            )
        return True

    @staticmethod
    def _has_prototype_users_shape(conn: sqlite3.Connection) -> bool:
        actual = tuple(
            (
                row["name"],
                row["type"].upper(),
                row["notnull"],
                row["dflt_value"],
                row["pk"],
            )
            for row in conn.execute("PRAGMA table_info(users)")
        )
        return actual == _PROTOTYPE_USERS_INFO

    @classmethod
    def _validate_complete_schema(cls, conn: sqlite3.Connection) -> None:
        for name, (expected_type, _) in _REQUIRED_SCHEMA_OBJECTS.items():
            row = cls._schema_object(conn, name)
            if row is None:
                raise MigrationBlocked(
                    f"cannot initialize database because required v3 {expected_type} "
                    f"{name} is missing"
                )
            if not cls._required_object_matches(row, name):
                raise MigrationBlocked(
                    f"cannot initialize database because {name} {expected_type} does "
                    "not match the required v3 definition"
                )

    @classmethod
    def _existing_schema_version(cls, conn: sqlite3.Connection) -> int | None:
        if not cls._table_exists(conn, "schema_meta"):
            return None
        row = conn.execute(
            "SELECT version FROM schema_meta WHERE id = ?", (1,)
        ).fetchone()
        if row is None:
            return None
        version = row[0]
        if type(version) is not int:
            raise RuntimeError("database schema version is not an integer")
        return version

    @classmethod
    def _migrate_prototype_users(cls, conn: sqlite3.Connection) -> None:
        if not cls._table_exists(conn, "users"):
            return
        columns = {row["name"] for row in conn.execute("PRAGMA table_info(users)")}
        required_columns = {
            "user_id",
            "username",
            "wallet_addr",
            "created_at",
            "updated_at",
        }
        if not required_columns <= columns:
            raise MigrationBlocked(
                "cannot migrate the prototype database because its users table is "
                "missing required fields"
            )
        if cls._table_exists(conn, "users_v2_migration"):
            raise MigrationBlocked(
                "cannot migrate the prototype database because a temporary users "
                "table already exists"
            )
        invalid_user = conn.execute(
            """
            SELECT user_id FROM users
            WHERE username IS NULL OR created_at IS NULL OR updated_at IS NULL
            LIMIT 1
            """
        ).fetchone()
        if invalid_user is not None:
            raise MigrationBlocked(
                "cannot migrate the prototype database because user records are "
                "missing a username or timestamp"
            )

        conn.execute("ALTER TABLE users RENAME TO users_v2_migration")
        conn.execute(_CREATE_USERS_STATEMENT)
        conn.execute(
            """
            INSERT INTO users (
              user_id, username, wallet_addr, created_at, updated_at
            )
            SELECT user_id, username, wallet_addr, created_at, updated_at
            FROM users_v2_migration
            """
        )
        conn.execute("DROP TABLE users_v2_migration")

    @staticmethod
    def _require_integer(value: object, label: str) -> None:
        if type(value) is not int:
            raise ValueError(f"{label} must be an integer")

    @classmethod
    def _require_optional_integer(cls, value: object, label: str) -> None:
        if value is not None:
            cls._require_integer(value, label)

    @staticmethod
    def _require_text(value: object, label: str) -> str:
        if type(value) is not str or not value.strip():
            raise ValueError(f"{label} must be non-empty text")
        return value.strip()

    @classmethod
    def _optional_text(cls, value: object, label: str) -> str | None:
        if value is None:
            return None
        return cls._require_text(value, label)

    @staticmethod
    def _optional_state_value(value: OrderState | None) -> str | None:
        if value is None:
            return None
        if type(value) is not OrderState:
            raise ValueError("audit state must be a valid order state")
        return value.value
