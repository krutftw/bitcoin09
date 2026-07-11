#!/usr/bin/env python3
"""Wait for consecutive post-reload OTC TLS signatures from new Nginx workers."""

from __future__ import annotations

import importlib.util
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable
from pathlib import Path
from types import ModuleType


READINESS_TIMEOUT_SECONDS = 10.0
READINESS_CONSECUTIVE_SUCCESSES = 3
READINESS_INTERVAL_SECONDS = 0.25


def wait_until_ready(
    probe: Callable[[float], bool],
    *,
    timeout: float = READINESS_TIMEOUT_SECONDS,
    consecutive: int = READINESS_CONSECUTIVE_SUCCESSES,
    interval: float = READINESS_INTERVAL_SECONDS,
    clock: Callable[[], float] = time.monotonic,
    pause: Callable[[float], None] = time.sleep,
) -> bool:
    if timeout <= 0 or consecutive < 1 or interval <= 0:
        raise ValueError("invalid readiness bounds")
    deadline = clock() + timeout
    successes = 0
    while True:
        remaining = deadline - clock()
        if remaining <= 0:
            return False
        if probe(remaining):
            successes += 1
            if successes >= consecutive:
                return True
        else:
            successes = 0
        remaining = deadline - clock()
        if remaining <= 0:
            return False
        pause(min(interval, remaining))


def _load_patch_helper(path: Path) -> ModuleType:
    details = os.lstat(path)
    if (
        not path.is_absolute()
        or path.resolve(strict=True) != path
        or not stat.S_ISREG(details.st_mode)
        or details.st_uid != 0
        or details.st_nlink != 1
        or stat.S_IMODE(details.st_mode) != 0o755
    ):
        raise ValueError("unsafe patch helper")
    spec = importlib.util.spec_from_file_location("otc_nginx_patch_readback", path)
    if spec is None or spec.loader is None:
        raise ValueError("patch helper cannot load")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    if not callable(getattr(module, "audit_tls_headers", None)):
        raise ValueError("patch helper has no header audit")
    return module


class LocalTlsProbe:
    def __init__(
        self,
        *,
        curl: str,
        headers_path: Path,
        audit_headers: Callable[[str], None],
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.curl = curl
        self.headers_path = headers_path
        self.audit_headers = audit_headers
        self.clock = clock
        self.last_failure: str | None = "not probed"

    @staticmethod
    def _curl_timeout(deadline: float, clock: Callable[[], float]) -> float:
        return max(0.05, min(1.0, deadline - clock()))

    def _run(
        self, arguments: list[str], deadline: float
    ) -> subprocess.CompletedProcess[str]:
        timeout = self._curl_timeout(deadline, self.clock)
        return subprocess.run(
            [self.curl, *arguments, "--max-time", f"{timeout:.3f}"],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout + 0.25,
        )

    def __call__(self, remaining: float) -> bool:
        deadline = self.clock() + remaining
        common = [
            "--silent",
            "--show-error",
            "--noproxy",
            "*",
            "--http1.1",
            "--no-keepalive",
            "--header",
            "Connection: close",
            "--resolve",
            "btc09.org:443:127.0.0.1",
        ]
        try:
            self.headers_path.unlink(missing_ok=True)
            feed = self._run(
                [
                    *common,
                    "--fail",
                    "--dump-header",
                    str(self.headers_path),
                    "--output",
                    os.devnull,
                    "https://btc09.org/otc-bot-feed.json",
                ],
                deadline,
            )
            if feed.returncode != 0:
                self.last_failure = "feed transport"
                return False
            if self.clock() >= deadline:
                self.last_failure = "feed deadline"
                return False
            try:
                self.audit_headers(self.headers_path.read_text(encoding="ascii"))
            except (OSError, UnicodeError, ValueError):
                self.last_failure = "feed headers"
                return False
            if self.clock() >= deadline:
                self.last_failure = "header deadline"
                return False
            health = self._run(
                [
                    *common,
                    "--output",
                    os.devnull,
                    "--write-out",
                    "%{http_code}",
                    "https://btc09.org/otc-feed-healthz",
                ],
                deadline,
            )
            if health.returncode != 0:
                self.last_failure = "health transport"
                return False
            if health.stdout != "404":
                self.last_failure = "health status"
                return False
            self.last_failure = None
            return True
        except subprocess.SubprocessError:
            self.last_failure = "probe timeout"
            return False
        except (OSError, UnicodeError, ValueError):
            self.last_failure = "probe io"
            return False


def readiness_failure_reason(probe: LocalTlsProbe) -> str:
    return probe.last_failure or "readiness streak timeout"


def main(argv: list[str]) -> int:
    if len(argv) != 2 or getattr(os, "geteuid", lambda: -1)() != 0:
        print("OTC nginx readiness failed", file=sys.stderr)
        return 1
    failure = "setup"
    try:
        patch = _load_patch_helper(Path(argv[1]))
        curl = shutil.which("curl")
        if curl is None:
            raise ValueError("curl is unavailable")
        with tempfile.TemporaryDirectory(
            prefix="btc09-nginx-readiness.", dir="/run"
        ) as root:
            probe = LocalTlsProbe(
                curl=curl,
                headers_path=Path(root) / "headers",
                audit_headers=patch.audit_tls_headers,
            )
            ready = wait_until_ready(probe)
            failure = readiness_failure_reason(probe)
    except (OSError, ValueError, ImportError):
        ready = False
    if not ready:
        print(f"OTC nginx readiness failed: {failure}", file=sys.stderr)
        return 1
    print("OTC nginx readiness passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
