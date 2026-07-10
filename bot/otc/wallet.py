from __future__ import annotations

import hashlib
import json
import math
import os
import re
import struct
import subprocess
import stat
import threading
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from bot.otc.domain import MAX_09C_UNITS, UNITS_PER_09C
from bot.otc.explorer import (
    BlockStatus,
    Explorer,
    ExplorerError,
    ExplorerProtocolError,
    Tip,
    TransactionStatus,
    _address_text as _explorer_address_text,
)


SCHEMA_VERSION = 1
MAINNET = "btc09-mainnet"
REGTEST = "btc09-regtest"
MAX_MACHINE_JSON_BYTES = 4 << 20
MAX_SIGNED_TX_HEX_CHARS = 20_000
MAX_RESTRICTED_OUTPOINTS = 4_096
MAX_WALLET_ADDRESSES = 10_000
MAX_ERROR_BYTES = 128
MAX_U32 = (1 << 32) - 1
MAX_U64 = (1 << 64) - 1

_HASH_RE = re.compile(r"[0-9a-f]{64}\Z", re.ASCII)
_SIGNED_HEX_RE = re.compile(r"(?:[0-9a-f][0-9a-f])+\Z", re.ASCII)
_OUTPOINT_RE = re.compile(
    r"(?P<txid>[0-9a-f]{64}):(?P<vout>0|[1-9][0-9]{0,9})\Z", re.ASCII
)
_ERROR_CODE_RE = re.compile(r"[a-z0-9_]{1,128}\Z", re.ASCII)


class WalletError(RuntimeError):
    """Base class for the fail-closed wallet adapter."""


class SafeSendFailure(WalletError):
    """No network submission occurred, so the reserved operation may retry."""


class UncertainSendFailure(WalletError):
    """A submission may have occurred and only the stored bytes may be retried."""


class WalletInvariantError(WalletError):
    """Trusted state contradicted a hard custody invariant."""


class AllocationLockBusy(SafeSendFailure):
    """Another process owns the wallet allocation recovery sequence."""


class WalletAllocationLock:
    """Crash-releasing advisory lock distinct from the Go wallet command lock."""

    def __init__(
        self,
        path: str | os.PathLike[str],
        *,
        acquire_timeout: float = 5.0,
        poll_interval: float = 0.05,
        monotonic: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        resolved = os.path.abspath(os.fspath(path))
        if resolved != os.fspath(path) or not os.path.isabs(resolved):
            raise ValueError("allocation lock path must be absolute")
        if (
            type(acquire_timeout) not in {int, float}
            or not math.isfinite(acquire_timeout)
            or not 0.05 <= acquire_timeout <= 60
            or type(poll_interval) not in {int, float}
            or not math.isfinite(poll_interval)
            or not 0.001 <= poll_interval <= min(1.0, acquire_timeout)
        ):
            raise ValueError("allocation lock timing is invalid")
        self.path = resolved
        self.acquire_timeout = float(acquire_timeout)
        self.poll_interval = float(poll_interval)
        self._monotonic = monotonic
        self._sleep = sleep
        self._fd: int | None = None

    def __enter__(self) -> "WalletAllocationLock":
        parent = os.path.dirname(self.path)
        if not os.path.isdir(parent):
            raise SafeSendFailure("wallet allocation lock failed safely")
        if os.name != "nt":
            parent_mode = stat.S_IMODE(os.stat(parent, follow_symlinks=False).st_mode)
            if parent_mode & 0o022:
                raise SafeSendFailure("wallet allocation lock failed safely")
        # The protected parent prevents path replacement; lstat covers systems
        # without O_NOFOLLOW while O_NOFOLLOW closes the open-time race on POSIX.
        if os.path.lexists(self.path) and stat.S_ISLNK(os.lstat(self.path).st_mode):
            raise SafeSendFailure("wallet allocation lock failed safely")
        flags = os.O_RDWR | os.O_CREAT
        flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        try:
            fd = os.open(self.path, flags, 0o600)
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or (os.name != "nt" and info.st_nlink != 1):
                raise OSError("allocation lock is not a single regular file")
            if os.name != "nt":
                os.fchmod(fd, 0o600)
            elif info.st_size == 0:
                os.write(fd, b"\0")
                os.fsync(fd)
            deadline = self._monotonic() + self.acquire_timeout
            while True:
                try:
                    self._try_lock(fd)
                    self._fd = fd
                    return self
                except BlockingIOError:
                    if self._monotonic() >= deadline:
                        raise AllocationLockBusy(
                            "wallet address allocation is busy"
                        ) from None
                    self._sleep(
                        min(self.poll_interval, max(0.0, deadline - self._monotonic()))
                    )
        except AllocationLockBusy:
            if "fd" in locals():
                os.close(fd)
            raise
        except Exception:
            if "fd" in locals():
                os.close(fd)
            raise SafeSendFailure("wallet allocation lock failed safely") from None
        except BaseException:
            if "fd" in locals():
                self._fd = None
                try:
                    os.close(fd)
                except BaseException:
                    pass
            raise

    @staticmethod
    def _try_lock(fd: int) -> None:
        if os.name == "nt":
            import msvcrt

            os.lseek(fd, 0, os.SEEK_SET)
            try:
                msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
            except OSError as exc:
                raise BlockingIOError from exc
        else:
            import fcntl

            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)

    def __exit__(self, exc_type, exc, traceback) -> None:
        fd, self._fd = self._fd, None
        if fd is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                os.lseek(fd, 0, os.SEEK_SET)
                msvcrt.locking(fd, msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)


