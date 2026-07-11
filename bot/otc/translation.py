from __future__ import annotations

import http.client
import ipaddress
import asyncio
import concurrent.futures
import json
import multiprocessing
import os
import re
import ssl
import time
import unicodedata
import threading
from collections.abc import Mapping
from multiprocessing.connection import Connection
from typing import Protocol
from urllib.parse import SplitResult, urlsplit

MAX_TRANSLATION_CHARACTERS = 2_000
MAX_TRANSLATION_RESPONSE_BYTES = 65_536
TRANSLATION_DEADLINE_SECONDS = 10.0
_JSON_CONTENT_TYPE = re.compile(
    r"application/json(?:\s*;\s*charset\s*=\s*utf-8)?\s*\Z", re.IGNORECASE
)


class TranslationUnavailable(RuntimeError):
    pass


class TranslationProtocolError(TranslationUnavailable):
    pass


class TranslationBusy(TranslationUnavailable):
    pass


class TranslationProvider(Protocol):
    def translate_to_english(self, text: str) -> str: ...


class DisabledTranslationProvider:
    def translate_to_english(self, text: str) -> str:
        raise TranslationUnavailable("translation is unavailable")


class TranslationExecutor:
    def __init__(
        self,
        provider: TranslationProvider,
        *,
        max_workers: int = 2,
        max_admitted: int = 4,
    ) -> None:
        if max_workers < 1 or max_admitted < max_workers:
            raise ValueError("translation executor bounds are invalid")
        self.provider = provider
        self._executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=max_workers, thread_name_prefix="btc09-translation"
        )
        self._admission = threading.BoundedSemaphore(max_admitted)
        self._state_lock = threading.Lock()
        self._closed = False

    async def translate_to_english(self, text: str) -> str:
        with self._state_lock:
            if self._closed:
                raise TranslationUnavailable("translation is unavailable")
            admitted = self._admission.acquire(blocking=False)
            if admitted:
                try:
                    submitted = self._executor.submit(
                        self.provider.translate_to_english, text
                    )
                except Exception as exc:
                    self._admission.release()
                    raise TranslationUnavailable(
                        "translation is unavailable"
                    ) from exc
                except BaseException:
                    self._admission.release()
                    raise
        if not admitted:
            raise TranslationBusy("translation service is busy")
        submitted.add_done_callback(lambda _future: self._admission.release())
        return await asyncio.wrap_future(submitted)

    def shutdown(self) -> None:
        with self._state_lock:
            already_closed = self._closed
            self._closed = True
        if not already_closed:
            self._executor.shutdown(wait=True, cancel_futures=True)

    async def aclose(self) -> None:
        await asyncio.to_thread(self.shutdown)


def _parse_url(url: object) -> SplitResult:
    if type(url) is not str:
        raise ValueError("translation API URL is invalid")
    parsed = urlsplit(url)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.netloc
        or parsed.hostname is None
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
    ):
        raise ValueError("translation API URL is invalid")
    try:
        port = parsed.port
    except ValueError:
        raise ValueError("translation API URL is invalid") from None
    if port is not None and not 1 <= port <= 65_535:
        raise ValueError("translation API URL is invalid")
    if parsed.scheme == "http":
        try:
            address = ipaddress.ip_address(parsed.hostname)
        except ValueError:
            raise ValueError(
                "HTTP translation is allowed only on numeric loopback"
            ) from None
        canonical = str(address)
        allowed = (
            isinstance(address, ipaddress.IPv4Address) and address.is_loopback
        ) or address == ipaddress.IPv6Address("::1")
        if not allowed or canonical != parsed.hostname.lower():
            raise ValueError("HTTP translation is allowed only on numeric loopback")
    return parsed


def _remaining(deadline: float) -> float:
    value = deadline - time.monotonic()
    if value <= 0:
        raise TimeoutError("deadline exceeded")
    return value


def _set_socket_deadline(
    connection: http.client.HTTPConnection, deadline: float
) -> None:
    if connection.sock is None:
        raise RuntimeError("translation connection socket is unavailable")
    connection.sock.settimeout(_remaining(deadline))


def _set_response_deadline(
    connection: http.client.HTTPConnection,
    response: http.client.HTTPResponse,
    deadline: float,
) -> None:
    timeout = _remaining(deadline)
    if connection.sock is not None:
        connection.sock.settimeout(timeout)
        return
    if response.fp is None:
        return
    response_socket = getattr(getattr(response.fp, "raw", None), "_sock", None)
    if response_socket is not None and getattr(response_socket, "_closed", False):
        return
    setter = getattr(response_socket, "settimeout", None)
    if not callable(setter):
        raise RuntimeError("translation response socket is unavailable")
    setter(timeout)


