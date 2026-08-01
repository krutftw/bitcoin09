import json
import pathlib
import unittest


class ExchangeStatusContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = pathlib.Path("docs")
        cls.data = json.loads((cls.root / "exchanges.json").read_text(encoding="utf-8"))
        cls.exchange_page = (cls.root / "exchanges.html").read_text(encoding="utf-8")
        cls.support_page = (cls.root / "support.html").read_text(encoding="utf-8")

    def test_summary_matches_exchange_rows(self):
        counts = {}
        for venue in self.data["exchanges"]:
            counts[venue["status"]] = counts.get(venue["status"], 0) + 1
            self.assertTrue(venue["official_url"].startswith("https://"))
            self.assertTrue(venue["public_note"].strip())

        summary = self.data["summary"]
        self.assertEqual(summary["awaiting_reply"], counts["awaiting_reply"])
        self.assertEqual(summary["terms_requested"], counts["terms_requested"])
        self.assertEqual(summary["requirements_needed"], counts["requirements_needed"])
        self.assertEqual(summary["engineering_needed"], counts["engineering_needed"])
        self.assertEqual(summary["paid_routes_published"], counts["funding_needed"])

    def test_funding_math_and_route_are_explicit(self):
        funding = self.data["funding"]
        cash_items = sum(item["amount_usd"] for item in funding["items"] if item["asset"] == "cash")
        coin_items = sum(item["amount_usd"] for item in funding["items"] if item["asset"] == "09C")
        self.assertEqual(funding["cash_target_usd"], cash_items)
        self.assertEqual(funding["coin_liquidity_target_usd"], coin_items)
        self.assertEqual(funding["total_package_usd"], cash_items + coin_items)
        self.assertEqual(funding["donation_url"], "https://btc09.org/support.html#contribute")
        self.assertNotIn("provider_fallback_url", funding)
        self.assertEqual(funding["status_url"], "https://btc09.org/api/support/v1/status")
        for phrase in ("Funds are reserved", "approves BTC09", "confirms its current integration"):
            self.assertIn(phrase, funding["note"])

    def test_public_pages_link_the_tracker_without_claiming_a_listing(self):
        home = (self.root / "index.html").read_text(encoding="utf-8")
        terms = (self.root / "terms.html").read_text(encoding="utf-8")
        privacy = (self.root / "privacy.html").read_text(encoding="utf-8")
        sitemap = (self.root / "sitemap.xml").read_text(encoding="utf-8")

        for token in ("exchanges.json", "Statuses come from submitted forms", "View funding target"):
            self.assertIn(token, self.exchange_page)
        for token in (
            'href="status.css?v=20260801-supporters"',
            "US$4,299",
            "Useful extras for people backing the work",
            "try again later",
            "setInterval(() => loadFunding()",
            "Promise.all([loadFunding(), loadCurrencies()])",
            "The current support target has been reached.",
            "'X-BTC09-Payment-Token': saved.token",
            "/api/support/v1/currencies",
            "/api/support/v1/payments",
            "after NOWPayments marks the payment finished",
            "US$25 · 🤝 Backer",
            "US$250 · ⭐ Core Supporter",
            "private build updates",
            "priority issue triage",
            "direct project feedback thread",
            "supporter lab",
            "/support claim",
            "const activePaymentKey = 'btc09-support-payment'",
            "btc09-support-claims",
            "Saved Discord claim codes",
            "saveFinishedClaim(saved)",
            "clearActivePayment()",
            "Each payment code can only be linked to one Discord account",
        ):
            self.assertIn(token, self.support_page)
        self.assertNotIn("?token=", self.support_page)
        self.assertNotIn("sessionStorage", self.support_page)
        self.assertIn('href="exchanges.html"', home)
        self.assertIn('href="support.html"', home)
        self.assertIn("Project support", terms)
        self.assertIn("payment provider", privacy)
        self.assertIn("https://btc09.org/exchanges.html", sitemap)
        self.assertIn("https://btc09.org/support.html", sitemap)


if __name__ == "__main__":
    unittest.main()
