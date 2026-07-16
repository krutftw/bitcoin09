import pathlib
import unittest


class CIWorkflowContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow_path = pathlib.Path(".github/workflows/ci.yml")
        cls.workflow = cls.workflow_path.read_text(encoding="utf-8")
        cls.gitlab_path = pathlib.Path(".gitlab-ci.yml")
        cls.gitlab = cls.gitlab_path.read_text(encoding="utf-8")

    def test_ci_runs_for_changes_and_manual_recovery(self):
        for token in ("push:", "pull_request:", "workflow_dispatch:"):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_ci_is_read_only_and_cancels_superseded_runs(self):
        for token in (
            "contents: read",
            "cancel-in-progress: true",
            "github.workflow",
            "github.ref",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_go_quality_gate_is_complete(self):
        for token in (
            "actions/checkout@v4",
            "actions/setup-go@v5",
            "go-version-file: go.mod",
            "golang/govulncheck-action@v1",
            "go-package: ./...",
            "go vet ./...",
            "go test -race ./...",
            "go test -race -tags walletedition ./desktop ./cmd/btc09",
            "go build -trimpath -tags walletedition -o btc09-wallet-core ./cmd/btc09",
            "node tools/desktop/verify-wallet-edition.mjs btc09-wallet-core",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_project_contracts_cover_public_surfaces(self):
        for token in (
            "actions/setup-node@v4",
            "node-version: 24",
            "actions/setup-python@v5",
            'python-version: "3.13"',
            "node --test tools/discord/*.test.mjs",
            "node --test nineinbox/web/*.test.mjs",
            "node --test tools/desktop/*.test.mjs tools/desktop/*.test.cjs",
            "python -m unittest discover -s bot/tests -p 'test_*.py'",
            "python -m unittest \\",
            "tools.release.test_package_macos",
            "tools.site.test_index_contract",
            "tools.deploy.test_nine_inbox_contract",
            "tools.deploy.test_open_miner_contract",
            "tools.deploy.test_web_presence_contract",
            "tools.exchange.test_btc09_exchange_smoke",
            "tools.ci.test_ci_workflow_contract",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_gitlab_mirror_runs_core_contracts_and_security_scanners(self):
        for token in (
            'GIT_DEPTH: "0"',
            "Jobs/SAST.gitlab-ci.yml",
            "Jobs/Secret-Detection.gitlab-ci.yml",
            "govulncheck@v1.3.0",
            "govulncheck ./...",
            "pip-audit==2.10.0",
            "pip-audit -r bot/requirements.txt",
            "apt-get install -y --no-install-recommends shellcheck",
            "npm --prefix walletapp audit --omit=dev --audit-level=high",
            "go vet ./...",
            "go test -race ./...",
            "go test -tags walletedition ./desktop ./cmd/btc09",
            "node tools/desktop/verify-wallet-edition.mjs btc09-wallet-core",
            "node --test tools/mobile/*.test.mjs",
            "python -m unittest discover -s bot/tests -p 'test_*.py'",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.gitlab)

        self.assertNotIn("Jobs/Dependency-Scanning.v2.gitlab-ci.yml", self.gitlab)

        for forbidden in ("BEGIN PRIVATE KEY", "codesign --sign", "signtool sign"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, self.gitlab)

        for secret_name in (
            "BTC09_ANDROID_KEYSTORE_B64",
            "BTC09_ANDROID_KEYSTORE_PASSWORD",
            "BTC09_ANDROID_KEY_PASSWORD",
        ):
            with self.subTest(secret_name=secret_name):
                self.assertNotRegex(
                    self.gitlab,
                    rf"(?m)^\s*{secret_name}\s*:\s*\S+",
                    "release secrets must be injected by GitLab, never defined in YAML",
                )

    def test_native_wallet_is_checked_on_each_desktop_platform(self):
        for token in (
            "name: Native wallet",
            "windows-latest",
            "macos-14",
            "ubuntu-22.04",
            "libwebkit2gtk-4.1-dev",
            "libayatana-appindicator3-dev",
            "dtolnay/rust-toolchain@stable",
            "cargo fmt --manifest-path walletapp/src-tauri/Cargo.toml -- --check",
            "cargo test --manifest-path walletapp/src-tauri/Cargo.toml",
            "npm run desktop:build -- --no-bundle",
            "npm run store:build -- --no-bundle",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_mobile_wallet_contracts_and_visual_flow_are_gated(self):
        for token in (
            "name: Mobile wallet contracts",
            "node --test tools/mobile/*.test.mjs",
            "gradle/actions/wrapper-validation@v6",
            "npm --prefix walletapp run mobile:check",
            "playwright install --with-deps chromium",
            "npm --prefix walletapp run mobile:ui-smoke",
            "cargo check --manifest-path walletapp/plugins/tauri-plugin-wallet-core/Cargo.toml",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_android_wallet_build_and_package_security_are_gated(self):
        for token in (
            "actions/setup-java@v4",
            'java-version: "17"',
            '"platforms;android-36"',
            '"build-tools;36.0.0"',
            '"ndk;29.0.14206865"',
            "rustup target add aarch64-linux-android",
            "npm run mobile:android:build",
            "app-universal-release.aab",
            "jar is unsigned",
            "zipalign\" -c -P 16",
            'android:allowBackup="false"',
            'android:usesCleartextTraffic="false"',
            "android.permission.CAMERA",
            "android.permission.VIBRATE",
            'android.hardware.camera.any"[[:space:]]*android:required="false"',
            "aapt2\" dump badging",
            "native-code: 'arm64-v8a'",
            "miner|sidecar|btc09-core",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_windows_store_package_is_built_without_signing_credentials(self):
        for token in (
            "package_windows_store.ps1",
            "BTC09.Test.Wallet",
            "CN=BTC09 Test Publisher",
            "btc09-wallet-store.msix",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_iphone_wallet_is_compiled_for_the_simulator(self):
        for token in (
            "name: iPhone wallet simulator",
            "runs-on: macos-14",
            "aarch64-apple-ios-sim",
            "npm --prefix walletapp run mobile:core:ios",
            "tauri -- ios init --ci --skip-targets-install",
            "npm run mobile:ios:simulator",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)


if __name__ == "__main__":
    unittest.main()
