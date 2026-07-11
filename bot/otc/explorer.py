from __future__ import annotations

import hashlib
import http.client
import json
import math
import re
import socket
import threading
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from bot.otc.domain import MAX_09C_UNITS


SCHEMA_VERSION = 1
MAINNET = "btc09-mainnet"
REGTEST = "btc09-regtest"
MAX_RESPONSE_BYTES = 4 << 20
MAX_WATCHED_ADDRESSES = 10_000
MAX_BATCH_OUTPOINTS = 100_000
MAX_BATCH_BYTES = 32 << 20
MAX_U32 = (1 << 32) - 1

_BASE_RE = re.compile(
    r"http://(?:(?P<ipv4>127\.0\.0\.1)|\[(?P<ipv6>::1)\]):(?P<port>[1-9][0-9]{0,4})\Z",
    re.ASCII,
)
_HASH_RE = re.compile(r"[0-9a-f]{64}\Z", re.ASCII)
_B58_ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
_B58_INDEX = {char: index for index, char in enumerate(_B58_ALPHABET)}


class ExplorerError(RuntimeError):
    """Base class for fail-closed explorer adapter errors."""


class ExplorerTransportError(ExplorerError):
    """The pinned local service could not be reached or finish in time."""


class ExplorerProtocolError(ExplorerError):
    """The local service returned an untrusted or inconsistent contract."""


class TipMismatch(ExplorerError):
    """A typed, authoritative expected-tip mismatch."""

    def __init__(self, tip: Tip) -> None:
        super().__init__("explorer tip mismatch")
        self.tip = tip


@dataclass(frozen=True, slots=True)
class Tip:
    hash: str
    height: int


@dataclass(frozen=True, slots=True)
class BlockAnchor:
    hash: str
    height: int


@dataclass(frozen=True, slots=True)
class BlockStatus:
    hash: str
    found: bool
    height: int | None
    canonical: bool
    tip: Tip


@dataclass(frozen=True, slots=True)
class TransactionStatus:
    txid: str
    status: str
    block: BlockAnchor | None
    confirmations: int
    tip: Tip


@dataclass(frozen=True, slots=True)
class ConfirmedSpend:
    txid: str
    input_index: int
    block: BlockAnchor


@dataclass(frozen=True, slots=True)
class ConfirmedOutput:
    txid: str
    transaction_index: int
    vout: int
    amount_units: int
    block: BlockAnchor
    confirmations: int
    coinbase: bool
    mature: bool
    spent_by: ConfirmedSpend | None


@dataclass(frozen=True, slots=True)
class AddressSnapshot:
    network: str
    address: str
    complete: bool
    tip: Tip
    outputs: tuple[ConfirmedOutput, ...]

    def to_store_snapshot(self) -> dict[str, object]:
        flattened_outputs: list[dict[str, object]] = []
        for output in self.outputs:
            spent: dict[str, object] | None = None
            if output.spent_by is not None:
                spent = {
                    "txid": output.spent_by.txid,
                    "vin": output.spent_by.input_index,
                    "block_hash": output.spent_by.block.hash,
                    "block_height": output.spent_by.block.height,
                }
            flattened_outputs.append(
                {
                    "txid": output.txid,
                    "vout": output.vout,
                    "amount_units": output.amount_units,
                    "block_hash": output.block.hash,
                    "block_height": output.block.height,
                    "confirmations": output.confirmations,
                    "coinbase": output.coinbase,
                    "mature": output.mature,
                    "spent_by": spent,
                }
            )
        return {
            "network": self.network,
            "address": self.address,
            "complete": self.complete,
            "tip_hash": self.tip.hash,
            "tip_height": self.tip.height,
            "outputs": flattened_outputs,
        }


@dataclass(frozen=True, slots=True)
class AddressBatch:
    network: str
    tip: Tip
    snapshots: tuple[AddressSnapshot, ...]

    def store_snapshots(self) -> tuple[dict[str, object], ...]:
        return tuple(snapshot.to_store_snapshot() for snapshot in self.snapshots)


