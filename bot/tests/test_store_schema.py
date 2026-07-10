import re
import sqlite3
import tempfile
import unittest
from contextlib import contextmanager
from inspect import Parameter, signature
from pathlib import Path

from bot.otc.domain import OrderSide, OrderState
from bot.otc.store import (
    SCHEMA_VERSION,
    MigrationBlocked,
    Store,
    UnsupportedSchemaVersion,
)


PROTOTYPE_SCHEMA = """
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
    amount TEXT NOT NULL,
    price TEXT NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
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
"""

EXPECTED_V3_SCHEMA_SQL = {
    "schema_meta": """
        CREATE TABLE schema_meta (
          id INTEGER PRIMARY KEY CHECK(id = 1),
          version INTEGER NOT NULL
        )
    """,
    "users": """
        CREATE TABLE users (
          user_id INTEGER PRIMARY KEY,
          username TEXT NOT NULL,
          wallet_addr TEXT,
          created_at INTEGER NOT NULL,
          updated_at INTEGER NOT NULL
        )
    """,
    "orders": """
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
        )
    """,
    "transfers": """
        CREATE TABLE transfers (
          transfer_id INTEGER PRIMARY KEY AUTOINCREMENT,
          order_id INTEGER REFERENCES orders(order_id),
          kind TEXT NOT NULL CHECK(kind IN (
            'release','refund','resolve_buyer','resolve_seller','fee_withdrawal','excess_refund'
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
    "orders_by_state":
        "CREATE INDEX orders_by_state ON orders(state, updated_at)",
    "orders_by_deposit":
        "CREATE INDEX orders_by_deposit ON orders(deposit_addr)",
    "one_active_order_transfer": """
        CREATE UNIQUE INDEX one_active_order_transfer
        ON transfers(order_id)
        WHERE state IN ('reserved','broadcast','uncertain')
    """,
    "audit_events": """
        CREATE TABLE audit_events (
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
}


def normalize_schema_sql(sql):
    normalized = re.sub(r"\s+", " ", sql.strip())
    return re.sub(r"\s*([(),])\s*", r"\1", normalized)


@contextmanager
def managed_connection(conn):
    try:
        with conn:
            yield conn
    finally:
        conn.close()


class StoreSchemaTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "otc.db"

    def tearDown(self):
        self.tmp.cleanup()

    def make_store(self) -> Store:
        store = Store(self.path)
        store.initialize()
        return store

    @staticmethod
    def order_values(**overrides):
        values = {
            "side": OrderSide.SELL,
            "maker_id": 7,
            "maker_name": "O'Brien",
            "net_amount_units": 500_000_000,
            "network_fee_units": 10_000,
            "service_fee_units": 2_500_000,
            "deposit_required_units": 502_510_000,
            "total_price": "250.00",
            "settlement_asset": "aud",
            "settlement_network": None,
            "payment_method": " Pay  ID ",
            "state": OrderState.AWAITING_DEPOSIT,
            "created_at": 1_700_000_000,
            "updated_at": 1_700_000_000,
        }
        values.update(overrides)
        return values

    def test_new_schema_has_required_tables_indexes_and_version(self):
        self.make_store()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            names = {
                row[0]
                for row in conn.execute(
                    "SELECT name FROM sqlite_master WHERE type='table'"
                )
            }
            self.assertTrue(
                {"users", "orders", "transfers", "audit_events", "schema_meta"}
                <= names
            )
            definitions = {
                row[0]: normalize_schema_sql(row[1])
                for row in conn.execute(
                    "SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL"
                )
                if row[0] in EXPECTED_V3_SCHEMA_SQL
            }
            self.assertEqual(
                definitions,
                {
                    name: normalize_schema_sql(sql)
                    for name, sql in EXPECTED_V3_SCHEMA_SQL.items()
                },
            )
            self.assertEqual(
                conn.execute("SELECT version FROM schema_meta").fetchone()[0],
                SCHEMA_VERSION,
            )

    def test_connection_policy_enables_foreign_keys_wal_and_busy_timeout(self):
        store = self.make_store()
        with managed_connection(store.connect()) as conn:
            self.assertEqual(conn.execute("PRAGMA foreign_keys").fetchone()[0], 1)
            self.assertEqual(conn.execute("PRAGMA journal_mode").fetchone()[0], "wal")
            self.assertEqual(conn.execute("PRAGMA busy_timeout").fetchone()[0], 30_000)
            self.assertIsInstance(
                conn.execute("SELECT version FROM schema_meta").fetchone(), sqlite3.Row
            )

    def test_empty_prototype_database_migrates_and_preserves_legacy_rows(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(PROTOTYPE_SCHEMA)
            conn.execute("INSERT INTO users VALUES (7, 'pilot', NULL, 1, 1)")
            conn.execute(
                """
                INSERT INTO withdrawals
                    (admin_id, amount, address, txid, status, created_at)
                VALUES (?, ?, ?, ?, ?, ?)
                """,
                (9, "1.25", "09c-address", None, "pending", 2),
            )

        store = Store(self.path)
        store.initialize()

        self.assertEqual(store.integrity_check(), "ok")
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT username FROM users WHERE user_id = ?", (7,)
                ).fetchone()[0],
                "pilot",
            )
            self.assertEqual(
                conn.execute(
                    "SELECT amount, address FROM withdrawals WHERE admin_id = ?", (9,)
                ).fetchone(),
                ("1.25", "09c-address"),
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM orders_v2_archive").fetchone()[0],
                0,
            )
            order_columns = {
                row[1] for row in conn.execute("PRAGMA table_info(orders)")
            }
            self.assertIn("side", order_columns)
            user_columns = {
                row[1]: row for row in conn.execute("PRAGMA table_info(users)")
            }
            for column in ("username", "created_at", "updated_at"):
                with self.subTest(column=column):
                    self.assertEqual(user_columns[column][3], 1)

    def test_nonempty_prototype_migration_is_blocked_and_fully_rolled_back(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(PROTOTYPE_SCHEMA)
            conn.execute("INSERT INTO users VALUES (7, 'pilot', NULL, 1, 1)")
            conn.execute(
                """
                INSERT INTO orders
                    (seller_id, amount, price, currency, status, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (7, "1", "100", "AUD", "open", 1, 1),
            )

        original_schema = self._schema_snapshot()
        with self.assertRaisesRegex(
            MigrationBlocked, "cannot migrate.*existing order"
        ):
            Store(self.path).initialize()

        self.assertEqual(self._schema_snapshot(), original_schema)
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT seller_id, amount, status FROM orders"
                ).fetchall(),
                [(7, "1", "open")],
            )
            self.assertEqual(
                conn.execute(
                    "SELECT username FROM users WHERE user_id = 7"
                ).fetchone()[0],
                "pilot",
            )
            self.assertIsNone(
                conn.execute(
                    "SELECT name FROM sqlite_master WHERE name = 'orders_v2_archive'"
                ).fetchone()
            )

    def test_incompatible_prototype_user_blocks_and_rolls_back(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(PROTOTYPE_SCHEMA)
            conn.execute("INSERT INTO users VALUES (7, NULL, NULL, 1, 1)")

        original_schema = self._schema_snapshot()
        with self.assertRaisesRegex(MigrationBlocked, "user records.*missing"):
            Store(self.path).initialize()

        self.assertEqual(self._schema_snapshot(), original_schema)
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT user_id, username FROM users WHERE user_id = ?", (7,)
                ).fetchone(),
                (7, None),
            )
            self.assertIsNotNone(
                conn.execute(
                    "SELECT name FROM sqlite_master WHERE name = 'orders'"
                ).fetchone()
            )
            self.assertIsNone(
                conn.execute(
                    "SELECT name FROM sqlite_master WHERE name = 'orders_v2_archive'"
                ).fetchone()
            )

    def test_initialize_is_idempotent_and_keeps_archive_as_evidence(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(PROTOTYPE_SCHEMA)

        store = Store(self.path)
        store.initialize()
        order_id = store.create_order(**self.order_values())
        store.initialize()
        store.initialize()

        self.assertEqual(store.get_order(order_id=order_id)["maker_name"], "O'Brien")
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM orders_v2_archive").fetchone()[0],
                0,
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM schema_meta").fetchone()[0], 1
            )

    def test_newer_schema_version_is_refused_without_rewriting(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(
                """
                CREATE TABLE schema_meta (
                    id INTEGER PRIMARY KEY CHECK(id = 1),
                    version INTEGER NOT NULL
                );
                INSERT INTO schema_meta VALUES (1, 4);
                CREATE TABLE future_data (value TEXT NOT NULL);
                INSERT INTO future_data VALUES ('keep me');
                """
            )

        original_schema = self._schema_snapshot()
        with self.assertRaisesRegex(
            UnsupportedSchemaVersion, "version 4.*supported version 3"
        ):
            Store(self.path).initialize()

        self.assertEqual(self._schema_snapshot(), original_schema)
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute("SELECT value FROM future_data").fetchone()[0], "keep me"
            )
            self.assertEqual(
                conn.execute("SELECT version FROM schema_meta").fetchone()[0], 4
            )

    def test_create_and_get_order_store_exact_units_and_domain_values(self):
        store = self.make_store()
        order_id = store.create_order(**self.order_values())

        row = store.get_order(order_id=order_id)
        self.assertIsNotNone(row)
        self.assertEqual(row["side"], "sell")
        self.assertEqual(row["state"], "awaiting_deposit")
        self.assertEqual(row["maker_name"], "O'Brien")
        self.assertEqual(row["seller_id"], 7)
        self.assertEqual(row["seller_name"], "O'Brien")
        self.assertIsNone(row["buyer_id"])
        self.assertEqual(row["net_amount_units"], 500_000_000)
        self.assertEqual(row["deposit_required_units"], 502_510_000)
        self.assertEqual(row["settlement_asset"], "AUD")
        self.assertEqual(row["payment_method"], "Pay ID")
        self.assertIsNone(store.get_order(order_id=order_id + 1))

    def test_buy_order_assigns_maker_as_buyer(self):
        store = self.make_store()
        order_id = store.create_order(
            **self.order_values(side=OrderSide.BUY, state=OrderState.OPEN)
        )

        row = store.get_order(order_id=order_id)
        self.assertEqual(row["buyer_id"], 7)
        self.assertEqual(row["buyer_name"], "O'Brien")
        self.assertIsNone(row["seller_id"])

    def test_order_interfaces_reject_bool_float_and_raw_enum_values(self):
        store = self.make_store()
        for field in (
            "maker_id",
            "buyer_id",
            "seller_id",
            "net_amount_units",
            "network_fee_units",
            "service_fee_units",
            "deposit_required_units",
            "deposit_confirmed_units",
            "buyer_confirmed",
            "seller_confirmed",
            "deposit_deadline",
            "matched_at",
            "trade_deadline",
            "disputed_at",
            "completed_at",
            "created_at",
            "updated_at",
        ):
            for bad in (True, 1.5):
                with self.subTest(field=field, bad=bad), self.assertRaisesRegex(
                    ValueError, "integer"
                ):
                    store.create_order(**self.order_values(**{field: bad}))

        for field, bad in (("side", "sell"), ("state", "open")):
            with self.subTest(field=field), self.assertRaisesRegex(
                ValueError, "valid order"
            ):
                store.create_order(**self.order_values(**{field: bad}))

        for bad in (True, 1.0, "1"):
            with self.subTest(order_id=bad), self.assertRaisesRegex(
                ValueError, "order ID must be an integer"
            ):
                store.get_order(order_id=bad)

    def test_public_store_arguments_are_keyword_only(self):
        for method in (Store.create_order, Store.get_order, Store.append_audit):
            parameters = list(signature(method).parameters.values())[1:]
            with self.subTest(method=method.__name__):
                self.assertTrue(parameters)
                self.assertTrue(
                    all(
                        parameter.kind is Parameter.KEYWORD_ONLY
                        for parameter in parameters
                    )
                )

    def test_append_audit_uses_deterministic_json_and_parameterized_values(self):
        store = self.make_store()
        order_id = store.create_order(**self.order_values())
        event_id = store.append_audit(
            order_id=order_id,
            actor_id=11,
            event_type="maker's deposit",
            old_state=OrderState.AWAITING_DEPOSIT,
            new_state=OrderState.OPEN,
            detail={"z": 2, "a": {"value": 1}},
            created_at=1_700_000_001,
        )

        with managed_connection(sqlite3.connect(self.path)) as conn:
            row = conn.execute(
                """
                SELECT event_type, old_state, new_state, detail_json
                FROM audit_events WHERE event_id = ?
                """,
                (event_id,),
            ).fetchone()
        self.assertEqual(
            row,
            (
                "maker's deposit",
                "awaiting_deposit",
                "open",
                '{"a":{"value":1},"z":2}',
            ),
        )

    def test_append_audit_defaults_optional_context_and_detail(self):
        store = self.make_store()
        event_id = store.append_audit(event_type="startup", created_at=10)

        with managed_connection(sqlite3.connect(self.path)) as conn:
            row = conn.execute(
                """
                SELECT order_id, actor_id, old_state, new_state, detail_json
                FROM audit_events WHERE event_id = ?
                """,
                (event_id,),
            ).fetchone()
        self.assertEqual(row, (None, None, None, None, "{}"))

    def test_append_audit_rejects_coerced_integers_and_raw_states(self):
        store = self.make_store()
        valid = {
            "order_id": None,
            "actor_id": None,
            "event_type": "startup",
            "old_state": None,
            "new_state": None,
            "detail": {},
            "created_at": 10,
        }
        for field in ("order_id", "actor_id", "created_at"):
            for bad in (True, 1.5):
                with self.subTest(field=field, bad=bad), self.assertRaisesRegex(
                    ValueError, "integer"
                ):
                    store.append_audit(**(valid | {field: bad}))

        with self.assertRaisesRegex(ValueError, "valid order state"):
            store.append_audit(**(valid | {"new_state": "open"}))

    def test_database_constraints_reject_invalid_values_and_active_duplicates(self):
        store = self.make_store()
        order_id = store.create_order(**self.order_values())
        with managed_connection(store.connect()) as conn:
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    "UPDATE orders SET buyer_confirmed = ? WHERE order_id = ?",
                    (2, order_id),
                )
            conn.execute(
                """
                INSERT INTO transfers (
                    order_id, kind, state, amount_units, network_fee_units,
                    destination, created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (order_id, "release", "reserved", 1, 0, "destination-1", 1, 1),
            )
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    INSERT INTO transfers (
                        order_id, kind, state, amount_units, network_fee_units,
                        destination, created_at, updated_at
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        order_id,
                        "refund",
                        "uncertain",
                        1,
                        0,
                        "destination-2",
                        2,
                        2,
                    ),
                )

    def _schema_snapshot(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            return conn.execute(
                """
                SELECT type, name, tbl_name, sql
                FROM sqlite_master
                WHERE name NOT LIKE 'sqlite_%'
                ORDER BY type, name
                """
            ).fetchall()


if __name__ == "__main__":
    unittest.main()
