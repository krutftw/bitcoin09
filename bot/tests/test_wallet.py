from __future__ import annotations

import copy
import ctypes
import hashlib
import json
import multiprocessing
import os
import shutil
import socket
import struct
import subprocess
import tempfile
import time
import unittest
from collections import deque
from dataclasses import dataclass, replace
from unittest import mock

from bot.otc.explorer import BlockStatus, Tip, TransactionStatus
from bot.otc.wallet import AllocationLockBusy, WalletAllocationLock


class FatalLockProbe(BaseException):
    pass


def _process_handle_count() -> int:
    if os.name == "nt":
        from ctypes import wintypes

        count = wintypes.DWORD()
        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.GetCurrentProcess.restype = wintypes.HANDLE
        kernel32.GetProcessHandleCount.argtypes = (
            wintypes.HANDLE,
            ctypes.POINTER(wintypes.DWORD),
        )
        kernel32.GetProcessHandleCount.restype = wintypes.BOOL
        if not kernel32.GetProcessHandleCount(
            kernel32.GetCurrentProcess(), ctypes.byref(count)
        ):
            raise ctypes.WinError(ctypes.get_last_error())
        return int(count.value)
    proc_fds = "/proc/self/fd"
    if os.path.isdir(proc_fds):
        return len(os.listdir(proc_fds))
    raise unittest.SkipTest("process handle counting is unavailable")


def _allocation_lock_process_worker(path, barrier, queue, timeout, hold):
    try:
        barrier.wait(timeout=10)
        try:
            with WalletAllocationLock(path, acquire_timeout=timeout):
                queue.put(("acquired", time.monotonic()))
                time.sleep(hold)
        except AllocationLockBusy:
            queue.put(("busy", time.monotonic()))
        except BaseException as exc:
            queue.put(("error", type(exc).__name__))
    finally:
        queue.close()
        queue.join_thread()


def _allocation_lock_baseexception_worker(path, connection, attempts):
    try:
        # Warm lazy platform imports before taking the isolated baseline.
        with WalletAllocationLock(path, acquire_timeout=0.1):
            pass
        os.remove(path)
        _process_handle_count()
        handles_before = _process_handle_count()
        exception_types = (KeyboardInterrupt, SystemExit, FatalLockProbe)
        original_try_lock = WalletAllocationLock._try_lock
        for attempt in range(attempts):
            expected = exception_types[attempt % len(exception_types)](
                f"setup interruption {attempt}"
            )

            def interrupt() -> float:
                raise expected

            lock = WalletAllocationLock(
                path,
                acquire_timeout=0.1,
                monotonic=interrupt,
            )
            try:
                lock.__enter__()
            except BaseException as caught:
                if caught is not expected:
                    raise AssertionError("setup exception identity changed")
            else:
                raise AssertionError("setup interruption was not raised")
            os.remove(path)
            with WalletAllocationLock(path, acquire_timeout=0.1):
                pass
            os.remove(path)

            expected = exception_types[attempt % len(exception_types)](
                f"post-lock interruption {attempt}"
            )

            def lock_then_interrupt(fd: int) -> None:
                original_try_lock(fd)
                raise expected

            lock = WalletAllocationLock(path, acquire_timeout=0.1)
            with mock.patch.object(
                WalletAllocationLock,
                "_try_lock",
                side_effect=lock_then_interrupt,
            ):
                try:
                    lock.__enter__()
                except BaseException as caught:
                    if caught is not expected:
                        raise AssertionError("post-lock exception identity changed")
                else:
                    raise AssertionError("post-lock interruption was not raised")
            os.remove(path)
            with WalletAllocationLock(path, acquire_timeout=0.1):
                pass
            os.remove(path)

        handles_after = _process_handle_count()
        if handles_after != handles_before:
            raise AssertionError(
                f"child handle count changed: {handles_before} -> {handles_after}"
            )
        connection.send(("ok", attempts))
    except BaseException as exc:
        try:
            connection.send(("error", type(exc).__name__))
        except (BrokenPipeError, EOFError, OSError):
            pass
    finally:
        connection.close()


def h(number: int) -> str:
    return f"{number:064x}"


_B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def address(number: int) -> str:
    payload = bytes([0x09]) + number.to_bytes(20, "big")
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


def compact(value: object) -> bytes:
    return json.dumps(value, separators=(",", ":")).encode("utf-8")


def txid_for(raw_hex: str) -> str:
    first = hashlib.sha256(bytes.fromhex(raw_hex)).digest()
    return hashlib.sha256(first).hexdigest()


def independent_snapshot_hash(
    network: str,
    tip: Tip,
    primary_address: str,
    addresses: list[str],
    outpoints: list[dict[str, object]],
) -> str:
    encoded = bytearray(b"btc09-wallet-snapshot-v2\0")
    network_bytes = network.encode("utf-8")
    encoded.extend(struct.pack(">H", len(network_bytes)))
    encoded.extend(network_bytes)
    encoded.extend(bytes.fromhex(tip.hash))
    encoded.extend(struct.pack(">Q", tip.height))
    primary_bytes = primary_address.encode("ascii")
    encoded.extend(struct.pack(">H", len(primary_bytes)))
    encoded.extend(primary_bytes)
    encoded.extend(struct.pack(">I", len(addresses)))
    indexes = {owner: index for index, owner in enumerate(addresses)}
    for owner in addresses:
        owner_bytes = owner.encode("ascii")
        encoded.extend(struct.pack(">H", len(owner_bytes)))
        encoded.extend(owner_bytes)
    encoded.extend(struct.pack(">I", len(outpoints)))
    for output in outpoints:
        txid, vout_text = str(output["outpoint"]).split(":")
        encoded.extend(bytes.fromhex(txid))
        encoded.extend(struct.pack(">I", int(vout_text)))
        encoded.extend(struct.pack(">Q", int(output["amount_units"])))
        encoded.extend(struct.pack(">I", indexes[str(output["address"])]))
    return hashlib.sha256(encoded).hexdigest()


def snapshot_payload(
    *,
    network: str = "btc09-regtest",
    tip: Tip | None = None,
    addresses: list[str] | None = None,
    primary_address: str | None = None,
    outpoints: list[dict[str, object]] | None = None,
) -> dict[str, object]:
    anchored_tip = tip or Tip(h(9), 70)
    owners = addresses or [address(1), address(2)]
    primary = primary_address or owners[0]
    outputs = outpoints or [
        {"outpoint": f"{h(20)}:0", "amount_units": 100, "address": owners[0]},
        {"outpoint": f"{h(21)}:2", "amount_units": 250, "address": owners[1]},
    ]
    return {
        "ok": True,
        "schema_version": 1,
        "network": network,
        "stage": "snapshot",
        "tip": {"hash": anchored_tip.hash, "height": anchored_tip.height},
        "primary_address": primary,
        "addresses": owners,
        "outpoints": outputs,
        "spendable_units": sum(int(item["amount_units"]) for item in outputs),
        "wallet_snapshot_hash": independent_snapshot_hash(
            network, anchored_tip, primary, owners, outputs
        ),
    }


@dataclass
class Call:
    argv: tuple[str, ...]
    stdin: bytes
    timeout: float


class ScriptedRunner:
    def __init__(self, *responses: object) -> None:
        self.responses = deque(responses)
        self.calls: list[Call] = []

    def __call__(
        self, argv: tuple[str, ...], stdin: bytes, timeout: float
    ) -> tuple[int, bytes, bytes]:
        self.calls.append(Call(argv, stdin, timeout))
        response = self.responses.popleft()
        if isinstance(response, BaseException):
            raise response
        if isinstance(response, tuple):
            return response
        return 0, compact(response), b""


