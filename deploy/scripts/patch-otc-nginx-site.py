#!/usr/bin/env python3
"""Patch one reviewed include into the Certbot-managed btc09.org TLS server."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import NamedTuple

MAX_SITE_BYTES = 1 << 20
SERVER_INCLUDE = "/etc/nginx/snippets/bitcoin09-otc-server.conf"


class NginxPatchError(ValueError):
    pass


class Token(NamedTuple):
    value: str
    start: int
    end: int


class Item(NamedTuple):
    header: tuple[str, ...]
    start: int
    end: int
    open_index: int | None
    close_index: int | None


class Server(NamedTuple):
    open_index: int
    close_index: int
    items: tuple[Item, ...]


def _tokens(text: str) -> tuple[Token, ...]:
    result: list[Token] = []
    index = 0
    while index < len(text):
        character = text[index]
        if character.isspace():
            index += 1
            continue
        if character == "#":
            newline = text.find("\n", index)
            index = len(text) if newline < 0 else newline + 1
            continue
        if character in "{};":
            result.append(Token(character, index, index + 1))
            index += 1
            continue
        start = index
        quote: str | None = None
        value: list[str] = []
        while index < len(text):
            current = text[index]
            if (
                quote is None
                and current == "$"
                and index + 1 < len(text)
                and text[index + 1] == "{"
            ):
                close = text.find("}", index + 2)
                if close < 0:
                    raise NginxPatchError("unterminated braced nginx variable")
                name = text[index + 2 : close]
                if not name or any(
                    not character.isascii()
                    or (not character.isalnum() and character != "_")
                    for character in name
                ):
                    raise NginxPatchError("invalid braced nginx variable")
                value.append(text[index : close + 1])
                index = close + 1
                continue
            if current == "\\":
                index += 1
                if index >= len(text):
                    raise NginxPatchError("unterminated nginx escape")
                escaped = text[index]
                value.append(
                    {"n": "\n", "r": "\r", "t": "\t"}.get(escaped, escaped)
                )
                index += 1
                continue
            if current in "'\"":
                if quote is None:
                    quote = current
                    index += 1
                    continue
                if current == quote:
                    quote = None
                    index += 1
                    continue
            if quote is None and (current.isspace() or current in "{};"):
                break
            value.append(current)
            index += 1
        if quote is not None:
            raise NginxPatchError("unterminated quoted nginx token")
        result.append(Token("".join(value), start, index))
    return tuple(result)


def _matching_brace(tokens: tuple[Token, ...], open_index: int) -> int:
    depth = 0
    for index in range(open_index, len(tokens)):
        if tokens[index].value == "{":
            depth += 1
        elif tokens[index].value == "}":
            depth -= 1
            if depth == 0:
                return index
    raise NginxPatchError("unbalanced nginx block")


def _direct_items(
    tokens: tuple[Token, ...], open_index: int, close_index: int
) -> tuple[Item, ...]:
    items: list[Item] = []
    index = open_index + 1
    while index < close_index:
        start_index = index
        cursor = index
        while cursor < close_index and tokens[cursor].value not in {"{", ";", "}"}:
            cursor += 1
        if cursor >= close_index or tokens[cursor].value == "}":
            break
        header = tuple(token.value for token in tokens[start_index:cursor])
        if not header:
            raise NginxPatchError("empty nginx directive")
        if tokens[cursor].value == ";":
            items.append(
                Item(header, tokens[start_index].start, tokens[cursor].end, None, None)
            )
            index = cursor + 1
            continue
        nested_close = _matching_brace(tokens, cursor)
        if nested_close > close_index:
            raise NginxPatchError("nested nginx block escapes server")
        items.append(
            Item(
                header,
                tokens[start_index].start,
                tokens[nested_close].end,
                cursor,
                nested_close,
            )
        )
        index = nested_close + 1
    return tuple(items)


def _servers(tokens: tuple[Token, ...]) -> tuple[Server, ...]:
    result: list[Server] = []
    for index, token in enumerate(tokens[:-1]):
        if token.value != "server" or tokens[index + 1].value != "{":
            continue
        close = _matching_brace(tokens, index + 1)
        result.append(Server(index + 1, close, _direct_items(tokens, index + 1, close)))
    return tuple(result)


def _server_names(server: Server) -> frozenset[str]:
    return frozenset(
        value
        for item in server.items
        if item.header[0] == "server_name"
        for value in item.header[1:]
    )


def _is_tls(server: Server) -> bool:
    return any(
        item.header[0] == "listen"
        and any(value.startswith("443") for value in item.header[1:])
        and "ssl" in item.header[1:]
        for item in server.items
    )


def _location_path(item: Item) -> str | None:
    if item.open_index is None or not item.header or item.header[0] != "location":
        return None
    for value in item.header[1:]:
        cleaned = value.strip("'\"")
        if cleaned.startswith("/"):
            return cleaned
    return None


def _location_blocks(tokens: tuple[Token, ...]) -> tuple[Item, ...]:
    result: list[Item] = []
    for index, token in enumerate(tokens):
        if token.value != "location":
            continue
        cursor = index + 1
        while cursor < len(tokens) and tokens[cursor].value not in {"{", ";", "}"}:
            cursor += 1
        if cursor >= len(tokens) or tokens[cursor].value != "{":
            continue
        close = _matching_brace(tokens, cursor)
        result.append(
            Item(
                tuple(item.value for item in tokens[index:cursor]),
                token.start,
                tokens[close].end,
                cursor,
                close,
            )
        )
    return tuple(result)


def _expanded_span(text: str, start: int, end: int) -> tuple[int, int]:
    line_start = text.rfind("\n", 0, start) + 1
    if not text[line_start:start].strip():
        start = line_start
    while end < len(text) and text[end] in " \t":
        end += 1
    if end < len(text) and text[end] == "\n":
        end += 1
    return start, end


def _directives(tokens: tuple[Token, ...]) -> tuple[tuple[str, ...], ...]:
    result: list[tuple[str, ...]] = []
    start = 0
    for index, token in enumerate(tokens):
        if token.value in {"{", "}"}:
            start = index + 1
        elif token.value == ";":
            header = tuple(item.value for item in tokens[start:index])
            if header:
                result.append(header)
            start = index + 1
    return tuple(result)


def _clean(value: str) -> str:
    return value


def _proxy_passes(tokens: tuple[Token, ...]) -> tuple[tuple[str, ...], ...]:
    return tuple(
        directive
        for directive in _directives(tokens)
        if directive[0] == "proxy_pass"
    )


def _reject_hidden_site_routes(text: str) -> None:
    tokens = _tokens(text)
    if any("8019" in _clean(token.value) for token in tokens):
        raise NginxPatchError("unreviewed OTC upstream remains in TLS site")
    canonical = tuple(
        server
        for server in _servers(tokens)
        if _is_tls(server) and "btc09.org" in _server_names(server)
    )
    if len(canonical) != 1:
        raise NginxPatchError("expected exactly one canonical btc09.org TLS server")
    server = canonical[0]
    scoped_tokens = tokens[server.open_index : server.close_index + 1]
    if _proxy_passes(scoped_tokens):
        raise NginxPatchError("canonical TLS site contains an unreviewed proxy")


def audit_effective_config(text: str) -> None:
    if type(text) is not str or not text or len(text.encode("utf-8")) > MAX_SITE_BYTES:
        raise NginxPatchError("effective nginx configuration is empty or oversized")
    tokens = _tokens(text)
    directives = _directives(tokens)
    expected_proxy_passes = (
        ("proxy_pass", "http://127.0.0.1:8019/otc-bot-feed.json"),
        ("proxy_pass", "http://127.0.0.1:8009"),
    )
    if tuple(sorted(_proxy_passes(tokens))) != tuple(
        sorted(expected_proxy_passes)
    ):
        raise NginxPatchError(
            "effective nginx configuration has an unexpected proxy multiset"
        )
    origin_references = tuple(
        _clean(token.value) for token in tokens if "8019" in _clean(token.value)
    )
    if origin_references != ("http://127.0.0.1:8019/otc-bot-feed.json",):
        raise NginxPatchError(
            "effective nginx configuration has an alternate OTC origin reference"
        )
    upstreams = tuple(
        directive
        for directive in directives
        if directive[0] == "proxy_pass"
        and any("127.0.0.1:8019" in _clean(value) for value in directive[1:])
    )
    if upstreams != (("proxy_pass", "http://127.0.0.1:8019/otc-bot-feed.json"),):
        raise NginxPatchError(
            "effective nginx configuration has an unexpected OTC upstream"
        )
    include = ("include", SERVER_INCLUDE)
    if sum(directive == include for directive in directives) != 1:
        raise NginxPatchError(
            "effective nginx configuration has an unexpected OTC include count"
        )
    canonical = tuple(
        server
        for server in _servers(tokens)
        if _is_tls(server) and "btc09.org" in _server_names(server)
    )
    if len(canonical) != 1:
        raise NginxPatchError(
            "effective nginx configuration has an unexpected canonical TLS server"
        )
    canonical_server = canonical[0]
    if sum(item.header == include for item in canonical_server.items) != 1:
        raise NginxPatchError(
            "effective nginx configuration has an unexpected canonical OTC include"
        )
    canonical_tokens = tokens[
        canonical_server.open_index : canonical_server.close_index + 1
    ]
    if any("healthz" in _clean(token.value) for token in canonical_tokens):
        raise NginxPatchError(
            "effective canonical TLS server exposes operational health"
        )
    feed_locations = tuple(
        item
        for item in _location_blocks(tokens)
        if _location_path(item) == "/otc-bot-feed.json"
    )
    if len(feed_locations) != 1:
        raise NginxPatchError(
            "effective nginx configuration has an unexpected feed route count"
        )
    feed_location = feed_locations[0]
    if feed_location.open_index is None or feed_location.close_index is None:
        raise NginxPatchError("effective nginx feed route is malformed")
    feed_recursive_tokens = tokens[
        feed_location.open_index : feed_location.close_index + 1
    ]
    feed_recursive_directives = _directives(feed_recursive_tokens)
    canonical_proxy_passes = _proxy_passes(canonical_tokens) + _proxy_passes(
        feed_recursive_tokens
    )
    if canonical_proxy_passes != (
        ("proxy_pass", "http://127.0.0.1:8019/otc-bot-feed.json"),
    ):
        raise NginxPatchError(
            "effective canonical TLS server has an unexpected proxy target"
        )
    if any(
        directive[0] in {"add_header", "proxy_hide_header"}
        for directive in feed_recursive_directives
    ):
        raise NginxPatchError(
            "effective nginx feed location overrides inherited headers"
        )


def audit_tls_headers(text: str) -> None:
    if (
        type(text) is not str
        or not text
        or len(text.encode("ascii", "ignore")) > 64 << 10
    ):
        raise NginxPatchError("TLS response headers are empty or oversized")
    lines = text.replace("\r\n", "\n").split("\n")
    status_lines = tuple(line for line in lines if line.startswith("HTTP/"))
    if len(status_lines) != 1 or status_lines[0] != lines[0] or " 200" not in lines[0]:
        raise NginxPatchError("TLS feed readback did not return 200")
    headers: dict[str, list[str]] = {}
    for line in lines[1:]:
        if not line:
            break
        if ":" not in line:
            raise NginxPatchError("TLS response header is malformed")
        name, value = line.split(":", 1)
        headers.setdefault(name.strip().lower(), []).append(value.strip())
    expected = {
        "access-control-allow-origin": ["*"],
        "cache-control": ["no-store"],
        "x-content-type-options": ["nosniff"],
    }
    for name, values in expected.items():
        if headers.get(name) != values:
            raise NginxPatchError("TLS response header ownership is invalid")


def transform_site(text: str) -> str:
    if type(text) is not str or not text or len(text.encode("utf-8")) > MAX_SITE_BYTES:
        raise NginxPatchError("nginx site is empty or oversized")
    if "\x00" in text or "\r" in text:
        raise NginxPatchError("nginx site contains invalid characters")
    tokens = _tokens(text)
    servers = _servers(tokens)
    tls_servers = tuple(
        server
        for server in servers
        if _is_tls(server)
        and _server_names(server).intersection({"btc09.org", "www.btc09.org"})
    )
    canonical = tuple(
        server for server in tls_servers if "btc09.org" in _server_names(server)
    )
    if len(canonical) != 1:
        raise NginxPatchError("expected exactly one canonical btc09.org TLS server")
    canonical_server = canonical[0]

    edits: list[tuple[int, int, str]] = []
    include_count = 0
    for server in servers:
        relevant = server in tls_servers
        for item in server.items:
            path = _location_path(item)
            location_header = (
                item.header if item.header and item.header[0] == "location" else ()
            )
            has_health_alias = any(
                "healthz" in value for value in location_header[1:]
            )
            has_feed_alias = any(
                "otc-bot-feed" in value for value in location_header[1:]
            )
            if path == "/otc-feed-healthz" and server is canonical_server:
                start, end = _expanded_span(text, item.start, item.end)
                edits.append((start, end, ""))
            elif has_health_alias and server is canonical_server:
                raise NginxPatchError("public operational health location is forbidden")
            if path == "/otc-bot-feed.json":
                if not relevant:
                    raise NginxPatchError(
                        "OTC feed location exists outside btc09.org TLS servers"
                    )
                start, end = _expanded_span(text, item.start, item.end)
                edits.append((start, end, ""))
            elif has_feed_alias:
                raise NginxPatchError("noncanonical OTC feed location is forbidden")
            if item.header == ("include", SERVER_INCLUDE):
                include_count += 1
                if server is not canonical_server:
                    raise NginxPatchError(
                        "OTC include exists outside canonical TLS server"
                    )
    if include_count > 1:
        raise NginxPatchError("duplicate OTC server include")
    if include_count == 0:
        insertion = tokens[canonical_server.close_index].start
        edits.append((insertion, insertion, f"    include {SERVER_INCLUDE};\n"))
    for start, end, replacement in sorted(edits, reverse=True):
        text = text[:start] + replacement + text[end:]
    _reject_hidden_site_routes(text)
    return text


def main(argv: list[str]) -> int:
    if len(argv) == 3 and argv[1] in {"--audit-effective", "--audit-headers"}:
        try:
            text = Path(argv[2]).read_text(encoding="utf-8")
            if argv[1] == "--audit-effective":
                audit_effective_config(text)
            else:
                audit_tls_headers(text)
        except (OSError, UnicodeError, NginxPatchError):
            print("nginx audit failed", file=sys.stderr)
            return 1
        return 0
    if len(argv) != 3:
        print(
            "usage: patch-otc-nginx-site.py SOURCE OUTPUT | --audit-effective FILE | --audit-headers FILE",
            file=sys.stderr,
        )
        return 2
    source = Path(argv[1])
    output = Path(argv[2])
    if not source.is_absolute() or not output.is_absolute() or source == output:
        print("nginx patch paths must be distinct absolute paths", file=sys.stderr)
        return 2
    try:
        encoded = source.read_bytes()
        text = encoded.decode("utf-8", "strict")
        patched = transform_site(text)
        output.write_text(patched, encoding="utf-8", newline="\n")
    except (OSError, UnicodeError, NginxPatchError):
        print("nginx site patch failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
