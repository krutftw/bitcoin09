from __future__ import annotations

import re
import threading
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import Any

from bot.otc.domain import (
    MAX_09C_UNITS,
    FeeQuote,
    OrderSide,
    OrderState,
    parse_asset,
    parse_method,
    parse_total_price,
    quote_deposit,
)
from bot.otc.explorer import (
    AddressBatch,
    ExplorerError,
    ExplorerProtocolError,
    Tip,
    TransactionStatus,
    _address_text as _canonical_09c_address,
)
from bot.otc.store import (
    AccountingInvariantError,
    AttachRecoveryResult,
    ClaimedTransfer,
    ReconciliationResult,
    Store,
)
from bot.otc.wallet import (
    PreparedTransfer,
    SafeSendFailure,
    UncertainSendFailure,
    WalletSnapshot,
)

_SETTLEMENT_NETWORK_RE = re.compile(r"[A-Za-z0-9._ -]+\Z", re.ASCII)


class TradeServiceError(RuntimeError):
    """Base class for command-independent trade orchestration failures."""


class AuthorizationError(TradeServiceError):
    """The actor is not authorized for the requested order action."""


class OrderConflict(TradeServiceError):
    """The action is valid in principle but not for the current order state."""


@dataclass(frozen=True, slots=True)
class ServiceEvent:
    kind: str
    order_id: int
    detail: Mapping[str, object] = field(default_factory=dict)


@dataclass(frozen=True, slots=True)
class OrderResult:
    order_id: int
    side: str
    state: str
    maker_id: int
    buyer_id: int | None
    seller_id: int | None
    net_amount_units: int
    network_fee_units: int
    service_fee_units: int
    deposit_required_units: int
    total_price: str
    settlement_asset: str
    settlement_network: str | None
    payment_method: str
    deposit_addr: str | None
    deposit_deadline: int | None
    buyer_confirmed: bool
    seller_confirmed: bool
    deposit_credited_units: int
    deposit_main_units: int
    deposit_recovery_units: int
    accepted: bool
    events: tuple[ServiceEvent, ...]


@dataclass(frozen=True, slots=True)
class TransferResult:
    transfer_id: int
    order_id: int | None
    state: str
    txid: str | None
    operation_key: str


@dataclass(frozen=True, slots=True)
class SystemHealth:
    accepting_orders: bool
    issues: tuple[str, ...]
    checked_at: int
    wallet_spendable_units: int | None
    customer_liability_units: int | None
    pending_platform_outflow_units: int | None


