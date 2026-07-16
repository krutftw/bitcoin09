import pathlib
import unittest


class StoreReadinessContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.listing = pathlib.Path("docs/store/listing-en.md").read_text(encoding="utf-8")
        cls.checklist = pathlib.Path("docs/store/RELEASE-CHECKLIST.md").read_text(encoding="utf-8")
        cls.privacy = pathlib.Path("docs/privacy.html").read_text(encoding="utf-8")

    def test_listing_is_plain_and_does_not_sell_a_financial_outcome(self):
        for token in (
            "BTC09 Wallet",
            "self-custody",
            "Create or restore",
            "Review the address, amount and fee",
            "No account is required",
            "not an exchange",
            "do not mine",
            "https://btc09.org/privacy.html",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.listing)

        for forbidden in ("guaranteed", "profit", "investment opportunity", "revolutionary"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, self.listing.lower())

    def test_store_accounts_and_signing_stay_explicit(self):
        for token in (
            "Microsoft Company account",
            "Google Play Organization account",
            "Apple Developer Program organization",
            "D-U-N-S",
            "US$25",
            "US$99 per year",
            "BTC09_ANDROID_KEYSTORE",
            "Partner Center identity",
            "Do not publish",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.checklist)

        for forbidden in ("keystore password:", "private key:", "seed phrase:"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, self.checklist.lower())

    def test_privacy_page_covers_the_native_mobile_wallet_and_camera(self):
        for token in (
            "Mobile wallet",
            "operating system",
            "camera",
            "not uploaded",
            "signed transaction",
            "recovery words",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.privacy)


if __name__ == "__main__":
    unittest.main()
