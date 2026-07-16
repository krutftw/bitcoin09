import pathlib
import unittest


class WindowsStorePackageContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.script_path = pathlib.Path("tools/release/package_windows_store.ps1")
        cls.script = cls.script_path.read_text(encoding="utf-8")

    def test_requires_partner_center_identity_without_embedding_credentials(self):
        for token in (
            "IdentityName",
            "Publisher",
            "PublisherDisplayName",
            "ProcessorArchitecture=\"x64\"",
            "uap10:RuntimeBehavior=\"packagedClassicApp\"",
            "uap10:TrustLevel=\"mediumIL\"",
            "rescap:Capability Name=\"runFullTrust\"",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.script)

        for forbidden in ("BEGIN PRIVATE KEY", "PFX_PASSWORD", "signtool sign"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, self.script)

    def test_packages_only_the_wallet_shell_core_and_store_artwork(self):
        for token in (
            "npm run store:build -- --no-bundle",
            "btc09-wallet.exe",
            "btc09-core.exe",
            "wallet edition",
            "Square150x150Logo.png",
            "Square44x44Logo.png",
            "StoreLogo.png",
            "MakeAppx.exe",
            "MakeAppx pack",
            "function Get-PeMachine",
            "Assert-X64Pe",
            "0x8664",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.script)

        self.assertNotIn("downloadBootstrapper", self.script)

    def test_store_version_is_valid_and_derived_from_the_wallet_version(self):
        for token in (
            "Cargo.toml",
            "0.1.33",
            "1.33.0.0",
            "Version must use 0.minor.patch",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.script)


if __name__ == "__main__":
    unittest.main()