class FakeExplorer:
    network = "btc09-regtest"

    def __init__(
        self,
        *,
        tips: list[object] | None = None,
        blocks: list[object] | None = None,
        transactions: list[object] | None = None,
    ) -> None:
        self.tips = deque(tips or [])
        self.blocks = deque(blocks or [])
        self.transactions = deque(transactions or [])
        self.block_queries: list[str] = []
        self.transaction_queries: list[str] = []

    @staticmethod
    def _next(values: deque[object]) -> object:
        value = values.popleft()
        if isinstance(value, BaseException):
            raise value
        return value

    def tip(self) -> Tip:
        return self._next(self.tips)  # type: ignore[return-value]

    def block(self, block_hash: str) -> BlockStatus:
        self.block_queries.append(block_hash)
        return self._next(self.blocks)  # type: ignore[return-value]

    def transaction(self, txid: str) -> TransactionStatus:
        self.transaction_queries.append(txid)
        return self._next(self.transactions)  # type: ignore[return-value]


class WalletTestCase(unittest.TestCase):
    def make_wallet(
        self, runner: ScriptedRunner, explorer: FakeExplorer | None = None
    ) -> object:
        from bot.otc.wallet import Wallet

        root = tempfile.mkdtemp(prefix="btc09-wallet-adapter-")
        self.addCleanup(lambda: __import__("shutil").rmtree(root, ignore_errors=True))
        return Wallet(
            binary_path=os.path.join(root, "btc09.exe"),
            wallet_path=os.path.join(root, "escrow.json"),
            data_dir=os.path.join(root, "chain"),
            network="btc09-regtest",
            seeds="127.0.0.1:19009",
            explorer=explorer or FakeExplorer(),
            command_timeout=1.0,
            observation_timeout=0.05,
            poll_interval=0.001,
            _runner=runner,
        )


class WalletDecimalTests(unittest.TestCase):
    def test_integer_units_are_formatted_without_binary_float(self) -> None:
        from bot.otc.wallet import format_units

        self.assertEqual(format_units(1), "0.00000001")
        self.assertEqual(format_units(29), "0.00000029")
        self.assertEqual(format_units(100_000_000), "1")
        self.assertEqual(format_units(123_456_789), "1.23456789")
        self.assertEqual(format_units(2_100_000_000_000_000), "21000000")

        for value in (-1, True, 2_100_000_000_000_001):
            with self.subTest(value=value), self.assertRaises(ValueError):
                format_units(value)  # type: ignore[arg-type]


