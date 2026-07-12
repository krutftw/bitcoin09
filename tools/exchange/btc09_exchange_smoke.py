#!/usr/bin/env python3
"""Read-only smoke test for the BTC09 exchange integration API."""

from __future__ import annotations

import argparse
import http.client
import json
import re
import sys
import urllib.parse


MAX_RESPONSE_BYTES = 4 * 1024 * 1024
HASH_RE = re.compile(r"^[0-9a-f]{64}$")
NETWORKS = {"btc09-mainnet", "btc09-regtest"}


class CheckFailed(RuntimeError):
    pass


class JsonClient:
    def __init__(self, base_url: str, timeout: float):
        parsed = urllib.parse.urlsplit(base_url.rstrip("/"))
        if (
            parsed.scheme not in {"http", "https"}
            or not parsed.hostname
            or parsed.username
            or parsed.password
            or parsed.query
            or parsed.fragment
        ):
            raise CheckFailed("base URL must be a plain HTTP or HTTPS origin")
        connection_type = (
            http.client.HTTPSConnection
            if parsed.scheme == "https"
            else http.client.HTTPConnection
        )
        self.connection = connection_type(parsed.hostname, parsed.port, timeout=timeout)
        self.prefix = parsed.path.rstrip("/")

    def close(self) -> None:
        self.connection.close()

    def get_json(self, path: str) -> dict[str, object]:
        try:
            self.connection.request(
                "GET",
                self.prefix + path,
                headers={
                    "Accept": "application/json",
                    "User-Agent": "btc09-exchange-smoke/1",
                },
            )
            response = self.connection.getresponse()
            raw = response.read(MAX_RESPONSE_BYTES + 1)
        except (OSError, http.client.HTTPException) as exc:
            raise CheckFailed("request failed") from exc
        if response.status != 200:
            raise CheckFailed("request returned HTTP " + str(response.status))
        if len(raw) > MAX_RESPONSE_BYTES:
            raise CheckFailed("response exceeds 4 MiB")
        try:
            value = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise CheckFailed("response is not valid JSON") from exc
        if not isinstance(value, dict):
            raise CheckFailed("response is not a JSON object")
        return value


def _tip(payload: dict[str, object]) -> tuple[str, int, str]:
    network = payload.get("network")
    tip = payload.get("tip")
    if payload.get("schema_version") != 1 or network not in NETWORKS:
        raise CheckFailed("unsupported tip schema or network")
    if not isinstance(tip, dict):
        raise CheckFailed("tip object missing")
    height = tip.get("height")
    tip_hash = tip.get("hash")
    if not isinstance(height, int) or isinstance(height, bool) or height < 0:
        raise CheckFailed("invalid tip height")
    if not isinstance(tip_hash, str) or not HASH_RE.fullmatch(tip_hash):
        raise CheckFailed("invalid tip hash")
    return str(network), height, tip_hash


def check_exchange_api(
    base_url: str,
    address: str | None = None,
    timeout: float = 10.0,
) -> dict[str, object]:
    if timeout <= 0 or timeout > 60:
        raise CheckFailed("timeout must be between 0 and 60 seconds")
    client = JsonClient(base_url, timeout)
    try:
        tip_payload = client.get_json("/api/v1/tip")
        network, height, tip_hash = _tip(tip_payload)
        expected_tip = {"hash": tip_hash, "height": height}

        block_payload = client.get_json("/api/v1/block/" + tip_hash)
        block = block_payload.get("block")
        if (
            block_payload.get("schema_version") != 1
            or block_payload.get("network") != network
            or block_payload.get("found") is not True
            or block_payload.get("tip") != expected_tip
            or not isinstance(block, dict)
            or block.get("hash") != tip_hash
            or block.get("height") != height
            or block.get("canonical") is not True
        ):
            raise CheckFailed("block response does not identify the canonical tip")

        result: dict[str, object] = {
            "ok": True,
            "schema_version": 1,
            "network": network,
            "height": height,
            "tip_hash": tip_hash,
        }
        if address:
            query = urllib.parse.urlencode(
                {"expected_tip_hash": tip_hash, "expected_tip_height": height}
            )
            path_address = urllib.parse.quote(address, safe="")
            output_payload = client.get_json(
                "/api/v1/address/" + path_address + "/outputs?" + query
            )
            outputs = output_payload.get("outputs")
            if (
                output_payload.get("schema_version") != 1
                or output_payload.get("network") != network
                or output_payload.get("address") != address
                or output_payload.get("complete") is not True
                or output_payload.get("tip") != expected_tip
                or not isinstance(outputs, list)
            ):
                raise CheckFailed("address snapshot is not pinned to the requested tip")
            result["address_outputs"] = len(outputs)
        return result
    finally:
        client.close()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Check the read-only BTC09 exchange integration API."
    )
    parser.add_argument(
        "--base-url", default="https://explorer.btc09.org", help="Explorer base URL"
    )
    parser.add_argument("--address", help="Optional BTC09 address for a tip-pinned scan")
    parser.add_argument("--timeout", type=float, default=10.0, help="HTTP timeout in seconds")
    args = parser.parse_args(argv)
    try:
        result = check_exchange_api(args.base_url, args.address, args.timeout)
    except CheckFailed as exc:
        print(
            json.dumps(
                {"error_code": "exchange_api_check_failed", "ok": False},
                separators=(",", ":"),
                sort_keys=True,
            )
        )
        print("BTC09 exchange API check failed: " + str(exc), file=sys.stderr)
        return 1
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
