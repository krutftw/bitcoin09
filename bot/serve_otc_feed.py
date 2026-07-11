#!/usr/bin/env python3
"""Serve the sanitized Bitcoin 09 OTC bot feed."""

from __future__ import annotations

import json
import copy
import math
import os
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from bot.otc.public_feed import (
    DEFAULT_PUBLIC_FEED_PATH,
    MAX_FEED_BYTES,
    health_is_operational,
    public_feed_projection,
)

FEED_PATH = Path(os.environ.get("OTC_FEED_PATH", str(DEFAULT_PUBLIC_FEED_PATH)))
LISTEN = "127.0.0.1"
PORT = int(os.environ.get("OTC_FEED_PORT", "8019"))
MAX_FEED_AGE_SECONDS = int(os.environ.get("OTC_FEED_MAX_AGE_SECONDS", "120"))
MAX_FUTURE_SKEW_SECONDS = int(
    os.environ.get("OTC_FEED_MAX_FUTURE_SKEW_SECONDS", "5")
)
MAX_HTTP_WORKERS = int(os.environ.get("OTC_FEED_MAX_WORKERS", "8"))
SOCKET_TIMEOUT_SECONDS = float(
    os.environ.get("OTC_FEED_SOCKET_TIMEOUT_SECONDS", "5")
)
if MAX_FEED_AGE_SECONDS < 1:
    raise ValueError("OTC_FEED_MAX_AGE_SECONDS must be positive")
if MAX_FUTURE_SKEW_SECONDS < 0:
    raise ValueError("OTC_FEED_MAX_FUTURE_SKEW_SECONDS must be non-negative")
if not 1 <= MAX_HTTP_WORKERS <= 64:
    raise ValueError("OTC_FEED_MAX_WORKERS must be 1-64")
if not math.isfinite(SOCKET_TIMEOUT_SECONDS) or not 0.1 <= SOCKET_TIMEOUT_SECONDS <= 30:
    raise ValueError("OTC_FEED_SOCKET_TIMEOUT_SECONDS must be 0.1-30")


def _unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("feed contains duplicate JSON keys")
        result[key] = value
    return result


class ValidatedFeedCache:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._key: tuple[int, int, int, int, int] | None = None
        self._payload: dict[str, object] | None = None

    @staticmethod
    def _stat_key(stat: os.stat_result) -> tuple[int, int, int, int, int]:
        return (
            stat.st_dev,
            stat.st_ino,
            stat.st_size,
            stat.st_mtime_ns,
            stat.st_ctime_ns,
        )

    def load(self, path: Path) -> dict[str, object]:
        with self._lock:
            for attempt in range(10):
                try:
                    with path.open("rb") as handle:
                        opened = os.fstat(handle.fileno())
                        key = self._stat_key(opened)
                        if self._key == key and self._payload is not None:
                            return copy.deepcopy(self._payload)
                        encoded = handle.read(MAX_FEED_BYTES + 1)
                        after = os.fstat(handle.fileno())
                    break
                except PermissionError:
                    if os.name != "nt" or attempt == 9:
                        raise
                    time.sleep(0.001)
            if key != self._stat_key(after):
                raise ValueError("feed changed while being read")
            if len(encoded) > MAX_FEED_BYTES:
                raise ValueError("feed exceeds maximum size")
            decoded = json.loads(encoded, object_pairs_hook=_unique_object)
            if type(decoded) is not dict:
                raise ValueError("feed root must be an object")
            public_feed_projection(decoded)
            if type(decoded.get("health")) is not dict:
                raise ValueError("feed health must be an object")
            self._key = key
            self._payload = decoded
            return copy.deepcopy(decoded)


_FEED_CACHE = ValidatedFeedCache()


def read_feed(
    path: Path = FEED_PATH,
    *,
    now: int | None = None,
    max_age_seconds: int = MAX_FEED_AGE_SECONDS,
    max_future_skew_seconds: int = MAX_FUTURE_SKEW_SECONDS,
    cache: ValidatedFeedCache = _FEED_CACHE,
) -> dict[str, object]:
    decoded = cache.load(path)
    if now is None:
        now = int(time.time())
    timestamp = decoded["health_timestamp"]
    if (
        type(now) is not int
        or type(max_age_seconds) is not int
        or max_age_seconds < 1
        or type(max_future_skew_seconds) is not int
        or max_future_skew_seconds < 0
        or timestamp - now > max_future_skew_seconds
        or now - timestamp > max_age_seconds
    ):
        raise ValueError("feed timestamp is stale or in the future")
    return decoded


