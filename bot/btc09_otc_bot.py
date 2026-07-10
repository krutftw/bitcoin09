#!/usr/bin/env python3
"""
Bitcoin 09 OTC escrow bot.

This is custodial Discord escrow for early 09C OTC trades. It uses one fresh
09C deposit address per order so deposits are attributed by address balance,
not by total hot-wallet balance deltas.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import re
import sqlite3
import subprocess
import threading
import time
from decimal import Decimal, InvalidOperation, getcontext
from pathlib import Path

import discord
from discord import app_commands
from discord.ext import commands
import requests

getcontext().prec = 28

BOT_VERSION = "otc-escrow-v0.2.1"
COIN_TICKER = "09C"
COIN = Decimal("0.00000001")
MIN_ORDER = Decimal("1")
ADDRESS_VERSION = 0x09
BASE58_ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
POOL_BASE = os.environ.get("POOL_BASE", "https://bitcoin09.tutuit.xyz/api/pools/09c").rstrip("/")
PUBLIC_EXPLORER_URL = os.environ.get("PUBLIC_EXPLORER_URL", "https://explorer.btc09.org").rstrip("/")
DISCORD_INVITE = os.environ.get("DISCORD_INVITE", "https://discord.gg/fUuGzwRTzP")

BOT_TOKEN = os.environ.get("BOT_TOKEN") or os.environ.get("DISCORD_BOT_TOKEN", "")
GUILD_ID = int(os.environ.get("DISCORD_GUILD_ID", "0") or "0")
BTC09_BIN = os.environ.get("BTC09_BIN", "/opt/btc09/btc09")
BTC09_DATADIR = os.environ.get("BTC09_DATADIR", "/opt/btc09/data")
EXPLORER_URL = os.environ.get("EXPLORER_URL", "http://localhost:8009").rstrip("/")
DB_PATH = os.environ.get("DB_PATH", "/opt/btc09/otc_bot.db")
FEE_PERCENT = Decimal(os.environ.get("FEE_PERCENT", "1.0"))
TRADING_ENABLED = os.environ.get("OTC_TRADING_ENABLED", "0").strip().lower() in {
    "1",
    "true",
    "yes",
    "on",
}
TX_FEE = os.environ.get("BTC09_TX_FEE", "0.0001")
ORDER_TIMEOUT_SECONDS = int(os.environ.get("ORDER_TIMEOUT_SECONDS", "86400"))
PUBLIC_FEED_PATH = os.environ.get("PUBLIC_FEED_PATH", "/opt/btc09/public/otc-bot-feed.json")
PUBLIC_FEED_LIMIT = int(os.environ.get("PUBLIC_FEED_LIMIT", "100"))
ADMIN_IDS = {int(x) for x in os.environ.get("ADMIN_IDS", "").split(",") if x.strip()}

SEND_LOCK = threading.Lock()
BOT_LOOP: asyncio.AbstractEventLoop | None = None


def now_ts() -> int:
    return int(time.time())


async def require_trading_enabled(interaction: discord.Interaction) -> bool:
    if TRADING_ENABLED:
        return True
    await interaction.response.send_message(
        "New OTC escrow actions are temporarily paused while the safer WTS/WTB "
        "trade system completes its controlled pilot. Do not send 09C to an old "
        "deposit address. Follow #announcements for the verified launch.",
        ephemeral=True,
    )
    return False


def iso_ts(ts: int | None) -> str | None:
    if not ts:
        return None
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(int(ts)))


def db() -> sqlite3.Connection:
    conn = sqlite3.connect(DB_PATH, timeout=30)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA busy_timeout=30000")
    return conn


def ensure_column(conn: sqlite3.Connection, table: str, column: str, ddl: str) -> None:
    columns = {row["name"] for row in conn.execute(f"PRAGMA table_info({table})")}
    if column not in columns:
        conn.execute(f"ALTER TABLE {table} ADD COLUMN {ddl}")


def init_db() -> None:
    Path(DB_PATH).parent.mkdir(parents=True, exist_ok=True)
    with db() as conn:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS users (
                user_id INTEGER PRIMARY KEY,
                username TEXT,
                wallet_addr TEXT,
                created_at INTEGER,
                updated_at INTEGER
            );

            CREATE TABLE IF NOT EXISTS orders (
                order_id INTEGER PRIMARY KEY AUTOINCREMENT,
                seller_id INTEGER NOT NULL,
                seller_name TEXT,
                amount TEXT NOT NULL,
                price TEXT NOT NULL,
                currency TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'pending_deposit',
                escrow_bal_before TEXT,
                deposit_addr TEXT,
                deposit_expected TEXT,
                deposit_confirmed_balance TEXT,
                buyer_id INTEGER,
                buyer_name TEXT,
                seller_confirmed INTEGER DEFAULT 0,
                buyer_confirmed INTEGER DEFAULT 0,
                release_txid TEXT,
                refund_txid TEXT,
                fee TEXT,
                cancel_reason TEXT,
                created_at INTEGER NOT NULL,
                updated_at INTEGER NOT NULL,
                matched_at INTEGER,
                disputed_at INTEGER,
                completed_at INTEGER
            );

            CREATE TABLE IF NOT EXISTS withdrawals (
                withdrawal_id INTEGER PRIMARY KEY AUTOINCREMENT,
                admin_id INTEGER NOT NULL,
                amount TEXT NOT NULL,
                address TEXT NOT NULL,
                txid TEXT,
                status TEXT NOT NULL,
                created_at INTEGER NOT NULL
            );

            """
        )

        # Migrate the first empty prototype DB without dropping any records.
        for column, ddl in {
            "updated_at": "updated_at INTEGER",
            "deposit_addr": "deposit_addr TEXT",
            "deposit_expected": "deposit_expected TEXT",
            "deposit_confirmed_balance": "deposit_confirmed_balance TEXT",
            "refund_txid": "refund_txid TEXT",
            "cancel_reason": "cancel_reason TEXT",
            "matched_at": "matched_at INTEGER",
            "disputed_at": "disputed_at INTEGER",
            "completed_at": "completed_at INTEGER",
        }.items():
            ensure_column(conn, "orders", column, ddl)
        ensure_column(conn, "users", "updated_at", "updated_at INTEGER")
        conn.executescript(
            """
            CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
            CREATE INDEX IF NOT EXISTS idx_orders_deposit_addr ON orders(deposit_addr);
            CREATE INDEX IF NOT EXISTS idx_orders_seller ON orders(seller_id);
            CREATE INDEX IF NOT EXISTS idx_orders_buyer ON orders(buyer_id);
            """
        )
        conn.commit()


