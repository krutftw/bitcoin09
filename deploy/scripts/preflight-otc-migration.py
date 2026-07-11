#!/usr/bin/env python3
"""Read-only, Store-backed OTC database migration preflight."""

from __future__ import annotations

import sqlite3
import sys
from pathlib import Path


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
if str(REPOSITORY_ROOT) not in sys.path:
    sys.path.insert(0, str(REPOSITORY_ROOT))

from bot.otc.store import Store  # noqa: E402


class PreflightError(RuntimeError):
    pass


def inspect(path: Path) -> str:
    if not path.is_absolute() or not path.is_file() or path.is_symlink():
        raise PreflightError("database path is invalid")
    connection = sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True, timeout=5)
    try:
        connection.execute("PRAGMA query_only=ON")
        if connection.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
            raise PreflightError("database integrity failed")
        if connection.execute("PRAGMA foreign_key_check").fetchone() is not None:
            raise PreflightError("database foreign keys failed")
        return Store.validate_migration_preflight(connection)
    finally:
        connection.close()


def main() -> None:
    if len(sys.argv) != 2:
        print("OTC migration preflight failed", file=sys.stderr)
        raise SystemExit(2)
    try:
        origin = inspect(Path(sys.argv[1]))
    except Exception:
        print("OTC migration preflight failed", file=sys.stderr)
        raise SystemExit(1)
    print(f"OTC migration preflight passed ({origin}, zero obligations)")


if __name__ == "__main__":
    main()