@dataclass(frozen=True, slots=True)
class WalletOutpoint:
    outpoint: str
    txid: str
    vout: int
    amount_units: int
    address: str
    owner_address_index: int


@dataclass(frozen=True, slots=True)
class WalletSnapshot:
    network: str
    tip: Tip
    primary_address: str
    addresses: tuple[str, ...]
    outpoints: tuple[WalletOutpoint, ...]
    spendable_units: int
    wallet_snapshot_hash: str


@dataclass(frozen=True, slots=True)
class InspectedOutput:
    index: int
    address: str
    amount_units: int


@dataclass(frozen=True, slots=True)
class PreparedTransfer:
    txid: str
    signed_tx_hex: str = field(repr=False)
    destination: str
    amount_units: int
    fee_units: int
    snapshot_tip: Tip
    wallet_snapshot_hash: str
    selected_outpoints: tuple[str, ...]


@dataclass(frozen=True, slots=True)
class BroadcastResult:
    txid: str
    status: str
    peer_writes: int
    submitted: bool
    observed_tip: Tip


@dataclass(frozen=True, slots=True)
class _CommandResult:
    returncode: int
    stdout: bytes
    stderr: bytes


Runner = Callable[[tuple[str, ...], bytes, float], tuple[int, bytes, bytes]]


def format_units(units: int) -> str:
    """Format integer base units as an exact plain decimal for the Go CLI."""
    if type(units) is not int or units < 0 or units > MAX_09C_UNITS:
        raise ValueError("09C units are out of range")
    whole, fraction = divmod(units, UNITS_PER_09C)
    if fraction == 0:
        return str(whole)
    return f"{whole}.{fraction:08d}".rstrip("0")


