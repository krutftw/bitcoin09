#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf '%s\n' "OTC health check failed: $1" >&2
  exit 1
}

[[ $# -eq 3 ]] || fail "usage: check-otc-health.sh LOCAL_URL DB EXPECTED_ACCEPTING_0_OR_1"
url=$1
db=$2
expected=$3
[[ $url == http://127.0.0.1:*/* ]] || fail "health URL must use numeric loopback HTTP"
[[ $db == /* && -f $db && ! -L $db ]] || fail "database must be an absolute regular non-symlink file"
[[ $expected == 0 || $expected == 1 ]] || fail "expected accepting value must be 0 or 1"

if [[ -n ${PYTHON_BIN:-} ]]; then
  python_bin=$PYTHON_BIN
elif [[ -x /opt/btc09/venv/bin/python ]]; then
  python_bin=/opt/btc09/venv/bin/python
else
  python_bin=$(command -v python3 || command -v python || true)
fi
[[ -n ${python_bin:-} && -x $python_bin ]] || fail "Python is unavailable"

if ! "$python_bin" - "$url" "$db" "$expected" <<'PY'
import json
import sqlite3
import sys
import urllib.error
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit


class HealthError(Exception):
    pass


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise HealthError("duplicate JSON key")
        result[key] = value
    return result


def integer(value, label):
    if type(value) is not int or value < 0:
        raise HealthError(f"invalid {label}")


def verify_http(url, expected):
    parsed = urlsplit(url)
    try:
        port = parsed.port
    except ValueError as exc:
        raise HealthError("invalid health URL") from exc
    if (
        parsed.scheme != "http"
        or parsed.hostname != "127.0.0.1"
        or parsed.username is not None
        or parsed.password is not None
        or port is None
        or parsed.path != "/healthz"
        or parsed.query
        or parsed.fragment
    ):
        raise HealthError("health URL is not canonical numeric loopback")
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    try:
        response = urllib.request.urlopen(request, timeout=5)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        status = response.status
        content_type = response.headers.get_content_type()
        body = response.read(65_537)
    if status != (200 if expected else 503):
        raise HealthError("unexpected HTTP status")
    if content_type != "application/json" or len(body) > 65_536:
        raise HealthError("invalid HTTP response")
    payload = json.loads(body.decode("utf-8", "strict"), object_pairs_hook=unique_object)
    required = {
        "integrity", "foreign_key_integrity", "explorer_snapshot_reachable",
        "explorer_tx_status_reachable", "wallet_spendable_units",
        "customer_liability_units", "pending_platform_outflow_units",
        "provisional_restricted_units", "common_ledger_tip",
        "stale_watched_address_count", "gross_fee_units", "available_fee_units",
        "negative_fee_invariant", "transfer_counts", "credited_noncanonical_count",
        "unknown_spend_count", "deposit_allocation", "accepting_orders", "checked_at",
        "feed_age_seconds",
    }
    if type(payload) is not dict or set(payload) != required:
        raise HealthError("invalid health schema")
    if payload["integrity"] != "ok" or payload["foreign_key_integrity"] != "ok":
        raise HealthError("reported database health failed")
    for field in ("explorer_snapshot_reachable", "explorer_tx_status_reachable"):
        if payload[field] is not True:
            raise HealthError("explorer health failed")
    if type(payload["accepting_orders"]) is not bool or payload["accepting_orders"] is not expected:
        raise HealthError("accepting state mismatch")
    if payload["negative_fee_invariant"] is not False:
        raise HealthError("fee invariant failed")
    for field in (
        "customer_liability_units", "pending_platform_outflow_units",
        "provisional_restricted_units", "stale_watched_address_count",
        "gross_fee_units", "credited_noncanonical_count", "unknown_spend_count",
        "checked_at", "feed_age_seconds",
    ):
        integer(payload[field], field)
    spendable = payload["wallet_spendable_units"]
    integer(spendable, "wallet_spendable_units")
    available = payload["available_fee_units"]
    if type(available) is not int or available < 0:
        raise HealthError("available fee invariant failed")
    if spendable < payload["provisional_restricted_units"]:
        raise HealthError("wallet provisional solvency failed")
    if spendable - payload["provisional_restricted_units"] < (
        payload["customer_liability_units"] + payload["pending_platform_outflow_units"]
    ):
        raise HealthError("wallet solvency failed")
    tip = payload["common_ledger_tip"]
    if (
        type(tip) is not dict or set(tip) != {"hash", "height"}
        or type(tip["hash"]) is not str or len(tip["hash"]) != 64
        or any(character not in "0123456789abcdef" for character in tip["hash"])
    ):
        raise HealthError("ledger tip is invalid")
    integer(tip["height"], "ledger tip height")
    counts = payload["transfer_counts"]
    if type(counts) is not dict or set(counts) != {"queued", "reserved", "prepared", "broadcast", "uncertain"}:
        raise HealthError("transfer health schema is invalid")
    for state, count in counts.items():
        integer(count, f"transfer {state}")
        if count != 0:
            raise HealthError("unresolved transfer state")
    allocation = payload["deposit_allocation"]
    allocation_fields = {"lifetime_count", "daily_count", "pending_count", "lifetime_headroom", "daily_headroom"}
    if type(allocation) is not dict or set(allocation) != allocation_fields:
        raise HealthError("allocation health schema is invalid")
    for field, count in allocation.items():
        integer(count, f"allocation {field}")
    if allocation["pending_count"] != 0 or payload["stale_watched_address_count"] != 0:
        raise HealthError("unresolved address state")
    if payload["credited_noncanonical_count"] != 0 or payload["unknown_spend_count"] != 0:
        raise HealthError("unresolved chain evidence")


def verify_db(path):
    uri = Path(path).resolve().as_uri() + "?mode=ro"
    connection = sqlite3.connect(uri, uri=True, timeout=5)
    try:
        integrity = connection.execute("PRAGMA integrity_check").fetchall()
        if integrity != [("ok",)]:
            raise HealthError("database integrity failed")
        if connection.execute("PRAGMA foreign_key_check").fetchone() is not None:
            raise HealthError("database foreign keys failed")
        schema = connection.execute("SELECT id,version,origin FROM schema_meta").fetchall()
        if len(schema) != 1 or schema[0][0:2] != (1, 4) or type(schema[0][2]) is not str or not schema[0][2]:
            raise HealthError("schema migration state is unresolved")
        if connection.execute("SELECT 1 FROM orders WHERE state='address_pending' LIMIT 1").fetchone():
            raise HealthError("address allocation state is unresolved")
        if connection.execute(
            "SELECT 1 FROM transfers WHERE state IN ('queued','reserved','prepared','broadcast','uncertain') LIMIT 1"
        ).fetchone():
            raise HealthError("transfer state is unresolved")
    finally:
        connection.close()


try:
    expected = sys.argv[3] == "1"
    verify_http(sys.argv[1], expected)
    verify_db(sys.argv[2])
except Exception:
    print("OTC health check failed", file=sys.stderr)
    raise SystemExit(1)
PY
then
  fail "validation failed"
fi

printf 'OTC health check passed (accepting_orders=%s)\n' "$expected"
