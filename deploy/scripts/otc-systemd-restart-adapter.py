#!/usr/bin/env python3
"""Production-backed adapter for the root-only OTC systemd restart test."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
if str(REPOSITORY_ROOT) not in sys.path:
    sys.path.insert(0, str(REPOSITORY_ROOT))

from bot.otc.explorer import (  # noqa: E402
    AddressBatch,
    AddressSnapshot,
    BlockAnchor,
    ConfirmedOutput,
    Tip,
    TransactionStatus,
)
from bot.otc.service import TradeService  # noqa: E402
from bot.otc.store import Store  # noqa: E402
from bot.otc.wallet import (  # noqa: E402
    BroadcastResult,
    PreparedTransfer,
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


class _Clock:
    def __init__(self, value: int = 80_000) -> None:
        self.value = value

    def __call__(self) -> int:
        self.value += 1
        return self.value


class _RestartExplorer:
    network = "btc09-regtest"

    def __init__(self) -> None:
        self.current_tip = Tip(h(900), 100)
        self.outputs_by_address: dict[str, tuple[ConfirmedOutput, ...]] = {}
        self.transactions: dict[str, TransactionStatus] = {}
        self.transaction_calls: list[str] = []

    def set_outputs(self, owner: str, amounts: list[int]) -> None:
        height = self.current_tip.height - 5
        self.outputs_by_address[owner] = tuple(
            ConfirmedOutput(
                txid=h(10_000 + offset),
                transaction_index=offset,
                vout=offset,
                amount_units=amount,
                block=BlockAnchor(h(20_000 + offset), height),
                confirmations=6,
                coinbase=False,
                mature=True,
                spent_by=None,
            )
            for offset, amount in enumerate(amounts)
        )

    def batch_outputs(self, read_watched_addresses: object) -> AddressBatch:
        addresses = tuple(read_watched_addresses())  # type: ignore[operator]
        snapshots = tuple(
            AddressSnapshot(
                self.network,
                owner,
                True,
                self.current_tip,
                self.outputs_by_address.get(owner, ()),
            )
            for owner in addresses
        )
        if tuple(read_watched_addresses()) != addresses:  # type: ignore[operator]
            raise RuntimeError("watched address set changed")
        return AddressBatch(self.network, self.current_tip, snapshots)

    def tip(self) -> Tip:
        return self.current_tip

    def transaction(self, txid: str) -> TransactionStatus:
        self.transaction_calls.append(txid)
        return self.transactions.get(
            txid, TransactionStatus(txid, "unknown", None, 0, self.current_tip)
        )

    def set_transaction(self, txid: str, status: str) -> None:
        self.transactions[txid] = TransactionStatus(
            txid, status, None, 0, self.current_tip
        )


class _RestartWallet:
    network = "btc09-regtest"

    def __init__(self, explorer: _RestartExplorer, lock_path: Path) -> None:
        self.explorer = explorer
        self.allocation_lock_path = str(lock_path.resolve())
        self.primary_address = address(9_999)
        self.addresses = [self.primary_address]
        self.address_count = 0

    def allocation_lock(self, *, timeout: float = 5.0) -> WalletAllocationLock:
        return WalletAllocationLock(self.allocation_lock_path, acquire_timeout=timeout)

    def new_address(self) -> str:
        self.address_count += 1
        created = address(10_000 + self.address_count)
        self.addresses.append(created)
        return created

    def snapshot(self, expected_tip: Tip) -> WalletSnapshot:
        owners = tuple(sorted(set(self.addresses) | set(self.explorer.outputs_by_address)))
        outputs = [
            WalletOutpoint(
                outpoint=f"{output.txid}:{output.vout}",
                txid=output.txid,
                vout=output.vout,
                amount_units=output.amount_units,
                address=owner,
                owner_address_index=owners.index(owner),
            )
            for owner in owners
            for output in self.explorer.outputs_by_address.get(owner, ())
        ]
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
        restricted_outpoints: tuple[str, ...],
        expected_snapshot: WalletSnapshot,
    ) -> PreparedTransfer:
        raw_hex = "01020304"
        txid = hashlib.sha256(hashlib.sha256(bytes.fromhex(raw_hex)).digest()).hexdigest()
        spendable = tuple(
            item.outpoint
            for item in expected_snapshot.outpoints
            if item.outpoint not in restricted_outpoints
        )
        return PreparedTransfer(
            txid,
            raw_hex,
            destination,
            amount_units,
            fee_units,
            expected_tip,
            expected_snapshot.wallet_snapshot_hash,
            spendable[:1],
        )

    def broadcast(
        self, signed_tx_hex: str, expected_txid: str, prepared_tip: Tip
    ) -> BroadcastResult:
        self.explorer.set_transaction(expected_txid, "mempool")
        return BroadcastResult(
            expected_txid, "mempool", 1, True, prepared_tip
        )


def _atomic_text(path: Path, value: str) -> None:
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("w", encoding="ascii") as handle:
        handle.write(value)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _service(
    db_path: Path,
    explorer: _RestartExplorer,
    wallet: _RestartWallet,
    *,
    clock_start: int = 80_000,
) -> TradeService:
    return TradeService(
        store=Store(db_path, network="btc09-regtest"),
        explorer=explorer,
        wallet=wallet,
        fresh_address=wallet.new_address,
        confirmation_depth=6,
        clock=_Clock(clock_start),
        network_fee_units=10,
        fee_bps=0,
        deposit_timeout_seconds=600,
        transfer_reconciliation_deadline_seconds=30,
    )


def prepare(db_path: Path, wallet_path: Path) -> None:
    Store(db_path, network="btc09-regtest").initialize()
    _atomic_text(
        wallet_path,
        json.dumps({"network": "btc09-regtest", "prepared": False}, separators=(",", ":")),
    )
    os.chmod(wallet_path, 0o600)


def inject(db_path: Path, wallet_path: Path) -> None:
    store = Store(db_path, network="btc09-regtest")
    store.initialize()
    explorer = _RestartExplorer()
    wallet = _RestartWallet(explorer, wallet_path.with_suffix(".allocation.lock"))
    service = _service(db_path, explorer, wallet)
    order = service.create_sell(
        seller_id=1,
        seller_name="Restart Seller",
        receive_address=address(20_001),
        net_amount=100,
        total_price="2",
        asset="AUD",
        method="PayID",
        network=None,
    )
    explorer.set_outputs(order.deposit_addr, [110])
    service.check_deposit(order.order_id, actor_id=1)
    service.accept(
        order.order_id,
        actor_id=2,
        actor_name="Restart Buyer",
        receive_address=address(30_002),
    )
    service.confirm_sent(order.order_id, actor_id=2)
    service.confirm_received(order.order_id, actor_id=1)
    original_broadcast = service._broadcast_stored
    service._broadcast_stored = lambda row: service._transfer_result(row)  # type: ignore[method-assign]
    try:
        result = service.mine()
    finally:
        service._broadcast_stored = original_broadcast  # type: ignore[method-assign]
    if result is None or result.state != "prepared":
        raise RuntimeError("production service did not create a prepared transfer")
    transfer = store.get_order_transfer(order_id=order.order_id)
    if transfer is None or transfer["state"] != "prepared" or transfer["txid"] != result.txid:
        raise RuntimeError("prepared transfer identity was not durable")
    decoy_txid = h(999_999)
    if decoy_txid == transfer["txid"]:
        raise RuntimeError("decoy transaction identity collided")
    _atomic_text(
        wallet_path,
        json.dumps(
            {
                "network": "btc09-regtest",
                "prepared": True,
                "deposit_address": order.deposit_addr,
                "deposit_amount_units": 110,
                "expected_txid": transfer["txid"],
                "decoy_txid": decoy_txid,
            },
            separators=(",", ":"),
        ),
    )
    os.chmod(wallet_path, 0o600)


def _load_wallet_state(wallet_path: Path) -> dict[str, object]:
    payload = json.loads(wallet_path.read_text(encoding="ascii"))
    if type(payload) is not dict or payload.get("network") != "btc09-regtest":
        raise RuntimeError("isolated regtest wallet adapter is invalid")
    return payload


def _clock_after_database(db_path: Path) -> int:
    connection = sqlite3.connect(db_path)
    try:
        values = [
            connection.execute(f"SELECT COALESCE(MAX({column}),0) FROM {table}").fetchone()[0]
            for table, column in (("orders", "updated_at"), ("transfers", "updated_at"), ("audit_events", "created_at"))
        ]
    finally:
        connection.close()
    return max(int(value) for value in values) + 100


def verify(db_path: Path, state_dir: Path) -> None:
    store = Store(db_path, network="btc09-regtest")
    store.initialize()
    connection = store.connect()
    try:
        rows = connection.execute("SELECT state,txid FROM transfers").fetchall()
    finally:
        connection.close()
    evidence = json.loads((state_dir / "recovery.json").read_text(encoding="ascii"))
    expected_txid = evidence.get("expected_txid")
    decoy_txid = evidence.get("decoy_txid")
    calls = evidence.get("transaction_calls")
    if (
        len(rows) != 1
        or rows[0]["state"] != "broadcast"
        or rows[0]["txid"] != expected_txid
        or calls != [expected_txid]
        or decoy_txid == expected_txid
        or decoy_txid in calls
    ):
        raise SystemExit("production recovery did not reconcile only the exact prepared txid")


def serve(db_path: Path, wallet_path: Path, state_dir: Path) -> None:
    state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    counter = state_dir / "generation-counter"
    generation = int(counter.read_text(encoding="ascii")) + 1 if counter.exists() else 1
    _atomic_text(counter, str(generation))
    _atomic_text(state_dir / "intake-enabled", "0\n")

    child = subprocess.Popen(
        [sys.executable, "-c", "import time; time.sleep(3600)"],
        close_fds=True,
    )
    stopped = False

    def stop(_signum: int, _frame: object) -> None:
        nonlocal stopped
        stopped = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        wallet_state = _load_wallet_state(wallet_path)
        explorer = _RestartExplorer()
        wallet = _RestartWallet(explorer, wallet_path.with_suffix(".allocation.lock"))
        if wallet_state.get("prepared") is True:
            deposit_address = wallet_state.get("deposit_address")
            deposit_amount = wallet_state.get("deposit_amount_units")
            expected_txid = wallet_state.get("expected_txid")
            decoy_txid = wallet_state.get("decoy_txid")
            if (
                type(deposit_address) is not str
                or type(deposit_amount) is not int
                or type(expected_txid) is not str
                or type(decoy_txid) is not str
            ):
                raise RuntimeError("isolated prepared evidence is invalid")
            wallet.addresses.append(deposit_address)
            explorer.set_outputs(deposit_address, [deposit_amount])
            explorer.set_transaction(expected_txid, "mempool")
            explorer.set_transaction(decoy_txid, "mempool")
        else:
            expected_txid = None
            decoy_txid = None

        service = _service(
            db_path,
            explorer,
            wallet,
            clock_start=_clock_after_database(db_path),
        )
        recovered = service.reconcile_transfers()
        if wallet_state.get("prepared") is True:
            if len(recovered) != 1 or recovered[0].txid != expected_txid:
                raise RuntimeError("prepared recovery result is invalid")
            evidence = {
                "expected_txid": expected_txid,
                "decoy_txid": decoy_txid,
                "transaction_calls": explorer.transaction_calls,
            }
            _atomic_text(state_dir / "recovery.json", json.dumps(evidence, separators=(",", ":")))

        ready = {
            "generation": generation,
            "parent_pid": os.getpid(),
            "child_pid": child.pid,
            "recovery_ready": True,
            "accepting_orders": False,
        }
        _atomic_text(state_dir / "ready.json", json.dumps(ready, separators=(",", ":")))
        while not stopped:
            time.sleep(0.1)
    finally:
        child.terminate()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("prepare", "inject", "serve", "verify"))
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--wallet", type=Path)
    parser.add_argument("--state-dir", type=Path)
    arguments = parser.parse_args()
    if arguments.mode == "prepare":
        if arguments.wallet is None:
            parser.error("prepare requires --wallet")
        prepare(arguments.db, arguments.wallet)
    elif arguments.mode == "inject":
        if arguments.wallet is None:
            parser.error("inject requires --wallet")
        inject(arguments.db, arguments.wallet)
    elif arguments.mode == "verify":
        if arguments.state_dir is None:
            parser.error("verify requires --state-dir")
        verify(arguments.db, arguments.state_dir)
    else:
        if arguments.wallet is None or arguments.state_dir is None:
            parser.error("serve requires --wallet and --state-dir")
        serve(arguments.db, arguments.wallet, arguments.state_dir)


if __name__ == "__main__":
    main()
