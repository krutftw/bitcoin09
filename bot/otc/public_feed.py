from __future__ import annotations

import json
import os
import secrets
import stat
import time
from decimal import Decimal, InvalidOperation, localcontext
from pathlib import Path
from typing import Any, Mapping

from bot.otc.domain import (
    PUBLIC_SETTLEMENT_METHODS,
    PUBLIC_SETTLEMENT_NETWORKS,
    parse_total_price,
)

DEFAULT_PUBLIC_FEED_PATH = Path("/var/lib/btc09-otc-public/otc-bot-feed.json")
UNITS_PER_09C = 100_000_000
MAX_FEED_BYTES = 4 * 1024 * 1024
_DIRECTORY_FSYNC_SUPPORTED = os.name != "nt"

_PUBLIC_ORDER_FIELDS = frozenset(
    {
        "order_id",
        "side",
        "status",
        "net_amount_09c",
        "total_price",
        "price_per_09c",
        "asset",
        "settlement_network",
        "payment_method",
        "created_at",
        "updated_at",
        "matched_at",
        "completed_at",
    }
)
_PUBLIC_STATES = frozenset(
    {
        "awaiting_deposit",
        "open",
        "matched",
        "disputed",
        "release_reserved",
        "refund_reserved",
        "broadcast",
        "completed",
        "refunded",
        "cancelled",
        "deposit_expired",
        "recovery_hold",
        "transfer_failed_safe",
        "transfer_uncertain",
    }
)
_HEALTH_FIELDS = frozenset(
    {
        "integrity",
        "foreign_key_integrity",
        "explorer_snapshot_reachable",
        "explorer_tx_status_reachable",
        "wallet_spendable_units",
        "customer_liability_units",
        "pending_platform_outflow_units",
        "provisional_restricted_units",
        "common_ledger_tip",
        "stale_watched_address_count",
        "gross_fee_units",
        "available_fee_units",
        "negative_fee_invariant",
        "transfer_counts",
        "credited_noncanonical_count",
        "unknown_spend_count",
        "deposit_allocation",
        "accepting_orders",
        "checked_at",
    }
)


def _canonical_decimal(value: Decimal) -> str:
    if not value.is_finite() or value < 0:
        raise ValueError("feed decimal is invalid")
    rendered = format(value, "f")
    if "." in rendered:
        rendered = rendered.rstrip("0").rstrip(".")
    return rendered or "0"


def _nonnegative_integer(value: object, label: str) -> int:
    if type(value) is not int or value < 0:
        raise ValueError(f"{label} is invalid")
    return value


def _canonical_positive_decimal(value: object, label: str) -> str:
    if type(value) is not str or not value or len(value) > 128:
        raise ValueError(f"{label} is invalid")
    try:
        decimal = Decimal(value)
    except InvalidOperation:
        raise ValueError(f"{label} is invalid") from None
    if decimal <= 0 or not decimal.is_finite() or _canonical_decimal(decimal) != value:
        raise ValueError(f"{label} is invalid")
    return value


def _order_projection(row: Mapping[str, Any]) -> dict[str, object]:
    units = row["net_amount_units"]
    if type(units) is not int or units <= 0:
        raise ValueError("stored net amount is invalid")
    try:
        total_text = parse_total_price(row["total_price"])
        total = Decimal(total_text)
        with localcontext() as context:
            context.prec = 80
            net = Decimal(units) / Decimal(UNITS_PER_09C)
            unit_price = total / net
    except (InvalidOperation, TypeError, ValueError, ZeroDivisionError):
        raise ValueError("stored order price is invalid") from None
    network = row["settlement_network"]
    method = row["payment_method"]
    return {
        "order_id": row["order_id"],
        "side": row["side"],
        "status": row["state"],
        "net_amount_09c": _canonical_decimal(net),
        "total_price": total_text,
        "price_per_09c": _canonical_decimal(unit_price),
        "asset": row["settlement_asset"],
        "settlement_network": (
            network
            if network in PUBLIC_SETTLEMENT_NETWORKS
            else "Private settlement network"
        ),
        "payment_method": (
            method
            if method in PUBLIC_SETTLEMENT_METHODS
            else "Private settlement method"
        ),
        "created_at": row["created_at"],
        "updated_at": row["updated_at"],
        "matched_at": row["matched_at"],
        "completed_at": row["completed_at"],
    }


