from __future__ import annotations

import copy
import hashlib
import json
import os
import threading
import time
import unittest
from contextlib import contextmanager
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

from bot.otc.explorer import (
    Explorer,
    ExplorerProtocolError,
    ExplorerTransportError,
    Tip,
    TipMismatch,
)


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
    return json.dumps(value, separators=(",", ":"), allow_nan=True).encode("utf-8")


def tip_body(*, tip_hash: str = h(1), height: int = 10) -> dict[str, object]:
    return {
        "schema_version": 1,
        "network": "btc09-regtest",
        "tip": {"hash": tip_hash, "height": height},
    }


def output_body(
    *,
    owner: str,
    txid: str = h(20),
    vout: int = 0,
    transaction_index: int = 1,
    amount: int = 100,
    block_hash: str = h(30),
    block_height: int = 5,
    tip_hash: str = h(1),
    tip_height: int = 10,
    coinbase: bool = False,
    mature: bool = True,
    spent_by: object = None,
) -> dict[str, object]:
    return {
        "schema_version": 1,
        "network": "btc09-regtest",
        "address": owner,
        "complete": True,
        "tip": {"hash": tip_hash, "height": tip_height},
        "outputs": [
            {
                "txid": txid,
                "transaction_index": transaction_index,
                "vout": vout,
                "amount_units": amount,
                "block": {"hash": block_hash, "height": block_height},
                "confirmations": tip_height - block_height + 1,
                "coinbase": coinbase,
                "mature": mature,
                "spent_by": spent_by,
            }
        ],
    }


@dataclass
class ResponseSpec:
    status: int = 200
    body: bytes = b"{}"
    headers: list[tuple[str, str]] = field(
        default_factory=lambda: [("Content-Type", "application/json; charset=utf-8")]
    )
    chunks: list[bytes] | None = None
    chunk_delay: float = 0.0
    chunked: bool = False


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self) -> None:  # noqa: N802 - stdlib hook name
        server = self.server
        server.paths.append(self.path)  # type: ignore[attr-defined]
        server.request_headers.append(dict(self.headers))  # type: ignore[attr-defined]
        try:
            spec = server.responses.pop(0)  # type: ignore[attr-defined]
        except IndexError:
            spec = ResponseSpec(status=500)
        self.send_response(spec.status)
        header_names = [name.lower() for name, _ in spec.headers]
        for name, value in spec.headers:
            self.send_header(name, value)
        if spec.chunked:
            self.send_header("Transfer-Encoding", "chunked")
        elif "content-length" not in header_names:
            length = len(spec.body) if spec.chunks is None else sum(map(len, spec.chunks))
            self.send_header("Content-Length", str(length))
        self.end_headers()
        try:
            if spec.chunks is not None:
                for chunk in spec.chunks:
                    if spec.chunked:
                        self.wfile.write(f"{len(chunk):x}\r\n".encode("ascii"))
                        self.wfile.write(chunk + b"\r\n")
                    else:
                        self.wfile.write(chunk)
                    self.wfile.flush()
                    if spec.chunk_delay:
                        time.sleep(spec.chunk_delay)
                if spec.chunked:
                    self.wfile.write(b"0\r\n\r\n")
                    self.wfile.flush()
            else:
                self.wfile.write(spec.body)
                self.wfile.flush()
        except OSError:
            pass

    def log_message(self, *_args: object) -> None:
        return


@contextmanager
def server_for(*responses: ResponseSpec):
    server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    server.responses = list(responses)
    server.paths = []
    server.request_headers = []
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server, f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


