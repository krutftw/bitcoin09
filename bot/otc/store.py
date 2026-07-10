from __future__ import annotations

import json
import re
import sqlite3
from collections.abc import Mapping
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

_LIVE_PROTOTYPE_INDEXES = {
    "idx_orders_status": "CREATE INDEX idx_orders_status ON orders(status)",
    "idx_orders_deposit_addr": (
        "CREATE INDEX idx_orders_deposit_addr ON orders(deposit_addr)"
    ),
    "idx_orders_seller": "CREATE INDEX idx_orders_seller ON orders(seller_id)",
    "idx_orders_buyer": "CREATE INDEX idx_orders_buyer ON orders(buyer_id)",
}

_LEGACY_WITHDRAWALS_SQL = """
CREATE TABLE withdrawals (
  withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id INTEGER NOT NULL,
  amount TEXT NOT NULL,
  address TEXT NOT NULL,
  txid TEXT,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL
)
"""


def _canonical_schema_sql(sql: str) -> str:
    normalized = re.sub(r"\s+", " ", sql.strip().removesuffix(";").strip())
    return re.sub(r"\s*([(),])\s*", r"\1", normalized)


def _catalog_from_script(script: str) -> dict[str, tuple[str, str]]:
    conn = sqlite3.connect(":memory:")
    try:
        conn.execute("PRAGMA foreign_keys=ON")
        conn.execute("PRAGMA recursive_triggers=ON")
        conn.executescript(script)
        return {
            row[1]: (row[0], row[2])
            for row in conn.execute(
                """
                SELECT type, name, sql FROM sqlite_master
                WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
                """
            )
        }
    finally:
        conn.close()


_EXPECTED_V4_OBJECTS = _catalog_from_script(_SCHEMA_SOURCE)
_EXPECTED_V3_OBJECTS = _catalog_from_script(_V3_SCHEMA_SOURCE)
_V3_INDEX_NAMES = (
    "orders_by_state",
    "orders_by_deposit",
    "one_active_order_transfer",
)