def _runtime_value(runtime_health: object | None, name: str, default: object) -> object:
    if runtime_health is None:
        return default
    if isinstance(runtime_health, Mapping):
        return runtime_health.get(name, default)
    return getattr(runtime_health, name, default)


def _database_health(
    snapshot: Mapping[str, object],
    *,
    runtime_health: object | None,
    checked_at: int,
) -> dict[str, object]:
    explorer_snapshot = (
        _runtime_value(runtime_health, "explorer_snapshot_reachable", False) is True
    )
    explorer_transaction = (
        _runtime_value(runtime_health, "explorer_tx_status_reachable", False) is True
    )
    external_tip = _runtime_value(runtime_health, "explorer_tip", None)
    if not (
        type(external_tip) is dict
        and set(external_tip) == {"hash", "height"}
        and type(external_tip["hash"]) is str
        and len(external_tip["hash"]) == 64
        and not any(
            character not in "0123456789abcdef"
            for character in external_tip["hash"]
        )
        and type(external_tip["height"]) is int
        and external_tip["height"] >= 0
    ):
        external_tip = None

    latest = snapshot["latest_watched_scans"]
    if type(latest) is not tuple:
        raise ValueError("database watched scan snapshot is invalid")
    stale_count = 0
    if external_tip is None:
        stale_count = len(latest)
    else:
        for scan in latest:
            if (
                type(scan) is not dict
                or scan.get("tip_hash") != external_tip["hash"]
                or scan.get("tip_height") != external_tip["height"]
            ):
                stale_count += 1
    common_tip = external_tip if stale_count == 0 else None

    integrity = snapshot["integrity"]
    foreign_keys = snapshot["foreign_key_integrity"]
    liability = snapshot["customer_liability_units"]
    pending = snapshot["pending_platform_outflow_units"]
    provisional = snapshot["provisional_restricted_units"]
    gross_fees = snapshot["gross_fee_units"]
    available_fees = snapshot["available_fee_units"]
    transfer_counts = snapshot["transfer_counts"]
    noncanonical = snapshot["credited_noncanonical_count"]
    unknown_spends = snapshot["unknown_spend_count"]
    allocation_value = _runtime_value(runtime_health, "deposit_allocation", None)
    allocation = (
        dict(allocation_value)
        if isinstance(allocation_value, Mapping)
        else {
            "lifetime_count": 0,
            "daily_count": 0,
            "pending_count": 0,
            "lifetime_headroom": 0,
            "daily_headroom": 0,
        }
    )
    wallet_spendable = _runtime_value(
        runtime_health, "wallet_spendable_units", None
    )
    issues_value = _runtime_value(runtime_health, "issues", ("health_unchecked",))
    issues = tuple(issues_value) if isinstance(issues_value, (tuple, list)) else ("invalid",)
    requested = _runtime_value(runtime_health, "accepting_orders", False) is True
    runtime_checked_at = _runtime_value(runtime_health, "checked_at", checked_at)
    checked = type(runtime_checked_at) is int and runtime_checked_at == checked_at
    solvent = (
        type(wallet_spendable) is int
        and type(provisional) is int
        and type(liability) is int
        and type(pending) is int
        and wallet_spendable >= provisional
        and wallet_spendable - provisional >= liability + pending
    )
    operational = (
        integrity == "ok"
        and foreign_keys == "ok"
        and explorer_snapshot
        and explorer_transaction
        and common_tip is not None
        and stale_count == 0
        and solvent
        and type(available_fees) is int
        and available_fees >= 0
        and type(transfer_counts) is dict
        and transfer_counts.get("uncertain") == 0
        and noncanonical == 0
        and unknown_spends == 0
        and allocation.get("pending_count") == 0
        and not issues
        and checked
    )
    return {
        "integrity": integrity,
        "foreign_key_integrity": foreign_keys,
        "explorer_snapshot_reachable": explorer_snapshot,
        "explorer_tx_status_reachable": explorer_transaction,
        "wallet_spendable_units": wallet_spendable,
        "customer_liability_units": liability,
        "pending_platform_outflow_units": pending,
        "provisional_restricted_units": provisional,
        "common_ledger_tip": common_tip,
        "stale_watched_address_count": stale_count,
        "gross_fee_units": gross_fees,
        "available_fee_units": available_fees,
        "negative_fee_invariant": type(available_fees) is int and available_fees < 0,
        "transfer_counts": transfer_counts,
        "credited_noncanonical_count": noncanonical,
        "unknown_spend_count": unknown_spends,
        "deposit_allocation": allocation,
        "accepting_orders": requested and operational,
        "checked_at": checked_at,
    }