class ExplorerTests(unittest.TestCase):
    def explorer(self, base: str, **kwargs: object) -> Explorer:
        return Explorer(base, network="btc09-regtest", **kwargs)

    def test_constructor_accepts_only_canonical_numeric_loopback(self) -> None:
        Explorer("http://127.0.0.1:1", network="btc09-mainnet")
        Explorer("http://[::1]:65535", network="btc09-regtest")
        invalid = [
            "https://127.0.0.1:443",
            "HTTP://127.0.0.1:80",
            "http://127.0.0.1",
            "http://127.0.0.1:080",
            "http://127.0.0.1:65536",
            "http://127.0.0.1:80/",
            "http://user@127.0.0.1:80",
            "http://127.0.0.1:80?x=1",
            "http://127.0.0.1:80#x",
            "http://127.1:80",
            "http://2130706433:80",
            "http://0177.0.0.1:80",
            "http://0x7f000001:80",
            "http://[::ffff:127.0.0.1]:80",
            "http://[::1%25lo0]:80",
            " http://127.0.0.1:80",
            "http://127.0.0.1:80\x00",
        ]
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(ValueError):
                Explorer(value, network="btc09-regtest")
        with self.assertRaises(ValueError):
            Explorer("http://127.0.0.1:80", network="regtest")
        with self.assertRaises(ValueError):
            Explorer("http://127.0.0.1:80", network="btc09-regtest", read_timeout=True)

    def test_tip_is_direct_no_proxy_and_does_not_follow_redirects(self) -> None:
        valid = ResponseSpec(body=compact(tip_body()))
        with server_for(valid) as (server, base):
            with mock.patch.dict(
                os.environ,
                {"HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1"},
            ):
                got = self.explorer(base).tip()
            self.assertEqual(got, Tip(hash=h(1), height=10))
            self.assertEqual(server.paths, ["/api/v1/tip"])
            self.assertEqual(server.request_headers[0].get("Accept-Encoding"), "identity")

        redirect = ResponseSpec(
            status=302,
            body=compact({"schema_version": 1}),
            headers=[
                ("Content-Type", "application/json; charset=utf-8"),
                ("Location", "http://169.254.169.254/latest/meta-data/"),
            ],
        )
        with server_for(redirect) as (server, base):
            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).tip()
            self.assertEqual(server.paths, ["/api/v1/tip"])

    def test_tip_rejects_strict_transport_and_json_forgeries(self) -> None:
        cases = {
            "duplicate nested key": ResponseSpec(
                body=(
                    b'{"schema_version":1,"network":"btc09-regtest","tip":'
                    b'{"hash":"' + h(1).encode() + b'","hash":"' + h(2).encode() + b'","height":10}}'
                )
            ),
            "extra key": ResponseSpec(body=compact({**tip_body(), "extra": 1})),
            "bool height": ResponseSpec(body=compact(tip_body(height=True))),
            "nan": ResponseSpec(
                body=b'{"schema_version":1,"network":"btc09-regtest","tip":{"hash":"'
                + h(1).encode()
                + b'","height":NaN}}'
            ),
            "trailing value": ResponseSpec(body=compact(tip_body()) + b"{}"),
            "bom": ResponseSpec(body=b"\xef\xbb\xbf" + compact(tip_body())),
            "bad utf8": ResponseSpec(body=b"\xff"),
            "deep nesting": ResponseSpec(
                body=b"[" * 100_000 + b"0" + b"]" * 100_000
            ),
            "bad charset": ResponseSpec(
                body=compact(tip_body()),
                headers=[("Content-Type", "application/json; charset=iso-8859-1")],
            ),
            "gzip": ResponseSpec(
                body=compact(tip_body()),
                headers=[
                    ("Content-Type", "application/json; charset=utf-8"),
                    ("Content-Encoding", "gzip"),
                ],
            ),
            "duplicate content type": ResponseSpec(
                body=compact(tip_body()),
                headers=[
                    ("Content-Type", "application/json"),
                    ("Content-Type", "application/json"),
                ],
            ),
            "oversized declared body": ResponseSpec(
                body=b"",
                headers=[
                    ("Content-Type", "application/json; charset=utf-8"),
                    ("Content-Length", str((4 << 20) + 1)),
                ],
            ),
        }
        for name, response in cases.items():
            with self.subTest(name=name), server_for(response) as (_server, base):
                with self.assertRaises(ExplorerProtocolError):
                    self.explorer(base).tip()

    def test_total_deadline_stops_slow_drip_and_chunked_cap_plus_one(self) -> None:
        body = compact(tip_body())
        slow = ResponseSpec(chunks=[bytes([byte]) for byte in body], chunk_delay=0.02)
        with server_for(slow) as (_server, base):
            with self.assertRaises(ExplorerTransportError):
                self.explorer(
                    base, connect_timeout=0.2, read_timeout=0.2, total_timeout=0.08
                ).tip()

        oversized = ResponseSpec(chunks=[b"x" * 65, b"y" * 64], chunked=True)
        with server_for(oversized) as (_server, base):
            explorer = self.explorer(base)
            explorer._max_response_bytes = 128
            with self.assertRaises(ExplorerProtocolError):
                explorer.tip()

    def test_outputs_validate_exact_schema_and_flatten_for_store(self) -> None:
        owner = address(1)
        spent = {
            "txid": h(40),
            "input_index": 2,
            "block": {"hash": h(41), "height": 9},
        }
        body = output_body(owner=owner, spent_by=spent)
        with server_for(ResponseSpec(body=compact(body))) as (server, base):
            snapshot = self.explorer(base).outputs(owner, Tip(hash=h(1), height=10))
            self.assertEqual(snapshot.address, owner)
            self.assertEqual(snapshot.outputs[0].spent_by.input_index, 2)
            flattened = snapshot.to_store_snapshot()
            self.assertEqual(flattened["tip_hash"], h(1))
            self.assertNotIn("tip", flattened)
            self.assertEqual(flattened["outputs"][0]["block_hash"], h(30))
            self.assertNotIn("block", flattened["outputs"][0])
            self.assertEqual(flattened["outputs"][0]["spent_by"]["vin"], 2)
            self.assertEqual(
                server.paths,
                [
                    f"/api/v1/address/{owner}/outputs?expected_tip_hash={h(1)}&expected_tip_height=10"
                ],
            )

        mismatch = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "address": owner,
            "complete": False,
            "tip": {"hash": h(2), "height": 11},
        }
        with server_for(ResponseSpec(status=409, body=compact(mismatch))) as (_server, base):
            with self.assertRaises(TipMismatch) as caught:
                self.explorer(base).outputs(owner, Tip(hash=h(1), height=10))
            self.assertEqual(caught.exception.tip, Tip(hash=h(2), height=11))

        with server_for(ResponseSpec(status=409, body=compact(mismatch))) as (_server, base):
            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).outputs(owner)

        matching = copy.deepcopy(mismatch)
        matching["tip"] = {"hash": h(1), "height": 10}
        with server_for(ResponseSpec(status=409, body=compact(matching))) as (_server, base):
            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).outputs(owner, Tip(hash=h(1), height=10))

        conflicting_height = copy.deepcopy(mismatch)
        conflicting_height["tip"] = {"hash": h(1), "height": 11}
        with server_for(
            ResponseSpec(status=409, body=compact(conflicting_height))
        ) as (_server, base):
            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).outputs(owner, Tip(hash=h(1), height=10))

    def test_outputs_reject_malformed_evidence_and_order(self) -> None:
        owner = address(2)
        valid = output_body(owner=owner)
        cases: dict[str, dict[str, object]] = {}
        changed = copy.deepcopy(valid)
        changed["complete"] = False
        cases["incomplete"] = changed
        changed = copy.deepcopy(valid)
        changed["network"] = "btc09-mainnet"
        cases["wrong network"] = changed
        changed = copy.deepcopy(valid)
        changed["address"] = address(3)
        cases["wrong address"] = changed
        changed = copy.deepcopy(valid)
        changed["outputs"][0]["amount_units"] = True
        cases["bool amount"] = changed
        changed = copy.deepcopy(valid)
        changed["outputs"][0]["confirmations"] = 5
        cases["bad confirmations"] = changed
        changed = copy.deepcopy(valid)
        changed["outputs"][0]["block"]["height"] = 10
        changed["outputs"][0]["confirmations"] = 1
        cases["tip height with wrong hash"] = changed
        changed = copy.deepcopy(valid)
        changed["outputs"][0]["coinbase"] = True
        changed["outputs"][0]["mature"] = False
        cases["bad maturity"] = changed
        changed = copy.deepcopy(valid)
        second = copy.deepcopy(changed["outputs"][0])
        second["transaction_index"] = 0
        second["vout"] = 1
        second["txid"] = h(21)
        changed["outputs"].append(second)
        cases["unsorted"] = changed
        changed = copy.deepcopy(valid)
        changed["outputs"].append(copy.deepcopy(changed["outputs"][0]))
        cases["duplicate outpoint"] = changed
        changed = copy.deepcopy(valid)
        second = copy.deepcopy(changed["outputs"][0])
        second["txid"] = h(21)
        second["vout"] = 1
        changed["outputs"].append(second)
        cases["conflicting transaction position"] = changed
        changed = copy.deepcopy(valid)
        spender = {
            "txid": h(40),
            "input_index": 0,
            "block": {"hash": h(41), "height": 9},
        }
        changed["outputs"][0]["spent_by"] = spender
        second = copy.deepcopy(changed["outputs"][0])
        second["txid"] = h(21)
        second["transaction_index"] = 2
        second["vout"] = 1
        changed["outputs"].append(second)
        cases["duplicate spending input"] = changed
        changed = copy.deepcopy(valid)
        changed["outputs"][0]["extra"] = 1
        cases["extra field"] = changed

        for name, body in cases.items():
            with self.subTest(name=name), server_for(ResponseSpec(body=compact(body))) as (
                _server,
                base,
            ):
                with self.assertRaises(ExplorerProtocolError):
                    self.explorer(base).outputs(owner)

    def test_block_and_transaction_statuses_are_typed_and_anchored(self) -> None:
        side = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "found": True,
            "block": {"hash": h(50), "height": 12, "canonical": False},
            "tip": {"hash": h(1), "height": 10},
        }
        missing = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "found": False,
            "block": {"hash": h(51), "height": None, "canonical": False},
            "tip": {"hash": h(1), "height": 10},
        }
        confirmed = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "txid": h(60),
            "status": "confirmed",
            "block": {"hash": h(61), "height": 9},
            "confirmations": 2,
            "tip": {"hash": h(1), "height": 10},
        }
        unknown = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "txid": h(62),
            "status": "unknown",
            "block": None,
            "confirmations": 0,
            "tip": {"hash": h(1), "height": 10},
        }
        with server_for(
            ResponseSpec(body=compact(side)),
            ResponseSpec(status=404, body=compact(missing)),
            ResponseSpec(body=compact(confirmed)),
            ResponseSpec(body=compact(unknown)),
        ) as (_server, base):
            explorer = self.explorer(base)
            self.assertFalse(explorer.block(h(50)).canonical)
            self.assertFalse(explorer.block(h(51)).found)
            self.assertEqual(explorer.transaction(h(60)).status, "confirmed")
            self.assertEqual(explorer.transaction(h(62)).status, "unknown")

        bad_tip_block = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "found": True,
            "block": {"hash": h(1), "height": 10, "canonical": False},
            "tip": {"hash": h(1), "height": 10},
        }
        bad_tip_tx = {
            "schema_version": 1,
            "network": "btc09-regtest",
            "txid": h(63),
            "status": "confirmed",
            "block": {"hash": h(64), "height": 10},
            "confirmations": 1,
            "tip": {"hash": h(1), "height": 10},
        }
        missing_tip = copy.deepcopy(missing)
        missing_tip["block"]["hash"] = h(1)
        with server_for(
            ResponseSpec(body=compact(bad_tip_block)),
            ResponseSpec(body=compact(bad_tip_tx)),
            ResponseSpec(status=404, body=compact(missing_tip)),
        ) as (_server, base):
            explorer = self.explorer(base)
            with self.assertRaises(ExplorerProtocolError):
                explorer.block(h(1))
            with self.assertRaises(ExplorerProtocolError):
                explorer.transaction(h(63))
            with self.assertRaises(ExplorerProtocolError):
                explorer.block(h(1))

    def test_batch_is_all_or_nothing_sorted_bounded_and_store_ready(self) -> None:
        a, b = address(10), address(11)
        pre = tip_body()
        a_body = output_body(owner=a, txid=h(70), vout=0)
        b_body = output_body(owner=b, txid=h(71), vout=1)
        with server_for(
            ResponseSpec(body=compact(pre)),
            ResponseSpec(body=compact(a_body)),
            ResponseSpec(body=compact(b_body)),
            ResponseSpec(body=compact(pre)),
        ) as (server, base):
            calls = 0

            def watched() -> list[str]:
                nonlocal calls
                calls += 1
                return [b, a] if calls == 1 else [a, b]

            batch = self.explorer(base).batch_outputs(watched)
            self.assertEqual(calls, 2)
            self.assertEqual([item.address for item in batch.snapshots], [a, b])
            self.assertEqual(
                server.paths,
                [
                    "/api/v1/tip",
                    f"/api/v1/address/{a}/outputs?expected_tip_hash={h(1)}&expected_tip_height=10",
                    f"/api/v1/address/{b}/outputs?expected_tip_hash={h(1)}&expected_tip_height=10",
                    "/api/v1/tip",
                ],
            )
            flattened = batch.store_snapshots()
            self.assertEqual([item["address"] for item in flattened], [a, b])
            self.assertNotIn("block", flattened[0]["outputs"][0])

        duplicate = copy.deepcopy(a_body)
        duplicate["address"] = b
        with server_for(
            ResponseSpec(body=compact(pre)),
            ResponseSpec(body=compact(a_body)),
            ResponseSpec(body=compact(duplicate)),
            ResponseSpec(body=compact(pre)),
        ) as (_server, base):
            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).batch_outputs(lambda: [a, b])

    def test_batch_rejects_invalid_callback_and_aggregate_limits(self) -> None:
        a, b = address(20), address(21)
        explorer = Explorer("http://127.0.0.1:1", network="btc09-regtest")
        for values in ("not-a-sequence", [a, a], ["not-an-address"]):
            with self.subTest(values=values), self.assertRaises(ExplorerProtocolError):
                explorer.batch_outputs(lambda values=values: values)
        explorer._max_watched_addresses = 1
        with self.assertRaises(ExplorerProtocolError):
            explorer.batch_outputs(lambda: [a, b])

        pre = tip_body()
        with server_for(
            ResponseSpec(body=compact(pre)),
            ResponseSpec(body=compact(output_body(owner=a, txid=h(80)))),
            ResponseSpec(body=compact(output_body(owner=b, txid=h(81)))),
        ) as (_server, base):
            bounded = self.explorer(base)
            bounded._max_batch_outpoints = 1
            with self.assertRaises(ExplorerProtocolError):
                bounded.batch_outputs(lambda: [a, b])

    def test_batch_detects_tip_or_watched_set_change_and_handles_empty_set(self) -> None:
        a = address(12)
        pre = tip_body()
        changed_tip = tip_body(tip_hash=h(2), height=11)
        with server_for(
            ResponseSpec(body=compact(pre)),
            ResponseSpec(body=compact(output_body(owner=a))),
            ResponseSpec(body=compact(changed_tip)),
        ) as (_server, base):
            with self.assertRaises(TipMismatch):
                self.explorer(base).batch_outputs(lambda: [a])

        conflicting_height = tip_body(tip_hash=h(1), height=11)
        with server_for(
            ResponseSpec(body=compact(pre)),
            ResponseSpec(body=compact(output_body(owner=a))),
            ResponseSpec(body=compact(conflicting_height)),
        ) as (_server, base):
            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).batch_outputs(lambda: [a])

        with server_for(
            ResponseSpec(body=compact(pre)),
            ResponseSpec(body=compact(output_body(owner=a))),
            ResponseSpec(body=compact(pre)),
        ) as (_server, base):
            calls = 0

            def changing() -> list[str]:
                nonlocal calls
                calls += 1
                return [a] if calls == 1 else []

            with self.assertRaises(ExplorerProtocolError):
                self.explorer(base).batch_outputs(changing)

        with server_for(
            ResponseSpec(body=compact(pre)), ResponseSpec(body=compact(pre))
        ) as (server, base):
            calls = 0

            def empty() -> list[str]:
                nonlocal calls
                calls += 1
                return []

            batch = self.explorer(base).batch_outputs(empty)
            self.assertEqual(calls, 2)
            self.assertEqual(batch.snapshots, ())
            self.assertEqual(server.paths, ["/api/v1/tip", "/api/v1/tip"])


if __name__ == "__main__":
    unittest.main()