class WalletSnapshotTests(WalletTestCase):
    def test_new_address_and_snapshot_use_only_exact_machine_contracts(self) -> None:
        from bot.otc.wallet import WalletSnapshot

        tip = Tip(h(9), 70)
        created = address(3)
        payload = snapshot_payload(tip=tip)
        runner = ScriptedRunner(
            {
                "ok": True,
                "schema_version": 1,
                "network": "btc09-regtest",
                "stage": "wallet_new",
                "address": created,
            },
            payload,
        )
        wallet = self.make_wallet(runner)

        self.assertEqual(wallet.new_address(), created)  # type: ignore[attr-defined]
        snapshot = wallet.snapshot(tip)  # type: ignore[attr-defined]

        self.assertIsInstance(snapshot, WalletSnapshot)
        self.assertEqual(snapshot.tip, tip)
        self.assertEqual(snapshot.spendable_units, 350)
        self.assertEqual(snapshot.primary_address, payload["primary_address"])
        self.assertEqual(snapshot.wallet_snapshot_hash, payload["wallet_snapshot_hash"])
        self.assertEqual(len(snapshot.addresses), 2)
        self.assertEqual(len(snapshot.outpoints), 2)
        self.assertEqual(
            runner.calls[0].argv[1:],
            (
                "wallet",
                "new",
                "-wallet-file",
                wallet.wallet_path,  # type: ignore[attr-defined]
                "-network",
                "btc09-regtest",
                "-json",
            ),
        )
        self.assertEqual(runner.calls[0].stdin, b"")
        self.assertEqual(runner.calls[1].argv[1:3], ("wallet", "snapshot"))
        self.assertIn("-expected-tip-hash", runner.calls[1].argv)
        self.assertIn(tip.hash, runner.calls[1].argv)
        self.assertNotIn("balance", runner.calls[1].argv)
        self.assertNotIn("list", runner.calls[1].argv)

    def test_snapshot_recomputes_hash_and_rejects_noncanonical_or_inconsistent_data(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        tip = Tip(h(9), 70)
        base = snapshot_payload(tip=tip)
        mutations: dict[str, object] = {}

        wrong_network = copy.deepcopy(base)
        wrong_network["network"] = "btc09-mainnet"
        mutations["wrong network"] = wrong_network

        wrong_tip = copy.deepcopy(base)
        wrong_tip["tip"] = {"hash": h(10), "height": 70}
        mutations["wrong tip"] = wrong_tip

        duplicate_address = copy.deepcopy(base)
        duplicate_address["addresses"] = [address(1), address(1)]
        mutations["duplicate address"] = duplicate_address

        unsorted_address = copy.deepcopy(base)
        unsorted_address["addresses"] = list(reversed(base["addresses"]))
        mutations["unsorted addresses"] = unsorted_address

        missing_primary = copy.deepcopy(base)
        del missing_primary["primary_address"]
        mutations["missing primary"] = missing_primary

        nonmember_primary = copy.deepcopy(base)
        nonmember_primary["primary_address"] = address(99)
        mutations["nonmember primary"] = nonmember_primary

        malformed_primary = copy.deepcopy(base)
        malformed_primary["primary_address"] = "not-an-address"
        mutations["malformed primary"] = malformed_primary

        duplicate_outpoint = copy.deepcopy(base)
        duplicate_outpoint["outpoints"][1]["outpoint"] = duplicate_outpoint["outpoints"][0]["outpoint"]
        mutations["duplicate outpoint"] = duplicate_outpoint

        unsorted_outpoint = copy.deepcopy(base)
        unsorted_outpoint["outpoints"] = list(reversed(base["outpoints"]))
        mutations["unsorted outpoints"] = unsorted_outpoint

        bad_sum = copy.deepcopy(base)
        bad_sum["spendable_units"] = 349
        mutations["bad sum"] = bad_sum

        overflowing_sum = copy.deepcopy(base)
        overflowing_sum["outpoints"][0]["amount_units"] = 2_100_000_000_000_000
        overflowing_sum["outpoints"][1]["amount_units"] = 1
        overflowing_sum["spendable_units"] = 2_100_000_000_000_001
        mutations["overflowing sum"] = overflowing_sum

        malformed_hash = copy.deepcopy(base)
        malformed_hash["wallet_snapshot_hash"] = "A" * 64
        mutations["malformed hash"] = malformed_hash

        bad_owner = copy.deepcopy(base)
        bad_owner["outpoints"][0]["address"] = address(99)
        mutations["unknown owner"] = bad_owner

        extra = copy.deepcopy(base)
        extra["secret"] = "must not be accepted"
        mutations["extra field"] = extra

        for label, payload in mutations.items():
            with self.subTest(label=label):
                wallet = self.make_wallet(ScriptedRunner(payload))
                with self.assertRaises(SafeSendFailure):
                    wallet.snapshot(tip)  # type: ignore[attr-defined]

    def test_snapshot_strict_json_rejects_duplicate_trailing_and_boolean_integer(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        tip = Tip(h(9), 70)
        base = compact(snapshot_payload(tip=tip))
        duplicate = base[:-1] + b',"network":"btc09-regtest"}'
        trailing = base + b"{}"
        bool_sum_payload = snapshot_payload(tip=tip)
        bool_sum_payload["spendable_units"] = True
        for label, raw in (
            ("duplicate", duplicate),
            ("trailing", trailing),
            ("boolean", compact(bool_sum_payload)),
            ("oversized", b"{" + b" " * (4 * 1024 * 1024)),
        ):
            with self.subTest(label=label):
                wallet = self.make_wallet(ScriptedRunner((0, raw, b"")))
                with self.assertRaises(SafeSendFailure):
                    wallet.snapshot(tip)  # type: ignore[attr-defined]

    def test_nonzero_machine_failure_is_safe_and_never_echoes_child_output(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        secret = "seed-secret-never-echo"
        failure = {
            "ok": False,
            "schema_version": 1,
            "network": "btc09-regtest",
            "stage": "wallet_new",
            "error_code": "safe_wallet_new_failure",
        }
        wallet = self.make_wallet(
            ScriptedRunner((1, compact(failure), secret.encode("ascii")))
        )
        with self.assertRaises(SafeSendFailure) as caught:
            wallet.new_address()  # type: ignore[attr-defined]
        self.assertNotIn(secret, str(caught.exception))


class WalletPrepareTests(WalletTestCase):
    def success_fixture(self) -> tuple[object, ScriptedRunner, Tip, object, str, str]:
        tip = Tip(h(9), 70)
        snapshot_json = snapshot_payload(tip=tip)
        signed_hex = "01020304"
        txid = txid_for(signed_hex)
        destination = address(50)
        selected = str(snapshot_json["outpoints"][1]["outpoint"])
        prepare_json = {
            "ok": True,
            "schema_version": 1,
            "network": "btc09-regtest",
            "stage": "prepared",
            "txid": txid,
            "signed_tx_hex": signed_hex,
            "destination": destination,
            "amount_units": 100,
            "fee_units": 10,
            "snapshot_tip": {"hash": tip.hash, "height": tip.height},
            "wallet_snapshot_hash": snapshot_json["wallet_snapshot_hash"],
            "selected_outpoints": [selected],
        }
        inspect_json = {
            "ok": True,
            "schema_version": 1,
            "network": "btc09-regtest",
            "stage": "inspected",
            "txid": txid,
            "inputs": [selected],
            "outputs": [
                {"index": 0, "address": destination, "amount_units": 100},
                {
                    "index": 1,
                    "address": snapshot_json["addresses"][0]
                    if snapshot_json["outpoints"][1]["address"]
                    == snapshot_json["addresses"][0]
                    else snapshot_json["addresses"][1],
                    "amount_units": 140,
                },
            ],
        }
        runner = ScriptedRunner(snapshot_json, prepare_json, inspect_json)
        explorer = FakeExplorer(tips=[tip, tip])
        wallet = self.make_wallet(runner, explorer)
        snapshot = wallet.snapshot(tip)  # type: ignore[attr-defined]
        return wallet, runner, tip, snapshot, destination, signed_hex

    def test_prepare_uses_common_tip_restricted_stdin_and_inspects_exact_bytes(self) -> None:
        from bot.otc.wallet import PreparedTransfer

        wallet, runner, tip, snapshot, destination, signed_hex = self.success_fixture()
        restricted = (snapshot.outpoints[0].outpoint,)  # type: ignore[attr-defined]

        prepared = wallet.prepare(  # type: ignore[attr-defined]
            destination,
            100,
            10,
            tip,
            restricted,
            snapshot,
        )

        self.assertIsInstance(prepared, PreparedTransfer)
        self.assertEqual(prepared.signed_tx_hex, signed_hex)
        self.assertEqual(prepared.txid, txid_for(signed_hex))
        self.assertNotIn(signed_hex, repr(prepared))
        self.assertEqual(prepared.selected_outpoints, (snapshot.outpoints[1].outpoint,))
        prepare_call = runner.calls[1]
        self.assertEqual(json.loads(prepare_call.stdin), list(restricted))
        self.assertIn("-amount", prepare_call.argv)
        self.assertEqual(
            prepare_call.argv[prepare_call.argv.index("-amount") + 1], "0.000001"
        )
        self.assertEqual(
            prepare_call.argv[prepare_call.argv.index("-fee") + 1], "0.0000001"
        )
        self.assertIn("-exclude-outpoints-json", prepare_call.argv)
        self.assertNotIn(signed_hex, prepare_call.argv)
        inspect_call = runner.calls[2]
        self.assertEqual(inspect_call.argv[1], "inspect-tx")
        self.assertEqual(inspect_call.stdin, signed_hex.encode("ascii"))
        self.assertNotIn(signed_hex, inspect_call.argv)

    def test_prepare_tip_races_and_snapshot_changes_are_safe_before_attach(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
        runner.calls.clear()
        wallet.explorer.tips = deque([Tip(h(10), 71)])  # type: ignore[attr-defined]
        with self.assertRaises(SafeSendFailure):
            wallet.prepare(destination, 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]
        self.assertEqual(runner.calls, [])


def block_status(
    prepared_tip: Tip,
    *,
    found: bool = True,
    canonical: bool = True,
    height: int | None = None,
    live_tip: Tip | None = None,
) -> BlockStatus:
    return BlockStatus(
        prepared_tip.hash,
        found,
        prepared_tip.height if height is None and found else height,
        canonical,
        live_tip or prepared_tip,
    )


def transaction_status(
    txid: str, status: str, tip: Tip, confirmations: int = 0
) -> TransactionStatus:
    from bot.otc.explorer import BlockAnchor

    block = None
    if status == "confirmed":
        block = BlockAnchor(tip.hash, tip.height)
        confirmations = 1
    return TransactionStatus(txid, status, block, confirmations, tip)


class WalletPrepareAndBroadcastTests(WalletTestCase):
    success_fixture = WalletPrepareTests.success_fixture

    def broadcast_fixture(
        self,
        *,
        transactions: list[object],
        blocks: list[object] | None = None,
        responses: tuple[object, ...] = (),
    ) -> tuple[object, ScriptedRunner, FakeExplorer, Tip, str, str]:
        tip = Tip(h(9), 70)
        signed_hex = "01020304"
        txid = txid_for(signed_hex)
        explorer = FakeExplorer(
            blocks=blocks
            or [block_status(tip), block_status(tip)],
            transactions=transactions,
        )
        runner = ScriptedRunner(*responses)
        wallet = self.make_wallet(runner, explorer)
        return wallet, runner, explorer, tip, signed_hex, txid

    def test_preobserved_mempool_or_confirmed_skips_submission(self) -> None:
        from bot.otc.wallet import BroadcastResult

        for status in ("mempool", "confirmed"):
            with self.subTest(status=status):
                tip = Tip(h(9), 70)
                raw_hex = "01020304"
                txid = txid_for(raw_hex)
                wallet, runner, explorer, tip, raw_hex, txid = self.broadcast_fixture(
                    transactions=[transaction_status(txid, status, tip)]
                )
                result = wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]
                self.assertIsInstance(result, BroadcastResult)
                self.assertEqual(result.status, status)
                self.assertFalse(result.submitted)
                self.assertEqual(result.peer_writes, 0)
                self.assertEqual(runner.calls, [])
                self.assertEqual(explorer.block_queries, [tip.hash, tip.hash])

    def test_unknown_broadcast_requires_peer_write_then_trusted_observation(self) -> None:
        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)
        response = {
            "ok": True,
            "schema_version": 1,
            "network": "btc09-regtest",
            "stage": "broadcast",
            "status": "submitted",
            "txid": txid,
            "peer_writes": 2,
        }
        wallet, runner, explorer, tip, raw_hex, txid = self.broadcast_fixture(
            transactions=[
                transaction_status(txid, "unknown", tip),
                transaction_status(txid, "unknown", tip),
                transaction_status(txid, "mempool", tip),
            ],
            responses=(response,),
        )

        result = wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]

        self.assertTrue(result.submitted)
        self.assertEqual(result.status, "mempool")
        self.assertEqual(result.peer_writes, 2)
        self.assertEqual(explorer.transaction_queries, [txid, txid, txid])
        call = runner.calls[0]
        self.assertEqual(call.stdin, raw_hex.encode("ascii"))
        self.assertNotIn(raw_hex, call.argv)
        self.assertIn("-require-broadcast=true", call.argv)
        self.assertIn("-expected-txid", call.argv)
        self.assertEqual(call.argv[call.argv.index("-expected-txid") + 1], txid)

    def test_canonical_loss_or_wrong_anchor_is_uncertain_before_submission(self) -> None:
        from bot.otc.explorer import ExplorerTransportError
        from bot.otc.wallet import UncertainSendFailure

        tip = Tip(h(9), 70)
        cases = (
            block_status(tip, found=False, canonical=False, height=None),
            block_status(tip, canonical=False),
            block_status(tip, height=69),
            ExplorerTransportError("secret transport detail"),
        )
        for status in cases:
            with self.subTest(status=type(status).__name__):
                wallet, runner, _, tip, raw_hex, txid = self.broadcast_fixture(
                    blocks=[status], transactions=[]
                )
                with self.assertRaises(UncertainSendFailure) as caught:
                    wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]
                self.assertEqual(runner.calls, [])
                self.assertNotIn("secret", str(caught.exception))

    def test_reorg_after_observation_is_uncertain_not_success(self) -> None:
        from bot.otc.wallet import UncertainSendFailure

        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)
        wallet, runner, _, tip, raw_hex, txid = self.broadcast_fixture(
            blocks=[
                block_status(tip),
                block_status(tip, canonical=False),
            ],
            transactions=[transaction_status(txid, "mempool", tip)],
        )
        with self.assertRaises(UncertainSendFailure):
            wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]
        self.assertEqual(runner.calls, [])