class Wallet:
    def __init__(
        self,
        *,
        binary_path: str,
        wallet_path: str,
        data_dir: str,
        network: str,
        seeds: str,
        explorer: Explorer,
        command_timeout: float = 30.0,
        observation_timeout: float = 20.0,
        poll_interval: float = 0.25,
        _runner: Runner | None = None,
        _monotonic: Callable[[], float] = time.monotonic,
        _sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.binary_path = _absolute_path(binary_path, "wallet binary")
        self.wallet_path = _absolute_path(wallet_path, "wallet file")
        self.allocation_lock_path = self.wallet_path + ".allocation.lock"
        self.data_dir = _absolute_path(data_dir, "chain data directory")
        if network not in (MAINNET, REGTEST):
            raise ValueError("wallet network is not canonical")
        if getattr(explorer, "network", None) != network:
            raise ValueError("wallet explorer network mismatch")
        if not isinstance(seeds, str) or not seeds or len(seeds) > 4_096:
            raise ValueError("wallet seeds are invalid")
        try:
            seeds.encode("ascii")
        except UnicodeEncodeError:
            raise ValueError("wallet seeds are invalid") from None
        if "\x00" in seeds or any(character.isspace() for character in seeds):
            raise ValueError("wallet seeds are invalid")
        self.network = network
        self.seeds = seeds
        self.explorer = explorer
        self._command_timeout = _positive_timeout(command_timeout, "command timeout")
        self._observation_timeout = _positive_timeout(
            observation_timeout, "observation timeout"
        )
        self._poll_interval = _positive_timeout(poll_interval, "poll interval")
        self._runner = _runner or _run_subprocess
        self._monotonic = _monotonic
        self._sleep = _sleep

    def allocation_lock(self, *, timeout: float = 5.0) -> WalletAllocationLock:
        return WalletAllocationLock(
            self.allocation_lock_path,
            acquire_timeout=timeout,
            monotonic=self._monotonic,
            sleep=self._sleep,
        )

    def new_address(self) -> str:
        payload = self._invoke(
            (
                self.binary_path,
                "wallet",
                "new",
                "-wallet-file",
                self.wallet_path,
                "-network",
                self.network,
                "-json",
            ),
            b"",
            stage="wallet_new",
            uncertain=False,
        )
        try:
            _exact_keys(
                payload,
                {"ok", "schema_version", "network", "stage", "address"},
            )
            _success_header(payload, self.network, "wallet_new")
            return _address(payload["address"])
        except _ContractError:
            raise SafeSendFailure("wallet address creation failed safely") from None

    def snapshot(self, expected_tip: Tip) -> WalletSnapshot:
        try:
            expected = _tip_value(expected_tip)
        except _ContractError:
            raise SafeSendFailure("wallet snapshot failed safely") from None
        payload = self._invoke(
            (
                self.binary_path,
                "wallet",
                "snapshot",
                "-wallet-file",
                self.wallet_path,
                "-datadir",
                self.data_dir,
                "-network",
                self.network,
                "-expected-tip-hash",
                expected.hash,
                "-expected-tip-height",
                str(expected.height),
                "-json",
            ),
            b"",
            stage="snapshot",
            uncertain=False,
        )
        try:
            return _parse_snapshot(payload, self.network, expected)
        except _ContractError:
            raise SafeSendFailure("wallet snapshot failed safely") from None

    def prepare(
        self,
        destination: str,
        amount_units: int,
        fee_units: int,
        expected_tip: Tip,
        restricted_outpoints: Sequence[str],
        expected_snapshot: WalletSnapshot,
    ) -> PreparedTransfer:
        if type(amount_units) is not int or not 1 <= amount_units <= MAX_09C_UNITS:
            raise ValueError("payment amount is out of range")
        if type(fee_units) is not int or not 0 <= fee_units <= MAX_09C_UNITS:
            raise ValueError("payment fee is out of range")
        if amount_units > MAX_09C_UNITS - fee_units:
            raise ValueError("payment amount plus fee is out of range")
        try:
            recipient = _address(destination)
            anchor = _tip_value(expected_tip)
            snapshot = _validated_snapshot(expected_snapshot, self.network, anchor)
        except _ContractError:
            raise SafeSendFailure("wallet preparation failed safely") from None
        if recipient in snapshot.addresses:
            raise SafeSendFailure("wallet preparation failed safely")
        restricted = _restricted_outpoints(restricted_outpoints, snapshot)
        try:
            before = _tip_value(self.explorer.tip())
        except (ExplorerError, _ContractError, AttributeError, TypeError, ValueError):
            raise SafeSendFailure("wallet preparation failed safely") from None
        if before != anchor:
            raise SafeSendFailure("wallet preparation failed safely")
        encoded_restricted = json.dumps(
            restricted, separators=(",", ":"), ensure_ascii=True
        ).encode("ascii")
        if len(encoded_restricted) > MAX_MACHINE_JSON_BYTES:
            raise WalletInvariantError("restricted outpoint set exceeds machine bound")
        payload = self._invoke(
            (
                self.binary_path,
                "prepare-send",
                "-to",
                recipient,
                "-amount",
                format_units(amount_units),
                "-fee",
                format_units(fee_units),
                "-datadir",
                self.data_dir,
                "-network",
                self.network,
                "-wallet-file",
                self.wallet_path,
                "-expected-tip-hash",
                anchor.hash,
                "-expected-tip-height",
                str(anchor.height),
                "-exclude-outpoints-json",
                "-",
                "-json",
            ),
            encoded_restricted,
            stage="prepared",
            uncertain=False,
        )
        try:
            prepared = _parse_prepared(
                payload,
                network=self.network,
                destination=recipient,
                amount_units=amount_units,
                fee_units=fee_units,
                expected_tip=anchor,
                expected_snapshot=snapshot,
                restricted=frozenset(restricted),
            )
        except WalletInvariantError:
            raise
        except _ContractError:
            raise SafeSendFailure("wallet preparation failed safely") from None
        inspected_payload = self._invoke(
            (
                self.binary_path,
                "inspect-tx",
                "-tx-hex",
                "-",
                "-network",
                self.network,
                "-json",
            ),
            prepared.signed_tx_hex.encode("ascii"),
            stage="inspected",
            uncertain=False,
        )
        try:
            _validate_inspection(inspected_payload, prepared, snapshot)
            after = _tip_value(self.explorer.tip())
        except (ExplorerError, _ContractError, AttributeError, TypeError, ValueError):
            raise SafeSendFailure("wallet preparation failed safely") from None
        if after != anchor:
            raise SafeSendFailure("wallet preparation failed safely")
        return prepared

    def broadcast(
        self,
        signed_tx_hex: str,
        expected_txid: str,
        prepared_tip: Tip,
    ) -> BroadcastResult:
        try:
            signed_hex = _signed_hex(signed_tx_hex)
            txid = _hash(expected_txid)
            anchor = _tip_value(prepared_tip)
            if _transaction_id(signed_hex) != txid:
                raise _ContractError
        except _ContractError:
            raise UncertainSendFailure("wallet broadcast outcome is uncertain") from None
        self._prove_prepared_tip(anchor)
        observed = self._trusted_transaction(txid)
        if observed.status in ("mempool", "confirmed"):
            self._prove_prepared_tip(anchor)
            return BroadcastResult(
                txid,
                observed.status,
                0,
                False,
                observed.tip,
            )
        payload = self._invoke(
            (
                self.binary_path,
                "broadcast-tx",
                "-tx-hex",
                "-",
                "-expected-txid",
                txid,
                "-datadir",
                self.data_dir,
                "-network",
                self.network,
                "-seeds",
                self.seeds,
                "-json",
                "-require-broadcast=true",
            ),
            signed_hex.encode("ascii"),
            stage="broadcast",
            uncertain=True,
        )
        try:
            peer_writes = _parse_broadcast(payload, self.network, txid)
        except _ContractError:
            raise UncertainSendFailure("wallet broadcast outcome is uncertain") from None
        deadline = self._monotonic() + self._observation_timeout
        while self._monotonic() < deadline:
            observed = self._trusted_transaction(txid)
            if observed.status in ("mempool", "confirmed"):
                self._prove_prepared_tip(anchor)
                return BroadcastResult(
                    txid,
                    observed.status,
                    peer_writes,
                    True,
                    observed.tip,
                )
            remaining = deadline - self._monotonic()
            if remaining > 0:
                self._sleep(min(self._poll_interval, remaining))
        raise UncertainSendFailure("wallet broadcast outcome is uncertain")

    def _prove_prepared_tip(self, prepared_tip: Tip) -> Tip:
        try:
            status = self.explorer.block(prepared_tip.hash)
            if not isinstance(status, BlockStatus):
                raise _ContractError
            live_tip = _tip_value(status.tip)
            if (
                status.hash != prepared_tip.hash
                or status.found is not True
                or status.canonical is not True
                or type(status.height) is not int
                or status.height != prepared_tip.height
                or live_tip.height < prepared_tip.height
                or (
                    live_tip.height == prepared_tip.height
                    and live_tip.hash != prepared_tip.hash
                )
            ):
                raise _ContractError
            return live_tip
        except (ExplorerError, _ContractError, AttributeError, TypeError, ValueError):
            raise UncertainSendFailure("wallet broadcast outcome is uncertain") from None

    def _trusted_transaction(self, txid: str) -> TransactionStatus:
        try:
            status = self.explorer.transaction(txid)
            if not isinstance(status, TransactionStatus) or status.txid != txid:
                raise _ContractError
            tip = _tip_value(status.tip)
            if status.status in ("unknown", "mempool"):
                if status.block is not None or status.confirmations != 0:
                    raise _ContractError
            elif status.status == "confirmed":
                if (
                    status.block is None
                    or type(status.confirmations) is not int
                    or status.confirmations < 1
                    or status.block.height < 0
                    or status.block.height > tip.height
                    or status.confirmations != tip.height - status.block.height + 1
                    or (
                        (status.block.height == tip.height)
                        != (status.block.hash == tip.hash)
                    )
                ):
                    raise _ContractError
            else:
                raise _ContractError
            return status
        except (ExplorerError, _ContractError, AttributeError, TypeError, ValueError, IndexError):
            raise UncertainSendFailure("wallet broadcast outcome is uncertain") from None

    def _invoke(
        self,
        argv: tuple[str, ...],
        stdin: bytes,
        *,
        stage: str,
        uncertain: bool,
    ) -> dict[str, object]:
        failure = UncertainSendFailure if uncertain else SafeSendFailure
        message = (
            "wallet broadcast outcome is uncertain"
            if uncertain
            else "wallet command failed safely"
        )
        if len(stdin) > MAX_MACHINE_JSON_BYTES:
            raise failure(message)
        try:
            raw = self._runner(argv, stdin, self._command_timeout)
            if (
                not isinstance(raw, tuple)
                or len(raw) != 3
                or type(raw[0]) is not int
                or not isinstance(raw[1], bytes)
                or not isinstance(raw[2], bytes)
            ):
                raise _ContractError
            result = _CommandResult(*raw)
            if (
                len(result.stdout) > MAX_MACHINE_JSON_BYTES
                or len(result.stderr) > MAX_ERROR_BYTES
            ):
                raise _ContractError
            payload = _strict_json(result.stdout)
            if result.returncode != 0:
                _failure_header(payload, self.network, stage)
                raise failure(message)
            if result.stderr:
                raise _ContractError
            return payload
        except failure:
            raise
        except BaseException as exc:
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            raise failure(message) from None


class _ContractError(Exception):
    pass


def _absolute_path(value: object, label: str) -> str:
    if not isinstance(value, str) or not value or "\x00" in value:
        raise ValueError(f"{label} must be an absolute path")
    path = Path(value)
    if not path.is_absolute():
        raise ValueError(f"{label} must be an absolute path")
    return str(path)


def _positive_timeout(value: object, label: str) -> float:
    if type(value) not in (int, float):
        raise ValueError(f"{label} must be finite and positive")
    timeout = float(value)
    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError(f"{label} must be finite and positive")
    return timeout


def _minimal_environment() -> dict[str, str]:
    environment: dict[str, str] = {}
    for name in ("SYSTEMROOT", "WINDIR"):
        value = os.environ.get(name)
        if value:
            environment[name] = value
    return environment


def _run_subprocess(
    argv: tuple[str, ...], stdin: bytes, timeout: float
) -> tuple[int, bytes, bytes]:
    process = subprocess.Popen(
        argv,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        shell=False,
        env=_minimal_environment(),
    )
    output = [bytearray(), bytearray()]
    overflow = threading.Event()
    io_failure = threading.Event()
    kill_lock = threading.Lock()

    def kill_child() -> None:
        with kill_lock:
            try:
                process.kill()
            except OSError:
                pass

    def kill_and_reap() -> None:
        kill_child()
        for _ in range(2):
            try:
                process.wait(timeout=5.0)
                return
            except subprocess.TimeoutExpired:
                kill_child()
            except BaseException:
                kill_child()

    def write_input() -> None:
        try:
            assert process.stdin is not None
            process.stdin.write(stdin)
            process.stdin.close()
        except BaseException:
            io_failure.set()
            kill_child()

    def read_bounded(stream: Any, destination: bytearray, limit: int) -> None:
        try:
            while True:
                chunk = stream.read(65_536)
                if not chunk:
                    return
                remaining = limit + 1 - len(destination)
                if remaining > 0:
                    destination.extend(chunk[:remaining])
                if len(destination) > limit:
                    overflow.set()
                    kill_child()
        except BaseException:
            io_failure.set()
            kill_child()

    assert process.stdout is not None and process.stderr is not None
    threads = [
        threading.Thread(target=write_input, daemon=True),
        threading.Thread(
            target=read_bounded,
            args=(process.stdout, output[0], MAX_MACHINE_JSON_BYTES),
            daemon=True,
        ),
        threading.Thread(
            target=read_bounded,
            args=(process.stderr, output[1], MAX_ERROR_BYTES),
            daemon=True,
        ),
    ]
    for worker in threads:
        worker.start()
    try:
        returncode = process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        kill_and_reap()
        raise TimeoutError("wallet child timed out") from None
    except (KeyboardInterrupt, SystemExit):
        kill_and_reap()
        raise
    except BaseException:
        kill_and_reap()
        raise OSError("wallet child wait failed") from None
    finally:
        for worker in threads:
            worker.join(timeout=1.0)
        if any(worker.is_alive() for worker in threads):
            kill_and_reap()
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None:
                try:
                    stream.close()
                except OSError:
                    pass
        for worker in threads:
            worker.join(timeout=1.0)
    if overflow.is_set() or io_failure.is_set() or any(worker.is_alive() for worker in threads):
        raise OSError("wallet child I/O failed")
    return returncode, bytes(output[0]), bytes(output[1])


def _strict_json(body: bytes) -> dict[str, object]:
    if not body or len(body) > MAX_MACHINE_JSON_BYTES or body.startswith(b"\xef\xbb\xbf"):
        raise _ContractError
    try:
        text = body.decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise _ContractError from None

    def unique_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
        result: dict[str, object] = {}
        for key, value in pairs:
            if key in result:
                raise _ContractError
            result[key] = value
        return result

    def reject_constant(_value: str) -> None:
        raise _ContractError

    try:
        value = json.loads(
            text,
            object_pairs_hook=unique_pairs,
            parse_constant=reject_constant,
        )
    except _ContractError:
        raise
    except (json.JSONDecodeError, RecursionError, TypeError, ValueError):
        raise _ContractError from None
    if not isinstance(value, dict):
        raise _ContractError
    return value


def _exact_keys(value: dict[str, object], expected: set[str]) -> None:
    if set(value) != expected:
        raise _ContractError


def _success_header(payload: dict[str, object], network: str, stage: str) -> None:
    if (
        payload.get("ok") is not True
        or type(payload.get("schema_version")) is not int
        or payload.get("schema_version") != SCHEMA_VERSION
        or payload.get("network") != network
        or payload.get("stage") != stage
    ):
        raise _ContractError


def _failure_header(payload: dict[str, object], network: str, stage: str) -> None:
    _exact_keys(
        payload,
        {"ok", "schema_version", "network", "stage", "error_code"},
    )
    error_code = payload.get("error_code")
    if (
        payload.get("ok") is not False
        or type(payload.get("schema_version")) is not int
        or payload.get("schema_version") != SCHEMA_VERSION
        or payload.get("network") != network
        or payload.get("stage") != stage
        or not isinstance(error_code, str)
        or _ERROR_CODE_RE.fullmatch(error_code) is None
    ):
        raise _ContractError


def _integer(
    value: object, *, minimum: int, maximum: int | None = None
) -> int:
    if type(value) is not int or value < minimum:
        raise _ContractError
    if maximum is not None and value > maximum:
        raise _ContractError
    return value


def _hash(value: object) -> str:
    if not isinstance(value, str) or _HASH_RE.fullmatch(value) is None:
        raise _ContractError
    return value


def _tip_value(value: object) -> Tip:
    if not isinstance(value, Tip):
        raise _ContractError
    return Tip(
        _hash(value.hash),
        _integer(value.height, minimum=0, maximum=MAX_U64),
    )


def _parse_tip(value: object) -> Tip:
    if not isinstance(value, dict):
        raise _ContractError
    _exact_keys(value, {"hash", "height"})
    return Tip(
        _hash(value["hash"]),
        _integer(value["height"], minimum=0, maximum=MAX_U64),
    )


def _address(value: object) -> str:
    try:
        return _explorer_address_text(value)
    except ExplorerProtocolError:
        raise _ContractError from None


def _outpoint(value: object) -> tuple[str, str, int]:
    if not isinstance(value, str):
        raise _ContractError
    match = _OUTPOINT_RE.fullmatch(value)
    if match is None:
        raise _ContractError
    vout = int(match.group("vout"))
    if vout > MAX_U32:
        raise _ContractError
    return value, match.group("txid"), vout


def _outpoint_order(value: WalletOutpoint | str) -> tuple[bytes, int]:
    if isinstance(value, WalletOutpoint):
        return bytes.fromhex(value.txid), value.vout
    _, txid, vout = _outpoint(value)
    return bytes.fromhex(txid), vout


def _parse_snapshot(
    payload: dict[str, object], network: str, expected_tip: Tip
) -> WalletSnapshot:
    _exact_keys(
        payload,
        {
            "ok",
            "schema_version",
            "network",
            "stage",
            "tip",
            "primary_address",
            "addresses",
            "outpoints",
            "spendable_units",
            "wallet_snapshot_hash",
        },
    )
    _success_header(payload, network, "snapshot")
    tip = _parse_tip(payload["tip"])
    if tip != expected_tip:
        raise _ContractError
    raw_addresses = payload["addresses"]
    if (
        not isinstance(raw_addresses, list)
        or not 1 <= len(raw_addresses) <= MAX_WALLET_ADDRESSES
    ):
        raise _ContractError
    addresses = tuple(_address(item) for item in raw_addresses)
    if tuple(sorted(addresses)) != addresses or len(set(addresses)) != len(addresses):
        raise _ContractError
    primary_address = _address(payload["primary_address"])
    if addresses.count(primary_address) != 1:
        raise _ContractError
    raw_outpoints = payload["outpoints"]
    if not isinstance(raw_outpoints, list):
        raise _ContractError
    address_indexes = {owner: index for index, owner in enumerate(addresses)}
    outputs: list[WalletOutpoint] = []
    total = 0
    for raw in raw_outpoints:
        if not isinstance(raw, dict):
            raise _ContractError
        _exact_keys(raw, {"outpoint", "amount_units", "address"})
        identity, txid, vout = _outpoint(raw["outpoint"])
        amount = _integer(raw["amount_units"], minimum=1, maximum=MAX_09C_UNITS)
        owner = _address(raw["address"])
        if owner not in address_indexes or total > MAX_09C_UNITS - amount:
            raise _ContractError
        total += amount
        outputs.append(
            WalletOutpoint(
                identity,
                txid,
                vout,
                amount,
                owner,
                address_indexes[owner],
            )
        )
    outpoints = tuple(outputs)
    if tuple(sorted(outpoints, key=_outpoint_order)) != outpoints:
        raise _ContractError
    if len({item.outpoint for item in outpoints}) != len(outpoints):
        raise _ContractError
    spendable = _integer(
        payload["spendable_units"], minimum=0, maximum=MAX_09C_UNITS
    )
    if spendable != total:
        raise _ContractError
    snapshot_hash = _hash(payload["wallet_snapshot_hash"])
    snapshot = WalletSnapshot(
        network,
        tip,
        primary_address,
        addresses,
        outpoints,
        spendable,
        snapshot_hash,
    )
    if _snapshot_hash(snapshot) != snapshot_hash:
        raise _ContractError
    return snapshot


def _snapshot_hash(snapshot: WalletSnapshot) -> str:
    if snapshot.network not in (MAINNET, REGTEST):
        raise _ContractError
    tip = _tip_value(snapshot.tip)
    if not 1 <= len(snapshot.addresses) <= MAX_WALLET_ADDRESSES:
        raise _ContractError
    primary_address = _address(snapshot.primary_address)
    if snapshot.addresses.count(primary_address) != 1:
        raise _ContractError
    if (
        tuple(sorted(snapshot.addresses)) != snapshot.addresses
        or len(set(snapshot.addresses)) != len(snapshot.addresses)
    ):
        raise _ContractError
    for owner in snapshot.addresses:
        _address(owner)
    if (
        tuple(sorted(snapshot.outpoints, key=_outpoint_order)) != snapshot.outpoints
        or len({item.outpoint for item in snapshot.outpoints})
        != len(snapshot.outpoints)
    ):
        raise _ContractError
    total = 0
    fields: list[tuple[str, int, int, int]] = []
    for item in snapshot.outpoints:
        identity, txid, vout = _outpoint(item.outpoint)
        if (
            identity != item.outpoint
            or txid != item.txid
            or vout != item.vout
            or not 0 <= item.owner_address_index < len(snapshot.addresses)
            or snapshot.addresses[item.owner_address_index] != item.address
            or item.amount_units <= 0
            or item.amount_units > MAX_09C_UNITS
            or total > MAX_09C_UNITS - item.amount_units
        ):
            raise _ContractError
        total += item.amount_units
        fields.append((txid, vout, item.amount_units, item.owner_address_index))
    if total != snapshot.spendable_units:
        raise _ContractError
    preimage = _snapshot_hash_preimage_fields(
        snapshot.network,
        tip,
        snapshot.primary_address,
        snapshot.addresses,
        tuple(fields),
    )
    return hashlib.sha256(preimage).hexdigest()


def _snapshot_hash_preimage_fields(
    network: str,
    tip: Tip,
    primary_address: str,
    addresses: Sequence[str],
    outpoints: Sequence[tuple[str, int, int, int]],
) -> bytes:
    if network not in (MAINNET, REGTEST):
        raise _ContractError
    anchor = _tip_value(tip)
    network_bytes = network.encode("utf-8")
    if len(network_bytes) > 0xFFFF or len(addresses) > MAX_U32:
        raise _ContractError
    canonical_addresses = tuple(addresses)
    if (
        tuple(sorted(canonical_addresses)) != canonical_addresses
        or len(set(canonical_addresses)) != len(canonical_addresses)
    ):
        raise _ContractError
    try:
        primary_bytes = primary_address.encode("ascii")
    except (AttributeError, UnicodeEncodeError):
        raise _ContractError from None
    if (
        not primary_bytes
        or len(primary_bytes) > 0xFFFF
        or canonical_addresses.count(primary_address) != 1
    ):
        raise _ContractError
    encoded = bytearray(b"btc09-wallet-snapshot-v2\0")
    encoded.extend(struct.pack(">H", len(network_bytes)))
    encoded.extend(network_bytes)
    encoded.extend(bytes.fromhex(anchor.hash))
    encoded.extend(struct.pack(">Q", anchor.height))
    encoded.extend(struct.pack(">H", len(primary_bytes)))
    encoded.extend(primary_bytes)
    encoded.extend(struct.pack(">I", len(canonical_addresses)))
    for owner in canonical_addresses:
        if not isinstance(owner, str):
            raise _ContractError
        try:
            raw_owner = owner.encode("ascii")
        except UnicodeEncodeError:
            raise _ContractError from None
        if not raw_owner or len(raw_owner) > 0xFFFF:
            raise _ContractError
        encoded.extend(struct.pack(">H", len(raw_owner)))
        encoded.extend(raw_owner)
    if len(outpoints) > MAX_U32:
        raise _ContractError
    encoded.extend(struct.pack(">I", len(outpoints)))
    previous: tuple[bytes, int] | None = None
    for raw in outpoints:
        if not isinstance(raw, tuple) or len(raw) != 4:
            raise _ContractError
        txid, vout, amount, owner_index = raw
        txid = _hash(txid)
        vout = _integer(vout, minimum=0, maximum=MAX_U32)
        amount = _integer(amount, minimum=1, maximum=MAX_09C_UNITS)
        owner_index = _integer(
            owner_index, minimum=0, maximum=len(canonical_addresses) - 1
        )
        order = (bytes.fromhex(txid), vout)
        if previous is not None and order <= previous:
            raise _ContractError
        previous = order
        encoded.extend(order[0])
        encoded.extend(struct.pack(">I", vout))
        encoded.extend(struct.pack(">Q", amount))
        encoded.extend(struct.pack(">I", owner_index))
    return bytes(encoded)


def _validated_snapshot(
    value: object, network: str, expected_tip: Tip
) -> WalletSnapshot:
    if not isinstance(value, WalletSnapshot):
        raise _ContractError
    if value.network != network or value.tip != expected_tip:
        raise _ContractError
    if (
        type(value.spendable_units) is not int
        or not 0 <= value.spendable_units <= MAX_09C_UNITS
    ):
        raise _ContractError
    if _snapshot_hash(value) != value.wallet_snapshot_hash:
        raise _ContractError
    return value


def _restricted_outpoints(
    value: object, snapshot: WalletSnapshot
) -> tuple[str, ...]:
    if isinstance(value, (str, bytes)) or not isinstance(value, Sequence):
        raise WalletInvariantError("restricted outpoint set is not a sequence")
    if len(value) > MAX_RESTRICTED_OUTPOINTS:
        raise WalletInvariantError("restricted outpoint set exceeds machine bound")
    restricted: list[str] = []
    try:
        for item in value:
            identity, _, _ = _outpoint(item)
            restricted.append(identity)
    except _ContractError:
        raise WalletInvariantError("restricted outpoint set is noncanonical") from None
    canonical = tuple(restricted)
    if tuple(sorted(canonical, key=_outpoint_order)) != canonical:
        raise WalletInvariantError("restricted outpoint set is not sorted")
    if len(set(canonical)) != len(canonical):
        raise WalletInvariantError("restricted outpoint set contains duplicates")
    anchored = {item.outpoint for item in snapshot.outpoints}
    if any(item not in anchored for item in canonical):
        raise WalletInvariantError("restricted outpoint is not wallet-anchored")
    return canonical


def _signed_hex(value: object) -> str:
    if (
        not isinstance(value, str)
        or not 2 <= len(value) <= MAX_SIGNED_TX_HEX_CHARS
        or len(value) % 2 != 0
        or _SIGNED_HEX_RE.fullmatch(value) is None
    ):
        raise _ContractError
    return value


def _transaction_id(signed_hex: str) -> str:
    first = hashlib.sha256(bytes.fromhex(signed_hex)).digest()
    return hashlib.sha256(first).hexdigest()


def _parse_prepared(
    payload: dict[str, object],
    *,
    network: str,
    destination: str,
    amount_units: int,
    fee_units: int,
    expected_tip: Tip,
    expected_snapshot: WalletSnapshot,
    restricted: frozenset[str],
) -> PreparedTransfer:
    _exact_keys(
        payload,
        {
            "ok",
            "schema_version",
            "network",
            "stage",
            "txid",
            "signed_tx_hex",
            "destination",
            "amount_units",
            "fee_units",
            "snapshot_tip",
            "wallet_snapshot_hash",
            "selected_outpoints",
        },
    )
    _success_header(payload, network, "prepared")
    txid = _hash(payload["txid"])
    signed_hex = _signed_hex(payload["signed_tx_hex"])
    if _transaction_id(signed_hex) != txid:
        raise _ContractError
    if _address(payload["destination"]) != destination:
        raise _ContractError
    if _integer(payload["amount_units"], minimum=1, maximum=MAX_09C_UNITS) != amount_units:
        raise _ContractError
    if _integer(payload["fee_units"], minimum=0, maximum=MAX_09C_UNITS) != fee_units:
        raise _ContractError
    if _parse_tip(payload["snapshot_tip"]) != expected_tip:
        raise _ContractError
    snapshot_hash = _hash(payload["wallet_snapshot_hash"])
    if snapshot_hash != expected_snapshot.wallet_snapshot_hash:
        raise _ContractError
    raw_selected = payload["selected_outpoints"]
    if (
        not isinstance(raw_selected, list)
        or not raw_selected
        or len(raw_selected) > MAX_RESTRICTED_OUTPOINTS
    ):
        raise _ContractError
    selected: list[str] = []
    for raw in raw_selected:
        identity, _, _ = _outpoint(raw)
        selected.append(identity)
    selected_tuple = tuple(selected)
    if len(set(selected_tuple)) != len(selected_tuple):
        raise _ContractError
    if any(item in restricted for item in selected_tuple):
        raise WalletInvariantError("wallet selected a restricted outpoint")
    anchored = {item.outpoint for item in expected_snapshot.outpoints}
    if any(item not in anchored for item in selected_tuple):
        raise WalletInvariantError("wallet selected an unanchored outpoint")
    return PreparedTransfer(
        txid,
        signed_hex,
        destination,
        amount_units,
        fee_units,
        expected_tip,
        snapshot_hash,
        selected_tuple,
    )


def _validate_inspection(
    payload: dict[str, object],
    prepared: PreparedTransfer,
    snapshot: WalletSnapshot,
) -> None:
    _exact_keys(
        payload,
        {"ok", "schema_version", "network", "stage", "txid", "inputs", "outputs"},
    )
    _success_header(payload, snapshot.network, "inspected")
    if _hash(payload["txid"]) != prepared.txid:
        raise _ContractError
    raw_inputs = payload["inputs"]
    if not isinstance(raw_inputs, list):
        raise _ContractError
    inputs = tuple(_outpoint(item)[0] for item in raw_inputs)
    if inputs != prepared.selected_outpoints:
        raise _ContractError
    amounts = {item.outpoint: item.amount_units for item in snapshot.outpoints}
    input_sum = 0
    for selected in inputs:
        amount = amounts[selected]
        if input_sum > MAX_09C_UNITS - amount:
            raise _ContractError
        input_sum += amount
    need = prepared.amount_units + prepared.fee_units
    if input_sum < need:
        raise _ContractError
    raw_outputs = payload["outputs"]
    if not isinstance(raw_outputs, list) or len(raw_outputs) not in (1, 2):
        raise _ContractError
    outputs: list[InspectedOutput] = []
    for position, raw in enumerate(raw_outputs):
        if not isinstance(raw, dict):
            raise _ContractError
        _exact_keys(raw, {"index", "address", "amount_units"})
        index = _integer(raw["index"], minimum=0, maximum=MAX_U32)
        amount = _integer(raw["amount_units"], minimum=1, maximum=MAX_09C_UNITS)
        if index != position:
            raise _ContractError
        outputs.append(InspectedOutput(index, _address(raw["address"]), amount))
    if (
        outputs[0].address != prepared.destination
        or outputs[0].amount_units != prepared.amount_units
    ):
        raise _ContractError
    change = input_sum - need
    if change == 0:
        if len(outputs) != 1:
            raise _ContractError
    elif (
        len(outputs) != 2
        or outputs[1].address not in snapshot.addresses
        or outputs[1].amount_units != change
    ):
        raise _ContractError


def _parse_broadcast(
    payload: dict[str, object], network: str, expected_txid: str
) -> int:
    _exact_keys(
        payload,
        {
            "ok",
            "schema_version",
            "network",
            "stage",
            "status",
            "txid",
            "peer_writes",
        },
    )
    _success_header(payload, network, "broadcast")
    if payload["status"] != "submitted" or _hash(payload["txid"]) != expected_txid:
        raise _ContractError
    return _integer(payload["peer_writes"], minimum=1, maximum=MAX_U32)
