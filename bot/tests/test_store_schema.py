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

    def test_live_empty_prototype_migrates_and_preserves_user_and_archive(self):
        with managed_connection(sqlite3.connect(self.path)) as conn:
            conn.executescript(LIVE_PROTOTYPE_SCHEMA)
            conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
        with managed_connection(sqlite3.connect(self.path)) as conn:
            self.assertEqual(
                conn.execute("PRAGMA journal_mode=WAL").fetchone()[0], "wal"
            )

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
        for existing_archive in (False, True):
            with self.subTest(existing_archive=existing_archive):
                path = Path(self.tmp.name) / f"v3-{existing_archive}.db"
                with managed_connection(sqlite3.connect(path)) as conn:
                    conn.executescript(_V3_SCHEMA_SOURCE)
                    conn.execute("INSERT INTO users VALUES(7, 'Pilot', NULL, 1, 2)")
                    if existing_archive:
                        conn.execute(
                            "CREATE TABLE orders_v2_archive(order_id INTEGER PRIMARY KEY)"
                        )
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
        with self.assertRaisesRegex(MigrationBlocked, "non-exact v3"):
            Store(self.path).initialize()
        self.assertEqual(self.database_dump(self.path), before)
        self.assertEqual(self.journal_mode(self.path), "delete")

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
                with self.assertRaises(sqlite3.IntegrityError):
                    conn.execute(sql, params)
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
