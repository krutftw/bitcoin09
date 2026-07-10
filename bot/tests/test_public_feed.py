from __future__ import annotations

import copy
import http.server
import json
import os
import re
import socket
import tempfile
import threading
import time
import unittest
from decimal import localcontext
from pathlib import Path
from unittest.mock import patch

from bot.otc.domain import (
    MAX_CONFIGURABLE_ACTIVE_ORDERS_TOTAL,
    OrderSide,
    OrderState,
)
from bot.otc.public_feed import (
    DEFAULT_PUBLIC_FEED_PATH,
    MAX_FEED_BYTES,
    build_public_feed,
    public_feed_projection,
    invalidate_public_feed,
    write_public_feed,
)
from bot.otc.store import PUBLIC_TERMINAL_HISTORY_LIMIT, Store
from bot.serve_otc_feed import (
    BoundedThreadingHTTPServer,
    SOCKET_TIMEOUT_SECONDS,
    ValidatedFeedCache,
    build_health_response,
    health_http_status,
    read_feed,
)


class PublicFeedTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.store = Store(Path(self.temporary.name) / "otc.db")
        self.store.initialize()
        self.store.create_order(
            side=OrderSide.SELL,
            maker_id=111_111_111_111_111_111,
            maker_name="PrivateSellerName",
            buyer_id=None,
            buyer_name=None,
            seller_id=111_111_111_111_111_111,
            seller_name="PrivateSellerName",
            net_amount_units=125_000_000,
            network_fee_units=10_000,
            service_fee_units=0,
            deposit_required_units=125_010_000,
            total_price="250.50",
            settlement_asset="AUD",
            settlement_network="TRC20",
            payment_method="PayID",
            state=OrderState.AWAITING_DEPOSIT,
            deposit_addr="private-deposit-address",
            deposit_deadline=1_800_000_000,
            created_at=1_700_000_000,
            updated_at=1_700_000_100,
        )
        self.store.set_user_wallet(
            user_id=111_111_111_111_111_111,
            username="PrivateSellerName",
            wallet_addr="private-wallet-address",
            now=1_700_000_200,
        )
        self.store.append_audit(
            order_id=1,
            actor_id=111_111_111_111_111_111,
            event_type="private_dispute_evidence",
            old_state=OrderState.OPEN,
            new_state=OrderState.OPEN,
            detail={"reason": "PRIVATE DISPUTE TEXT"},
            created_at=1_700_000_300,
        )
        connection = self.store.connect()
        try:
            connection.execute(
                """
                INSERT INTO deposit_scans(
                  network,address,tip_hash,tip_height,observed_at
                ) VALUES(?,?,?,?,?)
                """,
                (
                    "btc09-mainnet",
                    "private-deposit-address",
                    "a" * 64,
                    500,
                    1_700_000_350,
                ),
            )
        finally:
            connection.close()

    @staticmethod
    def healthy_external(*, checked_at: int = 1_700_000_400) -> dict[str, object]:
        return {
            "accepting_orders": True,
            "issues": (),
            "wallet_spendable_units": 1_000_000_000,
            "explorer_snapshot_reachable": True,
            "explorer_tx_status_reachable": True,
            "explorer_tip": {"hash": "a" * 64, "height": 500},
            "checked_at": checked_at,
            "deposit_allocation": {
                "lifetime_count": 0,
                "daily_count": 0,
                "pending_count": 0,
                "lifetime_headroom": 5_000,
                "daily_headroom": 100,
            },
        }

    def test_feed_is_exact_decimal_public_projection(self) -> None:
        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        encoded = json.dumps(payload, sort_keys=True)
        for secret in (
            "111111111111111111",
            "PrivateSellerName",
            "private-wallet-address",
            "private-deposit-address",
            "PRIVATE DISPUTE TEXT",
        ):
            self.assertNotIn(secret, encoded)
        self.assertEqual(payload["schema_version"], 1)
        self.assertEqual(payload["health_timestamp"], 1_700_000_400)
        self.assertEqual(
            payload["summary"],
            {"open": 0, "matched": 0, "completed": 0, "disputed": 0},
        )

        self.assertEqual(
            payload["orders"],
            [
                {
                    "order_id": 1,
                    "side": "sell",
                    "status": "awaiting_deposit",
                    "net_amount_09c": "1.25",
                    "total_price": "250.50",
                    "price_per_09c": "200.4",
                    "asset": "AUD",
                    "settlement_network": "TRC20",
                    "payment_method": "PayID",
                    "created_at": 1_700_000_000,
                    "updated_at": 1_700_000_100,
                    "matched_at": None,
                    "completed_at": None,
                }
            ],
        )

    def test_feed_decimal_math_ignores_ambient_context(self) -> None:
        with localcontext() as context:
            context.prec = 2
            order = build_public_feed(
                self.store, generated_at=1_700_000_400
            )["orders"][0]
        self.assertEqual(order["net_amount_09c"], "1.25")
        self.assertEqual(order["price_per_09c"], "200.4")

    def test_private_labels_are_replaced_and_malformed_price_fails_closed(self) -> None:
        conn = self.store.connect()
        try:
            conn.execute("DROP TRIGGER order_quote_immutable")
            conn.execute(
                "UPDATE orders SET settlement_network='Secret rail', payment_method='Secret method'"
            )
        finally:
            conn.close()
        order = build_public_feed(self.store, generated_at=1_700_000_400)["orders"][0]
        self.assertEqual(order["settlement_network"], "Private settlement network")
        self.assertEqual(order["payment_method"], "Private settlement method")

        class MalformedStore:
            network = "btc09-mainnet"

            def public_feed_snapshot(self):
                connection = self_store.connect()
                try:
                    connection.execute("DROP TRIGGER IF EXISTS order_quote_immutable")
                    connection.execute("PRAGMA ignore_check_constraints=ON")
                    connection.execute("UPDATE orders SET total_price='NaN'")
                finally:
                    connection.close()
                return self_store.public_feed_snapshot()

        self_store = self.store
        with self.assertRaises(ValueError):
            build_public_feed(MalformedStore(), generated_at=1_700_000_400)

    def test_detailed_health_is_local_only_and_complete(self) -> None:
        payload = build_public_feed(
            self.store,
            generated_at=1_700_000_400,
            runtime_health=self.healthy_external(),
        )
        details = payload["health"]
        expected = {
            "integrity",
            "foreign_key_integrity",
            "explorer_snapshot_reachable",
            "explorer_tx_status_reachable",
            "wallet_spendable_units",
            "customer_liability_units",
            "pending_platform_outflow_units",
            "provisional_restricted_units",
            "common_ledger_tip",
            "stale_watched_address_count",
            "gross_fee_units",
            "available_fee_units",
            "negative_fee_invariant",
            "transfer_counts",
            "credited_noncanonical_count",
            "unknown_spend_count",
            "deposit_allocation",
            "accepting_orders",
            "checked_at",
        }
        self.assertEqual(set(details), expected)
        public = public_feed_projection(payload)
        self.assertNotIn("health", public)
        response = build_health_response(payload, now=1_700_000_405)
        self.assertEqual(response["feed_age_seconds"], 5)
        self.assertEqual(response["integrity"], "ok")
        self.assertTrue(response["accepting_orders"])
        self.assertEqual(health_http_status(payload, now=1_700_000_405), 200)

    def test_address_pending_is_internal_and_forces_feed_intake_closed(self) -> None:
        pending_id = self.store.create_order(
            side=OrderSide.SELL,
            maker_id=222,
            maker_name="Pending Seller",
            net_amount_units=100_000_000,
            network_fee_units=10_000,
            service_fee_units=0,
            deposit_required_units=100_010_000,
            total_price="10",
            settlement_asset="AUD",
            settlement_network=None,
            payment_method="PayID",
            state=OrderState.ADDRESS_PENDING,
            created_at=1_700_000_390,
            updated_at=1_700_000_390,
        )
        runtime = self.healthy_external()
        runtime["issues"] = ("address_allocation_pending",)
        runtime["accepting_orders"] = False
        runtime["deposit_allocation"] = {
            "lifetime_count": 1,
            "daily_count": 1,
            "pending_count": 1,
            "lifetime_headroom": 4_999,
            "daily_headroom": 99,
        }
        payload = build_public_feed(
            self.store, generated_at=1_700_000_400, runtime_health=runtime
        )
        self.assertNotIn(pending_id, [order["order_id"] for order in payload["orders"]])
        self.assertNotIn("address_pending", json.dumps(payload, sort_keys=True))
        self.assertFalse(payload["health"]["accepting_orders"])
        self.assertEqual(payload["health"]["deposit_allocation"]["pending_count"], 1)

    def test_accepting_orders_requires_every_health_precondition(self) -> None:
        healthy = self.healthy_external()
        payload = build_public_feed(
            self.store, generated_at=1_700_000_400, runtime_health=healthy
        )
        self.assertTrue(payload["health"]["accepting_orders"])
        for key, bad in (
            ("explorer_snapshot_reachable", False),
            ("explorer_tx_status_reachable", False),
            ("wallet_spendable_units", None),
            ("issues", ("service_failure",)),
            ("explorer_tip", None),
        ):
            with self.subTest(key=key):
                external = dict(healthy)
                external[key] = bad
                result = build_public_feed(
                    self.store,
                    generated_at=1_700_000_400,
                    runtime_health=external,
                )
                self.assertFalse(result["health"]["accepting_orders"])
                self.assertEqual(health_http_status(result, now=1_700_000_401), 503)

    def test_public_and_health_endpoints_reject_stale_future_and_unhealthy(self) -> None:
        target = Path(self.temporary.name) / "feed.json"
        payload = build_public_feed(
            self.store,
            generated_at=1_700_000_400,
            runtime_health=self.healthy_external(),
        )
        write_public_feed(target, payload)
        self.assertEqual(read_feed(target, now=1_700_000_410, max_age_seconds=30), payload)
        with self.assertRaises(ValueError):
            read_feed(target, now=1_700_000_431, max_age_seconds=30)
        with self.assertRaises(ValueError):
            read_feed(
                target,
                now=1_700_000_394,
                max_age_seconds=30,
                max_future_skew_seconds=5,
            )
        payload["health"]["accepting_orders"] = False
        self.assertEqual(health_http_status(payload, now=1_700_000_401), 503)
        self.assertEqual(
            health_http_status(
                payload,
                now=1_700_000_431,
                max_age_seconds=30,
                max_future_skew_seconds=5,
            ),
            503,
        )
        self.assertEqual(
            health_http_status(
                payload,
                now=1_700_000_394,
                max_age_seconds=30,
                max_future_skew_seconds=5,
            ),
            503,
        )

    def test_validated_stat_cache_reuses_parse_but_rechecks_freshness(self) -> None:
        target = Path(self.temporary.name) / "feed.json"
        payload = build_public_feed(
            self.store,
            generated_at=1_700_000_400,
            runtime_health=self.healthy_external(),
        )
        write_public_feed(target, payload)
        cache = ValidatedFeedCache()
        with patch("bot.serve_otc_feed.json.loads", wraps=json.loads) as loads:
            first = read_feed(
                target, now=1_700_000_401, max_age_seconds=30, cache=cache
            )
            first["orders"][0]["status"] = "caller-poisoned"
            first["health"]["transfer_counts"]["queued"] = 999
            second = read_feed(
                target, now=1_700_000_402, max_age_seconds=30, cache=cache
            )
            self.assertEqual(second["orders"][0]["status"], "awaiting_deposit")
            self.assertEqual(second["health"]["transfer_counts"]["queued"], 0)
            self.assertEqual(loads.call_count, 1)
            with self.assertRaises(ValueError):
                read_feed(
                    target, now=1_700_000_431, max_age_seconds=30, cache=cache
                )
            self.assertEqual(loads.call_count, 1)

    def test_feed_server_returns_503_when_worker_capacity_is_saturated(self) -> None:
        entered = threading.Event()
        release = threading.Event()

        class BlockingHandler(http.server.BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                entered.set()
                release.wait(5)
                self.send_response(200)
                self.end_headers()

            def log_message(self, _format: str, *args) -> None:
                pass

        server = BoundedThreadingHTTPServer(
            ("127.0.0.1", 0), BlockingHandler, max_workers=1
        )
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        first = socket.create_connection(server.server_address, timeout=2)
        self.addCleanup(first.close)
        first.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
        self.assertTrue(entered.wait(2))
        second = socket.create_connection(server.server_address, timeout=2)
        try:
            second.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
            response = second.recv(4096)
        finally:
            second.close()
            release.set()
        self.assertIn(b" 503 ", response)

    def test_partial_headers_time_out_and_release_all_worker_slots(self) -> None:
        entered = threading.Event()
        entered_count = 0
        entered_lock = threading.Lock()

        class FastHandler(http.server.BaseHTTPRequestHandler):
            def setup(self) -> None:
                nonlocal entered_count
                super().setup()
                with entered_lock:
                    entered_count += 1
                    if entered_count >= 2:
                        entered.set()

            def do_GET(self) -> None:
                body = b"ok"
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, _format: str, *args) -> None:
                pass

        server = BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            FastHandler,
            max_workers=2,
            socket_timeout_seconds=0.15,
        )
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        partials = [
            socket.create_connection(server.server_address, timeout=2) for _ in range(2)
        ]
        for client in partials:
            self.addCleanup(client.close)
            client.sendall(b"GET / HTTP/1.1\r\nHost:")
        self.assertTrue(entered.wait(2))
        saturated = socket.create_connection(server.server_address, timeout=2)
        try:
            saturated.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
            self.assertIn(b" 503 ", saturated.recv(4096))
        finally:
            saturated.close()

        deadline = time.monotonic() + 2
        response = b""
        while time.monotonic() < deadline and b" 200 " not in response:
            probe = socket.create_connection(server.server_address, timeout=2)
            try:
                probe.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
                response = probe.recv(4096)
            finally:
                probe.close()
            if b" 200 " not in response:
                time.sleep(0.02)
        self.assertIn(b" 200 ", response)

    def test_nonreading_large_response_releases_worker_by_socket_deadline(self) -> None:
        large_started = threading.Event()

        class LargeHandler(http.server.BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                if self.path == "/ping":
                    body = b"ok"
                    self.send_response(200)
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return
                large_started.set()
                self.send_response(200)
                self.send_header("Content-Length", str(64 * 1024 * 1024))
                self.end_headers()
                block = b"x" * 65_536
                for _ in range(1_024):
                    self.wfile.write(block)

            def log_message(self, _format: str, *args) -> None:
                pass

        server = BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            LargeHandler,
            max_workers=1,
            socket_timeout_seconds=0.15,
        )
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        stalled = socket.socket()
        stalled.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1_024)
        stalled.settimeout(2)
        stalled.connect(server.server_address)
        self.addCleanup(stalled.close)
        stalled.sendall(b"GET /large HTTP/1.1\r\nHost: localhost\r\n\r\n")
        self.assertTrue(large_started.wait(2))

        deadline = time.monotonic() + 3
        response = b""
        while time.monotonic() < deadline and b" 200 " not in response:
            probe = socket.create_connection(server.server_address, timeout=2)
            try:
                probe.sendall(b"GET /ping HTTP/1.1\r\nHost: localhost\r\n\r\n")
                response = probe.recv(4096)
            finally:
                probe.close()
            if b" 200 " not in response:
                time.sleep(0.02)
        self.assertIn(b" 200 ", response)

    def test_worker_start_failure_releases_slot_and_closes_request(self) -> None:
        server = BoundedThreadingHTTPServer(
            ("127.0.0.1", 0),
            http.server.BaseHTTPRequestHandler,
            max_workers=1,
            socket_timeout_seconds=0.2,
        )
        self.addCleanup(server.server_close)
        request, peer = socket.socketpair()
        self.addCleanup(peer.close)
        with patch.object(
            http.server.ThreadingHTTPServer,
            "process_request",
            side_effect=RuntimeError("thread start failed"),
        ), self.assertRaisesRegex(RuntimeError, "thread start failed"):
            server.process_request(request, ("127.0.0.1", 1))
        self.assertEqual(request.fileno(), -1)
        self.assertTrue(server._worker_slots.acquire(blocking=False))
        server._worker_slots.release()

    def test_socket_timeout_default_and_bounds_are_strict(self) -> None:
        self.assertEqual(SOCKET_TIMEOUT_SECONDS, 5.0)
        for invalid in (0, 0.09, 30.01, True):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                server = BoundedThreadingHTTPServer(
                    ("127.0.0.1", 0),
                    http.server.BaseHTTPRequestHandler,
                    max_workers=1,
                    socket_timeout_seconds=invalid,
                )
                server.server_close()

    def test_public_projection_rejects_nested_private_or_loose_schema(self) -> None:
        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["orders"][0]["discord_id"] = 111_111_111_111_111_111
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["orders"][0]["price_per_09c"] = "200.5"
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["orders"][0]["updated_at"] = 1_700_000_401
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["summary"]["open"] = 1
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(
            self.store,
            generated_at=1_700_000_400,
            runtime_health=self.healthy_external(),
        )
        payload["health"]["explorer_snapshot_reachable"] = False
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["orders"].append(copy.deepcopy(payload["orders"][0]))
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["orders"][0]["total_price"] = " 250.50 "
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        order = payload["orders"][0]
        order["status"] = "completed"
        order["matched_at"] = 1_700_000_090
        order["completed_at"] = 1_700_000_080
        payload["summary"]["completed"] = 1
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        payload["summary"]["open"] = 1.5
        with self.assertRaises(ValueError):
            public_feed_projection(payload)

    def test_future_health_timestamp_fails_closed(self) -> None:
        payload = build_public_feed(self.store, generated_at=1_700_000_400)
        with self.assertRaises(ValueError):
            build_health_response(
                payload, now=1_700_000_394, max_future_skew_seconds=5
            )

    def test_atomic_write_fsyncs_file_replaces_in_same_directory_and_fsyncs_directory(self) -> None:
        target = Path(self.temporary.name) / "public" / "feed.json"
        payload = {"schema_version": 1, "health_timestamp": 123, "health": {}, "orders": []}
        real_replace = os.replace
        calls: list[tuple[str, str]] = []

        def recording_replace(source: str | os.PathLike[str], destination: str | os.PathLike[str]) -> None:
            calls.append((os.fspath(source), os.fspath(destination)))
            real_replace(source, destination)

        with patch("bot.otc.public_feed.os.replace", side_effect=recording_replace), patch(
            "bot.otc.public_feed.os.fsync", wraps=os.fsync
        ) as fsync:
            write_public_feed(target, payload)
        self.assertEqual(len(calls), 1)
        self.assertEqual(Path(calls[0][0]).parent, target.parent)
        self.assertEqual(Path(calls[0][1]), target)
        self.assertGreaterEqual(fsync.call_count, 1)
        if os.name != "nt":
            self.assertGreaterEqual(fsync.call_count, 2)
        self.assertEqual(json.loads(target.read_text(encoding="utf-8")), payload)
        self.assertEqual(list(target.parent.glob(f".{target.name}.*.tmp")), [])

    def test_atomic_replace_never_exposes_partial_json_to_readers(self) -> None:
        target = Path(self.temporary.name) / "feed.json"
        write_public_feed(target, {"generation": 0, "orders": []})
        failures: list[BaseException] = []
        done = threading.Event()

        def reader() -> None:
            while not done.is_set():
                try:
                    value = json.loads(target.read_bytes())
                    if value.get("generation") not in range(101):
                        raise AssertionError("unexpected generation")
                except PermissionError:
                    continue
                except BaseException as exc:
                    failures.append(exc)
                    done.set()

        thread = threading.Thread(target=reader)
        thread.start()
        try:
            for generation in range(1, 101):
                write_public_feed(
                    target,
                    {"generation": generation, "orders": [{"x": "y" * 10_000}]},
                )
        finally:
            done.set()
            thread.join(timeout=5)
        self.assertFalse(failures)

    def test_posix_directory_open_and_fsync_failures_propagate(self) -> None:
        target = Path(self.temporary.name) / "feed.json"
        payload = {"generation": 1}
        real_open = os.open

        def fail_directory_open(path, flags, mode=0o777):
            if Path(path) == target.parent:
                raise OSError("directory open failed")
            return real_open(path, flags, mode)

        with patch("bot.otc.public_feed._DIRECTORY_FSYNC_SUPPORTED", True), patch(
            "bot.otc.public_feed.os.open", side_effect=fail_directory_open
        ), self.assertRaises(OSError):
            write_public_feed(target, payload)

        with patch("bot.otc.public_feed._DIRECTORY_FSYNC_SUPPORTED", True), patch(
            "bot.otc.public_feed._fsync_parent_directory",
            side_effect=OSError("directory fsync failed"),
        ), self.assertRaises(OSError):
            write_public_feed(target, payload)

    def test_invalidate_removes_stale_feed_durably(self) -> None:
        target = Path(self.temporary.name) / "feed.json"
        target.write_text("stale", encoding="utf-8")
        invalidate_public_feed(target)
        self.assertFalse(target.exists())

    def test_store_public_feed_snapshot_is_one_read_transaction(self) -> None:
        reached = threading.Event()
        release = threading.Event()

        class CoordinatedStore(Store):
            def _public_feed_checkpoint(self, phase: str) -> None:
                if phase == "after_orders":
                    reached.set()
                    self.assert_release()

            @staticmethod
            def assert_release() -> None:
                if not release.wait(5):
                    raise RuntimeError("writer did not finish")

        db_path = Path(self.temporary.name) / "snapshot.db"
        store = CoordinatedStore(db_path)
        store.initialize()
        store.create_order(
            side=OrderSide.BUY,
            maker_id=7,
            maker_name="Buyer",
            buyer_id=7,
            buyer_name="Buyer",
            seller_id=None,
            seller_name=None,
            net_amount_units=100_000_000,
            network_fee_units=10_000,
            service_fee_units=0,
            deposit_required_units=100_010_000,
            total_price="10",
            settlement_asset="AUD",
            settlement_network=None,
            payment_method="PayID",
            state=OrderState.OPEN,
            deposit_addr=None,
            deposit_deadline=None,
            created_at=10,
            updated_at=10,
        )
        result: list[object] = []
        reader = threading.Thread(target=lambda: result.append(store.public_feed_snapshot()))
        reader.start()
        self.assertTrue(reached.wait(5))
        Store(db_path).create_order(
            side=OrderSide.BUY,
            maker_id=8,
            maker_name="Other Buyer",
            buyer_id=8,
            buyer_name="Other Buyer",
            seller_id=None,
            seller_name=None,
            net_amount_units=100_000_000,
            network_fee_units=10_000,
            service_fee_units=0,
            deposit_required_units=100_010_000,
            total_price="20",
            settlement_asset="USD",
            settlement_network=None,
            payment_method="Wise",
            state=OrderState.OPEN,
            deposit_addr=None,
            deposit_deadline=None,
            created_at=11,
            updated_at=11,
        )
        release.set()
        reader.join(5)
        self.assertFalse(reader.is_alive())
        snapshot = result[0]
        self.assertEqual(len(snapshot["orders"]), 1)
        self.assertEqual(snapshot["summary"]["open"], 1)

    def test_store_projection_caps_recent_terminal_history_but_keeps_actionable(self) -> None:
        db_path = Path(self.temporary.name) / "bounded.db"

        class TracedStore(Store):
            statements: list[str] = []

            def connect(self):
                connection = super().connect()
                connection.set_trace_callback(self.statements.append)
                return connection

        store = TracedStore(db_path)
        store.initialize()
        fixture_connection = store.connect()
        try:
            fixture_connection.execute("DROP TRIGGER order_state_machine")
        finally:
            fixture_connection.close()
        for offset in range(105):
            order_id = store.create_order(
                side=OrderSide.BUY,
                maker_id=10_000 + offset,
                maker_name=f"Buyer {offset}",
                buyer_id=10_000 + offset,
                buyer_name=f"Buyer {offset}",
                seller_id=None,
                seller_name=None,
                net_amount_units=100_000_000,
                network_fee_units=10_000,
                service_fee_units=0,
                deposit_required_units=100_010_000,
                total_price="10",
                settlement_asset="AUD",
                settlement_network=None,
                payment_method="PayID",
                state=OrderState.OPEN,
                deposit_addr=None,
                deposit_deadline=None,
                created_at=100 + offset,
                updated_at=100 + offset,
            )
            connection = store.connect()
            try:
                connection.execute(
                    """
                    UPDATE orders
                    SET state='completed',completed_at=?,updated_at=?
                    WHERE order_id=?
                    """,
                    (1_000 + offset, 1_000 + offset, order_id),
                )
            finally:
                connection.close()
        actionable = store.create_order(
            side=OrderSide.BUY,
            maker_id=20_000,
            maker_name="Actionable Buyer",
            buyer_id=20_000,
            buyer_name="Actionable Buyer",
            seller_id=None,
            seller_name=None,
            net_amount_units=100_000_000,
            network_fee_units=10_000,
            service_fee_units=0,
            deposit_required_units=100_010_000,
            total_price="20",
            settlement_asset="USD",
            settlement_network=None,
            payment_method="Wise",
            state=OrderState.OPEN,
            deposit_addr=None,
            deposit_deadline=None,
            created_at=500,
            updated_at=500,
        )
        store.statements.clear()
        snapshot = store.public_feed_snapshot()
        ids = [order["order_id"] for order in snapshot["orders"]]
        self.assertEqual(len(ids), 101)
        self.assertEqual(ids, [*range(6, 106), actionable])
        self.assertEqual(snapshot["summary"]["open"], 1)
        self.assertEqual(snapshot["summary"]["completed"], 105)
        payload = build_public_feed(store, generated_at=2_000)
        self.assertEqual(len(payload["orders"]), 101)
        self.assertEqual(payload["summary"]["completed"], 105)
        payload["summary"]["completed"] = 99
        with self.assertRaises(ValueError):
            public_feed_projection(payload)
        projection_query = next(
            statement
            for statement in store.statements
            if "SELECT * FROM (" in statement and "FROM orders" in statement
        )
        self.assertIn("LIMIT 100", projection_query)
        self.assertIn("WHERE state NOT IN", projection_query)
        self.assertIn("WHERE state IN", projection_query)
        aggregate_query = next(
            statement
            for statement in store.statements
            if "COUNT(*)" in statement and "GROUP BY state" in statement
        )
        self.assertIn("WHERE state IN", aggregate_query)

    def test_nginx_has_edge_limits_without_duplicate_cache_or_cors_headers(self) -> None:
        config = Path("deploy/nginx/bitcoin09.conf").read_text(encoding="utf-8")
        for required in ("limit_req_zone", "limit_conn_zone", "limit_req ", "limit_conn "):
            self.assertIn(required, config)
        self.assertIn("X-Content-Type-Options nosniff always", config)
        self.assertNotIn("add_header Cache-Control", config)
        self.assertNotIn("add_header Access-Control-Allow-Origin", config)
        expected_cloudflare_ranges = {
            "173.245.48.0/20",
            "103.21.244.0/22",
            "103.22.200.0/22",
            "103.31.4.0/22",
            "141.101.64.0/18",
            "108.162.192.0/18",
            "190.93.240.0/20",
            "188.114.96.0/20",
            "197.234.240.0/22",
            "198.41.128.0/17",
            "162.158.0.0/15",
            "104.16.0.0/13",
            "104.24.0.0/14",
            "172.64.0.0/13",
            "131.0.72.0/22",
            "2400:cb00::/32",
            "2606:4700::/32",
            "2803:f800::/32",
            "2405:b500::/32",
            "2405:8100::/32",
            "2a06:98c0::/29",
            "2c0f:f248::/32",
        }
        configured_ranges = set(
            re.findall(r"^set_real_ip_from\s+([^;]+);$", config, re.MULTILINE)
        )
        self.assertEqual(configured_ranges, expected_cloudflare_ranges)
        self.assertIn("real_ip_header CF-Connecting-IP;", config)
        self.assertIn("real_ip_recursive on;", config)

    def test_max_configured_feed_capacity_has_conservative_size_headroom(self) -> None:
        worst_public_order = {
            "order_id": 9_223_372_036_854_775_807,
            "side": "sell",
            "status": "awaiting_deposit",
            "net_amount_09c": "21000000",
            "total_price": "9" * 64,
            "price_per_09c": "9" * 64,
            "asset": "A" * 12,
            "settlement_network": "Private settlement network",
            "payment_method": "Private settlement method",
            "created_at": 9_223_372_036_854_775_807,
            "updated_at": 9_223_372_036_854_775_807,
            "matched_at": 9_223_372_036_854_775_807,
            "completed_at": None,
        }
        conservative_order_bytes = 2_048
        conservative_envelope_bytes = 65_536
        self.assertLess(
            len(json.dumps(worst_public_order, separators=(",", ":")).encode()),
            conservative_order_bytes,
        )
        maximum_feed_bound = (
            MAX_CONFIGURABLE_ACTIVE_ORDERS_TOTAL + PUBLIC_TERMINAL_HISTORY_LIMIT
        ) * conservative_order_bytes + conservative_envelope_bytes
        self.assertLess(maximum_feed_bound, MAX_FEED_BYTES)

    def test_default_path_is_not_under_opt(self) -> None:
        self.assertEqual(
            DEFAULT_PUBLIC_FEED_PATH,
            Path("/var/lib/btc09-otc/public/otc-bot-feed.json"),
        )


if __name__ == "__main__":
    unittest.main()
