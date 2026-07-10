from __future__ import annotations

import re
from dataclasses import dataclass
from enum import StrEnum

UNITS_PER_09C = 100_000_000
MAX_09C_UNITS = 21_000_000 * UNITS_PER_09C
AMOUNT_09C_RE = re.compile(r"(?P<whole>[0-9]+)(?:\.(?P<fraction>[0-9]{1,8}))?\Z")
ASSET_RE = re.compile(r"[A-Z0-9._-]{2,12}\Z")
METHOD_RE = re.compile(r"[A-Za-z0-9 ._+/-]{2,32}\Z")
METHOD_INPUT_RE = re.compile(r"[A-Za-z0-9 ._+/-]*\Z")


class OrderSide(StrEnum):
    BUY = "buy"
    SELL = "sell"


class OrderState(StrEnum):
    AWAITING_DEPOSIT = "awaiting_deposit"
    OPEN = "open"
    MATCHED = "matched"
    DISPUTED = "disputed"
    RELEASE_RESERVED = "release_reserved"
    REFUND_RESERVED = "refund_reserved"
    BROADCAST = "broadcast"
    COMPLETED = "completed"
    REFUNDED = "refunded"
    CANCELLED = "cancelled"
    DEPOSIT_EXPIRED = "deposit_expired"
    TRANSFER_FAILED_SAFE = "transfer_failed_safe"
    TRANSFER_UNCERTAIN = "transfer_uncertain"


class TransferState(StrEnum):
    RESERVED = "reserved"
    BROADCAST = "broadcast"
    CONFIRMED = "confirmed"
    FAILED_SAFE = "failed_safe"
    UNCERTAIN = "uncertain"


@dataclass(frozen=True)
class Money:
    units: int

    def __post_init__(self) -> None:
        if type(self.units) is not int:
            raise ValueError("money units must be an integer")
        if self.units < 0:
            raise ValueError("money units must be non-negative")


@dataclass(frozen=True)
class FeeQuote:
    net_amount: int
    network_fee: int
    service_fee: int
    deposit_required: int

    def __post_init__(self) -> None:
        amounts = (self.net_amount, self.network_fee, self.service_fee, self.deposit_required)
        if any(type(amount) is not int for amount in amounts):
            raise ValueError("fee quote amounts must be integers")
        if self.net_amount <= 0:
            raise ValueError("net amount must be positive")
        if self.network_fee < 0 or self.service_fee < 0 or self.deposit_required < 0:
            raise ValueError("fee quote amounts must not be negative")
        if self.deposit_required != self.net_amount + self.network_fee + self.service_fee:
            raise ValueError("deposit required must equal net amount plus fees")


@dataclass(frozen=True)
class SettlementTerms:
    asset: str
    method: str
    network: str | None


def _decimal_text_to_units(value: str, *, max_units: int | None = None) -> int:
    match = AMOUNT_09C_RE.fullmatch(value)
    if match is None:
        raise ValueError("09C amount must be a positive plain decimal with at most 8 decimals")

    units = 0
    whole = match.group("whole").lstrip("0")
    fraction = (match.group("fraction") or "").ljust(8, "0")
    for digit in whole:
        units = units * 10 + ord(digit) - ord("0")
        if max_units is not None and units > max_units:
            raise ValueError("09C amount must not exceed 21,000,000 09C")
    for digit in fraction:
        units = units * 10 + ord(digit) - ord("0")
        if max_units is not None and units > max_units:
            raise ValueError("09C amount must not exceed 21,000,000 09C")
    return units


def parse_09c(value: str) -> int:
    if type(value) is not str:
        raise ValueError("09C amount must be a positive plain decimal with at most 8 decimals")
    units = _decimal_text_to_units(value.strip(), max_units=MAX_09C_UNITS)
    if units == 0:
        raise ValueError("09C amount must be positive")
    return units


def parse_asset(value: str) -> str:
    asset = value.strip().upper()
    if not ASSET_RE.fullmatch(asset):
        raise ValueError("asset must be 2-12 letters, numbers, dot, underscore, or hyphen")
    return asset


def parse_method(value: str) -> str:
    if not isinstance(value, str) or not METHOD_INPUT_RE.fullmatch(value):
        raise ValueError("payment method must be 2-32 plain characters")
    method = " ".join(value.strip().split())
    if not METHOD_RE.fullmatch(method):
        raise ValueError("payment method must be 2-32 plain characters")
    return method


def quote_deposit(*, net_amount: int, network_fee: int, fee_bps: int) -> FeeQuote:
    if any(type(value) is not int for value in (net_amount, network_fee, fee_bps)):
        raise ValueError("fee quote values must be integers")
    if net_amount <= 0 or network_fee < 0 or not 0 <= fee_bps <= 10_000:
        raise ValueError("invalid fee quote")
    service_fee = (net_amount * fee_bps + 9_999) // 10_000
    return FeeQuote(net_amount, network_fee, service_fee, net_amount + network_fee + service_fee)