class WalletHardeningTests(WalletTestCase):
    def test_allocation_lock_is_distinct_hardened_and_releases_on_exception(self) -> None:
        runner = ScriptedRunner()
        wallet = self.make_wallet(runner)
        self.assertEqual(
            wallet.allocation_lock_path, wallet.wallet_path + ".allocation.lock"
        )
        self.assertNotEqual(wallet.allocation_lock_path, wallet.wallet_path + ".lock")
        with self.assertRaisesRegex(RuntimeError, "probe"):
            with wallet.allocation_lock(timeout=0.2):
                raise RuntimeError("probe")
        for _ in range(20):
            with wallet.allocation_lock(timeout=0.2):
                pass
        if os.name != "nt":
            self.assertEqual(
                os.stat(wallet.allocation_lock_path).st_mode & 0o777, 0o600
            )

    def test_allocation_lock_is_cross_process_exclusive_and_times_out_safely(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            path = os.path.join(root, "wallet.json.allocation.lock")
            context = multiprocessing.get_context("spawn")
            barrier = context.Barrier(2)
            queue = context.Queue()
            process = context.Process(
                target=_allocation_lock_process_worker,
                args=(path, barrier, queue, 0.15, 0.0),
            )
            started = False
            try:
                with WalletAllocationLock(path, acquire_timeout=1):
                    process.start()
                    started = True
                    barrier.wait(timeout=10)
                    result = queue.get(timeout=10)
                    process.join(10)
                self.assertFalse(process.is_alive(), "lock worker did not exit")
                self.assertEqual(result[0], "busy")
                self.assertEqual(process.exitcode, 0)
            finally:
                if started:
                    if process.is_alive():
                        process.terminate()
                        process.join(5)
                    if process.is_alive():
                        process.kill()
                        process.join(5)
                queue.close()
                queue.join_thread()
                process.close()
            with WalletAllocationLock(path, acquire_timeout=0.2):
                pass

    def test_allocation_lock_baseexception_cleanup_isolated_from_parent_handles(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            path = os.path.join(root, "wallet.json.allocation.lock")
            context = multiprocessing.get_context("spawn")
            parent_connection, child_connection = context.Pipe(duplex=False)
            process = context.Process(
                target=_allocation_lock_baseexception_worker,
                args=(path, child_connection, 60),
            )
            started = False
            try:
                process.start()
                started = True
                child_connection.close()
                if not parent_connection.poll(45):
                    self.fail("allocation lock cleanup child timed out")
                try:
                    result = parent_connection.recv()
                except EOFError:
                    result = ("error", "EOFError")
                process.join(10)
                self.assertFalse(process.is_alive(), "cleanup child did not exit")
                self.assertEqual(process.exitcode, 0)
                self.assertEqual(result, ("ok", 60))
            finally:
                parent_connection.close()
                child_connection.close()
                if started:
                    if process.is_alive():
                        process.terminate()
                        process.join(5)
                    if process.is_alive():
                        process.kill()
                        process.join(5)
                process.close()

    def test_allocation_lock_still_maps_ordinary_setup_exception_safely(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        with tempfile.TemporaryDirectory() as root:
            path = os.path.join(root, "wallet.json.allocation.lock")
            detail = RuntimeError("private setup detail")

            def fail_setup() -> float:
                raise detail

            lock = WalletAllocationLock(
                path,
                acquire_timeout=0.1,
                monotonic=fail_setup,
            )
            with self.assertRaises(SafeSendFailure) as caught:
                lock.__enter__()
            self.assertIsNot(caught.exception, detail)
            self.assertNotIn("private setup detail", str(caught.exception))
            os.remove(path)

    def test_allocation_lock_rejects_symlink_and_relative_paths(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaises(ValueError):
                WalletAllocationLock("relative.allocation.lock")
            target = os.path.join(root, "target")
            link = os.path.join(root, "wallet.allocation.lock")
            with open(target, "wb") as handle:
                handle.write(b"")
            try:
                os.symlink(target, link)
            except (OSError, NotImplementedError):
                self.skipTest("symlink creation is unavailable")
            with self.assertRaises(Exception):
                with WalletAllocationLock(link, acquire_timeout=0.1):
                    pass

    success_fixture = WalletPrepareTests.success_fixture
    broadcast_fixture = WalletPrepareAndBroadcastTests.broadcast_fixture

    def test_constructor_rejects_relative_paths_wrong_network_and_explorer_mismatch(self) -> None:
        from bot.otc.wallet import Wallet

        root = tempfile.mkdtemp(prefix="btc09-wallet-config-")
        self.addCleanup(lambda: __import__("shutil").rmtree(root, ignore_errors=True))
        valid = {
            "binary_path": os.path.join(root, "btc09.exe"),
            "wallet_path": os.path.join(root, "wallet.json"),
            "data_dir": os.path.join(root, "chain"),
            "network": "btc09-regtest",
            "seeds": "127.0.0.1:19009",
            "explorer": FakeExplorer(),
            "_runner": ScriptedRunner(),
        }
        for key, value in (
            ("binary_path", "btc09"),
            ("wallet_path", "wallet.json"),
            ("data_dir", "chain"),
            ("network", "regtest"),
            ("seeds", "127.0.0.1:1 secret"),
        ):
            with self.subTest(key=key), self.assertRaises(ValueError):
                Wallet(**(valid | {key: value}))
        mainnet_explorer = FakeExplorer()
        mainnet_explorer.network = "btc09-mainnet"
        with self.assertRaises(ValueError):
            Wallet(**(valid | {"explorer": mainnet_explorer}))

    def test_malformed_wallet_new_success_is_classified_safe_not_leaked_protocol_error(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        payload = {
            "ok": True,
            "schema_version": 1,
            "network": "btc09-regtest",
            "stage": "wallet_new",
            "address": address(3),
            "extra": "secret",
        }
        wallet = self.make_wallet(ScriptedRunner(payload))
        with self.assertRaises(SafeSendFailure):
            wallet.new_address()  # type: ignore[attr-defined]

    def test_prepare_revalidates_manually_constructed_snapshot_order_and_owner_index(self) -> None:
        from bot.otc.wallet import SafeSendFailure, WalletOutpoint, WalletSnapshot

        tip = Tip(h(9), 70)
        owners = (address(1), address(2))
        ordered = (
            WalletOutpoint(f"{h(20)}:0", h(20), 0, 100, owners[0], 0),
            WalletOutpoint(f"{h(21)}:0", h(21), 0, 200, owners[1], 1),
        )
        for outputs in (
            tuple(reversed(ordered)),
            (ordered[0], ordered[0]),
            (WalletOutpoint(f"{h(20)}:0", h(20), 0, 100, owners[1], -1),),
        ):
            forged = WalletSnapshot(
                "btc09-regtest",
                tip,
                owners[0],
                owners,
                outputs,
                sum(item.amount_units for item in outputs),
                h(1),
            )
            with self.subTest(outputs=outputs), self.assertRaises(SafeSendFailure):
                wallet = self.make_wallet(ScriptedRunner())
                wallet.prepare(address(50), 1, 0, tip, (), forged)  # type: ignore[attr-defined]

    def test_zero_write_unknown_and_missing_observation_are_uncertain(self) -> None:
        from bot.otc.wallet import UncertainSendFailure

        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)
        for writes in (0, True, -1):
            with self.subTest(writes=writes):
                response = {
                    "ok": True,
                    "schema_version": 1,
                    "network": "btc09-regtest",
                    "stage": "broadcast",
                    "status": "submitted",
                    "txid": txid,
                    "peer_writes": writes,
                }
                wallet, _, _, tip, raw_hex, txid = self.broadcast_fixture(
                    transactions=[transaction_status(txid, "unknown", tip)],
                    responses=(response,),
                )
                with self.assertRaises(UncertainSendFailure):
                    wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]

        response["peer_writes"] = 1
        unknowns = [transaction_status(txid, "unknown", tip) for _ in range(200)]
        wallet, _, _, tip, raw_hex, txid = self.broadcast_fixture(
            transactions=unknowns,
            responses=(response,),
        )
        with self.assertRaises(UncertainSendFailure):
            wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]

    def test_any_invoked_broadcast_crash_timeout_or_malformed_result_is_uncertain_and_redacted(self) -> None:
        from bot.otc.wallet import UncertainSendFailure

        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)
        malformed_cases: tuple[object, ...] = (
            TimeoutError(f"signed {raw_hex}"),
            OSError("seed-secret"),
            (1, b"", b"seed-secret"),
            (0, b"{}{}", b""),
            (
                1,
                compact(
                    {
                        "ok": False,
                        "schema_version": 1,
                        "network": "btc09-regtest",
                        "stage": "broadcast",
                        "error_code": "safe_broadcast_failure",
                    }
                ),
                b"",
            ),
        )
        for child_result in malformed_cases:
            with self.subTest(child=type(child_result).__name__):
                wallet, runner, _, tip, raw_hex, txid = self.broadcast_fixture(
                    transactions=[transaction_status(txid, "unknown", tip)],
                    responses=(child_result,),
                )
                with self.assertRaises(UncertainSendFailure) as caught:
                    wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]
                self.assertEqual(len(runner.calls), 1)
                self.assertNotIn(raw_hex, str(caught.exception))
                self.assertNotIn("seed-secret", str(caught.exception))

    def test_broadcast_rejects_corrupt_stored_identity_before_network_or_child(self) -> None:
        from bot.otc.wallet import UncertainSendFailure

        wallet, runner, explorer, tip, _, txid = self.broadcast_fixture(
            transactions=[]
        )
        for raw_hex, expected in (
            ("A0", txid),
            ("0", txid),
            ("00", txid),
            ("00" * 10_001, txid),
            ("01020304", h(999)),
        ):
            with self.subTest(length=len(raw_hex)):
                with self.assertRaises(UncertainSendFailure):
                    wallet.broadcast(raw_hex, expected, tip)  # type: ignore[attr-defined]
        self.assertEqual(runner.calls, [])
        self.assertEqual(explorer.block_queries, [])

    def test_forged_boolean_spendable_snapshot_fails_before_tip_or_child(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        tip = Tip(h(9), 70)
        owner = address(1)
        payload = snapshot_payload(
            tip=tip,
            addresses=[owner],
            outpoints=[
                {"outpoint": f"{h(20)}:0", "amount_units": 1, "address": owner}
            ],
        )
        runner = ScriptedRunner(payload)
        explorer = FakeExplorer(tips=[tip])
        wallet = self.make_wallet(runner, explorer)
        snapshot = wallet.snapshot(tip)  # type: ignore[attr-defined]
        forged = replace(snapshot, spendable_units=True)
        runner.calls.clear()
        with self.assertRaises(SafeSendFailure):
            wallet.prepare(address(50), 1, 0, tip, (), forged)  # type: ignore[attr-defined]
        self.assertEqual(runner.calls, [])
        self.assertEqual(len(explorer.tips), 1)

    def test_broadcast_peer_write_count_accepts_u32_cap_only(self) -> None:
        from bot.otc.wallet import UncertainSendFailure

        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)

        def response(writes: object) -> dict[str, object]:
            return {
                "ok": True,
                "schema_version": 1,
                "network": "btc09-regtest",
                "stage": "broadcast",
                "status": "submitted",
                "txid": txid,
                "peer_writes": writes,
            }

        wallet, _, _, tip, raw_hex, txid = self.broadcast_fixture(
            transactions=[
                transaction_status(txid, "unknown", tip),
                transaction_status(txid, "mempool", tip),
            ],
            responses=(response((1 << 32) - 1),),
        )
        self.assertEqual(
            wallet.broadcast(raw_hex, txid, tip).peer_writes,  # type: ignore[attr-defined]
            (1 << 32) - 1,
        )

        for invalid in ((1 << 32), True):
            with self.subTest(invalid=invalid):
                wallet, _, _, tip, raw_hex, txid = self.broadcast_fixture(
                    transactions=[
                        transaction_status(txid, "unknown", tip),
                        transaction_status(txid, "mempool", tip),
                    ],
                    responses=(response(invalid),),
                )
                with self.assertRaises(UncertainSendFailure):
                    wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]