def build_public_feed(
    store: object,
    *,
    generated_at: int | None = None,
    runtime_health: object | None = None,
) -> dict[str, object]:
    if generated_at is None:
        import time

        generated_at = int(time.time())
    if type(generated_at) is not int or generated_at < 0:
        raise ValueError("feed timestamp is invalid")
    snapshot = store.public_feed_snapshot()  # type: ignore[attr-defined]
    if not isinstance(snapshot, Mapping):
        raise ValueError("database public feed snapshot is invalid")
    rows = snapshot["orders"]
    if type(rows) is not tuple:
        raise ValueError("database order snapshot is invalid")
    orders = [_order_projection(row) for row in rows]
    counts = snapshot["summary"]
    health = _database_health(
        snapshot, runtime_health=runtime_health, checked_at=generated_at
    )
    payload = {
        "schema_version": 1,
        "health_timestamp": generated_at,
        "summary": counts,
        "orders": orders,
        "health": health,
    }
    public_feed_projection(payload)
    return payload


def public_feed_projection(payload: Mapping[str, object]) -> dict[str, object]:
    if set(payload) != {
        "schema_version",
        "health_timestamp",
        "summary",
        "orders",
        "health",
    }:
        raise ValueError("feed payload has an invalid schema")
    if payload["schema_version"] != 1 or type(payload["schema_version"]) is not int:
        raise ValueError("feed schema version is invalid")
    health_timestamp = _nonnegative_integer(
        payload["health_timestamp"], "feed health timestamp"
    )
    summary = payload["summary"]
    if type(summary) is not dict or set(summary) != {
        "open",
        "matched",
        "completed",
        "disputed",
    }:
        raise ValueError("feed summary has an invalid schema")
    safe_summary = {
        key: _nonnegative_integer(summary[key], f"feed {key} count")
        for key in ("open", "matched", "completed", "disputed")
    }
    orders = payload["orders"]
    if type(orders) is not list:
        raise ValueError("feed orders must be an array")
    safe_orders: list[dict[str, object]] = []
    order_ids: set[int] = set()
    for order in orders:
        if type(order) is not dict or set(order) != _PUBLIC_ORDER_FIELDS:
            raise ValueError("feed order has an invalid schema")
        order_id = order["order_id"]
        if type(order_id) is not int or order_id <= 0 or order_id in order_ids:
            raise ValueError("feed order ID is invalid")
        order_ids.add(order_id)
        if order["side"] not in {"buy", "sell"} or type(order["side"]) is not str:
            raise ValueError("feed order side is invalid")
        if order["status"] not in _PUBLIC_STATES or type(order["status"]) is not str:
            raise ValueError("feed order status is invalid")
        net_amount = _canonical_positive_decimal(
            order["net_amount_09c"], "feed net amount"
        )
        raw_total_price = order["total_price"]
        if type(raw_total_price) is not str:
            raise ValueError("feed total price is invalid")
        total_price = parse_total_price(raw_total_price)
        if total_price != raw_total_price:
            raise ValueError("feed total price is not canonical")
        price_per = _canonical_positive_decimal(
            order["price_per_09c"], "feed unit price"
        )
        with localcontext() as context:
            context.prec = 80
            net_decimal = Decimal(net_amount)
            scaled_units = net_decimal * Decimal(UNITS_PER_09C)
            if (
                scaled_units != scaled_units.to_integral_value()
                or scaled_units <= 0
                or scaled_units > 2_100_000_000_000_000
            ):
                raise ValueError("feed net amount is not exact integer units")
            expected_price = _canonical_decimal(Decimal(total_price) / net_decimal)
        if price_per != expected_price:
            raise ValueError("feed unit price does not match total and net amount")
        asset = order["asset"]
        if (
            type(asset) is not str
            or not 2 <= len(asset) <= 12
            or any(character not in "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-" for character in asset)
        ):
            raise ValueError("feed settlement asset is invalid")
        network = order["settlement_network"]
        method = order["payment_method"]
        if type(network) is not str or network not in PUBLIC_SETTLEMENT_NETWORKS:
            raise ValueError("feed settlement network is invalid")
        if type(method) is not str or method not in PUBLIC_SETTLEMENT_METHODS:
            raise ValueError("feed payment method is invalid")
        created = _nonnegative_integer(order["created_at"], "feed creation time")
        updated = _nonnegative_integer(order["updated_at"], "feed update time")
        if updated < created or updated > health_timestamp:
            raise ValueError("feed update time is invalid")
        for field in ("matched_at", "completed_at"):
            value = order[field]
            if value is not None:
                _nonnegative_integer(value, f"feed {field}")
                if value < created or value > updated:
                    raise ValueError(f"feed {field} is outside the order lifetime")
        if (order["status"] in {"completed", "refunded"}) != (
            order["completed_at"] is not None
        ):
            raise ValueError("feed completion state and timestamp disagree")
        if (
            order["matched_at"] is not None
            and order["completed_at"] is not None
            and order["matched_at"] > order["completed_at"]
        ):
            raise ValueError("feed completion precedes matching")
        safe_orders.append(
            {
                "order_id": order_id,
                "side": order["side"],
                "status": order["status"],
                "net_amount_09c": net_amount,
                "total_price": total_price,
                "price_per_09c": price_per,
                "asset": asset,
                "settlement_network": network,
                "payment_method": method,
                "created_at": created,
                "updated_at": updated,
                "matched_at": order["matched_at"],
                "completed_at": order["completed_at"],
            }
        )
    derived_summary = {
        state: sum(order["status"] == state for order in safe_orders)
        for state in ("open", "matched", "completed", "disputed")
    }
    exact_states = ("open", "matched", "disputed")
    if any(safe_summary[state] != derived_summary[state] for state in exact_states):
        raise ValueError("feed summary does not match public orders")
    if safe_summary["completed"] < derived_summary["completed"]:
        raise ValueError("feed summary does not match public orders")
    _validate_health(payload["health"], expected_timestamp=health_timestamp)
    return {
        "schema_version": 1,
        "health_timestamp": health_timestamp,
        "summary": safe_summary,
        "orders": safe_orders,
    }