def build_health_response(
    payload: dict[str, object],
    *,
    now: int | None = None,
    max_future_skew_seconds: int = MAX_FUTURE_SKEW_SECONDS,
) -> dict[str, object]:
    if now is None:
        now = int(time.time())
    timestamp = payload.get("health_timestamp")
    health = payload.get("health")
    if type(now) is not int or type(timestamp) is not int or type(health) is not dict:
        raise ValueError("feed health is invalid")
    if (
        type(max_future_skew_seconds) is not int
        or max_future_skew_seconds < 0
        or timestamp - now > max_future_skew_seconds
    ):
        raise ValueError("feed timestamp is in the future")
    result = dict(health)
    result["feed_age_seconds"] = max(0, now - timestamp)
    return result


def health_http_status(
    payload: dict[str, object],
    *,
    now: int | None = None,
    max_age_seconds: int = MAX_FEED_AGE_SECONDS,
    max_future_skew_seconds: int = MAX_FUTURE_SKEW_SECONDS,
) -> int:
    if now is None:
        now = int(time.time())
    timestamp = payload.get("health_timestamp")
    if (
        type(now) is not int
        or type(timestamp) is not int
        or type(max_age_seconds) is not int
        or max_age_seconds < 1
        or type(max_future_skew_seconds) is not int
        or max_future_skew_seconds < 0
        or timestamp - now > max_future_skew_seconds
        or now - timestamp > max_age_seconds
    ):
        return 503
    try:
        build_health_response(
            payload, now=now, max_future_skew_seconds=max_future_skew_seconds
        )
        return 200 if health_is_operational(payload) else 503
    except Exception:
        return 503


def _encoded(value: object) -> bytes:
    return (
        json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)
        + "\n"
    ).encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    server_version = "btc09-otc-feed/1.0"

    def do_GET(self) -> None:
        if self.path.split("?", 1)[0] == "/healthz":
            try:
                feed = read_feed()
                status = health_http_status(feed)
                payload = _encoded(build_health_response(feed))
            except Exception:
                self._unavailable()
                return
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(payload)
            return

        if self.path.split("?", 1)[0] != "/otc-bot-feed.json":
            self.send_error(404)
            return

        try:
            payload = _encoded(public_feed_projection(read_feed()))
        except Exception:
            self._unavailable(cors=True)
            return

        self.send_response(200)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(payload)

    def _unavailable(self, *, cors: bool = False) -> None:
        self.send_response(503)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        if cors:
            self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(b'{"error":"feed unavailable"}\n')

    def log_message(self, fmt: str, *args) -> None:
        print("%s - %s" % (self.address_string(), fmt % args))


class BoundedThreadingHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    block_on_close = True
    request_queue_size = 16

    def __init__(
        self,
        server_address,
        handler_class,
        *,
        max_workers: int,
        socket_timeout_seconds: float = SOCKET_TIMEOUT_SECONDS,
    ) -> None:
        if not 1 <= max_workers <= 64:
            raise ValueError("feed worker bound must be 1-64")
        if (
            type(socket_timeout_seconds) not in {int, float}
            or not math.isfinite(socket_timeout_seconds)
            or not 0.1 <= socket_timeout_seconds <= 30
        ):
            raise ValueError("feed socket timeout must be 0.1-30 seconds")
        self._worker_slots = threading.BoundedSemaphore(max_workers)
        self._socket_timeout_seconds = float(socket_timeout_seconds)
        super().__init__(server_address, handler_class)

    def process_request(self, request, client_address) -> None:
        try:
            request.settimeout(self._socket_timeout_seconds)
        except BaseException:
            self.shutdown_request(request)
            raise
        if not self._worker_slots.acquire(blocking=False):
            try:
                request.settimeout(0.05)
                received = b""
                while len(received) < 8_192 and b"\r\n\r\n" not in received:
                    chunk = request.recv(min(4_096, 8_192 - len(received)))
                    if not chunk:
                        break
                    received += chunk
                request.sendall(
                    b"HTTP/1.1 503 Service Unavailable\r\n"
                    b"Content-Type: application/json; charset=utf-8\r\n"
                    b"Cache-Control: no-store\r\n"
                    b"Access-Control-Allow-Origin: *\r\n"
                    b"Connection: close\r\n"
                    b"Content-Length: 24\r\n\r\n"
                    b'{"error":"server busy"}\n'
                )
                request.shutdown(socket.SHUT_WR)
            except OSError:
                pass
            finally:
                self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self._worker_slots.release()
            self.shutdown_request(request)
            raise

    def process_request_thread(self, request, client_address) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._worker_slots.release()


def main() -> None:
    httpd = BoundedThreadingHTTPServer(
        (LISTEN, PORT),
        Handler,
        max_workers=MAX_HTTP_WORKERS,
        socket_timeout_seconds=SOCKET_TIMEOUT_SECONDS,
    )
    print(f"serving {FEED_PATH} on http://{LISTEN}:{PORT}/otc-bot-feed.json")
    httpd.serve_forever()


if __name__ == "__main__":
    main()
