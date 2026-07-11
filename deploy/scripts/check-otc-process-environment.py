#!/usr/bin/env python3
"""Verify the effective safety-critical environment of the live OTC process."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

MAX_ENVIRON_BYTES = 1_048_576
SERVICE = "btc09-otc-bot.service"
EXPECTED = {
    "OTC_ACCEPTING_ORDERS": "1",
    "DB_PATH": "/var/lib/btc09-otc/otc_bot.db",
    "BTC09_WALLET_PATH": "/var/lib/btc09-otc/wallet-mainnet.json",
    "PUBLIC_FEED_PATH": "/var/lib/btc09-otc-public/otc-bot-feed.json",
    "BTC09_BIN": "/opt/btc09/btc09",
    "BTC09_DATADIR": "/opt/btc09/data",
    "BTC09_NETWORK": "btc09-mainnet",
}


class EnvironmentVerificationError(RuntimeError):
    pass


def verify_environment_blob(blob: bytes) -> None:
    if type(blob) is not bytes or not blob or len(blob) > MAX_ENVIRON_BYTES:
        raise EnvironmentVerificationError("effective environment is invalid")
    found: dict[str, str] = {}
    for entry in blob.rstrip(b"\x00").split(b"\x00"):
        if b"=" not in entry:
            raise EnvironmentVerificationError("effective environment is malformed")
        key_bytes, value_bytes = entry.split(b"=", 1)
        try:
            key = key_bytes.decode("ascii", "strict")
        except UnicodeDecodeError:
            continue
        if key not in EXPECTED:
            continue
        if key in found:
            raise EnvironmentVerificationError("effective environment repeats a pinned key")
        try:
            found[key] = value_bytes.decode("utf-8", "strict")
        except UnicodeDecodeError:
            raise EnvironmentVerificationError("effective environment has an invalid pinned value") from None
    if found != EXPECTED:
        raise EnvironmentVerificationError("effective environment differs from the pinned contract")


def main() -> None:
    if not hasattr(os, "geteuid") or os.geteuid() != 0:
        print("OTC process environment check failed", file=sys.stderr)
        raise SystemExit(1)
    try:
        result = subprocess.run(
            ["systemctl", "show", "-p", "MainPID", "--value", SERVICE],
            check=True,
            capture_output=True,
            text=True,
            timeout=5,
        )
        pid_text = result.stdout.strip()
        if not pid_text.isascii() or not pid_text.isdigit() or int(pid_text) <= 1:
            raise EnvironmentVerificationError("service main PID is invalid")
        environ_path = Path("/proc") / pid_text / "environ"
        with environ_path.open("rb") as handle:
            blob = handle.read(MAX_ENVIRON_BYTES + 1)
        verify_environment_blob(blob)
    except Exception:
        print("OTC process environment check failed", file=sys.stderr)
        raise SystemExit(1)
    print("OTC process environment check passed (7 pinned values)")


if __name__ == "__main__":
    main()
