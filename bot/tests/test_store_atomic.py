from __future__ import annotations

import hashlib
import random
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from pathlib import Path

from bot.otc.domain import MAX_09C_UNITS, OrderSide, OrderState
from bot.otc.store import AccountingInvariantError, Store


class StoreAtomicTests(unittest.TestCase):
    NETWORK = "btc09-mainnet"

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "otc.db"
        self.store = Store(self.path)
        self.store.initialize()

    def tearDown(self) -> None:
        self.tmp.cleanup()

    @staticmethod
    def h(number: int) -> str:
        return f"{number:064x}"

    def create_sell(
        self,
        *,
        maker_id: int = 7,
        address: str = "deposit-7",
        state: OrderState = OrderState.AWAITING_DEPOSIT,
        now: int = 100,
    ) -> int:
        order_id = self.store.create_order(
            side=OrderSide.SELL,
            maker_id=maker_id,
            maker_name=f"Seller {maker_id}",
            net_amount_units=100,
            network_fee_units=10,
            service_fee_units=7,
            deposit_required_units=117,
            total_price="250.00",
            settlement_asset="AUD",
            settlement_network=None,
            payment_method="Pay ID",
            state=state,
            deposit_addr=address,
            created_at=now,
            updated_at=now,
        )
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE users SET wallet_addr=? WHERE user_id=?",
                (f"seller-wallet-{maker_id}", maker_id),
            )
        return order_id

    def add_user(self, user_id: int, *, wallet: str | None = None) -> None:
        with closing(self.store.connect()) as conn:
            conn.execute(
                """
                INSERT INTO users(user_id, username, wallet_addr, created_at, updated_at)
                VALUES(?, ?, ?, 100, 100)
                ON CONFLICT(user_id) DO UPDATE SET wallet_addr=excluded.wallet_addr
                """,
                (user_id, f"User {user_id}", wallet),
            )

    def output(
        self,
        number: int,
        amount: int,
        *,
        confirmations: int = 6,
        coinbase: bool = False,
        mature: bool = True,
        spent_by: dict[str, object] | None = None,
        block_height: int = 5,
    ) -> dict[str, object]:
        return {
            "txid": self.h(10_000 + number),
            "vout": number % 3,
            "amount_units": amount,
            "block_hash": self.h(20_000 + number),
            "block_height": block_height,
            "confirmations": confirmations,
            "coinbase": coinbase,
            "mature": mature,
            "spent_by": spent_by,
        }

    def snapshot(
        self,
        address: str,
        outputs: list[dict[str, object]],
        *,
        tip_number: int,
        tip_height: int = 10,
        network: str | None = None,
        complete: bool = True,
    ) -> dict[str, object]:
        return {
            "network": network or self.NETWORK,
            "address": address,
            "complete": complete,
            "tip_hash": self.h(tip_number),
            "tip_height": tip_height,
            "outputs": outputs,
        }

    def reconcile(
        self,
        snapshots: list[dict[str, object]],
        *,
        tip_number: int,
        tip_height: int = 10,
        depth: int = 6,
        now: int = 200,
    ):
        return self.store.reconcile_all_deposit_outputs(
            network=self.NETWORK,
            expected_tip_hash=self.h(tip_number),
            expected_tip_height=tip_height,
            snapshots=snapshots,
            final_tip_hash=self.h(tip_number),
            final_tip_height=tip_height,
            credit_depth=depth,
            now=now,
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
                (self.NETWORK, self.NETWORK),
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

    def fund(self, order_id: int, *, amount: int = 117, number: int = 1) -> None:
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.reconcile(
            [
                self.snapshot(
                    order["deposit_addr"], [self.output(number, amount)], tip_number=1
                )
            ],
            tip_number=1,
        )

    def match_sell(self, order_id: int, buyer_id: int = 8, *, now: int = 210) -> None:
        self.add_user(buyer_id, wallet=f"buyer-wallet-{buyer_id}")
        winner = self.store.reserve_accept(
            order_id=order_id,
            actor_id=buyer_id,
            actor_name=f"User {buyer_id}",
            preallocated_deposit_addr=None,
            deposit_deadline=None,
            now=now,
        )
        self.assertIsNotNone(winner)

    def complete_release_queue(self, order_id: int, *, now: int = 220):
        self.store.record_confirmation(order_id=order_id, actor_id=8, now=now)
        self.store.record_confirmation(order_id=order_id, actor_id=7, now=now + 1)
        with closing(self.store.connect()) as conn:
            return dict(
                conn.execute(
                    "SELECT * FROM transfers WHERE order_id=? AND kind='release'",
                    (order_id,),
                ).fetchone()
            )

    def test_partial_credits_replay_and_outpoint_identity_are_exact(self):
        order_id = self.create_sell()
        first = self.output(1, 50)
        second = self.output(2, 67)
        result = self.reconcile(
            [self.snapshot("deposit-7", [second, first], tip_number=1)],
            tip_number=1,
        )

        self.assertTrue(result.healthy)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        with closing(self.store.connect()) as conn:
            rows = conn.execute(
                "SELECT amount_units,main_units,recovery_units FROM deposit_credits "
                "WHERE order_id=? ORDER BY credit_id",
                (order_id,),
            ).fetchall()
            self.assertEqual([tuple(row) for row in rows], [(50, 50, 0), (67, 67, 0)])
            scan_count = conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[
                0
            ]

        replay = self.reconcile(
            [self.snapshot("deposit-7", [first, second], tip_number=1)],
            tip_number=1,
            now=201,
        )
        self.assertTrue(replay.healthy)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_credits").fetchone()[0], 2
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[0],
                scan_count,
            )

        other_id = self.create_sell(maker_id=9, address="deposit-9", now=202)
        with self.assertRaises(AccountingInvariantError):
            self.reconcile(
                [
                    self.snapshot("deposit-7", [first, second], tip_number=2),
                    self.snapshot("deposit-9", [first], tip_number=2),
                ],
                tip_number=2,
                now=203,
            )
        self.assertEqual(self.store.order_liability_units(order_id=other_id), 0)

    def test_append_only_scans_are_latest_tip_aba_and_same_tip_is_semantic_noop(self):
        self.create_sell()
        output = self.output(1, 117)
        for tip, now in ((11, 200), (12, 201), (11, 202)):
            self.reconcile(
                [self.snapshot("deposit-7", [output], tip_number=tip)],
                tip_number=tip,
                now=now,
            )
        with closing(self.store.connect()) as conn:
            scans = conn.execute(
                "SELECT scan_id,tip_hash FROM deposit_scans ORDER BY scan_id"
            ).fetchall()
        self.assertEqual(
            [row[1] for row in scans], [self.h(11), self.h(12), self.h(11)]
        )
        self.assertEqual([row[0] for row in scans], sorted(row[0] for row in scans))

        contradictory = dict(output)
        contradictory["confirmations"] = 7
        with self.assertRaises(AccountingInvariantError):
            self.reconcile(
                [self.snapshot("deposit-7", [contradictory], tip_number=11)],
                tip_number=11,
                now=203,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[0], 3
            )

    def test_provisional_coinbase_and_precredit_disappearance_never_create_liability(
        self,
    ):
        order_id = self.create_sell()
        provisional = self.output(1, 200, confirmations=5, block_height=126)
        immature = self.output(
            2, 300, confirmations=99, coinbase=True, mature=False, block_height=32
        )
        result = self.reconcile(
            [
                self.snapshot(
                    "deposit-7", [provisional, immature], tip_number=1, tip_height=130
                )
            ],
            tip_number=1,
            tip_height=130,
        )
        self.assertTrue(result.healthy)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.assertEqual(self.store.provisional_restricted_units(), 200)
        self.assertEqual(
            result.restricted_outpoints,
            ((provisional["txid"], provisional["vout"], 200),),
        )

        self.reconcile(
            [self.snapshot("deposit-7", [], tip_number=2, tip_height=131)],
            tip_number=2,
            tip_height=131,
            now=201,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.assertEqual(self.store.provisional_restricted_units(), 0)

        mature = dict(immature)
        mature["mature"] = True
        mature["confirmations"] = 101
        self.reconcile(
            [self.snapshot("deposit-7", [mature], tip_number=3, tip_height=132)],
            tip_number=3,
            tip_height=132,
            now=202,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 300)

    def test_postcredit_reorg_and_unknown_spend_retain_liability_and_fail_health(self):
        order_id = self.create_sell()
        credited = self.output(1, 117)
        self.reconcile(
            [self.snapshot("deposit-7", [credited], tip_number=1)], tip_number=1
        )

        result = self.reconcile(
            [self.snapshot("deposit-7", [], tip_number=2)],
            tip_number=2,
            now=201,
        )
        self.assertFalse(result.healthy)
        self.assertIn(order_id, result.recovery_order_ids)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)

        spent = dict(credited)
        spent["confirmations"] = 6
        spent["spent_by"] = {
            "txid": self.h(90_001),
            "vin": 0,
            "block_hash": self.h(90_002),
            "block_height": 8,
        }
        result = self.reconcile(
            [self.snapshot("deposit-7", [spent], tip_number=3)],
            tip_number=3,
            now=202,
        )
        self.assertFalse(result.healthy)
        self.assertIn("unknown_spend", result.health_issues)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)

    def test_first_seen_unknown_spend_commits_credit_without_funding_progress(self):
        order_id = self.create_sell()
        spent = self.output(
            1,
            117,
            spent_by={
                "txid": self.h(91_001),
                "vin": 0,
                "block_hash": self.h(91_002),
                "block_height": 8,
            },
        )
        result = self.reconcile(
            [self.snapshot("deposit-7", [spent], tip_number=1)], tip_number=1
        )
        self.assertFalse(result.healthy)
        self.assertIn("unknown_spend", result.health_issues)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "awaiting_deposit")
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_credits").fetchone()[0], 1
            )

    def test_all_address_barrier_rejects_incomplete_mixed_and_racing_sets_atomically(
        self,
    ):
        self.create_sell()
        self.create_sell(maker_id=9, address="deposit-9")
        snapshots = [
            self.snapshot("deposit-7", [self.output(1, 117)], tip_number=1),
            self.snapshot("deposit-9", [self.output(2, 117)], tip_number=1),
        ]
        self.reconcile(snapshots, tip_number=1)

        bad_cases = [
            snapshots[:1],
            snapshots + [self.snapshot("extra", [], tip_number=1)],
            [snapshots[0], self.snapshot("deposit-9", [], tip_number=2)],
        ]
        for bad in bad_cases:
            with self.subTest(bad=len(bad)):
                with self.assertRaises(AccountingInvariantError):
                    self.reconcile(bad, tip_number=1, now=201)
        with self.assertRaises(AccountingInvariantError):
            self.store.reconcile_all_deposit_outputs(
                network=self.NETWORK,
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                snapshots=snapshots,
                final_tip_hash=self.h(99),
                final_tip_height=10,
                credit_depth=6,
                now=201,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[0], 2
            )

    def test_store_network_is_configured_and_cross_network_evidence_fails_health(self):
        order_id = self.create_sell()
        wrong_snapshot = self.snapshot(
            "deposit-7", [], tip_number=1, network="btc09-regtest"
        )
        with self.assertRaises(ValueError):
            self.store.reconcile_all_deposit_outputs(
                network="btc09-regtest",
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                snapshots=[wrong_snapshot],
                final_tip_hash=self.h(1),
                final_tip_height=10,
                credit_depth=6,
                now=200,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[0], 0
            )
            scan_id = conn.execute(
                """
                INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at)
                VALUES('btc09-regtest','deposit-7',?,10,199)
                """,
                (self.h(99),),
            ).lastrowid
            conn.execute(
                """
                INSERT INTO deposit_credits(
                  order_id,network,txid,vout,deposit_addr,amount_units,block_hash,
                  block_height,confirmations,coinbase,mature,current_best_chain,
                  first_seen_at,last_seen_at,last_seen_scan_id,last_checked_scan_id
                ) VALUES(?,'btc09-regtest',?,0,'deposit-7',1,?,5,6,0,1,1,
                         199,199,?,?)
                """,
                (order_id, self.h(98), self.h(97), scan_id, scan_id),
            )
        result = self.reconcile(
            [self.snapshot("deposit-7", [], tip_number=1)], tip_number=1
        )
        self.assertFalse(result.healthy)
        self.assertIn("cross_network_evidence", result.health_issues)
        with self.assertRaises(ValueError):
            Store(self.path, network="testnet")

    def test_snapshot_boundary_rejects_inflated_depth_and_early_coinbase_maturity(self):
        self.create_sell()
        inflated = self.output(1, 117, confirmations=6, block_height=6)
        with self.assertRaisesRegex(AccountingInvariantError, "confirmations"):
            self.reconcile(
                [self.snapshot("deposit-7", [inflated], tip_number=1)], tip_number=1
            )
        early = self.output(
            2,
            117,
            confirmations=99,
            coinbase=True,
            mature=True,
            block_height=32,
        )
        with self.assertRaisesRegex(AccountingInvariantError, "maturity"):
            self.reconcile(
                [self.snapshot("deposit-7", [early], tip_number=2, tip_height=130)],
                tip_number=2,
                tip_height=130,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[0], 0
            )

    def test_customer_payout_to_same_or_cross_escrow_address_is_atomic_error(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        with closing(self.store.connect()) as conn:
            conn.execute("UPDATE users SET wallet_addr='deposit-7' WHERE user_id=8")
            before = (
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0],
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
            )
        self.store.record_confirmation(order_id=order_id, actor_id=7, now=220)
        with self.assertRaisesRegex(AccountingInvariantError, "escrow deposit"):
            self.store.record_confirmation(order_id=order_id, actor_id=8, now=221)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], before[0]
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
                before[1] + 1,
            )
            order = conn.execute(
                "SELECT * FROM orders WHERE order_id=?", (order_id,)
            ).fetchone()
            self.assertEqual((order["buyer_confirmed"], order["state"]), (0, "matched"))

        other_id = self.create_sell(maker_id=9, address="deposit-9", now=222)
        self.assertGreater(other_id, order_id)
        with closing(self.store.connect()) as conn:
            conn.execute("UPDATE users SET wallet_addr='deposit-9' WHERE user_id=8")
            before_transfer = conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[
                0
            ]
            before_audit = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[
                0
            ]
        with self.assertRaisesRegex(AccountingInvariantError, "escrow deposit"):
            self.store.record_confirmation(order_id=order_id, actor_id=8, now=223)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0],
                before_transfer,
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
                before_audit,
            )

    @staticmethod
    def sha256d_hex(raw_hex: str) -> str:
        raw = bytes.fromhex(raw_hex)
        return hashlib.sha256(hashlib.sha256(raw).digest()).digest().hex()

    def claim_release(self, order_id: int, *, now: int = 300):
        transfer = self.complete_release_queue(order_id)
        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=self.store.customer_liability_units(),
            now=now,
        )
        self.assertIsNotNone(claim)
        return transfer, claim

    def attach_claim(self, claim, *, raw_hex: str = "01020304", now: int = 301):
        return self.store.attach_signed_transfer(
            transfer_id=claim.transfer_id,
            expected_attempt_count=claim.attempt_count,
            expected_reserved_at=claim.reserved_at,
            txid=self.sha256d_hex(raw_hex),
            signed_tx_hex=raw_hex,
            destination=claim.destination,
            amount_units=claim.amount_units,
            network_fee_units=claim.network_fee_units,
            prepared_tip_hash=claim.expected_tip_hash,
            prepared_tip_height=claim.expected_tip_height,
            live_tip_hash=claim.expected_tip_hash,
            live_tip_height=claim.expected_tip_height,
            wallet_snapshot_hash=self.h(800),
            expected_wallet_snapshot_hash=self.h(800),
            now=now,
        )

    def confirm_claim(self, claim, *, raw_hex: str = "01020304", now: int = 303):
        self.attach_claim(claim, raw_hex=raw_hex, now=now - 2)
        txid = self.sha256d_hex(raw_hex)
        self.mark_transfer_broadcast(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            observed_status="mempool",
            now=now - 1,
        )
        return self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(801),
            confirmed_block_height=11,
            confirmations=1,
            now=now,
        )

    def test_liability_discharges_only_on_confirmation_and_reorg_restores_fee(self):
        order_id = self.create_sell()
        self.fund(order_id, amount=117)
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.attach_claim(claim)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        txid = self.sha256d_hex("01020304")
        self.mark_transfer_broadcast(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            observed_status="mempool",
            now=302,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.mark_transfer_uncertain(
            transfer_id=claim.transfer_id,
            expected_state="broadcast",
            expected_txid=txid,
            error_text="observation timeout",
            now=303,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(801),
            confirmed_block_height=11,
            confirmations=1,
            now=304,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.assertEqual(self.store.earned_fee_units(), 7)

        self.mark_transfer_uncertain(
            transfer_id=claim.transfer_id,
            expected_state="confirmed",
            expected_txid=txid,
            error_text="outgoing reorg",
            now=305,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.assertEqual(self.store.earned_fee_units(), 0)
        self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(802),
            confirmed_block_height=12,
            confirmations=1,
            now=306,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.assertEqual(self.store.earned_fee_units(), 7)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 1
            )

    def test_excess_release_then_recovery_confirmation_discharges_exact_137(self):
        order_id = self.create_sell()
        self.fund(order_id, amount=137)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 137)
        self.match_sell(order_id)
        _, release_claim = self.claim_release(order_id)
        self.confirm_claim(release_claim, now=303)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 20)
        recovery = self.store.queue_order_transfer(
            order_id=order_id, kind="recovery_refund", now=304
        )
        self.assertIsNotNone(recovery)
        self.assertEqual(
            (recovery["amount_units"], recovery["network_fee_units"]), (10, 10)
        )
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "completed")
        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=20,
            now=305,
        )
        self.assertIsNotNone(claim)
        self.confirm_claim(claim, raw_hex="05060708", now=308)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.assertEqual(self.store.earned_fee_units(), 7)
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "completed")

    def test_terminal_recovery_callbacks_never_overwrite_main_outcome_uncertainty(self):
        order_id = self.create_sell()
        self.fund(order_id, amount=137)
        self.match_sell(order_id)
        _, main_claim = self.claim_release(order_id)
        self.confirm_claim(main_claim, now=303)
        recovery = self.store.queue_order_transfer(
            order_id=order_id, kind="recovery_refund", now=304
        )
        recovery_claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=20,
            now=305,
        )
        main_txid = self.sha256d_hex("01020304")
        self.mark_transfer_uncertain(
            transfer_id=main_claim.transfer_id,
            expected_state="confirmed",
            expected_txid=main_txid,
            error_text="main payout reorg while recovery reserved",
            now=306,
        )
        self.store.mark_transfer_failed_safe(
            transfer_id=recovery_claim.transfer_id,
            expected_attempt_count=recovery_claim.attempt_count,
            expected_reserved_at=recovery_claim.reserved_at,
            error_text="stop unsigned recovery child",
            now=307,
        )
        self.assertEqual(
            self.store.get_order(order_id=order_id)["state"], "transfer_uncertain"
        )
        self.mark_transfer_confirmed(
            transfer_id=main_claim.transfer_id,
            observed_txid=main_txid,
            confirmed_block_hash=self.h(982),
            confirmed_block_height=12,
            confirmations=1,
            now=308,
        )
        self.assertEqual(self.store.get_order(order_id=order_id)["state"], "completed")
        self.store.requeue_failed_safe(transfer_id=recovery_claim.transfer_id, now=309)
        recovery_claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=20,
            now=310,
        )
        self.attach_claim(recovery_claim, raw_hex="d1d2d3d4", now=311)
        recovery_txid = self.sha256d_hex("d1d2d3d4")
        self.mark_transfer_broadcast(
            transfer_id=recovery_claim.transfer_id,
            observed_txid=recovery_txid,
            observed_status="mempool",
            now=312,
        )
        self.mark_transfer_uncertain(
            transfer_id=main_claim.transfer_id,
            expected_state="confirmed",
            expected_txid=main_txid,
            error_text="main payout reorg while recovery broadcast",
            now=313,
        )
        self.assertEqual(
            self.store.get_order(order_id=order_id)["state"], "transfer_uncertain"
        )
        self.mark_transfer_confirmed(
            transfer_id=recovery_claim.transfer_id,
            observed_txid=recovery_txid,
            confirmed_block_hash=self.h(983),
            confirmed_block_height=13,
            confirmations=1,
            now=314,
        )
        self.assertEqual(
            self.store.get_order(order_id=order_id)["state"], "transfer_uncertain"
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.mark_transfer_confirmed(
            transfer_id=main_claim.transfer_id,
            observed_txid=main_txid,
            confirmed_block_hash=self.h(984),
            confirmed_block_height=14,
            confirmations=1,
            now=315,
        )
        self.assertEqual(self.store.get_order(order_id=order_id)["state"], "completed")
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        with closing(self.store.connect()) as conn:
            immutable = conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?",
                (recovery["transfer_id"],),
            ).fetchone()
            self.assertEqual(immutable["operation_key"], recovery["operation_key"])

    def test_partial_dust_hold_topup_queues_one_unit_without_subsidy(self):
        order_id = self.create_sell()
        partial = self.output(1, 10)
        self.reconcile(
            [self.snapshot("deposit-7", [partial], tip_number=1)], tip_number=1
        )
        self.assertIsNone(
            self.store.queue_order_transfer(
                order_id=order_id, kind="recovery_refund", now=201
            )
        )
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "recovery_hold")
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 10)
        with closing(self.store.connect()) as conn:
            credit = conn.execute(
                "SELECT main_units,recovery_units,recovery_reason FROM deposit_credits"
            ).fetchone()
            self.assertEqual(tuple(credit), (0, 10, "cancelled_partial"))
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 0
            )

        partial_at_tip_2 = dict(partial)
        partial_at_tip_2["confirmations"] = 7
        topup = self.output(2, 1, confirmations=7)
        self.reconcile(
            [
                self.snapshot(
                    "deposit-7", [partial_at_tip_2, topup], tip_number=2, tip_height=11
                )
            ],
            tip_number=2,
            tip_height=11,
            now=202,
        )
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "refund_reserved")
        with closing(self.store.connect()) as conn:
            transfer = conn.execute(
                "SELECT * FROM transfers WHERE kind='recovery_refund'"
            ).fetchone()
            self.assertEqual(
                (transfer["amount_units"], transfer["network_fee_units"]), (1, 10)
            )
            self.assertEqual(
                conn.execute(
                    "SELECT COALESCE(SUM(units),0) FROM transfer_credit_allocations "
                    "WHERE transfer_id=?",
                    (transfer["transfer_id"],),
                ).fetchone()[0],
                11,
            )
        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(2),
            expected_tip_height=11,
            wallet_spendable_units=11,
            now=203,
        )
        self.assertIsNotNone(claim)
        self.attach_claim(claim, raw_hex="c1c2c3c4", now=204)
        txid = self.sha256d_hex("c1c2c3c4")
        self.mark_transfer_broadcast(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            observed_status="mempool",
            now=205,
        )
        self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(980),
            confirmed_block_height=12,
            confirmations=1,
            now=206,
        )
        self.assertEqual(self.store.get_order(order_id=order_id)["state"], "refunded")
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.mark_transfer_uncertain(
            transfer_id=claim.transfer_id,
            expected_state="confirmed",
            expected_txid=txid,
            error_text="recovery payout reorg",
            now=207,
        )
        self.assertEqual(
            self.store.get_order(order_id=order_id)["state"], "transfer_uncertain"
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 11)
        self.mark_transfer_broadcast(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            observed_status="mempool",
            now=208,
        )
        self.assertEqual(self.store.get_order(order_id=order_id)["state"], "broadcast")
        self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(981),
            confirmed_block_height=13,
            confirmations=1,
            now=209,
        )
        self.assertEqual(self.store.get_order(order_id=order_id)["state"], "refunded")
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)

    def test_attach_sha256d_metadata_tip_single_identity_and_ambiguous_commit(self):
        self.assertEqual(
            self.sha256d_hex("00"),
            "1406e05881e299367766d313e26c05564ec91bf721d31726bd6e46e60689539a",
        )
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        base = {
            "transfer_id": claim.transfer_id,
            "expected_attempt_count": claim.attempt_count,
            "expected_reserved_at": claim.reserved_at,
            "txid": self.sha256d_hex("01020304"),
            "signed_tx_hex": "01020304",
            "destination": claim.destination,
            "amount_units": claim.amount_units,
            "network_fee_units": claim.network_fee_units,
            "prepared_tip_hash": claim.expected_tip_hash,
            "prepared_tip_height": claim.expected_tip_height,
            "live_tip_hash": claim.expected_tip_hash,
            "live_tip_height": claim.expected_tip_height,
            "wallet_snapshot_hash": self.h(800),
            "expected_wallet_snapshot_hash": self.h(800),
            "now": 301,
        }
        for override in (
            {"txid": self.h(900)},
            {"destination": "another-wallet"},
            {"amount_units": claim.amount_units + 1},
            {"live_tip_hash": self.h(901)},
            {"wallet_snapshot_hash": self.h(902)},
        ):
            with (
                self.subTest(override=override),
                self.assertRaises(AccountingInvariantError),
            ):
                self.store.attach_signed_transfer(**(base | override))
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT state FROM transfers WHERE transfer_id=?",
                    (claim.transfer_id,),
                ).fetchone()[0],
                "reserved",
            )

        def before_commit(_: sqlite3.Connection) -> None:
            raise sqlite3.OperationalError("before commit")

        before_store = Store(self.path, attach_commit_boundary=before_commit)
        before_result = before_store.attach_signed_transfer(**base)
        self.assertEqual(before_result.classification, "retry_reserved")

        def after_commit(conn: sqlite3.Connection) -> None:
            conn.execute("COMMIT")
            raise sqlite3.OperationalError("after commit")

        after_store = Store(self.path, attach_commit_boundary=after_commit)
        after_result = after_store.attach_signed_transfer(**base)
        self.assertEqual(after_result.classification, "prepared")
        with self.assertRaises(AccountingInvariantError):
            self.store.attach_signed_transfer(**base)
        with self.assertRaises(AccountingInvariantError):
            self.store.recover_ambiguous_attach(
                transfer_id=claim.transfer_id,
                expected_attempt_count=claim.attempt_count,
                expected_reserved_at=claim.reserved_at,
                txid=self.h(903),
                signed_tx_hex="01020304",
                prepared_tip_hash=claim.expected_tip_hash,
                prepared_tip_height=claim.expected_tip_height,
            )

    def test_subprocess_termination_around_attach_commit_reopens_as_reserved_or_prepared(
        self,
    ):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        signal = Path(self.tmp.name) / "commit-boundary.signal"
        raw_hex = "91929394"
        txid = self.sha256d_hex(raw_hex)
        script = """
import sqlite3
import sys
import time
from pathlib import Path
from bot.otc.store import Store

db, signal, transfer_id, attempt, reserved_at, txid, raw_hex, destination, amount, fee, tip_hash, tip_height = sys.argv[1:]
def boundary(conn):
    Path(signal).write_text('entered', encoding='ascii')
    time.sleep(0.06)
    conn.execute('COMMIT')
    time.sleep(0.06)
store = Store(db, attach_commit_boundary=boundary)
store.attach_signed_transfer(
    transfer_id=int(transfer_id),
    expected_attempt_count=int(attempt),
    expected_reserved_at=int(reserved_at),
    txid=txid,
    signed_tx_hex=raw_hex,
    destination=destination,
    amount_units=int(amount),
    network_fee_units=int(fee),
    prepared_tip_hash=tip_hash,
    prepared_tip_height=int(tip_height),
    live_tip_hash=tip_hash,
    live_tip_height=int(tip_height),
    wallet_snapshot_hash='0' * 63 + '8',
    expected_wallet_snapshot_hash='0' * 63 + '8',
    now=int(reserved_at) + 1,
)
"""
        process = subprocess.Popen(
            [
                sys.executable,
                "-c",
                script,
                str(self.path),
                str(signal),
                str(claim.transfer_id),
                str(claim.attempt_count),
                str(claim.reserved_at),
                txid,
                raw_hex,
                claim.destination,
                str(claim.amount_units),
                str(claim.network_fee_units),
                claim.expected_tip_hash,
                str(claim.expected_tip_height),
            ],
            cwd=Path(__file__).resolve().parents[2],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        deadline = time.monotonic() + 10
        while (
            not signal.exists()
            and process.poll() is None
            and time.monotonic() < deadline
        ):
            time.sleep(0.005)
        self.assertTrue(signal.exists(), "attach child never reached commit boundary")
        time.sleep(random.uniform(0.005, 0.105))
        process.kill()
        process.wait(timeout=10)
        result = self.store.recover_ambiguous_attach(
            transfer_id=claim.transfer_id,
            expected_attempt_count=claim.attempt_count,
            expected_reserved_at=claim.reserved_at,
            txid=txid,
            signed_tx_hex=raw_hex,
            prepared_tip_hash=claim.expected_tip_hash,
            prepared_tip_height=claim.expected_tip_height,
        )
        self.assertIn(result.classification, ("retry_reserved", "prepared"))
        row = result.transfer
        if result.classification == "retry_reserved":
            self.assertEqual(row["state"], "reserved")
            self.assertIsNone(row["signed_tx_hex"])
        else:
            self.assertEqual(
                (row["state"], row["txid"], row["signed_tx_hex"]),
                ("prepared", txid, raw_hex),
            )

    def test_stale_reservation_token_cannot_attach_fail_or_recover_later_attempt(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        _, first = self.claim_release(order_id)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.store.mark_transfer_failed_safe(
            transfer_id=first.transfer_id,
            expected_attempt_count=first.attempt_count,
            expected_reserved_at=first.reserved_at,
            error_text="prepare exited",
            now=301,
        )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 117)
        self.store.requeue_failed_safe(transfer_id=first.transfer_id, now=302)
        second = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=117,
            now=303,
        )
        self.assertIsNotNone(second)
        self.assertEqual(second.attempt_count, first.attempt_count + 1)
        with self.assertRaisesRegex(AccountingInvariantError, "stale"):
            self.store.recover_ambiguous_attach(
                transfer_id=first.transfer_id,
                expected_attempt_count=first.attempt_count,
                expected_reserved_at=first.reserved_at,
                txid=self.sha256d_hex("01020304"),
                signed_tx_hex="01020304",
                prepared_tip_hash=self.h(1),
                prepared_tip_height=10,
            )
        with self.assertRaises(AccountingInvariantError):
            self.store.mark_transfer_failed_safe(
                transfer_id=first.transfer_id,
                expected_attempt_count=first.attempt_count,
                expected_reserved_at=first.reserved_at,
                error_text="late callback",
                now=304,
            )
        self.attach_claim(second, now=305)

    def test_ambiguous_reopen_failure_sets_process_health_halt(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        store = Store(self.path)
        real_connect = store.connect

        def failed_connect():
            raise sqlite3.OperationalError("reopen denied")

        store.connect = failed_connect
        with self.assertRaisesRegex(AccountingInvariantError, "reopen failed"):
            store.recover_ambiguous_attach(
                transfer_id=claim.transfer_id,
                expected_attempt_count=claim.attempt_count,
                expected_reserved_at=claim.reserved_at,
                txid=self.sha256d_hex("01020304"),
                signed_tx_hex="01020304",
                prepared_tip_hash=claim.expected_tip_hash,
                prepared_tip_height=claim.expected_tip_height,
            )
        store.connect = real_connect
        health_result = store.reconcile_all_deposit_outputs(
            network=self.NETWORK,
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            snapshots=[self.snapshot("deposit-7", [self.output(1, 117)], tip_number=1)],
            final_tip_hash=self.h(1),
            final_tip_height=10,
            credit_depth=6,
            now=301,
        )
        self.assertFalse(health_result.healthy)
        self.assertIn("process_health_failure", health_result.health_issues)
        self.store.mark_transfer_failed_safe(
            transfer_id=claim.transfer_id,
            expected_attempt_count=claim.attempt_count,
            expected_reserved_at=claim.reserved_at,
            error_text="clear global lane for halt check",
            now=302,
        )
        self.store.requeue_failed_safe(transfer_id=claim.transfer_id, now=303)
        self.assertIsNone(
            store.claim_next_transfer(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=117,
                now=304,
            )
        )

    def test_stale_prepared_timeout_cannot_demote_a_confirmed_transfer(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        self.confirm_claim(claim, now=303)
        txid = self.sha256d_hex("01020304")
        with self.assertRaisesRegex(AccountingInvariantError, "stale"):
            self.mark_transfer_uncertain(
                transfer_id=claim.transfer_id,
                expected_state="prepared",
                expected_txid=txid,
                error_text="old prepare timeout",
                now=304,
            )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 0)
        self.assertEqual(self.store.earned_fee_units(), 7)

    def test_uncertain_transfer_blocks_new_intake_but_allows_evidence_and_repair(self):
        order_id = self.create_sell()
        original = self.output(1, 117)
        self.reconcile(
            [self.snapshot("deposit-7", [original], tip_number=1)], tip_number=1
        )
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        self.attach_claim(claim, raw_hex="b1b2b3b4", now=301)
        recovery_order = self.create_sell(
            maker_id=9, address="uncertain-recovery-deposit", now=301
        )
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE orders SET state='cancelled',updated_at=301 WHERE order_id=?",
                (recovery_order,),
            )
        self.reconcile(
            [
                self.snapshot("deposit-7", [original], tip_number=1),
                self.snapshot("uncertain-recovery-deposit", [], tip_number=1),
            ],
            tip_number=1,
            now=302,
        )
        txid = self.sha256d_hex("b1b2b3b4")
        self.mark_transfer_uncertain(
            transfer_id=claim.transfer_id,
            expected_state="prepared",
            expected_txid=txid,
            error_text="submit outcome unknown",
            now=302,
        )
        with closing(self.store.connect()) as conn:
            before = (
                conn.execute("SELECT COUNT(*) FROM users").fetchone()[0],
                conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0],
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
            )
        with self.assertRaisesRegex(AccountingInvariantError, "uncertain transfer"):
            self.create_sell(maker_id=93, address="blocked-uncertain", now=303)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                (
                    conn.execute("SELECT COUNT(*) FROM users").fetchone()[0],
                    conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0],
                    conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
                ),
                before,
            )
        original_two = dict(original)
        original_two["confirmations"] = 7
        late_recovery = self.output(2, 11, confirmations=7)
        reconcile_result = self.reconcile(
            [
                self.snapshot("deposit-7", [original_two], tip_number=2, tip_height=11),
                self.snapshot(
                    "uncertain-recovery-deposit",
                    [late_recovery],
                    tip_number=2,
                    tip_height=11,
                ),
            ],
            tip_number=2,
            tip_height=11,
            now=304,
        )
        self.assertFalse(reconcile_result.healthy)
        self.assertIn("uncertain_transfer", reconcile_result.health_issues)
        self.assertEqual(self.store.order_liability_units(order_id=recovery_order), 11)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM transfers WHERE kind='recovery_refund'"
                ).fetchone()[0],
                0,
            )
        self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(970),
            confirmed_block_height=12,
            confirmations=1,
            now=305,
        )
        repaired_recovery = self.store.queue_order_transfer(
            order_id=recovery_order, kind="recovery_refund", now=306
        )
        self.assertIsNotNone(repaired_recovery)
        new_id = self.create_sell(maker_id=93, address="allowed-after-repair", now=307)
        self.assertGreater(new_id, order_id)

    def earn_service_fee(self) -> tuple[int, object]:
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        _, claim = self.claim_release(order_id)
        self.confirm_claim(claim, now=303)
        return order_id, claim

    def test_fee_withdrawal_concurrency_idempotency_and_failed_safe_cancellation(self):
        self.earn_service_fee()
        barrier = threading.Barrier(2)

        def queue(key: str):
            barrier.wait()
            return self.store.queue_fee_withdrawal(
                operation_key=key,
                amount_units=5,
                network_fee_units=0,
                destination="admin-wallet",
                configured_admin_destination="admin-wallet",
                actor_id=99,
                now=310,
            )

        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(queue, ("fee:race:a", "fee:race:b")))
        winners = [result for result in results if result is not None]
        self.assertEqual(len(winners), 1)
        winner = winners[0]
        replay = self.store.queue_fee_withdrawal(
            operation_key=winner["operation_key"],
            amount_units=5,
            network_fee_units=0,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            actor_id=99,
            now=311,
        )
        self.assertEqual(replay["transfer_id"], winner["transfer_id"])
        self.assertEqual(self.store.available_fee_units(), 2)

        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=5,
            now=312,
        )
        self.assertIsNotNone(claim)
        self.store.mark_transfer_failed_safe(
            transfer_id=claim.transfer_id,
            expected_attempt_count=claim.attempt_count,
            expected_reserved_at=claim.reserved_at,
            error_text="no eligible input",
            now=313,
        )
        self.assertEqual(self.store.available_fee_units(), 2)
        self.store.cancel_failed_safe_fee_withdrawal(
            transfer_id=claim.transfer_id, actor_id=99, now=314
        )
        self.assertEqual(self.store.available_fee_units(), 7)

    def test_fee_withdrawal_different_keys_and_escrow_destination_guard(self):
        self.earn_service_fee()
        first = self.store.queue_fee_withdrawal(
            operation_key="fee:first",
            amount_units=2,
            network_fee_units=1,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=310,
        )
        second = self.store.queue_fee_withdrawal(
            operation_key="fee:second",
            amount_units=3,
            network_fee_units=1,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=311,
        )
        self.assertIsNotNone(first)
        self.assertIsNotNone(second)
        self.assertEqual(self.store.available_fee_units(), 0)
        with closing(self.store.connect()) as conn:
            before = (
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0],
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
            )
        with self.assertRaisesRegex(AccountingInvariantError, "escrow deposit"):
            self.store.queue_fee_withdrawal(
                operation_key="fee:loop",
                amount_units=1,
                network_fee_units=0,
                destination="deposit-7",
                configured_admin_destination="deposit-7",
                now=312,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], before[0]
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
                before[1],
            )

    def test_zero_percent_pilot_cannot_withdraw_unearned_fee(self):
        order_id = self.store.create_order(
            side=OrderSide.SELL,
            maker_id=70,
            maker_name="Zero Fee Seller",
            net_amount_units=100,
            network_fee_units=10,
            service_fee_units=0,
            deposit_required_units=110,
            total_price="100",
            settlement_asset="AUD",
            settlement_network=None,
            payment_method="Pay ID",
            state=OrderState.AWAITING_DEPOSIT,
            deposit_addr="zero-fee-deposit",
            created_at=100,
            updated_at=100,
        )
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE users SET wallet_addr='zero-fee-seller-wallet' WHERE user_id=70"
            )
        zero_credit = self.output(70, 110)
        self.reconcile(
            [self.snapshot("zero-fee-deposit", [zero_credit], tip_number=1)],
            tip_number=1,
        )
        self.add_user(71, wallet="zero-fee-buyer-wallet")
        self.store.reserve_accept(
            order_id=order_id,
            actor_id=71,
            actor_name="User 71",
            preallocated_deposit_addr=None,
            deposit_deadline=None,
            now=210,
        )
        self.store.record_confirmation(order_id=order_id, actor_id=70, now=220)
        self.store.record_confirmation(order_id=order_id, actor_id=71, now=221)
        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=110,
            now=300,
        )
        self.assertIsNotNone(claim)
        self.confirm_claim(claim, raw_hex="41424344", now=303)
        self.assertEqual(self.store.earned_fee_units(), 0)
        self.assertIsNone(
            self.store.queue_fee_withdrawal(
                operation_key="fee:zero",
                amount_units=1,
                network_fee_units=0,
                destination="admin-wallet",
                configured_admin_destination="admin-wallet",
                now=304,
            )
        )

    def test_confirmed_fee_replay_and_earning_reorg_negative_health_then_repair(self):
        order_id, earning_claim = self.earn_service_fee()
        fee = self.store.queue_fee_withdrawal(
            operation_key="fee:confirmed-replay",
            amount_units=6,
            network_fee_units=1,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=310,
        )
        self.assertIsNotNone(fee)
        fee_claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=7,
            now=311,
        )
        self.assertIsNotNone(fee_claim)
        self.confirm_claim(fee_claim, raw_hex="51525354", now=314)
        replay = self.store.queue_fee_withdrawal(
            operation_key="fee:confirmed-replay",
            amount_units=6,
            network_fee_units=1,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=315,
        )
        self.assertEqual(replay["transfer_id"], fee["transfer_id"])
        self.assertEqual(self.store.available_fee_units(), 0)

        new_buy = self.create_buy(maker_id=90, now=315)
        earning_txid = self.sha256d_hex("01020304")
        self.mark_transfer_uncertain(
            transfer_id=earning_claim.transfer_id,
            expected_state="confirmed",
            expected_txid=earning_txid,
            error_text="earning transaction reorg",
            now=316,
        )
        with self.assertRaisesRegex(AccountingInvariantError, "negative"):
            self.store.available_fee_units()
        with self.assertRaisesRegex(
            AccountingInvariantError, "uncertain transfer|negative_available_fees"
        ):
            self.store.reserve_accept(
                order_id=new_buy,
                actor_id=91,
                actor_name="Seller 91",
                preallocated_deposit_addr="unused-91",
                deposit_deadline=500,
                now=317,
            )
        self.assertIsNone(
            self.store.claim_next_transfer(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=0,
                now=317,
            )
        )
        with closing(self.store.connect()) as conn:
            intake_before = (
                conn.execute("SELECT COUNT(*) FROM users").fetchone()[0],
                conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0],
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
            )
        with self.assertRaisesRegex(
            AccountingInvariantError, "uncertain transfer|negative_available_fees"
        ):
            self.create_sell(maker_id=92, address="blocked-intake", now=317)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                (
                    conn.execute("SELECT COUNT(*) FROM users").fetchone()[0],
                    conn.execute("SELECT COUNT(*) FROM orders").fetchone()[0],
                    conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
                ),
                intake_before,
            )
        self.mark_transfer_confirmed(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            confirmed_block_hash=self.h(952),
            confirmed_block_height=12,
            confirmations=1,
            now=318,
        )
        self.assertEqual(self.store.earned_fee_units(), 7)
        self.assertEqual(self.store.available_fee_units(), 0)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM transfers WHERE order_id=? AND is_main_outcome=1",
                    (order_id,),
                ).fetchone()[0],
                1,
            )

    def test_postclaim_uncertainty_recovery_for_reserved_operation(self):
        _, earning_claim = self.earn_service_fee()
        fee = self.store.queue_fee_withdrawal(
            operation_key="fee:reserved-race",
            amount_units=5,
            network_fee_units=0,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=310,
        )
        self.assertIsNotNone(fee)
        reserved = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=5,
            now=311,
        )
        self.assertIsNotNone(reserved)
        earning_txid = self.sha256d_hex("01020304")
        self.mark_transfer_uncertain(
            transfer_id=earning_claim.transfer_id,
            expected_state="confirmed",
            expected_txid=earning_txid,
            error_text="earning reorg while fee reserved",
            now=312,
        )
        with self.assertRaises(AccountingInvariantError):
            self.attach_claim(reserved, raw_hex="61626364", now=313)
        self.store.mark_transfer_failed_safe(
            transfer_id=reserved.transfer_id,
            expected_attempt_count=reserved.attempt_count,
            expected_reserved_at=reserved.reserved_at,
            error_text="prepare child was terminated",
            now=314,
        )
        self.mark_transfer_broadcast(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            observed_status="mempool",
            now=315,
        )
        self.mark_transfer_confirmed(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            confirmed_block_hash=self.h(960),
            confirmed_block_height=12,
            confirmations=1,
            now=316,
        )
        self.store.requeue_failed_safe(transfer_id=reserved.transfer_id, now=317)

    def test_prepared_operation_becomes_uncertain_and_two_uncertain_rows_resolve_one_at_time(
        self,
    ):
        _, earning_claim = self.earn_service_fee()
        fee = self.store.queue_fee_withdrawal(
            operation_key="fee:prepared-race",
            amount_units=5,
            network_fee_units=0,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=310,
        )
        self.assertIsNotNone(fee)
        fee_claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=5,
            now=311,
        )
        self.assertIsNotNone(fee_claim)
        self.attach_claim(fee_claim, raw_hex="71727374", now=312)
        earning_txid = self.sha256d_hex("01020304")
        fee_txid = self.sha256d_hex("71727374")
        self.mark_transfer_uncertain(
            transfer_id=earning_claim.transfer_id,
            expected_state="confirmed",
            expected_txid=earning_txid,
            error_text="earning reorg while fee prepared",
            now=313,
        )
        with self.assertRaises(AccountingInvariantError):
            self.mark_transfer_broadcast(
                transfer_id=fee_claim.transfer_id,
                observed_txid=fee_txid,
                observed_status="mempool",
                now=314,
            )
        self.mark_transfer_uncertain(
            transfer_id=fee_claim.transfer_id,
            expected_state="prepared",
            expected_txid=fee_txid,
            error_text="blocked by global uncertainty",
            now=315,
        )
        for transfer_id, txid in (
            (earning_claim.transfer_id, earning_txid),
            (fee_claim.transfer_id, fee_txid),
        ):
            with self.assertRaisesRegex(AccountingInvariantError, "another uncertain"):
                self.mark_transfer_broadcast(
                    transfer_id=transfer_id,
                    observed_txid=txid,
                    observed_status="mempool",
                    now=316,
                )
        self.mark_transfer_confirmed(
            transfer_id=fee_claim.transfer_id,
            observed_txid=fee_txid,
            confirmed_block_hash=self.h(961),
            confirmed_block_height=12,
            confirmations=1,
            now=317,
        )
        self.mark_transfer_broadcast(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            observed_status="mempool",
            now=318,
        )
        self.mark_transfer_confirmed(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            confirmed_block_hash=self.h(962),
            confirmed_block_height=13,
            confirmations=1,
            now=319,
        )

    def test_broadcast_operation_is_reconciled_not_rebuilt_during_other_reorg(self):
        _, earning_claim = self.earn_service_fee()
        self.store.queue_fee_withdrawal(
            operation_key="fee:broadcast-race",
            amount_units=5,
            network_fee_units=0,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=310,
        )
        fee_claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=5,
            now=311,
        )
        self.attach_claim(fee_claim, raw_hex="a1a2a3a4", now=312)
        fee_txid = self.sha256d_hex("a1a2a3a4")
        self.mark_transfer_broadcast(
            transfer_id=fee_claim.transfer_id,
            observed_txid=fee_txid,
            observed_status="mempool",
            now=313,
        )
        earning_txid = self.sha256d_hex("01020304")
        self.mark_transfer_uncertain(
            transfer_id=earning_claim.transfer_id,
            expected_state="confirmed",
            expected_txid=earning_txid,
            error_text="earning reorg while fee broadcast",
            now=314,
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.mark_transfer_failed_safe(
                transfer_id=fee_claim.transfer_id,
                expected_attempt_count=fee_claim.attempt_count,
                expected_reserved_at=fee_claim.reserved_at,
                error_text="stale prepare callback",
                now=315,
            )
        with self.assertRaises(AccountingInvariantError):
            self.store.requeue_failed_safe(transfer_id=fee_claim.transfer_id, now=315)
        with self.assertRaisesRegex(AccountingInvariantError, "wallet lane"):
            self.mark_transfer_broadcast(
                transfer_id=earning_claim.transfer_id,
                observed_txid=earning_txid,
                observed_status="mempool",
                now=315,
            )
        self.mark_transfer_confirmed(
            transfer_id=fee_claim.transfer_id,
            observed_txid=fee_txid,
            confirmed_block_hash=self.h(965),
            confirmed_block_height=12,
            confirmations=1,
            now=316,
        )
        self.mark_transfer_broadcast(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            observed_status="mempool",
            now=317,
        )
        self.mark_transfer_confirmed(
            transfer_id=earning_claim.transfer_id,
            observed_txid=earning_txid,
            confirmed_block_hash=self.h(966),
            confirmed_block_height=13,
            confirmations=1,
            now=318,
        )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM transfers WHERE operation_key='fee:broadcast-race'"
                ).fetchone()[0],
                1,
            )

    def test_fee_cancellation_is_forbidden_from_every_non_failed_safe_state(self):
        _, earning_claim = self.earn_service_fee()
        fee = self.store.queue_fee_withdrawal(
            operation_key="fee:no-cancel",
            amount_units=6,
            network_fee_units=1,
            destination="admin-wallet",
            configured_admin_destination="admin-wallet",
            now=310,
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=fee["transfer_id"], actor_id=99, now=311
            )
        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=7,
            now=312,
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=claim.transfer_id, actor_id=99, now=313
            )
        self.attach_claim(claim, raw_hex="81828384", now=314)
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=claim.transfer_id, actor_id=99, now=315
            )
        txid = self.sha256d_hex("81828384")
        self.mark_transfer_broadcast(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            observed_status="mempool",
            now=316,
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=claim.transfer_id, actor_id=99, now=317
            )
        self.mark_transfer_uncertain(
            transfer_id=claim.transfer_id,
            expected_state="broadcast",
            expected_txid=txid,
            error_text="observation loss",
            now=318,
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=claim.transfer_id, actor_id=99, now=319
            )
        self.mark_transfer_confirmed(
            transfer_id=claim.transfer_id,
            observed_txid=txid,
            confirmed_block_hash=self.h(963),
            confirmed_block_height=12,
            confirmations=1,
            now=320,
        )
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=claim.transfer_id, actor_id=99, now=321
            )
        with self.assertRaises(AccountingInvariantError):
            self.store.cancel_failed_safe_fee_withdrawal(
                transfer_id=earning_claim.transfer_id, actor_id=99, now=321
            )

    def test_preterminal_residual_one_enters_hold_without_zero_payout(self):
        order_id = self.create_sell()
        self.reconcile(
            [self.snapshot("deposit-7", [self.output(1, 1)], tip_number=1)],
            tip_number=1,
        )
        self.assertIsNone(
            self.store.queue_order_transfer(
                order_id=order_id, kind="recovery_refund", now=201
            )
        )
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "recovery_hold")
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 1)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 0
            )

    def test_every_terminal_state_preserves_main_state_through_dust_and_topup(self):
        saved_store = self.store
        try:
            for terminal in ("completed", "refunded", "cancelled", "deposit_expired"):
                with (
                    self.subTest(terminal=terminal),
                    tempfile.TemporaryDirectory() as directory,
                ):
                    self.store = Store(Path(directory) / "terminal.db")
                    self.store.initialize()
                    order_id = self.create_sell()
                    original: dict[str, object] | None = None
                    if terminal in ("completed", "refunded"):
                        original = self.output(1, 117)
                        self.reconcile(
                            [self.snapshot("deposit-7", [original], tip_number=1)],
                            tip_number=1,
                        )
                        if terminal == "completed":
                            self.match_sell(order_id)
                            _, claim = self.claim_release(order_id)
                        else:
                            transfer = self.store.queue_order_transfer(
                                order_id=order_id, kind="refund", now=220
                            )
                            self.assertIsNotNone(transfer)
                            claim = self.store.claim_next_transfer(
                                expected_tip_hash=self.h(1),
                                expected_tip_height=10,
                                wallet_spendable_units=117,
                                now=221,
                            )
                            self.assertIsNotNone(claim)
                        self.confirm_claim(claim, raw_hex="11121314", now=303)
                    else:
                        with closing(self.store.connect()) as conn:
                            conn.execute(
                                "UPDATE orders SET state=?,updated_at=200 WHERE order_id=?",
                                (terminal, order_id),
                            )

                    late = self.output(2, 10, confirmations=7)
                    tip_two_outputs = [late]
                    if original is not None:
                        original_two = dict(original)
                        original_two["confirmations"] = 7
                        tip_two_outputs.insert(0, original_two)
                    self.reconcile(
                        [
                            self.snapshot(
                                "deposit-7",
                                tip_two_outputs,
                                tip_number=2,
                                tip_height=11,
                            )
                        ],
                        tip_number=2,
                        tip_height=11,
                        now=330,
                    )
                    order = self.store.get_order(order_id=order_id)
                    assert order is not None
                    self.assertEqual(order["state"], terminal)
                    self.assertEqual(
                        self.store.order_liability_units(order_id=order_id), 10
                    )
                    with closing(self.store.connect()) as conn:
                        self.assertEqual(
                            conn.execute(
                                "SELECT COUNT(*) FROM transfers WHERE kind='recovery_refund'"
                            ).fetchone()[0],
                            0,
                        )

                    tip_three_outputs = []
                    for prior in tip_two_outputs:
                        updated = dict(prior)
                        updated["confirmations"] = 8
                        tip_three_outputs.append(updated)
                    tip_three_outputs.append(self.output(3, 1, confirmations=8))
                    self.reconcile(
                        [
                            self.snapshot(
                                "deposit-7",
                                tip_three_outputs,
                                tip_number=3,
                                tip_height=12,
                            )
                        ],
                        tip_number=3,
                        tip_height=12,
                        now=331,
                    )
                    order = self.store.get_order(order_id=order_id)
                    assert order is not None
                    self.assertEqual(order["state"], terminal)
                    with closing(self.store.connect()) as conn:
                        recovery = conn.execute(
                            "SELECT * FROM transfers WHERE kind='recovery_refund'"
                        ).fetchone()
                        self.assertIsNotNone(recovery)
                        self.assertEqual(
                            (recovery["amount_units"], recovery["network_fee_units"]),
                            (1, 10),
                        )
                    recovery_claim = self.store.claim_next_transfer(
                        expected_tip_hash=self.h(3),
                        expected_tip_height=12,
                        wallet_spendable_units=11,
                        now=340,
                    )
                    self.assertIsNotNone(recovery_claim)
                    self.attach_claim(recovery_claim, raw_hex="31323334", now=341)
                    self.assertEqual(
                        self.store.get_order(order_id=order_id)["state"], terminal
                    )
                    recovery_txid = self.sha256d_hex("31323334")
                    self.mark_transfer_broadcast(
                        transfer_id=recovery_claim.transfer_id,
                        observed_txid=recovery_txid,
                        observed_status="mempool",
                        now=342,
                    )
                    self.assertEqual(
                        self.store.get_order(order_id=order_id)["state"], terminal
                    )
                    self.mark_transfer_uncertain(
                        transfer_id=recovery_claim.transfer_id,
                        expected_state="broadcast",
                        expected_txid=recovery_txid,
                        error_text="temporary observation loss",
                        now=343,
                    )
                    self.assertEqual(
                        self.store.get_order(order_id=order_id)["state"], terminal
                    )
                    self.mark_transfer_confirmed(
                        transfer_id=recovery_claim.transfer_id,
                        observed_txid=recovery_txid,
                        confirmed_block_hash=self.h(950),
                        confirmed_block_height=13,
                        confirmations=1,
                        now=344,
                    )
                    self.assertEqual(
                        self.store.get_order(order_id=order_id)["state"], terminal
                    )
        finally:
            self.store = saved_store

    def test_unsafe_auto_recovery_destination_commits_new_liability_before_reporting(
        self,
    ):
        order_id = self.create_sell()
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE orders SET state='cancelled',updated_at=150 WHERE order_id=?",
                (order_id,),
            )
            conn.execute("UPDATE users SET wallet_addr='deposit-7' WHERE user_id=7")
        result = self.reconcile(
            [self.snapshot("deposit-7", [self.output(1, 11)], tip_number=1)],
            tip_number=1,
        )
        self.assertFalse(result.healthy)
        self.assertIn("unsafe_recovery_destination", result.health_issues)
        self.assertIn(order_id, result.recovery_order_ids)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 11)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_scans").fetchone()[0], 1
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM deposit_credits").fetchone()[0], 1
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 0
            )
        with self.assertRaisesRegex(
            AccountingInvariantError, "unsafe_recovery_destination"
        ):
            self.store.queue_order_transfer(
                order_id=order_id, kind="recovery_refund", now=201
            )

    def test_new_recovery_credit_never_mutates_inflight_economics(self):
        order_id = self.create_sell()
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE orders SET state='cancelled',updated_at=150 WHERE order_id=?",
                (order_id,),
            )
        first_credit = self.output(1, 11)
        self.reconcile(
            [self.snapshot("deposit-7", [first_credit], tip_number=1)], tip_number=1
        )
        with closing(self.store.connect()) as conn:
            first_transfer = dict(
                conn.execute(
                    "SELECT * FROM transfers WHERE kind='recovery_refund'"
                ).fetchone()
            )
        first_at_two = dict(first_credit)
        first_at_two["confirmations"] = 7
        second_credit = self.output(2, 20, confirmations=7)
        self.reconcile(
            [
                self.snapshot(
                    "deposit-7",
                    [first_at_two, second_credit],
                    tip_number=2,
                    tip_height=11,
                )
            ],
            tip_number=2,
            tip_height=11,
            now=201,
        )
        with closing(self.store.connect()) as conn:
            unchanged = conn.execute(
                "SELECT * FROM transfers WHERE transfer_id=?",
                (first_transfer["transfer_id"],),
            ).fetchone()
            self.assertEqual(
                (unchanged["amount_units"], unchanged["network_fee_units"]), (1, 10)
            )
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM transfers WHERE kind='recovery_refund'"
                ).fetchone()[0],
                1,
            )
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 31)
        claim = self.store.claim_next_transfer(
            expected_tip_hash=self.h(2),
            expected_tip_height=11,
            wallet_spendable_units=31,
            now=202,
        )
        self.assertIsNotNone(claim)
        self.confirm_claim(claim, raw_hex="21222324", now=205)
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 20)
        later = self.store.queue_order_transfer(
            order_id=order_id, kind="recovery_refund", now=206
        )
        self.assertIsNotNone(later)
        self.assertNotEqual(later["operation_key"], first_transfer["operation_key"])
        self.assertEqual((later["amount_units"], later["network_fee_units"]), (10, 10))

    def create_buy(self, *, maker_id: int = 17, now: int = 100) -> int:
        order_id = self.store.create_order(
            side=OrderSide.BUY,
            maker_id=maker_id,
            maker_name=f"Buyer {maker_id}",
            net_amount_units=100,
            network_fee_units=10,
            service_fee_units=7,
            deposit_required_units=117,
            total_price="250.00",
            settlement_asset="USDT",
            settlement_network="TRON",
            payment_method="Wallet transfer",
            state=OrderState.OPEN,
            deposit_addr=None,
            created_at=now,
            updated_at=now,
        )
        with closing(self.store.connect()) as conn:
            conn.execute(
                "UPDATE users SET wallet_addr=? WHERE user_id=?",
                (f"buyer-wallet-{maker_id}", maker_id),
            )
        return order_id

    def test_wts_accept_has_one_winner_and_losers_do_not_audit(self):
        order_id = self.create_sell()
        self.fund(order_id)
        for actor in range(20, 40):
            self.add_user(actor, wallet=f"buyer-wallet-{actor}")
        barrier = threading.Barrier(20)

        def accept(actor: int):
            barrier.wait()
            return self.store.reserve_accept(
                order_id=order_id,
                actor_id=actor,
                actor_name=f"User {actor}",
                preallocated_deposit_addr=None,
                deposit_deadline=None,
                now=300 + actor,
            )

        with ThreadPoolExecutor(max_workers=20) as pool:
            results = list(pool.map(accept, range(20, 40)))
        winners = [result for result in results if result is not None]
        self.assertEqual(len(winners), 1)
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "matched")
        self.assertEqual(order["buyer_id"], winners[0]["buyer_id"])
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM audit_events WHERE event_type='order_accepted'"
                ).fetchone()[0],
                1,
            )

    def test_wtb_accept_has_one_seller_and_only_winner_address_is_stored(self):
        order_id = self.create_buy()
        for actor in range(40, 60):
            self.add_user(actor, wallet=f"seller-wallet-{actor}")
        barrier = threading.Barrier(20)

        def accept(actor: int):
            barrier.wait()
            return self.store.reserve_accept(
                order_id=order_id,
                actor_id=actor,
                actor_name=f"User {actor}",
                preallocated_deposit_addr=f"fresh-deposit-{actor}",
                deposit_deadline=1_000 + actor,
                now=400 + actor,
            )

        with ThreadPoolExecutor(max_workers=20) as pool:
            results = list(pool.map(accept, range(40, 60)))
        winners = [result for result in results if result is not None]
        self.assertEqual(len(winners), 1)
        winner = winners[0]
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(order["state"], "awaiting_deposit")
        self.assertEqual(order["seller_id"], winner["seller_id"])
        self.assertEqual(order["deposit_addr"], f"fresh-deposit-{winner['seller_id']}")
        self.assertEqual(order["deposit_deadline"], 1_000 + winner["seller_id"])

    def test_wrong_role_self_accept_and_malformed_accept_are_zero_side_effects(self):
        order_id = self.create_buy()
        with closing(self.store.connect()) as conn:
            before = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]
        self.assertIsNone(
            self.store.reserve_accept(
                order_id=order_id,
                actor_id=17,
                actor_name="Buyer 17",
                preallocated_deposit_addr="unused",
                deposit_deadline=500,
                now=200,
            )
        )
        with self.assertRaises(ValueError):
            self.store.reserve_accept(
                order_id=order_id,
                actor_id=18,
                actor_name="Seller",
                preallocated_deposit_addr=None,
                deposit_deadline=500,
                now=201,
            )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0], before
            )

    def test_public_audit_mutation_is_serialized_and_rolls_back_commit_failure(self):
        barrier = threading.Barrier(20)

        def append(number: int) -> int:
            barrier.wait()
            return self.store.append_audit(
                event_type=f"public_audit:{number}",
                created_at=200 + number,
                actor_id=number + 1,
                detail={"number": number},
            )

        with ThreadPoolExecutor(max_workers=20) as pool:
            event_ids = list(pool.map(append, range(20)))
        self.assertEqual(len(set(event_ids)), 20)

        real_connect = self.store.connect

        class CommitFailConnection:
            def __init__(inner_self, conn):
                inner_self.conn = conn

            @property
            def in_transaction(inner_self):
                return inner_self.conn.in_transaction

            def execute(inner_self, sql, parameters=()):
                if sql == "COMMIT":
                    raise sqlite3.OperationalError("injected commit failure")
                return inner_self.conn.execute(sql, parameters)

            def close(inner_self):
                inner_self.conn.close()

        self.store.connect = lambda: CommitFailConnection(real_connect())
        try:
            with self.assertRaisesRegex(sqlite3.OperationalError, "commit failure"):
                self.store.append_audit(
                    event_type="public_audit:rollback",
                    created_at=300,
                    detail={"must": "rollback"},
                )
        finally:
            self.store.connect = real_connect
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT COUNT(*) FROM audit_events WHERE event_type='public_audit:rollback'"
                ).fetchone()[0],
                0,
            )

    def test_concurrent_second_confirmation_queues_one_release_atomically(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        first = self.store.record_confirmation(order_id=order_id, actor_id=8, now=220)
        self.assertIsNotNone(first)
        barrier = threading.Barrier(20)

        def confirm(_: int):
            barrier.wait()
            return self.store.record_confirmation(
                order_id=order_id, actor_id=7, now=221
            )

        with ThreadPoolExecutor(max_workers=20) as pool:
            list(pool.map(confirm, range(20)))
        order = self.store.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual((order["buyer_confirmed"], order["seller_confirmed"]), (1, 1))
        self.assertEqual(order["state"], "release_reserved")
        with closing(self.store.connect()) as conn:
            transfers = conn.execute(
                "SELECT * FROM transfers WHERE operation_key=?",
                (f"order:{order_id}:main",),
            ).fetchall()
            self.assertEqual(len(transfers), 1)
            self.assertEqual(transfers[0]["state"], "queued")
            allocated = conn.execute(
                "SELECT COALESCE(SUM(units),0) FROM transfer_credit_allocations "
                "WHERE transfer_id=?",
                (transfers[0]["transfer_id"],),
            ).fetchone()[0]
            self.assertEqual(allocated, 117)

    def test_confirmation_role_authorization_and_first_flag_idempotency(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        with closing(self.store.connect()) as conn:
            before = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]
        self.assertIsNone(
            self.store.record_confirmation(order_id=order_id, actor_id=999, now=220)
        )
        first = self.store.record_confirmation(order_id=order_id, actor_id=8, now=221)
        replay = self.store.record_confirmation(order_id=order_id, actor_id=8, now=222)
        self.assertEqual(first["buyer_confirmed"], 1)
        self.assertEqual(replay["buyer_confirmed"], 1)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 0
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0],
                before + 1,
            )

    def test_main_outcome_replay_returns_exact_row_before_state_revalidation(self):
        order_id = self.create_sell()
        self.fund(order_id)
        first = self.store.queue_order_transfer(
            order_id=order_id, kind="refund", actor_id=99, now=220
        )
        self.assertIsNotNone(first)
        with closing(self.store.connect()) as conn:
            audits = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]
        replay = self.store.queue_order_transfer(
            order_id=order_id, kind="refund", actor_id=99, now=221
        )
        self.assertEqual(replay, first)
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM transfers").fetchone()[0], 1
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0], audits
            )
        with self.assertRaisesRegex(AccountingInvariantError, "different kind"):
            self.store.queue_order_transfer(
                order_id=order_id, kind="resolve_buyer", actor_id=99, now=222
            )

    def test_global_claim_race_has_one_winner_and_returns_barrier_and_attempt_token(
        self,
    ):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        transfer = self.complete_release_queue(order_id)
        barrier = threading.Barrier(20)

        def claim(_: int):
            barrier.wait()
            return self.store.claim_next_transfer(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=117,
                now=300,
            )

        with ThreadPoolExecutor(max_workers=20) as pool:
            results = list(pool.map(claim, range(20)))
        winners = [result for result in results if result is not None]
        self.assertEqual(len(winners), 1)
        winner = winners[0]
        self.assertEqual(winner.transfer_id, transfer["transfer_id"])
        self.assertEqual((winner.attempt_count, winner.reserved_at), (1, 300))
        self.assertEqual(
            (winner.expected_tip_hash, winner.expected_tip_height), (self.h(1), 10)
        )
        self.assertEqual(winner.restricted_outpoints, ())

    def test_claim_wrong_tip_and_stale_latest_scan_return_none_without_audit(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        transfer = self.complete_release_queue(order_id)
        with closing(self.store.connect()) as conn:
            audits = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]
        self.assertIsNone(
            self.store.claim_next_transfer(
                expected_tip_hash=self.h(2),
                expected_tip_height=10,
                wallet_spendable_units=117,
                now=300,
            )
        )
        for invalid in (True, -1, MAX_09C_UNITS + 1):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                self.store.claim_next_transfer(
                    expected_tip_hash=self.h(1),
                    expected_tip_height=10,
                    wallet_spendable_units=invalid,
                    now=300,
                )
        with closing(self.store.connect()) as conn:
            conn.execute(
                """
                INSERT INTO deposit_scans(network,address,tip_hash,tip_height,observed_at)
                VALUES('btc09-mainnet','deposit-7',?,11,301)
                """,
                (self.h(2),),
            )
        self.assertIsNone(
            self.store.claim_next_transfer(
                expected_tip_hash=self.h(2),
                expected_tip_height=11,
                wallet_spendable_units=117,
                now=302,
            )
        )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0], audits
            )
            self.assertEqual(
                conn.execute(
                    "SELECT state FROM transfers WHERE transfer_id=?",
                    (transfer["transfer_id"],),
                ).fetchone()[0],
                "queued",
            )

    def test_claim_missing_latest_watched_address_returns_none_without_audit(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        transfer = self.complete_release_queue(order_id)
        self.create_sell(maker_id=9, address="unscanned-deposit", now=250)
        with closing(self.store.connect()) as conn:
            audits = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]
        self.assertIsNone(
            self.store.claim_next_transfer(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=117,
                now=300,
            )
        )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0], audits
            )
            self.assertEqual(
                conn.execute(
                    "SELECT state FROM transfers WHERE transfer_id=?",
                    (transfer["transfer_id"],),
                ).fetchone()[0],
                "queued",
            )

    def test_second_confirmation_commit_is_restart_safe_without_both_flag_gap(self):
        order_id = self.create_sell()
        self.fund(order_id)
        self.match_sell(order_id)
        self.store.record_confirmation(order_id=order_id, actor_id=8, now=220)
        self.store.record_confirmation(order_id=order_id, actor_id=7, now=221)
        reopened = Store(self.path)
        order = reopened.get_order(order_id=order_id)
        assert order is not None
        self.assertEqual(
            (order["buyer_confirmed"], order["seller_confirmed"], order["state"]),
            (1, 1, "release_reserved"),
        )
        with closing(reopened.connect()) as conn:
            rows = conn.execute(
                "SELECT * FROM transfers WHERE operation_key=?",
                (f"order:{order_id}:main",),
            ).fetchall()
            self.assertEqual(len(rows), 1)
            self.assertEqual(rows[0]["state"], "queued")

    def test_claim_deficit_masked_by_provisional_funds_leaves_queue_and_audit_unchanged(
        self,
    ):
        order_id = self.create_sell()
        credited = self.output(1, 117)
        provisional = self.output(2, 10, confirmations=5, block_height=6)
        self.reconcile(
            [self.snapshot("deposit-7", [credited, provisional], tip_number=1)],
            tip_number=1,
        )
        self.match_sell(order_id)
        transfer = self.complete_release_queue(order_id)
        with closing(self.store.connect()) as conn:
            audits = conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0]
        self.assertIsNone(
            self.store.claim_next_transfer(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=126,
                now=300,
            )
        )
        with closing(self.store.connect()) as conn:
            self.assertEqual(
                conn.execute(
                    "SELECT state FROM transfers WHERE transfer_id=?",
                    (transfer["transfer_id"],),
                ).fetchone()[0],
                "queued",
            )
            self.assertEqual(
                conn.execute("SELECT COUNT(*) FROM audit_events").fetchone()[0], audits
            )

    def test_wallet_solvency_reads_one_wal_snapshot_during_concurrent_reconcile(self):
        order_id = self.create_sell()
        original = self.output(1, 117)
        self.reconcile(
            [self.snapshot("deposit-7", [original], tip_number=1)], tip_number=1
        )
        reader_ready = threading.Event()
        writer_done = threading.Event()

        class RacingStore(Store):
            def _solvency_checkpoint(inner_self, phase: str) -> None:
                if phase == "after_health":
                    reader_ready.set()
                    if not writer_done.wait(10):
                        raise AssertionError("writer did not finish")

        racing_store = RacingStore(self.path)
        original_two = dict(original)
        original_two["confirmations"] = 7
        extra = self.output(2, 20, confirmations=7)

        def writer() -> None:
            if not reader_ready.wait(10):
                raise AssertionError("reader did not establish snapshot")
            try:
                self.store.reconcile_all_deposit_outputs(
                    network=self.NETWORK,
                    expected_tip_hash=self.h(2),
                    expected_tip_height=11,
                    snapshots=[
                        self.snapshot(
                            "deposit-7",
                            [original_two, extra],
                            tip_number=2,
                            tip_height=11,
                        )
                    ],
                    final_tip_hash=self.h(2),
                    final_tip_height=11,
                    credit_depth=6,
                    now=201,
                )
            finally:
                writer_done.set()

        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(writer)
            snapshot = racing_store.wallet_solvency_snapshot(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=117,
                wallet_outpoints={f"{original['txid']}:{original['vout']}": 117},
                wallet_snapshot_hash=self.h(700),
            )
            future.result()
        self.assertEqual(snapshot.customer_liability_units, 117)
        self.assertEqual(snapshot.restricted_outpoints, ())
        self.assertEqual(self.store.order_liability_units(order_id=order_id), 137)

    def test_final_wallet_snapshot_requires_exact_structure_hash_and_total(self):
        self.create_sell()
        credited = self.output(1, 117)
        provisional = self.output(2, 10, confirmations=5, block_height=6)
        self.reconcile(
            [self.snapshot("deposit-7", [credited, provisional], tip_number=1)],
            tip_number=1,
        )
        wallet_outpoints = {
            f"{credited['txid']}:{credited['vout']}": 117,
            f"{provisional['txid']}:{provisional['vout']}": 10,
        }
        result = self.store.wallet_solvency_snapshot(
            expected_tip_hash=self.h(1),
            expected_tip_height=10,
            wallet_spendable_units=127,
            wallet_outpoints=wallet_outpoints,
            wallet_snapshot_hash=self.h(701),
        )
        self.assertEqual(result.usable_wallet_units, 117)
        self.assertEqual(result.customer_liability_units, 117)
        with self.assertRaises(ValueError):
            self.store.wallet_solvency_snapshot(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=127,
                wallet_outpoints=None,
                wallet_snapshot_hash=self.h(701),
            )
        with self.assertRaises(ValueError):
            self.store.wallet_solvency_snapshot(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=127,
                wallet_outpoints=wallet_outpoints,
                wallet_snapshot_hash=None,
            )
        with self.assertRaisesRegex(AccountingInvariantError, "structured"):
            self.store.wallet_solvency_snapshot(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=128,
                wallet_outpoints=wallet_outpoints,
                wallet_snapshot_hash=self.h(701),
            )

        class DuplicateMapping(dict):
            def items(inner_self):
                key = f"{credited['txid']}:{credited['vout']}"
                return ((key, 117), (key, 117))

        with self.assertRaisesRegex(ValueError, "unique"):
            self.store.wallet_solvency_snapshot(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=234,
                wallet_outpoints=DuplicateMapping(),
                wallet_snapshot_hash=self.h(701),
            )
        with self.assertRaisesRegex(ValueError, "protocol supply"):
            self.store.wallet_solvency_snapshot(
                expected_tip_hash=self.h(1),
                expected_tip_height=10,
                wallet_spendable_units=2_100_000_000_000_001,
                wallet_outpoints={
                    f"{self.h(991)}:0": 2_100_000_000_000_000,
                    f"{self.h(992)}:0": 1,
                },
                wallet_snapshot_hash=self.h(701),
            )


if __name__ == "__main__":
    unittest.main()
