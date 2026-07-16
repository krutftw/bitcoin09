import pathlib
import unittest


class DirectDistributionContractTest(unittest.TestCase):
    def test_public_guide_links_the_released_no_store_beta(self):
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
            "Available beta files",
            "releases/tag/v0.1.33-beta.1",
            "btc09-wallet-android-arm64.apk",
            "btc09-wallet-linux-x64.AppImage",
        ):
            with self.subTest(token=token):
                self.assertIn(token, guide)

        self.assertNotIn("will use GitHub Releases", guide)

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

    def test_homepage_links_the_released_native_wallet_beta(self):
        homepage = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        for token in (
            "Native wallet beta",
            "Windows signing",
            "Android signed APK",
            "Linux AppImage",
            "Not in this beta",
            "releases/download/v0.1.33-beta.1/btc09-wallet-android-arm64.apk",
            "releases/download/v0.1.33-beta.1/btc09-wallet-linux-x64.AppImage",
            "releases/download/v0.1.33-beta.1/SHA256SUMS",
        ):
            with self.subTest(token=token):
                self.assertIn(token, homepage)

        native_status = homepage.split("Native wallet beta", 1)[-1].split("</aside>", 1)[0]
        self.assertNotIn("In preparation", native_status)

    def test_homepage_and_download_guide_disclose_the_signing_policy(self):
        homepage = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        guide = pathlib.Path("docs/DIRECT-DOWNLOADS.md").read_text(encoding="utf-8")
        required = (
            "Code signing policy",
            "Free code signing provided by",
            "SignPath.io",
            "certificate by SignPath Foundation",
        )
        for surface in (homepage, guide):
            for token in required:
                with self.subTest(token=token):
                    self.assertIn(token, surface)

    def test_appveyor_builds_the_unsigned_windows_artifact_for_signpath(self):
        pipeline_path = pathlib.Path("appveyor.yml")
        self.assertTrue(pipeline_path.exists(), "free AppVeyor pipeline is missing")
        pipeline = pipeline_path.read_text(encoding="utf-8") if pipeline_path.exists() else ""
        toolchain = pathlib.Path("tools/release/install_appveyor_toolchain.ps1")
        build = pathlib.Path("tools/release/build_windows_appveyor.ps1")
        self.assertTrue(toolchain.exists(), "AppVeyor toolchain installer is missing")
        self.assertTrue(build.exists(), "AppVeyor Windows builder is missing")
        implementation = "\n".join(
            (
                pipeline,
                toolchain.read_text(encoding="utf-8") if toolchain.exists() else "",
                build.read_text(encoding="utf-8") if build.exists() else "",
            )
        )
        for token in (
            "Visual Studio 2022",
            "go1.25.12.windows-amd64.zip",
            "d5dc82da351b00e5eedd04f41356817d674cc4308131f0f638a5b14c5c3af4cb",
            "Install-Product node 24.16.0 x64",
            "rustup toolchain install 1.95.0 --profile minimal --no-self-update",
            "RUSTUP_TOOLCHAIN",
            "npm ci",
            "go test ./...",
            "package_windows_direct.ps1",
            "-AllowUnsignedPreflight",
            "btc09-wallet-windows-x64-setup.exe",
            "verify-wallet-edition.mjs",
        ):
            with self.subTest(token=token):
                self.assertIn(token, implementation)

        for forbidden in (
            "SIGNPATH_API_TOKEN",
            "BEGIN PRIVATE KEY",
            "signtool sign",
            "rustup default",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, implementation)

        self.assertLess(
            implementation.index("prepare-sidecar.mjs wallet"),
            implementation.index("cargo test --manifest-path walletapp/src-tauri/Cargo.toml"),
            "Tauri tests need the wallet-only sidecar before Cargo starts",
        )

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
            "libasound2",
        ):
            with self.subTest(token=token):
                self.assertIn(token, pipeline)


if __name__ == "__main__":
    unittest.main()