def _validate_health(value: object, *, expected_timestamp: int) -> None:
    if type(value) is not dict or set(value) != _HEALTH_FIELDS:
        raise ValueError("feed health has an invalid schema")
    if value["integrity"] not in {"ok", "failed"}:
        raise ValueError("feed integrity status is invalid")
    if value["foreign_key_integrity"] not in {"ok", "failed"}:
        raise ValueError("feed foreign-key status is invalid")
    for field in (
        "explorer_snapshot_reachable",
        "explorer_tx_status_reachable",
        "negative_fee_invariant",
        "accepting_orders",
    ):
        if type(value[field]) is not bool:
            raise ValueError(f"feed {field} is invalid")
    for field in (
        "customer_liability_units",
        "pending_platform_outflow_units",
        "provisional_restricted_units",
        "stale_watched_address_count",
        "gross_fee_units",
        "credited_noncanonical_count",
        "unknown_spend_count",
    ):
        _nonnegative_integer(value[field], f"feed {field}")
    spendable = value["wallet_spendable_units"]
    if spendable is not None:
        _nonnegative_integer(spendable, "feed wallet spendable units")
    available = value["available_fee_units"]
    if type(available) is not int:
        raise ValueError("feed available fee units are invalid")
    if value["negative_fee_invariant"] != (available < 0):
        raise ValueError("feed fee invariant status is invalid")
    tip = value["common_ledger_tip"]
    if tip is not None:
        if (
            type(tip) is not dict
            or set(tip) != {"hash", "height"}
            or type(tip["hash"]) is not str
            or len(tip["hash"]) != 64
            or any(character not in "0123456789abcdef" for character in tip["hash"])
        ):
            raise ValueError("feed common ledger tip is invalid")
        _nonnegative_integer(tip["height"], "feed common ledger height")
    counts = value["transfer_counts"]
    if type(counts) is not dict or set(counts) != {
        "queued",
        "reserved",
        "prepared",
        "broadcast",
        "uncertain",
    }:
        raise ValueError("feed transfer counts have an invalid schema")
    for state, count in counts.items():
        _nonnegative_integer(count, f"feed {state} transfer count")
    allocation = value["deposit_allocation"]
    if type(allocation) is not dict or set(allocation) != {
        "lifetime_count",
        "daily_count",
        "pending_count",
        "lifetime_headroom",
        "daily_headroom",
    }:
        raise ValueError("feed deposit allocation health has an invalid schema")
    for field, count in allocation.items():
        _nonnegative_integer(count, f"feed deposit allocation {field}")
    if value["checked_at"] != expected_timestamp or type(value["checked_at"]) is not int:
        raise ValueError("feed health check time is invalid")
    if value["accepting_orders"]:
        solvent = (
            type(spendable) is int
            and spendable >= value["provisional_restricted_units"]
            and spendable - value["provisional_restricted_units"]
            >= value["customer_liability_units"]
            + value["pending_platform_outflow_units"]
        )
        if not (
            value["integrity"] == "ok"
            and value["foreign_key_integrity"] == "ok"
            and value["explorer_snapshot_reachable"]
            and value["explorer_tx_status_reachable"]
            and solvent
            and tip is not None
            and value["stale_watched_address_count"] == 0
            and available >= 0
            and counts["uncertain"] == 0
            and value["credited_noncanonical_count"] == 0
            and value["unknown_spend_count"] == 0
            and allocation["pending_count"] == 0
        ):
            raise ValueError("feed accepts orders without complete healthy evidence")


