import json
import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
DOCS = ROOT / "docs"


class DecommissionContractTests(unittest.TestCase):
    def test_public_pages_are_closed(self):
        expected = {
            "index.html": "Bitcoin 09 has been discontinued.",
            "support.html": "Support and funding have ended.",
            "markets.html": "Official OTC trading and escrow are closed.",
            "exchanges.html": "Exchange outreach has ended.",
            "mining.html": "Official mining services are closed.",
        }
        for filename, closure_text in expected.items():
            page = (DOCS / filename).read_text(encoding="utf-8")
            with self.subTest(filename=filename):
                self.assertIn(closure_text, page)
                self.assertNotIn("/api/support/v1/payments", page)
                self.assertNotIn("/trade sell", page)
                self.assertNotIn("Escrow is live", page)

    def test_public_feeds_cannot_advertise_activity(self):
        exchanges = json.loads((DOCS / "exchanges.json").read_text(encoding="utf-8"))
        market = json.loads((DOCS / "market-data.json").read_text(encoding="utf-8"))
        escrow = json.loads((DOCS / "otc-bot-feed.json").read_text(encoding="utf-8"))

        self.assertEqual(exchanges["project_status"], "discontinued")
        self.assertFalse(exchanges["funding"]["accepting_payments"])
        self.assertEqual(exchanges["exchanges"], [])
        self.assertEqual(market["trading_status"], "closed")
        self.assertEqual(market["offers"], [])
        self.assertEqual(escrow["service_status"], "closed")
        self.assertEqual(escrow["orders"], [])

    def test_archive_notice_warns_against_old_addresses(self):
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        status = (ROOT / "PROJECT_STATUS.md").read_text(encoding="utf-8")
        self.assertIn("Do not send coins or payments", readme)
        self.assertIn("Nobody should send", status)
        self.assertIn("funds to an old project-controlled address", status)


if __name__ == "__main__":
    unittest.main()