class Explorer:
    def __init__(
        self,
        base_url: str,
        *,
        network: str,
        connect_timeout: float = 2.0,
        read_timeout: float = 3.0,
        total_timeout: float = 5.0,
    ) -> None:
        if not isinstance(base_url, str):
            raise ValueError("explorer base must be canonical loopback HTTP")
        match = _BASE_RE.fullmatch(base_url)
        if match is None:
            raise ValueError("explorer base must be canonical loopback HTTP")
        port = int(match.group("port"))
        if not 1 <= port <= 65_535:
            raise ValueError("explorer port is out of range")
        if network not in (MAINNET, REGTEST):
            raise ValueError("explorer network is not canonical")
        self._host = "127.0.0.1" if match.group("ipv4") else "::1"
        self._port = port
        self.network = network
        self._coinbase_maturity = 100 if network == MAINNET else 2
        self._connect_timeout = _positive_timeout(connect_timeout)
        self._read_timeout = _positive_timeout(read_timeout)
        self._total_timeout = _positive_timeout(total_timeout)
        self._max_response_bytes = MAX_RESPONSE_BYTES
        self._max_watched_addresses = MAX_WATCHED_ADDRESSES
        self._max_batch_outpoints = MAX_BATCH_OUTPOINTS
        self._max_batch_bytes = MAX_BATCH_BYTES

    def tip(self) -> Tip:
        tip, _ = self._tip_with_size()
        return tip

    def block(self, block_hash: str) -> BlockStatus:
        query_hash = _hash_text(block_hash, "block hash")
        status, payload, _ = self._request_json(
            f"/api/v1/block/{query_hash}", {200, 404}
        )
        _schema_network(payload, self.network)
        _exact_keys(payload, {"schema_version", "network", "found", "block", "tip"})
        found = _boolean(payload["found"], "block found")
        block = _mapping(payload["block"], "block")
        _exact_keys(block, {"hash", "height", "canonical"})
        if _hash_text(block["hash"], "block hash") != query_hash:
            raise ExplorerProtocolError("block identity mismatch")
        canonical = _boolean(block["canonical"], "canonical flag")
        tip = _parse_tip(payload["tip"])
        if status == 404:
            if (
                found
                or block["height"] is not None
                or canonical
                or query_hash == tip.hash
            ):
                raise ExplorerProtocolError("invalid missing block response")
            return BlockStatus(query_hash, False, None, False, tip)
        if not found:
            raise ExplorerProtocolError("found block response is inconsistent")
        height = _integer(block["height"], "block height", minimum=0)
        if canonical:
            if height > tip.height:
                raise ExplorerProtocolError("canonical block exceeds tip")
            if height == tip.height and query_hash != tip.hash:
                raise ExplorerProtocolError("canonical tip identity mismatch")
        if query_hash == tip.hash and (not canonical or height != tip.height):
            raise ExplorerProtocolError("tip block is not canonical")
        return BlockStatus(query_hash, True, height, canonical, tip)

    def transaction(self, txid: str) -> TransactionStatus:
        query_txid = _hash_text(txid, "transaction ID")
        status_code, payload, _ = self._request_json(
            f"/api/v1/transaction/{query_txid}", {200}
        )
        if status_code != 200:
            raise ExplorerProtocolError("unexpected transaction status")
        _schema_network(payload, self.network)
        _exact_keys(
            payload,
            {
                "schema_version",
                "network",
                "txid",
                "status",
                "block",
                "confirmations",
                "tip",
            },
        )
        if _hash_text(payload["txid"], "transaction ID") != query_txid:
            raise ExplorerProtocolError("transaction identity mismatch")
        chain_status = payload["status"]
        if chain_status not in ("unknown", "mempool", "confirmed"):
            raise ExplorerProtocolError("invalid transaction status")
        tip = _parse_tip(payload["tip"])
        confirmations = _integer(
            payload["confirmations"], "transaction confirmations", minimum=0
        )
        if chain_status in ("unknown", "mempool"):
            if payload["block"] is not None or confirmations != 0:
                raise ExplorerProtocolError("unconfirmed transaction has block data")
            return TransactionStatus(query_txid, chain_status, None, 0, tip)
        block = _parse_block_anchor(payload["block"], "transaction block")
        if (
            block.height > tip.height
            or confirmations != tip.height - block.height + 1
            or ((block.height == tip.height) != (block.hash == tip.hash))
        ):
            raise ExplorerProtocolError("confirmed transaction anchor mismatch")
        return TransactionStatus(query_txid, chain_status, block, confirmations, tip)

    def outputs(self, address: str, expected_tip: Tip | None = None) -> AddressSnapshot:
        snapshot, _ = self._outputs_with_size(address, expected_tip)
        return snapshot

    def batch_outputs(
        self, read_watched_addresses: Callable[[], Sequence[str]]
    ) -> AddressBatch:
        first_addresses = self._read_watched_addresses(read_watched_addresses)
        tip, total_bytes = self._tip_with_size()
        snapshots: list[AddressSnapshot] = []
        outpoints: set[tuple[str, str, int]] = set()
        spending_inputs: set[tuple[str, int]] = set()
        total_outpoints = 0
        for watched in first_addresses:
            snapshot, response_bytes = self._outputs_with_size(watched, tip)
            total_bytes += response_bytes
            total_outpoints += len(snapshot.outputs)
            if total_bytes > self._max_batch_bytes or total_outpoints > self._max_batch_outpoints:
                raise ExplorerProtocolError("explorer batch exceeds limits")
            for output in snapshot.outputs:
                identity = (snapshot.network, output.txid, output.vout)
                if identity in outpoints:
                    raise ExplorerProtocolError("duplicate outpoint in explorer batch")
                outpoints.add(identity)
                if output.spent_by is not None:
                    spend_identity = (
                        output.spent_by.txid,
                        output.spent_by.input_index,
                    )
                    if spend_identity in spending_inputs:
                        raise ExplorerProtocolError(
                            "duplicate spending input in explorer batch"
                        )
                    spending_inputs.add(spend_identity)
            snapshots.append(snapshot)
        final_tip, response_bytes = self._tip_with_size()
        total_bytes += response_bytes
        second_addresses = self._read_watched_addresses(read_watched_addresses)
        if total_bytes > self._max_batch_bytes:
            raise ExplorerProtocolError("explorer batch exceeds limits")
        if second_addresses != first_addresses:
            raise ExplorerProtocolError("watched address set changed")
        if final_tip != tip:
            if final_tip.hash == tip.hash:
                raise ExplorerProtocolError("tip hash has conflicting height")
            raise TipMismatch(final_tip)
        return AddressBatch(self.network, tip, tuple(snapshots))

    def _tip_with_size(self) -> tuple[Tip, int]:
        status, payload, response_bytes = self._request_json("/api/v1/tip", {200})
        if status != 200:
            raise ExplorerProtocolError("unexpected tip status")
        _schema_network(payload, self.network)
        _exact_keys(payload, {"schema_version", "network", "tip"})
        return _parse_tip(payload["tip"]), response_bytes

    def _outputs_with_size(
        self, address: str, expected_tip: Tip | None
    ) -> tuple[AddressSnapshot, int]:
        canonical_address = _address_text(address)
        path = f"/api/v1/address/{canonical_address}/outputs"
        expected: Tip | None = None
        if expected_tip is not None:
            expected = _validated_tip(expected_tip)
            path += (
                f"?expected_tip_hash={expected.hash}"
                f"&expected_tip_height={expected.height}"
            )
        status, payload, response_bytes = self._request_json(path, {200, 409})
        _schema_network(payload, self.network)
        if status == 409:
            _exact_keys(
                payload,
                {"schema_version", "network", "address", "complete", "tip"},
            )
            if payload["address"] != canonical_address or payload["complete"] is not False:
                raise ExplorerProtocolError("invalid expected-tip mismatch")
            mismatch_tip = _parse_tip(payload["tip"])
            if expected is None or mismatch_tip.hash == expected.hash:
                raise ExplorerProtocolError("invalid expected-tip mismatch")
            raise TipMismatch(mismatch_tip)
        _exact_keys(
            payload,
            {
                "schema_version",
                "network",
                "address",
                "complete",
                "tip",
                "outputs",
            },
        )
        if payload["address"] != canonical_address or payload["complete"] is not True:
            raise ExplorerProtocolError("address snapshot is incomplete")
        tip = _parse_tip(payload["tip"])
        if expected_tip is not None and tip != expected:
            raise ExplorerProtocolError("successful address tip mismatches expectation")
        raw_outputs = payload["outputs"]
        if not isinstance(raw_outputs, list) or len(raw_outputs) > self._max_batch_outpoints:
            raise ExplorerProtocolError("invalid address output collection")
        outputs = self._parse_outputs(raw_outputs, tip)
        return (
            AddressSnapshot(
                network=self.network,
                address=canonical_address,
                complete=True,
                tip=tip,
                outputs=outputs,
            ),
            response_bytes,
        )

    def _parse_outputs(
        self, raw_outputs: list[object], tip: Tip
    ) -> tuple[ConfirmedOutput, ...]:
        outputs: list[ConfirmedOutput] = []
        seen_outpoints: set[tuple[str, int]] = set()
        seen_spends: set[tuple[str, int]] = set()
        height_hashes: dict[int, str] = {}
        transaction_ids: dict[tuple[int, int], str] = {}
        previous_order: tuple[int, int, int] | None = None

        def anchor(value: object, label: str) -> BlockAnchor:
            block = _parse_block_anchor(value, label)
            if block.height > tip.height:
                raise ExplorerProtocolError("canonical block exceeds snapshot tip")
            if (block.height == tip.height) != (block.hash == tip.hash):
                raise ExplorerProtocolError("canonical tip identity mismatch")
            prior = height_hashes.setdefault(block.height, block.hash)
            if prior != block.hash:
                raise ExplorerProtocolError("conflicting block hash at one height")
            return block

        for raw in raw_outputs:
            item = _mapping(raw, "address output")
            _exact_keys(
                item,
                {
                    "txid",
                    "transaction_index",
                    "vout",
                    "amount_units",
                    "block",
                    "confirmations",
                    "coinbase",
                    "mature",
                    "spent_by",
                },
            )
            txid = _hash_text(item["txid"], "output transaction ID")
            transaction_index = _integer(
                item["transaction_index"], "transaction index", minimum=0, maximum=MAX_U32
            )
            vout = _integer(item["vout"], "output index", minimum=0, maximum=MAX_U32)
            amount = _integer(
                item["amount_units"],
                "output amount",
                minimum=1,
                maximum=MAX_09C_UNITS,
            )
            block = anchor(item["block"], "output block")
            confirmations = _integer(
                item["confirmations"], "output confirmations", minimum=1
            )
            if confirmations != tip.height - block.height + 1:
                raise ExplorerProtocolError("output confirmations mismatch")
            coinbase = _boolean(item["coinbase"], "coinbase flag")
            mature = _boolean(item["mature"], "maturity flag")
            if mature != (not coinbase or confirmations >= self._coinbase_maturity):
                raise ExplorerProtocolError("output maturity mismatch")
            order = (block.height, transaction_index, vout)
            if previous_order is not None and order <= previous_order:
                raise ExplorerProtocolError("address outputs are not canonically ordered")
            previous_order = order
            transaction_position = (block.height, transaction_index)
            prior_txid = transaction_ids.setdefault(transaction_position, txid)
            if prior_txid != txid:
                raise ExplorerProtocolError(
                    "conflicting transaction identity at one position"
                )
            outpoint = (txid, vout)
            if outpoint in seen_outpoints:
                raise ExplorerProtocolError("duplicate address outpoint")
            seen_outpoints.add(outpoint)

            spent_by: ConfirmedSpend | None = None
            if item["spent_by"] is not None:
                spent = _mapping(item["spent_by"], "confirmed spender")
                _exact_keys(spent, {"txid", "input_index", "block"})
                spent_txid = _hash_text(spent["txid"], "spending transaction ID")
                input_index = _integer(
                    spent["input_index"], "spending input index", minimum=0, maximum=MAX_U32
                )
                spent_block = anchor(spent["block"], "spending block")
                if spent_block.height < block.height:
                    raise ExplorerProtocolError("spender predates output")
                if coinbase and spent_block.height - block.height < self._coinbase_maturity:
                    raise ExplorerProtocolError("coinbase spent before maturity")
                spend_identity = (spent_txid, input_index)
                if spend_identity in seen_spends:
                    raise ExplorerProtocolError("duplicate confirmed spending input")
                seen_spends.add(spend_identity)
                spent_by = ConfirmedSpend(spent_txid, input_index, spent_block)
            outputs.append(
                ConfirmedOutput(
                    txid=txid,
                    transaction_index=transaction_index,
                    vout=vout,
                    amount_units=amount,
                    block=block,
                    confirmations=confirmations,
                    coinbase=coinbase,
                    mature=mature,
                    spent_by=spent_by,
                )
            )
        return tuple(outputs)

    def _read_watched_addresses(
        self, reader: Callable[[], Sequence[str]]
    ) -> tuple[str, ...]:
        if not callable(reader):
            raise ExplorerProtocolError("watched address reader is invalid")
        try:
            values = reader()
        except Exception:
            raise ExplorerProtocolError("watched address read failed") from None
        if isinstance(values, (str, bytes, bytearray)) or not isinstance(values, Sequence):
            raise ExplorerProtocolError("watched addresses are not a sequence")
        if len(values) > self._max_watched_addresses:
            raise ExplorerProtocolError("too many watched addresses")
        addresses = tuple(_address_text(value) for value in values)
        if len(set(addresses)) != len(addresses):
            raise ExplorerProtocolError("duplicate watched address")
        return tuple(sorted(addresses))

    def _request_json(
        self, path: str, accepted_statuses: set[int]
    ) -> tuple[int, dict[str, object], int]:
        started = time.monotonic()
        deadline = started + self._total_timeout
        connection = http.client.HTTPConnection(
            self._host, self._port, timeout=self._connect_timeout
        )
        expired = threading.Event()

        def expire() -> None:
            expired.set()
            connection.close()

        watchdog = threading.Timer(self._total_timeout, expire)
        watchdog.daemon = True
        watchdog.start()
        try:
            connection.connect()
            self._bound_socket(connection, deadline)
            connection.request(
                "GET",
                path,
                headers={
                    "Accept": "application/json",
                    "Accept-Encoding": "identity",
                    "Connection": "close",
                },
            )
            self._bound_socket(connection, deadline)
            response = connection.getresponse()
            self._validate_headers(response)
            declared = self._declared_length(response)
            if declared is not None and declared > self._max_response_bytes:
                raise ExplorerProtocolError("explorer response exceeds limit")
            body = bytearray()
            while True:
                self._bound_socket(connection, deadline)
                remaining = self._max_response_bytes + 1 - len(body)
                if remaining <= 0:
                    raise ExplorerProtocolError("explorer response exceeds limit")
                chunk = response.read1(min(65_536, remaining))
                if not chunk:
                    break
                body.extend(chunk)
                if len(body) > self._max_response_bytes:
                    raise ExplorerProtocolError("explorer response exceeds limit")
            if declared is not None and len(body) != declared:
                raise ExplorerProtocolError("explorer content length mismatch")
            if expired.is_set() or time.monotonic() > deadline:
                raise ExplorerTransportError("explorer response deadline exceeded")
            payload = _strict_json(bytes(body))
            watchdog.cancel()
            watchdog.join(timeout=0.2)
            if expired.is_set() or time.monotonic() > deadline:
                raise ExplorerTransportError("explorer response deadline exceeded")
            if response.status not in accepted_statuses:
                raise ExplorerProtocolError("unexpected explorer HTTP status")
            return response.status, payload, len(body)
        except ExplorerError:
            raise
        except (OSError, socket.timeout, TimeoutError, http.client.HTTPException):
            raise ExplorerTransportError("explorer transport unavailable") from None
        finally:
            watchdog.cancel()
            connection.close()
            watchdog.join(timeout=0.2)

    def _bound_socket(
        self, connection: http.client.HTTPConnection, deadline: float
    ) -> None:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ExplorerTransportError("explorer response deadline exceeded")
        if connection.sock is not None:
            connection.sock.settimeout(min(self._read_timeout, remaining))

    def _validate_headers(self, response: http.client.HTTPResponse) -> None:
        content_types = response.headers.get_all("Content-Type", [])
        if len(content_types) != 1 or content_types[0].strip().lower() not in {
            "application/json",
            "application/json; charset=utf-8",
        }:
            raise ExplorerProtocolError("invalid explorer content type")
        encodings = response.headers.get_all("Content-Encoding", [])
        if len(encodings) > 1 or (
            encodings and encodings[0].strip().lower() != "identity"
        ):
            raise ExplorerProtocolError("invalid explorer content encoding")
        transfers = response.headers.get_all("Transfer-Encoding", [])
        lengths = response.headers.get_all("Content-Length", [])
        if len(transfers) > 1 or (
            transfers and transfers[0].strip().lower() != "chunked"
        ):
            raise ExplorerProtocolError("invalid explorer transfer encoding")
        if transfers and lengths:
            raise ExplorerProtocolError("ambiguous explorer body framing")
        if len(lengths) > 1:
            raise ExplorerProtocolError("duplicate explorer content length")

    def _declared_length(self, response: http.client.HTTPResponse) -> int | None:
        lengths = response.headers.get_all("Content-Length", [])
        if not lengths:
            return None
        text = lengths[0]
        if not re.fullmatch(r"0|[1-9][0-9]*", text, re.ASCII):
            raise ExplorerProtocolError("invalid explorer content length")
        return int(text)