def health_is_operational(payload: Mapping[str, object]) -> bool:
    public_feed_projection(payload)
    health = payload["health"]
    return type(health) is dict and health["accepting_orders"] is True


def _open_public_directory(path: Path) -> int | None:
    entry = os.lstat(path)
    if not stat.S_ISDIR(entry.st_mode) or stat.S_ISLNK(entry.st_mode):
        raise ValueError("public feed parent must be a real directory")
    if not _DIRECTORY_FSYNC_SUPPORTED:
        return None
    flags = (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_DIRECTORY", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    directory_fd = os.open(path, flags)
    opened = os.fstat(directory_fd)
    if not stat.S_ISDIR(opened.st_mode) or (opened.st_dev, opened.st_ino) != (
        entry.st_dev,
        entry.st_ino,
    ):
        os.close(directory_fd)
        raise ValueError("public feed parent changed while opening")
    return directory_fd


def _fsync_parent_directory(path: Path, directory_fd: int | None = None) -> None:
    if not _DIRECTORY_FSYNC_SUPPORTED:
        return
    directory = directory_fd
    close_directory = False
    if directory is None:
        directory = _open_public_directory(path)
        close_directory = True
    if directory is None:
        return
    try:
        os.fsync(directory)
    finally:
        if close_directory:
            os.close(directory)


def write_public_feed(path: str | os.PathLike[str], payload: Mapping[str, object]) -> None:
    target = Path(path)
    encoded = (
        json.dumps(
            payload,
            ensure_ascii=True,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        + "\n"
    ).encode("utf-8")
    if len(encoded) > MAX_FEED_BYTES:
        raise ValueError("public feed exceeds the maximum size")
    temporary_name = f".{target.name}.{secrets.token_hex(8)}.tmp"
    directory_fd = _open_public_directory(target.parent)
    descriptor: int | None = None
    try:
        if directory_fd is None:
            descriptor = os.open(
                target.parent / temporary_name,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL,
                0o600,
            )
        else:
            descriptor = os.open(
                temporary_name,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
                0o600,
                dir_fd=directory_fd,
            )
        with os.fdopen(descriptor, "wb", closefd=True) as handle:
            descriptor = None
            handle.write(encoded)
            handle.flush()
            os.fchmod(handle.fileno(), 0o644)
            os.fsync(handle.fileno())
        for attempt in range(100):
            try:
                if directory_fd is None:
                    os.replace(target.parent / temporary_name, target)
                else:
                    os.replace(
                        temporary_name,
                        target.name,
                        src_dir_fd=directory_fd,
                        dst_dir_fd=directory_fd,
                    )
                break
            except PermissionError:
                if os.name != "nt" or attempt == 99:
                    raise
                time.sleep(0.001)
        _fsync_parent_directory(target.parent, directory_fd)
    finally:
        if descriptor is not None:
            os.close(descriptor)
        try:
            if directory_fd is None:
                (target.parent / temporary_name).unlink()
            else:
                os.unlink(temporary_name, dir_fd=directory_fd)
        except FileNotFoundError:
            pass
        if directory_fd is not None:
            os.close(directory_fd)


def invalidate_public_feed(path: str | os.PathLike[str]) -> None:
    target = Path(path)
    directory_fd = _open_public_directory(target.parent)
    try:
        if directory_fd is None:
            target.unlink()
        else:
            os.unlink(target.name, dir_fd=directory_fd)
        _fsync_parent_directory(target.parent, directory_fd)
    except FileNotFoundError:
        return
    finally:
        if directory_fd is not None:
            os.close(directory_fd)
