import unittest
from decimal import localcontext

from bot.otc.domain import (
    FeeQuote,
    Money,
    OrderSide,
    OrderState,
    _decimal_text_to_units,
    parse_09c,
    parse_asset,
    parse_method,
    quote_deposit,
)


class DomainTests(unittest.TestCase):
    def test_money_accepts_zero_and_positive_integer_units(self):
        self.assertEqual(Money(0).units, 0)
        self.assertEqual(Money(100_000_001).units, 100_000_001)

    def test_money_rejects_negative_units(self):
        with self.assertRaises(ValueError):
            Money(-1)

    def test_money_rejects_non_integer_units(self):
        for bad in (True, 1.0, "1"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                Money(bad)

    def test_amount_is_exact_integer_units(self):
        self.assertEqual(parse_09c("1.00000001"), 100_000_001)
        for bad in ("0", "-1", "1.000000001", "nan", "inf", True, 1.0):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_09c(bad)

    def test_large_amount_digit_conversion_does_not_round(self):
        self.assertEqual(
            _decimal_text_to_units("123456789012345678901.12345678"),
            12_345_678_901_234_567_890_112_345_678,
        )
        with self.assertRaises(ValueError):
            parse_09c("123456789012345678901.12345678")

    def test_public_amount_conversion_ignores_decimal_context(self):
        with localcontext() as context:
            context.prec = 1
            self.assertEqual(parse_09c("21000000.00000000"), 2_100_000_000_000_000)

    def test_amount_requires_plain_ascii_decimal_text(self):
        for bad in ("+1", ".1", "1.", "1E2", "１"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_09c(bad)

    def test_amount_rejects_scientific_notation_and_huge_exponents(self):
        with self.assertRaises(ValueError):
            parse_09c("1e2")
        with self.assertRaises(ValueError):
            parse_09c("1e1000000000")

    def test_amount_enforces_protocol_max_supply(self):
        self.assertEqual(parse_09c("21000000"), 2_100_000_000_000_000)
        with self.assertRaises(ValueError):
            parse_09c("21000000.00000001")

    def test_asset_and_method_validation(self):
        self.assertEqual(parse_asset(" usdt "), "USDT")
        self.assertEqual(parse_asset("x-custom"), "X-CUSTOM")
        self.assertEqual(parse_method("PayID"), "PayID")
        self.assertEqual(parse_method(" Pay  ID "), "Pay ID")
        for bad in ("$", "A", "USDT ERC20 PLEASE DM ME", "US DT"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_asset(bad)

    def test_method_rejects_non_plain_or_invalid_length_input(self):
        for bad in ("A", "A" * 33, "Pay💸", "Pay\nID", "Pay\u00a0ID", "Pay\x00ID"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_method(bad)

    def test_zero_percent_quote_reserves_network_fee(self):
        quote = quote_deposit(net_amount=5_000_000_000, network_fee=10_000, fee_bps=0)
        self.assertEqual(quote, FeeQuote(net_amount=5_000_000_000, network_fee=10_000, service_fee=0, deposit_required=5_000_010_000))

    def test_quote_rounds_nonzero_service_fee_up_in_integer_units(self):
        self.assertEqual(
            quote_deposit(net_amount=1, network_fee=0, fee_bps=1),
            FeeQuote(net_amount=1, network_fee=0, service_fee=1, deposit_required=2),
        )

    def test_quote_rejects_non_integer_boundary_values(self):
        valid = {"net_amount": 100, "network_fee": 10, "fee_bps": 1}
        for field in valid:
            for bad in (True, 1.5, "1"):
                values = valid | {field: bad}
                with self.subTest(field=field, bad=bad), self.assertRaises(ValueError):
                    quote_deposit(**values)

    def test_quote_rejects_fee_bps_outside_basis_point_range(self):
        for bad in (-1, 10_001):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                quote_deposit(net_amount=100, network_fee=10, fee_bps=bad)

    def test_fee_quote_rejects_non_integer_amounts(self):
        valid = {"net_amount": 100, "network_fee": 10, "service_fee": 1, "deposit_required": 111}
        for field in valid:
            for bad in (True, 1.5, "1"):
                values = valid | {field: bad}
                with self.subTest(field=field, bad=bad), self.assertRaises(ValueError):
                    FeeQuote(**values)

    def test_fee_quote_rejects_invalid_amounts_or_total(self):
        bad_quotes = (
            {"net_amount": 0, "network_fee": 0, "service_fee": 0, "deposit_required": 0},
            {"net_amount": 100, "network_fee": -1, "service_fee": 1, "deposit_required": 100},
            {"net_amount": 100, "network_fee": 1, "service_fee": -1, "deposit_required": 100},
            {"net_amount": 100, "network_fee": 1, "service_fee": 1, "deposit_required": -1},
            {"net_amount": 100, "network_fee": 1, "service_fee": 1, "deposit_required": 101},
        )
        for values in bad_quotes:
            with self.subTest(values=values), self.assertRaises(ValueError):
                FeeQuote(**values)

    def test_order_states_are_explicit(self):
        self.assertEqual(OrderSide.BUY.value, "buy")
        self.assertEqual(OrderState.TRANSFER_UNCERTAIN.value, "transfer_uncertain")


if __name__ == "__main__":
    unittest.main()