def _positive_timeout(value: object) -> float:
    if type(value) not in (int, float):
        raise ValueError("explorer timeout must be finite and positive")
    timeout = float(value)
    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("explorer timeout must be finite and positive")
    return timeout


def _strict_json(body: bytes) -> dict[str, object]:
    if body.startswith(b"\xef\xbb\xbf"):
        raise ExplorerProtocolError("explorer JSON contains BOM")
    try:
        text = body.decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise ExplorerProtocolError("explorer JSON is not UTF-8") from None

    def reject_constant(_value: str) -> None:
        raise ExplorerProtocolError("explorer JSON contains non-finite number")

    def unique_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
        result: dict[str, object] = {}
        for key, value in pairs:
            if key in result:
                raise ExplorerProtocolError("explorer JSON contains duplicate key")
            result[key] = value
        return result

    try:
        payload = json.loads(
            text,
            object_pairs_hook=unique_pairs,
            parse_constant=reject_constant,
        )
    except ExplorerProtocolError:
        raise
    except (json.JSONDecodeError, RecursionError, TypeError, ValueError):
        raise ExplorerProtocolError("explorer JSON is malformed") from None
    if not isinstance(payload, dict):
        raise ExplorerProtocolError("explorer JSON root is not an object")
    return payload


def _mapping(value: object, label: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise ExplorerProtocolError(f"{label} is not an object")
    return value


def _exact_keys(value: dict[str, object], expected: set[str]) -> None:
    if set(value) != expected:
        raise ExplorerProtocolError("explorer object has wrong fields")


def _schema_network(payload: dict[str, object], network: str) -> None:
    if type(payload.get("schema_version")) is not int or payload["schema_version"] != 1:
        raise ExplorerProtocolError("explorer schema version mismatch")
    if payload.get("network") != network:
        raise ExplorerProtocolError("explorer network mismatch")


def _boolean(value: object, label: str) -> bool:
    if type(value) is not bool:
        raise ExplorerProtocolError(f"{label} is not boolean")
    return value


def _integer(
    value: object,
    label: str,
    *,
    minimum: int,
    maximum: int | None = None,
) -> int:
    if type(value) is not int or value < minimum or (
        maximum is not None and value > maximum
    ):
        raise ExplorerProtocolError(f"{label} is out of range")
    return value


def _hash_text(value: object, label: str) -> str:
    if not isinstance(value, str) or _HASH_RE.fullmatch(value) is None:
        raise ExplorerProtocolError(f"{label} is not canonical")
    return value


def _parse_tip(value: object) -> Tip:
    tip = _mapping(value, "tip")
    _exact_keys(tip, {"hash", "height"})
    return Tip(
        hash=_hash_text(tip["hash"], "tip hash"),
        height=_integer(tip["height"], "tip height", minimum=0),
    )


def _validated_tip(value: object) -> Tip:
    if not isinstance(value, Tip):
        raise ExplorerProtocolError("expected tip is not typed")
    return Tip(
        hash=_hash_text(value.hash, "expected tip hash"),
        height=_integer(value.height, "expected tip height", minimum=0),
    )


def _parse_block_anchor(value: object, label: str) -> BlockAnchor:
    block = _mapping(value, label)
    _exact_keys(block, {"hash", "height"})
    return BlockAnchor(
        hash=_hash_text(block["hash"], f"{label} hash"),
        height=_integer(block["height"], f"{label} height", minimum=0),
    )


def _address_text(value: object) -> str:
    if not isinstance(value, str) or not value or len(value) > 128:
        raise ExplorerProtocolError("09C address is not canonical")
    try:
        value.encode("ascii")
    except UnicodeEncodeError:
        raise ExplorerProtocolError("09C address is not canonical") from None
    number = 0
    try:
        for char in value:
            number = number * 58 + _B58_INDEX[char]
    except KeyError:
        raise ExplorerProtocolError("09C address is not canonical") from None
    raw = number.to_bytes((number.bit_length() + 7) // 8, "big") if number else b""
    leading = len(value) - len(value.lstrip("1"))
    raw = b"\x00" * leading + raw
    if len(raw) != 25 or raw[0] != 0x09:
        raise ExplorerProtocolError("09C address is not canonical")
    payload, checksum = raw[:21], raw[21:]
    expected = hashlib.sha256(hashlib.sha256(payload).digest()).digest()[:4]
    if checksum != expected or _b58encode(raw) != value:
        raise ExplorerProtocolError("09C address is not canonical")
    return value


def _b58encode(raw: bytes) -> str:
    number = int.from_bytes(raw, "big")
    encoded = ""
    while number:
        number, remainder = divmod(number, 58)
        encoded = _B58_ALPHABET[remainder] + encoded
    for byte in raw:
        if byte:
            break
        encoded = "1" + encoded
    return encoded
