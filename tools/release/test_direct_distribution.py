import pathlib
import unittest


class DirectDistributionContractTest(unittest.TestCase):
    def test_public_guide_describes_the_no_store_release_without_overpromising(self):
        guide_path = pathlib.Path("docs/DIRECT-DOWNLOADS.md")
        self.assertTrue(guide_path.exists(), "direct-download guide is missing")
        guide = guide_path.read_text(encoding="utf-8") if guide_path.exists() else ""

        for token in (
            "GitHub Releases",
            "SignPath Foundation",
            "wallet-only",
            "signed APK",
            "AppImage",
            "standard sideload permission",
            "macOS",
            "iPhone",
            "SHA256SUMS",
        ):
            with self.subTest(token=token):
                self.assertIn(token, guide)

        self.assertNotIn("available now", guide.lower())

    def test_code_signing_policy_is_public_and_excludes_mining(self):
        policy_path = pathlib.Path("CODE_SIGNING.md")
        self.assertTrue(policy_path.exists(), "code-signing policy is missing")
        policy = policy_path.read_text(encoding="utf-8") if policy_path.exists() else ""

        for token in (
            "SignPath Foundation",
            "BTC09 Wallet",
            "wallet-only",
            "Mining software is outside",
            "Security reports",
            "GitHub Security Advisory",
            "Signing requests",
        ):
            with self.subTest(token=token):
                self.assertIn(token, policy)

        for forbidden in ("BEGIN PRIVATE KEY", "keystore password:", "seed phrase:"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, policy)

    def test_windows_direct_packager_is_wallet_only_and_fails_closed_on_signing(self):
        script_path = pathlib.Path("tools/release/package_windows_direct.ps1")
        self.assertTrue(script_path.exists(), "direct Windows packager is missing")
        script = script_path.read_text(encoding="utf-8") if script_path.exists() else ""

        for token in (
            "npm run store:build -- --bundles nsis",
            "verify-wallet-edition.mjs",
            "Get-AuthenticodeSignature",
            "AllowUnsignedPreflight",
            "signature is not trusted",
            "btc09-wallet-windows-x64-setup.exe",
        ):
            with self.subTest(token=token):
                self.assertIn(token, script)

        self.assertNotIn("Start mining", script)

    def test_homepage_reports_native_packaging_as_status_not_a_download(self):
        homepage = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        for token in (
            "Native wallet update",
            "Windows signing",
            "Android signed APK",
            "Linux AppImage",
            "Apple later",
            "v0.1.32 stays current",
        ):
            with self.subTest(token=token):
                self.assertIn(token, homepage)

        native_status = homepage.split("Native wallet update", 1)[-1].split("</aside>", 1)[0]
        self.assertNotIn("releases/download/v0.1.33", native_status)

    def test_gitlab_has_manual_reproducible_direct_package_jobs(self):
        pipeline = pathlib.Path(".gitlab-ci.yml").read_text(encoding="utf-8")
        for token in (
            "package-linux-appimage:",
            "package-android-apk:",
            "BTC09_ANDROID_KEYSTORE_B64",
            "BTC09_REQUIRE_ANDROID_SIGNATURE=1",
            "when: manual",
            "SHA256SUMS",
        ):
            with self.subTest(token=token):
                self.assertIn(token, pipeline)

    def test_gitlab_can_launch_check_an_exact_linux_release_artifact(self):
        pipeline = pathlib.Path(".gitlab-ci.yml").read_text(encoding="utf-8")
        for token in (
            "verify-linux-appimage:",
            "BTC09_LINUX_ARTIFACT_JOB_ID",
            "$CI_API_V4_URL/projects/$CI_PROJECT_ID/jobs/",
            "APPIMAGE_EXTRACT_AND_RUN=1",
            "xwininfo -root -tree",
            "BTC09 Wallet",
        ):
            with self.subTest(token=token):
                self.assertIn(token, pipeline)


if __name__ == "__main__":
    unittest.main()