@dataclass(frozen=True)
class _MigrationPlan:
    kind: str
    migrate_users: bool = False
    migrate_orders: bool = False


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
            self._apply_migration(conn, plan)
            self._migration_checkpoint("archives")

            for statement in _V4_SCHEMA_STATEMENTS:
                conn.execute(statement)
            self._migration_checkpoint("schema")
            self._validate_catalog(conn, _EXPECTED_V4_OBJECTS, "v4")

            foreign_key_failures = conn.execute("PRAGMA foreign_key_check").fetchall()
            if foreign_key_failures:
                raise MigrationBlocked("v4 migration failed foreign-key validation")
            self._migration_checkpoint("foreign_keys")

            conn.execute(
                """
                INSERT INTO schema_meta(id, version) VALUES(1, ?)
                ON CONFLICT(id) DO UPDATE SET version=excluded.version
                """,
                (SCHEMA_VERSION,),
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
        for value, label in ((created_at, "creation time"), (updated_at, "update time")):
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
        deposit_addr = self._optional_bounded_text(
            deposit_addr, "deposit address", 128
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
            conn.execute(
                """
                INSERT INTO users(user_id, username, wallet_addr, created_at, updated_at)
                VALUES(?, ?, NULL, ?, ?)
                ON CONFLICT(user_id) DO NOTHING
                """,
                (maker_id, maker_name, created_at, updated_at),
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
            return int(cursor.lastrowid)
        finally:
            conn.close()

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
                cls._validate_catalog(conn, _EXPECTED_V4_OBJECTS, "v4")
                return _MigrationPlan("v4")
            if version == 3:
                cls._validate_v3_migration(conn)
                return _MigrationPlan("v3", migrate_users=True)
            raise MigrationBlocked(
                f"database schema version {version} has no safe automatic migration"
            )
        return cls._preflight_unversioned(conn)

    @classmethod
    def _preflight_unversioned(cls, conn: sqlite3.Connection) -> _MigrationPlan:
        objects = {
            row["name"]: row
            for row in conn.execute(
                """
                SELECT type, name, sql FROM sqlite_master
                WHERE name NOT LIKE 'sqlite_%'
                """
            )
        }
        allowed = {
            "users",
            "orders",
            "withdrawals",
            *_LIVE_PROTOTYPE_INDEXES,
        }
        unexpected = sorted(set(objects) - allowed)
        if unexpected:
            raise MigrationBlocked(
                "unversioned database has an unsupported catalog object: "
                + unexpected[0]
            )

        migrate_orders = "orders" in objects
        if migrate_orders:
            if objects["orders"]["type"] != "table":
                raise MigrationBlocked("prototype orders object is not a table")
            columns = {row["name"] for row in conn.execute("PRAGMA table_info(orders)")}
            if "side" in columns:
                raise MigrationBlocked(
                    "unversioned side-aware orders table cannot be certified as v4"
                )
            if conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0] != 0:
                raise MigrationBlocked(
                    "cannot migrate the prototype database because it contains "
                    "existing orders"
                )
            if cls._schema_object(conn, "orders_v2_archive") is not None:
                raise MigrationBlocked("prototype order archive already exists")

        for name, expected_sql in _LIVE_PROTOTYPE_INDEXES.items():
            row = objects.get(name)
            if row is not None and (
                row["type"] != "index"
                or type(row["sql"]) is not str
                or _canonical_schema_sql(row["sql"])
                != _canonical_schema_sql(expected_sql)
            ):
                raise MigrationBlocked(f"prototype index {name} is malformed")

        migrate_users = "users" in objects
        if migrate_users:
            cls._validate_compatible_users(conn)
        if "withdrawals" in objects:
            if objects["withdrawals"]["type"] != "table":
                raise MigrationBlocked("prototype withdrawals object is not a table")
            if (
                type(objects["withdrawals"]["sql"]) is not str
                or _canonical_schema_sql(objects["withdrawals"]["sql"])
                != _canonical_schema_sql(_LEGACY_WITHDRAWALS_SQL)
            ):
                raise MigrationBlocked(
                    "legacy withdrawals table does not match the trusted definition"
                )
            if conn.execute("SELECT COUNT(*) FROM withdrawals").fetchone()[0] != 0:
                raise MigrationBlocked(
                    "legacy withdrawals have no trustworthy opening revenue provenance"
                )
        return _MigrationPlan(
            "prototype" if objects else "fresh",
            migrate_users=migrate_users,
            migrate_orders=migrate_orders,
        )

    @classmethod
    def _validate_v3_migration(cls, conn: sqlite3.Connection) -> None:
        cls._validate_catalog(conn, _EXPECTED_V3_OBJECTS, "v3")
        unexpected_financial_objects = conn.execute(
            """
            SELECT name FROM sqlite_master
            WHERE name NOT LIKE 'sqlite_%'
              AND type IN ('index','trigger')
              AND tbl_name IN ('users','orders','transfers','audit_events')
              AND name NOT IN (
                'orders_by_state','orders_by_deposit','one_active_order_transfer'
              )
            LIMIT 1
            """
        ).fetchone()
        if unexpected_financial_objects is not None:
            raise MigrationBlocked(
                "non-exact v3 financial catalog contains "
                f"{unexpected_financial_objects['name']}"
            )
        for archive_name in (
            "orders_v3_archive",
            "transfers_v3_archive",
            "audit_events_v3_archive",
            "schema_meta_v3_archive",
        ):
            if cls._schema_object(conn, archive_name) is not None:
                raise MigrationBlocked(f"v3 migration archive {archive_name} exists")
        for table in ("orders", "transfers", "audit_events"):
            if conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] != 0:
                raise MigrationBlocked(
                    f"cannot migrate v3 because {table} contains financial records"
                )
        for table in ("deposit_scans", "deposit_credits", "transfer_credit_allocations"):
            if cls._schema_object(conn, table) is not None:
                raise MigrationBlocked(
                    f"non-exact v3 financial catalog contains {table}"
                )
        withdrawals = cls._schema_object(conn, "withdrawals")
        if withdrawals is not None:
            if (
                withdrawals["type"] != "table"
                or type(withdrawals["sql"]) is not str
                or _canonical_schema_sql(withdrawals["sql"])
                != _canonical_schema_sql(_LEGACY_WITHDRAWALS_SQL)
            ):
                raise MigrationBlocked(
                    "legacy withdrawals table does not match the trusted definition"
                )
            if conn.execute("SELECT COUNT(*) FROM withdrawals").fetchone()[0] != 0:
                raise MigrationBlocked(
                    "legacy withdrawals have no trustworthy opening revenue provenance"
                )
        archive = cls._schema_object(conn, "orders_v2_archive")
        if archive is not None and (
            archive["type"] != "table"
            or conn.execute("SELECT COUNT(*) FROM orders_v2_archive").fetchone()[0]
            != 0
        ):
            raise MigrationBlocked("v3 prototype archive must remain empty")
        cls._validate_compatible_users(conn)

    @classmethod
    def _validate_catalog(
        cls,
        conn: sqlite3.Connection,
        expected: Mapping[str, tuple[str, str]],
        label: str,
    ) -> None:
        for name, (expected_type, expected_sql) in expected.items():
            row = cls._schema_object(conn, name)
            if row is None:
                raise MigrationBlocked(
                    f"required {label} {expected_type} {name} is missing"
                )
            if (
                row["type"] != expected_type
                or type(row["sql"]) is not str
                or _canonical_schema_sql(row["sql"])
                != _canonical_schema_sql(expected_sql)
            ):
                raise MigrationBlocked(
                    f"{name} does not match the required {label} definition"
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
        restored = str(conn.execute(f"PRAGMA journal_mode={mode}").fetchone()[0]).lower()
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
