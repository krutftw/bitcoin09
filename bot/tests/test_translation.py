from __future__ import annotations

import http.server
import asyncio
import io
import json
import logging
import multiprocessing
import socket
import threading
import time
import unittest
from unittest.mock import patch

from bot.otc.translation import (
    DisabledTranslationProvider,
    HTTPTranslationProvider,
    TranslationUnavailable,
    TranslationBusy,
    TranslationExecutor,
    translation_provider_from_environment,
)


def _response_server(
    *,
    status: int = 200,
    content: bytes = b'{"translated_text":"Hello","target_language":"en"}',
    content_type: str = "application/json; charset=utf-8",
    content_length: str | None = None,
) -> tuple[http.server.ThreadingHTTPServer, list[dict[str, object]]]:
    requests: list[dict[str, object]] = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            requests.append(
                {
                    "path": self.path,
                    "authorization": self.headers.get("Authorization"),
                    "accept": self.headers.get("Accept"),
                    "content_type": self.headers.get("Content-Type"),
                    "body": body,
                }
            )
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header(
                "Content-Length",
                str(len(content)) if content_length is None else content_length,
            )
            self.end_headers()
            if content:
                self.wfile.write(content)

        def log_message(self, _format: str, *args) -> None:
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, requests


class TranslationTests(unittest.TestCase):
    def test_url_policy_requires_https_except_numeric_loopback(self) -> None:
        for url in (
            "https://translate.example/v1",
            "http://127.0.0.1:8000/v1",
            "http://127.255.0.9/v1",
            "http://[::1]:8000/v1",
        ):
            with self.subTest(valid=url):
                HTTPTranslationProvider(url, "token")
        for url in (
            "http://localhost:8000/v1",
            "http://example.com/v1",
            "http://192.168.1.1/v1",
            "http://127.0.0.1.example/v1",
            "http://user@127.0.0.1/v1",
            "http://127.0.0.1/v1#fragment",
        ):
            with self.subTest(invalid=url), self.assertRaises(ValueError):
                HTTPTranslationProvider(url, "token")
    def test_disabled_provider_never_touches_network(self) -> None:
        with patch("bot.otc.translation.http.client.HTTPConnection") as connection:
            with self.assertRaises(TranslationUnavailable):
                DisabledTranslationProvider().translate_to_english("source")
        connection.assert_not_called()

    def test_provider_factory_requires_both_url_and_token(self) -> None:
        self.assertIsInstance(
            translation_provider_from_environment({}), DisabledTranslationProvider
        )
        self.assertIsInstance(
            translation_provider_from_environment(
                {"TRANSLATION_API_URL": "https://translate.example"}
            ),
            DisabledTranslationProvider,
        )
        self.assertIsInstance(
            translation_provider_from_environment(
                {
                    "TRANSLATION_API_URL": "https://translate.example/v1",
                    "TRANSLATION_API_TOKEN": "secret-token",
                }
            ),
            HTTPTranslationProvider,
        )

    def test_http_worker_posts_exact_english_request(self) -> None:
        server, requests = _response_server()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        provider = HTTPTranslationProvider(
            f"http://127.0.0.1:{server.server_port}/v1?mode=strict",
            "secret-token",
        )
        self.assertEqual(provider.translate_to_english("Bonjour"), "Hello")
        self.assertEqual(len(requests), 1)
        request = requests[0]
        self.assertEqual(request["path"], "/v1?mode=strict")
        self.assertEqual(request["authorization"], "Bearer secret-token")
        self.assertEqual(request["accept"], "application/json")
        self.assertEqual(request["content_type"], "application/json; charset=utf-8")
        self.assertEqual(
            json.loads(request["body"]),
            {"text": "Bonjour", "target_language": "en"},
        )
        with self.assertRaises(ValueError):
            provider.translate_to_english("x" * 2001)

    def test_worker_rejects_status_redirect_oversize_and_loose_json(self) -> None:
        cases = (
            {"status": 302, "content": b"{}"},
            {"status": 500, "content": b"{}"},
            {"content": b"", "content_length": "65537"},
            {
                "content": b'{"translated_text":"Hello","target_language":"fr"}'
            },
            {
                "content": b'{"translated_text":"Hello","target_language":"en","extra":1}'
            },
            {"content": b'["Hello"]'},
        )
        for case in cases:
            server, _ = _response_server(**case)
            try:
                provider = HTTPTranslationProvider(
                    f"http://127.0.0.1:{server.server_port}/translate",
                    "secret-token",
                )
                with self.assertRaises(TranslationUnavailable):
                    provider.translate_to_english("PRIVATE SOURCE")
            finally:
                server.shutdown()
                server.server_close()

    def test_failures_never_log_source_or_token(self) -> None:
        stream = io.StringIO()
        handler = logging.StreamHandler(stream)
        root = logging.getLogger()
        root.addHandler(handler)
        self.addCleanup(root.removeHandler, handler)
        provider = HTTPTranslationProvider(
            "http://127.0.0.1:1/translate", "PRIVATE TOKEN"
        )
        with self.assertRaises(TranslationUnavailable) as caught:
            provider.translate_to_english("PRIVATE SOURCE")
        combined = stream.getvalue() + str(caught.exception)
        self.assertNotIn("PRIVATE SOURCE", combined)
        self.assertNotIn("PRIVATE TOKEN", combined)

    def test_worker_requires_utf8_unique_exact_json(self) -> None:
        cases = (
            {
                "content": '{"translated_text":"Hello","target_language":"en"}'.encode(
                    "utf-16"
                )
            },
            {
                "content": b'{"translated_text":"Hello","translated_text":"Again","target_language":"en"}'
            },
            {
                "content": b'{"translated_text":"Hello","target_language":"en"} trailing'
            },
            {"content_type": "application/json; charset=utf-16"},
            {"content_type": "application/json; charset=utf-8; version=1"},
        )
        for case in cases:
            server, _ = _response_server(**case)
            try:
                provider = HTTPTranslationProvider(
                    f"http://127.0.0.1:{server.server_port}/translate",
                    "secret-token",
                )
                with self.assertRaises(TranslationUnavailable):
                    provider.translate_to_english("source")
            finally:
                server.shutdown()
                server.server_close()

    def test_repeated_fast_workers_are_reaped(self) -> None:
        server, requests = _response_server()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        before = {child.pid for child in multiprocessing.active_children()}
        provider = HTTPTranslationProvider(
            f"http://127.0.0.1:{server.server_port}/translate", "secret-token"
        )
        for _ in range(10):
            self.assertEqual(provider.translate_to_english("Bonjour"), "Hello")
        self.assertEqual(len(requests), 10)
        self.assertEqual(
            [
                child
                for child in multiprocessing.active_children()
                if child.pid not in before and child.is_alive()
            ],
            [],
        )


