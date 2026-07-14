from __future__ import annotations

import hashlib
import json
import multiprocessing
import os
import shutil
import tempfile
import threading
import time
import unittest
from collections.abc import Sequence
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from pathlib import Path

from bot.otc.domain import MAX_09C_UNITS, OrderSide, OrderState
from bot.otc.explorer import (
    AddressBatch,
    AddressSnapshot,
    BlockAnchor,
    ConfirmedOutput,
    ExplorerProtocolError,
    Tip,
    TransactionStatus,
)
from bot.otc.service import AuthorizationError, OrderResult, TradeService
from bot.otc.store import AccountingInvariantError, Store
from bot.otc.wallet import (
    BroadcastResult,
    PreparedTransfer,
    SafeSendFailure,
    UncertainSendFailure,
    WalletInvariantError,
    WalletAllocationLock,
    WalletOutpoint,
    WalletSnapshot,
)


def h(number: int) -> str:
    return f"{number:064x}"


_B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def address(number: int, *, version: int = 0x09) -> str:
    payload = bytes([version]) + number.to_bytes(20, "big")
    checksum = hashlib.sha256(hashlib.sha256(payload).digest()).digest()[:4]
    raw = payload + checksum
    value = int.from_bytes(raw, "big")
    encoded = ""
    while value:
        value, remainder = divmod(value, 58)
        encoded = _B58[remainder] + encoded
    for byte in raw:
        if byte:
            break
        encoded = "1" + encoded
    return encoded


class Clock:
    def __init__(self, value: int = 1_000) -> None:
        self.value = value
        self.lock = threading.Lock()

    def __call__(self) -> int:
        with self.lock:
            self.value += 1
            return self.value


class FakeExplorer:
    network = "btc09-regtest"

    def __init__(self) -> None:
        self.current_tip = Tip(h(900), 100)
        self.outputs_by_address: dict[str, tuple[ConfirmedOutput, ...]] = {}
        self.transactions: dict[str, TransactionStatus] = {}
        self.batch_calls = 0
        self.lock = threading.Lock()

    def set_outputs(
        self,
        address: str,
        amounts: Sequence[int],
        *,
        confirmations: int = 6,
        start: int | None = None,
    ) -> None:
        if start is None:
            start = sum(address.encode("ascii"))
        outputs: list[ConfirmedOutput] = []
        for offset, amount in enumerate(amounts):
            height = self.current_tip.height - confirmations + 1
            outputs.append(
                ConfirmedOutput(
                    txid=h(10_000 + start + offset),
                    transaction_index=offset,
                    vout=offset,
                    amount_units=amount,
                    block=BlockAnchor(h(20_000 + start + offset), height),
                    confirmations=confirmations,
                    coinbase=False,
                    mature=True,
                    spent_by=None,
                )
            )
        with self.lock:
            self.outputs_by_address[address] = tuple(outputs)

    def batch_outputs(self, read_watched_addresses: object) -> AddressBatch:
        addresses = tuple(read_watched_addresses())  # type: ignore[operator]
        with self.lock:
            self.batch_calls += 1
            snapshots = tuple(
                AddressSnapshot(
                    self.network,
                    address,
                    True,
                    self.current_tip,
                    self.outputs_by_address.get(address, ()),
                )
                for address in addresses
            )
        if tuple(read_watched_addresses()) != addresses:  # type: ignore[operator]
            raise RuntimeError("watched set changed")
        return AddressBatch(self.network, self.current_tip, snapshots)

    def tip(self) -> Tip:
        return self.current_tip

    def transaction(self, txid: str) -> TransactionStatus:
        return self.transactions.get(
            txid,
            TransactionStatus(txid, "unknown", None, 0, self.current_tip),
        )

    def set_transaction(
        self,
        txid: str,
        status: str,
        *,
        confirmations: int = 0,
        block_number: int = 70_000,
    ) -> None:
        block = None
        if status == "confirmed":
            height = self.current_tip.height - confirmations + 1
            block = BlockAnchor(h(block_number), height)
        self.transactions[txid] = TransactionStatus(
            txid, status, block, confirmations, self.current_tip
        )


class FakeWallet:
    network = "btc09-regtest"

    def __init__(self, explorer: FakeExplorer) -> None:
        self.explorer = explorer
        self.address_count = 0
        self.primary_address = address(9_999)
        self.addresses = [self.primary_address]
        self._allocation_tmp = tempfile.mkdtemp()
        self.allocation_lock_path = str(
            Path(self._allocation_tmp) / "wallet.json.allocation.lock"
        )
        self.prepare_calls: list[tuple[object, ...]] = []
        self.broadcast_calls: list[tuple[str, str, Tip]] = []
        self.prepare_error: BaseException | None = None
        self.prepare_side_effect: object | None = None
        self.broadcast_error: BaseException | None = None
        self.broadcast_entered: threading.Event | None = None
        self.broadcast_release: threading.Event | None = None
        self.lock = threading.Lock()

    def allocation_lock(self, *, timeout=5.0):
        return WalletAllocationLock(self.allocation_lock_path, acquire_timeout=timeout)

    def __del__(self):
        allocation_tmp = getattr(self, "_allocation_tmp", None)
        if allocation_tmp is not None:
            shutil.rmtree(allocation_tmp, ignore_errors=True)

    def new_address(self) -> str:
        with self.lock:
            self.address_count += 1
            created = address(10_000 + self.address_count)
            self.addresses.append(created)
            return created

    def snapshot(self, expected_tip: Tip) -> WalletSnapshot:
        outputs: list[WalletOutpoint] = []
        owners = tuple(
            sorted(set(self.addresses) | set(self.explorer.outputs_by_address))
        )
        for owner in owners:
            for output in self.explorer.outputs_by_address.get(owner, ()):
                outputs.append(
                    WalletOutpoint(
                        outpoint=f"{output.txid}:{output.vout}",
                        txid=output.txid,
                        vout=output.vout,
                        amount_units=output.amount_units,
                        address=owner,
                        owner_address_index=owners.index(owner),
                    )
                )
        reserve_txid = h(88_888)
        outputs.append(
            WalletOutpoint(
                outpoint=f"{reserve_txid}:0",
                txid=reserve_txid,
                vout=0,
                amount_units=10_000_000_000,
                address=self.primary_address,
                owner_address_index=owners.index(self.primary_address),
            )
        )
        outputs.sort(key=lambda item: (bytes.fromhex(item.txid), item.vout))
        return WalletSnapshot(
            self.network,
            expected_tip,
            self.primary_address,
            owners,
            tuple(outputs),
            sum(output.amount_units for output in outputs),
            h(777),
        )

    def prepare(
        self,
        destination: str,
        amount_units: int,
        fee_units: int,
        expected_tip: Tip,
        restricted_outpoints: Sequence[str],
        expected_snapshot: WalletSnapshot,
    ) -> PreparedTransfer:
        with self.lock:
            self.prepare_calls.append(
                (
                    destination,
                    amount_units,
                    fee_units,
                    expected_tip,
                    tuple(restricted_outpoints),
                    expected_snapshot,
                )
            )
            error = self.prepare_error
            side_effect = self.prepare_side_effect
            call_number = len(self.prepare_calls)
        if error is not None:
            raise error
        raw_hex = f"010203{3 + call_number:02x}"
        txid = hashlib.sha256(
            hashlib.sha256(bytes.fromhex(raw_hex)).digest()
        ).hexdigest()
        prepared = PreparedTransfer(
            txid,
            raw_hex,
            destination,
            amount_units,
            fee_units,
            expected_tip,
            expected_snapshot.wallet_snapshot_hash,
            tuple(
                item.outpoint
                for item in expected_snapshot.outpoints
                if item.outpoint not in restricted_outpoints
            )[:1],
        )
        if callable(side_effect):
            side_effect()
        return prepared

    def broadcast(
        self, signed_tx_hex: str, expected_txid: str, prepared_tip: Tip
    ) -> BroadcastResult:
        with self.lock:
            self.broadcast_calls.append((signed_tx_hex, expected_txid, prepared_tip))
            error = self.broadcast_error
            entered = self.broadcast_entered
            release = self.broadcast_release
        if entered is not None:
            entered.set()
        if release is not None and not release.wait(timeout=5):
            raise RuntimeError("broadcast test gate timed out")
        if error is not None:
            raise error
        self.explorer.set_transaction(expected_txid, "mempool")
        return BroadcastResult(
            expected_txid,
            "mempool",
            1,
            True,
            self.explorer.current_tip,
        )


class ProcessFileWallet:
    network = "btc09-regtest"

    def __init__(self, state_path, allocation_lock_path, *, die_after_new=False):
        self.state_path = state_path
        self.allocation_lock_path = allocation_lock_path
        self.die_after_new = die_after_new

    def allocation_lock(self, *, timeout=5.0):
        return WalletAllocationLock(self.allocation_lock_path, acquire_timeout=timeout)

    def _addresses(self):
        with open(self.state_path, encoding="ascii") as handle:
            return json.load(handle)["addresses"]

    def snapshot(self, expected_tip):
        addresses = tuple(sorted(self._addresses()))
        return WalletSnapshot(
            self.network,
            expected_tip,
            address(49_999),
            addresses,
            (),
            0,
            h(49_999),
        )

    def new_address(self):
        addresses = self._addresses()
        created = address(50_000 + len(addresses))
        addresses.append(created)
        temporary = self.state_path + ".tmp"
        with open(temporary, "w", encoding="ascii") as handle:
            json.dump({"addresses": addresses}, handle)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, self.state_path)
        if self.die_after_new:
            os._exit(17)
        return created


def _multiprocess_allocation_service_worker(
    db_path,
    wallet_state_path,
    allocation_lock_path,
    barrier,
    queue,
    die_after_new=False,
):
    try:
        explorer = FakeExplorer()
        wallet = ProcessFileWallet(
            wallet_state_path,
            allocation_lock_path,
            die_after_new=die_after_new,
        )
        service = TradeService(
            store=Store(db_path, network="btc09-regtest"),
            explorer=explorer,
            wallet=wallet,
            fresh_address=wallet.new_address,
            confirmation_depth=6,
            clock=Clock(60_000),
            network_fee_units=10,
        )
        barrier.wait(timeout=15)
        try:
            result = service.reconcile_pending_address()
            queue.put(("ok", None if result is None else result["order_id"]))
        except BaseException as exc:
            queue.put(("error", type(exc).__name__))
    finally:
        queue.close()
        queue.join_thread()


class TradeServiceOrderTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(Path(self.tmp.name) / "otc.db", network="btc09-regtest")
        self.store.initialize()
        self.explorer = FakeExplorer()
        self.wallet = FakeWallet(self.explorer)
        self.clock = Clock()
        self.service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            fee_bps=0,
            deposit_timeout_seconds=600,
        )

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def create_sell(self, *, seller_id: int = 1):
        return self.service.create_sell(
            seller_id=seller_id,
            seller_name=f"Seller {seller_id}",
            receive_address=address(20_000 + seller_id),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )

    def test_accept_missing_order_points_to_bot_order_list(self):
        with self.assertRaisesRegex(ValueError, r"bot escrow.*?/trade list"):
            self.service.accept(7, actor_id=2, actor_name="Buyer 2")

    def test_wtb_creation_spam_is_stopped_by_durable_maker_capacity(self):
        capped = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            fee_bps=0,
            deposit_timeout_seconds=600,
            max_active_orders_total=3,
            max_active_orders_per_maker=2,
        )
        for _ in range(2):
            capped.create_buy(
                buyer_id=77,
                buyer_name="Buyer 77",
                receive_address=address(40_077),
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        with self.assertRaisesRegex(ValueError, "maker active order capacity"):
            capped.create_buy(
                buyer_id=77,
                buyer_name="Buyer 77",
                receive_address=address(40_077),
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0], 2)

    def test_capped_wts_rejection_allocates_no_wallet_address(self):
        capped = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            max_active_orders_total=1,
            max_active_orders_per_maker=1,
        )
        capped.create_buy(
            buyer_id=70,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        before = self.wallet.address_count
        with self.assertRaisesRegex(ValueError, "total active order capacity"):
            capped.create_sell(
                seller_id=71,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        self.assertEqual(self.wallet.address_count, before)

    def test_simultaneous_wts_last_total_or_maker_slot_allocates_one_address(self):
        for capacity_kind in ("total", "maker"):
            with self.subTest(capacity_kind=capacity_kind), tempfile.TemporaryDirectory() as root:
                store = Store(Path(root) / "capacity.db", network="btc09-regtest")
                store.initialize()
                explorer = FakeExplorer()
                wallet = FakeWallet(explorer)
                clock = Clock()
                maximum_total = 2 if capacity_kind == "total" else 3
                maximum_per_maker = 2
                service = TradeService(
                    store=store,
                    explorer=explorer,
                    wallet=wallet,
                    fresh_address=wallet.new_address,
                    confirmation_depth=6,
                    clock=clock,
                    network_fee_units=10,
                    max_active_orders_total=maximum_total,
                    max_active_orders_per_maker=maximum_per_maker,
                )
                existing_maker = 80
                service.create_buy(
                    buyer_id=existing_maker,
                    net_amount=100,
                    total_price="2",
                    asset="AUD",
                    method="PayID",
                    network=None,
                )
                contenders = (
                    (81, 82) if capacity_kind == "total" else (existing_maker,) * 2
                )
                barrier = threading.Barrier(2)
                outcomes = []
                outcome_lock = threading.Lock()

                def create(seller_id):
                    barrier.wait(timeout=5)
                    try:
                        outcome = service.create_sell(
                            seller_id=seller_id,
                            net_amount=100,
                            total_price="2",
                            asset="AUD",
                            method="PayID",
                            network=None,
                        )
                    except BaseException as exc:
                        outcome = exc
                    with outcome_lock:
                        outcomes.append(outcome)

                threads = [
                    threading.Thread(target=create, args=(seller_id,))
                    for seller_id in contenders
                ]
                for thread in threads:
                    thread.start()
                for thread in threads:
                    thread.join(5)
                    self.assertFalse(thread.is_alive())
                self.assertEqual(sum(isinstance(item, OrderResult) for item in outcomes), 1)
                self.assertEqual(sum(isinstance(item, ValueError) for item in outcomes), 1)
                self.assertEqual(wallet.address_count, 1)
                with closing(store.connect()) as conn:
                    self.assertEqual(
                        conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0], 2
                    )

    def test_wts_address_factory_failure_rolls_back_without_raw_wallet_error(self):
        def fail_address():
            raise RuntimeError("raw-wallet-secret-must-not-escape")

        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=fail_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        with self.assertRaises(SafeSendFailure) as raised:
            service.create_sell(
                seller_id=91,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        self.assertEqual(str(raised.exception), "wallet address allocation failed safely")
        self.assertNotIn("raw-wallet-secret", str(raised.exception))
        with closing(self.store.connect()) as conn:
            self.assertEqual(conn.execute("SELECT COUNT(*) FROM users").fetchone()[0], 1)
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM orders WHERE state='address_pending'"
                ).fetchone()[0],
                1,
            )

        valid = address(12_346)
        invalid_checksum = valid[:-1] + ("1" if valid[-1] != "1" else "2")
        for malformed_address in (address(12_346, version=0), invalid_checksum):
            with self.subTest(malformed_address=malformed_address), tempfile.TemporaryDirectory() as root:
                store = Store(Path(root) / "malformed.db", network="btc09-regtest")
                store.initialize()
                malformed = TradeService(
                    store=store,
                    explorer=self.explorer,
                    wallet=self.wallet,
                    fresh_address=lambda value=malformed_address: value,
                    confirmation_depth=6,
                    clock=self.clock,
                    network_fee_units=10,
                )
                with self.assertRaisesRegex(
                    SafeSendFailure, "wallet address allocation failed safely"
                ):
                    malformed.create_sell(
                        seller_id=91,
                        net_amount=100,
                        total_price="2",
                        asset="AUD",
                        method="PayID",
                        network=None,
                    )
                with closing(store.connect()) as conn:
                    self.assertEqual(
                        conn.execute("SELECT COUNT(*) FROM users").fetchone()[0], 1
                    )
                    self.assertEqual(
                        conn.execute(
                            "SELECT COUNT(*) FROM orders WHERE state='address_pending'"
                        ).fetchone()[0],
                        1,
                    )

    def test_valid_wts_factory_address_is_canonical_and_persisted(self):
        expected = address(12_345)

        def persisted_address():
            self.wallet.addresses.append(expected)
            return expected

        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=persisted_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        created = service.create_sell(
            seller_id=92,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        stored = self.store.get_order(order_id=created.order_id)
        self.assertEqual(stored["deposit_addr"], expected)

    def test_wtb_ten_way_accept_race_reserves_and_allocates_one_address(self):
        order = self.service.create_buy(
            buyer_id=100,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        barrier = threading.Barrier(10)

        def accept(seller_id):
            barrier.wait(timeout=5)
            return self.service.accept(order.order_id, actor_id=seller_id)

        with ThreadPoolExecutor(max_workers=10) as pool:
            results = list(pool.map(accept, range(101, 111)))
        self.assertEqual(sum(result.accepted for result in results), 1)
        self.assertEqual(self.wallet.address_count, 1)
        stored = self.store.get_order(order_id=order.order_id)
        self.assertEqual(stored["state"], "awaiting_deposit")
        self.assertIsNotNone(stored["deposit_allocation_reserved_at"])

    def test_wallet_allocation_delay_never_holds_sqlite_writer_lane(self):
        entered = threading.Event()
        release = threading.Event()

        def delayed_address():
            entered.set()
            if not release.wait(5):
                raise RuntimeError("address test timed out")
            created = address(20_001)
            self.wallet.addresses.append(created)
            return created

        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=delayed_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        result = []
        creator = threading.Thread(
            target=lambda: result.append(
                service.create_sell(
                    seller_id=120,
                    net_amount=100,
                    total_price="2",
                    asset="AUD",
                    method="PayID",
                    network=None,
                )
            )
        )
        creator.start()
        self.assertTrue(entered.wait(2))
        writer_done = threading.Event()
        writer = threading.Thread(
            target=lambda: (
                self.store.set_user_wallet(
                    user_id=121,
                    username="Unrelated",
                    wallet_addr=address(20_121),
                    now=2_000,
                ),
                writer_done.set(),
            )
        )
        writer.start()
        try:
            completed_without_wallet = writer_done.wait(0.5)
        finally:
            release.set()
        creator.join(5)
        writer.join(5)
        self.assertTrue(completed_without_wallet)
        self.assertFalse(creator.is_alive())
        self.assertFalse(writer.is_alive())
        self.assertEqual(len(result), 1)

    def test_thirty_create_cancel_allocations_exhaust_budget_without_refund(self):
        class FixedClock:
            value = 10_000

            def __call__(self):
                return self.value

        clock = FixedClock()
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=clock,
            network_fee_units=10,
            max_deposit_allocations_lifetime_total=30,
            max_deposit_allocations_lifetime_per_seller=30,
            max_deposit_allocations_daily_total=30,
            max_deposit_allocations_daily_per_seller=30,
        )
        for _ in range(30):
            created = service.create_sell(
                seller_id=130,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
            service.cancel(created.order_id, actor_id=130)
        with self.assertRaisesRegex(ValueError, "deposit allocation global lifetime"):
            service.create_sell(
                seller_id=130,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        self.assertEqual(self.wallet.address_count, 30)
        self.assertEqual(len(self.store.watched_deposit_addresses()), 30)
        usage = self.store.deposit_allocation_usage(now=clock.value)
        self.assertEqual(usage["lifetime_count"], 30)
        self.assertEqual(usage["daily_count"], 30)

    def test_rolling_allocation_window_has_no_midnight_boundary_bypass(self):
        class FixedClock:
            value = 86_399

            def __call__(self):
                return self.value

        clock = FixedClock()
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=clock,
            network_fee_units=10,
            max_deposit_allocations_lifetime_total=3,
            max_deposit_allocations_lifetime_per_seller=3,
            max_deposit_allocations_daily_total=1,
            max_deposit_allocations_daily_per_seller=1,
        )
        first = service.create_sell(
            seller_id=140,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        service.cancel(first.order_id, actor_id=140)
        clock.value = 86_400
        with self.assertRaisesRegex(ValueError, "daily"):
            service.create_sell(
                seller_id=140,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        clock.value = 172_798
        with self.assertRaisesRegex(ValueError, "daily"):
            service.create_sell(
                seller_id=140,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        clock.value = 172_799
        second = service.create_sell(
            seller_id=140,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.assertEqual(second.state, "awaiting_deposit")
        self.assertEqual(self.wallet.address_count, 2)

    def test_seller_and_global_allocation_boundaries_consume_on_reservation(self):
        class FixedClock:
            def __call__(self):
                return 50_000

        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=FixedClock(),
            network_fee_units=10,
            max_deposit_allocations_lifetime_total=3,
            max_deposit_allocations_lifetime_per_seller=1,
            max_deposit_allocations_daily_total=2,
            max_deposit_allocations_daily_per_seller=1,
        )

        def allocate(seller_id):
            result = service.create_sell(
                seller_id=seller_id,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
            service.cancel(result.order_id, actor_id=seller_id)

        allocate(141)
        with self.assertRaisesRegex(ValueError, "seller lifetime"):
            allocate(141)
        allocate(142)
        with self.assertRaisesRegex(ValueError, "global daily"):
            allocate(143)
        self.assertEqual(self.wallet.address_count, 2)

    def test_wtb_accept_cancel_does_not_refund_seller_allocation_budget(self):
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            max_deposit_allocations_lifetime_total=2,
            max_deposit_allocations_lifetime_per_seller=1,
            max_deposit_allocations_daily_total=2,
            max_deposit_allocations_daily_per_seller=1,
        )
        first = service.create_buy(
            buyer_id=180,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        accepted = service.accept(first.order_id, actor_id=181)
        service.cancel(accepted.order_id, actor_id=181)
        second = service.create_buy(
            buyer_id=182,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        before = self.wallet.address_count
        with self.assertRaisesRegex(ValueError, "seller lifetime"):
            service.accept(second.order_id, actor_id=181)
        self.assertEqual(self.wallet.address_count, before)

    def test_pending_restart_recovers_persisted_address_and_full_deadline(self):
        now = 200_000
        order_id = self.store.create_order(
            side=OrderSide.SELL,
            maker_id=150,
            maker_name="Seller 150",
            net_amount_units=100,
            network_fee_units=10,
            service_fee_units=0,
            deposit_required_units=110,
            total_price="2",
            settlement_asset="AUD",
            settlement_network=None,
            payment_method="PayID",
            state=OrderState.ADDRESS_PENDING,
            created_at=now,
            updated_at=now,
        )
        persisted = self.wallet.new_address()

        class FixedClock:
            def __call__(self):
                return now + 10_000

        restarted = TradeService(
            store=Store(self.store.path, network="btc09-regtest"),
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=FixedClock(),
            network_fee_units=10,
            deposit_timeout_seconds=600,
        )
        restarted.reconcile_transfers()
        row = self.store.get_order(order_id=order_id)
        self.assertEqual(row["deposit_addr"], persisted)
        self.assertEqual(row["deposit_allocated_at"], now + 10_000)
        self.assertEqual(row["deposit_deadline"], now + 10_600)
        self.assertEqual(self.wallet.address_count, 1)

    def test_attach_commit_failure_restart_reuses_one_unassigned_address(self):
        class FailingStore(Store):
            fail_once = True

            def _address_allocation_checkpoint(self, phase):
                if phase == "before_commit" and self.fail_once:
                    self.fail_once = False
                    raise RuntimeError("simulated attach commit failure")

        path = Path(self.tmp.name) / "attach-failure.db"
        store = FailingStore(path, network="btc09-regtest")
        store.initialize()
        service = TradeService(
            store=store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        with self.assertRaises(SafeSendFailure):
            service.create_sell(
                seller_id=160,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        pending = store.pending_address_order()
        self.assertIsNotNone(pending)
        self.assertEqual(self.wallet.address_count, 1)
        restarted = TradeService(
            store=Store(path, network="btc09-regtest"),
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        restarted.reconcile_pending_address()
        row = restarted.store.get_order(order_id=pending["order_id"])
        self.assertEqual(row["state"], "awaiting_deposit")
        self.assertEqual(self.wallet.address_count, 1)

    def test_wallet_persists_address_then_errors_and_resnapshot_recovers_it(self):
        persisted = address(31_000)

        def persist_then_error():
            self.wallet.addresses.append(persisted)
            self.wallet.address_count += 1
            raise RuntimeError("subprocess ended after durable wallet write")

        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=persist_then_error,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        created = service.create_sell(
            seller_id=161,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.assertEqual(created.deposit_addr, persisted)
        self.assertEqual(self.wallet.address_count, 1)
        self.assertIsNone(self.store.pending_address_order())

    def test_returned_address_before_attach_is_recovered_on_restart(self):
        class PreAttachFailStore(Store):
            fail_once = True

            def attach_pending_deposit_address(self, **kwargs):
                if self.fail_once:
                    self.fail_once = False
                    raise RuntimeError("simulated crash before address attach")
                return super().attach_pending_deposit_address(**kwargs)

        path = Path(self.tmp.name) / "pre-attach-failure.db"
        store = PreAttachFailStore(path, network="btc09-regtest")
        store.initialize()
        service = TradeService(
            store=store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        with self.assertRaises(SafeSendFailure):
            service.create_sell(
                seller_id=162,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        self.assertEqual(self.wallet.address_count, 1)
        restarted = TradeService(
            store=Store(path, network="btc09-regtest"),
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        restarted.reconcile_pending_address()
        self.assertEqual(self.wallet.address_count, 1)
        self.assertIsNone(restarted.store.pending_address_order())

    def test_primary_is_never_allocated_and_multiple_unassigned_fail_closed(self):
        order_id = self.store.create_order(
            side=OrderSide.SELL,
            maker_id=170,
            maker_name="Seller 170",
            net_amount_units=100,
            network_fee_units=10,
            service_fee_units=0,
            deposit_required_units=110,
            total_price="2",
            settlement_asset="AUD",
            settlement_network=None,
            payment_method="PayID",
            state=OrderState.ADDRESS_PENDING,
            created_at=1_000,
            updated_at=1_000,
        )
        self.wallet.addresses.extend((address(30_001), address(30_002)))
        with self.assertRaises(AccountingInvariantError):
            self.service.reconcile_pending_address()
        row = self.store.get_order(order_id=order_id)
        self.assertEqual(row["state"], "address_pending")
        self.assertIsNone(row["deposit_addr"])
        self.assertNotIn(
            self.wallet.primary_address, self.store.watched_deposit_addresses()
        )
        self.assertEqual(self.store.list_actionable_orders(), ())
        self.assertEqual(self.store.public_feed_snapshot()["orders"], ())
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("address_allocation_pending", health.issues)
        self.assertEqual(health.deposit_allocation["pending_count"], 1)

    def test_lower_sorting_generated_address_never_replaces_explicit_primary(self):
        generated = next(
            address(value)
            for value in range(1, 20_000)
            if address(value) < self.wallet.primary_address
        )

        def new_lower_address():
            self.wallet.addresses.append(generated)
            self.wallet.address_count += 1
            return generated

        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=new_lower_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        created = service.create_sell(
            seller_id=171,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        snapshot = self.wallet.snapshot(self.explorer.current_tip)
        self.assertEqual(snapshot.primary_address, self.wallet.primary_address)
        self.assertNotEqual(snapshot.addresses[0], snapshot.primary_address)
        self.assertEqual(created.deposit_addr, generated)
        self.assertNotEqual(created.deposit_addr, snapshot.primary_address)

    def test_allocation_lock_timeout_retains_pending_without_wallet_mutation(self):
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            allocation_lock_timeout_seconds=0.1,
        )
        with WalletAllocationLock(self.wallet.allocation_lock_path, acquire_timeout=1):
            with self.assertRaisesRegex(SafeSendFailure, "allocation is busy"):
                service.create_sell(
                    seller_id=172,
                    net_amount=100,
                    total_price="2",
                    asset="AUD",
                    method="PayID",
                    network=None,
                )
            writer_done = threading.Event()
            writer = threading.Thread(
                target=lambda: (
                    self.store.set_user_wallet(
                        user_id=173,
                        username="Unrelated",
                        wallet_addr=address(20_173),
                        now=3_000,
                    ),
                    writer_done.set(),
                )
            )
            writer.start()
            self.assertTrue(writer_done.wait(1))
            writer.join(2)
        self.assertEqual(self.wallet.address_count, 0)
        self.assertIsNotNone(self.store.pending_address_order())
        service._allocation_lock_timeout_seconds = 1.0
        waiter_result = []
        with WalletAllocationLock(self.wallet.allocation_lock_path, acquire_timeout=1):
            waiter = threading.Thread(
                target=lambda: waiter_result.append(service.reconcile_pending_address())
            )
            waiter.start()
            time.sleep(0.03)
            self.assertTrue(waiter.is_alive())
            self.store.set_user_wallet(
                user_id=174,
                username="Writer While Waiting",
                wallet_addr=address(20_174),
                now=3_001,
            )
        waiter.join(2)
        self.assertFalse(waiter.is_alive())
        self.assertEqual(len(waiter_result), 1)
        self.assertEqual(self.wallet.address_count, 1)
        self.assertIsNone(self.store.pending_address_order())

    def test_cross_process_services_allocate_one_key_and_attach_once(self):
        context = multiprocessing.get_context("spawn")
        for iteration in range(10):
            with self.subTest(iteration=iteration), tempfile.TemporaryDirectory() as root:
                db_path = str(Path(root) / "otc.db")
                wallet_state = str(Path(root) / "wallet-state.json")
                allocation_lock = str(Path(root) / "wallet.json.allocation.lock")
                store = Store(db_path, network="btc09-regtest")
                store.initialize()
                order_id = store.create_order(
                    side=OrderSide.SELL,
                    maker_id=200 + iteration,
                    maker_name="Seller",
                    net_amount_units=100,
                    network_fee_units=10,
                    service_fee_units=0,
                    deposit_required_units=110,
                    total_price="2",
                    settlement_asset="AUD",
                    settlement_network=None,
                    payment_method="PayID",
                    state=OrderState.ADDRESS_PENDING,
                    created_at=50_000,
                    updated_at=50_000,
                )
                with open(wallet_state, "w", encoding="ascii") as handle:
                    json.dump({"addresses": [address(49_999)]}, handle)
                barrier = context.Barrier(2)
                queue = context.Queue()
                workers = [
                    context.Process(
                        target=_multiprocess_allocation_service_worker,
                        args=(
                            db_path,
                            wallet_state,
                            allocation_lock,
                            barrier,
                            queue,
                        ),
                    )
                    for _ in range(2)
                ]
                started_workers = []
                try:
                    for worker in workers:
                        worker.start()
                        started_workers.append(worker)
                    results = [queue.get(timeout=20) for _ in workers]
                    for worker in workers:
                        worker.join(20)
                        self.assertFalse(
                            worker.is_alive(), "allocation worker did not exit"
                        )
                        self.assertEqual(worker.exitcode, 0)
                    self.assertEqual([item[0] for item in results], ["ok", "ok"])
                    self.assertEqual(
                        sum(item[1] == order_id for item in results), 1
                    )
                finally:
                    for worker in started_workers:
                        if worker.is_alive():
                            worker.terminate()
                            worker.join(5)
                        if worker.is_alive():
                            worker.kill()
                            worker.join(5)
                    queue.close()
                    queue.join_thread()
                    for worker in workers:
                        worker.close()
                with open(wallet_state, encoding="ascii") as handle:
                    addresses = json.load(handle)["addresses"]
                self.assertEqual(len(addresses), 2)
                row = store.get_order(order_id=order_id)
                self.assertEqual(row["state"], "awaiting_deposit")
                self.assertEqual(row["deposit_addr"], addresses[1])
                self.assertIsNone(store.pending_address_order())

    def test_process_death_releases_lock_and_recovers_durable_unassigned_key(self):
        context = multiprocessing.get_context("spawn")
        with tempfile.TemporaryDirectory() as root:
            db_path = str(Path(root) / "otc.db")
            wallet_state = str(Path(root) / "wallet-state.json")
            allocation_lock = str(Path(root) / "wallet.json.allocation.lock")
            store = Store(db_path, network="btc09-regtest")
            store.initialize()
            order_id = store.create_order(
                side=OrderSide.SELL,
                maker_id=220,
                maker_name="Seller",
                net_amount_units=100,
                network_fee_units=10,
                service_fee_units=0,
                deposit_required_units=110,
                total_price="2",
                settlement_asset="AUD",
                settlement_network=None,
                payment_method="PayID",
                state=OrderState.ADDRESS_PENDING,
                created_at=50_000,
                updated_at=50_000,
            )
            with open(wallet_state, "w", encoding="ascii") as handle:
                json.dump({"addresses": [address(49_999)]}, handle)
            owner_barrier = context.Barrier(1)
            owner_queue = context.Queue()
            owner = context.Process(
                target=_multiprocess_allocation_service_worker,
                args=(
                    db_path,
                    wallet_state,
                    allocation_lock,
                    owner_barrier,
                    owner_queue,
                    True,
                ),
            )
            owner_started = False
            try:
                owner.start()
                owner_started = True
                owner.join(20)
                self.assertFalse(owner.is_alive(), "lock owner did not exit")
                self.assertEqual(owner.exitcode, 17)
            finally:
                if owner_started:
                    if owner.is_alive():
                        owner.terminate()
                        owner.join(5)
                    if owner.is_alive():
                        owner.kill()
                        owner.join(5)
                owner_queue.close()
                owner_queue.join_thread()
                owner.close()
            with open(wallet_state, encoding="ascii") as handle:
                persisted = json.load(handle)["addresses"]
            self.assertEqual(len(persisted), 2)
            self.assertIsNotNone(store.pending_address_order())

            recovery_barrier = context.Barrier(1)
            recovery_queue = context.Queue()
            recovery = context.Process(
                target=_multiprocess_allocation_service_worker,
                args=(
                    db_path,
                    wallet_state,
                    allocation_lock,
                    recovery_barrier,
                    recovery_queue,
                ),
            )
            recovery_started = False
            try:
                recovery.start()
                recovery_started = True
                result = recovery_queue.get(timeout=20)
                recovery.join(20)
                self.assertFalse(recovery.is_alive(), "recovery worker did not exit")
                self.assertEqual((recovery.exitcode, result), (0, ("ok", order_id)))
            finally:
                if recovery_started:
                    if recovery.is_alive():
                        recovery.terminate()
                        recovery.join(5)
                    if recovery.is_alive():
                        recovery.kill()
                        recovery.join(5)
                recovery_queue.close()
                recovery_queue.join_thread()
                recovery.close()
            with open(wallet_state, encoding="ascii") as handle:
                recovered = json.load(handle)["addresses"]
            self.assertEqual(recovered, persisted)
            row = store.get_order(order_id=order_id)
            self.assertEqual(row["deposit_addr"], persisted[1])
            self.assertIsNone(store.pending_address_order())

    def test_ui_boundaries_return_public_safe_order_and_noncustodial_status(self):
        unsafe = (
            "BSB 123-456 account 1234 5678",
            "+61 412 345 678",
            "GB82 WEST 1234 5698 7654 32",
            "trader@example.com",
            "@payment-handle",
            "0x52908400098527886E0F7030069857D2E4169EE7",
            "wallet bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
        )
        for offset, method in enumerate(unsafe):
            with self.subTest(method=method), self.assertRaises(ValueError):
                self.service.create_sell(
                    seller_id=71 + offset,
                    seller_name="Seller",
                    receive_address=address(20_071 + offset),
                    net_amount=100,
                    total_price="2",
                    asset="AUD",
                    method=method,
                    network=None,
                )
        for offset, network in enumerate(
            ("123-456 12345678", "GB82 WEST 1234 5698", "0x52908400098527886")
        ):
            with self.subTest(network=network), self.assertRaises(ValueError):
                self.service.create_buy(
                    buyer_id=90 + offset,
                    buyer_name="Buyer",
                    receive_address=address(20_090 + offset),
                    net_amount=100,
                    total_price="2",
                    asset="USDT",
                    method="Wallet transfer",
                    network=network,
                )
        self.assertEqual(self.wallet.address_count, 0)
        created = self.create_sell(seller_id=71)
        public = self.service.view_order(created.order_id)
        self.assertEqual(public.order_id, created.order_id)
        self.assertEqual(public.side, "sell")
        self.assertFalse(hasattr(public, "maker_id"))
        self.assertFalse(hasattr(public, "deposit_addr"))
        self.assertEqual(public.payment_method, "PayID")
        legacy_row = dict(self.store.get_order(order_id=created.order_id))
        legacy_row["payment_method"] = "legacy mixed A1B2C3D4E5F6"
        legacy_row["settlement_network"] = "123-456 12345678"
        legacy_public = self.service._public_order_result(legacy_row)
        self.assertEqual(
            legacy_public.payment_method,
            "Private settlement method",
        )
        self.assertEqual(
            legacy_public.settlement_network,
            "Private settlement network",
        )
        self.assertEqual(
            [item.order_id for item in self.service.list_actionable_public()],
            [created.order_id],
        )

        status = self.service.account_status(actor_id=71)
        self.assertEqual(status.order_count, 1)
        self.assertEqual(status.active_order_count, 1)
        self.assertFalse(hasattr(status, "balance_units"))

    def test_asset_code_in_network_field_has_actionable_guidance(self):
        with self.assertRaisesRegex(
            ValueError,
            "USDT is the asset.*TRC20.*leave network blank.*Wise",
        ):
            self.service.create_buy(
                buyer_id=92,
                buyer_name="Buyer",
                receive_address=address(20_092),
                net_amount=100,
                total_price="2",
                asset="USDT",
                method="Wise",
                network="USDT",
            )
        self.assertEqual(self.wallet.address_count, 0)

    def test_receive_address_update_is_validated_and_used_by_later_order(self):
        configured = address(91_001)
        saved = self.service.set_receive_address(
            actor_id=72, actor_name="Trader 72", address=configured
        )
        self.assertEqual(saved, configured)
        with self.assertRaises(ValueError):
            self.service.set_receive_address(
                actor_id=72, actor_name="Trader 72", address="not-an-address"
            )
        created = self.service.create_buy(
            buyer_id=72,
            buyer_name="Trader 72",
            receive_address=None,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.assertEqual(self.store.get_order(order_id=created.order_id)["buyer_id"], 72)

    def test_fee_status_and_withdrawal_reservation_are_real_and_idempotent(self):
        self.assertEqual(self.service.available_fee_units(), 0)
        destination = address(91_002)
        self.assertIsNone(
            self.service.reserve_fee_withdrawal(
                admin_id=9001,
                operation_key="discord:123:fee_withdrawal",
                amount=1,
                network_fee=0,
                destination=destination,
                configured_destination=destination,
            )
        )
        self.assertEqual(self.store.count_transfers(), 0)

    def fund(self, order: object, amount: int = 110, *, confirmations: int = 6):
        self.explorer.set_outputs(
            order.deposit_addr,
            [amount],
            confirmations=confirmations,  # type: ignore[attr-defined]
        )
        return self.service.check_deposit(
            order.order_id,
            actor_id=order.seller_id,  # type: ignore[attr-defined]
        )

    def accept_sell(self, order: object, *, buyer_id: int = 2):
        return self.service.accept(
            order.order_id,  # type: ignore[attr-defined]
            actor_id=buyer_id,
            actor_name=f"Buyer {buyer_id}",
            receive_address=address(30_000 + buyer_id),
        )

    def common_observation_tip(self) -> tuple[str, int]:
        with closing(self.store.connect()) as conn:
            watched = conn.execute(
                """
                SELECT DISTINCT deposit_addr FROM orders
                WHERE deposit_addr IS NOT NULL
                """
            ).fetchall()
            latest = conn.execute(
                """
                SELECT s.tip_hash,s.tip_height
                FROM deposit_scans s
                WHERE s.scan_id=(
                  SELECT MAX(candidate.scan_id) FROM deposit_scans candidate
                  WHERE candidate.network=? AND candidate.address=s.address
                )
                  AND s.network=? AND s.address IN (
                    SELECT deposit_addr FROM orders WHERE deposit_addr IS NOT NULL
                  )
                """,
                (self.store.network, self.store.network),
            ).fetchall()
        if len(latest) != len(watched) or not latest:
            raise AssertionError("test mutation lacks a complete watched-address tip")
        tips = {(row["tip_hash"], row["tip_height"]) for row in latest}
        if len(tips) != 1:
            raise AssertionError("test mutation lacks one common watched-address tip")
        return next(iter(tips))

    def mark_transfer_broadcast(self, **values: object):
        tip_hash, tip_height = self.common_observation_tip()
        return Store.mark_transfer_broadcast(
            self.store,
            expected_tip_hash=tip_hash,
            expected_tip_height=tip_height,
            **values,
        )

    def mark_transfer_uncertain(self, **values: object):
        tip_hash, tip_height = self.common_observation_tip()
        return Store.mark_transfer_uncertain(
            self.store,
            expected_tip_hash=tip_hash,
            expected_tip_height=tip_height,
            **values,
        )

    def mark_transfer_confirmed(self, **values: object):
        tip_hash, tip_height = self.common_observation_tip()
        return Store.mark_transfer_confirmed(
            self.store,
            expected_tip_hash=tip_hash,
            expected_tip_height=tip_height,
            **values,
        )

    def confirmed_then_prepared_transfers(self):
        first = self.accept_sell(self.fund(self.create_sell(seller_id=50)), buyer_id=51)
        self.service.confirm_sent(first.order_id, actor_id=51)
        self.service.confirm_received(first.order_id, actor_id=50)
        self.service.mine()
        first_transfer = self.store.get_order_transfer(order_id=first.order_id)
        self.mark_transfer_confirmed(
            transfer_id=first_transfer["transfer_id"],
            observed_txid=first_transfer["txid"],
            confirmed_block_hash=h(90_001),
            confirmed_block_height=99,
            confirmations=2,
            now=self.clock(),
        )
        self.explorer.set_transaction(
            first_transfer["txid"],
            "confirmed",
            confirmations=2,
            block_number=90_001,
        )

        second = self.accept_sell(
            self.fund(self.create_sell(seller_id=60)), buyer_id=61
        )
        self.service.confirm_sent(second.order_id, actor_id=61)
        self.service.confirm_received(second.order_id, actor_id=60)
        original_broadcast = self.service._broadcast_stored
        self.service._broadcast_stored = (  # type: ignore[method-assign]
            lambda transfer: self.service._transfer_result(transfer)
        )
        self.assertEqual(self.service.mine().state, "prepared")
        self.service._broadcast_stored = original_broadcast  # type: ignore[method-assign]
        second_transfer = self.store.get_order_transfer(order_id=second.order_id)
        self.wallet.broadcast_calls.clear()
        return first_transfer, second_transfer

    def earn_fee_and_queue_withdrawal(self):
        fee_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            fee_bps=1_000,
            deposit_timeout_seconds=600,
        )
        order = fee_service.create_sell(
            seller_id=70,
            seller_name="Seller 70",
            receive_address=address(20_070),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.explorer.set_outputs(order.deposit_addr, [120])
        funded = fee_service.check_deposit(order.order_id, actor_id=70)
        matched = fee_service.accept(
            funded.order_id,
            actor_id=71,
            actor_name="Buyer 71",
            receive_address=address(30_071),
        )
        fee_service.confirm_sent(matched.order_id, actor_id=71)
        fee_service.confirm_received(matched.order_id, actor_id=70)
        fee_service.mine()
        release = self.store.get_order_transfer(order_id=order.order_id)
        self.mark_transfer_confirmed(
            transfer_id=release["transfer_id"],
            observed_txid=release["txid"],
            confirmed_block_hash=h(95_001),
            confirmed_block_height=99,
            confirmations=2,
            now=self.clock(),
        )
        self.explorer.set_transaction(
            release["txid"],
            "confirmed",
            confirmations=2,
            block_number=95_001,
        )
        destination = address(40_000)
        reserved = fee_service.reserve_fee_withdrawal(
            admin_id=99,
            operation_key="fee:test:1",
            amount=9,
            network_fee=1,
            destination=destination,
            configured_destination=destination,
        )
        duplicate = fee_service.reserve_fee_withdrawal(
            admin_id=99,
            operation_key="fee:test:1",
            amount=9,
            network_fee=1,
            destination=destination,
            configured_destination=destination,
        )
        self.assertIsNotNone(reserved)
        self.assertEqual(duplicate, reserved)
        withdrawal = self.store.get_transfer(transfer_id=reserved.transfer_id)
        self.assertEqual(withdrawal["transfer_id"], reserved.transfer_id)
        self.wallet.broadcast_calls.clear()
        return fee_service, withdrawal

    def test_wts_requires_deposit_before_listing(self) -> None:
        order = self.create_sell()
        self.assertEqual(order.state, "awaiting_deposit")
        self.assertEqual(order.deposit_required_units, 110)
        self.assertEqual(self.service.list_open(), ())

        funded = self.fund(order)

        self.assertEqual(funded.state, "open")
        self.assertEqual([event.kind for event in funded.events], ["payment_ready"])
        self.assertEqual(
            [item.order_id for item in self.service.list_open()], [order.order_id]
        )

    def test_wtb_assigns_one_seller_then_requires_deposit(self) -> None:
        order = self.service.create_buy(
            buyer_id=2,
            buyer_name="Buyer 2",
            receive_address=address(30_002),
            net_amount=100,
            total_price="2",
            asset="USDT",
            method="external wallet",
            network="TRC20",
        )
        self.assertEqual(order.state, "open")
        accepted = self.service.accept(
            order.order_id,
            actor_id=1,
            actor_name="Seller 1",
            receive_address=address(20_001),
        )
        self.assertEqual(accepted.seller_id, 1)
        self.assertEqual(accepted.state, "awaiting_deposit")
        self.assertIsNotNone(accepted.deposit_addr)

        self.explorer.set_outputs(accepted.deposit_addr, [110])
        matched = self.service.check_deposit(accepted.order_id, actor_id=1)
        self.assertEqual(matched.state, "matched")
        self.assertEqual([event.kind for event in matched.events], ["payment_ready"])

    def test_losing_wtb_accept_has_no_database_side_effect(self) -> None:
        order = self.service.create_buy(
            buyer_id=2,
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )

        def accept(actor_id: int):
            return self.service.accept(
                order.order_id,
                actor_id=actor_id,
                actor_name=f"Seller {actor_id}",
                receive_address=address(20_000 + actor_id),
            )

        with ThreadPoolExecutor(max_workers=10) as pool:
            results = list(pool.map(accept, range(10, 20)))
        winners = {result.seller_id for result in results if result.accepted}
        self.assertEqual(len(winners), 1)
        stored = self.store.get_order(order_id=order.order_id)
        self.assertIsNotNone(stored)
        self.assertIn(stored["seller_id"], winners)  # type: ignore[index]
        self.assertEqual(stored["state"], "awaiting_deposit")  # type: ignore[index]
        with closing(self.store.connect()) as conn:
            users = {
                row[0] for row in conn.execute("SELECT user_id FROM users").fetchall()
            }
        self.assertEqual(users, {2, stored["seller_id"]})  # type: ignore[index]

    def test_wtb_fast_rejections_allocate_no_fresh_address_or_user(self) -> None:
        order = self.service.create_buy(
            buyer_id=2,
            buyer_name="Buyer 2",
            receive_address=address(30_002),
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )
        before = self.wallet.address_count

        self.assertFalse(self.service.accept(order.order_id, actor_id=2).accepted)
        self.assertEqual(self.wallet.address_count, before)
        winner = self.service.accept(
            order.order_id,
            actor_id=3,
            actor_name="Seller 3",
            receive_address=address(20_003),
        )
        after_winner = self.wallet.address_count
        self.assertTrue(winner.accepted)

        stale = self.service.accept(
            order.order_id,
            actor_id=4,
            actor_name="Seller 4",
            receive_address=address(20_004),
        )
        self.assertFalse(stale.accepted)
        self.assertEqual(self.wallet.address_count, after_winner)
        with closing(self.store.connect()) as conn:
            users = {row[0] for row in conn.execute("SELECT user_id FROM users")}
        self.assertEqual(users, {2, 3})

    def test_wtb_one_second_deadline_uses_the_acceptance_timestamp(self) -> None:
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=1,
        )
        order = service.create_buy(
            buyer_id=2,
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )

        accepted = service.accept(order.order_id, actor_id=1)

        stored = self.store.get_order(order_id=order.order_id)
        self.assertEqual(accepted.state, "awaiting_deposit")
        self.assertEqual(
            stored["deposit_deadline"], stored["deposit_allocated_at"] + 1
        )

    def test_buyer_is_not_told_to_pay_before_full_confirmed_deposit(self) -> None:
        buy = self.service.create_buy(
            buyer_id=2,
            net_amount=100,
            total_price="2",
            asset="USDT",
            method="external wallet",
            network="TRC20",
        )
        accepted = self.service.accept(buy.order_id, actor_id=1)
        self.explorer.set_outputs(accepted.deposit_addr, [109])

        under = self.service.check_deposit(accepted.order_id, actor_id=1)

        self.assertEqual(under.state, "awaiting_deposit")
        self.assertNotIn("payment_ready", [event.kind for event in under.events])
        self.assertEqual(self.store.order_liability_units(order_id=buy.order_id), 109)

    def test_overpayment_is_exact_residual_liability(self) -> None:
        order = self.create_sell()
        funded = self.fund(order, 137)

        self.assertEqual(funded.state, "open")
        self.assertEqual(funded.deposit_main_units, 110)
        self.assertEqual(funded.deposit_recovery_units, 27)
        self.assertEqual(self.store.order_liability_units(order_id=order.order_id), 137)
        self.assertIn(
            "excess_deposit_recovery", [event.kind for event in funded.events]
        )

    def test_late_payment_emits_recovery_without_reopening(self) -> None:
        order = self.create_sell()
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE orders SET state='cancelled',updated_at=? WHERE order_id=?",
                (self.clock(), order.order_id),
            )
        self.explorer.set_outputs(order.deposit_addr, [50])

        checked = self.service.check_deposit(order.order_id, actor_id=1)

        self.assertEqual(checked.state, "cancelled")
        self.assertIn("late_payment_recovery", [event.kind for event in checked.events])
        self.assertIn(order.deposit_addr, self.store.watched_deposit_addresses())

    def test_check_deposit_is_seller_authorized_and_failure_has_no_scan(self) -> None:
        order = self.create_sell()
        with self.assertRaises(AuthorizationError):
            self.service.check_deposit(order.order_id, actor_id=999)
        self.assertEqual(self.explorer.batch_calls, 0)

    def test_both_confirmation_orders_queue_exactly_one_release(self) -> None:
        for buyer_first in (True, False):
            with self.subTest(buyer_first=buyer_first):
                order = self.create_sell(seller_id=10 if buyer_first else 20)
                funded = self.fund(order)
                matched = self.accept_sell(funded, buyer_id=11 if buyer_first else 21)
                first = (
                    self.service.confirm_sent
                    if buyer_first
                    else self.service.confirm_received
                )
                second = (
                    self.service.confirm_received
                    if buyer_first
                    else self.service.confirm_sent
                )
                first_id = matched.buyer_id if buyer_first else matched.seller_id
                second_id = matched.seller_id if buyer_first else matched.buyer_id
                once = first(matched.order_id, actor_id=first_id)
                self.assertEqual(once.state, "matched")
                twice = second(matched.order_id, actor_id=second_id)
                self.assertEqual(twice.state, "release_reserved")
                self.assertEqual(
                    self.store.count_transfers(order_id=matched.order_id), 1
                )

    def test_concurrent_second_confirmation_emits_one_winner_event(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)

        with ThreadPoolExecutor(max_workers=20) as pool:
            results = list(
                pool.map(
                    lambda _: self.service.confirm_received(
                        order.order_id, actor_id=order.seller_id
                    ),
                    range(20),
                )
            )

        self.assertEqual(
            sum(
                event.kind == "seller_confirmed"
                for result in results
                for event in result.events
            ),
            1,
        )
        self.assertTrue(all(result.state == "release_reserved" for result in results))
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

    def test_concurrent_overpaid_deposit_emits_each_transition_once(self) -> None:
        order = self.create_sell()
        self.explorer.set_outputs(order.deposit_addr, [137])

        with ThreadPoolExecutor(max_workers=20) as pool:
            results = list(
                pool.map(
                    lambda _: self.service.check_deposit(order.order_id, actor_id=1),
                    range(20),
                )
            )

        kinds = [event.kind for result in results for event in result.events]
        self.assertEqual(kinds.count("payment_ready"), 1)
        self.assertEqual(kinds.count("excess_deposit_recovery"), 1)
        self.assertTrue(all(result.state == "open" for result in results))
        self.assertEqual(self.store.order_liability_units(order_id=order.order_id), 137)

    def test_confirmation_role_authorization_is_strict(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        with self.assertRaises(AuthorizationError):
            self.service.confirm_sent(order.order_id, actor_id=order.seller_id)
        with self.assertRaises(AuthorizationError):
            self.service.confirm_received(order.order_id, actor_id=order.buyer_id)
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 0)

    def test_twenty_confirmations_and_workers_prepare_attach_broadcast_once(
        self,
    ) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)

        with ThreadPoolExecutor(max_workers=20) as pool:
            confirmations = list(
                pool.map(
                    lambda _: self.service.confirm_received(
                        order.order_id, actor_id=order.seller_id
                    ),
                    range(20),
                )
            )
        self.assertTrue(
            all(result.state == "release_reserved" for result in confirmations)
        )
        with ThreadPoolExecutor(max_workers=20) as pool:
            list(pool.map(lambda _: self.service.mine(), range(20)))

        self.assertEqual(len(self.wallet.prepare_calls), 1)
        self.assertEqual(len(self.wallet.broadcast_calls), 1)
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "broadcast")
        self.assertEqual(transfer["attempt_count"], 1)
        self.assertIsNotNone(transfer["signed_at"])
        self.assertEqual(self.wallet.broadcast_calls[0][0], transfer["signed_tx_hex"])
        self.assertEqual(self.wallet.broadcast_calls[0][1], transfer["txid"])
        self.assertEqual(self.wallet.prepare_calls[0][1:3], (100, 10))

        # A later queue poll must not use a broadcast row as permission to
        # submit the same transaction again. Task 6 owns chain reconciliation.
        repeated = self.service.mine()
        self.assertEqual(repeated.state, "broadcast")
        self.assertEqual(len(self.wallet.broadcast_calls), 1)

    def test_all_address_barrier_restricts_another_orders_provisional_outpoint(
        self,
    ) -> None:
        paying = self.create_sell(seller_id=30)
        provisional = self.create_sell(seller_id=40)
        self.explorer.set_outputs(paying.deposit_addr, [110], confirmations=6)
        self.explorer.set_outputs(provisional.deposit_addr, [75], confirmations=5)

        funded = self.service.check_deposit(paying.order_id, actor_id=30)
        matched = self.accept_sell(funded, buyer_id=31)
        self.service.confirm_sent(matched.order_id, actor_id=31)
        self.service.confirm_received(matched.order_id, actor_id=30)
        self.service.mine()

        provisional_output = self.explorer.outputs_by_address[provisional.deposit_addr][
            0
        ]
        self.assertEqual(
            self.wallet.prepare_calls[0][4],
            (f"{provisional_output.txid}:{provisional_output.vout}",),
        )
        with closing(self.store.connect()) as conn:
            tips = {
                (row["address"], row["tip_hash"], row["tip_height"])
                for row in conn.execute(
                    """
                    SELECT address,tip_hash,tip_height FROM deposit_scans
                    WHERE scan_id IN (
                      SELECT MAX(scan_id) FROM deposit_scans GROUP BY address
                    )
                    """
                ).fetchall()
            }
        self.assertEqual(
            tips,
            {
                (paying.deposit_addr, self.explorer.current_tip.hash, 100),
                (provisional.deposit_addr, self.explorer.current_tip.hash, 100),
            },
        )

    def test_insolvent_snapshot_has_zero_claim_prepare_or_broadcast_side_effect(
        self,
    ) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        reserve = WalletOutpoint(
            outpoint=f"{h(55_555)}:0",
            txid=h(55_555),
            vout=0,
            amount_units=50,
            address=self.wallet.primary_address,
            owner_address_index=0,
        )
        self.wallet.snapshot = lambda tip: WalletSnapshot(  # type: ignore[method-assign]
            "btc09-regtest",
            tip,
            self.wallet.primary_address,
            (self.wallet.primary_address,),
            (reserve,),
            50,
            h(778),
        )

        with self.assertRaisesRegex(AccountingInvariantError, "insolvent"):
            self.service.mine()

        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "queued")
        self.assertEqual(self.wallet.prepare_calls, [])
        self.assertEqual(self.wallet.broadcast_calls, [])

    def test_startup_prepared_recovery_broadcasts_only_database_bytes(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        original_broadcast = self.service._broadcast_stored
        self.service._broadcast_stored = (  # type: ignore[method-assign]
            lambda transfer: self.service._transfer_result(transfer)
        )
        prepared = self.service.mine()
        self.service._broadcast_stored = original_broadcast  # type: ignore[method-assign]
        self.assertEqual(prepared.state, "prepared")
        stored = self.store.get_order_transfer(order_id=order.order_id)
        self.wallet.prepare_calls.clear()

        restarted = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=600,
        )
        result = restarted.mine()

        self.assertEqual(result.state, "broadcast")
        self.assertEqual(self.wallet.prepare_calls, [])
        self.assertEqual(
            self.wallet.broadcast_calls,
            [
                (
                    stored["signed_tx_hex"],
                    stored["txid"],
                    Tip(stored["prepared_tip_hash"], stored["prepared_tip_height"]),
                )
            ],
        )

    def test_existing_uncertainty_blocks_prepared_other_transfer_before_wallet(
        self,
    ) -> None:
        first_transfer, second_transfer = self.confirmed_then_prepared_transfers()

        self.mark_transfer_uncertain(
            transfer_id=first_transfer["transfer_id"],
            expected_state="confirmed",
            expected_txid=first_transfer["txid"],
            error_text="confirmed transfer disappeared",
            now=self.clock(),
        )
        result = self.service.mine()

        self.assertEqual(result.state, "prepared")
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.assertEqual(
            self.store.get_order_transfer(order_id=second_transfer["order_id"])["txid"],
            second_transfer["txid"],
        )

    def test_uncertainty_writer_wins_true_race_and_suppresses_wallet(self) -> None:
        first_transfer, second_transfer = self.confirmed_then_prepared_transfers()
        uncertainty_holds_writer = threading.Event()
        release_uncertainty = threading.Event()

        def checkpoint(_phase: str) -> None:
            uncertainty_holds_writer.set()
            if not release_uncertainty.wait(timeout=5):
                raise RuntimeError("uncertainty test gate timed out")

        self.store._uncertainty_checkpoint = checkpoint  # type: ignore[method-assign]
        tip_hash, tip_height = self.common_observation_tip()
        with ThreadPoolExecutor(max_workers=2) as pool:
            uncertainty = pool.submit(
                self.store.mark_transfer_uncertain,
                transfer_id=first_transfer["transfer_id"],
                expected_state="confirmed",
                expected_txid=first_transfer["txid"],
                error_text="confirmed transfer disappeared",
                now=self.clock(),
                expected_tip_hash=tip_hash,
                expected_tip_height=tip_height,
            )
            self.assertTrue(uncertainty_holds_writer.wait(timeout=5))
            broadcast = pool.submit(self.service.mine)
            release_uncertainty.set()
            self.assertEqual(uncertainty.result(timeout=5)["state"], "uncertain")
            self.assertEqual(broadcast.result(timeout=5).state, "prepared")

        self.assertEqual(self.wallet.broadcast_calls, [])
        self.assertEqual(
            self.store.get_order_transfer(order_id=second_transfer["order_id"])[
                "state"
            ],
            "prepared",
        )

    def test_broadcast_authorization_wins_true_race_before_uncertainty(self) -> None:
        first_transfer, second_transfer = self.confirmed_then_prepared_transfers()
        self.wallet.broadcast_entered = threading.Event()
        self.wallet.broadcast_release = threading.Event()
        uncertainty_started = threading.Event()

        def mark_uncertain():
            uncertainty_started.set()
            return self.mark_transfer_uncertain(
                transfer_id=first_transfer["transfer_id"],
                expected_state="confirmed",
                expected_txid=first_transfer["txid"],
                error_text="confirmed transfer disappeared",
                now=self.clock(),
            )

        with ThreadPoolExecutor(max_workers=2) as pool:
            broadcast = pool.submit(self.service.mine)
            self.assertTrue(self.wallet.broadcast_entered.wait(timeout=5))
            uncertainty = pool.submit(mark_uncertain)
            self.assertTrue(uncertainty_started.wait(timeout=5))
            self.assertFalse(uncertainty.done())
            self.wallet.broadcast_release.set()
            self.assertEqual(broadcast.result(timeout=5).state, "broadcast")
            self.assertEqual(uncertainty.result(timeout=5)["state"], "uncertain")

        self.assertEqual(len(self.wallet.broadcast_calls), 1)
        self.assertEqual(
            self.store.get_order_transfer(order_id=second_transfer["order_id"])[
                "state"
            ],
            "broadcast",
        )

    def test_unsigned_reserved_restart_requeues_only_after_child_known_dead(
        self,
    ) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        self.wallet.prepare_error = RuntimeError("simulated process death")
        with self.assertRaisesRegex(RuntimeError, "process death"):
            self.service.mine()
        reserved = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(reserved["state"], "reserved")
        self.assertIsNone(reserved["signed_tx_hex"])
        self.wallet.prepare_error = None

        not_dead = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            prepare_child_is_dead=lambda _row: False,
        )
        self.assertEqual(not_dead.mine().state, "reserved")
        self.assertEqual(len(self.wallet.prepare_calls), 1)

        dead = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            prepare_child_is_dead=lambda _row: True,
        )
        recovered = dead.mine()
        self.assertEqual(recovered.state, "broadcast")
        final = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(final["transfer_id"], reserved["transfer_id"])
        self.assertEqual(final["operation_key"], reserved["operation_key"])
        self.assertEqual(final["attempt_count"], 2)
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

    def test_address_creation_failure_retains_durable_pending_order(self) -> None:
        failing = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=lambda: (_ for _ in ()).throw(SafeSendFailure("safe")),
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
        )
        with self.assertRaises(SafeSendFailure):
            failing.create_sell(
                seller_id=1,
                net_amount=100,
                total_price="2",
                asset="AUD",
                method="PayID",
                network=None,
            )
        with closing(self.store.connect()) as conn:
            row = conn.execute("SELECT * FROM orders").fetchone()
            self.assertEqual(row["state"], "address_pending")
            self.assertIsNone(row["deposit_addr"])
            self.assertIsNotNone(row["deposit_allocation_reserved_at"])

    def test_invalid_sell_terms_are_rejected_before_address_allocation(self) -> None:
        with self.assertRaises(ValueError):
            self.service.create_sell(
                seller_id=1,
                seller_name="Seller 1",
                receive_address=address(20_001),
                net_amount=100,
                total_price="2",
                asset="bad asset",
                method="PayID",
                network=None,
            )
        self.assertEqual(self.wallet.address_count, 0)

    def test_quote_bounds_fail_before_address_or_database_mutation(self) -> None:
        def service(*, network_fee_units: int = 0, fee_bps: int = 0) -> TradeService:
            return TradeService(
                store=self.store,
                explorer=self.explorer,
                wallet=self.wallet,
                fresh_address=self.wallet.new_address,
                confirmation_depth=6,
                clock=self.clock,
                network_fee_units=network_fee_units,
                fee_bps=fee_bps,
            )

        invalid = (
            ("net over max", service(), MAX_09C_UNITS + 1),
            ("network fee over max", service(network_fee_units=MAX_09C_UNITS + 1), 1),
            ("fee sum over max", service(network_fee_units=10), MAX_09C_UNITS - 9),
            ("service fee overflow", service(fee_bps=1), MAX_09C_UNITS),
            ("boolean net", service(), True),
        )
        for label, bounded, net_amount in invalid:
            with self.subTest(label=label), self.assertRaises(ValueError):
                bounded.create_sell(
                    seller_id=99,
                    seller_name="Seller 99",
                    receive_address=address(20_099),
                    net_amount=net_amount,
                    total_price="2",
                    asset="AUD",
                    method="PayID",
                    network=None,
                )
            self.assertEqual(self.wallet.address_count, 0)
            with closing(self.store.connect()) as conn:
                self.assertEqual(
                    tuple(
                        conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
                        for table in ("users", "orders", "audit_events")
                    ),
                    (0, 0, 0),
                )

        for label, kwargs in (
            ("boolean network fee", {"network_fee_units": True}),
            ("boolean service fee rate", {"fee_bps": True}),
        ):
            with self.subTest(label=label), self.assertRaises(ValueError):
                service(**kwargs)
        self.assertEqual(self.wallet.address_count, 0)

        boundary = service().create_sell(
            seller_id=99,
            seller_name="Seller 99",
            receive_address=address(20_099),
            net_amount=MAX_09C_UNITS,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.assertEqual(boundary.net_amount_units, MAX_09C_UNITS)
        self.assertEqual(boundary.deposit_required_units, MAX_09C_UNITS)
        self.assertEqual(self.wallet.address_count, 1)

    def test_hard_wallet_invariant_is_not_misclassified_as_safe_retry(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        self.wallet.prepare_error = WalletInvariantError("restricted input selected")

        with self.assertRaises(WalletInvariantError):
            self.service.mine()

        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "reserved")
        self.assertEqual(self.wallet.broadcast_calls, [])

    def test_only_a_typed_prepared_result_can_reach_durable_attachment(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        self.wallet.prepare = lambda *_args: object()  # type: ignore[method-assign]

        with self.assertRaisesRegex(AccountingInvariantError, "prepared result"):
            self.service.mine()

        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "reserved")
        self.assertIsNone(transfer["signed_tx_hex"])
        self.assertEqual(self.wallet.broadcast_calls, [])

    def test_prepare_failure_is_safe_and_never_broadcasts(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        self.wallet.prepare_error = SafeSendFailure("safe")

        result = self.service.mine()

        self.assertEqual(result.state, "failed_safe")
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.assertEqual(
            self.store.get_order(order_id=order.order_id)["state"],
            "transfer_failed_safe",
        )

    def test_broadcast_ambiguity_is_uncertain_and_keeps_stored_identity(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        self.wallet.broadcast_error = UncertainSendFailure("unknown")

        result = self.service.mine()

        self.assertEqual(result.state, "uncertain")
        stored = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(stored["signed_tx_hex"], "01020304")
        self.assertIsNotNone(stored["txid"])
        self.assertEqual(
            self.store.get_order(order_id=order.order_id)["state"], "transfer_uncertain"
        )

    def test_control_exception_after_wallet_call_marks_uncertain_then_reraises(
        self,
    ) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        original_broadcast = self.service._broadcast_stored
        self.service._broadcast_stored = (  # type: ignore[method-assign]
            lambda transfer: self.service._transfer_result(transfer)
        )
        self.assertEqual(self.service.mine().state, "prepared")
        self.service._broadcast_stored = original_broadcast  # type: ignore[method-assign]
        control = KeyboardInterrupt("stop now")
        self.wallet.broadcast_error = control

        with self.assertRaises(KeyboardInterrupt) as raised:
            self.service.mine()

        self.assertIs(raised.exception, control)
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "uncertain")
        self.assertEqual(len(self.wallet.broadcast_calls), 1)

    def test_fee_withdrawal_uses_earned_reservation_and_full_wallet_path(self) -> None:
        service, withdrawal = self.earn_fee_and_queue_withdrawal()

        result = service.mine()

        self.assertEqual(result.transfer_id, withdrawal["transfer_id"])
        self.assertEqual(result.state, "broadcast")
        self.assertEqual(self.wallet.prepare_calls[-1][1:3], (9, 1))
        stored = self.store.active_wallet_transfer()
        self.assertEqual(stored["kind"], "fee_withdrawal")
        self.assertEqual(self.wallet.broadcast_calls[0][0], stored["signed_tx_hex"])

    def test_fee_withdrawal_uncertainty_recovers_only_exact_stored_tx(self) -> None:
        service, withdrawal = self.earn_fee_and_queue_withdrawal()
        self.wallet.broadcast_error = UncertainSendFailure("unknown")

        uncertain = service.mine()

        self.assertEqual(uncertain.state, "uncertain")
        with closing(self.store.connect()) as conn:
            stored = dict(
                conn.execute(
                    "SELECT * FROM transfers WHERE transfer_id=?",
                    (withdrawal["transfer_id"],),
                ).fetchone()
            )
        recovered = self.mark_transfer_broadcast(
            transfer_id=stored["transfer_id"],
            observed_txid=stored["txid"],
            observed_status="mempool",
            now=self.clock(),
        )
        self.assertEqual(recovered["transfer_id"], stored["transfer_id"])
        self.assertEqual(recovered["txid"], stored["txid"])
        self.assertEqual(recovered["signed_tx_hex"], stored["signed_tx_hex"])
        self.assertEqual(self.store.count_transfers(), 2)

    def test_attach_recovery_accepts_exact_monotonic_advanced_states(self) -> None:
        _, transfer = self.confirmed_then_prepared_transfers()

        def recover():
            return self.store.recover_ambiguous_attach(
                transfer_id=transfer["transfer_id"],
                expected_attempt_count=transfer["attempt_count"],
                expected_reserved_at=transfer["reserved_at"],
                txid=transfer["txid"],
                signed_tx_hex=transfer["signed_tx_hex"],
                prepared_tip_hash=transfer["prepared_tip_hash"],
                prepared_tip_height=transfer["prepared_tip_height"],
            )

        self.assertEqual(recover().classification, "prepared")
        transfer = self.mark_transfer_broadcast(
            transfer_id=transfer["transfer_id"],
            observed_txid=transfer["txid"],
            observed_status="mempool",
            now=self.clock(),
        )
        self.assertEqual(recover().classification, "broadcast")
        transfer = self.mark_transfer_confirmed(
            transfer_id=transfer["transfer_id"],
            observed_txid=transfer["txid"],
            confirmed_block_hash=h(96_001),
            confirmed_block_height=99,
            confirmations=2,
            now=self.clock(),
        )
        self.assertEqual(recover().classification, "confirmed")
        transfer = self.mark_transfer_uncertain(
            transfer_id=transfer["transfer_id"],
            expected_state="confirmed",
            expected_txid=transfer["txid"],
            error_text="reorg",
            now=self.clock(),
        )
        self.assertEqual(recover().classification, "uncertain")

    def test_ambiguous_attach_reads_cross_worker_broadcast_without_rebuild(
        self,
    ) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)

        def commit_then_advance(conn) -> None:
            conn.execute("COMMIT")
            prepared = self.store.get_order_transfer(order_id=order.order_id)
            self.mark_transfer_broadcast(
                transfer_id=prepared["transfer_id"],
                observed_txid=prepared["txid"],
                observed_status="mempool",
                now=self.clock(),
            )
            raise RuntimeError("original worker lost commit acknowledgement")

        self.store._attach_commit_boundary = commit_then_advance
        result = self.service.mine()

        self.assertEqual(result.state, "broadcast")
        self.assertEqual(self.wallet.broadcast_calls, [])
        stored = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(stored["txid"], result.txid)
        self.assertIsNone(self.store._health_failure)

    def test_late_wts_and_wtb_deposits_are_recovery_not_payment_ready(self) -> None:
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=1,
        )
        sell = service.create_sell(
            seller_id=80,
            seller_name="Seller 80",
            receive_address=address(20_080),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        buy = service.create_buy(
            buyer_id=81,
            buyer_name="Buyer 81",
            receive_address=address(30_081),
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )
        accepted = service.accept(
            buy.order_id,
            actor_id=82,
            actor_name="Seller 82",
            receive_address=address(20_082),
        )
        while self.clock.value <= max(sell.deposit_deadline, accepted.deposit_deadline):
            self.clock()
        self.explorer.set_outputs(sell.deposit_addr, [110])

        sell_result = service.check_deposit(sell.order_id, actor_id=80)
        self.explorer.current_tip = Tip(h(902), 101)
        self.explorer.set_outputs(sell.deposit_addr, [110], confirmations=7)
        self.explorer.set_outputs(accepted.deposit_addr, [110])
        buy_result = service.check_deposit(buy.order_id, actor_id=82)

        for result in (sell_result, buy_result):
            self.assertEqual(result.state, "deposit_expired")
            self.assertNotIn("payment_ready", [event.kind for event in result.events])
            self.assertIn(
                "late_payment_recovery", [event.kind for event in result.events]
            )
            self.assertEqual(result.deposit_recovery_units, 110)

    def test_deposit_at_exact_deadline_is_on_time(self) -> None:
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=1,
        )
        order = service.create_sell(
            seller_id=83,
            seller_name="Seller 83",
            receive_address=address(20_083),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.explorer.set_outputs(order.deposit_addr, [110])

        funded = service.check_deposit(order.order_id, actor_id=83)

        self.assertEqual(funded.state, "open")
        self.assertEqual([event.kind for event in funded.events], ["payment_ready"])

        buy = service.create_buy(
            buyer_id=84,
            buyer_name="Buyer 84",
            receive_address=address(30_084),
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )
        accepted = service.accept(
            buy.order_id,
            actor_id=85,
            actor_name="Seller 85",
            receive_address=address(20_085),
        )
        self.explorer.set_outputs(accepted.deposit_addr, [110])

        matched = service.check_deposit(accepted.order_id, actor_id=85)

        self.assertEqual(matched.state, "matched")
        self.assertEqual([event.kind for event in matched.events], ["payment_ready"])

    def test_same_tip_rescan_still_enforces_elapsed_deadline(self) -> None:
        service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=3,
        )
        order = service.create_sell(
            seller_id=84,
            seller_name="Seller 84",
            receive_address=address(20_084),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        first = service.check_deposit(order.order_id, actor_id=84)
        self.assertEqual(first.state, "awaiting_deposit")
        while self.clock.value <= order.deposit_deadline:
            self.clock()

        expired = service.check_deposit(order.order_id, actor_id=84)

        self.assertEqual(expired.state, "deposit_expired")
        self.assertEqual(self.explorer.current_tip, Tip(h(900), 100))

    def test_elapsed_full_deposit_waits_for_global_health_repair(self) -> None:
        blocker = self.accept_sell(self.fund(self.create_sell(seller_id=85)))
        self.service.confirm_sent(blocker.order_id, actor_id=blocker.buyer_id)
        self.service.confirm_received(blocker.order_id, actor_id=blocker.seller_id)
        target_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=100,
        )
        target = target_service.create_sell(
            seller_id=86,
            seller_name="Seller 86",
            receive_address=address(20_086),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.wallet.broadcast_error = UncertainSendFailure("unknown")
        self.assertEqual(self.service.mine().state, "uncertain")
        self.wallet.broadcast_error = None

        self.explorer.current_tip = Tip(h(903), 101)
        self.explorer.set_outputs(blocker.deposit_addr, [110], confirmations=7)
        self.explorer.set_outputs(target.deposit_addr, [110])
        credited = target_service.check_deposit(target.order_id, actor_id=86)
        self.assertEqual(credited.state, "awaiting_deposit")
        self.assertEqual(
            self.store.deposit_accounting(order_id=target.order_id),
            {"credited_units": 110, "main_units": 110, "recovery_units": 0},
        )

        while self.clock.value <= target.deposit_deadline:
            self.clock()
        halted = target_service.check_deposit(target.order_id, actor_id=86)

        self.assertEqual(halted.state, "awaiting_deposit")
        self.assertEqual(self.store.count_transfers(order_id=target.order_id), 0)
        blocker_transfer = self.store.get_order_transfer(order_id=blocker.order_id)
        self.mark_transfer_broadcast(
            transfer_id=blocker_transfer["transfer_id"],
            observed_txid=blocker_transfer["txid"],
            observed_status="mempool",
            now=self.clock(),
        )

        deferred = target_service.check_deposit(target.order_id, actor_id=86)

        self.assertEqual(deferred.state, "refund_reserved")
        target_transfer = self.store.get_order_transfer(order_id=target.order_id)
        self.assertEqual(target_transfer["kind"], "refund")
        self.assertEqual(target_transfer["state"], "queued")
        self.assertEqual(
            self.store.transfer_allocation_units(
                transfer_id=target_transfer["transfer_id"]
            ),
            110,
        )
        target_service.check_deposit(target.order_id, actor_id=86)
        self.assertEqual(self.store.count_transfers(order_id=target.order_id), 1)

    def test_timely_wts_credit_advances_on_same_tip_health_repair(self) -> None:
        blocker = self.accept_sell(self.fund(self.create_sell(seller_id=92)))
        self.service.confirm_sent(blocker.order_id, actor_id=blocker.buyer_id)
        self.service.confirm_received(blocker.order_id, actor_id=blocker.seller_id)
        target_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=100,
        )
        target = target_service.create_sell(
            seller_id=93,
            seller_name="Seller 93",
            receive_address=address(20_093),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.wallet.broadcast_error = UncertainSendFailure("unknown")
        self.assertEqual(self.service.mine().state, "uncertain")
        self.wallet.broadcast_error = None
        self.explorer.current_tip = Tip(h(906), 101)
        self.explorer.set_outputs(blocker.deposit_addr, [110], confirmations=7)
        self.explorer.set_outputs(target.deposit_addr, [137])
        self.assertLess(self.clock.value + 1, target.deposit_deadline)

        halted = target_service.check_deposit(target.order_id, actor_id=93)

        self.assertEqual(halted.state, "awaiting_deposit")
        self.assertEqual(halted.events, ())
        self.assertEqual(
            self.store.deposit_accounting(order_id=target.order_id),
            {"credited_units": 137, "main_units": 110, "recovery_units": 27},
        )
        self.assertEqual(self.store.count_transfers(order_id=target.order_id), 0)
        blocker_transfer = self.store.get_order_transfer(order_id=blocker.order_id)
        self.mark_transfer_broadcast(
            transfer_id=blocker_transfer["transfer_id"],
            observed_txid=blocker_transfer["txid"],
            observed_status="mempool",
            now=self.clock(),
        )
        self.assertLess(self.clock.value + 1, target.deposit_deadline)

        winner = target_service.check_deposit(target.order_id, actor_id=93)

        self.assertEqual(winner.state, "open")
        self.assertEqual(
            [event.kind for event in winner.events],
            ["payment_ready", "excess_deposit_recovery"],
        )
        self.assertEqual(winner.events[1].detail, {"units": 27})
        self.assertEqual(self.store.count_transfers(order_id=target.order_id), 0)
        replay = target_service.check_deposit(target.order_id, actor_id=93)
        self.assertEqual(replay.state, "open")
        self.assertEqual(replay.events, ())

    def test_timely_wtb_credit_advances_on_same_tip_health_repair(self) -> None:
        blocker = self.accept_sell(self.fund(self.create_sell(seller_id=94)))
        self.service.confirm_sent(blocker.order_id, actor_id=blocker.buyer_id)
        self.service.confirm_received(blocker.order_id, actor_id=blocker.seller_id)
        target_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=100,
        )
        target = target_service.create_buy(
            buyer_id=95,
            buyer_name="Buyer 95",
            receive_address=address(30_095),
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )
        accepted = target_service.accept(
            target.order_id,
            actor_id=96,
            actor_name="Seller 96",
            receive_address=address(20_096),
        )
        self.wallet.broadcast_error = UncertainSendFailure("unknown")
        self.assertEqual(self.service.mine().state, "uncertain")
        self.wallet.broadcast_error = None
        self.explorer.current_tip = Tip(h(907), 101)
        self.explorer.set_outputs(blocker.deposit_addr, [110], confirmations=7)
        self.explorer.set_outputs(accepted.deposit_addr, [110])
        self.assertLess(self.clock.value + 1, accepted.deposit_deadline)

        halted = target_service.check_deposit(accepted.order_id, actor_id=96)

        self.assertEqual(halted.state, "awaiting_deposit")
        self.assertEqual(halted.events, ())
        self.assertEqual(
            self.store.deposit_accounting(order_id=accepted.order_id),
            {"credited_units": 110, "main_units": 110, "recovery_units": 0},
        )
        self.assertEqual(self.store.count_transfers(order_id=accepted.order_id), 0)
        blocker_transfer = self.store.get_order_transfer(order_id=blocker.order_id)
        self.mark_transfer_broadcast(
            transfer_id=blocker_transfer["transfer_id"],
            observed_txid=blocker_transfer["txid"],
            observed_status="mempool",
            now=self.clock(),
        )
        self.assertLess(self.clock.value + 1, accepted.deposit_deadline)

        winner = target_service.check_deposit(accepted.order_id, actor_id=96)

        self.assertEqual(winner.state, "matched")
        self.assertEqual([event.kind for event in winner.events], ["payment_ready"])
        self.assertEqual(self.store.count_transfers(order_id=accepted.order_id), 0)
        replay = target_service.check_deposit(accepted.order_id, actor_id=96)
        self.assertEqual(replay.state, "matched")
        self.assertEqual(replay.events, ())

    def test_late_wts_recovery_waits_for_global_health_repair(self) -> None:
        blocker = self.accept_sell(self.fund(self.create_sell(seller_id=87)))
        self.service.confirm_sent(blocker.order_id, actor_id=blocker.buyer_id)
        self.service.confirm_received(blocker.order_id, actor_id=blocker.seller_id)
        target_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=100,
        )
        target = target_service.create_sell(
            seller_id=88,
            seller_name="Seller 88",
            receive_address=address(20_088),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.wallet.broadcast_error = UncertainSendFailure("unknown")
        self.assertEqual(self.service.mine().state, "uncertain")
        self.wallet.broadcast_error = None
        while self.clock.value <= target.deposit_deadline:
            self.clock()
        self.explorer.current_tip = Tip(h(904), 101)
        self.explorer.set_outputs(blocker.deposit_addr, [110], confirmations=7)
        self.explorer.set_outputs(target.deposit_addr, [110])

        halted = target_service.check_deposit(target.order_id, actor_id=88)

        self.assertEqual(halted.state, "awaiting_deposit")
        self.assertEqual(halted.events, ())
        self.assertEqual(
            self.store.deposit_accounting(order_id=target.order_id),
            {"credited_units": 110, "main_units": 0, "recovery_units": 110},
        )
        replay = target_service.check_deposit(target.order_id, actor_id=88)
        self.assertEqual(replay.events, ())
        self.assertEqual(self.store.count_transfers(order_id=target.order_id), 0)
        blocker_transfer = self.store.get_order_transfer(order_id=blocker.order_id)
        self.mark_transfer_broadcast(
            transfer_id=blocker_transfer["transfer_id"],
            observed_txid=blocker_transfer["txid"],
            observed_status="mempool",
            now=self.clock(),
        )

        winner = target_service.check_deposit(target.order_id, actor_id=88)

        self.assertEqual(winner.state, "refund_reserved")
        self.assertEqual(
            [event.kind for event in winner.events], ["late_payment_recovery"]
        )
        self.assertEqual(winner.events[0].detail, {"units": 110})
        with closing(self.store.connect()) as conn:
            recovery = conn.execute(
                "SELECT * FROM transfers WHERE order_id=? AND kind='recovery_refund'",
                (target.order_id,),
            ).fetchone()
        self.assertIsNotNone(recovery)
        self.assertEqual(
            (recovery["amount_units"], recovery["network_fee_units"]), (100, 10)
        )
        self.assertEqual(
            self.store.transfer_allocation_units(transfer_id=recovery["transfer_id"]),
            110,
        )
        final_replay = target_service.check_deposit(target.order_id, actor_id=88)
        self.assertEqual(final_replay.events, ())
        self.assertEqual(self.store.count_transfers(order_id=target.order_id), 1)

    def test_late_wtb_dust_waits_for_global_health_repair(self) -> None:
        blocker = self.accept_sell(self.fund(self.create_sell(seller_id=89)))
        self.service.confirm_sent(blocker.order_id, actor_id=blocker.buyer_id)
        self.service.confirm_received(blocker.order_id, actor_id=blocker.seller_id)
        target_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            deposit_timeout_seconds=100,
        )
        target = target_service.create_buy(
            buyer_id=90,
            buyer_name="Buyer 90",
            receive_address=address(30_090),
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )
        accepted = target_service.accept(
            target.order_id,
            actor_id=91,
            actor_name="Seller 91",
            receive_address=address(20_091),
        )
        self.wallet.broadcast_error = UncertainSendFailure("unknown")
        self.assertEqual(self.service.mine().state, "uncertain")
        self.wallet.broadcast_error = None
        while self.clock.value <= accepted.deposit_deadline:
            self.clock()
        self.explorer.current_tip = Tip(h(905), 101)
        self.explorer.set_outputs(blocker.deposit_addr, [110], confirmations=7)
        self.explorer.set_outputs(accepted.deposit_addr, [10])

        halted = target_service.check_deposit(accepted.order_id, actor_id=91)

        self.assertEqual(halted.state, "awaiting_deposit")
        self.assertEqual(halted.events, ())
        self.assertEqual(
            self.store.deposit_accounting(order_id=accepted.order_id),
            {"credited_units": 10, "main_units": 0, "recovery_units": 10},
        )
        with closing(self.store.connect()) as conn:
            audit_before_replays = conn.execute(
                """
                SELECT COUNT(*) FROM audit_events
                WHERE order_id=? AND event_type='deposit_reconciled'
                """,
                (accepted.order_id,),
            ).fetchone()[0]
        for _ in range(6):
            replay = target_service.check_deposit(accepted.order_id, actor_id=91)
            self.assertEqual(replay.state, "awaiting_deposit")
            self.assertEqual(replay.events, ())
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    """
                    SELECT COUNT(*) FROM audit_events
                    WHERE order_id=? AND event_type='deposit_reconciled'
                    """,
                    (accepted.order_id,),
                ).fetchone()[0],
                audit_before_replays,
            )
        self.assertEqual(self.store.count_transfers(order_id=accepted.order_id), 0)
        blocker_transfer = self.store.get_order_transfer(order_id=blocker.order_id)
        self.mark_transfer_broadcast(
            transfer_id=blocker_transfer["transfer_id"],
            observed_txid=blocker_transfer["txid"],
            observed_status="mempool",
            now=self.clock(),
        )

        winner = target_service.check_deposit(accepted.order_id, actor_id=91)

        self.assertEqual(winner.state, "recovery_hold")
        self.assertEqual(
            [event.kind for event in winner.events], ["late_payment_recovery"]
        )
        self.assertEqual(winner.events[0].detail, {"units": 10})
        self.assertEqual(self.store.count_transfers(order_id=accepted.order_id), 0)
        self.assertEqual(
            self.store.order_liability_units(order_id=accepted.order_id), 10
        )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    """
                    SELECT COUNT(*) FROM audit_events
                    WHERE order_id=? AND event_type='deposit_reconciled'
                    """,
                    (accepted.order_id,),
                ).fetchone()[0],
                audit_before_replays + 1,
            )
        final_replay = target_service.check_deposit(accepted.order_id, actor_id=91)
        self.assertEqual(final_replay.events, ())
        self.assertEqual(self.store.count_transfers(order_id=accepted.order_id), 0)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    """
                    SELECT COUNT(*) FROM audit_events
                    WHERE order_id=? AND event_type='deposit_reconciled'
                    """,
                    (accepted.order_id,),
                ).fetchone()[0],
                audit_before_replays + 1,
            )

    def test_expiry_and_reconcile_share_one_writer_race(self) -> None:
        for winner_now, expected_state, expected_main, expected_recovery in (
            (200, "open", 110, 0),
            (201, "deposit_expired", 0, 110),
        ):
            with self.subTest(winner_now=winner_now):
                with tempfile.TemporaryDirectory() as root:
                    store = Store(Path(root) / "race.db", network="btc09-regtest")
                    store.initialize()
                    deposit = address(60_000 + winner_now)
                    order_id = store.create_order(
                        side=OrderSide.SELL,
                        maker_id=winner_now,
                        maker_name=f"Seller {winner_now}",
                        maker_wallet_addr=address(70_000 + winner_now),
                        net_amount_units=100,
                        network_fee_units=10,
                        service_fee_units=0,
                        deposit_required_units=110,
                        total_price="2",
                        settlement_asset="AUD",
                        settlement_network=None,
                        payment_method="PayID",
                        state=OrderState.AWAITING_DEPOSIT,
                        deposit_addr=deposit,
                        deposit_deadline=200,
                        created_at=100,
                        updated_at=100,
                    )
                    snapshot = {
                        "network": "btc09-regtest",
                        "address": deposit,
                        "complete": True,
                        "tip_hash": h(990),
                        "tip_height": 100,
                        "outputs": [
                            {
                                "txid": h(61_000 + winner_now),
                                "vout": 0,
                                "amount_units": 110,
                                "block_hash": h(62_000 + winner_now),
                                "block_height": 95,
                                "confirmations": 6,
                                "coinbase": False,
                                "mature": True,
                                "spent_by": None,
                            }
                        ],
                    }
                    winner_holds_writer = threading.Event()
                    release_winner = threading.Event()
                    original = store._enforce_deposit_deadline_conn

                    def gate(conn, *, order_id, network, now, allow_fund_movement):
                        if now == winner_now:
                            winner_holds_writer.set()
                            if not release_winner.wait(timeout=5):
                                raise RuntimeError("deadline race gate timed out")
                        return original(
                            conn,
                            order_id=order_id,
                            network=network,
                            now=now,
                            allow_fund_movement=allow_fund_movement,
                        )

                    store._enforce_deposit_deadline_conn = gate  # type: ignore[method-assign]

                    def reconcile(now: int):
                        return store.reconcile_all_deposit_outputs(
                            network="btc09-regtest",
                            expected_tip_hash=h(990),
                            expected_tip_height=100,
                            snapshots=(snapshot,),
                            final_tip_hash=h(990),
                            final_tip_height=100,
                            credit_depth=6,
                            now=now,
                        )

                    loser_now = 201 if winner_now == 200 else 200
                    with ThreadPoolExecutor(max_workers=2) as pool:
                        winner = pool.submit(reconcile, winner_now)
                        self.assertTrue(winner_holds_writer.wait(timeout=5))
                        loser = pool.submit(reconcile, loser_now)
                        release_winner.set()
                        winner.result(timeout=5)
                        loser.result(timeout=5)
                    row = store.get_order(order_id=order_id)
                    accounting = store.deposit_accounting(order_id=order_id)
                    self.assertEqual(row["state"], expected_state)
                    self.assertEqual(accounting["main_units"], expected_main)
                    self.assertEqual(accounting["recovery_units"], expected_recovery)

    def test_post_prepare_tip_race_requeues_without_network_or_wedge(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        self.wallet.prepare_side_effect = lambda: setattr(
            self.explorer, "current_tip", Tip(h(901), 101)
        )

        safe = self.service.mine()

        self.assertEqual(safe.state, "queued")
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.wallet.prepare_side_effect = None
        self.explorer.set_outputs(order.deposit_addr, [110], confirmations=7)
        completed = self.service.mine()
        self.assertEqual(completed.state, "broadcast")
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

    def test_post_prepare_snapshot_hash_race_requeues_safely(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        original_prepare = self.wallet.prepare

        def mismatched_snapshot(*args):
            prepared = original_prepare(*args)
            return PreparedTransfer(
                prepared.txid,
                prepared.signed_tx_hex,
                prepared.destination,
                prepared.amount_units,
                prepared.fee_units,
                prepared.snapshot_tip,
                h(123_456),
                prepared.selected_outpoints,
            )

        self.wallet.prepare = mismatched_snapshot  # type: ignore[method-assign]

        safe = self.service.mine()

        self.assertEqual(safe.state, "queued")
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.wallet.prepare = original_prepare  # type: ignore[method-assign]
        self.assertEqual(self.service.mine().state, "broadcast")

    def test_post_prepare_new_watched_address_requeues_exact_attempt(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        created: list[object] = []

        def add_watched_address() -> None:
            created.append(self.create_sell(seller_id=97))

        self.wallet.prepare_side_effect = add_watched_address

        safe = self.service.mine()

        self.assertEqual(safe.state, "queued")
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.assertEqual(len(created), 1)
        added = created[0]
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM deposit_scans WHERE address=?",
                    (added.deposit_addr,),
                ).fetchone()[0],
                0,
            )
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        transfer_id = transfer["transfer_id"]
        operation_key = transfer["operation_key"]
        self.wallet.prepare_side_effect = None

        completed = self.service.mine()

        self.assertEqual(completed.state, "broadcast")
        self.assertEqual(completed.transfer_id, transfer_id)
        self.assertEqual(completed.operation_key, operation_key)
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

    def test_typed_txid_bytes_mismatch_is_hard_and_stays_reserved(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        original_prepare = self.wallet.prepare

        def wrong_txid(*args):
            prepared = original_prepare(*args)
            return PreparedTransfer(
                h(654_321),
                prepared.signed_tx_hex,
                prepared.destination,
                prepared.amount_units,
                prepared.fee_units,
                prepared.snapshot_tip,
                prepared.wallet_snapshot_hash,
                prepared.selected_outpoints,
            )

        self.wallet.prepare = wrong_txid  # type: ignore[method-assign]

        with self.assertRaisesRegex(AccountingInvariantError, "signed bytes"):
            self.service.mine()

        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "reserved")
        self.assertEqual(transfer["attempt_count"], 1)
        self.assertIsNone(transfer["signed_tx_hex"])
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.assertEqual(self.service.mine().state, "reserved")

    def test_typed_prepared_economics_mismatch_is_hard_and_stays_reserved(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        original_prepare = self.wallet.prepare

        def wrong_amount(*args):
            prepared = original_prepare(*args)
            return PreparedTransfer(
                prepared.txid,
                prepared.signed_tx_hex,
                prepared.destination,
                prepared.amount_units + 1,
                prepared.fee_units,
                prepared.snapshot_tip,
                prepared.wallet_snapshot_hash,
                prepared.selected_outpoints,
            )

        self.wallet.prepare = wrong_amount  # type: ignore[method-assign]

        with self.assertRaisesRegex(AccountingInvariantError, "metadata differs"):
            self.service.mine()

        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "reserved")
        self.assertIsNone(transfer["txid"])
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

    def test_post_prepare_explorer_failure_requeues_safely(self) -> None:
        order = self.accept_sell(self.fund(self.create_sell()))
        self.service.confirm_sent(order.order_id, actor_id=order.buyer_id)
        self.service.confirm_received(order.order_id, actor_id=order.seller_id)
        original_tip = self.explorer.tip

        def fail_tip():
            raise ExplorerProtocolError("tip unavailable")

        self.explorer.tip = fail_tip  # type: ignore[method-assign]

        safe = self.service.mine()

        self.assertEqual(safe.state, "queued")
        self.assertEqual(self.wallet.broadcast_calls, [])
        self.explorer.tip = original_tip  # type: ignore[method-assign]
        self.assertEqual(self.service.mine().state, "broadcast")

    def test_receive_addresses_are_canonical_before_any_mutation(self) -> None:
        valid = address(50_000)
        invalid = (
            address(50_001, version=0x00),
            valid[:-1] + ("1" if valid[-1] != "1" else "2"),
            "é" + valid,
            "1" * 129,
        )
        for candidate in invalid:
            with self.subTest(candidate=candidate[:16]):
                with self.assertRaises(ValueError):
                    self.service.create_sell(
                        seller_id=90,
                        seller_name="Seller 90",
                        receive_address=candidate,
                        net_amount=100,
                        total_price="2",
                        asset="AUD",
                        method="PayID",
                        network=None,
                    )
        self.assertEqual(self.wallet.address_count, 0)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0], 0
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM users").fetchone()[0], 0
            )

        buy = self.service.create_buy(
            buyer_id=89,
            buyer_name="Buyer 89",
            receive_address=address(50_089),
            net_amount=100,
            total_price="2",
            asset="USD",
            method="Wise",
            network=None,
        )
        before_accept = self.wallet.address_count
        with self.assertRaises(ValueError):
            self.service.accept(
                buy.order_id,
                actor_id=88,
                actor_name="Seller 88",
                receive_address=address(50_088, version=0x00),
            )
        self.assertEqual(self.wallet.address_count, before_accept)
        with closing(self.store.connect()) as conn:
            self.assertIsNone(
                conn.execute("SELECT 1 FROM users WHERE user_id=88").fetchone()
            )

        sell = self.service.create_sell(
            seller_id=90,
            seller_name="Seller 90",
            receive_address=valid,
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.assertEqual(sell.seller_id, 90)

        with tempfile.TemporaryDirectory() as root:
            main_store = Store(Path(root) / "main.db", network="btc09-mainnet")
            main_store.initialize()
            main_explorer = FakeExplorer()
            main_explorer.network = "btc09-mainnet"
            main_wallet = FakeWallet(main_explorer)
            main_wallet.network = "btc09-mainnet"
            main_service = TradeService(
                store=main_store,
                explorer=main_explorer,
                wallet=main_wallet,
                fresh_address=main_wallet.new_address,
                confirmation_depth=6,
                clock=Clock(),
            )
            main = main_service.create_buy(
                buyer_id=91,
                buyer_name="Buyer 91",
                receive_address=address(50_091),
                net_amount=100,
                total_price="2",
                asset="USD",
                method="Wise",
                network=None,
            )
            self.assertEqual(main.buyer_id, 91)

    def test_provisional_outputs_are_restricted_and_do_not_mask_solvency(self) -> None:
        order = self.create_sell()
        self.explorer.set_outputs(order.deposit_addr, [110], confirmations=5)
        checked = self.service.check_deposit(order.order_id, actor_id=1)
        self.assertEqual(checked.state, "awaiting_deposit")
        # No transfer exists yet, but the all-address barrier must preserve the
        # exact provisional outpoint for the later solvency/selection gate.
        self.assertEqual(self.store.provisional_restricted_units(), 110)


if __name__ == "__main__":
    unittest.main()
