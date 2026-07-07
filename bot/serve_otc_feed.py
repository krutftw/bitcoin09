#!/usr/bin/env python3
"""Serve the sanitized Bitcoin 09 OTC bot feed."""

from __future__ import annotations

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

FEED_PATH = Path(os.environ.get("OTC_FEED_PATH", "/opt/btc09/public/otc-bot-feed.json"))
LISTEN = os.environ.get("OTC_FEED_LISTEN", "0.0.0.0")
PORT = int(os.environ.get("OTC_FEED_PORT", "8019"))


def empty_feed() -> bytes:
    return (
        json.dumps(
            {
                "schema": 1,
                "generatedAt": None,
                "source": "Bitcoin 09 Discord OTC escrow bot",
                "privacy": "No Discord IDs, usernames, wallet addresses, deposit addresses, or off-chain payment details are published.",
                "summary": {"open": 0, "matched": 0, "disputed": 0, "completed": 0, "releaseFailed": 0},
                "orders": [],
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    ).encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    server_version = "btc09-otc-feed/1.0"

    def do_GET(self) -> None:
        if self.path.split("?", 1)[0] == "/healthz":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(b"ok\n")
            return

        if self.path.split("?", 1)[0] != "/otc-bot-feed.json":
            self.send_error(404)
            return

        try:
            payload = FEED_PATH.read_bytes()
            json.loads(payload)
        except FileNotFoundError:
            payload = empty_feed()
        except Exception as exc:
            self.send_response(503)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(json.dumps({"error": "feed unavailable", "detail": str(exc)[:160]}).encode("utf-8"))
            return

        self.send_response(200)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, fmt: str, *args) -> None:
        print("%s - %s" % (self.address_string(), fmt % args))


def main() -> None:
    httpd = ThreadingHTTPServer((LISTEN, PORT), Handler)
    print(f"serving {FEED_PATH} on http://{LISTEN}:{PORT}/otc-bot-feed.json")
    httpd.serve_forever()


if __name__ == "__main__":
    main()