class TranslationExecutorTests(unittest.IsolatedAsyncioTestCase):
    async def test_aclose_does_not_block_the_event_loop(self) -> None:
        executor = TranslationExecutor(
            DisabledTranslationProvider(), max_workers=1, max_admitted=1
        )
        event_loop_progressed = threading.Event()
        release_shutdown = threading.Event()
        blocked = []

        def observe_progress() -> None:
            blocked.append(not event_loop_progressed.wait(0.2))
            release_shutdown.set()

        def blocking_shutdown() -> None:
            if not release_shutdown.wait(2):
                raise RuntimeError("test shutdown release timed out")

        observer = threading.Thread(target=observe_progress)
        observer.start()
        try:
            with patch.object(executor, "shutdown", side_effect=blocking_shutdown):
                closing = asyncio.create_task(executor.aclose())
                await asyncio.sleep(0)
                event_loop_progressed.set()
                await closing
        finally:
            release_shutdown.set()
            observer.join(2)
            executor.shutdown()
        self.assertEqual(blocked, [False])

    async def test_executor_is_bounded_busy_and_default_work_stays_responsive(self) -> None:
        started = threading.Event()
        release = threading.Event()

        class BlockingProvider:
            def translate_to_english(self, text: str) -> str:
                started.set()
                if not release.wait(5):
                    raise RuntimeError("test release timed out")
                return "English"

        executor = TranslationExecutor(
            BlockingProvider(), max_workers=1, max_admitted=1
        )
        self.addAsyncCleanup(executor.aclose)
        owner = asyncio.create_task(executor.translate_to_english("source"))
        self.assertTrue(await asyncio.to_thread(started.wait, 2))
        with self.assertRaises(TranslationBusy):
            await executor.translate_to_english("flood")
        responsive = await asyncio.wait_for(
            asyncio.to_thread(lambda: "custody-responsive"), timeout=0.5
        )
        self.assertEqual(responsive, "custody-responsive")
        release.set()
        self.assertEqual(await owner, "English")

    async def test_cancelled_awaiter_holds_admission_until_worker_really_finishes(
        self,
    ) -> None:
        started = threading.Event()
        release = threading.Event()

        class BlockingProvider:
            def translate_to_english(self, text: str) -> str:
                started.set()
                if not release.wait(5):
                    raise RuntimeError("test release timed out")
                return "English"

        executor = TranslationExecutor(
            BlockingProvider(), max_workers=1, max_admitted=1
        )
        self.addAsyncCleanup(executor.aclose)
        cancelled = asyncio.create_task(executor.translate_to_english("owner"))
        self.assertTrue(await asyncio.to_thread(started.wait, 2))
        cancelled.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await cancelled
        with self.assertRaises(TranslationBusy):
            await executor.translate_to_english("must remain busy")
        release.set()
        deadline = time.monotonic() + 2
        while True:
            try:
                self.assertEqual(
                    await executor.translate_to_english("after completion"), "English"
                )
                break
            except TranslationBusy:
                if time.monotonic() >= deadline:
                    self.fail("translation admission was not released on completion")
                await asyncio.sleep(0.01)

    async def test_submit_failure_releases_exactly_one_admission_slot(self) -> None:
        executor = TranslationExecutor(
            DisabledTranslationProvider(), max_workers=1, max_admitted=1
        )
        self.addAsyncCleanup(executor.aclose)
        with patch.object(
            executor._executor,
            "submit",
            side_effect=OSError("executor rejected submit"),
        ), self.assertRaises(TranslationUnavailable):
            await executor.translate_to_english("source")
        self.assertTrue(executor._admission.acquire(blocking=False))
        self.assertFalse(executor._admission.acquire(blocking=False))
        executor._admission.release()

    async def test_shutdown_failure_closes_executor_without_over_releasing(self) -> None:
        executor = TranslationExecutor(
            DisabledTranslationProvider(), max_workers=1, max_admitted=1
        )
        with patch.object(
            executor._executor,
            "shutdown",
            side_effect=RuntimeError("shutdown failed"),
        ), self.assertRaisesRegex(RuntimeError, "shutdown failed"):
            executor.shutdown()
        with self.assertRaises(TranslationUnavailable):
            await executor.translate_to_english("after shutdown")
        self.assertTrue(executor._admission.acquire(blocking=False))
        self.assertFalse(executor._admission.acquire(blocking=False))
        executor._admission.release()

    def test_real_body_slow_drip_is_cancelled_and_worker_reaped(self) -> None:
        body = b'{"translated_text":"Hello","target_language":"en"}'

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self) -> None:
                self.send_response(200)
                self.send_header("Content-Type", "application/json; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                time.sleep(9.0)
                try:
                    self.wfile.write(body[:1])
                    self.wfile.flush()
                    time.sleep(2.0)
                    self.wfile.write(body[1:])
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionAbortedError, ConnectionResetError):
                    pass

            def log_message(self, _format: str, *args) -> None:
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        before = {child.pid for child in multiprocessing.active_children()}
        provider = HTTPTranslationProvider(
            f"http://127.0.0.1:{server.server_port}/translate", "secret-token"
        )
        started = time.monotonic()
        with self.assertRaises(TranslationUnavailable):
            provider.translate_to_english("slow source")
        self.assertLess(time.monotonic() - started, 10.5)
        self.assertEqual(
            [
                child
                for child in multiprocessing.active_children()
                if child.pid not in before and child.is_alive()
            ],
            [],
        )

    def test_real_header_drip_is_cancelled_and_worker_reaped(self) -> None:
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        self.addCleanup(listener.close)

        def serve() -> None:
            connection, _ = listener.accept()
            with connection:
                received = b""
                while b"\r\n\r\n" not in received:
                    chunk = connection.recv(4096)
                    if not chunk:
                        return
                    received += chunk
                time.sleep(9.0)
                try:
                    connection.sendall(b"HTTP/1.1 200 OK\r\n")
                except OSError:
                    return
                time.sleep(2.0)
                try:
                    connection.sendall(
                        b"Content-Type: application/json; charset=utf-8\r\n"
                        b"Content-Length: 54\r\n\r\n"
                    )
                except OSError:
                    return
                time.sleep(7.0)
                try:
                    connection.sendall(
                        b'{"translated_text":"Hello","target_language":"en"}'
                    )
                except OSError:
                    pass

        threading.Thread(target=serve, daemon=True).start()
        before = {child.pid for child in multiprocessing.active_children()}
        provider = HTTPTranslationProvider(
            f"http://127.0.0.1:{listener.getsockname()[1]}/translate",
            "secret-token",
        )
        started = time.monotonic()
        with self.assertRaises(TranslationUnavailable):
            provider.translate_to_english("slow headers")
        self.assertLess(time.monotonic() - started, 10.5)
        self.assertEqual(
            [
                child
                for child in multiprocessing.active_children()
                if child.pid not in before and child.is_alive()
            ],
            [],
        )


if __name__ == "__main__":
    unittest.main()
