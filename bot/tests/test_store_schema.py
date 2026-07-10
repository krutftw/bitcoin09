import json
import os
import re
import sqlite3
import subprocess
import sys
import tempfile
import threading
import types
import unittest
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from functools import lru_cache
from inspect import Parameter, signature
from pathlib import Path

from bot.otc.domain import OrderSide, OrderState
from bot.otc.store import (
    SCHEMA_VERSION,
    MigrationBlocked,
    Store,
    UnsupportedSchemaVersion,
    _EXPECTED_V4_OBJECTS,
    _V3_SCHEMA_SOURCE,
)


LIVE_PROTOTYPE_SCHEMA = """
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


@lru_cache(maxsize=1)
def committed_v3_store_class():
    repo_root = Path(__file__).resolve().parents[2]
    source = subprocess.run(
        ["git", "show", "735cec0:bot/otc/store.py"],
        cwd=repo_root,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    module = types.ModuleType("committed_otc_store_v3")
    module.__file__ = "git:735cec0:bot/otc/store.py"
    exec(compile(source, module.__file__, "exec"), module.__dict__)
    return module.Store


@contextmanager
def managed_connection(conn):
    try:
        with conn:
            yield conn
    finally:
        conn.close()


def canonical_sql(sql):
    normalized = re.sub(r"\s+", " ", sql.strip().removesuffix(";").strip())
    return re.sub(r"\s*([(),])\s*", r"\1", normalized)


class StoreSchemaTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "otc.db"

    def tearDown(self):
        self.tmp.cleanup()

    def make_store(self):
        store = Store(self.path)
        store.initialize()
        return store

    @staticmethod
    def hash(number):
        return f"{number:064x}"

    @staticmethod
    def order_values(**overrides):
        values = {
            "side": OrderSide.SELL,
            "maker_id": 7,
            "maker_name": "Seller",
            "net_amount_units": 100,
            "network_fee_units": 10,
            "service_fee_units": 7,
            "deposit_required_units": 117,
            "total_price": "250.00",
            "settlement_asset": "aud",
            "settlement_network": None,
            "payment_method": "Pay ID",
            "state": OrderState.AWAITING_DEPOSIT,
            "deposit_addr": "deposit-address-7",
            "created_at": 100,
            "updated_at": 100,
        }
        values.update(overrides)
        return values

    def create_sell(self, store=None, **overrides):
        store = store or self.make_store()
        order_id = store.create_order(**self.order_values(**overrides))
        with managed_connection(store.connect()) as conn:
            conn.execute(
                "UPDATE users SET wallet_addr='seller-wallet' WHERE user_id=7"
            )
        return store, order_id

    def add_user(self, conn, user_id, username, wallet):
        conn.execute(
            """
            INSERT INTO users(user_id, username, wallet_addr, created_at, updated_at)
            VALUES(?, ?, ?, 100, 100)
            """,
            (user_id, username, wallet),
        )

    def add_credit(
        self,
        conn,
        order_id,
        *,
        amount=117,
        main=117,
        recovery=0,
        recovery_reason=None,
        credit_number=1,
        scan_number=1,
        credited=True,
        address="deposit-address-7",
    ):
        cursor = conn.execute(
            """
            INSERT INTO deposit_scans(network, address, tip_hash, tip_height, observed_at)
            VALUES('btc09-mainnet', ?, ?, ?, ?)
            """,
            (address, self.hash(10_000 + scan_number), scan_number, 100 + scan_number),
        )
        scan_id = cursor.lastrowid
        cursor = conn.execute(
            """
            INSERT INTO deposit_credits(
              order_id, network, txid, vout, deposit_addr, amount_units,
              block_hash, block_height, confirmations, coinbase, mature,
              current_best_chain, credited_at, main_units, recovery_units,
              recovery_reason, first_seen_at, last_seen_at, last_seen_scan_id,
              last_checked_scan_id
            ) VALUES(
              ?, 'btc09-mainnet', ?, 0, ?, ?, ?, 1, 6, 0, 1, 1,
              ?, ?, ?, ?, 101, 101, ?, ?
            )
            """,
            (
                order_id,
                self.hash(20_000 + credit_number),
                address,
                amount,
                self.hash(30_000 + credit_number),
                101 if credited else None,
                main if credited else 0,
                recovery if credited else 0,
                recovery_reason,
                scan_id,
                scan_id,
            ),
        )
        return cursor.lastrowid, scan_id

    def fund_and_match(self, conn, order_id):
        credit_id, _ = self.add_credit(conn, order_id)
        conn.execute(
            "UPDATE orders SET state='open', funded_at=102, updated_at=102 WHERE order_id=?",
            (order_id,),
        )
        self.add_user(conn, 8, "Buyer", "buyer-wallet")
        conn.execute(
            """
            UPDATE orders SET buyer_id=8, buyer_name='Buyer', state='matched',
                              matched_at=103, updated_at=103
            WHERE order_id=?
            """,
            (order_id,),
        )
        return credit_id

    def queue_release(self, conn, order_id, credit_id):
        conn.execute(
            "UPDATE orders SET buyer_confirmed=1, updated_at=104 WHERE order_id=?",
            (order_id,),
        )
        cursor = conn.execute(
            """
            INSERT INTO transfers(
              operation_key, order_id, kind, is_main_outcome, state,
              amount_units, network_fee_units, earned_fee_units, destination,
              created_at, updated_at
            ) VALUES(?, ?, 'release', 1, 'queued', 100, 10, 7,
                     'buyer-wallet', 105, 105)
            """,
            (f"order:{order_id}:main", order_id),
        )
        transfer_id = cursor.lastrowid
        conn.execute(
            """
            INSERT INTO transfer_credit_allocations(
              transfer_id, credit_id, order_id, bucket, units
            ) VALUES(?, ?, ?, 'main', 117)
            """,
            (transfer_id, credit_id, order_id),
        )
        conn.execute(
            """
            UPDATE orders SET seller_confirmed=1, state='release_reserved', updated_at=106
            WHERE order_id=?
            """,
            (order_id,),
        )
        return transfer_id

    def confirm_release(self, conn, transfer_id):
        conn.execute(
            """
            UPDATE transfers SET state='reserved', attempt_count=1,
              reserved_at=106, updated_at=106 WHERE transfer_id=?
            """,
            (transfer_id,),
        )
        conn.execute(
            """
            UPDATE transfers SET state='prepared', txid=?, signed_tx_hex='aa',
              signed_at=107, prepared_tip_hash=?, prepared_tip_height=10,
              updated_at=107 WHERE transfer_id=?
            """,
            (self.hash(40_001), self.hash(40_002), transfer_id),
        )
        conn.execute(
            """
            UPDATE transfers SET state='broadcast', broadcast_at=108,
              result_class='broadcast', updated_at=108 WHERE transfer_id=?
            """,
            (transfer_id,),
        )
        conn.execute(
            """
            UPDATE transfers SET state='confirmed', confirmed_at=109,
              confirmed_block_hash=?, confirmed_block_height=11,
              confirmations=1, updated_at=109 WHERE transfer_id=?
            """,
            (self.hash(40_003), transfer_id),
        )

    def make_confirmed_release(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id = self.fund_and_match(conn, order_id)
            transfer_id = self.queue_release(conn, order_id, credit_id)
            self.confirm_release(conn, transfer_id)
        return store, transfer_id

    def queue_fee_withdrawal(self, conn, key, created_at):
        cursor = conn.execute(
            """
            INSERT INTO transfers(
              operation_key,kind,is_main_outcome,state,amount_units,
              network_fee_units,destination,created_at,updated_at
            ) VALUES(?,'fee_withdrawal',0,'queued',1,0,'fee-wallet',?,?)
            """,
            (key, created_at, created_at),
        )
        return cursor.lastrowid

    @staticmethod
    def claim_transfer(conn, transfer_id, timestamp):
        conn.execute(
            """
            UPDATE transfers SET state='reserved',attempt_count=attempt_count+1,
              reserved_at=?,updated_at=? WHERE transfer_id=?
            """,
            (timestamp, timestamp, transfer_id),
        )

    def prepare_transfer(self, conn, transfer_id, timestamp, number):
        conn.execute(
            """
            UPDATE transfers SET state='prepared',txid=?,signed_tx_hex='aa',
              signed_at=?,prepared_tip_hash=?,prepared_tip_height=10,updated_at=?
            WHERE transfer_id=?
            """,
            (
                self.hash(60_000 + number),
                timestamp,
                self.hash(61_000 + number),
                timestamp,
                transfer_id,
            ),
        )

    @staticmethod
    def broadcast_transfer(conn, transfer_id, timestamp):
        conn.execute(
            """
            UPDATE transfers SET state='broadcast',broadcast_at=?,updated_at=?
            WHERE transfer_id=?
            """,
            (timestamp, timestamp, transfer_id),
        )

    def test_new_schema_matches_exact_v4_catalog_and_connection_policy(self):
        store = self.make_store()
        self.assertEqual(SCHEMA_VERSION, 4)
        with managed_connection(store.connect()) as conn:
            self.assertEqual(conn.execute("PRAGMA foreign_keys").fetchone()[0], 1)
            self.assertEqual(conn.execute("PRAGMA recursive_triggers").fetchone()[0], 1)
            self.assertEqual(conn.execute("PRAGMA journal_mode").fetchone()[0], "wal")
            self.assertEqual(conn.execute("PRAGMA synchronous").fetchone()[0], 2)
            actual = {
                row[1]: (row[0], row[2])
                for row in conn.execute(
                    """
                    SELECT type, name, sql FROM sqlite_master
                    WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
                    """
                )
            }
            self.assertEqual(set(actual), set(_EXPECTED_V4_OBJECTS))
            for name, (object_type, sql) in _EXPECTED_V4_OBJECTS.items():
                with self.subTest(name=name):
                    self.assertEqual(actual[name][0], object_type)
                    self.assertEqual(canonical_sql(actual[name][1]), canonical_sql(sql))
            self.assertEqual(
                conn.execute("SELECT version FROM schema_meta WHERE id=1").fetchone()[0],
                4,
            )
            for table in (
                "schema_meta",
                "users",
                "orders",
                "deposit_scans",
                "deposit_credits",
                "transfers",
                "transfer_credit_allocations",
                "audit_events",
            ):
                self.assertTrue(actual[table][1].rstrip().endswith("STRICT"))
        self.assertEqual(store.integrity_check(), "ok")

    def test_initialize_is_idempotent(self):
        store = self.make_store()
        first = self.schema_snapshot()
        store.initialize()
        store.initialize()
        self.assertEqual(self.schema_snapshot(), first)

    def test_unversioned_empty_sqlite_sequence_blocks_fresh_certification(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute(
                "CREATE TABLE shadow_financial(id INTEGER PRIMARY KEY AUTOINCREMENT)"
            )
            conn.execute("INSERT INTO shadow_financial(id) VALUES(117)")
            conn.execute("DROP TABLE shadow_financial")
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM sqlite_master "
                    "WHERE type='table' AND name='sqlite_sequence'"
                ).fetchone()[0],
                1,
            )
            self.assertEqual(conn.execute("SELECT * FROM sqlite_sequence").fetchall(), [])
            before = conn.execute(
                "SELECT type,name,tbl_name,rootpage,sql FROM sqlite_master "
                "ORDER BY type,name,tbl_name,rootpage"
            ).fetchall()
            before_journal = conn.execute("PRAGMA journal_mode").fetchone()[0]

        with self.assertRaisesRegex(MigrationBlocked, "sqlite_sequence|previously"):
            Store(self.path).initialize()

        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT type,name,tbl_name,rootpage,sql FROM sqlite_master "
                    "ORDER BY type,name,tbl_name,rootpage"
                ).fetchall(),
                before,
            )
            self.assertEqual(
                conn.execute("PRAGMA journal_mode").fetchone()[0], before_journal
            )
            self.assertIsNone(
                conn.execute(
                    "SELECT 1 FROM sqlite_master WHERE name='schema_meta'"
                ).fetchone()
            )

    def test_live_empty_prototype_migrates_and_preserves_user_and_archive(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
            conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute("PRAGMA journal_mode=WAL").fetchone()[0], "wal"
            )

        Store(self.path).initialize()
        Store(self.path).initialize()

        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(conn.execute("PRAGMA journal_mode").fetchone()[0], "wal")
            self.assertEqual(
                conn.execute("SELECT * FROM users WHERE user_id=7").fetchone(),
                (7, "Pilot", None, 1, 2),
            )
            self.assertEqual(conn.execute("SELECT COUNT(*) FROM orders_v2_archive").fetchone()[0], 0)
            for index_name in (
                "idx_orders_status",
                "idx_orders_deposit_addr",
                "idx_orders_seller",
                "idx_orders_buyer",
            ):
                self.assertEqual(
                    conn.execute(
                        "SELECT tbl_name FROM sqlite_master WHERE name=?", (index_name,)
                    ).fetchone()[0],
                    "orders_v2_archive",
                )
            self.assertEqual(conn.execute("PRAGMA foreign_key_check").fetchall(), [])

    def test_nonempty_prototype_financial_tables_block_without_mutation(self):
        cases = ("orders", "withdrawals")
        for table in cases:
            with self.subTest(table=table):
                path = Path(self.tmp.name) / f"{table}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                    conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
                    if table == "orders":
                        conn.execute(
                            """
                            INSERT INTO orders(seller_id, amount, price, currency,
                              created_at, updated_at) VALUES(7, '1', '1', 'AUD', 1, 1)
                            """
                        )
                    else:
                        conn.execute(
                            """
                            INSERT INTO withdrawals(admin_id, amount, address, status,
                              created_at) VALUES(1, '1', 'address', 'pending', 1)
                            """
                        )
                before = self.database_dump(path)
                mode = self.journal_mode(path)
                with self.assertRaises(MigrationBlocked):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)
                self.assertEqual(self.journal_mode(path), mode)

    def test_exact_empty_v3_migrates_with_or_without_v2_archive(self):
        V3Store = committed_v3_store_class()
        for existing_archive in (False, True):
            with self.subTest(existing_archive=existing_archive):
                path = Path(self.tmp.name) / f"v3-{existing_archive}.db"
                if existing_archive:
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                        conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
                V3Store(path).initialize()
                if not existing_archive:
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
                Store(path).initialize()
                Store(path).initialize()
                with managed_connection(sqlite3.connect(path)) as conn:
                    self.assertEqual(conn.execute("SELECT version FROM schema_meta").fetchone()[0], 4)
                    for archive in (
                        "orders_v3_archive",
                        "transfers_v3_archive",
                        "audit_events_v3_archive",
                        "schema_meta_v3_archive",
                    ):
                        self.assertIsNotNone(
                            conn.execute(
                                "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
                                (archive,),
                            ).fetchone()
                        )
                    if existing_archive:
                        self.assertIsNotNone(
                            conn.execute(
                                "SELECT 1 FROM sqlite_master WHERE name='orders_v2_archive'"
                            ).fetchone()
                        )
                    self.assertEqual(conn.execute("SELECT username FROM users").fetchone()[0], "Pilot")
                    self.assertEqual(conn.execute("PRAGMA foreign_key_check").fetchall(), [])

    def test_exact_v3_with_empty_trusted_withdrawals_migrates_and_revalidates(self):
        V3Store = committed_v3_store_class()
        V3Store(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(
                """
                CREATE TABLE withdrawals(
                  withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
                  admin_id INTEGER NOT NULL, amount TEXT NOT NULL,
                  address TEXT NOT NULL, txid TEXT,
                  status TEXT NOT NULL, created_at INTEGER NOT NULL
                );
                """
            )
        Store(self.path).initialize()
        Store(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(conn.execute("SELECT version FROM schema_meta").fetchone()[0], 4)
            self.assertEqual(conn.execute("SELECT COUNT(*) FROM withdrawals").fetchone()[0], 0)

    def test_v3_nonempty_or_nonexact_financial_catalog_blocks(self):
        cases = ("orders", "transfers", "audit_events", "deposit_credits", "withdrawals")
        for case in cases:
            with self.subTest(case=case):
                path = Path(self.tmp.name) / f"blocked-{case}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(_V3_SCHEMA_SOURCE)
                    if case == "orders":
                        conn.execute(
                            """
                            INSERT INTO orders(
                              side,maker_id,maker_name,seller_id,seller_name,
                              net_amount_units,network_fee_units,service_fee_units,
                              deposit_required_units,total_price,settlement_asset,
                              payment_method,state,created_at,updated_at
                            ) VALUES('sell',7,'s',7,'s',1,0,0,1,'1','AUD','cash',
                                     'awaiting_deposit',1,1)
                            """
                        )
                    elif case == "transfers":
                        conn.execute(
                            """
                            INSERT INTO transfers(kind,state,amount_units,network_fee_units,
                              destination,created_at,updated_at)
                            VALUES('fee_withdrawal','reserved',1,0,'x',1,1)
                            """
                        )
                    elif case == "audit_events":
                        conn.execute("INSERT INTO audit_events(event_type,created_at) VALUES('x',1)")
                    elif case == "deposit_credits":
                        conn.execute("CREATE TABLE deposit_credits(credit_id INTEGER)")
                    else:
                        conn.execute("CREATE TABLE withdrawals(value TEXT)")
                        conn.execute("INSERT INTO withdrawals VALUES('x')")
                before = self.database_dump(path)
                mode = self.journal_mode(path)
                with self.assertRaises(MigrationBlocked):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)
                self.assertEqual(self.journal_mode(path), mode)

    def test_v3_extra_financial_trigger_blocks_without_mutation(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(_V3_SCHEMA_SOURCE)
            conn.execute(
                """
                CREATE TRIGGER rogue_order_rewrite AFTER INSERT ON orders
                BEGIN
                  UPDATE orders SET total_price='0' WHERE order_id=NEW.order_id;
                END
                """
            )
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "unexpected v3"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)
        self.assertEqual(self.journal_mode(self.path), "delete")

    def test_v3_populated_shadow_liability_table_blocks_without_mutation(self):
        V3Store = committed_v3_store_class()
        V3Store(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute(
                "CREATE TABLE shadow_liabilities(user_id INTEGER, units INTEGER)"
            )
            conn.execute("INSERT INTO shadow_liabilities VALUES(7, 117)")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "unexpected|non-exact"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)
        self.assertEqual(self.journal_mode(self.path), "wal")

    def test_v4_rogue_catalog_is_rejected_before_schema_meta_side_effect(self):
        store = self.make_store()
        with managed_connection(store.connect()) as conn:
            conn.execute(
                "INSERT INTO users VALUES(77,'Safe','safe-wallet',1,1)"
            )
            conn.execute(
                """
                CREATE TRIGGER rogue_stamp AFTER UPDATE ON schema_meta
                BEGIN
                  UPDATE users SET wallet_addr='attacker-wallet' WHERE user_id=77;
                END
                """
            )
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "unexpected|rogue_stamp"):
            store.initialize()
        self.assertEqual(self.database_dump(self.path), before)
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute("SELECT wallet_addr FROM users WHERE user_id=77").fetchone()[0],
                "safe-wallet",
            )

    def test_v4_shadow_table_is_rejected_without_mutation(self):
        store = self.make_store()
        with managed_connection(store.connect()) as conn:
            conn.execute("CREATE TABLE shadow_liabilities(user_id INTEGER, units INTEGER)")
            conn.execute("INSERT INTO shadow_liabilities VALUES(7,117)")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "unexpected|shadow_liabilities"):
            store.initialize()
        self.assertEqual(self.database_dump(self.path), before)

    def test_v3_and_v4_reject_every_unknown_catalog_object_kind(self):
        scripts = {
            "table": "CREATE TABLE rogue_object(value TEXT)",
            "view": "CREATE VIEW rogue_object AS SELECT user_id FROM users",
            "index": "CREATE INDEX rogue_object ON users(username)",
            "trigger": (
                "CREATE TRIGGER rogue_object AFTER UPDATE ON users "
                "BEGIN SELECT 1; END"
            ),
        }
        V3Store = committed_v3_store_class()
        for version, initializer in ((3, V3Store), (4, Store)):
            for kind, script in scripts.items():
                with self.subTest(version=version, kind=kind):
                    path = Path(self.tmp.name) / f"rogue-{version}-{kind}.db"
                    initializer(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.execute(script)
                    before = self.database_dump(path)
                    with self.assertRaisesRegex(
                        MigrationBlocked, "unexpected.*rogue_object"
                    ):
                        Store(path).initialize()
                    self.assertEqual(self.database_dump(path), before)

    def test_v4_rejects_trigger_hidden_behind_same_table_name(self):
        store = self.make_store()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute("PRAGMA foreign_keys=OFF")
            conn.execute("DROP TABLE users")
            conn.execute(
                """
                CREATE TRIGGER users AFTER UPDATE ON orders
                BEGIN
                  SELECT 1;
                END
                """
            )
            conn.execute(_EXPECTED_V4_OBJECTS["users"][1])
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "duplicate.*users"):
            store.initialize()
        self.assertEqual(self.database_dump(self.path), before)

    def test_catalog_comparison_preserves_whitespace_inside_quoted_literals(self):
        store = self.make_store()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            schema_version = conn.execute("PRAGMA schema_version").fetchone()[0]
            original = conn.execute(
                "SELECT sql FROM sqlite_master WHERE type='table' AND name='orders'"
            ).fetchone()[0]
            mutated = original.replace("*[^ -~]*", "*[^\t-~]*")
            self.assertNotEqual(mutated, original)
            conn.execute("PRAGMA writable_schema=ON")
            conn.execute(
                "UPDATE sqlite_master SET sql=? WHERE type='table' AND name='orders'",
                (mutated,),
            )
            conn.execute("PRAGMA writable_schema=OFF")
            conn.execute(f"PRAGMA schema_version={schema_version + 1}")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "orders.*definition"):
            store.initialize()
        self.assertEqual(self.database_dump(self.path), before)

    def test_v3_and_v4_reject_reserved_name_executable_catalog_rows(self):
        V3Store = committed_v3_store_class()
        for version, initializer in ((3, V3Store), (4, Store)):
            with self.subTest(version=version):
                path = Path(self.tmp.name) / f"sqlite-rogue-{version}.db"
                initializer(path).initialize()
                with managed_connection(sqlite3.connect(path)) as conn:
                    schema_version = conn.execute("PRAGMA schema_version").fetchone()[0]
                    conn.execute("PRAGMA writable_schema=ON")
                    conn.execute(
                        """
                        INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql)
                        VALUES('trigger','sqlite_rogue','users',0,?)
                        """,
                        (
                            "CREATE TRIGGER sqlite_rogue AFTER UPDATE ON users "
                            "BEGIN SELECT 1; END",
                        ),
                    )
                    conn.execute("PRAGMA writable_schema=OFF")
                    conn.execute(f"PRAGMA schema_version={schema_version + 1}")
                before = self.database_dump(path)
                with self.assertRaisesRegex(
                    MigrationBlocked, "unexpected.*sqlite_rogue"
                ):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)

    def test_v4_origin_marker_prevents_complete_evidence_set_deletion(self):
        V3Store = committed_v3_store_class()
        cases = ("prototype", "v3")
        for origin in cases:
            with self.subTest(origin=origin):
                path = Path(self.tmp.name) / f"lost-evidence-{origin}.db"
                if origin == "prototype":
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                    Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.execute("DROP TABLE orders_v2_archive")
                        conn.execute("DROP TABLE withdrawals")
                else:
                    V3Store(path).initialize()
                    Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        for table in (
                            "transfers_v3_archive",
                            "orders_v3_archive",
                            "audit_events_v3_archive",
                            "schema_meta_v3_archive",
                        ):
                            conn.execute(f'DROP TABLE "{table}"')
                before = self.database_dump(path)
                with self.assertRaisesRegex(MigrationBlocked, "origin|missing"):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)

    def test_schema_meta_records_exact_migration_origin(self):
        V3Store = committed_v3_store_class()
        for expected_origin in (
            "fresh",
            "live_prototype",
            "v3_fresh",
            "v3_with_withdrawals",
            "v3_live_prototype",
        ):
            with self.subTest(origin=expected_origin):
                path = Path(self.tmp.name) / f"origin-{expected_origin}.db"
                if expected_origin == "live_prototype":
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                elif expected_origin == "v3_fresh":
                    V3Store(path).initialize()
                elif expected_origin == "v3_with_withdrawals":
                    V3Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(
                            """
                            CREATE TABLE withdrawals(
                              withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
                              admin_id INTEGER NOT NULL, amount TEXT NOT NULL,
                              address TEXT NOT NULL, txid TEXT,
                              status TEXT NOT NULL, created_at INTEGER NOT NULL
                            );
                            """
                        )
                elif expected_origin == "v3_live_prototype":
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                    V3Store(path).initialize()
                Store(path).initialize()
                with managed_connection(sqlite3.connect(path)) as conn:
                    self.assertEqual(
                        conn.execute("SELECT origin FROM schema_meta WHERE id=1").fetchone()[0],
                        expected_origin,
                    )

    def test_schema_meta_version_and_origin_row_is_insert_once_and_immutable(self):
        store = self.make_store()
        with managed_connection(store.connect()) as conn:
            statements = (
                "UPDATE schema_meta SET origin='live_prototype' WHERE id=1",
                "UPDATE schema_meta SET version=3 WHERE id=1",
                "DELETE FROM schema_meta WHERE id=1",
                "INSERT INTO schema_meta(id,version,origin) VALUES(1,4,'fresh')",
                "INSERT OR REPLACE INTO schema_meta(id,version,origin) VALUES(1,4,'live_prototype')",
            )
            for statement in statements:
                with self.subTest(statement=statement), self.assertRaisesRegex(
                    sqlite3.IntegrityError, "schema metadata"
                ):
                    conn.execute(statement)
            self.assertEqual(
                tuple(
                    conn.execute(
                        "SELECT id,version,origin FROM schema_meta"
                    ).fetchone()
                ),
                (1, 4, "fresh"),
            )

    def test_each_wrong_origin_is_rejected_against_its_evidence_catalog(self):
        V3Store = committed_v3_store_class()
        wrong_origins = {
            "fresh": "live_prototype",
            "live_prototype": "fresh",
            "v3_fresh": "fresh",
            "v3_with_withdrawals": "v3_fresh",
            "v3_live_prototype": "v3_fresh",
        }
        for correct_origin, wrong_origin in wrong_origins.items():
            with self.subTest(correct=correct_origin, wrong=wrong_origin):
                path = Path(self.tmp.name) / f"wrong-origin-{correct_origin}.db"
                if correct_origin == "live_prototype":
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                elif correct_origin == "v3_fresh":
                    V3Store(path).initialize()
                elif correct_origin == "v3_with_withdrawals":
                    V3Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(
                            """
                            CREATE TABLE withdrawals(
                              withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
                              admin_id INTEGER NOT NULL, amount TEXT NOT NULL,
                              address TEXT NOT NULL, txid TEXT,
                              status TEXT NOT NULL, created_at INTEGER NOT NULL
                            );
                            """
                        )
                elif correct_origin == "v3_live_prototype":
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                    V3Store(path).initialize()
                Store(path).initialize()

                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.execute("DROP TRIGGER schema_meta_update_block")
                    conn.execute("UPDATE schema_meta SET origin=?", (wrong_origin,))
                    conn.execute(
                        _EXPECTED_V4_OBJECTS["schema_meta_update_block"][1]
                    )
                before = self.database_dump(path)
                with self.assertRaisesRegex(MigrationBlocked, "origin|unexpected|required"):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)

    def test_existing_v4_initialize_is_validation_only(self):
        class NoVersionWriteStore(Store):
            def _connect(self, *, apply_wal):
                conn = super()._connect(apply_wal=apply_wal)
                if not apply_wal:
                    def authorizer(action, first, _second, _db, _source):
                        if (
                            action == sqlite3.SQLITE_UPDATE
                            and str(first).lower() == "schema_meta"
                        ):
                            return sqlite3.SQLITE_DENY
                        return sqlite3.SQLITE_OK

                    conn.set_authorizer(authorizer)
                return conn

        self.make_store()
        NoVersionWriteStore(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(conn.execute("SELECT version FROM schema_meta").fetchone()[0], 4)

    def test_malformed_empty_legacy_withdrawals_table_blocks(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
            conn.execute("DROP TABLE withdrawals")
            conn.execute("CREATE TABLE withdrawals(value TEXT)")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "withdrawals.*definition"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)
        self.assertEqual(self.journal_mode(self.path), "delete")

    def test_prototype_tombstoned_orders_or_withdrawals_block(self):
        for table in ("orders", "withdrawals"):
            with self.subTest(table=table):
                path = Path(self.tmp.name) / f"prototype-tombstone-{table}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                    if table == "orders":
                        conn.execute(
                            """
                            INSERT INTO orders(seller_id,amount,price,currency,created_at,updated_at)
                            VALUES(7,'1','1','AUD',1,1)
                            """
                        )
                    else:
                        conn.execute(
                            """
                            INSERT INTO withdrawals(admin_id,amount,address,status,created_at)
                            VALUES(1,'1','address','pending',1)
                            """
                        )
                    conn.execute(f"DELETE FROM {table}")
                before = self.database_dump(path)
                with self.assertRaisesRegex(MigrationBlocked, "sequence|previously"):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)
                self.assertEqual(self.journal_mode(path), "delete")

    def test_prototype_zero_and_negative_sequence_rows_block(self):
        cases = {
            "orders": """
                INSERT INTO orders(
                  order_id,seller_id,amount,price,currency,created_at,updated_at
                ) VALUES(?,7,'1','1','AUD',1,1)
            """,
            "withdrawals": """
                INSERT INTO withdrawals(
                  withdrawal_id,admin_id,amount,address,status,created_at
                ) VALUES(?,1,'1','x','pending',1)
            """,
        }
        for table, insert_sql in cases.items():
            for explicit_id in (0, -1):
                with self.subTest(table=table, explicit_id=explicit_id):
                    path = Path(self.tmp.name) / f"prototype-seq-{table}-{explicit_id}.db"
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                        conn.execute(insert_sql, (explicit_id,))
                        conn.execute(f'DELETE FROM "{table}"')
                        sequence = conn.execute(
                            "SELECT seq FROM sqlite_sequence WHERE name=?", (table,)
                        ).fetchone()
                        self.assertIsNotNone(sequence)
                before = self.database_dump(path)
                with self.assertRaisesRegex(MigrationBlocked, "sqlite_sequence"):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)

    def test_v3_tombstoned_financial_sequences_block(self):
        V3Store = committed_v3_store_class()
        for table in ("orders", "transfers", "audit_events", "withdrawals"):
            with self.subTest(table=table):
                path = Path(self.tmp.name) / f"v3-tombstone-{table}.db"
                V3Store(path).initialize()
                with managed_connection(sqlite3.connect(path)) as conn:
                    if table == "orders":
                        conn.execute(
                            """
                            INSERT INTO orders(
                              side,maker_id,maker_name,seller_id,seller_name,
                              net_amount_units,network_fee_units,service_fee_units,
                              deposit_required_units,total_price,settlement_asset,
                              payment_method,state,created_at,updated_at
                            ) VALUES('sell',7,'s',7,'s',1,0,0,1,'1','AUD','cash',
                                     'awaiting_deposit',1,1)
                            """
                        )
                    elif table == "transfers":
                        conn.execute(
                            """
                            INSERT INTO transfers(kind,state,amount_units,network_fee_units,
                              destination,created_at,updated_at)
                            VALUES('fee_withdrawal','reserved',1,0,'x',1,1)
                            """
                        )
                    elif table == "audit_events":
                        conn.execute("INSERT INTO audit_events(event_type,created_at) VALUES('x',1)")
                    else:
                        conn.executescript(
                            """
                            CREATE TABLE withdrawals(
                              withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
                              admin_id INTEGER NOT NULL, amount TEXT NOT NULL,
                              address TEXT NOT NULL, txid TEXT,
                              status TEXT NOT NULL, created_at INTEGER NOT NULL
                            );
                            INSERT INTO withdrawals(admin_id,amount,address,status,created_at)
                            VALUES(1,'1','x','pending',1);
                            """
                        )
                    conn.execute(f"DELETE FROM {table}")
                before = self.database_dump(path)
                with self.assertRaisesRegex(MigrationBlocked, "sequence|previously"):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)

    def test_v3_zero_and_negative_sequence_rows_block(self):
        V3Store = committed_v3_store_class()
        cases = {
            "orders": """
                INSERT INTO orders(
                  order_id,side,maker_id,maker_name,seller_id,seller_name,
                  net_amount_units,network_fee_units,service_fee_units,
                  deposit_required_units,total_price,settlement_asset,
                  payment_method,state,created_at,updated_at
                ) VALUES(?,'sell',7,'s',7,'s',1,0,0,1,'1','AUD','cash',
                         'awaiting_deposit',1,1)
            """,
            "transfers": """
                INSERT INTO transfers(
                  transfer_id,kind,state,amount_units,network_fee_units,
                  destination,created_at,updated_at
                ) VALUES(?,'fee_withdrawal','reserved',1,0,'x',1,1)
            """,
            "audit_events": """
                INSERT INTO audit_events(event_id,event_type,created_at)
                VALUES(?,'x',1)
            """,
            "withdrawals": """
                INSERT INTO withdrawals(
                  withdrawal_id,admin_id,amount,address,status,created_at
                ) VALUES(?,1,'1','x','pending',1)
            """,
        }
        for table, insert_sql in cases.items():
            for explicit_id in (0, -1):
                with self.subTest(table=table, explicit_id=explicit_id):
                    path = Path(self.tmp.name) / f"v3-seq-{table}-{explicit_id}.db"
                    V3Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        if table == "withdrawals":
                            conn.executescript(
                                """
                                CREATE TABLE withdrawals(
                                  withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
                                  admin_id INTEGER NOT NULL, amount TEXT NOT NULL,
                                  address TEXT NOT NULL, txid TEXT,
                                  status TEXT NOT NULL, created_at INTEGER NOT NULL
                                );
                                """
                            )
                        conn.execute(insert_sql, (explicit_id,))
                        conn.execute(f'DELETE FROM "{table}"')
                        self.assertIsNotNone(
                            conn.execute(
                                "SELECT seq FROM sqlite_sequence WHERE name=?", (table,)
                            ).fetchone()
                        )
                    before = self.database_dump(path)
                    with self.assertRaisesRegex(MigrationBlocked, "sqlite_sequence"):
                        Store(path).initialize()
                    self.assertEqual(self.database_dump(path), before)

    def test_v3_tombstoned_orders_v2_archive_blocks(self):
        V3Store = committed_v3_store_class()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
        V3Store(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute(
                """
                INSERT INTO orders_v2_archive(
                  seller_id,amount,price,currency,created_at,updated_at
                ) VALUES(7,'1','1','AUD',1,1)
                """
            )
            conn.execute("DELETE FROM orders_v2_archive")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "sequence|previously"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)

    def test_v3_orders_v2_archive_zero_and_negative_sequence_rows_block(self):
        V3Store = committed_v3_store_class()
        for explicit_id in (0, -1):
            with self.subTest(explicit_id=explicit_id):
                path = Path(self.tmp.name) / f"v3-v2-seq-{explicit_id}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                V3Store(path).initialize()
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.execute(
                        """
                        INSERT INTO orders_v2_archive(
                          order_id,seller_id,amount,price,currency,created_at,updated_at
                        ) VALUES(?,7,'1','1','AUD',1,1)
                        """,
                        (explicit_id,),
                    )
                    conn.execute("DELETE FROM orders_v2_archive")
                    self.assertIsNotNone(
                        conn.execute(
                            "SELECT seq FROM sqlite_sequence WHERE name='orders_v2_archive'"
                        ).fetchone()
                    )
                before = self.database_dump(path)
                with self.assertRaisesRegex(MigrationBlocked, "sqlite_sequence"):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)

    def test_v4_migrated_archive_tombstone_is_not_recertified(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
        Store(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute(
                """
                INSERT INTO orders_v2_archive(
                  seller_id,amount,price,currency,created_at,updated_at
                ) VALUES(7,'1','1','AUD',1,1)
                """
            )
            conn.execute("DELETE FROM orders_v2_archive")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "previously.*sqlite_sequence"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)

    def test_v4_evidence_tables_must_remain_empty_for_zero_and_negative_ids(self):
        V3Store = committed_v3_store_class()
        cases = (
            (
                "withdrawals",
                "prototype",
                """
                INSERT INTO withdrawals(
                  withdrawal_id,admin_id,amount,address,status,created_at
                ) VALUES(?,1,'1','x','pending',1)
                """,
            ),
            (
                "orders_v2_archive",
                "prototype",
                """
                INSERT INTO orders_v2_archive(
                  order_id,seller_id,amount,price,currency,created_at,updated_at
                ) VALUES(?,7,'1','1','AUD',1,1)
                """,
            ),
            (
                "orders_v3_archive",
                "v3",
                """
                INSERT INTO orders_v3_archive(
                  order_id,side,maker_id,maker_name,seller_id,seller_name,
                  net_amount_units,network_fee_units,service_fee_units,
                  deposit_required_units,total_price,settlement_asset,
                  payment_method,state,created_at,updated_at
                ) VALUES(?,'sell',7,'s',7,'s',1,0,0,1,'1','AUD','cash',
                         'awaiting_deposit',1,1)
                """,
            ),
            (
                "transfers_v3_archive",
                "v3",
                """
                INSERT INTO transfers_v3_archive(
                  transfer_id,kind,state,amount_units,network_fee_units,
                  destination,created_at,updated_at
                ) VALUES(?,'fee_withdrawal','reserved',1,0,'x',1,1)
                """,
            ),
            (
                "audit_events_v3_archive",
                "v3",
                """
                INSERT INTO audit_events_v3_archive(event_id,event_type,created_at)
                VALUES(?,'x',1)
                """,
            ),
        )
        for table, origin, insert_sql in cases:
            for explicit_id in (0, -1):
                with self.subTest(table=table, explicit_id=explicit_id):
                    path = Path(self.tmp.name) / f"evidence-{table}-{explicit_id}.db"
                    if origin == "prototype":
                        with managed_connection(sqlite3.connect(path)) as conn:
                            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                        Store(path).initialize()
                    else:
                        V3Store(path).initialize()
                        Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.execute(insert_sql, (explicit_id,))
                    before = self.database_dump(path)
                    with self.assertRaisesRegex(MigrationBlocked, f"{table}.*empty"):
                        Store(path).initialize()
                    self.assertEqual(self.database_dump(path), before)

    def test_migrated_v4_zero_and_negative_evidence_sequences_block(self):
        V3Store = committed_v3_store_class()
        cases = (
            (
                "withdrawals",
                "prototype",
                """
                INSERT INTO withdrawals(
                  withdrawal_id,admin_id,amount,address,status,created_at
                ) VALUES(?,1,'1','x','pending',1)
                """,
            ),
            (
                "orders_v2_archive",
                "prototype",
                """
                INSERT INTO orders_v2_archive(
                  order_id,seller_id,amount,price,currency,created_at,updated_at
                ) VALUES(?,7,'1','1','AUD',1,1)
                """,
            ),
            (
                "orders_v3_archive",
                "v3",
                """
                INSERT INTO orders_v3_archive(
                  order_id,side,maker_id,maker_name,seller_id,seller_name,
                  net_amount_units,network_fee_units,service_fee_units,
                  deposit_required_units,total_price,settlement_asset,
                  payment_method,state,created_at,updated_at
                ) VALUES(?,'sell',7,'s',7,'s',1,0,0,1,'1','AUD','cash',
                         'awaiting_deposit',1,1)
                """,
            ),
            (
                "transfers_v3_archive",
                "v3",
                """
                INSERT INTO transfers_v3_archive(
                  transfer_id,kind,state,amount_units,network_fee_units,
                  destination,created_at,updated_at
                ) VALUES(?,'fee_withdrawal','reserved',1,0,'x',1,1)
                """,
            ),
            (
                "audit_events_v3_archive",
                "v3",
                """
                INSERT INTO audit_events_v3_archive(event_id,event_type,created_at)
                VALUES(?,'x',1)
                """,
            ),
        )
        for table, origin, insert_sql in cases:
            for explicit_id in (0, -1):
                with self.subTest(table=table, explicit_id=explicit_id):
                    path = Path(self.tmp.name) / f"v4-seq-{table}-{explicit_id}.db"
                    if origin == "prototype":
                        with managed_connection(sqlite3.connect(path)) as conn:
                            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                        Store(path).initialize()
                    else:
                        V3Store(path).initialize()
                        Store(path).initialize()
                    with managed_connection(sqlite3.connect(path)) as conn:
                        conn.execute(insert_sql, (explicit_id,))
                        conn.execute(f'DELETE FROM "{table}"')
                        self.assertIsNotNone(
                            conn.execute(
                                "SELECT seq FROM sqlite_sequence WHERE name=?", (table,)
                            ).fetchone()
                        )
                    before = self.database_dump(path)
                    with self.assertRaisesRegex(MigrationBlocked, "sqlite_sequence"):
                        Store(path).initialize()
                    self.assertEqual(self.database_dump(path), before)

    def test_v4_schema_meta_v3_archive_must_remain_exact(self):
        V3Store = committed_v3_store_class()
        V3Store(self.path).initialize()
        Store(self.path).initialize()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute("UPDATE schema_meta_v3_archive SET version=2 WHERE id=1")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "schema_meta_v3_archive.*1.*3"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)

    def test_fault_injected_v3_migration_rolls_back_catalog_data_and_journal(self):
        phases = (
            "writer_preflight",
            "v3_indexes",
            "v3_archives",
            "users",
            "archives",
            "schema",
            "foreign_keys",
            "stamp",
        )

        class FaultStore(Store):
            phase = None

            def _migration_checkpoint(self, phase):
                if phase == self.phase:
                    raise RuntimeError(f"fault:{phase}")

        for phase in phases:
            with self.subTest(phase=phase):
                path = Path(self.tmp.name) / f"fault-{phase}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(_V3_SCHEMA_SOURCE)
                    conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
                before = self.database_dump(path)
                FaultStore.phase = phase
                with self.assertRaisesRegex(RuntimeError, f"fault:{phase}"):
                    FaultStore(path).initialize()
                self.assertEqual(self.database_dump(path), before)
                self.assertEqual(self.journal_mode(path), "delete")

    def test_fault_injected_prototype_migration_rolls_back_every_phase(self):
        phases = (
            "writer_preflight",
            "v2_archive",
            "users",
            "archives",
            "schema",
            "foreign_keys",
            "stamp",
        )

        class FaultStore(Store):
            phase = None

            def _migration_checkpoint(self, phase):
                if phase == self.phase:
                    raise RuntimeError(f"fault:{phase}")

        for phase in phases:
            with self.subTest(phase=phase):
                path = Path(self.tmp.name) / f"prototype-fault-{phase}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(LIVE_PROTOTYPE_SCHEMA)
                    conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
                before = self.database_dump(path)
                FaultStore.phase = phase
                with self.assertRaisesRegex(RuntimeError, f"fault:{phase}"):
                    FaultStore(path).initialize()
                self.assertEqual(self.database_dump(path), before)
                self.assertEqual(self.journal_mode(path), "delete")

    def test_newer_or_malformed_schema_blocks_without_wal_or_stamp(self):
        cases = {
            "newer": "CREATE TABLE schema_meta(id INTEGER, version INTEGER); INSERT INTO schema_meta VALUES(1,99);",
            "malformed": "CREATE TABLE schema_meta(id INTEGER PRIMARY KEY, version INTEGER); INSERT INTO schema_meta VALUES(1,4);",
            "partial": "CREATE TABLE orders(order_id INTEGER PRIMARY KEY, side TEXT);",
        }
        for name, script in cases.items():
            with self.subTest(name=name):
                path = Path(self.tmp.name) / f"{name}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(script)
                before = self.database_dump(path)
                expected = UnsupportedSchemaVersion if name == "newer" else MigrationBlocked
                with self.assertRaises(expected):
                    Store(path).initialize()
                self.assertEqual(self.database_dump(path), before)
                self.assertEqual(self.journal_mode(path), "delete")

    def test_wal_activation_denial_does_not_stamp_or_rewrite(self):
        class WalDeniedStore(Store):
            def _connect(self, *, apply_wal):
                conn = super()._connect(apply_wal=apply_wal)
                if not apply_wal:
                    def authorizer(action, first, second, _db, _source):
                        if (
                            action == sqlite3.SQLITE_PRAGMA
                            and str(first).lower() == "journal_mode"
                            and str(second).lower() == "wal"
                        ):
                            return sqlite3.SQLITE_DENY
                        return sqlite3.SQLITE_OK

                    conn.set_authorizer(authorizer)
                return conn

        before = self.database_dump(self.path)
        with self.assertRaises(sqlite3.DatabaseError):
            WalDeniedStore(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)
        self.assertEqual(self.journal_mode(self.path), "delete")

    def test_existing_v4_missing_trigger_is_not_recertified(self):
        self.make_store()
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.execute("DROP TRIGGER transfer_delete_block")
        before = self.database_dump(self.path)
        with self.assertRaisesRegex(MigrationBlocked, "transfer_delete_block.*missing"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)
        self.assertEqual(self.journal_mode(self.path), "wal")

    def test_public_order_interface_normalizes_price_and_has_no_balance_aggregate(self):
        store, order_id = self.create_sell(total_price=" 250.00 ")
        row = store.get_order(order_id=order_id)
        self.assertEqual(row["total_price"], "250.00")
        self.assertEqual(row["settlement_asset"], "AUD")
        self.assertNotIn("deposit_confirmed_units", row.keys())
        self.assertIn("funded_at", row.keys())
        for bad in (True, 1.0, "1"):
            with self.subTest(net=bad), self.assertRaises(ValueError):
                store.create_order(**self.order_values(net_amount_units=bad))
        for bad in ("0", "01", "1e2", "1\x00.5", "1.1234567890123456789"):
            with self.subTest(price=bad), self.assertRaises(ValueError):
                store.create_order(**self.order_values(total_price=bad))

    def test_initial_order_roles_and_side_aware_participants_are_enforced(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id = self.add_credit(conn, order_id)[0]
            conn.execute("UPDATE orders SET state='open', updated_at=102 WHERE order_id=?", (order_id,))
            self.add_user(conn, 8, "Buyer", "buyer-wallet")
            self.add_user(conn, 9, "Other", "other-wallet")
            conn.execute(
                "UPDATE orders SET buyer_id=8,buyer_name='Buyer',state='matched',updated_at=103 WHERE order_id=?",
                (order_id,),
            )
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("UPDATE orders SET buyer_id=9,buyer_name='Other' WHERE order_id=?", (order_id,))
            conn.execute(
                "UPDATE orders SET buyer_id=NULL,buyer_name=NULL,state='open',updated_at=104 WHERE order_id=?",
                (order_id,),
            )
            conn.execute(
                "UPDATE orders SET buyer_id=9,buyer_name='Other',state='matched',updated_at=105 WHERE order_id=?",
                (order_id,),
            )
            self.assertIsNotNone(credit_id)

        buy_id = store.create_order(
            **self.order_values(
                side=OrderSide.BUY,
                maker_id=20,
                maker_name="BuyMaker",
                state=OrderState.OPEN,
                deposit_addr=None,
            )
        )
        with managed_connection(store.connect()) as conn:
            self.add_user(conn, 21, "Seller2", "seller-2-wallet")
            conn.execute(
                """
                UPDATE orders SET seller_id=21,seller_name='Seller2',
                  deposit_addr='buy-deposit',state='awaiting_deposit',updated_at=101
                WHERE order_id=?
                """,
                (buy_id,),
            )
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("UPDATE orders SET deposit_addr='redirected' WHERE order_id=?", (buy_id,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("UPDATE orders SET seller_id=7,seller_name='Seller' WHERE order_id=?", (buy_id,))

        cancelled = store.create_order(
            **self.order_values(
                side=OrderSide.BUY,
                maker_id=30,
                maker_name="CancelledBuyer",
                state=OrderState.OPEN,
                deposit_addr=None,
            )
        )
        with managed_connection(store.connect()) as conn:
            conn.execute("UPDATE orders SET state='cancelled',updated_at=101 WHERE order_id=?", (cancelled,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("UPDATE orders SET deposit_addr='poison' WHERE order_id=?", (cancelled,))

    def test_quote_order_and_audit_rows_are_append_only(self):
        store, order_id = self.create_sell()
        event_id = store.append_audit(
            order_id=order_id,
            event_type="order:created",
            new_state=OrderState.AWAITING_DEPOSIT,
            detail={"source": "test"},
            created_at=101,
        )
        with managed_connection(store.connect()) as conn:
            for sql, params in (
                ("UPDATE orders SET total_price='999' WHERE order_id=?", (order_id,)),
                ("DELETE FROM orders WHERE order_id=?", (order_id,)),
                ("UPDATE audit_events SET event_type='changed' WHERE event_id=?", (event_id,)),
                ("DELETE FROM audit_events WHERE event_id=?", (event_id,)),
            ):
                with self.subTest(sql=sql), self.assertRaises(sqlite3.IntegrityError):
                    conn.execute(sql, params)

    def test_strict_units_quote_sum_and_canonical_text_checks(self):
        store = self.make_store()
        valid = self.order_values()
        for field, bad in (
            ("net_amount_units", 1.5),
            ("net_amount_units", 2_100_000_000_000_001),
            ("deposit_required_units", 118),
        ):
            with self.subTest(field=field, bad=bad), self.assertRaises((ValueError, sqlite3.IntegrityError)):
                store.create_order(**(valid | {field: bad}))
        for field, bad in (
            ("maker_name", "Seller\x00hidden"),
            ("deposit_addr", "address\x00hidden"),
            ("settlement_network", "main\x00hidden"),
            ("payment_method", "Pay\x00ID"),
        ):
            with self.subTest(field=field), self.assertRaises(ValueError):
                store.create_order(**(valid | {field: bad}))

    def test_direct_sql_rejects_noncanonical_prices_and_fractional_units(self):
        store = self.make_store()
        with managed_connection(store.connect()) as conn:
            self.add_user(conn, 99, "RawSeller", "raw-wallet")

            def insert_order(price, net=100, address="raw-deposit"):
                conn.execute(
                    """
                    INSERT INTO orders(
                      side,maker_id,maker_name,seller_id,seller_name,
                      net_amount_units,network_fee_units,service_fee_units,
                      deposit_required_units,total_price,settlement_asset,
                      payment_method,state,deposit_addr,created_at,updated_at
                    ) VALUES('sell',99,'RawSeller',99,'RawSeller',?,10,7,117,?,
                             'AUD','Cash','awaiting_deposit',?,1,1)
                    """,
                    (net, price, address),
                )

            for bad in (
                "0",
                "00.1",
                ".1",
                "1.",
                "1..2",
                "1e2",
                "1,000",
                "+1",
                "-1",
                "1234567890123456789",
                "1.1234567890123456789",
                "１",
                "1\x00hidden",
            ):
                with self.subTest(price=bad), self.assertRaises(sqlite3.IntegrityError):
                    insert_order(bad)
            with self.assertRaises(sqlite3.IntegrityError):
                insert_order("1", net=100.5)

    def test_scan_credit_identity_and_observation_evidence_is_durable(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id, scan_id = self.add_credit(conn, order_id)
            for sql in (
                "UPDATE deposit_scans SET tip_height=99 WHERE scan_id=?",
                "DELETE FROM deposit_scans WHERE scan_id=?",
            ):
                with self.assertRaises(sqlite3.IntegrityError):
                    conn.execute(sql, (scan_id,))
            for assignment in (
                "txid='" + self.hash(77) + "'",
                "amount_units=118",
                "order_id=999",
                "credited_at=NULL",
            ):
                with self.subTest(assignment=assignment), self.assertRaises(sqlite3.IntegrityError):
                    conn.execute(f"UPDATE deposit_credits SET {assignment} WHERE credit_id=?", (credit_id,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("DELETE FROM deposit_credits WHERE credit_id=?", (credit_id,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("UPDATE deposit_credits SET confirmations=7 WHERE credit_id=?", (credit_id,))

            cursor = conn.execute(
                """
                INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at)
                VALUES('btc09-mainnet','deposit-address-7',?,2,102)
                """,
                (self.hash(88),),
            )
            scan2 = cursor.lastrowid
            conn.execute(
                """
                UPDATE deposit_credits SET confirmations=7,last_seen_at=102,
                  last_seen_scan_id=?,last_checked_scan_id=? WHERE credit_id=?
                """,
                (scan2, scan2, credit_id),
            )
            self.assertEqual(conn.execute("SELECT confirmations FROM deposit_credits WHERE credit_id=?", (credit_id,)).fetchone()[0], 7)

    def test_credit_composite_keys_partial_spend_tuple_and_duplicate_outpoint(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id, scan_id = self.add_credit(conn, order_id)
            with self.assertRaises(sqlite3.IntegrityError):
                self.add_credit(conn, order_id, credit_number=1, scan_number=2)
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    UPDATE deposit_credits SET spent_by_txid=?,last_checked_scan_id=?
                    WHERE credit_id=?
                    """,
                    (self.hash(90), scan_id, credit_id),
                )
            cursor = conn.execute(
                """
                INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at)
                VALUES('btc09-mainnet','other-address',?,2,102)
                """,
                (self.hash(91),),
            )
            wrong_scan = cursor.lastrowid
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    "UPDATE deposit_credits SET last_checked_scan_id=? WHERE credit_id=?",
                    (wrong_scan, credit_id),
                )

    def test_every_partial_spent_by_tuple_is_rejected(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id, _ = self.add_credit(conn, order_id)
            cursor = conn.execute(
                """
                INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at)
                VALUES('btc09-mainnet','deposit-address-7',?,2,102)
                """,
                (self.hash(92),),
            )
            scan2 = cursor.lastrowid
            values = (self.hash(93), 0, self.hash(94), 2)
            for mask in range(1, 15):
                selected = [values[index] if mask & (1 << index) else None for index in range(4)]
                with self.subTest(mask=f"{mask:04b}"), self.assertRaisesRegex(
                    sqlite3.IntegrityError, "CHECK constraint failed"
                ):
                    conn.execute(
                        """
                        UPDATE deposit_credits SET
                          spent_by_txid=?,spent_by_vin=?,spent_by_block_hash=?,
                          spent_by_block_height=?,last_seen_at=102,
                          last_seen_scan_id=?,last_checked_scan_id=?
                        WHERE credit_id=?
                        """,
                        (*selected, scan2, scan2, credit_id),
                    )
            conn.execute(
                """
                UPDATE deposit_credits SET
                  spent_by_txid=?,spent_by_vin=?,spent_by_block_hash=?,
                  spent_by_block_height=?,last_seen_at=102,
                  last_seen_scan_id=?,last_checked_scan_id=?
                WHERE credit_id=?
                """,
                (*values, scan2, scan2, credit_id),
            )
            row = conn.execute(
                """
                SELECT spent_by_txid,spent_by_vin,spent_by_block_hash,
                       spent_by_block_height FROM deposit_credits WHERE credit_id=?
                """,
                (credit_id,),
            ).fetchone()
            self.assertEqual(tuple(row), values)

    def test_underpaid_credit_reclassification_is_one_way_and_capacity_safe(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id, _ = self.add_credit(conn, order_id, amount=5, main=5)
            conn.execute("UPDATE orders SET state='recovery_hold',updated_at=102 WHERE order_id=?", (order_id,))
            conn.execute(
                """
                UPDATE deposit_credits SET main_units=0,recovery_units=5,
                  recovery_reason='cancelled_partial' WHERE credit_id=?
                """,
                (credit_id,),
            )
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    "UPDATE deposit_credits SET main_units=5,recovery_units=0,recovery_reason=NULL WHERE credit_id=?",
                    (credit_id,),
                )

        path = Path(self.tmp.name) / "full.db"
        full_store = Store(path)
        full_store.initialize()
        full_id = full_store.create_order(**self.order_values(deposit_addr="full-address"))
        with managed_connection(full_store.connect()) as conn:
            full_credit, _ = self.add_credit(conn, full_id, address="full-address")
            conn.execute("UPDATE orders SET state='open',updated_at=102 WHERE order_id=?", (full_id,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    "UPDATE deposit_credits SET main_units=0,recovery_units=117,recovery_reason='cancelled_partial' WHERE credit_id=?",
                    (full_credit,),
                )

    def test_transfer_economics_allocations_lifetime_keys_and_append_only_rules(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id = self.fund_and_match(conn, order_id)
            conn.execute("UPDATE orders SET buyer_confirmed=1,updated_at=104 WHERE order_id=?", (order_id,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    INSERT INTO transfers(operation_key,order_id,kind,is_main_outcome,
                      state,amount_units,network_fee_units,earned_fee_units,destination,
                      created_at,updated_at)
                    VALUES(?,?,'release',1,'queued',101,10,7,'buyer-wallet',105,105)
                    """,
                    (f"order:{order_id}:main", order_id),
                )
            transfer_id = self.queue_release(conn, order_id, credit_id)
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    INSERT INTO transfer_credit_allocations(
                      transfer_id,credit_id,order_id,bucket,units
                    ) VALUES(?,?,?,'main',1)
                    """,
                    (transfer_id, credit_id, order_id),
                )
            for sql in (
                "UPDATE transfers SET amount_units=99 WHERE transfer_id=?",
                "UPDATE transfer_credit_allocations SET units=116 WHERE transfer_id=?",
                "DELETE FROM transfer_credit_allocations WHERE transfer_id=?",
                "DELETE FROM transfers WHERE transfer_id=?",
            ):
                with self.subTest(sql=sql), self.assertRaises(sqlite3.IntegrityError):
                    conn.execute(sql, (transfer_id,))
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    INSERT OR REPLACE INTO transfers(
                      operation_key,order_id,kind,is_main_outcome,state,amount_units,
                      network_fee_units,earned_fee_units,destination,created_at,updated_at
                    ) VALUES(?,?,'release',1,'queued',100,10,7,'buyer-wallet',105,105)
                    """,
                    (f"order:{order_id}:main", order_id),
                )

    def test_direct_sql_rejects_same_and_cross_order_escrow_loop_destinations(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            self.fund_and_match(conn, order_id)
            conn.execute(
                "UPDATE users SET wallet_addr='deposit-address-7' WHERE user_id=8"
            )
            with self.assertRaisesRegex(
                sqlite3.IntegrityError, "destination is an escrow deposit address"
            ):
                conn.execute(
                    """
                    INSERT INTO transfers(
                      operation_key,order_id,kind,is_main_outcome,state,
                      amount_units,network_fee_units,earned_fee_units,destination,
                      created_at,updated_at
                    ) VALUES(?,?,'release',1,'queued',100,10,7,
                             'deposit-address-7',104,104)
                    """,
                    (f"order:{order_id}:main", order_id),
                )

            other_id = store.create_order(
                **self.order_values(
                    maker_id=9,
                    maker_name="Other Seller",
                    seller_id=9,
                    seller_name="Other Seller",
                    deposit_addr="other-deposit-address",
                    created_at=105,
                    updated_at=105,
                )
            )
            self.assertGreater(other_id, order_id)
            conn.execute(
                "UPDATE users SET wallet_addr='other-deposit-address' WHERE user_id=8"
            )
            with self.assertRaisesRegex(
                sqlite3.IntegrityError, "destination is an escrow deposit address"
            ):
                conn.execute(
                    """
                    INSERT INTO transfers(
                      operation_key,order_id,kind,is_main_outcome,state,
                      amount_units,network_fee_units,earned_fee_units,destination,
                      created_at,updated_at
                    ) VALUES(?,?,'release',1,'queued',100,10,7,
                             'other-deposit-address',106,106)
                    """,
                    (f"order:{order_id}:main", order_id),
                )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 0
            )
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM transfer_credit_allocations"
                ).fetchone()[0],
                0,
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0], 0
            )

    def test_direct_sql_rejects_fee_withdrawal_to_any_escrow_deposit_address(self):
        store, _ = self.make_confirmed_release()
        with managed_connection(store.connect()) as conn:
            conn.execute(
                """
                INSERT INTO users(user_id,username,wallet_addr,created_at,updated_at)
                VALUES(9,'Other Seller','seller-wallet-9',110,110)
                """
            )
            other_id = store.create_order(
                **self.order_values(
                    maker_id=9,
                    maker_name="Other Seller",
                    seller_id=9,
                    seller_name="Other Seller",
                    deposit_addr="fee-loop-deposit",
                    created_at=110,
                    updated_at=110,
                )
            )
            self.assertGreater(other_id, 0)
            with self.assertRaisesRegex(
                sqlite3.IntegrityError, "destination is an escrow deposit address"
            ):
                conn.execute(
                    """
                    INSERT INTO transfers(
                      operation_key,kind,is_main_outcome,state,amount_units,
                      network_fee_units,earned_fee_units,destination,created_at,updated_at
                    ) VALUES('fee:escrow-loop','fee_withdrawal',0,'queued',5,1,0,
                             'fee-loop-deposit',111,111)
                    """
                )
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM transfers WHERE kind='fee_withdrawal'"
                ).fetchone()[0],
                0,
            )

    def test_transfer_state_machine_lane_and_uncertain_global_halt(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id = self.fund_and_match(conn, order_id)
            transfer_id = self.queue_release(conn, order_id, credit_id)
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute("UPDATE transfers SET state='broadcast' WHERE transfer_id=?", (transfer_id,))
            conn.execute(
                "UPDATE transfers SET state='reserved',attempt_count=1,reserved_at=106,updated_at=106 WHERE transfer_id=?",
                (transfer_id,),
            )
            self.add_user(conn, 50, "FeeAdmin", "fee-wallet")
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    INSERT INTO transfers(operation_key,kind,is_main_outcome,state,
                      amount_units,network_fee_units,destination,created_at,updated_at)
                    VALUES('fee:one','fee_withdrawal',0,'reserved',1,0,'fee-wallet',1,1)
                    """
                )
            conn.execute(
                """
                UPDATE transfers SET state='prepared',txid=?,signed_tx_hex='aa',
                  signed_at=107,prepared_tip_hash=?,prepared_tip_height=10,updated_at=107
                WHERE transfer_id=?
                """,
                (self.hash(500), self.hash(501), transfer_id),
            )
            conn.execute("UPDATE transfers SET state='uncertain',updated_at=108 WHERE transfer_id=?", (transfer_id,))

            # Seed earned revenue without bypassing the ledger by returning to a
            # confirmed state first, then leave it uncertain again.
            conn.execute(
                """
                UPDATE transfers SET state='confirmed',confirmed_at=109,
                  confirmed_block_hash=?,confirmed_block_height=11,
                  confirmations=1,updated_at=109 WHERE transfer_id=?
                """,
                (self.hash(502), transfer_id),
            )
            cursor = conn.execute(
                """
                INSERT INTO transfers(operation_key,kind,is_main_outcome,state,
                  amount_units,network_fee_units,destination,created_at,updated_at)
                VALUES('fee:queued','fee_withdrawal',0,'queued',1,0,'fee-wallet',111,111)
                """
            )
            conn.execute("UPDATE transfers SET state='uncertain',updated_at=110 WHERE transfer_id=?", (transfer_id,))
            with self.assertRaisesRegex(sqlite3.IntegrityError, "uncertain"):
                conn.execute(
                    """
                    UPDATE transfers SET state='reserved',attempt_count=1,
                      reserved_at=112,updated_at=112 WHERE transfer_id=?
                    """,
                    (cursor.lastrowid,),
                )

    def test_uncertainty_blocks_reserved_prepare_and_prepared_broadcast(self):
        store, earning_transfer = self.make_confirmed_release()
        with managed_connection(store.connect()) as conn:
            fee_transfer = self.queue_fee_withdrawal(conn, "fee:post-claim", 120)
            self.claim_transfer(conn, fee_transfer, 121)
            conn.execute(
                "UPDATE transfers SET state='uncertain',updated_at=122 WHERE transfer_id=?",
                (earning_transfer,),
            )
            with self.assertRaisesRegex(
                sqlite3.IntegrityError, "uncertain transfer blocks pre-broadcast"
            ):
                self.prepare_transfer(conn, fee_transfer, 123, 1)

            conn.execute(
                """
                UPDATE transfers SET state='confirmed',confirmed_at=124,
                  confirmed_block_hash=?,confirmed_block_height=12,
                  confirmations=1,updated_at=124 WHERE transfer_id=?
                """,
                (self.hash(62_001), earning_transfer),
            )
            self.prepare_transfer(conn, fee_transfer, 125, 1)
            conn.execute(
                "UPDATE transfers SET state='uncertain',updated_at=126 WHERE transfer_id=?",
                (earning_transfer,),
            )
            with self.assertRaisesRegex(
                sqlite3.IntegrityError, "uncertain transfer blocks pre-broadcast"
            ):
                self.broadcast_transfer(conn, fee_transfer, 127)

            conn.execute(
                """
                UPDATE transfers SET state='confirmed',confirmed_at=128,
                  confirmed_block_hash=?,confirmed_block_height=13,
                  confirmations=1,updated_at=128 WHERE transfer_id=?
                """,
                (self.hash(62_002), earning_transfer),
            )
            self.broadcast_transfer(conn, fee_transfer, 129)

    def test_wallet_lane_exact_state_matrix(self):
        store, _ = self.make_confirmed_release()
        with managed_connection(store.connect()) as conn:
            queued = self.queue_fee_withdrawal(conn, "fee:lane:queued", 120)
            active = self.queue_fee_withdrawal(conn, "fee:lane:active", 121)
            waiting = self.queue_fee_withdrawal(conn, "fee:lane:waiting", 122)

            # A queued row does not own the wallet lane.
            self.claim_transfer(conn, active, 123)
            with self.assertRaisesRegex(
                sqlite3.IntegrityError,
                r"UNIQUE constraint failed: transfers.wallet_scope",
            ):
                self.claim_transfer(conn, queued, 124)

            conn.execute(
                """
                UPDATE transfers SET state='failed_safe',error_text='no network side effect',
                  updated_at=125 WHERE transfer_id=?
                """,
                (active,),
            )
            # failed_safe also releases the lane.
            self.claim_transfer(conn, queued, 126)
            self.prepare_transfer(conn, queued, 127, 2)
            with self.assertRaisesRegex(
                sqlite3.IntegrityError,
                r"UNIQUE constraint failed: transfers.wallet_scope",
            ):
                self.claim_transfer(conn, waiting, 128)

            self.broadcast_transfer(conn, queued, 129)
            with self.assertRaisesRegex(
                sqlite3.IntegrityError,
                r"UNIQUE constraint failed: transfers.wallet_scope",
            ):
                self.claim_transfer(conn, waiting, 130)

    def test_concurrent_claims_both_fail_while_uncertain_exists(self):
        store, earning_transfer = self.make_confirmed_release()
        with managed_connection(store.connect()) as conn:
            first = self.queue_fee_withdrawal(conn, "fee:race:first", 120)
            second = self.queue_fee_withdrawal(conn, "fee:race:second", 121)
            conn.execute(
                "UPDATE transfers SET state='uncertain',updated_at=122 WHERE transfer_id=?",
                (earning_transfer,),
            )

        barrier = threading.Barrier(2)

        def claim(transfer_id, timestamp):
            conn = store.connect()
            try:
                barrier.wait(timeout=10)
                try:
                    self.claim_transfer(conn, transfer_id, timestamp)
                except sqlite3.IntegrityError as exc:
                    return str(exc)
                return "claimed"
            finally:
                conn.close()

        with ThreadPoolExecutor(max_workers=2) as executor:
            results = list(
                executor.map(claim, (first, second), (123, 124))
            )
        self.assertEqual(
            results,
            [
                "uncertain transfer blocks wallet claim",
                "uncertain transfer blocks wallet claim",
            ],
        )
        with managed_connection(store.connect()) as conn:
            self.assertEqual(
                [
                    tuple(row)
                    for row in conn.execute(
                        "SELECT state FROM transfers WHERE transfer_id IN (?,?) ORDER BY transfer_id",
                        (first, second),
                    )
                ],
                [("queued",), ("queued",)],
            )

    def test_confirmed_transfer_discharges_liability_and_preserves_evidence(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            credit_id = self.fund_and_match(conn, order_id)
            transfer_id = self.queue_release(conn, order_id, credit_id)
            self.confirm_release(conn, transfer_id)
            liability = conn.execute(
                """
                SELECT COALESCE((SELECT SUM(main_units+recovery_units)
                  FROM deposit_credits WHERE credited_at IS NOT NULL),0)
                - COALESCE((SELECT SUM(a.units)
                  FROM transfer_credit_allocations a
                  JOIN transfers t ON t.transfer_id=a.transfer_id
                  WHERE t.state='confirmed'),0)
                """
            ).fetchone()[0]
            earned = conn.execute(
                "SELECT SUM(earned_fee_units) FROM transfers WHERE state='confirmed'"
            ).fetchone()[0]
            self.assertEqual(liability, 0)
            self.assertEqual(earned, 7)
            conn.execute(
                "UPDATE transfers SET confirmations=2,updated_at=110 WHERE transfer_id=?",
                (transfer_id,),
            )
            for sql in (
                "UPDATE transfers SET confirmations=1,updated_at=111 WHERE transfer_id=?",
                "UPDATE transfers SET confirmed_block_hash='" + self.hash(999) + "',updated_at=111 WHERE transfer_id=?",
                "UPDATE transfers SET signed_tx_hex='bb',updated_at=111 WHERE transfer_id=?",
            ):
                with self.subTest(sql=sql), self.assertRaises(sqlite3.IntegrityError):
                    conn.execute(sql, (transfer_id,))
            conn.execute("UPDATE transfers SET state='uncertain',updated_at=111 WHERE transfer_id=?", (transfer_id,))
            conn.execute(
                """
                UPDATE transfers SET state='confirmed',confirmed_at=112,
                  confirmed_block_hash=?,confirmed_block_height=12,
                  confirmations=1,updated_at=112 WHERE transfer_id=?
                """,
                (self.hash(1000), transfer_id),
            )

    def test_nul_hidden_hash_hex_operation_address_and_machine_strings_are_rejected(self):
        store, order_id = self.create_sell()
        with managed_connection(store.connect()) as conn:
            bad_hash = self.hash(1) + "\x00hidden"
            for user_id, username, wallet in (
                (90, "user\x00hidden", "wallet"),
                (91, "user", "wallet\x00hidden"),
            ):
                with self.assertRaisesRegex(sqlite3.IntegrityError, "CHECK constraint failed"):
                    conn.execute(
                        "INSERT INTO users VALUES(?,?,?,1,1)",
                        (user_id, username, wallet),
                    )
            for sql, params in (
                (
                    "INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at) VALUES('btc09-mainnet','a',?,1,1)",
                    (bad_hash,),
                ),
                (
                    "INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at) VALUES('btc09-mainnet',?, ?,1,1)",
                    ("address\x00hidden", self.hash(1)),
                ),
                (
                    "INSERT INTO audit_events(event_type,detail_json,created_at) VALUES(?, '{}',1)",
                    ("event\x00hidden",),
                ),
            ):
                with self.assertRaisesRegex(sqlite3.IntegrityError, "CHECK constraint failed"):
                    conn.execute(sql, params)

            cursor = conn.execute(
                """
                INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at)
                VALUES('btc09-mainnet','deposit-address-7',?,1,1)
                """,
                (self.hash(2),),
            )
            scan_id = cursor.lastrowid
            credit_cases = (
                (bad_hash, "deposit-address-7", self.hash(3), None, None),
                (self.hash(4), "deposit-address-7\x00hidden", self.hash(3), None, None),
                (self.hash(5), "deposit-address-7", bad_hash, None, None),
                (self.hash(6), "deposit-address-7", self.hash(3), bad_hash, self.hash(7)),
                (self.hash(8), "deposit-address-7", self.hash(3), self.hash(7), bad_hash),
            )
            for txid, address, block_hash, spent_txid, spent_block_hash in credit_cases:
                spent_vin = 0 if spent_txid is not None else None
                spent_height = 1 if spent_txid is not None else None
                with self.subTest(
                    txid=txid == bad_hash,
                    address="\x00" in address,
                    block=block_hash == bad_hash,
                    spent_txid=spent_txid == bad_hash,
                    spent_block=spent_block_hash == bad_hash,
                ), self.assertRaisesRegex(sqlite3.IntegrityError, "CHECK constraint failed"):
                    conn.execute(
                        """
                        INSERT INTO deposit_credits(
                          order_id,network,txid,vout,deposit_addr,amount_units,
                          block_hash,block_height,confirmations,coinbase,mature,
                          current_best_chain,spent_by_txid,spent_by_vin,
                          spent_by_block_hash,spent_by_block_height,first_seen_at,
                          last_seen_at,last_seen_scan_id,last_checked_scan_id
                        ) VALUES(1,'btc09-mainnet',?,0,?,1,?,1,1,0,1,1,?,?,?, ?,1,1,?,?)
                        """,
                        (
                            txid,
                            address,
                            block_hash,
                            spent_txid,
                            spent_vin,
                            spent_block_hash,
                            spent_height,
                            scan_id,
                            scan_id,
                        ),
                    )
            # Transfer insert is independently stopped by the machine-string
            # CHECK before it can create a hidden replay identity.
            with self.assertRaises(sqlite3.IntegrityError):
                conn.execute(
                    """
                    INSERT INTO transfers(operation_key,order_id,kind,is_main_outcome,
                      state,amount_units,network_fee_units,earned_fee_units,
                      destination,created_at,updated_at)
                    VALUES(?,?,'refund',1,'queued',107,10,0,'seller-wallet',1,1)
                    """,
                    ("order:1:main\x00hidden", order_id),
                )
            for column, value in (
                ("old_state", "open\x00hidden"),
                ("new_state", "open\x00hidden"),
                ("detail_json", '{"value":"bad\x00hidden"}'),
            ):
                with self.subTest(audit_column=column), self.assertRaisesRegex(
                    sqlite3.IntegrityError, "CHECK constraint failed"
                ):
                    conn.execute(
                        f"INSERT INTO audit_events(event_type,{column},created_at) VALUES('test',?,1)",
                        (value,),
                    )

    def test_every_bounded_schema_identity_has_explicit_nul_defense(self):
        store = self.make_store()
        protected = {
            "users": ("username", "wallet_addr"),
            "orders": (
                "maker_name",
                "buyer_name",
                "seller_name",
                "total_price",
                "settlement_asset",
                "settlement_network",
                "payment_method",
                "deposit_addr",
            ),
            "deposit_scans": ("address", "tip_hash"),
            "deposit_credits": (
                "txid",
                "deposit_addr",
                "block_hash",
                "spent_by_txid",
                "spent_by_block_hash",
            ),
            "transfers": (
                "operation_key",
                "destination",
                "txid",
                "signed_tx_hex",
                "prepared_tip_hash",
                "error_text",
                "confirmed_block_hash",
            ),
            "audit_events": ("event_type", "old_state", "new_state", "detail_json"),
        }
        with managed_connection(store.connect()) as conn:
            for table, columns in protected.items():
                sql = conn.execute(
                    "SELECT sql FROM sqlite_master WHERE type='table' AND name=?",
                    (table,),
                ).fetchone()[0]
                compact = re.sub(r"\s+", "", sql.lower())
                for column in columns:
                    with self.subTest(table=table, column=column):
                        self.assertIn(f"instr({column},char(0))=0", compact)

    def test_prepared_and_confirmed_hash_fields_reject_hidden_nul_suffixes(self):
        store, _ = self.make_confirmed_release()
        with managed_connection(store.connect()) as conn:
            transfer_id = self.queue_fee_withdrawal(conn, "fee:nul:prepared", 120)
            self.claim_transfer(conn, transfer_id, 121)
            valid_txid = self.hash(70_001)
            valid_tip = self.hash(70_002)
            cases = (
                (valid_txid + "\x00hidden", "aa", valid_tip),
                (valid_txid, "aa\x00hidden", valid_tip),
                (valid_txid, "aa", valid_tip + "\x00hidden"),
            )
            for txid, signed_hex, tip_hash in cases:
                with self.subTest(
                    txid=txid != valid_txid,
                    signed=signed_hex != "aa",
                    tip=tip_hash != valid_tip,
                ), self.assertRaisesRegex(
                    sqlite3.IntegrityError, "CHECK constraint failed"
                ):
                    conn.execute(
                        """
                        UPDATE transfers SET state='prepared',txid=?,signed_tx_hex=?,
                          signed_at=122,prepared_tip_hash=?,prepared_tip_height=10,
                          updated_at=122 WHERE transfer_id=?
                        """,
                        (txid, signed_hex, tip_hash, transfer_id),
                    )

            self.prepare_transfer(conn, transfer_id, 123, 3)
            with self.assertRaisesRegex(sqlite3.IntegrityError, "CHECK constraint failed"):
                conn.execute(
                    "UPDATE transfers SET error_text=? WHERE transfer_id=?",
                    ("error\x00hidden", transfer_id),
                )
            with self.assertRaisesRegex(sqlite3.IntegrityError, "CHECK constraint failed"):
                conn.execute(
                    """
                    UPDATE transfers SET state='confirmed',confirmed_at=124,
                      confirmed_block_hash=?,confirmed_block_height=11,
                      confirmations=1,updated_at=124 WHERE transfer_id=?
                    """,
                    (self.hash(70_003) + "\x00hidden", transfer_id),
                )

    def test_public_store_methods_are_keyword_only_and_audit_json_is_stable(self):
        for method in (Store.create_order, Store.get_order, Store.append_audit):
            parameters = list(signature(method).parameters.values())[1:]
            self.assertTrue(parameters)
            self.assertTrue(
                all(parameter.kind is Parameter.KEYWORD_ONLY for parameter in parameters)
            )
        store, order_id = self.create_sell()
        event_id = store.append_audit(
            order_id=order_id,
            actor_id=9,
            event_type="order:created",
            old_state=None,
            new_state=OrderState.AWAITING_DEPOSIT,
            detail={"z": 2, "a": 1},
            created_at=101,
        )
        with managed_connection(store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT detail_json FROM audit_events WHERE event_id=?", (event_id,)).fetchone()[0],
                '{"a":1,"z":2}',
            )

    def test_store_module_cli_requires_db_path_and_prints_bounded_json(self):
        repo_root = Path(__file__).resolve().parents[2]
        env = os.environ.copy()
        env.pop("DB_PATH", None)
        missing = subprocess.run(
            [sys.executable, "-m", "bot.otc.store"],
            cwd=repo_root,
            env=env,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertNotEqual(missing.returncode, 0)
        self.assertLess(len(missing.stderr), 256)
        self.assertEqual(json.loads(missing.stderr)["error"], "DB_PATH is required")

        cli_path = Path(self.tmp.name) / "cli-proof.db"
        env["DB_PATH"] = str(cli_path)
        success = subprocess.run(
            [sys.executable, "-m", "bot.otc.store"],
            cwd=repo_root,
            env=env,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(success.returncode, 0, success.stderr)
        self.assertEqual(success.stderr, "")
        self.assertLess(len(success.stdout), 256)
        self.assertEqual(
            json.loads(success.stdout),
            {"integrity": "ok", "schema_version": 4},
        )
        with managed_connection(sqlite3.connect(cli_path)) as conn:
            self.assertEqual(conn.execute("SELECT version FROM schema_meta").fetchone()[0], 4)

        blocked_path = Path(self.tmp.name) / "cli-blocked.db"
        with managed_connection(sqlite3.connect(blocked_path)) as conn:
            conn.executescript(
                """
                CREATE TABLE schema_meta (
                    id INTEGER PRIMARY KEY,
                    version INTEGER NOT NULL
                );
                INSERT INTO schema_meta(id, version) VALUES(1, 99);
                """
            )
        env["DB_PATH"] = str(blocked_path)
        blocked = subprocess.run(
            [sys.executable, "-m", "bot.otc.store"],
            cwd=repo_root,
            env=env,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(blocked.returncode, 1)
        self.assertEqual(blocked.stdout, "")
        self.assertLess(len(blocked.stderr), 256)
        self.assertEqual(
            json.loads(blocked.stderr),
            {"error": "UnsupportedSchemaVersion"},
        )

    def test_importing_store_module_has_no_cli_side_effect(self):
        repo_root = Path(__file__).resolve().parents[2]
        env = os.environ.copy()
        env["DB_PATH"] = str(Path(self.tmp.name) / "must-not-exist.db")
        result = subprocess.run(
            [sys.executable, "-c", "import bot.otc.store"],
            cwd=repo_root,
            env=env,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")
        self.assertFalse(Path(env["DB_PATH"]).exists())

    def schema_snapshot(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            return conn.execute(
                """
                SELECT type,name,tbl_name,sql FROM sqlite_master
                WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name
                """
            ).fetchall()

    @staticmethod
    def database_dump(path):
        with managed_connection(sqlite3.connect(path)) as conn:
            return "\n".join(conn.iterdump())

    @staticmethod
    def journal_mode(path):
        with managed_connection(sqlite3.connect(path)) as conn:
            return conn.execute("PRAGMA journal_mode").fetchone()[0]


if __name__ == "__main__":
    unittest.main()