class TradeService:
    """Application service for two-sided 09C-only escrow trades.

    This layer deliberately returns plain domain results and has no dependency
    on Discord, HTTP request objects, or presentation-specific exceptions.
    """

    def __init__(
        self,
        *,
        store: Store,
        explorer: object,
        wallet: object,
        fresh_address: Callable[[], str],
        confirmation_depth: int,
        clock: Callable[[], int],
        network_fee_units: int = 0,
        fee_bps: int = 0,
        deposit_timeout_seconds: int = 86_400,
        trade_timeout_seconds: int | None = None,
        prepare_child_is_dead: Callable[[Mapping[str, Any]], bool] | None = None,
        transfer_reconciliation_deadline_seconds: int = 600,
    ) -> None:
        if not isinstance(store, Store):
            raise ValueError("store must be a Store")
        if getattr(explorer, "network", None) != store.network:
            raise ValueError("explorer network does not match the store")
        wallet_network = getattr(wallet, "network", store.network)
        if wallet_network != store.network:
            raise ValueError("wallet network does not match the store")
        if not callable(fresh_address) or not callable(clock):
            raise ValueError("address and clock boundaries must be callable")
        if type(confirmation_depth) is not int or confirmation_depth < 1:
            raise ValueError("confirmation depth must be positive")
        if type(network_fee_units) is not int or network_fee_units < 0:
            raise ValueError("network fee must be non-negative integer units")
        if type(fee_bps) is not int or not 0 <= fee_bps <= 10_000:
            raise ValueError("service fee basis points are out of range")
        if type(deposit_timeout_seconds) is not int or deposit_timeout_seconds < 1:
            raise ValueError("deposit timeout must be positive")
        if trade_timeout_seconds is None:
            trade_timeout_seconds = deposit_timeout_seconds
        if type(trade_timeout_seconds) is not int or trade_timeout_seconds < 1:
            raise ValueError("trade timeout must be positive")
        if prepare_child_is_dead is not None and not callable(prepare_child_is_dead):
            raise ValueError("prepare child liveness boundary must be callable")
        if (
            type(transfer_reconciliation_deadline_seconds) is not int
            or transfer_reconciliation_deadline_seconds < 1
        ):
            raise ValueError("transfer reconciliation deadline must be positive")
        self.store = store
        self.explorer = explorer
        self.wallet = wallet
        self._fresh_address = fresh_address
        self._confirmation_depth = confirmation_depth
        self._clock = clock
        self._network_fee_units = network_fee_units
        self._fee_bps = fee_bps
        self._deposit_timeout_seconds = deposit_timeout_seconds
        self._trade_timeout_seconds = trade_timeout_seconds
        self._prepare_child_is_dead = prepare_child_is_dead or (lambda _row: False)
        self._transfer_reconciliation_deadline_seconds = (
            transfer_reconciliation_deadline_seconds
        )
        self._mine_lock = threading.Lock()
        self._broadcast_tip_context: Tip | None = None

    def create_sell(
        self,
        *,
        seller_id: int,
        net_amount: int,
        total_price: str,
        asset: str,
        method: str,
        network: str | None,
        seller_name: str | None = None,
        receive_address: str | None = None,
    ) -> OrderResult:
        self._positive_actor(seller_id)
        seller_name = self._bounded_text(
            seller_name or f"User {seller_id}", "seller name", 128
        )
        receive_address = self._optional_receive_address(
            receive_address, "seller receive address", 128
        )
        total_price = parse_total_price(total_price)
        asset = parse_asset(asset)
        method = parse_method(method)
        network = self._settlement_network(network)
        quote = self._bounded_quote(net_amount)
        now = self._now()
        deposit_addr = self._new_address()
        order_id = self.store.create_order(
            side=OrderSide.SELL,
            maker_id=seller_id,
            maker_name=seller_name,
            maker_wallet_addr=receive_address,
            net_amount_units=quote.net_amount,
            network_fee_units=quote.network_fee,
            service_fee_units=quote.service_fee,
            deposit_required_units=quote.deposit_required,
            total_price=total_price,
            settlement_asset=asset,
            settlement_network=network,
            payment_method=method,
            state=OrderState.AWAITING_DEPOSIT,
            deposit_addr=deposit_addr,
            deposit_deadline=now + self._deposit_timeout_seconds,
            created_at=now,
            updated_at=now,
        )
        return self._load_order(order_id)

    def create_buy(
        self,
        *,
        buyer_id: int,
        net_amount: int,
        total_price: str,
        asset: str,
        method: str,
        network: str | None,
        buyer_name: str | None = None,
        receive_address: str | None = None,
    ) -> OrderResult:
        self._positive_actor(buyer_id)
        buyer_name = self._bounded_text(
            buyer_name or f"User {buyer_id}", "buyer name", 128
        )
        receive_address = self._optional_receive_address(
            receive_address, "buyer receive address", 128
        )
        total_price = parse_total_price(total_price)
        asset = parse_asset(asset)
        method = parse_method(method)
        network = self._settlement_network(network)
        quote = self._bounded_quote(net_amount)
        now = self._now()
        order_id = self.store.create_order(
            side=OrderSide.BUY,
            maker_id=buyer_id,
            maker_name=buyer_name,
            maker_wallet_addr=receive_address,
            net_amount_units=quote.net_amount,
            network_fee_units=quote.network_fee,
            service_fee_units=quote.service_fee,
            deposit_required_units=quote.deposit_required,
            total_price=total_price,
            settlement_asset=asset,
            settlement_network=network,
            payment_method=method,
            state=OrderState.OPEN,
            created_at=now,
            updated_at=now,
        )
        return self._load_order(order_id)

    def accept(
        self,
        order_id: int,
        *,
        actor_id: int,
        actor_name: str | None = None,
        receive_address: str | None = None,
    ) -> OrderResult:
        self._positive_order(order_id)
        self._positive_actor(actor_id)
        initial = self.store.get_order(order_id=order_id)
        if initial is None:
            raise ValueError("order does not exist")
        if initial["state"] != "open" or actor_id == initial["maker_id"]:
            return self._order_result(initial, accepted=False)
        if initial["side"] == OrderSide.BUY.value and (
            initial["seller_id"] is not None
            or initial["seller_name"] is not None
            or initial["deposit_addr"] is not None
        ):
            return self._order_result(initial, accepted=False)
        actor_name = self._bounded_text(
            actor_name or f"User {actor_id}", "actor name", 128
        )
        receive_address = self._optional_receive_address(
            receive_address, "actor receive address", 128
        )
        preallocated: str | None = None
        deadline: int | None = None
        now = self._now()
        if initial["side"] == OrderSide.BUY.value:
            preallocated = self._new_address()
            deadline = now + self._deposit_timeout_seconds
        accepted = self.store.reserve_accept(
            order_id=order_id,
            actor_id=actor_id,
            actor_name=actor_name,
            actor_wallet=receive_address,
            preallocated_deposit_addr=preallocated,
            deposit_deadline=deadline,
            trade_deadline=(
                now + self._trade_timeout_seconds
                if initial["side"] == OrderSide.SELL.value
                else None
            ),
            now=now,
        )
        if accepted is None:
            current = self.store.get_order(order_id=order_id)
            if current is None:
                raise ValueError("order does not exist")
            return self._order_result(current, accepted=False)
        event = ServiceEvent("order_accepted", order_id, {"side": accepted["side"]})
        return self._order_result(accepted, accepted=True, events=(event,))

    def check_deposit(self, order_id: int, *, actor_id: int) -> OrderResult:
        self._positive_order(order_id)
        self._positive_actor(actor_id)
        before = self.store.get_order(order_id=order_id)
        if before is None:
            raise ValueError("order does not exist")
        if before["seller_id"] != actor_id:
            raise AuthorizationError("only the assigned seller may check this deposit")
        _, reconciliation = self._reconcile_all_deposits()
        after = self.store.get_order(order_id=order_id)
        if after is None:
            raise AccountingInvariantError("reconciled order disappeared")
        events: list[ServiceEvent] = []
        change = next(
            (
                item
                for item in reconciliation.order_changes
                if item.order_id == order_id
            ),
            None,
        )
        if (
            change is not None
            and change.old_state == "awaiting_deposit"
            and change.new_state in {"open", "matched"}
        ):
            events.append(ServiceEvent("payment_ready", order_id))
        recovery_event_units = 0
        if (
            change is not None
            and change.recovery_delta_units > 0
            and change.new_state != "awaiting_deposit"
        ):
            recovery_event_units = change.recovery_delta_units
        elif (
            change is not None
            and change.old_state == "awaiting_deposit"
            and change.new_state
            in {
                "open",
                "matched",
                "deposit_expired",
                "recovery_hold",
                "refund_reserved",
            }
        ):
            recovery_event_units = change.recovery_total_units
        if change is not None and recovery_event_units > 0:
            kind = (
                "excess_deposit_recovery"
                if change.new_state in {"open", "matched", "disputed"}
                else "late_payment_recovery"
            )
            events.append(ServiceEvent(kind, order_id, {"units": recovery_event_units}))
        return self._order_result(after, events=tuple(events))

    def confirm_sent(self, order_id: int, *, actor_id: int) -> OrderResult:
        return self._confirm(order_id, actor_id=actor_id, role="buyer")

    def confirm_received(self, order_id: int, *, actor_id: int) -> OrderResult:
        return self._confirm(order_id, actor_id=actor_id, role="seller")

    def list_open(self) -> tuple[OrderResult, ...]:
        return tuple(self._order_result(row) for row in self.store.list_open_orders())

    def cancel(self, order_id: int, *, actor_id: int) -> OrderResult:
        self._positive_order(order_id)
        self._positive_actor(actor_id)
        try:
            row = self.store.cancel_order(
                order_id=order_id, actor_id=actor_id, now=self._now()
            )
        except PermissionError as exc:
            raise AuthorizationError(str(exc)) from None
        except AccountingInvariantError as exc:
            raise OrderConflict(str(exc)) from None
        return self._order_result(row)

    def open_dispute(self, order_id: int, *, actor_id: int, reason: str) -> OrderResult:
        self._positive_order(order_id)
        self._positive_actor(actor_id)
        try:
            row = self.store.open_order_dispute(
                order_id=order_id,
                actor_id=actor_id,
                reason=reason,
                now=self._now(),
            )
        except PermissionError as exc:
            raise AuthorizationError(str(exc)) from None
        except AccountingInvariantError as exc:
            raise OrderConflict(str(exc)) from None
        return self._order_result(row)

    def resolve_dispute(
        self,
        order_id: int,
        *,
        admin_id: int,
        winner: str,
        reason: str,
    ) -> OrderResult:
        self._positive_order(order_id)
        self._positive_actor(admin_id)
        row = self.store.resolve_order_dispute(
            order_id=order_id,
            actor_id=admin_id,
            winner=winner,
            reason=reason,
            now=self._now(),
        )
        return self._order_result(row)

    def expire_orders(self) -> tuple[int, ...]:
        return self.store.expire_matched_orders(now=self._now())

    def cancel_fee_withdrawal(
        self, transfer_id: int, *, admin_id: int
    ) -> TransferResult:
        self._positive_order(transfer_id)
        self._positive_actor(admin_id)
        row = self.store.cancel_failed_safe_fee_withdrawal(
            transfer_id=transfer_id, actor_id=admin_id, now=self._now()
        )
        return self._transfer_result(row)

    def reconcile_transfers(self) -> tuple[TransferResult, ...]:
        """Reconcile durable transfer identities; safe at startup or on a timer."""

        with self._mine_lock:
            return self._reconcile_transfers_locked()

    def _reconcile_transfers_locked(self) -> tuple[TransferResult, ...]:
        batch, _ = self._reconcile_all_deposits()
        results: list[TransferResult] = []
        active = self.store.active_wallet_transfer()
        if active is not None and active["state"] == "reserved":
            if not self._prepare_child_is_dead(active):
                return (self._transfer_result(active),)
            failed = self.store.mark_transfer_failed_safe(
                transfer_id=active["transfer_id"],
                expected_attempt_count=active["attempt_count"],
                expected_reserved_at=active["reserved_at"],
                error_text="prior prepare child confirmed stopped",
                now=self._now(),
            )
            self.store.requeue_failed_safe(
                transfer_id=failed["transfer_id"], now=self._now()
            )

        for transfer in self.store.list_reconcilable_transfers():
            current = self._reconcile_transfer(transfer, batch.tip)
            results.append(self._transfer_result(current))

        if (
            self.store.active_wallet_transfer() is None
            and not self.store.health_issues()
        ):
            mined = self._mine_once(expected_tip=batch.tip)
            if mined is not None:
                results.append(mined)
        return tuple(results)

    def system_health(self) -> SystemHealth:
        now = self._now()
        issues: set[str] = set()
        spendable: int | None = None
        liability: int | None = None
        pending: int | None = None
        try:
            if self.store.integrity_check().lower() != "ok":
                issues.add("database_integrity")
        except Exception:
            return SystemHealth(False, ("database_failure",), now, None, None, None)
        try:
            batch, reconciliation = self._reconcile_all_deposits()
        except ExplorerError:
            return SystemHealth(False, ("explorer_failure",), now, None, None, None)
        except Exception:
            return SystemHealth(False, ("database_failure",), now, None, None, None)
        issues.update(reconciliation.health_issues)
        try:
            issues.update(self.store.health_issues())
            liability = self.store.customer_liability_units()
            pending = self.store.pending_platform_outflow_units()
        except Exception:
            issues.add("database_failure")
            return SystemHealth(
                False,
                tuple(sorted(issues)),
                now,
                spendable,
                liability,
                pending,
            )
        try:
            for transfer in self.store.list_reconcilable_transfers():
                txid = transfer["txid"]
                if not isinstance(txid, str):
                    issues.add("malformed_transfer_identity")
                    continue
                status = self.explorer.transaction(txid)
                if not isinstance(status, TransactionStatus) or status.txid != txid:
                    issues.add("explorer_failure")
                    continue
                if status.tip != batch.tip:
                    issues.add("explorer_failure")
                    continue
                if transfer["state"] == "confirmed":
                    if (
                        status.status != "confirmed"
                        or status.block is None
                        or status.block.hash != transfer["confirmed_block_hash"]
                        or status.block.height != transfer["confirmed_block_height"]
                        or status.confirmations < transfer["confirmations"]
                    ):
                        issues.add("transfer_reorg")
                elif transfer["state"] == "broadcast" and status.status == "unknown":
                    anchor = transfer["broadcast_at"] or transfer["updated_at"]
                    if now - anchor > self._transfer_reconciliation_deadline_seconds:
                        issues.add("uncertain_transfer")
        except ExplorerError:
            issues.add("explorer_failure")
        except Exception:
            issues.add("database_failure")
        try:
            snapshot = self.wallet.snapshot(batch.tip)
            self._require_wallet_snapshot(snapshot, batch.tip)
            spendable = snapshot.spendable_units
            if not issues:
                outpoints = {
                    item.outpoint: item.amount_units for item in snapshot.outpoints
                }
                if len(outpoints) != len(snapshot.outpoints):
                    raise AccountingInvariantError(
                        "wallet snapshot contains duplicate outpoints"
                    )
                self.store.wallet_solvency_snapshot(
                    expected_tip_hash=batch.tip.hash,
                    expected_tip_height=batch.tip.height,
                    wallet_spendable_units=snapshot.spendable_units,
                    wallet_outpoints=outpoints,
                    wallet_snapshot_hash=snapshot.wallet_snapshot_hash,
                )
        except AccountingInvariantError:
            if not issues:
                issues.add("wallet_insolvent")
        except Exception:
            issues.add("wallet_failure")
        return SystemHealth(
            not issues,
            tuple(sorted(issues)),
            now,
            spendable,
            liability,
            pending,
        )

    def mine(self) -> TransferResult | None:
        """Run at most one globally serialized wallet operation."""
        with self._mine_lock:
            reconciled = self._reconcile_transfers_locked()
            return None if not reconciled else reconciled[-1]

    def _mine_once(self, *, expected_tip: Tip) -> TransferResult | None:
        if not isinstance(expected_tip, Tip):
            raise ValueError("expected tip must be a canonical tip")
        active = self.store.active_wallet_transfer()
        if active is not None:
            # Active operations are recovered by the transfer reconciler.  This
            # primitive only owns the post-reconciliation queued claim.
            return self._transfer_result(active)

        barrier, _ = self._reconcile_all_deposits()
        if barrier.tip != expected_tip:
            raise AccountingInvariantError(
                "watched deposit barrier changed after transfer reconciliation"
            )
        wallet_snapshot = self.wallet.snapshot(expected_tip)
        self._require_wallet_snapshot(wallet_snapshot, expected_tip)
        wallet_outpoints = {
            item.outpoint: item.amount_units for item in wallet_snapshot.outpoints
        }
        if len(wallet_outpoints) != len(wallet_snapshot.outpoints):
            raise AccountingInvariantError(
                "wallet snapshot contains duplicate outpoints"
            )
        solvency = self.store.wallet_solvency_snapshot(
            expected_tip_hash=expected_tip.hash,
            expected_tip_height=expected_tip.height,
            wallet_spendable_units=wallet_snapshot.spendable_units,
            wallet_outpoints=wallet_outpoints,
            wallet_snapshot_hash=wallet_snapshot.wallet_snapshot_hash,
        )
        claimed = self.store.claim_next_transfer(
            expected_tip_hash=expected_tip.hash,
            expected_tip_height=expected_tip.height,
            wallet_spendable_units=wallet_snapshot.spendable_units,
            now=self._now(),
        )
        if claimed is None:
            return None
        self._validate_claim_coverage(claimed)
        if claimed.restricted_outpoints != solvency.restricted_outpoints:
            raise AccountingInvariantError(
                "claim restriction set changed after solvency"
            )
        restricted = tuple(
            f"{txid}:{vout}" for txid, vout, _ in solvency.restricted_outpoints
        )
        try:
            prepared = self.wallet.prepare(
                claimed.destination,
                claimed.amount_units,
                claimed.network_fee_units,
                expected_tip,
                restricted,
                wallet_snapshot,
            )
        except SafeSendFailure as exc:
            failed = self.store.mark_transfer_failed_safe(
                transfer_id=claimed.transfer_id,
                expected_attempt_count=claimed.attempt_count,
                expected_reserved_at=claimed.reserved_at,
                error_text=str(exc) or "wallet preparation failed safely",
                now=self._now(),
            )
            return self._transfer_result(failed)

        if not isinstance(prepared, PreparedTransfer):
            raise AccountingInvariantError("wallet returned an invalid prepared result")
        if prepared.wallet_snapshot_hash != wallet_snapshot.wallet_snapshot_hash:
            return self._safe_requeue_after_prepare(
                claimed,
                SafeSendFailure("wallet snapshot changed before signed attachment"),
            )
        try:
            live_tip = self.explorer.tip()
            if not isinstance(live_tip, Tip) or live_tip != prepared.snapshot_tip:
                raise SafeSendFailure("live tip changed before signed attachment")
        except (ExplorerError, SafeSendFailure) as exc:
            return self._safe_requeue_after_prepare(claimed, exc)
        attached = self._attach_prepared(
            claimed=claimed,
            prepared=prepared,
            live_tip=live_tip,
            expected_snapshot=wallet_snapshot,
        )
        if attached.classification == "safe_precondition_drift":
            return self._safe_requeue_after_prepare(
                claimed,
                SafeSendFailure(
                    "watched deposit barrier changed before signed attachment"
                ),
            )
        if attached.classification == "retry_reserved":
            # No bytes reached SQLite or the network.  Close the reservation as
            # a safe failure instead of later constructing an apparent
            # replacement for an unjournaled local transaction.
            failed = self.store.mark_transfer_failed_safe(
                transfer_id=claimed.transfer_id,
                expected_attempt_count=claimed.attempt_count,
                expected_reserved_at=claimed.reserved_at,
                error_text="signed attachment was not durably committed",
                now=self._now(),
            )
            return self._transfer_result(failed)
        if attached.classification in {"broadcast", "confirmed", "uncertain"}:
            return self._transfer_result(attached.transfer)
        return self._broadcast_with_tip(attached.transfer, expected_tip)

    def _confirm(self, order_id: int, *, actor_id: int, role: str) -> OrderResult:
        self._positive_order(order_id)
        self._positive_actor(actor_id)
        before = self.store.get_order(order_id=order_id)
        if before is None:
            raise ValueError("order does not exist")
        expected = before["buyer_id"] if role == "buyer" else before["seller_id"]
        if expected != actor_id:
            raise AuthorizationError(f"only the {role} may record this confirmation")
        mutation = self.store.record_confirmation_result(
            order_id=order_id, actor_id=actor_id, now=self._now()
        )
        if mutation is None:
            current = self.store.get_order(order_id=order_id)
            if current is None:
                raise ValueError("order does not exist")
            return self._order_result(current)
        events = (
            (ServiceEvent(f"{mutation.role}_confirmed", order_id),)
            if mutation.mutated
            else ()
        )
        return self._order_result(mutation.order, events=events)

    def _reconcile_transfer(
        self, transfer: Mapping[str, Any], expected_tip: Tip
    ) -> Mapping[str, Any]:
        txid = transfer["txid"]
        if not isinstance(txid, str):
            raise AccountingInvariantError("reconcilable transfer has no transaction")
        status = self.explorer.transaction(txid)
        if not isinstance(status, TransactionStatus) or status.txid != txid:
            raise AccountingInvariantError("explorer returned another transaction")
        if status.tip != expected_tip:
            raise AccountingInvariantError(
                "transaction observation tip differs from deposit barrier"
            )
        now = self._now()

        if transfer["state"] == "prepared":
            if status.status == "unknown":
                self._broadcast_with_tip(transfer, expected_tip)
                return self.store.get_transfer(transfer_id=transfer["transfer_id"])
            transfer = self.store.mark_transfer_broadcast(
                transfer_id=transfer["transfer_id"],
                observed_txid=txid,
                observed_status=status.status,
                now=now,
                expected_tip_hash=expected_tip.hash,
                expected_tip_height=expected_tip.height,
            )
        elif transfer["state"] == "broadcast" and status.status == "unknown":
            anchor = transfer["broadcast_at"] or transfer["updated_at"]
            if now - anchor > self._transfer_reconciliation_deadline_seconds:
                return self.store.mark_transfer_uncertain(
                    transfer_id=transfer["transfer_id"],
                    expected_state="broadcast",
                    expected_txid=txid,
                    error_text="broadcast transaction missed reconciliation deadline",
                    now=now,
                    expected_tip_hash=expected_tip.hash,
                    expected_tip_height=expected_tip.height,
                )
            return transfer
        elif transfer["state"] == "confirmed":
            same_anchor = bool(
                status.status == "confirmed"
                and status.block is not None
                and status.block.hash == transfer["confirmed_block_hash"]
                and status.block.height == transfer["confirmed_block_height"]
                and status.confirmations >= transfer["confirmations"]
            )
            if not same_anchor:
                return self.store.mark_transfer_uncertain(
                    transfer_id=transfer["transfer_id"],
                    expected_state="confirmed",
                    expected_txid=txid,
                    error_text="confirmed transaction lost its canonical anchor",
                    now=now,
                    expected_tip_hash=expected_tip.hash,
                    expected_tip_height=expected_tip.height,
                )
        elif transfer["state"] == "uncertain":
            if status.status == "unknown":
                return transfer
            if status.status == "mempool":
                return self.store.mark_transfer_broadcast(
                    transfer_id=transfer["transfer_id"],
                    observed_txid=txid,
                    observed_status="mempool",
                    now=now,
                    expected_tip_hash=expected_tip.hash,
                    expected_tip_height=expected_tip.height,
                )

        if status.status != "confirmed":
            return transfer
        if status.block is None:
            raise AccountingInvariantError("confirmed transaction has no block anchor")
        if status.confirmations < self._confirmation_depth:
            return transfer
        return self.store.mark_transfer_confirmed(
            transfer_id=transfer["transfer_id"],
            observed_txid=txid,
            confirmed_block_hash=status.block.hash,
            confirmed_block_height=status.block.height,
            confirmations=status.confirmations,
            now=now,
            expected_tip_hash=expected_tip.hash,
            expected_tip_height=expected_tip.height,
        )

    def _reconcile_all_deposits(
        self,
    ) -> tuple[AddressBatch, ReconciliationResult]:
        batch = self.explorer.batch_outputs(self.store.watched_deposit_addresses)
        if not isinstance(batch, AddressBatch):
            raise AccountingInvariantError("explorer returned an invalid address batch")
        if batch.network != self.store.network:
            raise AccountingInvariantError(
                "explorer address batch has the wrong network"
            )
        now = self._now()
        reconciliation = self.store.reconcile_all_deposit_outputs(
            network=batch.network,
            expected_tip_hash=batch.tip.hash,
            expected_tip_height=batch.tip.height,
            snapshots=batch.store_snapshots(),
            final_tip_hash=batch.tip.hash,
            final_tip_height=batch.tip.height,
            credit_depth=self._confirmation_depth,
            trade_timeout_seconds=self._trade_timeout_seconds,
            now=now,
        )
        return batch, reconciliation

    def _attach_prepared(
        self,
        *,
        claimed: ClaimedTransfer,
        prepared: object,
        live_tip: Tip,
        expected_snapshot: WalletSnapshot,
    ) -> AttachRecoveryResult:
        return self.store.attach_signed_transfer(
            transfer_id=claimed.transfer_id,
            expected_attempt_count=claimed.attempt_count,
            expected_reserved_at=claimed.reserved_at,
            txid=prepared.txid,
            signed_tx_hex=prepared.signed_tx_hex,
            destination=prepared.destination,
            amount_units=prepared.amount_units,
            network_fee_units=prepared.fee_units,
            prepared_tip_hash=prepared.snapshot_tip.hash,
            prepared_tip_height=prepared.snapshot_tip.height,
            live_tip_hash=live_tip.hash,
            live_tip_height=live_tip.height,
            wallet_snapshot_hash=prepared.wallet_snapshot_hash,
            expected_wallet_snapshot_hash=expected_snapshot.wallet_snapshot_hash,
            now=self._now(),
        )

    def _broadcast_with_tip(
        self, transfer: Mapping[str, Any], expected_tip: Tip
    ) -> TransferResult:
        if self._broadcast_tip_context is not None:
            raise AccountingInvariantError("broadcast tip context is already active")
        self._broadcast_tip_context = expected_tip
        try:
            return self._broadcast_stored(transfer)
        finally:
            self._broadcast_tip_context = None

    def _broadcast_stored(self, transfer: Mapping[str, Any]) -> TransferResult:
        expected_tip = self._broadcast_tip_context
        if expected_tip is None:
            raise AccountingInvariantError("broadcast lacks a common-tip barrier")
        state = transfer["state"]
        if state not in {"prepared", "broadcast"}:
            raise AccountingInvariantError("stored transfer is not broadcastable")
        txid = transfer["txid"]
        if state == "broadcast":
            return self._transfer_result(transfer)
        wallet_invoked = False

        def invoke(fresh: Mapping[str, Any]) -> tuple[str, str]:
            nonlocal wallet_invoked
            wallet_invoked = True
            prepared_tip = Tip(fresh["prepared_tip_hash"], fresh["prepared_tip_height"])
            result = self.wallet.broadcast(
                fresh["signed_tx_hex"], fresh["txid"], prepared_tip
            )
            if result.txid != fresh["txid"] or result.status not in {
                "mempool",
                "confirmed",
            }:
                raise UncertainSendFailure("trusted broadcast identity is ambiguous")
            if result.observed_tip != expected_tip:
                raise UncertainSendFailure(
                    "trusted broadcast tip differs from deposit barrier"
                )
            return result.txid, result.status

        try:
            authorization = self.store.broadcast_prepared_with_authorization(
                transfer_id=transfer["transfer_id"],
                expected_txid=txid,
                invoke=invoke,
                now=self._now(),
                expected_tip_hash=expected_tip.hash,
                expected_tip_height=expected_tip.height,
            )
        except BaseException as exc:
            if not wallet_invoked:
                raise
            uncertain = self.store.mark_transfer_uncertain(
                transfer_id=transfer["transfer_id"],
                expected_state="prepared",
                expected_txid=txid,
                error_text=str(exc) or "wallet broadcast outcome is uncertain",
                now=self._now(),
                expected_tip_hash=expected_tip.hash,
                expected_tip_height=expected_tip.height,
            )
            if not isinstance(exc, Exception):
                raise
            return self._transfer_result(uncertain)
        return self._transfer_result(authorization.transfer)

    def _validate_claim_coverage(self, claimed: ClaimedTransfer) -> None:
        required = (
            claimed.amount_units + claimed.network_fee_units + claimed.earned_fee_units
        )
        allocated = self.store.transfer_allocation_units(
            transfer_id=claimed.transfer_id
        )
        if claimed.kind == "fee_withdrawal":
            if (
                allocated != 0
                or claimed.order_id is not None
                or claimed.earned_fee_units != 0
            ):
                raise AccountingInvariantError(
                    "fee withdrawal claim has customer allocation evidence"
                )
            self.store.validate_claimed_fee_withdrawal(
                transfer_id=claimed.transfer_id,
                expected_attempt_count=claimed.attempt_count,
                expected_reserved_at=claimed.reserved_at,
            )
            return
        if allocated != required:
            raise AccountingInvariantError("claimed transfer allocation is incomplete")
        if (
            claimed.order_id is not None
            and self.store.order_liability_units(order_id=claimed.order_id) < required
        ):
            raise AccountingInvariantError("order cannot cover its immutable transfer")

    def _safe_requeue_after_prepare(
        self, claimed: ClaimedTransfer, error: BaseException
    ) -> TransferResult:
        failed = self.store.mark_transfer_failed_safe(
            transfer_id=claimed.transfer_id,
            expected_attempt_count=claimed.attempt_count,
            expected_reserved_at=claimed.reserved_at,
            error_text=str(error) or "pre-attachment state changed safely",
            now=self._now(),
        )
        try:
            queued = self.store.requeue_failed_safe(
                transfer_id=claimed.transfer_id, now=self._now()
            )
        except AccountingInvariantError:
            return self._transfer_result(failed)
        return self._transfer_result(queued)

    def _require_wallet_snapshot(self, value: object, expected_tip: Tip) -> None:
        if (
            not isinstance(value, WalletSnapshot)
            or value.tip != expected_tip
            or value.network != self.store.network
        ):
            raise AccountingInvariantError(
                "wallet returned an invalid anchored snapshot"
            )

    def _load_order(self, order_id: int) -> OrderResult:
        row = self.store.get_order(order_id=order_id)
        if row is None:
            raise AccountingInvariantError("persisted order disappeared")
        return self._order_result(row)

    def _order_result(
        self,
        row: Mapping[str, Any],
        *,
        accepted: bool = True,
        events: tuple[ServiceEvent, ...] = (),
    ) -> OrderResult:
        accounting = self.store.deposit_accounting(order_id=row["order_id"])
        return OrderResult(
            order_id=row["order_id"],
            side=row["side"],
            state=row["state"],
            maker_id=row["maker_id"],
            buyer_id=row["buyer_id"],
            seller_id=row["seller_id"],
            net_amount_units=row["net_amount_units"],
            network_fee_units=row["network_fee_units"],
            service_fee_units=row["service_fee_units"],
            deposit_required_units=row["deposit_required_units"],
            total_price=row["total_price"],
            settlement_asset=row["settlement_asset"],
            settlement_network=row["settlement_network"],
            payment_method=row["payment_method"],
            deposit_addr=row["deposit_addr"],
            deposit_deadline=row["deposit_deadline"],
            buyer_confirmed=bool(row["buyer_confirmed"]),
            seller_confirmed=bool(row["seller_confirmed"]),
            deposit_credited_units=accounting["credited_units"],
            deposit_main_units=accounting["main_units"],
            deposit_recovery_units=accounting["recovery_units"],
            accepted=accepted,
            events=events,
        )

    @staticmethod
    def _transfer_result(row: Mapping[str, Any]) -> TransferResult:
        return TransferResult(
            transfer_id=row["transfer_id"],
            order_id=row["order_id"],
            state=row["state"],
            txid=row["txid"],
            operation_key=row["operation_key"],
        )

    def _new_address(self) -> str:
        value = self._fresh_address()
        if (
            not isinstance(value, str)
            or not value
            or "\x00" in value
            or len(value.encode("utf-8")) > 128
        ):
            raise SafeSendFailure("wallet address creation failed safely")
        try:
            return _canonical_09c_address(value)
        except ExplorerProtocolError:
            raise SafeSendFailure("wallet address creation failed safely") from None

    def _bounded_quote(self, net_amount: object) -> FeeQuote:
        if type(net_amount) is not int or not 1 <= net_amount <= MAX_09C_UNITS:
            raise ValueError("net amount is outside protocol bounds")
        network_fee = self._network_fee_units
        if type(network_fee) is not int or not 0 <= network_fee <= MAX_09C_UNITS:
            raise ValueError("network fee is outside protocol bounds")
        quote = quote_deposit(
            net_amount=net_amount,
            network_fee=network_fee,
            fee_bps=self._fee_bps,
        )
        if (
            type(quote.service_fee) is not int
            or not 0 <= quote.service_fee <= MAX_09C_UNITS
        ):
            raise ValueError("service fee is outside protocol bounds")
        if quote.net_amount > MAX_09C_UNITS - quote.network_fee:
            raise ValueError("net amount plus network fee exceeds protocol bounds")
        subtotal = quote.net_amount + quote.network_fee
        if quote.service_fee > MAX_09C_UNITS - subtotal:
            raise ValueError("deposit quote exceeds protocol bounds")
        deposit_required = subtotal + quote.service_fee
        if (
            type(quote.deposit_required) is not int
            or quote.deposit_required != deposit_required
            or not 1 <= quote.deposit_required <= MAX_09C_UNITS
        ):
            raise ValueError("deposit required is outside protocol bounds")
        return quote

    def _optional_receive_address(
        self, value: object, label: str, max_bytes: int
    ) -> str | None:
        if value is None:
            return None
        bounded = self._bounded_text(value, label, max_bytes)
        if self.store.network not in {"btc09-mainnet", "btc09-regtest"}:
            raise ValueError("receive address network is invalid")
        try:
            return _canonical_09c_address(bounded)
        except ExplorerProtocolError:
            raise ValueError(f"{label} is not a canonical 09C address") from None

    @classmethod
    def _settlement_network(cls, value: object) -> str | None:
        if value is None:
            return None
        result = cls._bounded_text(value, "settlement network", 48)
        if _SETTLEMENT_NETWORK_RE.fullmatch(result) is None:
            raise ValueError("settlement network contains invalid characters")
        return result

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

    def _now(self) -> int:
        value = self._clock()
        if type(value) is not int or value <= 0:
            raise ValueError("clock must return a positive integer timestamp")
        return value

    @staticmethod
    def _positive_actor(actor_id: int) -> None:
        if type(actor_id) is not int or actor_id <= 0:
            raise ValueError("actor ID must be a positive integer")

    @staticmethod
    def _positive_order(order_id: int) -> None:
        if type(order_id) is not int or order_id <= 0:
            raise ValueError("order ID must be a positive integer")
