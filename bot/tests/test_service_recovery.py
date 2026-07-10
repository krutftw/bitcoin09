from __future__ import annotations

import json
import tempfile
import threading
import unittest
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from pathlib import Path

from bot.otc.explorer import (
    BlockAnchor,
    ConfirmedOutput,
    ConfirmedSpend,
    ExplorerTransportError,
    TransactionStatus,
)
from bot.otc.service import AuthorizationError, OrderConflict, TradeService
from bot.otc.store import AccountingInvariantError, Store
from bot.otc.wallet import WalletSnapshot
from bot.tests.test_service_orders import (
    Clock,
    FakeExplorer,
    FakeWallet,
    address,
    h,
)


class RecoveryExplorer(FakeExplorer):
    def __init__(self) -> None:
        super().__init__()
        self.transactions: dict[str, TransactionStatus] = {}
        self.transaction_calls: list[str] = []
        self.fail_batches = False
        self.fail_transactions = False
        self.transaction_barrier: threading.Barrier | None = None
        self.transaction_entered: threading.Event | None = None
        self.transaction_release: threading.Event | None = None
        self.transaction_return_hook: Callable[[str], None] | None = None

    def batch_outputs(self, read_watched_addresses: object):
        if self.fail_batches:
            raise ExplorerTransportError("offline")
        return super().batch_outputs(read_watched_addresses)

    def transaction(self, txid: str) -> TransactionStatus:
        if self.fail_transactions:
            raise ExplorerTransportError("transaction endpoint offline")
        self.transaction_calls.append(txid)
        status = self.transactions.get(
            txid,
            TransactionStatus(txid, "unknown", None, 0, self.current_tip),
        )
        if self.transaction_entered is not None:
            self.transaction_entered.set()
        if self.transaction_barrier is not None:
            self.transaction_barrier.wait(timeout=5)
        if self.transaction_release is not None and not self.transaction_release.wait(
            timeout=5
        ):
            raise RuntimeError("transaction test gate timed out")
        if self.transaction_return_hook is not None:
            self.transaction_return_hook(txid)
        return status

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


class MatrixHarness:
    def __init__(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(Path(self.tmp.name) / "matrix.db", network="btc09-regtest")
        self.store.initialize()
        self.explorer = RecoveryExplorer()
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
            transfer_reconciliation_deadline_seconds=30,
        )

    def close(self) -> None:
        self.tmp.cleanup()

    def create_sell(self, seller_id: int = 1):
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

    def create_buy(self, buyer_id: int = 11):
        return self.service.create_buy(
            buyer_id=buyer_id,
            buyer_name=f"Buyer {buyer_id}",
            receive_address=address(30_000 + buyer_id),
            net_amount=100,
            total_price="2",
            asset="USDT",
            method="Wallet transfer",
            network="TRC20",
        )

    def accept_sell(self, order: object, buyer_id: int = 2):
        return self.service.accept(
            order.order_id,  # type: ignore[attr-defined]
            actor_id=buyer_id,
            actor_name=f"Buyer {buyer_id}",
            receive_address=address(30_000 + buyer_id),
        )

    def accept_buy(self, order: object, seller_id: int = 12):
        return self.service.accept(
            order.order_id,  # type: ignore[attr-defined]
            actor_id=seller_id,
            actor_name=f"Seller {seller_id}",
            receive_address=address(20_000 + seller_id),
        )

    def credit(self, order: object, amounts: list[int]):
        self.explorer.set_outputs(order.deposit_addr, amounts)  # type: ignore[attr-defined]
        return self.service.check_deposit(
            order.order_id,
            actor_id=order.seller_id,  # type: ignore[attr-defined]
        )

    @staticmethod
    def race(*calls: object) -> tuple[tuple[str, object], ...]:
        barrier = threading.Barrier(len(calls))

        def invoke(call: object) -> tuple[str, object]:
            barrier.wait()
            try:
                return "ok", call()  # type: ignore[operator]
            except (
                AuthorizationError,
                OrderConflict,
                AccountingInvariantError,
                ValueError,
            ) as exc:
                return "expected_error", exc

        with ThreadPoolExecutor(max_workers=len(calls)) as pool:
            return tuple(pool.map(invoke, calls))

    def transfers(self, order_id: int) -> tuple[dict[str, object], ...]:
        with closing(self.store.connect()) as conn:
            return tuple(
                dict(row)
                for row in conn.execute(
                    "SELECT * FROM transfers WHERE order_id=? ORDER BY transfer_id",
                    (order_id,),
                ).fetchall()
            )

    def allocation_units(self, transfer_id: int) -> int:
        return self.store.transfer_allocation_units(transfer_id=transfer_id)


class TradeServiceRecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(Path(self.tmp.name) / "otc.db", network="btc09-regtest")
        self.store.initialize()
        self.explorer = RecoveryExplorer()
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
            transfer_reconciliation_deadline_seconds=30,
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

    def create_buy(self, *, buyer_id: int = 11):
        return self.service.create_buy(
            buyer_id=buyer_id,
            buyer_name=f"Buyer {buyer_id}",
            receive_address=address(30_000 + buyer_id),
            net_amount=100,
            total_price="2",
            asset="USDT",
            method="Wallet transfer",
            network="TRC20",
        )

    def fund(self, order: object, amount: int = 110):
        self.explorer.set_outputs(order.deposit_addr, [amount])  # type: ignore[attr-defined]
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

    def accept_buy(self, order: object, *, seller_id: int = 12):
        return self.service.accept(
            order.order_id,  # type: ignore[attr-defined]
            actor_id=seller_id,
            actor_name=f"Seller {seller_id}",
            receive_address=address(20_000 + seller_id),
        )

    def matched_sell(self, *, seller_id: int = 1, buyer_id: int = 2):
        return self.accept_sell(
            self.fund(self.create_sell(seller_id=seller_id)), buyer_id=buyer_id
        )

    def broadcast_release(self, *, seller_id: int = 1, buyer_id: int = 2):
        order = self.matched_sell(seller_id=seller_id, buyer_id=buyer_id)
        self.service.confirm_sent(order.order_id, actor_id=buyer_id)
        self.service.confirm_received(order.order_id, actor_id=seller_id)
        result = self.service.mine()
        self.assertIsNotNone(result)
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "broadcast")
        return order, transfer

    def prepared_release(self, *, seller_id: int, buyer_id: int):
        order = self.matched_sell(seller_id=seller_id, buyer_id=buyer_id)
        self.service.confirm_sent(order.order_id, actor_id=buyer_id)
        self.service.confirm_received(order.order_id, actor_id=seller_id)
        original = self.service._broadcast_stored
        self.service._broadcast_stored = (  # type: ignore[method-assign]
            lambda row: self.service._transfer_result(row)
        )
        try:
            self.assertEqual(self.service.mine().state, "prepared")
        finally:
            self.service._broadcast_stored = original  # type: ignore[method-assign]
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "prepared")
        return order, transfer

    def test_two_simultaneous_funded_cancellations_queue_and_send_once(self) -> None:
        order = self.fund(self.create_sell())
        barrier = threading.Barrier(2)

        def cancel() -> str:
            barrier.wait()
            return self.service.cancel(order.order_id, actor_id=order.seller_id).state

        with ThreadPoolExecutor(max_workers=2) as pool:
            states = tuple(pool.map(lambda _n: cancel(), range(2)))
        self.assertEqual(states, ("refund_reserved", "refund_reserved"))
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

        barrier = threading.Barrier(2)

        def mine() -> object:
            barrier.wait()
            return self.service.mine()

        with ThreadPoolExecutor(max_workers=2) as pool:
            tuple(pool.map(lambda _n: mine(), range(2)))
        self.assertEqual(len(self.wallet.prepare_calls), 1)
        self.assertEqual(len(self.wallet.broadcast_calls), 1)

    def test_underpayment_refunds_net_fee_or_holds_dust_until_topup(self) -> None:
        positive = self.create_sell(seller_id=3)
        self.explorer.set_outputs(positive.deposit_addr, [50])
        self.service.check_deposit(positive.order_id, actor_id=3)
        cancelled = self.service.cancel(positive.order_id, actor_id=3)
        transfer = self.store.get_order_recovery_transfer(order_id=positive.order_id)
        self.assertEqual(cancelled.state, "refund_reserved")
        self.assertEqual(transfer["kind"], "recovery_refund")
        self.assertEqual(transfer["amount_units"], 40)
        self.assertEqual(transfer["network_fee_units"], 10)

        dust = self.create_sell(seller_id=4)
        self.explorer.set_outputs(dust.deposit_addr, [10])
        self.service.check_deposit(dust.order_id, actor_id=4)
        held = self.service.cancel(dust.order_id, actor_id=4)
        self.assertEqual(held.state, "recovery_hold")
        self.assertEqual(self.store.count_transfers(order_id=dust.order_id), 0)
        self.assertIn(dust.deposit_addr, self.store.watched_deposit_addresses())

        self.explorer.current_tip = type(self.explorer.current_tip)(h(903), 100)
        self.explorer.set_outputs(dust.deposit_addr, [10, 1])
        topped = self.service.check_deposit(dust.order_id, actor_id=4)
        transfer = self.store.get_order_recovery_transfer(order_id=dust.order_id)
        self.assertEqual(topped.state, "refund_reserved")
        self.assertEqual(transfer["amount_units"], 1)
        self.assertEqual(transfer["network_fee_units"], 10)

    def test_cancellation_matrix_preserves_wts_and_closes_accepted_wtb(self) -> None:
        awaiting = self.create_sell(seller_id=5)
        self.assertEqual(
            self.service.cancel(awaiting.order_id, actor_id=5).state, "cancelled"
        )

        sell = self.matched_sell(seller_id=6, buyer_id=7)
        reopened = self.service.cancel(sell.order_id, actor_id=7)
        self.assertEqual(reopened.state, "open")
        self.assertIsNone(reopened.buyer_id)
        self.assertEqual(reopened.deposit_addr, sell.deposit_addr)

        buy = self.create_buy(buyer_id=8)
        self.assertEqual(
            self.service.cancel(buy.order_id, actor_id=8).state, "cancelled"
        )
        accepted = self.accept_buy(self.create_buy(buyer_id=9), seller_id=10)
        old_address = accepted.deposit_addr
        closed = self.service.cancel(accepted.order_id, actor_id=9)
        self.assertEqual(closed.state, "cancelled")
        self.assertEqual(closed.seller_id, 10)
        self.assertEqual(closed.deposit_addr, old_address)
        self.assertIn(old_address, self.store.watched_deposit_addresses())
        replay = self.service.accept(accepted.order_id, actor_id=13)
        self.assertFalse(replay.accepted)

    def test_accepted_wtb_partial_and_full_credit_refund_without_reopening(
        self,
    ) -> None:
        partial = self.accept_buy(self.create_buy(buyer_id=47), seller_id=48)
        partial_address = partial.deposit_addr
        self.explorer.set_outputs(partial_address, [50])
        self.service.check_deposit(partial.order_id, actor_id=48)
        partial_cancel = self.service.cancel(partial.order_id, actor_id=47)
        recovery = self.store.get_order_recovery_transfer(order_id=partial.order_id)
        self.assertEqual(partial_cancel.state, "refund_reserved")
        self.assertEqual(recovery["amount_units"], 40)
        self.assertEqual(partial_cancel.deposit_addr, partial_address)
        self.assertEqual(partial_cancel.seller_id, 48)

        full = self.accept_buy(self.create_buy(buyer_id=49), seller_id=50)
        full_address = full.deposit_addr
        funded = self.fund(full)
        self.assertEqual(funded.state, "matched")
        full_cancel = self.service.cancel(full.order_id, actor_id=50)
        transfer = self.store.get_order_transfer(order_id=full.order_id)
        self.assertEqual(full_cancel.state, "refund_reserved")
        self.assertEqual(transfer["kind"], "refund")
        self.assertEqual(full_cancel.deposit_addr, full_address)
        self.assertEqual(full_cancel.seller_id, 50)
        self.assertNotEqual(full_cancel.state, "open")

    def test_payment_flag_forces_dispute_and_timeout_only_opens_dispute(self) -> None:
        flagged = self.matched_sell(seller_id=14, buyer_id=15)
        self.service.confirm_sent(flagged.order_id, actor_id=15)
        with self.assertRaises(OrderConflict):
            self.service.cancel(flagged.order_id, actor_id=14)
        disputed = self.service.open_dispute(
            flagged.order_id, actor_id=14, reason="Payment receipt needs review"
        )
        self.assertEqual(disputed.state, "disputed")

        timed = self.matched_sell(seller_id=16, buyer_id=17)
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE orders SET trade_deadline=? WHERE order_id=?",
                (self.clock.value - 1, timed.order_id),
            )
        before = (len(self.wallet.prepare_calls), len(self.wallet.broadcast_calls))
        expired = self.service.expire_orders()
        self.assertIn(timed.order_id, expired)
        self.assertEqual(
            self.store.get_order(order_id=timed.order_id)["state"], "disputed"
        )
        self.assertEqual(
            before, (len(self.wallet.prepare_calls), len(self.wallet.broadcast_calls))
        )

    def test_matched_deadlines_start_when_external_payment_may_begin(self) -> None:
        sell = self.matched_sell(seller_id=41, buyer_id=42)
        sell_row = self.store.get_order(order_id=sell.order_id)
        self.assertEqual(sell_row["trade_deadline"], sell_row["matched_at"] + 600)

        buy = self.accept_buy(self.create_buy(buyer_id=43), seller_id=44)
        awaiting = self.store.get_order(order_id=buy.order_id)
        self.assertIsNone(awaiting["trade_deadline"])
        funded = self.fund(buy)
        funded_row = self.store.get_order(order_id=funded.order_id)
        self.assertEqual(funded.state, "matched")
        self.assertEqual(funded_row["matched_at"], funded_row["funded_at"])
        self.assertEqual(funded_row["trade_deadline"], funded_row["funded_at"] + 600)

    def test_admin_resolution_validates_private_reason_and_has_one_wallet_winner(
        self,
    ) -> None:
        order = self.matched_sell(seller_id=18, buyer_id=19)
        self.service.open_dispute(
            order.order_id, actor_id=18, reason="Buyer and seller disagree"
        )
        for winner, reason in (
            ("other", "A sufficiently long reason"),
            ("buyer", "short"),
        ):
            with self.assertRaises(ValueError):
                self.service.resolve_dispute(
                    order.order_id,
                    admin_id=99,
                    winner=winner,
                    reason=reason,
                )

        barrier = threading.Barrier(2)

        def resolve() -> str:
            barrier.wait()
            return self.service.resolve_dispute(
                order.order_id,
                admin_id=99,
                winner="buyer",
                reason="Reviewed private evidence and ruled for buyer",
            ).state

        with ThreadPoolExecutor(max_workers=2) as pool:
            states = tuple(pool.map(lambda _n: resolve(), range(2)))
        self.assertEqual(states, ("release_reserved", "release_reserved"))
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)
        with closing(self.store.connect()) as conn:
            details = [
                json.loads(row[0])
                for row in conn.execute(
                    "SELECT detail_json FROM audit_events WHERE order_id=?",
                    (order.order_id,),
                )
            ]
        self.assertEqual(
            sum("reason" in detail for detail in details),
            2,
        )
        with ThreadPoolExecutor(max_workers=2) as pool:
            tuple(pool.map(lambda _n: self.service.mine(), range(2)))
        self.assertEqual(len(self.wallet.prepare_calls), 1)
        self.assertEqual(len(self.wallet.broadcast_calls), 1)

    def test_private_reason_accepts_500_emoji_and_rejects_501_before_mutation(
        self,
    ) -> None:
        accepted = "🔐" * 500
        order = self.matched_sell(seller_id=61, buyer_id=62)
        self.service.open_dispute(
            order.order_id, actor_id=61, reason="Private evidence needs review"
        )
        result = self.service.resolve_dispute(
            order.order_id,
            admin_id=99,
            winner="buyer",
            reason=accepted,
        )
        self.assertNotIn(accepted, repr(result))
        with closing(self.store.connect()) as conn:
            row = conn.execute(
                """
                SELECT detail_json,length(CAST(detail_json AS BLOB)) AS bytes
                FROM audit_events
                WHERE order_id=? AND event_type='dispute_resolved'
                """,
                (order.order_id,),
            ).fetchone()
        self.assertEqual(json.loads(row["detail_json"])["reason"], accepted)
        self.assertLessEqual(row["bytes"], 4000)

        rejected = self.matched_sell(seller_id=63, buyer_id=64)
        self.service.open_dispute(
            rejected.order_id, actor_id=63, reason="Private evidence needs review"
        )
        with self.assertRaises(ValueError):
            self.service.resolve_dispute(
                rejected.order_id,
                admin_id=99,
                winner="seller",
                reason="🔐" * 501,
            )
        with self.assertRaises(ValueError):
            self.service.resolve_dispute(
                rejected.order_id,
                admin_id=99,
                winner="seller",
                reason="control\ncharacters are rejected",
            )
        self.assertEqual(
            self.store.get_order(order_id=rejected.order_id)["state"], "disputed"
        )
        self.assertEqual(self.store.count_transfers(order_id=rejected.order_id), 0)
        with closing(self.store.connect()) as conn:
            resolutions = conn.execute(
                """
                SELECT COUNT(*) FROM audit_events
                WHERE order_id=? AND event_type='dispute_resolved'
                """,
                (rejected.order_id,),
            ).fetchone()[0]
        self.assertEqual(resolutions, 0)

    def test_seller_resolution_queues_refund_economics(self) -> None:
        order = self.matched_sell(seller_id=20, buyer_id=21)
        self.service.open_dispute(
            order.order_id, actor_id=21, reason="Settlement cannot be verified"
        )
        resolved = self.service.resolve_dispute(
            order.order_id,
            admin_id=99,
            winner="seller",
            reason="Reviewed both accounts and ruled for seller",
        )
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(resolved.state, "refund_reserved")
        self.assertEqual(transfer["kind"], "resolve_seller")
        self.assertEqual(transfer["amount_units"], 100)

    def test_restart_queued_reserved_and_prepared_reuse_one_operation(self) -> None:
        order = self.matched_sell(seller_id=22, buyer_id=23)
        self.service.confirm_sent(order.order_id, actor_id=23)
        self.service.confirm_received(order.order_id, actor_id=22)
        queued = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(queued["state"], "queued")
        self.service.reconcile_transfers()
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(transfer["state"], "broadcast")
        self.assertEqual(len(self.wallet.prepare_calls), 1)
        self.explorer.set_transaction(transfer["txid"], "confirmed", confirmations=6)
        self.service.reconcile_transfers()

        second = self.matched_sell(seller_id=24, buyer_id=25)
        self.service.confirm_sent(second.order_id, actor_id=25)
        self.service.confirm_received(second.order_id, actor_id=24)
        batch, _ = self.service._reconcile_all_deposits()
        snapshot = self.wallet.snapshot(batch.tip)
        claimed = self.store.claim_next_transfer(
            expected_tip_hash=batch.tip.hash,
            expected_tip_height=batch.tip.height,
            wallet_spendable_units=snapshot.spendable_units,
            now=self.clock(),
        )
        self.assertIsNotNone(claimed)
        alive = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            prepare_child_is_dead=lambda _row: False,
            transfer_reconciliation_deadline_seconds=30,
        )
        self.assertEqual(alive.reconcile_transfers()[0].state, "reserved")
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
            transfer_reconciliation_deadline_seconds=30,
        )
        recovered = dead.reconcile_transfers()
        self.assertEqual(recovered[-1].state, "broadcast")
        self.assertEqual(self.store.count_transfers(order_id=second.order_id), 1)
        self.assertEqual(len(self.wallet.prepare_calls), 2)

    def test_prepared_restart_checks_exact_tx_and_rebroadcasts_stored_bytes(
        self,
    ) -> None:
        order = self.matched_sell(seller_id=26, buyer_id=27)
        self.service.confirm_sent(order.order_id, actor_id=27)
        self.service.confirm_received(order.order_id, actor_id=26)
        original = self.service._broadcast_stored
        self.service._broadcast_stored = lambda row: self.service._transfer_result(row)  # type: ignore[method-assign]
        self.assertEqual(self.service.mine().state, "prepared")
        self.service._broadcast_stored = original  # type: ignore[method-assign]
        prepared = self.store.get_order_transfer(order_id=order.order_id)
        self.service.reconcile_transfers()
        self.assertEqual(self.explorer.transaction_calls[-1], prepared["txid"])
        self.assertEqual(self.wallet.broadcast_calls[-1][0], prepared["signed_tx_hex"])
        self.assertEqual(self.wallet.broadcast_calls[-1][1], prepared["txid"])
        self.assertEqual(self.store.count_transfers(order_id=order.order_id), 1)

    def test_broadcast_confirms_at_depth_and_unknown_deadline_becomes_uncertain(
        self,
    ) -> None:
        order, transfer = self.broadcast_release(seller_id=28, buyer_id=29)
        self.explorer.set_transaction(transfer["txid"], "confirmed", confirmations=5)
        self.service.reconcile_transfers()
        self.assertEqual(
            self.store.get_order_transfer(order_id=order.order_id)["state"], "broadcast"
        )
        self.explorer.set_transaction(transfer["txid"], "confirmed", confirmations=6)
        self.service.reconcile_transfers()
        self.assertEqual(
            self.store.get_order_transfer(order_id=order.order_id)["state"], "confirmed"
        )
        self.assertEqual(
            self.store.get_order(order_id=order.order_id)["state"], "completed"
        )

        other, other_transfer = self.broadcast_release(seller_id=30, buyer_id=31)
        self.clock.value = other_transfer["broadcast_at"] + 31
        self.explorer.set_transaction(other_transfer["txid"], "unknown")
        self.service.reconcile_transfers()
        uncertain = self.store.get_order_transfer(order_id=other.order_id)
        self.assertEqual(uncertain["state"], "uncertain")
        self.assertEqual(self.store.order_liability_units(order_id=other.order_id), 110)

    def test_newer_tip_uncertainty_blocks_delayed_old_tip_confirmation(self) -> None:
        order, transfer = self.broadcast_release(seller_id=53, buyer_id=54)
        queued_order = self.matched_sell(seller_id=55, buyer_id=56)
        self.service.confirm_sent(queued_order.order_id, actor_id=56)
        self.service.confirm_received(queued_order.order_id, actor_id=55)
        queued = self.store.get_order_transfer(order_id=queued_order.order_id)
        self.assertEqual(queued["state"], "queued")

        tip_a = self.explorer.current_tip
        tip_b = type(tip_a)(h(901), tip_a.height)
        explorer_a = RecoveryExplorer()
        explorer_a.current_tip = tip_a
        explorer_a.outputs_by_address = dict(self.explorer.outputs_by_address)
        explorer_a.set_transaction(
            transfer["txid"], "confirmed", confirmations=6, block_number=93_001
        )
        explorer_a.transaction_entered = threading.Event()
        explorer_a.transaction_release = threading.Event()

        explorer_b = RecoveryExplorer()
        explorer_b.current_tip = tip_b
        explorer_b.outputs_by_address = dict(self.explorer.outputs_by_address)
        explorer_b.set_transaction(transfer["txid"], "unknown")
        self.clock.value = transfer["broadcast_at"] + 31

        def make_service(explorer: RecoveryExplorer) -> TradeService:
            return TradeService(
                store=self.store,
                explorer=explorer,
                wallet=self.wallet,
                fresh_address=self.wallet.new_address,
                confirmation_depth=6,
                clock=self.clock,
                network_fee_units=10,
                transfer_reconciliation_deadline_seconds=30,
            )

        service_a = make_service(explorer_a)
        service_b = make_service(explorer_b)
        with ThreadPoolExecutor(max_workers=1) as pool:
            delayed = pool.submit(service_a.reconcile_transfers)
            self.assertTrue(explorer_a.transaction_entered.wait(timeout=5))
            service_b.reconcile_transfers()
            uncertain = self.store.get_order_transfer(order_id=order.order_id)
            self.assertEqual(uncertain["state"], "uncertain")
            explorer_a.transaction_release.set()
            with self.assertRaises(AccountingInvariantError):
                delayed.result(timeout=5)

        final = self.store.get_order_transfer(order_id=order.order_id)
        self.assertEqual(final["state"], "uncertain")
        self.assertEqual(self.store.order_liability_units(order_id=order.order_id), 110)
        prepare_count = len(self.wallet.prepare_calls)
        self.assertEqual(service_b.mine().state, "uncertain")
        self.assertEqual(len(self.wallet.prepare_calls), prepare_count)
        self.assertEqual(
            self.store.get_order_transfer(order_id=queued_order.order_id)["state"],
            "queued",
        )

    def test_tip_flip_after_reconciliation_cannot_claim_follow_on_transfer(
        self,
    ) -> None:
        first_order, first = self.broadcast_release(seller_id=61, buyer_id=62)
        queued_order = self.matched_sell(seller_id=63, buyer_id=64)
        self.service.confirm_sent(queued_order.order_id, actor_id=64)
        self.service.confirm_received(queued_order.order_id, actor_id=63)
        self.assertEqual(
            self.store.get_order_transfer(order_id=queued_order.order_id)["state"],
            "queued",
        )

        tip_a = self.explorer.current_tip
        tip_b = type(tip_a)(h(908), tip_a.height)
        self.explorer.set_transaction(first["txid"], "confirmed", confirmations=6)
        self.explorer.transaction_return_hook = lambda _txid: setattr(
            self.explorer, "current_tip", tip_b
        )
        self.wallet.prepare_calls.clear()
        self.wallet.broadcast_calls.clear()

        with self.assertRaises(AccountingInvariantError):
            self.service.reconcile_transfers()

        self.assertEqual(
            self.store.get_order_transfer(order_id=first_order.order_id)["state"],
            "confirmed",
        )
        self.assertEqual(
            self.store.get_order_transfer(order_id=queued_order.order_id)["state"],
            "queued",
        )
        self.assertEqual(self.wallet.prepare_calls, [])
        self.assertEqual(self.wallet.broadcast_calls, [])

    def test_public_mine_reconciles_confirmed_reorg_before_next_claim(self) -> None:
        first_order, first = self.broadcast_release(seller_id=65, buyer_id=66)
        self.explorer.set_transaction(first["txid"], "confirmed", confirmations=6)
        self.service.reconcile_transfers()
        self.assertEqual(
            self.store.get_order_transfer(order_id=first_order.order_id)["state"],
            "confirmed",
        )

        queued_order = self.matched_sell(seller_id=67, buyer_id=68)
        self.service.confirm_sent(queued_order.order_id, actor_id=68)
        self.service.confirm_received(queued_order.order_id, actor_id=67)
        self.assertEqual(
            self.store.get_order_transfer(order_id=queued_order.order_id)["state"],
            "queued",
        )

        old_tip = self.explorer.current_tip
        self.explorer.current_tip = type(old_tip)(h(909), old_tip.height)
        self.explorer.set_transaction(first["txid"], "unknown")
        self.explorer.transaction_calls.clear()
        self.wallet.prepare_calls.clear()
        self.wallet.broadcast_calls.clear()

        result = self.service.mine()

        self.assertEqual(result.state, "uncertain")
        self.assertEqual(self.explorer.transaction_calls, [first["txid"]])
        self.assertEqual(
            self.store.get_order_transfer(order_id=first_order.order_id)["state"],
            "uncertain",
        )
        self.assertEqual(
            self.store.get_order_transfer(order_id=queued_order.order_id)["state"],
            "queued",
        )
        self.assertEqual(self.wallet.prepare_calls, [])
        self.assertEqual(self.wallet.broadcast_calls, [])

    def test_mine_once_requires_reconciled_tip(self) -> None:
        with self.assertRaises(TypeError):
            self.service._mine_once()  # type: ignore[call-arg]

    def test_concurrent_same_tip_deadline_uncertainty_is_idempotent(self) -> None:
        order, transfer = self.broadcast_release(seller_id=57, buyer_id=58)
        self.clock.value = transfer["broadcast_at"] + 31
        barrier = threading.Barrier(2)
        services: list[TradeService] = []
        for _ in range(2):
            explorer = RecoveryExplorer()
            explorer.current_tip = self.explorer.current_tip
            explorer.outputs_by_address = dict(self.explorer.outputs_by_address)
            explorer.set_transaction(transfer["txid"], "unknown")
            explorer.transaction_barrier = barrier
            services.append(
                TradeService(
                    store=self.store,
                    explorer=explorer,
                    wallet=self.wallet,
                    fresh_address=self.wallet.new_address,
                    confirmation_depth=6,
                    clock=self.clock,
                    network_fee_units=10,
                    transfer_reconciliation_deadline_seconds=30,
                )
            )
        with ThreadPoolExecutor(max_workers=2) as pool:
            results = tuple(
                pool.map(lambda service: service.reconcile_transfers(), services)
            )
        self.assertEqual(len(results), 2)
        self.assertTrue(all(result[0].state == "uncertain" for result in results))
        self.assertEqual(
            self.store.get_order_transfer(order_id=order.order_id)["state"],
            "uncertain",
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.mark_transfer_uncertain(
                transfer_id=transfer["transfer_id"],
                expected_state="broadcast",
                expected_txid=transfer["txid"],
                error_text="different uncertainty evidence",
                now=self.clock(),
                expected_tip_hash=self.explorer.current_tip.hash,
                expected_tip_height=self.explorer.current_tip.height,
            )
        with self.assertRaises(AccountingInvariantError):
            self.store.mark_transfer_uncertain(
                transfer_id=transfer["transfer_id"],
                expected_state="broadcast",
                expected_txid=h(123_456),
                error_text="broadcast transaction missed reconciliation deadline",
                now=self.clock(),
                expected_tip_hash=self.explorer.current_tip.hash,
                expected_tip_height=self.explorer.current_tip.height,
            )

    def test_health_rejects_equal_height_different_transaction_tip(self) -> None:
        _order, transfer = self.broadcast_release(seller_id=59, buyer_id=60)
        batch_tip = type(self.explorer.current_tip)(h(902), 100)
        stale_tip = type(self.explorer.current_tip)(h(903), 100)
        explorer = RecoveryExplorer()
        explorer.current_tip = batch_tip
        explorer.outputs_by_address = dict(self.explorer.outputs_by_address)
        explorer.transactions[transfer["txid"]] = TransactionStatus(
            transfer["txid"], "mempool", None, 0, stale_tip
        )
        service = TradeService(
            store=self.store,
            explorer=explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            transfer_reconciliation_deadline_seconds=30,
        )
        with self.assertRaises(AccountingInvariantError):
            service.reconcile_transfers()
        self.assertEqual(
            self.store.get_order_transfer(order_id=transfer["order_id"])["state"],
            "broadcast",
        )
        health = service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("explorer_failure", health.issues)

    def test_mark_broadcast_requires_complete_current_tip_without_mutation(
        self,
    ) -> None:
        _order, transfer = self.prepared_release(seller_id=65, buyer_id=66)
        before = dict(transfer)

        def call(**tip: object) -> object:
            return self.store.mark_transfer_broadcast(
                transfer_id=transfer["transfer_id"],
                observed_txid=transfer["txid"],
                observed_status="mempool",
                now=self.clock(),
                **tip,
            )

        with self.assertRaises(TypeError):
            call()
        self.assertEqual(
            dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])), before
        )
        with self.assertRaises(TypeError):
            call(expected_tip_hash=self.explorer.current_tip.hash)
        for tip in (
            {
                "expected_tip_hash": "invalid",
                "expected_tip_height": self.explorer.current_tip.height,
            },
            {
                "expected_tip_hash": self.explorer.current_tip.hash,
                "expected_tip_height": True,
            },
        ):
            with self.assertRaises(ValueError):
                call(**tip)
            self.assertEqual(
                dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])),
                before,
            )
        old_tip = self.explorer.current_tip
        self.explorer.current_tip = type(old_tip)(h(904), old_tip.height)
        self.service._reconcile_all_deposits()
        with self.assertRaises(AccountingInvariantError):
            call(
                expected_tip_hash=old_tip.hash,
                expected_tip_height=old_tip.height,
            )
        self.assertEqual(
            dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])), before
        )

    def test_broadcast_authorization_requires_complete_current_tip_without_call(
        self,
    ) -> None:
        _order, transfer = self.prepared_release(seller_id=67, buyer_id=68)
        before = dict(transfer)
        invoked: list[int] = []

        def invoke(_row: object) -> tuple[str, str]:
            invoked.append(1)
            return transfer["txid"], "mempool"

        def call(**tip: object) -> object:
            return self.store.broadcast_prepared_with_authorization(
                transfer_id=transfer["transfer_id"],
                expected_txid=transfer["txid"],
                invoke=invoke,
                now=self.clock(),
                **tip,
            )

        with self.assertRaises(TypeError):
            call()
        self.assertEqual(invoked, [])
        with self.assertRaises(TypeError):
            call(expected_tip_height=self.explorer.current_tip.height)
        for tip in (
            {
                "expected_tip_hash": self.explorer.current_tip.hash,
                "expected_tip_height": -1,
            },
            {
                "expected_tip_hash": self.explorer.current_tip.hash,
                "expected_tip_height": False,
            },
        ):
            with self.assertRaises(ValueError):
                call(**tip)
            self.assertEqual(invoked, [])
            self.assertEqual(
                dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])),
                before,
            )
        old_tip = self.explorer.current_tip
        self.explorer.current_tip = type(old_tip)(h(905), old_tip.height)
        self.service._reconcile_all_deposits()
        with self.assertRaises(AccountingInvariantError):
            call(
                expected_tip_hash=old_tip.hash,
                expected_tip_height=old_tip.height,
            )
        self.assertEqual(invoked, [])
        self.assertEqual(
            dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])), before
        )

    def test_mark_uncertain_requires_complete_current_tip_without_mutation(
        self,
    ) -> None:
        _order, transfer = self.broadcast_release(seller_id=69, buyer_id=70)
        before = dict(transfer)

        def call(**tip: object) -> object:
            return self.store.mark_transfer_uncertain(
                transfer_id=transfer["transfer_id"],
                expected_state="broadcast",
                expected_txid=transfer["txid"],
                error_text="bounded uncertainty evidence",
                now=self.clock(),
                **tip,
            )

        with self.assertRaises(TypeError):
            call()
        with self.assertRaises(TypeError):
            call(expected_tip_hash=self.explorer.current_tip.hash)
        for tip in (
            {
                "expected_tip_hash": h(1),
                "expected_tip_height": None,
            },
            {
                "expected_tip_hash": self.explorer.current_tip.hash,
                "expected_tip_height": True,
            },
        ):
            with self.assertRaises(ValueError):
                call(**tip)
            self.assertEqual(
                dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])),
                before,
            )
        old_tip = self.explorer.current_tip
        self.explorer.current_tip = type(old_tip)(h(906), old_tip.height)
        self.service._reconcile_all_deposits()
        with self.assertRaises(AccountingInvariantError):
            call(
                expected_tip_hash=old_tip.hash,
                expected_tip_height=old_tip.height,
            )
        self.assertEqual(
            dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])), before
        )

    def test_mark_confirmed_requires_complete_current_tip_without_mutation(
        self,
    ) -> None:
        _order, transfer = self.broadcast_release(seller_id=71, buyer_id=72)
        before = dict(transfer)

        def call(**tip: object) -> object:
            return self.store.mark_transfer_confirmed(
                transfer_id=transfer["transfer_id"],
                observed_txid=transfer["txid"],
                confirmed_block_hash=h(94_000),
                confirmed_block_height=95,
                confirmations=6,
                now=self.clock(),
                **tip,
            )

        with self.assertRaises(TypeError):
            call()
        with self.assertRaises(TypeError):
            call(expected_tip_height=self.explorer.current_tip.height)
        for tip in (
            {
                "expected_tip_hash": "0" * 63,
                "expected_tip_height": self.explorer.current_tip.height,
            },
            {
                "expected_tip_hash": self.explorer.current_tip.hash,
                "expected_tip_height": False,
            },
        ):
            with self.assertRaises(ValueError):
                call(**tip)
            self.assertEqual(
                dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])),
                before,
            )
        old_tip = self.explorer.current_tip
        self.explorer.current_tip = type(old_tip)(h(907), old_tip.height)
        self.service._reconcile_all_deposits()
        with self.assertRaises(AccountingInvariantError):
            call(
                expected_tip_hash=old_tip.hash,
                expected_tip_height=old_tip.height,
            )
        self.assertEqual(
            dict(self.store.get_transfer(transfer_id=transfer["transfer_id"])), before
        )

    def test_confirmed_reorg_restores_liability_and_reverses_earned_fee(self) -> None:
        fee_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            fee_bps=1_000,
            transfer_reconciliation_deadline_seconds=30,
        )
        order = fee_service.create_sell(
            seller_id=32,
            seller_name="Seller 32",
            receive_address=address(20_032),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.explorer.set_outputs(order.deposit_addr, [120])
        fee_service.check_deposit(order.order_id, actor_id=32)
        fee_service.accept(
            order.order_id,
            actor_id=33,
            actor_name="Buyer 33",
            receive_address=address(30_033),
        )
        fee_service.confirm_sent(order.order_id, actor_id=33)
        fee_service.confirm_received(order.order_id, actor_id=32)
        fee_service.mine()
        transfer = self.store.get_order_transfer(order_id=order.order_id)
        self.explorer.set_transaction(transfer["txid"], "confirmed", confirmations=6)
        fee_service.reconcile_transfers()
        self.assertEqual(self.store.earned_fee_units(), 10)
        self.explorer.set_transaction(transfer["txid"], "unknown")
        fee_service.reconcile_transfers()
        self.assertEqual(
            self.store.get_order_transfer(order_id=order.order_id)["state"], "uncertain"
        )
        self.assertEqual(self.store.earned_fee_units(), 0)
        self.assertEqual(self.store.order_liability_units(order_id=order.order_id), 120)

    def test_late_terminal_credit_preserves_main_state_and_health_fails_adverse_evidence(
        self,
    ) -> None:
        cancelled = self.create_sell(seller_id=34)
        self.service.cancel(cancelled.order_id, actor_id=34)
        self.explorer.set_outputs(cancelled.deposit_addr, [11])
        late = self.service.check_deposit(cancelled.order_id, actor_id=34)
        self.assertEqual(late.state, "cancelled")
        recovery = self.store.get_order_recovery_transfer(order_id=cancelled.order_id)
        self.assertEqual(recovery["kind"], "recovery_refund")
        self.assertEqual(recovery["amount_units"], 1)

        reorged = self.fund(self.create_sell(seller_id=35))
        unknown = self.create_sell(seller_id=36)
        self.explorer.current_tip = type(self.explorer.current_tip)(h(901), 100)
        self.explorer.outputs_by_address[reorged.deposit_addr] = ()
        block = BlockAnchor(h(80_000), 95)
        spend = ConfirmedSpend(h(80_001), 0, BlockAnchor(h(80_002), 100))
        self.explorer.outputs_by_address[unknown.deposit_addr] = (
            ConfirmedOutput(h(80_003), 0, 0, 110, block, 6, False, True, spend),
        )
        self.explorer.current_tip = type(self.explorer.current_tip)(h(902), 100)
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("post_credit_reorg", health.issues)
        self.assertIn("unknown_spend", health.issues)

    def test_fee_withdrawal_cancellation_and_negative_availability_fail_closed(
        self,
    ) -> None:
        with self.assertRaises(ValueError):
            self.service.cancel_fee_withdrawal(999, admin_id=99)

        fee_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            fee_bps=1_000,
            transfer_reconciliation_deadline_seconds=30,
        )
        order = fee_service.create_sell(
            seller_id=39,
            seller_name="Seller 39",
            receive_address=address(20_039),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.explorer.set_outputs(order.deposit_addr, [120])
        fee_service.check_deposit(order.order_id, actor_id=39)
        fee_service.accept(
            order.order_id,
            actor_id=40,
            actor_name="Buyer 40",
            receive_address=address(30_040),
        )
        fee_service.confirm_sent(order.order_id, actor_id=40)
        fee_service.confirm_received(order.order_id, actor_id=39)
        fee_service.mine()
        release = self.store.get_order_transfer(order_id=order.order_id)
        self.store.mark_transfer_confirmed(
            transfer_id=release["transfer_id"],
            observed_txid=release["txid"],
            confirmed_block_hash=h(91_000),
            confirmed_block_height=99,
            confirmations=2,
            now=self.clock(),
            expected_tip_hash=self.explorer.current_tip.hash,
            expected_tip_height=self.explorer.current_tip.height,
        )
        destination = address(40_001)
        withdrawal = self.store.queue_fee_withdrawal(
            operation_key="fee:test:failed",
            amount_units=9,
            network_fee_units=1,
            destination=destination,
            configured_admin_destination=destination,
            now=self.clock(),
            actor_id=99,
        )
        self.assertIsNotNone(withdrawal)
        batch, _ = fee_service._reconcile_all_deposits()
        snapshot = self.wallet.snapshot(batch.tip)
        claim = self.store.claim_next_transfer(
            expected_tip_hash=batch.tip.hash,
            expected_tip_height=batch.tip.height,
            wallet_spendable_units=snapshot.spendable_units,
            now=self.clock(),
        )
        failed = self.store.mark_transfer_failed_safe(
            transfer_id=claim.transfer_id,
            expected_attempt_count=claim.attempt_count,
            expected_reserved_at=claim.reserved_at,
            error_text="safe failure",
            now=self.clock(),
        )
        cancelled = fee_service.cancel_fee_withdrawal(
            failed["transfer_id"], admin_id=99
        )
        self.assertEqual(cancelled.state, "cancelled")
        with self.assertRaises(AccountingInvariantError):
            fee_service.cancel_fee_withdrawal(failed["transfer_id"], admin_id=99)

    def test_confirmed_fee_withdrawal_then_earning_reorg_closes_health(self) -> None:
        fee_service = TradeService(
            store=self.store,
            explorer=self.explorer,
            wallet=self.wallet,
            fresh_address=self.wallet.new_address,
            confirmation_depth=6,
            clock=self.clock,
            network_fee_units=10,
            fee_bps=1_000,
            transfer_reconciliation_deadline_seconds=30,
        )
        order = fee_service.create_sell(
            seller_id=51,
            seller_name="Seller 51",
            receive_address=address(20_051),
            net_amount=100,
            total_price="2",
            asset="AUD",
            method="PayID",
            network=None,
        )
        self.explorer.set_outputs(order.deposit_addr, [120])
        fee_service.check_deposit(order.order_id, actor_id=51)
        fee_service.accept(
            order.order_id,
            actor_id=52,
            actor_name="Buyer 52",
            receive_address=address(30_052),
        )
        fee_service.confirm_sent(order.order_id, actor_id=52)
        fee_service.confirm_received(order.order_id, actor_id=51)
        fee_service.mine()
        release = self.store.get_order_transfer(order_id=order.order_id)
        self.store.mark_transfer_confirmed(
            transfer_id=release["transfer_id"],
            observed_txid=release["txid"],
            confirmed_block_hash=h(92_000),
            confirmed_block_height=99,
            confirmations=2,
            now=self.clock(),
            expected_tip_hash=self.explorer.current_tip.hash,
            expected_tip_height=self.explorer.current_tip.height,
        )
        self.explorer.set_transaction(
            release["txid"],
            "confirmed",
            confirmations=2,
            block_number=92_000,
        )
        destination = address(40_002)
        withdrawal = self.store.queue_fee_withdrawal(
            operation_key="fee:test:confirmed",
            amount_units=9,
            network_fee_units=1,
            destination=destination,
            configured_admin_destination=destination,
            now=self.clock(),
            actor_id=99,
        )
        fee_service.mine()
        withdrawal = self.store.get_transfer(transfer_id=withdrawal["transfer_id"])
        self.store.mark_transfer_confirmed(
            transfer_id=withdrawal["transfer_id"],
            observed_txid=withdrawal["txid"],
            confirmed_block_hash=h(92_001),
            confirmed_block_height=99,
            confirmations=2,
            now=self.clock(),
            expected_tip_hash=self.explorer.current_tip.hash,
            expected_tip_height=self.explorer.current_tip.height,
        )
        self.store.mark_transfer_uncertain(
            transfer_id=release["transfer_id"],
            expected_state="confirmed",
            expected_txid=release["txid"],
            error_text="earning release reorged",
            now=self.clock(),
            expected_tip_hash=self.explorer.current_tip.hash,
            expected_tip_height=self.explorer.current_tip.height,
        )
        health = fee_service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("negative_available_fees", health.issues)

    def test_system_health_fails_db_explorer_solvency_and_uncertainty(self) -> None:
        self.assertTrue(self.service.system_health().accepting_orders)

        original_integrity = self.store.integrity_check
        self.store.integrity_check = lambda: (_ for _ in ()).throw(OSError("db"))  # type: ignore[method-assign]
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("database_failure", health.issues)
        self.store.integrity_check = original_integrity  # type: ignore[method-assign]

        self.explorer.fail_batches = True
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("explorer_failure", health.issues)
        self.explorer.fail_batches = False

        order = self.create_sell(seller_id=37)
        self.explorer.set_outputs(order.deposit_addr, [110])
        self.service.check_deposit(order.order_id, actor_id=37)
        original_snapshot = self.wallet.snapshot
        self.wallet.snapshot = lambda tip: WalletSnapshot(  # type: ignore[method-assign]
            self.store.network, tip, ("wallet-primary",), (), 0, h(999)
        )
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("wallet_insolvent", health.issues)
        self.wallet.snapshot = original_snapshot  # type: ignore[method-assign]

        matched = self.accept_sell(order, buyer_id=38)
        self.service.confirm_sent(matched.order_id, actor_id=38)
        self.service.confirm_received(matched.order_id, actor_id=37)
        self.wallet.broadcast_error = RuntimeError("ambiguous")
        self.service.mine()
        self.wallet.broadcast_error = None
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("uncertain_transfer", health.issues)

    def test_system_health_checks_reconcilable_transaction_endpoint(self) -> None:
        _order, _transfer = self.broadcast_release(seller_id=45, buyer_id=46)
        self.explorer.fail_transactions = True
        health = self.service.system_health()
        self.assertFalse(health.accepting_orders)
        self.assertIn("explorer_failure", health.issues)

    def test_true_barrier_cancellation_matrix_has_exact_outcomes(self) -> None:
        cases = (
            ("wts_awaiting_zero", self._matrix_wts_awaiting_zero),
            ("wts_awaiting_partial", self._matrix_wts_awaiting_partial),
            ("wts_awaiting_dust_topup", self._matrix_wts_awaiting_dust_topup),
            ("wts_open_accept_race", self._matrix_wts_open_accept_race),
            ("wts_matched_buyer_leave", self._matrix_wts_matched_buyer_leave),
            (
                "wts_matched_buyer_leave_confirm_race",
                self._matrix_wts_buyer_leave_confirm_race,
            ),
            (
                "wts_matched_seller_cancel_confirm_race",
                self._matrix_wts_seller_cancel_confirm_race,
            ),
            ("wtb_unaccepted_accept_race", self._matrix_wtb_unaccepted_accept_race),
            ("wtb_accepted_zero", self._matrix_wtb_accepted_zero),
            ("wtb_accepted_partial", self._matrix_wtb_accepted_partial),
            ("wtb_accepted_dust_topup", self._matrix_wtb_accepted_dust_topup),
            (
                "wtb_accepted_full_confirm_race",
                self._matrix_wtb_full_confirm_race,
            ),
            (
                "payment_flag_cancel_timeout_race",
                self._matrix_payment_flag_cancel_timeout_race,
            ),
        )
        for name, run_case in cases:
            with self.subTest(case=name):
                harness = MatrixHarness()
                try:
                    run_case(harness)
                finally:
                    harness.close()

    def _matrix_assert_common(self, harness: MatrixHarness, order_id: int) -> object:
        row = harness.store.get_order(order_id=order_id)
        self.assertIsNotNone(row)
        watched = harness.store.watched_deposit_addresses()
        if row["deposit_addr"] is None:
            self.assertEqual(watched, ())
        else:
            self.assertIn(row["deposit_addr"], watched)
        self.assertEqual(harness.wallet.prepare_calls, [])
        self.assertEqual(harness.wallet.broadcast_calls, [])
        return row

    def _matrix_assert_no_transfer(self, harness: MatrixHarness, order_id: int) -> None:
        self.assertEqual(harness.transfers(order_id), ())

    def _matrix_assert_transfer(
        self,
        harness: MatrixHarness,
        order_id: int,
        *,
        kind: str,
        destination: str,
        amount: int,
        fee: int = 10,
        earned: int = 0,
    ) -> None:
        transfers = harness.transfers(order_id)
        self.assertEqual(len(transfers), 1)
        transfer = transfers[0]
        self.assertEqual(transfer["kind"], kind)
        self.assertEqual(transfer["state"], "queued")
        self.assertEqual(transfer["destination"], destination)
        self.assertEqual(transfer["amount_units"], amount)
        self.assertEqual(transfer["network_fee_units"], fee)
        self.assertEqual(transfer["earned_fee_units"], earned)
        self.assertEqual(
            harness.allocation_units(transfer["transfer_id"]),
            amount + fee + earned,
        )

    def _matrix_assert_accounting(
        self,
        harness: MatrixHarness,
        order_id: int,
        *,
        credited: int,
        main: int,
        recovery: int,
    ) -> None:
        self.assertEqual(
            harness.store.deposit_accounting(order_id=order_id),
            {
                "credited_units": credited,
                "main_units": main,
                "recovery_units": recovery,
            },
        )
        self.assertEqual(
            harness.store.order_liability_units(order_id=order_id), credited
        )

    def _matrix_sell_matched(
        self, harness: MatrixHarness, *, seller_id: int, buyer_id: int
    ) -> object:
        order = harness.create_sell(seller_id)
        funded = harness.credit(order, [110])
        return harness.accept_sell(funded, buyer_id)

    def _matrix_buy_accepted(
        self, harness: MatrixHarness, *, buyer_id: int, seller_id: int
    ) -> object:
        return harness.accept_buy(harness.create_buy(buyer_id), seller_id)

    def _matrix_wts_awaiting_zero(self, harness: MatrixHarness) -> None:
        seller_id = 101
        order = harness.create_sell(seller_id)
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        self.assertEqual([item[0] for item in outcomes], ["ok", "ok"])
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "cancelled")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (None, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=0, main=0, recovery=0
        )
        self._matrix_assert_no_transfer(harness, order.order_id)

    def _matrix_wts_awaiting_partial(self, harness: MatrixHarness) -> None:
        seller_id = 102
        order = harness.create_sell(seller_id)
        harness.credit(order, [50])
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        self.assertEqual([item[0] for item in outcomes], ["ok", "ok"])
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "refund_reserved")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (None, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=50, main=0, recovery=50
        )
        self._matrix_assert_transfer(
            harness,
            order.order_id,
            kind="recovery_refund",
            destination=address(20_000 + seller_id),
            amount=40,
        )

    def _matrix_wts_awaiting_dust_topup(self, harness: MatrixHarness) -> None:
        seller_id = 103
        order = harness.create_sell(seller_id)
        harness.credit(order, [10])
        harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        held = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(held["state"], "recovery_hold")
        self.assertEqual((held["buyer_id"], held["seller_id"]), (None, seller_id))
        self.assertEqual(held["deposit_addr"], order.deposit_addr)
        self.assertEqual((held["buyer_confirmed"], held["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=10, main=0, recovery=10
        )
        self._matrix_assert_no_transfer(harness, order.order_id)

        harness.explorer.current_tip = type(harness.explorer.current_tip)(
            h(95_103), 100
        )
        harness.explorer.set_outputs(order.deposit_addr, [10, 1])
        harness.race(
            lambda: harness.service.check_deposit(order.order_id, actor_id=seller_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "refund_reserved")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (None, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=11, main=0, recovery=11
        )
        self._matrix_assert_transfer(
            harness,
            order.order_id,
            kind="recovery_refund",
            destination=address(20_000 + seller_id),
            amount=1,
        )

    def _matrix_wts_open_accept_race(self, harness: MatrixHarness) -> None:
        seller_id, buyer_id = 104, 204
        order = harness.credit(harness.create_sell(seller_id), [110])
        harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            lambda: harness.accept_sell(order, buyer_id),
        )
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "refund_reserved")
        self.assertIn(row["buyer_id"], (None, buyer_id))
        self.assertEqual(row["seller_id"], seller_id)
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=110, main=110, recovery=0
        )
        self._matrix_assert_transfer(
            harness,
            order.order_id,
            kind="refund",
            destination=address(20_000 + seller_id),
            amount=100,
        )

    def _matrix_wts_matched_buyer_leave(self, harness: MatrixHarness) -> None:
        seller_id, buyer_id = 105, 205
        order = self._matrix_sell_matched(
            harness, seller_id=seller_id, buyer_id=buyer_id
        )
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
        )
        self.assertEqual(sorted(item[0] for item in outcomes), ["expected_error", "ok"])
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "open")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (None, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=110, main=110, recovery=0
        )
        self._matrix_assert_no_transfer(harness, order.order_id)

    def _matrix_wts_buyer_leave_confirm_race(self, harness: MatrixHarness) -> None:
        seller_id, buyer_id = 106, 206
        order = self._matrix_sell_matched(
            harness, seller_id=seller_id, buyer_id=buyer_id
        )
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
            lambda: harness.service.confirm_sent(order.order_id, actor_id=buyer_id),
            lambda: harness.service.confirm_received(
                order.order_id, actor_id=seller_id
            ),
        )
        self.assertTrue(all(item[0] in {"ok", "expected_error"} for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertIn(row["state"], ("open", "release_reserved"))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self._matrix_assert_accounting(
            harness, order.order_id, credited=110, main=110, recovery=0
        )
        if row["state"] == "open":
            self.assertEqual((row["buyer_id"], row["seller_id"]), (None, seller_id))
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
            self._matrix_assert_no_transfer(harness, order.order_id)
        else:
            self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (1, 1))
            self._matrix_assert_transfer(
                harness,
                order.order_id,
                kind="release",
                destination=address(30_000 + buyer_id),
                amount=100,
            )

    def _matrix_wts_seller_cancel_confirm_race(self, harness: MatrixHarness) -> None:
        seller_id, buyer_id = 107, 207
        order = self._matrix_sell_matched(
            harness, seller_id=seller_id, buyer_id=buyer_id
        )
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            lambda: harness.service.confirm_sent(order.order_id, actor_id=buyer_id),
            lambda: harness.service.confirm_received(
                order.order_id, actor_id=seller_id
            ),
        )
        self.assertTrue(all(item[0] in {"ok", "expected_error"} for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertIn(row["state"], ("refund_reserved", "release_reserved"))
        self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self._matrix_assert_accounting(
            harness, order.order_id, credited=110, main=110, recovery=0
        )
        if row["state"] == "refund_reserved":
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
            self._matrix_assert_transfer(
                harness,
                order.order_id,
                kind="refund",
                destination=address(20_000 + seller_id),
                amount=100,
            )
        else:
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (1, 1))
            self._matrix_assert_transfer(
                harness,
                order.order_id,
                kind="release",
                destination=address(30_000 + buyer_id),
                amount=100,
            )

    def _matrix_wtb_unaccepted_accept_race(self, harness: MatrixHarness) -> None:
        buyer_id, seller_id = 208, 108
        order = harness.create_buy(buyer_id)
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
            lambda: harness.accept_buy(order, seller_id),
        )
        self.assertTrue(all(item[0] == "ok" for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "cancelled")
        self.assertNotEqual(row["state"], "open")
        self.assertEqual(row["buyer_id"], buyer_id)
        self.assertIn(row["seller_id"], (None, seller_id))
        self.assertEqual(row["deposit_addr"] is None, row["seller_id"] is None)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=0, main=0, recovery=0
        )
        self._matrix_assert_no_transfer(harness, order.order_id)

    def _matrix_wtb_accepted_zero(self, harness: MatrixHarness) -> None:
        buyer_id, seller_id = 209, 109
        order = self._matrix_buy_accepted(
            harness, buyer_id=buyer_id, seller_id=seller_id
        )
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        self.assertTrue(all(item[0] == "ok" for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "cancelled")
        self.assertNotEqual(row["state"], "open")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=0, main=0, recovery=0
        )
        self._matrix_assert_no_transfer(harness, order.order_id)

    def _matrix_wtb_accepted_partial(self, harness: MatrixHarness) -> None:
        buyer_id, seller_id = 210, 110
        order = self._matrix_buy_accepted(
            harness, buyer_id=buyer_id, seller_id=seller_id
        )
        harness.credit(order, [50])
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        self.assertTrue(all(item[0] == "ok" for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "refund_reserved")
        self.assertNotEqual(row["state"], "open")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=50, main=0, recovery=50
        )
        self._matrix_assert_transfer(
            harness,
            order.order_id,
            kind="recovery_refund",
            destination=address(20_000 + seller_id),
            amount=40,
        )

    def _matrix_wtb_accepted_dust_topup(self, harness: MatrixHarness) -> None:
        buyer_id, seller_id = 211, 111
        order = self._matrix_buy_accepted(
            harness, buyer_id=buyer_id, seller_id=seller_id
        )
        harness.credit(order, [10])
        harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
        )
        held = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(held["state"], "recovery_hold")
        self.assertNotEqual(held["state"], "open")
        self.assertEqual((held["buyer_id"], held["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(held["deposit_addr"], order.deposit_addr)
        self.assertEqual((held["buyer_confirmed"], held["seller_confirmed"]), (0, 0))
        self._matrix_assert_accounting(
            harness, order.order_id, credited=10, main=0, recovery=10
        )
        self._matrix_assert_no_transfer(harness, order.order_id)

        harness.explorer.current_tip = type(harness.explorer.current_tip)(
            h(96_111), 100
        )
        harness.explorer.set_outputs(order.deposit_addr, [10, 1])
        harness.race(
            lambda: harness.service.check_deposit(order.order_id, actor_id=seller_id),
            lambda: harness.service.cancel(order.order_id, actor_id=buyer_id),
        )
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertEqual(row["state"], "refund_reserved")
        self.assertNotEqual(row["state"], "open")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self._matrix_assert_accounting(
            harness, order.order_id, credited=11, main=0, recovery=11
        )
        self._matrix_assert_transfer(
            harness,
            order.order_id,
            kind="recovery_refund",
            destination=address(20_000 + seller_id),
            amount=1,
        )

    def _matrix_wtb_full_confirm_race(self, harness: MatrixHarness) -> None:
        buyer_id, seller_id = 212, 112
        order = self._matrix_buy_accepted(
            harness, buyer_id=buyer_id, seller_id=seller_id
        )
        order = harness.credit(order, [110])
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            lambda: harness.service.confirm_sent(order.order_id, actor_id=buyer_id),
            lambda: harness.service.confirm_received(
                order.order_id, actor_id=seller_id
            ),
        )
        self.assertTrue(all(item[0] in {"ok", "expected_error"} for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertIn(row["state"], ("refund_reserved", "release_reserved"))
        self.assertNotEqual(row["state"], "open")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self._matrix_assert_accounting(
            harness, order.order_id, credited=110, main=110, recovery=0
        )
        if row["state"] == "refund_reserved":
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (0, 0))
            self._matrix_assert_transfer(
                harness,
                order.order_id,
                kind="refund",
                destination=address(20_000 + seller_id),
                amount=100,
            )
        else:
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (1, 1))
            self._matrix_assert_transfer(
                harness,
                order.order_id,
                kind="release",
                destination=address(30_000 + buyer_id),
                amount=100,
            )

    def _matrix_payment_flag_cancel_timeout_race(self, harness: MatrixHarness) -> None:
        seller_id, buyer_id = 113, 213
        order = self._matrix_sell_matched(
            harness, seller_id=seller_id, buyer_id=buyer_id
        )
        harness.service.confirm_sent(order.order_id, actor_id=buyer_id)
        with closing(harness.store.connect()) as conn:
            conn.execute(
                "UPDATE orders SET trade_deadline=? WHERE order_id=?",
                (harness.clock.value - 1, order.order_id),
            )
        outcomes = harness.race(
            lambda: harness.service.cancel(order.order_id, actor_id=seller_id),
            harness.service.expire_orders,
            lambda: harness.service.confirm_received(
                order.order_id, actor_id=seller_id
            ),
        )
        self.assertTrue(all(item[0] in {"ok", "expected_error"} for item in outcomes))
        row = self._matrix_assert_common(harness, order.order_id)
        self.assertIn(row["state"], ("disputed", "release_reserved"))
        self.assertNotEqual(row["state"], "refund_reserved")
        self.assertEqual((row["buyer_id"], row["seller_id"]), (buyer_id, seller_id))
        self.assertEqual(row["deposit_addr"], order.deposit_addr)
        self._matrix_assert_accounting(
            harness, order.order_id, credited=110, main=110, recovery=0
        )
        if row["state"] == "disputed":
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (1, 0))
            self._matrix_assert_no_transfer(harness, order.order_id)
        else:
            self.assertEqual((row["buyer_confirmed"], row["seller_confirmed"]), (1, 1))
            self._matrix_assert_transfer(
                harness,
                order.order_id,
                kind="release",
                destination=address(30_000 + buyer_id),
                amount=100,
            )


if __name__ == "__main__":
    unittest.main()