class WalletProcessBackedTests(unittest.TestCase):
    _HELPER_SOURCE = r'''
package main

import (
    "bytes"
    "crypto/sha256"
    "fmt"
    "io"
    "os"
    "strings"
    "time"
)

func main() {
    modeBytes, _ := os.ReadFile(os.Args[0] + ".mode")
    mode := strings.TrimSpace(string(modeBytes))
    marker := os.Args[0] + ".marker"
    switch mode {
    case "oversize":
        _, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), (4 << 20) + 1))
    case "secret_failure":
        _, _ = fmt.Fprint(os.Stdout, "child-secret-signed-hex")
        os.Exit(1)
    case "hang_after_sign":
        _ = os.WriteFile(marker, []byte("signed-and-withheld\n"), 0600)
        time.Sleep(30 * time.Second)
    case "hang_after_local_submit", "hang_after_first_peer_write":
        raw, _ := io.ReadAll(os.Stdin)
        digest := sha256.Sum256(raw)
        expected := ""
        for i := 1; i+1 < len(os.Args); i++ {
            if os.Args[i] == "-expected-txid" { expected = os.Args[i+1] }
        }
        file, _ := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
        _, _ = fmt.Fprintf(file, "%s:%s:%x\n", mode, expected, digest)
        _ = file.Sync()
        _ = file.Close()
        time.Sleep(30 * time.Second)
    default:
        os.Exit(2)
    }
}
'''

    @classmethod
    def setUpClass(cls) -> None:
        go = shutil.which("go")
        if go is None:
            raise unittest.SkipTest("Go toolchain is unavailable for process harness")
        cls.root = tempfile.mkdtemp(prefix="btc09-wallet-process-")
        cls.helper = os.path.join(cls.root, "wallet-child.exe" if os.name == "nt" else "wallet-child")
        source = os.path.join(cls.root, "main.go")
        with open(source, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(cls._HELPER_SOURCE)
        subprocess.run(
            [go, "build", "-o", cls.helper, source],
            check=True,
            cwd=cls.root,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            timeout=60,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.root, ignore_errors=True)

    def set_mode(self, mode: str) -> str:
        marker = self.helper + ".marker"
        try:
            os.remove(marker)
        except FileNotFoundError:
            pass
        with open(self.helper + ".mode", "w", encoding="ascii") as handle:
            handle.write(mode)
        return marker

    def process_wallet(self, explorer: FakeExplorer) -> object:
        from bot.otc.wallet import Wallet

        return Wallet(
            binary_path=self.helper,
            wallet_path=os.path.join(self.root, "escrow.json"),
            data_dir=os.path.join(self.root, "chain"),
            network="btc09-regtest",
            seeds="127.0.0.1:19009",
            explorer=explorer,
            command_timeout=0.2,
            observation_timeout=0.2,
            poll_interval=0.01,
        )

    def parsed_snapshot(self, tip: Tip) -> object:
        runner = ScriptedRunner(snapshot_payload(tip=tip))
        wallet = WalletTestCase.make_wallet(self, runner, FakeExplorer())
        return wallet.snapshot(tip)  # type: ignore[attr-defined]

    def test_default_runner_bounds_real_pipe_output_and_redacts_child_secrets(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        for mode in ("oversize", "secret_failure"):
            with self.subTest(mode=mode):
                self.set_mode(mode)
                wallet = self.process_wallet(FakeExplorer())
                started = time.monotonic()
                with self.assertRaises(SafeSendFailure) as caught:
                    wallet.new_address()  # type: ignore[attr-defined]
                self.assertLess(time.monotonic() - started, 5.0)
                self.assertNotIn("child-secret", str(caught.exception))

    def test_unexpected_wait_exception_kills_and_reaps_real_child(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        self.set_mode("hang_after_sign")
        real_popen = subprocess.Popen
        wrappers: list[object] = []

        class ExplodingWait:
            def __init__(self, process: subprocess.Popen[bytes]) -> None:
                self.process = process
                self.stdin = process.stdin
                self.stdout = process.stdout
                self.stderr = process.stderr
                self.exploded = False

            def wait(self, *args: object, **kwargs: object) -> int:
                if not self.exploded:
                    self.exploded = True
                    raise RuntimeError("unexpected-wait-secret")
                return self.process.wait(*args, **kwargs)

            def kill(self) -> None:
                self.process.kill()

            def poll(self) -> int | None:
                return self.process.poll()

        def launch(*args: object, **kwargs: object) -> ExplodingWait:
            wrapper = ExplodingWait(real_popen(*args, **kwargs))
            wrappers.append(wrapper)
            return wrapper

        wallet = self.process_wallet(FakeExplorer())
        with mock.patch("bot.otc.wallet.subprocess.Popen", side_effect=launch):
            with self.assertRaises(SafeSendFailure) as caught:
                wallet.new_address()  # type: ignore[attr-defined]
        wrapper = wrappers[0]
        remained = wrapper.poll() is None  # type: ignore[attr-defined]
        if remained:
            wrapper.kill()  # type: ignore[attr-defined]
            wrapper.wait(timeout=5)  # type: ignore[attr-defined]
        self.assertFalse(remained)
        self.assertNotIn("unexpected-wait-secret", str(caught.exception))

    def test_wait_control_exceptions_are_reraised_unchanged_after_reap(self) -> None:
        self.set_mode("hang_after_sign")
        real_popen = subprocess.Popen
        for control in (KeyboardInterrupt(), SystemExit(7)):
            with self.subTest(control=type(control).__name__):
                wrappers: list[object] = []

                class ControlWait:
                    def __init__(self, process: subprocess.Popen[bytes]) -> None:
                        self.process = process
                        self.stdin = process.stdin
                        self.stdout = process.stdout
                        self.stderr = process.stderr
                        self.raised = False

                    def wait(self, *args: object, **kwargs: object) -> int:
                        if not self.raised:
                            self.raised = True
                            raise control
                        return self.process.wait(*args, **kwargs)

                    def kill(self) -> None:
                        self.process.kill()

                    def poll(self) -> int | None:
                        return self.process.poll()

                def launch(*args: object, **kwargs: object) -> ControlWait:
                    wrapper = ControlWait(real_popen(*args, **kwargs))
                    wrappers.append(wrapper)
                    return wrapper

                wallet = self.process_wallet(FakeExplorer())
                with mock.patch("bot.otc.wallet.subprocess.Popen", side_effect=launch):
                    with self.assertRaises(type(control)) as caught:
                        wallet.new_address()  # type: ignore[attr-defined]
                self.assertIs(caught.exception, control)
                self.assertIsNotNone(wrappers[0].poll())  # type: ignore[attr-defined]

    def test_unexpected_reader_exception_kills_and_reaps_real_child(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        self.set_mode("hang_after_sign")
        real_popen = subprocess.Popen
        wrappers: list[object] = []

        class ExplodingReader:
            def __init__(self, stream: object) -> None:
                self.stream = stream

            def read(self, _size: int) -> bytes:
                raise OSError("unexpected-reader-secret")

            def close(self) -> None:
                self.stream.close()  # type: ignore[attr-defined]

        class ReaderFailure:
            def __init__(self, process: subprocess.Popen[bytes]) -> None:
                self.process = process
                self.stdin = process.stdin
                self.stdout = ExplodingReader(process.stdout)
                self.stderr = process.stderr

            def wait(self, *args: object, **kwargs: object) -> int:
                return self.process.wait(*args, **kwargs)

            def kill(self) -> None:
                self.process.kill()

            def poll(self) -> int | None:
                return self.process.poll()

        def launch(*args: object, **kwargs: object) -> ReaderFailure:
            wrapper = ReaderFailure(real_popen(*args, **kwargs))
            wrappers.append(wrapper)
            return wrapper

        wallet = self.process_wallet(FakeExplorer())
        with mock.patch("bot.otc.wallet.subprocess.Popen", side_effect=launch):
            with self.assertRaises(SafeSendFailure) as caught:
                wallet.new_address()  # type: ignore[attr-defined]
        self.assertIsNotNone(wrappers[0].poll())  # type: ignore[attr-defined]
        self.assertNotIn("unexpected-reader-secret", str(caught.exception))

    def test_real_child_is_killed_after_signing_before_parent_receipt(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        marker = self.set_mode("hang_after_sign")
        tip = Tip(h(9), 70)
        snapshot = self.parsed_snapshot(tip)
        wallet = self.process_wallet(FakeExplorer(tips=[tip]))
        with self.assertRaises(SafeSendFailure):
            wallet.prepare(address(50), 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]
        with open(marker, encoding="ascii") as handle:
            self.assertEqual(handle.read(), "signed-and-withheld\n")

    def test_real_child_post_submit_and_first_peer_ambiguity_reuse_stored_identity(self) -> None:
        from bot.otc.wallet import UncertainSendFailure

        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)
        for phase in ("hang_after_local_submit", "hang_after_first_peer_write"):
            with self.subTest(phase=phase):
                marker = self.set_mode(phase)
                explorer = FakeExplorer(
                    blocks=[block_status(tip), block_status(tip)],
                    transactions=[
                        transaction_status(txid, "unknown", tip),
                        transaction_status(txid, "unknown", tip),
                    ],
                )
                wallet = self.process_wallet(explorer)
                for _ in range(2):
                    with self.assertRaises(UncertainSendFailure):
                        wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]
                with open(marker, encoding="ascii") as handle:
                    lines = handle.read().splitlines()
                self.assertEqual(len(lines), 2)
                self.assertEqual(lines[0], lines[1])
                self.assertTrue(lines[0].startswith(phase + ":" + txid + ":"))

    def test_shared_go_python_snapshot_fixed_vector(self) -> None:
        from bot.otc.wallet import _snapshot_hash_preimage_fields

        tip = Tip("".join(f"{value:02x}" for value in range(32)), 42)
        txid = "11" * 32
        preimage = _snapshot_hash_preimage_fields(
            "btc09-regtest",
            tip,
            "BC",
            ("A", "BC"),
            ((txid, 2, 3, 0), (txid, 10, 4, 1)),
        )
        self.assertEqual(len(preimage), 195)
        self.assertEqual(
            hashlib.sha256(preimage).hexdigest(),
            "2516412c8103507728b482f528c3eeef30d2c2fb1c5623071acfaee583d820bb",
        )
        swapped = _snapshot_hash_preimage_fields(
            "btc09-regtest",
            tip,
            "A",
            ("A", "BC"),
            ((txid, 2, 3, 0), (txid, 10, 4, 1)),
        )
        self.assertNotEqual(hashlib.sha256(preimage).digest(), hashlib.sha256(swapped).digest())


class WalletRegtestBinaryIntegrationTests(unittest.TestCase):
    make_wallet = WalletTestCase.make_wallet
    success_fixture = WalletPrepareTests.success_fixture
    broadcast_fixture = WalletPrepareAndBroadcastTests.broadcast_fixture

    @staticmethod
    def free_port() -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
            listener.bind(("127.0.0.1", 0))
            return int(listener.getsockname()[1])

    def distinct_ports(self) -> tuple[int, int]:
        for _ in range(10):
            p2p_port = self.free_port()
            explorer_port = self.free_port()
            if p2p_port != explorer_port:
                return p2p_port, explorer_port
        self.fail("could not allocate distinct loopback ports")

    def test_distinct_port_selection_retries_collisions(self) -> None:
        values = iter((19009, 19009, 19009, 19010))
        with mock.patch.object(self, "free_port", side_effect=lambda: next(values)):
            self.assertEqual(self.distinct_ports(), (19009, 19010))

    def test_node_startup_retries_early_bind_failure_with_cleanup(self) -> None:
        class FakeProcess:
            def __init__(self, returncode: int | None) -> None:
                self.returncode = returncode
                self.terminated = False

            def poll(self) -> int | None:
                return self.returncode

            def terminate(self) -> None:
                self.terminated = True
                self.returncode = 0

            def kill(self) -> None:
                self.terminated = True
                self.returncode = -1

            def wait(self, timeout: float | None = None) -> int:
                del timeout
                return self.returncode or 0

        failed = FakeProcess(1)
        live = FakeProcess(None)
        fake_explorer = mock.Mock()
        fake_explorer.tip.return_value = Tip(h(1), 0)
        with (
            mock.patch(
                "bot.tests.test_wallet.subprocess.Popen",
                side_effect=(failed, live),
            ) as launch,
            mock.patch("bot.otc.explorer.Explorer", return_value=fake_explorer),
        ):
            process, explorer = self.start_node("data", "wallet", mine=False)
        self.assertIs(process, live)
        self.assertIs(explorer, fake_explorer)
        self.assertEqual(launch.call_count, 2)
        for call in launch.call_args_list:
            args = call.args[0]
            p2p = args[args.index("-listen") + 1]
            http = args[args.index("-explorer") + 1]
            self.assertNotEqual(p2p, http)

    @classmethod
    def setUpClass(cls) -> None:
        go = shutil.which("go")
        if go is None:
            raise unittest.SkipTest("Go toolchain is unavailable for btc09 integration")
        cls.root = tempfile.mkdtemp(prefix="btc09-wallet-regtest-")
        cls.binary = os.path.join(cls.root, "btc09.exe" if os.name == "nt" else "btc09")
        subprocess.run(
            [go, "build", "-o", cls.binary, "./cmd/btc09"],
            check=True,
            cwd=os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..")),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            timeout=90,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.root, ignore_errors=True)

    def start_node(self, datadir: str, wallet_path: str, *, mine: bool) -> tuple[subprocess.Popen[bytes], object]:
        from bot.otc.explorer import Explorer, ExplorerError

        last_error: BaseException | None = None
        for _ in range(5):
            p2p_port, explorer_port = self.distinct_ports()
            args = [
                self.binary,
                "node",
                "-network",
                "regtest",
                "-datadir",
                datadir,
                "-wallet-file",
                wallet_path,
                "-listen",
                f"127.0.0.1:{p2p_port}",
                "-explorer",
                f"127.0.0.1:{explorer_port}",
                "-no-update-check",
            ]
            if mine:
                args.extend(("-mine", "-workers", "1"))
            process = subprocess.Popen(
                args,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            explorer = Explorer(
                f"http://127.0.0.1:{explorer_port}",
                network="btc09-regtest",
                connect_timeout=0.2,
                read_timeout=0.5,
                total_timeout=0.8,
            )
            startup_deadline = time.monotonic() + 2.0
            while time.monotonic() < startup_deadline:
                if process.poll() is not None:
                    last_error = RuntimeError("btc09 node exited during startup")
                    break
                try:
                    explorer.tip()
                    return process, explorer
                except ExplorerError as exc:
                    last_error = exc
                    time.sleep(0.05)
            self.stop_node(process)
        self.fail(f"btc09 node failed bounded startup retries: {type(last_error).__name__}")

    def wait_tip(self, process: subprocess.Popen[bytes], explorer: object, minimum: int) -> Tip:
        deadline = time.monotonic() + 30
        last_error: BaseException | None = None
        while time.monotonic() < deadline:
            if process.poll() is not None:
                self.fail(f"btc09 node exited early with code {process.returncode}")
            try:
                tip = explorer.tip()  # type: ignore[attr-defined]
                if tip.height >= minimum:
                    return tip
            except BaseException as exc:
                last_error = exc
            time.sleep(0.05)
        self.fail(f"btc09 regtest tip did not reach {minimum}: {type(last_error).__name__}")

    @staticmethod
    def stop_node(process: subprocess.Popen[bytes]) -> None:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)

    def test_actual_btc09_regtest_new_fund_snapshot_prepare_and_inspect(self) -> None:
        from bot.otc.explorer import Explorer
        from bot.otc.wallet import Wallet

        case = tempfile.mkdtemp(prefix="case-", dir=self.root)
        datadir = os.path.join(case, "chain")
        wallet_path = os.path.join(case, "escrow.json")
        destination_path = os.path.join(case, "destination.json")
        placeholder_port = self.free_port()
        placeholder = Explorer(
            f"http://127.0.0.1:{placeholder_port}", network="btc09-regtest"
        )
        escrow = Wallet(
            binary_path=self.binary,
            wallet_path=wallet_path,
            data_dir=datadir,
            network="btc09-regtest",
            seeds="127.0.0.1:1",
            explorer=placeholder,
            command_timeout=10,
        )
        self.assertTrue(escrow.new_address())
        destination_wallet = Wallet(
            binary_path=self.binary,
            wallet_path=destination_path,
            data_dir=datadir,
            network="btc09-regtest",
            seeds="127.0.0.1:1",
            explorer=placeholder,
            command_timeout=10,
        )
        destination = destination_wallet.new_address()

        miner, mining_explorer = self.start_node(datadir, wallet_path, mine=True)
        try:
            self.wait_tip(miner, mining_explorer, 5)
        finally:
            self.stop_node(miner)

        node, live_explorer = self.start_node(datadir, wallet_path, mine=False)
        try:
            tip = self.wait_tip(node, live_explorer, 5)
            live_wallet = Wallet(
                binary_path=self.binary,
                wallet_path=wallet_path,
                data_dir=datadir,
                network="btc09-regtest",
                seeds="127.0.0.1:1",
                explorer=live_explorer,
                command_timeout=10,
            )
            snapshot = live_wallet.snapshot(tip)
            self.assertEqual(snapshot.network, "btc09-regtest")
            self.assertGreater(snapshot.spendable_units, 100_000_000)
            prepared = live_wallet.prepare(destination, 100_000_000, 0, tip, (), snapshot)
            self.assertEqual(prepared.snapshot_tip, tip)
            self.assertEqual(prepared.wallet_snapshot_hash, snapshot.wallet_snapshot_hash)
            self.assertEqual(prepared.txid, txid_for(prepared.signed_tx_hex))
        finally:
            self.stop_node(node)


class WalletAdditionalBoundaryTests(WalletTestCase):
    success_fixture = WalletPrepareTests.success_fixture
    broadcast_fixture = WalletPrepareAndBroadcastTests.broadcast_fixture

    def test_trusted_transaction_transport_or_identity_failure_is_uncertain(self) -> None:
        from bot.otc.explorer import ExplorerProtocolError, ExplorerTransportError
        from bot.otc.wallet import SafeSendFailure, UncertainSendFailure

        tip = Tip(h(9), 70)
        raw_hex = "01020304"
        txid = txid_for(raw_hex)
        failures = (
            ExplorerTransportError("timeout"),
            ExplorerProtocolError("malformed"),
            transaction_status(h(888), "unknown", tip),
            transaction_status(txid, "invalid", tip),
        )
        for failure in failures:
            with self.subTest(failure=type(failure).__name__):
                wallet, runner, _, tip, raw_hex, txid = self.broadcast_fixture(
                    transactions=[failure]
                )
                with self.assertRaises(UncertainSendFailure):
                    wallet.broadcast(raw_hex, txid, tip)  # type: ignore[attr-defined]
                self.assertEqual(runner.calls, [])

        wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
        runner.responses[0]["wallet_snapshot_hash"] = h(999)  # type: ignore[index]
        with self.assertRaises(SafeSendFailure):
            wallet.prepare(destination, 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]
        self.assertEqual(len(runner.calls), 2)

        wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
        wallet.explorer.tips = deque([tip, Tip(h(11), 71)])  # type: ignore[attr-defined]
        with self.assertRaises(SafeSendFailure):
            wallet.prepare(destination, 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]
        self.assertEqual(len(runner.calls), 3)

    def test_prepare_rejects_selected_restricted_or_unanchored_input_as_hard_invariant(self) -> None:
        from bot.otc.wallet import WalletInvariantError

        wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
        restricted = (snapshot.outpoints[1].outpoint,)  # type: ignore[attr-defined]
        with self.assertRaises(WalletInvariantError):
            wallet.prepare(destination, 100, 10, tip, restricted, snapshot)  # type: ignore[attr-defined]
        self.assertEqual(len(runner.calls), 2)

        wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
        runner.responses[0]["selected_outpoints"] = [f"{h(1234)}:0"]  # type: ignore[index]
        with self.assertRaises(WalletInvariantError):
            wallet.prepare(destination, 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]
        self.assertEqual(len(runner.calls), 2)

    def test_prepare_rejects_inspection_identity_inputs_outputs_or_change(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        def mutate(case: str, payload: dict[str, object]) -> None:
            if case == "txid":
                payload["txid"] = h(777)
            elif case == "input":
                payload["inputs"] = [f"{h(888)}:0"]
            elif case == "destination":
                payload["outputs"][0]["address"] = address(88)  # type: ignore[index]
            elif case == "amount":
                payload["outputs"][0]["amount_units"] = 99  # type: ignore[index]
            elif case == "change owner":
                payload["outputs"][1]["address"] = address(89)  # type: ignore[index]
            elif case == "change amount":
                payload["outputs"][1]["amount_units"] = 139  # type: ignore[index]
            else:
                payload["outputs"].append(  # type: ignore[union-attr]
                    {"index": 2, "address": address(90), "amount_units": 1}
                )

        for case in (
            "txid",
            "input",
            "destination",
            "amount",
            "change owner",
            "change amount",
            "extra output",
        ):
            with self.subTest(case=case):
                wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
                inspect = runner.responses[1]
                self.assertIsInstance(inspect, dict)
                mutate(case, inspect)
                with self.assertRaises(SafeSendFailure):
                    wallet.prepare(destination, 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]

    def test_prepare_crash_timeout_and_unstructured_failure_are_safe_and_redacted(self) -> None:
        from bot.otc.wallet import SafeSendFailure

        for failure in (
            TimeoutError("contains 01020304 secret"),
            OSError("contains seed-secret"),
            (1, b"", b"signed 01020304"),
        ):
            with self.subTest(failure=type(failure).__name__):
                tip = Tip(h(9), 70)
                payload = snapshot_payload(tip=tip)
                runner = ScriptedRunner(payload, failure)
                wallet = self.make_wallet(runner, FakeExplorer(tips=[tip]))
                snapshot = wallet.snapshot(tip)  # type: ignore[attr-defined]
                with self.assertRaises(SafeSendFailure) as caught:
                    wallet.prepare(address(50), 100, 10, tip, (), snapshot)  # type: ignore[attr-defined]
                self.assertNotIn("01020304", str(caught.exception))
                self.assertNotIn("seed-secret", str(caught.exception))

    def test_prepare_restricted_input_is_strict_sorted_bounded_and_anchored(self) -> None:
        from bot.otc.wallet import WalletInvariantError

        wallet, runner, tip, snapshot, destination, _ = self.success_fixture()
        for restricted in (
            (snapshot.outpoints[1].outpoint, snapshot.outpoints[0].outpoint),  # type: ignore[attr-defined]
            (snapshot.outpoints[0].outpoint, snapshot.outpoints[0].outpoint),  # type: ignore[attr-defined]
            (f"{h(999)}:0",),
            tuple(f"{index + 1:064x}:0" for index in range(4_097)),
        ):
            with self.subTest(count=len(restricted)):
                runner.calls.clear()
                with self.assertRaises(WalletInvariantError):
                    wallet.prepare(destination, 100, 10, tip, restricted, snapshot)  # type: ignore[attr-defined]
                self.assertEqual(runner.calls, [])

    def test_prepare_rejects_invalid_money_before_any_child_or_network_call(self) -> None:
        runner = ScriptedRunner()
        wallet = self.make_wallet(runner)
        tip = Tip(h(9), 70)
        for amount, fee in (
            (0, 0),
            (True, 0),
            (1, -1),
            (2_100_000_000_000_000, 1),
        ):
            with self.subTest(amount=amount, fee=fee), self.assertRaises(ValueError):
                wallet.prepare(address(50), amount, fee, tip, (), object())  # type: ignore[attr-defined]
        self.assertEqual(runner.calls, [])


if __name__ == "__main__":
    unittest.main()