def _unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def _parse_translation_json(content: bytes) -> str:
    try:
        decoded = content.decode("utf-8", errors="strict")
        payload = json.loads(
            decoded,
            object_pairs_hook=_unique_object,
            parse_constant=lambda _value: (_ for _ in ()).throw(
                ValueError("non-JSON constant")
            ),
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
        raise ValueError("invalid translation JSON") from None
    if type(payload) is not dict or set(payload) != {
        "translated_text",
        "target_language",
    }:
        raise ValueError("invalid translation schema")
    translated = payload["translated_text"]
    if (
        payload["target_language"] != "en"
        or type(translated) is not str
        or not 1 <= len(translated) <= MAX_TRANSLATION_CHARACTERS
        or any(unicodedata.category(character) == "Cc" for character in translated)
    ):
        raise ValueError("invalid English translation")
    return translated


def _read_response(
    connection: http.client.HTTPConnection,
    response: http.client.HTTPResponse,
    deadline: float,
) -> str:
    if response.status != 200:
        raise ValueError("translation status is invalid")
    content_types = response.headers.get_all("Content-Type", failobj=[])
    content_lengths = response.headers.get_all("Content-Length", failobj=[])
    if len(content_types) != 1 or not _JSON_CONTENT_TYPE.fullmatch(
        content_types[0].strip()
    ):
        raise ValueError("translation content type is invalid")
    if len(content_lengths) > 1:
        raise ValueError("translation content length is invalid")
    if content_lengths:
        length = content_lengths[0]
        if not length.isascii() or not length.isdigit():
            raise ValueError("translation content length is invalid")
        if int(length) > MAX_TRANSLATION_RESPONSE_BYTES:
            raise ValueError("translation response is too large")
    chunks: list[bytes] = []
    size = 0
    while True:
        _set_response_deadline(connection, response, deadline)
        chunk = response.read1(8_192)
        _remaining(deadline)
        if not chunk:
            break
        size += len(chunk)
        if size > MAX_TRANSLATION_RESPONSE_BYTES:
            raise ValueError("translation response is too large")
        chunks.append(chunk)
    return _parse_translation_json(b"".join(chunks))


def _perform_translation(url: str, token: str, text: str, timeout: float) -> str:
    parsed = _parse_url(url)
    deadline = time.monotonic() + timeout
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    if parsed.scheme == "https":
        connection: http.client.HTTPConnection = http.client.HTTPSConnection(
            parsed.hostname,
            port,
            timeout=_remaining(deadline),
            context=ssl.create_default_context(),
        )
    else:
        connection = http.client.HTTPConnection(
            parsed.hostname, port, timeout=_remaining(deadline)
        )
    target = parsed.path or "/"
    if parsed.query:
        target += f"?{parsed.query}"
    body = json.dumps(
        {"text": text, "target_language": "en"},
        ensure_ascii=True,
        allow_nan=False,
        separators=(",", ":"),
    ).encode("utf-8")
    try:
        connection.connect()
        _set_socket_deadline(connection, deadline)
        connection.putrequest("POST", target, skip_accept_encoding=True)
        connection.putheader("Authorization", f"Bearer {token}")
        connection.putheader("Accept", "application/json")
        connection.putheader("Content-Type", "application/json; charset=utf-8")
        connection.putheader("Content-Length", str(len(body)))
        connection.endheaders(body)
        _set_socket_deadline(connection, deadline)
        response = connection.getresponse()
        try:
            return _read_response(connection, response, deadline)
        finally:
            response.close()
    finally:
        connection.close()


def _translation_worker(
    send_pipe: Connection,
    url: str,
    token: str,
    text: str,
    timeout: float,
) -> None:
    try:
        translated = _perform_translation(url, token, text, timeout)
        send_pipe.send(("ok", translated))
    except BaseException:
        try:
            send_pipe.send(("error",))
        except BaseException:
            pass
    finally:
        send_pipe.close()


def _terminate_process(process: multiprocessing.Process) -> None:
    if process.is_alive():
        process.terminate()
        process.join(0.2)
    if process.is_alive():
        process.kill()
        process.join(0.2)
    if process.is_alive():
        raise RuntimeError("translation worker could not be stopped")


class HTTPTranslationProvider:
    def __init__(
        self,
        url: str,
        token: str,
        *,
        process_context: multiprocessing.context.BaseContext | None = None,
        monotonic=time.monotonic,
    ) -> None:
        _parse_url(url)
        if not token or len(token.encode("utf-8")) > 4_096 or "\x00" in token:
            raise ValueError("translation API token is invalid")
        self._url = url
        self._token = token
        self._context = process_context or multiprocessing.get_context("spawn")
        self._monotonic = monotonic

    def translate_to_english(self, text: str) -> str:
        if type(text) is not str or not 1 <= len(text) <= MAX_TRANSLATION_CHARACTERS:
            raise ValueError("translation source must be 1-2000 characters")
        if any(unicodedata.category(character) == "Cc" for character in text):
            raise ValueError("translation source contains control characters")
        started = self._monotonic()
        deadline = started + TRANSLATION_DEADLINE_SECONDS
        receive_pipe, send_pipe = self._context.Pipe(duplex=False)
        process = self._context.Process(
            target=_translation_worker,
            args=(
                send_pipe,
                self._url,
                self._token,
                text,
                TRANSLATION_DEADLINE_SECONDS,
            ),
            name="btc09-translation",
        )
        process.daemon = True
        message: object | None = None
        try:
            process.start()
            send_pipe.close()
            remaining = deadline - self._monotonic()
            if remaining <= 0 or not receive_pipe.poll(remaining):
                raise TranslationUnavailable("translation is unavailable")
            try:
                message = receive_pipe.recv()
            except EOFError:
                raise TranslationUnavailable("translation is unavailable") from None
        except TranslationUnavailable:
            raise
        except BaseException:
            raise TranslationUnavailable("translation is unavailable") from None
        finally:
            send_pipe.close()
            receive_pipe.close()
            _terminate_process(process)
            process.close()
        if (
            type(message) is not tuple
            or len(message) != 2
            or message[0] != "ok"
            or type(message[1]) is not str
        ):
            raise TranslationUnavailable("translation is unavailable")
        return message[1]


def translation_provider_from_environment(
    environment: Mapping[str, str] | None = None,
) -> TranslationProvider:
    values = os.environ if environment is None else environment
    url = values.get("TRANSLATION_API_URL", "").strip()
    token = values.get("TRANSLATION_API_TOKEN", "").strip()
    if not url or not token:
        return DisabledTranslationProvider()
    return HTTPTranslationProvider(url, token)