def base58_decode(value: str) -> bytes:
    num = 0
    for char in value:
        idx = BASE58_ALPHABET.find(char)
        if idx == -1:
            raise ValueError("bad base58 character")
        num = num * 58 + idx
    raw = num.to_bytes((num.bit_length() + 7) // 8, "big") if num else b""
    leading_zeroes = len(value) - len(value.lstrip("1"))
    return b"\x00" * leading_zeroes + raw


def base58_encode(raw: bytes) -> str:
    num = int.from_bytes(raw, "big")
    out = ""
    while num:
        num, rem = divmod(num, 58)
        out = BASE58_ALPHABET[rem] + out
    leading_zeroes = len(raw) - len(raw.lstrip(b"\x00"))
    return "1" * leading_zeroes + (out or "1")


def validate_09c_address(address: str) -> str:
    address = address.strip()
    payload = base58_decode(address)
    if len(payload) != 25:
        raise ValueError("bad address length")
    body, checksum = payload[:-4], payload[-4:]
    if body[0] != ADDRESS_VERSION:
        raise ValueError("bad address version")
    expected = hashlib.sha256(hashlib.sha256(body).digest()).digest()[:4]
    if checksum != expected:
        raise ValueError("bad address checksum")
    return address


def parse_coin_amount(value: str, *, minimum: Decimal | None = None, allow_zero: bool = False) -> Decimal:
    try:
        amount = Decimal(value.strip())
    except (InvalidOperation, AttributeError):
        raise ValueError("invalid amount")
    if not amount.is_finite() or amount < 0 or (amount == 0 and not allow_zero):
        raise ValueError("amount must be positive")
    quantized = amount.quantize(COIN)
    if quantized != amount:
        raise ValueError("use at most 8 decimal places")
    if minimum is not None and quantized < minimum:
        raise ValueError(f"minimum order is {fmt_amt(minimum)} {COIN_TICKER}")
    return quantized


def parse_price(value: str) -> str:
    try:
        price = Decimal(value.strip())
    except (InvalidOperation, AttributeError):
        raise ValueError("price must be a number")
    if not price.is_finite() or price <= 0:
        raise ValueError("price must be positive")
    if len(value.strip()) > 32:
        raise ValueError("price is too long")
    return value.strip()


def parse_currency(value: str) -> str:
    currency = value.strip().upper()
    if not re.fullmatch(r"[A-Z0-9._-]{2,12}", currency):
        raise ValueError("currency must be 2-12 letters/numbers")
    return currency


def fmt_amt(amount: Decimal) -> str:
    return f"{amount.quantize(COIN):.8f}".rstrip("0").rstrip(".") or "0"


def run_btc09(args: list[str], timeout: int = 120) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [BTC09_BIN, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def btc09_wallet_new() -> str:
    result = run_btc09(["wallet", "new", "-datadir", BTC09_DATADIR], timeout=30)
    output = (result.stdout + "\n" + result.stderr).strip()
    if result.returncode != 0:
        raise RuntimeError(output or "btc09 wallet new failed")
    for line in output.splitlines():
        candidate = line.strip()
        try:
            return validate_09c_address(candidate)
        except ValueError:
            continue
    raise RuntimeError(f"btc09 wallet new did not return an address: {output[:200]}")


def address_from_seed_hex(seed_hex: str) -> str:
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

    seed = bytes.fromhex(seed_hex)
    private_key = Ed25519PrivateKey.from_private_bytes(seed)
    public_key = private_key.public_key().public_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PublicFormat.Raw,
    )
    first = hashlib.sha256(public_key).digest()
    pubkey_hash = hashlib.sha256(first).digest()[:20]
    body = bytes([ADDRESS_VERSION]) + pubkey_hash
    checksum = hashlib.sha256(hashlib.sha256(body).digest()).digest()[:4]
    return base58_encode(body + checksum)


def wallet_addresses() -> list[str]:
    wallet_path = Path(BTC09_DATADIR) / "wallet-mainnet.json"
    with wallet_path.open("r", encoding="utf-8") as fh:
        data = json.load(fh)
    addresses = []
    for seed_hex in data.get("keys", []):
        address = address_from_seed_hex(seed_hex)
        validate_09c_address(address)
        addresses.append(address)
    return addresses


def explorer_balance(address: str) -> Decimal:
    address = validate_09c_address(address)
    response = requests.get(f"{EXPLORER_URL}/address/{address}", timeout=8)
    response.raise_for_status()
    for line in response.text.splitlines():
        if "spendable balance" in line.lower():
            match = re.search(r"([\d.]+)\s+09C", line, re.I)
            if match:
                return parse_coin_amount(match.group(1), allow_zero=True)
    raise RuntimeError(f"explorer did not return spendable balance for {address}")


def btc09_send(to_addr: str, amount: Decimal) -> str:
    to_addr = validate_09c_address(to_addr)
    amount = parse_coin_amount(fmt_amt(amount))
    with SEND_LOCK:
        result = run_btc09(
            [
                "send",
                "-to",
                to_addr,
                "-amount",
                fmt_amt(amount),
                "-fee",
                TX_FEE,
                "-datadir",
                BTC09_DATADIR,
                "-seeds",
                "127.0.0.1:9009",
            ],
            timeout=180,
        )
    output = (result.stdout + "\n" + result.stderr).strip()
    if result.returncode != 0:
        raise RuntimeError(output[-500:] or "btc09 send failed")
    if "signed tx" not in output.lower():
        raise RuntimeError(output[-500:] or "btc09 send returned no tx")
    return output


def ensure_user(user: discord.User | discord.Member) -> None:
    with db() as conn:
        conn.execute(
            """
            INSERT INTO users (user_id, username, created_at, updated_at)
            VALUES (?, ?, ?, ?)
            ON CONFLICT(user_id) DO UPDATE SET username = excluded.username, updated_at = excluded.updated_at
            """,
            (user.id, user.name, now_ts(), now_ts()),
        )
        conn.commit()


def set_user_wallet(user_id: int, address: str) -> None:
    address = validate_09c_address(address)
    with db() as conn:
        conn.execute(
            "UPDATE users SET wallet_addr = ?, updated_at = ? WHERE user_id = ?",
            (address, now_ts(), user_id),
        )
        conn.commit()


def get_user_wallet(user_id: int | None) -> str | None:
    if user_id is None:
        return None
    with db() as conn:
        row = conn.execute("SELECT wallet_addr FROM users WHERE user_id = ?", (user_id,)).fetchone()
    return row["wallet_addr"] if row and row["wallet_addr"] else None


def fetch_order(order_id: int) -> sqlite3.Row | None:
    with db() as conn:
        return conn.execute("SELECT * FROM orders WHERE order_id = ?", (order_id,)).fetchone()


def is_admin(user_id: int) -> bool:
    return user_id in ADMIN_IDS


def order_amount(row: sqlite3.Row) -> Decimal:
    return parse_coin_amount(row["amount"])


def fee_for_amount(amount: Decimal) -> Decimal:
    return (amount * FEE_PERCENT / Decimal("100")).quantize(COIN)


def fee_collected() -> Decimal:
    with db() as conn:
        rows = conn.execute(
            "SELECT fee FROM orders WHERE status IN ('completed', 'resolved_buyer') AND fee IS NOT NULL"
        ).fetchall()
    return sum((Decimal(row["fee"]) for row in rows), Decimal("0")).quantize(COIN)


def fee_withdrawn() -> Decimal:
    with db() as conn:
        rows = conn.execute("SELECT amount FROM withdrawals WHERE status = 'completed'").fetchall()
    return sum((Decimal(row["amount"]) for row in rows), Decimal("0")).quantize(COIN)


def fee_available() -> Decimal:
    available = fee_collected() - fee_withdrawn()
    return max(available, Decimal("0")).quantize(COIN)


def locked_order_balance() -> Decimal:
    with db() as conn:
        rows = conn.execute(
            """
            SELECT amount FROM orders
            WHERE status IN ('open', 'matched', 'disputed', 'releasing', 'release_failed')
            """
        ).fetchall()
    return sum((Decimal(row["amount"]) for row in rows), Decimal("0")).quantize(COIN)


def wallet_total_balance() -> tuple[Decimal, int]:
    total = Decimal("0")
    addresses = wallet_addresses()
    for address in addresses:
        total += explorer_balance(address)
    return total.quantize(COIN), len(addresses)


def public_status(status: str) -> str:
    labels = {
        "open": "open",
        "matched": "matched",
        "disputed": "disputed",
        "completed": "completed",
        "resolved_buyer": "resolved to buyer",
        "resolved_seller": "resolved to seller",
        "release_failed": "release needs admin",
    }
    return labels.get(status, status.replace("_", " "))


def price_per_coin(total_price: str, amount: str) -> str | None:
    try:
        total = Decimal(total_price)
        coins = Decimal(amount)
        if coins <= 0:
            return None
        return f"{(total / coins).quantize(Decimal('0.0000000001')):f}".rstrip("0").rstrip(".")
    except (InvalidOperation, ZeroDivisionError):
        return None


def fmt_number(value, digits: int = 0) -> str:
    try:
        number = Decimal(str(value))
    except (InvalidOperation, TypeError):
        return "-"
    if digits <= 0:
        return f"{int(number):,}"
    return f"{number:,.{digits}f}"


def fmt_hashrate(value) -> str:
    try:
        rate = Decimal(str(value))
    except (InvalidOperation, TypeError):
        return "-"
    if rate >= Decimal("1000000"):
        return f"{rate / Decimal('1000000'):.2f} MH/s"
    if rate >= Decimal("1000"):
        return f"{rate / Decimal('1000'):.2f} KH/s"
    return f"{rate:.2f} H/s"


def fmt_seconds(value) -> str:
    try:
        seconds = Decimal(str(value))
    except (InvalidOperation, TypeError):
        return "-"
    if seconds <= 0:
        return "-"
    if seconds >= Decimal("3600"):
        return f"{seconds / Decimal('3600'):.1f}h"
    if seconds >= Decimal("60"):
        return f"{seconds / Decimal('60'):.1f}m"
    return f"{seconds:.0f}s"


def network_stats_text() -> str:
    status_response = requests.get(f"{EXPLORER_URL}/api/status", timeout=8)
    status_response.raise_for_status()
    status = status_response.json()

    pool_response = requests.get(POOL_BASE, timeout=8)
    pool_response.raise_for_status()
    pool = pool_response.json().get("pool", {})

    miners_response = requests.get(f"{POOL_BASE}/miners?pageSize=10", timeout=8)
    miners_response.raise_for_status()
    miners = miners_response.json()
    if not isinstance(miners, list):
        miners = []

    pool_stats = pool.get("poolStats") or {}
    top_miners = []
    for miner in miners[:5]:
        address = str(miner.get("miner", ""))
        short = address[:10] + "..." + address[-6:] if len(address) > 20 else address
        top_miners.append(f"- `{short}` {fmt_hashrate(miner.get('hashrate'))}")

    lines = [
        "Bitcoin 09 live mining stats",
        "",
        f"Height: `{fmt_number(status.get('height'))}` | Peers: `{fmt_number(status.get('peers'))}` | Difficulty: `{fmt_number(status.get('difficulty'), 2)}`",
        f"Target: `{fmt_seconds(status.get('target_block_seconds'))}` | Avg this window: `{fmt_seconds(status.get('epoch_average_block_seconds'))}` | Retarget: `{fmt_number(status.get('blocks_to_retarget'))}` blocks",
        f"Next retarget: height `{fmt_number(status.get('next_retarget_height'))}` | Est. next difficulty: `{fmt_number(status.get('estimated_next_difficulty'), 2)}`",
        f"Circulating: `{fmt_number(status.get('circulating_supply'), 2)} {COIN_TICKER}`",
        f"Pool hashrate: `{fmt_hashrate(pool_stats.get('poolHashrate'))}` | Active pool miner addresses: `{fmt_number(pool_stats.get('connectedMiners', len(miners)))}`",
        f"Pool blocks: `{fmt_number(pool.get('blocksFound'))}` | Pool paid: `{fmt_number(pool.get('totalPaid'), 2)} {COIN_TICKER}`",
        "",
        "Top pool addresses:",
        *(top_miners or ["No active pool miners reported."]),
        "",
        "Difficulty retargets every 2,016 blocks, Bitcoin-style. Miner count means public-pool payout addresses, not guaranteed unique people.",
        f"Pool: https://bitcoin09.tutuit.xyz | Explorer: {PUBLIC_EXPLORER_URL} | Discord: {DISCORD_INVITE}",
    ]
    return "\n".join(lines)


def export_public_feed() -> None:
    public_statuses = (
        "open",
        "matched",
        "disputed",
        "completed",
        "resolved_buyer",
        "resolved_seller",
        "release_failed",
    )
    placeholders = ",".join("?" for _ in public_statuses)
    with db() as conn:
        counts = {
            row["status"]: row["n"]
            for row in conn.execute("SELECT status, COUNT(*) AS n FROM orders GROUP BY status")
        }
        rows = conn.execute(
            f"""
            SELECT order_id, amount, price, currency, status, created_at, updated_at,
                   matched_at, disputed_at, completed_at
            FROM orders
            WHERE status IN ({placeholders})
            ORDER BY updated_at DESC, order_id DESC
            LIMIT ?
            """,
            (*public_statuses, PUBLIC_FEED_LIMIT),
        ).fetchall()

    orders = []
    for row in rows:
        order_id = int(row["order_id"])
        status = row["status"]
        orders.append(
            {
                "id": order_id,
                "source": "discord-escrow-bot",
                "kind": "sell",
                "status": status,
                "publicStatus": public_status(status),
                "amount": row["amount"],
                "totalPrice": row["price"],
                "currency": row["currency"],
                "pricePer09c": price_per_coin(row["price"], row["amount"]),
                "createdAt": iso_ts(row["created_at"]),
                "updatedAt": iso_ts(row["updated_at"]),
                "matchedAt": iso_ts(row["matched_at"]),
                "disputedAt": iso_ts(row["disputed_at"]),
                "completedAt": iso_ts(row["completed_at"]),
                "action": f"Use /buy {order_id} in the Bitcoin 09 Discord" if status == "open" else f"Use /admin orders or /dispute {order_id} in Discord",
            }
        )

    feed = {
        "schema": 1,
        "generatedAt": iso_ts(now_ts()),
        "source": "Bitcoin 09 Discord OTC escrow bot",
        "privacy": "No Discord IDs, usernames, wallet addresses, deposit addresses, or off-chain payment details are published.",
        "summary": {
            "open": counts.get("open", 0),
            "matched": counts.get("matched", 0),
            "disputed": counts.get("disputed", 0),
            "completed": counts.get("completed", 0) + counts.get("resolved_buyer", 0) + counts.get("resolved_seller", 0),
            "releaseFailed": counts.get("release_failed", 0),
        },
        "orders": orders,
    }

    path = Path(PUBLIC_FEED_PATH)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(json.dumps(feed, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    tmp.replace(path)


def safe_export_public_feed() -> None:
    try:
        export_public_feed()
    except Exception as exc:
        print(f"[WARN] public feed export failed: {exc}")


def escrow_embed(title: str, description: str = "", color: int = 0x09C4A1) -> discord.Embed:
    embed = discord.Embed(title=title, description=description, color=color)
    embed.set_footer(text=f"{COIN_TICKER} OTC escrow bot {BOT_VERSION} | fee {FEE_PERCENT}%")
    return embed


async def run_blocking(func, *args):
    loop = asyncio.get_running_loop()
    return await loop.run_in_executor(None, func, *args)


async def dm_user(user_id: int | None, embed: discord.Embed) -> None:
    if not user_id:
        return
    try:
        user = await bot.fetch_user(user_id)
        await user.send(embed=embed)
    except Exception as exc:
        print(f"[WARN] DM failed for {user_id}: {exc}")


def dispatch(coro) -> None:
    if BOT_LOOP and not BOT_LOOP.is_closed():
        asyncio.run_coroutine_threadsafe(coro, BOT_LOOP)


intents = discord.Intents.default()
bot = commands.Bot(command_prefix="!", intents=intents, allowed_mentions=discord.AllowedMentions.none())


@bot.event
async def on_ready() -> None:
    global BOT_LOOP
    BOT_LOOP = asyncio.get_running_loop()
    print(f"[INFO] {BOT_VERSION} online as {bot.user}")
    try:
        if GUILD_ID:
            guild = discord.Object(id=GUILD_ID)
            bot.tree.copy_global_to(guild=guild)
            synced = await bot.tree.sync(guild=guild)
        else:
            synced = await bot.tree.sync()
        print(f"[INFO] Synced {len(synced)} slash commands")
    except Exception as exc:
        print(f"[ERROR] command sync failed: {exc}")


@bot.tree.command(name="sell", description="Create a 09C sell order")
@app_commands.describe(amount="Amount of 09C to sell", price="Total price", currency="Payment currency, e.g. USDT")
async def sell(interaction: discord.Interaction, amount: str, price: str, currency: str) -> None:
    if not await require_trading_enabled(interaction):
        return
    ensure_user(interaction.user)
    try:
        amt = parse_coin_amount(amount, minimum=MIN_ORDER)
        clean_price = parse_price(price)
        clean_currency = parse_currency(currency)
    except ValueError as exc:
        await interaction.response.send_message(f"Invalid order: {exc}", ephemeral=True)
        return

    refund_addr = get_user_wallet(interaction.user.id)
    if not refund_addr:
        await interaction.response.send_message(
            f"Set your {COIN_TICKER} refund address first with `/setaddress <addr>`.",
            ephemeral=True,
        )
        return

    await interaction.response.defer(thinking=True)
    try:
        deposit_addr = await run_blocking(btc09_wallet_new)
    except Exception as exc:
        await interaction.followup.send(f"Could not create a deposit address: `{str(exc)[:300]}`", ephemeral=True)
        return

    with db() as conn:
        cur = conn.execute(
            """
            INSERT INTO orders (
                seller_id, seller_name, amount, price, currency, status,
                deposit_addr, deposit_expected, created_at, updated_at
            )
            VALUES (?, ?, ?, ?, ?, 'pending_deposit', ?, ?, ?, ?)
            """,
            (
                interaction.user.id,
                interaction.user.name,
                fmt_amt(amt),
                clean_price,
                clean_currency,
                deposit_addr,
                fmt_amt(amt),
                now_ts(),
                now_ts(),
            ),
        )
        order_id = cur.lastrowid
        conn.commit()
    safe_export_public_feed()

    embed = escrow_embed(
        f"Order #{order_id} created",
        "\n".join(
            [
                f"Seller: `{interaction.user.name}`",
                f"Selling: `{fmt_amt(amt)} {COIN_TICKER}`",
                f"Price: `{clean_price} {clean_currency}`",
                "",
                "Deposit exactly this order amount to:",
                f"```{deposit_addr}```",
                f"Then run `/deposit {order_id}`. This address is only for this order.",
            ]
        ),
        color=0x57F287,
    )
    await interaction.followup.send(embed=embed)


@bot.tree.command(name="deposit", description="Verify a seller deposit")
@app_commands.describe(order_id="Order ID")
async def deposit(interaction: discord.Interaction, order_id: int) -> None:
    if not await require_trading_enabled(interaction):
        return
    ensure_user(interaction.user)
    row = fetch_order(order_id)
    if not row:
        await interaction.response.send_message("Order not found.", ephemeral=True)
        return
    if row["seller_id"] != interaction.user.id:
        await interaction.response.send_message("That is not your order.", ephemeral=True)
        return
    if row["status"] != "pending_deposit":
        await interaction.response.send_message(f"Order #{order_id} is `{row['status']}`.", ephemeral=True)
        return

    await interaction.response.defer(thinking=True)
    try:
        bal = await run_blocking(explorer_balance, row["deposit_addr"])
    except Exception as exc:
        await interaction.followup.send(f"Explorer check failed: `{str(exc)[:300]}`", ephemeral=True)
        return

    amt = order_amount(row)
    if bal < amt:
        await interaction.followup.send(
            embed=escrow_embed(
                "Deposit not seen yet",
                f"Order #{order_id}: `{fmt_amt(bal)} / {fmt_amt(amt)} {COIN_TICKER}` at `{row['deposit_addr']}`.",
                color=0xFEE75C,
            ),
            ephemeral=True,
        )
        return

    with db() as conn:
        updated = conn.execute(
            """
            UPDATE orders
            SET status = 'open', deposit_confirmed_balance = ?, updated_at = ?
            WHERE order_id = ? AND status = 'pending_deposit'
            """,
            (fmt_amt(bal), now_ts(), order_id),
        ).rowcount
        conn.commit()
    safe_export_public_feed()

    if not updated:
        await interaction.followup.send("Order status changed while checking. Run `/orders`.", ephemeral=True)
        return

    await interaction.followup.send(
        embed=escrow_embed(
            f"Order #{order_id} is live",
            f"`{fmt_amt(amt)} {COIN_TICKER}` is in escrow. Buyers can run `/buy {order_id}`.",
            color=0x57F287,
        )
    )


@bot.tree.command(name="orders", description="Show open 09C sell orders")
async def orders(interaction: discord.Interaction) -> None:
    ensure_user(interaction.user)
    with db() as conn:
        rows = conn.execute(
            "SELECT * FROM orders WHERE status = 'open' ORDER BY order_id DESC LIMIT 20"
        ).fetchall()
    if not rows:
        await interaction.response.send_message("No open orders right now.")
        return

    lines = []
    for row in rows:
        lines.append(
            f"#{row['order_id']} | `{row['amount']} {COIN_TICKER}` for `{row['price']} {row['currency']}` | seller `{row['seller_name']}`"
        )
    lines.append("")
    lines.append("Accept with `/buy <order_id>`.")
    await interaction.response.send_message(embed=escrow_embed("Open OTC orders", "\n".join(lines)))


@bot.tree.command(name="buy", description="Accept an open sell order")
@app_commands.describe(order_id="Order ID")
async def buy(interaction: discord.Interaction, order_id: int) -> None:
    if not await require_trading_enabled(interaction):
        return
    ensure_user(interaction.user)
    buyer_wallet = get_user_wallet(interaction.user.id)
    if not buyer_wallet:
        await interaction.response.send_message(
            f"Set your {COIN_TICKER} receiving address first with `/setaddress <addr>`.",
            ephemeral=True,
        )
        return

    with db() as conn:
        row = conn.execute("SELECT * FROM orders WHERE order_id = ?", (order_id,)).fetchone()
        if not row:
            await interaction.response.send_message("Order not found.", ephemeral=True)
            return
        if row["status"] != "open":
            await interaction.response.send_message(f"Order #{order_id} is `{row['status']}`.", ephemeral=True)
            return
        if row["seller_id"] == interaction.user.id:
            await interaction.response.send_message("You cannot buy your own order.", ephemeral=True)
            return
        updated = conn.execute(
            """
            UPDATE orders
            SET status = 'matched', buyer_id = ?, buyer_name = ?, seller_confirmed = 0,
                buyer_confirmed = 0, matched_at = ?, updated_at = ?
            WHERE order_id = ? AND status = 'open'
            """,
            (interaction.user.id, interaction.user.name, now_ts(), now_ts(), order_id),
        ).rowcount
        conn.commit()
    safe_export_public_feed()

    if not updated:
        await interaction.response.send_message("Someone else accepted this order first.", ephemeral=True)
        return

    embed = escrow_embed(
        f"Order #{order_id} accepted",
        "\n".join(
            [
                f"Buying: `{row['amount']} {COIN_TICKER}`",
                f"Pay seller: `{row['price']} {row['currency']}`",
                "",
                "Pay the seller off-chain, then run:",
                f"`/confirm {order_id}`",
                "",
                "The seller must confirm too before escrow releases.",
            ]
        ),
        color=0x57F287,
    )
    await interaction.response.send_message(embed=embed, ephemeral=True)
    await dm_user(
        row["seller_id"],
        escrow_embed(
            f"Order #{order_id} accepted",
            f"Buyer `{interaction.user.name}` accepted `{row['amount']} {COIN_TICKER}` for `{row['price']} {row['currency']}`.\nConfirm only after you have received payment: `/confirm {order_id}`.",
        ),
    )


@bot.tree.command(name="confirm", description="Confirm your side of a trade")
@app_commands.describe(order_id="Order ID")
async def confirm(interaction: discord.Interaction, order_id: int) -> None:
    if not await require_trading_enabled(interaction):
        return
    ensure_user(interaction.user)
    uid = interaction.user.id

    with db() as conn:
        row = conn.execute("SELECT * FROM orders WHERE order_id = ?", (order_id,)).fetchone()
        if not row:
            await interaction.response.send_message("Order not found.", ephemeral=True)
            return
        if row["status"] != "matched":
            await interaction.response.send_message(f"Order #{order_id} is `{row['status']}`.", ephemeral=True)
            return
        is_seller = row["seller_id"] == uid
        is_buyer = row["buyer_id"] == uid
        if not is_seller and not is_buyer:
            await interaction.response.send_message("You are not part of this trade.", ephemeral=True)
            return
        column = "seller_confirmed" if is_seller else "buyer_confirmed"
        conn.execute(f"UPDATE orders SET {column} = 1, updated_at = ? WHERE order_id = ?", (now_ts(), order_id))
        conn.commit()
        row = conn.execute("SELECT * FROM orders WHERE order_id = ?", (order_id,)).fetchone()

    if not (row["seller_confirmed"] and row["buyer_confirmed"]):
        other = "buyer" if is_seller else "seller"
        await interaction.response.send_message(f"Confirmed. Waiting for the {other}.", ephemeral=True)
        return

    await interaction.response.defer(thinking=True, ephemeral=True)
    with db() as conn:
        updated = conn.execute(
            "UPDATE orders SET status = 'releasing', updated_at = ? WHERE order_id = ? AND status = 'matched'",
            (now_ts(), order_id),
        ).rowcount
        conn.commit()
    safe_export_public_feed()
    if not updated:
        await interaction.followup.send("Release already started or order status changed.", ephemeral=True)
        return

    amount = order_amount(row)
    fee = fee_for_amount(amount)
    payout = amount - fee
    buyer_wallet = get_user_wallet(row["buyer_id"])
    if not buyer_wallet:
        with db() as conn:
            conn.execute("UPDATE orders SET status = 'release_failed', updated_at = ? WHERE order_id = ?", (now_ts(), order_id))
            conn.commit()
        safe_export_public_feed()
        await interaction.followup.send("Buyer has no withdrawal address. Admin needs to resolve this.", ephemeral=True)
        return

    try:
        txout = await run_blocking(btc09_send, buyer_wallet, payout)
    except Exception as exc:
        with db() as conn:
            conn.execute("UPDATE orders SET status = 'release_failed', updated_at = ? WHERE order_id = ?", (now_ts(), order_id))
            conn.commit()
        safe_export_public_feed()
        await interaction.followup.send(f"Release failed: `{str(exc)[:500]}`", ephemeral=True)
        for admin_id in ADMIN_IDS:
            await dm_user(admin_id, escrow_embed(f"Release failed for order #{order_id}", str(exc)[:1000], color=0xED4245))
        return

    with db() as conn:
        conn.execute(
            """
            UPDATE orders
            SET status = 'completed', release_txid = ?, fee = ?, updated_at = ?, completed_at = ?
            WHERE order_id = ?
            """,
            (txout[:500], fmt_amt(fee), now_ts(), now_ts(), order_id),
        )
        conn.commit()
    safe_export_public_feed()

    done = escrow_embed(
        f"Order #{order_id} completed",
        f"Released `{fmt_amt(payout)} {COIN_TICKER}` to buyer.\nFee: `{fmt_amt(fee)} {COIN_TICKER}`\nTX: `{txout[:180]}`",
        color=0x57F287,
    )
    await interaction.followup.send(embed=done, ephemeral=True)
    await dm_user(row["seller_id"], done)
    await dm_user(row["buyer_id"], done)


@bot.tree.command(name="cancel", description="Cancel an order")
@app_commands.describe(order_id="Order ID")
async def cancel(interaction: discord.Interaction, order_id: int) -> None:
    if not await require_trading_enabled(interaction):
        return
    ensure_user(interaction.user)
    row = fetch_order(order_id)
    if not row:
        await interaction.response.send_message("Order not found.", ephemeral=True)
        return

    uid = interaction.user.id
    is_seller = row["seller_id"] == uid
    is_buyer = row["buyer_id"] == uid
    admin = is_admin(uid)
    if not (is_seller or is_buyer or admin):
        await interaction.response.send_message("You are not part of this order.", ephemeral=True)
        return

    await interaction.response.defer(thinking=True, ephemeral=True)

    if row["status"] == "pending_deposit" and (is_seller or admin):
        with db() as conn:
            conn.execute(
                "UPDATE orders SET status = 'cancelled', cancel_reason = 'seller cancelled before deposit', updated_at = ? WHERE order_id = ? AND status = 'pending_deposit'",
                (now_ts(), order_id),
            )
            conn.commit()
        safe_export_public_feed()
        await interaction.followup.send(f"Order #{order_id} cancelled.", ephemeral=True)
        return

    if row["status"] == "matched" and is_buyer and not row["buyer_confirmed"]:
        with db() as conn:
            conn.execute(
                """
                UPDATE orders
                SET status = 'open', buyer_id = NULL, buyer_name = NULL, buyer_confirmed = 0,
                    seller_confirmed = 0, matched_at = NULL, cancel_reason = 'buyer cancelled before confirming',
                    updated_at = ?
                WHERE order_id = ? AND status = 'matched'
                """,
                (now_ts(), order_id),
            )
            conn.commit()
        safe_export_public_feed()
        await interaction.followup.send(f"Order #{order_id} is open again.", ephemeral=True)
        await dm_user(row["seller_id"], escrow_embed(f"Order #{order_id} buyer cancelled", "The order is open again."))
        return

    if row["status"] == "matched" and is_seller and not admin:
        await interaction.followup.send("Seller cannot cancel after a buyer accepts. Use `/dispute`.", ephemeral=True)
        return

    if row["status"] == "open" and (is_seller or admin):
        seller_wallet = get_user_wallet(row["seller_id"])
        if not seller_wallet:
            await interaction.followup.send("Seller has no refund address set. Use `/setaddress` then ask admin.", ephemeral=True)
            return
        amount = order_amount(row)
        try:
            txout = await run_blocking(btc09_send, seller_wallet, amount)
        except Exception as exc:
            await interaction.followup.send(f"Refund failed: `{str(exc)[:500]}`", ephemeral=True)
            return
        with db() as conn:
            conn.execute(
                """
                UPDATE orders
                SET status = 'cancelled_refunded', refund_txid = ?, cancel_reason = 'seller/admin cancelled open order',
                    updated_at = ?, completed_at = ?
                WHERE order_id = ? AND status = 'open'
                """,
                (txout[:500], now_ts(), now_ts(), order_id),
            )
            conn.commit()
        safe_export_public_feed()
        await interaction.followup.send(
            f"Order #{order_id} cancelled and `{fmt_amt(amount)} {COIN_TICKER}` refunded.\nTX: `{txout[:180]}`",
            ephemeral=True,
        )
        return

    await interaction.followup.send(f"Order #{order_id} cannot be cancelled while `{row['status']}`. Use `/dispute`.", ephemeral=True)


@bot.tree.command(name="setaddress", description="Set your 09C receiving/refund address")
@app_commands.describe(address="Your 09C address")
async def setaddress(interaction: discord.Interaction, address: str) -> None:
    ensure_user(interaction.user)
    try:
        clean = validate_09c_address(address)
    except ValueError as exc:
        await interaction.response.send_message(f"Invalid 09C address: {exc}", ephemeral=True)
        return
    set_user_wallet(interaction.user.id, clean)
    await interaction.response.send_message(f"Address set: `{clean}`", ephemeral=True)


@bot.tree.command(name="balance", description="Show escrow wallet accounting")
async def balance(interaction: discord.Interaction) -> None:
    ensure_user(interaction.user)
    await interaction.response.defer(thinking=True, ephemeral=True)
    try:
        total, address_count = await run_blocking(wallet_total_balance)
    except Exception as exc:
        await interaction.followup.send(f"Balance check failed: `{str(exc)[:500]}`", ephemeral=True)
        return
    locked = locked_order_balance()
    fees = fee_available()
    await interaction.followup.send(
        embed=escrow_embed(
            "Escrow accounting",
            "\n".join(
                [
                    f"Wallet spendable: `{fmt_amt(total)} {COIN_TICKER}` across `{address_count}` wallet addresses.",
                    f"Locked in active orders: `{fmt_amt(locked)} {COIN_TICKER}`.",
                    f"Withdrawable recorded fees: `{fmt_amt(fees)} {COIN_TICKER}`.",
                    "",
                    "This is a hot-wallet bot. Use small OTC size until the flow has real history.",
                ]
            ),
        ),
        ephemeral=True,
    )


@bot.tree.command(name="stats", description="Show live Bitcoin 09 mining and network stats")
async def stats(interaction: discord.Interaction) -> None:
    await interaction.response.defer(thinking=True)
    try:
        text = await run_blocking(network_stats_text)
    except Exception as exc:
        await interaction.followup.send(f"Stats check failed: `{str(exc)[:500]}`", ephemeral=True)
        return
    await interaction.followup.send(text, allowed_mentions=discord.AllowedMentions.none())


@bot.tree.command(name="dispute", description="Open a dispute on a trade")
@app_commands.describe(order_id="Order ID")
async def dispute(interaction: discord.Interaction, order_id: int) -> None:
    if not await require_trading_enabled(interaction):
        return
    ensure_user(interaction.user)
    row = fetch_order(order_id)
    if not row:
        await interaction.response.send_message("Order not found.", ephemeral=True)
        return
    if interaction.user.id not in (row["seller_id"], row["buyer_id"]) and not is_admin(interaction.user.id):
        await interaction.response.send_message("You are not part of this order.", ephemeral=True)
        return
    if row["status"] not in ("open", "matched", "release_failed", "disputed"):
        await interaction.response.send_message(f"Order #{order_id} is `{row['status']}`.", ephemeral=True)
        return
    with db() as conn:
        conn.execute(
            "UPDATE orders SET status = 'disputed', disputed_at = ?, updated_at = ? WHERE order_id = ?",
            (now_ts(), now_ts(), order_id),
        )
        conn.commit()
    safe_export_public_feed()
    await interaction.response.send_message(f"Dispute opened for order #{order_id}. Admin has been notified.", ephemeral=True)
    for admin_id in ADMIN_IDS:
        await dm_user(
            admin_id,
            escrow_embed(
                f"Dispute opened for order #{order_id}",
                f"Seller: `{row['seller_name']}`\nBuyer: `{row['buyer_name'] or 'none'}`\nAmount: `{row['amount']} {COIN_TICKER}`\nResolve with `/admin resolve {order_id} buyer` or `/admin resolve {order_id} seller`.",
                color=0xFEE75C,
            ),
        )


@bot.tree.command(name="admin", description="Admin: resolve/stats/orders")
@app_commands.describe(action="resolve, stats, or orders", order_id="Order ID", winner="buyer or seller")
async def admin_cmd(interaction: discord.Interaction, action: str, order_id: int = 0, winner: str = "") -> None:
    if not is_admin(interaction.user.id):
        await interaction.response.send_message("Admin only.", ephemeral=True)
        return
    action = action.strip().lower()

    if action == "stats":
        with db() as conn:
            counts = conn.execute("SELECT status, COUNT(*) AS n FROM orders GROUP BY status ORDER BY status").fetchall()
        status_text = "\n".join(f"`{row['status']}`: {row['n']}" for row in counts) or "No orders."
        total, address_count = await run_blocking(wallet_total_balance)
        await interaction.response.send_message(
            embed=escrow_embed(
                "OTC bot stats",
                f"{status_text}\n\nWallet: `{fmt_amt(total)} {COIN_TICKER}` over `{address_count}` addresses.\nFees available: `{fmt_amt(fee_available())} {COIN_TICKER}`.",
            ),
            ephemeral=True,
        )
        return

    if action == "orders":
        with db() as conn:
            rows = conn.execute("SELECT * FROM orders ORDER BY order_id DESC LIMIT 15").fetchall()
        if not rows:
            await interaction.response.send_message("No orders.", ephemeral=True)
            return
        lines = [
            f"#{row['order_id']} `{row['status']}` `{row['amount']} {COIN_TICKER}` seller `{row['seller_name']}` buyer `{row['buyer_name'] or '-'}`"
            for row in rows
        ]
        await interaction.response.send_message(embed=escrow_embed("Recent orders", "\n".join(lines)), ephemeral=True)
        return

    if action != "resolve":
        await interaction.response.send_message("Actions: `resolve`, `stats`, `orders`.", ephemeral=True)
        return

    if not await require_trading_enabled(interaction):
        return

    winner = winner.strip().lower()
    if winner not in ("buyer", "seller") or not order_id:
        await interaction.response.send_message("Usage: `/admin resolve <order_id> <buyer|seller>`.", ephemeral=True)
        return

    row = fetch_order(order_id)
    if not row:
        await interaction.response.send_message("Order not found.", ephemeral=True)
        return
    if row["status"] not in ("disputed", "release_failed", "matched", "open"):
        await interaction.response.send_message(f"Order #{order_id} is `{row['status']}` and should not be resolved.", ephemeral=True)
        return
    if winner == "buyer" and not row["buyer_id"]:
        await interaction.response.send_message("No buyer is attached to this order.", ephemeral=True)
        return

    amount = order_amount(row)
    fee = fee_for_amount(amount) if winner == "buyer" else Decimal("0")
    payout = amount - fee
    target_id = row["buyer_id"] if winner == "buyer" else row["seller_id"]
    target_wallet = get_user_wallet(target_id)
    if not target_wallet:
        await interaction.response.send_message(f"The {winner} has no 09C address set.", ephemeral=True)
        return

    await interaction.response.defer(thinking=True, ephemeral=True)
    try:
        txout = await run_blocking(btc09_send, target_wallet, payout)
    except Exception as exc:
        await interaction.followup.send(f"Resolve send failed: `{str(exc)[:500]}`", ephemeral=True)
        return

    status = f"resolved_{winner}"
    with db() as conn:
        conn.execute(
            """
            UPDATE orders
            SET status = ?, release_txid = ?, fee = ?, updated_at = ?, completed_at = ?
            WHERE order_id = ?
            """,
            (status, txout[:500], fmt_amt(fee) if fee else None, now_ts(), now_ts(), order_id),
        )
        conn.commit()
    safe_export_public_feed()
    await interaction.followup.send(
        f"Order #{order_id} resolved to {winner}. Sent `{fmt_amt(payout)} {COIN_TICKER}`.\nTX: `{txout[:180]}`",
        ephemeral=True,
    )
    await dm_user(target_id, escrow_embed(f"Order #{order_id} resolved", f"Sent `{fmt_amt(payout)} {COIN_TICKER}` to your address."))


@bot.tree.command(name="withdraw", description="Admin: withdraw recorded escrow fees")
@app_commands.describe(amount="Amount of 09C fees to withdraw", address="Destination 09C address")
async def withdraw(interaction: discord.Interaction, amount: str, address: str) -> None:
    if not await require_trading_enabled(interaction):
        return
    if not is_admin(interaction.user.id):
        await interaction.response.send_message("Admin only.", ephemeral=True)
        return
    try:
        amt = parse_coin_amount(amount)
        clean_addr = validate_09c_address(address)
    except ValueError as exc:
        await interaction.response.send_message(f"Invalid withdraw request: {exc}", ephemeral=True)
        return
    available = fee_available()
    if amt > available:
        await interaction.response.send_message(
            f"Only `{fmt_amt(available)} {COIN_TICKER}` in recorded fees is withdrawable.",
            ephemeral=True,
        )
        return

    await interaction.response.defer(thinking=True, ephemeral=True)
    try:
        txout = await run_blocking(btc09_send, clean_addr, amt)
    except Exception as exc:
        await interaction.followup.send(f"Withdraw failed: `{str(exc)[:500]}`", ephemeral=True)
        return

    with db() as conn:
        conn.execute(
            """
            INSERT INTO withdrawals (admin_id, amount, address, txid, status, created_at)
            VALUES (?, ?, ?, ?, 'completed', ?)
            """,
            (interaction.user.id, fmt_amt(amt), clean_addr, txout[:500], now_ts()),
        )
        conn.commit()
    await interaction.followup.send(f"Withdrew `{fmt_amt(amt)} {COIN_TICKER}` in fees.\nTX: `{txout[:180]}`", ephemeral=True)


@bot.tree.command(name="help", description="Show OTC escrow bot commands")
async def help_cmd(interaction: discord.Interaction) -> None:
    ensure_user(interaction.user)
    await interaction.response.send_message(
        embed=escrow_embed(
            "OTC escrow commands",
            "\n".join(
                [
                    "`/setaddress <addr>` - set your 09C receive/refund address.",
                    "New escrow orders are temporarily paused during the controlled WTS/WTB pilot.",
                    "`/orders` - list open orders.",
                    "`/balance` - show escrow accounting.",
                    "",
                    "The bot holds 09C only. Payment in USDT/BTC/fiat happens between buyer and seller.",
                    "Do not send 09C to an old deposit address. The verified pilot launch will be posted in #announcements.",
                ]
            ),
        ),
        ephemeral=True,
    )


async def notify_deposit(order_id: int, seller_id: int, amount: str) -> None:
    await dm_user(seller_id, escrow_embed(f"Order #{order_id} is live", f"`{amount} {COIN_TICKER}` is now in escrow."))


async def notify_timeout(order_id: int) -> None:
    for admin_id in ADMIN_IDS:
        await dm_user(admin_id, escrow_embed(f"Order #{order_id} auto-disputed", "Matched trade timed out after 24 hours.", color=0xFEE75C))


def deposit_checker() -> None:
    while True:
        if not TRADING_ENABLED:
            time.sleep(60)
            continue
        try:
            cutoff = now_ts() - ORDER_TIMEOUT_SECONDS
            with db() as conn:
                stale = conn.execute(
                    "SELECT * FROM orders WHERE status = 'matched' AND matched_at IS NOT NULL AND matched_at < ?",
                    (cutoff,),
                ).fetchall()
            for row in stale:
                with db() as conn:
                    updated = conn.execute(
                        "UPDATE orders SET status = 'disputed', disputed_at = ?, updated_at = ? WHERE order_id = ? AND status = 'matched'",
                        (now_ts(), now_ts(), row["order_id"]),
                    ).rowcount
                    conn.commit()
                if updated:
                    print(f"[INFO] Auto-disputed stale order #{row['order_id']}")
                    safe_export_public_feed()
                    dispatch(notify_timeout(row["order_id"]))

            with db() as conn:
                pending = conn.execute("SELECT * FROM orders WHERE status = 'pending_deposit'").fetchall()
            for row in pending:
                if not row["deposit_addr"]:
                    continue
                try:
                    bal = explorer_balance(row["deposit_addr"])
                except Exception as exc:
                    print(f"[WARN] deposit check failed for order #{row['order_id']}: {exc}")
                    continue
                amt = order_amount(row)
                if bal < amt:
                    continue
                with db() as conn:
                    updated = conn.execute(
                        """
                        UPDATE orders
                        SET status = 'open', deposit_confirmed_balance = ?, updated_at = ?
                        WHERE order_id = ? AND status = 'pending_deposit'
                        """,
                        (fmt_amt(bal), now_ts(), row["order_id"]),
                    ).rowcount
                    conn.commit()
                if updated:
                    print(f"[INFO] Auto-confirmed deposit for order #{row['order_id']}")
                    safe_export_public_feed()
                    dispatch(notify_deposit(row["order_id"], row["seller_id"], row["amount"]))
        except Exception as exc:
            print(f"[ERROR] deposit checker: {exc}")
        time.sleep(60)


def main() -> None:
    if not BOT_TOKEN:
        raise SystemExit("Set BOT_TOKEN or DISCORD_BOT_TOKEN")
    if not ADMIN_IDS:
        raise SystemExit("Set ADMIN_IDS")
    init_db()
    safe_export_public_feed()
    print(f"[INFO] Starting {BOT_VERSION}")
    print(f"[INFO] BTC09_DATADIR={BTC09_DATADIR}")
    print(f"[INFO] Explorer={EXPLORER_URL}")
    print(f"[INFO] Admin count={len(ADMIN_IDS)}")
    print(f"[INFO] OTC trading enabled={TRADING_ENABLED}")
    thread = threading.Thread(target=deposit_checker, daemon=True)
    thread.start()
    bot.run(BOT_TOKEN)


if __name__ == "__main__":
    main()
